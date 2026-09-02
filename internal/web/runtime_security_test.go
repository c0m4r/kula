package web

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"kula/internal/collector"
	"kula/internal/config"
	"kula/internal/storage"
)

// Active runtime security tests.
//
// Unlike the unit tests that call individual middleware/handlers in isolation,
// these spin up the *whole* production handler chain via Server.buildHandler()
// behind a real httptest.Server and probe it the way an attacker (or a browser)
// would over the wire: full requests, real cookies, real Origin/Referer
// headers, raw byte-level paths. They assert the end-to-end behavior of the
// security middleware stack (security headers → base path → CORS → auth → CSRF
// → handlers) rather than any single layer.

const (
	secUser  = "admin"
	secPass  = "correct horse battery staple"
	secSalt  = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	secToken = "metrics-bearer-secret"
	// secSessionMaxLifetime is the absolute session ceiling these servers run
	// with. Long enough never to fire on its own during a test run; tests that
	// care about it backdate a session's createdAt past it.
	secSessionMaxLifetime = time.Hour
)

// newSecuredTestServer builds a Server with authentication, origin validation,
// security headers, and a Prometheus bearer token all enabled, then serves the
// production handler chain through an httptest.Server. store may be nil for
// tests that never reach /metrics or /api/history.
func newSecuredTestServer(t *testing.T, store *storage.Store) (*Server, *httptest.Server) {
	t.Helper()
	// Deliberately weak Argon2 params: these tests hash on every login and we
	// only care about correctness, not cost.
	params := config.Argon2Config{Time: 1, Memory: 8, Threads: 1}
	cfg := config.WebConfig{
		Enabled:                true,
		UI:                     true,
		EnableCompression:      false, // keep response bodies/headers inspectable
		MaxWebsocketConns:      100,
		MaxWebsocketConnsPerIP: 5,
		Security: config.SecurityConfig{
			Headers:          true,
			FrameProtection:  true,
			OriginValidation: true,
		},
		Auth: config.AuthConfig{
			Enabled:            true,
			Username:           secUser,
			PasswordHash:       HashPassword(secPass, secSalt, params),
			PasswordSalt:       secSalt,
			SessionTimeout:     time.Hour,
			SessionMaxLifetime: secSessionMaxLifetime,
			Argon2:             params,
		},
		PrometheusMetrics: config.MetricsConfig{Enabled: true, Token: secToken},
	}
	s := NewServer(cfg, config.GlobalConfig{Hostname: "testhost"}, &collector.Collector{}, store, t.TempDir(), config.OllamaConfig{})
	ts := httptest.NewServer(s.buildHandler())
	t.Cleanup(ts.Close)
	return s, ts
}

// loginResult captures everything a caller needs from a login attempt.
type loginResult struct {
	status  int
	body    string
	session string // value of the kula_session cookie, "" if none issued
	csrf    string // csrf_token from the JSON body, "" if none
}

// login performs POST /api/login with a same-origin Origin header (so the CSRF
// middleware admits it) and returns the parsed result.
func login(t *testing.T, ts *httptest.Server, username, password string) loginResult {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{"username": username, "password": password})
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/login", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("build login request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", ts.URL)

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("login request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)

	res := loginResult{status: resp.StatusCode, body: string(raw)}
	for _, c := range resp.Cookies() {
		if c.Name == "kula_session" {
			res.session = c.Value
		}
	}
	var parsed struct {
		CSRFToken string `json:"csrf_token"`
	}
	_ = json.Unmarshal(raw, &parsed)
	res.csrf = parsed.CSRFToken
	return res
}

// get issues a GET with an optional session cookie and returns status + body.
func get(t *testing.T, ts *httptest.Server, path, session string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatalf("build GET %s: %v", path, err)
	}
	if session != "" {
		req.Header.Set("Cookie", "kula_session="+session)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// TestRuntimeUnauthenticatedAPIBlocked verifies every protected API route is
// gated by auth: with no credentials the middleware must answer 401 before any
// handler runs.
func TestRuntimeUnauthenticatedAPIBlocked(t *testing.T) {
	_, ts := newSecuredTestServer(t, nil)

	for _, path := range []string{"/api/current", "/api/history", "/api/config", "/api/i18n?lang=en"} {
		if code, _ := get(t, ts, path, ""); code != http.StatusUnauthorized {
			t.Errorf("GET %s without session = %d, want 401", path, code)
		}
	}
}

// TestRuntimeForgedSessionRejected verifies that garbage or guessed cookie
// values do not authenticate.
func TestRuntimeForgedSessionRejected(t *testing.T) {
	_, ts := newSecuredTestServer(t, nil)

	for _, tok := range []string{
		"deadbeef",
		"",
		strings.Repeat("a", 64),
		"../../etc/passwd",
		"' OR '1'='1",
	} {
		if code, _ := get(t, ts, "/api/i18n?lang=en", tok); code != http.StatusUnauthorized {
			t.Errorf("GET with forged session %q = %d, want 401", tok, code)
		}
	}
}

// TestRuntimeLoginGrantsAccess verifies the full happy path: correct creds yield
// a session cookie that the auth middleware subsequently accepts on a protected
// route.
func TestRuntimeLoginGrantsAccess(t *testing.T) {
	_, ts := newSecuredTestServer(t, nil)

	res := login(t, ts, secUser, secPass)
	if res.status != http.StatusOK {
		t.Fatalf("login status = %d, want 200 (body %q)", res.status, res.body)
	}
	if res.session == "" {
		t.Fatal("login did not issue a kula_session cookie")
	}
	if res.csrf == "" {
		t.Fatal("login did not return a csrf_token")
	}

	// The issued session must now pass the auth middleware on a protected route.
	if code, _ := get(t, ts, "/api/i18n?lang=en", res.session); code != http.StatusOK {
		t.Errorf("authenticated GET /api/i18n = %d, want 200", code)
	}
}

// TestRuntimeUsernameEnumerationResistance is a regression guard for the login
// timing/enumeration fix: a wrong password for an existing username and a wrong
// password for a nonexistent username must be indistinguishable in the response
// (same status, same body). Any divergence reintroduces a username oracle.
func TestRuntimeUsernameEnumerationResistance(t *testing.T) {
	_, ts := newSecuredTestServer(t, nil)

	existing := login(t, ts, secUser, "wrong-password")
	missing := login(t, ts, "no-such-user", "wrong-password")

	if existing.status != http.StatusUnauthorized || missing.status != http.StatusUnauthorized {
		t.Fatalf("statuses = %d / %d, want 401 / 401", existing.status, missing.status)
	}
	if existing.body != missing.body {
		t.Errorf("response body differs by username existence (enumeration oracle):\n existing: %q\n missing:  %q", existing.body, missing.body)
	}
	if existing.session != "" || missing.session != "" {
		t.Error("a failed login issued a session cookie")
	}
}

// TestRuntimeLoginRequiresOrigin verifies the CSRF middleware blocks a
// state-changing login that carries no Origin/Referer at all.
func TestRuntimeLoginRequiresOrigin(t *testing.T) {
	_, ts := newSecuredTestServer(t, nil)

	payload, _ := json.Marshal(map[string]string{"username": secUser, "password": secPass})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/login", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	// No Origin, no Referer.

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("login request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("origin-less login = %d, want 403", resp.StatusCode)
	}
}

// TestRuntimeCSRFProtectsStateChange verifies the synchronizer-token + origin
// defenses on an authenticated, state-changing request (logout):
//   - foreign Origin            → 403 (origin check)
//   - same Origin, no CSRF token → 403 (token check)
//   - same Origin + valid token → 200
func TestRuntimeCSRFProtectsStateChange(t *testing.T) {
	_, ts := newSecuredTestServer(t, nil)

	res := login(t, ts, secUser, secPass)
	if res.status != http.StatusOK {
		t.Fatalf("setup login failed: %d", res.status)
	}

	post := func(origin, csrf string) int {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/logout", nil)
		req.Header.Set("Cookie", "kula_session="+res.session)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("logout POST: %v", err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	if code := post("https://evil.example", res.csrf); code != http.StatusForbidden {
		t.Errorf("logout from foreign origin = %d, want 403", code)
	}
	if code := post(ts.URL, ""); code != http.StatusForbidden {
		t.Errorf("logout with no CSRF token = %d, want 403", code)
	}
	if code := post(ts.URL, res.csrf); code != http.StatusOK {
		t.Errorf("logout with valid origin+CSRF = %d, want 200", code)
	}
}

// TestRuntimeSecurityHeaders verifies the security headers are emitted on a UI
// response and that the CSP nonce is freshly random per request.
func TestRuntimeSecurityHeaders(t *testing.T) {
	_, ts := newSecuredTestServer(t, nil)

	resp, err := ts.Client().Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	h := resp.Header
	_, _ = io.Copy(io.Discard, resp.Body) // drain so the server isn't reset mid-write
	_ = resp.Body.Close()

	if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := h.Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", got)
	}
	if got := h.Get("Referrer-Policy"); got == "" {
		t.Error("Referrer-Policy missing")
	}
	if got := h.Get("Permissions-Policy"); got == "" {
		t.Error("Permissions-Policy missing")
	}
	csp := h.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "nonce-", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q missing %q", csp, want)
		}
	}

	// Nonce must differ between two requests (it gates inline <script> exec).
	nonceRe := regexp.MustCompile(`nonce-([A-Za-z0-9+/=]+)`)
	first := nonceRe.FindStringSubmatch(csp)
	resp2, err := ts.Client().Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("second GET /: %v", err)
	}
	second := nonceRe.FindStringSubmatch(resp2.Header.Get("Content-Security-Policy"))
	_, _ = io.Copy(io.Discard, resp2.Body)
	_ = resp2.Body.Close()
	if first == nil || second == nil {
		t.Fatalf("could not extract nonces: %v / %v", first, second)
	}
	if first[1] == second[1] {
		t.Errorf("CSP nonce reused across requests: %q", first[1])
	}
}

// TestRuntimeStaticTraversalBlocked sends byte-level path-traversal payloads
// straight down a raw socket (bypassing client-side URL cleaning) and verifies
// none of them ever return file content from outside the embedded static dir.
// A legitimate asset is fetched first as a positive control.
func TestRuntimeStaticTraversalBlocked(t *testing.T) {
	_, ts := newSecuredTestServer(t, nil)

	// Positive control: a real embedded asset is served.
	if code, _ := rawRequest(t, ts, "GET /style.css HTTP/1.1"); code != http.StatusOK {
		t.Fatalf("control GET /style.css = %d, want 200 (static serving broken?)", code)
	}

	// Markers that would betray a file read from outside static/.
	leaks := []string{"module kula", "root:x:", "BEGIN", "password_hash", "package web"}

	for _, line := range []string{
		"GET /js/../../../../etc/passwd HTTP/1.1",
		"GET /js/..%2f..%2f..%2fgo.mod HTTP/1.1",
		"GET /js/%2e%2e/%2e%2e/config.yaml HTTP/1.1",
		"GET /style.css/../../server.go HTTP/1.1",
		"GET /../go.mod HTTP/1.1",
		"GET /fonts/....//....//etc/passwd HTTP/1.1",
		"GET /js/..\\..\\..\\config.yaml HTTP/1.1",
	} {
		code, body := rawRequest(t, ts, line)
		if code == http.StatusOK {
			for _, leak := range leaks {
				if strings.Contains(body, leak) {
					t.Errorf("traversal %q returned 200 leaking %q", line, leak)
				}
			}
		}
		// A 200 is only acceptable if it served a known-static file (no leak,
		// already checked above). Traversal should normally 400/404/301.
	}
}

// rawRequest opens a raw TCP connection to the test server and writes a literal
// HTTP/1.1 request line (so the path is sent verbatim, without any client-side
// normalization), returning the status code and body.
func rawRequest(t *testing.T, ts *httptest.Server, requestLine string) (int, string) {
	t.Helper()
	addr := strings.TrimPrefix(ts.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err := fmt.Fprintf(conn, "%s\r\nHost: %s\r\nConnection: close\r\n\r\n", requestLine, addr); err != nil {
		t.Fatalf("write raw request: %v", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		// A malformed request the server rejects at the protocol layer is a
		// perfectly acceptable (safe) outcome for a traversal probe.
		return 0, ""
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(body)
}

// TestRuntimePrometheusTokenEnforced verifies the metrics bearer-token gate
// end-to-end: missing and wrong tokens are 401, the correct token is 200.
func TestRuntimePrometheusTokenEnforced(t *testing.T) {
	store := newTestStoreWithSample(t)
	defer func() { _ = store.Close() }()
	_, ts := newSecuredTestServer(t, store)

	do := func(authz string) (int, http.Header) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/metrics", nil)
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("GET /metrics: %v", err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode, resp.Header
	}

	if code, hdr := do(""); code != http.StatusUnauthorized {
		t.Errorf("/metrics without token = %d, want 401", code)
	} else if !strings.Contains(hdr.Get("WWW-Authenticate"), "Bearer") {
		t.Errorf("missing Bearer challenge on 401")
	}
	if code, _ := do("Bearer wrong-token"); code != http.StatusUnauthorized {
		t.Errorf("/metrics with wrong token = %d, want 401", code)
	}
	if code, _ := do("Bearer " + secToken); code != http.StatusOK {
		t.Errorf("/metrics with correct token = %d, want 200", code)
	}
}

// TestRuntimeWebSocketGates verifies the WebSocket endpoint enforces both auth
// (unauthenticated upgrade rejected) and origin (cross-site WebSocket hijacking
// rejected even for an authenticated session).
func TestRuntimeWebSocketGates(t *testing.T) {
	_, ts := newSecuredTestServer(t, nil)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")

	// 1. No session → auth middleware blocks the upgrade with 401.
	if _, resp, err := websocket.DefaultDialer.Dial(wsURL+"/ws", nil); err == nil {
		t.Error("unauthenticated WebSocket upgrade succeeded, want rejection")
	} else if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated WS status = %v, want 401", statusOf(resp))
	}

	// 2. Authenticated session but foreign Origin → CheckOrigin blocks it (CSWSH).
	res := login(t, ts, secUser, secPass)
	if res.session == "" {
		t.Fatal("setup login failed")
	}
	hdr := http.Header{}
	hdr.Set("Cookie", "kula_session="+res.session)
	hdr.Set("Origin", "https://evil.example")
	if _, resp, err := websocket.DefaultDialer.Dial(wsURL+"/ws", hdr); err == nil {
		t.Error("cross-origin WebSocket upgrade succeeded, want rejection")
	} else if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin WS status = %v, want 403", statusOf(resp))
	}
}

// TestRuntimeWebSocketClosesAtSessionDeadline proves the absolute session lifetime
// reaches connections that are already established. Auth is checked once, at the
// upgrade; a socket opened just before the deadline would otherwise keep streaming
// live metrics indefinitely, right past the ceiling session_max_lifetime promises.
func TestRuntimeWebSocketClosesAtSessionDeadline(t *testing.T) {
	prev := wsSessionRecheckInterval
	wsSessionRecheckInterval = 20 * time.Millisecond
	t.Cleanup(func() { wsSessionRecheckInterval = prev })

	s, ts := newSecuredTestServer(t, nil)
	res := login(t, ts, secUser, secPass)
	if res.session == "" {
		t.Fatal("setup login failed")
	}

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	hdr := http.Header{}
	hdr.Set("Cookie", "kula_session="+res.session)
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL+"/ws", hdr)
	if err != nil {
		t.Fatalf("authenticated WebSocket upgrade failed: %v (%s)", err, statusOf(resp))
	}
	defer func() { _ = conn.Close() }()

	// Age the login past the ceiling. The upgrade slid expiresAt forward, so the
	// session is not idle-expired: only the absolute lifetime can end this socket.
	s.auth.mu.Lock()
	s.auth.sessions[hashToken(res.session)].createdAt = time.Now().Add(-2 * secSessionMaxLifetime)
	s.auth.mu.Unlock()

	// The read must fail because the server hung up — either the close frame it
	// sends or the abrupt EOF that follows it. A *timeout* here means the socket
	// was still happily open, which is the bug this test exists to catch, so it
	// is asserted against explicitly rather than counted as "some error".
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, err = conn.ReadMessage()
	if err == nil {
		t.Fatal("WebSocket kept streaming after the session passed its absolute lifetime")
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatalf("WebSocket stayed open past the session's absolute lifetime: read timed out (%v)", err)
	}
}

func statusOf(resp *http.Response) string {
	if resp == nil {
		return "<no response>"
	}
	return fmt.Sprintf("%d", resp.StatusCode)
}

// TestLoginLimitersBothReachable drives the real /api/login handler to prove the
// two login limiters are independently reachable. Every request here comes from
// one address (the httptest client), so if the IP threshold were <= the per-account
// threshold the IP counter — a superset of every username+IP counter for that
// address — would always trip first and the per-account limiter would be dead code.
func TestLoginLimitersBothReachable(t *testing.T) {
	_, ts := newSecuredTestServer(t, nil)

	attempts := 0
	bad := func(username string) loginResult {
		t.Helper()
		attempts++
		return login(t, ts, username, "wrong password")
	}

	// Spend the per-account budget against the admin account.
	for i := 0; i < loginUserAttemptLimit; i++ {
		if res := bad(secUser); res.status != http.StatusUnauthorized {
			t.Fatalf("attempt %d on %q: status = %d, want 401", i+1, secUser, res.status)
		}
	}

	// The next attempt on that account is throttled, well before the IP budget
	// is spent — this is the per-account limiter firing.
	if res := bad(secUser); res.status != http.StatusTooManyRequests {
		t.Fatalf("attempt %d on %q: status = %d, want 429 from the per-account limiter", attempts, secUser, res.status)
	}
	if attempts >= loginIPAttemptLimit {
		t.Fatalf("per-account limiter fired only after %d attempts, at/past the IP budget of %d", attempts, loginIPAttemptLimit)
	}

	// A different account name from the same address is still served, so the 429
	// above was keyed on the account and not on the caller.
	if res := bad("someone-else"); res.status != http.StatusUnauthorized {
		t.Fatalf("different username from the same IP: status = %d, want 401", res.status)
	}

	// Spend the rest of the IP budget across distinct account names, so no
	// per-account counter ever reaches its own threshold.
	for i := attempts; i < loginIPAttemptLimit; i++ {
		if res := bad(fmt.Sprintf("user-%d", i)); res.status != http.StatusUnauthorized {
			t.Fatalf("attempt %d on a fresh username: status = %d, want 401", attempts, res.status)
		}
	}

	// Now the caller itself is cut off, whatever name it presents.
	if res := bad("yet-another-name"); res.status != http.StatusTooManyRequests {
		t.Fatalf("attempt %d after the IP budget: status = %d, want 429 from the IP limiter", attempts, res.status)
	}
}
