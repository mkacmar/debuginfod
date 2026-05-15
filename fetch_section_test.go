package debuginfod

import (
	"bytes"
	"context"
	"debug/elf"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

const testBuildID = "aabbccdd"

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

// makeMinimalELF builds a minimal valid little-endian ELF64 file containing a single named section.
// Layout: [ELF header][section data][.shstrtab][section header table].
func makeMinimalELF(sectionName string, data []byte) []byte {
	// Section name table: leading NUL, then each name NUL-terminated.
	names := []byte{0}
	sectionNameOff := uint32(len(names))
	names = append(append(names, sectionName...), 0)
	shstrtabNameOff := uint32(len(names))
	names = append(append(names, ".shstrtab"...), 0)

	const headerSize = 64
	const sectionHeaderSize = 64
	dataOff := uint64(headerSize)
	namesOff := dataOff + uint64(len(data))
	sectionHeadersOff := namesOff + uint64(len(names))

	header := elf.Header64{
		Ident: [16]byte{
			0x7f, 'E', 'L', 'F',
			byte(elf.ELFCLASS64),
			byte(elf.ELFDATA2LSB),
			byte(elf.EV_CURRENT),
		},
		Type:      uint16(elf.ET_EXEC),
		Machine:   uint16(elf.EM_X86_64),
		Version:   uint32(elf.EV_CURRENT),
		Shoff:     sectionHeadersOff,
		Ehsize:    headerSize,
		Shentsize: sectionHeaderSize,
		Shnum:     3, // NULL + named section + .shstrtab
		Shstrndx:  2, // index of .shstrtab in the section header table
	}

	sections := []elf.Section64{
		{}, // SHN_UNDEF
		{
			Name:      sectionNameOff,
			Type:      uint32(elf.SHT_PROGBITS),
			Off:       dataOff,
			Size:      uint64(len(data)),
			Addralign: 1,
		},
		{
			Name:      shstrtabNameOff,
			Type:      uint32(elf.SHT_STRTAB),
			Off:       namesOff,
			Size:      uint64(len(names)),
			Addralign: 1,
		},
	}

	var buf bytes.Buffer
	mustWrite(&buf, &header)
	buf.Write(data)
	buf.Write(names)
	for i := range sections {
		mustWrite(&buf, &sections[i])
	}
	return buf.Bytes()
}

func mustWrite(buf *bytes.Buffer, v any) {
	if err := binary.Write(buf, binary.LittleEndian, v); err != nil {
		panic(fmt.Sprintf("test setup: binary.Write: %v", err))
	}
}
