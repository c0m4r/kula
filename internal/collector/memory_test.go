package collector

import (
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
