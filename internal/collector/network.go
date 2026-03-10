package collector

import (
	"bufio"
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

	result := make(map[string]netRaw)
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		if lineNum <= 2 {
			continue // skip header lines
		}
		line := scanner.Text()
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		if strings.HasPrefix(name, "veth") || strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "br-") {
			continue
		}

		if len(c.collCfg.Interfaces) > 0 {
			allowed := false
			for _, allowedIface := range c.collCfg.Interfaces {
				if allowedIface == name {
					allowed = true
					break
				}
			}
			if !allowed {
				continue
			}
		}

		fields := strings.Fields(parts[1])
		if len(fields) < 16 {
			continue
		}
		n := netRaw{}
		n.rxBytes = parseUint(fields[0], 10, 64, "network.rxBytes")
		n.rxPkts = parseUint(fields[1], 10, 64, "network.rxPkts")
		n.rxErrs = parseUint(fields[2], 10, 64, "network.rxErrs")
		n.rxDrop = parseUint(fields[3], 10, 64, "network.rxDrop")
		n.txBytes = parseUint(fields[8], 10, 64, "network.txBytes")
		n.txPkts = parseUint(fields[9], 10, 64, "network.txPkts")
		n.txErrs = parseUint(fields[10], 10, 64, "network.txErrs")
		n.txDrop = parseUint(fields[11], 10, 64, "network.txDrop")
		result[name] = n
	}
	return result
}

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
			rxDelta := cur.rxBytes - prev.rxBytes
			txDelta := cur.txBytes - prev.txBytes
			iface.RxMbps = round2(float64(rxDelta) * 8.0 / elapsed / 1_000_000.0)
			iface.TxMbps = round2(float64(txDelta) * 8.0 / elapsed / 1_000_000.0)
			rxPktDelta := cur.rxPkts - prev.rxPkts
			txPktDelta := cur.txPkts - prev.txPkts
			iface.RxPPS = round2(float64(rxPktDelta) / elapsed)
			iface.TxPPS = round2(float64(txPktDelta) / elapsed)
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
				val, _ := strconv.Atoi(fields[i+1])
				switch fields[i] {
				case "inuse":
					ss.TCPInUse = val
				case "tw":
					ss.TCPTw = val
				}
			}
		case "UDP:":
			for i := 1; i+1 < len(fields); i += 2 {
				val, _ := strconv.Atoi(fields[i+1])
				if fields[i] == "inuse" {
					ss.UDPInUse = val
				}
			}
		}
	}
	return ss
}

// tcpRaw holds the raw cumulative TCP counters from /proc/net/snmp.
type tcpRaw struct {
	currEstab uint64
	inErrs    uint64
	outRsts   uint64
}

// collectTCPStats reads /proc/net/snmp and returns per-second rates for
// InErrs and OutRsts, and the current gauge value for CurrEstab.
func (c *Collector) collectTCPStats(elapsed float64) TCPStats {
	cur := readTCPRaw()
	ts := TCPStats{
		CurrEstab: cur.currEstab,
	}
	if c.prevTCP.inErrs > 0 && elapsed > 0 {
		ts.InErrs = round2(float64(cur.inErrs-c.prevTCP.inErrs) / elapsed)
		ts.OutRsts = round2(float64(cur.outRsts-c.prevTCP.outRsts) / elapsed)
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

// DetectLinkSpeed returns the combined theoretical maximum throughput of all UP interfaces in Mbps, or 0 if undetected.
func DetectLinkSpeed() float64 {
	var totalSpeedMbps float64
	entries, err := os.ReadDir(filepath.Join(sysPath, "class", "net"))
	if err == nil {
		for _, entry := range entries {
			name := entry.Name()
			if name == "lo" || strings.HasPrefix(name, "veth") || strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "br-") {
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
