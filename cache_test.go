package debuginfod

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewDiskCache_RejectEmptyDir(t *testing.T) {
	_, err := NewDiskCache(DiskCacheOptions{})
	if err == nil {
		t.Error("expected error for empty dir")
	}
}

func TestDiskCache_CreateGet(t *testing.T) {
	cache := newTestDiskCache(t)
	ctx := context.Background()
	key := Key{BuildID: "abcdef1234567890", Kind: KindDebugInfo}
	data := []byte("ELF debug data here")

	if err := putReader(ctx, cache, key, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}

	rc, err := cache.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(got, data) {
		t.Errorf("got %q, want %q", got, data)
	}
}

func TestDiskCache_DoubleCommit(t *testing.T) {
	cache := newTestDiskCache(t)
	ctx := context.Background()
	key := Key{BuildID: "aabbccdd", Kind: KindDebugInfo}

	e, err := cache.Create(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, err := e.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	if err := e.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := e.Commit(); !errors.Is(err, ErrAlreadyCommitted) {
		t.Errorf("second Commit got %v, want ErrAlreadyCommitted", err)
	}
}

func TestDiskCache_GetMissing(t *testing.T) {
	cache := newTestDiskCache(t)

	rc, err := cache.Get(context.Background(), Key{BuildID: "nonexistent", Kind: KindDebugInfo})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
	if rc != nil {
		rc.Close()
		t.Error("expected nil ReadCloser for missing key")
	}
}

func TestDiskCache_Delete(t *testing.T) {
	cache := newTestDiskCache(t)
	ctx := context.Background()
	key := Key{BuildID: "abcdef1234567890", Kind: KindDebugInfo}

	if err := putReader(ctx, cache, key, bytes.NewReader([]byte("data"))); err != nil {
		t.Fatal(err)
	}

	if err := cache.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}

	got, err := cache.Get(ctx, key)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
	if got != nil {
		got.Close()
		t.Error("expected nil ReadCloser after delete")
	}
}

func TestDiskCache_DeleteMissing(t *testing.T) {
	cache := newTestDiskCache(t)

	if err := cache.Delete(context.Background(), Key{BuildID: "nonexistent", Kind: KindDebugInfo}); err != nil {
		t.Errorf("delete of missing key should return nil, got %v", err)
	}
}

func TestDiskCache_CommittedEntryIsReadOnly(t *testing.T) {
	cache := newTestDiskCache(t)
	ctx := context.Background()
	key := Key{BuildID: "aabbccdd", Kind: KindDebugInfo}

	if err := putReader(ctx, cache, key, bytes.NewReader([]byte("data"))); err != nil {
		t.Fatal(err)
	}

	p, err := cache.path(key)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o400 {
		t.Errorf("cached file mode = %o, want 0400", got)
	}
}

func TestDiskCache_RejectsTraversal(t *testing.T) {
	cache := newTestDiskCache(t)
	ctx := context.Background()

	for _, qualifier := range []string{"/../etc/passwd", "/usr/../../../escape", "/./still/bad/.."} {
		key := Key{BuildID: "aabbccdd", Kind: KindSource, Qualifier: qualifier}
		if err := putReader(ctx, cache, key, bytes.NewReader([]byte("x"))); err == nil {
			t.Errorf("Create(%q) should have failed", qualifier)
		}
	}

	parent := filepath.Dir(cache.dir)
	if entries, _ := os.ReadDir(parent); len(entries) > 1 {
		for _, e := range entries {
			if e.Name() != filepath.Base(cache.dir) {
				t.Errorf("traversal escaped cache dir: %s", filepath.Join(parent, e.Name()))
			}
		}
	}
}

func TestDiskCache_RejectsBadKey(t *testing.T) {
	cache := newTestDiskCache(t)
	ctx := context.Background()

	cases := []struct {
		name string
		key  Key
	}{
		{"EmptyBuildID", Key{Kind: KindDebugInfo}},
		{"QualifierOnDebugInfo", Key{BuildID: "aabbccdd", Kind: KindDebugInfo, Qualifier: "x"}},
		{"QualifierOnExecutable", Key{BuildID: "aabbccdd", Kind: KindExecutable, Qualifier: "x"}},
		{"MissingSourceQualifier", Key{BuildID: "aabbccdd", Kind: KindSource}},
		{"MissingSectionQualifier", Key{BuildID: "aabbccdd", Kind: KindSection}},
		{"UnknownKind", Key{BuildID: "aabbccdd", Kind: ArtifactKind(99)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := putReader(ctx, cache, tc.key, bytes.NewReader([]byte("x"))); err == nil {
				t.Errorf("Create with %s should have failed", tc.name)
			}
		})
	}
}

func TestClient_FetchWithoutCache(t *testing.T) {
	body := "streamed data"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	client, err := NewClient(Options{ServerURLs: []string{srv.URL}})
	if err != nil {
		t.Fatal(err)
	}

	rc, err := client.FetchDebugInfo(context.Background(), "aabbccdd")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("got %q, want %q", got, body)
	}
}

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
		rc, err := client.FetchDebugInfo(context.Background(), "aabbccdd")
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

	rc, err := client.FetchDebugInfo(context.Background(), "aabbccdd")
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

	rc, err := client.FetchDebugInfo(context.Background(), "aabbccdd")
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

	if _, err := cache.Get(context.Background(), Key{BuildID: "aabbccdd", Kind: KindDebugInfo}); !errors.Is(err, ErrNotFound) {
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

	rc, err := client.FetchDebugInfo(context.Background(), "aabbccdd")
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
		_, err := cache.Get(context.Background(), Key{BuildID: "aabbccdd", Kind: KindDebugInfo})
		if errors.Is(err, ErrNotFound) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected cache empty after early Close, got err=%v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func newTestDiskCache(t *testing.T) *DiskCache {
	t.Helper()
	cache, err := NewDiskCache(DiskCacheOptions{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return cache
}
