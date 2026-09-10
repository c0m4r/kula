package sysinfo

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
)

// Decode only Memory Device (type 17) records from the kernel's SMBIOS table.
// Field offsets and sentinels follow DMTF DSP0134, section 7.18. Every optional
// field is bounded by the record's formatted length; no /dev/mem access is used.
func memoryDevices(table []byte) []Details {
	result := []Details{}
	for len(table) >= 4 {
		kind, size := table[0], int(table[1])
		if size < 4 || size > len(table) {
			break
		}
		end := bytes.Index(table[size:], []byte{0, 0})
		if end < 0 {
			break
		}
		if kind == 17 && size >= 0x15 {
			stringsTable := bytes.Split(table[size:size+end], []byte{0})
			d := memoryDevice(table[:size], stringsTable)
			result = append(result, d)
		}
		if kind == 127 {
			break
		}
		table = table[size+end+2:]
	}
	return result
}

func memoryDevice(data []byte, stringsTable [][]byte) Details {
	d := Details{"source": "SMBIOS", "handle": fmt.Sprintf("0x%04x", binary.LittleEndian.Uint16(data[2:4]))}
	for key, offset := range map[string]int{"slot": 0x10, "bank": 0x11, "manufacturer": 0x17, "serial": 0x18, "asset_tag": 0x19, "part_number": 0x1a} {
		if offset >= len(data) {
			continue
		}
		index := int(data[offset])
		if index > 0 && index <= len(stringsTable) {
			if value := strings.TrimSpace(string(stringsTable[index-1])); value != "" {
				d[key] = value
			}
		}
	}
	size := binary.LittleEndian.Uint16(data[0x0c:0x0e])
	switch {
	case size == 0:
		d["status"] = "Empty slot"
	case size == 0xffff: // Unknown capacity.
	case size == 0x7fff:
		if len(data) >= 0x20 {
			d["size_mib"] = strconv.FormatUint(uint64(binary.LittleEndian.Uint32(data[0x1c:0x20])&0x7fffffff), 10)
		}
	case size&0x8000 != 0:
		d["size_kib"] = strconv.Itoa(int(size & 0x7fff))
	default:
		d["size_mib"] = strconv.Itoa(int(size))
	}
	for key, offset := range map[string]int{"total_width_bits": 8, "data_width_bits": 10, "configured_voltage_mv": 0x26} {
		if len(data) >= offset+2 {
			value := binary.LittleEndian.Uint16(data[offset:])
			if value != 0 && value != 0xffff {
				d[key] = strconv.Itoa(int(value))
			}
		}
	}
	for _, field := range []struct {
		name             string
		offset, extended int
	}{
		{"speed_mts", 0x15, 0x54}, {"configured_speed_mts", 0x20, 0x58},
	} {
		if len(data) < field.offset+2 {
			continue
		}
		value := uint32(binary.LittleEndian.Uint16(data[field.offset:]))
		if value == 0xffff {
			if len(data) < field.extended+4 {
				continue
			}
			value = binary.LittleEndian.Uint32(data[field.extended:]) & 0x7fffffff
		}
		if value > 0 {
			d[field.name] = strconv.FormatUint(uint64(value), 10)
		}
	}
	if len(data) > 0x1b && data[0x1b]&0x0f != 0 {
		d["ranks"] = strconv.Itoa(int(data[0x1b] & 0x0f))
	}
	memoryTypes := map[byte]string{0x12: "DDR", 0x13: "DDR2", 0x18: "DDR3", 0x1a: "DDR4", 0x1b: "LPDDR",
		0x1c: "LPDDR2", 0x1d: "LPDDR3", 0x1e: "LPDDR4", 0x20: "HBM", 0x21: "HBM2", 0x22: "DDR5", 0x23: "LPDDR5", 0x24: "HBM3"}
	if value := memoryTypes[data[0x12]]; value != "" {
		d["type"] = value
	} else if data[0x12] > 2 {
		d["type"] = fmt.Sprintf("SMBIOS 0x%02x", data[0x12])
	}
	formFactors := map[byte]string{0x09: "DIMM", 0x0d: "SODIMM", 0x0b: "Row of chips", 0x0f: "FB-DIMM", 0x10: "Die"}
	if value := formFactors[data[0x0e]]; value != "" {
		d["form_factor"] = value
	}
	return d
}
