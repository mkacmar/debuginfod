package debuginfod

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.kacmar.sk/debuginfod/key"
)

func TestClient_Fetch(t *testing.T) {
	cases := []struct {
		name string
		path string
		body string
		key  key.Key
	}{
		{
			name: "DebugInfo",
			path: "/buildid/" + testBuildID + "/debuginfo",
			body: "fake debug info",
			key:  key.DebugInfo(testBuildID),
		},
		{
			name: "Executable",
			path: "/buildid/" + testBuildID + "/executable",
			body: "fake executable",
			key:  key.Executable(testBuildID),
		},
		{
			name: "Source",
			path: "/buildid/" + testBuildID + "/source/usr/src/main.c",
			body: "int main() { return 0; }",
			key:  key.Source(testBuildID, "/usr/src/main.c"),
		},
		{
			name: "Section",
			path: "/buildid/" + testBuildID + "/section/.text",
			body: "section data",
			key:  key.Section(testBuildID, ".text"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.path {
					http.NotFound(w, r)
					return
				}
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()

			client := mustNewClient(t, Options{ServerURLs: []string{srv.URL}})

			rc, err := client.Fetch(context.Background(), tc.key)
			if err != nil {
				t.Fatal(err)
			}
			if got := readAndClose(t, rc); string(got) != tc.body {
				t.Errorf("got %q, want %q", got, tc.body)
			}
		})
	}
}

func TestClient_FetchExposesMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-DEBUGINFOD-FILE", "libc.so.6")
		w.Header().Set("X-DEBUGINFOD-SIZE", "42")
		_, _ = w.Write([]byte("data"))
	}))
	defer srv.Close()

	client := mustNewClient(t, Options{ServerURLs: []string{srv.URL}})

	resp, err := client.Fetch(context.Background(), testKey)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Meta.File != "libc.so.6" || resp.Meta.Size != 42 {
		t.Errorf("Meta = %+v, want File=libc.so.6 Size=42", resp.Meta)
	}
	if got := readAndClose(t, resp); string(got) != "data" {
		t.Errorf("body = %q, want %q", got, "data")
	}
}

func TestClient_UppercaseBuildIDLowercased(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte("data"))
	}))
	defer srv.Close()

	client := mustNewClient(t, Options{ServerURLs: []string{srv.URL}})

	rc, err := client.Fetch(context.Background(), key.DebugInfo(strings.ToUpper(testBuildID)))
	if err != nil {
		t.Fatal(err)
	}
	readAndClose(t, rc)

	if gotPath != "/buildid/"+testBuildID+"/debuginfo" {
		t.Errorf("expected lowercase path, got %q", gotPath)
	}
}

func TestClient_RejectsInvalidInput(t *testing.T) {
	client := mustNewClient(t, Options{ServerURLs: []string{"http://localhost"}})
	ctx := context.Background()

	cases := []struct {
		name string
		k    key.Key
	}{
		{"EmptyBuildIDDebugInfo", key.DebugInfo("")},
		{"EmptySource", key.Source(testBuildID, "")},
		{"EmptySection", key.Section(testBuildID, "")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := client.Fetch(ctx, tc.k); err == nil {
				t.Error("expected validation error")
			}
		})
	}
}

func TestClient_ContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	client := mustNewClient(t, Options{ServerURLs: []string{srv.URL}})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := client.Fetch(ctx, testKey); err == nil {
		t.Error("expected error from cancelled context")
	}
}

// TestClient_PerUpstreamRetry_IndependentOf404Peer asserts that one upstream's 404 does not cancel retries on a peer that is transiently failing.
func TestClient_PerUpstreamRetry_IndependentOf404Peer(t *testing.T) {
	notFoundSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer notFoundSrv.Close()

	var attempts atomic.Int32
	transientSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			http.Error(w, "try again", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(testPayload)
	}))
	defer transientSrv.Close()

	noBackoff := func(int) time.Duration { return 0 }
	client := mustNewClient(t, Options{
		ServerURLs: []string{notFoundSrv.URL, transientSrv.URL},
		HTTP: HTTPOptions{
			MaxRetries: 2,
			Backoff:    noBackoff,
		},
	})

	rc, err := client.Fetch(context.Background(), testKey)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := readAndClose(t, rc); !bytes.Equal(got, testPayload) {
		t.Errorf("body = %q, want %q", got, testPayload)
	}
	if got := attempts.Load(); got < 2 {
		t.Errorf("transient server attempts = %d, want >= 2 (must be retried past initial 503)", got)
	}
}

// TestClient_UnresolvedTransport_SuppressesNotFound asserts that a peer's authoritative 404 does not become the pool's verdict when another upstream remains unresolved.
func TestClient_UnresolvedTransport_SuppressesNotFound(t *testing.T) {
	notFoundSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer notFoundSrv.Close()

	brokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusServiceUnavailable)
	}))
	defer brokenSrv.Close()

	noBackoff := func(int) time.Duration { return 0 }
	client := mustNewClient(t, Options{
		ServerURLs: []string{notFoundSrv.URL, brokenSrv.URL},
		HTTP: HTTPOptions{
			MaxRetries: 1,
			Backoff:    noBackoff,
		},
	})

	_, err := client.Fetch(context.Background(), testKey)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, must not wrap ErrNotFound (unresolved transport error means pool verdict is unknown)", err)
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("err = %v, expected to wrap the 503 transport error", err)
	}
}
