package debuginfod

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

const (
	modulePath = "go.kacmar.sk/debuginfod"
)

var defaultUserAgent = sync.OnceValue(func() string {
	version := "unknown"
	if info, ok := debug.ReadBuildInfo(); ok {
		if info.Main.Path == modulePath {
			version = info.Main.Version
		} else {
			for _, dep := range info.Deps {
				if dep.Path == modulePath {
					version = dep.Version
					break
				}
			}
		}
	}
	return modulePath + "/" + version
})

// Client is a debuginfod client.
//
// A Client queries one or more upstream debuginfod servers, optionally backed by a cache.
// A Client is safe for concurrent use.
type Client struct {
	source source
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
		transport := http.DefaultTransport
		if t, ok := http.DefaultTransport.(*http.Transport); ok {
			transport = t.Clone()
		}
		httpClient = &http.Client{Transport: transport}
	}

	maxRetries := opts.HTTP.MaxRetries
	if maxRetries < 0 {
		return nil, fmt.Errorf("debuginfod: MaxRetries must be non-negative, got %d", maxRetries)
	}

	backoff := opts.HTTP.Backoff
	if backoff == nil {
		backoff, err = ExponentialBackoff(defaultBaseBackoff, defaultMaxBackoff)
		if err != nil {
			return nil, err
		}
	}

	userAgent := opts.HTTP.UserAgent
	if userAgent == "" {
		userAgent = defaultUserAgent()
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	servers := make([]source, len(serverURLs))
	for i, serverURL := range serverURLs {
		servers[i] = &httpSource{
			serverURL:  serverURL,
			httpClient: httpClient,
			userAgent:  userAgent,
		}
	}

	network := newRetrying(newAuthoritativeRace(servers, logger), maxRetries, backoff, logger)
	var pipeline source
	if opts.Cache != nil {
		pipeline = newChain([]source{opts.Cache, newTeeing(network, opts.Cache, logger)}, logger)
	} else {
		pipeline = network
	}

	return &Client{source: pipeline}, nil
}

func (c *Client) fetch(ctx context.Context, key Key) (io.ReadCloser, error) {
	key.BuildID = strings.ToLower(key.BuildID)
	if err := key.Validate(); err != nil {
		return nil, err
	}
	return c.source.Fetch(ctx, key)
}

// FetchDebugInfo fetches the debug info file for buildID.
func (c *Client) FetchDebugInfo(ctx context.Context, buildID string) (io.ReadCloser, error) {
	return c.fetch(ctx, Key{BuildID: buildID, Kind: KindDebugInfo})
}

// FetchExecutable fetches the executable for buildID.
func (c *Client) FetchExecutable(ctx context.Context, buildID string) (io.ReadCloser, error) {
	return c.fetch(ctx, Key{BuildID: buildID, Kind: KindExecutable})
}

// FetchSource fetches a source file for buildID identified by its absolute sourcePath.
func (c *Client) FetchSource(ctx context.Context, buildID string, sourcePath string) (io.ReadCloser, error) {
	return c.fetch(ctx, Key{BuildID: buildID, Kind: KindSource, Qualifier: sourcePath})
}

// FetchSection fetches sectionName for buildID.
func (c *Client) FetchSection(ctx context.Context, buildID string, sectionName string) (io.ReadCloser, error) {
	return c.fetch(ctx, Key{BuildID: buildID, Kind: KindSection, Qualifier: sectionName})
}
