package debuginfod

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
)

const testBuildID = "aabbccdd"

type memCache struct {
	mu   sync.Mutex
	data map[Key][]byte
}

func newMemCache() *memCache {
	return &memCache{data: make(map[Key][]byte)}
}

func (c *memCache) Get(_ context.Context, k Key) (io.ReadCloser, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, ok := c.data[k]
	if !ok {
		return nil, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (c *memCache) Create(_ context.Context, k Key) (CacheEntry, error) {
	return &memCacheEntry{c: c, key: k, buf: &bytes.Buffer{}}, nil
}

func (c *memCache) Delete(_ context.Context, k Key) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.data, k)
	return nil
}

type memCacheEntry struct {
	c         *memCache
	key       Key
	buf       *bytes.Buffer
	committed bool
	closed    bool
}

func (e *memCacheEntry) Write(p []byte) (int, error) { return e.buf.Write(p) }

func (e *memCacheEntry) Commit() error {
	if e.committed {
		return ErrAlreadyCommitted
	}
	e.c.mu.Lock()
	defer e.c.mu.Unlock()
	e.c.data[e.key] = append([]byte(nil), e.buf.Bytes()...)
	e.committed = true
	return nil
}

func (e *memCacheEntry) Close() error {
	e.closed = true
	return nil
}

type failingCreateCache struct{ memCache }

func (c *failingCreateCache) Create(_ context.Context, _ Key) (CacheEntry, error) {
	return nil, errors.New("simulated cache create failure")
}

type failingCommitCache struct{ memCache }

func (c *failingCommitCache) Create(_ context.Context, k Key) (CacheEntry, error) {
	return &failingCommitEntry{memCacheEntry: memCacheEntry{c: &c.memCache, key: k, buf: &bytes.Buffer{}}}, nil
}

type failingCommitEntry struct {
	memCacheEntry
}

func (e *failingCommitEntry) Commit() error {
	return errors.New("simulated cache commit failure")
}
