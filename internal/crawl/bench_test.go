package crawl

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/marcus-girolneto/linkpulse/internal/check"
	"github.com/marcus-girolneto/linkpulse/internal/hostlimit"
)

// benchSite serves n pages. Page i links to ten other pages and five shared
// static assets, so a full crawl fetches n pages and checks n+5 URLs. Every
// response is delayed by latency to model a remote server.
func benchSite(b *testing.B, n int, latency time.Duration) *httptest.Server {
	b.Helper()
	assets := []string{"/static/app.css", "/static/app.js", "/static/logo.svg", "/static/hero.jpg", "/static/font.woff2"}
	mux := http.NewServeMux()
	mux.HandleFunc("/static/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
	})
	mux.HandleFunc("/p/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil || id < 0 || id >= n {
			http.NotFound(w, r)
			return
		}
		var sb strings.Builder
		sb.WriteString(`<!doctype html><html><head><title>page</title>`)
		fmt.Fprintf(&sb, `<link rel="stylesheet" href="%s"><script src="%s"></script></head><body>`, assets[0], assets[1])
		for k := 1; k <= 10; k++ {
			fmt.Fprintf(&sb, `<p>Paragraph %d with some filler text to resemble content.</p><a href="/p/%d">next</a>`, k, (id*7+k*13+1)%n)
		}
		if id+1 < n {
			fmt.Fprintf(&sb, `<a href="/p/%d">following</a>`, id+1)
		}
		fmt.Fprintf(&sb, `<img src="%s"><img src="%s"><img src="%s"></body></html>`, assets[2], assets[3], assets[4])
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, sb.String())
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if latency > 0 {
			time.Sleep(latency)
		}
		mux.ServeHTTP(w, r)
	}))
	b.Cleanup(srv.Close)
	return srv
}

func BenchmarkCrawl(b *testing.B) {
	for _, latency := range []time.Duration{0, 5 * time.Millisecond} {
		for _, workers := range []int{1, 8, 32} {
			benchCrawl(b, 500, latency, workers)
		}
	}
}

func benchCrawl(b *testing.B, pages int, latency time.Duration, workers int) {
	b.Run(fmt.Sprintf("latency=%s/workers=%d", latency, workers), func(b *testing.B) {
		srv := benchSite(b, pages, latency)
		start := mustURL(b, srv.URL+"/p/0")

		transport := &http.Transport{MaxIdleConnsPerHost: workers, MaxConnsPerHost: workers}
		b.Cleanup(transport.CloseIdleConnections)
		client := &http.Client{Transport: transport}

		var checked int
		b.ReportAllocs()
		b.ResetTimer()
		began := time.Now()
		for b.Loop() {
			gate := hostlimit.New(hostlimit.Config{Concurrency: workers})
			chk := check.New(client, gate, check.Config{Timeout: 10 * time.Second})
			rep := New(chk, Config{MaxDepth: -1, Workers: workers}).Run(b.Context(), start)
			if rep.Pages != pages {
				b.Fatalf("crawled %d pages, want %d", rep.Pages, pages)
			}
			checked += len(rep.Links)
		}
		elapsed := time.Since(began).Seconds()
		b.ReportMetric(float64(b.N*pages)/elapsed, "pages/s")
		b.ReportMetric(float64(checked)/elapsed, "urls/s")
	})
}
