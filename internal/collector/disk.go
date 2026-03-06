package collector

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

type diskRaw struct {
	reads     uint64
	writes    uint64
	readSect  uint64
	writeSect uint64
}

func parseDiskStats() map[string]diskRaw {
	f, err := os.Open(filepath.Join(procPath, "diskstats"))
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	result := make(map[string]diskRaw)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 14 {
			continue
		}
		name := fields[2]

		// Skip loop devices
		if strings.HasPrefix(name, "loop") {
			continue
		}

		// Skip partitions — only keep whole devices and device-mapper
		// Heuristic: skip if name ends with a digit and is a partition (sda1, nvme0n1p1)
		if isPartition(name) {
			continue
		}

		d := diskRaw{}
		d.reads = parseUint(fields[3], 10, 64, "disk.reads")
		d.readSect = parseUint(fields[5], 10, 64, "disk.readSect")
		d.writes = parseUint(fields[7], 10, 64, "disk.writes")
		d.writeSect = parseUint(fields[9], 10, 64, "disk.writeSect")
		result[name] = d
	}
	return result
}

func isPartition(name string) bool {
	// sd[a-z][0-9] pattern
	if strings.HasPrefix(name, "sd") && len(name) > 3 {
		lastChar := name[len(name)-1]
		if lastChar >= '0' && lastChar <= '9' {
			return true
		}
	}
	// nvme0n1p1 pattern
	if strings.Contains(name, "p") && strings.HasPrefix(name, "nvme") {
		parts := strings.Split(name, "p")
		if len(parts) > 2 {
			return true
		}
		// Check if after last 'p' is a digit
		lastPart := parts[len(parts)-1]
		if len(lastPart) > 0 {
			if _, err := strconv.Atoi(lastPart); err == nil && strings.Contains(name, "n") {
				// This is a partition if the full pattern is nvme\d+n\d+p\d+
				idx := strings.LastIndex(name, "p")
				before := name[:idx]
				if strings.Contains(before, "n") {
					return true
				}
			}
		}
	}
	// vda1, xvda1 etc.
	for _, prefix := range []string{"vd", "xvd", "hd"} {
		if strings.HasPrefix(name, prefix) && len(name) > len(prefix)+1 {
			lastChar := name[len(name)-1]
			if lastChar >= '0' && lastChar <= '9' {
				return true
			}
		}
	}
	return false
}

func (c *Collector) collectDisks(elapsed float64) DiskStats {
	current := parseDiskStats()
	stats := DiskStats{}

	for name, cur := range current {
		dev := DiskDevice{
			Name: name,
		}

		if prev, ok := c.prevDisk[name]; ok && elapsed > 0 {
			dev.ReadsPerSec = round2(float64(cur.reads-prev.reads) / elapsed)
			dev.WritesPerSec = round2(float64(cur.writes-prev.writes) / elapsed)
			dev.ReadBytesPS = float64(cur.readSect-prev.readSect) * 512.0 / elapsed
			dev.WriteBytesPS = float64(cur.writeSect-prev.writeSect) * 512.0 / elapsed
		}

		stats.Devices = append(stats.Devices, dev)
	}

	c.prevDisk = current
	stats.FileSystems = collectFileSystems()
	return stats
}

func collectFileSystems() []FileSystemInfo {
	f, err := os.Open(filepath.Join(procPath, "mounts"))
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	var result []FileSystemInfo
	seen := make(map[string]bool)
	scanner := bufio.NewScanner(f)

	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		device := fields[0]
		mount := fields[1]
		fstype := fields[2]

		// Only real filesystems
		switch fstype {
		case "ext2", "ext3", "ext4", "xfs", "btrfs", "zfs", "f2fs",
			"fuseblk", "nfs", "nfs4", "cifs":
		default:
			continue
		}

		// Skip duplicates
		if seen[device] {
			continue
		}
		seen[device] = true

		var stat syscall.Statfs_t
		if err := syscall.Statfs(mount, &stat); err != nil {
			continue
		}

		total := stat.Blocks * uint64(stat.Bsize)
		free := stat.Bavail * uint64(stat.Bsize)
		used := total - (stat.Bfree * uint64(stat.Bsize))

		var usedPct float64
		if total > 0 {
			usedPct = round2(float64(used) / float64(total) * 100.0)
		}

		result = append(result, FileSystemInfo{
			Device:     device,
			MountPoint: mount,
			FSType:     fstype,
			Total:      total,
			Used:       used,
			Available:  free,
			UsedPct:    usedPct,
		})
	}
	return result
}
