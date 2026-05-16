package debuginfod

import (
	"fmt"
	"math/rand/v2"
	"time"
)

const (
	defaultBaseBackoff = 1 * time.Second
	defaultMaxBackoff  = 30 * time.Second
)

// ExponentialBackoff returns a backoff function using the "Full Jitter" algorithm.
// See https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/
func ExponentialBackoff(baseDelay, maxDelay time.Duration) (func(retry int) time.Duration, error) {
	if baseDelay <= 0 {
		return nil, fmt.Errorf("debuginfod: ExponentialBackoff: baseDelay must be positive, got %s", baseDelay)
	}
	if maxDelay < baseDelay {
		return nil, fmt.Errorf("debuginfod: ExponentialBackoff: maxDelay (%s) must be >= baseDelay (%s)", maxDelay, baseDelay)
	}
	maxShift := 0
	for d := baseDelay; d > 0 && d < maxDelay; d <<= 1 {
		maxShift++
	}
	return func(retry int) time.Duration {
		if retry < 1 {
			return 0
		}
		shift := retry - 1
		if shift > maxShift {
			shift = maxShift
		}
		upperBound := baseDelay << uint(shift)
		if upperBound > maxDelay {
			upperBound = maxDelay
		}
		return time.Duration(rand.Int64N(int64(upperBound))) // #nosec G404 -- jitter, not security
	}, nil
}
