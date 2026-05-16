package debuginfod

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestClient_FetchSection_LocalSliceFromNonSeekableCache(t *testing.T) {
	const sectionName = ".text"
	sectionContent := []byte("section payload")
	debugInfo := makeMinimalELF(sectionName, sectionContent)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected network request: %s", r.URL.Path)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	cache := newMemCache()
	ctx := context.Background()
	if err := putReader(ctx, cache, Key{BuildID: testBuildID, Kind: KindDebugInfo}, bytes.NewReader(debugInfo)); err != nil {
		t.Fatal(err)
	}

	client, err := NewClient(Options{
		ServerURLs: []string{srv.URL},
		Cache:      cache,
	})
	if err != nil {
		t.Fatal(err)
	}

	rc, err := client.FetchSection(ctx, testBuildID, sectionName)
	if err != nil {
		t.Fatalf("FetchSection: %v", err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if !bytes.Equal(got, sectionContent) {
		t.Errorf("section content = %q, want %q", got, sectionContent)
	}
}

func TestClient_FetchSection_EvictsCorruptCachedDebugInfo(t *testing.T) {
	const sectionName = ".text"
	sectionContent := []byte("section payload")
	debugInfo := makeMinimalELF(sectionName, sectionContent)

	srv := fallbackServer(t, sectionName, debugInfo, nil, nil)
	defer srv.Close()

	cache := newTestDiskCache(t)
	ctx := context.Background()

	debugKey := Key{BuildID: testBuildID, Kind: KindDebugInfo}
	if err := putReader(ctx, cache, debugKey, bytes.NewReader([]byte("not an elf file"))); err != nil {
		t.Fatal(err)
	}

	client, err := NewClient(Options{
		ServerURLs: []string{srv.URL},
		Cache:      cache,
	})
	if err != nil {
		t.Fatal(err)
	}

	rc, err := client.FetchSection(ctx, testBuildID, sectionName)
	if err != nil {
		t.Fatalf("FetchSection: %v", err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if !bytes.Equal(got, sectionContent) {
		t.Errorf("section content = %q, want %q", got, sectionContent)
	}

	cachedRC, err := cache.Get(ctx, debugKey)
	if err != nil {
		t.Fatalf("debuginfo not cached after refetch: %v", err)
	}
	cached, err := io.ReadAll(cachedRC)
	if err != nil {
		t.Fatal(err)
	}
	cachedRC.Close()
	if !bytes.Equal(cached, debugInfo) {
		t.Errorf("cached debuginfo not replaced with valid bytes (got %d bytes, want %d)", len(cached), len(debugInfo))
	}
}

func TestClient_FetchSection_FallsBackToFullDebugInfo(t *testing.T) {
	const sectionName = ".text"
	sectionContent := []byte("section payload")
	debugInfo := makeMinimalELF(sectionName, sectionContent)

	var sectionHits, debugInfoHits atomic.Int32
	srv := fallbackServer(t, sectionName, debugInfo, &sectionHits, &debugInfoHits)
	defer srv.Close()

	cache := newMemCache()
	client, err := NewClient(Options{
		ServerURLs: []string{srv.URL},
		Cache:      cache,
	})
	if err != nil {
		t.Fatal(err)
	}

	rc, err := client.FetchSection(context.Background(), testBuildID, sectionName)
	if err != nil {
		t.Fatalf("FetchSection: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()

	if !bytes.Equal(got, sectionContent) {
		t.Errorf("section content = %q, want %q", got, sectionContent)
	}
	if sectionHits.Load() != 1 {
		t.Errorf("section endpoint hits = %d, want 1", sectionHits.Load())
	}
	if debugInfoHits.Load() != 1 {
		t.Errorf("debuginfo endpoint hits = %d, want 1", debugInfoHits.Load())
	}

	// Section should be cached for next time.
	sectionKey := Key{BuildID: testBuildID, Kind: KindSection, Qualifier: sectionName}
	cachedRC, err := cache.Get(context.Background(), sectionKey)
	if err != nil {
		t.Fatalf("section not cached: %v", err)
	}
	cached, _ := io.ReadAll(cachedRC)
	cachedRC.Close()
	if !bytes.Equal(cached, sectionContent) {
		t.Errorf("cached section = %q, want %q", cached, sectionContent)
	}
}

func TestClient_FetchSection_FallbackMissingSection(t *testing.T) {
	debugInfo := makeMinimalELF(".other", []byte("payload"))
	srv := fallbackServer(t, ".missing", debugInfo, nil, nil)
	defer srv.Close()

	client, err := NewClient(Options{
		ServerURLs: []string{srv.URL},
		Cache:      newMemCache(),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.FetchSection(context.Background(), testBuildID, ".missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestClient_FetchSection_FallbackWithoutCache(t *testing.T) {
	const sectionName = ".text"
	sectionContent := []byte("payload")
	debugInfo := makeMinimalELF(sectionName, sectionContent)

	srv := fallbackServer(t, sectionName, debugInfo, nil, nil)
	defer srv.Close()

	client, err := NewClient(Options{ServerURLs: []string{srv.URL}})
	if err != nil {
		t.Fatal(err)
	}

	rc, err := client.FetchSection(context.Background(), testBuildID, sectionName)
	if err != nil {
		t.Fatalf("FetchSection: %v", err)
	}
	defer rc.Close()

	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, sectionContent) {
		t.Errorf("section content = %q, want %q", got, sectionContent)
	}
}

// fallbackServer serves debugInfo at /debuginfo and 404s the /section/ endpoint, simulating a server without section support.
// sectionHits and debugInfoHits, if non-nil, count requests to each endpoint.
func fallbackServer(t *testing.T, sectionName string, debugInfo []byte, sectionHits, debugInfoHits *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/buildid/" + testBuildID + "/section/" + sectionName:
			if sectionHits != nil {
				sectionHits.Add(1)
			}
			http.NotFound(w, r)
		case "/buildid/" + testBuildID + "/debuginfo":
			if debugInfoHits != nil {
				debugInfoHits.Add(1)
			}
			w.Write(debugInfo)
		default:
			t.Errorf("unexpected request path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
}
