package debuginfod

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"go.kacmar.sk/debuginfod/key"
)

// upstream fetches artifacts from a single debuginfod server.
// 200 returns the response body, 401/403 produce ErrAuthRequired, other 4xx produce ErrNotFound, 5xx/429/transport failures bubble up as retryable errors.
type upstream struct {
	serverURL  string
	httpClient *http.Client
	userAgent  string
}

func (s *upstream) Fetch(ctx context.Context, k key.Key) (io.ReadCloser, error) {
	endpoint := s.serverURL + buildURLPath(k)

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

// buildURLPath returns the debuginfod URL path for k. It assumes k is already validated.
func buildURLPath(k key.Key) string {
	base := "/buildid/" + k.BuildID() + "/" + k.Kind().String()
	switch k.Kind() {
	case key.KindSource:
		return base + escapeSourcePath(k.Qualifier())
	case key.KindSection:
		return base + "/" + url.PathEscape(k.Qualifier())
	}
	return base
}

func escapeSourcePath(path string) string {
	parts := strings.Split(path, "/")
	for i, segment := range parts {
		parts[i] = url.PathEscape(segment)
	}
	return strings.Join(parts, "/")
}

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
