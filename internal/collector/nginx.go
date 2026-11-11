package collector

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
)

// nginxRaw holds the raw cumulative counters from stub_status.
type nginxRaw struct {
	accepts  uint64
	handled  uint64
	requests uint64
	// seen is set once the counters come from a successful poll, so a
	// zero-valued baseline is never mistaken for one.
	seen bool
}

// collectNginx fetches the nginx stub_status page and parses it.
// Returns nil on any error (nginx down, bad format, etc.) — non-fatal.
//
// Expected stub_status format:
//
//	Active connections: 4
//	server accepts handled requests
//	 7 7 7
//	Reading: 0 Writing: 3 Waiting: 1
func (c *Collector) collectNginx(elapsed float64) *NginxStats {
	stats := c.fetchNginx(elapsed)
	if stats == nil {
		// Rates are deltas between consecutive successful polls. Dropping the
		// baseline here keeps the next poll from dividing two intervals'
		// growth by one.
		c.prevNginx = nginxRaw{}
		return nil
	}
	c.nginxLog.ok()
	return stats
}

func (c *Collector) fetchNginx(elapsed float64) *NginxStats {
	if c.nginxClient == nil {
		c.nginxClient = statusPageClient(c.collCfg.Interval)
	}

	req, err := http.NewRequestWithContext(c.requestContext(), http.MethodGet, c.appCfg.Nginx.StatusURL, nil)
	if err != nil {
		c.nginxLog.failf("bad status URL: %v", redactURLError(err))
		return nil
	}
	resp, err := c.nginxClient.Do(req)
	if err != nil {
		c.nginxLog.failf("fetch error: %v", redactURLError(err))
		return nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		c.nginxLog.failf("unexpected status: %d", resp.StatusCode)
		return nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		c.nginxLog.failf("read error: %v", err)
		return nil
	}

	c.debugf("[nginx] status body: %q", body)
	return c.parseNginxStatus(string(body), elapsed)
}

// parseNginxStatus parses the nginx stub_status text format and computes
// per-second rates using the previous raw counters.
func (c *Collector) parseNginxStatus(body string, elapsed float64) *NginxStats {
	stats := &NginxStats{}
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if len(lines) < 4 {
		c.nginxLog.failf("too few lines in body: %d (expected 4+)", len(lines))
		return nil
	}

	// Line 1: "Active connections: N"
	if _, err := fmt.Sscanf(lines[0], "Active connections: %d", &stats.ActiveConnections); err != nil {
		c.nginxLog.failf("failed to parse active connections: %v (line: %q)", err, lines[0])
		return nil
	}

	// Line 3: " accepts handled requests" (the values line)
	fields := strings.Fields(strings.TrimSpace(lines[2]))
	if len(fields) < 3 {
		c.nginxLog.failf("too few fields in accepts line: %d", len(fields))
		return nil
	}
	accepts, err1 := strconv.ParseUint(fields[0], 10, 64)
	handled, err2 := strconv.ParseUint(fields[1], 10, 64)
	requests, err3 := strconv.ParseUint(fields[2], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil {
		c.nginxLog.failf("failed to parse counters: %v %v %v", err1, err2, err3)
		return nil
	}

	stats.Accepts = accepts
	stats.Handled = handled
	stats.Requests = requests

	// Compute per-second rates from deltas. A restart resets all three
	// counters together, so any one going backwards means every delta of this
	// poll spans the restart: report no rates rather than an underflowed one.
	cur := nginxRaw{accepts: accepts, handled: handled, requests: requests, seen: true}
	prev := c.prevNginx
	if prev.seen && elapsed > 0 &&
		cur.accepts >= prev.accepts && cur.handled >= prev.handled && cur.requests >= prev.requests {
		stats.AcceptsPS = round2(float64(cur.accepts-prev.accepts) / elapsed)
		stats.HandledPS = round2(float64(cur.handled-prev.handled) / elapsed)
		stats.RequestsPS = round2(float64(cur.requests-prev.requests) / elapsed)
	}
	c.prevNginx = cur

	// Line 4: "Reading: N Writing: N Waiting: N"
	if _, err := fmt.Sscanf(lines[3], "Reading: %d Writing: %d Waiting: %d",
		&stats.Reading, &stats.Writing, &stats.Waiting); err != nil {
		// Non-fatal: we got the main counters at least
		if c.collCfg.DebugLog {
			log.Printf("[nginx] warn: line 4 parse error: %v", err)
		}
		return stats
	}

	c.debugf("[nginx] parsed: active=%d, accepts=%d, handled=%d, requests=%d, r/w/w=%d/%d/%d",
		stats.ActiveConnections, stats.Accepts, stats.Handled, stats.Requests,
		stats.Reading, stats.Writing, stats.Waiting)

	return stats
}
