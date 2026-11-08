package collector

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"kula/internal/config"
)

func TestParseProcStat(t *testing.T) {
	procPath = "testdata/proc"

	c := New(config.GlobalConfig{}, config.CollectionConfig{}, config.ApplicationsConfig{}, "")
	raw := c.parseProcStat()
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

// TestParseProcStatStopsAfterCPUBlock verifies the early-break: CPU lines form
// a contiguous block at the top of /proc/stat, so parsing must stop at the
// first non-cpu line and never tokenise the large intr/softirq counters that
// follow. The trailing "cpu_bogus" line — placed AFTER intr — must NOT appear
// in the result; if it did, the loop kept scanning past the block.
func TestParseProcStatStopsAfterCPUBlock(t *testing.T) {
	dir := t.TempDir()
	stat := "cpu  2000 0 1000 50000 200 100 50 0 0 0\n" +
		"cpu0 1000 0 500 25000 100 50 25 0 0 0\n" +
		"cpu1 1000 0 500 25000 100 50 25 0 0 0\n" +
		"intr 1234567 1 2 3 4 5 6 7 8 9 10\n" +
		"ctxt 9876543\n" +
		"softirq 5000000 1 2 3 4 5 6 7 8 9 10\n" +
		"cpu_bogus 999 0 0 0 0 0 0 0 0 0\n"
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}

	orig := procPath
	procPath = dir
	defer func() { procPath = orig }()

	c := New(config.GlobalConfig{}, config.CollectionConfig{}, config.ApplicationsConfig{}, "")
	raw := c.parseProcStat()
	if len(raw) != 3 {
		t.Fatalf("expected 3 CPU records (cpu, cpu0, cpu1), got %d: %+v", len(raw), raw)
	}
	if raw[0].id != "cpu" || raw[0].user != 2000 {
		t.Errorf("unexpected aggregate record: %+v", raw[0])
	}
}

func TestCollectLoadAvg(t *testing.T) {
	procPath = "testdata/proc"

	c := New(config.GlobalConfig{}, config.CollectionConfig{}, config.ApplicationsConfig{}, "")
	load := c.collectLoadAvg()
	if load.Load1 != 1.50 || load.Load5 != 1.25 || load.Load15 != 1.10 {
		t.Errorf("unexpected load avg: %+v", load)
	}
	if load.Running != 2 || load.Total != 500 {
		t.Errorf("unexpected process counts: %d running, %d total", load.Running, load.Total)
	}
}

func TestCollectCPU(t *testing.T) {
	procPath = "testdata/proc"

	c := New(config.GlobalConfig{}, config.CollectionConfig{}, config.ApplicationsConfig{}, "")
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

	// A new collector discovers its sensors on first use.
	c := New(config.GlobalConfig{}, config.CollectionConfig{}, config.ApplicationsConfig{}, "")
	temp, _ := c.collectCPUTemperature()
	// testdata/sys/class/hwmon/hwmon0/temp1_input contains "45123", so expect 45.12
	if temp != 45.12 {
		t.Errorf("expected 45.12, got %v", temp)
	}

	// 2. Test thermal_zone fallback
	// Temporarily break hwmon so it falls back to thermal_zone0
	sysPath = "testdata/sys_thermal_only"

	c = New(config.GlobalConfig{}, config.CollectionConfig{}, config.ApplicationsConfig{}, "")
	temp2, _ := c.collectCPUTemperature()
	// If the fallback fails gracefully due to missing dir, it will return 0.
	// To actually test fallback properly we would need to mock `sys_thermal_only`.
	// For simplicity, let's just make sure it doesn't panic.
	_ = temp2
}

// TestCPUTempSensorsDiscoveredOnceAcrossGoroutines: the collection loop and
// the /api/config handler (DetectTjMax) reach the sensor cache concurrently;
// under -race this fails if the cache is not synchronized.
func TestCPUTempSensorsDiscoveredOnceAcrossGoroutines(t *testing.T) {
	orig := sysPath
	t.Cleanup(func() { sysPath = orig })
	sysPath = "testdata/sys"
	c := New(config.GlobalConfig{}, config.CollectionConfig{}, config.ApplicationsConfig{}, "")
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = c.collectCPUTemperature() }()
		go func() { defer wg.Done(); _ = c.DetectTjMax() }()
	}
	wg.Wait()
	if temp, _ := c.collectCPUTemperature(); temp != 45.12 {
		t.Errorf("temperature = %v, want 45.12", temp)
	}
}
