// Package cache provides a filesystem-backed cache for debuginfod artifacts.
//
// DiskCache wraps a [debuginfod.Client] and turns its streaming responses into [*os.File] handles suitable for random-access reads.
//
// On-disk layout under the root directory:
//
//	<buildID>/debuginfo
//	<buildID>/executable
//	<buildID>/section/<escaped-name>
//	<buildID>/source/<escaped-seg>/<escaped-seg>/...
//
// Writes are atomic.
package cache

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

	"go.kacmar.sk/debuginfod"
	"go.kacmar.sk/debuginfod/key"
)

const (
	cacheDirMode      os.FileMode = 0o750
	cacheFileMode     os.FileMode = 0o400
	stagingFileMode   os.FileMode = 0o600
	stagingNamePrefix             = ".tmp-"
)

// Fetcher provides debuginfod artifacts to a DiskCache on a miss.
// [*debuginfod.Client] implements Fetcher.
// Implement it yourself to back the cache with a custom source such as a test stub or an alternative transport.
type Fetcher interface {
	Fetch(ctx context.Context, k key.Key) (io.ReadCloser, error)
}

var _ Fetcher = (*debuginfod.Client)(nil)

type DiskCacheOptions struct {
	// Client fetches artifacts on a miss. Required.
	Client Fetcher
	// Dir is the root directory for cached artifacts. Required.
	// Created if it does not already exist.
	Dir string
}

// DiskCache caches debuginfod artifacts on the local filesystem.
type DiskCache struct {
	client Fetcher
	root   *os.Root
}

func NewDiskCache(opts DiskCacheOptions) (*DiskCache, error) {
	if opts.Client == nil {
		return nil, fmt.Errorf("debuginfod/cache: client is required")
	}
	if opts.Dir == "" {
		return nil, fmt.Errorf("debuginfod/cache: directory is required")
	}
	if err := os.MkdirAll(opts.Dir, cacheDirMode); err != nil {
		return nil, fmt.Errorf("debuginfod/cache: failed to create cache directory: %w", err)
	}
	root, err := os.OpenRoot(opts.Dir)
	if err != nil {
		return nil, fmt.Errorf("debuginfod/cache: failed to open cache directory: %w", err)
	}
	return &DiskCache{client: opts.Client, root: root}, nil
}

func (c *DiskCache) Close() error {
	return c.root.Close()
}

// Get returns an [*os.File] for the artifact identified by k, fetching and caching it on a miss.
func (c *DiskCache) Get(ctx context.Context, k key.Key) (*os.File, error) {
	path, err := c.path(k)
	if err != nil {
		return nil, err
	}
	if file, err := c.root.Open(path); err == nil {
		return file, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("debuginfod/cache: open %s: %w", k, err)
	}
	if err := c.fetchAndStore(ctx, k, path); err != nil {
		return nil, fmt.Errorf("debuginfod/cache: fetch %s: %w", k, err)
	}
	file, err := c.root.Open(path)
	if err != nil {
		return nil, fmt.Errorf("debuginfod/cache: reopen %s after fetch: %w", k, err)
	}
	return file, nil
}

// Delete removes the cached artifact for k and best-effort prunes empty parent directories.
// Returns nil if the artifact is not cached.
func (c *DiskCache) Delete(_ context.Context, k key.Key) error {
	path, err := c.path(k)
	if err != nil {
		return err
	}
	if err := c.root.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("debuginfod/cache: delete %s: %w", k, err)
	}
	for dir := filepath.Dir(path); dir != "." && dir != ""; dir = filepath.Dir(dir) {
		if err := c.root.Remove(dir); err != nil {
			break
		}
	}
	return nil
}

func (c *DiskCache) fetchAndStore(ctx context.Context, k key.Key, finalPath string) error {
	rc, err := c.client.Fetch(ctx, k)
	if err != nil {
		return err
	}
	defer rc.Close()

	parent := filepath.Dir(finalPath)
	if err := c.root.MkdirAll(parent, cacheDirMode); err != nil {
		return err
	}

	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return err
	}
	stagingPath := filepath.Join(parent, stagingNamePrefix+hex.EncodeToString(buf[:]))
	staging, err := c.root.OpenFile(stagingPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, stagingFileMode)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = staging.Close()
			_ = c.root.Remove(stagingPath)
		}
	}()

	if _, err := io.Copy(staging, rc); err != nil {
		return err
	}
	if err := staging.Sync(); err != nil {
		return err
	}
	if err := staging.Close(); err != nil {
		return err
	}
	if err := c.root.Chmod(stagingPath, cacheFileMode); err != nil {
		return err
	}
	if err := c.root.Rename(stagingPath, finalPath); err != nil {
		return err
	}
	committed = true
	return nil
}

// path computes the root-relative on-disk location for k after validating it.
func (c *DiskCache) path(k key.Key) (string, error) {
	if err := k.Validate(); err != nil {
		return "", err
	}
	buildID := k.BuildID()
	switch k.Kind() {
	case key.KindDebugInfo, key.KindExecutable:
		return filepath.Join(buildID, k.Kind().String()), nil
	case key.KindSection:
		return filepath.Join(buildID, k.Kind().String(), url.PathEscape(k.Qualifier())), nil
	case key.KindSource:
		segments := strings.Split(strings.TrimPrefix(k.Qualifier(), "/"), "/")
		for i, seg := range segments {
			segments[i] = url.PathEscape(seg)
		}
		return filepath.Join(buildID, k.Kind().String(), filepath.Join(segments...)), nil
	}
	return "", fmt.Errorf("debuginfod/cache: key %s has unknown kind", k)
}
