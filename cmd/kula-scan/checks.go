package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// This file holds the non-disruptive safeguard probes, grouped by category.
// Each probe returns one or more Findings; PASS means the safeguard is present
// and behaves as expected, FAIL means it is missing or bypassable.

// ============================== headers ======================================

func headerChecks() []check {
	return []check{{
		ids: []string{"HDR-000", "HDR-001", "HDR-002", "HDR-003", "HDR-004", "HDR-005", "HDR-006", "HDR-007", "HDR-008", "HDR-009",
			"HDR-010", "HDR-011", "HDR-012", "HDR-013"},
		run: runHeaderChecks,
	}}
}

// probed is the answer to a request for path.
type probed struct {
	path string
	r    httpResult
}

func runHeaderChecks(s *Scanner) []Finding {
	var out []Finding

	r := s.do(http.MethodGet, "/", nil, "")
	if r.err != nil {
		return []Finding{finding("HDR-000", SevInfo, StatusError,
			"could not GET / to inspect security headers: "+r.err.Error())}
	}
	h := r.header

	// A static asset and an API response, which need no session: headers
	// every response must carry are checked on them too.
	asset := probed{"/style.css", s.do(http.MethodGet, "/style.css", nil, "")}
	status := probed{"/api/auth/status", s.do(http.MethodGet, "/api/auth/status", nil, "")}
	index := probed{"/", r}

	want := func(id, header, expect string, sev Severity) Finding {
		got := h.Get(header)
		if expect == "" {
			if got != "" {
				return finding(id, sev, StatusPass, header+" is set").withEvidence("%s: %s", header, got)
			}
			return finding(id, sev, StatusFail, header+" is missing").
				withRemediation("Enable web.security.headers in config.yaml.")
		}
		if strings.EqualFold(strings.TrimSpace(got), expect) {
			return finding(id, sev, StatusPass, header+" is correctly set").withEvidence("%s: %s", header, got)
		}
		return finding(id, sev, StatusFail, header+" missing or wrong (want "+expect+")").
			withEvidence("%s: %q", header, got).
			withRemediation("Enable web.security.headers (and web.security.frame_protection) in config.yaml.")
	}

	out = append(out,
		nosniffFinding([]probed{index, asset, status}),
		want("HDR-002", "X-Frame-Options", "DENY", SevMedium),
		want("HDR-005", "Referrer-Policy", "", SevLow),
		want("HDR-006", "Permissions-Policy", "", SevLow),
	)

	csp := strings.Join(h.Values("Content-Security-Policy"), ", ")
	out = append(out, cspFindings(parseCSP(h.Values("Content-Security-Policy")), csp)...)

	// Nonce freshness: a reused nonce defeats the whole point of a nonce-CSP.
	if strings.Contains(csp, "nonce-") {
		r2 := s.do(http.MethodGet, "/", nil, "")
		n1, n2 := extractNonce(csp), extractNonce(r2.header.Get("Content-Security-Policy"))
		switch {
		case r2.err != nil:
			out = append(out, finding("HDR-004", SevInfo, StatusError,
				"could not GET / a second time to compare nonces: "+r2.err.Error()))
		case n1 == "" || n2 == "":
			out = append(out, finding("HDR-004", SevMedium, StatusWarn,
				"could not extract nonces from two responses to compare."))
		case n1 == n2:
			out = append(out, finding("HDR-004", SevHigh, StatusFail,
				"the CSP nonce is identical across two requests; a static nonce is trivially guessable and defeats the policy.").
				withEvidence("nonce=%s on both responses", n1).
				withRemediation("The nonce must be regenerated per request (this is a build/proxy caching problem)."))
		default:
			out = append(out, finding("HDR-004", SevMedium, StatusPass,
				"the CSP nonce differs between requests (freshly random per response)."))
		}
	}

	out = append(out, hstsFinding(s, h))

	// Banner / version disclosure.
	if banner := h.Get("Server"); banner != "" {
		out = append(out, finding("HDR-009", SevInfo, StatusWarn,
			"a Server header is present; minor information disclosure.").withEvidence("Server: %s", banner))
	}

	// The API answers /api/config to the scanner when auth is off or a
	// session is open; it carries the most data of the open routes.
	cached := []probed{index, status}
	if hdr, ok := s.apiHeaders(); ok {
		cached = append(cached, probed{"/api/config", s.do(http.MethodGet, "/api/config", hdr, "")})
	}
	out = append(out, noStoreFinding(cached))

	return out
}

// nosniffFinding judges HDR-001 over every response in responses: each must
// carry X-Content-Type-Options: nosniff.
func nosniffFinding(responses []probed) Finding {
	var missing, unreached []string
	for _, p := range responses {
		if p.r.err != nil {
			unreached = append(unreached, p.path)
			continue
		}
		if got := p.r.header.Get("X-Content-Type-Options"); !strings.EqualFold(strings.TrimSpace(got), "nosniff") {
			missing = append(missing, fmt.Sprintf("%s (%q)", p.path, got))
		}
	}
	switch {
	case len(unreached) == len(responses):
		return finding("HDR-001", SevInfo, StatusError,
			"could not fetch any response to inspect X-Content-Type-Options.")
	case len(missing) > 0:
		return finding("HDR-001", SevMedium, StatusFail,
			"X-Content-Type-Options: nosniff is missing on "+strings.Join(missing, ", ")+unreachedNote(unreached)).
			withRemediation("Enable web.security.headers in config.yaml; Kula sets nosniff on every response.")
	case len(unreached) > 0:
		return finding("HDR-001", SevMedium, StatusPass,
			"every response fetched carries X-Content-Type-Options: nosniff"+unreachedNote(unreached)+".")
	default:
		return finding("HDR-001", SevMedium, StatusPass,
			"the UI page, a static asset and an API response all carry X-Content-Type-Options: nosniff.")
	}
}

// unreachedNote names the paths a probe could not fetch, for a detail line.
func unreachedNote(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	return "; could not fetch " + strings.Join(paths, ", ")
}

// noStoreFinding judges HDR-010: every successful response in responses is
// Cache-Control: no-store. The index page carries the per-request CSP nonce,
// which a cached copy would serve twice, and API responses carry live,
// session-bound data. An answer other than 2xx (an index page the UI does not
// serve, a refused Host) carries neither and is not judged.
func noStoreFinding(responses []probed) Finding {
	var missing, judged, unreached, other []string
	for _, p := range responses {
		switch {
		case p.r.err != nil:
			unreached = append(unreached, p.path)
		case p.r.status < 200 || p.r.status > 299:
			other = append(other, fmt.Sprintf("%s (HTTP %d)", p.path, p.r.status))
		default:
			judged = append(judged, p.path)
			value := strings.Join(p.r.header.Values("Cache-Control"), ", ")
			if !hasDirective(value, "no-store") {
				missing = append(missing, fmt.Sprintf("%s (Cache-Control: %q)", p.path, value))
			}
		}
	}
	notJudged := ""
	if len(other) > 0 {
		notJudged = "; not judged: " + strings.Join(other, ", ")
	}
	switch {
	case len(unreached) == len(responses):
		return finding("HDR-010", SevInfo, StatusError,
			"could not fetch any response to inspect Cache-Control.")
	case len(judged) == 0:
		return finding("HDR-010", SevInfo, StatusSkip,
			"no response answered 2xx, so none carries a nonce or live data to protect"+notJudged+unreachedNote(unreached)+".")
	case len(missing) > 0:
		return finding("HDR-010", SevMedium, StatusFail,
			"these responses may be kept by a browser or shared cache, which would serve a CSP nonce twice or session data to another user: "+
				strings.Join(missing, ", ")+notJudged+unreachedNote(unreached)).
			withRemediation("Upgrade Kula (0.22.0 and later mark the UI page and API responses no-store), and keep any proxy in front from overriding Cache-Control.")
	default:
		return finding("HDR-010", SevMedium, StatusPass,
			"Cache-Control: no-store on "+strings.Join(judged, ", ")+notJudged+unreachedNote(unreached)+".")
	}
}

// hasDirective reports whether a comma-separated header value, such as
// Cache-Control, carries the directive name (with or without a value).
func hasDirective(value, name string) bool {
	for _, part := range strings.Split(value, ",") {
		directive, _, _ := strings.Cut(part, "=")
		if strings.EqualFold(strings.TrimSpace(directive), name) {
			return true
		}
	}
	return false
}

// minHSTSMaxAge is the shortest HSTS max-age HDR-008 accepts: 180 days.
const minHSTSMaxAge = 180 * 24 * 60 * 60

// hstsFinding judges HDR-008. HSTS means something only over TLS: over plain
// HTTP the finding is a SKIP, or a FAIL when -expect hsts declares it.
func hstsFinding(s *Scanner, h http.Header) Finding {
	if !s.https {
		if s.expects("hsts") {
			return finding("HDR-008", SevLow, StatusFail,
				"the target was reached over plain HTTP, where no Strict-Transport-Security applies.").
				withRemediation("Serve Kula over HTTPS (TLS or a TLS-terminating proxy with trust_proxy), and scan it by its https:// URL.").
				expecting("hsts")
		}
		return finding("HDR-008", SevInfo, StatusSkip,
			"HSTS applies over TLS only; the target was reached over plain HTTP.")
	}
	value := h.Get("Strict-Transport-Security")
	if value == "" {
		return finding("HDR-008", SevLow, StatusWarn,
			"served over HTTPS but no Strict-Transport-Security header.").
			withRemediation("Enable web.security.headers; HSTS is emitted automatically over TLS.").
			expecting("hsts")
	}
	maxAge, ok := hstsMaxAge(value)
	switch {
	case !ok:
		return finding("HDR-008", SevLow, StatusWarn,
			"Strict-Transport-Security carries no valid max-age, so browsers ignore it.").
			withEvidence("Strict-Transport-Security: %s", value).
			withRemediation("Send a max-age of at least 15552000 seconds (180 days); Kula sends one year.").
			expecting("hsts")
	case maxAge == 0:
		return finding("HDR-008", SevLow, StatusWarn,
			"Strict-Transport-Security has max-age=0, which tells browsers to forget HSTS for this host.").
			withEvidence("Strict-Transport-Security: %s", value).
			withRemediation("Send a max-age of at least 15552000 seconds (180 days); Kula sends one year.").
			expecting("hsts")
	case maxAge < minHSTSMaxAge:
		return finding("HDR-008", SevLow, StatusWarn,
			fmt.Sprintf("Strict-Transport-Security max-age is %d seconds (%d days), under 180 days.", maxAge, maxAge/86400)).
			withEvidence("Strict-Transport-Security: %s", value).
			withRemediation("Send a max-age of at least 15552000 seconds (180 days); Kula sends one year. A proxy in front may be rewriting the header.")
	default:
		return finding("HDR-008", SevLow, StatusPass,
			fmt.Sprintf("Strict-Transport-Security is set with a max-age of %d days.", maxAge/86400)).
			withEvidence("Strict-Transport-Security: %s", value)
	}
}

// hstsMaxAge returns the max-age directive of a Strict-Transport-Security
// value, in seconds; ok is false when it has none or it is not a number.
func hstsMaxAge(value string) (int64, bool) {
	for _, part := range strings.Split(value, ";") {
		name, arg, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found || !strings.EqualFold(strings.TrimSpace(name), "max-age") {
			continue
		}
		n, err := strconv.ParseInt(strings.Trim(strings.TrimSpace(arg), `"`), 10, 64)
		if err != nil || n < 0 {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// cspPolicy is one Content-Security-Policy: each directive, by its lower-case
// name, with its source list as sent.
type cspPolicy map[string][]string

// parseCSP parses Content-Security-Policy header values. A value may carry
// several policies separated by commas; each is enforced, so a resource must
// pass all of them. Within a policy the first occurrence of a directive wins,
// as in browsers.
func parseCSP(values []string) []cspPolicy {
	var policies []cspPolicy
	for _, value := range values {
		for _, text := range strings.Split(value, ",") {
			policy := cspPolicy{}
			for _, directive := range strings.Split(text, ";") {
				fields := strings.Fields(directive)
				if len(fields) == 0 {
					continue
				}
				name := strings.ToLower(fields[0])
				if _, seen := policy[name]; !seen {
					policy[name] = fields[1:]
				}
			}
			if len(policy) > 0 {
				policies = append(policies, policy)
			}
		}
	}
	return policies
}

// effective returns the source list that governs directive: its own, or
// default-src's when it falls back to that one. ok is false when neither is set.
func (p cspPolicy) effective(directive string, fallsBack bool) (name string, sources []string, ok bool) {
	if sources, ok := p[directive]; ok {
		return directive, sources, true
	}
	if fallsBack {
		if sources, ok := p["default-src"]; ok {
			return "default-src", sources, true
		}
	}
	return directive, nil, false
}

// restrictedTo reports whether directive is set to exactly one of keywords
// ('self', 'none'), compared case-insensitively as browsers compare keywords.
func (p cspPolicy) restrictedTo(directive string, fallsBack bool, keywords ...string) bool {
	_, sources, ok := p.effective(directive, fallsBack)
	if !ok || len(sources) != 1 {
		return false
	}
	for _, k := range keywords {
		if strings.EqualFold(sources[0], k) {
			return true
		}
	}
	return false
}

// scriptProblems lists what in the effective script policy (script-src, else
// default-src) admits a script without the nonce; none when scripts run only
// by nonce. name is the directive that governs scripts.
func (p cspPolicy) scriptProblems() (name string, problems []string) {
	name, sources, ok := p.effective("script-src", true)
	if !ok {
		return "script-src", []string{"is not set (nor default-src)"}
	}
	nonce := false
	for _, src := range sources {
		lower := strings.ToLower(src)
		switch {
		case strings.HasPrefix(lower, "'nonce-") && len(lower) > len("'nonce-'"):
			nonce = true
		case lower == "'unsafe-inline'", lower == "'unsafe-eval'", lower == "*", isSchemeSource(lower):
			problems = append(problems, "allows "+src)
		}
	}
	if !nonce {
		problems = append(problems, "carries no nonce")
	}
	return name, problems
}

// isSchemeSource reports whether a CSP source is a bare scheme, such as
// https: or data:, which admits anything served over that scheme.
func isSchemeSource(src string) bool {
	scheme, ok := strings.CutSuffix(src, ":")
	if !ok || scheme == "" {
		return false
	}
	for i, c := range scheme {
		switch {
		case c >= 'a' && c <= 'z':
		case i > 0 && (c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'):
		default:
			return false
		}
	}
	return true
}

// anyPolicy reports whether one of policies satisfies ok. Every policy is
// enforced, so a restriction any one of them makes holds.
func anyPolicy(policies []cspPolicy, ok func(cspPolicy) bool) bool {
	for _, p := range policies {
		if ok(p) {
			return true
		}
	}
	return false
}

// cspFindings judges the Content-Security-Policy: whether it contains scripts
// (HDR-003), forbids framing (HDR-007), and pins the directives that do not
// fall back to default-src (HDR-011, HDR-012) and object-src (HDR-013). raw is
// the policy as sent, for evidence.
func cspFindings(policies []cspPolicy, raw string) []Finding {
	const upgrade = "Upgrade Kula (0.22.0 and later send it), and keep any proxy in front from rewriting the Content-Security-Policy."
	if len(policies) == 0 {
		noCSP := "no Content-Security-Policy header (HDR-003)."
		return []Finding{
			finding("HDR-003", SevHigh, StatusFail,
				"no Content-Security-Policy header; the dashboard has no script/XSS containment.").
				withRemediation("Enable web.security.headers in config.yaml."),
			finding("HDR-007", SevMedium, StatusWarn,
				"no Content-Security-Policy, so no frame-ancestors: only X-Frame-Options (HDR-002) can forbid framing.").
				withRemediation("Enable web.security.headers and web.security.frame_protection.").
				expecting("frame-protection"),
			finding("HDR-011", SevLow, StatusWarn, noCSP).withRemediation("Enable web.security.headers in config.yaml."),
			finding("HDR-012", SevLow, StatusWarn, noCSP).withRemediation("Enable web.security.headers in config.yaml."),
			finding("HDR-013", SevLow, StatusWarn, noCSP).withRemediation("Enable web.security.headers in config.yaml."),
		}
	}
	var out []Finding

	// HDR-003: default-src is 'self' (or 'none') and scripts run only by nonce.
	var problems []string
	if !anyPolicy(policies, func(p cspPolicy) bool { return p.restrictedTo("default-src", false, "'self'", "'none'") }) {
		if sources, ok := policies[0]["default-src"]; ok {
			problems = append(problems, fmt.Sprintf("default-src is %q, not 'self'", strings.Join(sources, " ")))
		} else {
			problems = append(problems, "default-src is not set")
		}
	}
	if !anyPolicy(policies, func(p cspPolicy) bool { _, issues := p.scriptProblems(); return len(issues) == 0 }) {
		name, issues := policies[0].scriptProblems()
		problems = append(problems, name+" "+strings.Join(issues, ", "))
	}
	if len(problems) == 0 {
		out = append(out, finding("HDR-003", SevHigh, StatusPass,
			"CSP constrains default-src to 'self' and admits scripts only by a per-request nonce.").withEvidence("CSP: %s", raw))
	} else {
		out = append(out, finding("HDR-003", SevHigh, StatusFail,
			"the CSP does not contain scripts: "+strings.Join(problems, "; ")+".").withEvidence("CSP: %s", raw).
			withRemediation("Re-enable web.security.headers, and keep any proxy in front from rewriting the Content-Security-Policy; Kula sends default-src 'self' and a nonce-only script-src."))
	}

	// HDR-007: frame-ancestors 'none' is the modern clickjacking control.
	if anyPolicy(policies, func(p cspPolicy) bool { return p.restrictedTo("frame-ancestors", false, "'none'") }) {
		out = append(out, finding("HDR-007", SevMedium, StatusPass,
			"CSP forbids framing (frame-ancestors 'none')."))
	} else {
		out = append(out, finding("HDR-007", SevMedium, StatusWarn,
			"CSP does not set frame-ancestors 'none'; relies on X-Frame-Options alone.").
			withRemediation("Enable web.security.frame_protection.").
			expecting("frame-protection"))
	}

	// HDR-011, HDR-012: base-uri and form-action do not fall back to
	// default-src. Without base-uri an injected <base> moves the relative URLs
	// of the nonce'd scripts to another host; without form-action an injected
	// form submits anywhere.
	for _, d := range []struct{ id, directive string }{{"HDR-011", "base-uri"}, {"HDR-012", "form-action"}} {
		switch {
		case anyPolicy(policies, func(p cspPolicy) bool { return p.restrictedTo(d.directive, false, "'self'", "'none'") }):
			out = append(out, finding(d.id, SevLow, StatusPass,
				"CSP restricts "+d.directive+" to 'self' or 'none'."))
		default:
			detail := "CSP sets no " + d.directive + ", which does not fall back to default-src."
			if sources, ok := policies[0][d.directive]; ok {
				detail = fmt.Sprintf("CSP sets %s %q, not 'self' or 'none'.", d.directive, strings.Join(sources, " "))
			}
			out = append(out, finding(d.id, SevLow, StatusWarn, detail).withEvidence("CSP: %s", raw).withRemediation(upgrade))
		}
	}

	// HDR-013: object-src falls back to default-src, but 'self' still admits
	// plugin content from the origin; 'none' admits none.
	if anyPolicy(policies, func(p cspPolicy) bool { return p.restrictedTo("object-src", true, "'none'") }) {
		out = append(out, finding("HDR-013", SevLow, StatusPass,
			"CSP sets object-src 'none'."))
	} else {
		detail := "CSP sets neither object-src nor default-src, so plugin content may load from anywhere."
		if name, sources, ok := policies[0].effective("object-src", true); ok {
			detail = fmt.Sprintf("the effective object-src (%s %q) is not 'none'.", name, strings.Join(sources, " "))
		}
		out = append(out, finding("HDR-013", SevLow, StatusWarn, detail).
			withEvidence("CSP: %s", raw).withRemediation(upgrade))
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

// protectedRoute is an API request that must answer 401 without a session
// when web.auth is enabled.
type protectedRoute struct{ method, path string }

func (p protectedRoute) String() string {
	if p.method == http.MethodGet {
		return p.path
	}
	return p.method + " " + p.path
}

// protectedRoutes lists the requests AUTH-001 sends without a session. Kula
// puts every route under /api/ but login, logout and auth/status behind
// AuthMiddleware, whatever features are enabled, so the Ollama routes and an
// unknown path answer 401 too. All but the chat are read-only GETs; the chat
// is sent no body, so a broken gate answers it 400 without reaching Ollama.
func protectedRoutes() []protectedRoute {
	return []protectedRoute{
		{http.MethodGet, "/api/current"},
		{http.MethodGet, "/api/history"},
		{http.MethodGet, "/api/config"},
		{http.MethodGet, "/api/i18n?lang=en"},
		{http.MethodGet, "/api/system-info"},
		{http.MethodGet, "/api/ollama/models"},
		{http.MethodGet, "/api/ollama/context"},
		{http.MethodPost, "/api/ollama/chat"},
		{http.MethodGet, "/api/kula-scan-" + randToken(8)},
	}
}

func authChecks() []check {
	return []check{
		// AUTH-010 is judged by the run's closing logout (closeSession).
		{ids: []string{"AUTH-000", "AUTH-001", "AUTH-002", "AUTH-003", "AUTH-004", "AUTH-005", "AUTH-007", "AUTH-008", "AUTH-009", "AUTH-010"}, run: runAuthChecks},
		// A wrong password for the supplied account counts against that
		// account's login limit (five per address in five minutes), so three
		// default runs would lock it out.
		{ids: []string{"AUTH-006"}, run: func(s *Scanner) []Finding {
			return []Finding{enumerationFinding(s)}
		}},
	}
}

func runAuthChecks(s *Scanner) []Finding {
	var out []Finding

	if !s.authEnabled {
		out = append(out, finding("AUTH-000", SevMedium, StatusWarn,
			"web.auth is disabled: the API, history, and config endpoints are served to anyone who can reach the port.").
			withRemediation("Enable web.auth (and put kula behind TLS) unless access is restricted at the network layer.").
			expecting("auth"))
		return out
	}

	// AUTH-001: protected routes reject anonymous access.
	routes := protectedRoutes()
	var leaked, unreached []string
	for _, p := range routes {
		var hdr map[string]string
		if p.method != http.MethodGet {
			// Same-origin, so origin validation lets it through to the gate.
			hdr = map[string]string{"Origin": s.base.Scheme + "://" + s.base.Host, "Content-Type": "application/json"}
		}
		r := s.do(p.method, p.path, hdr, "")
		switch {
		case r.err != nil:
			unreached = append(unreached, p.String())
		case r.status != http.StatusUnauthorized:
			leaked = append(leaked, fmt.Sprintf("%s (HTTP %d)", p, r.status))
		}
	}
	switch {
	case len(leaked) > 0:
		out = append(out, finding("AUTH-001", SevHigh, StatusFail,
			"these routes answered without authentication instead of 401: "+strings.Join(leaked, ", ")+unreachedNote(unreached)).
			withRemediation("This indicates a broken auth gate; routes under /api/ must sit behind AuthMiddleware."))
	case len(unreached) == len(routes):
		out = append(out, finding("AUTH-001", SevInfo, StatusError,
			"could not send any request to the protected API routes."))
	default:
		out = append(out, finding("AUTH-001", SevHigh, StatusPass,
			fmt.Sprintf("all %d protected API requests (data, Ollama, an unknown /api/ path) answer 401 without credentials%s.",
				len(routes)-len(unreached), unreachedNote(unreached))))
	}

	// AUTH-002: forged session cookies are rejected.
	forged := []string{"deadbeef", "", strings.Repeat("a", 64), "../../etc/passwd", "' OR '1'='1"}
	var accepted []string
	failed := 0
	for _, tok := range forged {
		hdr := map[string]string{"Cookie": "kula_session=" + tok}
		r := s.do(http.MethodGet, "/api/i18n?lang=en", hdr, "")
		switch {
		case r.err != nil:
			failed++
		case r.status == http.StatusOK:
			accepted = append(accepted, tok)
		}
	}
	switch {
	case len(accepted) > 0:
		out = append(out, finding("AUTH-002", SevCritical, StatusFail,
			"a forged session value authenticated successfully").withEvidence("accepted: %q", accepted).
			withRemediation("Session validation must hash-and-lookup the token; never accept attacker-supplied values."))
	case failed == len(forged):
		out = append(out, finding("AUTH-002", SevInfo, StatusError,
			"could not send any request with a forged session cookie."))
	default:
		detail := "garbage, empty, traversal, and SQLi-style session values are all rejected."
		if failed > 0 {
			detail = fmt.Sprintf("the forged session values that reached the target were rejected; %d of %d requests failed.", failed, len(forged))
		}
		out = append(out, finding("AUTH-002", SevHigh, StatusPass, detail))
	}

	// AUTH-003: garbage bearer token is rejected.
	switch r := s.do(http.MethodGet, "/api/i18n?lang=en", map[string]string{"Authorization": "Bearer not-a-real-token"}, ""); {
	case r.err != nil:
		out = append(out, finding("AUTH-003", SevInfo, StatusError,
			"could not send a request with a bogus bearer token: "+r.err.Error()))
	case r.status == http.StatusUnauthorized:
		out = append(out, finding("AUTH-003", SevHigh, StatusPass,
			"a bogus Authorization: Bearer token is rejected with 401."))
	default:
		out = append(out, finding("AUTH-003", SevCritical, StatusFail,
			"a bogus bearer token was accepted").withEvidence("status=%d", r.status))
	}

	// AUTH-004: login method enforcement.
	switch r := s.do(http.MethodGet, "/api/login", nil, ""); {
	case r.err != nil:
		out = append(out, finding("AUTH-004", SevInfo, StatusError,
			"could not send GET /api/login: "+r.err.Error()))
	case r.status == http.StatusMethodNotAllowed:
		out = append(out, finding("AUTH-004", SevLow, StatusPass,
			"GET /api/login returns 405."))
	default:
		out = append(out, finding("AUTH-004", SevLow, StatusWarn,
			"GET /api/login did not return 405").withEvidence("status=%d", r.status))
	}

	// AUTH-005: happy-path login + session cookie attributes (needs creds).
	if s.username == "" || s.password == "" {
		out = append(out, finding("AUTH-005", SevInfo, StatusSkip,
			"no -username/-password supplied; skipping authenticated login probe."))
	} else if !s.ensureSession() {
		// The checks that need a session skip, so the run is inconclusive.
		out = append(out, finding("AUTH-005", SevInfo, StatusError,
			"could not log in with the supplied credentials: "+s.loginDetail+"; the checks that need a session are skipped."))
	} else {
		out = append(out, finding("AUTH-005", SevInfo, StatusPass,
			"the supplied credentials logged in and returned a session cookie + CSRF token."))
		out = append(out, cookieAttributeFindings(s)...)
	}

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
		out = append(out, finding("AUTH-007", SevMedium, StatusPass,
			"the session cookie is HttpOnly (not readable from JavaScript)."))
	} else {
		out = append(out, finding("AUTH-007", SevHigh, StatusFail,
			"the session cookie is NOT HttpOnly; XSS could steal it.").
			withRemediation("Session cookies must be issued HttpOnly."))
	}
	if c.SameSite != http.SameSiteDefaultMode {
		out = append(out, finding("AUTH-008", SevLow, StatusPass,
			"the session cookie sets a SameSite attribute.").withEvidence("SameSite=%d", c.SameSite))
	} else {
		out = append(out, finding("AUTH-008", SevLow, StatusWarn,
			"the session cookie has no SameSite attribute."))
	}
	if s.https {
		if c.Secure {
			out = append(out, finding("AUTH-009", SevMedium, StatusPass,
				"the session cookie is marked Secure."))
		} else {
			out = append(out, finding("AUTH-009", SevHigh, StatusFail,
				"served over HTTPS but the session cookie is not Secure; it can leak over a downgraded request.").
				withRemediation("Ensure TLS/trust_proxy is detected so cookies get the Secure flag."))
		}
	}
	return out
}

// closeSession ends the run's session with the closing logout. When probe is
// set (the check that declares AUTH-010 ran), that logout is also the AUTH-010
// probe: the session cookie it ended must no longer authenticate. It costs no
// login, since it replays the session the scan opened anyway. The finding goes
// after the other auth findings, so it is reported once, under auth. An
// interrupted run still logs out, but does not wait for the replay.
func (s *Scanner) closeSession(findings []Finding, probe bool) []Finding {
	ctx, cancel := s.closingContext()
	defer cancel()
	if !probe || !s.authEnabled {
		s.logout(ctx)
		return findings
	}
	var f Finding
	switch {
	case s.interrupted:
		s.logout(ctx)
		f = finding("AUTH-010", SevInfo, StatusSkip,
			"the run was interrupted: the session was logged out, but whether the logout revoked it was not checked.")
	case !s.loggedIn:
		f = finding("AUTH-010", SevInfo, StatusSkip,
			"no session to log out and replay: "+s.noSessionReason()+".")
	default:
		token, r, _ := s.logout(ctx)
		f = s.revocationFinding(ctx, token, r)
	}
	return insertAfterCategory(findings, f)
}

// revocationFinding judges AUTH-010 from the closing logout's answer: the
// session cookie it ended (token) is replayed against a protected route under
// ctx and must be refused with 401.
func (s *Scanner) revocationFinding(ctx context.Context, token string, logout httpResult) Finding {
	switch {
	case logout.err != nil:
		return finding("AUTH-010", SevInfo, StatusError,
			"the closing logout failed, so the session may stay open on the target until it expires: "+logout.err.Error())
	case logout.status != http.StatusOK:
		return finding("AUTH-010", SevInfo, StatusError,
			fmt.Sprintf("the closing logout answered HTTP %d, so whether a logout revokes the session could not be judged; the session may stay open until it expires.", logout.status))
	}
	replay := s.doContext(ctx, http.MethodGet, "/api/config", map[string]string{"Cookie": "kula_session=" + token}, "")
	switch {
	case replay.err != nil:
		return finding("AUTH-010", SevInfo, StatusError,
			"could not replay the logged-out session cookie: "+replay.err.Error())
	case replay.status == http.StatusUnauthorized:
		return finding("AUTH-010", SevHigh, StatusPass,
			"after the logout, the session cookie is refused with 401: the session was revoked on the server.")
	case replay.status >= 200 && replay.status <= 299:
		return finding("AUTH-010", SevHigh, StatusFail,
			"after the logout, the old session cookie still authenticates: logging out does not end the session on the server, so a stolen cookie outlives it.").
			withEvidence("GET /api/config with the logged-out cookie: HTTP %d", replay.status).
			withRemediation("Logout must revoke the session on the server, not only clear the browser's cookie.")
	default:
		return finding("AUTH-010", SevInfo, StatusError,
			fmt.Sprintf("after the logout, the session cookie was answered HTTP %d, neither 401 nor data.", replay.status))
	}
}

// insertAfterCategory returns findings with f placed after the last finding of
// its category, or at the end when there is none.
func insertAfterCategory(findings []Finding, f Finding) []Finding {
	at := len(findings)
	for i := len(findings) - 1; i >= 0; i-- {
		if findings[i].Category == f.Category {
			at = i + 1
			break
		}
	}
	return slices.Insert(findings, at, f)
}

// noSessionReason says why a check that needs a session has none, for its
// SKIP detail: a probe ended it, no credentials were supplied, or the
// supplied ones did not log in. Call it after ensureSession returned false.
func (s *Scanner) noSessionReason() string {
	switch {
	case s.sessionLost != "":
		return s.sessionLost
	case s.username == "" || s.password == "":
		return "supply -username/-password to open one"
	case s.loginDetail != "":
		return "the supplied credentials did not log in: " + s.loginDetail
	default:
		return "no session is open"
	}
}

// enumerationFinding probes the login timing/enumeration fix: a wrong password
// for a real username and for a random username must be indistinguishable. It
// runs only with -aggressive (AUTH-006 in authChecks).
func enumerationFinding(s *Scanner) Finding {
	if !s.authEnabled {
		// Without auth every login answers the same 404, which would read as
		// a pass.
		return finding("AUTH-006", SevInfo, StatusSkip,
			"auth is disabled; the instance has no login endpoint to compare responses from.")
	}
	if s.username == "" {
		return finding("AUTH-006", SevInfo, StatusSkip,
			"no -username supplied; cannot compare a known user against an unknown one.")
	}
	existing := s.loginRaw(s.username, "definitely-the-wrong-password-xyz")
	missing := s.loginRaw("no-such-user-"+randToken(6), "definitely-the-wrong-password-xyz")

	if existing.err != nil || missing.err != nil {
		return finding("AUTH-006", SevInfo, StatusError,
			"could not complete enumeration probe (network error).")
	}
	// A 429 means we tripped the rate limiter; the comparison is unreliable.
	if existing.status == http.StatusTooManyRequests || missing.status == http.StatusTooManyRequests {
		return finding("AUTH-006", SevInfo, StatusSkip,
			"login rate limiter engaged during the probe; re-run later for a clean comparison.")
	}
	if existing.status == missing.status && existing.body == missing.body {
		return finding("AUTH-006", SevMedium, StatusPass,
			"a wrong password yields an identical response for known and unknown usernames (no enumeration oracle).")
	}
	return finding("AUTH-006", SevMedium, StatusFail,
		"the login response differs between a known and an unknown username, leaking which accounts exist.").
		withEvidence("known: %d %q | unknown: %d %q", existing.status, truncate(existing.body, 80), missing.status, truncate(missing.body, 80)).
		withRemediation("Equalise the response (status + body + timing) for valid and invalid usernames.")
}

// loginRaw posts credentials with a same-origin header (so CSRF admits it) and
// returns the result. ensureSession opens the scan's session with it, and
// probes that must reach the login handler call it directly.
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
		ids: []string{"CSRF-001", "CSRF-002", "CSRF-003", "CSRF-004"},
		run: runCSRFChecks,
	}}
}

func runCSRFChecks(s *Scanner) []Finding {
	var out []Finding

	// CSRF-001: a state-changing POST with no Origin/Referer is blocked.
	noOrigin := s.do(http.MethodPost, "/api/login",
		map[string]string{"Content-Type": "application/json"},
		`{"username":"x","password":"y"}`)
	switch {
	case noOrigin.err != nil:
		out = append(out, finding("CSRF-001", SevInfo, StatusError,
			"could not send a POST without Origin or Referer: "+noOrigin.err.Error()))
	case noOrigin.status == http.StatusForbidden:
		out = append(out, finding("CSRF-001", SevMedium, StatusPass,
			"a POST with no Origin/Referer is rejected with 403 (origin validation active)."))
	default:
		out = append(out, finding("CSRF-001", SevMedium, StatusWarn,
			"a POST with no Origin/Referer was not rejected with 403; origin validation appears disabled.").
			withEvidence("status=%d", noOrigin.status).
			withRemediation("Enable web.security.origin_validation for defense-in-depth against CSRF.").
			expecting("origin-validation"))
	}

	out = append(out, foreignOriginFinding(s))

	// CSRF-003, CSRF-004 (need a session): authenticated, same-origin state
	// changes without the CSRF token and with a wrong one. Without auth Kula
	// checks no token.
	if !s.authEnabled {
		return out
	}
	s.ensureSession()
	out = append(out,
		s.csrfTokenFinding("CSRF-003", "", "without the X-CSRF-Token header"),
		s.csrfTokenFinding("CSRF-004", wrongCSRFToken(s.csrf), "with a wrong X-CSRF-Token"))
	return out
}

// foreignOriginFinding judges CSRF-002: a state change from a foreign site is
// refused, whether the browser names it in Origin or, without one, in
// Referer. The Referer probe goes to the logout with no session: it ends
// nothing, and with origin validation off it counts against no login limit.
func foreignOriginFinding(s *Scanner) Finding {
	probes := []struct {
		name, path, body string
		headers          map[string]string
	}{
		{"Origin: https://evil.example", "/api/login", `{"username":"x","password":"y"}`,
			map[string]string{"Content-Type": "application/json", "Origin": "https://evil.example"}},
		{"no Origin, Referer: https://evil.example/", "/api/logout", "",
			map[string]string{"Referer": "https://evil.example/page"}},
	}
	var admitted, unreached []string
	for _, p := range probes {
		r := s.do(http.MethodPost, p.path, p.headers, p.body)
		switch {
		case r.err != nil:
			unreached = append(unreached, p.name)
		case r.status != http.StatusForbidden:
			admitted = append(admitted, fmt.Sprintf("%s to %s (HTTP %d)", p.name, p.path, r.status))
		}
	}
	switch {
	case len(admitted) > 0:
		return finding("CSRF-002", SevHigh, StatusWarn,
			"a POST from a foreign site was not rejected with 403: "+strings.Join(admitted, ", ")+unreachedNote(unreached)).
			withRemediation("Enable web.security.origin_validation.").
			expecting("origin-validation")
	case len(unreached) == len(probes):
		return finding("CSRF-002", SevInfo, StatusError,
			"could not send a POST from a foreign Origin or Referer.")
	default:
		return finding("CSRF-002", SevHigh, StatusPass,
			"a POST from a foreign Origin, or with no Origin and a foreign Referer, is rejected with 403"+unreachedNote(unreached)+".")
	}
}

// csrfTokenFinding sends an authenticated, same-origin POST /api/logout with
// token as its X-CSRF-Token ("" sends none), which the synchronizer token
// check must refuse with 403. A target that accepts it has ended the scan's
// session: the session is marked lost, so the checks after this one skip
// instead of misreporting, and the finding says so.
func (s *Scanner) csrfTokenFinding(id, token, sent string) Finding {
	if !s.loggedIn {
		return finding(id, SevInfo, StatusSkip,
			"no authenticated session to test the CSRF token gate: "+s.noSessionReason()+".")
	}
	hdr := map[string]string{"Cookie": "kula_session=" + s.session, "Origin": s.base.Scheme + "://" + s.base.Host}
	if token != "" {
		hdr["X-CSRF-Token"] = token
	}
	r := s.do(http.MethodPost, "/api/logout", hdr, "")
	switch {
	case r.err != nil:
		return finding(id, SevInfo, StatusError,
			"could not send an authenticated POST "+sent+": "+r.err.Error())
	case r.status == http.StatusForbidden:
		return finding(id, SevHigh, StatusPass,
			"an authenticated, same-origin POST "+sent+" is rejected with 403.")
	case r.status >= 200 && r.status <= 299:
		s.markSessionLost(fmt.Sprintf("the target accepted %s's logout %s (HTTP %d), which ended the scan's session", id, sent, r.status))
		return finding(id, SevHigh, StatusFail,
			"an authenticated state change "+sent+" succeeded; the session is open to CSRF. The logout it sent ended the scan's session, so the checks after this one that need a session are skipped.").
			withEvidence("POST /api/logout %s: HTTP %d", sent, r.status).
			withRemediation("Require a matching X-CSRF-Token for state-changing requests on authenticated sessions.")
	default:
		return finding(id, SevMedium, StatusWarn,
			fmt.Sprintf("an authenticated POST %s answered HTTP %d: neither refused with 403 nor accepted.", sent, r.status))
	}
}

// wrongCSRFToken returns a token that differs from the session's real one
// only in its last character, so a check that compares no more than the
// length or a prefix still has to refuse it.
func wrongCSRFToken(token string) string {
	if token == "" {
		return "kula-scan-" + randToken(16)
	}
	last := byte('0')
	if token[len(token)-1] == last {
		last = '1'
	}
	return token[:len(token)-1] + string(last)
}

// ============================== cors =========================================

func corsChecks() []check {
	return []check{{
		ids: []string{"CORS-001", "CORS-002", "CORS-003"},
		run: runCORSChecks,
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
		out = append(out, finding("CORS-001", SevInfo, StatusError,
			"could not send CORS preflight: "+r.err.Error()))
		return append(out, corsEdgeCaseFinding(s))
	}

	acao := r.header.Get("Access-Control-Allow-Origin")
	acac := strings.EqualFold(r.header.Get("Access-Control-Allow-Credentials"), "true")

	switch {
	case acao == evil:
		out = append(out, finding("CORS-001", SevCritical, StatusFail,
			"the server reflected an attacker-controlled Origin into Access-Control-Allow-Origin, enabling cross-site reads of authenticated data.").
			withEvidence("ACAO: %s (credentials=%v)", acao, acac).
			withRemediation("Only echo origins present in web.security.allowed_origins; never reflect the request Origin blindly."))
	case acao == "*" && acac:
		out = append(out, finding("CORS-001", SevCritical, StatusFail,
			"Access-Control-Allow-Origin: * combined with Allow-Credentials: true exposes authenticated responses to any site.").
			withEvidence("ACAO: * ; ACAC: true").
			withRemediation("Never combine a wildcard ACAO with credentialed CORS."))
	case acao == "*" && !s.authEnabled:
		out = append(out, finding("CORS-001", SevHigh, StatusFail,
			"Access-Control-Allow-Origin: * lets any website read the API through a visitor's browser: with auth disabled the API needs no credentials, so a page on any site can read the metrics of an instance its visitor can reach.").
			withEvidence("ACAO: *").
			withRemediation("Kula never sends a wildcard; remove it from the proxy in front, and list the origins that may read the API in web.security.allowed_origins."))
	case acao == "*":
		out = append(out, finding("CORS-001", SevMedium, StatusWarn,
			"Access-Control-Allow-Origin: * is sent. A browser does not send the session cookie with a wildcard grant, so authenticated data stays out of reach, but any website can read what the API answers without a session.").
			withEvidence("ACAO: *").
			withRemediation("Kula never sends a wildcard; remove it from the proxy in front, and list the origins that may read the API in web.security.allowed_origins."))
	case acao == "":
		out = append(out, finding("CORS-001", SevMedium, StatusPass,
			"a preflight from an arbitrary foreign origin gets no Access-Control-Allow-Origin (cross-origin reads blocked)."))
	default:
		// Some other allow-listed origin was returned (not our evil one) — fine.
		out = append(out, finding("CORS-001", SevMedium, StatusPass,
			"the foreign origin was not reflected; ACAO is restricted to the configured allow-list.").
			withEvidence("ACAO: %s", acao))
	}

	// When CORS headers are emitted, Vary: Origin must be set so shared caches
	// don't serve a cross-origin response to the wrong site.
	if acao != "" {
		if containsFold(r.header.Values("Vary"), "Origin") {
			out = append(out, finding("CORS-002", SevLow, StatusPass,
				"Vary: Origin is present, preventing cross-origin cache poisoning."))
		} else {
			out = append(out, finding("CORS-002", SevMedium, StatusWarn,
				"CORS headers are emitted without Vary: Origin; a shared cache could leak them to another origin.").
				withRemediation("Add Vary: Origin whenever Access-Control-Allow-Origin is set."))
		}
	}

	out = append(out, corsEdgeCaseFinding(s))
	return out
}

// corsEdgeCaseFinding probes Origins that commonly slip past sloppy allow-list
// matching: the literal "null" origin, the target's host with a trailing dot,
// with embedded credentials, and inside look-alike names. None of them must be
// reflected back into Access-Control-Allow-Origin, even when the target lists
// its own origin. A case variant of an allowed origin is not probed: host
// names are case-insensitive, so it is the same origin, and Kula rightly
// matches it.
func corsEdgeCaseFinding(s *Scanner) Finding {
	scheme, hostname, port := s.base.Scheme, s.base.Hostname(), s.base.Port()
	origin := func(host string) string {
		if port != "" {
			host = net.JoinHostPort(host, port)
		}
		return scheme + "://" + host
	}
	edge := []string{
		"null",
		origin(hostname + "."),                 // trailing dot
		scheme + "://user:pass@" + s.base.Host, // userinfo
		origin(hostname + ".evil.example"),     // look-alike suffix
		origin("evil-" + hostname),             // look-alike prefix
	}
	var reflected, unreached []string
	for _, o := range edge {
		r := s.do(http.MethodGet, "/api/config", map[string]string{"Origin": o}, "")
		if r.err != nil {
			unreached = append(unreached, o)
			continue
		}
		acao := r.header.Get("Access-Control-Allow-Origin")
		if acao != "" && (acao == o || acao == "*") {
			reflected = append(reflected, fmt.Sprintf("%s → ACAO %s", o, acao))
		}
	}
	switch {
	case len(reflected) > 0:
		return finding("CORS-003", SevHigh, StatusFail,
			"an edge-case origin slipped past CORS matching and was reflected, enabling cross-site reads"+unreachedNote(unreached)+".").
			withEvidence("%s", strings.Join(reflected, " | ")).
			withRemediation("Match origins by exact, normalized string equality; reject 'null' and any host that isn't an exact allow-list entry.")
	case len(unreached) == len(edge):
		return finding("CORS-003", SevInfo, StatusError,
			"could not send any request with an edge-case Origin.")
	default:
		return finding("CORS-003", SevMedium, StatusPass,
			"null, trailing-dot, userinfo and look-alike origins are not reflected"+unreachedNote(unreached)+".")
	}
}

// ============================== traversal ====================================

func traversalChecks() []check {
	return []check{{
		ids: []string{"TRAV-000", "TRAV-001", "TRAV-002"},
		run: runTraversalChecks,
	}}
}

func runTraversalChecks(s *Scanner) []Finding {
	var out []Finding
	bp := s.base.Path // "" or "/something"

	// Positive control: a real static asset is served, so negatives are meaningful.
	code, _, err := s.rawTCP("GET " + bp + "/style.css HTTP/1.1")
	if err != nil {
		// A raw request reaches nothing, so no probe below can tell a thing.
		detail := "could not deliver a raw-socket request to the target: " + err.Error()
		return []Finding{
			finding("TRAV-000", SevInfo, StatusError, detail),
			finding("TRAV-001", SevInfo, StatusError, detail+"; no traversal payload was sent."),
			finding("TRAV-002", SevInfo, StatusError, detail+"; no directory listing was requested."),
		}
	}
	if code != http.StatusOK {
		out = append(out, finding("TRAV-000", SevInfo, StatusWarn,
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
	var lost undelivered
	for _, p := range payloads {
		code, body, err := s.rawTCP(p)
		if err != nil {
			lost.add(err)
			continue
		}
		if code != http.StatusOK {
			continue
		}
		for _, leak := range leaks {
			if strings.Contains(body, leak) {
				hits = append(hits, p+" → leaked "+leak)
			}
		}
	}
	switch {
	case len(hits) > 0:
		out = append(out, finding("TRAV-001", SevCritical, StatusFail,
			"a traversal payload leaked file content from outside the embedded static directory.").
			withEvidence("%s", strings.Join(hits, "; ")).
			withRemediation("Reject paths that escape the static root; never read user-controlled paths off disk."))
	case lost.n > 0:
		out = append(out, finding("TRAV-001", SevInfo, StatusError,
			lost.detail(len(payloads), "traversal payloads")+"; the others read no file outside the static dir, which says nothing of those."))
	default:
		out = append(out, finding("TRAV-001", SevHigh, StatusPass,
			"none of the encoded / dot-dot / backslash traversal payloads returned file content from outside the static dir."))
	}

	// Directory listing must not be exposed.
	dirs := []string{"/js/", "/fonts/"}
	var listed []string
	lost = undelivered{}
	for _, d := range dirs {
		code, body, err := s.rawTCP("GET " + bp + d + " HTTP/1.1")
		if err != nil {
			lost.add(err)
			continue
		}
		if code == http.StatusOK && (strings.Contains(body, "<a href") || strings.Contains(body, "Index of")) {
			listed = append(listed, d)
		}
	}
	switch {
	case len(listed) > 0:
		out = append(out, finding("TRAV-002", SevMedium, StatusFail,
			"a static directory returned a browsable listing: "+strings.Join(listed, ", ")).
			withRemediation("Return 403/404 for directory requests; never serve an autoindex."))
	case lost.n > 0:
		out = append(out, finding("TRAV-002", SevInfo, StatusError,
			lost.detail(len(dirs), "directory requests")+"."))
	default:
		out = append(out, finding("TRAV-002", SevMedium, StatusPass,
			"static directories do not return a browsable index."))
	}

	return out
}

// ============================== metrics ======================================

func metricsChecks() []check {
	return []check{{
		ids: []string{"PROM-001", "PROM-002", "PROM-003"},
		run: runMetricsChecks,
	}}
}

func runMetricsChecks(s *Scanner) []Finding {
	r := s.do(http.MethodGet, "/metrics", nil, "")
	if r.err != nil {
		return []Finding{finding("PROM-001", SevInfo, StatusError,
			"could not query /metrics: "+r.err.Error())}
	}

	switch r.status {
	case http.StatusNotFound:
		return []Finding{
			finding("PROM-001", SevInfo, StatusSkip, "/metrics is not enabled on this instance."),
			finding("PROM-003", SevInfo, StatusSkip, "/metrics is not enabled on this instance."),
		}
	case http.StatusUnauthorized:
		out := []Finding{}
		if strings.Contains(r.header.Get("WWW-Authenticate"), "Bearer") {
			out = append(out, finding("PROM-001", SevMedium, StatusPass,
				"/metrics requires a bearer token and advertises the Bearer challenge."))
		} else {
			out = append(out, finding("PROM-001", SevMedium, StatusPass,
				"/metrics requires authentication (401 without a token)."))
		}
		// A wrong token must also be rejected.
		w := s.do(http.MethodGet, "/metrics", map[string]string{"Authorization": "Bearer wrong-" + randToken(8)}, "")
		switch {
		case w.err != nil:
			out = append(out, finding("PROM-002", SevInfo, StatusError,
				"could not query /metrics with a wrong token: "+w.err.Error()))
		case w.status == http.StatusUnauthorized:
			out = append(out, finding("PROM-002", SevMedium, StatusPass,
				"an incorrect bearer token is rejected with 401."))
		default:
			out = append(out, finding("PROM-002", SevHigh, StatusFail,
				"an incorrect bearer token was not rejected").withEvidence("status=%d", w.status))
		}
		return append(out, metricsTokenFinding(s))
	case http.StatusOK:
		return []Finding{
			finding("PROM-001", SevMedium, StatusWarn,
				"/metrics is served with no authentication; it discloses the hostname and detailed system metrics to anyone who can reach the port.").
				withEvidence("%d bytes of metrics returned unauthenticated", len(r.body)).
				withRemediation("Set web.prometheus_metrics.token, or restrict /metrics at the network/proxy layer.").
				expecting("metrics-token"),
			finding("PROM-003", SevInfo, StatusSkip,
				"/metrics answers without a token, so no token is required."),
		}
	default:
		return []Finding{
			finding("PROM-001", SevInfo, StatusWarn,
				"unexpected status from /metrics").withEvidence("status=%d", r.status),
			finding("PROM-003", SevInfo, StatusSkip,
				fmt.Sprintf("/metrics answered HTTP %d without a token, not 401, so a token was not tried.", r.status)),
		}
	}
}

// metricsTokenFinding is PROM-003, the positive control of PROM-001 and
// PROM-002: /metrics, which refuses a request without a token, must answer
// the supplied -metrics-token with the metrics. A refused token leaves the
// run inconclusive, as refused credentials do. The token is never printed.
func metricsTokenFinding(s *Scanner) Finding {
	if s.metricsToken == "" {
		return finding("PROM-003", SevInfo, StatusSkip,
			"no -metrics-token (or "+metricsTokenEnv+") supplied; the configured token was not tried.")
	}
	r := s.do(http.MethodGet, "/metrics", map[string]string{"Authorization": "Bearer " + s.metricsToken}, "")
	switch {
	case r.err != nil:
		return finding("PROM-003", SevInfo, StatusError,
			"could not query /metrics with the supplied token: "+r.err.Error())
	case r.status == http.StatusUnauthorized:
		return finding("PROM-003", SevInfo, StatusError,
			"/metrics refused the supplied token (HTTP 401): it is not web.prometheus_metrics.token; check -metrics-token or "+metricsTokenEnv+".")
	case r.status == http.StatusOK && prometheusExposition(r):
		return finding("PROM-003", SevMedium, StatusPass,
			"/metrics answers the supplied token with 200 and a Prometheus exposition body.").
			withEvidence("%d bytes of %s", len(r.body), r.header.Get("Content-Type"))
	case r.status == http.StatusOK:
		return finding("PROM-003", SevLow, StatusWarn,
			"/metrics answered the supplied token with 200, but not with a Prometheus exposition body; something other than Kula may have answered.").
			withEvidence("%d bytes of %q", len(r.body), r.header.Get("Content-Type"))
	default:
		return finding("PROM-003", SevLow, StatusWarn,
			fmt.Sprintf("/metrics answered the supplied token with HTTP %d, not 200.", r.status))
	}
}

// promSample matches a sample line of the Prometheus text format:
// name{labels} value.
var promSample = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*(\{.*\})?[ \t]+\S`)

// prometheusExposition reports whether r carries a Prometheus text
// exposition: a text/plain (or OpenMetrics) body whose first line that is
// not blank or a comment is a sample. Kula answers an empty one, typed
// version=0.0.4, before it has taken a sample.
func prometheusExposition(r httpResult) bool {
	contentType := strings.ToLower(r.header.Get("Content-Type"))
	if !strings.HasPrefix(contentType, "text/plain") && !strings.HasPrefix(contentType, "application/openmetrics-text") {
		return false
	}
	for line := range strings.Lines(r.body) {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			return promSample.MatchString(line)
		}
	}
	return strings.Contains(contentType, "version=")
}

// ============================== input ========================================

func inputChecks() []check {
	return []check{{
		ids: []string{"INPUT-001", "INPUT-002", "INPUT-003"},
		run: runInputChecks,
	}}
}

func runInputChecks(s *Scanner) []Finding {
	hdr, ok := s.apiHeaders()
	if !ok {
		// /api/history and /api/i18n sit behind the auth gate.
		why := "auth is enabled and no session is available to reach the API: " + s.noSessionReason() + "."
		return []Finding{
			finding("INPUT-001", SevInfo, StatusSkip, why),
			finding("INPUT-002", SevInfo, StatusSkip, why),
			finding("INPUT-003", SevInfo, StatusSkip, why),
		}
	}
	return []Finding{historyValidationFinding(s, hdr), pointsCapFinding(s, hdr), languageFinding(s, hdr)}
}

// historyValidationFinding judges INPUT-001: /api/history answers 400 to a
// bad, inverted or over-long time range, a negative or non-numeric tier and
// an unknown section.
func historyValidationFinding(s *Scanner, hdr map[string]string) Finding {
	cases := []struct{ name, query string }{
		{"malformed 'from'", "/api/history?from=not-a-time"},
		{"malformed 'to'", "/api/history?to=garbage"},
		{"inverted range", "/api/history?from=2026-01-02T00:00:00Z&to=2026-01-01T00:00:00Z"},
		{"range > 31 days", "/api/history?from=2020-01-01T00:00:00Z&to=2026-01-01T00:00:00Z"},
		{"negative tier", "/api/history?tier=-1"},
		{"non-numeric tier", "/api/history?tier=first"},
		{"unknown section", "/api/history?sections=kula-scan-nosuch"},
	}
	var bad, unreached []string
	for _, c := range cases {
		r := s.do(http.MethodGet, c.query, hdr, "")
		switch {
		case r.err != nil:
			unreached = append(unreached, c.name)
		case r.status != http.StatusBadRequest:
			bad = append(bad, fmt.Sprintf("%s (HTTP %d)", c.name, r.status))
		}
	}
	switch {
	case len(bad) > 0:
		return finding("INPUT-001", SevMedium, StatusWarn,
			"these /api/history inputs were not rejected with 400: "+strings.Join(bad, ", ")+unreachedNote(unreached)).
			withRemediation("Validate and bound the from/to window, the tier and the sections server-side; older Kula releases do not validate tier or sections.")
	case len(unreached) == len(cases):
		return finding("INPUT-001", SevInfo, StatusError,
			"could not send any request to /api/history.")
	default:
		return finding("INPUT-001", SevLow, StatusPass,
			"malformed, inverted and over-long time ranges, a negative or non-numeric tier and an unknown section are all rejected with 400"+unreachedNote(unreached)+".")
	}
}

// maxHistoryPoints is the most samples Kula answers a history request with,
// whatever points value it asks for.
const maxHistoryPoints = 5000

// pointsCapFinding judges INPUT-002 by counting what an absurd points value
// gets: the samples of the last two hours, which at one-second resolution are
// more than the cap, of one section to keep them small.
func pointsCapFinding(s *Scanner, hdr map[string]string) Finding {
	to := time.Now().UTC().Truncate(time.Second)
	from := to.Add(-2 * time.Hour)
	path := fmt.Sprintf("/api/history?from=%s&to=%s&points=999999999&sections=lavg", from.Format(time.RFC3339), to.Format(time.RFC3339))
	p := s.countHistory(path, hdr)
	switch {
	case p.err != nil:
		return finding("INPUT-002", SevInfo, StatusError,
			"could not request /api/history with a huge points value: "+p.err.Error())
	case p.status != http.StatusOK:
		return finding("INPUT-002", SevInfo, StatusWarn,
			"a huge points value returned a non-200 status").withEvidence("status=%d", p.status)
	case p.readErr != nil:
		return finding("INPUT-002", SevInfo, StatusError,
			"the answer to a huge points value could not be read as a history response: "+p.readErr.Error())
	case p.count.more:
		return finding("INPUT-002", SevMedium, StatusFail,
			fmt.Sprintf("a huge points value was answered with more than %d samples: the points value is not capped, so one request can make the server build an unbounded response.", maxHistoryPoints)).
			withEvidence("GET %s: over %d samples", path, maxHistoryPoints).
			withRemediation("Cap the points a history request may ask for; Kula caps it at 5000.")
	default:
		detail := fmt.Sprintf("points=999999999 over the last two hours was answered with %d samples, no more than Kula's cap of %d", p.count.samples, maxHistoryPoints)
		if p.count.downsampled && p.count.resolution != "" {
			detail += ", downsampled to " + p.count.resolution + " buckets"
		}
		return finding("INPUT-002", SevLow, StatusPass, detail+".").
			withEvidence("samples=%d resolution=%q downsampled=%v", p.count.samples, p.count.resolution, p.count.downsampled)
	}
}

// languageFinding judges INPUT-003: invalid and traversal language codes read
// no file.
func languageFinding(s *Scanner, hdr map[string]string) Finding {
	langs := []string{"../../etc/passwd", "..%2f..%2fconfig.yaml", "zz", "<script>", "%00en"}
	var leaked []string
	failed := 0
	for _, lang := range langs {
		r := s.do(http.MethodGet, "/api/i18n?lang="+lang, hdr, "")
		switch {
		case r.err != nil:
			failed++
		case r.status == http.StatusOK && (strings.Contains(r.body, "root:x:") || strings.Contains(r.body, "password_hash") || strings.Contains(r.body, "module kula")):
			leaked = append(leaked, lang)
		}
	}
	switch {
	case len(leaked) > 0:
		return finding("INPUT-003", SevCritical, StatusFail,
			"a crafted lang parameter leaked file content: "+strings.Join(leaked, ", ")).
			withRemediation("Validate lang against the fixed supported-language allow-list.")
	case failed == len(langs):
		return finding("INPUT-003", SevInfo, StatusError,
			"could not send any request to /api/i18n.")
	default:
		detail := "invalid / traversal language codes are rejected and never read arbitrary files."
		if failed > 0 {
			detail = fmt.Sprintf("the invalid / traversal language codes that reached the target read no file; %d of %d requests failed.", failed, len(langs))
		}
		return finding("INPUT-003", SevMedium, StatusPass, detail)
	}
}

// maxHistoryBody bounds what countHistory reads; it stops counting past the
// cap well before.
const maxHistoryBody = 64 << 20

// historyCount is what INPUT-002 reads of a history response.
type historyCount struct {
	samples     int  // counted up to maxHistoryPoints
	more        bool // and there were more
	resolution  string
	downsampled bool
}

// historyProbe is how a countHistory request ended: err when the request
// failed, readErr when a 200's body was not a history response.
type historyProbe struct {
	status  int
	count   historyCount
	err     error
	readErr error
}

// countHistory requests a history path and counts the samples of a 200
// answer as they stream in, up to one past maxHistoryPoints: an answer at
// the cap is larger than do reads.
func (s *Scanner) countHistory(path string, headers map[string]string) historyProbe {
	ctx, cancel := context.WithTimeout(s.ctx, s.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.urlFor(path), nil)
	if err != nil {
		return historyProbe{err: err}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		if s.verbose {
			_, _ = fmt.Fprintf(s.log, "    GET %s -> %s\n", printable(path), printable(err.Error()))
		}
		return historyProbe{err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	if s.verbose {
		_, _ = fmt.Fprintf(s.log, "    GET %s -> %d (streamed)\n", printable(path), resp.StatusCode)
	}
	p := historyProbe{status: resp.StatusCode}
	if resp.StatusCode == http.StatusOK {
		p.count, p.readErr = readHistoryCount(io.LimitReader(resp.Body, maxHistoryBody))
	}
	return p
}

// readHistoryCount counts the samples of a history response, stopping once
// there are more than maxHistoryPoints, and reads its resolution and
// downsampled fields when it gets that far.
func readHistoryCount(r io.Reader) (historyCount, error) {
	var c historyCount
	dec := json.NewDecoder(r)
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return c, errors.New("the body is not a JSON object")
	}
	sawSamples := false
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return c, err
		}
		var dst any
		switch tok {
		case "samples":
			sawSamples = true
			if tok, err = dec.Token(); err != nil {
				return c, err
			}
			if tok == nil {
				continue // null: no samples
			}
			if tok != json.Delim('[') {
				return c, errors.New("samples is not a list")
			}
			for dec.More() {
				if c.samples == maxHistoryPoints {
					c.more = true
					return c, nil
				}
				var sample json.RawMessage
				if err := dec.Decode(&sample); err != nil {
					return c, err
				}
				c.samples++
			}
			if _, err := dec.Token(); err != nil {
				return c, err
			}
			continue
		case "resolution":
			dst = &c.resolution
		case "downsampled":
			dst = &c.downsampled
		default:
			dst = new(json.RawMessage)
		}
		if err := dec.Decode(dst); err != nil {
			return c, err
		}
	}
	if !sawSamples {
		return c, errors.New("the body has no samples list")
	}
	return c, nil
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
