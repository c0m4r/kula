package collector

import (
	"log"
	"sync"
	"time"

	"kula/internal/config"
)

// cadence takes readings at their own interval, longer than the collection
// tick, for sources whose reads cost the device rather than Kula: each NVMe
// temperature read sends the drive a SMART command, which can keep it out of
// its deeper idle states; each battery read is a round trip to the embedded
// controller, tens of milliseconds. The samples in between repeat the last
// reading.
//
// Readings are keyed (a disk, the power supply class) and each read runs in
// its own goroutine. The tick that finds a reading due starts it and waits
// briefly, so a responsive device's reading lands in that tick's sample. A
// read that has not returned is never started again: its key keeps its last
// reading while that is recent, then reports none until the read returns. A
// failing drive whose admin command takes a minute to time out loses its own
// readings, not the sample. mountStats is the same arrangement for statfs,
// which runs every tick.
type cadence[T any] struct {
	name    string // log prefix, e.g. "[disk] temperature read on"
	mu      sync.Mutex
	entries map[string]*cadenceEntry[T]
}

type cadenceEntry[T any] struct {
	ident   string        // what the readings belong to; another one starts afresh
	running chan struct{} // set while a read is in flight, closed when it returns
	value   T
	at      time.Time // the tick that started the read behind value; zero before one returned
	started time.Time // the tick that started the last read
	asked   time.Time // the last tick that wanted this key
	missing bool      // logged as not answering
}

// cadenceRead asks for one key's reading. ident tells apart what the key
// names over time, such as the disk behind a reused kernel name. read takes
// the reading in its own goroutine, so it must not touch what the tick
// changes (package path variables included).
type cadenceRead[T any] struct {
	key, ident string
	read       func() T
}

// cadenceResult is one key's reading; ok is false while it has none current.
type cadenceResult[T any] struct {
	value T
	ok    bool
}

// cadenceSchedule says when a key is read and how long its reading lasts.
type cadenceSchedule struct {
	every time.Duration // between two reads of a key
	slack time.Duration // how early a read may come, so tick jitter cannot skip a tick
	wait  time.Duration // how long a tick waits for the reads it starts
	stale time.Duration // how long a reading stays current
}

// newCadenceSchedule reads every configured interval, resolved as the config
// resolves it, on a collection tick of interval.
func newCadenceSchedule(configured, interval time.Duration) cadenceSchedule {
	if interval <= 0 {
		interval = time.Second
	}
	every := config.ReadingInterval(configured, interval)
	return cadenceSchedule{
		every: every,
		slack: interval / 2,
		wait:  statfsWait(interval),
		stale: pollStale(every),
	}
}

// get returns the reading of each key, in order, first starting the reads
// that are due: a key never read, one last read every ago, or one that now
// names another identity. Keys no longer asked for are forgotten once their
// readings would have gone stale.
func (c *cadence[T]) get(now time.Time, s cadenceSchedule, reads []cadenceRead[T]) []cadenceResult[T] {
	c.mu.Lock()
	if c.entries == nil {
		c.entries = make(map[string]*cadenceEntry[T])
	}
	var pending []chan struct{}
	for _, r := range reads {
		e := c.entries[r.key]
		if e == nil || e.ident != r.ident {
			// A read still running for the old identity returns into the
			// replaced entry, where nothing reads it.
			e = &cadenceEntry[T]{ident: r.ident}
			c.entries[r.key] = e
		}
		e.asked = now
		if e.running == nil && (e.started.IsZero() || now.Sub(e.started) >= s.every-s.slack) {
			e.running, e.started = make(chan struct{}), now
			go c.run(e, now, r.read)
			pending = append(pending, e.running)
		}
	}
	for key, e := range c.entries {
		if e.running == nil && now.Sub(e.asked) > s.stale {
			delete(c.entries, key)
		}
	}
	c.mu.Unlock()

	if len(pending) > 0 {
		timer := time.NewTimer(s.wait)
		defer timer.Stop()
	wait:
		for _, done := range pending {
			select {
			case <-done:
			case <-timer.C:
				break wait
			}
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	results := make([]cadenceResult[T], len(reads))
	for i, r := range reads {
		e := c.entries[r.key]
		fresh := !e.at.IsZero() && now.Sub(e.at) <= s.stale
		if fresh {
			results[i] = cadenceResult[T]{value: e.value, ok: true}
		}
		switch {
		// A read that is merely slow is not reported: only one that has
		// outlasted its interval with nothing current to show.
		case !fresh && e.running != nil && now.Sub(e.started) >= s.every && !e.missing:
			log.Printf("%s %s has not returned; reporting it missing until it does", c.name, r.key)
			e.missing = true
		case fresh && e.missing:
			log.Printf("%s %s answers again", c.name, r.key)
			e.missing = false
		}
	}
	return results
}

// run takes one reading into e for the tick at started.
func (c *cadence[T]) run(e *cadenceEntry[T], started time.Time, read func() T) {
	value := read()
	c.mu.Lock()
	e.value, e.at = value, started
	done := e.running
	e.running = nil
	c.mu.Unlock()
	close(done)
}
