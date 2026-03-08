package collector

import (
	"kula-szpiegula/internal/config"
	"testing"
)

func TestParseNetDev(t *testing.T) {
	procPath = "testdata/proc"

	raw := parseNetDev()
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

	raw := readTCPRaw()
	if raw.currEstab != 100 || raw.inErrs != 2 || raw.outRsts != 10 {
		t.Errorf("unexpected tcp raw stats: %+v", raw)
	}
}

func TestCollectNetwork(t *testing.T) {
	procPath = "testdata/proc"

	c := New(config.GlobalConfig{})
	// First collect sets baseline
	stats := c.collectNetwork(1.0)
	if len(stats.Interfaces) != 1 {
		t.Errorf("expected 1 interface, got %d", len(stats.Interfaces))
	}
}
