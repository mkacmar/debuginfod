package debuginfod

import (
	"context"
	"io"
)

// Cache is a read-write artifact store used as the persistence layer for Client.
//
// Implementations must be safe for concurrent use.
type Cache interface {
	// Fetch returns the cached artifact for key, or ErrNotFound if no entry exists.
	Fetch(ctx context.Context, key Key) (io.ReadCloser, error)

	// Stage opens a new writable entry for key.
	// Callers write bytes to the returned CacheEntry and then call Commit to atomically promote them.
	// Closing without Commit discards the staged bytes.
	// Stage overwrites any existing entry for key on Commit.
	Stage(ctx context.Context, key Key) (CacheEntry, error)

	// Evict removes the artifact for key.
	// Returns nil if key does not exist.
	Evict(ctx context.Context, key Key) error
}

// CacheEntry is a writable staging handle for a new cache entry.
// Commit atomically promotes the staged bytes.
// Close before Commit discards them.
// Close must be idempotent and is a no-op after Commit.
//
// CacheEntry is not safe for concurrent use. Each entry has a single owner from Stage through Commit or Close.
type CacheEntry interface {
	io.WriteCloser

	// Commit atomically promotes the staged bytes to a live entry.
	// Commit returns ErrAlreadyCommitted if called more than once.
	Commit() error
}
