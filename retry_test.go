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

// flakySource succeeds after a given number of transport failures.
type flakySource struct {
	mu        sync.Mutex
	calls     int
	failUntil int
	transErr  error
	success   []byte
}

func (f *flakySource) Fetch(_ context.Context, _ Key) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= f.failUntil {
		return nil, f.transErr
	}
	return io.NopCloser(bytes.NewReader(f.success)), nil
}

func (f *flakySource) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestRetrying_SuccessOnFirstAttempt(t *testing.T) {
	inner := newStubSource(stubBytes(testPayload))
	backoff := &backoffRecorder{}

	r := newRetrying(inner, 3, backoff.fn, discardLogger())
	rc, err := r.Fetch(context.Background(), testKey)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	rc.Close()

	if inner.callCount() != 1 {
		t.Errorf("inner calls = %d, want 1", inner.callCount())
	}
	if len(backoff.snapshot()) != 0 {
		t.Errorf("backoff invoked on first success: %v", backoff.snapshot())
	}
}

func TestRetrying_TerminalErrorsShortCircuit(t *testing.T) {
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

			r := newRetrying(inner, 5, backoff.fn, discardLogger())
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

func TestRetrying_SucceedsAfterTransientFailures(t *testing.T) {
	transient := errors.New("connection refused")
	inner := &flakySource{failUntil: 2, transErr: transient, success: testPayload}
	backoff := &backoffRecorder{}

	r := newRetrying(inner, 3, backoff.fn, discardLogger())
	rc, err := r.Fetch(context.Background(), testKey)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	rc.Close()

	if inner.callCount() != 3 {
		t.Errorf("inner calls = %d, want 3", inner.callCount())
	}
	if got, want := backoff.snapshot(), []int{1, 2}; !slices.Equal(got, want) {
		t.Errorf("backoff rounds = %v, want %v", got, want)
	}
}

func TestRetrying_ExhaustsRetries(t *testing.T) {
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

			r := newRetrying(inner, tc.maxRetries, backoff.fn, discardLogger())
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

func TestRetrying_ContextCancellationDuringBackoff(t *testing.T) {
	transient := errors.New("connection refused")
	inner := newStubSource(stubError(transient))
	backoff := &backoffRecorder{delay: time.Hour}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r := newRetrying(inner, 5, backoff.fn, discardLogger())
	_, err := r.Fetch(ctx, testKey)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if inner.callCount() != 1 {
		t.Errorf("inner calls = %d, want 1 (no retry once ctx is done)", inner.callCount())
	}
}
