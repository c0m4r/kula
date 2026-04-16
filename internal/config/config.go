package config

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Global       GlobalConfig       `yaml:"global"`
	Collection   CollectionConfig   `yaml:"collection"`
	Storage      StorageConfig      `yaml:"storage"`
	Web          WebConfig          `yaml:"web"`
	Applications ApplicationsConfig `yaml:"applications"`
	TUI          TUIConfig          `yaml:"tui"`
	Ollama       OllamaConfig       `yaml:"ollama"`
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
	// MountsDetection controls how mount points are detected.
	// Options: "auto" (default), "host", "self"
	MountsDetection string `yaml:"mounts_detection"`
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

// OllamaConfig controls the optional Ollama LLM integration for AI-powered
// metric analysis. When enabled, the backend proxies requests to the local
// Ollama instance and streams responses to both the web UI and TUI.
type OllamaConfig struct {
	Enabled bool   `yaml:"enabled"`
	URL     string `yaml:"url"`     // e.g. http://localhost:11434
	Model   string `yaml:"model"`   // e.g. llama3
	Timeout string `yaml:"timeout"` // e.g. 120s
}

// ApplicationsConfig groups monitoring modules for external applications.
type ApplicationsConfig struct {
	Nginx      NginxConfig                    `yaml:"nginx"`
	Containers ContainersConfig               `yaml:"containers"`
	Postgres   PostgresConfig                 `yaml:"postgres"`
	Custom     map[string][]CustomMetricConfig `yaml:"custom"`
}

// CustomMetricConfig defines a single metric line within a custom chart group.
// Multiple metrics with different names form separate lines in the same chart.
type CustomMetricConfig struct {
	Name string  `yaml:"name"`
	Unit string  `yaml:"unit"`
	Max  float64 `yaml:"max"`
}

// NginxConfig controls monitoring via the nginx stub_status module.
// The status_url should point to the stub_status endpoint, e.g.
// http://localhost/status
type NginxConfig struct {
	Enabled   bool   `yaml:"enabled"`
	StatusURL string `yaml:"status_url"`
}

// ContainersConfig controls Docker/Podman container monitoring.
// Discovery uses the container runtime API socket. If the socket is
// unavailable, it falls back to cgroups-based discovery (without container
// name mapping). The active mode is logged at startup.
type ContainersConfig struct {
	Enabled    bool     `yaml:"enabled"`
	SocketPath string   `yaml:"socket_path"` // default: auto-detect docker/podman
	Containers []string `yaml:"containers"`  // filter by name/id; empty = all
}

// PostgresConfig controls PostgreSQL database monitoring.
// Connects via database/sql + lib/pq. Supports both TCP and Unix socket.
// For Unix socket connections, set host to the socket directory
// (e.g. /var/run/postgresql) and leave port as 0.
type PostgresConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	DBName   string `yaml:"dbname"`
	SSLMode  string `yaml:"sslmode"`
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
			DefaultTheme:   "auto",
		},
		Collection: CollectionConfig{
			Interval:        time.Second,
			MountsDetection: "auto",
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
			DefaultAggregation: "max",
			Auth: AuthConfig{
				SessionTimeout: 24 * time.Hour,
				Argon2: Argon2Config{
					Time:    3,
					Memory:  32 * 1024,
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
		Applications: ApplicationsConfig{
			Nginx: NginxConfig{
				Enabled:   false,
				StatusURL: "http://localhost/status",
			},
			Containers: ContainersConfig{
				Enabled: true,
				// SocketPath empty = auto-detect: try docker, then podman
			},
			Postgres: PostgresConfig{
				Enabled: false,
				Host:    "localhost",
				Port:    5432,
				User:    "kula_monitor",
				DBName:  "postgres",
				SSLMode: "disable",
			},
		},
		TUI: TUIConfig{
			RefreshRate: time.Second,
		},
		Ollama: OllamaConfig{
			Enabled: false,
			URL:     "http://localhost:11434",
			Model:   "llama3",
			Timeout: "120s",
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
	if md := os.Getenv("KULA_MOUNTS_DETECTION"); md != "" {
		cfg.Collection.MountsDetection = md
	}
	if dir := os.Getenv("KULA_DIRECTORY"); dir != "" {
		cfg.Storage.Directory = dir
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

	if err := cfg.validateTiers(); err != nil {
		return nil, err
	}

	if cfg.Ollama.Enabled {
		if err := validateOllamaURL(cfg.Ollama.URL); err != nil {
			return nil, err
		}
	}

	return cfg, nil
}

// validateOllamaURL ensures the Ollama URL only targets loopback addresses
// to prevent SSRF via a maliciously crafted config file.
func validateOllamaURL(rawURL string) error {
	if rawURL == "" {
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("ollama.url: invalid URL: %w", err)
	}
	host := u.Hostname()
	if host != "localhost" && host != "127.0.0.1" && host != "::1" {
		return fmt.Errorf("ollama.url: host %q is not a loopback address; only localhost, 127.0.0.1, or ::1 are allowed", host)
	}
	return nil
}

// validateTiers checks that collection.interval and storage.tiers form a
// consistent, ascending hierarchy with clean divisibility.
func (c *Config) validateTiers() error {
	tiers := c.Storage.Tiers
	if len(tiers) == 0 {
		return fmt.Errorf("at least one storage tier is required")
	}

	// Tier 0 resolution must equal the collection interval.
	if tiers[0].Resolution != c.Collection.Interval {
		return fmt.Errorf("storage.tiers[0].resolution (%s) must equal collection.interval (%s)",
			tiers[0].Resolution, c.Collection.Interval)
	}

	// Tier 0 allowed values: 1s, 2s, 5s, 10s, 15s, 30s.
	allowed := []time.Duration{
		time.Second,
		2 * time.Second,
		5 * time.Second,
		10 * time.Second,
		15 * time.Second,
		30 * time.Second,
	}
	valid := false
	for _, a := range allowed {
		if tiers[0].Resolution == a {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Errorf("storage.tiers[0].resolution (%s) must be one of: 1s, 2s, 5s, 10s, 15s, 30s",
			tiers[0].Resolution)
	}

	// Maximum aggregation ratio between consecutive tiers.
	// A ratio of 300 means at most 300 samples buffered in memory before
	// flushing to the next tier (e.g. 1s→5m = 300, 5s→1m = 12).
	const maxRatio = 300

	for i := 1; i < len(tiers); i++ {
		prev := tiers[i-1].Resolution
		curr := tiers[i].Resolution

		// Each tier must have a strictly higher resolution than the previous.
		if curr <= prev {
			return fmt.Errorf("storage.tiers[%d].resolution (%s) must be greater than tiers[%d].resolution (%s)",
				i, curr, i-1, prev)
		}

		// Higher tier must be evenly divisible by the previous tier.
		if curr%prev != 0 {
			return fmt.Errorf("storage.tiers[%d].resolution (%s) must be a multiple of tiers[%d].resolution (%s)",
				i, curr, i-1, prev)
		}

		// Ratio must not exceed maxRatio to limit memory usage and data
		// loss on shutdown (up to ratio-1 samples can be lost).
		ratio := int(curr / prev)
		if ratio > maxRatio {
			return fmt.Errorf("storage.tiers[%d].resolution (%s) / tiers[%d].resolution (%s) = %d exceeds maximum ratio of %d",
				i, curr, i-1, prev, ratio, maxRatio)
		}
	}

	return nil
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
