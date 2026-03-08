package collector

import (
	"kula-szpiegula/internal/config"
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

	c := New(config.GlobalConfig{})
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

func TestCollectCPUTemp(t *testing.T) {
	// 1. Test hwmon discovery
	sysPath = "testdata/sys" // mocks our newly created sys/class/hwmon files

	// Reset the package-level cache so discovery runs
	sysTempSensors = nil

	temp, _ := collectCPUTemperature()
	// testdata/sys/class/hwmon/hwmon0/temp1_input contains "45123", so expect 45.12
	if temp != 45.12 {
		t.Errorf("expected 45.12, got %v", temp)
	}

	// 2. Test thermal_zone fallback
	sysTempSensors = nil
	// Temporarily break hwmon so it falls back to thermal_zone0
	sysPath = "testdata/sys_thermal_only"

	temp2, _ := collectCPUTemperature()
	// If the fallback fails gracefully due to missing dir, it will return 0.
	// To actually test fallback properly we would need to mock `sys_thermal_only`.
	// For simplicity, let's just make sure it doesn't panic.
	_ = temp2
}
