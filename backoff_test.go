package debuginfod

import (
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

func TestExponentialBackoff_DelayStaysWithinMaxDelay(t *testing.T) {
	baseDelay := 100 * time.Millisecond
	maxDelay := time.Second
	backoff, err := ExponentialBackoff(baseDelay, maxDelay)
	if err != nil {
		t.Fatal(err)
	}
	for retry := 1; retry <= 10; retry++ {
		delay := backoff(retry)
		if delay < 0 || delay > maxDelay {
			t.Errorf("backoff(%d) = %s, out of [0, %s]", retry, delay, maxDelay)
		}
	}
}

func TestExponentialBackoff_LargeRetryDoesNotOverflow(t *testing.T) {
	backoff, err := ExponentialBackoff(time.Nanosecond, time.Duration(1<<62))
	if err != nil {
		t.Fatal(err)
	}
	for _, retry := range []int{100, 1000, 1 << 30} {
		delay := backoff(retry)
		if delay < 0 {
			t.Errorf("backoff(%d) = %s, want non-negative", retry, delay)
		}
	}
}
