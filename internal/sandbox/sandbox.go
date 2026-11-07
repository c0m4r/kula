// Package sandbox implements Landlock-based process sandboxing.
//
// After calling Enforce(), the process is restricted to only the filesystem
// paths and network ports it needs:
//   - /proc, /sys: read-only (system metrics collection)
//   - utmp and systemd-logind sessions: read-only (logged-in user count)
//   - config file: read-only
//   - storage directory: read-write
//   - web port: TCP bind only
//
// This uses BestEffort() to gracefully degrade on kernels without Landlock
// support (pre-5.13). The process will still function, just without sandboxing.
package sandbox

import (
	"crypto/x509"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

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
//   - parent dir of web.UnixSocket (read-write, if configured)
//
// It restricts network access to only binding on the given TCP port. When the
// web server is configured to listen on a Unix socket, no TCP bind rule is
// added.
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
func Enforce(configPath string, storageDir string, webCfg config.WebConfig, appCfg config.ApplicationsConfig, ollamaCfg config.OllamaConfig) error {
	webPort := 0
	if webCfg.Enabled && webCfg.UnixSocket == "" {
		webPort = webCfg.Port
	}
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

		// Logged-in user count: utmp, or systemd-logind's session files
		// where a distribution no longer keeps utmp.
		landlock.ROFiles("/run/utmp").IgnoreIfMissing(),
		landlock.ROFiles("/var/run/utmp").IgnoreIfMissing(),
		landlock.RODirs("/run/systemd/sessions").IgnoreIfMissing(),

		// Data storage: read-write access
		landlock.RWDirs(absStorageDir),
	}

	// Web Unix socket: allow RW on the parent directory so we can create,
	// chmod and unlink the socket file.
	var webSocketInfo string
	if webCfg.Enabled && webCfg.UnixSocket != "" {
		absSock, err := filepath.Abs(webCfg.UnixSocket)
		if err != nil {
			return fmt.Errorf("sandbox: resolving web unix_socket: %w", err)
		}
		sockDir := filepath.Dir(absSock)
		fsRules = append(fsRules, landlock.RWDirs(sockDir))
		webSocketInfo = absSock
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

	// Apache2: allow outbound TCP connection to the status URL port
	if appCfg.Apache2.Enabled && appCfg.Apache2.StatusURL != "" {
		if u, err := url.Parse(appCfg.Apache2.StatusURL); err == nil {
			port := 80
			if u.Port() != "" {
				if p, err := strconv.Atoi(u.Port()); err == nil && p > 0 && p <= 65535 {
					port = p
				}
			} else if u.Scheme == "https" {
				port = 443
			}
			netRules = append(netRules, landlock.ConnectTCP(uint16(port)))
			appInfo = append(appInfo, fmt.Sprintf("apache2:connect-tcp/%d", port))
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

	// MySQL: allow outbound TCP or Unix socket file access
	if appCfg.Mysql.Enabled {
		if appCfg.Mysql.Port > 0 {
			netRules = append(netRules, landlock.ConnectTCP(uint16(appCfg.Mysql.Port)))
			appInfo = append(appInfo, fmt.Sprintf("mysql:connect-tcp/%d", appCfg.Mysql.Port))
		} else if appCfg.Mysql.Host != "" {
			// Unix socket mode: host is the socket file path (e.g. /var/run/mysqld/mysqld.sock)
			fsRules = append(fsRules, landlock.RWFiles(appCfg.Mysql.Host).IgnoreIfMissing())
			appInfo = append(appInfo, fmt.Sprintf("mysql:rw(%s)", appCfg.Mysql.Host))
		}
	}

	// PostgreSQL over TLS: lib/pq reads its CA, client certificate and key
	// from ~/.postgresql (root.crt, postgresql.crt, postgresql.key) or from
	// the PGSSL* environment paths, at connect time.
	if appCfg.Postgres.Enabled && postgresUsesTLS(appCfg.Postgres.SSLMode) {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			fsRules = append(fsRules, landlock.RODirs(filepath.Join(home, ".postgresql")).IgnoreIfMissing())
		}
		for _, env := range []string{"PGSSLROOTCERT", "PGSSLCERT", "PGSSLKEY", "PGSSLCRL"} {
			if path := os.Getenv(env); path != "" && path != "system" {
				fsRules = append(fsRules, landlock.ROFiles(path).IgnoreIfMissing())
			}
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

	// TLS verification (https:// status and Ollama URLs, PostgreSQL
	// verify-ca/verify-full) uses the system roots, which crypto/x509 loads
	// once, on first use, and then caches. Load them while the CA bundle — and
	// the distribution-specific tree its symlinks point into — is readable;
	// once confined, every verification would fail for lack of roots.
	if needsSystemRoots(appCfg, ollamaCfg) {
		if pool, err := x509.SystemCertPool(); err != nil || len(pool.Subjects()) == 0 { //nolint:staticcheck // Subjects only counts the roots here
			log.Printf("Warning: no system CA certificates loaded (%v); TLS connections to monitored targets will fail to verify", err)
		} else {
			appInfo = append(appInfo, "tls:system-roots")
		}
	}

	// Combine all rules. fsRules is not used after this point, so appending
	// into its backing array is safe.
	allRules := append(fsRules, netRules...) //nolint:gocritic // appendAssign: fsRules is not reused

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
	switch {
	case webSocketInfo != "":
		netStatus = fmt.Sprintf(", web: unix:%s", webSocketInfo)
	case webPort == 0:
		netStatus = ", net: disabled"
	case abi < 4:
		netStatus = " (network protection NOT supported by kernel, ABI < 4)"
	default:
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

// postgresUsesTLS reports whether a PostgreSQL sslmode can negotiate TLS.
func postgresUsesTLS(sslmode string) bool {
	return sslmode != "" && sslmode != "disable"
}

// needsSystemRoots reports whether an enabled outbound target verifies a TLS
// certificate against the system roots.
func needsSystemRoots(appCfg config.ApplicationsConfig, ollamaCfg config.OllamaConfig) bool {
	https := func(raw string) bool {
		u, err := url.Parse(raw)
		return err == nil && strings.EqualFold(u.Scheme, "https")
	}
	switch {
	case appCfg.Nginx.Enabled && https(appCfg.Nginx.StatusURL),
		appCfg.Apache2.Enabled && https(appCfg.Apache2.StatusURL),
		ollamaCfg.Enabled && https(ollamaCfg.URL):
		return true
	case appCfg.Postgres.Enabled:
		mode := appCfg.Postgres.SSLMode
		return mode == "verify-ca" || mode == "verify-full" || os.Getenv("PGSSLROOTCERT") == "system"
	}
	return false
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
