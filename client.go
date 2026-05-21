package debuginfod

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"go.kacmar.sk/debuginfod/key"
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

// source is a read-only origin for debuginfod artifacts.
// An error wrapping ErrNotFound means the source authoritatively reports the artifact as absent.
// Transport errors are retryable.
type source interface {
	Fetch(ctx context.Context, k key.Key) (io.ReadCloser, error)
}

// Client queries one or more upstream debuginfod servers in parallel and returns the first body-bearing response.
type Client struct {
	source source
}

type Options struct {
	// ServerURLs is the list of debuginfod server base URLs to query in parallel.
	ServerURLs []string

	HTTP HTTPOptions

	// Logger is an optional structured logger. If nil, logging is disabled.
	Logger *slog.Logger
}

type HTTPOptions struct {
	// Client is the HTTP client used for debuginfod requests.
	// Supply a custom client to inject authentication, configure TLS, or set per-attempt timeouts.
	// If nil, a client wrapping a cloned http.DefaultTransport is used.
	Client *http.Client

	// MaxRetries is the maximum number of additional retry rounds after the initial attempt.
	MaxRetries int

	// Backoff returns the delay before retry round n (starting at 1).
	// If nil, a default exponential backoff with jitter is used.
	Backoff func(retry int) time.Duration

	// UserAgent is the User-Agent header sent on every request.
	// If empty, defaults to "go.kacmar.sk/debuginfod/<version>".
	UserAgent string
}

// NewClient validates opts and constructs a Client.
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
		servers[i] = &upstream{
			serverURL:  serverURL,
			httpClient: httpClient,
			userAgent:  userAgent,
		}
	}

	pipeline := newRetrier(newRace(servers, logger), maxRetries, backoff, logger)

	return &Client{source: pipeline}, nil
}

// Fetch streams the artifact for k from the configured servers.
// It returns ErrNotFound if no upstream has the artifact, ErrAuthRequired if at least one upstream needed auth and no other source could satisfy it, or a wrapped transport error after retries are exhausted.
func (c *Client) Fetch(ctx context.Context, k key.Key) (io.ReadCloser, error) {
	if err := k.Validate(); err != nil {
		return nil, err
	}
	return c.source.Fetch(ctx, k)
}
