package collector

import (
	"bufio"
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Block-device name prefixes for virtual/logical/optical devices that are
// excluded from disk I/O monitoring (to avoid double-counting). Kept as
// package-level byte slices so /proc/diskstats lines — which include many loop
// and dm- entries on some hosts — can be filtered without allocating a string.
var (
	prefixDM   = []byte("dm-")
	prefixMD   = []byte("md")
	prefixLoop = []byte("loop")
	prefixSR   = []byte("sr")
	prefixRAM  = []byte("ram")
	prefixZram = []byte("zram")
	prefixFD   = []byte("fd")
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

	explicitFilter := len(c.collCfg.Devices) > 0
	// Only construct device-name strings for log lines when debug output is
	// actually on this tick; the steady-state path stays allocation-light.
	dbg := c.collCfg.DebugLog && !c.debugDone
	result := make(map[string]diskRaw)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Bytes()

		// Walk the columns once, capturing only the fields we use as sub-slices
		// of the line: name=2, reads=3, sectors_read=5, writes=7,
		// sectors_written=9. Nothing is allocated until the device is kept.
		var nameB, readsB, readSectB, writesB, writeSectB []byte
		fieldIdx := 0
		pos := 0
		for pos < len(line) {
			for pos < len(line) && (line[pos] == ' ' || line[pos] == '\t') {
				pos++
			}
			if pos >= len(line) {
				break
			}
			start := pos
			for pos < len(line) && line[pos] != ' ' && line[pos] != '\t' {
				pos++
			}
			switch fieldIdx {
			case 2:
				nameB = line[start:pos]
			case 3:
				readsB = line[start:pos]
			case 5:
				readSectB = line[start:pos]
			case 7:
				writesB = line[start:pos]
			case 9:
				writeSectB = line[start:pos]
			}
			fieldIdx++
		}
		if fieldIdx < 14 {
			continue
		}

		// Skip virtual, logical, and optical devices to prevent IO double-counting:
		// dm- (device-mapper/LVM/LUKS), md (software RAID), loop, sr (optical), ram, zram, fd (floppy)
		var virtualReason string
		switch {
		case bytes.HasPrefix(nameB, prefixDM):
			virtualReason = "device-mapper (LVM/LUKS)"
		case bytes.HasPrefix(nameB, prefixMD):
			virtualReason = "software RAID"
		case bytes.HasPrefix(nameB, prefixLoop):
			virtualReason = "loop device"
		case bytes.HasPrefix(nameB, prefixSR):
			virtualReason = "optical drive"
		case bytes.HasPrefix(nameB, prefixRAM):
			virtualReason = "RAM disk"
		case bytes.HasPrefix(nameB, prefixZram):
			virtualReason = "zram (compressed RAM)"
		case bytes.HasPrefix(nameB, prefixFD):
			virtualReason = "floppy disk"
		}
		if virtualReason != "" {
			if dbg {
				c.debugf(" disk: skipping %q — virtual device (%s)", nameB, virtualReason)
			}
			continue
		}

		name := string(nameB)

		// When an explicit device list is configured, it takes full priority —
		// partitions (e.g. sda1, mmcblk0p2) are allowed if explicitly listed.
		if explicitFilter {
			allowed := false
			for _, allowedDev := range c.collCfg.Devices {
				if allowedDev == name {
					allowed = true
					break
				}
			}
			if !allowed {
				if dbg {
					c.debugf(" disk: skipping %q — not in configured devices list", name)
				}
				continue
			}
		} else if isPartition(name) {
			// Auto-discovery mode: skip partitions, only keep whole physical devices
			// to avoid double-counting IO across parent disk + its partitions.
			if dbg {
				c.debugf(" disk: skipping %q — partition (auto-discovery mode; add to 'devices' config to include)", name)
			}
			continue
		}

		d := diskRaw{
			reads:     parseUintBytes(readsB),
			readSect:  parseUintBytes(readSectB),
			writes:    parseUintBytes(writesB),
			writeSect: parseUintBytes(writeSectB),
		}
		result[name] = d
		if dbg {
			c.debugf(" disk: monitoring device %q", name)
		}
	}
	if dbg {
		if len(result) == 0 {
			c.debugf(" disk: no devices selected for monitoring")
		} else {
			c.debugf(" disk: monitoring %d device(s)", len(result))
		}
	}
	// Warn for any explicitly-configured device that was never found in /proc/diskstats
	if explicitFilter && !c.debugDone {
		for _, want := range c.collCfg.Devices {
			if _, found := result[want]; !found {
				log.Printf("Warning: configured device %q was not found in /proc/diskstats — check name or drive availability", want)
			}
		}
	}
	return result
}

// isPartition returns true if name looks like a disk partition rather than a whole device.
// Conventions by driver family:
//
//	sda, sdb, ... sdaa, ...      — whole SCSI/SATA/USB disk
//	sda1, sdb2, sdaa1            — partition (ends with digit)
//	nvme0n1                      — whole NVMe namespace
//	nvme0n1p1                    — NVMe partition (contains 'p' after 'n')
//	mmcblk0                      — whole eMMC/SD card
//	mmcblk0p1, mmcblk0p2         — eMMC/SD partition (contains 'p')
//	vda, xvda, hda               — whole virtio/Xen/IDE disk
//	vda1, xvda1, hda1            — partition (ends with digit)
func isPartition(name string) bool {
	// SCSI/SATA/USB: sda → whole disk; sda1, sdb12, sdaa1 → partition
	if strings.HasPrefix(name, "sd") && len(name) > 2 {
		lastChar := name[len(name)-1]
		if lastChar >= '0' && lastChar <= '9' {
			return true
		}
	}
	// NVMe: nvme0n1 → whole namespace; nvme0n1p1 → partition
	// Only names with 'p' are partitions (nvme0n1 has no 'p').
	if strings.HasPrefix(name, "nvme") && strings.Contains(name, "p") {
		return true
	}
	// eMMC/SD: mmcblk0 → whole device; mmcblk0p2 → partition
	if strings.HasPrefix(name, "mmcblk") && strings.Contains(name, "p") {
		return true
	}
	// Virtio (vda1), Xen (xvda1), old IDE (hda1): ends with a digit
	for _, prefix := range []string{"vd", "xvd", "hd"} {
		if strings.HasPrefix(name, prefix) && len(name) > len(prefix) {
			if name[len(name)-1] >= '0' && name[len(name)-1] <= '9' {
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
			// Guard against uint64 underflow on counter reset
			if cur.reads >= prev.reads {
				dev.ReadsPerSec = round2(float64(cur.reads-prev.reads) / elapsed)
			}
			if cur.writes >= prev.writes {
				dev.WritesPerSec = round2(float64(cur.writes-prev.writes) / elapsed)
			}
			if cur.readSect >= prev.readSect {
				dev.ReadBytesPS = float64(cur.readSect-prev.readSect) * 512.0 / elapsed
			}
			if cur.writeSect >= prev.writeSect {
				dev.WriteBytesPS = float64(cur.writeSect-prev.writeSect) * 512.0 / elapsed
			}
		}

		dev.Temperature, dev.Sensors = c.getDiskTemperature(name)

		stats.Devices = append(stats.Devices, dev)
	}

	c.prevDisk = current
	stats.FileSystems = c.collectFileSystems()
	return stats
}

// realFSTypes is the set of filesystem types considered "real" for monitoring purposes.
// tmpfs, sysfs, proc, devtmpfs, cgroup, etc. are intentionally excluded.
var realFSTypes = map[string]bool{
	// Linux native
	"ext2": true, "ext3": true, "ext4": true,
	"xfs": true, "btrfs": true, "zfs": true,
	"f2fs": true, "bcachefs": true,
	// FAT / exFAT / NTFS (boot partitions, external drives, SD cards)
	"vfat": true, "exfat": true, "ntfs": true, "ntfs3": true,
	// FUSE-based
	"fuseblk": true,
	// Network filesystems
	"nfs": true, "nfs4": true, "cifs": true, "smb3": true,
	// Container overlay
	"overlay": true,
}

func (c *Collector) collectFileSystems() []FileSystemInfo {
	// Merge two mount sources to cover both host and container-internal mounts:
	//
	//  /proc/1/mounts  — PID 1's (host systemd's) mount namespace.
	//                    When /proc is bind-mounted from the host, this exposes
	//                    all host mounts (/mnt/*, /media/*, etc.) that are
	//                    invisible inside the container's own namespace.
	//                    On bare metal this is identical to self/mounts.
	//
	//  /proc/self/mounts — the current process's mount namespace.
	//                      Adds container-specific mounts not present on the
	//                      host (Docker volumes, overlayfs, bind-mounted config
	//                      files, etc.).
	//
	// If PID 1 and self share the same mount namespace (bare metal), only
	// /proc/1/mounts is scanned to avoid duplicate debug log noise.
	// If they differ (container), both are scanned and merged.

	explicitFilter := len(c.collCfg.MountPoints) > 0
	var result []FileSystemInfo
	seen := make(map[string]bool)

	var sources []string
	switch c.collCfg.MountsDetection {
	case "host":
		sources = []string{filepath.Join(procPath, "1", "mounts")}
	case "self":
		sources = []string{filepath.Join(procPath, "mounts")}
	case "auto":
		fallthrough
	default:
		sources = []string{filepath.Join(procPath, "1", "mounts")}
		if !sameMountNamespace() {
			sources = append(sources, filepath.Join(procPath, "mounts"))
		}
	}

	for _, src := range sources {
		f, err := os.Open(src)
		if err != nil {
			// If requested source is not accessible, fall back to what's available
			// only if in "auto" mode or if it was the primary choice.
			if src == filepath.Join(procPath, "1", "mounts") {
				if f2, err2 := os.Open(filepath.Join(procPath, "mounts")); err2 == nil {
					c.scanMounts(f2, &result, seen, explicitFilter)
					_ = f2.Close()
				}
			}
			continue
		}
		c.scanMounts(f, &result, seen, explicitFilter)
		_ = f.Close()
	}

	if len(result) == 0 {
		c.debugf(" fs: no filesystems selected for monitoring")
	} else {
		c.debugf(" fs: monitoring %d filesystem(s)", len(result))
	}
	return result
}

// sameMountNamespace reports whether PID 1 and the current process share the
// same Linux mount namespace by comparing their ns/mnt symlink targets.
// Returns true (same namespace) when either symlink is unreadable — this
// conservatively avoids duplicate scanning on systems where /proc/1/ns is
// inaccessible.
func sameMountNamespace() bool {
	ns1, err1 := os.Readlink(filepath.Join(procPath, "1", "ns", "mnt"))
	nsSelf, err2 := os.Readlink(filepath.Join(procPath, "self", "ns", "mnt"))
	if err1 != nil || err2 != nil {
		return true // assume same namespace; avoids double-scan noise
	}
	return ns1 == nsSelf
}


// scanMounts reads one mounts-format file and appends matching filesystems to
// result, using seen to deduplicate by mount point across multiple sources.
func (c *Collector) scanMounts(f *os.File, result *[]FileSystemInfo, seen map[string]bool, explicitFilter bool) {
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		device := fields[0]
		mount := fields[1]
		fstype := fields[2]

		// Skip floppy disks
		if strings.HasPrefix(device, "/dev/fd") {
			c.debugf(" fs: skipping %q at %q — floppy disk", device, mount)
			continue
		}

		// Only accepted filesystem types
		if !realFSTypes[fstype] {
			c.debugf(" fs: skipping %q at %q — filesystem type %q not monitored", device, mount, fstype)
			continue
		}

		// Avoid container-injected bind mounts like /etc/resolv.conf, /etc/hostname, /etc/hosts
		if strings.HasPrefix(mount, "/etc/") {
			c.debugf(" fs: skipping %q at %q — /etc/ bind mount (container artifact)", device, mount)
			continue
		}

		// Deduplicate: same mount point appearing twice (across sources or within)
		// is always a no-op. Different mount points on the same device are allowed.
		if seen[mount] {
			c.debugf(" fs: skipping %q at %q — duplicate mount point", device, mount)
			continue
		}

		// Apply configuration filter if set
		if explicitFilter {
			allowed := false
			for _, allowedMount := range c.collCfg.MountPoints {
				if allowedMount == mount {
					allowed = true
					break
				}
			}
			if !allowed {
				c.debugf(" fs: skipping %q at %q — not in configured mountpoints list", device, mount)
				continue
			}
		}

		seen[mount] = true

		var stat syscall.Statfs_t
		if err := syscall.Statfs(mount, &stat); err != nil {
			c.debugf(" fs: skipping %q at %q — statfs error: %v", device, mount, err)
			continue
		}

		total := stat.Blocks * uint64(stat.Bsize)
		free := stat.Bavail * uint64(stat.Bsize)
		used := total - (stat.Bfree * uint64(stat.Bsize))

		var usedPct float64
		if total > 0 {
			usedPct = round2(float64(used) / float64(total) * 100.0)
		}

		c.debugf(" fs: monitoring %q at %q (type=%s)", device, mount, fstype)
		*result = append(*result, FileSystemInfo{
			Device:     device,
			MountPoint: mount,
			FSType:     fstype,
			Total:      total,
			Used:       used,
			Available:  free,
			UsedPct:    usedPct,
		})
	}
}


// getDiskTemperature attempts to read temperature for a disk device.
func (c *Collector) getDiskTemperature(devName string) (float64, []DiskTempSensor) {
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
				// Using parseInt without fieldName to avoid double logging (caller handles it below with path)
				tempMilliC := c.parseInt(valStr, 10, 64, "")
				if tempMilliC == 0 && valStr != "0" {
					c.debugf(" disk.temp: failed to parse %q (%q)", input, valStr)
					continue
				}

				// Handle absolute zero fallback (-273.15C) which is common for unavailable sensors
				if tempMilliC <= -273000 {
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
func (c *Collector) DetectDiskTjMax() float64 {
	var maxCrit float64

	matches, err := filepath.Glob(filepath.Join(sysPath, "class", "block", "*"))
	if err != nil {
		return 0
	}

	for _, match := range matches {
		name := filepath.Base(match)
		if strings.HasPrefix(name, "fd") {
			continue
		}
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
						tempMilliC := c.parseInt(valStr, 10, 64, "disk.temp_crit")
						if tempMilliC > 0 {
							val := float64(tempMilliC) / 1000.0
							if val > maxCrit {
								maxCrit = val
							}
						} else if tempMilliC < 0 && valStr != "0" {
							c.debugf(" disk.temp_crit: ignoring negative value %d at %q", tempMilliC, crit)
						}
					}
				}
			}
		}
	}

	return maxCrit
}
