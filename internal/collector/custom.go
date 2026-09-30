package collector

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"kula/internal/config"
)

// customConnIdleTimeout bounds how long a connection may sit without sending a
// line before it is closed. It reaps dead or hung clients while staying well
// above any realistic push interval, so active streaming producers that push on
// the collection loop are never dropped mid-stream.
const customConnIdleTimeout = 5 * time.Minute

const (
	// defaultCustomStaleFloor is the minimum derived staleness window. It keeps
	// sub-second collection intervals from flapping on socket/scheduling jitter.
	defaultCustomStaleFloor = 5 * time.Second
	// defaultCustomStaleCycles is how many collection intervals a group may miss
	// before it is treated as stale, when no explicit window is configured.
	defaultCustomStaleCycles = 5
)

// defaultCustomStaleAfter derives a staleness window from the collection
// interval. Producers are expected to push roughly once per interval, so a few
// missed cycles means the feed has stopped.
func defaultCustomStaleAfter(interval time.Duration) time.Duration {
	d := time.Duration(defaultCustomStaleCycles) * interval
	if d < defaultCustomStaleFloor {
		d = defaultCustomStaleFloor
	}
	return d
}

// customCollector listens on a Unix socket for JSON-encoded custom metrics.
//
// Clients connect, send a single JSON line, and disconnect. The expected format:
//
//	{"custom": {"cpu_fans": [{"fan1": 4423}, {"fan2": 8512}]}}
//
// Each top-level key under "custom" is a chart group. Each array element is an
// object with a single key (metric name) → numeric value. The collector maps
// incoming metric names to the configured CustomMetricConfig entries to validate
// and store values.
type customCollector struct {
	mu          sync.RWMutex
	latest      map[string]customGroup
	configSet   map[string]map[string]struct{} // group -> set of configured names (membership)
	configOrder map[string][]string            // group -> configured names in config order
	staleAfter  time.Duration                  // drop a group's values this long after its last update
	sockPath    string
	listener    net.Listener
	debug       bool
	debugDone   atomic.Bool
}

// customGroup is the latest accepted values for one chart group together with
// the time they arrived, so a feed whose producer has stopped can be expired.
type customGroup struct {
	values []CustomMetricValue
	seen   time.Time
}

// indexConfigs precomputes, per group, the set of configured metric names (for
// O(1) membership tests) and their declaration order (for deterministic output).
// Duplicate names within a group's config are collapsed — first occurrence wins.
// Built once at construction so processMessage allocates no per-message lookup.
func indexConfigs(configs map[string][]config.CustomMetricConfig) (set map[string]map[string]struct{}, order map[string][]string) {
	set = make(map[string]map[string]struct{}, len(configs))
	order = make(map[string][]string, len(configs))
	for group, cfgs := range configs {
		names := make(map[string]struct{}, len(cfgs))
		ordered := make([]string, 0, len(cfgs))
		for _, c := range cfgs {
			if _, dup := names[c.Name]; dup {
				continue
			}
			names[c.Name] = struct{}{}
			ordered = append(ordered, c.Name)
		}
		set[group] = names
		order[group] = ordered
	}
	return set, order
}

// customMessage is the expected JSON envelope from socket clients.
type customMessage struct {
	Custom map[string][]map[string]float64 `json:"custom"`
}

// newCustomCollector creates a new collector and starts listening on the socket.
// staleAfter must be > 0; callers resolve a zero config value to a default
// (see defaultCustomStaleAfter) before constructing.
func newCustomCollector(ctx context.Context, sockPath string, configs map[string][]config.CustomMetricConfig, staleAfter time.Duration, debug bool) (*customCollector, error) {
	// A socket another process still serves is live, not stale: a connection
	// to it succeeds. Removing it would silently cut that process (a running
	// `kula serve`) off from every client, so refuse instead.
	if conn, err := net.DialTimeout("unix", sockPath, time.Second); err == nil {
		_ = conn.Close()
		return nil, fmt.Errorf("custom metrics socket %s is in use by another process", sockPath)
	}
	// Remove any stale socket file
	_ = os.Remove(sockPath)

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("custom metrics socket: %w", err)
	}

	// Make the socket writable by owner + group
	if err := os.Chmod(sockPath, 0660); err != nil {
		log.Printf("[custom] warning: chmod socket: %v", err)
	}

	set, order := indexConfigs(configs)
	cc := &customCollector{
		latest:      make(map[string]customGroup),
		configSet:   set,
		configOrder: order,
		staleAfter:  staleAfter,
		sockPath:    sockPath,
		listener:    listener,
		debug:       debug,
	}

	go cc.acceptLoop(ctx)

	log.Printf("[custom] listening on %s (%d chart groups configured, stale after %s)", sockPath, len(configs), staleAfter)
	return cc, nil
}

func (cc *customCollector) debugf(format string, args ...any) {
	if cc.debug && !cc.debugDone.Load() {
		log.Printf(format, args...)
	}
}

// acceptLoop handles incoming connections.
func (cc *customCollector) acceptLoop(ctx context.Context) {
	for {
		conn, err := cc.listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				// Listener closed or transient error
				if errors.Is(err, net.ErrClosed) {
					return
				}
				log.Printf("[custom] accept error: %v", err)
				continue
			}
		}
		if cc.debug && !cc.debugDone.Load() {
			log.Printf("[custom] new connection from %v", conn.RemoteAddr())
		}
		go cc.handleConn(conn)
	}
}

// handleConn reads a single JSON message from a connection.
func (cc *customCollector) handleConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 64*1024) // 64KB max message

	for {
		// Reset the idle deadline before each read: a client pushing on the
		// collection interval keeps the connection alive, while one that
		// connects and goes silent is reaped after customConnIdleTimeout.
		_ = conn.SetReadDeadline(time.Now().Add(customConnIdleTimeout))
		if !scanner.Scan() {
			break
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var msg customMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			log.Printf("[custom] invalid JSON from %v: %v", conn.RemoteAddr(), err)
			continue
		}

		cc.debugf("[custom] received from %v: %s", conn.RemoteAddr(), string(line))

		if msg.Custom == nil {
			continue
		}

		cc.processMessage(msg.Custom)
	}

	// A clean disconnect (io.EOF) and the idle timeout are both expected and go
	// unreported; surface only genuine read failures such as an over-long line
	// (bufio.ErrTooLong), which would otherwise drop a payload silently.
	if err := scanner.Err(); err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
		log.Printf("[custom] read error from %v: %v", conn.RemoteAddr(), err)
	}
}

// processMessage converts incoming raw metrics into validated CustomMetricValues.
func (cc *customCollector) processMessage(data map[string][]map[string]float64) {
	cc.mu.Lock()
	defer cc.mu.Unlock()

	for group, entries := range data {
		// Only accept groups that are configured.
		nameSet, ok := cc.configSet[group]
		if !ok {
			continue
		}

		// Collapse to one value per configured name (last writer wins). A client
		// that repeats a name, or splits names across multiple objects, must not
		// produce duplicate series downstream — duplicates would, for instance,
		// make Prometheus reject the whole scrape. Map presence (not the value)
		// signals "seen", so a legitimate 0.0 is preserved.
		seen := make(map[string]float64, len(nameSet))
		for _, entry := range entries {
			for name, val := range entry {
				if _, configured := nameSet[name]; !configured {
					continue // skip unconfigured metrics
				}
				seen[name] = val
			}
		}

		if len(seen) == 0 {
			cc.debugf("[custom] discarded message for group %q: no configured metrics matched", group)
			continue
		}

		// Emit in config order for a deterministic, stable encoding.
		values := make([]CustomMetricValue, 0, len(seen))
		for _, name := range cc.configOrder[group] {
			if val, ok := seen[name]; ok {
				values = append(values, CustomMetricValue{Name: name, Value: val})
			}
		}
		cc.latest[group] = customGroup{values: values, seen: time.Now()}
		cc.debugDone.Store(true)
	}
}

// Latest returns the most recently received custom metrics.
func (cc *customCollector) Latest() map[string][]CustomMetricValue {
	cc.mu.RLock()
	defer cc.mu.RUnlock()

	if len(cc.latest) == 0 {
		return nil
	}

	// Return a copy to avoid races, dropping feeds whose producer has gone quiet
	// so a stopped feed leaves a gap on the chart instead of freezing at its last
	// value.
	now := time.Now()
	result := make(map[string][]CustomMetricValue, len(cc.latest))
	for k, g := range cc.latest {
		if now.Sub(g.seen) > cc.staleAfter {
			continue
		}
		cp := make([]CustomMetricValue, len(g.values))
		copy(cp, g.values)
		result[k] = cp
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// Close stops listening and removes the socket file.
func (cc *customCollector) Close() {
	if cc.listener != nil {
		_ = cc.listener.Close()
	}
	_ = os.Remove(cc.sockPath)
}
