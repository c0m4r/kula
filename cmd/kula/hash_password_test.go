package main

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"kula/internal/config"
	"kula/internal/web"
)

// runHashPasswordCommand runs `kula <args>` in a child process with stdin,
// returning its exit code, stdout and stderr.
func runHashPasswordCommand(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHashPasswordCommand$")
	cmd.Dir = t.TempDir() // no config.yaml: built-in defaults
	cmd.Env = append(os.Environ(), "KULA_TEST_HASH_ARGS="+strings.Join(args, "\x1f"))
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, stdout.String(), stderr.String()
}

// TestHashPasswordCommand: --help prints usage without prompting, an empty
// password is refused, and a piped password is hashed exactly as a login
// sends it, surrounding spaces included.
func TestHashPasswordCommand(t *testing.T) {
	if args, ok := os.LookupEnv("KULA_TEST_HASH_ARGS"); ok {
		flag.CommandLine = flag.NewFlagSet("kula", flag.ExitOnError)
		os.Args = append([]string{"kula"}, strings.Split(args, "\x1f")...)
		main()
		os.Exit(0)
	}

	for _, help := range []string{"--help", "-h"} {
		code, stdout, stderr := runHashPasswordCommand(t, "", "hash-password", help)
		if code != 0 || !strings.Contains(stderr, "Usage: kula [global flags] hash-password") ||
			strings.Contains(stdout, "Enter password") {
			t.Errorf("hash-password %s: exit %d, stdout %q, stderr %q; want its usage without a prompt", help, code, stdout, stderr)
		}
	}
	if code, _, stderr := runHashPasswordCommand(t, "", "hash-password", "extra"); code != 2 || !strings.Contains(stderr, `unexpected argument "extra"`) {
		t.Errorf("hash-password extra: exit %d, stderr %q; want 2 naming the argument", code, stderr)
	}
	for _, input := range []string{"", "\n", "\r\n"} {
		code, stdout, stderr := runHashPasswordCommand(t, input, "hash-password")
		if code != 1 || !strings.Contains(stderr, "the password is empty") || strings.Contains(stdout, "password_hash") {
			t.Errorf("hash-password with %q on stdin: exit %d, stdout %q, stderr %q; want a refusal", input, code, stdout, stderr)
		}
	}

	code, stdout, stderr := runHashPasswordCommand(t, " correct horse \r\n", "hash-password")
	if code != 0 {
		t.Fatalf("hash-password: exit %d, stderr %q", code, stderr)
	}
	field := func(name string) string {
		match := regexp.MustCompile(`(?m)^\s+` + name + `:\s+"?([0-9a-f]+)"?$`).FindStringSubmatch(stdout)
		if match == nil {
			t.Fatalf("no %s in the output:\n%s", name, stdout)
		}
		return match[1]
	}
	number := func(name string) uint64 {
		n, err := strconv.ParseUint(regexp.MustCompile(`(?m)^\s+` + name + `: (\d+)$`).FindStringSubmatch(stdout)[1], 10, 32)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	params := config.Argon2Config{Time: uint32(number("time")), Memory: uint32(number("memory")), Threads: uint8(number("threads"))}
	if got, want := field("password_hash"), web.HashPassword(" correct horse ", field("password_salt"), params); got != want {
		t.Error("the printed hash is not of the password as typed, surrounding spaces included")
	}
}
