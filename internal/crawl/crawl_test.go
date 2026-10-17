package crawl

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcus-girolneto/linkpulse/internal/check"
	"github.com/marcus-girolneto/linkpulse/internal/hostlimit"
	"github.com/marcus-girolneto/linkpulse/internal/robots"
)

// site is an httptest server serving a small graph of pages and recording
// how often each path was requested.
type site struct {
	*httptest.Server
	mu   sync.Mutex
	hits map[string]int
}

func (s *site) count(method, path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[method+" "+path]
}

func newSite(t testing.TB, pages map[string]string, extra func(mux *http.ServeMux)) *site {
	t.Helper()
	s := &site{hits: make(map[string]int)}
	mux := http.NewServeMux()
	for path, body := range pages {
		if path == "/" {
			path = "/{$}"
		}
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, body)
		})
	}
	if extra != nil {
		extra(mux)
	}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits[r.Method+" "+r.URL.Path]++
		s.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func page(links ...string) string {
	var b strings.Builder
	b.WriteString("<!doctype html><html><body>")
	for _, l := range links {
		if strings.HasPrefix(l, "img:") {
			fmt.Fprintf(&b, `<img src="%s">`, strings.TrimPrefix(l, "img:"))
			continue
		}
		fmt.Fprintf(&b, `<a href="%s">link</a>`, l)
	}
	b.WriteString("</body></html>")
	return b.String()
}

// fixture builds the main test site:
//
//	/          -> /a /b /deep/1 /missing /error /redir /slow /private/secret
//	              /logo.png (img) mailto:, #frag, /a#section, external links
//	/a         -> /  /b          (cycle)
//	/b         -> /a             (cycle)
//	/deep/N    -> /deep/N+1      (chain of three)
//	/nofollow  -> /hidden        (meta robots nofollow)
func fixture(t testing.TB) (*site, *site) {
	t.Helper()
	ext := newSite(t, map[string]string{"/ok": page()}, func(mux *http.ServeMux) {
		mux.HandleFunc("/gone", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusGone)
		})
	})
	pages := map[string]string{
		"/": page("/a", "/b", "/deep/1", "/missing", "/error", "/redir", "/slow",
			"/private/secret", "img:/logo.png", "mailto:someone", "#top", "/a#section",
			"/nofollow", ext.URL+"/ok", ext.URL+"/gone"),
		"/a":      page("/", "/b"),
		"/b":      page("/a"),
		"/deep/1": page("/deep/2"),
		"/deep/2": page("/deep/3"),
		"/deep/3": page(),
		"/hidden": page(),
		"/nofollow": `<html><head><meta name="robots" content="nofollow"></head>` +
			`<body><a href="/hidden">h</a></body></html>`,
	}
	s := newSite(t, pages, func(mux *http.ServeMux) {
		mux.HandleFunc("/error", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		mux.HandleFunc("/redir", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/a", http.StatusMovedPermanently)
		})
		mux.HandleFunc("/slow", func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-time.After(2 * time.Second):
			case <-r.Context().Done():
			}
		})
		mux.HandleFunc("/logo.png", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "image/png")
		})
		mux.HandleFunc("/private/", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, "secret")
		})
	})
	return s, ext
}

func newChecker(timeout time.Duration) *check.Checker {
	gate := hostlimit.New(hostlimit.Config{Concurrency: 8})
	return check.New(&http.Client{}, gate, check.Config{Timeout: timeout, Retries: -1})
}

func mustURL(t testing.TB, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func byURL(r *Report) map[string]Link {
	m := make(map[string]Link, len(r.Links))
	for _, l := range r.Links {
		m[l.URL] = l
	}
	return m
}

func TestCrawlSite(t *testing.T) {
	t.Parallel()
	s, ext := fixture(t)
	rules := robots.Parse(strings.NewReader("User-agent: *\nDisallow: /private/\n"), "linkpulse")

	var callbacks atomic.Int32
	c := New(newChecker(150*time.Millisecond), Config{
		MaxDepth: -1,
		Workers:  8,
		Robots:   rules,
		OnResult: func(Link) { callbacks.Add(1) },
	})
	rep := c.Run(t.Context(), mustURL(t, s.URL+"/"))
	links := byURL(rep)

	if rep.Interrupted {
		t.Error("report marked interrupted")
	}
	// /, /a, /b, /deep/1-3 and /nofollow; /redir lands on /a, which is
	// parsed only once.
	if rep.Pages != 7 {
		t.Errorf("pages parsed = %d, want 7", rep.Pages)
	}

	tests := []struct {
		url      string
		status   int
		broken   bool
		page     bool
		skipped  string
		errText  string
		method   string
		finalURL string
	}{
		{url: s.URL + "/", status: 200, page: true, method: "GET"},
		{url: s.URL + "/a", status: 200, page: true, method: "GET"},
		{url: s.URL + "/deep/3", status: 200, page: true, method: "GET"},
		{url: s.URL + "/missing", status: 404, broken: true, method: "GET"},
		{url: s.URL + "/error", status: 500, broken: true, method: "GET"},
		{url: s.URL + "/slow", broken: true, errText: "timeout", method: "GET"},
		{url: s.URL + "/redir", status: 200, page: true, method: "GET", finalURL: s.URL + "/a"},
		{url: s.URL + "/private/secret", skipped: SkipRobots},
		{url: s.URL + "/logo.png", status: 200, method: "HEAD"},
		{url: s.URL + "/hidden", status: 200, method: "HEAD"},
		{url: ext.URL + "/ok", status: 200, method: "HEAD"},
		{url: ext.URL + "/gone", status: 410, broken: true, method: "GET"},
	}
	for _, tt := range tests {
		l, ok := links[tt.url]
		if !ok {
			t.Errorf("%s: missing from report", tt.url)
			continue
		}
		if l.Status != tt.status || l.Broken() != tt.broken || l.Page != tt.page ||
			l.Skipped != tt.skipped || l.Error != tt.errText || l.Method != tt.method || l.FinalURL != tt.finalURL {
			t.Errorf("%s:\n got status=%d broken=%v page=%v skipped=%q err=%q method=%q final=%q\nwant status=%d broken=%v page=%v skipped=%q err=%q method=%q final=%q",
				tt.url, l.Status, l.Broken(), l.Page, l.Skipped, l.Error, l.Method, l.FinalURL,
				tt.status, tt.broken, tt.page, tt.skipped, tt.errText, tt.method, tt.finalURL)
		}
	}

	if _, ok := links["mailto:someone"]; ok {
		t.Error("non-http link should not be reported")
	}
	if l := links[s.URL+"/missing"]; len(l.Sources) != 1 || l.Sources[0].Page != s.URL+"/" || l.Sources[0].Tag != "a" {
		t.Errorf("/missing sources = %+v, want found on /", l.Sources)
	}
	if l := links[s.URL+"/a"]; l.SourceCount != 2 {
		t.Errorf("/a found on %d pages, want 2 (/ and /b, duplicates within a page collapse)", l.SourceCount)
	}
	if n := s.count("GET", "/"); n != 1 {
		t.Errorf("GET / requested %d times; cycles must not refetch pages", n)
	}
	if n := s.count("GET", "/b"); n != 1 {
		t.Errorf("GET /b requested %d times", n)
	}
	if n := s.count("GET", "/private/secret") + s.count("HEAD", "/private/secret"); n != 0 {
		t.Errorf("robots-disallowed path requested %d times", n)
	}
	if n := s.count("GET", "/hidden"); n != 0 {
		t.Errorf("link on nofollow page was crawled %d times", n)
	}

	st := rep.Stats()
	want := Stats{Total: len(rep.Links), OK: st.Total - 4 - 1, Broken: 4, Redirected: 1, Skipped: 1}
	if st != want {
		t.Errorf("stats = %+v, want %+v", st, want)
	}
	if got := int(callbacks.Load()); got != st.OK+st.Broken {
		t.Errorf("OnResult called %d times, want %d", got, st.OK+st.Broken)
	}
}

func TestCrawlLimits(t *testing.T) {
	t.Parallel()
	s, _ := fixture(t)

	tests := []struct {
		name      string
		cfg       Config
		wantPages map[string]bool
		absent    []string
		skipped   map[string]string
	}{
		{
			name:      "depth zero checks start page links only",
			cfg:       Config{MaxDepth: 0, SkipExternal: true},
			wantPages: map[string]bool{"/": true, "/a": false, "/deep/1": false},
			absent:    []string{"/deep/2"},
		},
		{
			name:      "depth one",
			cfg:       Config{MaxDepth: 1, SkipExternal: true},
			wantPages: map[string]bool{"/deep/1": true, "/deep/2": false},
			absent:    []string{"/deep/3"},
		},
		{
			name:      "include restricts crawled pages",
			cfg:       Config{MaxDepth: -1, Include: []*regexp.Regexp{regexp.MustCompile(`/deep/`)}, SkipExternal: true},
			wantPages: map[string]bool{"/": true, "/a": false, "/b": false, "/deep/1": true, "/deep/2": true, "/deep/3": true},
		},
		{
			name:      "exclude skips matches entirely",
			cfg:       Config{MaxDepth: -1, Exclude: []*regexp.Regexp{regexp.MustCompile(`/(error|slow)$`)}, SkipExternal: true},
			wantPages: map[string]bool{"/": true},
			skipped:   map[string]string{"/error": SkipExcluded, "/slow": SkipExcluded},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rep := New(newChecker(100*time.Millisecond), tt.cfg).Run(t.Context(), mustURL(t, s.URL+"/"))
			links := byURL(rep)
			for path, wantPage := range tt.wantPages {
				l, ok := links[s.URL+path]
				if !ok {
					t.Errorf("%s missing from report", path)
					continue
				}
				if l.Page != wantPage {
					t.Errorf("%s page = %v, want %v", path, l.Page, wantPage)
				}
			}
			for _, path := range tt.absent {
				if _, ok := links[s.URL+path]; ok {
					t.Errorf("%s should not have been discovered", path)
				}
			}
			for path, reason := range tt.skipped {
				if got := links[s.URL+path].Skipped; got != reason {
					t.Errorf("%s skipped = %q, want %q", path, got, reason)
				}
			}
			for _, l := range rep.Links {
				if !l.Internal && l.Skipped != SkipExternal {
					t.Errorf("external %s not skipped", l.URL)
				}
			}
		})
	}
}

func TestCrawlMaxPages(t *testing.T) {
	t.Parallel()
	s, _ := fixture(t)
	rep := New(newChecker(100*time.Millisecond), Config{MaxDepth: -1, MaxPages: 3, SkipExternal: true}).
		Run(t.Context(), mustURL(t, s.URL+"/"))
	if rep.Pages != 3 {
		t.Errorf("pages = %d, want 3", rep.Pages)
	}
}

func TestCrawlFollowsStartRedirectToNewHost(t *testing.T) {
	t.Parallel()
	canonical := newSite(t, map[string]string{
		"/":      page("/inner"),
		"/inner": page("/leaf"),
		"/leaf":  page(),
	}, nil)
	alias := newSite(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, canonical.URL+"/", http.StatusMovedPermanently)
		})
	})

	rep := New(newChecker(time.Second), Config{MaxDepth: -1}).Run(t.Context(), mustURL(t, alias.URL+"/"))
	links := byURL(rep)
	if l := links[canonical.URL+"/inner"]; !l.Page || !l.Internal {
		t.Errorf("/inner on canonical host: page=%v internal=%v, want crawled as internal", l.Page, l.Internal)
	}
	if rep.Pages != 3 {
		t.Errorf("pages = %d, want 3", rep.Pages)
	}
}

func TestCrawlStartDisallowed(t *testing.T) {
	t.Parallel()
	s, _ := fixture(t)
	rep := New(newChecker(time.Second), Config{Robots: robots.DisallowAll}).Run(t.Context(), mustURL(t, s.URL+"/"))
	if len(rep.Links) != 1 || rep.Links[0].Skipped != SkipRobots {
		t.Fatalf("links = %+v, want only the skipped start URL", rep.Links)
	}
	if rep.Pages != 0 {
		t.Errorf("pages = %d", rep.Pages)
	}
}

func TestCrawlCancellation(t *testing.T) {
	t.Parallel()

	// Every page is slow and links to ten new pages, so the crawl would run
	// for a long time without cancellation.
	slow := newSite(t, nil, func(mux *http.ServeMux) {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-time.After(30 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Content-Type", "text/html")
			var links []string
			for i := range 10 {
				links = append(links, fmt.Sprintf("%s%d/", r.URL.Path, i))
			}
			fmt.Fprint(w, page(links...))
		})
	})

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	began := time.Now()
	rep := New(newChecker(5*time.Second), Config{MaxDepth: -1, Workers: 4}).Run(ctx, mustURL(t, slow.URL+"/"))
	if elapsed := time.Since(began); elapsed > 2*time.Second {
		t.Errorf("Run took %v after cancellation", elapsed)
	}
	if !rep.Interrupted {
		t.Error("report not marked interrupted")
	}
	st := rep.Stats()
	if st.OK == 0 {
		t.Error("expected some links checked before cancellation")
	}
	if st.Unchecked == 0 {
		t.Error("expected unchecked links after cancellation")
	}
	if st.Broken != 0 {
		t.Errorf("cancellation produced %d broken links; interrupted checks must stay unchecked", st.Broken)
	}
}

// stubChecker returns canned results without any network I/O.
type stubChecker struct {
	results map[string]check.Result
}

func (s stubChecker) Check(_ context.Context, u *url.URL, _ bool) check.Result {
	r, ok := s.results[u.String()]
	if !ok {
		return check.Result{Status: 200, Method: "HEAD", FinalURL: u}
	}
	if r.FinalURL == nil {
		r.FinalURL = u
	}
	return r
}

func TestCrawlUpgradesCheckedLinkToPage(t *testing.T) {
	t.Parallel()
	// /far is first reached at depth 2 (too deep for MaxDepth 1) through
	// /slow-branch, and later at depth 1 directly from /. Whatever order the
	// workers run in, /far must end up crawled.
	start := mustURL(t, "http://site.test/")
	html := func(links ...string) check.Result {
		return check.Result{Status: 200, Method: "GET", Body: []byte(page(links...))}
	}
	stub := stubChecker{results: map[string]check.Result{
		"http://site.test/":       html("/branch", "/far"),
		"http://site.test/branch": html("/far"),
		"http://site.test/far":    html("/leaf"),
	}}
	for range 20 {
		rep := New(stub, Config{MaxDepth: 1, Workers: 4}).Run(t.Context(), start)
		links := byURL(rep)
		if l := links["http://site.test/far"]; !l.Page || l.Depth != 1 {
			t.Fatalf("/far page=%v depth=%d, want crawled at depth 1", l.Page, l.Depth)
		}
		if _, ok := links["http://site.test/leaf"]; !ok {
			t.Fatal("/leaf not discovered from /far")
		}
	}
}

func TestLinkPredicates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		link             Link
		broken, redirect bool
	}{
		{"ok", Link{Checked: true, Status: 200}, false, false},
		{"not found", Link{Checked: true, Status: 404}, true, false},
		{"error", Link{Checked: true, Error: "timeout"}, true, false},
		{"redirected", Link{Checked: true, Status: 200, Redirects: []check.Hop{{URL: "x", Status: 301}}}, false, true},
		{"redirect to 404", Link{Checked: true, Status: 404, Redirects: []check.Hop{{URL: "x", Status: 301}}}, true, false},
		{"unchecked", Link{}, false, false},
	}
	for _, tt := range tests {
		if got := tt.link.Broken(); got != tt.broken {
			t.Errorf("%s: Broken() = %v, want %v", tt.name, got, tt.broken)
		}
		if got := tt.link.Redirected(); got != tt.redirect {
			t.Errorf("%s: Redirected() = %v, want %v", tt.name, got, tt.redirect)
		}
	}
}
