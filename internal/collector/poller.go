package collector

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// pollStale is how long a background poll's result stays current: a few
// intervals, so one slow poll does not blank a reading but a server that
// stopped answering soon reads as missing.
func pollStale(interval time.Duration) time.Duration {
	return max(3*time.Second, 3*interval)
}

// tickInterval is d, or a second when d is not positive: time.NewTicker
// panics on zero. Config loading never passes one, but a caller that skips it
// (a test, an embedding) must not take the collector down.
func tickInterval(d time.Duration) time.Duration {
	if d <= 0 {
		return time.Second
	}
	return d
}

// poller runs an application collector's polls in one goroutine and keeps the
// latest result, so a slow or unresponsive server delays only its own
// readings, never the collection tick that reads them. A tick that arrives
// while a poll runs is dropped (time.Ticker drops ticks), so polls never
// overlap: whatever the poll touches (a database session, rate baselines)
// belongs to the goroutine alone. cleanup runs once the context is done.
//
// A poll that panics fails alone (see run): the goroutine is the only one
// that runs it, and an unrecovered panic there would end the whole process.
type poller[T any] struct {
	mu     sync.Mutex
	latest *T
	at     time.Time
	stale  time.Duration
	done   chan struct{}

	// name prefixes the poller's log lines, e.g. "[mysql]".
	name string
	// discard, when set, drops what a poll that panicked was in the middle
	// of (a database session, rate baselines), so the next one starts clean.
	discard func()
	// panicked is set by a poll that panicked and cleared by one that returns
	// a result, so a streak of panics is logged once. It belongs to the poll
	// goroutine.
	panicked bool
}

func (p *poller[T]) start(ctx context.Context, interval time.Duration, poll func(context.Context) *T, cleanup func()) {
	p.done = make(chan struct{})
	go func() {
		defer close(p.done)
		defer p.guard("closing", cleanup)
		ticker := time.NewTicker(tickInterval(interval))
		defer ticker.Stop()
		for {
			if ctx.Err() != nil {
				return
			}
			if stats := p.run(ctx, poll); stats != nil {
				p.panicked = false
				p.mu.Lock()
				p.latest, p.at = stats, time.Now()
				p.mu.Unlock()
			}
			select {
			case <-ticker.C:
			case <-ctx.Done():
				return
			}
		}
	}()
}

// run runs one poll. The database drivers parse what the server sends without
// checking every length, so a malformed reply — from a broken server, or from
// whoever answers on its port — can panic inside them. A panic fails this poll
// alone: it is logged once per streak, discard drops the session or baselines
// the poll was in the middle of, and the poll counts as one that found nothing.
func (p *poller[T]) run(ctx context.Context, poll func(context.Context) *T) (stats *T) {
	defer func() {
		if r := recover(); r != nil {
			stats = nil
			if !p.panicked {
				log.Printf("%s poll panicked: %q at %s; discarding its state (logged once until a poll succeeds)",
					p.name, fmt.Sprint(r), panicSite())
				p.panicked = true
			}
			if p.discard != nil {
				p.guard("discarding a panicked poll", p.discard)
			}
		}
	}()
	return poll(ctx)
}

// guard runs f, logging a panic in it instead of letting it end the process.
func (p *poller[T]) guard(what string, f func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("%s %s panicked: %q at %s", p.name, what, fmt.Sprint(r), panicSite())
		}
	}()
	f()
}

// panicSite names where the panic being recovered started, and the caller of
// that frame (the site is often a small helper, such as a bounds-checked
// read). Called from the deferred function that recovers the panic, the
// panicking frames are still on the stack below runtime.gopanic: the first of
// them outside the runtime is the site.
func panicSite() string {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(1, pcs)])
	var site []string
	inPanic := false
	for len(site) < 2 {
		f, more := frames.Next()
		if inPanic && !strings.HasPrefix(f.Function, "runtime.") {
			site = append(site, fmt.Sprintf("%s (%s:%d)", f.Function, filepath.Base(f.File), f.Line))
		}
		inPanic = inPanic || f.Function == "runtime.gopanic"
		if !more {
			break
		}
	}
	if len(site) == 0 {
		return "an unknown site"
	}
	return strings.Join(site, ", called from ")
}

// Latest returns the newest poll result while it is current, nil otherwise.
func (p *poller[T]) Latest() *T {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.latest == nil || time.Since(p.at) > p.stale {
		return nil
	}
	return p.latest
}

// wait blocks until the poll goroutine has exited, up to timeout.
func (p *poller[T]) wait(timeout time.Duration) {
	if p.done == nil {
		return
	}
	select {
	case <-p.done:
	case <-time.After(timeout):
	}
}

// startStatusPoller polls an HTTP status page (nginx, Apache2) every interval
// in its own goroutine. collect receives the seconds since the poll before,
// whose counters are its rate baseline; reset drops that baseline after a
// poll that panicked.
func startStatusPoller[T any](ctx context.Context, p *poller[T], name string, interval time.Duration, collect func(elapsed float64) *T, reset func()) {
	p.stale = pollStale(interval)
	p.name, p.discard = name, reset
	var last time.Time
	p.start(ctx, interval, func(context.Context) *T {
		now := time.Now()
		elapsed := 1.0
		if !last.IsZero() {
			if e := now.Sub(last).Seconds(); e > 0 {
				elapsed = e
			}
		}
		last = now
		return collect(elapsed)
	}, func() {})
}

// statusPageClient fetches a status page. Its poll runs off the collection
// tick, so a request may outlast the interval; one slower still is reported
// missing rather than waited for. Redirects are not followed: a status URL
// that redirects is misconfigured, and following it could reach another host.
func statusPageClient(interval time.Duration) *http.Client {
	return &http.Client{
		Timeout: max(2*time.Second, 2*interval),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// pollLog reports a poller's failures when they change, and its recovery, so
// an application that stops answering after startup is reported once rather
// than never or every poll. It belongs to the poll goroutine.
type pollLog struct {
	name string // log prefix, e.g. "[nginx]"
	last string
}

func (l *pollLog) failf(format string, args ...any) {
	if msg := fmt.Sprintf(format, args...); msg != l.last {
		log.Printf("%s %s", l.name, msg)
		l.last = msg
	}
}

func (l *pollLog) ok() {
	if l.last != "" {
		log.Printf("%s status page answering again", l.name)
		l.last = ""
	}
}
