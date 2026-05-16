package debuginfod

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"
)

// retrying wraps a source so that transport-level errors are retried with backoff.
//
// ErrNotFound and ErrAuthRequired are treated as authoritative and short-circuit the retry loop.
// After maxRetries additional attempts, the last error is returned wrapped with a descriptive prefix.
type retrying struct {
	inner      source
	maxRetries int
	backoff    func(retry int) time.Duration
	logger     *slog.Logger
}

func newRetrying(inner source, maxRetries int, backoff func(int) time.Duration, logger *slog.Logger) *retrying {
	return &retrying{inner: inner, maxRetries: maxRetries, backoff: backoff, logger: logger}
}

func (r *retrying) Fetch(ctx context.Context, key Key) (io.ReadCloser, error) {
	for retry := 0; ; retry++ {
		body, err := r.inner.Fetch(ctx, key)
		if err == nil {
			return body, nil
		}
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrAuthRequired) {
			return nil, err
		}
		if retry >= r.maxRetries {
			return nil, fmt.Errorf("debuginfod: retries exhausted: %w", err)
		}
		delay := r.backoff(retry + 1)
		r.logger.Debug("retrying after backoff",
			slog.String("key", key.String()),
			slog.Int("retry", retry+1),
			slog.Duration("backoff", delay),
		)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
