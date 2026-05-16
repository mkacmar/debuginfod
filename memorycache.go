package debuginfod

import (
	"bytes"
	"context"
	"io"
	"sync"
)

// MemoryCache is an in-process Cache implementation backed by a map.
//
// MemoryCache is suitable for short-lived processes, tests, and environments without writable disk.
// It is unbounded: entries live until Evict is called or the process exits.
// For long-running processes use DiskCache or implement a bounded Cache.
//
// MemoryCache is safe for concurrent use.
type MemoryCache struct {
	mu   sync.Mutex
	data map[Key][]byte
}

// NewMemoryCache returns a new empty MemoryCache.
func NewMemoryCache() *MemoryCache {
	return &MemoryCache{data: make(map[Key][]byte)}
}

func (c *MemoryCache) Fetch(_ context.Context, k Key) (io.ReadCloser, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, ok := c.data[k]
	if !ok {
		return nil, ErrNotFound
	}
	return bytesReadCloser{bytes.NewReader(data)}, nil
}

func (c *MemoryCache) Stage(_ context.Context, k Key) (CacheEntry, error) {
	return &memoryCacheEntry{cache: c, key: k, buf: &bytes.Buffer{}}, nil
}

func (c *MemoryCache) Evict(_ context.Context, k Key) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.data, k)
	return nil
}

type memoryCacheEntry struct {
	cache     *MemoryCache
	key       Key
	buf       *bytes.Buffer
	committed bool
}

func (e *memoryCacheEntry) Write(p []byte) (int, error) { return e.buf.Write(p) }

func (e *memoryCacheEntry) Commit() error {
	if e.committed {
		return ErrAlreadyCommitted
	}
	e.cache.mu.Lock()
	e.cache.data[e.key] = append([]byte(nil), e.buf.Bytes()...)
	e.cache.mu.Unlock()
	e.committed = true
	return nil
}

func (e *memoryCacheEntry) Close() error { return nil }

// bytesReadCloser adapts *bytes.Reader to io.ReadCloser with a no-op Close.
type bytesReadCloser struct{ *bytes.Reader }

func (bytesReadCloser) Close() error { return nil }
