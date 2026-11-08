package collector

import (
	"strings"
	"testing"
)

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
