package urlnorm

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

func mustParse(t testing.TB, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

func TestResolve(t *testing.T) {
	t.Parallel()

	base := "https://example.com/docs/guide/index.html"
	tests := []struct {
		name    string
		base    string
		ref     string
		want    string
		wantErr error
	}{
		{name: "absolute", ref: "https://example.com/a", want: "https://example.com/a"},
		{name: "strip fragment", ref: "https://example.com/a#section", want: "https://example.com/a"},
		{name: "lowercase scheme and host", ref: "HTTPS://Example.COM/Path", want: "https://example.com/Path"},
		{name: "empty path becomes slash", ref: "https://example.com", want: "https://example.com/"},
		{name: "default https port", ref: "https://example.com:443/a", want: "https://example.com/a"},
		{name: "default http port", ref: "http://example.com:80/a", want: "http://example.com/a"},
		{name: "non-default port kept", ref: "http://example.com:8080/a", want: "http://example.com:8080/a"},
		{name: "https on port 80 kept", ref: "https://example.com:80/", want: "https://example.com:80/"},
		{name: "trailing dot host", ref: "https://example.com./a", want: "https://example.com/a"},
		{name: "ipv6 host", ref: "http://[::1]:8080/", want: "http://[::1]:8080/"},
		{name: "ipv6 default port", ref: "http://[::1]:80/x", want: "http://[::1]/x"},
		{name: "dot segments absolute", ref: "https://example.com/a/./b/../c", want: "https://example.com/a/c"},
		{name: "query kept verbatim", ref: "https://example.com/?b=2&a=1", want: "https://example.com/?b=2&a=1"},
		{name: "space in query encoded", ref: "https://example.com/?q=a b", want: "https://example.com/?q=a%20b"},
		{name: "empty query dropped", ref: "https://example.com/a?", want: "https://example.com/a"},
		{name: "relative sibling", base: base, ref: "setup.html", want: "https://example.com/docs/guide/setup.html"},
		{name: "relative parent", base: base, ref: "../api/", want: "https://example.com/docs/api/"},
		{name: "root relative", base: base, ref: "/about#team", want: "https://example.com/about"},
		{name: "scheme relative", base: base, ref: "//CDN.example.net/x.js", want: "https://cdn.example.net/x.js"},
		{name: "query only", base: base, ref: "?page=2", want: "https://example.com/docs/guide/index.html?page=2"},
		{name: "fragment only", base: base, ref: "#top", want: "https://example.com/docs/guide/index.html"},
		{name: "surrounding whitespace", base: base, ref: "  /a  ", want: "https://example.com/a"},
		{name: "embedded newline", base: base, ref: "/lo\nng/pa\tth", want: "https://example.com/long/path"},
		{name: "percent encoding preserved", base: base, ref: "/a%2Fb", want: "https://example.com/a%2Fb"},
		{name: "mailto", base: base, ref: "mailto:team", wantErr: ErrUnsupportedScheme},
		{name: "javascript", base: base, ref: "javascript:void(0)", wantErr: ErrUnsupportedScheme},
		{name: "tel", base: base, ref: "tel:+15555555555", wantErr: ErrUnsupportedScheme},
		{name: "data", base: base, ref: "data:image/png;base64,AAAA", wantErr: ErrUnsupportedScheme},
		{name: "ftp", ref: "ftp://example.com/file", wantErr: ErrUnsupportedScheme},
		{name: "relative without base", ref: "/a", wantErr: ErrNoHost},
		{name: "missing host", ref: "http:///a", wantErr: ErrNoHost},
		{name: "opaque", ref: "http:example.com", wantErr: ErrNoHost},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var b *url.URL
			if tt.base != "" {
				b = mustParse(t, tt.base)
			}
			got, err := String(b, tt.ref)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("String(%q) error = %v, want %v", tt.ref, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("String(%q) unexpected error: %v", tt.ref, err)
			}
			if got != tt.want {
				t.Errorf("String(%q) = %q, want %q", tt.ref, got, tt.want)
			}
		})
	}
}

func TestParseInvalid(t *testing.T) {
	t.Parallel()
	if _, err := Parse("http://exa mple.com/%zz"); err == nil {
		t.Fatal("expected error for malformed URL")
	}
}

func TestResolveDoesNotMutateBase(t *testing.T) {
	t.Parallel()
	base := mustParse(t, "https://Example.com/a/b#frag")
	before := base.String()
	if _, err := Resolve(base, "../c"); err != nil {
		t.Fatal(err)
	}
	if base.String() != before {
		t.Errorf("base mutated: %q -> %q", before, base.String())
	}
}

func FuzzNormalize(f *testing.F) {
	seeds := []string{
		"https://example.com/a#b",
		"HTTP://EXAMPLE.com:80/./a/../b?x=1",
		"//cdn.example.net/x",
		"../up",
		"?q",
		"#frag",
		"http://[::1]:443/",
		"https://example.com/%7Euser",
		"mailto:x@y",
		" \t/space\n ",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	base := mustParse(f, "https://example.com/dir/page.html")

	f.Fuzz(func(t *testing.T, ref string) {
		u, err := Resolve(base, ref)
		if err != nil {
			return
		}
		got := u.String()
		if u.Fragment != "" || u.RawFragment != "" {
			t.Fatalf("fragment survived normalization: %q", got)
		}
		if u.Host != strings.ToLower(u.Host) {
			t.Fatalf("host not lowercased: %q", u.Host)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			t.Fatalf("unexpected scheme %q", u.Scheme)
		}
		again, err := String(nil, got)
		if err != nil {
			t.Fatalf("normalized URL %q does not re-parse: %v", got, err)
		}
		if again != got {
			t.Fatalf("not idempotent: %q -> %q (input %q)", got, again, ref)
		}
	})
}
