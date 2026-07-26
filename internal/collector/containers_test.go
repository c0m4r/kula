package collector

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
	knownSocketPaths = nil
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
