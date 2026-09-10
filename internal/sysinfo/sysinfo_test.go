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
	if s.Sensors[0].Kind != SensorTemperature || s.Sensors[1].Kind != SensorFan {
		t.Fatalf("sensor kinds not classified: %+v", s.Sensors)
	}
	if s.Live.Hottest == nil || s.Live.Hottest.Value != -5 || s.Live.Hottest.Name != "Ambient" {
		t.Fatalf("warmest sensor not reported: %+v", s.Live.Hottest)
	}
	if len(s.USB) != 1 || s.Power[0]["capacity_percent"] != "0" {
		t.Fatal("missing optional devices")
	}
	encoded, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	// used_pct was deliberately restored with the storage tab rewrite: operators
	// need free space, which the inventory alone cannot answer. The remaining
	// entries are still banned.
	for _, obsolete := range []string{"board", "bios", "dimms", "vulnerabilities", "rx_bytes", "read_bps"} {
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

// TestBlockDeviceClassificationAndTracking covers the split between real drives
// and the stacked or synthetic devices that share /sys/class/block, plus the
// link back to the devices the collector actually stores history for.
func TestBlockDeviceClassificationAndTracking(t *testing.T) {
	root := t.TempDir()
	p := &Provider{proc: filepath.Join(root, "proc"), sys: filepath.Join(root, "sys")}
	// Real /sys/class/block entries are symlinks into the device tree, and the
	// partition parent is derived from the resolved path, so the fixture has to
	// mirror that layout instead of using plain directories.
	device := func(relative string, files map[string]string) string {
		t.Helper()
		dir := filepath.Join(root, "sys/devices", relative)
		if err := os.MkdirAll(dir, 0750); err != nil {
			t.Fatal(err)
		}
		for name, text := range files {
			target := filepath.Join(dir, name)
			if err := os.MkdirAll(filepath.Dir(target), 0750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	link := func(name, target string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, "sys/class/block"), 0750); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(root, "sys/class/block", name)); err != nil {
			t.Fatal(err)
		}
	}
	device("pci0000:00/nvme/nvme0/nvme0n1", map[string]string{"queue/rotational": "0", "size": "2048"})
	device("pci0000:00/nvme/nvme0/nvme0n1/nvme0n1p1", map[string]string{"partition": "1", "size": "1024"})
	device("pci0000:00/ata1/host0/target0:0:0/0:0:0:0/block/sda", map[string]string{"queue/rotational": "1"})
	device("virtual/block/dm-0", map[string]string{"size": "1024", "dm/name": "luks-abc"})
	device("virtual/block/loop0", map[string]string{"size": "64"})
	device("virtual/block/zram0", map[string]string{"size": "512"})
	link("nvme0n1", "../../devices/pci0000:00/nvme/nvme0/nvme0n1")
	link("nvme0n1p1", "../../devices/pci0000:00/nvme/nvme0/nvme0n1/nvme0n1p1")
	link("sda", "../../devices/pci0000:00/ata1/host0/target0:0:0/0:0:0:0/block/sda")
	link("dm-0", "../../devices/virtual/block/dm-0")
	link("loop0", "../../devices/virtual/block/loop0")
	link("zram0", "../../devices/virtual/block/zram0")
	sample := &collector.Sample{Disks: collector.DiskStats{Devices: []collector.DiskDevice{
		{ID: "wwid:1", Name: "nvme0n1"}, {ID: "wwid:2", Name: "sda"},
	}}}
	s := p.Current(sample, "os", "kernel", "amd64", "host")
	byName := map[string]Disk{}
	for _, disk := range s.Disks {
		byName[disk.Name] = disk
	}
	if len(byName) != 6 {
		t.Fatalf("expected 6 block devices, got %d", len(byName))
	}
	for name, want := range map[string]string{
		"nvme0n1": ClassDisk, "sda": ClassDisk, "nvme0n1p1": ClassPartition,
		"dm-0": ClassVirtual, "loop0": ClassLoop, "zram0": ClassCompressed,
	} {
		if got := byName[name].Class; got != want {
			t.Errorf("%s class = %q, want %q", name, got, want)
		}
	}
	if byName["nvme0n1"].Medium != MediumSSD || byName["sda"].Medium != MediumHDD {
		t.Errorf("rotation medium not derived: %+v", byName)
	}
	if byName["nvme0n1p1"].Parent != "nvme0n1" {
		t.Errorf("partition parent = %q, want nvme0n1", byName["nvme0n1p1"].Parent)
	}
	if !byName["nvme0n1"].Tracked || !byName["sda"].Tracked {
		t.Error("collected devices are not marked as tracked")
	}
	if byName["dm-0"].Tracked || byName["zram0"].Tracked {
		t.Error("uncollected devices are marked as tracked")
	}
}

func TestFilesystemUsageAndTracking(t *testing.T) {
	p, write := fixture(t)
	write("proc/self/mountinfo", "24 1 253:0 / / rw - ext4 /dev/mapper/data rw\n26 1 8:1 / /mnt/nas rw - nfs4 nas:/export rw\n")
	sample := &collector.Sample{Disks: collector.DiskStats{FileSystems: []collector.FileSystemInfo{
		{Device: "/dev/mapper/data", FSType: "ext4", MountPoint: "/", Total: 1000, Used: 250, Available: 700, UsedPct: 25},
	}}}
	s := p.Current(sample, "os", "kernel", "amd64", "host")
	root := s.Filesystems[0]
	if root.Usage == nil || root.Usage.Used != 250 || root.Usage.Available != 700 || root.Usage.UsedPct != 25 {
		t.Fatalf("filesystem usage not exposed: %+v", root.Usage)
	}
	if !root.Tracked {
		t.Error("collected mount is not marked as tracked")
	}
	for _, fs := range s.Filesystems {
		if fs.Mount == "/mnt/nas" && (fs.Usage != nil || fs.Tracked) {
			t.Error("unmeasured mount gained usage or tracking")
		}
	}
}

func TestInterfaceIdentityAndKind(t *testing.T) {
	p, write := fixture(t)
	write("sys/class/net/wlan0/operstate", "up")
	write("sys/class/net/wlan0/wireless", "")
	write("sys/class/net/eno1/operstate", "up")
	write("sys/class/net/eno1/device/vendor", "0x8086")
	write("sys/class/net/docker0/operstate", "down")
	write("sys/class/net/lo/operstate", "unknown")
	sample := &collector.Sample{Network: collector.NetworkStats{Interfaces: []collector.NetInterface{{Name: "eno1"}}}}
	s := p.Current(sample, "os", "kernel", "amd64", "host")
	kinds := map[string]string{}
	for _, iface := range s.Network {
		kinds[iface.Name] = iface.Kind
	}
	for name, want := range map[string]string{
		"wlan0": KindWireless, "eno1": KindWired, "docker0": KindVirtual, "lo": KindLocal,
	} {
		if kinds[name] != want {
			t.Errorf("%s kind = %q, want %q", name, kinds[name], want)
		}
	}
	for _, iface := range s.Network {
		if iface.Name == "eno1" && !iface.Tracked {
			t.Error("collected interface is not marked as tracked")
		}
		if iface.Name == "wlan0" && iface.Tracked {
			t.Error("uncollected interface is marked as tracked")
		}
	}
}

// TestBusIdentityResolution checks that PCI and USB functions are described by
// something other than their raw bus address.
func TestBusIdentityResolution(t *testing.T) {
	p, write := fixture(t)
	write("sys/bus/pci/devices/0000:03:00.0/vendor", "0x1002")
	write("sys/bus/pci/devices/0000:03:00.0/device", "0x1638")
	write("sys/bus/pci/devices/0000:03:00.0/class", "0x030000")
	write("sys/bus/pci/devices/0000:1f.3/vendor", "0xffff")
	write("sys/bus/pci/devices/0000:1f.3/class", "0x040300")
	write("sys/bus/usb/devices/1-3/idVendor", "0bda")
	write("sys/bus/usb/devices/1-3/idProduct", "8153")
	write("sys/bus/usb/devices/1-3/bDeviceClass", "00")
	write("sys/bus/usb/devices/1-3/1-3:1.0/bInterfaceClass", "08")
	pci, usb := p.devices()
	if len(pci) != 2 || len(usb) != 1 {
		t.Fatalf("devices: %d pci, %d usb", len(pci), len(usb))
	}
	byAddress := map[string]Details{}
	for _, entry := range pci {
		byAddress[entry["address"]] = entry
	}
	if got := byAddress["0000:03:00.0"]; got["vendor"] != "AMD / ATI" || got["class"] != "display" || got["device_id"] != "1002:1638" {
		t.Errorf("known PCI vendor not resolved: %+v", got)
	}
	if got := byAddress["0000:1f.3"]; got["vendor"] != "" || got["class"] != "multimedia" {
		t.Errorf("unknown vendor invented a name: %+v", got)
	}
	if got := usb[0]; got["manufacturer"] != "Realtek Semiconductor" || got["class"] != "storage" || got["device_id"] != "0bda:8153" {
		t.Errorf("USB interface class not resolved: %+v", got)
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
