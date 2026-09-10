package sysinfo

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"kula/internal/collector"
)

// RefreshInterval bounds discovery work across all clients.
const RefreshInterval = 5 * time.Second

type Provider struct {
	mu     sync.Mutex
	proc   string
	sys    string
	cached *Snapshot
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
		"manufacturer": "sys_vendor", "product": "product_name",
		"family": "product_family",
	})
	if arch == "" {
		arch = runtime.GOARCH
	}
	s.System["os"], s.System["kernel"], s.System["architecture"] = osName, kernel, arch
	if hostname == "" {
		hostname, _ = os.Hostname()
	}
	s.System["hostname"] = hostname
	if exists(filepath.Join(p.sys, "firmware/efi")) {
		s.System["firmware"] = "UEFI"
	}
	if hypervisor := read(filepath.Join(p.sys, "hypervisor/type")); hypervisor != "" {
		s.System["hypervisor"] = hypervisor
	}
	s.CPU = p.cpu()
	s.Filesystems = p.filesystems(sample)
	s.Disks = p.disks(s.Filesystems)
	s.Network = p.network()
	s.PCI, s.USB = p.devices()
	s.Sensors = p.sensors()
	s.Power = p.power()
	markTracked(s, sample)
	if sample != nil {
		ts := sample.Timestamp
		s.MetricsTime = &ts
		s.Live = &Live{
			Memory: LiveMemory{
				Total:   sample.Memory.Total,
				Used:    sample.Memory.Used,
				UsedPct: sample.Memory.UsedPercent,
			},
			System: LiveSystem{UptimeHuman: sample.System.UptimeHuman, Processes: sample.Process.Total},
			CPU: LiveCPU{
				UsagePct: sample.CPU.Total.Usage,
				Load1:    sample.LoadAvg.Load1,
				Load5:    sample.LoadAvg.Load5,
				Load15:   sample.LoadAvg.Load15,
			},
			Hottest: warmest(s.Sensors),
			GPU:     make([]LiveGPU, 0, len(sample.GPU)),
		}
		for _, gpu := range sample.GPU {
			s.Live.GPU = append(s.Live.GPU, LiveGPU{Name: gpu.Name, Driver: gpu.Driver})
		}
	}
	p.cached = s
	return s
}

// markTracked links inventory entries to the devices and mounts the regular
// collector actually stores history for. The page uses this to tell an operator
// which hardware is being monitored and which is merely visible.
func markTracked(s *Snapshot, sample *collector.Sample) {
	if sample == nil {
		return
	}
	devices := make(map[string]bool, len(sample.Disks.Devices))
	for _, device := range sample.Disks.Devices {
		devices[device.Name] = true
	}
	for i := range s.Disks {
		s.Disks[i].Tracked = devices[s.Disks[i].Name]
	}
	mounts := make(map[string]bool, len(sample.Disks.FileSystems))
	for _, filesystem := range sample.Disks.FileSystems {
		mounts[filesystem.MountPoint] = true
	}
	for i := range s.Filesystems {
		s.Filesystems[i].Tracked = mounts[s.Filesystems[i].Mount]
	}
	interfaces := make(map[string]bool, len(sample.Network.Interfaces))
	for _, iface := range sample.Network.Interfaces {
		interfaces[iface.Name] = true
	}
	for i := range s.Network {
		s.Network[i].Tracked = interfaces[s.Network[i].Name]
	}
}

// warmest reports the hottest temperature the kernel currently exposes, so the
// overview can answer "is anything running hot" without opening the sensor tab.
func warmest(sensors []Sensor) *LiveSensor {
	var best *LiveSensor
	for _, sensor := range sensors {
		if sensor.Kind != SensorTemperature {
			continue
		}
		if best == nil || sensor.Value > best.Value {
			best = &LiveSensor{Device: sensor.Device, Name: sensor.Name, Value: sensor.Value, Unit: sensor.Unit}
		}
	}
	return best
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
