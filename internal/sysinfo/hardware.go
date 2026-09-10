package sysinfo

import (
	"math"
	"path/filepath"
	"strconv"
	"strings"
)

func (p *Provider) cpu() CPU {
	c := CPU{}
	for block := range strings.SplitSeq(read(filepath.Join(p.proc, "cpuinfo")), "\n\n") {
		d := keyValues(block, ":")
		if c.ModelName == "" {
			for _, key := range []string{"model name", "Processor", "cpu model", "uarch"} {
				if d[key] != "" {
					c.ModelName = d[key]
					break
				}
			}
		}
		if _, ok := d["processor"]; ok {
			c.LogicalCPUs++
		}
	}
	base := filepath.Join(p.sys, "devices/system/cpu")
	cores := map[string]bool{}
	for _, path := range matches(filepath.Join(base, "cpu[0-9]*")) {
		if read(filepath.Join(path, "online")) == "0" {
			continue
		}
		socket, core := read(filepath.Join(path, "topology/physical_package_id")), read(filepath.Join(path, "topology/core_id"))
		if n, err := strconv.Atoi(socket); err == nil && n >= 0 {
			if n, err := strconv.Atoi(core); err == nil && n >= 0 {
				cores[socket+":"+core] = true
			}
		}
	}
	c.Cores = len(cores)
	return c
}

func (p *Provider) devices() ([]Details, []Details) {
	pci, usb := []Details{}, []Details{}
	for _, path := range matches(filepath.Join(p.sys, "bus/pci/devices/*")) {
		d := Details{"address": filepath.Base(path)}
		if driver := linkName(filepath.Join(path, "driver")); driver != "" {
			d["driver"] = driver
		}
		pciIdentity(d, path)
		pci = append(pci, d)
	}
	for _, path := range matches(filepath.Join(p.sys, "bus/usb/devices/*")) {
		if !exists(filepath.Join(path, "idVendor")) {
			continue
		}
		d := p.attributes(path, map[string]string{"manufacturer": "manufacturer", "product": "product",
			"speed_mbps": "speed", "max_power": "bMaxPower"})
		d["address"] = filepath.Base(path)
		usbIdentity(d, path)
		usb = append(usb, d)
	}
	return pci, usb
}

// pciIdentity adds the vendor, class and raw identifier pair. Without these a
// PCI function is just an opaque bus address to everyone but the kernel.
func pciIdentity(d Details, path string) {
	vendor, vendorOK := hexID(read(filepath.Join(path, "vendor")))
	device, deviceOK := hexID(read(filepath.Join(path, "device")))
	if name := pciVendors[vendor]; vendorOK && name != "" {
		d["vendor"] = name
	}
	if id := hexPair(vendor, device, vendorOK && deviceOK); id != "" {
		d["device_id"] = id
	}
	if class, ok := hexID(read(filepath.Join(path, "class"))); ok {
		// The base class is the top byte of the 24-bit class/subclass/prog-if word.
		d["class"] = pciClasses[(class>>16)&0xff]
	}
}

// usbIdentity adds the product ID and the interface class. bDeviceClass is zero
// for most peripherals because they declare their class on the interface, so the
// first interface is consulted as well.
func usbIdentity(d Details, path string) {
	vendor, vendorOK := hexID(read(filepath.Join(path, "idVendor")))
	product, productOK := hexID(read(filepath.Join(path, "idProduct")))
	if name := usbVendors[vendor]; vendorOK && name != "" && d["manufacturer"] == "" {
		d["manufacturer"] = name
	}
	if id := hexPair(vendor, product, vendorOK && productOK); id != "" {
		d["device_id"] = id
	}
	class, ok := hexID(read(filepath.Join(path, "bDeviceClass")))
	if !ok || class == 0 {
		for _, iface := range matches(filepath.Join(path, "*:*")) {
			if candidate, found := hexID(read(filepath.Join(iface, "bInterfaceClass"))); found {
				class, ok = candidate, true
				break
			}
		}
	}
	if ok {
		if slug := usbClasses[class]; slug != "" {
			d["class"] = slug
		}
	}
}

func (p *Provider) sensors() []Sensor {
	result := []Sensor{}
	for _, path := range matches(filepath.Join(p.sys, "class/hwmon/hwmon*")) {
		device := read(filepath.Join(path, "name"))
		if device == "" {
			device = filepath.Base(path)
		}
		for _, kind := range []struct {
			prefix, unit, slug string
			scale              float64
		}{
			{"temp", "°C", SensorTemperature, 1000}, {"fan", "RPM", SensorFan, 1}, {"in", "V", SensorVoltage, 1000},
			{"curr", "A", SensorCurrent, 1000}, {"power", "W", SensorPower, 1e6}, {"humidity", "%", SensorHumidity, 1000},
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
				result = append(result, Sensor{Device: device, Name: name, Value: value / kind.scale, Unit: kind.unit, Kind: kind.slug})
			}
		}
	}
	for _, path := range matches(filepath.Join(p.sys, "class/thermal/thermal_zone*")) {
		value, err := strconv.ParseFloat(read(filepath.Join(path, "temp")), 64)
		if err == nil && !math.IsNaN(value) && !math.IsInf(value, 0) {
			result = append(result, Sensor{Device: filepath.Base(path), Name: read(filepath.Join(path, "type")),
				Value: value / 1000, Unit: "°C", Kind: SensorTemperature})
		}
	}
	return result
}

func (p *Provider) power() []Details {
	result := []Details{}
	for _, path := range matches(filepath.Join(p.sys, "class/power_supply/*")) {
		d := p.attributes(path, map[string]string{"status": "status", "capacity_percent": "capacity", "online": "online"})
		d["name"] = filepath.Base(path)
		result = append(result, d)
	}
	return result
}
