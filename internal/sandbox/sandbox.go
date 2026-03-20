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
	"os"
	"path/filepath"

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
// This function should be called after config and storage are initialized
// but before starting goroutines that serve requests.
//
// On kernels without Landlock support, this logs a warning and returns nil.
func Enforce(configPath string, storageDir string, webPort int) error {
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

		// Data storage: read-write access
		landlock.RWDirs(absStorageDir),
	}

	// Build network rules: only allow binding to the web port
	if webPort < 1 || webPort > 65535 {
		return fmt.Errorf("sandbox: invalid web port %d", webPort)
	}
	netRules := []landlock.Rule{
		landlock.BindTCP(uint16(webPort)),
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
	// Network restrictions (BindTCP) require ABI v4+ (kernel 6.7+)
	if abi < 4 {
		netStatus = " (network protection NOT supported by kernel, ABI < 4)"
	} else {
		netStatus = fmt.Sprintf(", net: bind TCP/%d", webPort)
	}

	log.Printf("Landlock sandbox enforced (ABI v%d, paths: /proc[ro] /sys[ro] %s[ro] %s[rw]%s)",
		abi, absConfigPath, absStorageDir, netStatus)

	return nil
}

// BuildRuleSummary returns a human-readable summary of the sandbox rules
// for logging and debugging purposes.
func BuildRuleSummary(configPath string, storageDir string, webPort int) string {
	absConfig, _ := filepath.Abs(configPath)
	absStorage, _ := filepath.Abs(storageDir)
	return fmt.Sprintf(
		"FS: /proc[ro] /sys[ro] %s[ro] %s[rw] | Net: bind TCP/%d",
		absConfig, absStorage, webPort,
	)
}
