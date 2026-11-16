package collector

import (
	"strings"
	"testing"
	"time"
)

// TestFormatForAIStartsWithTheSampleTime: a session pins its snapshot, so the
// snapshot says when it was taken, in the server's zone with its offset, and
// which host it is, with the core count beside CPU usage and load.
func TestFormatForAIStartsWithTheSampleTime(t *testing.T) {
	cet := time.FixedZone("CET", 3600)
	s := &Sample{
		Timestamp: time.Date(2026, 11, 16, 21, 14, 3, 0, time.UTC),
		System:    SystemStats{Hostname: "web1"},
		CPU:       CPUStats{Total: CPUCoreStats{Usage: 12.5}, NumCores: 8},
		LoadAvg:   LoadAvg{Load1: 3, Load5: 2, Load15: 1},
	}
	out := s.formatForAI(cet)
	want := "Sample time: 2026-11-16 22:14:03 CET (UTC+01:00)\nHost: web1\n"
	if !strings.HasPrefix(out, want) {
		t.Fatalf("snapshot does not start with the sample time and host:\n%s", out)
	}
	if !strings.Contains(out, "  CPU usage:    12.5%\n  CPU cores:    8 logical\n  Load avg:     3.00 / 2.00 / 1.00") {
		t.Errorf("core count is not beside CPU usage and load:\n%s", out)
	}

	// What a sample does not carry is left out, not shown as zero.
	out = (&Sample{}).formatForAI(cet)
	for _, absent := range []string{"Sample time", "Host:", "CPU cores"} {
		if strings.Contains(out, absent) {
			t.Errorf("empty sample shows %q:\n%s", absent, out)
		}
	}
}

// TestFormatForAIUsesCurrentApacheRates: the model is told Apache's current
// traffic, not mod_status's averages over the whole uptime.
func TestFormatForAIUsesCurrentApacheRates(t *testing.T) {
	s := &Sample{Apps: ApplicationsStats{Apache2: &Apache2Stats{
		BusyWorkers: 2, IdleWorkers: 8,
		AccessesPS: 42.5, KBytesPS: 300,
		ReqPerSec: 0.4, BytesPerSec: 2048,
	}}}
	out := s.FormatForAI()
	if !strings.Contains(out, "Traffic: 42.5 req/s, 300.0 kB/s") {
		t.Fatalf("Apache2 traffic is not the current rate:\n%s", out)
	}
}
