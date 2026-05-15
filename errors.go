package debuginfod

import "errors"

// ErrNotFound is returned when no server in the federation has the requested artifact.
var ErrNotFound = errors.New("debuginfod: artifact not found")

// ErrAlreadyCommitted is returned by CacheEntry.Commit if Commit has already been called.
var ErrAlreadyCommitted = errors.New("debuginfod: cache entry already committed")
