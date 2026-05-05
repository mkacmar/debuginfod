package debuginfod

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestClient_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	client, err := NewClient(Options{
		ServerURLs: []string{srv.URL},
		HTTP:       HTTPOptions{MaxRetries: 0},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.FetchDebugInfo(context.Background(), "deadbeef")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// Only network failures should retry. HTTP responses are authoritative.
func TestClient_AnyHTTPResponseIsNotFound(t *testing.T) {
	for _, code := range []int{400, 401, 403, 405, 410, 500, 501, 503} {
		t.Run(fmt.Sprintf("status_%d", code), func(t *testing.T) {
			var requestCount atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestCount.Add(1)
				w.WriteHeader(code)
			}))
			defer srv.Close()

			client, err := NewClient(Options{
				ServerURLs: []string{srv.URL},
				HTTP: HTTPOptions{
					MaxRetries: 2,
					Backoff:    func(retry int) time.Duration { return time.Millisecond },
				},
			})
			if err != nil {
				t.Fatal(err)
			}

			_, err = client.FetchDebugInfo(context.Background(), "aabbccdd")
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("expected ErrNotFound, got %v", err)
			}
			if got := requestCount.Load(); got != 1 {
				t.Errorf("expected 1 request (no retry on HTTP response), got %d", got)
			}
		})
	}
}

// Callers must be able to distinguish transient outages from a definitive miss.
func TestClient_AllUnreachableReturnsNonNotFoundError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	client, err := NewClient(Options{
		ServerURLs: []string{url},
		HTTP: HTTPOptions{
			MaxRetries: 1,
			Backoff:    func(retry int) time.Duration { return time.Millisecond },
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.FetchDebugInfo(context.Background(), "aabbccdd")
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("did not expect ErrNotFound for unreachable server, got %v", err)
	}
}

func TestClient_ContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	client, err := NewClient(Options{ServerURLs: []string{srv.URL}})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err = client.FetchDebugInfo(ctx, "aabbccdd")
	if err == nil {
		t.Error("expected error from cancelled context")
	}
}
