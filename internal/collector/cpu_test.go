package collector

import (
	"testing"
)

func TestParseProcStat(t *testing.T) {
	procPath = "testdata/proc"

	raw := parseProcStat()
	if len(raw) != 3 {
		t.Fatalf("expected 3 CPU records, got %d", len(raw))
	}

	if raw[0].id != "cpu" || raw[0].user != 2000 {
		t.Errorf("unexpected cpu total stats: %+v", raw[0])
	}
	if raw[1].id != "cpu0" || raw[1].user != 1000 {
		t.Errorf("unexpected cpu0 stats: %+v", raw[1])
	}
}

func TestCollectLoadAvg(t *testing.T) {
	procPath = "testdata/proc"

	load := collectLoadAvg()
	if load.Load1 != 1.50 || load.Load5 != 1.25 || load.Load15 != 1.10 {
		t.Errorf("unexpected load avg: %+v", load)
	}
	if load.Running != 2 || load.Total != 500 {
		t.Errorf("unexpected process counts: %d running, %d total", load.Running, load.Total)
	}
}

func TestCollectCPU(t *testing.T) {
	procPath = "testdata/proc"

	c := New()
	// First collect sets baseline
	stats := c.collectCPU(1.0)
	if stats.NumCores != 2 {
		t.Errorf("expected 2 cores, got %d", stats.NumCores)
	}
	// Total uses deltas, so on first run it should be 0s, or we can just ensure it doesn't panic
	if stats.Total.Usage != 0 {
		t.Errorf("expected 0 usage on first delta, got %v", stats.Total.Usage)
	}
}
