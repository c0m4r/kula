package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// These tests validate the scanner's classification logic against two faithful
// mock servers: a "secure" one that emulates kula's safeguards (every probe must
// PASS) and an "insecure" one that omits them (the matching probes must FAIL or
// WARN). They exercise the check functions over a real loopback socket, so the
// HTTP, raw-TCP, and WebSocket helpers are all covered end to end without
// needing the full kula binary.

const (
	mockUser  = "admin"
	mockPass  = "secret"
	mockToken = "goodtoken"
)

// ---- secure mock -------------------------------------------------------------

// secureMock models a current, default-configured Kula with auth enabled. Like
// buildHandler it gates every /api/ route but login, logout and auth/status
// behind the session check, unknown ones included; a logout revokes the
// session; and CSRFMiddleware's order holds: the Origin (else Referer) check
// first, then the token, for a request carrying a valid session cookie.
func secureMock() http.Handler {
	mux := http.NewServeMux()

	// One session at a time, issued as mockToken and revoked by logout.
	var revoked atomic.Bool
	valid := func(r *http.Request) bool { return hasValidSession(r) && !revoked.Load() }

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("kula is healthy"))
	})
	mux.HandleFunc("/api/auth/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, map[string]any{"auth_required": true, "authenticated": valid(r)})
	})

	api := http.NewServeMux()
	api.HandleFunc("/api/current", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, map[string]any{"cpu": 1})
	})
	api.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, map[string]any{"auth_enabled": true, "ollama_enabled": false})
	})
	api.HandleFunc("/api/system-info", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, map[string]any{"os": "linux"})
	})
	api.HandleFunc("/api/history", mockHistory)
	api.HandleFunc("/api/i18n", func(w http.ResponseWriter, r *http.Request) {
		lang := r.URL.Query().Get("lang")
		if lang != "en" && lang != "de" {
			http.Error(w, `{"error":"invalid language"}`, http.StatusBadRequest)
			return
		}
		writeJSONResp(w, map[string]any{"hello": "hi"})
	})
	api.HandleFunc("/api/ollama/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"ollama is not enabled"}`, http.StatusServiceUnavailable)
	})
	api.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		if !valid(r) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		api.ServeHTTP(w, r)
	})

	// csrf answers 403 to a state change CSRFMiddleware refuses.
	csrf := func(w http.ResponseWriter, r *http.Request) bool {
		if !sameOriginPost(r) {
			http.Error(w, `{"error":"invalid origin"}`, http.StatusForbidden)
			return false
		}
		if valid(r) && r.Header.Get("X-CSRF-Token") != "csrftok" {
			http.Error(w, `{"error":"invalid csrf token"}`, http.StatusForbidden)
			return false
		}
		return true
	}

	mux.HandleFunc("/api/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		if !csrf(w, r) {
			return
		}
		var creds struct{ Username, Password string }
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&creds)
		if creds.Username != mockUser || creds.Password != mockPass {
			// Constant response regardless of which field is wrong (no enumeration).
			http.Error(w, `{"error":"invalid credentials"}`, http.StatusUnauthorized)
			return
		}
		revoked.Store(false)
		http.SetCookie(w, &http.Cookie{
			Name: "kula_session", Value: mockToken, Path: "/",
			HttpOnly: true, SameSite: http.SameSiteStrictMode,
		})
		writeJSONResp(w, map[string]any{"status": "logged in", "csrf_token": "csrftok"})
	})

	mux.HandleFunc("/api/logout", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		if !csrf(w, r) {
			return
		}
		if hasValidSession(r) {
			revoked.Store(true)
		}
		writeJSONResp(w, map[string]any{"status": "logged out"})
	})

	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") ||
			strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") != "metricssecret" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="kula-metrics"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte("kula_cpu_usage_percent 1\n"))
	})

	mux.HandleFunc("/style.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		_, _ = w.Write([]byte("body{color:#000}"))
	})

	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		if !valid(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		up := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool {
			o := r.Header.Get("Origin")
			if o == "" {
				return true
			}
			u, err := url.Parse(o)
			return err == nil && u.Host == r.Host
		}}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		// Mirror kula's read limit so the WS-MSGBOMB probe has something to assert
		// against: an oversized client message trips ErrReadLimit and gorilla
		// sends a 1009 close frame, dropping the connection.
		c.SetReadLimit(4096)
		go func() {
			defer func() { _ = c.Close() }()
			for {
				if _, _, err := c.ReadMessage(); err != nil {
					return
				}
			}
		}()
	})

	// Everything else (including the index) returns 200 so header checks have a page.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		// The page carries the response's nonce: never cached.
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte("<html><body>kula</body></html>"))
	})

	return secureHeaders(mux)
}

// kulaCSP is the policy Kula sends (contentSecurityPolicy in
// internal/web/server.go) with frame protection on.
func kulaCSP(nonce string) string {
	return fmt.Sprintf("default-src 'self'; script-src 'self' 'nonce-%s'; style-src 'self'; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none';", nonce)
}

// secureHeaders wraps h with kula's security headers (fresh nonce per request)
// and marks API responses no-store, as Kula's noStore does.
func secureHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		nonce := base64.StdEncoding.EncodeToString(b)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", kulaCSP(nonce))
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
		if r.TLS != nil {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		h.ServeHTTP(w, r)
	})
}

// mockHistory validates /api/history as handleHistory does and answers one
// sample per second of the window, capped at the 5000 points Kula allows.
var mockHistory = historyHandler(5000)

// historyHandler is mockHistory with a points cap of maxPoints.
func historyHandler(maxPoints int) http.HandlerFunc {
	sections := map[string]bool{"cpu": true, "lavg": true, "mem": true, "swap": true, "net": true, "disk": true,
		"sys": true, "proc": true, "self": true, "gpu": true, "psu": true, "apps": true}
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if raw := q.Get("sections"); raw != "" {
			for _, name := range strings.Split(raw, ",") {
				if name = strings.TrimSpace(name); name != "" && !sections[name] {
					http.Error(w, `{"error":"unknown history section"}`, http.StatusBadRequest)
					return
				}
			}
		}
		to, from := time.Now(), time.Time{}
		if raw := q.Get("to"); raw != "" {
			t, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				http.Error(w, `{"error":"invalid 'to' time"}`, http.StatusBadRequest)
				return
			}
			to = t
		}
		from = to.Add(-5 * time.Minute)
		if raw := q.Get("from"); raw != "" {
			f, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				http.Error(w, `{"error":"invalid 'from' time"}`, http.StatusBadRequest)
				return
			}
			from = f
		}
		if to.Before(from) || to.Sub(from) > 31*24*time.Hour {
			http.Error(w, `{"error":"bad range"}`, http.StatusBadRequest)
			return
		}
		points := 450
		if raw := q.Get("points"); raw != "" {
			_, _ = fmt.Sscanf(raw, "%d", &points)
		}
		points = max(1, min(points, maxPoints))
		if raw := q.Get("tier"); raw != "" {
			if v, err := strconv.Atoi(raw); err != nil || v < 0 {
				http.Error(w, `{"error":"invalid 'tier'"}`, http.StatusBadRequest)
				return
			}
		}
		n := min(points, int(to.Sub(from)/time.Second))
		samples := make([]map[string]any, n)
		for i := range samples {
			samples[i] = map[string]any{"ts": i, "data": map[string]any{"lavg": map[string]any{"load1": 0.1}}}
		}
		writeJSONResp(w, map[string]any{"samples": samples, "resolution": "2s", "downsampled": true})
	}
}

func hasValidSession(r *http.Request) bool {
	c, err := r.Cookie("kula_session")
	if err == nil && c.Value == mockToken {
		return true
	}
	authz := r.Header.Get("Authorization")
	return strings.HasPrefix(authz, "Bearer ") && strings.TrimPrefix(authz, "Bearer ") == mockToken
}

func sameOriginPost(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		o = r.Header.Get("Referer")
	}
	if o == "" {
		return false
	}
	u, err := url.Parse(o)
	return err == nil && u.Host == r.Host
}

func writeJSONResp(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// ---- insecure mock -----------------------------------------------------------

// insecureMock is a single handler (no ServeMux, so dot-dot paths are NOT
// cleaned/redirected) wrapped in a credential-reflecting CORS middleware. It
// omits security headers, leaves the API open, and serves file content for
// traversal paths — every safeguard is deliberately broken.
func insecureMock() http.Handler {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case p == "/health":
			_, _ = w.Write([]byte("kula is healthy"))
		case p == "/api/auth/status":
			writeJSONResp(w, map[string]any{"auth_required": true}) // claims auth, enforces none
		case p == "/api/config":
			writeJSONResp(w, map[string]any{"auth_enabled": true, "ollama_enabled": false})
		case p == "/api/login":
			http.SetCookie(w, &http.Cookie{Name: "kula_session", Value: "anything", Path: "/"})
			writeJSONResp(w, map[string]any{"status": "logged in"}) // no origin check
		case p == "/metrics":
			_, _ = w.Write([]byte("kula_cpu_usage_percent 1\n")) // no token
		case p == "/style.css":
			_, _ = w.Write([]byte("body{}"))
		case strings.Contains(p, "etc/passwd"):
			_, _ = w.Write([]byte("root:x:0:0:root:/root:/bin/bash")) // traversal leak
		case strings.Contains(p, "go.mod"):
			_, _ = w.Write([]byte("module kula"))
		case strings.HasPrefix(p, "/api/"):
			writeJSONResp(w, map[string]any{"data": 1}) // open API, incl. /api/current,/history,/i18n
		default:
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>kula</html>"))
		}
	})
	// Reflect any Origin with credentials — a textbook CORS misconfiguration.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" {
			w.Header().Set("Access-Control-Allow-Origin", o)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}
		h.ServeHTTP(w, r)
	})
}

// ---- harness -----------------------------------------------------------------

func scannerFor(t *testing.T, ts *httptest.Server, user, pass string) *Scanner {
	t.Helper()
	s, err := NewScanner(Options{
		Target: ts.URL, Username: user, Password: pass,
		Timeout: 5 * time.Second, DoSWait: 3 * time.Second, FuzzIter: 60, Seed: 1,
	})
	if err != nil {
		t.Fatalf("NewScanner: %v", err)
	}
	s.discover()
	return s
}

func byID(findings []Finding, id string) (Finding, bool) {
	for _, f := range findings {
		if f.ID == id {
			return f, true
		}
	}
	return Finding{}, false
}

func wantStatus(t *testing.T, findings []Finding, id string, want Status) {
	t.Helper()
	f, ok := byID(findings, id)
	if !ok {
		t.Errorf("%s: finding not produced", id)
		return
	}
	if f.Status != want {
		t.Errorf("%s (%s): status = %s, want %s — detail: %s | evidence: %s",
			id, f.Title, f.Status, want, f.Detail, f.Evidence)
	}
}

// ---- secure-target tests: everything must PASS -------------------------------

func TestSecureTargetPasses(t *testing.T) {
	ts := httptest.NewServer(secureMock())
	defer ts.Close()
	s := scannerFor(t, ts, mockUser, mockPass)

	hdr := runHeaderChecks(s)
	for _, id := range []string{"HDR-001", "HDR-002", "HDR-003", "HDR-004", "HDR-007", "HDR-010", "HDR-011", "HDR-012", "HDR-013"} {
		wantStatus(t, hdr, id, StatusPass)
	}
	wantStatus(t, hdr, "HDR-008", StatusSkip) // plain HTTP

	auth := runAuthChecks(s)
	wantStatus(t, auth, "AUTH-001", StatusPass)
	wantStatus(t, auth, "AUTH-002", StatusPass)
	wantStatus(t, auth, "AUTH-003", StatusPass)
	wantStatus(t, auth, "AUTH-005", StatusPass)
	wantStatus(t, auth, "AUTH-007", StatusPass) // HttpOnly
	// Enumeration resistance, an aggressive check.
	wantStatus(t, []Finding{enumerationFinding(s)}, "AUTH-006", StatusPass)

	csrf := runCSRFChecks(s)
	for _, id := range []string{"CSRF-001", "CSRF-002", "CSRF-003", "CSRF-004"} {
		wantStatus(t, csrf, id, StatusPass)
	}

	cors := runCORSChecks(s)
	wantStatus(t, cors, "CORS-001", StatusPass)
	wantStatus(t, cors, "CORS-003", StatusPass)

	trav := runTraversalChecks(s)
	wantStatus(t, trav, "TRAV-001", StatusPass)
	wantStatus(t, trav, "TRAV-002", StatusPass)

	// Secure mock requires the token "metricssecret".
	prom := runMetricsChecks(s)
	wantStatus(t, prom, "PROM-001", StatusPass)
	wantStatus(t, prom, "PROM-002", StatusPass)

	ws := runWSChecks(s)
	wantStatus(t, ws, "WS-001", StatusPass)
	wantStatus(t, ws, "WS-002", StatusPass)

	input := runInputChecks(s)
	wantStatus(t, input, "INPUT-001", StatusPass)
	wantStatus(t, input, "INPUT-003", StatusPass)

	// The closing logout revokes the session the checks shared.
	wantStatus(t, s.closeSession(nil, true), "AUTH-010", StatusPass)
}

// ---- insecure-target tests: the matching probes must FAIL/WARN ---------------

func TestInsecureTargetFails(t *testing.T) {
	ts := httptest.NewServer(insecureMock())
	defer ts.Close()
	s := scannerFor(t, ts, mockUser, "anything") // the insecure login takes any password

	hdr := runHeaderChecks(s)
	for _, id := range []string{"HDR-001", "HDR-002", "HDR-003", "HDR-010"} {
		wantStatus(t, hdr, id, StatusFail)
	}
	for _, id := range []string{"HDR-007", "HDR-011", "HDR-012", "HDR-013"} {
		wantStatus(t, hdr, id, StatusWarn)
	}

	auth := runAuthChecks(s)
	wantStatus(t, auth, "AUTH-001", StatusFail) // open API despite auth_required
	wantStatus(t, auth, "AUTH-002", StatusFail) // forged cookie accepted
	wantStatus(t, auth, "AUTH-007", StatusFail) // cookie readable from script

	// The insecure logout clears nothing on the server: a second session,
	// ended by the closing logout, still authenticates.
	closing := scannerFor(t, ts, mockUser, "anything")
	closing.ensureSession()
	wantStatus(t, closing.closeSession(nil, true), "AUTH-010", StatusFail)

	csrf := runCSRFChecks(s)
	wantStatus(t, csrf, "CSRF-001", StatusWarn) // no origin enforcement
	wantStatus(t, csrf, "CSRF-002", StatusWarn)
	wantStatus(t, csrf, "CSRF-003", StatusFail) // no token needed
	// CSRF-003's accepted logout ended the session; CSRF-004 says so.
	if f, _ := byID(csrf, "CSRF-004"); f.Status != StatusSkip || !strings.Contains(f.Detail, "CSRF-003") {
		t.Errorf("CSRF-004 after the session was lost = %s %q, want a SKIP naming CSRF-003", f.Status, f.Detail)
	}

	cors := runCORSChecks(s)
	if f, ok := byID(cors, "CORS-001"); !ok || f.Status != StatusFail || f.Severity != SevCritical {
		t.Errorf("CORS-001: want FAIL/CRITICAL, got %v", f)
	}
	wantStatus(t, cors, "CORS-003", StatusFail)

	trav := runTraversalChecks(s)
	wantStatus(t, trav, "TRAV-001", StatusFail)

	prom := runMetricsChecks(s)
	wantStatus(t, prom, "PROM-001", StatusWarn) // exposed without token
}

// ---- headers -----------------------------------------------------------------

// TestCSPJudgement: today's Kula policy passes every CSP finding; each
// weakening fails or warns the finding that names it, and no other.
func TestCSPJudgement(t *testing.T) {
	cspIDs := []string{"HDR-003", "HDR-007", "HDR-011", "HDR-012", "HDR-013"}
	cases := []struct {
		name   string
		values []string
		want   map[string]Status // IDs not named must PASS
	}{
		{"today's Kula", []string{kulaCSP("cmFuZG9t")}, nil},
		{"keywords in upper case", []string{strings.ToUpper(kulaCSP("cmFuZG9t"))}, nil},
		{"frame protection off", []string{strings.TrimSuffix(kulaCSP("n"), " frame-ancestors 'none';")},
			map[string]Status{"HDR-007": StatusWarn}},
		{"a 0.21 policy", []string{"default-src 'self'; script-src 'self' 'nonce-n'; style-src 'self'; frame-ancestors 'none';"},
			map[string]Status{"HDR-011": StatusWarn, "HDR-012": StatusWarn, "HDR-013": StatusWarn}},
		{"unsafe-inline scripts", []string{strings.Replace(kulaCSP("n"), "'nonce-n'", "'nonce-n' 'unsafe-inline'", 1)},
			map[string]Status{"HDR-003": StatusFail}},
		{"unsafe-eval scripts", []string{strings.Replace(kulaCSP("n"), "'nonce-n'", "'nonce-n' 'unsafe-eval'", 1)},
			map[string]Status{"HDR-003": StatusFail}},
		{"wildcard scripts", []string{strings.Replace(kulaCSP("n"), "script-src 'self' 'nonce-n'", "script-src *", 1)},
			map[string]Status{"HDR-003": StatusFail}},
		{"https: scripts", []string{strings.Replace(kulaCSP("n"), "'nonce-n'", "'nonce-n' https:", 1)},
			map[string]Status{"HDR-003": StatusFail}},
		{"data: scripts", []string{strings.Replace(kulaCSP("n"), "'nonce-n'", "'nonce-n' data:", 1)},
			map[string]Status{"HDR-003": StatusFail}},
		{"no script nonce", []string{strings.Replace(kulaCSP("n"), " 'nonce-n'", "", 1)},
			map[string]Status{"HDR-003": StatusFail}},
		{"no script-src, so default-src without a nonce", []string{strings.Replace(kulaCSP("n"), " script-src 'self' 'nonce-n';", "", 1)},
			map[string]Status{"HDR-003": StatusFail}},
		{"wide default-src", []string{strings.Replace(kulaCSP("n"), "default-src 'self'", "default-src 'self' https:", 1)},
			map[string]Status{"HDR-003": StatusFail}},
		{"no base-uri", []string{strings.Replace(kulaCSP("n"), " base-uri 'self';", "", 1)},
			map[string]Status{"HDR-011": StatusWarn}},
		{"wide base-uri", []string{strings.Replace(kulaCSP("n"), "base-uri 'self'", "base-uri *", 1)},
			map[string]Status{"HDR-011": StatusWarn}},
		{"no form-action", []string{strings.Replace(kulaCSP("n"), " form-action 'self';", "", 1)},
			map[string]Status{"HDR-012": StatusWarn}},
		{"object-src from default-src 'self'", []string{strings.Replace(kulaCSP("n"), " object-src 'none';", "", 1)},
			map[string]Status{"HDR-013": StatusWarn}},
		{"object-src from default-src 'none'", []string{"default-src 'none'; script-src 'nonce-n'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"}, nil},
		{"a later duplicate directive is ignored", []string{kulaCSP("n") + " script-src *;"}, nil},
		{"an earlier duplicate directive wins", []string{"script-src *; " + kulaCSP("n")},
			map[string]Status{"HDR-003": StatusFail}},
		{"a second, strict policy still binds", []string{"script-src *", kulaCSP("n")}, nil},
		{"two policies in one value", []string{"default-src *, " + kulaCSP("n")}, nil},
	}
	for _, c := range cases {
		findings := cspFindings(parseCSP(c.values), strings.Join(c.values, ", "))
		for _, id := range cspIDs {
			want, named := c.want[id]
			if !named {
				want = StatusPass
			}
			f, ok := byID(findings, id)
			if !ok || f.Status != want {
				t.Errorf("%s: %s = %s %q, want %s", c.name, id, f.Status, f.Detail, want)
			}
		}
	}

	// No policy at all: scripts are not contained, and the frame-protection
	// expectation is still judged.
	findings := cspFindings(nil, "")
	wantStatus(t, findings, "HDR-003", StatusFail)
	if f, _ := byID(findings, "HDR-007"); f.Status != StatusWarn || f.Expectation != "frame-protection" {
		t.Errorf("no CSP: HDR-007 = %s, expectation %q; want a WARN against frame-protection", f.Status, f.Expectation)
	}
}

func TestExpectFrameProtectionWithoutCSP(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth/status" {
			writeJSONResp(w, map[string]any{"auth_required": false})
			return
		}
		_, _ = w.Write([]byte("kula is healthy"))
	}))
	defer ts.Close()
	s, err := NewScanner(Options{Target: ts.URL, Timeout: 5 * time.Second, Expect: []string{"frame-protection"}})
	if err != nil {
		t.Fatal(err)
	}
	s.discover()
	findings := runHeaderChecks(s)
	s.applyExpectations(findings)
	if f, _ := byID(findings, "HDR-007"); f.Status != StatusFail || f.Expectation != "frame-protection" {
		t.Errorf("no CSP with -expect frame-protection: HDR-007 = %s %q", f.Status, f.Expectation)
	}
}

func TestHSTSJudgement(t *testing.T) {
	https, err := NewScanner(Options{Target: "https://kula-scan.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		value  string
		want   Status
		tagged bool
	}{
		{"max-age=31536000; includeSubDomains", StatusPass, false},
		{"max-age=15552000", StatusPass, false},
		{`max-age="31536000"`, StatusPass, false},
		{"includeSubDomains; MAX-AGE=63072000; preload", StatusPass, false},
		{"max-age=86400", StatusWarn, false},
		{"max-age=0", StatusWarn, true},
		{"includeSubDomains", StatusWarn, true},
		{"max-age=soon", StatusWarn, true},
		{"", StatusWarn, true},
	} {
		h := http.Header{}
		if c.value != "" {
			h.Set("Strict-Transport-Security", c.value)
		}
		f := hstsFinding(https, h)
		if f.Status != c.want || (f.Expectation == "hsts") != c.tagged {
			t.Errorf("HSTS %q: %s, expectation %q; want %s, tagged %v", c.value, f.Status, f.Expectation, c.want, c.tagged)
		}
	}

	plain, err := NewScanner(Options{Target: "http://kula-scan.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	if f := hstsFinding(plain, http.Header{}); f.Status != StatusSkip || !strings.Contains(f.Detail, "TLS only") {
		t.Errorf("plain HTTP: %s %q, want a SKIP", f.Status, f.Detail)
	}
	expecting, err := NewScanner(Options{Target: "http://kula-scan.invalid", Expect: []string{"hsts"}})
	if err != nil {
		t.Fatal(err)
	}
	if f := hstsFinding(expecting, http.Header{}); f.Status != StatusFail || f.Expectation != "hsts" {
		t.Errorf("plain HTTP with -expect hsts: %s, expectation %q; want a FAIL against hsts", f.Status, f.Expectation)
	}
}

// withoutHeader serves the secure mock, but without header on path
// (dropHeader is in selection_test.go).
func withoutHeader(path, header string) http.Handler {
	mock := secureMock()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == path {
			w = &dropHeader{ResponseWriter: w, name: header}
		}
		mock.ServeHTTP(w, r)
	})
}

func TestHeadersCheckedBeyondTheIndexPage(t *testing.T) {
	for _, c := range []struct {
		path, header, id string
	}{
		{"/style.css", "X-Content-Type-Options", "HDR-001"},
		{"/api/auth/status", "X-Content-Type-Options", "HDR-001"},
		{"/", "Cache-Control", "HDR-010"},
		{"/api/auth/status", "Cache-Control", "HDR-010"},
		{"/api/config", "Cache-Control", "HDR-010"},
	} {
		ts := httptest.NewServer(withoutHeader(c.path, c.header))
		s := scannerFor(t, ts, mockUser, mockPass)
		f, _ := byID(runHeaderChecks(s), c.id)
		ts.Close()
		if f.Status != StatusFail || !strings.Contains(f.Detail, c.path+" (") {
			t.Errorf("%s missing on %s: %s = %s %q, want a FAIL naming the path", c.header, c.path, c.id, f.Status, f.Detail)
		}
	}
}

// ---- sessions and CSRF -------------------------------------------------------

// wrapLogout serves the secure mock, but hands /api/logout to logout first:
// it answers the request itself or returns false to pass it on.
func wrapLogout(logout func(w http.ResponseWriter, r *http.Request) bool) http.Handler {
	mock := secureMock()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/logout" && logout(w, r) {
			return
		}
		mock.ServeHTTP(w, r)
	})
}

func TestClosingLogoutIsTheRevocationProbe(t *testing.T) {
	findings, ledger := scanWithLedger(t, false, nil)
	wantStatus(t, findings, "AUTH-010", StatusPass)
	if got := ledger.attempts[mockUser]; len(got) != 1 {
		t.Errorf("logged in %d times, want once", len(got))
	}
	// Reported once, after the other auth findings.
	at, last := -1, -1
	for i, f := range findings {
		switch {
		case f.ID == "AUTH-010":
			if at >= 0 {
				t.Error("AUTH-010 reported twice")
			}
			at = i
		case f.Category == "auth":
			last = i
		}
	}
	if at != last+1 {
		t.Errorf("AUTH-010 at %d, want right after the last other auth finding at %d", at, last)
	}

	// -only AUTH-010 still opens the session it replays, and nothing else.
	only, err := newSelection("auth-010", "")
	if err != nil {
		t.Fatal(err)
	}
	findings, ledger = scanWithLedger(t, false, only)
	if got := strings.Join(reportedIDsOf(findings), ","); got != "DISC-001,AUTH-010" {
		t.Errorf("-only AUTH-010 reported %s", got)
	}
	wantStatus(t, findings, "AUTH-010", StatusPass)
	if len(ledger.attempts[mockUser]) != 1 || len(ledger.logouts) != 1 {
		t.Errorf("-only AUTH-010: %d logins, %d logouts; want one each", len(ledger.attempts[mockUser]), len(ledger.logouts))
	}
}

// TestInterruptedRunLogsOutWithoutTheReplay: an interrupt still sends the
// closing logout, but not the replay that judges it.
func TestInterruptedRunLogsOutWithoutTheReplay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ledger := &loginLedger{attempts: map[string][]string{}}
	mock := ledger.wrap(secureMock())
	var replays atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/metrics":
			cancel() // after the auth check
		case r.URL.Path == "/api/config" && ctx.Err() != nil:
			replays.Add(1)
		}
		mock.ServeHTTP(w, r)
	}))
	defer ts.Close()
	t.Setenv(passwordEnv, mockPass)

	code, stdout, _ := runCapturedContext(t, ctx, "-json", "-username", mockUser, ts.URL)
	if code != exitInterrupted {
		t.Fatalf("exit %d, want 130", code)
	}
	report := decodeReport(t, stdout)
	if f, _ := byID(report.Findings, "AUTH-010"); f.Status != StatusSkip || !strings.Contains(f.Detail, "interrupted") {
		t.Errorf("AUTH-010 = %s %q, want a SKIP naming the interrupt", f.Status, f.Detail)
	}
	if replays.Load() != 0 {
		t.Errorf("the interrupted run replayed the logged-out cookie %d times", replays.Load())
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if n := len(ledger.logouts); n == 0 || ledger.logouts[n-1].Get("X-CSRF-Token") != "csrftok" {
		t.Error("the interrupted run did not send the closing logout")
	}
}

func reportedIDsOf(findings []Finding) []string {
	ids := make([]string, 0, len(findings))
	for _, f := range findings {
		ids = append(ids, f.ID)
	}
	return ids
}

func TestLogoutThatKeepsTheSessionFails(t *testing.T) {
	// The logout answers as Kula does, but revokes nothing.
	keeps := wrapLogout(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("X-CSRF-Token") != "csrftok" {
			return false
		}
		writeJSONResp(w, map[string]any{"status": "logged out"})
		return true
	})
	t.Setenv(passwordEnv, mockPass)
	code, report, _ := scanJSON(t, keeps, "-username", mockUser, "-only", "auth")
	f, _ := byID(report.Findings, "AUTH-010")
	if f.Status != StatusFail || f.Severity != SevHigh || code != exitFailing {
		t.Errorf("AUTH-010 = %s %s, exit %d; want a HIGH FAIL and exit 1", f.Status, f.Severity, code)
	}
}

func TestRevocationSkipsSayWhy(t *testing.T) {
	_, report, _ := scanJSON(t, secureMock(), "-only", "auth")
	if f, _ := byID(report.Findings, "AUTH-010"); f.Status != StatusSkip || !strings.Contains(f.Detail, "-username/-password") {
		t.Errorf("no credentials: AUTH-010 = %s %q", f.Status, f.Detail)
	}
	_, report, _ = scanJSON(t, secureMock(), "-only", "auth,csrf", "-username", mockUser, "-password", "wrong")
	for _, id := range []string{"AUTH-010", "CSRF-003", "CSRF-004"} {
		f, _ := byID(report.Findings, id)
		if f.Status != StatusSkip || !strings.Contains(f.Detail, "wrong username or password") || strings.Contains(f.Detail, "supply") {
			t.Errorf("refused credentials: %s = %s %q, want a SKIP naming the refusal", id, f.Status, f.Detail)
		}
	}
	// With auth off there is no session to revoke, nor a finding about it.
	var loginHits, chatHits atomic.Int64
	_, report, _ = scanJSON(t, authOffMock(false, &loginHits, &chatHits), "-only", "auth,csrf", "-username", mockUser, "-password", mockPass)
	for _, id := range []string{"AUTH-010", "CSRF-003", "CSRF-004"} {
		if f, ok := byID(report.Findings, id); ok {
			t.Errorf("auth off: %s reported: %+v", id, f)
		}
	}
}

// TestBrokenTokenGateEndsTheSession: a target that accepts CSRF-003's logout
// ends the scan's session. The checks after it skip and say why instead of
// misreporting, nothing logs in again, and no closing logout is sent.
func TestBrokenTokenGateEndsTheSession(t *testing.T) {
	ledger := &loginLedger{attempts: map[string][]string{}}
	noToken := ledger.wrap(wrapLogout(func(w http.ResponseWriter, r *http.Request) bool {
		r.Header.Set("X-CSRF-Token", "csrftok") // the token is never checked
		return false
	}))
	t.Setenv(passwordEnv, mockPass)
	_, report, _ := scanJSON(t, noToken, "-username", mockUser)

	wantStatus(t, report.Findings, "CSRF-003", StatusFail)
	if f, _ := byID(report.Findings, "CSRF-003"); !strings.Contains(f.Detail, "skipped") {
		t.Errorf("CSRF-003 does not say the session ended: %q", f.Detail)
	}
	for _, id := range []string{"CSRF-004", "INPUT-001", "AUTH-010"} {
		f, _ := byID(report.Findings, id)
		if f.Status != StatusSkip || !strings.Contains(f.Detail, "CSRF-003") {
			t.Errorf("%s after the session was lost = %s %q, want a SKIP naming CSRF-003", id, f.Status, f.Detail)
		}
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if got := ledger.attempts[mockUser]; len(got) != 1 {
		t.Errorf("logged in %d times, want once", len(got))
	}
	withSession := 0
	for _, h := range ledger.logouts {
		if h.Get("Cookie") == "kula_session="+mockToken {
			withSession++
		}
	}
	if withSession != 1 {
		t.Errorf("%d logouts carried the session, want only CSRF-003's", withSession)
	}
}

func TestWrongCSRFTokenRefused(t *testing.T) {
	if got := wrongCSRFToken("abc0"); got != "abc1" {
		t.Errorf("wrongCSRFToken(abc0) = %q", got)
	}
	if got := wrongCSRFToken("abcf"); got != "abc0" {
		t.Errorf("wrongCSRFToken(abcf) = %q", got)
	}
	if got := wrongCSRFToken(""); got == "" {
		t.Error("wrongCSRFToken without a real token sent none")
	}

	// The token gate checks only that one is sent.
	anyToken := wrapLogout(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("X-CSRF-Token") != "" {
			r.Header.Set("X-CSRF-Token", "csrftok")
		}
		return false
	})
	ts := httptest.NewServer(anyToken)
	defer ts.Close()
	s := scannerFor(t, ts, mockUser, mockPass)
	csrf := runCSRFChecks(s)
	wantStatus(t, csrf, "CSRF-003", StatusPass)
	wantStatus(t, csrf, "CSRF-004", StatusFail)
	if s.loggedIn || s.sessionLost == "" {
		t.Error("the session CSRF-004's logout ended is still taken for open")
	}
}

func TestForeignRefererRefused(t *testing.T) {
	// Origin validation that reads Origin only: a foreign Referer passes.
	originOnly := wrapLogout(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Origin") == "" && r.Header.Get("Referer") != "" {
			writeJSONResp(w, map[string]any{"status": "logged out"})
			return true
		}
		return false
	})
	ts := httptest.NewServer(originOnly)
	defer ts.Close()
	s := scannerFor(t, ts, "", "")
	f, _ := byID(runCSRFChecks(s), "CSRF-002")
	if f.Status != StatusWarn || !strings.Contains(f.Detail, "Referer") {
		t.Errorf("CSRF-002 = %s %q, want a WARN naming the Referer probe", f.Status, f.Detail)
	}
}

func TestProtectedRoutesCoverOllamaAndUnknownPaths(t *testing.T) {
	for _, c := range []struct {
		name, want string
		open       func(r *http.Request) bool
	}{
		{"chat", "POST /api/ollama/chat", func(r *http.Request) bool { return r.URL.Path == "/api/ollama/chat" }},
		{"models", "/api/ollama/models", func(r *http.Request) bool { return r.URL.Path == "/api/ollama/models" }},
		{"system-info", "/api/system-info", func(r *http.Request) bool { return r.URL.Path == "/api/system-info" }},
		{"unknown", "/api/kula-scan-", func(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, "/api/kula-scan-") }},
	} {
		mock := secureMock()
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if c.open(r) {
				http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest) // past the gate
				return
			}
			mock.ServeHTTP(w, r)
		}))
		s := scannerFor(t, ts, "", "")
		f, _ := byID(runAuthChecks(s), "AUTH-001")
		ts.Close()
		if f.Status != StatusFail || !strings.Contains(f.Detail, c.want) {
			t.Errorf("%s ungated: AUTH-001 = %s %q", c.name, f.Status, f.Detail)
		}
	}
}

// ---- report logic ------------------------------------------------------------

func TestExitCodeThreshold(t *testing.T) {
	findings := []Finding{
		finding("A", SevMedium, StatusFail, ""),
		finding("B", SevInfo, StatusPass, ""),
	}
	r := newReport("t", findings)
	if r.Summary.Fail != 1 || r.Summary.Pass != 1 {
		t.Fatalf("summary = %+v", r.Summary)
	}
	if code := r.exitCode(SevHigh); code != 0 {
		t.Errorf("exit with fail-on=high and only a medium fail = %d, want 0", code)
	}
	if code := r.exitCode(SevMedium); code != 1 {
		t.Errorf("exit with fail-on=medium and a medium fail = %d, want 1", code)
	}
	if code := r.exitCode(SevLow); code != 1 {
		t.Errorf("exit with fail-on=low = %d, want 1", code)
	}
}

func TestParseSeverityAndOnly(t *testing.T) {
	if _, ok := parseSeverity("bogus"); ok {
		t.Error("parseSeverity accepted a bogus value")
	}
	if sev, ok := parseSeverity("CRITICAL"); !ok || sev != SevCritical {
		t.Errorf("parseSeverity(CRITICAL) = %v,%v", sev, ok)
	}
	sel, err := newSelection("auth, CORS ,headers", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"AUTH-001", "CORS-001", "HDR-001"} {
		if !sel.selected(id) {
			t.Errorf("-only auth,CORS,headers does not select %s", id)
		}
	}
	if sel.selected("TRAV-001") {
		t.Error("-only auth,CORS,headers selects TRAV-001")
	}
	if all, err := newSelection("", ""); err != nil || !all.selected("TRAV-001") {
		t.Error("an empty -only should select everything")
	}
}

// ---- DoS / resource-exhaustion checks ----------------------------------------

// scannerForDos builds a scanner with an explicit dosWait so the slow-connection
// probes finish quickly in tests.
func scannerForDos(t *testing.T, ts *httptest.Server, user, pass string, dosWait time.Duration) *Scanner {
	t.Helper()
	s, err := NewScanner(Options{
		Target: ts.URL, Username: user, Password: pass,
		Timeout: 5 * time.Second, DoSWait: dosWait, Seed: 1,
	})
	if err != nil {
		t.Fatalf("NewScanner: %v", err)
	}
	s.discover()
	return s
}

// readTimeoutServer serves the secure mock with a short ReadTimeout, emulating
// kula's http.Server.ReadTimeout so slow/idle connections get reaped.
func readTimeoutServer(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewUnstartedServer(secureMock())
	ts.Config.ReadTimeout = 1 * time.Second
	ts.Start()
	t.Cleanup(ts.Close)
	return ts
}

func TestDoSSlowlorisReapedWithReadTimeout(t *testing.T) {
	ts := readTimeoutServer(t)
	s := scannerForDos(t, ts, mockUser, mockPass, 4*time.Second)
	wantStatus(t, runSlowlorisCheck(s), "DOS-SLOWLORIS", StatusPass)
}

func TestDoSSlowlorisNotReapedWithoutTimeout(t *testing.T) {
	ts := httptest.NewServer(secureMock()) // no ReadTimeout
	defer ts.Close()
	s := scannerForDos(t, ts, mockUser, mockPass, 1500*time.Millisecond)
	f, _ := byID(runSlowlorisCheck(s), "DOS-SLOWLORIS")
	if f.Status != StatusFail {
		t.Errorf("DOS-SLOWLORIS without ReadTimeout = %s, want FAIL — %s", f.Status, f.Detail)
	}
}

func TestDoSHeaderBombRejected(t *testing.T) {
	ts := httptest.NewServer(secureMock()) // default MaxHeaderBytes = 1 MiB
	defer ts.Close()
	s := scannerForDos(t, ts, "", "", 3*time.Second)
	wantStatus(t, runHeaderBombCheck(s), "DOS-HEADERBOMB", StatusPass)
}

func TestDoSConnFloodResilient(t *testing.T) {
	ts := readTimeoutServer(t)
	s := scannerForDos(t, ts, mockUser, mockPass, 4*time.Second)
	wantStatus(t, runConnFloodCheck(s), "DOS-CONNFLOOD", StatusPass)
}

func TestDoSWSMessageBombDropped(t *testing.T) {
	ts := httptest.NewServer(secureMock())
	defer ts.Close()
	s := scannerForDos(t, ts, mockUser, mockPass, 3*time.Second)
	wantStatus(t, runWSMsgBombCheck(s), "WS-MSGBOMB", StatusPass)
}

// ---- fuzzing / fault injection -----------------------------------------------

func scannerForFuzz(t *testing.T, ts *httptest.Server, user, pass string) *Scanner {
	t.Helper()
	s, err := NewScanner(Options{
		Target: ts.URL, Username: user, Password: pass,
		Timeout: 5 * time.Second, DoSWait: 2 * time.Second,
		Fuzz: true, FuzzIter: 60, Seed: 1,
	})
	if err != nil {
		t.Fatalf("NewScanner: %v", err)
	}
	s.discover()
	return s
}

func TestClassifyResultOracle(t *testing.T) {
	cases := []struct {
		name string
		r    httpResult
		bad  bool
	}{
		{"400 is fine", httpResult{status: 400}, false},
		{"401 is fine", httpResult{status: 401}, false},
		{"429 is fine", httpResult{status: 429}, false},
		{"200 is fine", httpResult{status: 200}, false},
		{"500 is an anomaly", httpResult{status: 500}, true},
		{"503 is an anomaly", httpResult{status: 503}, true},
		{"EOF is an anomaly", httpResult{err: errString("unexpected EOF")}, true},
		{"reset is an anomaly", httpResult{err: errString("read: connection reset by peer")}, true},
		{"timeout is an anomaly", httpResult{err: errString("context deadline exceeded")}, true},
		{"dial refused later is noise", httpResult{err: errString("no such host")}, false},
	}
	for _, c := range cases {
		if bad, _ := classifyResult(c.r); bad != c.bad {
			t.Errorf("%s: classifyResult bad=%v, want %v", c.name, bad, c.bad)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// TestFuzzRobustAgainstSecureMock confirms the well-behaved mock produces no
// fuzz anomalies and survives the barrage.
func TestFuzzRobustAgainstSecureMock(t *testing.T) {
	ts := httptest.NewServer(secureMock())
	defer ts.Close()
	s := scannerForFuzz(t, ts, mockUser, mockPass)

	for _, run := range []func(*Scanner) []Finding{runFuzzQuery, runFuzzPath, runFuzzBody, runFuzzMethods, runFuzzSmuggle, runFuzzWS, runFuzzLiveness} {
		for _, f := range run(s) {
			if f.Status == StatusFail {
				t.Errorf("%s unexpectedly FAILed against the secure mock: %s | %s", f.ID, f.Detail, f.Evidence)
			}
		}
	}
}

// buggyMock 500s on /api/history and reflects the request path into an HTML
// body — exactly the anomalies the fuzzer should catch.
func buggyMock() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/health":
			_, _ = w.Write([]byte("ok"))
		case r.URL.Path == "/api/auth/status":
			writeJSONResp(w, map[string]any{"auth_required": false})
		case strings.HasPrefix(r.URL.Path, "/api/history"):
			http.Error(w, "boom", http.StatusInternalServerError)
		case strings.HasPrefix(r.URL.Path, "/api/i18n"):
			writeJSONResp(w, map[string]any{"ok": 1})
		default:
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>path=" + r.URL.Path + "</html>")) // reflects canary
		}
	})
}

func TestFuzzDetectsAnomalies(t *testing.T) {
	ts := httptest.NewServer(buggyMock())
	defer ts.Close()
	s := scannerForFuzz(t, ts, "", "")

	f, _ := byID(runFuzzQuery(s), "FUZZ-QUERY")
	if f.Status != StatusFail {
		t.Errorf("FUZZ-QUERY against a 500-ing endpoint = %s, want FAIL (%s)", f.Status, f.Evidence)
	}
	p, _ := byID(runFuzzPath(s), "FUZZ-PATH")
	if p.Status != StatusFail {
		t.Errorf("FUZZ-PATH against a path-reflecting endpoint = %s, want FAIL (%s)", p.Status, p.Evidence)
	}
}

// ---- open redirect -----------------------------------------------------------

func TestOpenRedirectDetected(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "evil.example") {
			http.Redirect(w, r, "http://evil.example/", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()
	s := scannerForDos(t, ts, "", "", time.Second)
	wantStatus(t, runOpenRedirectCheck(s), "REDIR-001", StatusFail)
}

func TestNoOpenRedirectSecureMock(t *testing.T) {
	ts := httptest.NewServer(secureMock())
	defer ts.Close()
	s := scannerForDos(t, ts, "", "", time.Second)
	wantStatus(t, runOpenRedirectCheck(s), "REDIR-001", StatusPass)
}

// ---- X-Forwarded-For rate-limit bypass --------------------------------------

// xffMock simulates a login endpoint that throttles after 5 attempts per key.
// When trustProxy is true it (wrongly) keys on the X-Forwarded-For header, so
// rotating it bypasses the throttle.
func xffMock(trustProxy bool) http.Handler {
	var mu sync.Mutex
	counts := map[string]int{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte("ok"))
		case "/api/auth/status":
			writeJSONResp(w, map[string]any{"auth_required": true})
		case "/api/login":
			key := r.RemoteAddr
			if i := strings.LastIndexByte(key, ':'); i >= 0 {
				key = key[:i]
			}
			if trustProxy {
				if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
					key = xff
				}
			}
			mu.Lock()
			counts[key]++
			n := counts[key]
			mu.Unlock()
			if n > 5 {
				http.Error(w, `{"error":"too many requests"}`, http.StatusTooManyRequests)
				return
			}
			http.Error(w, `{"error":"invalid credentials"}`, http.StatusUnauthorized)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func TestXFFBypassDetected(t *testing.T) {
	ts := httptest.NewServer(xffMock(true)) // keys on X-Forwarded-For → bypassable
	defer ts.Close()
	s := scannerForDos(t, ts, "", "", time.Second)
	wantStatus(t, runXFFBypassCheck(s), "BYPASS-XFF", StatusFail)
}

func TestXFFBypassNotPossible(t *testing.T) {
	ts := httptest.NewServer(xffMock(false)) // keys on the real connection → throttled
	defer ts.Close()
	s := scannerForDos(t, ts, "", "", time.Second)
	wantStatus(t, runXFFBypassCheck(s), "BYPASS-XFF", StatusPass)
}

// ---- auth disabled -----------------------------------------------------------

// authOffMock answers like Kula with web.auth disabled: /api/login is a JSON
// 404 whatever is sent to it. It counts the requests that reach /api/login
// and /api/ollama/chat.
func authOffMock(ollama bool, loginHits, chatHits *atomic.Int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte("ok"))
		case "/api/auth/status":
			writeJSONResp(w, map[string]any{"auth_required": false, "authenticated": true})
		case "/api/config":
			writeJSONResp(w, map[string]any{"auth_enabled": false, "ollama_enabled": ollama})
		case "/api/login":
			loginHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		case "/api/ollama/chat":
			chatHits.Add(1)
			http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// TestLoginProbesSkipWithoutAuth: with auth disabled there is no login
// endpoint, so the probes that drive it must skip instead of reading its 404
// as a missing safeguard (INPUT-AGG warned, RATE-LOGIN failed HIGH) or as a
// passed one (AUTH-006, FUZZ-BODY), and must not send it anything.
func TestLoginProbesSkipWithoutAuth(t *testing.T) {
	for _, ollama := range []bool{false, true} {
		var loginHits, chatHits atomic.Int64
		ts := httptest.NewServer(authOffMock(ollama, &loginHits, &chatHits))
		s := scannerForFuzz(t, ts, mockUser, mockPass)
		if s.authEnabled || s.ollama != ollama {
			t.Fatalf("discovery: authEnabled=%v ollama=%v, want false/%v", s.authEnabled, s.ollama, ollama)
		}

		wantStatus(t, runOversizedBodyCheck(s), "INPUT-AGG", StatusSkip)
		wantStatus(t, runLoginRateCheck(s), "RATE-LOGIN", StatusSkip)
		wantStatus(t, []Finding{enumerationFinding(s)}, "AUTH-006", StatusSkip)
		wantStatus(t, runXFFBypassCheck(s), "BYPASS-XFF", StatusSkip)
		body := runFuzzBody(s)
		if ollama {
			// The Ollama chat still parses a body, so the probe runs there.
			wantStatus(t, body, "FUZZ-BODY", StatusPass)
			if chatHits.Load() == 0 {
				t.Error("FUZZ-BODY sent nothing to /api/ollama/chat with Ollama enabled")
			}
		} else {
			wantStatus(t, body, "FUZZ-BODY", StatusSkip)
		}
		if n := loginHits.Load(); n != 0 {
			t.Errorf("ollama=%v: %d requests reached /api/login with auth disabled", ollama, n)
		}
		ts.Close()
	}
}
