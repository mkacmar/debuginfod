package debuginfod

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// ArtifactKind identifies which artifact a Key refers to.
type ArtifactKind int

const (
	KindDebugInfo ArtifactKind = iota
	KindExecutable
	KindSource
	KindSection
)

func (k ArtifactKind) String() string {
	switch k {
	case KindDebugInfo:
		return "debuginfo"
	case KindExecutable:
		return "executable"
	case KindSource:
		return "source"
	case KindSection:
		return "section"
	}
	return fmt.Sprintf("unknown(%d)", int(k))
}

// Key uniquely identifies a cached artifact.
// Qualifier is the source path for KindSource, the section name for KindSection, and empty otherwise.
type Key struct {
	BuildID   string
	Kind      ArtifactKind
	Qualifier string
}

func (k Key) String() string {
	if k.Qualifier == "" {
		return fmt.Sprintf("%s/%s", k.BuildID, k.Kind)
	}
	return fmt.Sprintf("%s/%s/%s", k.BuildID, k.Kind, k.Qualifier)
}

// validate reports whether k is well-formed for use as a cache key.
// Concrete Cache implementations should validate before computing storage locations.
func (k Key) validate() error {
	if k.BuildID == "" {
		return fmt.Errorf("debuginfod: cache key has empty BuildID")
	}
	if len(k.BuildID)%2 != 0 {
		return fmt.Errorf("debuginfod: cache key has odd-length BuildID: %q", k.BuildID)
	}
	for _, ch := range k.BuildID {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')) {
			return fmt.Errorf("debuginfod: cache key has non-hex character in BuildID: %q", ch)
		}
	}
	switch k.Kind {
	case KindDebugInfo, KindExecutable:
		if k.Qualifier != "" {
			return fmt.Errorf("debuginfod: cache key %s has unexpected qualifier", k)
		}
	case KindSource:
		if k.Qualifier == "" {
			return fmt.Errorf("debuginfod: cache key %s requires qualifier", k)
		}
	case KindSection:
		if k.Qualifier == "" {
			return fmt.Errorf("debuginfod: cache key %s requires qualifier", k)
		}
		if k.Qualifier == "." || k.Qualifier == ".." {
			return fmt.Errorf("debuginfod: cache key %s has invalid section name", k)
		}
		if strings.ContainsAny(k.Qualifier, "/\x00") {
			return fmt.Errorf("debuginfod: cache key %s has invalid section name", k)
		}
	default:
		return fmt.Errorf("debuginfod: cache key %s has unknown kind", k)
	}
	return nil
}

// Cache is the interface for artifact storage backends.
// Implementations must be safe for concurrent use.
type Cache interface {
	// Get retrieves the artifact for the given key.
	// Returns ErrNotFound if the key does not exist.
	Get(ctx context.Context, k Key) (io.ReadCloser, error)

	// Create opens a new staging entry for the given key.
	// The caller writes bytes to the returned CacheEntry and then calls Commit to atomically promote them.
	// Closing without Commit discards the staged bytes.
	// Create overwrites any existing entry for the key on Commit.
	Create(ctx context.Context, k Key) (CacheEntry, error)

	// Delete removes the artifact for the given key.
	// Returns nil if the key does not exist.
	Delete(ctx context.Context, k Key) error
}

// CacheEntry is a writable staging handle for a new cache entry.
// Commit atomically promotes the staged bytes, Close before Commit discards them.
// Close must be idempotent and is a no-op after Commit.
type CacheEntry interface {
	io.WriteCloser
	// Commit atomically promotes the staged bytes to a live entry.
	// Commit returns ErrAlreadyCommitted if called more than once.
	Commit() error
}

// putReader stores all bytes from reader under k.
// It runs the full Create-Copy-Commit-Close lifecycle and aborts on any error.
func putReader(ctx context.Context, cache Cache, k Key, reader io.Reader) error {
	entry, err := cache.Create(ctx, k)
	if err != nil {
		return err
	}
	defer entry.Close()
	if _, err := io.Copy(entry, reader); err != nil {
		return err
	}
	return entry.Commit()
}
