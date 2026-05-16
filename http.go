package debuginfod

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// httpSource fetches artifacts from a single debuginfod server over HTTP.
//
// 200 returns the response body.
// 401 and 403 produce ErrAuthRequired, other 4xx produce ErrNotFound.
// 5xx, 429, and transport failures bubble up as retryable errors.
type httpSource struct {
	serverURL  string
	httpClient *http.Client
	userAgent  string
}

func (s *httpSource) Fetch(ctx context.Context, key Key) (io.ReadCloser, error) {
	endpoint := s.serverURL + key.URLPath()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.serverURL, err)
	}
	req.Header.Set("User-Agent", s.userAgent)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.serverURL, err)
	}

	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			return nil, fmt.Errorf("%s: server returned %d", s.serverURL, resp.StatusCode)
		}
		sentinel := ErrNotFound
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			sentinel = ErrAuthRequired
		}
		return nil, fmt.Errorf("%s: server returned %d: %w", s.serverURL, resp.StatusCode, sentinel)
	}

	return resp.Body, nil
}

// normalizeServerURLs validates and canonicalizes server URLs, removing duplicates.
func normalizeServerURLs(urls []string) ([]string, error) {
	seen := make(map[string]struct{}, len(urls))
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
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	return out, nil
}
