package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An explicit -config that cannot be read must stop the generator before it
// opens any storage. With the built-in defaults it would write mock samples
// into the default storage directory, where a real host keeps its history.
func TestExplicitConfigMustExist(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.yaml")
	// Were the fallback ever taken again, it must land here, not in a real
	// storage directory.
	fallback := t.TempDir()
	t.Setenv("KULA_DIRECTORY", fallback)

	var output strings.Builder
	err := run([]string{"-config", missing, "-yes", "-duration", "1m"}, strings.NewReader(""), &output)
	if err == nil {
		t.Fatalf("run with a missing explicit -config succeeded; output:\n%s", output.String())
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error %q does not name the missing config", err)
	}
	if output.Len() != 0 {
		t.Errorf("run wrote %q before failing, want nothing", output.String())
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Errorf("run left %d entries in the config directory, want none", len(entries))
	}
	if entries, _ := os.ReadDir(fallback); len(entries) != 0 {
		t.Errorf("run wrote %d entries into the default storage directory, want none", len(entries))
	}
}

func TestExplicitConfigIsUsed(t *testing.T) {
	dir := t.TempDir()
	storageDir := filepath.Join(dir, "data")
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := "storage:\n  directory: " + storageDir + "\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	err := run([]string{"-config", cfgPath, "-yes", "-duration", "10s"}, strings.NewReader(""), io.Discard)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(storageDir, "tier_0.dat")); err != nil {
		t.Errorf("no tier file in the configured storage directory: %v", err)
	}
}
