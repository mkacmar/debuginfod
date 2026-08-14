package cache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"go.kacmar.sk/debuginfod"
	"go.kacmar.sk/debuginfod/key"
)

const testBuildID = "cafebabedeadbeef0123456789abcdef00112233"

type stubFetcher struct {
	responses map[string][]byte
	metas     map[string]debuginfod.Metadata
	errs      map[string]error
	calls     []key.Key
}

func newStubFetcher() *stubFetcher {
	return &stubFetcher{
		responses: map[string][]byte{},
		metas:     map[string]debuginfod.Metadata{},
		errs:      map[string]error{},
	}
}

func (s *stubFetcher) Fetch(_ context.Context, k key.Key) (debuginfod.Response, error) {
	s.calls = append(s.calls, k)
	if err, ok := s.errs[k.String()]; ok {
		return debuginfod.Response{}, err
	}
	body, ok := s.responses[k.String()]
	if !ok {
		return debuginfod.Response{}, errors.New("stub: no response configured for " + k.String())
	}
	return debuginfod.Response{
		ReadCloser: io.NopCloser(bytes.NewReader(body)),
		Meta:       s.metas[k.String()],
	}, nil
}

func newTestCache(t *testing.T, fetcher Fetcher) (*DiskCache, string) {
	t.Helper()
	dir := t.TempDir()
	cache, err := NewDiskCache(DiskCacheOptions{Client: fetcher, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	return cache, dir
}

func TestNewDiskCache_RejectsMissingFields(t *testing.T) {
	cases := []struct {
		name string
		opts DiskCacheOptions
	}{
		{"missing Client", DiskCacheOptions{Dir: t.TempDir()}},
		{"missing Dir", DiskCacheOptions{Client: newStubFetcher()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewDiskCache(tc.opts); err == nil {
				t.Error("expected error")
			}
		})
	}
}

func TestDiskCache_Get_MissThenHit(t *testing.T) {
	fetcher := newStubFetcher()
	k := key.DebugInfo(testBuildID)
	payload := []byte("debuginfo bytes")
	fetcher.responses[k.String()] = payload

	cache, _ := newTestCache(t, fetcher)
	ctx := context.Background()

	first, err := cache.Get(ctx, k)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(first)
	first.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("miss open payload = %q, want %q", got, payload)
	}

	second, err := cache.Get(ctx, k)
	if err != nil {
		t.Fatal(err)
	}
	got, err = io.ReadAll(second)
	second.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("hit open payload = %q, want %q", got, payload)
	}
	if len(fetcher.calls) != 1 {
		t.Errorf("fetcher calls = %d, want 1 (second open should be a hit)", len(fetcher.calls))
	}
}

func TestDiskCache_Get_PropagatesFetcherError(t *testing.T) {
	fetcher := newStubFetcher()
	k := key.DebugInfo(testBuildID)
	sentinel := errors.New("upstream broken")
	fetcher.errs[k.String()] = sentinel

	cache, _ := newTestCache(t, fetcher)

	_, err := cache.Get(context.Background(), k)
	if !errors.Is(err, sentinel) {
		t.Fatalf("got error %v, want chain wrapping %v", err, sentinel)
	}
}

func TestDiskCache_Get_LeavesNoStagingFileOnFetchError(t *testing.T) {
	fetcher := newStubFetcher()
	k := key.DebugInfo(testBuildID)
	fetcher.errs[k.String()] = errors.New("fetch failure")

	cache, dir := newTestCache(t, fetcher)
	if _, err := cache.Get(context.Background(), k); err == nil {
		t.Fatal("expected fetch error")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("cache dir should be empty after failed fetch, got %v", entries)
	}
}

func TestDiskCache_CommittedFileIsReadOnly(t *testing.T) {
	fetcher := newStubFetcher()
	k := key.DebugInfo(testBuildID)
	fetcher.responses[k.String()] = []byte("x")

	cache, dir := newTestCache(t, fetcher)
	f, err := cache.Get(context.Background(), k)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	info, err := os.Stat(filepath.Join(dir, testBuildID, "debuginfo"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o400 {
		t.Errorf("committed file mode = %o, want 0400", got)
	}
}

func TestDiskCache_PathLayout(t *testing.T) {
	cache, _ := newTestCache(t, newStubFetcher())
	cases := []struct {
		name string
		key  key.Key
		want string
	}{
		{"DebugInfo", key.DebugInfo(testBuildID), filepath.Join(testBuildID, "debuginfo")},
		{"Executable", key.Executable(testBuildID), filepath.Join(testBuildID, "executable")},
		{"Section", key.Section(testBuildID, ".text"), filepath.Join(testBuildID, "section", ".text")},
		{"SectionEscapesSlash", key.Section(testBuildID, ".rela/.text"), filepath.Join(testBuildID, "section", ".rela%2F.text")},
		{"Source", key.Source(testBuildID, "/usr/src/main.c"), filepath.Join(testBuildID, "source", "usr", "src", "main.c")},
		{"SourceEscapesSpace", key.Source(testBuildID, "/a b/c.c"), filepath.Join(testBuildID, "source", "a%20b", "c.c")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cache.path(tc.key)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("path = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDiskCache_Delete_PrunesUpToFirstNonEmpty(t *testing.T) {
	sectionKey := key.Section(testBuildID, ".text")
	debugKey := key.DebugInfo(testBuildID)

	cases := []struct {
		name     string
		sibling  bool
		wantGone []string
		wantKept []string
	}{
		{
			name:     "NoSiblingPrunesAllParents",
			sibling:  false,
			wantGone: []string{filepath.Join(testBuildID, "section"), testBuildID},
		},
		{
			name:     "SiblingStopsPruning",
			sibling:  true,
			wantGone: []string{filepath.Join(testBuildID, "section")},
			wantKept: []string{filepath.Join(testBuildID, "debuginfo")},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fetcher := newStubFetcher()
			fetcher.responses[sectionKey.String()] = []byte("s")
			if tc.sibling {
				fetcher.responses[debugKey.String()] = []byte("d")
			}

			cache, dir := newTestCache(t, fetcher)
			ctx := context.Background()

			keys := []key.Key{sectionKey}
			if tc.sibling {
				keys = append(keys, debugKey)
			}
			for _, k := range keys {
				f, err := cache.Get(ctx, k)
				if err != nil {
					t.Fatal(err)
				}
				f.Close()
			}
			if err := cache.Delete(ctx, sectionKey); err != nil {
				t.Fatal(err)
			}

			for _, rel := range tc.wantGone {
				if _, err := os.Stat(filepath.Join(dir, rel)); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("%s should be gone, stat err = %v", rel, err)
				}
			}
			for _, rel := range tc.wantKept {
				if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
					t.Errorf("%s should still exist: %v", rel, err)
				}
			}
		})
	}
}

func TestDiskCache_Delete_MissingIsNoop(t *testing.T) {
	cache, _ := newTestCache(t, newStubFetcher())
	if err := cache.Delete(context.Background(), key.DebugInfo(testBuildID)); err != nil {
		t.Errorf("delete on missing entry should be a no-op, got %v", err)
	}
}

func TestDiskCache_RejectsInvalidKey(t *testing.T) {
	cache, _ := newTestCache(t, newStubFetcher())
	ctx := context.Background()
	var zero key.Key
	if _, err := cache.Get(ctx, zero); err == nil {
		t.Error("Get with zero key should fail validation")
	}
	if err := cache.Delete(ctx, zero); err == nil {
		t.Error("Delete with zero key should fail validation")
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

	cache, err := NewDiskCache(DiskCacheOptions{Client: newStubFetcher(), Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })

	f, err := cache.Get(context.Background(), key.DebugInfo(testBuildID))
	if err == nil {
		body, _ := io.ReadAll(f)
		f.Close()
		t.Fatalf("Get followed symlink out of cache root and returned %q", body)
	}
}

// assertMetaEqual compares every field, so a field added to Metadata is covered without touching this helper.
func assertMetaEqual(t *testing.T, label string, got, want debuginfod.Metadata) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s Meta = %+v, want %+v", label, got, want)
	}
}

// assertMetaFullyPopulated fails when a fixture leaves a Metadata field zero.
// It keeps the round-trip honest, so a field added to Metadata must be set here rather than escaping the sidecar tests unnoticed.
func assertMetaFullyPopulated(t *testing.T, meta debuginfod.Metadata) {
	t.Helper()
	v := reflect.ValueOf(meta)
	for i := range v.NumField() {
		if v.Field(i).IsZero() {
			t.Fatalf("fixture leaves Metadata.%s zero, set it so the sidecar round-trip covers every field", v.Type().Field(i).Name)
		}
	}
}

func TestDiskCache_Get_PersistsAndReadsMetadata(t *testing.T) {
	fetcher := newStubFetcher()
	k := key.DebugInfo(testBuildID)
	fetcher.responses[k.String()] = []byte("payload")
	meta := debuginfod.Metadata{Size: 7, File: "/lib/debug/x.debug", Archive: "/a.rpm", IMASignature: []byte{0xde, 0xad}}
	assertMetaFullyPopulated(t, meta)
	fetcher.metas[k.String()] = meta

	cache, dir := newTestCache(t, fetcher)
	ctx := context.Background()

	cold, err := cache.Get(ctx, k)
	if err != nil {
		t.Fatal(err)
	}
	cold.Close()
	assertMetaEqual(t, "cold", cold.Meta, meta)

	if _, err := os.Stat(filepath.Join(dir, testBuildID, metaDirName, "debuginfo")); err != nil {
		t.Errorf("sidecar not written to per-buildID .meta subdirectory: %v", err)
	}

	warm, err := cache.Get(ctx, k)
	if err != nil {
		t.Fatal(err)
	}
	warm.Close()
	assertMetaEqual(t, "warm", warm.Meta, meta)
	if len(fetcher.calls) != 1 {
		t.Errorf("fetcher calls = %d, want 1 (warm hit must not refetch)", len(fetcher.calls))
	}
}

// TestDiskCache_Get_MissingSidecarYieldsZeroMeta asserts that an entry cached before metadata support (body present, no sidecar) is a hit with zero Meta.
func TestDiskCache_Get_MissingSidecarYieldsZeroMeta(t *testing.T) {
	fetcher := newStubFetcher()
	k := key.DebugInfo(testBuildID)

	cache, dir := newTestCache(t, fetcher)

	if err := os.MkdirAll(filepath.Join(dir, testBuildID), cacheDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, testBuildID, "debuginfo"), []byte("legacy"), cacheFileMode); err != nil {
		t.Fatal(err)
	}

	entry, err := cache.Get(context.Background(), k)
	if err != nil {
		t.Fatal(err)
	}
	defer entry.Close()
	assertMetaEqual(t, "legacy entry", entry.Meta, debuginfod.Metadata{})
	if len(fetcher.calls) != 0 {
		t.Errorf("fetcher calls = %d, want 0 (legacy body is a hit)", len(fetcher.calls))
	}
}

// TestDiskCache_Get_CorruptSidecarYieldsZeroMeta asserts that an undecodable (corrupt JSON) sidecar degrades to zero Meta on a hit rather than failing the Get.
func TestDiskCache_Get_CorruptSidecarYieldsZeroMeta(t *testing.T) {
	fetcher := newStubFetcher()
	k := key.DebugInfo(testBuildID)

	cache, dir := newTestCache(t, fetcher)

	if err := os.MkdirAll(filepath.Join(dir, testBuildID), cacheDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, testBuildID, "debuginfo"), []byte("body"), cacheFileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, testBuildID, metaDirName), cacheDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, testBuildID, metaDirName, "debuginfo"), []byte("{not json"), cacheFileMode); err != nil {
		t.Fatal(err)
	}

	entry, err := cache.Get(context.Background(), k)
	if err != nil {
		t.Fatalf("corrupt sidecar should not fail Get: %v", err)
	}
	defer entry.Close()
	assertMetaEqual(t, "corrupt sidecar", entry.Meta, debuginfod.Metadata{})
	if len(fetcher.calls) != 0 {
		t.Errorf("fetcher calls = %d, want 0 (body is a hit despite corrupt sidecar)", len(fetcher.calls))
	}
}

func TestDiskCache_Delete_RemovesSidecarAndPrunesBuildIDDir(t *testing.T) {
	fetcher := newStubFetcher()
	k := key.Section(testBuildID, ".text")
	fetcher.responses[k.String()] = []byte("s")
	fetcher.metas[k.String()] = debuginfod.Metadata{Size: 1}

	cache, dir := newTestCache(t, fetcher)
	ctx := context.Background()

	entry, err := cache.Get(ctx, k)
	if err != nil {
		t.Fatal(err)
	}
	entry.Close()

	sidecar := filepath.Join(dir, testBuildID, metaDirName, "section", ".text")
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("sidecar should exist before delete: %v", err)
	}
	if err := cache.Delete(ctx, k); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sidecar); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("sidecar should be gone, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, testBuildID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("buildID dir should be pruned (body + metadata), stat err = %v", err)
	}
}

// TestDiskCache_SectionMetaSuffixNoCollision guards the per-buildID .meta layout.
// A section literally named ".text.meta" must not collide with the sidecar of section ".text".
func TestDiskCache_SectionMetaSuffixNoCollision(t *testing.T) {
	fetcher := newStubFetcher()
	plain := key.Section(testBuildID, ".text")
	suffixed := key.Section(testBuildID, ".text.meta")
	fetcher.responses[plain.String()] = []byte("plain-body")
	fetcher.responses[suffixed.String()] = []byte("suffixed-body")
	fetcher.metas[plain.String()] = debuginfod.Metadata{File: "plain"}
	fetcher.metas[suffixed.String()] = debuginfod.Metadata{File: "suffixed"}

	cache, _ := newTestCache(t, fetcher)
	ctx := context.Background()

	p, err := cache.Get(ctx, plain)
	if err != nil {
		t.Fatal(err)
	}
	pbody, _ := io.ReadAll(p)
	p.Close()

	s, err := cache.Get(ctx, suffixed)
	if err != nil {
		t.Fatal(err)
	}
	sbody, _ := io.ReadAll(s)
	s.Close()

	if string(pbody) != "plain-body" || p.Meta.File != "plain" {
		t.Errorf("plain section corrupted: body=%q meta=%+v", pbody, p.Meta)
	}
	if string(sbody) != "suffixed-body" || s.Meta.File != "suffixed" {
		t.Errorf("suffixed section corrupted: body=%q meta=%+v", sbody, s.Meta)
	}
}
