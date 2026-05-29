package collector

import (
	"math"
	"strconv"
)

// round2 rounds a float to 2 decimal places
func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

// parseUintBytes parses an unsigned base-10 integer directly from a byte slice
// without allocating an intermediate string. It returns 0 if b is empty or holds
// any non-digit byte, matching the error-to-zero behaviour of the strconv-based
// parse wrappers used elsewhere in the collector. This keeps the per-second
// /proc parsing hot path allocation-free.
func parseUintBytes(b []byte) uint64 {
	if len(b) == 0 {
		return 0
	}
	var n uint64
	for _, ch := range b {
		if ch < '0' || ch > '9' {
			return 0
		}
		n = n*10 + uint64(ch-'0')
	}
	return n
}

// parseUint wrapper replacing `strconv.ParseUint` that logs errors explicitly at debug level
func (c *Collector) parseUint(s string, base int, bitSize int, fieldName string) uint64 {
	if s == "" {
		return 0
	}
	val, err := strconv.ParseUint(s, base, bitSize)
	if err != nil {
		if fieldName != "" {
			c.debugf(" collector: failed to parse %s (%q): %v", fieldName, s, err)
		}
		return 0
	}
	return val
}

// parseInt wrapper replacing `strconv.ParseInt` that logs errors explicitly at debug level
func (c *Collector) parseInt(s string, base int, bitSize int, fieldName string) int64 {
	if s == "" {
		return 0
	}
	val, err := strconv.ParseInt(s, base, bitSize)
	if err != nil {
		if fieldName != "" {
			c.debugf(" collector: failed to parse %s (%q): %v", fieldName, s, err)
		}
		return 0
	}
	return val
}

// parseFloat wrapper replacing `strconv.ParseFloat` that logs errors explicitly at debug level
func (c *Collector) parseFloat(s string, bitSize int, fieldName string) float64 {
	if s == "" {
		return 0
	}
	val, err := strconv.ParseFloat(s, bitSize)
	if err != nil {
		if fieldName != "" {
			c.debugf(" collector: failed to parse %s (%q): %v", fieldName, s, err)
		}
		return 0
	}
	return val
}
