package debuginfod

import (
	"context"
	"errors"
	"io"
	"log/slog"
)

// chain tries each source in order, falling through to the next on any error.
//
// On success the body of the first responsive source is returned.
// On ErrNotFound from a non-final source, the next source is tried silently.
// On any other error from a non-final source, the error is logged and the next source is tried.
// If every source fails, the last source's error is returned verbatim.
//
// The typical use is chain(cache, network), where the cache is queried first and the network serves as a fallback that may itself populate the cache.
type chain struct {
	sources []source
	logger  *slog.Logger
}

func newChain(sources []source, logger *slog.Logger) *chain {
	if len(sources) == 0 {
		panic("debuginfod: chain requires at least one source")
	}
	return &chain{sources: sources, logger: logger}
}

func (c *chain) Fetch(ctx context.Context, key Key) (io.ReadCloser, error) {
	for i, src := range c.sources[:len(c.sources)-1] {
		body, err := src.Fetch(ctx, key)
		if err == nil {
			return body, nil
		}
		if !errors.Is(err, ErrNotFound) {
			c.logger.Debug("source failed, falling through",
				slog.String("key", key.String()),
				slog.Int("source", i),
				slog.Any("error", err),
			)
		}
	}
	return c.sources[len(c.sources)-1].Fetch(ctx, key)
}
