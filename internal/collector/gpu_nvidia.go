package collector

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// maxNvidiaLogBytes bounds a read of nvidia.log: nvidia-exporter.sh writes a
// line of about a hundred bytes per GPU.
const maxNvidiaLogBytes = 1 << 20

// parseNvidiaLog reads the entire nvidia.log once and returns stats mapped by PCI ID.
// This is more efficient for multi-GPU setups than reading the file per-GPU.
func (c *Collector) parseNvidiaLog() map[string]GPUStats {
	statsMap := make(map[string]GPUStats)

	// nvidia-exporter.sh renames a regular file into place. A link or a FIFO
	// put there instead is not read: the link is not followed, and the open
	// and the type check keep a FIFO from blocking GPU collection.
	logPath := filepath.Join(c.storageDir, "nvidia.log")
	f, err := os.OpenFile(logPath, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		c.debugf("gpu: failed to open nvidia.log: %v", err)
		return statsMap
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return statsMap
	}
	if !info.Mode().IsRegular() {
		c.debugf("gpu: nvidia.log is not a regular file (%v)", info.Mode().Type())
		return statsMap
	}

	// Hardening: check for overly permissive permissions (group/others should not have access)
	if info.Mode().Perm()&0077 != 0 {
		c.debugf("gpu: nvidia.log has overly permissive mode: %o", info.Mode().Perm())
	}

	// If the log is older than 5x the interval (min 5s), consider it stale (the exporter stopped)
	staleThreshold := c.collCfg.Interval * 5
	if staleThreshold < 5*time.Second {
		staleThreshold = 5 * time.Second
	}
	if time.Since(info.ModTime()) > staleThreshold {
		c.debugf("gpu: nvidia.log is stale (age: %v, threshold: %v)", time.Since(info.ModTime()), staleThreshold)
		return statsMap
	}

	data, err := io.ReadAll(io.LimitReader(f, maxNvidiaLogBytes))
	if err != nil {
		c.debugf("gpu: failed to read nvidia.log: %v", err)
		return statsMap
	}

	c.debugf("gpu: read nvidia.log (%d bytes)", len(data))

	// nvidia-smi CSV uses ", " as delimiter; split on "," and trim spaces
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for _, line := range lines {
		fields := strings.Split(line, ",")
		if len(fields) >= 6 {
			nvPciID := strings.ToLower(c.trimNvidiaField(fields[0]))
			if strings.HasPrefix(nvPciID, "00000000:") {
				nvPciID = nvPciID[4:]
			}

			s := GPUStats{}
			if val := c.trimNvidiaField(fields[1]); val != "" {
				s.Temperature = c.parseFloat(val, 64, "gpu.temp")
			}
			if val := c.trimNvidiaField(fields[2]); val != "" {
				s.LoadPct = c.parseFloat(val, 64, "gpu.load")
			}
			if val := c.trimNvidiaField(fields[3]); val != "" {
				s.VRAMUsed = c.parseUint(val, 10, 64, "gpu.vram.used") * 1024 * 1024
			}
			if val := c.trimNvidiaField(fields[4]); val != "" {
				s.VRAMTotal = c.parseUint(val, 10, 64, "gpu.vram.total") * 1024 * 1024
			}

			if s.VRAMTotal > 0 {
				s.VRAMUsedPct = round2(float64(s.VRAMUsed) / float64(s.VRAMTotal) * 100.0)
			}
			if val := c.trimNvidiaField(fields[5]); val != "" {
				s.PowerW = c.parseFloat(val, 64, "gpu.power")
			}

			statsMap[nvPciID] = s
		}
	}

	return statsMap
}

func (c *Collector) trimNvidiaField(s string) string {
	s = strings.TrimSpace(s)
	// Handle [N/A] values gracefully to avoid noisy parse errors in debug log
	if s == "[N/A]" || s == "N/A" || strings.HasPrefix(s, "N/A ") {
		return ""
	}
	return s
}
