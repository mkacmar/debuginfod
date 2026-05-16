package debuginfod

import "errors"

// ErrNotFound is returned when no upstream server has the requested artifact.
var ErrNotFound = errors.New("debuginfod: artifact not found")

// ErrAuthRequired is returned when at least one upstream rejected the request with 401 or 403 and no other source could satisfy it.
// Callers should treat this as a hint to provide credentials, not as an authoritative absence.
var ErrAuthRequired = errors.New("debuginfod: authentication required")

// ErrAlreadyCommitted is returned by CacheEntry.Commit if Commit has already been called.
var ErrAlreadyCommitted = errors.New("debuginfod: cache entry already committed")
