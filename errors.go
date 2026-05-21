package debuginfod

import "errors"

// ErrNotFound is returned when no upstream server has the requested artifact.
var ErrNotFound = errors.New("debuginfod: artifact not found")

// ErrAuthRequired is returned when at least one upstream rejected the request with 401 or 403 and no other source could satisfy it.
var ErrAuthRequired = errors.New("debuginfod: authentication required")
