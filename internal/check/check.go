// Package check performs the HTTP side of link checking: it issues requests
// through a politeness gate, follows redirects by hand so the chain can be
// reported, falls back from HEAD to GET when servers reject HEAD, and retries
// transient failures with exponential backoff.
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

// Gate grants permission to send a request to a host. The returned release
// function is called once the response body has been closed.
type Gate interface {
	Acquire(ctx context.Context, host string) (release func(), err error)
}

// Config tunes a Checker. Zero values fall back to the defaults noted on
// each field.
type Config struct {
	// UserAgent is sent with every request.
	UserAgent string
	// Timeout bounds a single attempt, including reading the body.
	// Default 10s.
	Timeout time.Duration
	// Retries is the number of additional attempts after a transient
	// failure (network error, timeout, 429, 502, 503, 504). Negative
	// disables retries.
	Retries int
	// MaxRedirects is the longest redirect chain that is followed.
	// Default 10.
	MaxRedirects int
	// Backoff is the base delay between retries; attempt n waits a
	// jittered Backoff * 2^n. Default 250ms.
	Backoff time.Duration
	// MaxRetryAfter is the longest Retry-After the checker is willing to
	// honor; longer waits are reported as failures instead. Default 10s.
	MaxRetryAfter time.Duration
	// MaxBodyBytes caps how much of an HTML page is read when fetching.
	// Default 5 MiB.
	MaxBodyBytes int64
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
	// Status is the HTTP status of the final response, or 0 if no response
	// was received.
	Status int
	// Method is the HTTP method of the final request.
	Method string
	// Redirects lists every redirect response that was followed, in order.
	Redirects []Hop
	// FinalURL is the URL that produced the final response.
	FinalURL *url.URL
	// ContentType is the media type of the final response, without
	// parameters.
	ContentType string
	// Body holds the response body when Fetch was requested and the final
	// response is a successful HTML document.
	Body []byte
	// Attempts counts every request sent, across retries and redirects.
	Attempts int
	// Duration is the wall time spent on the check, including waits.
	Duration time.Duration
	// Err is set when no usable final response was obtained.
	Err error
}

// Checker checks URLs over HTTP. It is safe for concurrent use.
type Checker struct {
	client *http.Client
	gate   Gate
	cfg    Config
	sleep  func(ctx context.Context, d time.Duration) error
}

// New returns a Checker that sends requests with a copy of client, waiting
// on gate before each request. client's redirect policy is replaced because
// the Checker follows redirects itself.
func New(client *http.Client, gate Gate, cfg Config) *Checker {
	cfg.setDefaults()
	cl := *client
	cl.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Checker{client: &cl, gate: gate, cfg: cfg, sleep: sleepCtx}
}

// Check requests u. With fetch set it uses GET and, if the final response
// is a successful HTML page, returns its body. Otherwise it sends HEAD and
// repeats the check with GET if the server answers 405 or 501.
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
	return status == http.StatusMethodNotAllowed || status == http.StatusNotImplemented
}

func (c *Checker) follow(ctx context.Context, method string, u *url.URL, fetch bool) Result {
	res := Result{Method: method}
	cur := u
	seen := map[string]bool{cur.String(): true}
	for {
		// The body is closed by discard on every path below; a deferred close
		// would hold connections and gate permits across redirect hops.
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

// doWithRetry sends one request, retrying transient failures. The returned
// response body releases the gate and the attempt's context when closed.
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

// discard drains a small amount of the body so the connection can be reused,
// then closes it.
func discard(body io.ReadCloser) {
	_, _ = io.CopyN(io.Discard, body, 64<<10)
	_ = body.Close()
}

func (c *Checker) backoff(attempt int) time.Duration {
	d := c.cfg.Backoff << min(attempt, 10)
	// Jitter spreads retries from many workers hitting the same host; it
	// does not need a cryptographic source.
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

// retryAfter parses a Retry-After header given as delay-seconds or as an
// HTTP date.
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

// unwrapURLError strips the *url.Error wrapper, whose message repeats the
// method and URL that the report already shows.
func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
