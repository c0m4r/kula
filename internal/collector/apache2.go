package collector

import (
	"io"
	"net/http"
	"strconv"
	"strings"
)

// apache2Raw holds the raw cumulative counters from mod_status ?auto, and
// whether each was on the page: without ExtendedStatus the page lacks both, and
// a counter that was never read is no baseline for a rate.
type apache2Raw struct {
	totalAccesses uint64
	totalKBytes   uint64
	hasAccesses   bool
	hasKBytes     bool
}

// collectApache2 fetches the Apache2 mod_status ?auto page and parses it.
// Returns nil on any error (server down, bad format, etc.) — non-fatal.
//
// Expected ?auto format (key-value pairs):
//
//	Total Accesses: 1234
//	Total kBytes: 5678
//	CPULoad: .12345
//	Uptime: 12345
//	ReqPerSec: .1
//	BytesPerSec: 470.7
//	BytesPerReq: 4707.21
//	BusyWorkers: 3
//	IdleWorkers: 7
//	Scoreboard: _W___R___................................................
func (c *Collector) collectApache2(elapsed float64) *Apache2Stats {
	stats := c.fetchApache2(elapsed)
	if stats == nil {
		// Rates are deltas between consecutive successful polls. Dropping the
		// baseline here keeps the next poll from dividing two intervals'
		// growth by one.
		c.prevApache = apache2Raw{}
		return nil
	}
	c.apacheLog.ok()
	return stats
}

func (c *Collector) fetchApache2(elapsed float64) *Apache2Stats {
	if c.apacheClient == nil {
		c.apacheClient = statusPageClient(c.collCfg.Interval)
	}

	req, err := http.NewRequestWithContext(c.requestContext(), http.MethodGet, c.appCfg.Apache2.StatusURL, nil)
	if err != nil {
		c.apacheLog.failf("bad status URL: %v", redactURLError(err))
		return nil
	}
	resp, err := c.apacheClient.Do(req)
	if err != nil {
		c.apacheLog.failf("fetch error: %v", redactURLError(err))
		return nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		c.apacheLog.failf("unexpected status: %d", resp.StatusCode)
		return nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 65536))
	if err != nil {
		c.apacheLog.failf("read error: %v", err)
		return nil
	}

	c.debugf("[apache2] status body: %q", body)
	return c.parseApache2Status(string(body), elapsed)
}

// parseApache2Status parses the Apache2 mod_status ?auto key-value format
// and computes per-second rates using the previous raw counters. The
// scoreboard, which may continue over the lines after its key, is counted as
// it is read: the page is the server's to shape, and gathering a scoreboard
// cut into many lines by concatenation would cost the square of its length.
func (c *Collector) parseApache2Status(body string, elapsed float64) *Apache2Stats {
	stats := &Apache2Stats{}
	var cur apache2Raw
	scoreboardActive := false

	for line := range strings.Lines(body) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			if scoreboardActive {
				countApache2Scoreboard(stats, line)
			}
			continue
		}

		key := strings.TrimSpace(line[:colon])
		val := strings.TrimSpace(line[colon+1:])

		if key == "Scoreboard" {
			scoreboardActive = true
			countApache2Scoreboard(stats, val)
			continue
		}

		scoreboardActive = false

		switch key {
		case "BusyWorkers":
			if v, err := strconv.Atoi(val); err == nil {
				stats.BusyWorkers = v
			}
		case "IdleWorkers":
			if v, err := strconv.Atoi(val); err == nil {
				stats.IdleWorkers = v
			}
		case "Total Accesses":
			if v, err := strconv.ParseUint(val, 10, 64); err == nil {
				stats.TotalAccesses = v
				cur.hasAccesses = true
			}
		case "Total kBytes":
			if v, err := strconv.ParseUint(val, 10, 64); err == nil {
				stats.TotalKBytes = v
				cur.hasKBytes = true
			}
		case "ReqPerSec":
			stats.ReqPerSec = parseFloatQuiet(val)
		case "BytesPerSec":
			stats.BytesPerSec = parseFloatQuiet(val)
		case "BytesPerReq":
			stats.BytesPerReq = parseFloatQuiet(val)
		case "CPULoad":
			stats.CPULoad = parseFloatQuiet(val)
		case "Uptime":
			if v, err := strconv.ParseInt(val, 10, 64); err == nil {
				stats.Uptime = v
			}
		}
	}

	// Compute per-second rates from cumulative deltas
	// Each counter needs a reading at both ends, and a counter that went
	// backwards (a restart) yields no rate rather than an underflowed one.
	cur.totalAccesses = stats.TotalAccesses
	cur.totalKBytes = stats.TotalKBytes
	prev := c.prevApache
	if elapsed > 0 {
		if prev.hasAccesses && cur.hasAccesses && cur.totalAccesses >= prev.totalAccesses {
			stats.AccessesPS = round2(float64(cur.totalAccesses-prev.totalAccesses) / elapsed)
		}
		if prev.hasKBytes && cur.hasKBytes && cur.totalKBytes >= prev.totalKBytes {
			stats.KBytesPS = round2(float64(cur.totalKBytes-prev.totalKBytes) / elapsed)
		}
	}
	c.prevApache = cur

	c.debugf("[apache2] parsed: busy=%d idle=%d accesses=%d kbytes=%d rps=%.2f bps=%.2f W=%d R=%d S=%d K=%d D=%d C=%d L=%d G=%d I=%d .=%d",
		stats.BusyWorkers, stats.IdleWorkers, stats.TotalAccesses, stats.TotalKBytes,
		stats.ReqPerSec, stats.BytesPerSec,
		stats.Waiting, stats.Reading, stats.Sending, stats.Keepalive,
		stats.DNS, stats.Closing, stats.Logging, stats.Graceful, stats.IdleCleanup, stats.OpenSlots)

	if stats.TotalAccesses == 0 && stats.BusyWorkers == 0 && stats.IdleWorkers == 0 {
		c.apacheLog.failf("status page has no worker or access counts (is it mod_status ?auto?)")
		return nil
	}

	return stats
}

// countApache2Scoreboard adds the worker states of a piece of the scoreboard.
func countApache2Scoreboard(stats *Apache2Stats, states string) {
	for i := 0; i < len(states); i++ {
		switch states[i] {
		case '_':
			stats.Waiting++
		case 'S':
			stats.Starting++
		case 'R':
			stats.Reading++
		case 'W':
			stats.Sending++
		case 'K':
			stats.Keepalive++
		case 'D':
			stats.DNS++
		case 'C':
			stats.Closing++
		case 'L':
			stats.Logging++
		case 'G':
			stats.Graceful++
		case 'I':
			stats.IdleCleanup++
		case '.':
			stats.OpenSlots++
		}
	}
}

// parseFloatQuiet parses a float64 from a string, returning 0 on failure.
func parseFloatQuiet(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		if vv, e2 := strconv.ParseFloat(strings.ReplaceAll(s, ",", "."), 64); e2 == nil {
			return vv
		}
		return 0
	}
	return v
}
