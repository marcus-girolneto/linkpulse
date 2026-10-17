package extract

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

func TestLinksGolden(t *testing.T) {
	t.Parallel()

	inputs, err := filepath.Glob(filepath.Join("testdata", "*.html"))
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) == 0 {
		t.Fatal("no testdata/*.html fixtures found")
	}
	for _, in := range inputs {
		name := strings.TrimSuffix(filepath.Base(in), ".html")
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, err := os.Open(in)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()

			res, err := Links(f)
			if err != nil {
				t.Fatalf("Links: %v", err)
			}
			got, err := json.MarshalIndent(res, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')

			golden := strings.TrimSuffix(in, ".html") + ".golden.json"
			if *update {
				if err := os.WriteFile(golden, got, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("read golden (run with -update to create): %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("mismatch with %s (run go test -update to accept)\n--- got\n%s\n--- want\n%s", golden, got, want)
			}
		})
	}
}

func TestLinks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		html     string
		want     []string
		base     string
		nofollow bool
	}{
		{name: "empty document", html: "", want: nil},
		{name: "anchor", html: `<a href="/x">x</a>`, want: []string{"/x"}},
		{name: "uppercase tags", html: `<A HREF="/x">x</A>`, want: []string{"/x"}},
		{name: "unquoted attribute", html: `<a href=/x>x</a>`, want: []string{"/x"}},
		{name: "anchor without href", html: `<a name="top">x</a>`, want: nil},
		{name: "empty href skipped", html: `<a href="  ">x</a>`, want: nil},
		{name: "self closing img", html: `<img src="/i.png"/>`, want: []string{"/i.png"}},
		{name: "srcset", html: `<img src="a.png" srcset="a-1x.png 1x, a-2x.png 2x">`, want: []string{"a.png", "a-1x.png", "a-2x.png"}},
		{name: "script src", html: `<script src="/app.js"></script>`, want: []string{"/app.js"}},
		{name: "inline script ignored", html: `<script>var a = "<a href='/no'>";</script>`, want: nil},
		{name: "stylesheet", html: `<link rel="stylesheet" href="/s.css">`, want: []string{"/s.css"}},
		{name: "preconnect skipped", html: `<link rel="preconnect" href="https://fonts.example">`, want: nil},
		{name: "duplicate attribute first wins", html: `<a href="/first" href="/second">x</a>`, want: []string{"/first"}},
		{name: "entities decoded", html: `<a href="/s?a=1&amp;b=2">x</a>`, want: []string{"/s?a=1&b=2"}},
		{name: "comment ignored", html: `<!-- <a href="/hidden"> -->`, want: nil},
		{name: "base href", html: `<base href="/root/"><a href="x">x</a>`, want: []string{"x"}, base: "/root/"},
		{name: "meta nofollow", html: `<meta name="robots" content="noindex, nofollow"><a href="/x">`, want: []string{"/x"}, nofollow: true},
		{name: "meta none", html: `<meta name="ROBOTS" content="none">`, want: nil, nofollow: true},
		{name: "video poster", html: `<video src="/v.mp4" poster="/p.jpg"></video>`, want: []string{"/v.mp4", "/p.jpg"}},
		{name: "unterminated tag", html: `<a href="/x"`, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res, err := Links(strings.NewReader(tt.html))
			if err != nil {
				t.Fatalf("Links: %v", err)
			}
			var got []string
			for _, l := range res.Links {
				got = append(got, l.URL)
			}
			if strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Errorf("links = %q, want %q", got, tt.want)
			}
			if res.Base != tt.base {
				t.Errorf("base = %q, want %q", res.Base, tt.base)
			}
			if res.NoFollow != tt.nofollow {
				t.Errorf("nofollow = %v, want %v", res.NoFollow, tt.nofollow)
			}
		})
	}
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

func TestLinksReadError(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	r := io.MultiReader(strings.NewReader(`<a href="/ok">`), failingReader{boom})
	res, err := Links(r)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if len(res.Links) != 1 {
		t.Errorf("expected partial result with 1 link, got %d", len(res.Links))
	}
}

func BenchmarkLinks(b *testing.B) {
	page := syntheticPage(500)
	b.SetBytes(int64(len(page)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Links(bytes.NewReader(page)); err != nil {
			b.Fatal(err)
		}
	}
}

// syntheticPage builds a document with n links interleaved with typical
// markup noise (text, nested divs, inline styles and scripts).
func syntheticPage(n int) []byte {
	var sb strings.Builder
	sb.WriteString(`<!doctype html><html><head><title>bench</title>`)
	sb.WriteString(`<link rel="stylesheet" href="/css/site.css"><script src="/js/app.js"></script>`)
	sb.WriteString(`<style>body{font-family:sans-serif}.x{color:red}</style></head><body>`)
	for i := range n {
		sb.WriteString(`<div class="card"><p>Lorem ipsum dolor sit amet, consectetur adipiscing elit.</p>`)
		sb.WriteString(`<a class="link" href="/articles/`)
		sb.WriteString(strings.Repeat("x", i%7+1))
		sb.WriteString(`?page=2#c">Read more</a><img src="/img/thumb.png" alt="thumb"></div>`)
	}
	sb.WriteString(`</body></html>`)
	return []byte(sb.String())
}
