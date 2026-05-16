package debuginfod

import (
	"context"
	"errors"
	"io"
	"log/slog"
)

// authoritativeRace fans out a fetch to all configured sources in parallel and returns the first body-bearing response.
//
// If any source returns a body, it is returned and the other sources are cancelled and drained.
// If every source returns an error, the result is:
//   - ErrNotFound, if at least one source's error wraps ErrNotFound (definitive "absent" answer).
//   - ErrAuthRequired, if no source reported absent but at least one rejected the request as needing auth.
//   - errors.Join of all errors otherwise.
//
// The intent matches the debuginfod model where any HTTP response from any server is treated as authoritative.
type authoritativeRace struct {
	sources []source
	logger  *slog.Logger
}

func newAuthoritativeRace(sources []source, logger *slog.Logger) *authoritativeRace {
	return &authoritativeRace{sources: sources, logger: logger}
}

type raceResult struct {
	idx  int
	body io.ReadCloser
	err  error
}

func (r *authoritativeRace) Fetch(ctx context.Context, key Key) (io.ReadCloser, error) {
	n := len(r.sources)
	cancels := make([]context.CancelFunc, n)
	results := make(chan raceResult, n)

	for i, src := range r.sources {
		srcCtx, cancel := context.WithCancel(ctx) // #nosec G118 -- cancel is stored in cancels[i] and invoked either via drainLosers on a winner or via the cleanup loop below
		cancels[i] = cancel
		go func(i int, src source, ctx context.Context) {
			body, err := src.Fetch(ctx, key)
			results <- raceResult{i, body, err}
		}(i, src, srcCtx)
	}

	var errs []error
	notFound := false
	authRequired := false
	for i := range n {
		res := <-results
		if res.body != nil {
			r.logger.Debug("source served",
				slog.String("key", key.String()),
				slog.Int("source", res.idx),
			)
			go drainLosers(results, cancels, res.idx, n-i-1)
			return &cancelOnClose{ReadCloser: res.body, cancel: cancels[res.idx]}, nil
		}
		switch {
		case errors.Is(res.err, ErrNotFound):
			notFound = true
			r.logger.Debug("source reports absent",
				slog.String("key", key.String()),
				slog.Int("source", res.idx),
				slog.Any("error", res.err),
			)
		case errors.Is(res.err, ErrAuthRequired):
			authRequired = true
			r.logger.Debug("source requires authentication",
				slog.String("key", key.String()),
				slog.Int("source", res.idx),
				slog.Any("error", res.err),
			)
		default:
			r.logger.Debug("source unreachable",
				slog.String("key", key.String()),
				slog.Int("source", res.idx),
				slog.Any("error", res.err),
			)
		}
		errs = append(errs, res.err)
	}
	for _, cancel := range cancels {
		cancel()
	}
	switch {
	case notFound:
		return nil, ErrNotFound
	case authRequired:
		return nil, ErrAuthRequired
	}
	return nil, errors.Join(errs...)
}

// drainLosers cancels every non-winning source and closes any late-arriving bodies.
func drainLosers(results <-chan raceResult, cancels []context.CancelFunc, winner, remaining int) {
	for i, cancel := range cancels {
		if i != winner {
			cancel()
		}
	}
	for range remaining {
		res := <-results
		if res.body != nil {
			_ = res.body.Close()
		}
	}
}

// cancelOnClose ties a context cancel to a body's Close so the request's resources are released only when the caller finishes reading.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	c.cancel()
	return c.ReadCloser.Close()
}
