package debuginfod

import (
	"testing"
	"time"
)

func TestExponentialBackoff_InvalidArgs(t *testing.T) {
	cases := []struct {
		name      string
		baseDelay time.Duration
		maxDelay  time.Duration
	}{
		{"zero baseDelay", 0, time.Second},
		{"negative baseDelay", -time.Second, time.Second},
		{"maxDelay less than baseDelay", 2 * time.Second, time.Second},
		{"negative maxDelay", time.Second, -time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ExponentialBackoff(tc.baseDelay, tc.maxDelay); err == nil {
				t.Errorf("expected error for baseDelay=%s maxDelay=%s", tc.baseDelay, tc.maxDelay)
			}
		})
	}
}

func TestExponentialBackoff_ValidBounds(t *testing.T) {
	base := 100 * time.Millisecond
	maxDelay := 1 * time.Second
	backoff, err := ExponentialBackoff(base, maxDelay)
	if err != nil {
		t.Fatal(err)
	}

	if d := backoff(0); d != 0 {
		t.Errorf("backoff(0) = %s, want 0", d)
	}
	if d := backoff(-1); d != 0 {
		t.Errorf("backoff(-1) = %s, want 0", d)
	}

	for retry := 1; retry <= 10; retry++ {
		d := backoff(retry)
		if d < 0 || d > maxDelay {
			t.Errorf("backoff(%d) = %s, out of [0, %s]", retry, d, maxDelay)
		}
	}
}
