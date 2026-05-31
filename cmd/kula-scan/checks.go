package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// This file holds the non-disruptive safeguard probes, grouped by category.
// Each probe returns one or more Findings; PASS means the safeguard is present
// and behaves as expected, FAIL means it is missing or bypassable.

// ============================== headers ======================================

func headerChecks() []check {
	return []check{{
		id:       "HDR",
		category: "headers",
		run:      runHeaderChecks,
	}}
}

func runHeaderChecks(s *Scanner) []Finding {
	var out []Finding

	r := s.do(http.MethodGet, "/", nil, "")
	if r.err != nil {
		return []Finding{finding("HDR-000", "headers", "Fetch UI page for header inspection", SevInfo, StatusError,
			"could not GET / to inspect security headers: "+r.err.Error())}
	}
	h := r.header

	want := func(id, title, header, expect string, sev Severity) Finding {
		got := h.Get(header)
		if expect == "" {
			if got != "" {
				return finding(id, "headers", title, sev, StatusPass, header+" is set").withEvidence("%s: %s", header, got)
			}
			return finding(id, "headers", title, sev, StatusFail, header+" is missing").
				withRemediation("Enable web.security.headers in config.yaml.")
		}
		if strings.EqualFold(strings.TrimSpace(got), expect) {
			return finding(id, "headers", title, sev, StatusPass, header+" is correctly set").withEvidence("%s: %s", header, got)
		}
		return finding(id, "headers", title, sev, StatusFail, header+" missing or wrong (want "+expect+")").
			withEvidence("%s: %q", header, got).
			withRemediation("Enable web.security.headers (and web.security.frame_protection) in config.yaml.")
	}

	out = append(out,
		want("HDR-001", "X-Content-Type-Options nosniff", "X-Content-Type-Options", "nosniff", SevMedium),
		want("HDR-002", "X-Frame-Options DENY (clickjacking)", "X-Frame-Options", "DENY", SevMedium),
		want("HDR-005", "Referrer-Policy present", "Referrer-Policy", "", SevLow),
		want("HDR-006", "Permissions-Policy present", "Permissions-Policy", "", SevLow),
	)

	// CSP: must constrain default-src to self, lock framing, and use a nonce.
	csp := h.Get("Content-Security-Policy")
	switch csp {
	case "":
		out = append(out, finding("HDR-003", "headers", "Content-Security-Policy present", SevHigh, StatusFail,
			"no Content-Security-Policy header; the dashboard has no script/XSS containment.").
			withRemediation("Enable web.security.headers in config.yaml."))
	default:
		var missing []string
		for _, tok := range []string{"default-src 'self'", "nonce-"} {
			if !strings.Contains(csp, tok) {
				missing = append(missing, tok)
			}
		}
		if len(missing) == 0 {
			out = append(out, finding("HDR-003", "headers", "Content-Security-Policy hardened", SevHigh, StatusPass,
				"CSP constrains default-src to 'self' and uses a per-request script nonce.").withEvidence("CSP: %s", csp))
		} else {
			out = append(out, finding("HDR-003", "headers", "Content-Security-Policy weak", SevHigh, StatusFail,
				"CSP is present but missing: "+strings.Join(missing, ", ")).withEvidence("CSP: %s", csp).
				withRemediation("Upgrade to a version that ships the nonce-based CSP, or re-enable web.security.headers."))
		}
		// frame-ancestors 'none' is the modern clickjacking control.
		if strings.Contains(csp, "frame-ancestors 'none'") {
			out = append(out, finding("HDR-007", "headers", "CSP frame-ancestors 'none'", SevMedium, StatusPass,
				"CSP forbids framing (frame-ancestors 'none')."))
		} else {
			out = append(out, finding("HDR-007", "headers", "CSP frame-ancestors 'none'", SevMedium, StatusWarn,
				"CSP does not set frame-ancestors 'none'; relies on X-Frame-Options alone.").
				withRemediation("Enable web.security.frame_protection."))
		}
	}

	// Nonce freshness: a reused nonce defeats the whole point of a nonce-CSP.
	if strings.Contains(csp, "nonce-") {
		r2 := s.do(http.MethodGet, "/", nil, "")
		n1, n2 := extractNonce(csp), extractNonce(r2.header.Get("Content-Security-Policy"))
		switch {
		case n1 == "" || n2 == "":
			out = append(out, finding("HDR-004", "headers", "CSP nonce freshness", SevMedium, StatusWarn,
				"could not extract nonces from two responses to compare."))
		case n1 == n2:
			out = append(out, finding("HDR-004", "headers", "CSP nonce freshness", SevHigh, StatusFail,
				"the CSP nonce is identical across two requests; a static nonce is trivially guessable and defeats the policy.").
				withEvidence("nonce=%s on both responses", n1).
				withRemediation("The nonce must be regenerated per request (this is a build/proxy caching problem)."))
		default:
			out = append(out, finding("HDR-004", "headers", "CSP nonce freshness", SevMedium, StatusPass,
				"the CSP nonce differs between requests (freshly random per response)."))
		}
	}

	// HSTS only makes sense over TLS.
	if s.https {
		if h.Get("Strict-Transport-Security") != "" {
			out = append(out, finding("HDR-008", "headers", "HSTS over TLS", SevLow, StatusPass,
				"Strict-Transport-Security is set.").withEvidence("%s", h.Get("Strict-Transport-Security")))
		} else {
			out = append(out, finding("HDR-008", "headers", "HSTS over TLS", SevLow, StatusWarn,
				"served over HTTPS but no Strict-Transport-Security header.").
				withRemediation("Enable web.security.headers; HSTS is emitted automatically over TLS."))
		}
	}

	// Banner / version disclosure.
	if banner := h.Get("Server"); banner != "" {
		out = append(out, finding("HDR-009", "headers", "Server banner disclosure", SevInfo, StatusWarn,
			"a Server header is present; minor information disclosure.").withEvidence("Server: %s", banner))
	}

	return out
}

// extractNonce pulls the base64 nonce out of a CSP string, or "" if absent.
func extractNonce(csp string) string {
	const marker = "nonce-"
	i := strings.Index(csp, marker)
	if i < 0 {
		return ""
	}
	rest := csp[i+len(marker):]
	end := strings.IndexAny(rest, "'\" ;")
	if end < 0 {
		return rest
	}
	return rest[:end]
}

// ============================== auth =========================================

// protectedRoutes are API endpoints that must require authentication when
// web.auth is enabled. They are all read-only GETs (no rate-limit cost).
var protectedRoutes = []string{"/api/current", "/api/history", "/api/config", "/api/i18n?lang=en"}

func authChecks() []check {
	return []check{{
		id:       "AUTH",
		category: "auth",
		run:      runAuthChecks,
	}}
}

func runAuthChecks(s *Scanner) []Finding {
	var out []Finding

	if !s.authEnabled {
		out = append(out, finding("AUTH-000", "auth", "Authentication posture", SevMedium, StatusWarn,
			"web.auth is disabled: the API, history, and config endpoints are served to anyone who can reach the port.").
			withRemediation("Enable web.auth (and put kula behind TLS) unless access is restricted at the network layer."))
		return out
	}

	// AUTH-001: protected routes reject anonymous access.
	var leaked []string
	for _, p := range protectedRoutes {
		if r := s.do(http.MethodGet, p, nil, ""); r.err == nil && r.status != http.StatusUnauthorized {
			leaked = append(leaked, p)
		}
	}
	if len(leaked) == 0 {
		out = append(out, finding("AUTH-001", "auth", "Protected API requires auth", SevHigh, StatusPass,
			"all protected API routes return 401 without credentials."))
	} else {
		out = append(out, finding("AUTH-001", "auth", "Protected API requires auth", SevHigh, StatusFail,
			"these routes served data without authentication: "+strings.Join(leaked, ", ")).
			withRemediation("This indicates a broken auth gate; routes under /api/ must sit behind AuthMiddleware."))
	}

	// AUTH-002: forged session cookies are rejected.
	forged := []string{"deadbeef", "", strings.Repeat("a", 64), "../../etc/passwd", "' OR '1'='1"}
	var accepted []string
	for _, tok := range forged {
		hdr := map[string]string{"Cookie": "kula_session=" + tok}
		if r := s.do(http.MethodGet, "/api/i18n?lang=en", hdr, ""); r.err == nil && r.status == http.StatusOK {
			accepted = append(accepted, tok)
		}
	}
	if len(accepted) == 0 {
		out = append(out, finding("AUTH-002", "auth", "Forged session cookies rejected", SevHigh, StatusPass,
			"garbage, empty, traversal, and SQLi-style session values are all rejected."))
	} else {
		out = append(out, finding("AUTH-002", "auth", "Forged session cookies rejected", SevCritical, StatusFail,
			"a forged session value authenticated successfully").withEvidence("accepted: %q", accepted).
			withRemediation("Session validation must hash-and-lookup the token; never accept attacker-supplied values."))
	}

	// AUTH-003: garbage bearer token is rejected.
	if r := s.do(http.MethodGet, "/api/i18n?lang=en", map[string]string{"Authorization": "Bearer not-a-real-token"}, ""); r.err == nil {
		if r.status == http.StatusUnauthorized {
			out = append(out, finding("AUTH-003", "auth", "Forged bearer token rejected", SevHigh, StatusPass,
				"a bogus Authorization: Bearer token is rejected with 401."))
		} else {
			out = append(out, finding("AUTH-003", "auth", "Forged bearer token rejected", SevCritical, StatusFail,
				"a bogus bearer token was accepted").withEvidence("status=%d", r.status))
		}
	}

	// AUTH-004: login method enforcement.
	if r := s.do(http.MethodGet, "/api/login", nil, ""); r.err == nil {
		if r.status == http.StatusMethodNotAllowed {
			out = append(out, finding("AUTH-004", "auth", "Login rejects non-POST", SevLow, StatusPass,
				"GET /api/login returns 405."))
		} else {
			out = append(out, finding("AUTH-004", "auth", "Login rejects non-POST", SevLow, StatusWarn,
				"GET /api/login did not return 405").withEvidence("status=%d", r.status))
		}
	}

	// AUTH-005: happy-path login + session cookie attributes (needs creds).
	if s.username == "" || s.password == "" {
		out = append(out, finding("AUTH-005", "auth", "Login + session cookie attributes", SevInfo, StatusSkip,
			"no -username/-password supplied; skipping authenticated login probe."))
	} else if !s.ensureSession() {
		out = append(out, finding("AUTH-005", "auth", "Login with supplied credentials", SevInfo, StatusWarn,
			"could not log in with the supplied credentials (wrong creds, or already rate-limited)."))
	} else {
		out = append(out, finding("AUTH-005", "auth", "Login grants access", SevInfo, StatusPass,
			"the supplied credentials logged in and returned a session cookie + CSRF token."))
		out = append(out, cookieAttributeFindings(s)...)
	}

	// AUTH-006: username enumeration resistance (needs a real username).
	out = append(out, enumerationFinding(s))

	return out
}

// cookieAttributeFindings inspects the Set-Cookie from login for HttpOnly,
// SameSite, and (over TLS) Secure.
func cookieAttributeFindings(s *Scanner) []Finding {
	c := s.loginCookie
	if c == nil {
		return nil
	}
	var out []Finding
	if c.HttpOnly {
		out = append(out, finding("AUTH-007", "auth", "Session cookie HttpOnly", SevMedium, StatusPass,
			"the session cookie is HttpOnly (not readable from JavaScript)."))
	} else {
		out = append(out, finding("AUTH-007", "auth", "Session cookie HttpOnly", SevHigh, StatusFail,
			"the session cookie is NOT HttpOnly; XSS could steal it.").
			withRemediation("Session cookies must be issued HttpOnly."))
	}
	if c.SameSite != http.SameSiteDefaultMode {
		out = append(out, finding("AUTH-008", "auth", "Session cookie SameSite", SevLow, StatusPass,
			"the session cookie sets a SameSite attribute.").withEvidence("SameSite=%d", c.SameSite))
	} else {
		out = append(out, finding("AUTH-008", "auth", "Session cookie SameSite", SevLow, StatusWarn,
			"the session cookie has no SameSite attribute."))
	}
	if s.https {
		if c.Secure {
			out = append(out, finding("AUTH-009", "auth", "Session cookie Secure over TLS", SevMedium, StatusPass,
				"the session cookie is marked Secure."))
		} else {
			out = append(out, finding("AUTH-009", "auth", "Session cookie Secure over TLS", SevHigh, StatusFail,
				"served over HTTPS but the session cookie is not Secure; it can leak over a downgraded request.").
				withRemediation("Ensure TLS/trust_proxy is detected so cookies get the Secure flag."))
		}
	}
	return out
}

// enumerationFinding probes the login timing/enumeration fix: a wrong password
// for a real username and for a random username must be indistinguishable.
func enumerationFinding(s *Scanner) Finding {
	if s.username == "" {
		return finding("AUTH-006", "auth", "Username enumeration resistance", SevInfo, StatusSkip,
			"no -username supplied; cannot compare a known user against an unknown one.")
	}
	existing := s.loginRaw(s.username, "definitely-the-wrong-password-xyz")
	missing := s.loginRaw("no-such-user-"+randToken(6), "definitely-the-wrong-password-xyz")

	if existing.err != nil || missing.err != nil {
		return finding("AUTH-006", "auth", "Username enumeration resistance", SevInfo, StatusError,
			"could not complete enumeration probe (network error).")
	}
	// A 429 means we tripped the rate limiter; the comparison is unreliable.
	if existing.status == http.StatusTooManyRequests || missing.status == http.StatusTooManyRequests {
		return finding("AUTH-006", "auth", "Username enumeration resistance", SevInfo, StatusSkip,
			"login rate limiter engaged during the probe; re-run later for a clean comparison.")
	}
	if existing.status == missing.status && existing.body == missing.body {
		return finding("AUTH-006", "auth", "Username enumeration resistance", SevMedium, StatusPass,
			"a wrong password yields an identical response for known and unknown usernames (no enumeration oracle).")
	}
	return finding("AUTH-006", "auth", "Username enumeration resistance", SevMedium, StatusFail,
		"the login response differs between a known and an unknown username, leaking which accounts exist.").
		withEvidence("known: %d %q | unknown: %d %q", existing.status, truncate(existing.body, 80), missing.status, truncate(missing.body, 80)).
		withRemediation("Equalise the response (status + body + timing) for valid and invalid usernames.")
}

// loginRaw posts credentials with a same-origin header (so CSRF admits it) and
// returns the result. Used by probes that must reach the login handler.
func (s *Scanner) loginRaw(username, password string) httpResult {
	b, err := json.Marshal(map[string]string{"username": username, "password": password})
	if err != nil {
		return httpResult{err: err}
	}
	body := string(b)
	headers := map[string]string{
		"Content-Type": "application/json",
		"Origin":       s.base.Scheme + "://" + s.base.Host,
	}
	return s.do(http.MethodPost, "/api/login", headers, body)
}

// ============================== csrf =========================================

func csrfChecks() []check {
	return []check{{
		id:       "CSRF",
		category: "csrf",
		run:      runCSRFChecks,
	}}
}

func runCSRFChecks(s *Scanner) []Finding {
	var out []Finding
	sameOrigin := s.base.Scheme + "://" + s.base.Host

	// CSRF-001: a state-changing POST with no Origin/Referer is blocked.
	noOrigin := s.do(http.MethodPost, "/api/login",
		map[string]string{"Content-Type": "application/json"},
		`{"username":"x","password":"y"}`)
	if noOrigin.err == nil {
		if noOrigin.status == http.StatusForbidden {
			out = append(out, finding("CSRF-001", "csrf", "Origin required on state change", SevMedium, StatusPass,
				"a POST with no Origin/Referer is rejected with 403 (origin validation active)."))
		} else {
			out = append(out, finding("CSRF-001", "csrf", "Origin required on state change", SevMedium, StatusWarn,
				"a POST with no Origin/Referer was not rejected with 403; origin validation appears disabled.").
				withEvidence("status=%d", noOrigin.status).
				withRemediation("Enable web.security.origin_validation for defense-in-depth against CSRF."))
		}
	}

	// CSRF-002: a state change from a foreign Origin is blocked.
	foreign := s.do(http.MethodPost, "/api/login",
		map[string]string{"Content-Type": "application/json", "Origin": "https://evil.example"},
		`{"username":"x","password":"y"}`)
	if foreign.err == nil {
		if foreign.status == http.StatusForbidden {
			out = append(out, finding("CSRF-002", "csrf", "Cross-origin state change blocked", SevHigh, StatusPass,
				"a POST from a foreign Origin is rejected with 403."))
		} else {
			out = append(out, finding("CSRF-002", "csrf", "Cross-origin state change blocked", SevHigh, StatusWarn,
				"a POST from https://evil.example was not rejected with 403.").
				withEvidence("status=%d", foreign.status).
				withRemediation("Enable web.security.origin_validation."))
		}
	}

	// CSRF-003 (needs session): authenticated state change without a CSRF token.
	if s.authEnabled && s.ensureSession() {
		hdr := map[string]string{"Cookie": "kula_session=" + s.session, "Origin": sameOrigin}
		r := s.do(http.MethodPost, "/api/logout", hdr, "")
		if r.status == http.StatusForbidden {
			out = append(out, finding("CSRF-003", "csrf", "Synchronizer token enforced", SevHigh, StatusPass,
				"an authenticated, same-origin POST without the X-CSRF-Token header is rejected with 403."))
		} else {
			out = append(out, finding("CSRF-003", "csrf", "Synchronizer token enforced", SevHigh, StatusFail,
				"an authenticated state change succeeded without a CSRF token; the session is open to CSRF.").
				withEvidence("status=%d", r.status).
				withRemediation("Require a matching X-CSRF-Token for state-changing requests on authenticated sessions."))
		}
	} else if s.authEnabled {
		out = append(out, finding("CSRF-003", "csrf", "Synchronizer token enforced", SevInfo, StatusSkip,
			"no authenticated session (supply -username/-password) to test the CSRF token gate."))
	}

	return out
}

// ============================== cors =========================================

func corsChecks() []check {
	return []check{{
		id:       "CORS",
		category: "cors",
		run:      runCORSChecks,
	}}
}

func runCORSChecks(s *Scanner) []Finding {
	var out []Finding
	evil := "https://evil-" + randToken(6) + ".example"

	// A preflight from an arbitrary origin must not be reflected back.
	r := s.do(http.MethodOptions, "/api/current", map[string]string{
		"Origin":                         evil,
		"Access-Control-Request-Method":  "GET",
		"Access-Control-Request-Headers": "authorization",
	}, "")
	if r.err != nil {
		return []Finding{finding("CORS-001", "cors", "CORS reflection", SevInfo, StatusError,
			"could not send CORS preflight: "+r.err.Error())}
	}

	acao := r.header.Get("Access-Control-Allow-Origin")
	acac := strings.EqualFold(r.header.Get("Access-Control-Allow-Credentials"), "true")

	switch {
	case acao == evil:
		out = append(out, finding("CORS-001", "cors", "CORS does not reflect arbitrary origin", SevCritical, StatusFail,
			"the server reflected an attacker-controlled Origin into Access-Control-Allow-Origin, enabling cross-site reads of authenticated data.").
			withEvidence("ACAO: %s (credentials=%v)", acao, acac).
			withRemediation("Only echo origins present in web.security.allowed_origins; never reflect the request Origin blindly."))
	case acao == "*" && acac:
		out = append(out, finding("CORS-001", "cors", "CORS wildcard with credentials", SevCritical, StatusFail,
			"Access-Control-Allow-Origin: * combined with Allow-Credentials: true exposes authenticated responses to any site.").
			withEvidence("ACAO: * ; ACAC: true").
			withRemediation("Never combine a wildcard ACAO with credentialed CORS."))
	case acao == "":
		out = append(out, finding("CORS-001", "cors", "CORS does not reflect arbitrary origin", SevMedium, StatusPass,
			"a preflight from an arbitrary foreign origin gets no Access-Control-Allow-Origin (cross-origin reads blocked)."))
	default:
		// Some other allow-listed origin was returned (not our evil one) — fine.
		out = append(out, finding("CORS-001", "cors", "CORS restricted to allow-list", SevMedium, StatusPass,
			"the foreign origin was not reflected; ACAO is restricted to the configured allow-list.").
			withEvidence("ACAO: %s", acao))
	}

	// When CORS headers are emitted, Vary: Origin must be set so shared caches
	// don't serve a cross-origin response to the wrong site.
	if acao != "" {
		if containsFold(r.header.Values("Vary"), "Origin") {
			out = append(out, finding("CORS-002", "cors", "Vary: Origin on CORS responses", SevLow, StatusPass,
				"Vary: Origin is present, preventing cross-origin cache poisoning."))
		} else {
			out = append(out, finding("CORS-002", "cors", "Vary: Origin on CORS responses", SevMedium, StatusWarn,
				"CORS headers are emitted without Vary: Origin; a shared cache could leak them to another origin.").
				withRemediation("Add Vary: Origin whenever Access-Control-Allow-Origin is set."))
		}
	}

	out = append(out, corsEdgeCaseFinding(s))
	return out
}

// corsEdgeCaseFinding probes Origins that commonly slip past sloppy allow-list
// matching: the literal "null" origin, a trailing-dot host, a host with
// embedded credentials, a case-varied host, and a look-alike subdomain. None of
// them must be reflected back into Access-Control-Allow-Origin.
func corsEdgeCaseFinding(s *Scanner) Finding {
	host := s.base.Host
	edge := []string{
		"null",
		s.base.Scheme + "://" + host + ".",             // trailing dot
		s.base.Scheme + "://user:pass@" + host,         // userinfo
		s.base.Scheme + "://" + strings.ToUpper(host),  // case variation
		s.base.Scheme + "://" + host + ".evil.example", // look-alike suffix
		s.base.Scheme + "://evil-" + host,              // look-alike prefix
	}
	var reflected []string
	for _, o := range edge {
		r := s.do(http.MethodGet, "/api/config", map[string]string{"Origin": o}, "")
		if r.err != nil {
			continue
		}
		acao := r.header.Get("Access-Control-Allow-Origin")
		if acao != "" && (acao == o || acao == "*") {
			reflected = append(reflected, fmt.Sprintf("%s → ACAO %s", o, acao))
		}
	}
	if len(reflected) == 0 {
		return finding("CORS-003", "cors", "CORS allow-list edge cases", SevMedium, StatusPass,
			"null / trailing-dot / userinfo / case / look-alike origins are not reflected.")
	}
	return finding("CORS-003", "cors", "CORS allow-list edge cases", SevHigh, StatusFail,
		"an edge-case origin slipped past CORS matching and was reflected, enabling cross-site reads.").
		withEvidence("%s", strings.Join(reflected, " | ")).
		withRemediation("Match origins by exact, normalized string equality; reject 'null' and any host that isn't an exact allow-list entry.")
}

// ============================== traversal ====================================

func traversalChecks() []check {
	return []check{{
		id:       "TRAV",
		category: "traversal",
		run:      runTraversalChecks,
	}}
}

func runTraversalChecks(s *Scanner) []Finding {
	var out []Finding
	bp := s.base.Path // "" or "/something"

	// Positive control: a real static asset is served, so negatives are meaningful.
	if code, _ := s.rawTCP("GET " + bp + "/style.css HTTP/1.1"); code != http.StatusOK {
		out = append(out, finding("TRAV-000", "traversal", "Static serving control", SevInfo, StatusWarn,
			"GET /style.css did not return 200; traversal results may be inconclusive (UI may be disabled).").
			withEvidence("status=%d", code))
	}

	// Markers that would betray a file read from outside the embedded static dir.
	leaks := []string{"module kula", "root:x:", "BEGIN ", "password_hash", "package web", "package main"}
	payloads := []string{
		"GET " + bp + "/js/../../../../etc/passwd HTTP/1.1",
		"GET " + bp + "/js/..%2f..%2f..%2fgo.mod HTTP/1.1",
		"GET " + bp + "/js/%2e%2e/%2e%2e/config.yaml HTTP/1.1",
		"GET " + bp + "/style.css/../../server.go HTTP/1.1",
		"GET " + bp + "/../go.mod HTTP/1.1",
		"GET " + bp + "/fonts/....//....//etc/passwd HTTP/1.1",
		"GET " + bp + "/js/..\\..\\..\\config.yaml HTTP/1.1",
		"GET " + bp + "/js/%2e%2e%2f%2e%2e%2fsessions.json HTTP/1.1",
	}

	var hits []string
	for _, p := range payloads {
		code, body := s.rawTCP(p)
		if code != http.StatusOK {
			continue
		}
		for _, leak := range leaks {
			if strings.Contains(body, leak) {
				hits = append(hits, p+" → leaked "+leak)
			}
		}
	}
	if len(hits) == 0 {
		out = append(out, finding("TRAV-001", "traversal", "Path traversal blocked", SevHigh, StatusPass,
			"none of the encoded / dot-dot / backslash traversal payloads returned file content from outside the static dir."))
	} else {
		out = append(out, finding("TRAV-001", "traversal", "Path traversal blocked", SevCritical, StatusFail,
			"a traversal payload leaked file content from outside the embedded static directory.").
			withEvidence("%s", strings.Join(hits, "; ")).
			withRemediation("Reject paths that escape the static root; never read user-controlled paths off disk."))
	}

	// Directory listing must not be exposed.
	var listed []string
	for _, d := range []string{"/js/", "/fonts/"} {
		code, body := s.rawTCP("GET " + bp + d + " HTTP/1.1")
		if code == http.StatusOK && (strings.Contains(body, "<a href") || strings.Contains(body, "Index of")) {
			listed = append(listed, d)
		}
	}
	if len(listed) == 0 {
		out = append(out, finding("TRAV-002", "traversal", "No directory listing", SevMedium, StatusPass,
			"static directories do not return a browsable index."))
	} else {
		out = append(out, finding("TRAV-002", "traversal", "No directory listing", SevMedium, StatusFail,
			"a static directory returned a browsable listing: "+strings.Join(listed, ", ")).
			withRemediation("Return 403/404 for directory requests; never serve an autoindex."))
	}

	return out
}

// ============================== metrics ======================================

func metricsChecks() []check {
	return []check{{
		id:       "PROM",
		category: "metrics",
		run:      runMetricsChecks,
	}}
}

func runMetricsChecks(s *Scanner) []Finding {
	r := s.do(http.MethodGet, "/metrics", nil, "")
	if r.err != nil {
		return []Finding{finding("PROM-001", "metrics", "Prometheus endpoint", SevInfo, StatusError,
			"could not query /metrics: "+r.err.Error())}
	}

	switch r.status {
	case http.StatusNotFound:
		return []Finding{finding("PROM-001", "metrics", "Prometheus endpoint", SevInfo, StatusSkip,
			"/metrics is not enabled on this instance.")}
	case http.StatusUnauthorized:
		out := []Finding{}
		if strings.Contains(r.header.Get("WWW-Authenticate"), "Bearer") {
			out = append(out, finding("PROM-001", "metrics", "Prometheus token enforced", SevMedium, StatusPass,
				"/metrics requires a bearer token and advertises the Bearer challenge."))
		} else {
			out = append(out, finding("PROM-001", "metrics", "Prometheus token enforced", SevMedium, StatusPass,
				"/metrics requires authentication (401 without a token)."))
		}
		// A wrong token must also be rejected.
		w := s.do(http.MethodGet, "/metrics", map[string]string{"Authorization": "Bearer wrong-" + randToken(8)}, "")
		if w.status == http.StatusUnauthorized {
			out = append(out, finding("PROM-002", "metrics", "Prometheus rejects wrong token", SevMedium, StatusPass,
				"an incorrect bearer token is rejected with 401."))
		} else {
			out = append(out, finding("PROM-002", "metrics", "Prometheus rejects wrong token", SevHigh, StatusFail,
				"an incorrect bearer token was not rejected").withEvidence("status=%d", w.status))
		}
		return out
	case http.StatusOK:
		return []Finding{finding("PROM-001", "metrics", "Prometheus exposed without token", SevMedium, StatusWarn,
			"/metrics is served with no authentication; it discloses the hostname and detailed system metrics to anyone who can reach the port.").
			withEvidence("%d bytes of metrics returned unauthenticated", len(r.body)).
			withRemediation("Set web.prometheus_metrics.token, or restrict /metrics at the network/proxy layer.")}
	default:
		return []Finding{finding("PROM-001", "metrics", "Prometheus endpoint", SevInfo, StatusWarn,
			"unexpected status from /metrics").withEvidence("status=%d", r.status)}
	}
}

// ============================== input ========================================

func inputChecks() []check {
	return []check{{
		id:       "INPUT",
		category: "input",
		run:      runInputChecks,
	}}
}

func runInputChecks(s *Scanner) []Finding {
	var out []Finding

	hdr, ok := s.apiHeaders()
	if !ok {
		out = append(out, finding("INPUT-001", "input", "History input validation", SevInfo, StatusSkip,
			"auth is enabled and no session is available; supply -username/-password to exercise /api/history."))
	} else {
		// Bad / inverted / oversized time ranges must be rejected with 400.
		cases := []struct{ name, query string }{
			{"malformed 'from'", "/api/history?from=not-a-time"},
			{"malformed 'to'", "/api/history?to=garbage"},
			{"inverted range", "/api/history?from=2026-01-02T00:00:00Z&to=2026-01-01T00:00:00Z"},
			{"range > 31 days", "/api/history?from=2020-01-01T00:00:00Z&to=2026-01-01T00:00:00Z"},
		}
		var bad []string
		for _, c := range cases {
			if r := s.do(http.MethodGet, c.query, hdr, ""); r.err == nil && r.status != http.StatusBadRequest {
				bad = append(bad, c.name)
			}
		}
		if len(bad) == 0 {
			out = append(out, finding("INPUT-001", "input", "History range validation", SevLow, StatusPass,
				"malformed, inverted, and over-long time ranges are all rejected with 400."))
		} else {
			out = append(out, finding("INPUT-001", "input", "History range validation", SevMedium, StatusWarn,
				"these /api/history inputs were not rejected with 400: "+strings.Join(bad, ", ")).
				withRemediation("Validate and bound the from/to window server-side."))
		}

		// An absurd points value must be capped, not cause an error/OOM.
		if r := s.do(http.MethodGet, "/api/history?points=999999999", hdr, ""); r.err == nil {
			if r.status == http.StatusOK {
				out = append(out, finding("INPUT-002", "input", "History points cap", SevLow, StatusPass,
					"a huge points value is accepted but capped (no error, no resource blow-up)."))
			} else {
				out = append(out, finding("INPUT-002", "input", "History points cap", SevInfo, StatusWarn,
					"a huge points value returned a non-200 status").withEvidence("status=%d", r.status))
			}
		}
	}

	// i18n language validation (open endpoint when auth off; behind auth when on).
	if hdr, ok := s.apiHeaders(); ok {
		var leaked []string
		for _, lang := range []string{"../../etc/passwd", "..%2f..%2fconfig.yaml", "zz", "<script>", "\x00en"} {
			r := s.do(http.MethodGet, "/api/i18n?lang="+lang, hdr, "")
			if r.err != nil {
				continue
			}
			if r.status == http.StatusOK && (strings.Contains(r.body, "root:x:") || strings.Contains(r.body, "password_hash") || strings.Contains(r.body, "module kula")) {
				leaked = append(leaked, lang)
			}
		}
		if len(leaked) == 0 {
			out = append(out, finding("INPUT-003", "input", "i18n language validation", SevMedium, StatusPass,
				"invalid / traversal language codes are rejected and never read arbitrary files."))
		} else {
			out = append(out, finding("INPUT-003", "input", "i18n language validation", SevCritical, StatusFail,
				"a crafted lang parameter leaked file content: "+strings.Join(leaked, ", ")).
				withRemediation("Validate lang against the fixed supported-language allow-list."))
		}
	}

	return out
}

// apiHeaders returns the headers needed to reach authenticated API endpoints
// and whether such access is possible. When auth is off, no headers are needed.
func (s *Scanner) apiHeaders() (map[string]string, bool) {
	if !s.authEnabled {
		return nil, true
	}
	if s.ensureSession() {
		return map[string]string{"Cookie": "kula_session=" + s.session}, true
	}
	return nil, false
}
