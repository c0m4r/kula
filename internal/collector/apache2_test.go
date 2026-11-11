package collector

import (
	"runtime"
	"strings"
	"testing"
)

// apache2LongScoreboard is a mod_status page as large as the collector reads
// (64 KiB) whose scoreboard is cut into one-character lines, each continuing
// the one before. It returns the page and its number of slots.
func apache2LongScoreboard() (body string, slots int) {
	var b strings.Builder
	b.WriteString("Total Accesses: 1\nBusyWorkers: 1\nIdleWorkers: 1\nScoreboard: W\n")
	slots = 1
	for b.Len()+2 <= 65536 {
		b.WriteString("_\n")
		slots++
	}
	return b.String(), slots
}

// TestApache2LongScoreboardParsesInLinearTime: a scoreboard continued over
// many lines is counted as it is read. Concatenating it line by line copied
// the whole scoreboard so far for each line, so the server chose the cost: a
// 64 KiB page allocated 537 MiB.
func TestApache2LongScoreboardParsesInLinearTime(t *testing.T) {
	body, slots := apache2LongScoreboard()
	c := &Collector{}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	stats := c.parseApache2Status(body, 1)
	runtime.ReadMemStats(&after)
	if stats == nil || stats.Sending != 1 || stats.Waiting != slots-1 {
		t.Fatalf("scoreboard of %d slots read as %+v", slots, stats)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 1<<20 {
		t.Fatalf("parsing a %d-byte page allocated %.1f MiB", len(body), float64(allocated)/(1<<20))
	}
}

// TestApache2ScoreboardAcrossLines: the scoreboard's continuation lines count
// like the first, and the next key ends it.
func TestApache2ScoreboardAcrossLines(t *testing.T) {
	c := &Collector{}
	stats := c.parseApache2Status("BusyWorkers: 1\nScoreboard: _W\n  RK..\nDC\nIdleWorkers: 3\nLG\n", 1)
	if stats == nil {
		t.Fatal("no stats")
	}
	got := []int{stats.Waiting, stats.Sending, stats.Reading, stats.Keepalive, stats.OpenSlots, stats.DNS, stats.Closing, stats.Logging, stats.Graceful}
	want := []int{1, 1, 1, 1, 2, 1, 1, 0, 0}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("_ W R K . D C L G = %v, want %v", got, want)
		}
	}
	if stats.IdleWorkers != 3 {
		t.Fatalf("IdleWorkers = %d, want 3", stats.IdleWorkers)
	}
}

func BenchmarkParseApache2LongScoreboard(b *testing.B) {
	body, _ := apache2LongScoreboard()
	c := &Collector{}
	b.ReportAllocs()
	for b.Loop() {
		c.parseApache2Status(body, 1)
	}
}
