package collector

import (
	"os"
	"path/filepath"
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
