package collector

import (
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// fakeStatfs replaces statfsFunc: "/hung" blocks while hang holds, every
// other mount answers at once with 100 blocks of 4 KiB.
func fakeStatfs(t *testing.T) (hang func(bool), calls *atomic.Int32) {
	t.Helper()
	calls = new(atomic.Int32)
	var mu sync.Mutex
	gate := make(chan struct{})
	close(gate)
	orig := statfsFunc
	statfsFunc = func(path string, st *syscall.Statfs_t) error {
		if path == "/hung" {
			calls.Add(1)
			mu.Lock()
			g := gate
			mu.Unlock()
			<-g
		}
		st.Blocks, st.Bsize, st.Bfree, st.Bavail = 100, 4096, 50, 40
		return nil
	}
	hung := false
	hang = func(on bool) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case on && !hung:
			gate = make(chan struct{})
		case !on && hung:
			close(gate)
		}
		hung = on
	}
	t.Cleanup(func() {
		statfsFunc = orig
	})
	return hang, calls
}

// A mount whose statfs never returns (an NFS server gone away) must cost the
// tick no more than its wait, report nothing rather than a stale reading
// forever, and hold one blocked call however many ticks ask for it.
func TestHungStatfsCannotStallCollection(t *testing.T) {
	hang, calls := fakeStatfs(t)
	hang(true)
	defer hang(false)

	var m mountStats
	for tick := 0; tick < 5; tick++ {
		started := time.Now()
		res := m.stat([]string{"/", "/hung"}, 20*time.Millisecond, time.Hour)
		if took := time.Since(started); took > 500*time.Millisecond {
			t.Fatalf("tick %d waited %v on a hung statfs", tick, took)
		}
		if !res[0].ok || res[0].stat.Blocks != 100 {
			t.Fatalf("tick %d: the responsive mount reported %+v", tick, res[0])
		}
		if res[1].ok {
			t.Fatalf("tick %d: the hung mount reported a result", tick)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("%d statfs calls in flight on the hung mount, want 1", n)
	}

	hang(false)
	deadline := time.Now().Add(2 * time.Second)
	for !m.stat([]string{"/hung"}, 20*time.Millisecond, time.Hour)[0].ok {
		if time.Now().After(deadline) {
			t.Fatal("the mount never reported after its statfs returned")
		}
	}
}

// A mount whose statfs hangs after answering keeps its last reading while it
// is recent, then reads as missing.
func TestStatfsResultExpires(t *testing.T) {
	hang, _ := fakeStatfs(t)
	var m mountStats
	const stale = 150 * time.Millisecond
	if !m.stat([]string{"/hung"}, 20*time.Millisecond, stale)[0].ok {
		t.Fatal("a responsive mount reported nothing")
	}

	hang(true)
	defer hang(false)
	if !m.stat([]string{"/hung"}, 20*time.Millisecond, stale)[0].ok {
		t.Fatal("a mount that answered moments ago lost its reading at once")
	}
	time.Sleep(stale + 50*time.Millisecond)
	if m.stat([]string{"/hung"}, 20*time.Millisecond, stale)[0].ok {
		t.Fatal("a stale reading was reported as current")
	}
}
