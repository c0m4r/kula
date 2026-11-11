package collector

import (
	"fmt"
	"hash/fnv"
	"slices"
	"unicode/utf8"
)

// MaxStoredString is the longest string the storage codec keeps: names and
// labels are written with a one-byte length prefix.
const MaxStoredString = 255

// boundSuffixLen is the length of what BoundString appends to a cut string:
// an ellipsis (3 bytes in UTF-8) and eight hex digits of a hash.
const boundSuffixLen = 3 + 8

// BoundString returns s unchanged when it fits in MaxStoredString bytes.
// A longer string is cut on a rune boundary and ends in "…" and eight hex
// digits of a hash of the whole string, so two long names that share a prefix
// stay distinct: interface, mount point and container names are identities
// that history matches samples by.
//
// One over-long name used to fail the whole record, so a long mount path or
// a container name chosen by whoever runs it stopped all history recording.
func BoundString(s string) string {
	if len(s) <= MaxStoredString {
		return s
	}
	cut := MaxStoredString - boundSuffixLen
	// Back up to the start of a rune, at most a rune's length: a run of
	// continuation bytes is not UTF-8 anyway, and is cut where it stands.
	for back := 0; back < utf8.UTFMax-1 && cut > 0 && !utf8.RuneStart(s[cut]); back++ {
		cut--
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%s…%08x", s[:cut], h.Sum32())
}

// boundSampleStrings bounds every name and label a sample carries to what
// storage keeps (BoundString), before the sample reaches storage, the live
// dashboard, /metrics or the TUI, so all of them show the same name.
//
// Subsystems refresh at their own intervals and hand the same slice to several
// samples, which other goroutines may be reading, so a slice is copied before
// anything in it changes. In the usual case nothing is too long and nothing is
// copied.
func boundSampleStrings(s *Sample) {
	s.System.Hostname = BoundString(s.System.Hostname)
	s.System.ClockSource = BoundString(s.System.ClockSource)

	s.Network.Interfaces = boundEach(s.Network.Interfaces, func(i *NetInterface) []*string {
		return []*string{&i.Name}
	})
	s.CPU.Sensors = boundEach(s.CPU.Sensors, func(t *CPUTempSensor) []*string {
		return []*string{&t.Name}
	})
	s.Disks.FileSystems = boundEach(s.Disks.FileSystems, func(fs *FileSystemInfo) []*string {
		return []*string{&fs.Device, &fs.MountPoint, &fs.FSType}
	})
	if slices.ContainsFunc(s.Disks.Devices, deviceNeedsBound) {
		devices := slices.Clone(s.Disks.Devices)
		for i := range devices {
			devices[i].Name = BoundString(devices[i].Name)
			devices[i].Sensors = boundEach(devices[i].Sensors, func(t *DiskTempSensor) []*string {
				return []*string{&t.Name}
			})
		}
		s.Disks.Devices = devices
	}
	s.GPU = boundEach(s.GPU, func(g *GPUStats) []*string {
		return []*string{&g.Name, &g.Driver}
	})
	s.PSU = boundEach(s.PSU, func(p *PowerSupplyStats) []*string {
		return []*string{&p.Name, &p.Type, &p.Status}
	})
	s.Apps.Containers = boundEach(s.Apps.Containers, func(c *ContainerStats) []*string {
		return []*string{&c.ID, &c.Name}
	})
}

// boundEach returns items with every string that fields names bounded,
// copying the slice first when one of them is too long.
func boundEach[T any](items []T, fields func(*T) []*string) []T {
	long := slices.IndexFunc(items, func(item T) bool {
		for _, field := range fields(&item) {
			if len(*field) > MaxStoredString {
				return true
			}
		}
		return false
	})
	if long < 0 {
		return items
	}
	items = slices.Clone(items)
	for i := long; i < len(items); i++ {
		for _, field := range fields(&items[i]) {
			*field = BoundString(*field)
		}
	}
	return items
}

func deviceNeedsBound(d DiskDevice) bool {
	if len(d.Name) > MaxStoredString {
		return true
	}
	return slices.ContainsFunc(d.Sensors, func(t DiskTempSensor) bool { return len(t.Name) > MaxStoredString })
}
