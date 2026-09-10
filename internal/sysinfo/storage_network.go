package sysinfo

import (
	"math"
	"net"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"kula/internal/collector"
)

func (p *Provider) disks(filesystems []Filesystem, next map[string]counters, elapsed float64) []Disk {
	result := []Disk{}
	for _, path := range matches(filepath.Join(p.sys, "class/block/*")) {
		d := Disk{Name: filepath.Base(path), Slaves: entries(filepath.Join(path, "slaves")), Mounts: []string{}}
		d.Details = p.attributes(path, map[string]string{"model": "device/model", "vendor": "device/vendor",
			"serial": "device/serial", "firmware": "device/rev", "wwid": "wwid", "device_id": "dev",
			"rotational": "queue/rotational", "removable": "removable", "read_only": "ro",
			"logical_sector_bytes": "queue/logical_block_size", "physical_sector_bytes": "queue/physical_block_size",
			"scheduler": "queue/scheduler", "state": "device/state", "volume_name": "dm/name", "raid_level": "md/level"})
		if d.Details["serial"] == "" {
			d.Details["serial"] = read(filepath.Join(path, "serial"))
		}
		if d.Details["firmware"] == "" {
			d.Details["firmware"] = read(filepath.Join(path, "device/firmware_rev"))
		}
		if n := uintValue(read(filepath.Join(path, "size"))); n != nil && *n <= math.MaxUint64/512 {
			size := *n * 512 // Linux block counters and size always use 512-byte sectors.
			d.Size = &size
		}
		resolved, _ := filepath.EvalSymlinks(path)
		if exists(filepath.Join(path, "partition")) {
			d.Details["type"] = "partition"
			if resolved != "" {
				d.Parent = filepath.Base(filepath.Dir(resolved))
			}
		} else if strings.Contains(resolved, "/virtual/") {
			d.Details["type"] = "virtual"
		} else if d.Details["rotational"] == "1" {
			d.Details["type"] = "HDD"
		} else if d.Details["rotational"] == "0" {
			d.Details["type"] = "Non-rotating"
		}
		d.Details["driver"] = linkName(filepath.Join(path, "device/driver"))
		fields := strings.Fields(read(filepath.Join(path, "stat")))
		if len(fields) >= 11 {
			values := []uint64{}
			for _, i := range []int{0, 2, 4, 6, 9} {
				n := uintValue(fields[i])
				if n == nil {
					break
				}
				values = append(values, *n)
			}
			d.InFlight = uintValue(fields[8])
			if len(values) == 5 {
				sequence := read(filepath.Join(path, "diskseq"))
				if sequence == "" && d.Parent != "" {
					sequence = read(filepath.Join(p.sys, "class/block", d.Parent, "diskseq"))
				}
				identity := resolved + ":" + sequence + ":" + d.Details["serial"] + ":" + d.Details["wwid"]
				if rates := p.rates("disk:"+d.Name, counters{identity, values}, next, elapsed); rates != nil {
					r, w, busy := rates[1]*512, rates[3]*512, math.Min(100, rates[4]/10)
					d.ReadsPS, d.WritesPS, d.ReadBPS, d.WriteBPS, d.BusyPct = &rates[0], &rates[2], &r, &w, &busy
				}
			}
		}
		for _, fs := range filesystems {
			if fs.DeviceID != "" && fs.DeviceID == d.Details["device_id"] {
				d.Mounts = append(d.Mounts, fs.Mount)
			}
		}
		result = append(result, d)
	}
	// Propagate mount associations through partitions and stacked devices (LVM,
	// dm-crypt, RAID). Keep individual filesystems separate; summing their space
	// would double-count bind mounts, pools, and shared volumes.
	indices := map[string]int{}
	for i := range result {
		indices[result[i].Name] = i
	}
	for _, disk := range result {
		visited := map[string]bool{}
		var propagate func(string)
		propagate = func(name string) {
			i, ok := indices[name]
			if !ok || visited[name] {
				return
			}
			visited[name] = true
			for _, mount := range disk.Mounts {
				found := false
				for _, existing := range result[i].Mounts {
					if existing == mount {
						found = true
						break
					}
				}
				if !found {
					result[i].Mounts = append(result[i].Mounts, mount)
				}
			}
			propagate(result[i].Parent)
			for _, slave := range result[i].Slaves {
				propagate(slave)
			}
		}
		propagate(disk.Name)
	}
	for i := range result {
		sort.Strings(result[i].Mounts)
	}
	return result
}

var mountUnescape = strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)

func (p *Provider) filesystems(sample *collector.Sample) []Filesystem {
	result := []Filesystem{}
	usage, seen := map[string]*collector.FileSystemInfo{}, map[string]bool{}
	if sample != nil {
		for i := range sample.Disks.FileSystems {
			fs := &sample.Disks.FileSystems[i]
			usage[fs.MountPoint] = fs
		}
	}
	// Only stat local filesystems. Network/FUSE usage comes from the regular
	// collector when configured, so a hardware request cannot hang on a remote FS.
	local := map[string]bool{"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true,
		"vfat": true, "exfat": true, "ntfs3": true, "f2fs": true, "tmpfs": true, "squashfs": true, "zfs": true}
	pseudo := map[string]bool{"proc": true, "sysfs": true, "devpts": true, "cgroup": true, "cgroup2": true,
		"securityfs": true, "debugfs": true, "tracefs": true, "configfs": true, "pstore": true, "mqueue": true,
		"hugetlbfs": true, "fusectl": true, "binfmt_misc": true, "autofs": true, "rpc_pipefs": true}
	for line := range strings.SplitSeq(read(filepath.Join(p.proc, "self/mountinfo")), "\n") {
		left, right, ok := strings.Cut(line, " - ")
		fields, tail := strings.Fields(left), strings.Fields(right)
		if !ok || len(fields) < 6 || len(tail) < 2 || pseudo[tail[0]] {
			continue
		}
		fs := Filesystem{Device: mountUnescape.Replace(tail[1]), Mount: mountUnescape.Replace(fields[4]), Type: tail[0], DeviceID: fields[2], Options: fields[5]}
		if seen[fs.Mount] {
			continue
		}
		seen[fs.Mount] = true
		if measured := usage[fs.Mount]; measured != nil && measured.Device == fs.Device && measured.FSType == fs.Type {
			fs.Usage = measured
		}
		if fs.Usage == nil && local[fs.Type] {
			var stat syscall.Statfs_t
			if err := syscall.Statfs(fs.Mount, &stat); err == nil && stat.Bsize > 0 && stat.Blocks > 0 && stat.Bfree <= stat.Blocks {
				total, used := stat.Blocks*uint64(stat.Bsize), (stat.Blocks-stat.Bfree)*uint64(stat.Bsize)
				fs.Usage = &collector.FileSystemInfo{Device: fs.Device, MountPoint: fs.Mount, FSType: fs.Type,
					Total: total, Used: used, Available: stat.Bavail * uint64(stat.Bsize), UsedPct: float64(used) / float64(total) * 100}
			}
		}
		result = append(result, fs)
	}
	// Include host-namespace mounts discovered by the configured collector even
	// when this process's mount namespace has no corresponding mountinfo entry.
	for mount, stat := range usage {
		if !seen[mount] {
			result = append(result, Filesystem{Device: stat.Device, Mount: mount, Type: stat.FSType, Usage: stat})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Mount < result[j].Mount })
	return result
}

func (p *Provider) network(next map[string]counters, elapsed float64) []Interface {
	result := []Interface{}
	addresses := map[string][]string{}
	ifaces, _ := net.Interfaces()
	for _, iface := range ifaces {
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			addresses[iface.Name] = append(addresses[iface.Name], addr.String())
		}
	}
	for _, path := range matches(filepath.Join(p.sys, "class/net/*")) {
		i := Interface{Name: filepath.Base(path), Addresses: []string{}}
		i.Addresses = append(i.Addresses, addresses[i.Name]...)
		sort.Strings(i.Addresses)
		i.Details = p.attributes(path, map[string]string{"mac": "address", "state": "operstate", "carrier": "carrier", "mtu": "mtu",
			"duplex": "duplex", "type": "type", "ifindex": "ifindex", "port": "phys_port_name", "numa_node": "device/numa_node"})
		i.Details["driver"], i.Details["master"] = linkName(filepath.Join(path, "device/driver")), linkName(filepath.Join(path, "master"))
		if speed := uintValue(read(filepath.Join(path, "speed"))); speed != nil && *speed > 0 && *speed < math.MaxUint32 {
			i.SpeedMbps = speed
		}
		i.RxBytes, i.TxBytes = uintValue(read(filepath.Join(path, "statistics/rx_bytes"))), uintValue(read(filepath.Join(path, "statistics/tx_bytes")))
		i.RxErrors, i.TxErrors = uintValue(read(filepath.Join(path, "statistics/rx_errors"))), uintValue(read(filepath.Join(path, "statistics/tx_errors")))
		i.RxDropped, i.TxDropped = uintValue(read(filepath.Join(path, "statistics/rx_dropped"))), uintValue(read(filepath.Join(path, "statistics/tx_dropped")))
		if i.RxBytes != nil && i.TxBytes != nil {
			identity := i.Details["ifindex"] + ":" + i.Details["mac"]
			if rates := p.rates("net:"+i.Name, counters{identity, []uint64{*i.RxBytes, *i.TxBytes}}, next, elapsed); rates != nil {
				rx, tx := rates[0]*8/1e6, rates[1]*8/1e6
				i.RxMbps, i.TxMbps = &rx, &tx
				if i.SpeedMbps != nil {
					rxPct, txPct := math.Min(100, rx/float64(*i.SpeedMbps)*100), math.Min(100, tx/float64(*i.SpeedMbps)*100)
					i.RxPct, i.TxPct = &rxPct, &txPct
				}
			}
		}
		result = append(result, i)
	}
	return result
}
