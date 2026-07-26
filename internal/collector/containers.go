package collector

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// containerDiscoveryMode describes how containers were discovered.
const (
	containerModeSocket = "socket"  // via Docker/Podman API socket
	containerModeCgroup = "cgroups" // fallback: cgroups-based discovery
	containerModeNone   = "none"    // no containers found / not available
)

// cgroupRoot is a variable so cgroup discovery and PID resolution can be
// exercised against temporary trees in tests.
var cgroupRoot = "/sys/fs/cgroup"

// containerCollector runs async container discovery and metric collection.
type containerCollector struct {
	mu        sync.RWMutex
	cfg       ContainersCollectorConfig
	latest    []ContainerStats
	mode      string // one of containerMode* constants
	prevCPU   map[string]containerCPURaw
	prevNet   map[string]containerNetRaw
	prevDisk  map[string]containerDiskRaw
	prevTime  time.Time
	client    *http.Client
	socket    string // resolved socket path
	debugDone bool   // set after first collection cycle
	lastCount int    // previous container count; log only on change
}

// ContainersCollectorConfig is the internal config needed by the container collector.
type ContainersCollectorConfig struct {
	Enabled    bool
	SocketPath string
	Containers []string
	DebugLog   bool
	Interval   time.Duration // collection interval, used for HTTP timeouts
}

type containerCPURaw struct {
	usageUsec uint64
}

type containerNetRaw struct {
	rxBytes uint64
	txBytes uint64
}

type containerDiskRaw struct {
	readBytes  uint64
	writeBytes uint64
}

// dockerContainer is a minimal representation from the Docker API.
type dockerContainer struct {
	ID    string   `json:"Id"`
	Names []string `json:"Names"`
	State string   `json:"State"`
}

// knownSocketPaths lists default socket paths to try in order.
var knownSocketPaths = []string{
	"/var/run/docker.sock",
	"/run/docker.sock",
	"/var/run/podman/podman.sock",
	"/run/podman/podman.sock",
	"/run/user/1000/podman/podman.sock", // rootless podman
}

func newContainerCollector(cfg ContainersCollectorConfig) *containerCollector {
	cc := &containerCollector{
		cfg:       cfg,
		prevCPU:   make(map[string]containerCPURaw),
		prevNet:   make(map[string]containerNetRaw),
		prevDisk:  make(map[string]containerDiskRaw),
		lastCount: -1, // force first-cycle log
	}
	cc.resolveSocket()
	return cc
}

// resolveSocket finds a usable container runtime socket.
func (cc *containerCollector) resolveSocket() {
	if cc.cfg.SocketPath != "" {
		// User-configured socket
		if _, err := os.Stat(cc.cfg.SocketPath); err == nil {
			if err := probeSocket(cc.cfg.SocketPath); err != nil {
				logSocketAccessError(cc.cfg.SocketPath, err)
			} else {
				cc.socket = cc.cfg.SocketPath
				cc.mode = containerModeSocket
				log.Printf("[containers] using configured socket: %s", cc.socket)
				return
			}
		}
		log.Printf("[containers] configured socket %s not found, falling back to auto-detect", cc.cfg.SocketPath)
	}

	// Auto-detect
	for _, path := range knownSocketPaths {
		if _, err := os.Stat(path); err == nil {
			if err := probeSocket(path); err != nil {
				logSocketAccessError(path, err)
				continue
			}
			cc.socket = path
			cc.mode = containerModeSocket
			log.Printf("[containers] discovered runtime socket: %s", cc.socket)
			return
		}
	}

	// Fallback to cgroups-based discovery (no name mapping)
	if _, err := os.Stat(cgroupRoot); err == nil {
		cc.mode = containerModeCgroup
		log.Printf("[containers] no runtime socket found, using cgroups-based discovery (container names unavailable)")
		return
	}

	cc.mode = containerModeNone
	log.Printf("[containers] no runtime socket or cgroups found, container monitoring disabled")
}

// probeSocket attempts a dial on the Unix socket to verify access.
func probeSocket(path string) error {
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

// logSocketAccessError logs a socket access failure with a hint when the error is
// permission-related.
func logSocketAccessError(path string, err error) {
	if errors.Is(err, os.ErrPermission) {
		log.Printf("[containers] socket %s found but permission denied — add the current user to the docker/podman group or run as root", path)
	} else {
		log.Printf("[containers] socket %s found but not accessible: %v", path, err)
	}
}

// debugf logs a formatted message only when DebugLog is enabled AND only once.
func (cc *containerCollector) debugf(format string, args ...any) {
	if cc.cfg.DebugLog && !cc.debugDone {
		log.Printf(format, args...)
	}
}

// initHTTPClient creates an HTTP client that dials over the Unix socket.
func (cc *containerCollector) initHTTPClient() {
	if cc.client != nil || cc.socket == "" {
		return
	}
	timeout := cc.cfg.Interval
	if timeout <= 0 {
		timeout = time.Second
	}
	cc.client = &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.DialTimeout("unix", cc.socket, timeout)
			},
		},
	}
}

// Start begins the async collection goroutine.
func (cc *containerCollector) Start(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		// Initial collection
		cc.collect()

		for {
			select {
			case <-ticker.C:
				cc.collect()
			case <-ctx.Done():
				return
			}
		}
	}()
}

// Latest returns the most recently collected container stats.
func (cc *containerCollector) Latest() []ContainerStats {
	cc.mu.RLock()
	defer cc.mu.RUnlock()
	return cc.latest
}

// collect performs a single collection cycle.
func (cc *containerCollector) collect() {
	now := time.Now()
	var elapsed float64
	if cc.prevTime.IsZero() {
		elapsed = 1
	} else {
		elapsed = now.Sub(cc.prevTime).Seconds()
		if elapsed <= 0 {
			elapsed = 1
		}
	}
	cc.prevTime = now

	var stats []ContainerStats

	switch cc.mode {
	case containerModeSocket:
		stats = cc.collectViaSocket(elapsed)
	case containerModeCgroup:
		stats = cc.collectViaCgroups(elapsed)
	default:
		return
	}

	cc.mu.Lock()
	cc.latest = stats
	cc.debugDone = true
	cc.mu.Unlock()
}

// collectViaSocket discovers containers via the Docker/Podman API.
func (cc *containerCollector) collectViaSocket(elapsed float64) []ContainerStats {
	cc.initHTTPClient()
	if cc.client == nil {
		return nil
	}

	resp, err := cc.client.Get("http://localhost/containers/json")
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB limit
	if err != nil {
		return nil
	}

	var containers []dockerContainer
	if err := json.Unmarshal(body, &containers); err != nil {
		return nil
	}

	// Log only on state transitions (count changes) to avoid per-cycle spam.
	if len(containers) != cc.lastCount {
		log.Printf("[containers] discovered %d containers via socket", len(containers))
		cc.lastCount = len(containers)
	}

	var stats []ContainerStats
	for _, c := range containers {
		if c.State != "running" {
			continue
		}

		name := c.ID[:12]
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}

		if !cc.matchFilter(c.ID, name) {
			continue
		}

		s := cc.collectContainerMetrics(c.ID, name, elapsed)
		stats = append(stats, s)
	}
	return stats
}

// quadletCgroupPatterns returns the common rootful, custom-slice, and rootless
// systemd layouts used by Podman containers running with cgroups=split.
func quadletCgroupPatterns(idPattern string) []string {
	payload := "libpod-payload-" + idPattern
	return []string{
		// Rootful/default: system.slice/<unit>.service/libpod-payload-<id>
		filepath.Join(cgroupRoot, "*.slice", "*.service", payload),
		// Rootful service placed in a nested custom slice.
		filepath.Join(cgroupRoot, "*.slice", "*.slice", "*.service", payload),
		// Rootless/default: user.slice/user-<uid>.slice/user@<uid>.service/
		// app.slice/<unit>.service/libpod-payload-<id>. The *.slice component
		// also permits a custom user slice.
		filepath.Join(cgroupRoot, "user.slice", "user-*.slice", "user@*.service", "*.slice", "*.service", payload),
	}
}

// collectViaCgroups enumerates container cgroup directories without API socket.
// Container names are not available in this mode — IDs are used instead.
func (cc *containerCollector) collectViaCgroups(elapsed float64) []ContainerStats {
	patterns := []string{
		filepath.Join(cgroupRoot, "system.slice", "docker-*.scope"),
		filepath.Join(cgroupRoot, "system.slice", "libpod-*.scope"),
		filepath.Join(cgroupRoot, "machine.slice", "libpod-*.scope"),
	}
	patterns = append(patterns, quadletCgroupPatterns("*")...)

	var stats []ContainerStats
	seen := make(map[string]bool)

	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, dir := range matches {
			base := filepath.Base(dir)
			// Extract the container ID from Docker/Podman scope names and
			// Podman's split-payload cgroup name.
			id := base
			for _, prefix := range []string{"docker-", "libpod-payload-", "libpod-"} {
				if strings.HasPrefix(id, prefix) {
					id = strings.TrimPrefix(id, prefix)
					id = strings.TrimSuffix(id, ".scope")
					break
				}
			}
			if seen[id] {
				continue
			}
			seen[id] = true

			shortID := id[:minInt(12, len(id))]
			cc.debugf("[containers] found cgroup: %s (id: %s)", base, id)

			if !cc.matchFilter(id, shortID) {
				continue
			}

			s := cc.collectContainerMetricsCgroup(dir, id, shortID, elapsed)
			stats = append(stats, s)
		}
	}
	return stats
}

// matchFilter checks if a container matches the configured filter.
func (cc *containerCollector) matchFilter(id, name string) bool {
	if len(cc.cfg.Containers) == 0 {
		return true // no filter = all containers
	}
	for _, filter := range cc.cfg.Containers {
		if filter == name || strings.HasPrefix(id, filter) {
			return true
		}
	}
	return false
}

// containerPid queries the runtime's Docker-compatible inspect endpoint for a
// container's host PID. It returns 0 when inspect is unavailable.
func (cc *containerCollector) containerPid(id string) int {
	if cc.client == nil {
		return 0
	}

	resp, err := cc.client.Get(fmt.Sprintf("http://localhost/containers/%s/json", id))
	if err != nil {
		return 0
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return 0
	}

	var info struct {
		State struct {
			Pid int `json:"Pid"`
		} `json:"State"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info); err != nil {
		return 0
	}
	return info.State.Pid
}

// containerCgroupPath trims a process cgroup path back to the container root.
// A payload running systemd may move PID 1 into a child such as init.scope;
// reading metrics there would omit processes in sibling cgroups.
func containerCgroupPath(path, id string) string {
	if id == "" || !strings.HasPrefix(path, "/") {
		return ""
	}

	clean := filepath.Clean(path)
	parts := strings.Split(strings.TrimPrefix(clean, string(filepath.Separator)), string(filepath.Separator))
	for i, part := range parts {
		if part == id ||
			part == "docker-"+id+".scope" ||
			part == "libpod-"+id+".scope" ||
			part == "libpod-"+id ||
			part == "libpod-payload-"+id {
			return string(filepath.Separator) + filepath.Join(parts[:i+1]...)
		}
	}
	return ""
}

// cgroupDirFromPid resolves a process's cgroup v2 membership and normalizes it
// to the matching container root. This supports arbitrary Docker/Podman cgroup
// parents and Podman Quadlets, including rootless and custom-slice layouts.
func cgroupDirFromPid(pid int, id string) string {
	if pid <= 0 {
		return ""
	}

	data, err := os.ReadFile(filepath.Join(procPath, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return ""
	}

	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.SplitN(line, ":", 3)
		if len(fields) != 3 || fields[0] != "0" || fields[1] != "" {
			continue
		}

		path := containerCgroupPath(fields[2], id)
		if path == "" {
			continue
		}
		dir := filepath.Join(cgroupRoot, strings.TrimPrefix(path, string(filepath.Separator)))
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir
		}
	}
	return ""
}

// collectContainerMetrics gathers metrics for a container discovered via socket.
// It resolves the cgroup via the container PID, then falls back to conventional
// cgroup paths so an inspect failure does not disable CPU/memory/disk metrics.
func (cc *containerCollector) collectContainerMetrics(id, name string, elapsed float64) ContainerStats {
	s := ContainerStats{
		ID:   id[:minInt(12, len(id))],
		Name: name,
	}

	pid := cc.containerPid(id)
	cgroupDir := cgroupDirFromPid(pid, id)
	if cgroupDir == "" {
		cgroupDir = cc.findCgroupDir(id)
	}
	cc.debugf("[containers] id=%s name=%s pid=%d cgroupDir=%q", id, name, pid, cgroupDir)
	if cgroupDir == "" {
		return s
	}

	// CPU usage from cpu.stat
	s.CPUPct = cc.readCPUUsage(cgroupDir, id, elapsed)

	// Memory, matching `docker stats` (usage excludes file cache).
	s.MemUsed, s.MemLimit = readContainerMemory(cgroupDir)
	if s.MemLimit > 0 {
		s.MemPct = round2(float64(s.MemUsed) / float64(s.MemLimit) * 100)
	}

	// Network I/O requires the runtime PID; the other metrics remain available
	// through direct cgroup fallback when inspect is unavailable.
	if pid > 0 {
		s.NetRxBPS, s.NetTxBPS = cc.readNetIO(pid, id, elapsed)
	}

	// Disk I/O from io.stat
	s.DiskRBPS, s.DiskWBPS = cc.readDiskIO(cgroupDir, id, elapsed)

	return s
}

// collectContainerMetricsCgroup gathers metrics using a known cgroup directory path.
func (cc *containerCollector) collectContainerMetricsCgroup(cgroupDir, id, shortID string, elapsed float64) ContainerStats {
	s := ContainerStats{
		ID:   shortID,
		Name: shortID, // no name available in cgroups-only mode
	}

	s.CPUPct = cc.readCPUUsage(cgroupDir, id, elapsed)
	s.MemUsed, s.MemLimit = readContainerMemory(cgroupDir)
	if s.MemLimit > 0 {
		s.MemPct = round2(float64(s.MemUsed) / float64(s.MemLimit) * 100)
	}
	s.DiskRBPS, s.DiskWBPS = cc.readDiskIO(cgroupDir, id, elapsed)

	return s
}

// findCgroupDir locates the cgroup v2 directory for a container ID.
func (cc *containerCollector) findCgroupDir(id string) string {
	candidates := []string{
		filepath.Join(cgroupRoot, "system.slice", "docker-"+id+".scope"),
		filepath.Join(cgroupRoot, "system.slice", "libpod-"+id+".scope"),
		filepath.Join(cgroupRoot, "machine.slice", "libpod-"+id+".scope"),
		filepath.Join(cgroupRoot, "docker", id),
		filepath.Join(cgroupRoot, "libpod-"+id),
	}
	for _, path := range candidates {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			return path
		}
	}

	for _, pattern := range quadletCgroupPatterns(id) {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, path := range matches {
			if info, err := os.Stat(path); err == nil && info.IsDir() {
				return path
			}
		}
	}
	return ""
}

// readCPUUsage reads cpu.stat and computes CPU usage percentage.
func (cc *containerCollector) readCPUUsage(cgroupDir, id string, elapsed float64) float64 {
	data, err := os.ReadFile(filepath.Join(cgroupDir, "cpu.stat"))
	if err != nil {
		return 0
	}
	var usageUsec uint64
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "usage_usec ") {
			usageUsec, _ = strconv.ParseUint(strings.TrimPrefix(line, "usage_usec "), 10, 64)
			break
		}
	}

	cur := containerCPURaw{usageUsec: usageUsec}
	var cpuPct float64
	if prev, ok := cc.prevCPU[id]; ok && elapsed > 0 && cur.usageUsec >= prev.usageUsec {
		deltaUsec := cur.usageUsec - prev.usageUsec
		// Convert microseconds delta to percentage (100% = 1 full core)
		cpuPct = round2(float64(deltaUsec) / (elapsed * 1_000_000) * 100)
	}
	cc.prevCPU[id] = cur
	return cpuPct
}

// readNetIO reads network I/O for a container from /proc/<pid>/net/dev.
func (cc *containerCollector) readNetIO(pid int, id string, elapsed float64) (rxBPS, txBPS float64) {
	netDevPath := filepath.Join(procPath, strconv.Itoa(pid), "net/dev")
	f, err := os.Open(netDevPath)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()

	var totalRx, totalTx uint64
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		if lineNum <= 2 {
			continue
		}
		line := scanner.Text()
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		if name == "lo" {
			continue
		}
		fields := strings.Fields(parts[1])
		if len(fields) < 10 {
			continue
		}
		rx, _ := strconv.ParseUint(fields[0], 10, 64)
		tx, _ := strconv.ParseUint(fields[8], 10, 64)
		totalRx += rx
		totalTx += tx
	}

	cur := containerNetRaw{rxBytes: totalRx, txBytes: totalTx}
	if prev, ok := cc.prevNet[id]; ok && elapsed > 0 {
		if cur.rxBytes >= prev.rxBytes {
			rxBPS = round2(float64(cur.rxBytes-prev.rxBytes) / elapsed)
		}
		if cur.txBytes >= prev.txBytes {
			txBPS = round2(float64(cur.txBytes-prev.txBytes) / elapsed)
		}
	}
	cc.prevNet[id] = cur
	return
}

// readDiskIO reads io.stat from the container's cgroup.
func (cc *containerCollector) readDiskIO(cgroupDir, id string, elapsed float64) (rBPS, wBPS float64) {
	data, err := os.ReadFile(filepath.Join(cgroupDir, "io.stat"))
	if err != nil {
		return
	}

	var totalRead, totalWrite uint64
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		for _, f := range fields {
			if strings.HasPrefix(f, "rbytes=") {
				v, _ := strconv.ParseUint(strings.TrimPrefix(f, "rbytes="), 10, 64)
				totalRead += v
			} else if strings.HasPrefix(f, "wbytes=") {
				v, _ := strconv.ParseUint(strings.TrimPrefix(f, "wbytes="), 10, 64)
				totalWrite += v
			}
		}
	}

	cur := containerDiskRaw{readBytes: totalRead, writeBytes: totalWrite}
	if prev, ok := cc.prevDisk[id]; ok && elapsed > 0 {
		if cur.readBytes >= prev.readBytes {
			rBPS = round2(float64(cur.readBytes-prev.readBytes) / elapsed)
		}
		if cur.writeBytes >= prev.writeBytes {
			wBPS = round2(float64(cur.writeBytes-prev.writeBytes) / elapsed)
		}
	}
	cc.prevDisk[id] = cur
	return
}

// readContainerMemory reads memory usage and limit from a cgroup v2 directory,
// matching the values reported by `docker stats`:
//   - usage is memory.current minus the inactive file cache (inactive_file),
//     since the page cache is reclaimable and not counted as container usage.
//   - when no limit is configured (memory.max is "max"), the host's total
//     memory is reported as the limit, as Docker does.
func readContainerMemory(cgroupDir string) (used, limit uint64) {
	current := readUint64File(filepath.Join(cgroupDir, "memory.current"))
	inactiveFile := readMemStatField(filepath.Join(cgroupDir, "memory.stat"), "inactive_file")
	if current > inactiveFile {
		used = current - inactiveFile
	}

	memMax := readUint64File(filepath.Join(cgroupDir, "memory.max"))
	if memMax > 0 && memMax < 1<<62 { // explicit limit set (excludes "max" sentinel)
		limit = memMax
	} else {
		// No limit configured — report host total memory like `docker stats`.
		limit = parseMemInfo().memTotal
	}
	return used, limit
}

// readMemStatField reads a single numeric field from a cgroup memory.stat file.
func readMemStatField(path, field string) uint64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	prefix := field + " "
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, prefix) {
			v, _ := strconv.ParseUint(strings.TrimPrefix(line, prefix), 10, 64)
			return v
		}
	}
	return 0
}

// readUint64File reads a file and parses its content as uint64.
func readUint64File(path string) uint64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	s := strings.TrimSpace(string(data))
	if s == "max" {
		return 0 // cgroups "max" means no limit
	}
	v, _ := strconv.ParseUint(s, 10, 64)
	return v
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
