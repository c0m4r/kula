package config

import (
	"os"
	"path/filepath"
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
