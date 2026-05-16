package debuginfod

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

// testCacheConformance exercises the Cache interface contract.
// Each subtest gets a fresh cache from newCache.
func testCacheConformance(t *testing.T, newCache func(t *testing.T) Cache) {
	t.Helper()

	key := Key{BuildID: testBuildID, Kind: KindDebugInfo}

	t.Run("StageFetchRoundTrip", func(t *testing.T) {
		cache := newCache(t)
		ctx := context.Background()
		data := []byte("ELF debug data")

		if err := copyToCache(ctx, cache, key, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}

		rc, err := cache.Fetch(ctx, key)
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
	})

	t.Run("FetchMissing", func(t *testing.T) {
		cache := newCache(t)
		rc, err := cache.Fetch(context.Background(), key)
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("expected ErrNotFound, got %v", err)
		}
		if rc != nil {
			rc.Close()
			t.Error("expected nil ReadCloser for missing key")
		}
	})

	t.Run("DoubleCommit", func(t *testing.T) {
		cache := newCache(t)
		ctx := context.Background()

		entry, err := cache.Stage(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		defer entry.Close()
		if _, err := entry.Write([]byte("data")); err != nil {
			t.Fatal(err)
		}
		if err := entry.Commit(); err != nil {
			t.Fatal(err)
		}
		if err := entry.Commit(); !errors.Is(err, ErrAlreadyCommitted) {
			t.Errorf("second Commit got %v, want ErrAlreadyCommitted", err)
		}
	})

	t.Run("CloseWithoutCommitDiscards", func(t *testing.T) {
		cache := newCache(t)
		ctx := context.Background()

		entry, err := cache.Stage(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte("data")); err != nil {
			t.Fatal(err)
		}
		if err := entry.Close(); err != nil {
			t.Fatal(err)
		}

		if _, err := cache.Fetch(ctx, key); !errors.Is(err, ErrNotFound) {
			t.Errorf("expected ErrNotFound after Close-without-Commit, got %v", err)
		}
	})

	t.Run("Evict", func(t *testing.T) {
		cache := newCache(t)
		ctx := context.Background()

		if err := copyToCache(ctx, cache, key, bytes.NewReader([]byte("data"))); err != nil {
			t.Fatal(err)
		}
		if err := cache.Evict(ctx, key); err != nil {
			t.Fatal(err)
		}
		rc, err := cache.Fetch(ctx, key)
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("expected ErrNotFound, got %v", err)
		}
		if rc != nil {
			rc.Close()
			t.Error("expected nil ReadCloser after evict")
		}
	})

	t.Run("EvictMissingIsNoOp", func(t *testing.T) {
		cache := newCache(t)
		if err := cache.Evict(context.Background(), key); err != nil {
			t.Errorf("evict of missing key should return nil, got %v", err)
		}
	})
}
