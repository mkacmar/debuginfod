package debuginfod

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
