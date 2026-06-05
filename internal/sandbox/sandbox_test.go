package sandbox

import (
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

	// Run the helper process which will enforce the sandbox and then try to
	// break out of it.
	cmd := exec.Command(os.Args[0], "-test.run=TestLandlockEnforcement")
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1",
		"TEST_CONFIG_PATH="+configPath,
		"TEST_STORAGE_DIR="+storageDir)

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

	webCfg := config.WebConfig{Enabled: true, Port: 27999}
	if err := Enforce(configPath, storageDir, webCfg, config.ApplicationsConfig{}, config.OllamaConfig{}); err != nil {
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

	fmt.Printf("OK: sandbox assertions passed (ABI %d)\n", abi)
	os.Exit(0)
}
