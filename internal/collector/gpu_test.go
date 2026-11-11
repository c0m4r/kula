package collector

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A host without a GPU must not rediscover on every tick (a PATH lookup and a
// /sys/class/drm walk each second), but still finds a GPU whose driver loads
// later, within gpuRescan.
func TestGPUDiscoveryOnAHostWithoutAGPU(t *testing.T) {
	oldSys := sysPath
	sysPath = t.TempDir()
	t.Cleanup(func() { sysPath = oldSys })

	c := &Collector{}
	c.discoverGPUs()
	if c.gpus != nil || c.gpuScanAt.IsZero() {
		t.Fatalf("discovery on a host without GPUs: gpus %v, scanned %v", c.gpus, c.gpuScanAt)
	}

	// A GPU appears: amdgpu with a hwmon directory.
	device := filepath.Join(sysPath, "class", "drm", "card0", "device")
	if err := os.MkdirAll(filepath.Join(device, "hwmon", "hwmon3"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/sys/bus/pci/drivers/amdgpu", filepath.Join(device, "driver")); err != nil {
		t.Fatal(err)
	}
	c.discoverGPUs()
	if c.gpus != nil {
		t.Fatal("a host without GPUs rediscovered on the next tick")
	}

	c.gpuScanAt = time.Now().Add(-gpuRescan)
	c.discoverGPUs()
	if len(c.gpus) != 1 || c.gpus[0].Driver != "amdgpu" {
		t.Fatalf("a GPU whose driver loaded later was not found: %+v", c.gpus)
	}
}

// nvidia.log is read only as a regular file of bounded size: a link put in
// its place is not followed, and a FIFO does not block GPU collection.
func TestNvidiaLogIsReadOnlyAsARegularFile(t *testing.T) {
	dir := t.TempDir()
	c := &Collector{storageDir: dir}
	path := filepath.Join(dir, "nvidia.log")
	line := "00000000:01:00.0, 45, 30, 1024, 8192, 120.5\n"

	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := c.parseNvidiaLog(); len(got) != 1 || got["0000:01:00.0"].Temperature != 45 {
		t.Fatalf("regular nvidia.log: %+v", got)
	}

	elsewhere := filepath.Join(t.TempDir(), "gpu.csv")
	if err := os.Rename(path, elsewhere); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, path); err != nil {
		t.Fatal(err)
	}
	if got := c.parseNvidiaLog(); len(got) != 0 {
		t.Fatalf("a linked nvidia.log was followed: %+v", got)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan map[string]GPUStats, 1)
	go func() { done <- c.parseNvidiaLog() }()
	select {
	case got := <-done:
		if len(got) != 0 {
			t.Fatalf("a FIFO was read as nvidia.log: %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading nvidia.log blocked on a FIFO")
	}
}
