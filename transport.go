package debuginfod

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

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

// fanout starts one goroutine per configured server, returning the first 200 response and aborting the rest.
func (c *Client) fanout(ctx context.Context, urlPath string) (io.ReadCloser, error) {
	serverCount := len(c.serverURLs)
	cancels := make([]context.CancelFunc, serverCount)
	results := make(chan serverResult, serverCount)

	for i, serverURL := range c.serverURLs {
		serverCtx, cancel := context.WithCancel(ctx) // #nosec G118 -- cancel is stored in cancels[i] and invoked via drainLosers or the all-error cleanup loop below
		cancels[i] = cancel
		go func(i int, serverURL string, ctx context.Context) {
			body, responded, err := c.fetchFromServer(ctx, serverURL, urlPath)
			results <- serverResult{i, body, err, responded}
		}(i, serverURL, serverCtx)
	}

	var errs []error
	responded := false
	for i := 0; i < serverCount; i++ {
		result := <-results
		server := c.serverURLs[result.idx]
		if result.body != nil {
			c.logger.Debug("fetched from server", slog.String("server", server), slog.String("path", urlPath))
			go drainLosers(results, cancels, result.idx, serverCount-i-1)
			return &cancelOnClose{ReadCloser: result.body, cancel: cancels[result.idx]}, nil
		}
		if result.responded {
			responded = true
			c.logger.Debug("server reports artifact absent", slog.String("server", server), slog.Any("error", result.err))
		} else {
			c.logger.Debug("server unreachable", slog.String("server", server), slog.Any("error", result.err))
		}
		errs = append(errs, result.err)
	}
	for _, cancel := range cancels {
		cancel()
	}
	if responded {
		return nil, ErrNotFound
	}
	return nil, errors.Join(errs...)
}

type serverResult struct {
	idx       int
	body      io.ReadCloser
	err       error
	responded bool
}

func drainLosers(results <-chan serverResult, cancels []context.CancelFunc, winner, remaining int) {
	for i, cancel := range cancels {
		if i != winner {
			cancel()
		}
	}
	for i := 0; i < remaining; i++ {
		result := <-results
		if result.body != nil {
			_ = result.body.Close()
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
