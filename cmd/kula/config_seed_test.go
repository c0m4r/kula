package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kula"
)

// TestSeedExplicitConfig: a -config path that does not exist is created from
// the packaged example; one that cannot be created is an error rather than
// a start on built-in defaults; an existing file, or a link, is left alone.
func TestSeedExplicitConfig(t *testing.T) {
	dir := t.TempDir()

	path := filepath.Join(dir, "config.yaml")
	if err := seedExplicitConfig(path); err != nil {
		t.Fatalf("seeding a missing config: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, kula.ExampleConfig) {
		t.Fatalf("seeded config is not the packaged example (%v)", err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("seeded config mode = %v (%v), want 0600", info.Mode().Perm(), err)
	}

	if err := os.WriteFile(path, []byte("web: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := seedExplicitConfig(path); err != nil {
		t.Errorf("an existing config: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "web: {}\n" {
		t.Errorf("an existing config was rewritten: %q", data)
	}

	missingDir := filepath.Join(dir, "typo", "config.yaml")
	if err := seedExplicitConfig(missingDir); err == nil || !strings.Contains(err.Error(), "cannot be created") {
		t.Errorf("a config in a directory that does not exist: %v, want an error", err)
	}

	if os.Geteuid() != 0 {
		readOnly := filepath.Join(dir, "ro")
		if err := os.Mkdir(readOnly, 0o500); err != nil {
			t.Fatal(err)
		}
		if err := seedExplicitConfig(filepath.Join(readOnly, "config.yaml")); err == nil {
			t.Error("a config in an unwritable directory: no error, want one")
		}
	}

	target, link := filepath.Join(dir, "elsewhere.yaml"), filepath.Join(dir, "link.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	_ = seedExplicitConfig(link)
	if _, err := os.Lstat(target); err == nil {
		t.Error("seeding followed a dangling link at the config path")
	}
}

// TestExplicitConfigThatCannotBeSeededStopsStartup: `kula -config <path>
// serve` with a path that neither exists nor can be created exits, where it
// used to serve on built-in defaults (auth off).
func TestExplicitConfigThatCannotBeSeededStopsStartup(t *testing.T) {
	if os.Getenv("KULA_TEST_SERVE_ARGS") != "" {
		runServeMain()
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "run"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Were it to start on defaults, it would serve here rather than on a
	// TCP port of this machine.
	t.Setenv("KULA_DIRECTORY", filepath.Join(dir, "storage"))
	t.Setenv("KULA_UNIX_SOCKET", filepath.Join(dir, "run", "kula.sock"))

	configPath := filepath.Join(dir, "typo", "config.yaml")
	cmd, logPath := startKula(t, "TestExplicitConfigThatCannotBeSeededStopsStartup", dir, "-config", configPath, "serve")
	code := waitExit(cmd, 10*time.Second)
	logText := readLog(t, logPath)
	if code == -1 {
		t.Fatalf("kula serve started without its config:\n%s", logText)
	}
	if code == 0 || !strings.Contains(logText, configPath) || !strings.Contains(logText, "cannot be created") {
		t.Errorf("exit %d; want a refusal naming %s:\n%s", code, configPath, logText)
	}
}
