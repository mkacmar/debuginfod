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

func TestNewDiskCache_RejectEmptyDir(t *testing.T) {
	_, err := NewDiskCache(DiskCacheOptions{})
	if err == nil {
		t.Error("expected error for empty dir")
	}
}

func TestDiskCache_CreateGet(t *testing.T) {
	cache := newTestDiskCache(t)
	ctx := context.Background()
	key := Key{BuildID: testBuildID, Kind: KindDebugInfo}
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

func TestDiskCache_GetMissing(t *testing.T) {
	cache := newTestDiskCache(t)

	rc, err := cache.Get(context.Background(), Key{BuildID: testBuildID, Kind: KindDebugInfo})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
	if rc != nil {
		rc.Close()
		t.Error("expected nil ReadCloser for missing key")
	}
}

func TestDiskCache_DoubleCommit(t *testing.T) {
	cache := newTestDiskCache(t)
	ctx := context.Background()
	key := Key{BuildID: testBuildID, Kind: KindDebugInfo}

	entry, err := cache.Create(ctx, key)
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
}

func TestDiskCache_CommittedEntryIsReadOnly(t *testing.T) {
	cache := newTestDiskCache(t)
	ctx := context.Background()
	key := Key{BuildID: testBuildID, Kind: KindDebugInfo}

	if err := putReader(ctx, cache, key, bytes.NewReader([]byte("data"))); err != nil {
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

func TestDiskCache_Delete(t *testing.T) {
	cache := newTestDiskCache(t)
	ctx := context.Background()
	key := Key{BuildID: testBuildID, Kind: KindDebugInfo}

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

	if err := cache.Delete(context.Background(), Key{BuildID: testBuildID, Kind: KindDebugInfo}); err != nil {
		t.Errorf("delete of missing key should return nil, got %v", err)
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
		{"SlashSectionQualifier", Key{BuildID: testBuildID, Kind: KindSection, Qualifier: "foo/bar"}},
		{"NulSectionQualifier", Key{BuildID: testBuildID, Kind: KindSection, Qualifier: "foo\x00bar"}},
		{"UnknownKind", Key{BuildID: testBuildID, Kind: ArtifactKind(99)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := putReader(ctx, cache, tc.key, bytes.NewReader([]byte("x"))); err == nil {
				t.Errorf("Create with %s should have failed", tc.name)
			}
			if _, err := cache.Get(ctx, tc.key); err == nil {
				t.Errorf("Get with %s should have failed", tc.name)
			}
			if err := cache.Delete(ctx, tc.key); err == nil {
				t.Errorf("Delete with %s should have failed", tc.name)
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

	rc, err := cache.Get(context.Background(), Key{BuildID: testBuildID, Kind: KindDebugInfo})
	if err == nil {
		body, _ := io.ReadAll(rc)
		rc.Close()
		t.Fatalf("Get followed symlink out of cache root and returned %q", body)
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
