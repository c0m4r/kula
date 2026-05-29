package collector

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type cpuRaw struct {
	id                                                    string
	user, nice, system, idle, iowait, irq, softirq, steal uint64
	guest, guestNice                                      uint64
}

type sysSensor struct {
	Name string
	Path string
}

var (
	// Cached sensors for CPU temperature so we don't scan on every tick.
	// Nil means not yet initialized. Empty means initialized but not found.
	sysTempSensors []sysSensor
)

func (c *Collector) parseProcStat() []cpuRaw {
	f, err := os.Open(filepath.Join(procPath, "stat"))
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	// Preallocate to the previously seen core count (stable across ticks) so the
	// per-second hot path performs no slice growth.
	capHint := len(c.prevCPU)
	if capHint == 0 {
		capHint = runtime.NumCPU() + 1
	}
	result := make([]cpuRaw, 0, capHint)

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Bytes()
		// Only the aggregate "cpu" line and per-core "cpuN" lines are relevant.
		// Skip everything else (notably the large intr/softirq lines, which can
		// be several KB) *before* touching the bytes, so they are never copied.
		if len(line) < 3 || line[0] != 'c' || line[1] != 'p' || line[2] != 'u' {
			continue
		}

		var r cpuRaw
		idx := 0
		pos := 0
		for {
			// Skip the spaces between fields ("cpu" has two leading spaces).
			for pos < len(line) && line[pos] == ' ' {
				pos++
			}
			if pos >= len(line) {
				break
			}
			start := pos
			for pos < len(line) && line[pos] != ' ' {
				pos++
			}
			field := line[start:pos]
			switch idx {
			case 0:
				r.id = string(field) // small (e.g. "cpu", "cpu0"); needed by callers
			case 1:
				r.user = parseUintBytes(field)
			case 2:
				r.nice = parseUintBytes(field)
			case 3:
				r.system = parseUintBytes(field)
			case 4:
				r.idle = parseUintBytes(field)
			case 5:
				r.iowait = parseUintBytes(field)
			case 6:
				r.irq = parseUintBytes(field)
			case 7:
				r.softirq = parseUintBytes(field)
			case 8:
				r.steal = parseUintBytes(field)
			case 9:
				r.guest = parseUintBytes(field)
			case 10:
				r.guestNice = parseUintBytes(field)
			}
			idx++
			if idx > 10 {
				break
			}
		}
		// Require at least id + user..softirq (8 fields), matching prior behaviour.
		if idx < 8 {
			continue
		}
		result = append(result, r)
	}
	return result
}

func (r cpuRaw) total() uint64 {
	return r.user + r.nice + r.system + r.idle + r.iowait + r.irq + r.softirq + r.steal
}

func calcCorePct(prev, cur cpuRaw) CPUCoreStats {
	curTotal := cur.total()
	prevTotal := prev.total()
	if curTotal <= prevTotal {
		return CPUCoreStats{} // counter reset or no progress
	}
	totalDelta := float64(curTotal - prevTotal)

	pct := func(prevVal, curVal uint64) float64 {
		if curVal < prevVal {
			return 0
		}
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
	current := c.parseProcStat()
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

func (c *Collector) collectLoadAvg() LoadAvg {
	data, err := os.ReadFile(filepath.Join(procPath, "loadavg"))
	if err != nil {
		return LoadAvg{}
	}
	fields := strings.Fields(string(data))
	if len(fields) < 5 {
		return LoadAvg{}
	}
	la := LoadAvg{}
	la.Load1 = c.parseFloat(fields[0], 64, "loadavg.1")
	la.Load5 = c.parseFloat(fields[1], 64, "loadavg.5")
	la.Load15 = c.parseFloat(fields[2], 64, "loadavg.15")

	parts := strings.Split(fields[3], "/")
	if len(parts) == 2 {
		la.Running = int(c.parseInt(parts[0], 10, 32, "loadavg.running"))
		la.Total = int(c.parseInt(parts[1], 10, 32, "loadavg.total"))
	}
	return la
}

// collectCPUTemperature reads the CPU temperature from sysfs.
func (c *Collector) collectCPUTemperature() (float64, []CPUTempSensor) {
	if sysTempSensors == nil {
		sysTempSensors = discoverCPUTempPath()
	}

	if len(sysTempSensors) == 0 {
		return 0, nil // No temperature sensors found
	}

	var primaryTemp float64
	var sensors []CPUTempSensor

	for _, sensor := range sysTempSensors {
		data, err := os.ReadFile(sensor.Path)
		if err != nil {
			continue
		}

		valStr := strings.TrimSpace(string(data))
		// Use parseInt without fieldName to avoid double logging (caller handles it below with path)
		tempMilliC := c.parseInt(valStr, 10, 64, "")
		if tempMilliC == 0 && valStr != "0" {
			c.debugf(" cpu.temp: failed to parse %q (%q)", sensor.Path, valStr)
			continue
		}

		tempC := round2(float64(tempMilliC) / 1000.0)

		sensors = append(sensors, CPUTempSensor{
			Name:  sensor.Name,
			Value: tempC,
		})
	}

	// Filter out synthetic Tctl (thermal pressure) if we have physical sensors like Tccd or Tdie
	var hasPhysicalAMD bool
	for _, s := range sensors {
		if strings.HasPrefix(s.Name, "Tccd") || s.Name == "Tdie" {
			hasPhysicalAMD = true
			break
		}
	}

	if hasPhysicalAMD {
		var filtered []CPUTempSensor
		for _, s := range sensors {
			if s.Name != "Tctl" {
				filtered = append(filtered, s)
			}
		}
		sensors = filtered
	}

	// Make the first sensor (often Tctl or temp1_input on others) the primary temperature
	if primaryTemp == 0 && len(sensors) > 0 {
		// Prefer a package/control/die temperature for primary if multiple exist, otherwise just use the first
		for _, s := range sensors {
			sNameLow := strings.ToLower(s.Name)
			if s.Name == "Tctl" || s.Name == "Tdie" || strings.Contains(sNameLow, "package") {
				primaryTemp = s.Value
				break
			}
		}
		if primaryTemp == 0 {
			primaryTemp = sensors[0].Value
		}
	}

	return primaryTemp, sensors
}

// discoverCPUTempPath attempts to find files containing CPU temperatures.
func discoverCPUTempPath() []sysSensor {
	var sensors []sysSensor

	// 1. Try hwmon (usually more reliable on x86, e.g. coretemp, k10temp, zenpower)
	hwmonPath := filepath.Join(sysPath, "class", "hwmon")
	entries, err := os.ReadDir(hwmonPath)
	if err == nil {
		for _, entry := range entries {
			if !entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
				continue
			}

			dir := filepath.Join(hwmonPath, entry.Name())

			// Some systems nest hwmon under device module
			nameFile := filepath.Join(dir, "name")
			nameData, err := os.ReadFile(nameFile)
			if err != nil {
				continue
			}
			name := strings.TrimSpace(string(nameData))

			// Common CPU temperature drivers
			if name == "coretemp" || name == "k10temp" || name == "zenpower" || name == "cpu_thermal" {
				// We can just scan for all temp*_input
				inputs, _ := filepath.Glob(filepath.Join(dir, "temp*_input"))
				for _, input := range inputs {
					sensorName := name
					prefix := strings.TrimSuffix(filepath.Base(input), "_input")

					// Look for corresponding label to get names like Tctl, Tccd1
					labelFile := filepath.Join(dir, prefix+"_label")
					if labelData, err := os.ReadFile(labelFile); err == nil {
						lbl := strings.TrimSpace(string(labelData))
						if lbl != "" {
							sensorName = lbl
						}
					} else {
						// If no label, use Name + prefix (e.g. coretemp_temp1)
						sensorName = fmt.Sprintf("%s_%s", name, prefix)
					}

					sensors = append(sensors, sysSensor{
						Name: sensorName,
						Path: input,
					})
				}
				if len(sensors) > 0 {
					return sensors // Found our sensors
				}
			}
		}
	}

	// 2. Try thermal_zone (Common on ARM/Raspberry Pi)
	thermalPath := filepath.Join(sysPath, "class", "thermal")
	entries, err = os.ReadDir(thermalPath)
	if err == nil {
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), "thermal_zone") {
				continue
			}

			dir := filepath.Join(thermalPath, entry.Name())
			typeFile := filepath.Join(dir, "type")
			typeData, err := os.ReadFile(typeFile)
			if err != nil {
				continue
			}

			typ := strings.TrimSpace(string(typeData))
			// Usually named something like "cpu-thermal", "cpu_thermal", "x86_pkg_temp"
			if strings.Contains(strings.ToLower(typ), "cpu") || strings.Contains(strings.ToLower(typ), "pkg_temp") {
				tempFile := filepath.Join(dir, "temp")
				if _, err := os.Stat(tempFile); err == nil {
					sensors = append(sensors, sysSensor{
						Name: typ,
						Path: tempFile,
					})
				}
			}
		}

		if len(sensors) > 0 {
			return sensors
		}

		// Fallback: If no explicit 'cpu' type is found, thermal_zone0 is often the main CPU temp
		temp0 := filepath.Join(thermalPath, "thermal_zone0", "temp")
		if _, err := os.Stat(temp0); err == nil {
			sensors = append(sensors, sysSensor{
				Name: "thermal_zone0",
				Path: temp0,
			})
			return sensors
		}
	}

	// Ensure we return an initialized slice so we don't try to detect over and over if none found
	return make([]sysSensor, 0)
}

// DetectTjMax returns the maximum critical temperature in Celsius, or 0 if undetected.
func (c *Collector) DetectTjMax() float64 {
	if sysTempSensors == nil {
		sysTempSensors = discoverCPUTempPath()
	}

	var maxCrit float64
	for _, sensor := range sysTempSensors {
		critPath := strings.TrimSuffix(sensor.Path, "_input") + "_crit"
		data, err := os.ReadFile(critPath)
		if err == nil {
			valStr := strings.TrimSpace(string(data))
			tempMilliC := c.parseInt(valStr, 10, 64, "cpu.temp_crit")
			if tempMilliC > 0 {
				val := float64(tempMilliC) / 1000.0
				if val > maxCrit {
					maxCrit = val
				}
			} else if tempMilliC < 0 && valStr != "0" {
				c.debugf(" cpu.temp_crit: ignoring negative value %d at %q", tempMilliC, critPath)
			}
		}
	}
	return maxCrit
}

// memInfo holds only the /proc/meminfo fields Kula reports, avoiding a map.
type memInfo struct {
	memTotal, memFree, memAvailable, buffers, cached, shmem uint64
	swapTotal, swapFree                                     uint64
}

// collectMemSwap reads /proc/meminfo once and derives both the memory and swap
// stats from it. The collection loop calls this directly so the file is parsed a
// single time per tick (the prior code called collectMemory and collectSwap
// separately, parsing /proc/meminfo — and allocating a 50+ key map — twice).
func collectMemSwap() (MemoryStats, SwapStats) {
	mi := parseMemInfo()

	mem := MemoryStats{
		Total:     mi.memTotal,
		Free:      mi.memFree,
		Available: mi.memAvailable,
		Buffers:   mi.buffers,
		Cached:    mi.cached,
		Shmem:     mi.shmem,
	}
	mem.Used = mem.Total - mem.Free - mem.Buffers - mem.Cached
	if mem.Total > 0 {
		mem.UsedPercent = round2(float64(mem.Used) / float64(mem.Total) * 100.0)
	}

	swap := SwapStats{
		Total: mi.swapTotal,
		Free:  mi.swapFree,
	}
	swap.Used = swap.Total - swap.Free
	if swap.Total > 0 {
		swap.UsedPercent = round2(float64(swap.Used) / float64(swap.Total) * 100.0)
	}

	return mem, swap
}

// collectMemory and collectSwap are retained for callers (and tests) that need
// only one half; both defer to collectMemSwap.
func collectMemory() MemoryStats {
	mem, _ := collectMemSwap()
	return mem
}

func collectSwap() SwapStats {
	_, swap := collectMemSwap()
	return swap
}

// parseMemInfo reads /proc/meminfo and extracts only the fields Kula needs,
// parsing directly from the file bytes — no scanner, no per-line strings, no map.
func parseMemInfo() memInfo {
	var mi memInfo
	data, err := os.ReadFile(filepath.Join(procPath, "meminfo"))
	if err != nil {
		return mi
	}

	for len(data) > 0 {
		line := data
		if nl := bytes.IndexByte(data, '\n'); nl >= 0 {
			line = data[:nl]
			data = data[nl+1:]
		} else {
			data = nil
		}

		colon := bytes.IndexByte(line, ':')
		if colon < 0 {
			continue
		}

		var dst *uint64
		switch string(line[:colon]) { // string(b) in a switch does not allocate
		case "MemTotal":
			dst = &mi.memTotal
		case "MemFree":
			dst = &mi.memFree
		case "MemAvailable":
			dst = &mi.memAvailable
		case "Buffers":
			dst = &mi.buffers
		case "Cached":
			dst = &mi.cached
		case "Shmem":
			dst = &mi.shmem
		case "SwapTotal":
			dst = &mi.swapTotal
		case "SwapFree":
			dst = &mi.swapFree
		default:
			continue
		}

		// Value layout: optional spaces, decimal digits, then " kB". Stored in bytes.
		rest := line[colon+1:]
		i := 0
		for i < len(rest) && rest[i] == ' ' {
			i++
		}
		start := i
		for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
			i++
		}
		*dst = parseUintBytes(rest[start:i]) * 1024
	}

	return mi
}

// FormatUptime converts seconds to human-readable uptime.
func FormatUptime(secs float64) string {
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
