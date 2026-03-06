package collector

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type cpuRaw struct {
	id                                                    string
	user, nice, system, idle, iowait, irq, softirq, steal uint64
	guest, guestNice                                      uint64
}

func parseProcStat() []cpuRaw {
	f, err := os.Open(filepath.Join(procPath, "stat"))
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	var result []cpuRaw
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "cpu") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}
		r := cpuRaw{id: fields[0]}
		r.user = parseUint(fields[1], 10, 64, "cpu.user")
		r.nice = parseUint(fields[2], 10, 64, "cpu.nice")
		r.system = parseUint(fields[3], 10, 64, "cpu.system")
		r.idle = parseUint(fields[4], 10, 64, "cpu.idle")
		if len(fields) > 5 {
			r.iowait = parseUint(fields[5], 10, 64, "cpu.iowait")
		}
		if len(fields) > 6 {
			r.irq = parseUint(fields[6], 10, 64, "cpu.irq")
		}
		if len(fields) > 7 {
			r.softirq = parseUint(fields[7], 10, 64, "cpu.softirq")
		}
		if len(fields) > 8 {
			r.steal = parseUint(fields[8], 10, 64, "cpu.steal")
		}
		if len(fields) > 9 {
			r.guest = parseUint(fields[9], 10, 64, "cpu.guest")
		}
		if len(fields) > 10 {
			r.guestNice = parseUint(fields[10], 10, 64, "cpu.guest_nice")
		}
		result = append(result, r)
	}
	return result
}

func (r cpuRaw) total() uint64 {
	return r.user + r.nice + r.system + r.idle + r.iowait + r.irq + r.softirq + r.steal
}

func calcCorePct(prev, cur cpuRaw) CPUCoreStats {
	totalDelta := float64(cur.total() - prev.total())
	if totalDelta == 0 {
		return CPUCoreStats{}
	}

	pct := func(prevVal, curVal uint64) float64 {
		return round2(float64(curVal-prevVal) / totalDelta * 100.0)
	}

	idlePct := pct(prev.idle, cur.idle)
	cs := CPUCoreStats{
		User:    pct(prev.user, cur.user),
		System:  pct(prev.system, cur.system),
		IOWait:  pct(prev.iowait, cur.iowait),
		IRQ:     pct(prev.irq, cur.irq),
		SoftIRQ: pct(prev.softirq, cur.softirq),
		Steal:   pct(prev.steal, cur.steal),
	}
	cs.Usage = round2(100.0 - idlePct)
	return cs
}

func (c *Collector) collectCPU(_ float64) CPUStats {
	current := parseProcStat()
	if current == nil {
		return CPUStats{}
	}

	result := CPUStats{}
	var numCores int

	if len(c.prevCPU) == len(current) {
		for i, cur := range current {
			if cur.id == "cpu" {
				result.Total = calcCorePct(c.prevCPU[i], cur)
			} else {
				numCores++
			}
		}
	} else {
		// First collection — no delta yet
		for _, cur := range current {
			if cur.id == "cpu" {
				result.Total = CPUCoreStats{}
			} else {
				numCores++
			}
		}
	}

	result.NumCores = numCores
	c.prevCPU = current
	return result
}

func collectLoadAvg() LoadAvg {
	data, err := os.ReadFile(filepath.Join(procPath, "loadavg"))
	if err != nil {
		return LoadAvg{}
	}
	fields := strings.Fields(string(data))
	if len(fields) < 5 {
		return LoadAvg{}
	}
	la := LoadAvg{}
	la.Load1 = parseFloat(fields[0], 64, "loadavg.1")
	la.Load5 = parseFloat(fields[1], 64, "loadavg.5")
	la.Load15 = parseFloat(fields[2], 64, "loadavg.15")

	parts := strings.Split(fields[3], "/")
	if len(parts) == 2 {
		la.Running, _ = strconv.Atoi(parts[0])
		la.Total, _ = strconv.Atoi(parts[1])
	}
	return la
}

func collectMemory() MemoryStats {
	m := parseMemInfo()
	mem := MemoryStats{
		Total:     m["MemTotal"],
		Free:      m["MemFree"],
		Available: m["MemAvailable"],
		Buffers:   m["Buffers"],
		Cached:    m["Cached"],
		Shmem:     m["Shmem"],
	}
	mem.Used = mem.Total - mem.Free - mem.Buffers - mem.Cached
	if mem.Total > 0 {
		mem.UsedPercent = round2(float64(mem.Used) / float64(mem.Total) * 100.0)
	}
	return mem
}

func collectSwap() SwapStats {
	m := parseMemInfo()
	s := SwapStats{
		Total: m["SwapTotal"],
		Free:  m["SwapFree"],
	}
	s.Used = s.Total - s.Free
	if s.Total > 0 {
		s.UsedPercent = round2(float64(s.Used) / float64(s.Total) * 100.0)
	}
	return s
}

func parseMemInfo() map[string]uint64 {
	f, err := os.Open(filepath.Join(procPath, "meminfo"))
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	result := make(map[string]uint64)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		valStr := strings.TrimSpace(parts[1])
		valStr = strings.TrimSuffix(valStr, " kB")
		val, err := strconv.ParseUint(strings.TrimSpace(valStr), 10, 64)
		if err != nil {
			continue
		}
		// Convert kB to bytes
		result[key] = val * 1024
	}
	return result
}

// formatUptime converts seconds to human-readable uptime.
func formatUptime(secs float64) string {
	d := int(secs) / 86400
	h := (int(secs) % 86400) / 3600
	m := (int(secs) % 3600) / 60
	if d > 0 {
		return fmt.Sprintf("%dd %dh %dm", d, h, m)
	}
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}
