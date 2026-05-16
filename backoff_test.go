package debuginfod

import (
	"math"
	"testing"
	"time"
)

func TestExponentialBackoff_RejectsInvalidArgs(t *testing.T) {
	cases := []struct {
		name      string
		baseDelay time.Duration
		maxDelay  time.Duration
	}{
		{"ZeroBaseDelay", 0, time.Second},
		{"NegativeBaseDelay", -time.Second, time.Second},
		{"MaxDelayLessThanBaseDelay", 2 * time.Second, time.Second},
		{"NegativeMaxDelay", time.Second, -time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ExponentialBackoff(tc.baseDelay, tc.maxDelay); err == nil {
				t.Errorf("expected error for baseDelay=%s maxDelay=%s", tc.baseDelay, tc.maxDelay)
			}
		})
	}
}

func TestExponentialBackoff_NonPositiveRetryReturnsZero(t *testing.T) {
	backoff, err := ExponentialBackoff(100*time.Millisecond, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, retry := range []int{0, -1, -100} {
		if delay := backoff(retry); delay != 0 {
			t.Errorf("backoff(%d) = %s, want 0", retry, delay)
		}
	}
}

func TestExponentialBackoff_DelayWithinBounds(t *testing.T) {
	cases := []struct {
		name      string
		baseDelay time.Duration
		maxDelay  time.Duration
		retries   []int
	}{
		{"normal", 100 * time.Millisecond, time.Second, []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}},
		{"extremeBoundsNoOverflow", time.Nanosecond, time.Duration(1 << 62), []int{100, 1000, 1 << 30}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backoff, err := ExponentialBackoff(tc.baseDelay, tc.maxDelay)
			if err != nil {
				t.Fatal(err)
			}
			for _, retry := range tc.retries {
				delay := backoff(retry)
				if delay < 0 || delay > tc.maxDelay {
					t.Errorf("backoff(%d) = %s, out of [0, %s]", retry, delay, tc.maxDelay)
				}
			}
		})
	}
}

func TestExponentialBackoff_DoesNotHangOnExtremeMaxDelay(t *testing.T) {
	done := make(chan struct{})
	go func() {
		_, _ = ExponentialBackoff(time.Nanosecond, time.Duration(math.MaxInt64))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ExponentialBackoff hung, likely overflow in the maxShift loop")
	}
}
