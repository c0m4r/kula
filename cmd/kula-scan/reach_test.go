package main

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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

func TestBasePathIsNormalised(t *testing.T) {
	for _, c := range []struct{ target, basePath, want string }{
		{"http://kula.test:27960", "kula", "/kula"},
		{"http://kula.test:27960", "/kula/", "/kula"},
		{"http://kula.test:27960", "kula//", "/kula"},
		{"http://kula.test:27960", "//kula", "/kula"},
		{"http://kula.test:27960", "monitoring/kula", "/monitoring/kula"},
		{"http://kula.test:27960", "/", ""},
		{"http://kula.test:27960", "", ""},
		{"http://kula.test:27960/kula/", "", "/kula"},
		{"http://kula.test:27960/", "", ""},
		{"http://kula.test:27960/other", "kula", "/kula"}, // -base-path wins
	} {
		s, err := NewScanner(Options{Target: c.target, BasePath: c.basePath})
		if err != nil {
			t.Errorf("%s with -base-path %q: %v", c.target, c.basePath, err)
			continue
		}
		if s.base.Path != c.want || s.urlFor("/api/config") != "http://kula.test:27960"+c.want+"/api/config" {
			t.Errorf("%s with -base-path %q: base path %q, requests %s; want %q", c.target, c.basePath, s.base.Path, s.urlFor("/api/config"), c.want)
		}
	}
}

func TestBadBasePathsAreUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"-base-path", "kula?x=1", "http://kula-scan.invalid"},
		{"-base-path", "/kula#top", "http://kula-scan.invalid"},
		{"-base-path", "/ku la", "http://kula-scan.invalid"},
		{"-base-path", "/kula\t", "http://kula-scan.invalid"},
		{"-base-path", "/kula\r\nX-Injected: 1", "http://kula-scan.invalid"},
		{"-base-path", "http://kula-scan.invalid/kula", "http://kula-scan.invalid"},
		{"-base-path", "https:/kula", "http://kula-scan.invalid"},
		{"http://kula-scan.invalid/ku%20la"},
	} {
		code, stdout, stderr := runCaptured(t, args...)
		if code != exitUsage || stdout != "" {
			t.Errorf("%q: exit %d, stdout %q; want a usage error", args, code, stdout)
		}
		if !strings.Contains(stderr, "base path") && !strings.Contains(stderr, "-base-path") {
			t.Errorf("%q: the error does not name the base path:\n%s", args, stderr)
		}
	}
}

// Extra header values the tests send: secrets, which no output may show.
const (
	gatewayToken  = "gw-token-s3cret"
	gatewayCookie = "gw=c00kie-s3cret"
)

// TestExtraHeadersReachEveryRequest: -H headers travel on HTTP, raw-socket
// and WebSocket requests alike, a -H cookie with the session's, and their
// values appear nowhere in the output.
func TestExtraHeadersReachEveryRequest(t *testing.T) {
	rec := &requestRecorder{}
	ts := httptest.NewServer(rec.wrap(secureMock()))
	defer ts.Close()
	t.Setenv(passwordEnv, mockPass)

	code, stdout, stderr := runCaptured(t, "-json", "-v", "-username", mockUser,
		"-H", "x-gateway-token: "+gatewayToken, "-H", "Cookie:"+gatewayCookie,
		"-only", "headers,auth,csrf,traversal,metrics,ws", ts.URL)
	if code != exitClean {
		t.Fatalf("exit %d; stderr:\n%s", code, stderr)
	}
	report := decodeReport(t, stdout)
	for _, id := range []string{"AUTH-005", "CSRF-003", "TRAV-001", "WS-000", "WS-002"} {
		wantStatus(t, report.Findings, id, StatusPass)
	}
	if got := strings.Join(report.Options.Headers, ","); got != "X-Gateway-Token,Cookie" {
		t.Errorf("options.headers = %q, want the names X-Gateway-Token,Cookie", got)
	}
	for _, secret := range []string{gatewayToken, gatewayCookie} {
		if strings.Contains(stdout, secret) || strings.Contains(stderr, secret) {
			t.Errorf("the output shows the header value %q", secret)
		}
	}
	if !strings.Contains(stderr, "GET /metrics -> 401") {
		t.Errorf("-v logged no requests:\n%s", stderr)
	}

	requests := rec.requests()
	session := 0
	for _, r := range requests {
		if got := r.header.Values("X-Gateway-Token"); len(got) != 1 || got[0] != gatewayToken {
			t.Errorf("%s carried X-Gateway-Token %q", r.path, got)
		}
		cookie := r.header.Get("Cookie")
		switch {
		case cookie == gatewayCookie+"; kula_session="+mockToken:
			session++
		case !strings.HasPrefix(cookie, gatewayCookie):
			t.Errorf("%s carried Cookie %q, want the -H cookie first", r.path, cookie)
		}
	}
	if session == 0 {
		t.Error("no request carried the -H cookie together with the session cookie")
	}
	if raw, ws := sawRawAndWebSocket(requests); !raw || !ws {
		t.Errorf("the target saw a raw-socket request: %v, a WebSocket handshake: %v", raw, ws)
	}
}

// TestProbeHeadersWinOverExtraHeaders: a probe that sets a header itself
// sends its own value, not the -H one.
func TestProbeHeadersWinOverExtraHeaders(t *testing.T) {
	rec := &requestRecorder{}
	ts := httptest.NewServer(rec.wrap(secureMock()))
	defer ts.Close()

	code, _, stderr := runCaptured(t, "-json", "-H", "Authorization: Basic Z2F0ZXdheQ==",
		"-H", "Content-Type: text/plain", "-only", "metrics,csrf", ts.URL)
	if code != exitClean {
		t.Fatalf("exit %d; stderr:\n%s", code, stderr)
	}
	var basic, bearer, plain, json bool
	for _, r := range rec.requests() {
		authorization, contentType := r.header.Values("Authorization"), r.header.Values("Content-Type")
		if len(authorization) != 1 || len(contentType) != 1 {
			t.Errorf("%s carried Authorization %q and Content-Type %q, want one of each", r.path, authorization, contentType)
			continue
		}
		basic = basic || (r.path == "/metrics" && authorization[0] == "Basic Z2F0ZXdheQ==")
		bearer = bearer || (r.path == "/metrics" && strings.HasPrefix(authorization[0], "Bearer wrong-"))
		plain = plain || (r.path != "/api/login" && contentType[0] == "text/plain")
		json = json || (r.path == "/api/login" && contentType[0] == "application/json")
		if r.path == "/api/login" && contentType[0] != "application/json" {
			t.Errorf("a login probe carried Content-Type %q, not its own application/json", contentType[0])
		}
	}
	if !basic || !bearer || !plain || !json {
		t.Errorf("-H Authorization on the tokenless request: %v; the probe's wrong bearer token: %v; "+
			"-H Content-Type where the probe sets none: %v; the login probe's own Content-Type: %v", basic, bearer, plain, json)
	}
}

// headCapture accepts connections and records the first bytes each sends,
// up to the end of the request head or a short pause, then answers 400.
func headCapture(t *testing.T) (string, func() []string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	var mu sync.Mutex
	var heads []string
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				var head []byte
				buf := make([]byte, 4096)
				for len(head) < 64<<10 && !strings.Contains(string(head), "\r\n\r\n") {
					_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
					n, err := conn.Read(buf)
					head = append(head, buf[:n]...)
					if err != nil {
						break
					}
				}
				mu.Lock()
				heads = append(heads, string(head))
				mu.Unlock()
				_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
				_, _ = conn.Write([]byte("HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
			}()
		}
	}()
	return "http://" + listener.Addr().String(), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), heads...)
	}
}

// TestExtraHeadersReachRawRequests: the request heads the raw-socket probes
// write by hand carry the -H headers after their Host line.
func TestExtraHeadersReachRawRequests(t *testing.T) {
	target, heads := headCapture(t)
	s, err := NewScanner(Options{Target: target, Timeout: 5 * time.Second, DoSWait: time.Second,
		Headers: []string{"X-Gateway-Token: " + gatewayToken}})
	if err != nil {
		t.Fatal(err)
	}
	s.rawTCP("GET /style.css HTTP/1.1")
	runSlowlorisCheck(s)
	runHeaderBombCheck(s)
	runFuzzSmuggle(s)

	want := "\r\nX-Gateway-Token: " + gatewayToken + "\r\n"
	captured := heads()
	for _, head := range captured {
		if !strings.Contains(head, want) {
			t.Errorf("a request head lacks the -H header:\n%q", head)
		}
	}
	for _, probe := range []string{"GET /style.css", "X-Slowloris", "X-Bomb", "Transfer-Encoding: chunked"} {
		if !slices.ContainsFunc(captured, func(head string) bool { return strings.Contains(head, probe) }) {
			t.Errorf("the target saw no request head with %q", probe)
		}
	}
}

func TestBadExtraHeadersAreUsageErrors(t *testing.T) {
	for _, c := range []struct{ header, want string }{
		{"X-Token: a\r\nX-Injected: 1", "line break"},
		{"X-Token: a\nb", "line break"},
		{"X-Token: a\x00b", "control character"},
		{"X-Token-s3cret", "Name: value"},
		{": s3cret", "Name: value"},
		{"Bad Name: s3cret", "Name: value"},
		{"Authorization Bearer s3cret:x", "Name: value"},
		{"X-Token:", "no value"},
		{"X-Token: \t ", "no value"},
		{"Host: other.example", "-connect"},
		{"host: other.example", "-connect"},
		{"Content-Length: 5", "Content-Length"},
		{"Transfer-Encoding: chunked", "Transfer-Encoding"},
		{"Connection: keep-alive", "Connection"},
		{"Sec-WebSocket-Key: x", "Sec-Websocket-Key"},
		// Headers the probes send or leave out on purpose.
		{"Origin: https://s3cret.example", "-H Origin: the CSRF, CORS and WebSocket probes send or omit this header on purpose"},
		{"referer: https://s3cret.example/", "-H Referer: the CSRF, CORS and WebSocket probes send or omit this header on purpose"},
		{"X-CSRF-Token: s3cret", "-H X-Csrf-Token: the CSRF, CORS and WebSocket probes send or omit this header on purpose"},
		{"Cookie: kula_session=s3cret", "-H Cookie: it carries a kula_session cookie; the scanner's session comes from -username"},
		{"Cookie: gw=s3cret;kula_session=s3cret", "kula_session"},
		{"Cookie: gw=s3cret; kula_session = s3cret; other=1", "kula_session"},
	} {
		code, stdout, stderr := runCaptured(t, "-H", "X-Fine: 1", "-H", c.header, "http://kula-scan.invalid")
		if code != exitUsage || stdout != "" {
			t.Errorf("-H %q: exit %d, stdout %q; want a usage error", c.header, code, stdout)
		}
		if !strings.Contains(stderr, c.want) {
			t.Errorf("-H %q: the error does not say %q:\n%s", c.header, c.want, stderr)
		}
		if strings.Contains(stderr, "s3cret") {
			t.Errorf("-H %q: the error shows the value:\n%s", c.header, stderr)
		}
	}

	// Other cookies, a cookie merely named like the session's, and a
	// gateway's Authorization stay allowed.
	for _, header := range []string{"Cookie: gw=1; kula_session_hint=2", "Cookie: gw=kula_session", "Authorization: Basic Z2F0ZXdheQ=="} {
		if _, err := NewScanner(Options{Target: "http://kula-scan.invalid", Headers: []string{header}}); err != nil {
			t.Errorf("-H %q refused: %v", header, err)
		}
	}
}

// closedAddress returns an address nothing listens on.
func closedAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	return address
}

// hangUpListener reads each request, then closes the connection without an
// answer, as a server rejecting it at the protocol layer does. With
// answerFirst > 0 it instead answers that many requests 200 and stops
// listening before it answers the last of them.
func hangUpListener(t *testing.T, answerFirst int) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for served := 1; ; served++ {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			_, _ = conn.Read(make([]byte, 4096))
			if answerFirst > 0 {
				if served == answerFirst {
					_ = listener.Close()
				}
				_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 6\r\nConnection: close\r\n\r\nbody{}"))
			}
			_ = conn.Close()
		}
	}()
	return listener.Addr().String()
}

func TestRawHelpersTellUndeliveredFromRejected(t *testing.T) {
	closed, err := NewScanner(Options{Target: "http://" + closedAddress(t), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if status, _, err := closed.rawTCP("GET / HTTP/1.1"); err == nil || status != 0 {
		t.Errorf("rawTCP to a closed port: status %d, error %v; want an error", status, err)
	}
	if status, _, err := closed.rawExchange("GET / HTTP/1.1\r\nHost: x\r\n\r\n"); err == nil || status != 0 {
		t.Errorf("rawExchange to a closed port: status %d, error %v; want an error", status, err)
	}

	hangUp, err := NewScanner(Options{Target: "http://" + hangUpListener(t, 0), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if status, _, err := hangUp.rawTCP("GET / HTTP/1.1"); err != nil || status != 0 {
		t.Errorf("rawTCP to a target that hangs up: status %d, error %v; want status 0 and no error", status, err)
	}
	if status, _, err := hangUp.rawExchange("GET / HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil || status != 0 {
		t.Errorf("rawExchange to a target that hangs up: status %d, error %v; want status 0 and no error", status, err)
	}
}

// TestUndeliveredProbesAreErrors: raw-socket probes that could not send
// their requests report an ERROR, never a PASS.
func TestUndeliveredProbesAreErrors(t *testing.T) {
	s, err := NewScanner(Options{Target: "http://" + closedAddress(t), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	traversal := runTraversalChecks(s)
	for _, id := range []string{"TRAV-000", "TRAV-001", "TRAV-002"} {
		wantStatus(t, traversal, id, StatusError)
	}
	if f, _ := byID(traversal, "TRAV-001"); !strings.Contains(f.Detail, "could not deliver") {
		t.Errorf("TRAV-001 does not say why: %q", f.Detail)
	}
	wantStatus(t, runFuzzSmuggle(s), "FUZZ-SMUGGLE", StatusError)

	// The control request is delivered, then the target goes away.
	s, err = NewScanner(Options{Target: "http://" + hangUpListener(t, 1), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	traversal = runTraversalChecks(s)
	if _, ok := byID(traversal, "TRAV-000"); ok {
		t.Errorf("the control answered 200, yet TRAV-000 is reported: %+v", traversal)
	}
	wantStatus(t, traversal, "TRAV-001", StatusError)
	wantStatus(t, traversal, "TRAV-002", StatusError)
	if f, _ := byID(traversal, "TRAV-001"); !strings.Contains(f.Detail, "8 of 8 traversal payloads could not be delivered") {
		t.Errorf("TRAV-001 does not count the payloads not delivered: %q", f.Detail)
	}

	// A target that takes each request and hangs up rejected them all.
	s, err = NewScanner(Options{Target: "http://" + hangUpListener(t, 0), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	traversal = runTraversalChecks(s)
	wantStatus(t, traversal, "TRAV-000", StatusWarn)
	wantStatus(t, traversal, "TRAV-001", StatusPass)
	wantStatus(t, traversal, "TRAV-002", StatusPass)
}
