package debuginfod

import (
	"fmt"
	"math/rand/v2"
	"time"
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
	maxRetry := 1
	for d := baseDelay; d < maxDelay; d <<= 1 {
		maxRetry++
	}
	return func(retry int) time.Duration {
		if retry < 1 {
			return 0
		}
		if retry > maxRetry {
			retry = maxRetry
		}
		delay := baseDelay << uint(retry-1)
		if delay > maxDelay {
			delay = maxDelay
		}
		jitter := time.Duration(rand.Int64N(int64(delay))) // #nosec G404 -- jitter, not security
		return jitter
	}, nil
}
