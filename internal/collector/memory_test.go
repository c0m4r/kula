package collector

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCollectMemory(t *testing.T) {
	procPath = "testdata/proc"

	mem := collectMemory()
	// Total: 16000000 kB = 16384000000 bytes
	expectedTotal := uint64(16000000 * 1024)
	if mem.Total != expectedTotal {
		t.Errorf("expected Memory Total %d, got %d", expectedTotal, mem.Total)
	}

	expectedFree := uint64(4000000 * 1024)
	if mem.Free != expectedFree {
		t.Errorf("expected Memory Free %d, got %d", expectedFree, mem.Free)
	}
}

func TestCollectSwap(t *testing.T) {
	procPath = "testdata/proc"

	swap := collectSwap()
	expectedTotal := uint64(4000000 * 1024)
	if swap.Total != expectedTotal {
		t.Errorf("expected Swap Total %d, got %d", expectedTotal, swap.Total)
	}
	if swap.Used != 0 {
		t.Errorf("expected Swap Used 0, got %d", swap.Used) // Total = Free
	}
}

// TestMemoryUsedNeverWraps: meminfo whose parts exceed the total (it is not
// an atomic snapshot, and container views of it are synthesized) reads as
// nothing used, not as an unsigned wrap to about 1.8e19 bytes.
func TestMemoryUsedNeverWraps(t *testing.T) {
	orig := procPath
	t.Cleanup(func() { procPath = orig })
	procPath = t.TempDir()
	meminfo := "MemTotal: 1000 kB\nMemFree: 600 kB\nMemAvailable: 900 kB\nBuffers: 100 kB\nCached: 400 kB\n" +
		"SwapTotal: 100 kB\nSwapFree: 200 kB\n"
	if err := os.WriteFile(filepath.Join(procPath, "meminfo"), []byte(meminfo), 0o644); err != nil {
		t.Fatal(err)
	}
	mem, swap := collectMemSwap()
	if mem.Used != 0 || mem.UsedPercent != 0 || swap.Used != 0 || swap.UsedPercent != 0 {
		t.Fatalf("used memory %d (%.2f%%), swap %d (%.2f%%), want 0", mem.Used, mem.UsedPercent, swap.Used, swap.UsedPercent)
	}
}
