package check

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type countingGate struct {
	acquired atomic.Int32
	inFlight atomic.Int32
}

func (g *countingGate) Acquire(ctx context.Context, _ string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g.acquired.Add(1)
	g.inFlight.Add(1)
	var once sync.Once
	return func() { once.Do(func() { g.inFlight.Add(-1) }) }, nil
}

type sleepRecorder struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (s *sleepRecorder) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.waits = append(s.waits, d)
	s.mu.Unlock()
	return ctx.Err()
}

func newTestChecker(t *testing.T, cfg Config) (*Checker, *countingGate, *sleepRecorder) {
	t.Helper()
	gate := &countingGate{}
	rec := &sleepRecorder{}
	c := New(http.DefaultClient, gate, cfg)
	c.sleep = rec.sleep
	t.Cleanup(func() {
		if n := gate.inFlight.Load(); n != 0 {
			t.Errorf("%d gate permits leaked", n)
		}
	})
	return c, gate, rec
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func testServer(t *testing.T) *httptest.Server {
	t.Helper()
	var flaky, flakyRA atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<a href="/x">x</a>`)
	})
	mux.HandleFunc("/missing", http.NotFound)
	mux.HandleFunc("/no-head", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		fmt.Fprint(w, "ok")
	})
	mux.HandleFunc("/head-404", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, "single-page apps sometimes do this")
	})
	mux.HandleFunc("/head-501", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNotImplemented)
			return
		}
		w.WriteHeader(http.StatusGone)
	})
	mux.HandleFunc("/r1", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/r2", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/r2", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "r3#frag", http.StatusFound)
	})
	mux.HandleFunc("/r3", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ok", http.StatusPermanentRedirect)
	})
	mux.HandleFunc("/loop-a", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop-b", http.StatusFound)
	})
	mux.HandleFunc("/loop-b", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop-a", http.StatusFound)
	})
	mux.HandleFunc("/bad-location", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "mailto:someone")
		w.WriteHeader(http.StatusFound)
	})
	mux.HandleFunc("/redirect-no-location", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusFound)
	})
	mux.HandleFunc("/flaky", func(w http.ResponseWriter, _ *http.Request) {
		if flaky.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, "recovered")
	})
	mux.HandleFunc("/flaky-retry-after", func(w http.ResponseWriter, _ *http.Request) {
		if flakyRA.Add(1) == 1 {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, "ok")
	})
	mux.HandleFunc("/always-503", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	mux.HandleFunc("/retry-after-long", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
		fmt.Fprint(w, "late")
	})
	mux.HandleFunc("/image", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		fmt.Fprint(w, "\x89PNG")
	})
	mux.HandleFunc("/untyped", func(w http.ResponseWriter, _ *http.Request) {
		w.Header()["Content-Type"] = nil
		fmt.Fprint(w, `<!DOCTYPE html><html><body><a href="/y">y</a></body></html>`)
	})
	mux.HandleFunc("/untyped-text", func(w http.ResponseWriter, _ *http.Request) {
		w.Header()["Content-Type"] = nil
		fmt.Fprint(w, "just some text")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestCheck(t *testing.T) {
	t.Parallel()
	srv := testServer(t)

	tests := []struct {
		name         string
		path         string
		fetch        bool
		cfg          Config
		wantStatus   int
		wantMethod   string
		wantErr      error
		wantDescribe string
		wantHops     int
		wantAttempts int
		wantFinal    string
		wantBody     bool
		wantType     string
	}{
		{name: "head ok", path: "/ok", wantStatus: 200, wantMethod: "HEAD", wantAttempts: 1, wantFinal: "/ok", wantType: "text/html"},
		{name: "get ok with body", path: "/ok", fetch: true, wantStatus: 200, wantMethod: "GET", wantAttempts: 1, wantFinal: "/ok", wantBody: true, wantType: "text/html"},
		{name: "not found confirmed with get", path: "/missing", wantStatus: 404, wantMethod: "GET", wantAttempts: 2, wantFinal: "/missing"},
		{name: "head 404 but get 200", path: "/head-404", wantStatus: 200, wantMethod: "GET", wantAttempts: 2, wantFinal: "/head-404"},
		{name: "fetch not found has no body", path: "/missing", fetch: true, wantStatus: 404, wantMethod: "GET", wantAttempts: 1, wantFinal: "/missing"},
		{name: "head 405 falls back to get", path: "/no-head", wantStatus: 200, wantMethod: "GET", wantAttempts: 2, wantFinal: "/no-head"},
		{name: "head 501 falls back to get", path: "/head-501", wantStatus: 410, wantMethod: "GET", wantAttempts: 2, wantFinal: "/head-501"},
		{name: "redirect chain", path: "/r1", wantStatus: 200, wantMethod: "HEAD", wantHops: 3, wantAttempts: 4, wantFinal: "/ok", wantType: "text/html"},
		{name: "redirect chain fetch", path: "/r1", fetch: true, wantStatus: 200, wantMethod: "GET", wantHops: 3, wantAttempts: 4, wantFinal: "/ok", wantBody: true, wantType: "text/html"},
		{name: "too many redirects", path: "/r1", cfg: Config{MaxRedirects: 2}, wantErr: ErrTooManyRedirects, wantDescribe: "too many redirects", wantHops: 3, wantAttempts: 3, wantMethod: "HEAD"},
		{name: "redirect loop", path: "/loop-a", wantErr: ErrRedirectLoop, wantDescribe: "redirect loop", wantHops: 2, wantAttempts: 2, wantMethod: "HEAD"},
		{name: "redirect to unsupported scheme", path: "/bad-location", wantHops: 1, wantAttempts: 1, wantMethod: "HEAD", wantDescribe: `invalid redirect location "mailto:someone": unsupported scheme: mailto`},
		{name: "3xx without location is final", path: "/redirect-no-location", wantStatus: 302, wantMethod: "HEAD", wantAttempts: 1, wantFinal: "/redirect-no-location"},
		{name: "retry transient 503", path: "/flaky", cfg: Config{Retries: 2}, wantStatus: 200, wantMethod: "HEAD", wantAttempts: 2, wantFinal: "/flaky"},
		{name: "retries exhausted", path: "/always-503", fetch: true, cfg: Config{Retries: 2}, wantStatus: 503, wantMethod: "GET", wantAttempts: 3, wantFinal: "/always-503"},
		{name: "retry-after beyond limit not retried", path: "/retry-after-long", cfg: Config{Retries: 3}, wantStatus: 429, wantMethod: "HEAD", wantAttempts: 1, wantFinal: "/retry-after-long"},
		{name: "retries exhausted then confirmed with get", path: "/always-503", cfg: Config{Retries: 1}, wantStatus: 503, wantMethod: "GET", wantAttempts: 4, wantFinal: "/always-503"},
		{name: "timeout retried then reported", path: "/slow", cfg: Config{Timeout: 30 * time.Millisecond, Retries: 1}, wantErr: ErrTimeout, wantDescribe: "timeout", wantAttempts: 2, wantMethod: "HEAD"},
		{name: "fetch non-html skips body", path: "/image", fetch: true, wantStatus: 200, wantMethod: "GET", wantAttempts: 1, wantFinal: "/image", wantType: "image/png"},
		{name: "fetch sniffs html", path: "/untyped", fetch: true, wantStatus: 200, wantMethod: "GET", wantAttempts: 1, wantFinal: "/untyped", wantBody: true, wantType: "text/html"},
		{name: "fetch sniffs non-html", path: "/untyped-text", fetch: true, wantStatus: 200, wantMethod: "GET", wantAttempts: 1, wantFinal: "/untyped-text", wantType: "text/plain"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, gate, _ := newTestChecker(t, tt.cfg)
			res := c.Check(t.Context(), mustURL(t, srv.URL+tt.path), tt.fetch)

			if tt.wantErr != nil && !errors.Is(res.Err, tt.wantErr) {
				t.Errorf("err = %v, want %v", res.Err, tt.wantErr)
			}
			if tt.wantErr == nil && tt.wantDescribe == "" && res.Err != nil {
				t.Errorf("unexpected err: %v", res.Err)
			}
			if tt.wantDescribe != "" {
				if got := Describe(res.Err); got != tt.wantDescribe {
					t.Errorf("Describe = %q, want %q", got, tt.wantDescribe)
				}
			}
			if res.Status != tt.wantStatus {
				t.Errorf("status = %d, want %d", res.Status, tt.wantStatus)
			}
			if res.Method != tt.wantMethod {
				t.Errorf("method = %q, want %q", res.Method, tt.wantMethod)
			}
			if len(res.Redirects) != tt.wantHops {
				t.Errorf("hops = %v, want %d", res.Redirects, tt.wantHops)
			}
			if res.Attempts != tt.wantAttempts {
				t.Errorf("attempts = %d, want %d", res.Attempts, tt.wantAttempts)
			}
			if got := int(gate.acquired.Load()); got != res.Attempts {
				t.Errorf("gate acquired %d times for %d attempts", got, res.Attempts)
			}
			if tt.wantFinal != "" {
				if res.FinalURL == nil || res.FinalURL.Path != tt.wantFinal {
					t.Errorf("final = %v, want path %q", res.FinalURL, tt.wantFinal)
				}
			}
			if (len(res.Body) > 0) != tt.wantBody {
				t.Errorf("body present = %v, want %v", len(res.Body) > 0, tt.wantBody)
			}
			if tt.wantType != "" && res.ContentType != tt.wantType {
				t.Errorf("content type = %q, want %q", res.ContentType, tt.wantType)
			}
		})
	}
}

func TestCheckHonorsRetryAfter(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	c, _, rec := newTestChecker(t, Config{Retries: 1, Backoff: time.Millisecond})
	res := c.Check(t.Context(), mustURL(t, srv.URL+"/flaky-retry-after"), false)
	if res.Status != 200 {
		t.Fatalf("status = %d, want 200", res.Status)
	}
	if len(rec.waits) != 1 || rec.waits[0] != 3*time.Second {
		t.Errorf("waits = %v, want [3s]", rec.waits)
	}
}

func TestCheckRedirectHopsAreRecorded(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	c, _, _ := newTestChecker(t, Config{})
	res := c.Check(t.Context(), mustURL(t, srv.URL+"/r1"), false)
	want := []Hop{{srv.URL + "/r1", 301}, {srv.URL + "/r2", 302}, {srv.URL + "/r3", 308}}
	if fmt.Sprint(res.Redirects) != fmt.Sprint(want) {
		t.Errorf("redirects = %v, want %v", res.Redirects, want)
	}
}

func TestCheckConnectionRefusedNotRetried(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := srv.URL
	srv.Close()

	c, _, rec := newTestChecker(t, Config{Retries: 3})
	res := c.Check(t.Context(), mustURL(t, addr+"/"), false)
	if got := Describe(res.Err); got != "connection refused" {
		t.Errorf("Describe = %q (%v), want connection refused", got, res.Err)
	}
	if res.Attempts != 1 || len(rec.waits) != 0 {
		t.Errorf("attempts = %d waits = %v, want a single attempt", res.Attempts, rec.waits)
	}
}

func TestCheckCanceled(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	c := New(http.DefaultClient, &countingGate{}, Config{Retries: 3})
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	res := c.Check(ctx, mustURL(t, srv.URL+"/slow"), false)
	if !errors.Is(res.Err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", res.Err)
	}
	if res.Attempts != 1 {
		t.Errorf("attempts = %d, canceled requests must not be retried", res.Attempts)
	}
}

func TestCheckSendsUserAgent(t *testing.T) {
	t.Parallel()
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got <- r.UserAgent()
	}))
	t.Cleanup(srv.Close)
	c, _, _ := newTestChecker(t, Config{UserAgent: "linkpulse-test/1"})
	c.Check(t.Context(), mustURL(t, srv.URL), false)
	if ua := <-got; ua != "linkpulse-test/1" {
		t.Errorf("User-Agent = %q", ua)
	}
}

func TestCheckBodyLimit(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Length", strconv.Itoa(4096))
		w.Write(make([]byte, 4096))
	}))
	t.Cleanup(srv.Close)
	c, _, _ := newTestChecker(t, Config{MaxBodyBytes: 100})
	res := c.Check(t.Context(), mustURL(t, srv.URL), true)
	if len(res.Body) != 100 {
		t.Errorf("body length = %d, want 100", len(res.Body))
	}
}

type errGate struct{ err error }

func (g errGate) Acquire(context.Context, string) (func(), error) { return nil, g.err }

func TestCheckGateError(t *testing.T) {
	t.Parallel()
	boom := errors.New("gate closed")
	c := New(http.DefaultClient, errGate{boom}, Config{})
	res := c.Check(t.Context(), mustURL(t, "http://example.invalid/"), false)
	if !errors.Is(res.Err, boom) {
		t.Fatalf("err = %v, want %v", res.Err, boom)
	}
}

func TestRetryAfter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	tests := []struct {
		in     string
		want   time.Duration
		wantOK bool
	}{
		{"", 0, false},
		{"5", 5 * time.Second, true},
		{" 0 ", 0, true},
		{"-1", 0, false},
		{"soon", 0, false},
		{now.Add(30 * time.Second).Format(http.TimeFormat), 30 * time.Second, true},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0, true},
	}
	for _, tt := range tests {
		got, ok := retryAfter(tt.in, now)
		if got != tt.want || ok != tt.wantOK {
			t.Errorf("retryAfter(%q) = %v, %v; want %v, %v", tt.in, got, ok, tt.want, tt.wantOK)
		}
	}
}

func TestBackoffGrowsWithJitter(t *testing.T) {
	t.Parallel()
	c := New(http.DefaultClient, &countingGate{}, Config{Backoff: 100 * time.Millisecond})
	for attempt := range 4 {
		base := 100 * time.Millisecond << attempt
		for range 50 {
			d := c.backoff(attempt)
			if d < base/2 || d > base {
				t.Fatalf("backoff(%d) = %v, want within [%v, %v]", attempt, d, base/2, base)
			}
		}
	}
}

func TestMediaType(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"":                         "",
		"text/html; charset=utf-8": "text/html",
		"TEXT/HTML":                "text/html",
		"application/xhtml+xml":    "application/xhtml+xml",
		"text/html; charset=\"bad": "text/html",
		"image/png;;":              "image/png",
	}
	for in, want := range tests {
		if got := mediaType(in); got != want {
			t.Errorf("mediaType(%q) = %q, want %q", in, got, want)
		}
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestRetryableErr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		err  error
		want bool
	}{
		{fmt.Errorf("wrapped: %w", ErrTimeout), true},
		{io.ErrUnexpectedEOF, true},
		{&net.OpError{Op: "read", Err: syscall.ECONNRESET}, true},
		{timeoutErr{}, true},
		{&net.DNSError{IsNotFound: true}, false},
		{&net.DNSError{IsTemporary: true}, true},
		{&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, false},
		{errors.New("tls: bad certificate"), false},
	}
	for _, tt := range tests {
		if got := retryableErr(tt.err); got != tt.want {
			t.Errorf("retryableErr(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}

func TestDescribe(t *testing.T) {
	t.Parallel()
	long := errors.New(string(make([]byte, 200)))
	tests := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{context.Canceled, "canceled"},
		{context.DeadlineExceeded, "timeout"},
		{&net.DNSError{Err: "no such host", Name: "x.invalid", IsNotFound: true}, "dns: no such host"},
		{&net.DNSError{Err: "server misbehaving", Name: "x"}, "dns: lookup failed"},
		{&net.OpError{Op: "read", Err: syscall.ECONNRESET}, "connection reset"},
		{io.EOF, "connection closed by server"},
		{x509.HostnameError{Certificate: &x509.Certificate{}, Host: "x"}, "tls: certificate hostname mismatch"},
		{x509.UnknownAuthorityError{}, "tls: unknown certificate authority"},
		{x509.CertificateInvalidError{Reason: x509.Expired}, "tls: certificate expired"},
		{x509.CertificateInvalidError{Reason: x509.NotAuthorizedToSign}, "tls: invalid certificate"},
		{&tls.CertificateVerificationError{Err: errors.New("x")}, "tls: certificate verification failed"},
		{tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}, "tls: server did not speak TLS"},
		{errors.New("something odd"), "something odd"},
	}
	for _, tt := range tests {
		if got := Describe(tt.err); got != tt.want {
			t.Errorf("Describe(%v) = %q, want %q", tt.err, got, tt.want)
		}
	}
	if got := Describe(long); len(got) > 120 {
		t.Errorf("long error not truncated: %d chars", len(got))
	}
}
