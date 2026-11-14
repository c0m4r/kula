package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestParseCommandLine: the global -config, -h and --help are read before
// the command and after it, a -config after it is as explicit as one before
// it, and an argument a command does not take is refused.
func TestParseCommandLine(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		command  string
		config   string
		explicit bool
		rest     []string
	}{
		{nil, "serve", "config.yaml", false, nil},
		{[]string{"tui"}, "tui", "config.yaml", false, []string{}},
		{[]string{"-config", "a.yaml"}, "serve", "a.yaml", true, nil},
		{[]string{"-config", "a.yaml", "serve"}, "serve", "a.yaml", true, []string{}},
		{[]string{"serve", "--config", "/etc/kula/config.yaml"}, "serve", "/etc/kula/config.yaml", true, []string{"--config", "/etc/kula/config.yaml"}},
		{[]string{"serve", "-config=b.yaml"}, "serve", "b.yaml", true, []string{"-config=b.yaml"}},
		{[]string{"-config", "a.yaml", "tui", "-config", "b.yaml"}, "tui", "b.yaml", true, []string{"-config", "b.yaml"}},
		{[]string{"inspect", "--verbose", "-config", "c.yaml"}, "inspect", "c.yaml", true, []string{"--verbose", "-config", "c.yaml"}},
		{[]string{"hash-password", "-config", "c.yaml"}, "hash-password", "c.yaml", true, []string{"-config", "c.yaml"}},
		{[]string{"disks", "-config", "c.yaml"}, "disks", "c.yaml", true, []string{"-config", "c.yaml"}},
	} {
		var stderr bytes.Buffer
		inv, err := parseCommandLine(tc.args, &stderr)
		if err != nil {
			t.Errorf("%q: %v\n%s", tc.args, err, stderr.String())
			continue
		}
		if inv.command != tc.command || inv.configPath != tc.config || inv.configExplicit != tc.explicit ||
			!reflect.DeepEqual(inv.args, tc.rest) {
			t.Errorf("%q: command %q, config %q (explicit %v), args %q; want %q, %q (%v), %q",
				tc.args, inv.command, inv.configPath, inv.configExplicit, inv.args, tc.command, tc.config, tc.explicit, tc.rest)
		}
	}

	for _, command := range []string{"serve", "tui", "hash-password", "inspect", "disks"} {
		for _, help := range []string{"--help", "-h"} {
			var stderr bytes.Buffer
			_, err := parseCommandLine([]string{command, "-config", "x.yaml", help}, &stderr)
			if !errors.Is(err, flag.ErrHelp) || !strings.Contains(stderr.String(), "Usage: kula [global flags] "+command+" [flags]") {
				t.Errorf("%s %s: %v, %q; want its usage", command, help, err, stderr.String())
			}
		}
		var stderr bytes.Buffer
		if _, err := parseCommandLine([]string{command, "extra"}, &stderr); !errors.Is(err, errUsage) ||
			!strings.Contains(stderr.String(), `unexpected argument "extra"`) {
			t.Errorf("%s extra: %v, %q; want a refusal naming the argument", command, err, stderr.String())
		}
		stderr.Reset()
		// -v after a command is not the version: after inspect it would read
		// as its -verbose.
		if _, err := parseCommandLine([]string{command, "-v"}, &stderr); !errors.Is(err, errUsage) {
			t.Errorf("%s -v: %v, want a refusal", command, err)
		}
	}

	var stderr bytes.Buffer
	if _, err := parseCommandLine([]string{"--help"}, &stderr); !errors.Is(err, flag.ErrHelp) ||
		!strings.Contains(stderr.String(), "kula [flags] [command] [flags]") || !strings.Contains(stderr.String(), "-v, -version") {
		t.Errorf("--help: %v, %q; want the usage, -version listed", err, stderr.String())
	}
	if inv, err := parseCommandLine([]string{"-version"}, &stderr); err != nil || !inv.showVersion {
		t.Errorf("-version: %v, version %v", err, inv.showVersion)
	}
	if inv, err := parseCommandLine([]string{"bogus", "--help"}, &stderr); !errors.Is(err, errUnknownCommand) || inv.command != "bogus" {
		t.Errorf("bogus: %v, command %q; want an unknown command", err, inv.command)
	}
}

// runKulaCommand runs `kula <args>` in a child process, in dir, with HOME,
// the storage directory and the web socket inside dir, and returns its exit
// code and output; -1 when it was still running after 20 seconds (a command
// that ignored its flags and started serving).
func runKulaCommand(t *testing.T, dir string, args ...string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCLICommand$")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"KULA_TEST_CLI_ARGS="+strings.Join(args, "\x1f"),
		"HOME="+filepath.Join(dir, "home"),
		"KULA_DIRECTORY="+filepath.Join(dir, "storage"),
		"KULA_UNIX_SOCKET="+filepath.Join(dir, "kula.sock"))
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), string(out)
	}
	if err != nil {
		t.Fatal(err)
	}
	return 0, string(out)
}

// TestCLICommand: flags after the command are read, not ignored. --help after
// any command prints its help and exits 0 before a config is read or seeded
// or storage is touched; an extra argument exits 2; a -config after the
// command is seeded and required like one before it; disks seeds nothing;
// inspect exits 1 when a tier file exists but cannot be inspected.
func TestCLICommand(t *testing.T) {
	if args, ok := os.LookupEnv("KULA_TEST_CLI_ARGS"); ok {
		os.Args = append([]string{"kula"}, strings.Split(args, "\x1f")...)
		main()
		os.Exit(0)
	}

	untouched := func(t *testing.T, dir string, paths ...string) {
		t.Helper()
		for _, path := range append(paths, "storage", "home", "config.yaml") {
			if _, err := os.Lstat(filepath.Join(dir, path)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s was created (%v)", path, err)
			}
		}
	}

	for _, command := range []string{"serve", "tui", "hash-password", "inspect", "disks"} {
		dir := t.TempDir()
		code, out := runKulaCommand(t, dir, command, "-config", filepath.Join(dir, "new.yaml"), "--help")
		if code != 0 || !strings.Contains(out, "Usage: kula [global flags] "+command) ||
			strings.Contains(out, "starting") || strings.Contains(out, "Enter password") {
			t.Errorf("%s --help: exit %d, output %q; want its help and nothing started", command, code, out)
		}
		untouched(t, dir, "new.yaml")

		if command != "hash-password" {
			code, out = runKulaCommand(t, dir, command, "extra")
			if code != 2 || !strings.Contains(out, `unexpected argument "extra"`) {
				t.Errorf("%s extra: exit %d, output %q; want 2 naming the argument", command, code, out)
			}
			untouched(t, dir)
		}
	}

	t.Run("an explicit -config after the command", func(t *testing.T) {
		dir := t.TempDir()
		// One that cannot be created stops kula, as before the command.
		typo := filepath.Join(dir, "typo", "config.yaml")
		code, out := runKulaCommand(t, dir, "serve", "--config", typo)
		if code == 0 || !strings.Contains(out, typo) || !strings.Contains(out, "cannot be created") {
			t.Errorf("serve --config %s: exit %d; want a refusal naming it:\n%s", typo, code, out)
		}
		// One that does not exist is seeded from the packaged example.
		seeded := filepath.Join(dir, "seeded.yaml")
		code, out = runKulaCommand(t, dir, "inspect", "-config", seeded)
		if code != 0 || !strings.Contains(out, "seeded it from the packaged example") {
			t.Errorf("inspect -config %s: exit %d:\n%s", seeded, code, out)
		}
		if _, err := os.Stat(seeded); err != nil {
			t.Errorf("inspect -config did not seed %s: %v", seeded, err)
		}
	})

	t.Run("disks seeds no config", func(t *testing.T) {
		dir := t.TempDir()
		code, out := runKulaCommand(t, dir, "disks", "-config", filepath.Join(dir, "new.yaml"))
		if code != 0 || (!strings.HasPrefix(out, "DEVICE") && out != "No disks found.\n") {
			t.Errorf("disks -config: exit %d, output %q", code, out)
		}
		untouched(t, dir, "new.yaml")
	})

	t.Run("inspect exits 1 on a tier it cannot read", func(t *testing.T) {
		dir := t.TempDir()
		configPath := filepath.Join(dir, "kula.yaml")
		if err := os.WriteFile(configPath, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		// No tier file yet, as on a fresh install: not a failure. (-config
		// before the command, as kula has always read it.)
		code, out := runKulaCommand(t, dir, "-config", configPath, "inspect")
		if code != 0 || strings.Count(out, "(not found)") != 3 {
			t.Errorf("inspect with no tier files: exit %d:\n%s", code, out)
		}
		if err := os.MkdirAll(filepath.Join(dir, "storage"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "storage", "tier_1.dat"), []byte("not a tier"), 0o600); err != nil {
			t.Fatal(err)
		}
		code, out = runKulaCommand(t, dir, "-config", configPath, "inspect")
		if code != 1 || !strings.Contains(out, "Error inspecting tier file") ||
			strings.Count(out, "(not found)") != 2 || !strings.Contains(out, "1 of 3 tier files not inspected") {
			t.Errorf("inspect with an unreadable tier file: exit %d; want 1 after the other tiers:\n%s", code, out)
		}
	})
}
