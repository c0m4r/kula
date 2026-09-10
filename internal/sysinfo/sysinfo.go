package sysinfo

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"kula/internal/collector"
)

// RefreshInterval bounds discovery work across all clients. Idle gaps discard
// rate baselines, so reopening the panel never shows an average over old activity.
const RefreshInterval = 5 * time.Second

type counters struct {
	identity string
	values   []uint64
}

type Provider struct {
	mu       sync.Mutex
	proc     string
	sys      string
	cached   *Snapshot
	previous map[string]counters
	last     time.Time
}

func New() *Provider { return &Provider{proc: "/proc", sys: "/sys"} }

// Current returns an immutable snapshot shared by concurrent requests. Host
// labels are supplied by the server, which reads OS information before Landlock.
func (p *Provider) Current(sample *collector.Sample, osName, kernel, arch, hostname string) *Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if p.cached != nil && now.Sub(p.cached.Timestamp) < RefreshInterval {
		return p.cached
	}
	s := &Snapshot{Timestamp: now}
	s.System = p.attributes(filepath.Join(p.sys, "class/dmi/id"), map[string]string{
		"manufacturer": "sys_vendor", "product": "product_name", "version": "product_version",
		"serial": "product_serial", "uuid": "product_uuid", "family": "product_family",
		"chassis_vendor": "chassis_vendor", "chassis_type": "chassis_type", "chassis_serial": "chassis_serial",
	})
	if arch == "" {
		arch = runtime.GOARCH
	}
	s.System["os"], s.System["kernel"], s.System["architecture"] = osName, kernel, arch
	if hostname == "" {
		hostname, _ = os.Hostname()
	}
	s.System["hostname"] = hostname
	if model := read(filepath.Join(p.sys, "firmware/devicetree/base/model")); model != "" {
		s.System["device_tree_model"] = model
	}
	if exists(filepath.Join(p.sys, "firmware/efi")) {
		s.System["firmware"] = "UEFI"
	}
	if hypervisor := read(filepath.Join(p.sys, "hypervisor/type")); hypervisor != "" {
		s.System["hypervisor"] = hypervisor
	}
	s.Board = p.attributes(filepath.Join(p.sys, "class/dmi/id"), map[string]string{
		"manufacturer": "board_vendor", "model": "board_name", "version": "board_version",
		"serial": "board_serial", "asset_tag": "board_asset_tag",
	})
	s.BIOS = p.attributes(filepath.Join(p.sys, "class/dmi/id"), map[string]string{
		"vendor": "bios_vendor", "version": "bios_version", "date": "bios_date", "release": "bios_release",
	})
	s.CPU = p.cpu()
	s.Memory = keyValues(read(filepath.Join(p.proc, "meminfo")), ":")
	s.DIMMs = p.dimms()
	s.Filesystems = p.filesystems(sample)
	next := make(map[string]counters)
	elapsed := now.Sub(p.last).Seconds()
	if p.last.IsZero() || elapsed > 3*RefreshInterval.Seconds() {
		elapsed = 0
	}
	s.Disks = p.disks(s.Filesystems, next, elapsed)
	s.Network = p.network(next, elapsed)
	s.PCI, s.USB = p.devices()
	s.Sensors = p.sensors()
	s.Power = p.power()
	if sample != nil {
		ts := sample.Timestamp
		s.MetricsTime = &ts
		s.Live = &Live{CPU: sample.CPU, Memory: sample.Memory, Swap: sample.Swap,
			System: sample.System, Load: sample.LoadAvg, Process: sample.Process, GPU: sample.GPU}
	}
	p.previous, p.last, p.cached = next, now, s
	return s
}

// Kernel pseudo-files have no useful Stat size. Bound reads, including proc
// tables, and never execute external tools or read raw device nodes.
func read(path string) string {
	return strings.TrimSpace(strings.Trim(string(readBytes(path)), "\x00"))
}

func readBytes(path string) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, 2<<20))
	if err != nil {
		return nil
	}
	return b
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

func entries(path string) []string {
	dirs, _ := os.ReadDir(path)
	result := make([]string, 0, len(dirs))
	for _, d := range dirs {
		result = append(result, d.Name())
	}
	return result
}

func matches(pattern string) []string { result, _ := filepath.Glob(pattern); return result }

func (p *Provider) attributes(base string, fields map[string]string) Details {
	d := Details{}
	for key, file := range fields {
		if value := read(filepath.Join(base, file)); value != "" {
			switch strings.ToLower(value) {
			case "none", "unknown", "not specified", "to be filled by o.e.m.", "default string":
				continue
			}
			d[key] = value
		}
	}
	return d
}

func keyValues(text, separator string) Details {
	d := Details{}
	for line := range strings.SplitSeq(text, "\n") {
		k, v, ok := strings.Cut(line, separator)
		if ok {
			d[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return d
}

func uintValue(text string) *uint64 {
	n, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

func linkName(path string) string {
	target, err := os.Readlink(path)
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}

func sortedKeys(m map[string]bool) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// rates requires complete, monotonic counters and unchanged device identity.
func (p *Provider) rates(key string, current counters, next map[string]counters, elapsed float64) []float64 {
	next[key] = current
	prev, ok := p.previous[key]
	if !ok || elapsed <= 0 || prev.identity != current.identity || len(prev.values) != len(current.values) {
		return nil
	}
	rates := make([]float64, len(current.values))
	for i, v := range current.values {
		if v < prev.values[i] {
			return nil
		}
		rates[i] = float64(v-prev.values[i]) / elapsed
	}
	return rates
}
