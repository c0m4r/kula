package collector

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kula/internal/config"
)

func diskTestFile(t *testing.T, root, name, value string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestResolveDiskIdentity(t *testing.T) {
	for _, tt := range []struct {
		name  string
		attrs map[string]string
		want  string
	}{
		{"nvme0n1", map[string]string{"wwid": "eui.00A07501304D79E5\n", "device/serial": "ignored"}, "wwid:eui.00a07501304d79e5"},
		{"nvme1n1", map[string]string{"uuid": "12345678-1234-1234-1234-123456789ABC", "eui": "0011223344556677"}, "wwid:uuid.12345678123412341234123456789abc"},
		{"nvme2n1", map[string]string{"uuid": "00000000-0000-0000-0000-000000000000", "nguid": "00112233445566778899aabbccddeeff"}, "wwid:eui.00112233445566778899aabbccddeeff"},
		{"nvme3n1", map[string]string{"wwid": "eui.0000000000000000", "eui": "00 11 22 33 44 55 66 77"}, "wwid:eui.0011223344556677"},
		{"sda", map[string]string{"device/wwid": "naa.5000112233445566"}, "wwid:naa.5000112233445566"},
		{"sdb", map[string]string{"device/vendor": " ATA ", "device/model": " A  Disk ", "device/serial": "SN|42 "}, "serial:ATA|A%20Disk|SN%7C42"},
		{"vda", map[string]string{"serial": "volume-42"}, "serial:||volume-42"},
		{"nvme4n2", map[string]string{"device/model": "Drive", "device/serial": "SN42", "nsid": "2"}, "serial:|Drive|SN42:ns:2"},
		{"nvme5n1", map[string]string{"device/serial": "SN42"}, ""},
		{"sdc", map[string]string{"device/serial": "unknown"}, ""},
		{"sdd", map[string]string{"device/serial": "000000"}, ""},
		{"sde", map[string]string{"wwid": "bad\x00id"}, ""},
		{"sdf", map[string]string{"wwid": strings.Repeat("x", 1025)}, ""},
		{"sdg", nil, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			base := filepath.Join(t.TempDir(), tt.name)
			for attr, value := range tt.attrs {
				diskTestFile(t, base, attr, value)
			}
			if got := resolveDiskID(base); got != tt.want {
				t.Fatalf("id = %q, want %q", got, tt.want)
			}
		})
	}
}

func diskTestEnvironment(t *testing.T) *Collector {
	t.Helper()
	oldProc, oldSys := procPath, sysPath
	procPath, sysPath = t.TempDir(), t.TempDir()
	t.Cleanup(func() { procPath, sysPath = oldProc, oldSys })
	return New(config.GlobalConfig{}, config.CollectionConfig{}, config.ApplicationsConfig{}, "")
}

func diskStatsLine(name string, counter int) string {
	return fmt.Sprintf("8 0 %s %d 0 %d 0 %d 0 %d 0 0 0 0\n", name, counter, counter, counter, counter)
}

func TestDiskIdentityRenameReplacementAndReset(t *testing.T) {
	c := diskTestEnvironment(t)
	write := func(aID, bID string, a, b int) {
		diskTestFile(t, sysPath, "class/block/sda/device/serial", aID)
		diskTestFile(t, sysPath, "class/block/sdb/device/serial", bID)
		diskTestFile(t, procPath, "diskstats", diskStatsLine("sda", a)+diskStatsLine("sdb", b))
	}
	write("A", "B", 100, 1000)
	c.collectDisks(1)
	write("A", "B", 110, 1020)
	before := c.collectDisks(1)
	if before.Devices[0].ReadsPerSec != 10 || before.Devices[1].ReadsPerSec != 20 {
		t.Fatalf("rates before rename: %+v", before.Devices)
	}
	write("B", "A", 1030, 120)
	swapped := c.collectDisks(1)
	if swapped.Devices[0].ID != before.Devices[0].ID || swapped.Devices[0].Name != "sdb" || swapped.Devices[0].ReadsPerSec != 0 {
		t.Fatalf("identity or baseline did not follow rename: %+v", swapped.Devices)
	}
	write("B", "A", 1040, 125)
	if got := c.collectDisks(1).Devices[0].ReadsPerSec; got != 5 {
		t.Fatalf("post-rename rate = %v", got)
	}
	write("B", "C", 1050, 99999) // replacement with greater counters must not spike
	for _, d := range c.collectDisks(1).Devices {
		if d.Name == "sdb" && (d.ID != "serial:||C" || d.ReadsPerSec != 0) {
			t.Fatalf("replacement reused another disk's counters: %+v", d)
		}
	}
	diskTestFile(t, sysPath, "class/block/sda/diskseq", "2")
	write("B", "C", 999999, 100000)
	if got := c.collectDisks(1).Devices[0].ReadsPerSec; got != 0 {
		t.Fatalf("new disk sequence reused counters: %v", got)
	}
	write("B", "C", 2, 3)
	for _, d := range c.collectDisks(1).Devices {
		if d.ReadsPerSec != 0 || d.ReadBytesPS != 0 {
			t.Fatalf("counter rollback: %+v", d)
		}
	}
	diskTestFile(t, procPath, "diskstats", "")
	c.collectDisks(1)
	write("B", "C", 1000, 1000)
	for _, d := range c.collectDisks(1).Devices {
		if d.ReadsPerSec != 0 {
			t.Fatalf("reappearing disk reused counters: %+v", d)
		}
	}
}

func TestDiskIdentityFilterPartitionsAndDuplicates(t *testing.T) {
	c := diskTestEnvironment(t)
	diskTestFile(t, sysPath, "class/block/sda/device/serial", "A")
	diskTestFile(t, sysPath, "class/block/sdb/device/serial", "B")
	diskTestFile(t, sysPath, "class/block/sda/sda1/partition", "1")
	if err := os.Symlink("sda/sda1", filepath.Join(sysPath, "class/block/sda1")); err != nil {
		t.Fatal(err)
	}
	diskTestFile(t, procPath, "diskstats", diskStatsLine("sda", 100)+diskStatsLine("sdb", 200)+diskStatsLine("sda1", 50))
	c.collCfg.Devices = []string{"serial:||A:part:1"}
	raw := c.parseDiskStats()
	if len(raw) != 1 || raw["sda1"].id != "serial:||A:part:1" {
		t.Fatalf("partition ID filter: %+v", raw)
	}
	// A serial containing our partition delimiter must remain a separate ID.
	diskTestFile(t, sysPath, "class/block/sdb/device/serial", "A:part:1")
	c.collCfg.Devices = []string{"sda1", "sdb"}
	raw = c.parseDiskStats()
	if raw["sdb"].id != "serial:||A%3Apart%3A1" || raw["sdb"].id == raw["sda1"].id {
		t.Fatalf("serial collided with partition identity: %+v", raw)
	}
	c.collCfg.Devices = []string{"serial:||A"}
	if got := c.parseDiskStats(); len(got) != 1 || got["sda"].id != "serial:||A" {
		t.Fatalf("whole-disk ID filter: %+v", got)
	}
	diskTestFile(t, sysPath, "class/block/sdb/device/serial", "A")
	if got := c.parseDiskStats(); len(got) != 0 {
		t.Fatalf("stable filter accepted ambiguous disk: %+v", got)
	}
	c.collCfg.Devices = []string{"sda", "sda1", "sdb"}
	got := c.parseDiskStats()
	if len(got) != 3 {
		t.Fatalf("unstable disks disappeared: %+v", got)
	}
	for name, raw := range got {
		if raw.id != "" {
			t.Errorf("duplicate identity on %s: %s", name, raw.id)
		}
	}
}

func TestListDisksIncludesUnselectedDisksAndPartitions(t *testing.T) {
	c := diskTestEnvironment(t)
	diskTestFile(t, sysPath, "class/block/sda/device/serial", "A")
	diskTestFile(t, sysPath, "class/block/sdb/device/serial", "B")
	diskTestFile(t, sysPath, "class/block/sda/sda1/partition", "1")
	if err := os.Symlink("sda/sda1", filepath.Join(sysPath, "class/block/sda1")); err != nil {
		t.Fatal(err)
	}
	diskTestFile(t, procPath, "diskstats", diskStatsLine("sdb", 20)+diskStatsLine("sda1", 5)+
		diskStatsLine("sda", 10)+diskStatsLine("vda", 30)+diskStatsLine("loop0", 40)+diskStatsLine("dm-0", 50))
	c.collCfg.Devices = []string{"sda"}
	if len(c.parseDiskStats()) != 1 {
		t.Fatal("configured monitoring filter did not apply")
	}
	disks, err := ListDisks()
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) != 4 {
		t.Fatalf("listing omitted eligible devices or included excluded devices: %+v", disks)
	}
	wantNames := []string{"sda", "sda1", "sdb", "vda"}
	c.collCfg.Devices = wantNames
	monitored := c.parseDiskStats()
	for i, disk := range disks {
		if disk.Name != wantNames[i] || disk.ID != monitored[disk.Name].id {
			t.Fatalf("listing disagrees with monitored IDs or name order: %+v", disks)
		}
	}
	if disks[1].ID != "serial:||A:part:1" || disks[3].ID != "" {
		t.Fatalf("partition or unavailable identity: %+v", disks)
	}
	diskTestFile(t, sysPath, "class/block/sdb/device/serial", "A")
	disks, err = ListDisks()
	if err != nil {
		t.Fatal(err)
	}
	for _, disk := range disks {
		if disk.ID != "" {
			t.Fatalf("listing claimed a duplicate identity was stable: %+v", disk)
		}
	}
}

func TestListDisksEmptyAndReadErrors(t *testing.T) {
	diskTestEnvironment(t)
	if _, err := ListDisks(); err == nil {
		t.Fatal("missing diskstats was silently reported as no disks")
	}
	diskTestFile(t, procPath, "diskstats", "")
	if disks, err := ListDisks(); err != nil || len(disks) != 0 {
		t.Fatalf("empty disk list: %+v, %v", disks, err)
	}
	diskTestFile(t, procPath, "diskstats", diskStatsLine("sda", 10)+strings.Repeat("x", 65537))
	if _, err := ListDisks(); err == nil {
		t.Fatal("scanner error returned a partial disk list as complete")
	}
}
