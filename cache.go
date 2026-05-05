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

	// Put stores the artifact from r under the given key.
	// Put overwrites any existing entry for the key.
	// Put is atomic: either the new entry is fully readable, or the cache is unchanged.
	Put(ctx context.Context, k Key, r io.Reader) error

	// Delete removes the artifact for the given key.
	// Returns nil if the key does not exist.
	Delete(ctx context.Context, k Key) error
}

// DefaultCacheDir returns the platform default cache directory.
func DefaultCacheDir() (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("debuginfod: failed to determine user cache directory: %w", err)
	}
	return filepath.Join(cacheDir, "debuginfod"), nil
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

func (c *DiskCache) Put(_ context.Context, k Key, r io.Reader) error {
	p, err := c.path(k)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0750); err != nil {
		return fmt.Errorf("debuginfod: cache put %s: %w", k, err)
	}

	f, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return fmt.Errorf("debuginfod: cache put %s: %w", k, err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	defer func() { _ = f.Close() }()

	if _, err := io.Copy(f, r); err != nil {
		return fmt.Errorf("debuginfod: cache put %s: %w", k, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("debuginfod: cache put %s: %w", k, err)
	}
	if err := os.Chmod(tmp, 0400); err != nil {
		return fmt.Errorf("debuginfod: cache put %s: %w", k, err)
	}
	if err := os.Rename(tmp, p); err != nil {
		return fmt.Errorf("debuginfod: cache put %s: %w", k, err)
	}
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
		segs, err := safeSourceSegments(k.Qualifier)
		if err != nil {
			return "", fmt.Errorf("debuginfod: cache key %s: %w", k, err)
		}
		return filepath.Join(append([]string{c.dir, k.BuildID, k.Kind.String()}, segs...)...), nil
	}
	return "", fmt.Errorf("debuginfod: cache key %s has unknown kind", k)
}

// safeSourceSegments URL-escapes each path segment and rejects "." / ".." segments.
func safeSourceSegments(p string) ([]string, error) {
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
