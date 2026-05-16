package debuginfod

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestClient_FastestServerWins(t *testing.T) {
	slowSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
			fmt.Fprint(w, "slow")
		case <-r.Context().Done():
		}
	}))
	defer slowSrv.Close()

	fastSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "fast")
	}))
	defer fastSrv.Close()

	client, err := NewClient(Options{
		ServerURLs: []string{slowSrv.URL, fastSrv.URL},
	})
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	rc, err := client.FetchDebugInfo(context.Background(), "aabbccdd")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	elapsed := time.Since(start)

	if string(got) != "fast" {
		t.Errorf("got %q, want %q", got, "fast")
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("fetch took %s; expected fast server to win quickly", elapsed)
	}
}

// fanout waits for all results before declaring a miss, so a 200 wins even if a 404 arrived first.
func TestClient_PrefersBodyOverNotFound(t *testing.T) {
	notFoundSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer notFoundSrv.Close()

	body := "from server 2"
	successSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer successSrv.Close()

	client, err := NewClient(Options{
		ServerURLs: []string{notFoundSrv.URL, successSrv.URL},
		Cache:      newMemCache(),
	})
	if err != nil {
		t.Fatal(err)
	}

	rc, err := client.FetchDebugInfo(context.Background(), "aabbccdd")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	got, _ := io.ReadAll(rc)
	if string(got) != body {
		t.Errorf("got %q, want %q", got, body)
	}
}

func TestClient_LosingServerRequestIsCancelled(t *testing.T) {
	loserStarted := make(chan struct{})
	loserCancelled := make(chan struct{})
	loserSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(loserStarted)
		<-r.Context().Done()
		close(loserCancelled)
	}))
	defer loserSrv.Close()

	// Block the winner until the loser's handler is running, so the loser request is guaranteed to be in flight when fanout cancels it.
	winnerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-loserStarted
		fmt.Fprint(w, "winner")
	}))
	defer winnerSrv.Close()

	client, err := NewClient(Options{
		ServerURLs: []string{loserSrv.URL, winnerSrv.URL},
	})
	if err != nil {
		t.Fatal(err)
	}

	rc, err := client.FetchDebugInfo(context.Background(), "aabbccdd")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, rc)
	rc.Close()

	select {
	case <-loserCancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("losing server request was not cancelled after winner finished")
	}
}

// A 404 from one server is authoritative and prevents retrying an unreachable peer.
func TestClient_NotFoundShortCircuitsRetry(t *testing.T) {
	notFoundSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer notFoundSrv.Close()

	unreachableSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	unreachableURL := unreachableSrv.URL
	unreachableSrv.Close()

	client, err := NewClient(Options{
		ServerURLs: []string{notFoundSrv.URL, unreachableURL},
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
		t.Errorf("expected ErrNotFound (one server gave a definitive 404), got %v", err)
	}
}

// unreachableURL returns a URL to a server that has been closed.
// Any TCP dial against it fails immediately, exercising the retry path.
func unreachableURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	return url
}

func TestClient_RetryLoop_ExhaustsMaxRetriesAndRecordsBackoff(t *testing.T) {
	var mu sync.Mutex
	var calls []int
	client, err := NewClient(Options{
		ServerURLs: []string{unreachableURL(t)},
		HTTP: HTTPOptions{
			MaxRetries: 3,
			Backoff: func(retry int) time.Duration {
				mu.Lock()
				calls = append(calls, retry)
				mu.Unlock()
				return 0
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.FetchDebugInfo(context.Background(), testBuildID)
	if err == nil {
		t.Fatal("expected error from unreachable server")
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("network failure must not be reported as ErrNotFound: %v", err)
	}

	mu.Lock()
	got := append([]int(nil), calls...)
	mu.Unlock()
	want := []int{1, 2, 3}
	if len(got) != len(want) {
		t.Fatalf("backoff invocations = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("backoff call %d: got retry=%d, want %d", i, got[i], want[i])
		}
	}
}

func TestClient_RetryLoop_HonorsContextCancelDuringBackoff(t *testing.T) {
	client, err := NewClient(Options{
		ServerURLs: []string{unreachableURL(t)},
		HTTP: HTTPOptions{
			MaxRetries: 5,
			Backoff:    func(retry int) time.Duration { return time.Hour },
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err = client.FetchDebugInfo(ctx, testBuildID)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
	if elapsed > time.Second {
		t.Errorf("cancel took %s; expected sub-second return", elapsed)
	}
}
