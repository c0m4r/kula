package sysinfo

import (
	"math"
	"path/filepath"
	"strconv"
	"strings"
)

func (p *Provider) cpu() CPU {
	c := CPU{Details: Details{}, Vulnerabilities: Details{}, Caches: []Details{}, Frequency: []Details{}}
	models, vendors, features := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for block := range strings.SplitSeq(read(filepath.Join(p.proc, "cpuinfo")), "\n\n") {
		d := keyValues(block, ":")
		for _, key := range []string{"model name", "Processor", "cpu model", "uarch"} {
			if d[key] != "" {
				models[d[key]] = true
				break
			}
		}
		for _, key := range []string{"vendor_id", "CPU implementer", "vendor"} {
			if d[key] != "" {
				vendors[d[key]] = true
			}
		}
		for _, key := range []string{"flags", "Features", "isa"} {
			for _, f := range strings.Fields(d[key]) {
				features[f] = true
			}
		}
		if _, ok := d["processor"]; ok {
			c.LogicalCPUs++
		}
		for _, key := range []string{"microcode", "cpu family", "model", "stepping", "CPU architecture", "CPU part", "CPU revision"} {
			if d[key] != "" && c.Details[strings.ReplaceAll(key, " ", "_")] == "" {
				c.Details[strings.ReplaceAll(key, " ", "_")] = d[key]
			}
		}
	}
	c.Details["model_name"], c.Details["vendor"], c.Details["features"] = sortedKeys(models), sortedKeys(vendors), sortedKeys(features)
	base := filepath.Join(p.sys, "devices/system/cpu")
	for _, key := range []string{"online", "offline", "possible", "present"} {
		if value := read(filepath.Join(base, key)); value != "" {
			c.Details[key] = value
		}
	}
	cores, sockets, caches := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, path := range matches(filepath.Join(base, "cpu[0-9]*")) {
		if read(filepath.Join(path, "online")) == "0" {
			continue
		}
		socket, core := read(filepath.Join(path, "topology/physical_package_id")), read(filepath.Join(path, "topology/core_id"))
		if n, err := strconv.Atoi(socket); err == nil && n >= 0 {
			sockets[socket] = true
			if n, err := strconv.Atoi(core); err == nil && n >= 0 {
				cores[socket+":"+core] = true
			}
		}
		for _, cache := range matches(filepath.Join(path, "cache/index*")) {
			d := p.attributes(cache, map[string]string{"level": "level", "type": "type", "size": "size",
				"shared_cpus": "shared_cpu_list", "line_size": "coherency_line_size", "ways": "ways_of_associativity"})
			key := d["level"] + ":" + d["type"] + ":" + d["shared_cpus"]
			if len(d) > 0 && !caches[key] {
				c.Caches = append(c.Caches, d)
				caches[key] = true
			}
		}
	}
	c.Cores, c.Sockets = len(cores), len(sockets)
	if nodes := read(filepath.Join(p.sys, "devices/system/node/online")); nodes != "" {
		c.Details["numa_nodes"] = nodes
	}
	for _, policy := range matches(filepath.Join(base, "cpufreq/policy*")) {
		d := p.attributes(policy, map[string]string{"cpus": "related_cpus", "driver": "scaling_driver", "governor": "scaling_governor",
			"current_khz": "scaling_cur_freq", "minimum_khz": "cpuinfo_min_freq", "maximum_khz": "cpuinfo_max_freq"})
		if len(d) > 0 {
			c.Frequency = append(c.Frequency, d)
		}
	}
	for _, name := range entries(filepath.Join(base, "vulnerabilities")) {
		if value := read(filepath.Join(base, "vulnerabilities", name)); value != "" {
			c.Vulnerabilities[name] = value
		}
	}
	return c
}

func (p *Provider) dimms() []Details {
	result := memoryDevices(readBytes(filepath.Join(p.sys, "firmware/dmi/tables/DMI")))
	for _, path := range matches(filepath.Join(p.sys, "devices/system/edac/mc/mc*/[dr]*[0-9]")) {
		d := p.attributes(path, map[string]string{"label": "dimm_label", "location": "dimm_location", "size_mib": "size",
			"type": "dimm_mem_type", "device_type": "dimm_dev_type", "ecc": "dimm_edac_mode",
			"corrected_errors": "dimm_ce_count", "uncorrected_errors": "dimm_ue_count"})
		if len(d) > 0 {
			d["slot"] = filepath.Base(filepath.Dir(path)) + "/" + filepath.Base(path)
			d["source"] = "EDAC"
			result = append(result, d)
		}
	}
	return result
}

func (p *Provider) devices() ([]Details, []Details) {
	pci, usb := []Details{}, []Details{}
	for _, path := range matches(filepath.Join(p.sys, "bus/pci/devices/*")) {
		d := p.attributes(path, map[string]string{"vendor_id": "vendor", "device_id": "device", "class": "class",
			"subsystem_vendor": "subsystem_vendor", "subsystem_device": "subsystem_device", "revision": "revision",
			"numa_node": "numa_node", "link_speed": "current_link_speed", "link_width": "current_link_width"})
		d["address"], d["driver"], d["iommu_group"] = filepath.Base(path), linkName(filepath.Join(path, "driver")), linkName(filepath.Join(path, "iommu_group"))
		pci = append(pci, d)
	}
	for _, path := range matches(filepath.Join(p.sys, "bus/usb/devices/*")) {
		if !exists(filepath.Join(path, "idVendor")) {
			continue
		}
		d := p.attributes(path, map[string]string{"vendor_id": "idVendor", "device_id": "idProduct", "manufacturer": "manufacturer",
			"product": "product", "serial": "serial", "speed_mbps": "speed", "usb_version": "version", "max_power": "bMaxPower"})
		d["address"] = filepath.Base(path)
		usb = append(usb, d)
	}
	return pci, usb
}

func (p *Provider) sensors() []Sensor {
	result := []Sensor{}
	for _, path := range matches(filepath.Join(p.sys, "class/hwmon/hwmon*")) {
		device := read(filepath.Join(path, "name"))
		if device == "" {
			device = filepath.Base(path)
		}
		for _, kind := range []struct {
			prefix, unit string
			scale        float64
		}{
			{"temp", "°C", 1000}, {"fan", "RPM", 1}, {"in", "V", 1000}, {"curr", "A", 1000}, {"power", "W", 1e6}, {"humidity", "%", 1000},
		} {
			for _, file := range matches(filepath.Join(path, kind.prefix+"[0-9]*_input")) {
				value, err := strconv.ParseFloat(read(file), 64)
				if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
					continue
				}
				stem := strings.TrimSuffix(file, "_input")
				if read(stem+"_fault") == "1" {
					continue
				}
				name := read(stem + "_label")
				if name == "" {
					name = filepath.Base(stem)
				}
				result = append(result, Sensor{Device: device, Name: name, Value: value / kind.scale, Unit: kind.unit})
			}
		}
	}
	for _, path := range matches(filepath.Join(p.sys, "class/thermal/thermal_zone*")) {
		value, err := strconv.ParseFloat(read(filepath.Join(path, "temp")), 64)
		if err == nil && !math.IsNaN(value) && !math.IsInf(value, 0) {
			result = append(result, Sensor{Device: filepath.Base(path), Name: read(filepath.Join(path, "type")), Value: value / 1000, Unit: "°C"})
		}
	}
	return result
}

func (p *Provider) power() []Details {
	result := []Details{}
	for _, path := range matches(filepath.Join(p.sys, "class/power_supply/*")) {
		d := p.attributes(path, map[string]string{"type": "type", "manufacturer": "manufacturer", "model": "model_name",
			"serial": "serial_number", "status": "status", "health": "health", "technology": "technology",
			"capacity_percent": "capacity", "cycle_count": "cycle_count", "online": "online",
			"energy_now_uwh": "energy_now", "energy_full_uwh": "energy_full", "energy_design_uwh": "energy_full_design",
			"charge_now_uah": "charge_now", "charge_full_uah": "charge_full", "voltage_uv": "voltage_now", "power_uw": "power_now"})
		d["name"] = filepath.Base(path)
		result = append(result, d)
	}
	return result
}
