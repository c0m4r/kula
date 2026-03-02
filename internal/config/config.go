package config

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Collection CollectionConfig `yaml:"collection"`
	Storage    StorageConfig    `yaml:"storage"`
	Web        WebConfig        `yaml:"web"`
	TUI        TUIConfig        `yaml:"tui"`
}

type CollectionConfig struct {
	Interval time.Duration `yaml:"interval"`
}

type StorageConfig struct {
	Directory string       `yaml:"directory"`
	Tiers     []TierConfig `yaml:"tiers"`
}

type TierConfig struct {
	Resolution time.Duration `yaml:"resolution"`
	MaxSize    string        `yaml:"max_size"`
	MaxBytes   int64         `yaml:"-"`
}

type WebConfig struct {
	Enabled     bool       `yaml:"enabled"`
	Listen      string     `yaml:"listen"`
	Port        int        `yaml:"port"`
	Auth        AuthConfig `yaml:"auth"`
	JoinMetrics bool       `yaml:"join_metrics"`
	Logging     LogConfig  `yaml:"logging"`
	Version     string     `yaml:"-"` // injected at runtime, not from config file
	OS          string     `yaml:"-"`
	Kernel      string     `yaml:"-"`
	Arch        string     `yaml:"-"`
}

type LogConfig struct {
	Enabled bool   `yaml:"enabled"`
	Level   string `yaml:"level"` // "access" or "perf"
}

type AuthConfig struct {
	Enabled        bool          `yaml:"enabled"`
	Username       string        `yaml:"username"`
	PasswordHash   string        `yaml:"password_hash"`
	PasswordSalt   string        `yaml:"password_salt"`
	SessionTimeout time.Duration `yaml:"session_timeout"`
}

type TUIConfig struct {
	RefreshRate time.Duration `yaml:"refresh_rate"`
}

func isWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".kula-write-test-*")
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(f.Name())
	return true
}

func DefaultConfig() *Config {
	// Try /var/lib/kula first (typically requires root to create, but let's try)
	dataDir := "/var/lib/kula"
	if err := os.MkdirAll(dataDir, 0755); err != nil || !isWritable(dataDir) {
		// Fallback to ~/.kula
		homeDir, err := os.UserHomeDir()
		if err == nil {
			dataDir = filepath.Join(homeDir, ".kula")
			log.Printf("Notice: Insufficient permissions for /var/lib/kula, falling back to %s", dataDir)
			if err := os.MkdirAll(dataDir, 0755); err != nil || !isWritable(dataDir) {
				fmt.Fprintf(os.Stderr, "Error: Insufficient permissions to create data storage in /var/lib/kula or %s\n", dataDir)
				os.Exit(1)
			}
		} else {
			fmt.Fprintf(os.Stderr, "Error: Insufficient permissions to create data storage in /var/lib/kula\n")
			os.Exit(1)
		}
	}

	return &Config{
		Collection: CollectionConfig{
			Interval: time.Second,
		},
		Storage: StorageConfig{
			Directory: dataDir,
			Tiers: []TierConfig{
				{Resolution: time.Second, MaxSize: "250MB"},
				{Resolution: time.Minute, MaxSize: "150MB"},
				{Resolution: 5 * time.Minute, MaxSize: "50MB"},
			},
		},
		Web: WebConfig{
			Enabled: true,
			Listen:  "0.0.0.0",
			Port:    8080,
			Auth: AuthConfig{
				SessionTimeout: 24 * time.Hour,
			},
			Logging: LogConfig{
				Enabled: true,
				Level:   "perf",
			},
		},
		TUI: TUIConfig{
			RefreshRate: time.Second,
		},
	}
}

func Load(path string) (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if err := cfg.parseMaxBytes(); err != nil {
				return nil, err
			}
			return cfg, nil
		}
		return nil, fmt.Errorf("reading config: %w", err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	// Expand ~/ shorthand to the user's home directory
	if len(cfg.Storage.Directory) > 1 && cfg.Storage.Directory[:2] == "~/" {
		if homeDir, err := os.UserHomeDir(); err == nil {
			cfg.Storage.Directory = filepath.Join(homeDir, cfg.Storage.Directory[2:])
		}
	}

	if err := cfg.parseMaxBytes(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func (c *Config) parseMaxBytes() error {
	for i := range c.Storage.Tiers {
		b, err := parseSize(c.Storage.Tiers[i].MaxSize)
		if err != nil {
			return fmt.Errorf("tier %d max_size: %w", i, err)
		}
		c.Storage.Tiers[i].MaxBytes = b
	}
	return nil
}

func parseSize(s string) (int64, error) {
	var val float64
	var unit string
	_, err := fmt.Sscanf(s, "%f%s", &val, &unit)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	switch unit {
	case "B":
		return int64(val), nil
	case "KB":
		return int64(val * 1024), nil
	case "MB":
		return int64(val * 1024 * 1024), nil
	case "GB":
		return int64(val * 1024 * 1024 * 1024), nil
	default:
		return 0, fmt.Errorf("unknown unit %q in size %q", unit, s)
	}
}
