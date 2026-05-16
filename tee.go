package debuginfod

import (
	"context"
	"errors"
	"io"
	"log/slog"
)

// teeing wraps a source so successfully-fetched bytes are also written into a Cache as they flow to the caller.
//
// A clean EOF followed by Close commits the entry.
// An upstream body error or caller Close before EOF discards the entry.
// A cache write error discards the entry, the caller still receives the full body.
type teeing struct {
	inner  source
	cache  Cache
	logger *slog.Logger
}

func newTeeing(inner source, cache Cache, logger *slog.Logger) *teeing {
	return &teeing{inner: inner, cache: cache, logger: logger}
}

func (t *teeing) Fetch(ctx context.Context, key Key) (io.ReadCloser, error) {
	body, err := t.inner.Fetch(ctx, key)
	if err != nil {
		return nil, err
	}
	entry, err := t.cache.Stage(ctx, key)
	if err != nil {
		t.logger.Warn("cache stage failed",
			slog.String("key", key.String()),
			slog.Any("error", err),
		)
		return body, nil
	}

	return &teeReader{body: body, entry: entry, logger: t.logger.With(slog.String("key", key.String()))}, nil
}

// teeReader streams body bytes to the caller while teeing a copy into a staged cache entry.
// Cache write errors are swallowed so the caller's read is unaffected.
// Commit happens on Close after a clean EOF, otherwise the entry is discarded.
type teeReader struct {
	body     io.ReadCloser
	entry    CacheEntry
	logger   *slog.Logger
	eof      bool
	cacheErr error
}

func (r *teeReader) Read(p []byte) (int, error) {
	n, err := r.body.Read(p)
	if n > 0 {
		r.cacheWrite(p[:n])
	}
	if errors.Is(err, io.EOF) {
		r.eof = true
	}
	return n, err
}

// cacheWrite feeds bytes to the cache entry, retaining the first error in cacheErr.
func (r *teeReader) cacheWrite(p []byte) {
	if r.cacheErr != nil {
		return
	}
	n, err := r.entry.Write(p)
	if err == nil && n < len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		r.cacheErr = err
	}
}

func (r *teeReader) Close() error {
	switch {
	case r.cacheErr != nil:
		r.logger.Warn("cache write failed, entry discarded", slog.Any("error", r.cacheErr))
	case r.eof:
		if err := r.entry.Commit(); err != nil {
			r.logger.Warn("cache commit failed", slog.Any("error", err))
		} else {
			r.logger.Debug("cached artifact")
		}
	}
	return errors.Join(r.entry.Close(), r.body.Close())
}
