// Package sandbox implements Landlock-based process sandboxing.
//
// After calling Enforce(), the process is restricted to only the filesystem
// paths and network ports it needs:
//   - /proc, /sys: read-only (system metrics collection)
//   - config file: read-only
//   - storage directory: read-write
//   - web port: TCP bind only
//
// This uses BestEffort() to gracefully degrade on kernels without Landlock
// support (pre-5.13). The process will still function, just without sandboxing.
package sandbox

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	"kula/internal/config"

	"github.com/landlock-lsm/go-landlock/landlock"
	llsyscall "github.com/landlock-lsm/go-landlock/landlock/syscall"
)

// Enforce applies Landlock restrictions to the current process.
//
// It restricts filesystem access to only the paths Kula needs:
//   - /proc and /sys (read-only, for metrics collection)
//   - configPath (read-only)
//   - storageDir (read-write)
//
// It restricts network access to only binding on the given TCP port.
//
// When application monitoring is enabled, additional rules are added:
//   - Nginx: ConnectTCP to the status URL port (for HTTP GET)
//   - Containers: ROFiles on the Docker/Podman Unix socket
//   - PostgreSQL: ConnectTCP to the configured port, or RODirs for
//     Unix socket directories
//
// This function should be called after config and storage are initialized
// but before starting goroutines that serve requests.
//
// On kernels without Landlock support, this logs a warning and returns nil.
func Enforce(configPath string, storageDir string, webPort int, appCfg config.ApplicationsConfig, ollamaCfg config.OllamaConfig) error {
	// Resolve paths to absolute to satisfy Landlock requirements
	absConfigPath, err := filepath.Abs(configPath)
	if err != nil {
		return fmt.Errorf("sandbox: resolving config path: %w", err)
	}

	absStorageDir, err := filepath.Abs(storageDir)
	if err != nil {
		return fmt.Errorf("sandbox: resolving storage dir: %w", err)
	}

	// Ensure the storage directory exists before restricting
	if err := os.MkdirAll(absStorageDir, 0750); err != nil {
		return fmt.Errorf("sandbox: creating storage dir: %w", err)
	}

	// Build filesystem rules
	fsRules := []landlock.Rule{
		// System metrics: read-only access to /proc and /sys
		landlock.RODirs("/proc"),
		landlock.RODirs("/sys").IgnoreIfMissing(),

		// Config file: read-only
		landlock.ROFiles(absConfigPath).IgnoreIfMissing(),

		// Core system config for network resolution
		landlock.ROFiles("/etc/hosts").IgnoreIfMissing(),
		landlock.ROFiles("/etc/resolv.conf").IgnoreIfMissing(),
		landlock.ROFiles("/etc/nsswitch.conf").IgnoreIfMissing(),

		// Data storage: read-write access
		landlock.RWDirs(absStorageDir),
	}

	// Build network rules: only allow binding to the web port
	var netRules []landlock.Rule
	if webPort > 0 {
		if webPort > 65535 {
			return fmt.Errorf("sandbox: invalid web port %d", webPort)
		}
		netRules = []landlock.Rule{
			landlock.BindTCP(uint16(webPort)),
		}
	}

	// Application monitoring rules
	var appInfo []string

	// Nginx: allow outbound TCP connection to the status URL port
	if appCfg.Nginx.Enabled && appCfg.Nginx.StatusURL != "" {
		if u, err := url.Parse(appCfg.Nginx.StatusURL); err == nil {
			port := 80
			if u.Port() != "" {
				if p, err := strconv.Atoi(u.Port()); err == nil && p > 0 && p <= 65535 {
					port = p
				}
			} else if u.Scheme == "https" {
				port = 443
			}
			netRules = append(netRules, landlock.ConnectTCP(uint16(port)))
			appInfo = append(appInfo, fmt.Sprintf("nginx:connect-tcp/%d", port))
		}
	}

	// Containers: allow read access to the runtime socket
	if appCfg.Containers.Enabled {
		socketPath := appCfg.Containers.SocketPath
		if socketPath == "" {
			// Auto-detect: try known paths
			for _, p := range []string{
				"/var/run/docker.sock",
				"/run/docker.sock",
				"/var/run/podman/podman.sock",
				"/run/podman/podman.sock",
			} {
				if _, err := os.Stat(p); err == nil {
					socketPath = p
					break
				}
			}
		}
		if socketPath != "" {
			fsRules = append(fsRules, landlock.RWFiles(socketPath).IgnoreIfMissing())
			appInfo = append(appInfo, fmt.Sprintf("containers:rw(%s)", socketPath))
		}
	}

	// PostgreSQL: allow outbound TCP or Unix socket access
	if appCfg.Postgres.Enabled {
		if appCfg.Postgres.Port > 0 {
			netRules = append(netRules, landlock.ConnectTCP(uint16(appCfg.Postgres.Port)))
			appInfo = append(appInfo, fmt.Sprintf("postgres:connect-tcp/%d", appCfg.Postgres.Port))
		} else if appCfg.Postgres.Host != "" {
			// Unix socket mode: host is the socket directory
			fsRules = append(fsRules, landlock.RWDirs(appCfg.Postgres.Host).IgnoreIfMissing())
			appInfo = append(appInfo, fmt.Sprintf("postgres:rw(%s)", appCfg.Postgres.Host))
		}
	}

	// Ollama: allow outbound TCP connection to the Ollama API port
	if ollamaCfg.Enabled && ollamaCfg.URL != "" {
		if u, err := url.Parse(ollamaCfg.URL); err == nil {
			ollamaPort := 11434
			if u.Port() != "" {
				if p, err := strconv.Atoi(u.Port()); err == nil && p > 0 && p <= 65535 {
					ollamaPort = p
				}
			}
			netRules = append(netRules, landlock.ConnectTCP(uint16(ollamaPort)))
			appInfo = append(appInfo, fmt.Sprintf("ollama:connect-tcp/%d", ollamaPort))
		}
	}

	// Combine all rules
	allRules := append(fsRules, netRules...)

	// Apply Landlock restrictions using V5 with BestEffort.
	// V5 (kernel 6.7+) includes: filesystem + networking + ioctl on devices.
	abi, err := llsyscall.LandlockGetABIVersion()
	if err != nil {
		log.Printf("Landlock not supported or disabled by kernel (skipping sandbox enforcement): %v", err)
		return nil
	}

	if abi < 1 {
		log.Println("Landlock ABI < 1, skipping sandbox enforcement")
		return nil
	}

	err = landlock.V5.BestEffort().Restrict(allRules...)
	if err != nil {
		return fmt.Errorf("sandbox: enforcing landlock: %w", err)
	}

	var netStatus string
	// Network restrictions (BindTCP/ConnectTCP) require ABI v4+ (kernel 6.7+)
	if webPort == 0 {
		netStatus = ", net: disabled"
	} else if abi < 4 {
		netStatus = " (network protection NOT supported by kernel, ABI < 4)"
	} else {
		netStatus = fmt.Sprintf(", net: bind TCP/%d", webPort)
	}

	var appStatus string
	if len(appInfo) > 0 {
		appStatus = fmt.Sprintf(", apps: %v", appInfo)
	}

	log.Printf("Landlock sandbox enforced (ABI v%d, paths: /proc[ro] /sys[ro] %s[ro] %s[rw]%s%s)",
		abi, absConfigPath, absStorageDir, netStatus, appStatus)

	return nil
}

// BuildRuleSummary returns a human-readable summary of the sandbox rules
// for logging and debugging purposes.
func BuildRuleSummary(configPath string, storageDir string, webPort int) string {
	absConfig, _ := filepath.Abs(configPath)
	absStorage, _ := filepath.Abs(storageDir)
	net := fmt.Sprintf("bind TCP/%d", webPort)
	if webPort == 0 {
		net = "disabled"
	}
	return fmt.Sprintf(
		"FS: /proc[ro] /sys[ro] %s[ro] %s[rw] | Net: %s",
		absConfig, absStorage, net,
	)
}
