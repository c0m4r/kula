package collector

import (
	"log"
	"sync"
	"syscall"
	"time"
)

// statfsFunc is syscall.Statfs; tests replace it.
var statfsFunc = syscall.Statfs

// mountStats runs statfs off the collection path. On a network filesystem
// whose server has gone away (a hard NFS mount, a stalled FUSE daemon), statfs
// blocks inside the kernel, where no timeout reaches it, and would hold up
// every later sample. Each mount point has at most one call in flight, so a
// hung one costs one blocked goroutine, not one per tick. A tick waits briefly
// for the calls; a mount whose call is still running reports its last result
// while that is recent, then nothing until the call returns.
type mountStats struct {
	mu      sync.Mutex
	entries map[string]*mountStat
}

type mountStat struct {
	running chan struct{} // set while a call is in flight, closed when it returns
	stat    syscall.Statfs_t
	err     error
	at      time.Time // when the last call returned
	asked   time.Time // the last tick that wanted this mount
	missing bool      // logged as not answering
}

// mountResult is one mount's statfs outcome. ok is false when the mount has
// no recent result, its call still running.
type mountResult struct {
	stat syscall.Statfs_t
	err  error
	ok   bool
}

// statfsWait is how long a tick waits for its statfs calls: plenty for a
// local filesystem or a responsive server, short against the interval.
func statfsWait(interval time.Duration) time.Duration {
	if interval <= 0 {
		interval = time.Second
	}
	return min(200*time.Millisecond, max(50*time.Millisecond, interval/5))
}

// stat returns the statfs result of each mount point, in order. A result
// older than stale counts as missing.
func (m *mountStats) stat(mounts []string, wait, stale time.Duration) []mountResult {
	now := time.Now()
	m.mu.Lock()
	if m.entries == nil {
		m.entries = make(map[string]*mountStat)
	}
	pending := make([]chan struct{}, 0, len(mounts))
	for _, mount := range mounts {
		e := m.entries[mount]
		if e == nil {
			e = &mountStat{}
			m.entries[mount] = e
		}
		e.asked = now
		if e.running == nil {
			e.running = make(chan struct{})
			go m.run(statfsFunc, mount, e, e.running)
		}
		pending = append(pending, e.running)
	}
	for mount, e := range m.entries {
		// Forget unmounted filesystems; a call still blocked keeps its entry.
		if e.running == nil && now.Sub(e.asked) > stale {
			delete(m.entries, mount)
		}
	}
	m.mu.Unlock()

	timer := time.NewTimer(wait)
	defer timer.Stop()
wait:
	for _, done := range pending {
		select {
		case <-done:
		case <-timer.C:
			break wait
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	results := make([]mountResult, len(mounts))
	for i, mount := range mounts {
		e := m.entries[mount]
		fresh := !e.at.IsZero() && now.Sub(e.at) <= stale
		if fresh {
			results[i] = mountResult{stat: e.stat, err: e.err, ok: true}
		}
		switch {
		case !fresh && e.running != nil && !e.missing:
			log.Printf("[disk] statfs on %s has not returned; reporting it missing until it does", mount)
			e.missing = true
		case fresh && e.missing:
			log.Printf("[disk] statfs on %s answers again", mount)
			e.missing = false
		}
	}
	return results
}

func (m *mountStats) run(statfs func(string, *syscall.Statfs_t) error, mount string, e *mountStat, done chan struct{}) {
	var st syscall.Statfs_t
	err := statfs(mount, &st)
	m.mu.Lock()
	e.stat, e.err, e.at, e.running = st, err, time.Now(), nil
	m.mu.Unlock()
	close(done)
}
