package debuginfod

import "errors"

// ErrNotFound is returned when no server in the federation has the requested artifact.
var ErrNotFound = errors.New("debuginfod: artifact not found")
