package report

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marcus-girolneto/linkpulse/internal/check"
	"github.com/marcus-girolneto/linkpulse/internal/crawl"
)

var update = flag.Bool("update", false, "rewrite golden files")

func fixtureReport() *crawl.Report {
	home := crawl.Source{Page: "https://example.com/", Tag: "a", Attr: "href"}
	blog := crawl.Source{Page: "https://example.com/blog/", Tag: "a", Attr: "href"}
	return &crawl.Report{
		Start:     "https://example.com/",
		StartedAt: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC),
		Duration:  1234 * time.Millisecond,
		Pages:     3,
		Links: []crawl.Link{
			{
				URL: "https://cdn.example.net/app.js", Checked: true, Status: 200, Method: "HEAD", Attempts: 1, Duration: 40 * time.Millisecond,
				Sources: []crawl.Source{{Page: "https://example.com/", Tag: "script", Attr: "src"}}, SourceCount: 1,
			},
			{URL: "https://example.com/", Internal: true, Page: true, Checked: true, Status: 200, Method: "GET", Attempts: 1, Duration: 80 * time.Millisecond},
			{
				URL: "https://example.com/about", Internal: true, Page: true, Depth: 1, Checked: true, Status: 200, Method: "GET", Attempts: 1, Duration: 20 * time.Millisecond,
				Sources: []crawl.Source{home, blog}, SourceCount: 2,
			},
			{
				URL: "https://example.com/admin/", Internal: true, Depth: 1, Skipped: crawl.SkipRobots,
				Sources: []crawl.Source{home}, SourceCount: 1,
			},
			{
				URL: "https://example.com/blog/", Internal: true, Page: true, Depth: 1, Checked: true, Status: 200, Method: "GET", Attempts: 1, Duration: 30 * time.Millisecond,
				Sources: []crawl.Source{home}, SourceCount: 1,
			},
			{
				URL: "https://example.com/blog/old-post", Internal: true, Depth: 2, Checked: true, Status: 404, Method: "GET", Attempts: 1, Duration: 15 * time.Millisecond,
				Sources: []crawl.Source{blog, home, {Page: "https://example.com/about", Tag: "a", Attr: "href"}, {Page: "https://example.com/x", Tag: "a", Attr: "href"}}, SourceCount: 6,
			},
			{
				URL: "https://example.com/docs", Internal: true, Page: true, Depth: 1, Checked: true, Status: 200, Method: "GET", Attempts: 2, Duration: 300 * time.Millisecond,
				Redirects: []check.Hop{{URL: "https://example.com/docs", Status: 301}}, FinalURL: "https://docs.example.com/",
				Sources: []crawl.Source{home}, SourceCount: 1,
			},
			{
				URL: "https://example.com/search?q=a|b", Internal: true, Depth: 1, Checked: true, Status: 500, Method: "GET", Attempts: 3, Duration: 900 * time.Millisecond,
				Sources: []crawl.Source{home}, SourceCount: 1,
			},
			{
				URL: "https://gone.example.org/", Checked: true, Error: "dns: no such host", Method: "HEAD", Attempts: 1, Duration: 5 * time.Millisecond,
				Sources: []crawl.Source{blog}, SourceCount: 1,
			},
			{
				URL: "https://slow.example.org/report.pdf", Checked: true, Error: "timeout", Method: "HEAD", Attempts: 3, Duration: 30 * time.Second,
				Sources: []crawl.Source{{Page: "https://example.com/blog/", Tag: "a", Attr: "href"}}, SourceCount: 1,
			},
		},
	}
}

func cleanReport() *crawl.Report {
	r := fixtureReport()
	var ok []crawl.Link
	for _, l := range r.Links {
		if !l.Broken() {
			ok = append(ok, l)
		}
	}
	r.Links = ok
	return r
}

func interruptedReport() *crawl.Report {
	r := fixtureReport()
	r.Interrupted = true
	r.Links = append(r.Links, crawl.Link{
		URL: "https://example.com/zzz-pending", Internal: true, Depth: 2,
		Sources: []crawl.Source{{Page: "https://example.com/blog/", Tag: "a", Attr: "href"}}, SourceCount: 1,
	})
	return r
}

func TestGolden(t *testing.T) {
	t.Parallel()

	reports := map[string]*crawl.Report{
		"broken":      fixtureReport(),
		"clean":       cleanReport(),
		"interrupted": interruptedReport(),
	}
	variants := []struct {
		name   string
		format Format
		opts   Options
		ext    string
	}{
		{name: "text", format: Text, ext: "txt"},
		{name: "text-color", format: Text, opts: Options{Color: true, ShowRedirects: true}, ext: "ansi"},
		{name: "json", format: JSON, ext: "json"},
		{name: "markdown", format: Markdown, opts: Options{ShowRedirects: true}, ext: "md"},
	}
	for repName, rep := range reports {
		for _, v := range variants {
			t.Run(repName+"/"+v.name, func(t *testing.T) {
				t.Parallel()
				var buf bytes.Buffer
				if err := Write(&buf, rep, v.format, v.opts); err != nil {
					t.Fatal(err)
				}
				golden := filepath.Join("testdata", repName+"."+v.name+"."+v.ext)
				if *update {
					if err := os.WriteFile(golden, buf.Bytes(), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				want, err := os.ReadFile(golden)
				if err != nil {
					t.Fatalf("read golden (run with -update to create): %v", err)
				}
				if !bytes.Equal(buf.Bytes(), want) {
					t.Errorf("output differs from %s (run go test -update to accept)\n--- got\n%s\n--- want\n%s", golden, buf.Bytes(), want)
				}
			})
		}
	}
}

func TestParseFormat(t *testing.T) {
	t.Parallel()
	tests := map[string]Format{"": Text, "text": Text, "TABLE": Text, "json": JSON, "md": Markdown, " markdown ": Markdown}
	for in, want := range tests {
		got, err := ParseFormat(in)
		if err != nil || got != want {
			t.Errorf("ParseFormat(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseFormat("xml"); !errors.Is(err, ErrUnknownFormat) {
		t.Errorf("ParseFormat(xml) err = %v", err)
	}
	if err := Write(&bytes.Buffer{}, fixtureReport(), Format("xml"), Options{}); !errors.Is(err, ErrUnknownFormat) {
		t.Errorf("Write(xml) err = %v", err)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestWriteErrorsPropagate(t *testing.T) {
	t.Parallel()
	for _, f := range []Format{Text, JSON, Markdown} {
		if err := Write(failWriter{}, fixtureReport(), f, Options{}); err == nil {
			t.Errorf("%s: expected write error", f)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	t.Parallel()
	tests := map[time.Duration]string{
		1234567 * time.Nanosecond: "1ms",
		1234 * time.Millisecond:   "1.23s",
		95 * time.Second:          "1m35s",
	}
	for in, want := range tests {
		if got := formatDuration(in); got != want {
			t.Errorf("formatDuration(%v) = %q, want %q", in, got, want)
		}
	}
}
