package collector

import (
	"testing"

	"golang.org/x/sys/unix"

	"kula/internal/config"
)

func TestClockSyncedFromTimex(t *testing.T) {
	tests := []struct {
		name string
		tx   unix.Timex
		want bool
	}{
		{"synced", unix.Timex{Status: unix.STA_PLL, Maxerror: 250000}, true},
		// systemd-timesyncd with LocalRTC=yes keeps STA_UNSYNC set while synced.
		{"synced local rtc", unix.Timex{Status: unix.STA_PLL | unix.STA_UNSYNC, Maxerror: 250000}, true},
		{"unsynced at limit", unix.Timex{Status: unix.STA_UNSYNC, Maxerror: ntpMaxErrorLimit}, false},
		{"unsynced no flag", unix.Timex{Maxerror: ntpMaxErrorLimit}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clockSyncedFromTimex(&tt.tx); got != tt.want {
				t.Errorf("clockSyncedFromTimex() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCollectSystem(t *testing.T) {
	procPath = "testdata/proc"
	sysPath = "testdata/sys"
	runPath = "testdata/run"

	c := New(config.GlobalConfig{}, config.CollectionConfig{}, config.ApplicationsConfig{}, "")
	sys := c.collectSystem()
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
	if ps.Total != 2 {
		t.Errorf("expected 2 processes, got %d", ps.Total)
	}
	if ps.Threads != 2 {
		t.Errorf("expected 2 threads, got %d", ps.Threads)
	}
	if ps.Sleeping != 2 {
		t.Errorf("expected 2 sleeping processes, got %d", ps.Sleeping)
	}
}

func TestCollectSelf(t *testing.T) {
	procPath = "testdata/proc"

	c := New(config.GlobalConfig{}, config.CollectionConfig{}, config.ApplicationsConfig{}, "")
	self := c.collectSelf(1.0)
	if self.FDs != 1 {
		t.Errorf("expected 1 FD, got %d", self.FDs)
	}
	if self.MemRSS != 16000*1024 {
		t.Errorf("expected 16MB RSS, got %d", self.MemRSS)
	}
}
