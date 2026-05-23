package debuginfod

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"go.kacmar.sk/debuginfod/key"
)

// race fans out a fetch to all sources in parallel and returns the first body-bearing response.
// If no source serves a body, the result follows this precedence:
//   - errors.Join of every source's error, if any source returned an inconclusive error (i.e. neither ErrNotFound nor ErrAuthRequired).
//   - ErrNotFound, if every source authoritatively responded and at least one reported absent.
//   - ErrAuthRequired, if every source authoritatively responded and all reported auth required.
//
// Joined errors include authoritative sentinel messages for diagnostics but do not match errors.Is against them.
type race struct {
	sources []source
	logger  *slog.Logger
}

func newRace(sources []source, logger *slog.Logger) *race {
	return &race{sources: sources, logger: logger}
}

func (r *race) Fetch(ctx context.Context, k key.Key) (io.ReadCloser, error) {
	n := len(r.sources)
	cancels := make([]context.CancelFunc, n)
	results := make(chan raceResult, n)

	for i, src := range r.sources {
		srcCtx, cancel := context.WithCancel(ctx) // #nosec G118 -- cancel is stored in cancels[i] and invoked either via drainLosers on a winner or via the cleanup loop below
		cancels[i] = cancel
		go func(i int, src source, ctx context.Context) {
			body, err := src.Fetch(ctx, k)
			results <- raceResult{i, body, err}
		}(i, src, srcCtx)
	}

	var errs []error
	v := verdictUnset
	for i := range n {
		res := <-results
		if res.body != nil {
			r.logger.Debug("source served",
				slog.String("key", k.String()),
				slog.Int("source", res.idx),
			)
			go drainLosers(results, cancels, res.idx, n-i-1)
			return &cancelOnClose{ReadCloser: res.body, cancel: cancels[res.idx]}, nil
		}
		switch {
		case errors.Is(res.err, ErrNotFound):
			v = max(v, verdictNotFound)
			errs = append(errs, &nonAuthoritative{err: res.err})
			r.logger.Debug("source reports absent",
				slog.String("key", k.String()),
				slog.Int("source", res.idx),
				slog.Any("error", res.err),
			)
		case errors.Is(res.err, ErrAuthRequired):
			v = max(v, verdictAuthRequired)
			errs = append(errs, &nonAuthoritative{err: res.err})
			r.logger.Debug("source requires authentication",
				slog.String("key", k.String()),
				slog.Int("source", res.idx),
				slog.Any("error", res.err),
			)
		default:
			v = max(v, verdictInconclusive)
			errs = append(errs, res.err)
			r.logger.Debug("source unreachable",
				slog.String("key", k.String()),
				slog.Int("source", res.idx),
				slog.Any("error", res.err),
			)
		}
	}
	for _, cancel := range cancels {
		cancel()
	}
	switch v {
	case verdictInconclusive:
		return nil, errors.Join(errs...)
	case verdictNotFound:
		return nil, ErrNotFound
	case verdictAuthRequired:
		return nil, ErrAuthRequired
	default:
		panic("debuginfod: race finished without a body or an error")
	}
}

type raceResult struct {
	idx  int
	body io.ReadCloser
	err  error
}

// verdict is the pool's overall conclusion after all sources have responded.
type verdict int

// Verdict constants are listed in ascending order of precedence: a higher value takes precedence over any lower value observed from another source.
const (
	verdictUnset verdict = iota
	verdictAuthRequired
	verdictNotFound
	verdictInconclusive
)

// nonAuthoritative wraps an error so its message remains visible in a joined error.
// It deliberately omits Unwrap to suppress errors.Is matches against sentinels like ErrNotFound or ErrAuthRequired.
type nonAuthoritative struct{ err error }

func (n *nonAuthoritative) Error() string { return n.err.Error() }

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

// cancelOnClose ties a context cancel to a body's Close so request resources outlive the body only until the caller finishes reading.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	c.cancel()
	return c.ReadCloser.Close()
}
