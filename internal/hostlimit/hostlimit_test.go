package hostlimit

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConcurrencyCap(t *testing.T) {
	t.Parallel()

	const capPerHost = 3
	l := New(Config{Concurrency: capPerHost})

	var inFlight, peak atomic.Int32
	var wg sync.WaitGroup
	for range 30 {
		wg.Go(func() {
			release, err := l.Acquire(t.Context(), "example.com")
			if err != nil {
				t.Error(err)
				return
			}
			defer release()
			n := inFlight.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			inFlight.Add(-1)
		})
	}
	wg.Wait()
	if got := peak.Load(); got != capPerHost {
		t.Errorf("peak concurrency = %d, want %d", got, capPerHost)
	}
}

func TestRateLimit(t *testing.T) {
	t.Parallel()

	// 11 requests at 50/s need >= 200ms
	l := New(Config{Rate: 50, Burst: 1, Concurrency: 100})
	start := time.Now()
	for range 11 {
		release, err := l.Acquire(t.Context(), "example.com")
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	if elapsed := time.Since(start); elapsed < 190*time.Millisecond {
		t.Errorf("11 requests at 50 rps took %v, want >= ~200ms", elapsed)
	}
}

func TestHostsAreIndependent(t *testing.T) {
	t.Parallel()

	l := New(Config{Rate: 1, Burst: 1, Concurrency: 1})
	hold, err := l.Acquire(t.Context(), "slow.example")
	if err != nil {
		t.Fatal(err)
	}
	defer hold()

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	release, err := l.Acquire(ctx, "fast.example")
	if err != nil {
		t.Fatalf("other host blocked by a busy host: %v", err)
	}
	release()
}

func TestAcquireHonorsContext(t *testing.T) {
	t.Parallel()

	t.Run("waiting for slot", func(t *testing.T) {
		t.Parallel()
		l := New(Config{Concurrency: 1})
		hold, err := l.Acquire(t.Context(), "h")
		if err != nil {
			t.Fatal(err)
		}
		defer hold()
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()
		if _, err := l.Acquire(ctx, "h"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want deadline exceeded", err)
		}
	})

	t.Run("waiting for token releases slot", func(t *testing.T) {
		t.Parallel()
		l := New(Config{Rate: 0.1, Burst: 1, Concurrency: 1})
		first, err := l.Acquire(t.Context(), "h")
		if err != nil {
			t.Fatal(err)
		}
		first()
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()
		if _, err := l.Acquire(ctx, "h"); err == nil {
			t.Fatal("expected error waiting for a token")
		}
		select {
		case l.get("h").sem <- struct{}{}:
		default:
			t.Fatal("slot leaked after failed Acquire")
		}
	})
}

func TestReleaseIsIdempotent(t *testing.T) {
	t.Parallel()
	l := New(Config{Concurrency: 1})
	release, err := l.Acquire(t.Context(), "h")
	if err != nil {
		t.Fatal(err)
	}
	release()
	release()
	if n := len(l.get("h").sem); n != 0 {
		t.Fatalf("slots in use = %d, want 0", n)
	}
}

func TestSlowDown(t *testing.T) {
	t.Parallel()

	l := New(Config{Rate: 1000, Burst: 10, Concurrency: 4})
	l.SlowDown("h", 30*time.Millisecond)
	start := time.Now()
	for range 4 {
		release, err := l.Acquire(t.Context(), "h")
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	if elapsed := time.Since(start); elapsed < 85*time.Millisecond {
		t.Errorf("4 requests with 30ms crawl delay took %v, want >= ~90ms", elapsed)
	}

	slow := New(Config{Rate: 1, Burst: 1})
	slow.SlowDown("h", time.Millisecond)
	if got := slow.get("h").limiter.Limit(); got != 1 {
		t.Errorf("limit = %v, want 1", got)
	}
	slow.SlowDown("h", 0)
}

func BenchmarkAcquireUnlimited(b *testing.B) {
	l := New(Config{Concurrency: 64})
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			release, err := l.Acquire(context.Background(), "example.com")
			if err != nil {
				b.Fatal(err)
			}
			release()
		}
	})
}
