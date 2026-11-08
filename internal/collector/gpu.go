package collector

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type GPUInfo struct {
	Index      int
	Name       string
	Driver     string
	DRMPath    string
	HwmonPath  string
	PciID      string
	UseLogFile bool // true = closed NVIDIA driver, read from nvidia.log
}

// gpuRescan is how often a host without a known GPU looks again, so a driver
// loaded after startup is found. GPUs once found are kept.
const gpuRescan = time.Minute

func (c *Collector) discoverGPUs() {
	if c.gpus != nil || (!c.gpuScanAt.IsZero() && time.Since(c.gpuScanAt) < gpuRescan) {
		return
	}
	c.gpuScanAt = time.Now()

	// Check if nvidia-smi exists (local check for logging/discovery)
	hasNvidiaSmi := false
	if _, err := exec.LookPath("nvidia-smi"); err == nil {
		hasNvidiaSmi = true
		c.debugf("gpu: found nvidia-smi")
	} else {
		c.debugf("gpu: nvidia-smi not found in PATH")
	}

	c.debugf("gpu: scanning %s/class/drm for GPUs", sysPath)

	// 1. Walk /sys/class/drm/
	entries, err := os.ReadDir(filepath.Join(sysPath, "class", "drm"))
	if err != nil {
		return
	}

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "card") || strings.Contains(name, "-") {
			continue
		}

		cardPath := filepath.Join(sysPath, "class", "drm", name)
		devicePath := filepath.Join(cardPath, "device")

		// Identify driver
		driverLink, err := os.Readlink(filepath.Join(devicePath, "driver"))
		if err != nil {
			continue
		}
		driver := filepath.Base(driverLink)
		// Skip drivers that are known virtual/display-only with no monitoring value
		skipDrivers := map[string]bool{
			"virtio-pci": true,
			"bochs":      true,
			"simpledrm":  true,
			"vboxvideo":  true,
		}
		if skipDrivers[driver] {
			c.debugf("gpu: skipping virtual/headless GPU %s (driver: %s)", name, driver)
			continue
		}

		info := GPUInfo{
			Index:   len(c.gpus),
			Driver:  driver,
			DRMPath: cardPath,
		}

		// PCI ID discovery via uevent (more robust than symlink base)
		if uevent, err := os.ReadFile(filepath.Join(devicePath, "uevent")); err == nil {
			for _, line := range strings.Split(string(uevent), "\n") {
				if strings.HasPrefix(line, "PCI_SLOT_NAME=") {
					info.PciID = strings.TrimPrefix(line, "PCI_SLOT_NAME=")
					break
				}
			}
		}

		// Find hwmon path
		hwmonEntries, err := os.ReadDir(filepath.Join(devicePath, "hwmon"))
		if err == nil {
			for _, hwEntry := range hwmonEntries {
				if strings.HasPrefix(hwEntry.Name(), "hwmon") {
					info.HwmonPath = filepath.Join(devicePath, "hwmon", hwEntry.Name())
					break
				}
			}
		}

		// Determine collection method: closed NVIDIA driver doesn't expose temp in sysfs
		if driver == "nvidia" {
			_, err := os.Stat(filepath.Join(info.HwmonPath, "temp1_input"))
			info.UseLogFile = err != nil
			if info.UseLogFile && !hasNvidiaSmi {
				c.debugf("gpu[%d]: closed NVIDIA driver detected but nvidia-smi not found/no log exporter active", info.Index)
			}
		}

		// GPU Name discovery
		if info.UseLogFile {
			if info.PciID != "" {
				info.Name = c.getNvidiaName(info.PciID)
			}
		} else {
			// For AMD/Intel/Open-NVIDIA, try to get from sysfs
			if nameData, err := os.ReadFile(filepath.Join(devicePath, "product_name")); err == nil {
				info.Name = strings.TrimSpace(string(nameData))
			} else if modelData, err := os.ReadFile(filepath.Join(devicePath, "model")); err == nil {
				info.Name = strings.TrimSpace(string(modelData))
			}
		}

		if info.Name == "" {
			info.Name = strings.ToUpper(driver) + " GPU " + name[4:]
		}

		c.debugf("gpu: registered GPU %d: %q (driver: %s, pci: %s, logfile: %v)", info.Index, info.Name, info.Driver, info.PciID, info.UseLogFile)
		c.gpus = append(c.gpus, info)
	}
}

func (c *Collector) getNvidiaName(pciID string) string {
	path := filepath.Join(procPath, "driver/nvidia/gpus", pciID, "information")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "Model:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "Model:"))
		}
	}
	return ""
}

func (c *Collector) collectGPUs(elapsed float64) []GPUStats {
	c.discoverGPUs()
	if len(c.gpus) == 0 {
		return nil
	}

	nvStats := c.parseNvidiaLog()

	var stats []GPUStats
	for _, info := range c.gpus {
		s := GPUStats{
			Index:  info.Index,
			Name:   info.Name,
			Driver: info.Driver,
		}

		// Dispatch based on driver
		if info.UseLogFile {
			if nvS, ok := nvStats[strings.ToLower(info.PciID)]; ok {
				s.Temperature = nvS.Temperature
				s.LoadPct = nvS.LoadPct
				s.VRAMUsed = nvS.VRAMUsed
				s.VRAMTotal = nvS.VRAMTotal
				s.VRAMUsedPct = nvS.VRAMUsedPct
				s.PowerW = nvS.PowerW
			}
		} else {
			c.debugf("gpu[%d]: collecting stats via sysfs", info.Index)
			c.collectSysfsGPUStats(info, &s, elapsed)
		}

		// Only append if we have at least some metrics
		if s.Temperature > 0 || s.LoadPct > 0 || s.VRAMTotal > 0 || s.PowerW > 0 {
			stats = append(stats, s)
		} else {
			c.debugf("gpu[%d]: no metrics available, skipping", info.Index)
		}
	}
	return stats
}
