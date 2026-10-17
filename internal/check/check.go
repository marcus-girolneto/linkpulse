// Package check sends the HTTP requests that check links.
package check

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/marcus-girolneto/linkpulse/internal/urlnorm"
)

// Sentinel errors reported in Result.Err.
var (
	ErrTooManyRedirects = errors.New("too many redirects")
	ErrRedirectLoop     = errors.New("redirect loop")
	ErrTimeout          = errors.New("timeout")
)

// Gate grants permission to send a request to a host.
type Gate interface {
	Acquire(ctx context.Context, host string) (release func(), err error)
}

// Config tunes a Checker. Zero values get defaults.
type Config struct {
	UserAgent     string
	Timeout       time.Duration // per attempt, default 10s
	Retries       int
	MaxRedirects  int           // default 10
	Backoff       time.Duration // base retry delay, default 250ms
	MaxRetryAfter time.Duration // longer Retry-After counts as failure, default 10s
	MaxBodyBytes  int64         // default 5 MiB
}

func (c *Config) setDefaults() {
	if c.Timeout <= 0 {
		c.Timeout = 10 * time.Second
	}
	if c.MaxRedirects <= 0 {
		c.MaxRedirects = 10
	}
	if c.Backoff <= 0 {
		c.Backoff = 250 * time.Millisecond
	}
	if c.MaxRetryAfter <= 0 {
		c.MaxRetryAfter = 10 * time.Second
	}
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = 5 << 20
	}
	if c.Retries < 0 {
		c.Retries = 0
	}
}

// Hop is one redirect response in a chain.
type Hop struct {
	URL    string `json:"url"`
	Status int    `json:"status"`
}

// Result describes the outcome of checking one URL.
type Result struct {
	Status      int // 0 if no response
	Method      string
	Redirects   []Hop
	FinalURL    *url.URL
	ContentType string
	Body        []byte // only for fetched HTML pages
	Attempts    int
	Duration    time.Duration
	Err         error
}

// Checker checks URLs over HTTP. It is safe for concurrent use.
type Checker struct {
	client *http.Client
	gate   Gate
	cfg    Config
	sleep  func(ctx context.Context, d time.Duration) error
}

// New returns a Checker using a copy of client with redirects disabled.
func New(client *http.Client, gate Gate, cfg Config) *Checker {
	cfg.setDefaults()
	cl := *client
	cl.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Checker{client: &cl, gate: gate, cfg: cfg, sleep: sleepCtx}
}

// Check requests u. With fetch it GETs and returns HTML bodies, otherwise it
// sends HEAD and confirms error statuses (except 429) with GET.
func (c *Checker) Check(ctx context.Context, u *url.URL, fetch bool) Result {
	start := time.Now()
	method := http.MethodHead
	if fetch {
		method = http.MethodGet
	}
	res := c.follow(ctx, method, u, fetch)
	if method == http.MethodHead && res.Err == nil && headRejected(res.Status) {
		attempts := res.Attempts
		res = c.follow(ctx, http.MethodGet, u, false)
		res.Attempts += attempts
	}
	res.Duration = time.Since(start)
	return res
}

func headRejected(status int) bool {
	return status >= 400 && status != http.StatusTooManyRequests
}

func (c *Checker) follow(ctx context.Context, method string, u *url.URL, fetch bool) Result {
	res := Result{Method: method}
	cur := u
	seen := map[string]bool{cur.String(): true}
	for {
		// closed by discard below, not deferred, so permits aren't held across hops
		resp, attempts, err := c.doWithRetry(ctx, method, cur) //nolint:bodyclose // see above
		res.Attempts += attempts
		if err != nil {
			res.Err = err
			return res
		}

		if loc := resp.Header.Get("Location"); isRedirect(resp.StatusCode) && loc != "" {
			discard(resp.Body)
			res.Redirects = append(res.Redirects, Hop{URL: cur.String(), Status: resp.StatusCode})
			if len(res.Redirects) > c.cfg.MaxRedirects {
				res.Err = fmt.Errorf("%w (more than %d)", ErrTooManyRedirects, c.cfg.MaxRedirects)
				return res
			}
			next, err := urlnorm.Resolve(cur, loc)
			if err != nil {
				res.Err = fmt.Errorf("invalid redirect location %q: %w", loc, err)
				return res
			}
			if seen[next.String()] {
				res.Err = fmt.Errorf("%w at %s", ErrRedirectLoop, next)
				return res
			}
			seen[next.String()] = true
			cur = next
			continue
		}

		res.Status = resp.StatusCode
		res.FinalURL = cur
		res.ContentType = mediaType(resp.Header.Get("Content-Type"))
		if fetch && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			res.Body, res.Err = c.readHTML(resp, &res.ContentType)
		}
		discard(resp.Body)
		return res
	}
}

func (c *Checker) readHTML(resp *http.Response, contentType *string) ([]byte, error) {
	if *contentType != "" && !isHTML(*contentType) {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.cfg.MaxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if *contentType == "" {
		*contentType = mediaType(http.DetectContentType(body))
		if !isHTML(*contentType) {
			return nil, nil
		}
	}
	return body, nil
}

func (c *Checker) doWithRetry(ctx context.Context, method string, u *url.URL) (*http.Response, int, error) {
	for attempt := 0; ; attempt++ {
		resp, err := c.do(ctx, method, u)
		last := attempt >= c.cfg.Retries
		if err != nil {
			if ctx.Err() != nil {
				return nil, attempt + 1, ctx.Err()
			}
			if last || !retryableErr(err) {
				return nil, attempt + 1, err
			}
			if err := c.sleep(ctx, c.backoff(attempt)); err != nil {
				return nil, attempt + 1, err
			}
			continue
		}
		if last || !retryableStatus(resp.StatusCode) {
			return resp, attempt + 1, nil
		}
		wait := c.backoff(attempt)
		if ra, ok := retryAfter(resp.Header.Get("Retry-After"), time.Now()); ok {
			if ra > c.cfg.MaxRetryAfter {
				return resp, attempt + 1, nil
			}
			wait = max(wait, ra)
		}
		discard(resp.Body)
		if err := c.sleep(ctx, wait); err != nil {
			return nil, attempt + 1, err
		}
	}
}

func (c *Checker) do(ctx context.Context, method string, u *url.URL) (*http.Response, error) {
	release, err := c.gate.Acquire(ctx, u.Host)
	if err != nil {
		return nil, err
	}
	actx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	req, err := http.NewRequestWithContext(actx, method, u.String(), nil)
	if err != nil {
		cancel()
		release()
		return nil, fmt.Errorf("build request: %w", err)
	}
	if c.cfg.UserAgent != "" {
		req.Header.Set("User-Agent", c.cfg.UserAgent)
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")

	resp, err := c.client.Do(req)
	if err != nil {
		timedOut := errors.Is(actx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
		cancel()
		release()
		if timedOut {
			return nil, fmt.Errorf("%w after %s", ErrTimeout, c.cfg.Timeout)
		}
		return nil, unwrapURLError(err)
	}
	resp.Body = &releasingBody{ReadCloser: resp.Body, done: func() { cancel(); release() }}
	return resp, nil
}

type releasingBody struct {
	io.ReadCloser
	done func()
	once bool
}

func (b *releasingBody) Close() error {
	err := b.ReadCloser.Close()
	if !b.once {
		b.once = true
		b.done()
	}
	return err
}

func discard(body io.ReadCloser) {
	_, _ = io.CopyN(io.Discard, body, 64<<10)
	_ = body.Close()
}

func (c *Checker) backoff(attempt int) time.Duration {
	d := c.cfg.Backoff << min(attempt, 10)
	return d/2 + rand.N(d/2+1) //nolint:gosec // jitter only
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func isRedirect(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

func retryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func retryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(t.Sub(now), 0), true
	}
	return 0, false
}

func mediaType(ct string) string {
	if ct == "" {
		return ""
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		mt, _, _ = strings.Cut(ct, ";")
		mt = strings.ToLower(strings.TrimSpace(mt))
	}
	return mt
}

func isHTML(mt string) bool {
	return mt == "text/html" || mt == "application/xhtml+xml"
}

func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
