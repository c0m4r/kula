package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMain runs the tests with HOME at an empty directory of their own, and
// fails if one of them wrote there. A config without a storage directory
// falls back from /var/lib/kula to ~/.kula and creates it, so a test that
// loads one must give it a directory of its own (see ownStorage).
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "kula-config-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.Setenv("HOME", home); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	entries, err := os.ReadDir(home)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	if len(entries) > 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		fmt.Fprintf(os.Stderr, "FAIL: the tests wrote %q into HOME: a test that loads a config "+
			"without a storage directory must give it one of its own (ownStorage)\n", names)
		code = 1
	}
	_ = os.RemoveAll(home)
	os.Exit(code)
}

// ownStorage gives a test that loads a config without a storage directory one
// of its own, through KULA_DIRECTORY: the default, /var/lib/kula, falls back
// to ~/.kula, creating it and writing a probe file there.
func ownStorage(t *testing.T) {
	t.Helper()
	t.Setenv("KULA_DIRECTORY", t.TempDir())
}

func TestParseSize(t *testing.T) {
	tests := []struct {
		input    string
		expected int64
		wantErr  bool
	}{
		{"100B", 100, false},
		{"1KB", 1024, false},
		{"100MB", 100 * 1024 * 1024, false},
		{"1GB", 1024 * 1024 * 1024, false},
		{"2.5MB", int64(2.5 * 1024 * 1024), false},
		{"", 0, true},
		{"100XB", 0, true},
		{"abc", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseSize(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseSize(%q) expected error, got nil", tt.input)
				}
				return
			}
			if err != nil {
				t.Errorf("parseSize(%q) unexpected error: %v", tt.input, err)
				return
			}
			if got != tt.expected {
				t.Errorf("parseSize(%q) = %d, want %d", tt.input, got, tt.expected)
			}
		})
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg == nil {
		t.Fatal("DefaultConfig() returned nil")
	}
	if cfg.Collection.Interval != time.Second {
		t.Errorf("Collection.Interval = %v, want 1s", cfg.Collection.Interval)
	}
	if cfg.Web.Port != 27960 {
		t.Errorf("Web.Port = %d, want 27960", cfg.Web.Port)
	}
	if !cfg.Web.Enabled {
		t.Error("Web.Enabled should be true by default")
	}
	if !cfg.Web.UI {
		t.Error("Web.UI should be true by default")
	}
	if cfg.Web.PrometheusMetrics.Enabled {
		t.Error("Web.PrometheusMetrics.Enabled should be false by default")
	}
	if cfg.Web.DefaultAggregation != "avg" {
		t.Errorf("Web.DefaultAggregation = %q, want avg", cfg.Web.DefaultAggregation)
	}
	if cfg.Web.Auth.Enabled {
		t.Error("Web.Auth.Enabled should be false by default")
	}
	if len(cfg.Storage.Tiers) != 3 {
		t.Errorf("Storage.Tiers count = %d, want 3", len(cfg.Storage.Tiers))
	}
	if cfg.TUI.RefreshRate != time.Second {
		t.Errorf("TUI.RefreshRate = %v, want 1s", cfg.TUI.RefreshRate)
	}
	if !cfg.Web.Appearance.StickyTopbar {
		t.Error("Web.Appearance.StickyTopbar should be true by default")
	}
	if !cfg.Web.Appearance.Gauges {
		t.Error("Web.Appearance.Gauges should be true by default")
	}
	if cfg.Web.Accessibility != (AccessibilityConfig{TextSize: DefaultTextSize}) {
		t.Errorf("Web.Accessibility = %+v, want every option off and text size %d by default",
			cfg.Web.Accessibility, DefaultTextSize)
	}
	if cfg.Global.ShowSystemDetails {
		t.Error("Global.ShowSystemDetails should be false by default")
	}
}

// Host details stay opt-in, so a config file that predates the option keeps
// them off, and only an explicit true turns them on.
func TestLoadShowSystemDetails(t *testing.T) {
	ownStorage(t)
	for _, test := range []struct {
		name    string
		yaml    string
		details bool
	}{
		{"absent", "global:\n  show_system_info: true\n", false},
		{"explicit", "global:\n  show_system_info: true\n  show_system_details: true\n", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(test.yaml), 0644); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load() error: %v", err)
			}
			if !cfg.Global.ShowSystemInfo {
				t.Error("show_system_info was not read")
			}
			if cfg.Global.ShowSystemDetails != test.details {
				t.Errorf("ShowSystemDetails = %v, want %v", cfg.Global.ShowSystemDetails, test.details)
			}
		})
	}
}

// The appearance settings default to true, so an explicit `false` in the
// config file only survives because Load unmarshals over DefaultConfig().
func TestLoadAppearanceAndAccessibility(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	yaml := `
storage:
  directory: ` + dir + `
web:
  appearance:
    sticky_topbar: false
    gauges: false
  accessibility:
    high_contrast: true
    text_size: 120
`
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Web.Appearance.StickyTopbar {
		t.Error("Web.Appearance.StickyTopbar = true, want false")
	}
	if cfg.Web.Appearance.Gauges {
		t.Error("Web.Appearance.Gauges = true, want false")
	}
	if !cfg.Web.Accessibility.HighContrast {
		t.Error("Web.Accessibility.HighContrast = false, want true")
	}
	if cfg.Web.Accessibility.TextSize != 120 {
		t.Errorf("Web.Accessibility.TextSize = %d, want 120", cfg.Web.Accessibility.TextSize)
	}
	// Options absent from the file keep their (false) defaults.
	if cfg.Web.Accessibility.ReduceMotion || cfg.Web.Accessibility.UnderlineLinks || cfg.Web.Accessibility.FocusOutline {
		t.Errorf("unset accessibility options changed: %+v", cfg.Web.Accessibility)
	}
}

func TestLoadTextSizeOutOfRange(t *testing.T) {
	for _, size := range []int{0, MinTextSize - 1, MaxTextSize + 1} {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		yaml := fmt.Sprintf("storage:\n  directory: %s\nweb:\n  accessibility:\n    text_size: %d\n", dir, size)
		if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Errorf("Load() with text_size %d should return an error", size)
		}
	}
}

// Values that load but break at runtime are refused: Argon2id panics on zero
// time or threads (hash-password uses it with auth off), a zero session
// timeout expires every session at once, and a zero WebSocket limit refuses
// every live connection. The last two are left alone while auth, or the web
// server, is off: a config that never used them loads as before.
func TestLoadRejectsBrokenWebLimits(t *testing.T) {
	load := func(t *testing.T, web string) error {
		t.Helper()
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		yaml := fmt.Sprintf("storage:\n  directory: %s\nweb:\n%s", dir, web)
		if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
			t.Fatal(err)
		}
		_, err := Load(path)
		return err
	}
	for _, tc := range []struct{ name, yaml, key string }{
		{"argon2 time", "  auth:\n    argon2:\n      time: 0\n", "argon2.time"},
		{"argon2 threads with auth off", "  enabled: false\n  auth:\n    argon2:\n      threads: 0\n", "argon2.threads"},
		{"session timeout", "  auth:\n    enabled: true\n    session_timeout: 0s\n", "session_timeout"},
		{"negative session timeout", "  auth:\n    enabled: true\n    session_timeout: -1h\n", "session_timeout"},
		{"websocket conns", "  max_websocket_conns: 0\n", "max_websocket_conns must"},
		{"websocket conns per ip", "  max_websocket_conns_per_ip: -1\n", "max_websocket_conns_per_ip"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := load(t, tc.yaml); err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Errorf("Load() of\n%s returned %v, want a refusal naming %s", tc.yaml, err, tc.key)
			}
		})
	}
	for _, tc := range []struct{ name, yaml string }{
		{"session timeout with auth off", "  auth:\n    session_timeout: 0s\n"},
		{"websocket limits with the web server off", "  enabled: false\n  max_websocket_conns: 0\n  max_websocket_conns_per_ip: 0\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := load(t, tc.yaml); err != nil {
				t.Errorf("Load() refused a setting that is not in use:\n%s\n%v", tc.yaml, err)
			}
		})
	}
	if err := DefaultConfig().validateWebLimits(); err != nil {
		t.Errorf("defaults rejected: %v", err)
	}
}

// TestLoadRefusesUnknownSettings: a key the schema lacks stops startup with
// its line, instead of being ignored. Ignored, a misspelled setting fails open:
// `web.auth.enable: true` started Kula with authentication off.
func TestLoadRefusesUnknownSettings(t *testing.T) {
	ownStorage(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	typo := fmt.Sprintf("storage:\n  directory: %s\nweb:\n  auth:\n    enable: true\n", dir)
	if err := os.WriteFile(path, []byte(typo), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), `line 5: unknown setting "enable"`) {
		t.Fatalf("Load() of a misspelled setting returned %v, want a refusal naming it and its line", err)
	}

	// An empty file is no config at all: every setting at its default.
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("Load() of an empty file: %v", err)
	}
}

// The example Kula ships, and seeds a missing -config from, must load as it
// is: every key in it is one the schema knows.
func TestExampleConfigLoadsStrictly(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := decodeStrict(data, DefaultConfig()); err != nil {
		t.Fatalf("config.example.yaml: %v", err)
	}
}

// Disk temperatures and power supplies are read every 10 s when the config
// does not say, or every sample when the collection interval is longer; any
// other value must be a multiple of the collection interval.
func TestLoadReadingIntervals(t *testing.T) {
	load := func(t *testing.T, interval, settings string) (*Config, error) {
		t.Helper()
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		yaml := fmt.Sprintf("storage:\n  directory: %s\n  tiers:\n    - resolution: %s\n      max_size: 10MB\n"+
			"collection:\n  interval: %s\n%s", dir, interval, interval, settings)
		if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
			t.Fatal(err)
		}
		return Load(path)
	}
	for _, tc := range []struct {
		name, interval, settings string
		disk, psu                time.Duration
	}{
		{"defaults", "1s", "", 10 * time.Second, 10 * time.Second},
		{"defaults at 15s", "15s", "", 15 * time.Second, 15 * time.Second},
		{"every sample", "1s", "  disk_temp_interval: 1s\n  power_supply_interval: 1s\n", time.Second, time.Second},
		{"longer", "5s", "  disk_temp_interval: 1m\n", time.Minute, 10 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := load(t, tc.interval, tc.settings)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Collection.DiskTempInterval != tc.disk || cfg.Collection.PowerSupplyInterval != tc.psu {
				t.Fatalf("disk_temp_interval %v, power_supply_interval %v; want %v and %v",
					cfg.Collection.DiskTempInterval, cfg.Collection.PowerSupplyInterval, tc.disk, tc.psu)
			}
		})
	}
	for _, tc := range []struct{ name, interval, settings string }{
		{"below the interval", "1s", "  disk_temp_interval: 500ms\n"},
		{"not a multiple", "2s", "  power_supply_interval: 5s\n"},
		{"negative", "1s", "  disk_temp_interval: -10s\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := load(t, tc.interval, tc.settings); err == nil || !strings.Contains(err.Error(), "must be a multiple of collection.interval") {
				t.Fatalf("Load() = %v, want a refusal for:\n%s", err, tc.settings)
			}
		})
	}
}

func TestNormalizeOrigin(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://app.example.com", "https://app.example.com"},
		{" HTTPS://App.Example.COM/ ", "https://app.example.com"},
		{"https://app.example.com:443", "https://app.example.com"},
		{"http://app.example.com:80/", "http://app.example.com"},
		{"https://app.example.com:8443", "https://app.example.com:8443"},
		{"http://app.example.com:443", "http://app.example.com:443"},
		{"http://[::1]:8080", "http://[::1]:8080"},
		{"http://[::1]:80", "http://[::1]"},
		{"http://192.0.2.1", "http://192.0.2.1"},
		// App webviews and extensions send their own schemes, as 0.21.0 accepted.
		{"tauri://localhost", "tauri://localhost"},
		{"Chrome-Extension://ABCDEFGHIJKLMNOPABCDEFGHIJKLMNOP/", "chrome-extension://abcdefghijklmnopabcdefghijklmnop"},
		{"moz-extension://0b5c3a8e-6f3d-4e8a-9a51-2f6c1d7e8b90", "moz-extension://0b5c3a8e-6f3d-4e8a-9a51-2f6c1d7e8b90"},
	} {
		got, err := normalizeOrigin(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("normalizeOrigin(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{
		"", "app.example.com", "*", "null", "https://*.example.com", "ftp://app.example.com",
		"https://app.example.com/path", "https://app.example.com/?x=1", "https://app.example.com/#x",
		"https://user@app.example.com", "https:app.example.com", "https://bücher.example",
		"ws://app.example.com", "wss://app.example.com", "file:///srv/kula", "app.example.com:8443", "app://",
		"tauri://localhost/index.html", "chrome-extension://*",
	} {
		if got, err := normalizeOrigin(in); err == nil {
			t.Errorf("normalizeOrigin(%q) = %q, want an error", in, got)
		}
	}
}

func TestLoadNormalizesAllowedOrigins(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := fmt.Sprintf("storage:\n  directory: %s\nweb:\n  security:\n    allowed_origins:\n      - https://App.Example.com:443/\n", dir)
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Web.Security.AllowedOrigins; len(got) != 1 || got[0] != "https://app.example.com" {
		t.Errorf("AllowedOrigins = %q, want [https://app.example.com]", got)
	}

	yaml = fmt.Sprintf("storage:\n  directory: %s\nweb:\n  security:\n    allowed_origins:\n      - app.example.com\n", dir)
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("Load() accepted an allowed origin without a scheme")
	}
}

func TestNormalizeAllowedHost(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"kula.example.com", "kula.example.com"},
		{" Kula.Example.COM. ", "kula.example.com"},
		{"kula.example.com:8443", "kula.example.com"},
		{"[::1]:27960", "::1"},
		{"[fe80::1]", "fe80::1"},
		{"192.0.2.1", "192.0.2.1"},
		{"*", "*"},
	} {
		got, err := normalizeAllowedHost(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("normalizeAllowedHost(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{
		"", " ", ".", "https://kula.example.com", "kula.example.com/", "kula.example.com/path",
		"user@kula.example.com", "*.example.com", "kula*", "bücher.example",
	} {
		if got, err := normalizeAllowedHost(in); err == nil {
			t.Errorf("normalizeAllowedHost(%q) = %q, want an error", in, got)
		}
	}
}

func TestLoadNormalizesAllowedHosts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := fmt.Sprintf("storage:\n  directory: %s\nweb:\n  security:\n    allowed_hosts:\n      - Kula.Example.com:443\n", dir)
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Web.Security.AllowedHosts; len(got) != 1 || got[0] != "kula.example.com" {
		t.Errorf("AllowedHosts = %q, want [kula.example.com]", got)
	}

	yaml = fmt.Sprintf("storage:\n  directory: %s\nweb:\n  security:\n    allowed_hosts:\n      - https://kula.example.com\n", dir)
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("Load() accepted an allowed host with a scheme")
	}
}

func TestLoadMissingFile(t *testing.T) {
	ownStorage(t)
	cfg, err := Load("/nonexistent/path/config.yaml")
	if err != nil {
		t.Fatalf("Load() with missing file should return defaults, got error: %v", err)
	}
	if cfg == nil {
		t.Fatal("Load() returned nil config")
	}
	if cfg.Web.Port != 27960 {
		t.Errorf("Web.Port = %d, want 27960 (default)", cfg.Web.Port)
	}
}

func TestLoadRequiredMissingFile(t *testing.T) {
	if _, err := LoadRequired("/nonexistent/path/config.yaml"); err == nil {
		t.Fatal("LoadRequired() with missing file should return an error, got nil")
	}
}

func TestLoadRequiredUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses file permission checks")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("web:\n  port: 9090\n"), 0000); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRequired(path); err == nil {
		t.Fatal("LoadRequired() with unreadable file should return an error, got nil")
	}
}

func TestLoadRequiredPresentFile(t *testing.T) {
	ownStorage(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("web:\n  port: 9090\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadRequired(path)
	if err != nil {
		t.Fatalf("LoadRequired() error: %v", err)
	}
	if cfg.Web.Port != 9090 {
		t.Errorf("Web.Port = %d, want 9090", cfg.Web.Port)
	}
}

func TestLoadValidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	yaml := `
collection:
  interval: 5s
web:
  enabled: true
  listen: "127.0.0.1"
  port: 9090
storage:
  directory: /tmp/kula-test
  tiers:
    - resolution: 5s
      max_size: 50MB
tui:
  refresh_rate: 2s
`
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Collection.Interval != 5*time.Second {
		t.Errorf("Collection.Interval = %v, want 5s", cfg.Collection.Interval)
	}
	if cfg.Web.Port != 9090 {
		t.Errorf("Web.Port = %d, want 9090", cfg.Web.Port)
	}
	if cfg.Web.Listen != "127.0.0.1" {
		t.Errorf("Web.Listen = %q, want 127.0.0.1", cfg.Web.Listen)
	}
	if len(cfg.Storage.Tiers) != 1 {
		t.Fatalf("Storage.Tiers count = %d, want 1", len(cfg.Storage.Tiers))
	}
	if cfg.Storage.Tiers[0].MaxBytes != 50*1024*1024 {
		t.Errorf("Tier 0 MaxBytes = %d, want %d", cfg.Storage.Tiers[0].MaxBytes, 50*1024*1024)
	}
}

func TestLoadInvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")

	if err := os.WriteFile(path, []byte("{{invalid yaml"), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil {
		t.Error("Load() with invalid YAML should return error")
	}
}

func TestLoadEnvOverrides(t *testing.T) {
	ownStorage(t)
	t.Setenv("KULA_LISTEN", "10.0.0.1")
	t.Setenv("KULA_PORT", "1234")

	cfg, err := Load("/nonexistent/path/config.yaml")
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Web.Listen != "10.0.0.1" {
		t.Errorf("Web.Listen = %q, want 10.0.0.1", cfg.Web.Listen)
	}
	if cfg.Web.Port != 1234 {
		t.Errorf("Web.Port = %d, want 1234", cfg.Web.Port)
	}
}

func TestValidateTiers(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		tiers    []TierConfig
		wantErr  bool
	}{
		{
			name:     "valid default config",
			interval: time.Second,
			tiers: []TierConfig{
				{Resolution: time.Second, MaxSize: "10MB"},
				{Resolution: time.Minute, MaxSize: "10MB"},
				{Resolution: 5 * time.Minute, MaxSize: "10MB"},
			},
		},
		{
			name:     "valid 5s interval",
			interval: 5 * time.Second,
			tiers: []TierConfig{
				{Resolution: 5 * time.Second, MaxSize: "10MB"},
				{Resolution: time.Minute, MaxSize: "10MB"},
			},
		},
		{
			name:     "valid single tier",
			interval: 10 * time.Second,
			tiers: []TierConfig{
				{Resolution: 10 * time.Second, MaxSize: "10MB"},
			},
		},
		{
			name:     "valid sub-second interval",
			interval: 500 * time.Millisecond,
			tiers: []TierConfig{
				{Resolution: 500 * time.Millisecond, MaxSize: "10MB"},
				{Resolution: time.Second, MaxSize: "10MB"},
				{Resolution: time.Minute, MaxSize: "10MB"},
			},
		},
		{
			name:     "valid 100ms interval",
			interval: 100 * time.Millisecond,
			tiers: []TierConfig{
				{Resolution: 100 * time.Millisecond, MaxSize: "10MB"},
				{Resolution: time.Second, MaxSize: "10MB"},
			},
		},
		{
			name:     "interval != tier0 resolution",
			interval: 5 * time.Second,
			tiers: []TierConfig{
				{Resolution: time.Second, MaxSize: "10MB"},
			},
			wantErr: true,
		},
		{
			name:     "tier0 resolution not in allowed set",
			interval: 3 * time.Second,
			tiers: []TierConfig{
				{Resolution: 3 * time.Second, MaxSize: "10MB"},
			},
			wantErr: true,
		},
		{
			name:     "tiers not ascending",
			interval: time.Second,
			tiers: []TierConfig{
				{Resolution: time.Second, MaxSize: "10MB"},
				{Resolution: time.Second, MaxSize: "10MB"},
			},
			wantErr: true,
		},
		{
			name:     "tiers inverted",
			interval: time.Second,
			tiers: []TierConfig{
				{Resolution: time.Second, MaxSize: "10MB"},
				{Resolution: 500 * time.Millisecond, MaxSize: "10MB"},
			},
			wantErr: true,
		},
		{
			name:     "tier not evenly divisible",
			interval: 5 * time.Second,
			tiers: []TierConfig{
				{Resolution: 5 * time.Second, MaxSize: "10MB"},
				{Resolution: 7 * time.Second, MaxSize: "10MB"},
			},
			wantErr: true,
		},
		{
			name:     "ratio at limit (300)",
			interval: time.Second,
			tiers: []TierConfig{
				{Resolution: time.Second, MaxSize: "10MB"},
				{Resolution: 5 * time.Minute, MaxSize: "10MB"},
			},
		},
		{
			name:     "ratio exceeds limit",
			interval: time.Second,
			tiers: []TierConfig{
				{Resolution: time.Second, MaxSize: "10MB"},
				{Resolution: 10 * time.Minute, MaxSize: "10MB"},
			},
			wantErr: true,
		},
		{
			name:     "no tiers",
			interval: time.Second,
			tiers:    nil,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				Collection: CollectionConfig{Interval: tt.interval},
				Storage:    StorageConfig{Tiers: tt.tiers},
			}
			err := cfg.validateTiers()
			if tt.wantErr && err == nil {
				t.Error("validateTiers() expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("validateTiers() unexpected error: %v", err)
			}
		})
	}
}

func TestNormalizeBasePath(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"/", "", false},
		{"  ", "", false},
		{"kula", "/kula", false},
		{"/kula", "/kula", false},
		{"/kula/", "/kula", false},
		{"/kula///", "/kula", false},
		{"/monitoring/kula", "/monitoring/kula", false},
		{"monitoring/kula/", "/monitoring/kula", false},
		{"/kula//foo", "/kula/foo", false},
		{"  /kula  ", "/kula", false},
		{"/kula?x=1", "", true},
		{"/kula#frag", "", true},
		{"/kula\\bad", "", true},
		{"/has space", "", true},
		{"/has\ttab", "", true},
		{"/has\nnewline", "", true},
		{"/./kula", "", true},
		{"/../kula", "", true},
		{"/kula/..", "", true},
		// Open-redirect (CWE-601): protocol-relative prefixes must be rejected.
		{"//evil.com", "", true},
		{"///kula", "", true},
		{"//evil.com/path", "", true},
		{"/\\evil.com", "", true},
		{"\\\\evil.com", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := normalizeBasePath(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("normalizeBasePath(%q) = %q, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeBasePath(%q) unexpected error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("normalizeBasePath(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestLoadBasePathEnvOverride(t *testing.T) {
	ownStorage(t)
	t.Setenv("KULA_BASE_PATH", "/kula/")
	cfg, err := Load("/nonexistent/path/config.yaml")
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Web.BasePath != "/kula" {
		t.Errorf("Web.BasePath = %q, want /kula", cfg.Web.BasePath)
	}
}

func TestLoadBasePathEnvInvalid(t *testing.T) {
	t.Setenv("KULA_BASE_PATH", "/bad#frag")
	if _, err := Load("/nonexistent/path/config.yaml"); err == nil {
		t.Fatal("Load() expected error for invalid base path, got nil")
	}
}

func TestParseRetention(t *testing.T) {
	tests := []struct {
		input   string
		want    time.Duration
		wantErr bool
	}{
		{"", 0, false},
		{"30s", 30 * time.Second, false},
		{"15m", 15 * time.Minute, false},
		{"12h", 12 * time.Hour, false},
		{"1d", 24 * time.Hour, false},
		{"2.5d", time.Duration(2.5 * 24 * float64(time.Hour)), false},
		{"1w", 0, true},
		{"abc", 0, true},
		{"-1d", 0, true},
		{"10", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseRetention(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseRetention(%q) expected error", tt.input)
				}
				return
			}
			if err != nil {
				t.Errorf("parseRetention(%q) unexpected error: %v", tt.input, err)
				return
			}
			if got != tt.want {
				t.Errorf("parseRetention(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestValidateBackup(t *testing.T) {
	// Disabled backup: invalid fields are ignored.
	cfg := DefaultConfig()
	cfg.Backup.Enabled = false
	cfg.Backup.MaxTier = 0
	cfg.Backup.Retention = "garbage"
	if err := cfg.validateBackup(); err != nil {
		t.Errorf("disabled backup should not validate: %v", err)
	}

	// Enabled with good defaults: retention parsed.
	cfg = DefaultConfig()
	cfg.Backup.Enabled = true
	if err := cfg.validateBackup(); err != nil {
		t.Fatalf("default backup should validate: %v", err)
	}
	if cfg.Backup.RetentionDur != 24*time.Hour {
		t.Errorf("RetentionDur = %v, want 24h", cfg.Backup.RetentionDur)
	}

	// Enabled with bad maxtier.
	cfg = DefaultConfig()
	cfg.Backup.Enabled = true
	cfg.Backup.MaxTier = 0
	if err := cfg.validateBackup(); err == nil {
		t.Error("maxtier 0 should fail validation")
	}

	// Enabled with bad retention.
	cfg = DefaultConfig()
	cfg.Backup.Enabled = true
	cfg.Backup.Retention = "5x"
	if err := cfg.validateBackup(); err == nil {
		t.Error("bad retention should fail validation")
	}
}

func TestGameScoreURL(t *testing.T) {
	ownStorage(t)
	dir := t.TempDir()
	tests := []struct {
		name   string
		value  string
		valid  bool
		origin string
	}{
		{name: "empty", valid: true},
		{name: "https endpoint", value: "https://example.com/score", valid: true, origin: "https://example.com"},
		{name: "http endpoint", value: "http://example.com/score", valid: true, origin: "http://example.com"},
		{name: "endpoint with port and query", value: "https://scores.example.com:8443/v1/submit?game=kula", valid: true, origin: "https://scores.example.com:8443"},
		{name: "ipv6 endpoint", value: "https://[2001:db8::1]:8443/score", valid: true, origin: "https://[2001:db8::1]:8443"},
		{name: "unsupported scheme", value: "ftp://example.com/score"},
		{name: "relative endpoint", value: "/score"},
		{name: "missing host", value: "https:///score"},
		{name: "credentials", value: "https://user:pass@example.com/score"},
		{name: "fragment", value: "https://example.com/score#fragment"},
		{name: "backslash", value: "https://example.com\\@other.example/score"},
		{name: "csp injection host", value: "https://example.com;script-src/score"},
		{name: "invalid port", value: "https://example.com:0/score"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tt.name, " ", "_")+".yaml")
			yamlData := "global:\n  game_score_url: " + strconv.Quote(tt.value) + "\n"
			if err := os.WriteFile(path, []byte(yamlData), 0644); err != nil {
				t.Fatal(err)
			}

			cfg, err := Load(path)
			if tt.valid {
				if err != nil {
					t.Fatalf("Load() unexpected error: %v", err)
				}
				if cfg.Global.GameScoreURL != tt.value {
					t.Errorf("game_score_url = %q, want %q", cfg.Global.GameScoreURL, tt.value)
				}
				origin, err := GameScoreURLOrigin(tt.value)
				if err != nil {
					t.Fatalf("GameScoreURLOrigin() unexpected error: %v", err)
				}
				if origin != tt.origin {
					t.Errorf("GameScoreURLOrigin() = %q, want %q", origin, tt.origin)
				}
				return
			}
			if err == nil {
				t.Fatal("Load() succeeded for invalid game_score_url")
			}
		})
	}
}

// A database host that is an IP address in brackets, as a URL writes one,
// loads as the bare address: the drivers bracket an IPv6 address themselves
// when they join it with the port. A MySQL socket path (port 0) and anything
// that is not an address keep their brackets.
func TestLoadUnbracketsDatabaseHosts(t *testing.T) {
	for _, tc := range []struct {
		name, settings string
		mysql, pg      string
	}{
		{"bracketed IPv6", "mysql:\n    host: \"[::1]\"\n  postgres:\n    host: \"[::1]\"\n", "::1", "::1"},
		{"bare IPv6", "mysql:\n    host: \"::1\"\n  postgres:\n    host: \"::1\"\n", "::1", "::1"},
		{"bracketed IPv6 with a zone", "mysql:\n    host: \"[fe80::1%eth0]\"\n", "fe80::1%eth0", "localhost"},
		{"bracketed IPv4", "mysql:\n    host: \"[192.0.2.1]\"\n", "192.0.2.1", "localhost"},
		{"IPv4", "mysql:\n    host: 192.0.2.1\n  postgres:\n    host: 192.0.2.1\n", "192.0.2.1", "192.0.2.1"},
		{"a name", "mysql:\n    host: db.example.com\n  postgres:\n    host: db.example.com\n", "db.example.com", "db.example.com"},
		{"a name in brackets", "mysql:\n    host: \"[db.example.com]\"\n", "[db.example.com]", "localhost"},
		{"a MySQL socket path", "mysql:\n    host: \"[::1]\"\n    port: 0\n", "[::1]", "localhost"},
		{"a PostgreSQL host list", "postgres:\n    host: \"[::1],db.example.com,[2001:db8::5],/run/postgresql\"\n", "localhost",
			"::1,db.example.com,2001:db8::5,/run/postgresql"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			yaml := fmt.Sprintf("storage:\n  directory: %s\napplications:\n  %s", dir, tc.settings)
			if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.Applications.Mysql.Host; got != tc.mysql {
				t.Errorf("applications.mysql.host = %q, want %q", got, tc.mysql)
			}
			if got := cfg.Applications.Postgres.Host; got != tc.pg {
				t.Errorf("applications.postgres.host = %q, want %q", got, tc.pg)
			}
		})
	}
}

// MySQL TLS settings that would protect the connection less than they read
// are refused: an unknown mode, a CA file no mode uses, TLS on a Unix socket,
// verify-ca without its CA, verify-full without a name to check. A disabled
// collector's settings are left alone, and the default is plaintext.
func TestLoadValidatesMysqlTLS(t *testing.T) {
	if mode := DefaultConfig().Applications.Mysql.SSLMode; mode != "disable" {
		t.Fatalf("default applications.mysql.sslmode = %q, want disable", mode)
	}
	for _, tc := range []struct {
		name, mysql string
		wantErr     string
	}{
		{"default", "enabled: true", ""},
		{"disable", "enabled: true\n    sslmode: disable", ""},
		{"require", "enabled: true\n    sslmode: require", ""},
		{"require with a CA", "enabled: true\n    sslmode: require\n    sslrootcert: /etc/kula/ca.pem", ""},
		{"verify-ca", "enabled: true\n    sslmode: verify-ca\n    sslrootcert: /etc/kula/ca.pem", ""},
		{"verify-full against the system CAs", "enabled: true\n    host: db.example.com\n    sslmode: verify-full", ""},
		{"verify-full with a CA", "enabled: true\n    host: db.example.com\n    sslmode: verify-full\n    sslrootcert: /etc/kula/ca.pem", ""},
		{"disabled collector", "enabled: false\n    sslmode: bogus", ""},
		{"unknown mode", "enabled: true\n    sslmode: VERIFY_IDENTITY", "is not one of"},
		{"prefer", "enabled: true\n    sslmode: prefer", "is not one of"},
		{"CA without TLS", "enabled: true\n    sslrootcert: /etc/kula/ca.pem", "does not use TLS"},
		{"Unix socket", "enabled: true\n    host: /run/mysqld/mysqld.sock\n    port: 0\n    sslmode: require", "Unix socket"},
		{"verify-ca against the system CAs", "enabled: true\n    sslmode: verify-ca", "needs sslrootcert"},
		{"verify-full without a host", "enabled: true\n    host: \"\"\n    sslmode: verify-full", "needs host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			yaml := fmt.Sprintf("storage:\n  directory: %s\napplications:\n  mysql:\n    %s\n", dir, tc.mysql)
			if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("Load() = %v, want no error", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("Load() = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}
