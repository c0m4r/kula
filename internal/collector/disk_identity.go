package collector

import (
	"log"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// ListDisks returns the names and persistent IDs of all available disks and
// partitions supported by the collector, sorted by kernel name. It does not
// load configuration or collect metrics, and uses the same duplicate-identity
// checks as monitoring. An empty ID denotes unavailable or ambiguous identity.
func ListDisks() ([]DiskDevice, error) {
	c := &Collector{}
	raw, err := c.readDiskStats(true)
	if err != nil {
		return nil, err
	}
	c.identifyDisks(raw)
	disks := make([]DiskDevice, 0, len(raw))
	for name, dev := range raw {
		disks = append(disks, DiskDevice{Name: name, ID: dev.id})
	}
	sort.Slice(disks, func(i, j int) bool { return disks[i].Name < disks[j].Name })
	return disks, nil
}

// SeriesKey keeps unidentified disks in a separate namespace from persistent IDs.
func (d DiskDevice) SeriesKey() string {
	if d.ID != "" {
		return d.ID
	}
	return "kernel:" + d.Name
}

// IdentitySource is derived from the stored, scheme-tagged identity.
func (d DiskDevice) IdentitySource() string {
	if d.ID == "" {
		return "kernel"
	}
	source, _, _ := strings.Cut(d.ID, ":")
	return source
}

func (c *Collector) warnDiskOnce(key, format string, args ...any) {
	if c.diskWarnings == nil {
		c.diskWarnings = make(map[string]bool)
	}
	if !c.diskWarnings[key] {
		c.diskWarnings[key] = true
		log.Printf("Warning: "+format, args...)
	}
}

// Disk identities are refreshed each tick, including unselected devices, so a
// hotplug/replacement or duplicate identifier cannot leave a stale name mapping.
func (c *Collector) identifyDisks(disks map[string]diskRaw) {
	type identity struct{ id, seq string }
	parents := make(map[string]identity)
	paths := make(map[string]string, len(disks))
	parts := make(map[string]string)
	counts := make(map[string]int)
	for name := range disks {
		base := filepath.Join(sysPath, "class", "block", name)
		if part := readSysfsFile(filepath.Join(base, "partition")); part != "" {
			// A partition's real sysfs directory is nested beneath its disk.
			resolved, err := filepath.EvalSymlinks(base)
			if n, parseErr := strconv.ParseUint(part, 10, 32); err == nil && parseErr == nil && n > 0 {
				base = filepath.Dir(resolved)
				parts[name] = strconv.FormatUint(n, 10)
			} else {
				continue
			}
		} else if isPartition(name) {
			// Missing partition metadata must never turn into a parent disk ID.
			continue
		} else if resolved, err := filepath.EvalSymlinks(base); err == nil {
			base = resolved
		}
		paths[name] = base
		if _, ok := parents[base]; !ok {
			id := resolveDiskID(base)
			parents[base] = identity{id, readSysfsFile(filepath.Join(base, "diskseq"))}
			if id != "" {
				counts[id]++
			}
		}
	}
	for name, raw := range disks {
		info := parents[paths[name]]
		raw.diskseq = info.seq
		if info.id != "" && counts[info.id] == 1 {
			raw.id = info.id
			if part := parts[name]; part != "" {
				raw.id += ":part:" + part
			}
		} else if info.id != "" {
			c.warnDiskOnce("duplicate:"+info.id, "duplicate disk identity %q; affected disks use unstable kernel names", info.id)
		}
		disks[name] = raw
	}
}

func diskAttribute(base, attribute string) string {
	value := readSysfsFile(filepath.Join(base, attribute))
	if len(value) > 1024 || strings.IndexFunc(value, func(r rune) bool { return unicode.IsControl(r) && !unicode.IsSpace(r) }) >= 0 {
		return ""
	}
	value = strings.Join(strings.Fields(value), " ")
	switch strings.ToLower(value) {
	case "", "unknown", "none", "null", "n/a":
		return ""
	}
	if strings.Trim(value, "0 -") == "" {
		return ""
	}
	return value
}

func diskHexID(value string, lengths ...int) string {
	value = strings.ToLower(strings.Join(strings.Fields(value), ""))
	value = strings.ReplaceAll(value, "-", "")
	validLength := false
	for _, n := range lengths {
		validLength = validLength || len(value) == n
	}
	if !validLength || strings.Trim(value, "0") == "" {
		return ""
	}
	for _, ch := range value {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return ""
		}
	}
	return value
}

func diskWWID(value string) string {
	if value == "" {
		return ""
	}
	// Canonicalize hexadecimal identifiers across the wwid and raw attributes.
	for _, scheme := range []string{"uuid.", "eui.", "naa."} {
		if strings.HasPrefix(strings.ToLower(value), scheme) {
			hex := diskHexID(value[len(scheme):], 16, 32)
			if hex == "" {
				return ""
			}
			return "wwid:" + scheme + hex
		}
	}
	return "wwid:" + diskIDComponent(value)
}

// Escape structural delimiters too: a literal serial "A:part:1" must not
// collide with partition 1 on a disk whose serial is "A".
func diskIDComponent(value string) string {
	return strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
}

func resolveDiskID(base string) string {
	if id := diskWWID(diskAttribute(base, "wwid")); id != "" {
		return id
	}
	for _, field := range []struct {
		name, prefix string
		size         int
	}{{"uuid", "uuid.", 32}, {"nguid", "eui.", 32}, {"eui", "eui.", 16}} {
		if id := diskHexID(diskAttribute(base, field.name), field.size); id != "" {
			return "wwid:" + field.prefix + id
		}
	}
	if id := diskWWID(diskAttribute(base, "device/wwid")); id != "" {
		return id
	}
	serial := diskAttribute(base, "device/serial")
	if serial == "" {
		serial = diskAttribute(base, "serial") // virtio-blk
	}
	if serial == "" {
		return ""
	}
	id := "serial:" + diskIDComponent(diskAttribute(base, "device/vendor")) + "|" +
		diskIDComponent(diskAttribute(base, "device/model")) + "|" + diskIDComponent(serial)
	if strings.HasPrefix(filepath.Base(base), "nvme") {
		// Several namespaces share a controller's serial and model.
		nsid, err := strconv.ParseUint(readSysfsFile(filepath.Join(base, "nsid")), 10, 32)
		if err != nil || nsid == 0 {
			return ""
		}
		id += ":ns:" + strconv.FormatUint(nsid, 10)
	}
	return id
}
