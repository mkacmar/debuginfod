package debuginfod

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
)

func TestMemoryCache_Conformance(t *testing.T) {
	testCacheConformance(t, func(*testing.T) Cache { return NewMemoryCache() })
}

func TestMemoryCache_ConcurrentStageDifferentKeys(t *testing.T) {
	cache := NewMemoryCache()
	ctx := context.Background()
	const n = 32

	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			key := Key{BuildID: testBuildID, Kind: KindSection, Qualifier: string(rune('a' + i))}
			payload := []byte{byte(i)}
			if err := copyToCache(ctx, cache, key, bytes.NewReader(payload)); err != nil {
				t.Errorf("stage %d: %v", i, err)
			}
		})
	}
	wg.Wait()

	for i := range n {
		key := Key{BuildID: testBuildID, Kind: KindSection, Qualifier: string(rune('a' + i))}
		rc, err := cache.Fetch(ctx, key)
		if err != nil {
			t.Errorf("fetch %d: %v", i, err)
			continue
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if len(got) != 1 || got[0] != byte(i) {
			t.Errorf("entry %d = %v, want [%d]", i, got, i)
		}
	}
}
