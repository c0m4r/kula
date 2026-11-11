package collector

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBoundString(t *testing.T) {
	for _, short := range []string{"", "eth0", strings.Repeat("x", MaxStoredString), strings.Repeat("é", MaxStoredString/2)} {
		if got := BoundString(short); got != short {
			t.Errorf("BoundString of %d bytes changed it", len(short))
		}
	}

	long := "/mnt/" + strings.Repeat("ł", 300) // two-byte runes straddle the cut
	got := BoundString(long)
	if len(got) > MaxStoredString || !utf8.ValidString(got) {
		t.Fatalf("BoundString = %d bytes, valid UTF-8 %v", len(got), utf8.ValidString(got))
	}
	if !strings.HasPrefix(got, "/mnt/łł") || !strings.Contains(got, "…") {
		t.Errorf("BoundString = %q, want the start of the name and an ellipsis", got)
	}
	if BoundString(long) != got {
		t.Error("BoundString is not deterministic")
	}
	if BoundString(got) != got {
		t.Error("a bounded string changes when bounded again")
	}
	// Names that differ only past the cut stay distinct: they are identities.
	if BoundString(long+"a") == BoundString(long+"b") {
		t.Error("two long names sharing a prefix bound to the same string")
	}
	// Not UTF-8 to begin with: still bounded, never longer.
	if got := BoundString(strings.Repeat("\x80", 400)); len(got) > MaxStoredString {
		t.Errorf("invalid UTF-8 bounded to %d bytes", len(got))
	}
}

// Every name and label in a sample is bounded before anyone sees it, and a
// slice a subsystem may hand to several samples is copied, not changed.
func TestBoundSampleStrings(t *testing.T) {
	long := strings.Repeat("n", 300)
	bounded := BoundString(long)
	containers := []ContainerStats{{ID: "abc", Name: "web"}, {ID: "def", Name: long}}
	filesystems := []FileSystemInfo{{Device: long, MountPoint: long, FSType: long}}
	s := &Sample{
		System:  SystemStats{Hostname: long, ClockSource: long},
		Network: NetworkStats{Interfaces: []NetInterface{{Name: long}}},
		CPU:     CPUStats{Sensors: []CPUTempSensor{{Name: long}}},
		Disks: DiskStats{
			Devices:     []DiskDevice{{Name: "sda", Sensors: []DiskTempSensor{{Name: long}}}},
			FileSystems: filesystems,
		},
		GPU:  []GPUStats{{Name: long, Driver: long}},
		PSU:  []PowerSupplyStats{{Name: long, Type: long, Status: long}},
		Apps: ApplicationsStats{Containers: containers},
	}
	boundSampleStrings(s)

	for name, value := range map[string]string{
		"hostname":       s.System.Hostname,
		"clock source":   s.System.ClockSource,
		"interface":      s.Network.Interfaces[0].Name,
		"cpu sensor":     s.CPU.Sensors[0].Name,
		"disk sensor":    s.Disks.Devices[0].Sensors[0].Name,
		"fs device":      s.Disks.FileSystems[0].Device,
		"mount point":    s.Disks.FileSystems[0].MountPoint,
		"fs type":        s.Disks.FileSystems[0].FSType,
		"gpu name":       s.GPU[0].Name,
		"gpu driver":     s.GPU[0].Driver,
		"psu name":       s.PSU[0].Name,
		"psu type":       s.PSU[0].Type,
		"psu status":     s.PSU[0].Status,
		"container name": s.Apps.Containers[1].Name,
	} {
		if value != bounded {
			t.Errorf("%s: %d bytes, want it bounded", name, len(value))
		}
	}
	if s.Disks.Devices[0].Name != "sda" || s.Apps.Containers[0].Name != "web" {
		t.Error("a name that fits was changed")
	}
	if containers[1].Name != long || filesystems[0].MountPoint != long {
		t.Error("the subsystem's own slice was changed instead of copied")
	}

	// Nothing too long: nothing is copied.
	fitting := []NetInterface{{Name: "eth0"}}
	s = &Sample{Network: NetworkStats{Interfaces: fitting}}
	boundSampleStrings(s)
	if &s.Network.Interfaces[0] != &fitting[0] {
		t.Error("a slice with nothing to bound was copied")
	}
}
