package collector

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// powerSupplies returns the power supply readings, taken every
// collection.power_supply_interval and repeated in the samples between (see
// cadence): reading a laptop battery is a round trip to the embedded
// controller, tens of milliseconds.
func (c *Collector) powerSupplies() []PowerSupplyStats {
	dir := filepath.Join(sysPath, "class", "power_supply")
	schedule := newCadenceSchedule(c.collCfg.PowerSupplyInterval, c.collCfg.Interval)
	r := c.psuReads.get(time.Now(), schedule, []cadenceRead[[]PowerSupplyStats]{{
		key:  dir,
		read: func() []PowerSupplyStats { return c.collectPSU(dir) },
	}})[0]
	if !r.ok {
		return nil
	}
	return slices.Clone(r.value) // a copy: the reading repeats in later samples
}

// collectPSU reads the power supplies under baseDir (/sys/class/power_supply)
// and returns stats for each. Returns nil if no power supplies are found
// (e.g., desktops without batteries).
func (c *Collector) collectPSU(baseDir string) []PowerSupplyStats {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return nil
	}

	var stats []PowerSupplyStats
	for _, entry := range entries {
		dir := filepath.Join(baseDir, entry.Name())
		psuType := readStringFile(filepath.Join(dir, "type"))
		if psuType == "" {
			continue
		}

		ps := PowerSupplyStats{
			Name:   entry.Name(),
			Type:   psuType,
			Status: readStringFile(filepath.Join(dir, "status")),
		}

		// Capacity (0-100%)
		if v, ok := readIntFile(filepath.Join(dir, "capacity")); ok && v >= 0 && v <= 100 {
			ps.Capacity = int(v)
		}

		// Voltage (microvolts → volts)
		if v, ok := readIntFile(filepath.Join(dir, "voltage_now")); ok {
			ps.VoltageV = round2(float64(v) / 1_000_000.0)
		}

		// Current (microamps → amps)
		if v, ok := readIntFile(filepath.Join(dir, "current_now")); ok {
			ps.CurrentA = round2(float64(v) / 1_000_000.0)
		}

		// Power (microwatts → watts)
		if v, ok := readIntFile(filepath.Join(dir, "power_now")); ok {
			ps.PowerW = round2(float64(v) / 1_000_000.0)
		}

		// Energy (microwatt-hours → watt-hours)
		if v, ok := readIntFile(filepath.Join(dir, "energy_now")); ok {
			ps.EnergyWhNow = round2(float64(v) / 1_000_000.0)
		} else if v, ok := readIntFile(filepath.Join(dir, "charge_now")); ok {
			// Some batteries report charge (µAh) instead of energy (µWh).
			// Convert to Wh using voltage: Wh = Ah * V
			if ps.VoltageV > 0 {
				ps.EnergyWhNow = round2(float64(v) / 1_000_000.0 * ps.VoltageV)
			}
		}

		if v, ok := readIntFile(filepath.Join(dir, "energy_full")); ok {
			ps.EnergyWhFull = round2(float64(v) / 1_000_000.0)
		} else if v, ok := readIntFile(filepath.Join(dir, "charge_full")); ok {
			if ps.VoltageV > 0 {
				ps.EnergyWhFull = round2(float64(v) / 1_000_000.0 * ps.VoltageV)
			}
		}

		c.debugf(" psu: found %s type=%s status=%s capacity=%d%%", ps.Name, ps.Type, ps.Status, ps.Capacity)
		stats = append(stats, ps)
	}
	return stats
}

// readStringFile reads a sysfs file and returns its trimmed content.
// Reads at most 256 bytes to prevent unbounded allocation from malformed sysfs.
func readStringFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 256)
	n, _ := f.Read(buf)
	return strings.TrimSpace(string(buf[:n]))
}

// readIntFile reads a sysfs file containing an integer value.
func readIntFile(path string) (int64, bool) {
	s := readStringFile(path)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
