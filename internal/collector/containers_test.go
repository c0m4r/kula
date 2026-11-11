package collector

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestContainerCollector(t *testing.T) {
	sockPath := "/tmp/container_test.sock"
	defer func() { _ = os.Remove(sockPath) }()

	// Make the inspect PID resolve to a Quadlet split-payload cgroup.
	origRoot, origProc := cgroupRoot, procPath
	cgroupRoot, procPath = t.TempDir(), t.TempDir()
	defer func() { cgroupRoot, procPath = origRoot, origProc }()

	const id = "c1234567890abcdef"
	cgroupRel := filepath.Join("system.slice", "test.service", "libpod-payload-"+id)
	cgroupDir := filepath.Join(cgroupRoot, cgroupRel)
	if err := os.MkdirAll(cgroupDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"memory.current": "104857600\n",
		"memory.max":     "209715200\n",
		"memory.stat":    "inactive_file 0\n",
	} {
		if err := os.WriteFile(filepath.Join(cgroupDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(procPath, "123"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(procPath, "123", "cgroup"),
		[]byte("0::/"+filepath.ToSlash(cgroupRel)+"\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	// Mock Docker/Podman socket server
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Failed to listen on unix socket: %v", err)
	}
	defer func() { _ = listener.Close() }()

	go func() {
		mux := http.NewServeMux()
		// List containers
		mux.HandleFunc("/containers/json", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `[{"Id":"%s","Names":["/test-container"],"State":"running"}]`, id)
		})
		// Container inspect (for PID)
		mux.HandleFunc("/containers/"+id+"/json", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"State":{"Pid":123}}`)
		})
		server := http.Server{Handler: mux}
		_ = server.Serve(listener)
	}()

	cfg := ContainersCollectorConfig{
		Enabled:    true,
		SocketPath: sockPath,
	}

	cc := newContainerCollector(cfg)
	// Mock mode to socket since resolveSocket might fail if stat doesn't work on /tmp socket immediately
	cc.socket = sockPath
	cc.mode = containerModeSocket

	// Perform collection with retries since the mock server is async
	var stats []ContainerStats
	for i := 0; i < 10; i++ {
		cc.collect()
		stats = cc.Latest()
		if len(stats) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if len(stats) == 0 {
		t.Fatal("No container stats collected")
	}

	s := stats[0]
	if s.Name != "test-container" {
		t.Errorf("Expected test-container, got %s", s.Name)
	}
	if !strings.HasPrefix(id, s.ID) {
		t.Errorf("Expected s.ID to be a prefix of the mock ID, got %s", s.ID)
	}
	if s.MemUsed != 104857600 {
		t.Errorf("MemUsed = %d, want 104857600", s.MemUsed)
	}
}

func TestReadContainerMemory(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Point host-meminfo lookup at a fixture so the fallback is deterministic.
	origProc := procPath
	procPath = filepath.Join(dir, "proc")
	if err := os.MkdirAll(procPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(procPath, "meminfo"), []byte("MemTotal: 4000000 kB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer func() { procPath = origProc }()
	const hostTotal = 4000000 * 1024

	t.Run("unlimited subtracts cache and falls back to host total", func(t *testing.T) {
		// memory.current includes a large reclaimable file cache.
		write("memory.current", "5599784960\n")
		write("memory.stat", "anon 565674000\ninactive_file 5034110976\nactive_file 64589824\n")
		write("memory.max", "max\n")

		used, limit := readContainerMemory(dir)
		if want := uint64(5599784960 - 5034110976); used != want {
			t.Errorf("used = %d, want %d", used, want)
		}
		if limit != hostTotal {
			t.Errorf("limit = %d, want host total %d", limit, hostTotal)
		}
	})

	t.Run("explicit limit is used directly", func(t *testing.T) {
		write("memory.current", "32145408\n")
		write("memory.stat", "inactive_file 1048576\n")
		write("memory.max", "104857600\n")

		used, limit := readContainerMemory(dir)
		if want := uint64(32145408 - 1048576); used != want {
			t.Errorf("used = %d, want %d", used, want)
		}
		if limit != 104857600 {
			t.Errorf("limit = %d, want 104857600", limit)
		}
	})
}

func TestContainerCollectorCgroupDetect(t *testing.T) {
	// Temporarily clear auto-detect paths to force cgroup fallback
	oldPaths := knownSocketPaths
	knownSocketPaths = func() []string { return nil }
	defer func() { knownSocketPaths = oldPaths }()

	cfg := ContainersCollectorConfig{
		Enabled:    true,
		SocketPath: "/nonexistent/socket",
	}
	cc := newContainerCollector(cfg)
	// On most systems /sys/fs/cgroup exists, so it should be modeCgroup or modeNone
	// We just want to ensure it doesn't crash and handles the nonexistent socket.
	if cc.mode == containerModeSocket {
		t.Error("Should not be in socket mode for nonexistent socket")
	}
}

func TestCgroupDirFromPidNormalizesContainerRoot(t *testing.T) {
	origRoot, origProc := cgroupRoot, procPath
	cgroupRoot, procPath = t.TempDir(), t.TempDir()
	defer func() { cgroupRoot, procPath = origRoot, origProc }()

	tests := []struct {
		name string
		pid  int
		id   string
		root string
		leaf string
	}{
		{
			name: "quadlet payload running systemd",
			pid:  100,
			id:   strings.Repeat("a", 64),
			root: filepath.Join("system.slice", "myapp.service", "libpod-payload-"+strings.Repeat("a", 64)),
			leaf: "init.scope",
		},
		{
			name: "rootless quadlet custom slice",
			pid:  101,
			id:   strings.Repeat("b", 64),
			root: filepath.Join(
				"user.slice", "user-1000.slice", "user@1000.service", "apps.slice",
				"myapp.service", "libpod-payload-"+strings.Repeat("b", 64),
			),
			leaf: filepath.Join("system.slice", "worker.service"),
		},
		{
			name: "docker payload running systemd",
			pid:  102,
			id:   strings.Repeat("c", 64),
			root: filepath.Join("system.slice", "docker-"+strings.Repeat("c", 64)+".scope"),
			leaf: "init.scope",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := filepath.Join(cgroupRoot, tt.root)
			if err := os.MkdirAll(filepath.Join(root, tt.leaf), 0o755); err != nil {
				t.Fatal(err)
			}
			pidDir := filepath.Join(procPath, fmt.Sprint(tt.pid))
			if err := os.MkdirAll(pidDir, 0o755); err != nil {
				t.Fatal(err)
			}
			content := "12:pids:/ignored-v1-path\n0::/" + filepath.ToSlash(filepath.Join(tt.root, tt.leaf)) + "\n"
			if err := os.WriteFile(filepath.Join(pidDir, "cgroup"), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}

			if got := cgroupDirFromPid(tt.pid, tt.id); got != root {
				t.Errorf("cgroupDirFromPid(%d) = %q, want %q", tt.pid, got, root)
			}
			if got := cgroupDirFromPid(tt.pid, strings.Repeat("d", 64)); got != "" {
				t.Errorf("cgroupDirFromPid with mismatched ID = %q, want empty", got)
			}
		})
	}

	if got := cgroupDirFromPid(99999, strings.Repeat("e", 64)); got != "" {
		t.Errorf("cgroupDirFromPid for missing process = %q, want empty", got)
	}
}

func TestCollectContainerMetricsFallsBackWithoutInspect(t *testing.T) {
	origRoot := cgroupRoot
	cgroupRoot = t.TempDir()
	defer func() { cgroupRoot = origRoot }()

	id := strings.Repeat("d", 64)
	dir := filepath.Join(cgroupRoot, "system.slice", "docker-"+id+".scope")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"memory.current": "67108864\n",
		"memory.max":     "134217728\n",
		"memory.stat":    "inactive_file 0\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cc := &containerCollector{
		prevCPU:  make(map[string]containerCPURaw),
		prevNet:  make(map[string]containerNetRaw),
		prevDisk: make(map[string]containerDiskRaw),
	}
	got := cc.collectContainerMetrics(id, "fallback-test", 1)
	if got.MemUsed != 67108864 || got.MemLimit != 134217728 {
		t.Errorf("fallback memory = %d/%d, want 67108864/134217728", got.MemUsed, got.MemLimit)
	}
}

// TestContainerRatesSkipAnUnresolvedCycle: a cycle that cannot find the
// container's cgroup reads no counters, so the next cycle has no baseline
// rather than one two intervals old (which read 200% for a container using
// one core). Without a PID the network counters go unread too.
func TestContainerRatesSkipAnUnresolvedCycle(t *testing.T) {
	origRoot := cgroupRoot
	cgroupRoot = t.TempDir()
	defer func() { cgroupRoot = origRoot }()

	id := strings.Repeat("e", 64)
	dir := filepath.Join(cgroupRoot, "system.slice", "docker-"+id+".scope")
	hidden := dir + ".hidden"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cc := &containerCollector{
		prevCPU:  make(map[string]containerCPURaw),
		prevNet:  make(map[string]containerNetRaw),
		prevDisk: make(map[string]containerDiskRaw),
	}
	cycle := func(usageUsec int, resolved bool) float64 {
		t.Helper()
		where := dir
		if !resolved {
			where = hidden
		}
		stat := fmt.Sprintf("usage_usec %d\n", usageUsec)
		if err := os.WriteFile(filepath.Join(where, "cpu.stat"), []byte(stat), 0o644); err != nil {
			t.Fatal(err)
		}
		return cc.collectContainerMetrics(id, "web", 1).CPUPct
	}
	cycle(1_000_000, true)
	if got := cycle(2_000_000, true); got != 100 {
		t.Fatalf("one core for one second: %v%%, want 100", got)
	}
	if err := os.Rename(dir, hidden); err != nil {
		t.Fatal(err)
	}
	cycle(3_000_000, false)
	if err := os.Rename(hidden, dir); err != nil {
		t.Fatal(err)
	}
	if got := cycle(4_000_000, true); got != 0 {
		t.Fatalf("first cycle after an unresolved one: %v%%, want 0 (no baseline)", got)
	}
	if got := cycle(5_000_000, true); got != 100 {
		t.Fatalf("second cycle after an unresolved one: %v%%, want 100", got)
	}

	// No runtime PID (inspect unavailable): no network baseline is kept.
	cc.prevNet[id] = containerNetRaw{}
	cycle(6_000_000, true)
	if _, ok := cc.prevNet[id]; ok {
		t.Fatal("a cycle without a PID kept a network baseline")
	}
}

func TestCollectViaCgroupsDiscoversQuadlets(t *testing.T) {
	origRoot := cgroupRoot
	cgroupRoot = t.TempDir()
	defer func() { cgroupRoot = origRoot }()

	rootfulID := strings.Repeat("a", 64)
	rootlessID := strings.Repeat("b", 64)
	scopeID := strings.Repeat("c", 64)
	for _, dir := range []string{
		filepath.Join(cgroupRoot, "system.slice", "rootful.service", "libpod-payload-"+rootfulID),
		filepath.Join(
			cgroupRoot, "user.slice", "user-1000.slice", "user@1000.service",
			"app.slice", "rootless.service", "libpod-payload-"+rootlessID,
		),
		filepath.Join(cgroupRoot, "system.slice", "libpod-"+scopeID+".scope"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cc := &containerCollector{
		prevCPU:  make(map[string]containerCPURaw),
		prevDisk: make(map[string]containerDiskRaw),
	}
	stats := cc.collectViaCgroups(1)

	ids := make(map[string]bool)
	for _, stat := range stats {
		ids[stat.ID] = true
	}
	for _, id := range []string{rootfulID[:12], rootlessID[:12], scopeID[:12]} {
		if !ids[id] {
			t.Errorf("container %s not discovered; got %+v", id, stats)
		}
	}
	if len(stats) != 3 {
		t.Errorf("got %d containers, want 3: %+v", len(stats), stats)
	}
}

// A socket proxy or shim can return IDs that are not the usual 64 hex
// characters; short or empty ones must not panic the collector goroutine.
func TestCollectViaSocketShortContainerID(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "short_id.sock")

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Failed to listen on unix socket: %v", err)
	}
	defer func() { _ = listener.Close() }()

	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/containers/json", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `[`+
				`{"Id":"abc","Names":[],"State":"running"},`+
				`{"Id":"","Names":[],"State":"running"}`+
				`]`)
		})
		server := http.Server{Handler: mux}
		_ = server.Serve(listener)
	}()

	cc := newContainerCollector(ContainersCollectorConfig{Enabled: true, SocketPath: sockPath})
	cc.socket = sockPath
	cc.mode = containerModeSocket

	var stats []ContainerStats
	for i := 0; i < 10; i++ {
		cc.collect() // must not panic
		stats = cc.Latest()
		if len(stats) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if len(stats) != 1 {
		t.Fatalf("got %d stats, want 1 (the empty ID must be skipped)", len(stats))
	}
	if stats[0].Name != "abc" {
		t.Errorf("Name = %q, want %q", stats[0].Name, "abc")
	}
}

// TestContainerSocketPathsSharedOrder pins the one candidate list the
// collector and the sandbox share: configured socket first, then Docker,
// rootful Podman and the rootless Podman sockets of the running user.
func TestContainerSocketPathsSharedOrder(t *testing.T) {
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	paths := ContainerSocketPaths("/srv/podman.sock")
	if paths[0] != "/srv/podman.sock" {
		t.Fatalf("configured socket is not tried first: %v", paths)
	}
	for _, want := range []string{
		"/var/run/docker.sock",
		"/run/podman/podman.sock",
		filepath.Join(runtimeDir, "podman", "podman.sock"),
		rootlessPodmanSocket(os.Getuid()),
		rootlessPodmanSocket(1000),
	} {
		if !slices.Contains(paths, want) {
			t.Errorf("candidates lack %s: %v", want, paths)
		}
	}
	seen := map[string]bool{}
	for _, path := range ContainerSocketPaths("/var/run/docker.sock") {
		if seen[path] {
			t.Fatalf("candidate %s listed twice", path)
		}
		seen[path] = true
	}
}

func TestSocketAccessErrorNamesRootlessOwner(t *testing.T) {
	var out strings.Builder
	log.SetOutput(&out)
	defer log.SetOutput(os.Stderr)

	logSocketAccessError(rootlessPodmanSocket(os.Getuid()+4242), os.ErrPermission)
	if !strings.Contains(out.String(), "another user's rootless Podman") {
		t.Errorf("rootless socket of another user: %q", out.String())
	}
	out.Reset()
	logSocketAccessError("/var/run/docker.sock", os.ErrPermission)
	if !strings.Contains(out.String(), "docker/podman group") {
		t.Errorf("rootful socket: %q", out.String())
	}
}

// A container's PID comes from inspect, one runtime round trip per container.
// It is kept while /proc/<pid>/cgroup still places that process in the
// container, and asked again once a restart moves the container elsewhere.
// Containers that leave the list take their rate baselines with them.
func TestContainerPIDsAreInspectedOncePerProcess(t *testing.T) {
	origRoot, origProc := cgroupRoot, procPath
	cgroupRoot, procPath = t.TempDir(), t.TempDir()
	defer func() { cgroupRoot, procPath = origRoot, origProc }()

	id := strings.Repeat("f", 64)
	if err := os.MkdirAll(filepath.Join(cgroupRoot, "system.slice", "docker-"+id+".scope"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cgroupRoot, "system.slice", "docker-"+id+".scope", "cpu.stat"), []byte("usage_usec 100\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setCgroup := func(pid int, path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(procPath, strconv.Itoa(pid)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(procPath, strconv.Itoa(pid), "cgroup"), []byte("0::"+path+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	setCgroup(4242, "/system.slice/docker-"+id+".scope")

	var inspects atomic.Int32
	var listed atomic.Bool
	listed.Store(true)
	pid := atomic.Int32{}
	pid.Store(4242)
	sockPath := filepath.Join(t.TempDir(), "docker.sock")
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/containers/json", func(w http.ResponseWriter, r *http.Request) {
		if listed.Load() {
			_, _ = fmt.Fprintf(w, `[{"Id":%q,"Names":["/web"],"State":"running"}]`, id)
		} else {
			_, _ = fmt.Fprint(w, `[]`)
		}
	})
	mux.HandleFunc("/containers/"+id+"/json", func(w http.ResponseWriter, r *http.Request) {
		inspects.Add(1)
		_, _ = fmt.Fprintf(w, `{"State":{"Pid":%d}}`, pid.Load())
	})
	server := http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	defer func() { _ = server.Close() }()

	cc := newContainerCollector(ContainersCollectorConfig{Enabled: true, SocketPath: sockPath})
	cc.socket = sockPath
	cc.mode = containerModeSocket
	for range 3 {
		cc.collect()
	}
	if n := inspects.Load(); n != 1 {
		t.Fatalf("3 cycles inspected the container %d times, want once", n)
	}
	if got := cc.Latest(); len(got) != 1 || got[0].Name != "web" {
		t.Fatalf("collected %+v", got)
	}

	// The container restarts: its old PID now belongs to another cgroup.
	setCgroup(4242, "/system.slice/other.service")
	setCgroup(5151, "/system.slice/docker-"+id+".scope")
	pid.Store(5151)
	cc.collect()
	cc.collect()
	if n := inspects.Load(); n != 2 || cc.pids[id] != 5151 {
		t.Fatalf("after a restart: %d inspects, cached PID %d; want 2 and 5151", n, cc.pids[id])
	}

	listed.Store(false)
	cc.collect()
	if len(cc.prevCPU)+len(cc.prevDisk)+len(cc.prevNet)+len(cc.pids) != 0 {
		t.Fatalf("a removed container left state behind: cpu %v disk %v net %v pids %v",
			cc.prevCPU, cc.prevDisk, cc.prevNet, cc.pids)
	}
}

// TestDecodeContainerListPastTheOldCap: a list far beyond the former 1 MiB
// cap (which truncated it and lost every container) decodes whole, and one
// past the current cap is refused by name rather than as broken JSON.
func TestDecodeContainerListPastTheOldCap(t *testing.T) {
	padding := strings.Repeat("x", 2500) // about the size of a real entry
	var b strings.Builder
	b.WriteString("[")
	const n = 2000
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"Id":"c%04d","Names":["/c%04d"],"State":"running","Labels":{"pad":%q}}`, i, i, padding)
	}
	b.WriteString("]")
	body := b.String()
	if len(body) <= 1<<20 {
		t.Fatalf("test list is only %d bytes", len(body))
	}

	containers, err := decodeContainerList(strings.NewReader(body))
	if err != nil || len(containers) != n || containers[n-1].ID != fmt.Sprintf("c%04d", n-1) {
		t.Fatalf("decoded %d containers, err %v", len(containers), err)
	}

	saved := maxContainerListBytes
	maxContainerListBytes = 1 << 20
	defer func() { maxContainerListBytes = saved }()
	if _, err := decodeContainerList(strings.NewReader(body)); err == nil || !strings.Contains(err.Error(), "larger than 1 MiB") {
		t.Fatalf("over-cap list: err = %v, want the cap named", err)
	}
	for body, want := range map[string]string{"null": "", "{}": "not an array", `[{"Id":"a"},`: "decoding the container list"} {
		got, err := decodeContainerList(strings.NewReader(body))
		if (want == "" && (err != nil || got != nil)) || (want != "" && (err == nil || !strings.Contains(err.Error(), want))) {
			t.Fatalf("decodeContainerList(%q) = %v, %v; want error %q", body, got, err, want)
		}
	}
	exact := `[{"Id":"a","Names":["/a"],"State":"running"}]`
	maxContainerListBytes = int64(len(exact))
	if got, err := decodeContainerList(strings.NewReader(exact)); err != nil || len(got) != 1 {
		t.Fatalf("list exactly at the cap: %v, %v", got, err)
	}
}
