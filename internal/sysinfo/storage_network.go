package sysinfo

import (
	"math"
	"net"
	"path/filepath"
	"sort"
	"strings"

	"kula/internal/collector"
)

func (p *Provider) disks(filesystems []Filesystem) []Disk {
	result := []Disk{}
	for _, path := range matches(filepath.Join(p.sys, "class/block/*")) {
		d := Disk{Name: filepath.Base(path), Slaves: entries(filepath.Join(path, "slaves")), Mounts: []string{}}
		d.Details = p.attributes(path, map[string]string{"model": "device/model", "volume_name": "dm/name"})
		deviceID := read(filepath.Join(path, "dev"))
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
		} else if rotational := read(filepath.Join(path, "queue/rotational")); rotational == "1" {
			d.Details["type"] = "HDD"
		} else if rotational == "0" {
			d.Details["type"] = "Non-rotating"
		}
		for _, fs := range filesystems {
			if fs.DeviceID != "" && fs.DeviceID == deviceID {
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
	// Space totals come from the regular collector. The inventory request never
	// stats a mount, so remote and userspace filesystems cannot delay this route.
	pseudo := map[string]bool{"proc": true, "sysfs": true, "devpts": true, "cgroup": true, "cgroup2": true,
		"securityfs": true, "debugfs": true, "tracefs": true, "configfs": true, "pstore": true, "mqueue": true,
		"hugetlbfs": true, "fusectl": true, "binfmt_misc": true, "autofs": true, "rpc_pipefs": true}
	for line := range strings.SplitSeq(read(filepath.Join(p.proc, "self/mountinfo")), "\n") {
		left, right, ok := strings.Cut(line, " - ")
		fields, tail := strings.Fields(left), strings.Fields(right)
		if !ok || len(fields) < 6 || len(tail) < 2 || pseudo[tail[0]] {
			continue
		}
		fs := Filesystem{Device: mountUnescape.Replace(tail[1]), Mount: mountUnescape.Replace(fields[4]), Type: tail[0], DeviceID: fields[2]}
		if seen[fs.Mount] {
			continue
		}
		seen[fs.Mount] = true
		if measured := usage[fs.Mount]; measured != nil && measured.Device == fs.Device && measured.FSType == fs.Type {
			fs.Usage = &FilesystemUsage{Total: measured.Total}
		}
		result = append(result, fs)
	}
	// Include host-namespace mounts discovered by the configured collector even
	// when this process's mount namespace has no corresponding mountinfo entry.
	for mount, stat := range usage {
		if !seen[mount] {
			result = append(result, Filesystem{Device: stat.Device, Mount: mount, Type: stat.FSType,
				Usage: &FilesystemUsage{Total: stat.Total}})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Mount < result[j].Mount })
	return result
}

func (p *Provider) network() []Interface {
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
		i.Details = p.attributes(path, map[string]string{"state": "operstate"})
		if driver := linkName(filepath.Join(path, "device/driver")); driver != "" {
			i.Details["driver"] = driver
		}
		if speed := uintValue(read(filepath.Join(path, "speed"))); speed != nil && *speed > 0 && *speed < math.MaxUint32 {
			i.SpeedMbps = speed
		}
		result = append(result, i)
	}
	return result
}
