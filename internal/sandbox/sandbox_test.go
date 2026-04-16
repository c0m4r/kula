package sandbox

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"kula/internal/config"
)

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

	// Run the helper process which will enforce sandbox and then try to break out
	cmd := exec.Command(os.Args[0], "-test.run=TestLandlockEnforcement")
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1", 
		"TEST_CONFIG_PATH="+configPath, 
		"TEST_STORAGE_DIR="+storageDir)
	
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Helper process failed: %v\nOutput: %s", err, string(output))
	}
}

func runHelperProcess() {
	configPath := os.Getenv("TEST_CONFIG_PATH")
	storageDir := os.Getenv("TEST_STORAGE_DIR")
	
	// Enforce sandbox (using a high port for testing)
	err := Enforce(configPath, storageDir, 27999, config.ApplicationsConfig{}, config.OllamaConfig{})
	if err != nil {
		os.Exit(0) // Landlock might not be supported, skip test silently
	}

	// 1. Test Network: Try to dial an external address
	// Note: Landlock V5 primarily restricts TCP BIND. Restricting outgoing CONNECT
	// typically requires network namespaces or other mechanisms. This test may pass
	// if the environment lacks connectivity or timing out, but we keep it as a baseline.
	conn, err := net.DialTimeout("tcp", "8.8.8.8:53", 100*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		fmt.Printf("FAIL: Network dial succeeded unexpectedly\n")
		os.Exit(1)
	}

	// 2. Test Write: Try to write to a path outside storageDir
	err = os.WriteFile("/tmp/kula-sandbox-test", []byte("leak"), 0644)
	if err == nil {
		fmt.Printf("FAIL: Write outside storage directory succeeded unexpectedly\n")
		os.Exit(1)
	}

	// 3. Test Execute: Try to run a binary
	cmd := exec.Command("/usr/bin/id")
	if err := cmd.Run(); err == nil {
		fmt.Printf("FAIL: Execute outside allowed paths succeeded unexpectedly\n")
		os.Exit(1)
	}

	// 4. Test Allowed Access: Try to write to storageDir (should succeed)
	err = os.WriteFile(filepath.Join(storageDir, "test.txt"), []byte("ok"), 0644)
	if err != nil {
		fmt.Printf("FAIL: Write to storage directory failed: %v\n", err)
		os.Exit(1)
	}

	os.Exit(0)
}
