package debuginfod

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"go.kacmar.sk/debuginfod/key"
)

func TestNewClient_RejectsInvalidOptions(t *testing.T) {
	cases := []struct {
		name string
		opts Options
	}{
		{"NoServers", Options{}},
		{"NegativeMaxRetries", Options{
			ServerURLs: []string{"http://srv"},
			HTTP:       HTTPOptions{MaxRetries: -1},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewClient(tc.opts); err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

func TestClient_NormalizesServerURLs(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	client := mustNewClient(t, Options{
		ServerURLs: []string{srv.URL, srv.URL + "/", srv.URL},
	})

	_, _ = client.Fetch(context.Background(), key.DebugInfo(testBuildID))

	if got := hits.Load(); got != 1 {
		t.Errorf("expected 1 request after URL normalization, got %d", got)
	}
}

func TestNormalizeServerURLs(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		cases := []struct {
			name string
			in   []string
			want []string
		}{
			{"trim trailing slash", []string{"http://srv/"}, []string{"http://srv"}},
			{"lowercase scheme and host", []string{"HTTP://Example.COM/Path"}, []string{"http://example.com/Path"}},
			{"preserve path query port", []string{"https://srv:8080/sub?q=1"}, []string{"https://srv:8080/sub?q=1"}},
			{"dedup after normalize", []string{"http://srv", "http://srv/", "HTTP://SRV"}, []string{"http://srv"}},
			{"distinct entries kept in order", []string{"http://b", "http://a"}, []string{"http://b", "http://a"}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got, err := normalizeServerURLs(tc.in)
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if len(got) != len(tc.want) {
					t.Fatalf("len = %d, want %d (%v)", len(got), len(tc.want), got)
				}
				for i := range got {
					if got[i] != tc.want[i] {
						t.Errorf("[%d] = %q, want %q", i, got[i], tc.want[i])
					}
				}
			})
		}
	})

	t.Run("invalid", func(t *testing.T) {
		cases := []struct {
			name string
			in   string
		}{
			{"empty string", ""},
			{"missing scheme", "example.com"},
			{"unsupported scheme ftp", "ftp://srv"},
			{"unsupported scheme file", "file:///etc/passwd"},
			{"scheme but no host", "http://"},
			{"unparseable", "http://[bad"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := normalizeServerURLs([]string{tc.in}); err == nil {
					t.Errorf("expected error for %q", tc.in)
				}
			})
		}
	})
}

func TestClient_UserAgent(t *testing.T) {
	cases := []struct {
		name      string
		userAgent string
		want      string
	}{
		{"custom", "myapp/1.0", "myapp/1.0"},
		{"default", "", defaultUserAgent()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotUA string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotUA = r.Header.Get("User-Agent")
				fmt.Fprint(w, "data")
			}))
			defer srv.Close()

			client := mustNewClient(t, Options{
				ServerURLs: []string{srv.URL},
				HTTP:       HTTPOptions{UserAgent: tc.userAgent},
			})

			rc, err := client.Fetch(context.Background(), key.DebugInfo(testBuildID))
			if err != nil {
				t.Fatal(err)
			}
			readAndClose(t, rc)

			if gotUA != tc.want {
				t.Errorf("User-Agent = %q, want %q", gotUA, tc.want)
			}
		})
	}
}
