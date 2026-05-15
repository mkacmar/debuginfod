package debuginfod

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// ArtifactKind identifies which artifact a Key refers to.
type ArtifactKind int

const (
	KindDebugInfo ArtifactKind = iota
	KindExecutable
	KindSource
	KindSection
)

func (k ArtifactKind) String() string {
	switch k {
	case KindDebugInfo:
		return "debuginfo"
	case KindExecutable:
		return "executable"
	case KindSource:
		return "source"
	case KindSection:
		return "section"
	}
	return fmt.Sprintf("unknown(%d)", int(k))
}

// Key uniquely identifies a cached artifact.
// Qualifier is the source path for KindSource, the section name for KindSection, and empty otherwise.
type Key struct {
	BuildID   string
	Kind      ArtifactKind
	Qualifier string
}

func (k Key) String() string {
	if k.Qualifier == "" {
		return fmt.Sprintf("%s/%s", k.BuildID, k.Kind)
	}
	return fmt.Sprintf("%s/%s/%s", k.BuildID, k.Kind, k.Qualifier)
}

// Cache is the interface for artifact storage backends.
// Implementations must be safe for concurrent use.
type Cache interface {
	// Get retrieves the artifact for the given key.
	// Returns ErrNotFound if the key does not exist.
	Get(ctx context.Context, k Key) (io.ReadCloser, error)

	// Create opens a new staging entry for the given key.
	// The caller writes bytes to the returned CacheEntry and then calls Commit to atomically promote them.
	// Closing without Commit discards the staged bytes.
	// Create overwrites any existing entry for the key on Commit.
	Create(ctx context.Context, k Key) (CacheEntry, error)

	// Delete removes the artifact for the given key.
	// Returns nil if the key does not exist.
	Delete(ctx context.Context, k Key) error
}

// CacheEntry is a writable staging handle for a new cache entry.
// Commit atomically promotes the staged bytes, Close before Commit discards them.
// Close must be idempotent and is a no-op after Commit.
type CacheEntry interface {
	io.WriteCloser
	// Commit atomically promotes the staged bytes to a live entry.
	// Commit returns ErrAlreadyCommitted if called more than once.
	Commit() error
}

// putReader stores all bytes from r under key k.
// It runs the full Create-Copy-Commit-Close lifecycle and aborts on any error.
func putReader(ctx context.Context, c Cache, k Key, r io.Reader) error {
	e, err := c.Create(ctx, k)
	if err != nil {
		return err
	}
	defer e.Close()
	if _, err := io.Copy(e, r); err != nil {
		return err
	}
	return e.Commit()
}

// DiskCacheOptions configures a DiskCache.
type DiskCacheOptions struct {
	// Dir is the root directory for cached artifacts.
	Dir string
}

// DiskCache is a file-system-backed Cache implementation.
// Layout: <dir>/<buildID>/{debuginfo,executable,section/<escaped-name>,source/<escaped-path>}
type DiskCache struct {
	dir string
}

// NewDiskCache creates a new DiskCache.
func NewDiskCache(opts DiskCacheOptions) (*DiskCache, error) {
	if opts.Dir == "" {
		return nil, fmt.Errorf("debuginfod: cache directory is required")
	}
	if err := os.MkdirAll(opts.Dir, 0750); err != nil {
		return nil, fmt.Errorf("debuginfod: failed to create cache directory: %w", err)
	}
	return &DiskCache{dir: opts.Dir}, nil
}

func (c *DiskCache) Get(_ context.Context, k Key) (io.ReadCloser, error) {
	p, err := c.path(k)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p) // #nosec G304 -- path derived from configured cache directory
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("debuginfod: cache get %s: %w", k, err)
	}
	return f, nil
}

func (c *DiskCache) Create(_ context.Context, k Key) (CacheEntry, error) {
	p, err := c.path(k)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0750); err != nil {
		return nil, fmt.Errorf("debuginfod: cache create %s: %w", k, err)
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return nil, fmt.Errorf("debuginfod: cache create %s: %w", k, err)
	}
	return &diskCacheEntry{f: f, finalPath: p, key: k}, nil
}

// diskCacheEntry stages bytes in a temp file and atomically renames to the final path on Commit.
type diskCacheEntry struct {
	f         *os.File
	finalPath string
	key       Key
	committed bool
	closed    bool
}

func (e *diskCacheEntry) Write(p []byte) (int, error) {
	return e.f.Write(p)
}

func (e *diskCacheEntry) Commit() error {
	if e.committed {
		return ErrAlreadyCommitted
	}
	tmp := e.f.Name()
	if err := e.f.Close(); err != nil {
		return fmt.Errorf("debuginfod: cache commit %s: %w", e.key, err)
	}
	if err := os.Chmod(tmp, 0400); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("debuginfod: cache commit %s: %w", e.key, err)
	}
	if err := os.Rename(tmp, e.finalPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("debuginfod: cache commit %s: %w", e.key, err)
	}
	e.committed = true
	return nil
}

func (e *diskCacheEntry) Close() error {
	if e.closed {
		return nil
	}
	e.closed = true
	if e.committed {
		return nil
	}
	tmp := e.f.Name()
	_ = e.f.Close()
	_ = os.Remove(tmp)
	return nil
}

func (c *DiskCache) Delete(_ context.Context, k Key) error {
	p, err := c.path(k)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("debuginfod: cache delete %s: %w", k, err)
	}
	return nil
}

func (c *DiskCache) path(k Key) (string, error) {
	if k.BuildID == "" {
		return "", fmt.Errorf("debuginfod: cache key has empty BuildID")
	}
	switch k.Kind {
	case KindDebugInfo, KindExecutable:
		if k.Qualifier != "" {
			return "", fmt.Errorf("debuginfod: cache key %s has unexpected qualifier", k)
		}
		return filepath.Join(c.dir, k.BuildID, k.Kind.String()), nil
	case KindSection:
		if k.Qualifier == "" {
			return "", fmt.Errorf("debuginfod: cache key %s requires qualifier", k)
		}
		return filepath.Join(c.dir, k.BuildID, k.Kind.String(), url.PathEscape(k.Qualifier)), nil
	case KindSource:
		if k.Qualifier == "" {
			return "", fmt.Errorf("debuginfod: cache key %s requires qualifier", k)
		}
		segs, err := sourcePathSegments(k.Qualifier)
		if err != nil {
			return "", fmt.Errorf("debuginfod: cache key %s: %w", k, err)
		}
		return filepath.Join(append([]string{c.dir, k.BuildID, k.Kind.String()}, segs...)...), nil
	}
	return "", fmt.Errorf("debuginfod: cache key %s has unknown kind", k)
}

// sourcePathSegments URL-escapes each path segment and rejects "." / ".." segments.
func sourcePathSegments(p string) ([]string, error) {
	var out []string
	for _, seg := range strings.Split(p, "/") {
		if seg == "" {
			continue
		}
		if seg == "." || seg == ".." {
			return nil, fmt.Errorf("invalid path segment %q", seg)
		}
		out = append(out, url.PathEscape(seg))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("path is empty")
	}
	return out, nil
}
