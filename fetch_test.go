package debuginfod

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClient_Fetch(t *testing.T) {
	cases := []struct {
		name  string
		path  string
		body  string
		fetch func(c *Client, ctx context.Context) (io.ReadCloser, error)
	}{
		{
			name: "DebugInfo",
			path: "/buildid/aabbccdd/debuginfo",
			body: "fake debug info",
			fetch: func(c *Client, ctx context.Context) (io.ReadCloser, error) {
				return c.FetchDebugInfo(ctx, "aabbccdd")
			},
		},
		{
			name: "Executable",
			path: "/buildid/aabbccdd/executable",
			body: "fake executable",
			fetch: func(c *Client, ctx context.Context) (io.ReadCloser, error) {
				return c.FetchExecutable(ctx, "aabbccdd")
			},
		},
		{
			name: "Source",
			path: "/buildid/aabbccdd/source/usr/src/main.c",
			body: "int main() { return 0; }",
			fetch: func(c *Client, ctx context.Context) (io.ReadCloser, error) {
				return c.FetchSource(ctx, "aabbccdd", "/usr/src/main.c")
			},
		},
		{
			name: "Section",
			path: "/buildid/aabbccdd/section/.text",
			body: "section data",
			fetch: func(c *Client, ctx context.Context) (io.ReadCloser, error) {
				return c.FetchSection(ctx, "aabbccdd", ".text")
			},
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

			client, err := NewClient(Options{
				ServerURLs: []string{srv.URL},
				Cache:      newMemCache(),
			})
			if err != nil {
				t.Fatal(err)
			}

			rc, err := tc.fetch(client, context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer rc.Close()

			got, _ := io.ReadAll(rc)
			if string(got) != tc.body {
				t.Errorf("got %q, want %q", got, tc.body)
			}
		})
	}
}

func TestClient_UppercaseBuildIDLowercased(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		fmt.Fprint(w, "data")
	}))
	defer srv.Close()

	client, err := NewClient(Options{ServerURLs: []string{srv.URL}})
	if err != nil {
		t.Fatal(err)
	}

	rc, err := client.FetchDebugInfo(context.Background(), "AABBCCDD")
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()

	if gotPath != "/buildid/aabbccdd/debuginfo" {
		t.Errorf("expected lowercase path, got %q", gotPath)
	}
}

func TestClient_FetchEscapesSpecialChars(t *testing.T) {
	cases := []struct {
		name        string
		fetch       func(c *Client, ctx context.Context) (io.ReadCloser, error)
		wantEscaped string
		wantDecoded string
	}{
		{
			name: "Source",
			fetch: func(c *Client, ctx context.Context) (io.ReadCloser, error) {
				return c.FetchSource(ctx, "aabbccdd", "/usr/src/foo bar.c")
			},
			wantEscaped: "/buildid/aabbccdd/source/usr/src/foo%20bar.c",
			wantDecoded: "/buildid/aabbccdd/source/usr/src/foo bar.c",
		},
		{
			name: "Section",
			fetch: func(c *Client, ctx context.Context) (io.ReadCloser, error) {
				return c.FetchSection(ctx, "aabbccdd", "weird name/with slash")
			},
			wantEscaped: "/buildid/aabbccdd/section/weird%20name%2Fwith%20slash",
			wantDecoded: "/buildid/aabbccdd/section/weird name/with slash",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotEscapedPath, gotDecodedPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotEscapedPath = r.URL.EscapedPath()
				gotDecodedPath = r.URL.Path
				fmt.Fprint(w, "data")
			}))
			defer srv.Close()

			client, err := NewClient(Options{ServerURLs: []string{srv.URL}})
			if err != nil {
				t.Fatal(err)
			}

			rc, err := tc.fetch(client, context.Background())
			if err != nil {
				t.Fatal(err)
			}
			rc.Close()

			if gotEscapedPath != tc.wantEscaped {
				t.Errorf("escaped path = %q, want %q", gotEscapedPath, tc.wantEscaped)
			}
			if gotDecodedPath != tc.wantDecoded {
				t.Errorf("decoded path = %q, want %q", gotDecodedPath, tc.wantDecoded)
			}
		})
	}
}

func TestClient_FetchWithoutCache(t *testing.T) {
	body := "streamed data"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	client, err := NewClient(Options{ServerURLs: []string{srv.URL}})
	if err != nil {
		t.Fatal(err)
	}

	rc, err := client.FetchDebugInfo(context.Background(), testBuildID)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("got %q, want %q", got, body)
	}
}
