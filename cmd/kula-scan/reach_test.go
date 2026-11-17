package main

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests cover how the scanner reaches its target: the one dial path,
// the address it dials, the TLS server name it sends, -connect and -unix.

// seenRequest is what a target saw of one request.
type seenRequest struct {
	host, path string
	header     http.Header
	upgrade    bool // a WebSocket handshake
}

// requestRecorder records every request a handler serves.
type requestRecorder struct {
	mu   sync.Mutex
	seen []seenRequest
}

func (rec *requestRecorder) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.seen = append(rec.seen, seenRequest{
			host: r.Host, path: r.URL.Path, header: r.Header.Clone(),
			upgrade: strings.EqualFold(r.Header.Get("Upgrade"), "websocket"),
		})
		rec.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

func (rec *requestRecorder) requests() []seenRequest {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]seenRequest(nil), rec.seen...)
}

// sawRawAndWebSocket reports whether the target saw the traversal check's
// raw-socket control request and a WebSocket handshake.
func sawRawAndWebSocket(requests []seenRequest) (raw, ws bool) {
	for _, r := range requests {
		raw = raw || r.path == "/style.css"
		ws = ws || (r.path == "/ws" && r.upgrade)
	}
	return raw, ws
}

// serveUnix serves handler on a Unix socket in a fresh directory and returns
// the socket's path.
func serveUnix(t *testing.T, handler http.Handler) string {
	t.Helper()
	dir := t.TempDir()
	if len(dir) > 80 { // a socket path has at most 108 bytes
		var err error
		if dir, err = os.MkdirTemp("/tmp", "kula-scan-"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
	}
	path := filepath.Join(dir, "kula.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return path
}

func TestDialAddressFillsTheSchemesPort(t *testing.T) {
	for target, want := range map[string]string{
		"http://[::1]":              "[::1]:80",
		"https://[2001:db8::1]":     "[2001:db8::1]:443",
		"http://[::1]:8080/":        "[::1]:8080",
		"http://[fe80::1%25eth0]/":  "[fe80::1%eth0]:80",
		"http://127.0.0.1":          "127.0.0.1:80",
		"https://kula.example":      "kula.example:443",
		"https://kula.example:8443": "kula.example:8443",
		"http://kula.example:27960": "kula.example:27960",
	} {
		s, err := NewScanner(Options{Target: target})
		if err != nil {
			t.Errorf("%s: %v", target, err)
			continue
		}
		if s.network != "tcp" || s.address != want {
			t.Errorf("%s dials %s %s, want tcp %s", target, s.network, s.address, want)
		}
	}
}

func TestTLSServerNameIsTheURLsHostName(t *testing.T) {
	for target, want := range map[string]string{
		"https://kula.example:8443/kula": "kula.example",
		"https://[2001:db8::1]":          "2001:db8::1",
		"https://127.0.0.1":              "127.0.0.1",
	} {
		s, err := NewScanner(Options{Target: target})
		if err != nil {
			t.Fatal(err)
		}
		if got := s.tlsConfig().ServerName; got != want {
			t.Errorf("%s: ServerName %q, want %q", target, got, want)
		}
	}
}

// silentListener accepts connections and never answers on them.
func silentListener(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		var held []net.Conn
		defer func() {
			for _, c := range held {
				_ = c.Close()
			}
		}()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			held = append(held, conn)
		}
	}()
	return listener
}

// TestTLSCheckEndsWithTheRun: a handshake the target never answers is
// abandoned when the run is interrupted, not after -timeout.
func TestTLSCheckEndsWithTheRun(t *testing.T) {
	listener := silentListener(t)
	s, err := NewScanner(Options{Target: "https://" + listener.Addr().String(), Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.ctx = ctx
	timer := time.AfterFunc(200*time.Millisecond, cancel)
	defer timer.Stop()

	start := time.Now()
	findings := runTLSChecks(s)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("the TLS check took %s after the interrupt", elapsed)
	}
	wantStatus(t, findings, "TLS-001", StatusWarn)
}

// TestUnixSocketTarget: with -unix every probe, the raw-socket and WebSocket
// ones too, reaches the socket under the URL's Host.
func TestUnixSocketTarget(t *testing.T) {
	rec := &requestRecorder{}
	socket := serveUnix(t, rec.wrap(secureMock()))
	t.Setenv(passwordEnv, mockPass)

	code, stdout, stderr := runCaptured(t, "-json", "-unix", socket, "-username", mockUser,
		"-only", "headers,auth,csrf,traversal,ws", "http://kula.test/")
	if code != exitClean {
		t.Fatalf("exit %d; stderr:\n%s", code, stderr)
	}
	report := decodeReport(t, stdout)
	for _, id := range []string{"DISC-001", "HDR-001", "AUTH-005", "CSRF-003", "TRAV-001", "TRAV-002", "WS-000", "WS-001", "WS-002"} {
		wantStatus(t, report.Findings, id, StatusPass)
	}
	if report.Target != "http://kula.test/" || report.Options.Unix != socket || report.Options.Connect != "" {
		t.Errorf("target %q, options.unix %q, options.connect %q", report.Target, report.Options.Unix, report.Options.Connect)
	}

	requests := rec.requests()
	for _, r := range requests {
		if r.host != "kula.test" {
			t.Errorf("%s arrived with Host %q, want the URL's kula.test", r.path, r.host)
		}
	}
	if raw, ws := sawRawAndWebSocket(requests); !raw || !ws {
		t.Errorf("the socket saw a raw-socket request: %v, a WebSocket handshake: %v", raw, ws)
	}
}

// TestConnectKeepsTheURLsHost: -connect sends every connection to its
// address, and every request names the URL's host, which need not resolve.
func TestConnectKeepsTheURLsHost(t *testing.T) {
	rec := &requestRecorder{}
	ts := httptest.NewServer(rec.wrap(secureMock()))
	defer ts.Close()
	t.Setenv(passwordEnv, mockPass)
	address := ts.Listener.Addr().String()

	code, stdout, stderr := runCaptured(t, "-json", "-connect", address, "-username", mockUser,
		"-only", "headers,auth,csrf,traversal,ws", "http://kula-scan.invalid:8080/")
	if code != exitClean {
		t.Fatalf("exit %d; stderr:\n%s", code, stderr)
	}
	report := decodeReport(t, stdout)
	for _, id := range []string{"DISC-001", "AUTH-005", "CSRF-002", "CSRF-003", "TRAV-001", "WS-000", "WS-002"} {
		wantStatus(t, report.Findings, id, StatusPass)
	}
	if report.Target != "http://kula-scan.invalid:8080/" || report.Options.Connect != address {
		t.Errorf("target %q, options.connect %q", report.Target, report.Options.Connect)
	}

	requests := rec.requests()
	for _, r := range requests {
		if r.host != "kula-scan.invalid:8080" {
			t.Errorf("%s arrived with Host %q, want the URL's kula-scan.invalid:8080", r.path, r.host)
		}
	}
	if raw, ws := sawRawAndWebSocket(requests); !raw || !ws {
		t.Errorf("the target saw a raw-socket request: %v, a WebSocket handshake: %v", raw, ws)
	}
}

// TestConnectSendsTheURLsNameOverTLS: over TLS every handshake, the TLS
// check's included, names the URL's host, not the address it connects to.
func TestConnectSendsTheURLsNameOverTLS(t *testing.T) {
	var mu sync.Mutex
	var names []string
	rec := &requestRecorder{}
	ts := httptest.NewUnstartedServer(rec.wrap(secureMock()))
	ts.TLS = &tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		mu.Lock()
		names = append(names, hello.ServerName)
		mu.Unlock()
		return nil, nil
	}}
	ts.StartTLS()
	defer ts.Close()
	t.Setenv(passwordEnv, mockPass)

	code, stdout, stderr := runCaptured(t, "-json", "-insecure", "-connect", ts.Listener.Addr().String(),
		"-username", mockUser, "-only", "tls,headers,traversal,ws", "https://kula-scan.invalid/")
	if code != exitClean {
		t.Fatalf("exit %d; stderr:\n%s", code, stderr)
	}
	report := decodeReport(t, stdout)
	for _, id := range []string{"DISC-001", "TLS-001", "TLS-002", "TRAV-001", "WS-000", "WS-002"} {
		wantStatus(t, report.Findings, id, StatusPass)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(names) < 3 {
		t.Errorf("only %d TLS handshakes reached the target", len(names))
	}
	for _, name := range names {
		if name != "kula-scan.invalid" {
			t.Errorf("a handshake sent the server name %q, want kula-scan.invalid", name)
		}
	}
	requests := rec.requests()
	for _, r := range requests {
		if r.host != "kula-scan.invalid" {
			t.Errorf("%s arrived with Host %q, want kula-scan.invalid", r.path, r.host)
		}
	}
	if raw, ws := sawRawAndWebSocket(requests); !raw || !ws {
		t.Errorf("the target saw a raw-socket request: %v, a WebSocket handshake: %v", raw, ws)
	}
}

func TestBadConnectionFlagsAreUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"-connect", "127.0.0.1:27960", "-unix", "/run/kula/kula.sock"},
		{"-connect", "127.0.0.1"},
		{"-connect", ":27960"},
		{"-connect", "127.0.0.1:0"},
		{"-connect", "127.0.0.1:65536"},
		{"-connect", "127.0.0.1:http"},
		{"-connect", "http://127.0.0.1:27960"},
	} {
		code, stdout, stderr := runCaptured(t, append(args, "http://kula-scan.invalid")...)
		if code != exitUsage || stdout != "" {
			t.Errorf("%v: exit %d, stdout %q; want a usage error", args, code, stdout)
		}
		if !strings.Contains(stderr, "-connect") {
			t.Errorf("%v: the error does not name -connect:\n%s", args, stderr)
		}
	}
}
