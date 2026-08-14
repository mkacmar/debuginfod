package debuginfod

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.kacmar.sk/debuginfod/key"
)

func newTestUpstream(serverURL string) *upstream {
	return &upstream{
		serverURL:  serverURL,
		httpClient: http.DefaultClient,
		userAgent:  "test-agent",
	}
}

func TestUpstream_SuccessReturnsBody(t *testing.T) {
	body := "the bytes"
	srv := staticServer(t, body)

	src := newTestUpstream(srv.URL)
	rc, _, err := src.Fetch(context.Background(), testKey)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := readAndClose(t, rc); string(got) != body {
		t.Errorf("body = %q, want %q", got, body)
	}
}

func TestUpstream_ParsesMetadataHeaders(t *testing.T) {
	imaHex := "deadbeef"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-DEBUGINFOD-SIZE", "12345")
		w.Header().Set("X-DEBUGINFOD-FILE", "/usr/lib/debug/ls.debug")
		w.Header().Set("X-DEBUGINFOD-ARCHIVE", "/path/coreutils.rpm")
		w.Header().Set("X-DEBUGINFOD-IMASIGNATURE", imaHex)
		fmt.Fprint(w, "body")
	}))
	defer srv.Close()

	src := newTestUpstream(srv.URL)
	rc, meta, err := src.Fetch(context.Background(), testKey)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	readAndClose(t, rc)

	if meta.Size != 12345 {
		t.Errorf("Size = %d, want 12345", meta.Size)
	}
	if meta.File != "/usr/lib/debug/ls.debug" {
		t.Errorf("File = %q, want /usr/lib/debug/ls.debug", meta.File)
	}
	if meta.Archive != "/path/coreutils.rpm" {
		t.Errorf("Archive = %q, want /path/coreutils.rpm", meta.Archive)
	}
	if got := hex.EncodeToString(meta.IMASignature); got != imaHex {
		t.Errorf("IMASignature = %q, want %q", got, imaHex)
	}
}

func TestUpstream_MetadataBestEffort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-DEBUGINFOD-SIZE", "not-a-number")
		w.Header().Set("X-DEBUGINFOD-IMASIGNATURE", "zzzz")
		fmt.Fprint(w, "body")
	}))
	defer srv.Close()

	src := newTestUpstream(srv.URL)
	rc, meta, err := src.Fetch(context.Background(), testKey)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	readAndClose(t, rc)

	if meta.Size != 0 || meta.File != "" || meta.Archive != "" || meta.IMASignature != nil {
		t.Errorf("malformed/absent headers should yield zero Metadata, got %+v", meta)
	}
}

func TestUpstream_SendsUserAgent(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	src := &upstream{serverURL: srv.URL, httpClient: http.DefaultClient, userAgent: "my-agent/1.0"}
	rc, _, err := src.Fetch(context.Background(), testKey)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	rc.Close()

	if gotUA != "my-agent/1.0" {
		t.Errorf("User-Agent = %q, want %q", gotUA, "my-agent/1.0")
	}
}

// TestUpstream_StatusMapping asserts how upstream translates upstream status codes into errors.
// 401/403 produce ErrAuthRequired, other 4xx produce ErrNotFound, 5xx/429 produce transport errors that engage the retry layer.
func TestUpstream_StatusMapping(t *testing.T) {
	cases := []struct {
		name      string
		code      int
		wantIs    error
		wantNotIs error
	}{
		{"400_BadRequest", 400, ErrNotFound, nil},
		{"404_NotFound", 404, ErrNotFound, nil},
		{"405_MethodNotAllowed", 405, ErrNotFound, nil},
		{"410_Gone", 410, ErrNotFound, nil},
		{"401_Unauthorized", 401, ErrAuthRequired, ErrNotFound},
		{"403_Forbidden", 403, ErrAuthRequired, ErrNotFound},
		{"429_TooManyRequests", 429, nil, ErrNotFound},
		{"500_InternalServerError", 500, nil, ErrNotFound},
		{"502_BadGateway", 502, nil, ErrNotFound},
		{"503_ServiceUnavailable", 503, nil, ErrNotFound},
		{"504_GatewayTimeout", 504, nil, ErrNotFound},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.code)
			}))
			defer srv.Close()

			src := newTestUpstream(srv.URL)
			_, _, err := src.Fetch(context.Background(), testKey)
			if err == nil {
				t.Fatalf("status %d: expected error", tc.code)
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("status %d: err = %v, want errors.Is(_, %v)", tc.code, err, tc.wantIs)
			}
			if tc.wantNotIs != nil && errors.Is(err, tc.wantNotIs) {
				t.Errorf("status %d: err = %v, must not wrap %v", tc.code, err, tc.wantNotIs)
			}
		})
	}
}

func TestUpstream_NetworkErrorIsNotErrNotFound(t *testing.T) {
	src := newTestUpstream(unreachableURL(t))
	_, _, err := src.Fetch(context.Background(), testKey)
	if err == nil {
		t.Fatal("expected error from unreachable server")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("network failure must not be reported as ErrNotFound: %v", err)
	}
}

func TestUpstream_PropagatesContextCancellation(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	src := newTestUpstream(srv.URL)

	errCh := make(chan error, 1)
	go func() {
		_, _, err := src.Fetch(ctx, testKey)
		errCh <- err
	}()

	<-started
	cancel()

	if err := <-errCh; err == nil {
		t.Error("expected error after context cancellation")
	}
}

// unreachableURL returns a URL to a server that has been closed.
// Any TCP dial against it fails immediately, exercising the transport-error path.
func unreachableURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	return url
}

func TestBuildURLPath(t *testing.T) {
	cases := []struct {
		name string
		key  key.Key
		want string
	}{
		{"DebugInfo", key.DebugInfo(testBuildID), "/buildid/" + testBuildID + "/debuginfo"},
		{"Executable", key.Executable(testBuildID), "/buildid/" + testBuildID + "/executable"},
		{"Source", key.Source(testBuildID, "/usr/src/main.c"), "/buildid/" + testBuildID + "/source/usr/src/main.c"},
		{"SourcePreservesSlashes", key.Source(testBuildID, "/a/b/c.c"), "/buildid/" + testBuildID + "/source/a/b/c.c"},
		{"SourceEscapesSegment", key.Source(testBuildID, "/dir with space/x.c"), "/buildid/" + testBuildID + "/source/dir%20with%20space/x.c"},
		{"SourceEscapesPercent", key.Source(testBuildID, "/a/100%foo.c"), "/buildid/" + testBuildID + "/source/a/100%25foo.c"},
		{"Section", key.Section(testBuildID, ".text"), "/buildid/" + testBuildID + "/section/.text"},
		{"SectionEscapesSlash", key.Section(testBuildID, ".rela/.text"), "/buildid/" + testBuildID + "/section/.rela%2F.text"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildURLPath(tc.key); got != tc.want {
				t.Errorf("buildURLPath() = %q, want %q", got, tc.want)
			}
		})
	}
}
