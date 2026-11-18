package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestScanJudgesAKulaWithoutTheUI scans the real `kula serve` with
// web.ui: false, which serves /health, /status and /metrics and answers a
// plain 404 everywhere else. The scan must not read that 404 as auth off:
// the checks that need the UI or API skip, the others pass, and nothing
// fails or errs. -expect auth, which nothing can judge there, is an ERROR.
// It reuses TestScanJudgesTheRealKula's harness (integration_test.go).
func TestScanJudgesAKulaWithoutTheUI(t *testing.T) {
	if testing.Short() {
		t.Skip("builds kula and scans two kula serve instances")
	}
	bin := buildRealKula(t)
	creds := newRealCredentials(t, bin)
	t.Setenv(passwordEnv, creds.password)
	t.Setenv(metricsTokenEnv, creds.token)

	// Without the UI or API, only these judge anything: TLS (skipped over
	// plain HTTP), /metrics, REDIR-001, the connection-level DoS probes,
	// FUZZ-PATH and FUZZ-LIVENESS. Never reported: DISC-002 and DISC-003,
	// which need the auth posture and a refused Host name.
	runsWithoutUI := []string{"DISC-001", "REDIR-001", "PROM-001", "PROM-002", "PROM-003",
		"DOS-SLOWLORIS", "DOS-HEADERBOMB", "DOS-CONNFLOOD", "FUZZ-PATH", "FUZZ-LIVENESS"}
	never := []string{"DISC-002", "DISC-003"}
	slow := os.Getenv(slowScanEnv) == "1"
	var slowSkip, slowSilent []string
	if !slow {
		slowSkip, slowSilent = []string{"-skip", strings.Join(realSlow, ",")}, realSlow
	}
	// Each scan's SKIPs: every ID but those that run, those never reported
	// and those named.
	skipped := func(except ...[]string) []string {
		return allIDsBut(idsOf(append([][]string{runsWithoutUI, never}, except...)...)...)
	}

	postures := []struct {
		name  string
		kula  realKulaSetup
		scans []realScan
	}{{
		name: "auth off, no metrics",
		kula: realKulaSetup{},
		scans: []realScan{{
			name: "default",
			args: func(_ *testing.T, k *realKula) []string { return []string{k.url} },
			want: realScanWant{
				// /metrics is off: PROM-001 and PROM-003 skip, PROM-002 is
				// not reported.
				skip:   idsOf(skipped(), []string{"PROM-001", "PROM-003", "DOS-SLOWLORIS", "DOS-HEADERBOMB", "DOS-CONNFLOOD", "FUZZ-PATH", "FUZZ-LIVENESS"}),
				silent: idsOf(never, []string{"PROM-002"}),
			},
		}},
	}, {
		name: "auth on, metrics with a token",
		kula: realKulaSetup{auth: true, metrics: true},
		scans: []realScan{{
			name: "-aggressive -fuzz",
			args: func(_ *testing.T, k *realKula) []string {
				return argv([]string{"-aggressive", "-fuzz"}, slowSkip, k.url)
			},
			want: realScanWant{
				skip:   skipped(),
				silent: idsOf(never, slowSilent),
			},
		}, {
			name: "-expect auth",
			args: func(_ *testing.T, k *realKula) []string { return []string{"-expect", "auth", k.url} },
			want: realScanWant{
				exit:   exitInconclusive,
				errors: []string{"AUTH-000"},
				skip:   idsOf(skipped([]string{"AUTH-000"}), []string{"DOS-SLOWLORIS", "DOS-HEADERBOMB", "DOS-CONNFLOOD", "FUZZ-PATH", "FUZZ-LIVENESS"}),
				silent: never,
			},
		}},
	}}

	for _, p := range postures {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			k := startKulaWithoutUI(t, bin, p.kula, creds)
			for _, sc := range p.scans {
				t.Run(sc.name, func(t *testing.T) { runRealScan(t, sc, sc.args(t, k)) })
			}
			k.stop(t)
			k.checkLog(t)
		})
	}
}

// startKulaWithoutUI starts `kula serve` with web.ui: false on a local TCP
// port, as startRealKula does for the postures with the UI, with setup's
// auth and metrics, and waits until /health answers.
func startKulaWithoutUI(t *testing.T, bin string, setup realKulaSetup, creds realCredentials) *realKula {
	t.Helper()
	dir := t.TempDir()
	storage := filepath.Join(dir, "storage")
	k := &realKula{setup: setup, logPath: filepath.Join(dir, "kula.log"), exited: make(chan struct{})}
	k.port = strconv.Itoa(closedPort(t))
	k.addr = "127.0.0.1:" + k.port
	k.url = "http://" + k.addr

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
  ui: false
  listen: 127.0.0.1
  port: %s
  logging:
    enabled: true
    level: access
`, storage, k.port)
	if setup.metrics {
		fmt.Fprintf(&cfg, "  prometheus_metrics:\n    enabled: true\n    token: %q\n", creds.token)
	}
	fmt.Fprintf(&cfg, "  auth:\n    enabled: %t\n    username: admin\n    password_hash: %q\n    password_salt: %q\n%s",
		setup.auth, creds.hash, creds.salt, realArgon2)
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
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("kula serve, config:\n%s\noutput:\n%s", cfg.String(), k.logSummary())
		}
	})
	t.Cleanup(func() { k.stop(t) })

	k.waitHealthy(t)
	return k
}
