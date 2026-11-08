package collector

import (
	"log"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"kula/internal/config"
)

var cadenceEpoch = time.Unix(1_700_000_000, 0)

func TestNewCadenceSchedule(t *testing.T) {
	for _, tc := range []struct {
		configured, interval time.Duration
		want                 cadenceSchedule
	}{
		{0, time.Second, cadenceSchedule{every: 10 * time.Second, slack: 500 * time.Millisecond, wait: 200 * time.Millisecond, stale: 30 * time.Second}},
		{time.Second, time.Second, cadenceSchedule{every: time.Second, slack: 500 * time.Millisecond, wait: 200 * time.Millisecond, stale: 3 * time.Second}},
		{30 * time.Second, 5 * time.Second, cadenceSchedule{every: 30 * time.Second, slack: 2500 * time.Millisecond, wait: 200 * time.Millisecond, stale: 90 * time.Second}},
		// A collection interval longer than the default reads every sample.
		{0, 15 * time.Second, cadenceSchedule{every: 15 * time.Second, slack: 7500 * time.Millisecond, wait: 200 * time.Millisecond, stale: 45 * time.Second}},
		// Collectors built without a config (tests, the TUI's defaults).
		{0, 0, cadenceSchedule{every: config.DefaultReadingInterval, slack: 500 * time.Millisecond, wait: 200 * time.Millisecond, stale: 30 * time.Second}},
	} {
		if got := newCadenceSchedule(tc.configured, tc.interval); got != tc.want {
			t.Errorf("newCadenceSchedule(%v, %v) = %+v, want %+v", tc.configured, tc.interval, got, tc.want)
		}
	}
}

// A reading due every 10 s is taken on every tenth tick, lands in the tick
// that took it, and repeats on the ticks between, even with ticks arriving a
// little early: no tick is skipped, none read twice.
func TestCadenceReadsAtItsOwnInterval(t *testing.T) {
	var c cadence[int]
	s := newCadenceSchedule(10*time.Second, time.Second)
	var reads atomic.Int32
	read := func() int { return int(reads.Add(1)) }
	for tick := 0; tick < 35; tick++ {
		now := cadenceEpoch.Add(time.Duration(tick) * (time.Second - time.Millisecond))
		r := c.get(now, s, []cadenceRead[int]{{key: "nvme0n1", read: read}})[0]
		if want := tick/10 + 1; !r.ok || r.value != want {
			t.Fatalf("tick %d: reading %+v, want %d", tick, r, want)
		}
	}
	if n := reads.Load(); n != 4 {
		t.Fatalf("%d reads in 35 ticks, want 4", n)
	}

	// Set to the collection interval, it reads on every tick.
	var everyTick cadence[int]
	s = newCadenceSchedule(time.Second, time.Second)
	for tick := 0; tick < 5; tick++ {
		r := everyTick.get(cadenceEpoch.Add(time.Duration(tick)*time.Second), s, []cadenceRead[int]{{key: "BAT1", read: read}})[0]
		if want := 5 + tick; !r.ok || r.value != want {
			t.Fatalf("every tick, tick %d: reading %+v, want %d", tick, r, want)
		}
	}
}

// A read that never returns (a failing drive's admin command) costs the tick
// no more than its wait, is not started twice, keeps the last reading while
// that is recent and then reports none, logged once each way.
func TestCadenceHungReadCannotStallTheTick(t *testing.T) {
	var logs strings.Builder
	orig := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(orig)

	c := cadence[int]{name: "[disk] temperature read on"}
	s := cadenceSchedule{every: 10 * time.Second, slack: 500 * time.Millisecond, wait: 20 * time.Millisecond, stale: 30 * time.Second}
	gate := make(chan struct{})
	var reads atomic.Int32
	read := func() int {
		n := int(reads.Add(1))
		if n == 2 {
			<-gate // the second read hangs
		}
		return n
	}
	at := func(sec int) cadenceResult[int] {
		t.Helper()
		started := time.Now()
		r := c.get(cadenceEpoch.Add(time.Duration(sec)*time.Second), s, []cadenceRead[int]{{key: "sda", read: read}})[0]
		if took := time.Since(started); took > 500*time.Millisecond {
			t.Fatalf("second %d: the tick waited %v on a hung read", sec, took)
		}
		return r
	}

	if r := at(0); !r.ok || r.value != 1 {
		t.Fatalf("first reading %+v, want 1", r)
	}
	for sec := 10; sec <= 30; sec++ {
		if r := at(sec); !r.ok || r.value != 1 {
			t.Fatalf("second %d: %+v, want the reading from 0 s while it is recent", sec, r)
		}
	}
	for sec := 31; sec <= 45; sec++ {
		if r := at(sec); r.ok {
			t.Fatalf("second %d: a stale reading was reported as current: %+v", sec, r)
		}
	}
	if n := reads.Load(); n != 2 {
		t.Fatalf("%d reads started, want 2: none beside the hung one", n)
	}

	close(gate)
	deadline := time.Now().Add(2 * time.Second)
	for sec := 46; ; sec++ {
		r := at(sec)
		if r.ok {
			if r.value != 3 {
				t.Fatalf("after the hung read returned: %+v, want a fresh read (3), not its stale result", r)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no reading after the hung read returned")
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := logs.String()
	if strings.Count(got, "[disk] temperature read on sda has not returned") != 1 ||
		strings.Count(got, "[disk] temperature read on sda answers again") != 1 {
		t.Fatalf("log:\n%s\nwant one line each way", got)
	}
}

// A read slower than the wait is not reported as missing: the tick goes on
// with the reading it has, and the slow one lands in a later tick.
func TestCadenceSlowReadIsNotLogged(t *testing.T) {
	var logs strings.Builder
	orig := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(orig)

	c := cadence[int]{name: "[psu] read of"}
	s := cadenceSchedule{every: 10 * time.Second, slack: 500 * time.Millisecond, wait: time.Millisecond, stale: 30 * time.Second}
	read := func() int { time.Sleep(50 * time.Millisecond); return 7 }
	keys := []cadenceRead[int]{{key: "/sys/class/power_supply", read: read}}
	if r := c.get(cadenceEpoch, s, keys)[0]; r.ok {
		t.Fatalf("a read that has not returned reported %+v", r)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !c.get(cadenceEpoch.Add(time.Second), s, keys)[0].ok {
		if time.Now().After(deadline) {
			t.Fatal("the slow read never landed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if logs.Len() != 0 {
		t.Fatalf("a slow read was logged:\n%s", logs.String())
	}
}

// Another disk behind a reused kernel name is read at once, not served the
// previous disk's reading until the next interval.
func TestCadenceNewIdentityReadsAtOnce(t *testing.T) {
	var c cadence[int]
	s := newCadenceSchedule(10*time.Second, time.Second)
	var reads atomic.Int32
	read := func() int { return int(reads.Add(1)) }
	get := func(sec int, ident string) int {
		t.Helper()
		r := c.get(cadenceEpoch.Add(time.Duration(sec)*time.Second), s, []cadenceRead[int]{{key: "sda", ident: ident, read: read}})[0]
		if !r.ok {
			t.Fatalf("second %d: no reading", sec)
		}
		return r.value
	}
	if get(0, "serial:A") != 1 || get(1, "serial:A") != 1 {
		t.Fatal("the same disk was read again before its interval")
	}
	if got := get(2, "serial:B"); got != 2 {
		t.Fatalf("a replaced disk was served %d, the previous disk's reading", got)
	}
}

// Keys no longer asked for (a removed disk) are forgotten.
func TestCadenceForgetsKeysNoLongerAsked(t *testing.T) {
	var c cadence[int]
	s := newCadenceSchedule(10*time.Second, time.Second)
	read := func() int { return 1 }
	c.get(cadenceEpoch, s, []cadenceRead[int]{{key: "sda", read: read}, {key: "sdb", read: read}})
	c.get(cadenceEpoch.Add(s.stale+time.Second), s, []cadenceRead[int]{{key: "sda", read: read}})
	if _, ok := c.entries["sdb"]; ok || len(c.entries) != 1 {
		t.Fatalf("entries after sdb left: %v", c.entries)
	}
}
