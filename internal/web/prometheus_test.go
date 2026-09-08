package web

import (
	"math"
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
			Total:    collector.CPUCoreStats{Usage: 1.4199999570846558, User: 8.0, System: 3.0},
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
		PSU: []collector.PowerSupplyStats{{
			Name: "BAT0", Type: "Battery", Status: "Discharging",
			Capacity: 87, VoltageV: 12.3, CurrentA: 1.1, PowerW: 13.5,
			EnergyWhNow: 40.0, EnergyWhFull: 48.0,
		}},
		Apps: collector.ApplicationsStats{
			Nginx: &collector.NginxStats{
				ActiveConnections: 12, Accepts: 1000, Handled: 999, Requests: 5000,
				AcceptsPS: 1.5, HandledPS: 1.5, RequestsPS: 7.5,
				Reading: 1, Writing: 3, Waiting: 8,
			},
			Apache2: &collector.Apache2Stats{
				BusyWorkers: 4, IdleWorkers: 16, TotalAccesses: 2500, TotalKBytes: 8000,
				ReqPerSec: 2.5, BytesPerSec: 4096, BytesPerReq: 1638, CPULoad: 0.75,
				Uptime: 3600, Waiting: 14, Reading: 1, Sending: 3, Keepalive: 0,
				Starting: 0, DNS: 0, Closing: 1, Logging: 0, Graceful: 0,
				IdleCleanup: 0, OpenSlots: 80,
			},
			Containers: []collector.ContainerStats{{
				ID: "abc123", Name: "web",
				CPUPct: 12.0, MemUsed: 128 * 1024 * 1024, MemLimit: 512 * 1024 * 1024,
				MemPct: 25.0, NetRxBPS: 1024, NetTxBPS: 2048, DiskRBPS: 4096, DiskWBPS: 8192,
			}},
			Postgres: &collector.PostgresStats{
				ActiveConns: 5, IdleConns: 10, IdleInTxConns: 1, WaitingConns: 0, MaxConns: 100,
				TxCommitPS: 50.0, TxRollbackPS: 0.5,
				TupFetchedPS: 1000, TupReturnedPS: 5000, TupInsertedPS: 10,
				TupUpdatedPS: 5, TupDeletedPS: 1,
				BlksReadPS: 20, BlksHitPS: 980, BlksHitPct: 98.0,
				DeadlocksPS: 0,
				DeadTuples:  100, LiveTuples: 10000, AutovacuumCount: 3,
				BufCheckpointPS: 1, BufBackendPS: 2,
				DBSizeBytes: 100 * 1024 * 1024,
			},
			Mysql: &collector.MysqlStats{
				ThreadsConnected: 8, ThreadsRunning: 2, ThreadsCached: 4, MaxConnections: 150,
				QueriesPS: 100, ComSelectPS: 80, ComInsertPS: 10, ComUpdatePS: 5, ComDeletePS: 1,
				SlowQueriesPS:          0.1,
				InnodbBufferPoolHitPct: 99.5, InnodbBPReadsPS: 5,
				TableLocksWaitedPS: 0, RowLockWaitsPS: 0,
			},
			Custom: map[string][]collector.CustomMetricValue{
				"my_app": {{Name: "queue_depth", Value: 42}},
			},
		},
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
		"kula_psu_capacity_percent",
		"kula_nginx_active_connections",
		"kula_nginx_requests_total",
		"kula_apache2_busy_workers",
		"kula_apache2_scoreboard",
		"kula_container_cpu_percent",
		"kula_container_memory_used_bytes",
		"kula_postgres_connections_active",
		"kula_postgres_buffer_cache_hit_percent",
		"kula_mysql_threads_connected",
		"kula_mysql_queries_per_second",
		"kula_custom_metric",
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
	for line := range strings.SplitSeq(body, "\n") {
		if !strings.HasPrefix(line, "kula_") {
			continue
		}
		value := line[strings.LastIndexByte(line, ' ')+1:]
		if strings.ContainsAny(value, "eE") {
			t.Errorf("metric value uses exponent notation: %s", line)
		}
	}
	for _, want := range []string{
		`kula_cpu_usage_percent{host="testhost"} 1.42`,
		`kula_memory_total_bytes{host="testhost"} 8589934592`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rounded metric %q not found in /metrics output", want)
		}
	}
}

func TestFormatPrometheusValue(t *testing.T) {
	tests := []struct {
		name  string
		value float64
		want  string
	}{
		{name: "floating point noise", value: 1.4199999570846558, want: "1.42"},
		{name: "whole bytes", value: 29261729792, want: "29261729792"},
		{name: "ordinary decimal", value: 12.3456, want: "12.35"},
		{name: "small rate", value: 0.00123456, want: "0.00123"},
		{name: "very small rate", value: 0.0000001, want: "0.0000001"},
		{name: "negative zero", value: math.Copysign(0, -1), want: "0"},
		{name: "positive infinity", value: math.Inf(1), want: "+Inf"},
		{name: "negative infinity", value: math.Inf(-1), want: "-Inf"},
		{name: "not a number", value: math.NaN(), want: "NaN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatPrometheusValue(tt.value); got != tt.want {
				t.Errorf("formatPrometheusValue(%v) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}

func TestDiskMetricsFollowStableIdentity(t *testing.T) {
	store := newTestStoreWithSample(t)
	defer func() { _ = store.Close() }()
	srv := newTestServer(t, store)
	for _, name := range []string{"sda", "sdb"} {
		sample := &collector.Sample{Timestamp: time.Now(), System: collector.SystemStats{Hostname: "host"},
			Disks: collector.DiskStats{Devices: []collector.DiskDevice{
				{ID: "wwid:eui.0011223344556677", Name: name, ReadsPerSec: 42},
				{Name: "vda", ReadsPerSec: 3},
			}}}
		if err := store.WriteSample(sample); err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		srv.handleMetrics(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		body := w.Body.String()
		for _, want := range []string{
			`kula_disk_reads_per_second{host="host",device="wwid:eui.0011223344556677"} 42`,
			`kula_disk_info{host="host",device="wwid:eui.0011223344556677",kernel_name="` + name + `",identity_source="wwid"} 1`,
			`kula_disk_reads_per_second{host="host",device="kernel:vda"} 3`,
			`kula_disk_info{host="host",device="kernel:vda",kernel_name="vda",identity_source="kernel"} 1`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("missing metric %s", want)
			}
		}
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
