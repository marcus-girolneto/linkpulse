// Package hostlimit caps per-host concurrency and request rate.
package hostlimit

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Config controls the limits applied to each host.
type Config struct {
	Rate        float64 // req/s per host, <= 0 means unlimited
	Burst       int
	Concurrency int
}

// Limiter hands out per-host permits. Safe for concurrent use.
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

// Acquire blocks until a request to hostname may start. Call release when
// the request finishes. On error nothing is held.
func (l *Limiter) Acquire(ctx context.Context, hostname string) (release func(), err error) {
	h := l.get(hostname)
	select {
	case h.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	// take the slot before the token so waiters can't drain the bucket
	if err := h.limiter.Wait(ctx); err != nil {
		<-h.sem
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(func() { <-h.sem }) }, nil
}

// SlowDown spaces requests to hostname at least delay apart, never faster.
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
