package debuginfod

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

const testBuildID = "cafebabedeadbeef0123456789abcdef00112233"

var (
	testKey     = Key{BuildID: testBuildID, Kind: KindDebugInfo}
	testPayload = []byte("payload")
)

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// staticServer starts an httptest server that responds to every request with body.
// The server is closed automatically when the test ends.
func staticServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// mustNewClient constructs a Client or fails the test.
func mustNewClient(t *testing.T, opts Options) *Client {
	t.Helper()
	client, err := NewClient(opts)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

// readAndClose drains rc, closes it, and returns the bytes read.
// Read or close errors fail the test.
func readAndClose(t *testing.T, rc io.ReadCloser) []byte {
	t.Helper()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}
	return data
}

// stubSource is a source whose Fetch behavior is controlled by a caller-supplied function.
// It records every call for inspection.
type stubSource struct {
	fetch func(ctx context.Context, key Key) (io.ReadCloser, error)

	mu    sync.Mutex
	calls []Key
}

func newStubSource(fetch func(ctx context.Context, key Key) (io.ReadCloser, error)) *stubSource {
	return &stubSource{fetch: fetch}
}

func (s *stubSource) Fetch(ctx context.Context, key Key) (io.ReadCloser, error) {
	s.mu.Lock()
	s.calls = append(s.calls, key)
	s.mu.Unlock()
	return s.fetch(ctx, key)
}

func (s *stubSource) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// stubBytes returns a fetch function that always serves the given bytes.
func stubBytes(data []byte) func(context.Context, Key) (io.ReadCloser, error) {
	return func(context.Context, Key) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	}
}

// stubError returns a fetch function that always returns err.
func stubError(err error) func(context.Context, Key) (io.ReadCloser, error) {
	return func(context.Context, Key) (io.ReadCloser, error) {
		return nil, err
	}
}

type failingStageCache struct{ MemoryCache }

func (c *failingStageCache) Stage(_ context.Context, _ Key) (CacheEntry, error) {
	return nil, errors.New("simulated cache stage failure")
}

type failingCommitCache struct{ MemoryCache }

func (c *failingCommitCache) Stage(_ context.Context, k Key) (CacheEntry, error) {
	return &failingCommitEntry{memoryCacheEntry: memoryCacheEntry{cache: &c.MemoryCache, key: k, buf: &bytes.Buffer{}}}, nil
}

type failingCommitEntry struct {
	memoryCacheEntry
}

func (e *failingCommitEntry) Commit() error {
	return errors.New("simulated cache commit failure")
}

// failingWriteCache wraps MemoryCache to produce entries whose Write returns an error after the first failAfter bytes.
type failingWriteCache struct {
	MemoryCache
	failAfter int
}

func (c *failingWriteCache) Stage(ctx context.Context, k Key) (CacheEntry, error) {
	inner, err := c.MemoryCache.Stage(ctx, k)
	if err != nil {
		return nil, err
	}
	return &failingWriteEntry{CacheEntry: inner, remaining: c.failAfter}, nil
}

type failingWriteEntry struct {
	CacheEntry
	remaining int
	failed    bool
}

func (e *failingWriteEntry) Write(p []byte) (int, error) {
	if e.failed {
		return 0, errors.New("simulated cache write failure")
	}
	if len(p) <= e.remaining {
		e.remaining -= len(p)
		return e.CacheEntry.Write(p)
	}
	n, _ := e.CacheEntry.Write(p[:e.remaining])
	e.remaining = 0
	e.failed = true
	return n, errors.New("simulated cache write failure")
}

// signalingCache wraps a Cache and reports the outcome of every staged entry on a channel.
// The reported value is true if the entry was committed, false if it was closed without commit.
// Tests use it to assert whether a staged entry was committed or discarded.
type signalingCache struct {
	Cache
	closed chan bool
}

func newSignalingCache(inner Cache) *signalingCache {
	return &signalingCache{Cache: inner, closed: make(chan bool, 16)}
}

func (c *signalingCache) Stage(ctx context.Context, k Key) (CacheEntry, error) {
	inner, err := c.Cache.Stage(ctx, k)
	if err != nil {
		return nil, err
	}
	return &signalingEntry{CacheEntry: inner, closed: c.closed}, nil
}

type signalingEntry struct {
	CacheEntry
	closed    chan<- bool
	committed bool
}

func (e *signalingEntry) Commit() error {
	err := e.CacheEntry.Commit()
	if err == nil {
		e.committed = true
	}
	return err
}

func (e *signalingEntry) Close() error {
	err := e.CacheEntry.Close()
	e.closed <- e.committed
	return err
}

// copyToCache copies all bytes from reader into a new cache entry under key.
// It runs the full Stage-Copy-Commit-Close lifecycle and aborts on any error.
func copyToCache(ctx context.Context, cache Cache, key Key, reader io.Reader) error {
	entry, err := cache.Stage(ctx, key)
	if err != nil {
		return err
	}
	defer entry.Close()
	if _, err := io.Copy(entry, reader); err != nil {
		return err
	}
	return entry.Commit()
}
