package debuginfod

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestDiskCache_Conformance(t *testing.T) {
	testCacheConformance(t, func(t *testing.T) Cache { return newTestDiskCache(t) })
}

func TestDiskCache_RejectsEmptyDir(t *testing.T) {
	_, err := NewDiskCache(DiskCacheOptions{})
	if err == nil {
		t.Error("expected error for empty dir")
	}
}

func TestDiskCache_CommittedEntryIsReadOnly(t *testing.T) {
	cache := newTestDiskCache(t)
	ctx := context.Background()
	key := Key{BuildID: testBuildID, Kind: KindDebugInfo}

	if err := copyToCache(ctx, cache, key, bytes.NewReader([]byte("data"))); err != nil {
		t.Fatal(err)
	}

	relPath, err := cache.path(key)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(cache.root.Name(), relPath))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o400 {
		t.Errorf("cached file mode = %o, want 0400", got)
	}
}

func TestDiskCache_EvictPrunesEmptyParents(t *testing.T) {
	dir := t.TempDir()
	cache, err := NewDiskCache(DiskCacheOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })

	ctx := context.Background()
	key := Key{BuildID: testBuildID, Kind: KindSection, Qualifier: ".text"}

	if err := copyToCache(ctx, cache, key, bytes.NewReader([]byte("data"))); err != nil {
		t.Fatal(err)
	}
	if err := cache.Evict(ctx, key); err != nil {
		t.Fatal(err)
	}

	// Both <buildID>/section/ and <buildID>/ should be gone now that they're empty.
	for _, rel := range []string{filepath.Join(testBuildID, "section"), testBuildID} {
		if _, err := os.Stat(filepath.Join(dir, rel)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("directory %s should have been pruned, stat err = %v", rel, err)
		}
	}
}

func TestDiskCache_EvictStopsAtNonEmptyParent(t *testing.T) {
	dir := t.TempDir()
	cache, err := NewDiskCache(DiskCacheOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })

	ctx := context.Background()
	sectionKey := Key{BuildID: testBuildID, Kind: KindSection, Qualifier: ".text"}
	debugKey := Key{BuildID: testBuildID, Kind: KindDebugInfo}

	if err := copyToCache(ctx, cache, sectionKey, bytes.NewReader([]byte("s"))); err != nil {
		t.Fatal(err)
	}
	if err := copyToCache(ctx, cache, debugKey, bytes.NewReader([]byte("d"))); err != nil {
		t.Fatal(err)
	}

	if err := cache.Evict(ctx, sectionKey); err != nil {
		t.Fatal(err)
	}

	// The section subdir is gone, but the buildID dir survives, the debuginfo file sits in it.
	if _, err := os.Stat(filepath.Join(dir, testBuildID, "section")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("section dir should be gone, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, testBuildID, "debuginfo")); err != nil {
		t.Errorf("debuginfo file should still exist: %v", err)
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
		{"OddLengthBuildID", Key{BuildID: "abc", Kind: KindDebugInfo}},
		{"NonHexBuildID", Key{BuildID: "zzzz", Kind: KindDebugInfo}},
		{"TraversalBuildID", Key{BuildID: "../../etc", Kind: KindDebugInfo}},
		{"SlashBuildID", Key{BuildID: "aa/bb", Kind: KindDebugInfo}},
		{"QualifierOnDebugInfo", Key{BuildID: testBuildID, Kind: KindDebugInfo, Qualifier: "x"}},
		{"QualifierOnExecutable", Key{BuildID: testBuildID, Kind: KindExecutable, Qualifier: "x"}},
		{"MissingSourceQualifier", Key{BuildID: testBuildID, Kind: KindSource}},
		{"TraversalSourceQualifier", Key{BuildID: testBuildID, Kind: KindSource, Qualifier: "/../etc/passwd"}},
		{"DeepTraversalSourceQualifier", Key{BuildID: testBuildID, Kind: KindSource, Qualifier: "/usr/../../../escape"}},
		{"DotDotSourceQualifier", Key{BuildID: testBuildID, Kind: KindSource, Qualifier: "/./still/bad/.."}},
		{"MissingSectionQualifier", Key{BuildID: testBuildID, Kind: KindSection}},
		{"DotSectionQualifier", Key{BuildID: testBuildID, Kind: KindSection, Qualifier: "."}},
		{"DotDotSectionQualifier", Key{BuildID: testBuildID, Kind: KindSection, Qualifier: ".."}},
		{"NulSectionQualifier", Key{BuildID: testBuildID, Kind: KindSection, Qualifier: "foo\x00bar"}},
		{"UnknownKind", Key{BuildID: testBuildID, Kind: ArtifactKind(99)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := copyToCache(ctx, cache, tc.key, bytes.NewReader([]byte("x"))); err == nil {
				t.Errorf("Stage with %s should have failed", tc.name)
			}
			if _, err := cache.Fetch(ctx, tc.key); err == nil {
				t.Errorf("Fetch with %s should have failed", tc.name)
			}
			if err := cache.Evict(ctx, tc.key); err == nil {
				t.Errorf("Evict with %s should have failed", tc.name)
			}
		})
	}
}

func TestDiskCache_RejectsSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "secret")
	if err := os.WriteFile(secret, []byte("top secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Mkdir(filepath.Join(dir, testBuildID), cacheDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, testBuildID, "debuginfo")); err != nil {
		t.Fatal(err)
	}

	cache, err := NewDiskCache(DiskCacheOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })

	rc, err := cache.Fetch(context.Background(), Key{BuildID: testBuildID, Kind: KindDebugInfo})
	if err == nil {
		body, _ := io.ReadAll(rc)
		rc.Close()
		t.Fatalf("Fetch followed symlink out of cache root and returned %q", body)
	}
}

func newTestDiskCache(t *testing.T) *DiskCache {
	t.Helper()
	cache, err := NewDiskCache(DiskCacheOptions{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	return cache
}
