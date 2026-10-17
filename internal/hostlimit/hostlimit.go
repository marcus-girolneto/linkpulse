// Package hostlimit enforces per-host politeness: a cap on concurrent
// in-flight requests and a token-bucket request rate for every host.
package hostlimit

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Config controls the limits applied to each host.
type Config struct {
	// Rate is the sustained number of requests per second allowed per host.
	// Zero or negative disables rate limiting.
	Rate float64
	// Burst is the token bucket size. Values below 1 are treated as 1.
	Burst int
	// Concurrency caps simultaneous requests per host. Values below 1 are
	// treated as 1.
	Concurrency int
}

// Limiter hands out per-host permits. The zero value is not usable; create
// one with New. A Limiter is safe for concurrent use.
type Limiter struct {
	cfg   Config
	mu    sync.Mutex
	hosts map[string]*host
}

type host struct {
	sem     chan struct{}
	limiter *rate.Limiter
}

// New returns a Limiter applying cfg to every host it sees.
func New(cfg Config) *Limiter {
	if cfg.Burst < 1 {
		cfg.Burst = 1
	}
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 1
	}
	return &Limiter{cfg: cfg, hosts: make(map[string]*host)}
}

func (l *Limiter) get(name string) *host {
	l.mu.Lock()
	defer l.mu.Unlock()
	h, ok := l.hosts[name]
	if !ok {
		lim := rate.Inf
		if l.cfg.Rate > 0 {
			lim = rate.Limit(l.cfg.Rate)
		}
		h = &host{
			sem:     make(chan struct{}, l.cfg.Concurrency),
			limiter: rate.NewLimiter(lim, l.cfg.Burst),
		}
		l.hosts[name] = h
	}
	return h
}

// Acquire blocks until a request to host may start: a concurrency slot is
// free and the host's rate limiter grants a token. The returned release
// function must be called exactly once when the request finishes. Acquire
// returns ctx.Err() if ctx ends first, in which case nothing is held.
func (l *Limiter) Acquire(ctx context.Context, hostname string) (release func(), err error) {
	h := l.get(hostname)
	select {
	case h.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	// The slot is taken before waiting for a token so that a burst of
	// waiters cannot drain the bucket and then pile onto the host at once.
	if err := h.limiter.Wait(ctx); err != nil {
		<-h.sem
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(func() { <-h.sem }) }, nil
}

// SlowDown lowers the rate for hostname so that consecutive requests are at
// least delay apart, for example to honor a robots.txt Crawl-delay. It never
// raises the configured rate.
func (l *Limiter) SlowDown(hostname string, delay time.Duration) {
	if delay <= 0 {
		return
	}
	h := l.get(hostname)
	want := rate.Every(delay)
	if want < h.limiter.Limit() {
		h.limiter.SetLimit(want)
		h.limiter.SetBurst(1)
	}
}
