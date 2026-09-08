package main

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDisksCommandWithoutConfig(t *testing.T) {
	if configPath := os.Getenv("KULA_TEST_DISKS_CONFIG"); configPath != "" {
		flag.CommandLine = flag.NewFlagSet("kula", flag.ExitOnError)
		os.Args = []string{"kula", "-config", configPath, "disks"}
		main()
		os.Exit(0)
	}
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "invalid"}[existing], func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.yaml")
			storagePath := filepath.Join(dir, "storage")
			if existing {
				if err := os.WriteFile(configPath, []byte("invalid: [yaml"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestDisksCommandWithoutConfig$")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "KULA_TEST_DISKS_CONFIG="+configPath, "KULA_DIRECTORY="+storagePath)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("disks command: %v", err)
			}
			if !strings.HasPrefix(string(out), "DEVICE") && string(out) != "No disks found.\n" {
				t.Fatalf("unexpected disk listing: %s", out)
			}
			if _, err := os.Stat(storagePath); !os.IsNotExist(err) {
				t.Fatalf("disks command created storage: %v", err)
			}
			if !existing {
				if _, err := os.Stat(configPath); !os.IsNotExist(err) {
					t.Fatalf("disks command seeded a config: %v", err)
				}
			}
		})
	}
}
