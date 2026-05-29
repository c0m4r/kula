package collector

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type netRaw struct {
	rxBytes, txBytes uint64
	rxPkts, txPkts   uint64
	rxErrs, txErrs   uint64
	rxDrop, txDrop   uint64
}

func (c *Collector) parseNetDev() map[string]netRaw {
	f, err := os.Open(filepath.Join(procPath, "net/dev"))
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	explicitFilter := len(c.collCfg.Interfaces) > 0
	// Only build interface-name strings for log lines when debug output is
	// actually enabled this tick; otherwise the hot path stays allocation-free.
	dbg := c.collCfg.DebugLog && !c.debugDone
	result := make(map[string]netRaw)
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		if lineNum <= 2 {
			continue // skip header lines
		}
		line := scanner.Bytes()
		colon := bytes.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		name := bytes.TrimSpace(line[:colon])

		if explicitFilter {
			// Explicit list: the user gets exactly what they asked for, no filtering.
			allowed := false
			for _, allowedIface := range c.collCfg.Interfaces {
				if string(name) == allowedIface { // string(b) == s does not allocate
					allowed = true
					break
				}
			}
			if !allowed {
				if dbg {
					c.debugf(" net: skipping %q — not in configured interfaces list", name)
				}
				continue
			}
		} else {
			// Auto-discovery mode: skip loopback and virtual/container interfaces.
			// Matched on bytes so skipped interfaces (e.g. the many veth devices on
			// a container host) never cost a string allocation.
			var skipReason string
			switch {
			case string(name) == "lo":
				skipReason = "loopback"
			case bytes.HasPrefix(name, prefixVeth):
				skipReason = "veth (container virtual interface)"
			case bytes.HasPrefix(name, prefixDocker):
				skipReason = "docker bridge"
			case bytes.HasPrefix(name, prefixBr):
				skipReason = "Linux bridge"
			case bytes.HasPrefix(name, prefixVirbr):
				skipReason = "libvirt bridge"
			case bytes.HasPrefix(name, prefixVnet):
				skipReason = "KVM/QEMU virtual NIC"
			case bytes.HasPrefix(name, prefixTap):
				skipReason = "TAP interface (VM/VPN)"
			case bytes.HasPrefix(name, prefixTun):
				skipReason = "TUN interface (VPN)"
			}
			if skipReason != "" {
				if dbg {
					c.debugf(" net: skipping %q — %s", name, skipReason)
				}
				continue
			}
		}

		// Parse the counter columns directly from the line bytes. /proc/net/dev
		// lists 16 columns; we keep rx bytes/pkts/errs/drop (0-3) and tx
		// bytes/pkts/errs/drop (8-11).
		var n netRaw
		idx := 0
		pos := colon + 1
		for {
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
				n.rxBytes = parseUintBytes(field)
			case 1:
				n.rxPkts = parseUintBytes(field)
			case 2:
				n.rxErrs = parseUintBytes(field)
			case 3:
				n.rxDrop = parseUintBytes(field)
			case 8:
				n.txBytes = parseUintBytes(field)
			case 9:
				n.txPkts = parseUintBytes(field)
			case 10:
				n.txErrs = parseUintBytes(field)
			case 11:
				n.txDrop = parseUintBytes(field)
			}
			idx++
		}
		if idx < 16 {
			continue
		}
		result[string(name)] = n
		if dbg {
			c.debugf(" net: monitoring interface %q", name)
		}
	}
	if dbg {
		if len(result) == 0 {
			c.debugf(" net: no interfaces selected for monitoring")
		} else {
			c.debugf(" net: monitoring %d interface(s)", len(result))
		}
	}
	return result
}

// Interface-name prefixes for virtual/container devices skipped during
// auto-discovery, kept as package-level byte slices so the match loop allocates
// nothing per line.
var (
	prefixVeth   = []byte("veth")
	prefixDocker = []byte("docker")
	prefixBr     = []byte("br-")
	prefixVirbr  = []byte("virbr")
	prefixVnet   = []byte("vnet")
	prefixTap    = []byte("tap")
	prefixTun    = []byte("tun")
)

func (c *Collector) collectNetwork(elapsed float64) NetworkStats {
	current := c.parseNetDev()
	stats := NetworkStats{}

	for name, cur := range current {
		iface := NetInterface{
			Name:    name,
			RxBytes: cur.rxBytes,
			TxBytes: cur.txBytes,
			RxPkts:  cur.rxPkts,
			TxPkts:  cur.txPkts,
			RxErrs:  cur.rxErrs,
			TxErrs:  cur.txErrs,
			RxDrop:  cur.rxDrop,
			TxDrop:  cur.txDrop,
		}

		if prev, ok := c.prevNet[name]; ok && elapsed > 0 {
			// Guard against uint64 underflow on counter reset/wrap
			if cur.rxBytes >= prev.rxBytes {
				iface.RxMbps = round2(float64(cur.rxBytes-prev.rxBytes) * 8.0 / elapsed / 1_000_000.0)
			}
			if cur.txBytes >= prev.txBytes {
				iface.TxMbps = round2(float64(cur.txBytes-prev.txBytes) * 8.0 / elapsed / 1_000_000.0)
			}
			if cur.rxPkts >= prev.rxPkts {
				iface.RxPPS = round2(float64(cur.rxPkts-prev.rxPkts) / elapsed)
			}
			if cur.txPkts >= prev.txPkts {
				iface.TxPPS = round2(float64(cur.txPkts-prev.txPkts) / elapsed)
			}
		}

		stats.Interfaces = append(stats.Interfaces, iface)
	}

	c.prevNet = current
	stats.Sockets = parseSocketStats()
	stats.TCP = c.collectTCPStats(elapsed)

	return stats
}

// parseSocketStats reads /proc/net/sockstat and extracts the three
// counters we actually display: tcp_inuse, tcp_tw, udp_inuse.
func parseSocketStats() SocketStats {
	ss := SocketStats{}
	f, err := os.Open(filepath.Join(procPath, "net/sockstat"))
	if err != nil {
		return ss
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		switch fields[0] {
		case "TCP:":
			for i := 1; i+1 < len(fields); i += 2 {
				val64, _ := strconv.ParseInt(fields[i+1], 10, 32)
				val := int(val64)
				switch fields[i] {
				case "inuse":
					ss.TCPInUse = val
				case "tw":
					ss.TCPTw = val
				}
			}
		case "UDP:":
			for i := 1; i+1 < len(fields); i += 2 {
				val64, _ := strconv.ParseInt(fields[i+1], 10, 32)
				val := int(val64)
				if fields[i] == "inuse" {
					ss.UDPInUse = val
				}
			}
		}
	}
	return ss
}

// tcpRaw holds the raw cumulative TCP counters from /proc/net/snmp and /proc/net/netstat.
type tcpRaw struct {
	currEstab uint64
	inErrs    uint64
	outRsts   uint64
	retrans   uint64 // TCPRetransSegs from /proc/net/netstat (TcpExt)
}

// collectTCPStats reads /proc/net/snmp and /proc/net/netstat and returns
// per-second rates for InErrs, OutRsts, Retrans, and the current gauge value for CurrEstab.
func (c *Collector) collectTCPStats(elapsed float64) TCPStats {
	cur := readTCPRaw()
	cur.retrans = readTCPRetrans()
	ts := TCPStats{
		CurrEstab: cur.currEstab,
	}
	if elapsed > 0 {
		// Guard against uint64 underflow on counter reset
		if c.prevTCP.inErrs > 0 && cur.inErrs >= c.prevTCP.inErrs {
			ts.InErrs = round2(float64(cur.inErrs-c.prevTCP.inErrs) / elapsed)
		}
		if c.prevTCP.outRsts > 0 && cur.outRsts >= c.prevTCP.outRsts {
			ts.OutRsts = round2(float64(cur.outRsts-c.prevTCP.outRsts) / elapsed)
		}
		if c.prevTCP.retrans > 0 && cur.retrans >= c.prevTCP.retrans {
			ts.Retrans = round2(float64(cur.retrans-c.prevTCP.retrans) / elapsed)
		}
	}
	c.prevTCP = cur
	return ts
}

// readTCPRaw reads the raw cumulative TCP counters from /proc/net/snmp.
func readTCPRaw() tcpRaw {
	f, err := os.Open(filepath.Join(procPath, "net/snmp"))
	if err != nil {
		return tcpRaw{}
	}
	defer func() { _ = f.Close() }()

	var raw tcpRaw
	scanner := bufio.NewScanner(f)
	var headerFields []string
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		prefix := strings.TrimSuffix(fields[0], ":")
		if prefix != "Tcp" {
			continue
		}
		if headerFields == nil {
			headerFields = fields[1:]
			continue
		}
		// Values line
		values := fields[1:]
		for i, hdr := range headerFields {
			if i >= len(values) {
				break
			}
			val, _ := strconv.ParseUint(values[i], 10, 64)
			switch hdr {
			case "CurrEstab":
				raw.currEstab = val
			case "InErrs":
				raw.inErrs = val
			case "OutRsts":
				raw.outRsts = val
			}
		}
		break
	}
	return raw
}

// readTCPRetrans reads TCPRetransSegs from /proc/net/netstat (TcpExt section).
func readTCPRetrans() uint64 {
	f, err := os.Open(filepath.Join(procPath, "net/netstat"))
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	// Buffer large enough for the long TcpExt lines
	scanner.Buffer(make([]byte, 0, 8192), 65536)
	var headerFields []string
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		prefix := strings.TrimSuffix(fields[0], ":")
		if prefix != "TcpExt" {
			headerFields = nil
			continue
		}
		if headerFields == nil {
			headerFields = fields[1:]
			continue
		}
		// Values line
		values := fields[1:]
		for i, hdr := range headerFields {
			if i >= len(values) {
				break
			}
			if hdr == "TCPRetransSegs" {
				val, _ := strconv.ParseUint(values[i], 10, 64)
				return val
			}
		}
		break
	}
	return 0
}

// DetectLinkSpeed returns the combined theoretical maximum throughput of all UP interfaces in Mbps, or 0 if undetected.
func (c *Collector) DetectLinkSpeed() float64 {
	var totalSpeedMbps float64
	entries, err := os.ReadDir(filepath.Join(sysPath, "class", "net"))
	if err == nil {
		for _, entry := range entries {
			name := entry.Name()
			// Skip loopback and virtual/container interfaces — same set as parseNetDev auto-discovery
			if name == "lo" ||
				strings.HasPrefix(name, "veth") ||
				strings.HasPrefix(name, "docker") ||
				strings.HasPrefix(name, "br-") ||
				strings.HasPrefix(name, "virbr") ||
				strings.HasPrefix(name, "vnet") ||
				strings.HasPrefix(name, "tap") ||
				strings.HasPrefix(name, "tun") {
				continue
			}

			// Ensure interface is up before including its speed
			operstate, err := os.ReadFile(filepath.Join(sysPath, "class", "net", name, "operstate"))
			if err != nil || strings.TrimSpace(string(operstate)) != "up" {
				continue
			}

			data, err := os.ReadFile(filepath.Join(sysPath, "class", "net", name, "speed"))
			if err == nil {
				val, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
				// Negative values map to unknown speed in sysfs speed reports (-1)
				if err == nil && val > 0 {
					totalSpeedMbps += val
				}
			}
		}
	}

	return totalSpeedMbps
}
