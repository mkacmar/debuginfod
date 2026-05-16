package debuginfod

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// Pipeline tests exercise the assembled Client end-to-end against real httptest servers.
// Per-decorator unit tests live alongside each decorator file, see chain_test.go and friends.

func TestPipeline_FetchSucceeds(t *testing.T) {
	body := "data"
	srv := staticServer(t, body)

	client := mustNewClient(t, Options{ServerURLs: []string{srv.URL}})

	rc, err := client.FetchDebugInfo(context.Background(), testBuildID)
	if err != nil {
		t.Fatal(err)
	}
	if got := readAndClose(t, rc); string(got) != body {
		t.Errorf("got %q, want %q", got, body)
	}
}

func TestPipeline_ContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	client := mustNewClient(t, Options{ServerURLs: []string{srv.URL}})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := client.FetchDebugInfo(ctx, testBuildID); err == nil {
		t.Error("expected error from cancelled context")
	}
}

func TestPipeline_CacheHit(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("data"))
	}))
	defer srv.Close()

	client := mustNewClient(t, Options{
		ServerURLs: []string{srv.URL},
		Cache:      NewMemoryCache(),
	})

	for i := range 2 {
		rc, err := client.FetchDebugInfo(context.Background(), testBuildID)
		if err != nil {
			t.Fatal(err)
		}
		if got := readAndClose(t, rc); string(got) != "data" {
			t.Errorf("iteration %d: got %q", i, got)
		}
	}

	if n := hits.Load(); n != 1 {
		t.Errorf("expected 1 HTTP request (cache hit on second fetch), got %d", n)
	}
}

func TestPipeline_EarlyCloseLeavesCacheEmpty(t *testing.T) {
	body := "0123456789ABCDEF"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	cache := NewMemoryCache()
	client := mustNewClient(t, Options{
		ServerURLs: []string{srv.URL},
		Cache:      cache,
	})

	rc, err := client.FetchDebugInfo(context.Background(), testBuildID)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(rc, buf); err != nil {
		t.Fatalf("read partial: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := cache.Fetch(context.Background(), testKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cache should be empty immediately after early Close, got err=%v", err)
	}
}

// Section endpoint miss returns ErrNotFound with no further library action.
// The library never escalates a section request into a debuginfo download, that policy belongs to the caller.
func TestPipeline_FetchSectionOnlyHitsSectionEndpoint(t *testing.T) {
	const sectionName = ".text"

	var sectionHits, debugInfoHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/buildid/" + testBuildID + "/section/" + sectionName:
			sectionHits.Add(1)
			http.NotFound(w, r)
		case "/buildid/" + testBuildID + "/debuginfo":
			debugInfoHits.Add(1)
			http.NotFound(w, r)
		default:
			t.Errorf("unexpected request path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cache := NewMemoryCache()
	client := mustNewClient(t, Options{ServerURLs: []string{srv.URL}, Cache: cache})

	_, err := client.FetchSection(context.Background(), testBuildID, sectionName)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("FetchSection err = %v, want ErrNotFound", err)
	}
	if sectionHits.Load() != 1 {
		t.Errorf("section endpoint hits = %d, want 1", sectionHits.Load())
	}
	if debugInfoHits.Load() != 0 {
		t.Errorf("debuginfo endpoint hits = %d, want 0 (library must not auto-fetch debuginfo)", debugInfoHits.Load())
	}
}
