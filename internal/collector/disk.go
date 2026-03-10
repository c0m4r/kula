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

func (c *Collector) parseDiskStats() map[string]diskRaw {
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

		// Skip virtual, logical, and optical devices to prevent IO double-counting
		// dm- (device-mapper/LVM/LUKS), md (software RAID), loop, sr (optical), ram, zram
		if strings.HasPrefix(name, "dm-") || strings.HasPrefix(name, "md") ||
			strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "sr") ||
			strings.HasPrefix(name, "ram") || strings.HasPrefix(name, "zram") {
			continue
		}

		// Skip partitions — only keep whole physical devices
		// Heuristic: skip if name ends with a digit and is a partition (sda1, nvme0n1p1)
		if isPartition(name) {
			continue
		}

		// Apply configuration filter if set
		if len(c.collCfg.Devices) > 0 {
			allowed := false
			for _, allowedDev := range c.collCfg.Devices {
				if allowedDev == name {
					allowed = true
					break
				}
			}
			if !allowed {
				continue
			}
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
	current := c.parseDiskStats()
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

		dev.Temperature, dev.Sensors = getDiskTemperature(name)

		stats.Devices = append(stats.Devices, dev)
	}

	c.prevDisk = current
	stats.FileSystems = c.collectFileSystems()
	return stats
}

func (c *Collector) collectFileSystems() []FileSystemInfo {
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
			"fuseblk", "nfs", "nfs4", "cifs", "overlay":
		default:
			continue
		}

		// Avoid docker specific mounts like /etc/resolv.conf, /etc/hostname, /etc/hosts
		if strings.HasPrefix(mount, "/etc/") {
			continue
		}

		// Skip duplicates
		if seen[device] {
			continue
		}

		// Apply configuration filter if set
		if len(c.collCfg.MountPoints) > 0 {
			allowed := false
			for _, allowedMount := range c.collCfg.MountPoints {
				if allowedMount == mount {
					allowed = true
					break
				}
			}
			if !allowed {
				continue
			}
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

// getDiskTemperature attempts to read temperature for a disk device.
func getDiskTemperature(devName string) (float64, []DiskTempSensor) {
	pathsToCheck := []string{
		filepath.Join(sysPath, "class", "block", devName, "device", "hwmon"),
		filepath.Join(sysPath, "class", "block", devName, "device", "device", "hwmon"),
		filepath.Join(sysPath, "class", "block", devName, "device"), // fallback for nvme direct hwmon0
	}

	var primaryTemp float64
	var sensors []DiskTempSensor

	for _, basePath := range pathsToCheck {
		entries, err := os.ReadDir(basePath)
		if err != nil {
			continue
		}

		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), "hwmon") {
				continue
			}

			hwmonDir := filepath.Join(basePath, entry.Name())

			// Find all temp*_input
			inputs, _ := filepath.Glob(filepath.Join(hwmonDir, "temp*_input"))
			if len(inputs) == 0 {
				continue
			}

			for _, input := range inputs {
				data, err := os.ReadFile(input)
				if err != nil {
					continue
				}

				valStr := strings.TrimSpace(string(data))
				tempMilliC := parseUint(valStr, 10, 64, "disk.temp")
				if tempMilliC == 0 && valStr != "0" {
					continue
				}

				tempC := round2(float64(tempMilliC) / 1000.0)

				// Fetch label if exists
				labelFile := strings.TrimSuffix(input, "_input") + "_label"
				labelName := "Temperature"
				if labelData, err := os.ReadFile(labelFile); err == nil {
					lbl := strings.TrimSpace(string(labelData))
					if lbl != "" {
						labelName = lbl
					}
				} else {
					// e.g. "temp1"
					base := filepath.Base(input)
					labelName = strings.TrimSuffix(base, "_input")
				}

				sensors = append(sensors, DiskTempSensor{
					Name:  labelName,
					Value: tempC,
				})
			}

			if len(sensors) > 0 {
				// We found sensors in this hwmon dir.
				// Find primary temp
				for _, s := range sensors {
					sNameLow := strings.ToLower(s.Name)
					if sNameLow == "composite" || sNameLow == "temp1" {
						primaryTemp = s.Value
						break
					}
				}
				if primaryTemp == 0 {
					primaryTemp = sensors[0].Value
				}
				return primaryTemp, sensors
			}
		}
	}
	return 0, nil
}

// DetectDiskTjMax returns the maximum critical temperature of any disk in Celsius, or 0 if undetected.
func DetectDiskTjMax() float64 {
	var maxCrit float64

	matches, err := filepath.Glob(filepath.Join(sysPath, "class", "block", "*"))
	if err != nil {
		return 0
	}

	for _, match := range matches {
		pathsToCheck := []string{
			filepath.Join(match, "device", "hwmon"),
			filepath.Join(match, "device", "device", "hwmon"),
			filepath.Join(match, "device"),
		}

		for _, basePath := range pathsToCheck {
			entries, err := os.ReadDir(basePath)
			if err != nil {
				continue
			}

			for _, entry := range entries {
				if !strings.HasPrefix(entry.Name(), "hwmon") {
					continue
				}

				hwmonDir := filepath.Join(basePath, entry.Name())
				crits, _ := filepath.Glob(filepath.Join(hwmonDir, "temp*_crit"))

				for _, crit := range crits {
					data, err := os.ReadFile(crit)
					if err == nil {
						valStr := strings.TrimSpace(string(data))
						tempMilliC := parseUint(valStr, 10, 64, "disk.temp_crit")
						if tempMilliC > 0 {
							val := float64(tempMilliC) / 1000.0
							if val > maxCrit {
								maxCrit = val
							}
						}
					}
				}
			}
		}
	}

	return maxCrit
}
