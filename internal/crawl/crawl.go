// Package crawl walks a site breadth-first and checks every link it finds.
package crawl

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/marcus-girolneto/linkpulse/internal/check"
	"github.com/marcus-girolneto/linkpulse/internal/extract"
	"github.com/marcus-girolneto/linkpulse/internal/urlnorm"
)

// Checker checks a single URL. *check.Checker satisfies it.
type Checker interface {
	Check(ctx context.Context, u *url.URL, fetch bool) check.Result
}

// RobotsPolicy decides whether a same-host path may be requested.
type RobotsPolicy interface {
	Allowed(path string) bool
}

// Skip reasons reported in Link.Skipped.
const (
	SkipExcluded = "excluded"
	SkipRobots   = "robots.txt"
	SkipExternal = "external"
)

const maxSources = 25

// Config controls a crawl.
type Config struct {
	MaxDepth     int              // negative means unlimited
	MaxPages     int              // <= 0 means unlimited
	Workers      int              // default 16
	Include      []*regexp.Regexp // limits which pages are crawled, not checked
	Exclude      []*regexp.Regexp
	Robots       RobotsPolicy
	SkipExternal bool
	// OnResult is called concurrently from workers.
	OnResult func(Link)
}

// Source records where a link was found.
type Source struct {
	Page string
	Tag  string
	Attr string
}

// Link is the outcome for one unique (normalized) URL.
type Link struct {
	URL       string
	Internal  bool
	Page      bool // fetched and parsed as HTML
	Depth     int
	Checked   bool
	Skipped   string
	Status    int
	Method    string
	Redirects []check.Hop
	FinalURL  string
	Error     string
	Attempts  int
	Duration  time.Duration
	// Sources is capped at maxSources; SourceCount is the full count.
	Sources     []Source
	SourceCount int
}

// Broken reports whether the link was checked and failed.
func (l *Link) Broken() bool {
	return l.Checked && (l.Error != "" || l.Status >= 400 || l.Status == 0)
}

// Redirected reports whether the link succeeded after a redirect.
func (l *Link) Redirected() bool {
	return l.Checked && !l.Broken() && len(l.Redirects) > 0
}

// Report is the result of a crawl.
type Report struct {
	Start       string
	StartedAt   time.Time
	Duration    time.Duration
	Interrupted bool
	Pages       int
	Links       []Link // sorted by URL
}

// Stats summarizes a report.
type Stats struct {
	Total, OK, Broken, Redirected, Skipped, Unchecked int
}

// Stats counts links by outcome. Redirected links are also counted as OK.
func (r *Report) Stats() Stats {
	s := Stats{Total: len(r.Links)}
	for i := range r.Links {
		l := &r.Links[i]
		switch {
		case l.Skipped != "":
			s.Skipped++
		case !l.Checked:
			s.Unchecked++
		case l.Broken():
			s.Broken++
		default:
			s.OK++
			if len(l.Redirects) > 0 {
				s.Redirected++
			}
		}
	}
	return s
}

// Crawler runs crawls. It holds no per-crawl state and may be reused.
type Crawler struct {
	checker Checker
	cfg     Config
}

// New returns a Crawler that uses checker for every request.
func New(checker Checker, cfg Config) *Crawler {
	if cfg.Workers <= 0 {
		cfg.Workers = 16
	}
	return &Crawler{checker: checker, cfg: cfg}
}

type job struct {
	e     *entry
	crawl bool
}

// Run crawls from start. If ctx ends first the report is partial and Interrupted.
func (c *Crawler) Run(ctx context.Context, start *url.URL) *Report {
	began := time.Now()
	st := newState(&c.cfg, start)

	jobs := make(chan job)
	results := make(chan []job)
	var wg sync.WaitGroup
	for range c.cfg.Workers {
		wg.Go(func() {
			for j := range jobs {
				results <- c.process(ctx, st, j)
			}
		})
	}

	var pending []job
	if first, ok := st.seed(start); ok {
		pending = append(pending, first)
	}
	outstanding := len(pending)
	done := ctx.Done()
	for outstanding > 0 {
		var (
			out  chan<- job
			next job
		)
		if len(pending) > 0 {
			out, next = jobs, pending[0]
		}
		select {
		case out <- next:
			pending[0] = job{}
			pending = pending[1:]
		case found := <-results:
			outstanding--
			if ctx.Err() == nil {
				pending = append(pending, found...)
				outstanding += len(found)
			}
		case <-done:
			outstanding -= len(pending)
			pending = nil
			done = nil
		}
	}
	close(jobs)
	wg.Wait()

	return st.report(began, ctx.Err() != nil)
}

func (c *Crawler) process(ctx context.Context, st *state, j job) []job {
	res := c.checker.Check(ctx, j.e.u, j.crawl)
	if res.Err != nil && ctx.Err() != nil && errors.Is(res.Err, ctx.Err()) {
		// canceled mid-check, leave it unchecked
		return nil
	}
	link := st.record(j, &res)
	if c.cfg.OnResult != nil {
		c.cfg.OnResult(link)
	}

	if !j.crawl || len(res.Body) == 0 || !st.claimPage(res.FinalURL) {
		return nil
	}
	page, err := extract.Links(bytes.NewReader(res.Body))
	if err != nil {
		return nil
	}
	base := res.FinalURL
	if page.Base != "" {
		if b, err := urlnorm.Resolve(base, page.Base); err == nil {
			base = b
		}
	}

	seen := make(map[string]bool, len(page.Links))
	var found []job
	for _, l := range page.Links {
		u, err := urlnorm.Resolve(base, l.URL)
		if err != nil {
			continue
		}
		key := u.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		src := Source{Page: j.e.u.String(), Tag: l.Tag, Attr: l.Attr}
		honorNoFollow := page.NoFollow && c.cfg.Robots != nil
		follow := navigable(l.Tag) && !honorNoFollow
		if next, ok := st.discover(u, key, src, j.e.depth+1, follow); ok {
			found = append(found, next)
		}
	}
	return found
}

type entry struct {
	u     *url.URL
	depth int
	// a link first seen too deep can still be upgraded to a crawl later
	crawlScheduled bool
	link           Link
}

type state struct {
	cfg *Config

	mu        sync.Mutex
	hosts     map[string]bool
	entries   map[string]*entry
	scheduled int
	parsed    map[string]bool
	start     string
}

func newState(cfg *Config, start *url.URL) *state {
	return &state{
		cfg:     cfg,
		hosts:   map[string]bool{start.Host: true},
		entries: make(map[string]*entry),
		parsed:  make(map[string]bool),
		start:   start.String(),
	}
}

func (s *state) seed(start *url.URL) (job, bool) {
	return s.discover(start, start.String(), Source{}, 0, true)
}

// claimPage dedups parsing by final URL, so /old -> /new isn't parsed twice.
func (s *state) claimPage(final *url.URL) bool {
	if final == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := final.String()
	if !s.hosts[final.Host] || s.parsed[key] {
		return false
	}
	s.parsed[key] = true
	return true
}

func navigable(tag string) bool {
	return tag == "a" || tag == "area" || tag == "iframe"
}

func (s *state) discover(u *url.URL, key string, src Source, depth int, follow bool) (job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	internal := s.hosts[u.Host]
	e, exists := s.entries[key]
	if !exists {
		e = &entry{u: u, depth: depth, link: Link{URL: key, Internal: internal, Depth: depth}}
		s.entries[key] = e
	}
	if src.Page != "" {
		e.link.SourceCount++
		if len(e.link.Sources) < maxSources {
			e.link.Sources = append(e.link.Sources, src)
		}
	}
	if depth < e.depth {
		e.depth = depth
		e.link.Depth = depth
	}

	if exists {
		if e.link.Skipped == "" && !e.crawlScheduled && s.shouldCrawl(e, follow) {
			e.crawlScheduled = true
			s.scheduled++
			return job{e: e, crawl: true}, true
		}
		return job{}, false
	}

	switch {
	case matchAny(s.cfg.Exclude, key):
		e.link.Skipped = SkipExcluded
		return job{}, false
	case internal && s.cfg.Robots != nil && !s.cfg.Robots.Allowed(u.RequestURI()):
		e.link.Skipped = SkipRobots
		return job{}, false
	case !internal && s.cfg.SkipExternal:
		e.link.Skipped = SkipExternal
		return job{}, false
	}
	if s.shouldCrawl(e, follow) {
		e.crawlScheduled = true
		s.scheduled++
		return job{e: e, crawl: true}, true
	}
	return job{e: e}, true
}

func (s *state) shouldCrawl(e *entry, follow bool) bool {
	if !follow || !s.hosts[e.u.Host] {
		return false
	}
	if s.cfg.MaxDepth >= 0 && e.depth > s.cfg.MaxDepth {
		return false
	}
	if s.cfg.MaxPages > 0 && s.scheduled >= s.cfg.MaxPages {
		return false
	}
	if e.depth > 0 && len(s.cfg.Include) > 0 && !matchAny(s.cfg.Include, e.link.URL) {
		return false
	}
	return true
}

func (s *state) record(j job, res *check.Result) Link {
	s.mu.Lock()
	defer s.mu.Unlock()

	e := j.e
	// don't let a plain check overwrite a crawl result
	if j.crawl || !e.crawlScheduled || !e.link.Checked {
		l := &e.link
		l.Checked = true
		l.Page = j.crawl && len(res.Body) > 0
		l.Status = res.Status
		l.Method = res.Method
		l.Redirects = res.Redirects
		l.Attempts = res.Attempts
		l.Duration = res.Duration
		l.Error = check.Describe(res.Err)
		l.FinalURL = ""
		if res.FinalURL != nil && len(res.Redirects) > 0 {
			l.FinalURL = res.FinalURL.String()
		}
	}

	// start URL redirecting to www or https: treat that host as internal too
	if e.link.URL == s.start && res.FinalURL != nil {
		s.hosts[res.FinalURL.Host] = true
	}

	snap := e.link
	snap.Sources = append([]Source(nil), e.link.Sources...)
	return snap
}

func (s *state) report(began time.Time, interrupted bool) *Report {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := &Report{
		Start:       s.start,
		StartedAt:   began,
		Duration:    time.Since(began),
		Interrupted: interrupted,
		Pages:       len(s.parsed),
		Links:       make([]Link, 0, len(s.entries)),
	}
	for _, e := range s.entries {
		r.Links = append(r.Links, e.link)
	}
	sort.Slice(r.Links, func(i, k int) bool { return r.Links[i].URL < r.Links[k].URL })
	return r
}

func matchAny(res []*regexp.Regexp, s string) bool {
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}
