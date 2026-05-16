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

// putReader stores all bytes from reader under key.
// It runs the full Create-Copy-Commit-Close lifecycle and aborts on any error.
func putReader(ctx context.Context, cache Cache, key Key, reader io.Reader) error {
	entry, err := cache.Create(ctx, key)
	if err != nil {
		return err
	}
	defer entry.Close()
	if _, err := io.Copy(entry, reader); err != nil {
		return err
	}
	return entry.Commit()
}

type DiskCacheOptions struct {
	// Dir is the root directory for cached artifacts.
	Dir string
}

// DiskCache is a file-system-backed Cache implementation.
// Layout: <dir>/<buildID>/{debuginfo,executable,section/<escaped-name>,source/<escaped-path>}
type DiskCache struct {
	dir string
}

func NewDiskCache(opts DiskCacheOptions) (*DiskCache, error) {
	if opts.Dir == "" {
		return nil, fmt.Errorf("debuginfod: cache directory is required")
	}
	if err := os.MkdirAll(opts.Dir, 0750); err != nil {
		return nil, fmt.Errorf("debuginfod: failed to create cache directory: %w", err)
	}
	return &DiskCache{dir: opts.Dir}, nil
}

func (c *DiskCache) Get(_ context.Context, key Key) (io.ReadCloser, error) {
	path, err := c.path(key)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path) // #nosec G304 -- path derived from configured cache directory
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("debuginfod: cache get %s: %w", key, err)
	}
	return file, nil
}

func (c *DiskCache) Create(_ context.Context, key Key) (CacheEntry, error) {
	path, err := c.path(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return nil, fmt.Errorf("debuginfod: cache create %s: %w", key, err)
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return nil, fmt.Errorf("debuginfod: cache create %s: %w", key, err)
	}
	return &diskCacheEntry{file: file, finalPath: path, key: key}, nil
}

// diskCacheEntry stages bytes in a temp file and atomically renames to the final path on Commit.
type diskCacheEntry struct {
	file      *os.File
	finalPath string
	key       Key
	committed bool
	closed    bool
}

func (e *diskCacheEntry) Write(p []byte) (int, error) {
	return e.file.Write(p)
}

func (e *diskCacheEntry) Commit() error {
	if e.committed {
		return ErrAlreadyCommitted
	}
	tempPath := e.file.Name()
	if err := e.file.Close(); err != nil {
		return fmt.Errorf("debuginfod: cache commit %s: %w", e.key, err)
	}
	if err := os.Chmod(tempPath, 0400); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("debuginfod: cache commit %s: %w", e.key, err)
	}
	if err := os.Rename(tempPath, e.finalPath); err != nil {
		_ = os.Remove(tempPath)
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
	tempPath := e.file.Name()
	_ = e.file.Close()
	_ = os.Remove(tempPath)
	return nil
}

func (c *DiskCache) Delete(_ context.Context, key Key) error {
	path, err := c.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("debuginfod: cache delete %s: %w", key, err)
	}
	return nil
}

func (c *DiskCache) path(key Key) (string, error) {
	if key.BuildID == "" {
		return "", fmt.Errorf("debuginfod: cache key has empty BuildID")
	}
	switch key.Kind {
	case KindDebugInfo, KindExecutable:
		if key.Qualifier != "" {
			return "", fmt.Errorf("debuginfod: cache key %s has unexpected qualifier", key)
		}
		return filepath.Join(c.dir, key.BuildID, key.Kind.String()), nil
	case KindSection:
		if key.Qualifier == "" {
			return "", fmt.Errorf("debuginfod: cache key %s requires qualifier", key)
		}
		return filepath.Join(c.dir, key.BuildID, key.Kind.String(), url.PathEscape(key.Qualifier)), nil
	case KindSource:
		if key.Qualifier == "" {
			return "", fmt.Errorf("debuginfod: cache key %s requires qualifier", key)
		}
		segments, err := sourcePathSegments(key.Qualifier)
		if err != nil {
			return "", fmt.Errorf("debuginfod: cache key %s: %w", key, err)
		}
		return filepath.Join(append([]string{c.dir, key.BuildID, key.Kind.String()}, segments...)...), nil
	}
	return "", fmt.Errorf("debuginfod: cache key %s has unknown kind", key)
}

// sourcePathSegments URL-escapes each path segment and rejects "." / ".." segments.
func sourcePathSegments(path string) ([]string, error) {
	var escaped []string
	for _, segment := range strings.Split(path, "/") {
		if segment == "" {
			continue
		}
		if segment == "." || segment == ".." {
			return nil, fmt.Errorf("invalid path segment %q", segment)
		}
		escaped = append(escaped, url.PathEscape(segment))
	}
	if len(escaped) == 0 {
		return nil, fmt.Errorf("path is empty")
	}
	return escaped, nil
}
