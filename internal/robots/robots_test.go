package robots

import (
	"strings"
	"testing"
	"time"
)

const sample = `
# Comments and blank lines are ignored.
User-agent: *
Disallow: /private/
Disallow: /tmp
Allow: /private/public-report.html
Disallow: /*.pdf$
Disallow: /search*q=
Crawl-delay: 2

User-agent: linkpulse
User-agent: otherbot
Disallow: /no-linkpulse/
Allow: /

User-agent: badbot
Disallow: /
`

func TestAllowed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		ua    string
		path  string
		allow bool
	}{
		{name: "wildcard group allows root", ua: "curl/8", path: "/", allow: true},
		{name: "wildcard disallow dir", ua: "curl/8", path: "/private/secret", allow: false},
		{name: "more specific allow wins", ua: "curl/8", path: "/private/public-report.html", allow: true},
		{name: "prefix match without slash", ua: "curl/8", path: "/tmpfile", allow: false},
		{name: "anchored pdf", ua: "curl/8", path: "/docs/a.pdf", allow: false},
		{name: "anchored pdf with query", ua: "curl/8", path: "/docs/a.pdf?x=1", allow: true},
		{name: "mid wildcard", ua: "curl/8", path: "/search?page=1&q=go", allow: false},
		{name: "mid wildcard miss", ua: "curl/8", path: "/search?page=1", allow: true},
		{name: "robots.txt always allowed", ua: "badbot", path: "/robots.txt", allow: true},
		{name: "named group replaces wildcard", ua: "linkpulse/1.0 (+https://example.com)", path: "/private/secret", allow: true},
		{name: "named group own rule", ua: "LinkPulse/1.0", path: "/no-linkpulse/x", allow: false},
		{name: "shared group", ua: "otherbot", path: "/no-linkpulse/", allow: false},
		{name: "disallow all", ua: "badbot/2", path: "/anything", allow: false},
		{name: "empty path treated as root", ua: "badbot", path: "", allow: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := Parse(strings.NewReader(sample), tt.ua)
			if got := r.Allowed(tt.path); got != tt.allow {
				t.Errorf("Allowed(%q) for %q = %v, want %v", tt.path, tt.ua, got, tt.allow)
			}
		})
	}
}

func TestCrawlDelay(t *testing.T) {
	t.Parallel()
	if got := Parse(strings.NewReader(sample), "curl").CrawlDelay(); got != 2*time.Second {
		t.Errorf("wildcard crawl delay = %v, want 2s", got)
	}
	if got := Parse(strings.NewReader(sample), "linkpulse").CrawlDelay(); got != 0 {
		t.Errorf("named crawl delay = %v, want 0", got)
	}
	r := Parse(strings.NewReader("User-agent: *\nCrawl-delay: 0.5\n"), "x")
	if got := r.CrawlDelay(); got != 500*time.Millisecond {
		t.Errorf("fractional crawl delay = %v, want 500ms", got)
	}
}

func TestParseEdgeCases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		body  string
		path  string
		allow bool
	}{
		{name: "empty file", body: "", path: "/x", allow: true},
		{name: "rules before any group ignored", body: "Disallow: /\n", path: "/x", allow: true},
		{name: "empty disallow allows", body: "User-agent: *\nDisallow:\n", path: "/x", allow: true},
		{name: "case insensitive keys", body: "USER-AGENT: *\nDISALLOW: /x\n", path: "/x", allow: false},
		{name: "inline comment", body: "User-agent: * # all\nDisallow: /x # no\n", path: "/x", allow: false},
		{name: "garbage lines", body: "hello\n:::\nUser-agent: *\nfoo bar\nDisallow: /x\n", path: "/x/y", allow: false},
		{name: "path case sensitive", body: "User-agent: *\nDisallow: /X\n", path: "/x", allow: true},
		{name: "tie prefers allow", body: "User-agent: *\nDisallow: /a\nAllow: /a\n", path: "/a", allow: true},
		{name: "groups for same agent merge", body: "User-agent: *\nDisallow: /a\n\nUser-agent: *\nDisallow: /b\n", path: "/b", allow: false},
		{name: "crlf line endings", body: "User-agent: *\r\nDisallow: /x\r\n", path: "/x", allow: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Parse(strings.NewReader(tt.body), "linkpulse").Allowed(tt.path); got != tt.allow {
				t.Errorf("Allowed(%q) = %v, want %v", tt.path, got, tt.allow)
			}
		})
	}
}

func TestPresets(t *testing.T) {
	t.Parallel()
	if !AllowAll.Allowed("/anything") {
		t.Error("AllowAll disallowed a path")
	}
	if DisallowAll.Allowed("/anything") {
		t.Error("DisallowAll allowed a path")
	}
}

func TestMatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		pattern, path string
		want          bool
	}{
		{"/", "/anything", true},
		{"/a*b", "/a-x-b-y", true},
		{"/a*b$", "/a-x-b-y", false},
		{"/a*b$", "/a-x-b", true},
		{"*", "/x", true},
		{"/x$", "/x", true},
		{"/x$", "/xy", false},
		{"/a**b", "/ab", true},
		{"/a*b*c$", "/a1b2c", true},
		{"/a*b*c$", "/a1b2c3", false},
	}
	for _, tt := range tests {
		if got := match(tt.pattern, tt.path); got != tt.want {
			t.Errorf("match(%q, %q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
		}
	}
}

func FuzzParse(f *testing.F) {
	f.Add(sample, "/private/x")
	f.Add("User-agent: *\nDisallow: /*$\n", "/")
	f.Add("User-agent: *\nAllow: /$\nDisallow: /\n", "/page")
	f.Fuzz(func(t *testing.T, body, path string) {
		r := Parse(strings.NewReader(body), "linkpulse/1.0")
		_ = r.Allowed(path)
		if !r.Allowed("/robots.txt") {
			t.Fatal("robots.txt must always be allowed")
		}
		if r.CrawlDelay() < 0 {
			t.Fatal("negative crawl delay")
		}
	})
}
