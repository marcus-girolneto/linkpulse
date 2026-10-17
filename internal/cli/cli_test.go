package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testSite(t *testing.T, broken bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "User-agent: *\nDisallow: /admin/\n")
	})
	mux.HandleFunc("/{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		links := `<a href="/about">about</a><a href="/old">old</a><a href="/admin/">admin</a><img src="/logo.png">`
		if broken {
			links += `<a href="/missing">missing</a>`
		}
		fmt.Fprint(w, "<html><body>"+links+"</body></html>")
	})
	mux.HandleFunc("/about", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body><a href="/">home</a></body></html>`)
	})
	mux.HandleFunc("/old", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/about", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/logo.png", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

type result struct {
	code           int
	stdout, stderr string
}

func runCLI(t *testing.T, env Env, args ...string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	env.Stdout, env.Stderr = &stdout, &stderr
	if env.Getenv == nil {
		env.Getenv = func(string) string { return "" }
	}
	code := Run(t.Context(), args, env)
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func TestRunExitCodes(t *testing.T) {
	t.Parallel()
	clean := testSite(t, false)
	dirty := testSite(t, true)

	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  []string
		wantErr  []string
	}{
		{name: "clean site", args: []string{clean.URL}, wantCode: ExitOK, wantOut: []string{"No broken links", "1 skipped"}},
		{name: "broken site", args: []string{dirty.URL}, wantCode: ExitFailed, wantOut: []string{"Broken links (1)", dirty.URL + "/missing", "404"}},
		{name: "fail-on none", args: []string{"--fail-on", "none", dirty.URL}, wantCode: ExitOK},
		{name: "fail-on redirect", args: []string{"--fail-on=redirect", clean.URL}, wantCode: ExitFailed},
		{name: "flags after url", args: []string{clean.URL, "--fail-on", "redirect", "--show-redirects"}, wantCode: ExitFailed, wantOut: []string{"Redirects (1)"}},
		{name: "no robots", args: []string{"--no-robots", clean.URL}, wantCode: ExitFailed, wantOut: []string{"0 skipped", clean.URL + "/admin/"}},
		{name: "exclude", args: []string{"--exclude", `/missing$`, dirty.URL}, wantCode: ExitOK},
		{name: "markdown", args: []string{"--format", "markdown", dirty.URL}, wantCode: ExitFailed, wantOut: []string{"## linkpulse report", "| 404 |"}},
		{name: "version", args: []string{"--version"}, wantCode: ExitOK, wantOut: []string{"linkpulse 1.2.3 (commit abc123, built unknown)"}},
		{name: "help", args: []string{"--help"}, wantCode: ExitOK, wantErr: []string{"Usage:", "--fail-on"}},
		{name: "missing url", args: nil, wantCode: ExitUsage, wantErr: []string{"expected exactly one URL"}},
		{name: "two urls", args: []string{clean.URL, clean.URL}, wantCode: ExitUsage},
		{name: "unknown flag", args: []string{"--nope", clean.URL}, wantCode: ExitUsage},
		{name: "bad regex", args: []string{"--exclude", "(", clean.URL}, wantCode: ExitUsage},
		{name: "bad format", args: []string{"--format", "xml", clean.URL}, wantCode: ExitUsage, wantErr: []string{"unknown format"}},
		{name: "bad fail-on", args: []string{"--fail-on", "sometimes", clean.URL}, wantCode: ExitUsage},
		{name: "bad color", args: []string{"--color", "rainbow", clean.URL}, wantCode: ExitUsage},
		{name: "bad url scheme", args: []string{"ftp://example.com"}, wantCode: ExitUsage, wantErr: []string{"unsupported scheme"}},
		{name: "zero workers", args: []string{"--workers", "0", clean.URL}, wantCode: ExitUsage},
		{name: "zero host concurrency", args: []string{"--host-concurrency", "0", clean.URL}, wantCode: ExitUsage},
		{name: "negative rate", args: []string{"--rate", "-1", clean.URL}, wantCode: ExitUsage},
		{name: "zero timeout", args: []string{"--timeout", "0s", clean.URL}, wantCode: ExitUsage},
		{name: "negative retries", args: []string{"--retries", "-1", clean.URL}, wantCode: ExitUsage},
		{name: "zero redirects", args: []string{"--max-redirects", "0", clean.URL}, wantCode: ExitUsage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := runCLI(t, Env{Version: "1.2.3", Commit: "abc123"}, tt.args...)
			if r.code != tt.wantCode {
				t.Errorf("exit code = %d, want %d\nstdout:\n%s\nstderr:\n%s", r.code, tt.wantCode, r.stdout, r.stderr)
			}
			for _, s := range tt.wantOut {
				if !strings.Contains(r.stdout, s) {
					t.Errorf("stdout missing %q:\n%s", s, r.stdout)
				}
			}
			for _, s := range tt.wantErr {
				if !strings.Contains(r.stderr, s) {
					t.Errorf("stderr missing %q:\n%s", s, r.stderr)
				}
			}
		})
	}
}

func TestRunJSON(t *testing.T) {
	t.Parallel()
	srv := testSite(t, true)
	r := runCLI(t, Env{}, "--format", "json", srv.URL)
	var doc struct {
		Summary struct {
			Pages, Total, Broken, Skipped, Redirected int
		}
		Links []struct {
			URL     string
			State   string
			Status  int
			FoundOn []struct{ Page string } `json:"found_on"`
		}
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, r.stdout)
	}
	if doc.Summary.Pages != 2 || doc.Summary.Broken != 1 || doc.Summary.Skipped != 1 || doc.Summary.Redirected != 1 {
		t.Errorf("summary = %+v", doc.Summary)
	}
	for _, l := range doc.Links {
		if l.State == "broken" && (l.URL != srv.URL+"/missing" || len(l.FoundOn) != 1 || l.FoundOn[0].Page != srv.URL+"/") {
			t.Errorf("unexpected broken link %+v", l)
		}
	}
}

func TestRunOutputFile(t *testing.T) {
	t.Parallel()
	srv := testSite(t, true)
	out := filepath.Join(t.TempDir(), "report.md")
	r := runCLI(t, Env{StdoutTTY: true}, "--format", "md", "--output", out, srv.URL)
	if r.code != ExitFailed {
		t.Fatalf("exit code = %d", r.code)
	}
	if r.stdout != "" {
		t.Errorf("stdout should be empty when --output is set, got %q", r.stdout)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "### Broken links (1)") || strings.Contains(string(data), "\x1b[") {
		t.Errorf("unexpected file content:\n%s", data)
	}

	bad := runCLI(t, Env{}, "--output", filepath.Join(t.TempDir(), "missing-dir", "x.txt"), srv.URL)
	if bad.code != ExitUsage || !strings.Contains(bad.stderr, "create output") {
		t.Errorf("unwritable output: code=%d stderr=%q", bad.code, bad.stderr)
	}
}

func TestRunColor(t *testing.T) {
	t.Parallel()
	srv := testSite(t, true)
	noColor := func(k string) string {
		if k == "NO_COLOR" {
			return "1"
		}
		return ""
	}
	tests := []struct {
		name   string
		env    Env
		args   []string
		escape bool
	}{
		{name: "tty", env: Env{StdoutTTY: true}, escape: true},
		{name: "pipe", env: Env{StdoutTTY: false}, escape: false},
		{name: "NO_COLOR", env: Env{StdoutTTY: true, Getenv: noColor}, escape: false},
		{name: "always", env: Env{}, args: []string{"--color", "always"}, escape: true},
		{name: "never", env: Env{StdoutTTY: true}, args: []string{"--color=never"}, escape: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := runCLI(t, tt.env, append(tt.args, srv.URL)...)
			if got := strings.Contains(r.stdout, "\x1b["); got != tt.escape {
				t.Errorf("ANSI escapes present = %v, want %v\n%q", got, tt.escape, r.stdout)
			}
		})
	}
}

func TestRunProgressOnTTY(t *testing.T) {
	t.Parallel()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(250 * time.Millisecond)
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html></html>")
	}))
	t.Cleanup(slow.Close)

	r := runCLI(t, Env{StderrTTY: true}, "--no-robots", slow.URL)
	if !strings.Contains(r.stderr, "checking: ") {
		t.Errorf("expected progress line on stderr, got %q", r.stderr)
	}
	q := runCLI(t, Env{StderrTTY: true}, "--no-robots", "--quiet", slow.URL)
	if q.stderr != "" {
		t.Errorf("--quiet still wrote %q", q.stderr)
	}
}

func TestRunRobotsWarningsAndCrawlDelay(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "User-agent: *\nCrawl-delay: 0.05\n")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<a href="/a">a</a><a href="/b">b</a>`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	r := runCLI(t, Env{}, srv.URL)
	if r.code != ExitOK || !strings.Contains(r.stderr, "Crawl-delay of 50ms") {
		t.Errorf("code=%d stderr=%q", r.code, r.stderr)
	}

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(down.Close)
	r = runCLI(t, Env{}, down.URL)
	if !strings.Contains(r.stderr, "treating the site as disallowed") || !strings.Contains(r.stdout, "1 skipped") {
		t.Errorf("robots 503: stdout=%q stderr=%q", r.stdout, r.stderr)
	}
}

func TestRunInterrupted(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-time.After(20 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/html")
		for i := range 20 {
			fmt.Fprintf(w, `<a href="%s%d/">x</a>`, r.URL.Path, i)
		}
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	var stdout, stderr bytes.Buffer
	code := Run(ctx, []string{"--no-robots", "--depth", "-1", "--rate", "0", srv.URL},
		Env{Stdout: &stdout, Stderr: &stderr, Getenv: func(string) string { return "" }})
	if code != ExitInterrupted {
		t.Errorf("exit code = %d, want %d", code, ExitInterrupted)
	}
	if !strings.Contains(stdout.String(), "Interrupted: partial results") {
		t.Errorf("partial report missing:\n%s", stdout.String())
	}
}

func TestRunAddsSchemeToBareHost(t *testing.T) {
	t.Parallel()
	o := options{workers: 1, hostConcurrency: 1, timeout: time.Second, maxRedirects: 1, failOn: "broken", color: "auto", format: "text"}
	cfg, err := o.validate([]string{"Example.com/docs#intro"}, Env{Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.start.String(); got != "https://example.com/docs" {
		t.Errorf("start = %q", got)
	}
}
