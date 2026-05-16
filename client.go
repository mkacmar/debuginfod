package debuginfod

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultUserAgent = "debuginfod-go"
)

// Client is a debuginfod HTTP client with retries and exponential backoff.
//
// A Client is safe for concurrent use.
type Client struct {
	serverURLs []string
	cache      Cache
	httpClient *http.Client
	maxRetries int
	backoff    func(round int) time.Duration
	userAgent  string
	logger     *slog.Logger
}

type Options struct {
	// ServerURLs is the list of debuginfod server base URLs to query.
	// All servers are queried in parallel.
	ServerURLs []string

	// Cache is the artifact storage backend.
	// If nil, no caching is performed.
	Cache Cache

	// HTTP configures the HTTP transport and retries.
	HTTP HTTPOptions

	// Logger is an optional structured logger.
	// If nil, logging is disabled.
	Logger *slog.Logger
}

type HTTPOptions struct {
	// Client is the HTTP client used for debuginfod requests.
	// If nil, a client with a cloned http.DefaultTransport is used.
	Client *http.Client

	// MaxRetries is the maximum number of additional retry rounds after the initial attempt.
	// Zero (the default) means no retries.
	MaxRetries int

	// Backoff returns the delay before retry round n (starting at 1).
	// If nil, a default exponential backoff with jitter is used.
	Backoff func(retry int) time.Duration

	// UserAgent is the User-Agent header sent with requests.
	UserAgent string
}

// NewClient creates a new Client with the given options.
// Returns an error if no server URLs are provided or any URL is invalid.
func NewClient(opts Options) (*Client, error) {
	if len(opts.ServerURLs) == 0 {
		return nil, fmt.Errorf("debuginfod: no server URLs configured")
	}

	serverURLs, err := normalizeServerURLs(opts.ServerURLs)
	if err != nil {
		return nil, err
	}

	httpClient := opts.HTTP.Client
	if httpClient == nil {
		httpClient = &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}
	}

	maxRetries := opts.HTTP.MaxRetries
	if maxRetries < 0 {
		return nil, fmt.Errorf("debuginfod: MaxRetries must be non-negative, got %d", maxRetries)
	}

	backoff := opts.HTTP.Backoff
	if backoff == nil {
		var err error
		backoff, err = ExponentialBackoff(1*time.Second, 30*time.Second)
		if err != nil {
			return nil, err
		}
	}

	userAgent := opts.HTTP.UserAgent
	if userAgent == "" {
		userAgent = defaultUserAgent
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	return &Client{
		serverURLs: serverURLs,
		cache:      opts.Cache,
		httpClient: httpClient,
		maxRetries: maxRetries,
		backoff:    backoff,
		userAgent:  userAgent,
		logger:     logger,
	}, nil
}

// normalizeServerURLs validates and canonicalizes server URLs, removing duplicates.
func normalizeServerURLs(urls []string) ([]string, error) {
	seen := make(map[string]bool, len(urls))
	out := make([]string, 0, len(urls))
	for _, raw := range urls {
		parsed, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("debuginfod: invalid server URL %q: %w", raw, err)
		}
		scheme := strings.ToLower(parsed.Scheme)
		if scheme != "http" && scheme != "https" {
			return nil, fmt.Errorf("debuginfod: server URL %q must use http or https", raw)
		}
		if parsed.Host == "" {
			return nil, fmt.Errorf("debuginfod: server URL %q has no host", raw)
		}
		parsed.Scheme = scheme
		parsed.Host = strings.ToLower(parsed.Host)
		parsed.Path = strings.TrimRight(parsed.Path, "/")
		normalized := parsed.String()
		if seen[normalized] {
			continue
		}
		seen[normalized] = true
		out = append(out, normalized)
	}
	return out, nil
}

func validateBuildID(buildID string) (string, error) {
	lowered := strings.ToLower(buildID)
	if err := (Key{BuildID: lowered, Kind: KindDebugInfo}).validate(); err != nil {
		return "", err
	}
	return lowered, nil
}

// FetchDebugInfo fetches the debug info file for the given build ID.
func (c *Client) FetchDebugInfo(ctx context.Context, buildID string) (io.ReadCloser, error) {
	id, err := validateBuildID(buildID)
	if err != nil {
		return nil, err
	}
	return c.fetch(ctx, Key{BuildID: id, Kind: KindDebugInfo}, id+"/debuginfo")
}

// FetchExecutable fetches the executable for the given build ID.
func (c *Client) FetchExecutable(ctx context.Context, buildID string) (io.ReadCloser, error) {
	id, err := validateBuildID(buildID)
	if err != nil {
		return nil, err
	}
	return c.fetch(ctx, Key{BuildID: id, Kind: KindExecutable}, id+"/executable")
}

// FetchSource fetches a source file for the given build ID and absolute source path.
func (c *Client) FetchSource(ctx context.Context, buildID string, sourcePath string) (io.ReadCloser, error) {
	id, err := validateBuildID(buildID)
	if err != nil {
		return nil, err
	}
	if sourcePath == "" {
		return nil, fmt.Errorf("debuginfod: source path is empty")
	}
	if !strings.HasPrefix(sourcePath, "/") {
		return nil, fmt.Errorf("debuginfod: source path must be absolute (start with /): %q", sourcePath)
	}
	key := Key{BuildID: id, Kind: KindSource, Qualifier: sourcePath}
	return c.fetch(ctx, key, id+"/source"+urlEscapeSourcePath(sourcePath))
}

// FetchSection fetches a specific ELF section for the given build ID.
// If the server doesn't support the /section/ endpoint, falls back to fetching the full debuginfo and slicing the section from it.

// urlEscapeSourcePath URL-escapes each "/"-separated segment, preserving the separators.
func urlEscapeSourcePath(path string) string {
	parts := strings.Split(path, "/")
	for i, segment := range parts {
		parts[i] = url.PathEscape(segment)
	}
	return strings.Join(parts, "/")
}

func (c *Client) fetch(ctx context.Context, key Key, urlPath string) (io.ReadCloser, error) {
	if c.cache == nil {
		return c.fetchFromServers(ctx, urlPath)
	}

	rc, err := c.cache.Get(ctx, key)
	if err == nil {
		c.logger.Debug("cache hit", slog.String("key", key.String()))
		return rc, nil
	}
	if !errors.Is(err, ErrNotFound) {
		c.logger.Warn("cache get failed", slog.String("key", key.String()), slog.Any("error", err))
	}

	body, err := c.fetchFromServers(ctx, urlPath)
	if err != nil {
		return nil, err
	}

	entry, err := c.cache.Create(ctx, key)
	if err != nil {
		c.logger.Warn("cache create failed", slog.String("key", key.String()), slog.Any("error", err))
		return body, nil
	}

	return c.streamThroughCache(key, body, entry), nil
}

// streamThroughCache fans bytes from body into both the cache entry and the caller's reader.
// On clean EOF the entry is committed.
// On any error or early caller Close the entry is discarded.
func (c *Client) streamThroughCache(key Key, body io.ReadCloser, entry CacheEntry) io.ReadCloser {
	pipeReader, pipeWriter := io.Pipe()

	go func() {
		_, copyErr := io.Copy(io.MultiWriter(entry, pipeWriter), body)
		_ = body.Close()
		if copyErr != nil {
			_ = entry.Close()
			_ = pipeWriter.CloseWithError(copyErr)
			c.logger.Debug("cache stream aborted", slog.String("key", key.String()), slog.Any("error", copyErr))
			return
		}
		if cerr := entry.Commit(); cerr != nil {
			c.logger.Warn("cache commit failed", slog.String("key", key.String()), slog.Any("error", cerr))
		} else {
			c.logger.Debug("cached artifact", slog.String("key", key.String()))
		}
		_ = entry.Close()
		_ = pipeWriter.Close()
	}()

	return &cacheStreamReader{pipe: pipeReader, body: body}
}

// cacheStreamReader is the caller's view of a fetch that is being teed into a cache entry.
// Closing it before EOF unblocks the copy goroutine, which then discards the staged entry.
type cacheStreamReader struct {
	pipe *io.PipeReader
	body io.Closer
}

func (r *cacheStreamReader) Read(p []byte) (int, error) {
	return r.pipe.Read(p)
}

func (r *cacheStreamReader) Close() error {
	_ = r.body.Close()
	return r.pipe.Close()
}
