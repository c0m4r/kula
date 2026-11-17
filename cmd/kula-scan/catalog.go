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

// catalog lists every finding ID, grouped by category in the order the scan
// runs them. A new finding ID needs an entry here and a place in the ids of
// the registry check that emits it; TestCatalogCoversEveryEmittedID and
// TestRegistryDeclaresEveryCatalogID fail otherwise.
var catalog = []catalogEntry{
	{"DISC-001", "discovery", ModeDefault, "Target fingerprint",
		"The target answers HTTP and identifies as Kula; records whether auth and Ollama are enabled."},
	{"DISC-002", "discovery", ModeDefault, "Credentials over plaintext HTTP",
		"With auth enabled, the target is reached over HTTPS, so credentials and session cookies do not cross the wire in the clear."},

	{"HDR-000", "headers", ModeDefault, "Fetch UI page for header inspection",
		"GET / answers, so the security headers can be inspected."},
	{"HDR-001", "headers", ModeDefault, "X-Content-Type-Options nosniff",
		"The UI page, a static asset and an API response carry X-Content-Type-Options: nosniff."},
	{"HDR-002", "headers", ModeDefault, "X-Frame-Options DENY (clickjacking)",
		"Responses carry X-Frame-Options: DENY."},
	{"HDR-003", "headers", ModeDefault, "Content-Security-Policy hardened",
		"A Content-Security-Policy restricts default-src to 'self' and admits scripts only by nonce: no 'unsafe-inline', 'unsafe-eval', wildcard or bare scheme source."},
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
		"web.auth is enabled; warns when the API is served to anyone who reaches the port."},
	{"AUTH-001", "auth", ModeDefault, "Protected API requires auth",
		"Protected API routes answer 401 without a session."},
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

	{"CSRF-001", "csrf", ModeDefault, "Origin required on state change",
		"A POST without Origin or Referer is refused with 403."},
	{"CSRF-002", "csrf", ModeDefault, "Cross-origin state change blocked",
		"A POST from a foreign Origin is refused with 403."},
	{"CSRF-003", "csrf", ModeDefault, "Synchronizer token enforced",
		"An authenticated same-origin POST without X-CSRF-Token is refused with 403."},

	{"CORS-001", "cors", ModeDefault, "CORS does not reflect arbitrary origin",
		"A preflight from a foreign Origin is not granted, and never with a wildcard plus credentials."},
	{"CORS-002", "cors", ModeDefault, "Vary: Origin on CORS responses",
		"Responses that carry CORS headers also carry Vary: Origin."},
	{"CORS-003", "cors", ModeDefault, "CORS allow-list edge cases",
		"null, trailing-dot, userinfo, case-varied and look-alike origins are not reflected."},

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
		"The server drops a WebSocket connection that sends a message past its read limit."},

	{"INPUT-001", "input", ModeDefault, "History range validation",
		"/api/history answers 400 to malformed, inverted and over-long time ranges."},
	{"INPUT-002", "input", ModeDefault, "History points cap",
		"/api/history caps an absurd points value instead of failing."},
	{"INPUT-003", "input", ModeDefault, "i18n language validation",
		"/api/i18n rejects invalid and traversal language codes and reads no other file."},
	{"INPUT-AGG", "input", ModeAggressive, "Login body size cap",
		"An 8 KiB login body is rejected by the 4 KiB body cap."},

	{"REDIR-001", "redirect", ModeDefault, "No open redirect",
		"Crafted paths do not produce a redirect to a foreign host."},

	{"TLS-001", "tls", ModeDefault, "TLS protocol version",
		"Over HTTPS, the connection negotiates TLS 1.2 or later."},
	{"TLS-002", "tls", ModeDefault, "TLS cipher strength",
		"The negotiated cipher suite is not on Go's insecure list."},
	{"TLS-003", "tls", ModeDefault, "Certificate validity",
		"The server certificate is valid for at least 14 more days."},

	{"RATE-OLLAMA", "rate", ModeAggressive, "Ollama rate limiting",
		"A burst of Ollama requests starts answering 429."},
	{"RATE-LOGIN", "rate", ModeAggressive, "Login brute-force rate limiting",
		"A burst of failed logins starts answering 429 (locks out login from this address for about five minutes)."},

	{"DOS-SLOWLORIS", "dos", ModeAggressive, "Slow-request (Slowloris) reaping",
		"Half-open requests are dropped within -dos-wait while the server keeps answering."},
	{"DOS-HEADERBOMB", "dos", ModeAggressive, "Oversized request headers rejected",
		"A 2 MiB request header block is refused."},
	{"DOS-CONNFLOOD", "dos", ModeAggressive, "Idle-connection flood resilience",
		"The server keeps answering under a flood of idle connections and drops them within -dos-wait."},

	{"BYPASS-XFF", "bypass", ModeAggressive, "X-Forwarded-For rate-limit bypass",
		"Rotating a spoofed X-Forwarded-For does not escape the login rate limiter."},

	{"FUZZ-QUERY", "fuzz", ModeFuzz, "Query-parameter fuzzing (/api/history, /api/i18n)",
		"Randomized hostile query parameters produce no 5xx, dropped connection, hang or reflected input."},
	{"FUZZ-PATH", "fuzz", ModeFuzz, "Path fuzzing",
		"Randomized hostile paths produce no 5xx, dropped connection, hang or unescaped reflection."},
	{"FUZZ-BODY", "fuzz", ModeFuzz, "Malformed JSON body handling",
		"Malformed JSON bodies sent to login and the Ollama chat produce no 5xx, dropped connection or hang."},
	{"FUZZ-METHODS", "fuzz", ModeFuzz, "HTTP method matrix",
		"Unusual methods on every route produce no 5xx, and POST-only routes accept no other verb."},
	{"FUZZ-SMUGGLE", "fuzz", ModeFuzz, "Request smuggling / desync",
		"Conflicting Content-Length and Transfer-Encoding framing is rejected."},
	{"FUZZ-WS", "fuzz", ModeFuzz, "WebSocket protocol fuzzing",
		"Malformed and binary WebSocket frames leave HTTP and new upgrades working."},
	{"FUZZ-LIVENESS", "fuzz", ModeFuzz, "Server survived the fuzz run",
		"The instance still answers /health after the fuzz barrage."},
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

// categories returns every category in the catalog, in scan order.
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
