package debuginfod

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestClient_Fetch is the public-API smoke test for every Fetch* convenience method.
// Per-kind URL building and escaping are unit-tested in key_test.go and http_test.go.
func TestClient_Fetch(t *testing.T) {
	cases := []struct {
		name  string
		path  string
		body  string
		fetch func(c *Client, ctx context.Context) (io.ReadCloser, error)
	}{
		{
			name: "DebugInfo",
			path: "/buildid/" + testBuildID + "/debuginfo",
			body: "fake debug info",
			fetch: func(c *Client, ctx context.Context) (io.ReadCloser, error) {
				return c.FetchDebugInfo(ctx, testBuildID)
			},
		},
		{
			name: "Executable",
			path: "/buildid/" + testBuildID + "/executable",
			body: "fake executable",
			fetch: func(c *Client, ctx context.Context) (io.ReadCloser, error) {
				return c.FetchExecutable(ctx, testBuildID)
			},
		},
		{
			name: "Source",
			path: "/buildid/" + testBuildID + "/source/usr/src/main.c",
			body: "int main() { return 0; }",
			fetch: func(c *Client, ctx context.Context) (io.ReadCloser, error) {
				return c.FetchSource(ctx, testBuildID, "/usr/src/main.c")
			},
		},
		{
			name: "Section",
			path: "/buildid/" + testBuildID + "/section/.text",
			body: "section data",
			fetch: func(c *Client, ctx context.Context) (io.ReadCloser, error) {
				return c.FetchSection(ctx, testBuildID, ".text")
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

			client := mustNewClient(t, Options{
				ServerURLs: []string{srv.URL},
				Cache:      NewMemoryCache(),
			})

			rc, err := tc.fetch(client, context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got := readAndClose(t, rc); string(got) != tc.body {
				t.Errorf("got %q, want %q", got, tc.body)
			}
		})
	}
}
