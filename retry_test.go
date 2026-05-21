package debuginfod

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	"go.kacmar.sk/debuginfod/key"
)

// backoffRecorder records the round number passed to each backoff invocation.
type backoffRecorder struct {
	mu     sync.Mutex
	rounds []int
	delay  time.Duration
}

func (b *backoffRecorder) fn(round int) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rounds = append(b.rounds, round)
	return b.delay
}

func (b *backoffRecorder) snapshot() []int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]int(nil), b.rounds...)
}

func TestRetrier_SucceedsAfter(t *testing.T) {
	transient := errors.New("connection refused")

	cases := []struct {
		name        string
		failsBefore int
		maxRetries  int
		wantCalls   int
		wantRounds  []int
	}{
		{"FirstAttempt", 0, 3, 1, nil},
		{"TwoTransientFailures", 2, 3, 3, []int{1, 2}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var fails int
			inner := newStubSource(func(_ context.Context, _ key.Key) (io.ReadCloser, error) {
				fails++
				if fails <= tc.failsBefore {
					return nil, transient
				}
				return io.NopCloser(bytes.NewReader(testPayload)), nil
			})
			backoff := &backoffRecorder{}

			r := newRetrier(inner, tc.maxRetries, backoff.fn, discardLogger())
			rc, err := r.Fetch(context.Background(), testKey)
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			readAndClose(t, rc)

			if inner.callCount() != tc.wantCalls {
				t.Errorf("inner calls = %d, want %d", inner.callCount(), tc.wantCalls)
			}
			if got := backoff.snapshot(); !slices.Equal(got, tc.wantRounds) {
				t.Errorf("backoff rounds = %v, want %v", got, tc.wantRounds)
			}
		})
	}
}

func TestRetrier_TerminalErrorsShortCircuit(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"NotFound", ErrNotFound},
		{"AuthRequired", ErrAuthRequired},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inner := newStubSource(stubError(tc.err))
			backoff := &backoffRecorder{}

			r := newRetrier(inner, 5, backoff.fn, discardLogger())
			_, err := r.Fetch(context.Background(), testKey)
			if !errors.Is(err, tc.err) {
				t.Errorf("err = %v, want %v", err, tc.err)
			}
			if inner.callCount() != 1 {
				t.Errorf("inner calls = %d, want 1 (%v must not be retried)", inner.callCount(), tc.err)
			}
			if len(backoff.snapshot()) != 0 {
				t.Errorf("backoff invoked on %v: %v", tc.err, backoff.snapshot())
			}
		})
	}
}

func TestRetrier_ExhaustsRetries(t *testing.T) {
	transient := errors.New("connection refused")

	cases := []struct {
		name       string
		maxRetries int
		wantCalls  int
		wantRounds []int
	}{
		{"ZeroRetriesMeansSingleAttempt", 0, 1, nil},
		{"WrapsLastErrorAfterRetries", 2, 3, []int{1, 2}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inner := newStubSource(stubError(transient))
			backoff := &backoffRecorder{}

			r := newRetrier(inner, tc.maxRetries, backoff.fn, discardLogger())
			_, err := r.Fetch(context.Background(), testKey)
			if !errors.Is(err, transient) {
				t.Errorf("err = %v, does not wrap %v", err, transient)
			}
			if inner.callCount() != tc.wantCalls {
				t.Errorf("inner calls = %d, want %d", inner.callCount(), tc.wantCalls)
			}
			if got := backoff.snapshot(); !slices.Equal(got, tc.wantRounds) {
				t.Errorf("backoff rounds = %v, want %v", got, tc.wantRounds)
			}
		})
	}
}

func TestRetrier_ContextCancellationDuringBackoff(t *testing.T) {
	transient := errors.New("connection refused")
	inner := newStubSource(stubError(transient))
	backoff := &backoffRecorder{delay: time.Hour}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r := newRetrier(inner, 5, backoff.fn, discardLogger())
	_, err := r.Fetch(ctx, testKey)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if inner.callCount() != 1 {
		t.Errorf("inner calls = %d, want 1 (no retry once ctx is done)", inner.callCount())
	}
}
