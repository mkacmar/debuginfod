package debuginfod

import (
	"bytes"
	"context"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
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

// Options configures a Client.
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

// HTTPOptions configures the HTTP client behavior for debuginfod requests.
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
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("debuginfod: invalid server URL %q: %w", raw, err)
		}
		scheme := strings.ToLower(u.Scheme)
		if scheme != "http" && scheme != "https" {
			return nil, fmt.Errorf("debuginfod: server URL %q must use http or https", raw)
		}
		if u.Host == "" {
			return nil, fmt.Errorf("debuginfod: server URL %q has no host", raw)
		}
		u.Scheme = scheme
		u.Host = strings.ToLower(u.Host)
		u.Path = strings.TrimRight(u.Path, "/")
		n := u.String()
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out, nil
}

func validateBuildID(buildID string) (string, error) {
	if buildID == "" {
		return "", fmt.Errorf("debuginfod: build ID is empty")
	}
	if len(buildID)%2 != 0 {
		return "", fmt.Errorf("debuginfod: build ID has odd length: %q", buildID)
	}
	for _, c := range buildID {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return "", fmt.Errorf("debuginfod: build ID contains invalid character: %q", c)
		}
	}
	return strings.ToLower(buildID), nil
}

// FetchDebugInfo fetches the debug info file for the given build ID.
// Returns an io.ReadCloser the caller must close.
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
	// Fetch the full debuginfo and slice the section from it.
	c.logger.Debug("section endpoint unavailable, falling back to full debuginfo",
		slog.String("buildID", id),
		slog.String("section", sectionName),
	)
	return c.fetchSectionViaDebugInfo(ctx, id, sectionName)
}

// fetchSectionViaDebugInfo fetches the full debuginfo for buildID, slices out the requested section,
// and caches the section if a cache is configured.
func (c *Client) fetchSectionViaDebugInfo(ctx context.Context, buildID, sectionName string) (io.ReadCloser, error) {
	debugRC, err := c.FetchDebugInfo(ctx, buildID)
	if err != nil {
		return nil, err
	}
	defer debugRC.Close()

	ra, ok := debugRC.(io.ReaderAt)
	if !ok {
		data, err := io.ReadAll(debugRC)
		if err != nil {
			return nil, fmt.Errorf("debuginfod: read debuginfo for section %q: %w", sectionName, err)
		}
		ra = bytes.NewReader(data)
	}

	elfFile, err := elf.NewFile(ra)
	if err != nil {
		return nil, fmt.Errorf("debuginfod: parse debuginfo for section %q: %w", sectionName, err)
	}
	defer elfFile.Close()

	sec := elfFile.Section(sectionName)
	if sec == nil {
		return nil, ErrNotFound
	}

	sectionData, err := io.ReadAll(sec.Open())
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

// tryLocalSection serves a section from cache or by slicing cached debuginfo.
// A nil return for both values means the caller should go to the network.
func (c *Client) tryLocalSection(ctx context.Context, buildID, sectionName string) (io.ReadCloser, error) {
	sectionKey := Key{BuildID: buildID, Kind: KindSection, Qualifier: sectionName}
	rc, err := c.cache.Get(ctx, sectionKey)
	if err == nil {
		c.logger.Debug("section cache hit", slog.String("buildID", buildID), slog.String("section", sectionName))
		return rc, nil
	}
	if !errors.Is(err, ErrNotFound) {
		c.logger.Warn("section cache get failed",
			slog.String("buildID", buildID),
			slog.String("section", sectionName),
			slog.Any("error", err),
		)
	}

	debugKey := Key{BuildID: buildID, Kind: KindDebugInfo}
	debugRC, err := c.cache.Get(ctx, debugKey)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			c.logger.Warn("debuginfo cache get failed",
				slog.String("buildID", buildID),
				slog.Any("error", err),
			)
		}
		return nil, nil
	}
	defer debugRC.Close()

	ra, ok := debugRC.(io.ReaderAt)
	if !ok {
		c.logger.Debug("cached debuginfo not seekable, buffering for local slice",
			slog.String("buildID", buildID),
		)
		data, err := io.ReadAll(debugRC)
		if err != nil {
			c.logger.Warn("cached debuginfo read failed",
				slog.String("buildID", buildID),
				slog.Any("error", err),
			)
			return nil, nil
		}
		ra = bytes.NewReader(data)
	}

	elfFile, err := elf.NewFile(ra)
	if err != nil {
		c.logger.Warn("cached debuginfo not parseable as ELF, evicting",
			slog.String("buildID", buildID),
			slog.Any("error", err),
		)
		if err := c.cache.Delete(ctx, debugKey); err != nil {
			c.logger.Warn("debuginfo cache delete failed",
				slog.String("buildID", buildID),
				slog.Any("error", err),
			)
		}
		return nil, nil
	}
	defer elfFile.Close()

	sec := elfFile.Section(sectionName)
	if sec == nil {
		c.logger.Debug("section not in cached debuginfo",
			slog.String("buildID", buildID),
			slog.String("section", sectionName),
		)
		return nil, ErrNotFound
	}

	data, err := io.ReadAll(sec.Open())
	if err != nil {
		c.logger.Warn("cached debuginfo section read failed, evicting",
			slog.String("buildID", buildID),
			slog.String("section", sectionName),
			slog.Any("error", err),
		)
		if err := c.cache.Delete(ctx, debugKey); err != nil {
			c.logger.Warn("debuginfo cache delete failed",
				slog.String("buildID", buildID),
				slog.Any("error", err),
			)
		}
		return nil, fmt.Errorf("debuginfod: read section %q from cached debuginfo: %w", sectionName, err)
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

// ExponentialBackoff returns a backoff function using the "Full Jitter" algorithm.
// See https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/
func ExponentialBackoff(baseDelay, maxDelay time.Duration) (func(retry int) time.Duration, error) {
	if baseDelay <= 0 {
		return nil, fmt.Errorf("debuginfod: ExponentialBackoff: baseDelay must be positive, got %s", baseDelay)
	}
	if maxDelay < baseDelay {
		return nil, fmt.Errorf("debuginfod: ExponentialBackoff: maxDelay (%s) must be >= baseDelay (%s)", maxDelay, baseDelay)
	}
	return func(retry int) time.Duration {
		if retry < 1 {
			return 0
		}
		delay := time.Duration(1<<uint(retry-1)) * baseDelay
		if delay > maxDelay || delay <= 0 {
			delay = maxDelay
		}
		jitter := time.Duration(rand.Int64N(int64(delay))) // #nosec G404 -- jitter, not security
		return jitter
	}, nil
}

// urlEscapeSourcePath URL-escapes each "/"-separated segment, preserving the separators.
func urlEscapeSourcePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
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
	pr, pw := io.Pipe()

	go func() {
		_, copyErr := io.Copy(io.MultiWriter(entry, pw), body)
		_ = body.Close()
		if copyErr != nil {
			_ = entry.Close()
			_ = pw.CloseWithError(copyErr)
			c.logger.Debug("cache stream aborted", slog.String("key", key.String()), slog.Any("error", copyErr))
			return
		}
		if cerr := entry.Commit(); cerr != nil {
			c.logger.Warn("cache commit failed", slog.String("key", key.String()), slog.Any("error", cerr))
		} else {
			c.logger.Debug("cached artifact", slog.String("key", key.String()))
		}
		_ = entry.Close()
		_ = pw.Close()
	}()

	return &cacheStreamReader{pr: pr, body: body}
}

// cacheStreamReader is the caller's view of a fetch that is being teed into a cache entry.
// Closing it before EOF unblocks the copy goroutine, which then discards the staged entry.
type cacheStreamReader struct {
	pr   *io.PipeReader
	body io.Closer
}

func (r *cacheStreamReader) Read(p []byte) (int, error) {
	return r.pr.Read(p)
}

func (r *cacheStreamReader) Close() error {
	_ = r.body.Close()
	return r.pr.Close()
}

// fetchFromServers queries the configured servers in parallel.
// Network failures (no HTTP response from any server) are retried with exponential backoff up to MaxRetries.
// Any non-200 HTTP response from any server is treated as authoritative "artifact absent" and short-circuits to ErrNotFound.
func (c *Client) fetchFromServers(ctx context.Context, urlPath string) (io.ReadCloser, error) {
	for retry := 0; ; retry++ {
		rc, err := c.fanout(ctx, urlPath)
		if err == nil {
			return rc, nil
		}
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		if retry >= c.maxRetries {
			return nil, fmt.Errorf("debuginfod: all servers unreachable: %w", err)
		}
		delay := c.backoff(retry + 1)
		c.logger.Debug("retrying after backoff",
			slog.Int("retry", retry+1),
			slog.Duration("backoff", delay),
		)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
}

type serverResult struct {
	idx       int
	body      io.ReadCloser
	err       error
	responded bool
}

// fanout starts one goroutine per configured server, returning the first 200 response and aborting the rest.
func (c *Client) fanout(ctx context.Context, urlPath string) (io.ReadCloser, error) {
	n := len(c.serverURLs)
	cancels := make([]context.CancelFunc, n)
	ch := make(chan serverResult, n)

	for i, s := range c.serverURLs {
		serverCtx, cancel := context.WithCancel(ctx) // #nosec G118 -- cancel is stored in cancels[i] and invoked via drainLosers or the all-error cleanup loop below
		cancels[i] = cancel
		go func(i int, s string, ctx context.Context) {
			rc, responded, err := c.fetchFromServer(ctx, s, urlPath)
			ch <- serverResult{i, rc, err, responded}
		}(i, s, serverCtx)
	}

	var errs []error
	responded := false
	for i := 0; i < n; i++ {
		r := <-ch
		server := c.serverURLs[r.idx]
		if r.body != nil {
			c.logger.Debug("fetched from server", slog.String("server", server), slog.String("path", urlPath))
			go drainLosers(ch, cancels, r.idx, n-i-1)
			return &cancelOnClose{ReadCloser: r.body, cancel: cancels[r.idx]}, nil
		}
		if r.responded {
			responded = true
			c.logger.Debug("server reports artifact absent", slog.String("server", server), slog.Any("error", r.err))
		} else {
			c.logger.Debug("server unreachable", slog.String("server", server), slog.Any("error", r.err))
		}
		errs = append(errs, r.err)
	}
	for _, cancel := range cancels {
		cancel()
	}
	if responded {
		return nil, ErrNotFound
	}
	return nil, errors.Join(errs...)
}

func drainLosers(ch <-chan serverResult, cancels []context.CancelFunc, winner, remaining int) {
	for i, cancel := range cancels {
		if i != winner {
			cancel()
		}
	}
	for i := 0; i < remaining; i++ {
		r := <-ch
		if r.body != nil {
			_ = r.body.Close()
		}
	}
}

// cancelOnClose ties a context cancel to the body's Close, so the winning request's HTTP connection is released only once the caller is done reading.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

func (c *Client) fetchFromServer(ctx context.Context, serverURL, urlPath string) (io.ReadCloser, bool, error) {
	endpoint := fmt.Sprintf("%s/buildid/%s", strings.TrimSuffix(serverURL, "/"), urlPath)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", serverURL, err)
	}
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", serverURL, err)
	}

	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, true, fmt.Errorf("%s: server returned %d", serverURL, resp.StatusCode)
	}

	return resp.Body, true, nil
}
