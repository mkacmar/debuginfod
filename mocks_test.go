package debuginfod

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
)

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

func (c *memCache) Put(_ context.Context, k Key, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data[k] = data
	return nil
}

func (c *memCache) Delete(_ context.Context, k Key) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.data, k)
	return nil
}

type failingPutCache struct{ memCache }

func (c *failingPutCache) Put(_ context.Context, _ Key, _ io.Reader) error {
	return errors.New("simulated cache failure")
}
