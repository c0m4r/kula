package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// startOneSlotKula starts `kula serve` with auth off, origin validation off
// and one WebSocket per address (web.max_websocket_conns_per_ip: 1), so every
// upgrade the scanner makes needs the slot its previous connection held. It
// reuses the integration test's build and process helpers; realKulaSetup
// has no field for the WebSocket limits, so the config is written here.
func startOneSlotKula(t *testing.T, bin string) *realKula {
	t.Helper()
	dir := t.TempDir()
	storage := filepath.Join(dir, "storage")
	k := &realKula{logPath: filepath.Join(dir, "kula.log"), exited: make(chan struct{})}
	k.port = strconv.Itoa(closedPort(t))
	k.addr = "127.0.0.1:" + k.port
	k.url = "http://" + k.addr

	cfg := fmt.Sprintf(`storage:
  directory: %q
  tiers:
    - resolution: 1s
      max_size: 1MB
applications:
  containers:
    enabled: false
web:
  listen: 127.0.0.1
  port: %s
  max_websocket_conns_per_ip: 1
  logging:
    enabled: true
    level: access
  auth:
    enabled: false
  security:
    origin_validation: false
`, storage, k.port)
	config := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(config, []byte(cfg), 0o600); err != nil {
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
			t.Logf("kula serve, config:\n%s\noutput:\n%s", cfg, k.logSummary())
		}
	})
	t.Cleanup(func() { k.stop(t) })
	k.waitHealthy(t)
	return k
}

// lateTeardownProxy relays TCP connections to target and returns its
// address. Data passes at once both ways, and so does a close by the target;
// but when a client ends its connection, the proxy holds the upstream one
// open for linger more, as a reverse proxy may before it tears down the
// connection of a client that vanished.
func lateTeardownProxy(t *testing.T, target string, linger time.Duration) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				upstream, err := net.Dial("tcp", target)
				if err != nil {
					_ = client.Close()
					return
				}
				go func() {
					_, _ = io.Copy(client, upstream)
					_ = client.Close()
				}()
				_, _ = io.Copy(upstream, client)
				time.Sleep(linger)
				_ = upstream.Close()
			}()
		}
	}()
	return listener.Addr().String()
}

// TestWebSocketChecksFitInOneSlot scans a real Kula that admits one
// WebSocket per address, directly and through a proxy that tears down the
// upstream connection of a vanished client late. Each WebSocket check closes
// its connection with a close frame and waits for the target to end it, by
// which time Kula has freed the slot, and retries an upgrade refused with
// 429 once; so the same-origin control (WS-000), the message-size probe
// (WS-MSGBOMB) and the six connections of FUZZ-WS all get their upgrade.
func TestWebSocketChecksFitInOneSlot(t *testing.T) {
	if testing.Short() {
		t.Skip("builds kula and scans a kula serve instance")
	}
	k := startOneSlotKula(t, buildRealKula(t))
	proxy := lateTeardownProxy(t, k.addr, 300*time.Millisecond)
	for name, reach := range map[string][]string{
		"directly":          nil,
		"through the proxy": {"-connect", proxy},
	} {
		args := append([]string{"-json", "-aggressive", "-fuzz", "-only", "WS-000,WS-002,WS-MSGBOMB,FUZZ-WS"}, reach...)
		var stdout, stderr bytes.Buffer
		runCLI(context.Background(), append(args, k.url), &stdout, &stderr)
		report := decodeReport(t, stdout.String())
		for _, id := range []string{"WS-000", "WS-MSGBOMB", "FUZZ-WS"} {
			f, ok := byID(report.Findings, id)
			if !ok || f.Status != StatusPass {
				t.Errorf("%s: %s = %s: %s | %s; want PASS", name, id, f.Status, f.Detail, f.Evidence)
			}
		}
		// Origin validation is off and auth too: any page can open the stream.
		if f, _ := byID(report.Findings, "WS-002"); f.Status != StatusFail {
			t.Errorf("%s: WS-002 = %s: %s; want FAIL", name, f.Status, f.Detail)
		}
	}
	k.stop(t)
	k.checkLog(t)
}
