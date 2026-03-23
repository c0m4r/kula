package config

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
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
	ShowVersion    bool   `yaml:"show_version"`
	DefaultTheme   string `yaml:"default_theme"`
}

type CollectionConfig struct {
	Interval    time.Duration `yaml:"interval"`
	Devices     []string      `yaml:"devices"`
	MountPoints []string      `yaml:"mountpoints"`
	Interfaces  []string      `yaml:"interfaces"`
	// DebugLog enables verbose debug logging for device/interface/filesystem
	// discovery. Activated when web.logging.level = "debug". Not exposed in YAML.
	DebugLog bool `yaml:"-"`
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
	UI                 bool        `yaml:"ui"`
	Listen             string      `yaml:"listen"`
	Port               int         `yaml:"port"`
	Auth               AuthConfig  `yaml:"auth"`
	PrometheusMetrics  MetricsConfig `yaml:"prometheus_metrics"`
	JoinMetrics        bool        `yaml:"join_metrics"`
	DefaultAggregation string      `yaml:"default_aggregation"`
	Logging            LogConfig   `yaml:"logging"`
	TrustProxy         bool        `yaml:"trust_proxy"`
	EnableCompression  bool        `yaml:"enable_compression"`
	Graphs             GraphConfig `yaml:"graphs"`
	Lang               LangConfig  `yaml:"lang"`
	Version            string      `yaml:"-"` // injected at runtime, not from config file
	OS                 string      `yaml:"-"`
	Kernel             string      `yaml:"-"`
	Arch               string      `yaml:"-"`
	MaxWebsocketConns  int         `yaml:"max_websocket_conns"`
	MaxWebsocketConnsPerIP int         `yaml:"max_websocket_conns_per_ip"`
}

type MetricsConfig struct {
	Enabled bool   `yaml:"enabled"`
	Token   string `yaml:"token"`
}

type GraphConfig struct {
	CPUTemp  GraphMaxConfig   `yaml:"cpu_temp"`
	DiskTemp GraphMaxConfig   `yaml:"disk_temp"`
	GPUTemp  GraphMaxConfig   `yaml:"gpu_temp"`
	Network  GraphMaxConfig   `yaml:"network"`
	Split    GraphSplitConfig `yaml:"split"`
}

type GraphMaxConfig struct {
	MaxMode  string  `yaml:"max_mode"` // "off", "on", "auto"
	MaxValue float64 `yaml:"max_value"`
}

type GraphSplitConfig struct {
	Network   bool `yaml:"network"`
	DiskIo    bool `yaml:"disk_io"`
	DiskSpace bool `yaml:"disk_space"`
	DiskTemp  bool `yaml:"disk_temp"`
	Gpu       bool `yaml:"gpu"`
}

type LangConfig struct {
	Default string `yaml:"default"`
	Force   bool   `yaml:"force"`
}

type LogConfig struct {
	Enabled bool   `yaml:"enabled"`
	Level   string `yaml:"level"` // "access", "perf", or "debug"
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
			ShowVersion:    true,
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
			UI:                 true,
			Listen:             "",
			Port:               27960,
			PrometheusMetrics: MetricsConfig{
				Enabled: false,
			},
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
			EnableCompression: true,
			Graphs: GraphConfig{
				CPUTemp:  GraphMaxConfig{MaxMode: "off", MaxValue: 100},  // 100 Celsius
				DiskTemp: GraphMaxConfig{MaxMode: "off", MaxValue: 100},
				GPUTemp:  GraphMaxConfig{MaxMode: "off", MaxValue: 100},
				Network:  GraphMaxConfig{MaxMode: "off", MaxValue: 1000}, // 1000 Mbps
			},
			Lang: LangConfig{
				Default: "en",
				Force:   false,
			},
			MaxWebsocketConns:      100,
			MaxWebsocketConnsPerIP: 5,
		},
		TUI: TUIConfig{
			RefreshRate: time.Second,
		},
	}
}

func Load(path string) (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err == nil {
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parsing config: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	// Override with environment variables
	if listen := os.Getenv("KULA_LISTEN"); listen != "" {
		cfg.Web.Listen = listen
	}
	if portStr := os.Getenv("KULA_PORT"); portStr != "" {
		if port64, err := strconv.ParseInt(portStr, 10, 32); err == nil {
			port := int(port64)
			if port > 0 && port <= 65535 {
				cfg.Web.Port = port
			} else {
				log.Printf("Warning: KULA_PORT %d out of range (1-65535), ignoring", port)
			}
		} else {
			log.Printf("Warning: invalid KULA_PORT %q: %v", portStr, err)
		}
	}
	if level, set := os.LookupEnv("KULA_LOGLEVEL"); set {
		if level != "" {
			cfg.Web.Logging.Enabled = true
			cfg.Web.Logging.Level = level
		} else {
			cfg.Web.Logging.Enabled = false
		}
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
		if err := os.MkdirAll(cfg.Storage.Directory, 0750); err != nil || !isWritable(cfg.Storage.Directory) {
			homeDir, err := os.UserHomeDir()
			if err == nil {
				fallbackDir := filepath.Join(homeDir, ".kula")
				log.Printf("Notice: Insufficient permissions for /var/lib/kula, falling back to %s", fallbackDir)
				if err := os.MkdirAll(fallbackDir, 0750); err != nil || !isWritable(fallbackDir) {
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
