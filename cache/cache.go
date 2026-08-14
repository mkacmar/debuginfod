// Package cache provides a filesystem-backed cache for debuginfod artifacts.
//
// DiskCache wraps a [debuginfod.Client] and turns its streaming responses into [Entry] handles (an embedded [*os.File] plus the artifact's [debuginfod.Metadata]) suitable for random-access reads.
//
// On-disk layout under the root directory:
//
//	<buildID>/debuginfo
//	<buildID>/executable
//	<buildID>/section/<escaped-name>
//	<buildID>/source/<escaped-seg>/<escaped-seg>/...
//
// Response metadata is persisted in a per-buildID .meta/ subdirectory that mirrors the body layout, e.g. the sidecar for <buildID>/debuginfo lives at <buildID>/.meta/debuginfo.
// Keeping metadata beneath the buildID means removing a buildID directory removes its metadata with it.
// A missing or undecodable sidecar means the metadata is unknown (for example, an entry cached before metadata support), in which case Meta is returned as its zero value.
//
// Writes are atomic.
package cache

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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
	metaDirName                   = ".meta"
)

// Fetcher provides debuginfod artifacts to a DiskCache on a miss.
// [*debuginfod.Client] implements Fetcher.
// Implement it yourself to back the cache with a custom source such as a test stub or an alternative transport.
type Fetcher interface {
	Fetch(ctx context.Context, k key.Key) (debuginfod.Response, error)
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

// Entry is a cached artifact: the embedded [*os.File] exposes random-access reads (ReadAt, Close, Name) via method promotion, and Meta carries the artifact's metadata.
// Meta is the zero [debuginfod.Metadata] when the cache has no metadata for the artifact.
type Entry struct {
	*os.File
	Meta debuginfod.Metadata
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

// Get returns an [Entry] for the artifact identified by k, fetching and caching it on a miss.
func (c *DiskCache) Get(ctx context.Context, k key.Key) (Entry, error) {
	path, err := c.path(k)
	if err != nil {
		return Entry{}, err
	}
	if entry, err := c.open(k, path); err == nil {
		return entry, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Entry{}, fmt.Errorf("debuginfod/cache: open %s: %w", k, err)
	}
	if err := c.fetchAndStore(ctx, k, path); err != nil {
		return Entry{}, fmt.Errorf("debuginfod/cache: fetch %s: %w", k, err)
	}
	entry, err := c.open(k, path)
	if err != nil {
		return Entry{}, fmt.Errorf("debuginfod/cache: reopen %s after fetch: %w", k, err)
	}
	return entry, nil
}

// open opens the cached body for k and pairs it with its sidecar metadata.
// A missing body returns an error wrapping os.ErrNotExist.
// A missing sidecar yields zero Meta.
func (c *DiskCache) open(k key.Key, path string) (Entry, error) {
	file, err := c.root.Open(path)
	if err != nil {
		return Entry{}, err
	}
	meta, err := c.readMeta(k)
	if err != nil {
		_ = file.Close()
		return Entry{}, fmt.Errorf("read metadata: %w", err)
	}
	return Entry{File: file, Meta: meta}, nil
}

// Delete removes the cached artifact for k and its metadata sidecar, and best-effort prunes empty parent directories.
// Returns nil if the artifact is not cached.
func (c *DiskCache) Delete(_ context.Context, k key.Key) error {
	path, err := c.path(k)
	if err != nil {
		return err
	}
	metaPath, err := c.metaPath(k)
	if err != nil {
		return err
	}
	if err := c.root.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("debuginfod/cache: delete %s: %w", k, err)
	}
	if err := c.root.Remove(metaPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("debuginfod/cache: delete metadata %s: %w", k, err)
	}
	c.pruneEmptyParents(path)
	c.pruneEmptyParents(metaPath)
	return nil
}

// pruneEmptyParents best-effort removes empty ancestor directories of path, stopping at the first non-empty one.
func (c *DiskCache) pruneEmptyParents(path string) {
	for dir := filepath.Dir(path); dir != "." && dir != ""; dir = filepath.Dir(dir) {
		if err := c.root.Remove(dir); err != nil {
			break
		}
	}
}

func (c *DiskCache) fetchAndStore(ctx context.Context, k key.Key, finalPath string) error {
	resp, err := c.client.Fetch(ctx, k)
	if err != nil {
		return err
	}
	defer resp.Close()

	metaFinalPath, err := c.metaPath(k)
	if err != nil {
		return err
	}
	metaJSON, err := json.Marshal(resp.Meta)
	if err != nil {
		return err
	}

	if err := c.root.MkdirAll(filepath.Dir(finalPath), cacheDirMode); err != nil {
		return err
	}
	if err := c.root.MkdirAll(filepath.Dir(metaFinalPath), cacheDirMode); err != nil {
		return err
	}

	bodyStaging, err := c.writeStaging(filepath.Dir(finalPath), resp.ReadCloser)
	if err != nil {
		return err
	}
	bodyCommitted := false
	defer func() {
		if !bodyCommitted {
			_ = c.root.Remove(bodyStaging)
		}
	}()

	metaStaging, err := c.writeStaging(filepath.Dir(metaFinalPath), bytes.NewReader(metaJSON))
	if err != nil {
		return err
	}
	metaCommitted := false
	defer func() {
		if !metaCommitted {
			_ = c.root.Remove(metaStaging)
		}
	}()

	// Commit the sidecar before the body so a present body always has its metadata.
	// A committed sidecar is never rolled back, since removing it could delete one a concurrent writer just committed.
	// An orphan sidecar is harmless because a sidecar is never read without its body, and the next fetch overwrites it.
	if err := c.root.Rename(metaStaging, metaFinalPath); err != nil {
		return err
	}
	metaCommitted = true
	if err := c.root.Rename(bodyStaging, finalPath); err != nil {
		return err
	}
	bodyCommitted = true
	return nil
}

// writeStaging streams src to a fresh staging file in dir, fsyncs and closes it, and makes it read-only.
// It returns the staging path for the caller to rename into place.
// On error the staging file is removed.
func (c *DiskCache) writeStaging(dir string, src io.Reader) (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	stagingPath := filepath.Join(dir, stagingNamePrefix+hex.EncodeToString(buf[:]))
	staging, err := c.root.OpenFile(stagingPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, stagingFileMode)
	if err != nil {
		return "", err
	}
	cleanup := func(err error) (string, error) {
		_ = staging.Close()
		_ = c.root.Remove(stagingPath)
		return "", err
	}
	if _, err := io.Copy(staging, src); err != nil {
		return cleanup(err)
	}
	if err := staging.Sync(); err != nil {
		return cleanup(err)
	}
	if err := staging.Close(); err != nil {
		_ = c.root.Remove(stagingPath)
		return "", err
	}
	if err := c.root.Chmod(stagingPath, cacheFileMode); err != nil {
		_ = c.root.Remove(stagingPath)
		return "", err
	}
	return stagingPath, nil
}

// metaPath returns the root-relative location of k's metadata sidecar in the per-buildID .meta/ subdirectory.
func (c *DiskCache) metaPath(k key.Key) (string, error) {
	buildID, rest, err := c.pathParts(k)
	if err != nil {
		return "", err
	}
	return filepath.Join(buildID, metaDirName, rest), nil
}

// readMeta loads the metadata sidecar for k.
// A missing or undecodable sidecar yields the zero Metadata and a nil error.
// Other read errors, including an os.Root path rejection, propagate and fail the Get.
func (c *DiskCache) readMeta(k key.Key) (debuginfod.Metadata, error) {
	metaPath, err := c.metaPath(k)
	if err != nil {
		return debuginfod.Metadata{}, err
	}
	f, err := c.root.Open(metaPath)
	if errors.Is(err, os.ErrNotExist) {
		return debuginfod.Metadata{}, nil
	}
	if err != nil {
		return debuginfod.Metadata{}, err
	}
	defer f.Close()
	var meta debuginfod.Metadata
	if err := json.NewDecoder(f).Decode(&meta); err != nil {
		return debuginfod.Metadata{}, nil
	}
	return meta, nil
}

// path computes the root-relative on-disk location for k after validating it.
func (c *DiskCache) path(k key.Key) (string, error) {
	buildID, rest, err := c.pathParts(k)
	if err != nil {
		return "", err
	}
	return filepath.Join(buildID, rest), nil
}

// pathParts splits k into its build ID directory and the artifact path beneath it, after validating k.
// It is the single source of truth for the on-disk layout, so bodies and metadata sidecars cannot drift apart.
func (c *DiskCache) pathParts(k key.Key) (string, string, error) {
	if err := k.Validate(); err != nil {
		return "", "", err
	}
	buildID := k.BuildID()
	switch k.Kind() {
	case key.KindDebugInfo, key.KindExecutable:
		return buildID, k.Kind().String(), nil
	case key.KindSection:
		return buildID, filepath.Join(k.Kind().String(), url.PathEscape(k.Qualifier())), nil
	case key.KindSource:
		segments := strings.Split(strings.TrimPrefix(k.Qualifier(), "/"), "/")
		for i, seg := range segments {
			segments[i] = url.PathEscape(seg)
		}
		return buildID, filepath.Join(k.Kind().String(), filepath.Join(segments...)), nil
	}
	return "", "", fmt.Errorf("debuginfod/cache: key %s has unknown kind", k)
}
