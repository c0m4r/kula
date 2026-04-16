package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kula/internal/collector"
	"kula/internal/config"
	"kula/internal/storage"
)

// newTestStoreWithSample creates a temporary store, writes one sample, and
// returns the store. The caller is responsible for closing it.
func newTestStoreWithSample(t *testing.T) *storage.Store {
	t.Helper()
	dir := t.TempDir()
	cfg := config.StorageConfig{
		Directory: dir,
		Tiers: []config.TierConfig{
			{Resolution: time.Second, MaxSize: "10MB", MaxBytes: 10 * 1024 * 1024},
		},
	}
	store, err := storage.NewStore(cfg)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	ts := time.Now()
	sample := &collector.Sample{
		Timestamp: ts,
		CPU: collector.CPUStats{
			Total:    collector.CPUCoreStats{Usage: 12.5, User: 8.0, System: 3.0},
			NumCores: 4,
		},
		LoadAvg: collector.LoadAvg{Load1: 0.5, Load5: 0.4, Load15: 0.3, Running: 1, Total: 80},
		Memory: collector.MemoryStats{
			Total: 8 * 1024 * 1024 * 1024,
			Used:  2 * 1024 * 1024 * 1024,
			Free:  6 * 1024 * 1024 * 1024,
		},
		Swap: collector.SwapStats{
			Total: 2 * 1024 * 1024 * 1024,
			Used:  512 * 1024 * 1024,
			Free:  1536 * 1024 * 1024,
		},
		Network: collector.NetworkStats{
			Interfaces: []collector.NetInterface{{
				Name:   "eth0",
				RxMbps: 10.0,
				TxMbps: 2.5,
			}},
			TCP:     collector.TCPStats{CurrEstab: 42},
			Sockets: collector.SocketStats{TCPInUse: 20, UDPInUse: 5},
		},
		Disks: collector.DiskStats{
			Devices: []collector.DiskDevice{{
				Name:        "sda",
				ReadsPerSec: 50.0,
				Utilization: 15.0,
			}},
			FileSystems: []collector.FileSystemInfo{{
				Device:     "/dev/sda1",
				MountPoint: "/",
				FSType:     "ext4",
				Total:      100 * 1024 * 1024 * 1024,
				Used:       40 * 1024 * 1024 * 1024,
			}},
		},
		System: collector.SystemStats{
			Hostname:  "testhost",
			Uptime:    3600.0,
			Entropy:   3000,
			ClockSync: true,
		},
		Process: collector.ProcessStats{Total: 120, Running: 2},
		Self:    collector.SelfStats{CPUPercent: 0.5, MemRSS: 16 * 1024 * 1024, FDs: 20},
	}
	if err := store.WriteSample(sample); err != nil {
		t.Fatalf("WriteSample: %v", err)
	}
	return store
}

// newTestServer creates a minimal web.Server backed by a temp store.
func newTestServer(t *testing.T, store *storage.Store) *Server {
	t.Helper()
	cfg := config.WebConfig{}
	global := config.GlobalConfig{Hostname: "testhost"}
	c := &collector.Collector{}
	return NewServer(cfg, global, c, store, t.TempDir(), config.OllamaConfig{})
}

// TestHandleMetricsOK verifies that /metrics responds 200 with the correct
// Content-Type and includes expected metric names.
func TestHandleMetrics(t *testing.T) {
	store := newTestStoreWithSample(t)
	defer func() { _ = store.Close() }()

	srv := newTestServer(t, store)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	srv.handleMetrics(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}

	body := w.Body.String()

	// Check that key metric families are present.
	mustContain := []string{
		"kula_cpu_usage_percent",
		"kula_cpu_cores",
		"kula_memory_total_bytes",
		"kula_memory_used_bytes",
		"kula_swap_total_bytes",
		"kula_network_rx_mbps",
		"kula_tcp_established",
		"kula_disk_reads_per_second",
		"kula_filesystem_size_bytes",
		"kula_system_uptime_seconds",
		"kula_system_clock_synced",
		"kula_processes_total",
		"kula_self_cpu_percent",
		"kula_self_memory_rss_bytes",
	}
	for _, name := range mustContain {
		if !strings.Contains(body, name) {
			t.Errorf("metric %q not found in /metrics output", name)
		}
	}

	// host label should be the hostname.
	if !strings.Contains(body, `host="testhost"`) {
		t.Errorf("host label not found in /metrics output")
	}
}

// TestHandleMetricsMethodNotAllowed verifies that POST /metrics returns 405.
func TestHandleMetricsMethodNotAllowed(t *testing.T) {
	store := newTestStoreWithSample(t)
	defer func() { _ = store.Close() }()

	srv := newTestServer(t, store)

	req := httptest.NewRequest(http.MethodPost, "/metrics", nil)
	w := httptest.NewRecorder()
	srv.handleMetrics(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /metrics status = %d, want 405", w.Code)
	}
}

func TestHandleMetricsBearerTokenUnauthorized(t *testing.T) {
	store := newTestStoreWithSample(t)
	defer func() { _ = store.Close() }()

	srv := newTestServer(t, store)
	srv.cfg.PrometheusMetrics.Token = "secret-token"

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	srv.handleMetrics(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("GET /metrics without token status = %d, want 401", w.Code)
	}
	if got := w.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
		t.Errorf("WWW-Authenticate = %q, want Bearer challenge", got)
	}
}

func TestHandleMetricsBearerTokenAuthorized(t *testing.T) {
	store := newTestStoreWithSample(t)
	defer func() { _ = store.Close() }()

	srv := newTestServer(t, store)
	srv.cfg.PrometheusMetrics.Token = "secret-token"

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	w := httptest.NewRecorder()
	srv.handleMetrics(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /metrics with token status = %d, want 200", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, "kula_cpu_usage_percent") {
		t.Errorf("authorized /metrics body missing expected metric family")
	}
}

// TestHandleMetricsEmptyStore verifies that /metrics returns 200 with an
// empty body when no sample has been written yet.
func TestHandleMetricsEmptyStore(t *testing.T) {
	dir := t.TempDir()
	cfg := config.StorageConfig{
		Directory: dir,
		Tiers: []config.TierConfig{
			{Resolution: time.Second, MaxSize: "1MB", MaxBytes: 1024 * 1024},
		},
	}
	store, err := storage.NewStore(cfg)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	srv := newTestServer(t, store)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	srv.handleMetrics(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("empty store status = %d, want 200", w.Code)
	}
	// Body should be empty (no samples yet).
	if body := w.Body.String(); body != "" {
		t.Errorf("empty store /metrics body should be empty, got: %q", body[:min(len(body), 80)])
	}
}

// TestEscapeLabel verifies that the Prometheus label escaping handles
// special characters correctly.
func TestEscapeLabel(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{`hello`, `hello`},
		{`back\slash`, `back\\slash`},
		{`quo"te`, `quo\"te`},
		{"new\nline", `new\nline`},
		{`mix\and"and` + "\n", `mix\\and\"and\n`},
	}
	for _, tc := range cases {
		got := escapeLabel(tc.input)
		if got != tc.want {
			t.Errorf("escapeLabel(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
