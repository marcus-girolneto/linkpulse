package robots

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// Fetch downloads and parses robots.txt for site's origin. The returned rules
// are always usable, a non-nil error only explains the fallback.
func Fetch(ctx context.Context, client *http.Client, site *url.URL, userAgent string) (*Rules, error) {
	robotsURL := url.URL{Scheme: site.Scheme, Host: site.Host, Path: "/robots.txt"}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, robotsURL.String(), nil)
	if err != nil {
		return AllowAll, fmt.Errorf("robots.txt: %w", err)
	}
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	resp, err := client.Do(req)
	if err != nil {
		return AllowAll, fmt.Errorf("robots.txt unreachable, proceeding without it: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return Parse(resp.Body, userAgent), nil
	case resp.StatusCode >= 500:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxSize))
		return DisallowAll, fmt.Errorf("robots.txt returned %d, treating the site as disallowed (use --no-robots to override)", resp.StatusCode)
	default:
		return AllowAll, nil
	}
}
