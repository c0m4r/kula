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
	Global     GlobalConfig     `yaml:"global"`
	Collection CollectionConfig `yaml:"collection"`
	Storage    StorageConfig    `yaml:"storage"`
	Web        WebConfig        `yaml:"web"`
	TUI        TUIConfig        `yaml:"tui"`
}

type GlobalConfig struct {
	Hostname       string `yaml:"hostname"`
	ShowSystemInfo bool   `yaml:"show_system_info"`
	DefaultTheme   string `yaml:"default_theme"`
}

type CollectionConfig struct {
	Interval    time.Duration `yaml:"interval"`
	Devices     []string      `yaml:"devices"`
	MountPoints []string      `yaml:"mountpoints"`
	Interfaces  []string      `yaml:"interfaces"`
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
	Enabled            bool        `yaml:"enabled"`
	Listen             string      `yaml:"listen"`
	Port               int         `yaml:"port"`
	Auth               AuthConfig  `yaml:"auth"`
	JoinMetrics        bool        `yaml:"join_metrics"`
	DefaultAggregation string      `yaml:"default_aggregation"`
	Logging            LogConfig   `yaml:"logging"`
	TrustProxy         bool        `yaml:"trust_proxy"`
	Graphs             GraphConfig `yaml:"graphs"`
	Version            string      `yaml:"-"` // injected at runtime, not from config file
	OS                 string      `yaml:"-"`
	Kernel             string      `yaml:"-"`
	Arch               string      `yaml:"-"`
}

type GraphConfig struct {
	CPUTemp GraphMaxConfig `yaml:"cpu_temp"`
	Network GraphMaxConfig `yaml:"network"`
}

type GraphMaxConfig struct {
	MaxMode  string  `yaml:"max_mode"` // "off", "on", "auto"
	MaxValue float64 `yaml:"max_value"`
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
	Argon2         Argon2Config  `yaml:"argon2"`
}

type Argon2Config struct {
	Time    uint32 `yaml:"time"`
	Memory  uint32 `yaml:"memory"` // memory in KB
	Threads uint8  `yaml:"threads"`
}

type TUIConfig struct {
	RefreshRate time.Duration `yaml:"refresh_rate"`
}

func isWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".kula-write-test-*")
	if err != nil {
		return false
	}
	_ = f.Close()
	_ = os.Remove(f.Name())
	return true
}

func DefaultConfig() *Config {
	return &Config{
		Global: GlobalConfig{
			ShowSystemInfo: true,
			DefaultTheme:   "dark",
		},
		Collection: CollectionConfig{
			Interval: time.Second,
		},
		Storage: StorageConfig{
			Directory: "/var/lib/kula",
			Tiers: []TierConfig{
				{Resolution: time.Second, MaxSize: "250MB"},
				{Resolution: time.Minute, MaxSize: "150MB"},
				{Resolution: 5 * time.Minute, MaxSize: "50MB"},
			},
		},
		Web: WebConfig{
			Enabled:            true,
			Listen:             "127.0.0.1",
			Port:               8080,
			JoinMetrics:        false,
			DefaultAggregation: "avg",
			Auth: AuthConfig{
				SessionTimeout: 24 * time.Hour,
				Argon2: Argon2Config{
					Time:    1,
					Memory:  64 * 1024,
					Threads: 4,
				},
			},
			Logging: LogConfig{
				Enabled: true,
				Level:   "perf",
			},
			Graphs: GraphConfig{
				CPUTemp: GraphMaxConfig{MaxMode: "off", MaxValue: 100},  // 100 Celsius
				Network: GraphMaxConfig{MaxMode: "off", MaxValue: 1000}, // 1000 Mbps
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
			if err := checkStorageDirectory(cfg); err != nil {
				return nil, err
			}
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

	if err := checkStorageDirectory(cfg); err != nil {
		return nil, err
	}

	if err := cfg.parseMaxBytes(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func checkStorageDirectory(cfg *Config) error {
	if cfg.Storage.Directory == "/var/lib/kula" {
		if err := os.MkdirAll(cfg.Storage.Directory, 0755); err != nil || !isWritable(cfg.Storage.Directory) {
			homeDir, err := os.UserHomeDir()
			if err == nil {
				fallbackDir := filepath.Join(homeDir, ".kula")
				log.Printf("Notice: Insufficient permissions for /var/lib/kula, falling back to %s", fallbackDir)
				if err := os.MkdirAll(fallbackDir, 0755); err != nil || !isWritable(fallbackDir) {
					return fmt.Errorf("insufficient permissions to create data storage in /var/lib/kula or %s", fallbackDir)
				}
				cfg.Storage.Directory = fallbackDir
			} else {
				return fmt.Errorf("insufficient permissions to create data storage in /var/lib/kula: %w", err)
			}
		}
	}
	return nil
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
