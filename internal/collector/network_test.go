package collector

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"kula/internal/config"
)

func TestParseNetDev(t *testing.T) {
	procPath = "testdata/proc"

	c := New(config.GlobalConfig{}, config.CollectionConfig{}, config.ApplicationsConfig{}, "")
	raw := c.parseNetDev()
	if len(raw) != 1 {
		t.Fatalf("expected 1 interface, got %d", len(raw))
	}

	eth0, ok := raw["eth0"]
	if !ok {
		t.Fatalf("missing eth0 from parseNetDev")
	}
	if eth0.rxBytes != 1000000 || eth0.txBytes != 500000 {
		t.Errorf("unexpected eth0 stats: %+v", eth0)
	}
}

func TestParseSocketStats(t *testing.T) {
	procPath = "testdata/proc"

	sock := parseSocketStats()
	if sock.TCPInUse != 20 || sock.TCPTw != 5 || sock.UDPInUse != 10 {
		t.Errorf("unexpected socket stats: %+v", sock)
	}
}

func TestReadTCPRaw(t *testing.T) {
	procPath = "testdata/proc"

	// RetransSegs is in the Tcp line too; /proc/net/netstat has no such
	// counter, which left the retransmit rate 0 on every real host.
	raw, ok := readTCPRaw()
	if !ok || raw.currEstab != 100 || raw.inErrs != 2 || raw.outRsts != 10 || raw.retrans != 50 {
		t.Errorf("unexpected tcp raw stats: %+v (ok %v)", raw, ok)
	}
}

func TestCollectNetwork(t *testing.T) {
	procPath = "testdata/proc"

	c := New(config.GlobalConfig{}, config.CollectionConfig{}, config.ApplicationsConfig{}, "")
	// First collect sets baseline
	stats := c.collectNetwork(1.0)
	if len(stats.Interfaces) != 1 {
		t.Errorf("expected 1 interface, got %d", len(stats.Interfaces))
	}
}

// TestParseNetDevFiltering verifies that virtual interfaces are skipped in
// auto-discovery and that an explicit interfaces config bypasses all filters.
func TestParseNetDevFiltering(t *testing.T) {
	// Swap in the richer testdata file that includes lo, veth, docker, br-, virbr, tun, tap
	origDev := "testdata/proc/net/dev"
	richDev := "testdata/proc/net/dev_with_virtual"

	// Save original and replace for this test
	orig, err := os.ReadFile(origDev)
	if err != nil {
		t.Fatal(err)
	}
	rich, err := os.ReadFile(richDev)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(origDev, rich, 0644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(origDev, orig, 0644) })

	procPath = "testdata/proc"

	t.Run("auto-discovery skips virtual interfaces", func(t *testing.T) {
		c := New(config.GlobalConfig{}, config.CollectionConfig{}, config.ApplicationsConfig{}, "")
		raw := c.parseNetDev()
		// Only eth0 should survive; lo, veth0, docker0, br-abc, virbr0, tun0, tap0 must be skipped
		if len(raw) != 1 {
			t.Errorf("expected 1 interface (eth0), got %d: %v", len(raw), mapKeys(raw))
		}
		if _, ok := raw["eth0"]; !ok {
			t.Errorf("eth0 missing from results")
		}
		for _, name := range []string{"lo", "veth0", "docker0", "br-abc", "virbr0", "tun0", "tap0"} {
			if _, ok := raw[name]; ok {
				t.Errorf("virtual interface %q should have been filtered", name)
			}
		}
	})

	t.Run("explicit config allows any interface including virtual", func(t *testing.T) {
		c := New(config.GlobalConfig{}, config.CollectionConfig{
			Interfaces: []string{"eth0", "lo"},
		}, config.ApplicationsConfig{}, "")
		raw := c.parseNetDev()
		if len(raw) != 2 {
			t.Errorf("expected 2 interfaces (eth0, lo), got %d: %v", len(raw), mapKeys(raw))
		}
		if _, ok := raw["lo"]; !ok {
			t.Errorf("lo should be allowed when explicitly configured")
		}
	})
}

func mapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// TestTCPRatesFromABaseline walks the TCP counters through a quiet boot (all
// zero), a first burst, a failed read and a reset. Only a tick whose previous
// read succeeded gives rates: the first burst counts although the counters
// were zero before it, and the read after a failure does not report the
// whole counter as one second's worth.
func TestTCPRatesFromABaseline(t *testing.T) {
	orig := procPath
	t.Cleanup(func() { procPath = orig })
	procPath = t.TempDir()
	snmp := filepath.Join(procPath, "net", "snmp")
	if err := os.MkdirAll(filepath.Dir(snmp), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(retrans, inErrs, outRsts uint64) {
		t.Helper()
		body := fmt.Sprintf("Tcp: RtoAlgorithm CurrEstab RetransSegs InErrs OutRsts\nTcp: 1 3 %d %d %d\n", retrans, inErrs, outRsts)
		if err := os.WriteFile(snmp, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c := New(config.GlobalConfig{}, config.CollectionConfig{}, config.ApplicationsConfig{}, "")
	steps := []struct {
		name  string
		setup func()
		want  TCPStats
	}{
		{"first read: no baseline yet", func() { write(0, 0, 0) }, TCPStats{CurrEstab: 3}},
		{"quiet tick", func() { write(0, 0, 0) }, TCPStats{CurrEstab: 3}},
		{"first burst after a quiet boot", func() { write(40, 6, 20) }, TCPStats{CurrEstab: 3, Retrans: 20, InErrs: 3, OutRsts: 10}},
		{"failed read", func() { _ = os.Remove(snmp) }, TCPStats{}},
		{"read after a failure: baseline only", func() { write(100, 10, 50) }, TCPStats{CurrEstab: 3}},
		{"steady", func() { write(110, 12, 54) }, TCPStats{CurrEstab: 3, Retrans: 5, InErrs: 1, OutRsts: 2}},
		{"reset", func() { write(1, 0, 0) }, TCPStats{CurrEstab: 3}},
		{"after the reset", func() { write(3, 2, 4) }, TCPStats{CurrEstab: 3, Retrans: 1, InErrs: 1, OutRsts: 2}},
	}
	for _, step := range steps {
		step.setup()
		if got := c.collectTCPStats(2); got != step.want {
			t.Errorf("%s: got %+v, want %+v", step.name, got, step.want)
		}
	}
}
