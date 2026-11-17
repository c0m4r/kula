package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// slowScanEnv set to 1 makes TestScanJudgesTheRealKula run DOS-SLOWLORIS and
// DOS-CONNFLOOD too. Each waits out Kula's 30 s read timeout, so they are
// left out of the default run.
const slowScanEnv = "KULA_SCAN_SLOW"

// The finding IDs a sound Kula reports as something other than a PASS, by
// why. Each scan below names its WARN, SKIP and unreported IDs from these;
// every other ID in the catalog must PASS. A new check that does not pass
// against the real Kula therefore fails the test until a line here says why.
var (
	// Reported only when something is wrong, or, for DISC-003, when Kula
	// refuses the Host name the scanner sends.
	realOnlyWhenWrong = []string{"DISC-003", "HDR-000", "HDR-009", "CORS-002", "TRAV-000"}
	// Judged only with web.auth on: a Kula without auth gets none of them.
	realAuthOnly = []string{"DISC-002", "AUTH-001", "AUTH-002", "AUTH-003", "AUTH-004", "AUTH-005",
		"AUTH-007", "AUTH-008", "AUTH-010", "CSRF-003", "CSRF-004", "WS-001"}
	// Skipped over plain HTTP; AUTH-009, the Secure cookie, is not reported.
	realTLSOnly = []string{"HDR-008", "TLS-001", "TLS-002", "TLS-003", "TLS-004"}
	// Run with -aggressive only.
	realAggressive = []string{"AUTH-006", "INPUT-AGG", "INPUT-CHAT", "WS-FLOOD", "WS-MSGBOMB", "RATE-OLLAMA",
		"RATE-LOGIN", "RATE-CHAT", "DOS-SLOWLORIS", "DOS-HEADERBOMB", "DOS-CONNFLOOD", "DOS-HISTORY", "BYPASS-XFF"}
	// The aggressive checks that wait out Kula's read timeout (slowScanEnv).
	realSlow = []string{"DOS-SLOWLORIS", "DOS-CONNFLOOD"}
	// The aggressive checks of the login endpoint, which Kula serves with
	// auth only.
	realLogin = []string{"AUTH-006", "INPUT-AGG", "RATE-LOGIN", "BYPASS-XFF"}
	// Run with -fuzz only.
	realFuzz = []string{"FUZZ-QUERY", "FUZZ-PATH", "FUZZ-BODY", "FUZZ-METHODS", "FUZZ-SMUGGLE", "FUZZ-WS", "FUZZ-LIVENESS"}
	// Skipped while /metrics is off, as it is by default; PROM-002 is not
	// reported.
	realNoMetrics = []string{"PROM-001", "PROM-003"}
	// Skipped when Kula refuses the scanner's Host name (DISC-003): they would
	// read the refusal as their own safeguard holding.
	realRefusedHost = []string{"CSRF-001", "CSRF-002", "CSRF-003", "CSRF-004", "CORS-001", "CORS-002", "CORS-003",
		"TRAV-000", "TRAV-001", "TRAV-002", "PROM-001", "PROM-002", "PROM-003", "WS-000", "WS-001", "WS-002",
		"INPUT-001", "INPUT-002", "INPUT-003", "REDIR-001", "SRI-001", "SRI-002"}
)

// realKulaSetup is the configuration of one `kula serve` the test starts.
type realKulaSetup struct {
	auth     bool     // web.auth, with the test's admin account
	metrics  bool     // web.prometheus_metrics, with the test's token
	ai       bool     // ollama, at a closed local port; logs at debug level, which shows a chat starting
	basePath string   // web.base_path
	unix     bool     // web.unix_socket instead of a TCP port
	security []string // lines under web.security
}

// realScanWant is what one scan of the real Kula must report: its exit
// status and, by status, the exact set of finding IDs. Every catalog ID not
// named must PASS.
type realScanWant struct {
	exit     int
	fail     []string
	warn     []string
	errors   []string
	skip     []string
	silent   []string // not reported at all
	expected string   // the -expect key every FAIL is judged against
}

// realScan is one kula-scan run against a posture's Kula. A scan with
// usageError set stops at the command line: exit status 2, no report, and
// that text on stderr.
type realScan struct {
	name       string
	args       func(t *testing.T, k *realKula) []string
	want       realScanWant
	usageError string
}

// realPosture is one Kula configuration and the scans run against it, in
// order. Each posture gets a fresh instance: the login and chat limiters live
// in its memory.
type realPosture struct {
	name  string
	kula  realKulaSetup
	scans []realScan
}

// realPostures returns the postures TestScanJudgesTheRealKula scans. slow
// adds DOS-SLOWLORIS and DOS-CONNFLOOD to the -aggressive scans.
func realPostures(slow bool) []realPosture {
	// The aggressive scans leave out the two slow probes unless slow.
	var slowSkip, slowSilent []string
	if !slow {
		slowSkip, slowSilent = []string{"-skip", strings.Join(realSlow, ",")}, realSlow
	}
	authOffSilent := idsOf(realOnlyWhenWrong, realAuthOnly, []string{"AUTH-009", "PROM-002"})
	authOnSilent := idsOf(realOnlyWhenWrong, []string{"AUTH-000", "AUTH-009"})
	// The posture a sound Kula declares with -expect.
	authOffExpect := []string{"-expect", "origin-validation,frame-protection,host-check"}
	authOnExpect := []string{"-expect", "auth,origin-validation,frame-protection,metrics-token"}
	asAdmin := []string{"-username", "admin"} // the password comes from passwordEnv

	return []realPosture{
		{
			name: "auth off",
			kula: realKulaSetup{},
			scans: []realScan{{
				name: "default",
				args: func(_ *testing.T, k *realKula) []string { return argv(authOffExpect, k.url) },
				want: realScanWant{
					warn:   []string{"AUTH-000"},
					skip:   idsOf(realTLSOnly, realAggressive, realFuzz, realNoMetrics),
					silent: authOffSilent,
				},
			}, {
				// Kula answers only local names while auth is off: the
				// scan says it could not judge, and does not pass the
				// refusal off as the safeguards behind it.
				name: "under a Host name Kula refuses",
				args: func(_ *testing.T, k *realKula) []string {
					return []string{"-connect", k.addr, "http://kula.example.com:" + k.port + "/"}
				},
				want: realScanWant{
					exit:   exitInconclusive,
					errors: []string{"DISC-003"},
					warn:   []string{"AUTH-000"},
					// HDR-010: every response is the refusal, which carries
					// nothing to keep out of a cache.
					skip: idsOf(realTLSOnly, realAggressive, realFuzz, realRefusedHost, []string{"HDR-010"}),
					silent: idsOf([]string{"HDR-000", "HDR-009", "AUTH-009"},
						without(realAuthOnly, realRefusedHost...)),
				},
			}},
		},
		{
			name: "auth on",
			kula: realKulaSetup{auth: true, metrics: true, ai: true},
			scans: []realScan{{
				name: "-fuzz",
				args: func(_ *testing.T, k *realKula) []string {
					return argv(asAdmin, authOnExpect, []string{"-fuzz"}, k.url)
				},
				want: realScanWant{
					warn:   []string{"DISC-002"},
					skip:   idsOf(realTLSOnly, realAggressive, []string{"HOST-001"}),
					silent: authOnSilent,
				},
			}},
		},
		{
			name: "auth on, aggressive",
			kula: realKulaSetup{auth: true, metrics: true, ai: true},
			scans: []realScan{{
				name: "-aggressive -fuzz",
				args: func(_ *testing.T, k *realKula) []string {
					return argv(asAdmin, authOnExpect, []string{"-aggressive", "-fuzz"}, slowSkip, k.url)
				},
				want: realScanWant{
					warn: []string{"DISC-002"},
					// RATE-OLLAMA: discovery reads ollama_enabled from
					// /api/config, which auth hides from a client without a
					// session, so the check skips although the assistant is
					// on.
					skip:   idsOf(realTLSOnly, []string{"HOST-001", "RATE-OLLAMA"}),
					silent: idsOf(authOnSilent, slowSilent),
				},
			}},
		},
		{
			name: "auth off, aggressive",
			kula: realKulaSetup{ai: true},
			scans: []realScan{{
				name: "-aggressive -fuzz",
				args: func(_ *testing.T, k *realKula) []string {
					return argv(authOffExpect, []string{"-aggressive", "-fuzz"}, slowSkip, k.url)
				},
				want: realScanWant{
					warn:   []string{"AUTH-000"},
					skip:   idsOf(realTLSOnly, realNoMetrics, realLogin),
					silent: idsOf(authOffSilent, slowSilent),
				},
			}},
		},
		{
			name: "web.base_path",
			kula: realKulaSetup{auth: true, metrics: true, basePath: "/kula"},
			scans: []realScan{{
				name: "-fuzz, the base path in the URL",
				args: func(_ *testing.T, k *realKula) []string {
					return argv(asAdmin, authOnExpect, []string{"-fuzz"}, k.url)
				},
				want: realScanWant{
					warn:   []string{"DISC-002"},
					skip:   idsOf(realTLSOnly, realAggressive, []string{"HOST-001"}),
					silent: authOnSilent,
				},
			}, {
				// Nothing answers outside the base path, so the root is not
				// Kula.
				name: "the root, outside the base path",
				args: func(_ *testing.T, k *realKula) []string { return []string{"http://" + k.addr + "/"} },
				want: realScanWant{
					exit:   exitUsage,
					errors: []string{"DISC-001"},
					silent: allIDsBut("DISC-001"),
				},
			}},
		},
		{
			name: "web.unix_socket",
			kula: realKulaSetup{auth: true, metrics: true, unix: true},
			scans: []realScan{{
				name: "-unix -fuzz",
				args: func(_ *testing.T, k *realKula) []string {
					return argv(asAdmin, authOnExpect, []string{"-fuzz", "-unix", k.socket}, k.url)
				},
				want: realScanWant{
					warn:   []string{"DISC-002"},
					skip:   idsOf(realTLSOnly, realAggressive, []string{"HOST-001"}),
					silent: authOnSilent,
				},
			}},
		},
		{
			name: "refused credentials",
			kula: realKulaSetup{auth: true, metrics: true},
			scans: []realScan{{
				name: "a wrong password",
				args: func(t *testing.T, k *realKula) []string {
					return argv(asAdmin, []string{"-password", "wrong-" + randomHex(t, 4)}, k.url)
				},
				want: realScanWant{
					exit:   exitInconclusive,
					errors: []string{"AUTH-005"},
					warn:   []string{"DISC-002"},
					// The checks that need a session say so; there is no
					// session cookie to judge (AUTH-007, AUTH-008), and no
					// same-origin upgrade to compare a foreign one with
					// (WS-000).
					skip: idsOf(realTLSOnly, realAggressive, realFuzz, []string{"HOST-001",
						"AUTH-010", "CSRF-003", "CSRF-004", "WS-002", "INPUT-001", "INPUT-002", "INPUT-003"}),
					silent: idsOf(authOnSilent, []string{"AUTH-007", "AUTH-008", "WS-000"}),
				},
			}, {
				name:       "a mistyped -only",
				args:       func(_ *testing.T, k *realKula) []string { return []string{"-only", "headrs", k.url} },
				usageError: `-only: unknown category or finding ID "headrs"`,
			}, {
				name: "an unreachable port",
				args: func(t *testing.T, _ *realKula) []string {
					return []string{"-timeout", "2s", "http://127.0.0.1:" + strconv.Itoa(closedPort(t)) + "/"}
				},
				want: realScanWant{
					exit:   exitUsage,
					errors: []string{"DISC-001"},
					silent: allIDsBut("DISC-001"),
				},
			}},
		},

		// A misconfigured Kula: the scan must notice.
		{
			name: "origin validation off",
			kula: realKulaSetup{auth: true, metrics: true, security: []string{"origin_validation: false"}},
			scans: []realScan{{
				// The session cookie's SameSite still holds, so these warn.
				name: "default",
				args: func(_ *testing.T, k *realKula) []string { return argv(asAdmin, k.url) },
				want: realScanWant{
					warn:   []string{"DISC-002", "CSRF-001", "CSRF-002", "WS-002"},
					skip:   idsOf(realTLSOnly, realAggressive, realFuzz, []string{"HOST-001"}),
					silent: authOnSilent,
				},
			}, {
				name: "-expect origin-validation",
				args: func(_ *testing.T, k *realKula) []string {
					return argv(asAdmin, []string{"-expect", "origin-validation"}, k.url)
				},
				want: realScanWant{
					exit:     exitFailing,
					fail:     []string{"CSRF-001", "CSRF-002", "WS-002"},
					expected: "origin-validation",
					warn:     []string{"DISC-002"},
					skip:     idsOf(realTLSOnly, realAggressive, realFuzz, []string{"HOST-001"}),
					silent:   authOnSilent,
				},
			}},
		},
		{
			name: "any Host name answered",
			kula: realKulaSetup{security: []string{`allowed_hosts: ["*"]`}},
			scans: []realScan{{
				name: "default",
				args: func(_ *testing.T, k *realKula) []string { return []string{k.url} },
				want: realScanWant{
					warn:   []string{"AUTH-000", "HOST-001"},
					skip:   idsOf(realTLSOnly, realAggressive, realFuzz, realNoMetrics),
					silent: authOffSilent,
				},
			}, {
				name: "-expect host-check",
				args: func(_ *testing.T, k *realKula) []string { return []string{"-expect", "host-check", k.url} },
				want: realScanWant{
					exit:     exitFailing,
					fail:     []string{"HOST-001"},
					expected: "host-check",
					warn:     []string{"AUTH-000"},
					skip:     idsOf(realTLSOnly, realAggressive, realFuzz, realNoMetrics),
					silent:   authOffSilent,
				},
			}},
		},
		{
			name: "security headers off",
			kula: realKulaSetup{security: []string{"headers: false"}},
			scans: []realScan{{
				// Without a CSP there is no nonce to compare (HDR-004), and
				// the CSP directives warn as missing.
				name: "default",
				args: func(_ *testing.T, k *realKula) []string { return []string{k.url} },
				want: realScanWant{
					exit:   exitFailing,
					fail:   []string{"HDR-001", "HDR-002", "HDR-003", "HDR-005", "HDR-006"},
					warn:   []string{"AUTH-000", "HDR-007", "HDR-011", "HDR-012", "HDR-013"},
					skip:   idsOf(realTLSOnly, realAggressive, realFuzz, realNoMetrics),
					silent: idsOf(authOffSilent, []string{"HDR-004"}),
				},
			}},
		},
	}
}

// TestScanJudgesTheRealKula builds `kula`, starts `kula serve` in each
// posture above and scans it with kula-scan as a user would, asserting the
// exit status and every finding's status. A server change that weakens a
// safeguard fails it, and so does a check that misjudges a sound Kula.
// KULA_SCAN_SLOW=1 adds the two probes that wait out Kula's read timeout.
func TestScanJudgesTheRealKula(t *testing.T) {
	if testing.Short() {
		t.Skip("builds kula and scans several kula serve instances")
	}
	bin := buildRealKula(t)
	creds := newRealCredentials(t, bin)
	// Every authenticated scan reads the password, and every scan the
	// metrics token, from the environment, as documented for users. All
	// instances share them, so the postures can run in parallel.
	t.Setenv(passwordEnv, creds.password)
	t.Setenv(metricsTokenEnv, creds.token)

	for _, p := range realPostures(os.Getenv(slowScanEnv) == "1") {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			k := startRealKula(t, bin, p.kula, creds)
			for _, sc := range p.scans {
				t.Run(sc.name, func(t *testing.T) { runRealScan(t, sc, sc.args(t, k)) })
			}
			k.stop(t)
			k.checkLog(t)
		})
	}
}

// runRealScan runs kula-scan in-process with a JSON report and checks it
// against sc.
func runRealScan(t *testing.T, sc realScan, scanArgs []string) {
	t.Helper()
	cmdLine := append([]string{"-json"}, scanArgs...)
	var stdout, stderr bytes.Buffer
	code := runCLI(context.Background(), cmdLine, &stdout, &stderr)
	shown := "kula-scan " + strings.Join(cmdLine, " ")

	if sc.usageError != "" {
		if code != exitUsage || stdout.Len() != 0 || !strings.Contains(stderr.String(), sc.usageError) {
			t.Errorf("%s: exit %d, want %d with no report and %q on stderr\nstdout:\n%s\nstderr:\n%s",
				shown, code, exitUsage, sc.usageError, stdout.String(), stderr.String())
		}
		return
	}
	report := decodeReport(t, stdout.String())
	if problems := judgeRealScan(sc.want, code, report); len(problems) > 0 {
		t.Errorf("%s (seed %d):\n  %s\nstderr:\n%s", shown, report.Options.Seed,
			strings.Join(problems, "\n  "), stderr.String())
	}
}

// judgeRealScan lists how report and its exit status code differ from want,
// one line per difference, quoting each finding that is not as expected.
func judgeRealScan(want realScanWant, code int, report Report) []string {
	var problems []string
	if code != want.exit || report.ExitCode != code {
		problems = append(problems, fmt.Sprintf("exit %d (report says %d), want %d", code, report.ExitCode, want.exit))
	}

	expected, err := want.statuses()
	if err != nil {
		return append(problems, "the test's table: "+err.Error())
	}
	got := map[string]Finding{}
	for _, f := range report.Findings {
		if _, dup := got[f.ID]; dup {
			problems = append(problems, f.ID+" is reported twice")
		}
		if _, known := lookupID(f.ID); !known {
			problems = append(problems, fmt.Sprintf("%s is not in the catalog: %s", f.ID, describeFinding(f)))
		}
		got[f.ID] = f
	}
	for _, e := range catalog {
		f, reported := got[e.ID]
		status := "not reported"
		if reported {
			status = f.StatusStr
		}
		if status != expected[e.ID] {
			line := fmt.Sprintf("%s: %s, want %s", e.ID, status, expected[e.ID])
			if reported {
				line += ": " + describeFinding(f)
			}
			problems = append(problems, line)
		}
		if reported && f.Status == StatusFail && f.Expectation != want.expected {
			problems = append(problems, fmt.Sprintf("%s fails against -expect %q, want %q", e.ID, f.Expectation, want.expected))
		}
	}
	return problems
}

// statuses returns the status, or "not reported", each catalog ID must have.
// An ID named twice, or not in the catalog, is an error in the table.
func (w realScanWant) statuses() (map[string]string, error) {
	out := make(map[string]string, len(catalog))
	for _, e := range catalog {
		out[e.ID] = StatusPass.String()
	}
	named := map[string]bool{}
	for _, list := range []struct {
		status string
		ids    []string
	}{
		{StatusFail.String(), w.fail},
		{StatusWarn.String(), w.warn},
		{StatusError.String(), w.errors},
		{StatusSkip.String(), w.skip},
		{"not reported", w.silent},
	} {
		for _, id := range list.ids {
			if _, ok := out[id]; !ok {
				return nil, fmt.Errorf("%s is not in the catalog", id)
			}
			if named[id] {
				return nil, fmt.Errorf("%s is named twice", id)
			}
			named[id] = true
			out[id] = list.status
		}
	}
	return out, nil
}

// describeFinding renders a finding in one line, for a failure message.
func describeFinding(f Finding) string {
	s := fmt.Sprintf("%s %s [%s] %s", f.StatusStr, f.ID, f.SeverityStr, f.Detail)
	if f.Evidence != "" {
		s += " (evidence: " + f.Evidence + ")"
	}
	return s
}

// idsOf joins lists of finding IDs.
func idsOf(lists ...[]string) []string {
	var out []string
	for _, list := range lists {
		out = append(out, list...)
	}
	return out
}

// argv joins command-line pieces: strings and lists of them.
func argv(pieces ...any) []string {
	var out []string
	for _, p := range pieces {
		switch v := p.(type) {
		case string:
			out = append(out, v)
		case []string:
			out = append(out, v...)
		}
	}
	return out
}

// without returns list without the IDs in drop.
func without(list []string, drop ...string) []string {
	var out []string
	for _, id := range list {
		if !slices.Contains(drop, id) {
			out = append(out, id)
		}
	}
	return out
}

// allIDsBut returns every catalog ID except keep.
func allIDsBut(keep ...string) []string {
	var out []string
	for _, e := range catalog {
		if !slices.Contains(keep, e.ID) {
			out = append(out, e.ID)
		}
	}
	return out
}

// buildRealKula builds the kula command of this checkout, CGO-free as it
// ships, and returns the binary's path.
func buildRealKula(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "kula")
	build := exec.Command("go", "build", "-o", bin, "kula/cmd/kula")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build kula/cmd/kula: %v\n%s", err, out)
	}
	return bin
}

// realCredentials are the admin password, its hash and salt, and the
// Prometheus token every instance is configured with.
type realCredentials struct {
	password, hash, salt, token string
}

// realArgon2 lowers Kula's Argon2 cost for the test's account, so logins and
// the login bursts of -aggressive cost little. hash-password and serve read
// it from the same config block.
const realArgon2 = "    argon2:\n      time: 1\n      memory: 8192\n      threads: 1\n"

// newRealCredentials makes a random password and token and hashes the
// password with `kula hash-password`, which reads it on stdin.
func newRealCredentials(t *testing.T, bin string) realCredentials {
	t.Helper()
	creds := realCredentials{password: randomHex(t, 12), token: randomHex(t, 24)}
	dir := t.TempDir()
	storage := filepath.Join(dir, "storage")
	config := filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf("storage:\n  directory: %q\nweb:\n  auth:\n%s", storage, realArgon2)
	if err := os.WriteFile(config, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-config", config, "hash-password")
	cmd.Dir = dir
	cmd.Env = realKulaEnv(storage)
	cmd.Stdin = strings.NewReader(creds.password + "\n")
	out, err := cmd.CombinedOutput()
	hash := regexp.MustCompile(`(?m)^Password hash: ([0-9a-f]+)$`).FindSubmatch(out)
	salt := regexp.MustCompile(`(?m)^Salt: ([0-9a-f]+)$`).FindSubmatch(out)
	if err != nil || hash == nil || salt == nil {
		t.Fatalf("kula hash-password: %v\n%s", err, out)
	}
	creds.hash, creds.salt = string(hash[1]), string(salt[1])
	return creds
}

// randomHex returns n random bytes in hex.
func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// realKulaEnv is the environment kula runs in: the test's, without any
// KULA_ variable that would override its config, and with KULA_DIRECTORY
// naming storage, so it never falls back to ~/.kula.
func realKulaEnv(storage string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "KULA_") {
			env = append(env, kv)
		}
	}
	return append(env, "KULA_DIRECTORY="+storage)
}

// handedOut holds the ports closedPort returned, so the postures running in
// parallel never get the same one: one instance's Ollama port must not be
// another's web port.
var handedOut = struct {
	sync.Mutex
	ports map[int]bool
}{ports: map[int]bool{}}

// closedPort returns a local TCP port nothing listens on, and that it has not
// returned before.
func closedPort(t *testing.T) int {
	t.Helper()
	handedOut.Lock()
	defer handedOut.Unlock()
	for {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		_ = ln.Close()
		if !handedOut.ports[port] {
			handedOut.ports[port] = true
			return port
		}
	}
}

// realKula is a `kula serve` the test started.
type realKula struct {
	setup   realKulaSetup
	url     string // what a scan names: http://127.0.0.1:<port><base path>, or http://localhost<base path> on the socket
	addr    string // 127.0.0.1:<port>; empty on a Unix socket
	port    string
	socket  string // web.unix_socket
	logPath string
	cmd     *exec.Cmd
	exited  chan struct{} // closed when the process has exited
	waitErr error
	stopped bool
}

// startRealKula writes the config for setup, starts `kula serve` with it and
// waits until /health answers. The instance is stopped when the test ends,
// and its output printed when the test failed.
func startRealKula(t *testing.T, bin string, setup realKulaSetup, creds realCredentials) *realKula {
	t.Helper()
	dir := t.TempDir()
	storage := filepath.Join(dir, "storage")
	k := &realKula{setup: setup, logPath: filepath.Join(dir, "kula.log"), exited: make(chan struct{})}

	var cfg strings.Builder
	fmt.Fprintf(&cfg, `storage:
  directory: %q
  tiers:
    - resolution: 1s
      max_size: 1MB
    - resolution: 1m
      max_size: 1MB
    - resolution: 5m
      max_size: 1MB
applications:
  containers:
    enabled: false
web:
`, storage)
	if setup.unix {
		k.socket = filepath.Join(dir, "run", "kula.sock")
		if err := os.Mkdir(filepath.Dir(k.socket), 0o700); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&cfg, "  unix_socket: %q\n", k.socket)
		k.url = "http://localhost" + setup.basePath
	} else {
		k.port = strconv.Itoa(closedPort(t))
		k.addr = "127.0.0.1:" + k.port
		fmt.Fprintf(&cfg, "  listen: 127.0.0.1\n  port: %s\n", k.port)
		k.url = "http://" + k.addr + setup.basePath
	}
	if setup.basePath != "" {
		fmt.Fprintf(&cfg, "  base_path: %q\n", setup.basePath)
	}
	level := "access"
	if setup.ai {
		level = "debug"
	}
	fmt.Fprintf(&cfg, "  logging:\n    enabled: true\n    level: %s\n", level)
	if setup.metrics {
		fmt.Fprintf(&cfg, "  prometheus_metrics:\n    enabled: true\n    token: %q\n", creds.token)
	}
	fmt.Fprintf(&cfg, "  auth:\n    enabled: %t\n    username: admin\n    password_hash: %q\n    password_salt: %q\n%s",
		setup.auth, creds.hash, creds.salt, realArgon2)
	if len(setup.security) > 0 {
		cfg.WriteString("  security:\n")
		for _, line := range setup.security {
			cfg.WriteString("    " + line + "\n")
		}
	}
	if setup.ai {
		fmt.Fprintf(&cfg, "ollama:\n  enabled: true\n  url: \"http://127.0.0.1:%d\"\n", closedPort(t))
	}
	config := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(config, []byte(cfg.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	logFile, err := os.Create(k.logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logFile.Close() }()
	k.cmd = exec.Command(bin, "-config", config, "serve")
	k.cmd.Dir = dir
	k.cmd.Env = realKulaEnv(storage)
	k.cmd.Stdout, k.cmd.Stderr = logFile, logFile
	if err := k.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		k.waitErr = k.cmd.Wait()
		close(k.exited)
	}()
	// Cleanups run last first: stop, then print the log of a failed test.
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("kula serve, config:\n%s\noutput:\n%s", cfg.String(), k.logSummary())
		}
	})
	t.Cleanup(func() { k.stop(t) })

	k.waitHealthy(t)
	return k
}

// client returns an HTTP client that reaches the instance.
func (k *realKula) client() *http.Client {
	transport := &http.Transport{DisableKeepAlives: true}
	if k.socket != "" {
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", k.socket)
		}
	}
	return &http.Client{Timeout: 5 * time.Second, Transport: transport}
}

// waitHealthy waits until /health answers that Kula is healthy.
func (k *realKula) waitHealthy(t *testing.T) {
	t.Helper()
	client := k.client()
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := client.Get(k.url + "/health")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK && string(body) == "kula is healthy" {
				return
			}
		}
		select {
		case <-k.exited:
			t.Fatalf("kula serve exited before /health answered: %v", k.waitErr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("/health did not answer within 30s: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// stop sends SIGTERM and waits for Kula to exit, killing it after 15 s. An
// instance that does not stop cleanly fails the test: a scan must not leave
// Kula unable to shut down.
func (k *realKula) stop(t *testing.T) {
	t.Helper()
	if k.stopped {
		return
	}
	k.stopped = true
	_ = k.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-k.exited:
	case <-time.After(15 * time.Second):
		_ = k.cmd.Process.Kill()
		<-k.exited
		t.Errorf("kula serve did not stop within 15s of SIGTERM")
		return
	}
	if k.waitErr != nil {
		t.Errorf("kula serve exited with %v", k.waitErr)
	}
}

// checkLog checks the stopped instance's output: no handler or collector
// panicked under the scans, and with the AI assistant on, the scans' chat
// requests reached Kula and none of them started a chat or a model call.
func (k *realKula) checkLog(t *testing.T) {
	t.Helper()
	text := k.logText()
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "http: panic serving") || strings.Contains(line, " panicked: ") {
			t.Errorf("kula panicked under the scan: %s", line)
		}
	}
	if !k.setup.ai {
		return
	}
	// web.logging at debug level logs "[Ollama] chat:" as a chat starts,
	// and "[Ollama] POST" or "[Ollama/OpenAI] POST" as it calls the model;
	// the access log shows the answer to each chat request.
	chats := regexp.MustCompile(`POST /api/ollama/chat (\d{3}) `).FindAllStringSubmatch(text, -1)
	if len(chats) == 0 {
		t.Errorf("no chat request reached Kula: the chat probes did not run")
	}
	for _, c := range chats {
		if c[1] == "200" {
			t.Errorf("a chat request was answered 200: a chat started")
			break
		}
	}
	for _, started := range []string{"[Ollama] chat:", "[Ollama] POST ", "[Ollama/OpenAI] POST "} {
		if strings.Contains(text, started) {
			t.Errorf("Kula logged %q: a scan started a chat", started)
		}
	}
}

// logText returns the instance's output.
func (k *realKula) logText() string {
	data, err := os.ReadFile(k.logPath)
	if err != nil {
		return "(no output: " + err.Error() + ")"
	}
	return string(data)
}

// logSummary returns the instance's output for a failure message: every line
// but the access log's, and the last 60 lines of that, each cut at 300 bytes.
// A fuzzed request line can run to kilobytes, and a scan logs thousands.
func (k *realKula) logSummary() string {
	var kept, access []string
	omitted := 0
	for line := range strings.Lines(k.logText()) {
		line = strings.TrimRight(line, "\n")
		if len(line) > 300 {
			line = line[:300] + "…"
		}
		if !strings.Contains(line, " [API] ") && !strings.Contains(line, " [WEB] ") {
			kept = append(kept, line)
			continue
		}
		if access = append(access, line); len(access) > 60 {
			access, omitted = access[1:], omitted+1
		}
	}
	if omitted > 0 {
		kept = append(kept, fmt.Sprintf("(the access log's first %d lines omitted)", omitted))
	}
	return strings.Join(append(kept, access...), "\n")
}
