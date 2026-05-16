package debuginfod

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestHTTPSource(serverURL string) *httpSource {
	return &httpSource{
		serverURL:  serverURL,
		httpClient: http.DefaultClient,
		userAgent:  "test-agent",
	}
}

func TestHTTPSource_SuccessReturnsBody(t *testing.T) {
	body := "the bytes"
	srv := staticServer(t, body)

	src := newTestHTTPSource(srv.URL)
	rc, err := src.Fetch(context.Background(), testKey)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := readAndClose(t, rc); string(got) != body {
		t.Errorf("body = %q, want %q", got, body)
	}
}

func TestHTTPSource_BuildsRequestPathFromKey(t *testing.T) {
	cases := []struct {
		name string
		key  Key
		want string
	}{
		{"DebugInfo", Key{BuildID: testBuildID, Kind: KindDebugInfo}, "/buildid/" + testBuildID + "/debuginfo"},
		{"Executable", Key{BuildID: testBuildID, Kind: KindExecutable}, "/buildid/" + testBuildID + "/executable"},
		{"Source", Key{BuildID: testBuildID, Kind: KindSource, Qualifier: "/usr/src/main.c"}, "/buildid/" + testBuildID + "/source/usr/src/main.c"},
		{"Section", Key{BuildID: testBuildID, Kind: KindSection, Qualifier: ".text"}, "/buildid/" + testBuildID + "/section/.text"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				fmt.Fprint(w, "ok")
			}))
			defer srv.Close()

			src := newTestHTTPSource(srv.URL)
			rc, err := src.Fetch(context.Background(), tc.key)
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			rc.Close()

			if gotPath != tc.want {
				t.Errorf("request path = %q, want %q", gotPath, tc.want)
			}
		})
	}
}

func TestHTTPSource_SendsUserAgent(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	src := &httpSource{serverURL: srv.URL, httpClient: http.DefaultClient, userAgent: "my-agent/1.0"}
	rc, err := src.Fetch(context.Background(), testKey)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	rc.Close()

	if gotUA != "my-agent/1.0" {
		t.Errorf("User-Agent = %q, want %q", gotUA, "my-agent/1.0")
	}
}

// TestHTTPSource_StatusMapping asserts how httpSource translates upstream status codes into errors.
// 401/403 produce ErrAuthRequired, other 4xx produce ErrNotFound, 5xx/429 produce transport errors that engage the retry layer.
func TestHTTPSource_StatusMapping(t *testing.T) {
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

			src := newTestHTTPSource(srv.URL)
			_, err := src.Fetch(context.Background(), testKey)
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

func TestHTTPSource_NetworkErrorIsNotErrNotFound(t *testing.T) {
	src := newTestHTTPSource(unreachableURL(t))
	_, err := src.Fetch(context.Background(), testKey)
	if err == nil {
		t.Fatal("expected error from unreachable server")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("network failure must not be reported as ErrNotFound: %v", err)
	}
}

func TestHTTPSource_PropagatesContextCancellation(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	src := newTestHTTPSource(srv.URL)

	errCh := make(chan error, 1)
	go func() {
		_, err := src.Fetch(ctx, testKey)
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
