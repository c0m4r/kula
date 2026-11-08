package collector

import (
	"os"
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
	raw := readTCPRaw()
	if raw.currEstab != 100 || raw.inErrs != 2 || raw.outRsts != 10 || raw.retrans != 50 {
		t.Errorf("unexpected tcp raw stats: %+v", raw)
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
