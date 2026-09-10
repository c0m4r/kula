package sysinfo

import (
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
	write("sys/class/dmi/id/sys_vendor", "Test Systems\n")
	write("sys/class/dmi/id/product_name", "Test Server\n")
	write("proc/cpuinfo", "processor : 0\nmodel name : Fixture CPU\nvendor_id : Vendor\nflags : sse hypervisor\n\nprocessor : 1\nmodel name : Fixture CPU\n")
	write("sys/devices/system/cpu/cpu0/topology/physical_package_id", "0")
	write("sys/devices/system/cpu/cpu0/topology/core_id", "0")
	write("sys/devices/system/cpu/cpu1/topology/physical_package_id", "0")
	write("sys/devices/system/cpu/cpu1/topology/core_id", "0")
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
	sample := &collector.Sample{Timestamp: time.Now(), Memory: collector.MemoryStats{Total: 1024},
		System: collector.SystemStats{UptimeHuman: "2h 3m"}, GPU: []collector.GPUStats{{Name: "GPU", Driver: "test"}}}
	s := p.Current(sample, "Test OS", "Test Kernel", "arm64", "host")
	if s.System["manufacturer"] != "Test Systems" || s.System["product"] != "Test Server" {
		t.Fatalf("bad system identity: %+v", s.System)
	}
	if s.CPU.ModelName != "Fixture CPU" || s.CPU.LogicalCPUs != 2 || s.CPU.Cores != 1 {
		t.Fatalf("bad topology: %+v", s.CPU)
	}
	if s.Live.Memory.Total != 1024 || s.Live.System.UptimeHuman != "2h 3m" ||
		len(s.Live.GPU) != 1 || !s.MetricsTime.Equal(sample.Timestamp) {
		t.Fatal("latest sample not preserved")
	}
	if len(s.Sensors) != 2 || s.Sensors[0].Value != -5 || s.Sensors[1].Value != 0 {
		t.Fatalf("sensors: %+v", s.Sensors)
	}
	if len(s.USB) != 1 || s.Power[0]["capacity_percent"] != "0" {
		t.Fatal("missing optional devices")
	}
	encoded, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, obsolete := range []string{"board", "bios", "dimms", "vulnerabilities", "rx_bytes", "read_bps", "used_pct"} {
		if strings.Contains(string(encoded), `"`+obsolete+`"`) {
			t.Fatalf("obsolete technical field %q remains in snapshot", obsolete)
		}
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

func TestMissingInventoryDoesNotFabricateLiveValues(t *testing.T) {
	p, write := fixture(t)
	write("sys/class/net/test0/operstate", "down")
	s := p.Current(nil, "", "", "", "")
	if s.Live != nil || s.MetricsTime != nil {
		t.Fatal("fabricated readings before first sample")
	}
	if len(s.Network) != 1 || s.Network[0].Details["state"] != "down" {
		t.Fatal("network identity missing")
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
	if len(fs) != 1 || fs[0].Mount != "/mnt/my data" || fs[0].Usage.Total != 100 {
		t.Fatalf("mount parsing: %+v", fs)
	}
	disks := p.disks(fs)
	for _, disk := range disks {
		if len(disk.Mounts) != 1 || disk.Mounts[0] != "/mnt/my data" {
			t.Fatalf("missing mapped volume on %s: %+v", disk.Name, disk.Mounts)
		}
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

func TestMalformedCounters(t *testing.T) {
	p, write := fixture(t)
	write("sys/class/block/bad/size", "18446744073709551615")
	write("sys/class/block/bad/stat", "1 0 bad 0 1 0 1 0 0 1 0")
	d := p.disks(nil)[0]
	if d.Size != nil {
		t.Fatal("overflowing size accepted")
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
