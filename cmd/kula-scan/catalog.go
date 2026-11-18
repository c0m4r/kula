package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// Mode says when a check runs.
//   - ModeDefault checks run on every scan.
//   - ModeAggressive checks have side effects on the target (rate-limit
//     lockout, connection floods) and run only with -aggressive.
//   - ModeFuzz checks perform blind fault injection (malformed/extreme input)
//     and run only with -fuzz.
type Mode int

const (
	ModeDefault Mode = iota
	ModeAggressive
	ModeFuzz
)

func (m Mode) String() string {
	switch m {
	case ModeAggressive:
		return "aggressive"
	case ModeFuzz:
		return "fuzz"
	default:
		return "default"
	}
}

// catalogEntry describes one finding ID the scanner can emit. The ID is the
// stable name of the finding: its category and title never vary with the
// outcome, so a finding can be selected, ignored and baselined by ID. What
// varies goes into the finding's detail.
type catalogEntry struct {
	ID          string
	Category    string
	Mode        Mode
	Title       string
	Description string // one line: what the finding verifies
}

// catalog lists every finding ID, grouped by category; -list prints it in
// this order, and categories() returns the categories in it. The scan runs
// the checks in allChecks' order instead: by category too, but most
// aggressive checks and the fuzz checks after the default ones, and the
// login and chat budgets put RATE-CHAT, RATE-LOGIN, BYPASS-XFF and
// FUZZ-LIVENESS at the end of the run. A new finding ID needs an entry here and a place in the ids of
// the registry check that emits it; TestCatalogCoversEveryEmittedID and
// TestRegistryDeclaresEveryCatalogID fail otherwise.
var catalog = []catalogEntry{
	{"DISC-001", "discovery", ModeDefault, "Target fingerprint",
		"The target answers HTTP and identifies as Kula; records whether auth and Ollama are enabled."},
	{"DISC-002", "discovery", ModeDefault, "Credentials over plaintext HTTP",
		"With auth enabled, the target is reached over HTTPS, so credentials and session cookies do not cross the wire in the clear."},
	{"DISC-003", "discovery", ModeDefault, "Scanner's Host name answered",
		"Kula's DNS-rebinding check answers the Host name the scanner sends. With auth off it refuses a public name on every route but /health, /status and a /metrics that asks for a bearer token; then the checks that would read that refusal as their own result are skipped and the run is inconclusive."},

	{"HDR-000", "headers", ModeDefault, "Fetch UI page for header inspection",
		"GET / answers, so the security headers can be inspected."},
	{"HDR-001", "headers", ModeDefault, "X-Content-Type-Options nosniff",
		"The UI page, a static asset and an API response carry X-Content-Type-Options: nosniff."},
	{"HDR-002", "headers", ModeDefault, "X-Frame-Options DENY (clickjacking)",
		"The UI page carries X-Frame-Options: DENY; without it, but with the other security headers (web.security.frame_protection off), it warns."},
	{"HDR-003", "headers", ModeDefault, "Content-Security-Policy hardened",
		"A Content-Security-Policy restricts default-src to 'self' and admits scripts only by nonce: no 'unsafe-inline', 'unsafe-eval', wildcard host (*, https://*, *.example.com) or bare scheme source."},
	{"HDR-004", "headers", ModeDefault, "CSP nonce freshness",
		"The CSP script nonce differs between two responses."},
	{"HDR-005", "headers", ModeDefault, "Referrer-Policy present",
		"Responses carry a Referrer-Policy header."},
	{"HDR-006", "headers", ModeDefault, "Permissions-Policy present",
		"Responses carry a Permissions-Policy header."},
	{"HDR-007", "headers", ModeDefault, "CSP frame-ancestors 'none'",
		"The CSP forbids framing with frame-ancestors 'none'."},
	{"HDR-008", "headers", ModeDefault, "HSTS over TLS",
		"Over HTTPS, responses carry Strict-Transport-Security with a max-age of at least 180 days."},
	{"HDR-009", "headers", ModeDefault, "Server banner disclosure",
		"No Server header names the software that answers."},
	{"HDR-010", "headers", ModeDefault, "Cache-Control no-store",
		"The UI page, which carries the per-request CSP nonce, and API responses, which carry live data, are Cache-Control: no-store."},
	{"HDR-011", "headers", ModeDefault, "CSP base-uri restricted",
		"The CSP restricts base-uri to 'self' or 'none'; base-uri does not fall back to default-src."},
	{"HDR-012", "headers", ModeDefault, "CSP form-action restricted",
		"The CSP restricts form-action to 'self' or 'none'; form-action does not fall back to default-src."},
	{"HDR-013", "headers", ModeDefault, "CSP object-src 'none'",
		"The CSP's effective object-src (object-src, else default-src) is 'none'."},

	{"AUTH-000", "auth", ModeDefault, "Authentication posture",
		"web.auth is enabled; warns when the API is served to anyone who reaches the port. Skipped when Kula serves no UI or API at the address (web.ui: false); an ERROR when /api/auth/status cannot be read, which skips every check that needs the UI or API."},
	{"AUTH-001", "auth", ModeDefault, "Protected API requires auth",
		"Protected API routes, the Ollama routes and unknown /api/ paths answer 401 without a session."},
	{"AUTH-002", "auth", ModeDefault, "Forged session cookies rejected",
		"Garbage, empty, traversal and SQL-injection session cookie values are rejected."},
	{"AUTH-003", "auth", ModeDefault, "Forged bearer token rejected",
		"A bogus Authorization: Bearer token is rejected with 401."},
	{"AUTH-004", "auth", ModeDefault, "Login rejects non-POST",
		"GET /api/login answers 405."},
	{"AUTH-005", "auth", ModeDefault, "Login with supplied credentials",
		"The supplied -username/-password open a session with a cookie and a CSRF token."},
	{"AUTH-006", "auth", ModeAggressive, "Username enumeration resistance",
		"A wrong password answers the same for a known and an unknown username."},
	{"AUTH-007", "auth", ModeDefault, "Session cookie HttpOnly",
		"The session cookie is HttpOnly."},
	{"AUTH-008", "auth", ModeDefault, "Session cookie SameSite",
		"The session cookie sets a SameSite attribute."},
	{"AUTH-009", "auth", ModeDefault, "Session cookie Secure over TLS",
		"Over HTTPS, the session cookie is Secure."},
	{"AUTH-010", "auth", ModeDefault, "Logout revokes the session",
		"After the scan's closing logout, its session cookie no longer authenticates (401)."},

	{"CSRF-001", "csrf", ModeDefault, "Origin required on state change",
		"A POST without Origin or Referer is refused with 403."},
	{"CSRF-002", "csrf", ModeDefault, "Cross-origin state change blocked",
		"A POST from a foreign Origin, or with no Origin and a foreign Referer, is refused with 403."},
	{"CSRF-003", "csrf", ModeDefault, "Synchronizer token enforced",
		"An authenticated same-origin POST without X-CSRF-Token is refused with 403."},
	{"CSRF-004", "csrf", ModeDefault, "Wrong CSRF token refused",
		"An authenticated same-origin POST with a wrong X-CSRF-Token is refused with 403."},

	{"CORS-001", "cors", ModeDefault, "CORS does not reflect arbitrary origin",
		"A preflight from a foreign Origin is not granted, nor answered with a wildcard (FAIL without auth, WARN with it)."},
	{"CORS-002", "cors", ModeDefault, "Vary: Origin on CORS responses",
		"Responses that carry CORS headers also carry Vary: Origin."},
	{"CORS-003", "cors", ModeDefault, "CORS allow-list edge cases",
		"null, trailing-dot, userinfo and look-alike variants of the target's origin are not reflected, even when it allow-lists its own origin."},

	{"HOST-001", "host", ModeDefault, "Foreign Host names refused (DNS rebinding)",
		"Without auth, the API answers no Host name a public DNS server could answer for, with a port, a trailing dot, in mixed case or behind a local label; with auth, neither does a /metrics served without a token."},

	{"TRAV-000", "traversal", ModeDefault, "Static serving control",
		"GET /style.css answers 200, so the traversal probes are meaningful."},
	{"TRAV-001", "traversal", ModeDefault, "Path traversal blocked",
		"Encoded, dot-dot and backslash traversal paths sent over a raw socket read no file outside the static directory."},
	{"TRAV-002", "traversal", ModeDefault, "No directory listing",
		"Static directories do not answer with a browsable index."},

	{"PROM-001", "metrics", ModeDefault, "Prometheus token enforced",
		"/metrics requires a bearer token; warns when it is served without one."},
	{"PROM-002", "metrics", ModeDefault, "Prometheus rejects wrong token",
		"/metrics rejects a wrong bearer token with 401."},
	{"PROM-003", "metrics", ModeDefault, "Prometheus accepts the configured token",
		"With -metrics-token, /metrics answers the right bearer token with 200 and a Prometheus exposition body."},

	{"WS-000", "ws", ModeDefault, "Same-origin WebSocket allowed",
		"A same-origin WebSocket upgrade succeeds, so the gates below are selective."},
	{"WS-001", "ws", ModeDefault, "Unauthenticated WebSocket blocked",
		"With auth enabled, a WebSocket upgrade without a session is refused with 401."},
	{"WS-002", "ws", ModeDefault, "Cross-origin WebSocket blocked (CSWSH)",
		"A WebSocket upgrade from a foreign Origin is refused with 403."},
	{"WS-FLOOD", "ws", ModeAggressive, "WebSocket per-IP connection limit",
		"Upgrades past the per-IP WebSocket limit are refused with 429."},
	{"WS-MSGBOMB", "ws", ModeAggressive, "WebSocket message size limit",
		"The server drops a WebSocket connection that sends a well-formed 64 KiB JSON command, past its read limit, instead of reading it and answering a ping after it."},

	{"INPUT-001", "input", ModeDefault, "History range validation",
		"/api/history answers 400 to malformed, inverted and over-long time ranges, a negative or non-numeric tier and an unknown section."},
	{"INPUT-002", "input", ModeDefault, "History points cap",
		"/api/history answers an absurd points value over the last two hours with no more than 5000 samples, once those hours hold more than 5000 stored records; a store that holds fewer is skipped (cap not reached)."},
	{"INPUT-003", "input", ModeDefault, "i18n language validation",
		"/api/i18n rejects invalid and traversal language codes and reads no other file."},
	{"INPUT-AGG", "input", ModeAggressive, "Login body size cap",
		"An 8 KiB login body is rejected by the 4 KiB body cap."},
	{"INPUT-CHAT", "input", ModeAggressive, "AI chat body size cap",
		"A 320 KiB chat body is refused before it is sent whole, by the 32 KiB cap; no body sent decodes, so no chat starts."},

	{"REDIR-001", "redirect", ModeDefault, "No open redirect",
		"Crafted paths do not produce a redirect whose Location, read as a browser reads it, names a foreign host."},

	{"SRI-001", "integrity", ModeDefault, "Scripts carry integrity",
		"Every external script and modulepreload link of the UI page is same-origin with a sha256, sha384 or sha512 integrity hash, and every inline script carries the CSP nonce."},
	{"SRI-002", "integrity", ModeDefault, "Served scripts match their integrity",
		"Each same-origin script the UI page names hashes, as served and content-decoded, to its strongest integrity hash."},

	{"TLS-001", "tls", ModeDefault, "TLS protocol version",
		"Over HTTPS, handshakes capped at TLS 1.1 and at TLS 1.0 are refused."},
	{"TLS-002", "tls", ModeDefault, "TLS cipher strength",
		"A TLS 1.2 handshake offering only the cipher suites on Go's insecure list is refused."},
	{"TLS-003", "tls", ModeDefault, "Certificate validity and strength",
		"The server certificate is valid now and for at least 14 more days, with no RSA key under 2048 bits and no SHA-1 signature in its chain."},
	{"TLS-004", "tls", ModeDefault, "Certificate trusted for the host name",
		"The certificate chain leads to a system root and names the target URL's host; with -insecure the scanner verifies it itself."},

	{"RATE-OLLAMA", "rate", ModeAggressive, "Ollama rate limiting",
		"A burst of Ollama requests starts answering 429."},
	{"RATE-LOGIN", "rate", ModeAggressive, "Login brute-force rate limiting",
		"A burst of failed logins starts answering 429 (locks out login from this address for about five minutes)."},
	{"RATE-CHAT", "rate", ModeAggressive, "AI chat rate limiting",
		"Chat requests past ten a minute from one address are answered 429; no body sent decodes, so no chat starts."},

	{"DOS-SLOWLORIS", "dos", ModeAggressive, "Slow-request (Slowloris) reaping",
		"Half-open requests are dropped within -dos-wait while the server keeps answering."},
	{"DOS-HEADERBOMB", "dos", ModeAggressive, "Oversized request headers rejected",
		"A 2 MiB request header block is refused."},
	{"DOS-CONNFLOOD", "dos", ModeAggressive, "Idle-connection flood resilience",
		"The server keeps answering under a flood of idle connections and drops them within -dos-wait."},
	{"DOS-HISTORY", "dos", ModeAggressive, "History request burst",
		"Each of a burst of concurrent 31-day, 5000-point history requests is answered with data or deferred with 503 and Retry-After, none hangs, and /health keeps answering."},

	{"BYPASS-XFF", "bypass", ModeAggressive, "X-Forwarded-For rate-limit bypass",
		"Rotating a spoofed X-Forwarded-For does not escape the login rate limiter."},

	{"FUZZ-QUERY", "fuzz", ModeFuzz, "Query-parameter fuzzing (/api/history, /api/i18n)",
		"Randomized hostile history (from, to, points, tier, sections) and i18n (lang) parameters, well formed and raw, produce no unexpected 5xx, dropped connection, hang or reflected input."},
	{"FUZZ-PATH", "fuzz", ModeFuzz, "Path fuzzing",
		"Randomized hostile paths, well formed and raw, produce no unexpected 5xx, dropped connection, hang or unescaped reflection."},
	{"FUZZ-BODY", "fuzz", ModeFuzz, "Malformed JSON body handling",
		"Malformed JSON bodies sent to login (at most 6, within the login limit) and the AI chat (only bodies its decoder refuses) produce no 5xx, dropped connection or hang."},
	{"FUZZ-METHODS", "fuzz", ModeFuzz, "HTTP method matrix",
		"Unusual methods, HEAD and OPTIONS on every route, and POST on the read-only API routes, sent past the auth and CSRF gates, produce no unexpected 5xx, dropped connection or hang; read-only API routes accept no POST, PUT, DELETE or PATCH, and login, logout and the AI chat no other method than POST, which is never sent to them."},
	{"FUZZ-SMUGGLE", "fuzz", ModeFuzz, "Ambiguous request framing",
		"Three requests to /api/login framed ambiguously (two Content-Lengths, Content-Length with chunked Transfer-Encoding, two Transfer-Encodings) are refused, or handled without a 2xx or a 500; one server's answers cannot show a desync with a proxy in front."},
	{"FUZZ-WS", "fuzz", ModeFuzz, "WebSocket protocol fuzzing",
		"Malformed and binary frames on six same-origin WebSocket connections, opened in turn with the session behind auth, leave HTTP and new upgrades working; a run that opens none does not pass."},
	{"FUZZ-LIVENESS", "fuzz", ModeFuzz, "Server survived the fuzz run",
		"The instance still answers /health at the end of the run, after the fuzz barrage and the probes that exhaust the login limiter."},
}

// catalogIndex maps a lowercased finding ID to its catalog entry.
var catalogIndex = func() map[string]catalogEntry {
	index := make(map[string]catalogEntry, len(catalog))
	for _, e := range catalog {
		index[strings.ToLower(e.ID)] = e
	}
	return index
}()

// lookupID returns the catalog entry for a finding ID, case-insensitively.
func lookupID(id string) (catalogEntry, bool) {
	e, ok := catalogIndex[strings.ToLower(id)]
	return e, ok
}

// catalogEntryFor returns the entry for id, or a stand-in titled with the ID
// itself in category "unknown", so a finding missing from the catalog still
// renders instead of panicking.
func catalogEntryFor(id string) catalogEntry {
	if e, ok := lookupID(id); ok {
		return e
	}
	return catalogEntry{ID: id, Category: "unknown", Title: id}
}

// categories returns every category in the catalog, in catalog order.
func categories() []string {
	var out []string
	seen := map[string]bool{}
	for _, e := range catalog {
		if !seen[e.Category] {
			seen[e.Category] = true
			out = append(out, e.Category)
		}
	}
	return out
}

// writeCatalog prints the catalog as a table, for -list.
func writeCatalog(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tCATEGORY\tMODE\tTITLE")
	for _, e := range catalog {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", e.ID, e.Category, e.Mode, e.Title)
	}
	return tw.Flush()
}
