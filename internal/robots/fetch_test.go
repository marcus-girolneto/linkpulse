package robots

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

func TestFetch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		handler    http.HandlerFunc
		path       string
		wantAllow  bool
		wantWarn   bool
		wantUA     bool
	}{
		{
			name: "parsed",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Write([]byte("User-agent: *\nDisallow: /private\n"))
			},
			path: "/private/x", wantAllow: false, wantUA: true,
		},
		{
			name:    "not found allows everything",
			handler: http.NotFound,
			path:    "/private/x", wantAllow: true,
		},
		{
			name: "server error disallows",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
			},
			path: "/", wantAllow: false, wantWarn: true,
		},
		{
			name: "redirect followed",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/robots.txt" {
					http.Redirect(w, r, "/real-robots.txt", http.StatusMovedPermanently)
					return
				}
				w.Write([]byte("User-agent: *\nDisallow: /\n"))
			},
			path: "/x", wantAllow: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var gotUA atomic.Value
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotUA.Store(r.UserAgent())
				tt.handler(w, r)
			}))
			t.Cleanup(srv.Close)

			site, _ := url.Parse(srv.URL + "/some/page")
			rules, err := Fetch(t.Context(), srv.Client(), site, "linkpulse/test")
			if (err != nil) != tt.wantWarn {
				t.Errorf("err = %v, wantWarn %v", err, tt.wantWarn)
			}
			if got := rules.Allowed(tt.path); got != tt.wantAllow {
				t.Errorf("Allowed(%q) = %v, want %v", tt.path, got, tt.wantAllow)
			}
			if ua, _ := gotUA.Load().(string); tt.wantUA && ua != "linkpulse/test" {
				t.Errorf("User-Agent = %q", ua)
			}
		})
	}
}

func TestFetchUnreachable(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.NotFoundHandler())
	site, _ := url.Parse(srv.URL)
	srv.Close()

	rules, err := Fetch(t.Context(), http.DefaultClient, site, "")
	if err == nil {
		t.Fatal("expected advisory error")
	}
	if rules != AllowAll {
		t.Error("unreachable robots.txt should fall back to AllowAll")
	}
}
