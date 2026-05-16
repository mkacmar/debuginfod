package debuginfod

import (
	"bytes"
	"context"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
)

func (c *Client) FetchSection(ctx context.Context, buildID string, sectionName string) (io.ReadCloser, error) {
	id, err := validateBuildID(buildID)
	if err != nil {
		return nil, err
	}
	if sectionName == "" {
		return nil, fmt.Errorf("debuginfod: section name is empty")
	}
	key := Key{BuildID: id, Kind: KindSection, Qualifier: sectionName}
	urlPath := id + "/section/" + url.PathEscape(sectionName)

	if c.cache != nil {
		if rc, err := c.tryLocalSection(ctx, id, sectionName); rc != nil || err != nil {
			return rc, err
		}
	}

	rc, err := c.fetch(ctx, key, urlPath)
	if err == nil {
		return rc, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	// Server doesn't support the section endpoint, or it doesn't have this section.
	c.logger.Debug("section endpoint unavailable, falling back to full debuginfo",
		slog.String("buildID", id),
		slog.String("section", sectionName),
	)
	return c.fetchSectionViaDebugInfo(ctx, id, sectionName)
}

// tryLocalSection serves a section from cache or by slicing cached debuginfo.
// A nil return for both values means the caller should go to the network.
func (c *Client) tryLocalSection(ctx context.Context, buildID, sectionName string) (io.ReadCloser, error) {
	sectionKey := Key{BuildID: buildID, Kind: KindSection, Qualifier: sectionName}
	if rc, ok := c.tryCachedSection(ctx, sectionKey, buildID, sectionName); ok {
		return rc, nil
	}

	cachedDebugInfo, ok := c.loadCachedDebugInfoReaderAt(ctx, buildID)
	if !ok {
		return nil, nil
	}
	defer cachedDebugInfo.Close()

	data, err := c.sliceSectionFromCachedDebugInfo(ctx, cachedDebugInfo, buildID, sectionName)
	if err != nil || data == nil {
		return nil, err
	}

	if err := putReader(ctx, c.cache, sectionKey, bytes.NewReader(data)); err != nil {
		c.logger.Warn("section cache put failed",
			slog.String("buildID", buildID),
			slog.String("section", sectionName),
			slog.Any("error", err),
		)
	}
	c.logger.Debug("section sliced from cached debuginfo",
		slog.String("buildID", buildID),
		slog.String("section", sectionName),
	)
	return io.NopCloser(bytes.NewReader(data)), nil
}

// tryCachedSection returns a cached section reader on cache hit.
// On cache miss or read error it logs and returns ok=false, so the caller falls back to slicing.
func (c *Client) tryCachedSection(ctx context.Context, key Key, buildID, sectionName string) (io.ReadCloser, bool) {
	rc, err := c.cache.Get(ctx, key)
	if err == nil {
		c.logger.Debug("section cache hit", slog.String("buildID", buildID), slog.String("section", sectionName))
		return rc, true
	}
	if !errors.Is(err, ErrNotFound) {
		c.logger.Warn("section cache get failed",
			slog.String("buildID", buildID),
			slog.String("section", sectionName),
			slog.Any("error", err),
		)
	}
	return nil, false
}

// readerAtCloser pairs an io.ReaderAt with the Close method of the underlying source.
// It lets loadCachedDebugInfoReaderAt return a single value whether the source is seekable or had to be buffered in memory.
type readerAtCloser struct {
	io.ReaderAt
	io.Closer
}

// loadCachedDebugInfoReaderAt fetches cached debuginfo and exposes it as an io.ReaderAt.
// If the cached reader is not itself an io.ReaderAt, the contents are buffered in memory.
// The caller must Close the returned value when done.
// Returns ok=false on cache miss, read error, or buffering failure.
func (c *Client) loadCachedDebugInfoReaderAt(ctx context.Context, buildID string) (readerAtCloser, bool) {
	debugKey := Key{BuildID: buildID, Kind: KindDebugInfo}
	debugRC, err := c.cache.Get(ctx, debugKey)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			c.logger.Warn("debuginfo cache get failed",
				slog.String("buildID", buildID),
				slog.Any("error", err),
			)
		}
		return readerAtCloser{}, false
	}

	if readerAt, ok := debugRC.(io.ReaderAt); ok {
		return readerAtCloser{ReaderAt: readerAt, Closer: debugRC}, true
	}

	c.logger.Debug("cached debuginfo not seekable, buffering for local slice",
		slog.String("buildID", buildID),
	)
	data, err := io.ReadAll(debugRC)
	if err != nil {
		_ = debugRC.Close()
		c.logger.Warn("cached debuginfo read failed",
			slog.String("buildID", buildID),
			slog.Any("error", err),
		)
		return readerAtCloser{}, false
	}
	return readerAtCloser{ReaderAt: bytes.NewReader(data), Closer: debugRC}, true
}

// sliceSectionFromCachedDebugInfo parses debugInfo as ELF and returns the bytes of the named section.
// Returns (nil, nil) when the cached debuginfo is corrupt, in which case it is evicted before returning so the caller falls back to the network.
// Returns (nil, ErrNotFound) when the ELF parses but does not contain the section.
// Returns (nil, err) when the section is present but cannot be read, in which case the cached debuginfo is also evicted.
func (c *Client) sliceSectionFromCachedDebugInfo(ctx context.Context, debugInfo io.ReaderAt, buildID, sectionName string) ([]byte, error) {
	debugKey := Key{BuildID: buildID, Kind: KindDebugInfo}

	elfFile, err := elf.NewFile(debugInfo)
	if err != nil {
		c.logger.Warn("cached debuginfo not parseable as ELF, evicting",
			slog.String("buildID", buildID),
			slog.Any("error", err),
		)
		c.evictDebugInfo(ctx, debugKey, buildID)
		return nil, nil
	}
	defer elfFile.Close()

	section := elfFile.Section(sectionName)
	if section == nil {
		c.logger.Debug("section not in cached debuginfo",
			slog.String("buildID", buildID),
			slog.String("section", sectionName),
		)
		return nil, ErrNotFound
	}

	data, err := io.ReadAll(section.Open())
	if err != nil {
		c.logger.Warn("cached debuginfo section read failed, evicting",
			slog.String("buildID", buildID),
			slog.String("section", sectionName),
			slog.Any("error", err),
		)
		c.evictDebugInfo(ctx, debugKey, buildID)
		return nil, fmt.Errorf("debuginfod: read section %q from cached debuginfo: %w", sectionName, err)
	}
	return data, nil
}

func (c *Client) evictDebugInfo(ctx context.Context, key Key, buildID string) {
	if err := c.cache.Delete(ctx, key); err != nil {
		c.logger.Warn("debuginfo cache delete failed",
			slog.String("buildID", buildID),
			slog.Any("error", err),
		)
	}
}

// fetchSectionViaDebugInfo fetches the full debuginfo for buildID, slices out the requested section,
// and caches the section if a cache is configured.
func (c *Client) fetchSectionViaDebugInfo(ctx context.Context, buildID, sectionName string) (io.ReadCloser, error) {
	debugRC, err := c.FetchDebugInfo(ctx, buildID)
	if err != nil {
		return nil, err
	}
	defer debugRC.Close()

	debugInfo, ok := debugRC.(io.ReaderAt)
	if !ok {
		data, err := io.ReadAll(debugRC)
		if err != nil {
			return nil, fmt.Errorf("debuginfod: read debuginfo for section %q: %w", sectionName, err)
		}
		debugInfo = bytes.NewReader(data)
	}

	elfFile, err := elf.NewFile(debugInfo)
	if err != nil {
		return nil, fmt.Errorf("debuginfod: parse debuginfo for section %q: %w", sectionName, err)
	}
	defer elfFile.Close()

	section := elfFile.Section(sectionName)
	if section == nil {
		return nil, ErrNotFound
	}

	sectionData, err := io.ReadAll(section.Open())
	if err != nil {
		return nil, fmt.Errorf("debuginfod: read section %q: %w", sectionName, err)
	}

	if c.cache != nil {
		sectionKey := Key{BuildID: buildID, Kind: KindSection, Qualifier: sectionName}
		if err := putReader(ctx, c.cache, sectionKey, bytes.NewReader(sectionData)); err != nil {
			c.logger.Warn("section cache put failed",
				slog.String("buildID", buildID),
				slog.String("section", sectionName),
				slog.Any("error", err),
			)
		}
	}

	c.logger.Debug("section sliced from fetched debuginfo",
		slog.String("buildID", buildID),
		slog.String("section", sectionName),
	)
	return io.NopCloser(bytes.NewReader(sectionData)), nil
}
