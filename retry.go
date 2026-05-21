package debuginfod

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"go.kacmar.sk/debuginfod/key"
)

// retrier retries transport errors with backoff. ErrNotFound and ErrAuthRequired short-circuit.
type retrier struct {
	inner      source
	maxRetries int
	backoff    func(retry int) time.Duration
	logger     *slog.Logger
}

func newRetrier(inner source, maxRetries int, backoff func(int) time.Duration, logger *slog.Logger) *retrier {
	return &retrier{inner: inner, maxRetries: maxRetries, backoff: backoff, logger: logger}
}

func (r *retrier) Fetch(ctx context.Context, k key.Key) (io.ReadCloser, error) {
	for retry := 0; ; retry++ {
		body, err := r.inner.Fetch(ctx, k)
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
			slog.String("key", k.String()),
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
