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

func TestLoadMissingFile(t *testing.T) {
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
