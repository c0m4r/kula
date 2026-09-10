package sysinfo

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kula/internal/collector"
)

func fixture(t *testing.T) (*Provider, func(string, string)) {
	t.Helper()
	root := t.TempDir()
	p := &Provider{proc: filepath.Join(root, "proc"), sys: filepath.Join(root, "sys")}
	write := func(path, text string) {
		t.Helper()
		path = filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return p, write
}

func TestInventoryHardwareAndMissingFields(t *testing.T) {
	p, write := fixture(t)
	write("sys/class/dmi/id/board_name", "Test Board\n")
	write("sys/class/dmi/id/board_vendor", "To be filled by O.E.M.\n")
	write("sys/class/dmi/id/bios_version", "1.23")
	write("sys/firmware/devicetree/base/model", "ARM Board\x00")
	write("proc/cpuinfo", "processor : 0\nmodel name : Fixture CPU\nvendor_id : Vendor\nflags : sse hypervisor\n\nprocessor : 1\nmodel name : Fixture CPU\n")
	write("sys/devices/system/cpu/cpu0/topology/physical_package_id", "0")
	write("sys/devices/system/cpu/cpu0/topology/core_id", "0")
	write("sys/devices/system/cpu/cpu1/topology/physical_package_id", "0")
	write("sys/devices/system/cpu/cpu1/topology/core_id", "0")
	for _, cpu := range []string{"cpu0", "cpu1"} {
		for key, value := range map[string]string{"level": "3", "type": "Unified", "size": "16M", "shared_cpu_list": "0-1"} {
			write("sys/devices/system/cpu/"+cpu+"/cache/index0/"+key, value)
		}
	}
	write("sys/devices/system/cpu/vulnerabilities/spectre_v2", "Mitigation: Retpolines")
	write("sys/devices/system/edac/mc/mc0/dimm0/dimm_label", "DIMM_A1")
	write("sys/devices/system/edac/mc/mc0/dimm0/size", "16384")
	write("sys/class/hwmon/hwmon0/name", "chip")
	write("sys/class/hwmon/hwmon0/temp1_input", "-5000")
	write("sys/class/hwmon/hwmon0/temp1_label", "Ambient")
	write("sys/class/hwmon/hwmon0/temp2_input", "NaN")
	write("sys/class/hwmon/hwmon0/temp3_input", "50000")
	write("sys/class/hwmon/hwmon0/temp3_fault", "1")
	write("sys/class/hwmon/hwmon0/fan1_input", "0")
	write("sys/bus/usb/devices/1-1/idVendor", "1234")
	write("sys/bus/usb/devices/1-1/product", "USB Device")
	write("sys/class/power_supply/BAT0/capacity", "0")
	write("proc/meminfo", "MemTotal: 1024 kB\nHugePages_Total: 0\n")
	sample := &collector.Sample{Timestamp: time.Now(), CPU: collector.CPUStats{Total: collector.CPUCoreStats{Usage: 37}}}
	s := p.Current(sample, "Test OS", "Test Kernel", "arm64", "host")
	if s.Board["model"] != "Test Board" || s.Board["manufacturer"] != "" || s.System["device_tree_model"] != "ARM Board" {
		t.Fatalf("bad DMI: %+v %+v", s.Board, s.System)
	}
	if s.CPU.LogicalCPUs != 2 || s.CPU.Cores != 1 || s.CPU.Sockets != 1 || len(s.CPU.Caches) != 1 {
		t.Fatalf("bad topology: %+v", s.CPU)
	}
	if s.Live.CPU.Total.Usage != 37 || !s.MetricsTime.Equal(sample.Timestamp) {
		t.Fatal("latest sample not preserved")
	}
	if len(s.Sensors) != 2 || s.Sensors[0].Value != -5 || s.Sensors[1].Value != 0 {
		t.Fatalf("sensors: %+v", s.Sensors)
	}
	if len(s.DIMMs) != 1 || s.DIMMs[0]["size_mib"] != "16384" || len(s.USB) != 1 || s.Power[0]["capacity_percent"] != "0" {
		t.Fatal("missing optional devices")
	}
	if _, err := json.Marshal(s); err != nil {
		t.Fatal(err)
	}
	// The snapshot is shared safely across clients; discovery stays bounded.
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			if got := p.Current(nil, "", "", "", ""); got != s {
				t.Error("snapshot cache not shared")
			}
		})
	}
	wg.Wait()
}

func TestMissingInventoryAndIdleBaseline(t *testing.T) {
	p, write := fixture(t)
	write("sys/class/net/test0/ifindex", "1")
	write("sys/class/net/test0/statistics/rx_bytes", "100")
	write("sys/class/net/test0/statistics/tx_bytes", "200")
	s := p.Current(nil, "", "", "", "")
	if s.Live != nil || s.MetricsTime != nil || s.Network[0].RxMbps != nil {
		t.Fatal("fabricated readings before first sample")
	}
	p.cached = nil
	p.last = time.Now().Add(-time.Minute)
	write("sys/class/net/test0/statistics/rx_bytes", "1000000")
	s = p.Current(nil, "", "", "", "")
	if s.Network[0].RxMbps != nil {
		t.Fatal("reopening must not average activity across idle time")
	}
}

func TestDiskAndNetworkRates(t *testing.T) {
	p, write := fixture(t)
	write("sys/class/block/sda/size", "2048")
	write("sys/class/block/sda/diskseq", "7")
	write("sys/class/block/sda/stat", "10 0 100 0 20 0 200 0 3 400 0")
	write("sys/class/net/test0/ifindex", "2")
	write("sys/class/net/test0/speed", "1000")
	write("sys/class/net/test0/statistics/rx_bytes", "0")
	write("sys/class/net/test0/statistics/tx_bytes", "1000")
	next := map[string]counters{}
	disk := p.disks(nil, next, 5)[0]
	nic := p.network(next, 5)[0]
	if disk.ReadBPS != nil || nic.RxMbps != nil || *disk.Size != 1048576 {
		t.Fatal("bad initial readings")
	}
	p.previous = next
	write("sys/class/block/sda/stat", "20 0 120 0 30 0 240 0 1 1400 0")
	write("sys/class/net/test0/statistics/rx_bytes", "62500000")
	write("sys/class/net/test0/statistics/tx_bytes", "31251000")
	next = map[string]counters{}
	disk, nic = p.disks(nil, next, 5)[0], p.network(next, 5)[0]
	if *disk.ReadBPS != 2048 || *disk.WriteBPS != 4096 || *disk.BusyPct != 20 || *disk.ReadsPS != 2 {
		t.Fatalf("bad disk rates: %+v", disk)
	}
	if *nic.RxMbps != 100 || *nic.TxMbps != 50 || *nic.RxPct != 10 || *nic.TxPct != 5 {
		t.Fatalf("bad NIC rates: %+v", nic)
	}
	p.previous = next
	write("sys/class/block/sda/diskseq", "8") // Replacement with larger counters.
	write("sys/class/net/test0/ifindex", "3")
	next = map[string]counters{}
	if p.disks(nil, next, 5)[0].ReadBPS != nil || p.network(next, 5)[0].RxMbps != nil {
		t.Fatal("replacement device reused old counters")
	}
	p.previous = next
	write("sys/class/block/sda/stat", "1 0 1 0 1 0 1 0 0 0 0")
	write("sys/class/net/test0/statistics/rx_bytes", "0")
	write("sys/class/net/test0/speed", "-1")
	next = map[string]counters{}
	if p.disks(nil, next, 5)[0].ReadBPS != nil {
		t.Fatal("disk reset produced rate")
	}
	nic = p.network(next, 5)[0]
	if nic.RxMbps != nil || nic.SpeedMbps != nil || nic.RxPct != nil {
		t.Fatal("reset or unknown speed produced utilization")
	}
}

func TestMountAssociationAndEscapes(t *testing.T) {
	p, write := fixture(t)
	write("proc/self/mountinfo", "24 1 253:0 / /mnt/my\\040data rw - ext4 /dev/mapper/data rw\n25 1 0:1 / /proc rw - proc proc rw\nmalformed\n")
	write("sys/class/block/sda/dev", "8:0")
	write("sys/class/block/dm-0/dev", "253:0")
	write("sys/class/block/dm-0/slaves/sda", "")
	sample := &collector.Sample{Disks: collector.DiskStats{FileSystems: []collector.FileSystemInfo{{Device: "/dev/mapper/data", FSType: "ext4", MountPoint: "/mnt/my data", Total: 100, Used: 25, UsedPct: 25}}}}
	fs := p.filesystems(sample)
	if len(fs) != 1 || fs[0].Mount != "/mnt/my data" || fs[0].Usage.UsedPct != 25 {
		t.Fatalf("mount parsing: %+v", fs)
	}
	disks := p.disks(fs, map[string]counters{}, 0)
	for _, disk := range disks {
		if len(disk.Mounts) != 1 || disk.Mounts[0] != "/mnt/my data" {
			t.Fatalf("missing mapped volume on %s: %+v", disk.Name, disk.Mounts)
		}
	}
}

func TestSMBIOSMemoryDevices(t *testing.T) {
	data := make([]byte, 0x5c)
	data[0], data[1], data[0x10], data[0x11], data[0x17], data[0x1a], data[0x12], data[0x0e] = 17, 0x5c, 1, 2, 3, 4, 0x22, 9
	binary.LittleEndian.PutUint16(data[0xc:], 0x7fff)
	binary.LittleEndian.PutUint32(data[0x1c:], 65536)
	binary.LittleEndian.PutUint16(data[0x15:], 6400)
	binary.LittleEndian.PutUint16(data[0x20:], 0xffff)
	binary.LittleEndian.PutUint32(data[0x58:], 70000)
	data = append(data, []byte("DIMM_A1\x00BANK_0\x00Vendor\x00Part\x00\x00")...)
	devices := memoryDevices(data)
	if len(devices) != 1 || devices[0]["slot"] != "DIMM_A1" || devices[0]["size_mib"] != "65536" || devices[0]["configured_speed_mts"] != "70000" || devices[0]["type"] != "DDR5" {
		t.Fatalf("memory devices: %+v", devices)
	}
	for i := range len(data) {
		if got := memoryDevices(data[:i]); len(got) != 0 {
			t.Fatalf("accepted truncated record at %d", i)
		}
	}
	data[1] = 3
	if len(memoryDevices(data)) != 0 {
		t.Fatal("accepted invalid formatted length")
	}
}

func TestMountUsageRequiresSameFilesystem(t *testing.T) {
	p, write := fixture(t)
	write("proc/self/mountinfo", "24 1 0:10 / / rw - overlay overlay rw\n")
	sample := &collector.Sample{Disks: collector.DiskStats{FileSystems: []collector.FileSystemInfo{{Device: "/dev/sda1", FSType: "ext4", MountPoint: "/", Total: 100, Used: 25}}}}
	fs := p.filesystems(sample)
	if len(fs) != 1 || fs[0].Usage != nil {
		t.Fatal("host filesystem usage attached to a different filesystem in this namespace")
	}
}

func TestPartitionReplacementDiscardsRates(t *testing.T) {
	p, write := fixture(t)
	write("sys/class/block/sda/diskseq", "1")
	write("sys/class/block/sda/sda1/partition", "1")
	write("sys/class/block/sda/sda1/stat", "10 0 100 0 20 0 200 0 0 400 0")
	if err := os.Symlink("sda/sda1", filepath.Join(p.sys, "class/block/sda1")); err != nil {
		t.Fatal(err)
	}
	next := map[string]counters{}
	p.disks(nil, next, 5)
	p.previous = next
	write("sys/class/block/sda/diskseq", "2")
	write("sys/class/block/sda/sda1/stat", "20 0 200 0 40 0 400 0 0 800 0")
	for _, disk := range p.disks(nil, map[string]counters{}, 5) {
		if disk.Name == "sda1" && (disk.Parent != "sda" || disk.ReadBPS != nil) {
			t.Fatal("partition reused counters from a replaced parent drive")
		}
	}
}

func FuzzMemoryDevices(f *testing.F) {
	f.Add([]byte{17, 4, 0, 0, 0, 0})
	f.Add([]byte(strings.Repeat("\x00", 32)))
	f.Fuzz(func(t *testing.T, data []byte) {
		devices := memoryDevices(data)
		if _, err := json.Marshal(devices); err != nil {
			t.Fatal(err)
		}
	})
}

func TestMalformedCounters(t *testing.T) {
	p, write := fixture(t)
	write("sys/class/block/bad/size", "18446744073709551615")
	write("sys/class/block/bad/stat", "1 0 bad 0 1 0 1 0 0 1 0")
	d := p.disks(nil, map[string]counters{}, 1)[0]
	if d.Size != nil || d.ReadBPS != nil {
		t.Fatal("malformed counter or overflowing size accepted")
	}
	if n := uintValue("-1"); n != nil {
		t.Fatal("negative counter accepted")
	}
	if n := uintValue("0"); n == nil || *n != 0 {
		t.Fatal("measured zero missing")
	}
	if uintValue("18446744073709551615") == nil {
		t.Fatal("64-bit counters not supported")
	}
}
