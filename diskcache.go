package debuginfod

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const (
	cacheDirMode      os.FileMode = 0o750
	cacheFileMode     os.FileMode = 0o400
	stagingFileMode   os.FileMode = 0o600
	stagingNamePrefix             = ".tmp-"
	stagingRetries                = 3
)

type DiskCacheOptions struct {
	// Dir is the root directory for cached artifacts.
	Dir string
}

// DiskCache is a file-system-backed Cache implementation.
// Layout: <Dir>/<buildID>/{debuginfo,executable,section/<escaped-name>,source/<escaped-path>}
type DiskCache struct {
	root *os.Root
}

func NewDiskCache(opts DiskCacheOptions) (*DiskCache, error) {
	if opts.Dir == "" {
		return nil, fmt.Errorf("debuginfod: cache directory is required")
	}
	if err := os.MkdirAll(opts.Dir, cacheDirMode); err != nil {
		return nil, fmt.Errorf("debuginfod: failed to create cache directory: %w", err)
	}
	root, err := os.OpenRoot(opts.Dir)
	if err != nil {
		return nil, fmt.Errorf("debuginfod: failed to open cache directory: %w", err)
	}
	return &DiskCache{root: root}, nil
}

// Close releases the underlying directory handle.
func (c *DiskCache) Close() error {
	return c.root.Close()
}

func (c *DiskCache) Get(_ context.Context, k Key) (io.ReadCloser, error) {
	relPath, err := c.path(k)
	if err != nil {
		return nil, err
	}
	file, err := c.root.Open(relPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("debuginfod: cache get %s: %w", k, err)
	}
	return file, nil
}

func (c *DiskCache) Create(_ context.Context, k Key) (CacheEntry, error) {
	relPath, err := c.path(k)
	if err != nil {
		return nil, err
	}
	if err := c.root.MkdirAll(filepath.Dir(relPath), cacheDirMode); err != nil {
		return nil, fmt.Errorf("debuginfod: cache create %s: %w", k, err)
	}
	file, stagingRel, err := c.createStagingFile(filepath.Dir(relPath))
	if err != nil {
		return nil, fmt.Errorf("debuginfod: cache create %s: %w", k, err)
	}
	return &diskCacheEntry{root: c.root, file: file, stagingPath: stagingRel, finalPath: relPath, key: k}, nil
}

func (c *DiskCache) Delete(_ context.Context, k Key) error {
	relPath, err := c.path(k)
	if err != nil {
		return err
	}
	if err := c.root.Remove(relPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("debuginfod: cache delete %s: %w", k, err)
	}
	return nil
}

// createStagingFile opens a uniquely-named writable file in parentDir.
// Generated names are random so concurrent Create calls don't collide.
func (c *DiskCache) createStagingFile(parentDir string) (*os.File, string, error) {
	var buf [8]byte
	for range stagingRetries {
		if _, err := rand.Read(buf[:]); err != nil {
			return nil, "", err
		}
		name := stagingNamePrefix + hex.EncodeToString(buf[:])
		relPath := filepath.Join(parentDir, name)
		file, err := c.root.OpenFile(relPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, stagingFileMode)
		if err == nil {
			return file, relPath, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, "", err
		}
	}
	return nil, "", fmt.Errorf("debuginfod: failed to allocate unique staging file in %s after %d attempts", parentDir, stagingRetries)
}

// path computes the root-relative on-disk location for k.
func (c *DiskCache) path(k Key) (string, error) {
	if err := k.validate(); err != nil {
		return "", err
	}
	switch k.Kind {
	case KindDebugInfo, KindExecutable:
		return filepath.Join(k.BuildID, k.Kind.String()), nil
	case KindSection:
		return filepath.Join(k.BuildID, k.Kind.String(), url.PathEscape(k.Qualifier)), nil
	case KindSource:
		segments, err := sourcePathSegments(k.Qualifier)
		if err != nil {
			return "", fmt.Errorf("debuginfod: cache key %s: %w", k, err)
		}
		return filepath.Join(append([]string{k.BuildID, k.Kind.String()}, segments...)...), nil
	}
	return "", fmt.Errorf("debuginfod: cache key %s has unknown kind", k)
}

// diskCacheEntry stages bytes in a temp file and atomically renames to the final path on Commit.
type diskCacheEntry struct {
	root        *os.Root
	file        *os.File
	stagingPath string
	finalPath   string
	key         Key
	committed   bool
	closed      bool
}

func (e *diskCacheEntry) Write(p []byte) (int, error) {
	return e.file.Write(p)
}

func (e *diskCacheEntry) Commit() error {
	if e.committed {
		return ErrAlreadyCommitted
	}
	if err := e.file.Close(); err != nil {
		return fmt.Errorf("debuginfod: cache commit %s: %w", e.key, err)
	}
	if err := e.root.Chmod(e.stagingPath, cacheFileMode); err != nil {
		_ = e.root.Remove(e.stagingPath)
		return fmt.Errorf("debuginfod: cache commit %s: %w", e.key, err)
	}
	if err := e.root.Rename(e.stagingPath, e.finalPath); err != nil {
		_ = e.root.Remove(e.stagingPath)
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
	_ = e.file.Close()
	_ = e.root.Remove(e.stagingPath)
	return nil
}

// sourcePathSegments URL-escapes each path segment so distinct source paths map to distinct on-disk locations.
// Rejects "." / ".." segments to keep that mapping injective.
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
