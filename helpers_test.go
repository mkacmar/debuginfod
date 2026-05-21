package debuginfod

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"go.kacmar.sk/debuginfod/key"
)

const testBuildID = "cafebabedeadbeef0123456789abcdef00112233"

var (
	testKey     = key.DebugInfo(testBuildID)
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
	fetch func(ctx context.Context, k key.Key) (io.ReadCloser, error)

	mu    sync.Mutex
	calls []key.Key
}

func newStubSource(fetch func(ctx context.Context, k key.Key) (io.ReadCloser, error)) *stubSource {
	return &stubSource{fetch: fetch}
}

func (s *stubSource) Fetch(ctx context.Context, k key.Key) (io.ReadCloser, error) {
	s.mu.Lock()
	s.calls = append(s.calls, k)
	s.mu.Unlock()
	return s.fetch(ctx, k)
}

func (s *stubSource) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// stubBytes returns a fetch function that always serves the given bytes.
func stubBytes(data []byte) func(context.Context, key.Key) (io.ReadCloser, error) {
	return func(context.Context, key.Key) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	}
}

// stubError returns a fetch function that always returns err.
func stubError(err error) func(context.Context, key.Key) (io.ReadCloser, error) {
	return func(context.Context, key.Key) (io.ReadCloser, error) {
		return nil, err
	}
}
