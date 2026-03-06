package collector

import (
	"os"
	"path/filepath"
	"strings"
)

type selfRaw struct {
	utime uint64
	stime uint64
}

func (c *Collector) collectSelf(elapsed float64) SelfStats {
	s := SelfStats{}

	// Read /proc/self/stat for CPU
	data, err := os.ReadFile(filepath.Join(procPath, "self/stat"))
	if err == nil {
		content := string(data)
		// Find fields after the command name (enclosed in parens)
		idx := strings.LastIndex(content, ")")
		if idx >= 0 && idx+2 < len(content) {
			fields := strings.Fields(content[idx+2:])
			// utime is field index 11 (0-based from after state), stime is 12
			if len(fields) > 12 {
				cur := selfRaw{}
				cur.utime = parseUint(fields[11], 10, 64, "self.utime")
				cur.stime = parseUint(fields[12], 10, 64, "self.stime")

				if c.prevSelf.utime > 0 && elapsed > 0 {
					// Clock ticks per second is typically 100
					const clockTick = 100
					uDelta := cur.utime - c.prevSelf.utime
					sDelta := cur.stime - c.prevSelf.stime
					totalDelta := float64(uDelta+sDelta) / clockTick
					s.CPUPercent = round2(totalDelta / elapsed * 100.0)
				}
				c.prevSelf = cur
			}
		}
	}

	// Read /proc/self/status for RSS memory
	statusData, err := os.ReadFile(filepath.Join(procPath, "self/status"))
	if err == nil {
		for _, line := range strings.Split(string(statusData), "\n") {
			if strings.HasPrefix(line, "VmRSS:") {
				s.MemRSS = parseStatusKB(line, "self.rss") * 1024
				break
			}
		}
	}

	// Count FDs
	if fds, err := os.ReadDir(filepath.Join(procPath, "self/fd")); err == nil {
		s.FDs = len(fds)
	}

	return s
}

func parseStatusKB(line string, metricName string) uint64 {
	parts := strings.Fields(line)
	if len(parts) >= 2 {
		val := parseUint(parts[1], 10, 64, metricName)
		return val
	}
	return 0
}
