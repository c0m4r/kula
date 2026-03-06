package collector

import "testing"

func TestCollectSystem(t *testing.T) {
	procPath = "testdata/proc"
	sysPath = "testdata/sys"
	runPath = "testdata/run"

	sys := collectSystem()
	if sys.Uptime != 123456.78 {
		t.Errorf("unexpected uptime: %v", sys.Uptime)
	}
	if sys.Entropy != 3200 {
		t.Errorf("unexpected entropy: %v", sys.Entropy)
	}
	if sys.ClockSource != "tsc" {
		t.Errorf("unexpected clock source: %v", sys.ClockSource)
	}
}

func TestCollectProcesses(t *testing.T) {
	procPath = "testdata/proc"

	ps := collectProcesses()
	if ps.Total != 1 {
		t.Errorf("expected 1 process, got %d", ps.Total)
	}
	if ps.Threads != 1 {
		t.Errorf("expected 1 thread, got %d", ps.Threads)
	}
	if ps.Sleeping != 1 {
		t.Errorf("expected 1 sleeping process, got %d", ps.Sleeping)
	}
}

func TestCollectSelf(t *testing.T) {
	procPath = "testdata/proc"

	c := New()
	self := c.collectSelf(1.0)
	if self.FDs != 1 {
		t.Errorf("expected 1 FD, got %d", self.FDs)
	}
	if self.MemRSS != 16000*1024 {
		t.Errorf("expected 16MB RSS, got %d", self.MemRSS)
	}
}
