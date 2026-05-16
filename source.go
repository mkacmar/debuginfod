package debuginfod

import (
	"context"
	"io"
)

// source is a read-only origin for debuginfod artifacts.
//
// Fetch returns the artifact bytes for key, or an error.
// A returned error that wraps ErrNotFound means the source authoritatively reports the artifact as absent.
// Transport-level errors are retryable.
//
// Implementations must be safe for concurrent use.
type source interface {
	Fetch(ctx context.Context, key Key) (io.ReadCloser, error)
}
