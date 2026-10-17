package cli

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marcus-girolneto/linkpulse/internal/crawl"
)

// progress renders a single, continuously rewritten status line on a
// terminal while the crawl runs.
type progress struct {
	w       io.Writer
	checked atomic.Int64
	broken  atomic.Int64
	pages   atomic.Int64
	began   time.Time

	done chan struct{}
	wg   sync.WaitGroup
}

func newProgress(w io.Writer) *progress {
	return &progress{w: w, done: make(chan struct{})}
}

func (p *progress) observe(l crawl.Link) {
	p.checked.Add(1)
	if l.Page {
		p.pages.Add(1)
	}
	if l.Broken() {
		p.broken.Add(1)
	}
}

func (p *progress) start() {
	p.began = time.Now()
	p.wg.Go(func() {
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				p.render()
			case <-p.done:
				// Erase the status line so the report starts on a clean line.
				fmt.Fprint(p.w, "\r\x1b[K")
				return
			}
		}
	})
}

func (p *progress) render() {
	elapsed := time.Since(p.began).Truncate(100 * time.Millisecond)
	fmt.Fprintf(p.w, "\r\x1b[Kchecking: %d links, %d pages, %d broken (%s)",
		p.checked.Load(), p.pages.Load(), p.broken.Load(), elapsed)
}

func (p *progress) stop() {
	close(p.done)
	p.wg.Wait()
}
