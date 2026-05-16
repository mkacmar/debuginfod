package debuginfod

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestClient_CacheHit(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, "data")
	}))
	defer srv.Close()

	client, err := NewClient(Options{
		ServerURLs: []string{srv.URL},
		Cache:      newMemCache(),
	})
	if err != nil {
		t.Fatal(err)
	}

	for i := range 2 {
		rc, err := client.FetchDebugInfo(context.Background(), testBuildID)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		rc.Close()
		if string(got) != "data" {
			t.Errorf("iteration %d: got %q", i, got)
		}
	}

	if n := hits.Load(); n != 1 {
		t.Errorf("expected 1 HTTP request (cache hit on second fetch), got %d", n)
	}
}

func TestClient_CacheCreateFailure(t *testing.T) {
	body := "data"
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	client, err := NewClient(Options{
		ServerURLs: []string{srv.URL},
		Cache:      &failingCreateCache{memCache: *newMemCache()},
	})
	if err != nil {
		t.Fatal(err)
	}

	rc, err := client.FetchDebugInfo(context.Background(), testBuildID)
	if err != nil {
		t.Fatalf("expected success despite cache failure, got %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("got %q, want %q", got, body)
	}
	if h := hits.Load(); h != 1 {
		t.Errorf("server hit %d times, want exactly 1", h)
	}
}

func TestClient_CacheCommitFailure(t *testing.T) {
	body := "data"
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	cache := &failingCommitCache{memCache: *newMemCache()}
	client, err := NewClient(Options{
		ServerURLs: []string{srv.URL},
		Cache:      cache,
	})
	if err != nil {
		t.Fatal(err)
	}

	rc, err := client.FetchDebugInfo(context.Background(), testBuildID)
	if err != nil {
		t.Fatalf("expected success despite commit failure, got %v", err)
	}

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("got %q, want %q", got, body)
	}
	if h := hits.Load(); h != 1 {
		t.Errorf("server hit %d times, want exactly 1", h)
	}

	if _, err := cache.Get(context.Background(), Key{BuildID: testBuildID, Kind: KindDebugInfo}); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected cache to be empty after commit failure, got err=%v", err)
	}
}

func TestClient_EarlyCloseLeavesCacheEmpty(t *testing.T) {
	body := "0123456789ABCDEF"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	cache := newMemCache()
	client, err := NewClient(Options{
		ServerURLs: []string{srv.URL},
		Cache:      cache,
	})
	if err != nil {
		t.Fatal(err)
	}

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

	deadline := time.Now().Add(2 * time.Second)
	for {
		_, err := cache.Get(context.Background(), Key{BuildID: testBuildID, Kind: KindDebugInfo})
		if errors.Is(err, ErrNotFound) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected cache empty after early Close, got err=%v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
