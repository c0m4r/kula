package sandbox

import (
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"kula/internal/config"

	llsyscall "github.com/landlock-lsm/go-landlock/landlock/syscall"
)

// helperSkipCode is the exit status the sandboxed helper uses to tell the parent
// that Landlock can't be meaningfully exercised in this environment, so the
// parent should t.Skip instead of failing.
const helperSkipCode = 2

func TestLandlockEnforcement(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") == "1" {
		runHelperProcess()
		return
	}

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	_ = os.WriteFile(configPath, []byte("test: 1"), 0644)
	storageDir := filepath.Join(tempDir, "storage")
	_ = os.Mkdir(storageDir, 0750)
	caDir := filepath.Join(tempDir, "tls")
	_ = os.Mkdir(caDir, 0750)
	mysqlCA := filepath.Join(caDir, "mysql-ca.pem")
	_ = os.WriteFile(mysqlCA, []byte("ca"), 0644)
	_ = os.WriteFile(filepath.Join(caDir, "other.pem"), []byte("other"), 0644)

	// Run the helper process which will enforce the sandbox and then try to
	// break out of it.
	cmd := exec.Command(os.Args[0], "-test.run=TestLandlockEnforcement")
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1",
		"TEST_CONFIG_PATH="+configPath,
		"TEST_STORAGE_DIR="+storageDir,
		"TEST_MYSQL_CA="+mysqlCA)

	output, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == helperSkipCode {
			t.Skipf("Landlock not enforceable in this environment:\n%s", output)
		}
		t.Fatalf("Helper process failed: %v\nOutput: %s", err, string(output))
	}
	t.Logf("helper output:\n%s", output)
}

func runHelperProcess() {
	configPath := os.Getenv("TEST_CONFIG_PATH")
	storageDir := os.Getenv("TEST_STORAGE_DIR")

	// Landlock capabilities are ABI-gated, so we only assert a restriction the
	// running kernel can actually enforce:
	//   ABI >= 1: filesystem read/write/execute restrictions
	//   ABI >= 4: TCP connect/bind restrictions (Linux 6.7+)
	// On older kernels (e.g. 6.6 = ABI v3) the network simply isn't restricted,
	// so a dial there is governed purely by real connectivity — asserting it was
	// the source of the flaky pass/fail.
	abi, abiErr := llsyscall.LandlockGetABIVersion()
	if abiErr != nil || abi < 1 {
		fmt.Printf("SKIP: Landlock unavailable (abi=%d, err=%v)\n", abi, abiErr)
		os.Exit(helperSkipCode)
	}

	// Sources the sandbox must keep readable, when this host has them.
	_, utmpErr := os.ReadFile("/run/utmp")
	_, sessionsErr := os.ReadDir("/run/systemd/sessions")
	// Stat (unlike reading) does not load crypto/x509's cached roots.
	_, bundleErr := os.Stat("/etc/ssl/certs/ca-certificates.crt")

	// An https:// target makes Enforce load the system roots first. Its
	// connect rule (port 8443) leaves the dial assertion below intact.
	apps := config.ApplicationsConfig{Nginx: config.NginxConfig{Enabled: true, StatusURL: "https://127.0.0.1:8443/status"}}
	// MySQL over TLS with its own CA file, which it reads at connect time.
	mysqlCA := os.Getenv("TEST_MYSQL_CA")
	apps.Mysql = config.MysqlConfig{Enabled: true, Host: "127.0.0.1", Port: 8444, SSLMode: "verify-full", SSLRootCert: mysqlCA}
	webCfg := config.WebConfig{Enabled: true, Port: 27999}
	if err := Enforce(configPath, storageDir, webCfg, apps, config.OllamaConfig{}); err != nil {
		fmt.Printf("SKIP: Enforce failed: %v\n", err)
		os.Exit(helperSkipCode)
	}

	// 1. Network: outbound TCP connect is only restricted on ABI v4+. On v4+ the
	// connect is denied by the kernel at the syscall level (deterministic,
	// independent of connectivity). Below v4 there is nothing to assert.
	if abi >= 4 {
		conn, err := net.DialTimeout("tcp", "8.8.8.8:53", 2*time.Second)
		if err == nil {
			_ = conn.Close()
			fmt.Printf("FAIL: network dial succeeded unexpectedly (ABI %d must block connect)\n", abi)
			os.Exit(1)
		}
	} else {
		fmt.Printf("INFO: ABI %d < 4, skipping network-restriction assertion\n", abi)
	}

	// 2. Write outside the storage directory must be denied (ABI v1+).
	if err := os.WriteFile("/tmp/kula-sandbox-test", []byte("leak"), 0644); err == nil {
		_ = os.Remove("/tmp/kula-sandbox-test")
		fmt.Printf("FAIL: write outside storage directory succeeded unexpectedly\n")
		os.Exit(1)
	}

	// 3. Executing a binary outside allowed paths must be denied (ABI v1+).
	if err := exec.Command("/usr/bin/id").Run(); err == nil {
		fmt.Printf("FAIL: execute outside allowed paths succeeded unexpectedly\n")
		os.Exit(1)
	}

	// 4. Writing inside the storage directory must still succeed.
	if err := os.WriteFile(filepath.Join(storageDir, "test.txt"), []byte("ok"), 0644); err != nil {
		fmt.Printf("FAIL: write to storage directory failed: %v\n", err)
		os.Exit(1)
	}

	// 5. The logged-in user count keeps its sources (utmp, logind sessions).
	if utmpErr == nil {
		if _, err := os.ReadFile("/run/utmp"); err != nil {
			fmt.Printf("FAIL: /run/utmp readable before the sandbox but not after: %v\n", err)
			os.Exit(1)
		}
	}
	if sessionsErr == nil {
		if _, err := os.ReadDir("/run/systemd/sessions"); err != nil {
			fmt.Printf("FAIL: logind sessions readable before the sandbox but not after: %v\n", err)
			os.Exit(1)
		}
	}

	// 6. The MySQL CA file stays readable, and only that file of its directory.
	if _, err := os.ReadFile(mysqlCA); err != nil {
		fmt.Printf("FAIL: MySQL sslrootcert unreadable under the sandbox: %v\n", err)
		os.Exit(1)
	}
	if _, err := os.ReadFile(filepath.Join(filepath.Dir(mysqlCA), "other.pem")); err == nil {
		fmt.Printf("FAIL: a file beside the MySQL sslrootcert is readable under the sandbox\n")
		os.Exit(1)
	}

	// 7. TLS targets still verify: the system roots were loaded before the
	// CA bundle became unreadable.
	if bundleErr == nil {
		pool, err := x509.SystemCertPool()
		if err != nil || len(pool.Subjects()) == 0 { //nolint:staticcheck // counting roots
			fmt.Printf("FAIL: no system CA roots under the sandbox with an https target configured: %v\n", err)
			os.Exit(1)
		}
	}

	fmt.Printf("OK: sandbox assertions passed (ABI %d)\n", abi)
	os.Exit(0)
}

func TestNeedsSystemRoots(t *testing.T) {
	https := config.ApplicationsConfig{Nginx: config.NginxConfig{Enabled: true, StatusURL: "https://web/status"}}
	for _, tc := range []struct {
		name   string
		apps   config.ApplicationsConfig
		ollama config.OllamaConfig
		want   bool
	}{
		{"nothing outbound", config.ApplicationsConfig{}, config.OllamaConfig{}, false},
		{"nginx over http", config.ApplicationsConfig{Nginx: config.NginxConfig{Enabled: true, StatusURL: "http://web/status"}}, config.OllamaConfig{}, false},
		{"nginx over https", https, config.OllamaConfig{}, true},
		{"https nginx disabled", config.ApplicationsConfig{Nginx: config.NginxConfig{StatusURL: "https://web/status"}}, config.OllamaConfig{}, false},
		{"apache over https", config.ApplicationsConfig{Apache2: config.Apache2Config{Enabled: true, StatusURL: "HTTPS://web/server-status"}}, config.OllamaConfig{}, true},
		{"ollama over https", config.ApplicationsConfig{}, config.OllamaConfig{Enabled: true, URL: "https://llm:11434"}, true},
		{"postgres require", config.ApplicationsConfig{Postgres: config.PostgresConfig{Enabled: true, SSLMode: "require"}}, config.OllamaConfig{}, false},
		{"postgres verify-full", config.ApplicationsConfig{Postgres: config.PostgresConfig{Enabled: true, SSLMode: "verify-full"}}, config.OllamaConfig{}, true},
		{"postgres verify-ca", config.ApplicationsConfig{Postgres: config.PostgresConfig{Enabled: true, SSLMode: "verify-ca"}}, config.OllamaConfig{}, true},
		{"mysql require", config.ApplicationsConfig{Mysql: config.MysqlConfig{Enabled: true, SSLMode: "require"}}, config.OllamaConfig{}, false},
		{"mysql verify-ca", config.ApplicationsConfig{Mysql: config.MysqlConfig{Enabled: true, SSLMode: "verify-ca", SSLRootCert: "/etc/kula/ca.pem"}}, config.OllamaConfig{}, false},
		{"mysql verify-full", config.ApplicationsConfig{Mysql: config.MysqlConfig{Enabled: true, SSLMode: "verify-full"}}, config.OllamaConfig{}, true},
		{"mysql verify-full with a CA file", config.ApplicationsConfig{Mysql: config.MysqlConfig{Enabled: true, SSLMode: "verify-full", SSLRootCert: "/etc/kula/ca.pem"}}, config.OllamaConfig{}, false},
		{"mysql verify-full beside postgres", config.ApplicationsConfig{
			Postgres: config.PostgresConfig{Enabled: true, SSLMode: "disable"},
			Mysql:    config.MysqlConfig{Enabled: true, SSLMode: "verify-full"},
		}, config.OllamaConfig{}, true},
	} {
		if got := needsSystemRoots(tc.apps, tc.ollama); got != tc.want {
			t.Errorf("%s: needsSystemRoots = %v, want %v", tc.name, got, tc.want)
		}
	}

	// sslrootcert=system through the environment verifies against the system
	// roots whatever the configured mode.
	t.Setenv("PGSSLROOTCERT", "system")
	if !needsSystemRoots(config.ApplicationsConfig{Postgres: config.PostgresConfig{Enabled: true, SSLMode: "require"}}, config.OllamaConfig{}) {
		t.Error("PGSSLROOTCERT=system: needsSystemRoots = false, want true")
	}
}

// TestURLConnectPort pins the port each HTTP target is allowed to connect to:
// the one the client dials, so a URL without a port gets its scheme's default
// (an Ollama URL behind an HTTPS proxy on 443 used to be allowed 11434).
func TestURLConnectPort(t *testing.T) {
	for raw, want := range map[string]uint16{
		"http://localhost/status":    80,
		"https://localhost/status":   443,
		"HTTPS://localhost/status":   443,
		"http://127.0.0.1:8080/":     8080,
		"https://[::1]:8443/x":       8443,
		"http://localhost:11434":     11434,
		"https://localhost":          443,
		"http://localhost:0/":        0,
		"http://localhost:70000/":    0,
		"http://localhost:port/":     0,
		"http://local host:8080/bad": 0,
	} {
		got, ok := urlConnectPort(raw)
		if want == 0 {
			if ok {
				t.Errorf("urlConnectPort(%q) = %d, want rejected", raw, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("urlConnectPort(%q) = %d, %v, want %d", raw, got, ok, want)
		}
	}
}

// TestPickContainerSocket mirrors the collector's choice: the first candidate
// that accepts a connection, else the first that exists.
func TestPickContainerSocket(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.sock")
	dead := filepath.Join(dir, "dead.sock")
	if err := os.WriteFile(dead, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(dir, "live.sock")
	listener, err := net.Listen("unix", live)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()

	for _, tc := range []struct {
		candidates []string
		want       string
	}{
		{[]string{missing, dead, live}, live},
		{[]string{missing, dead}, dead},
		{[]string{missing}, ""},
	} {
		if got := pickContainerSocket(tc.candidates); got != tc.want {
			t.Errorf("pickContainerSocket(%v) = %q, want %q", tc.candidates, got, tc.want)
		}
	}
}
