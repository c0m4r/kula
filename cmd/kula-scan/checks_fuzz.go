package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// Blind fault-injection probes. Unlike the safeguard checks (which assert a
// known defense), these throw malformed and extreme inputs at the surface and
// watch for things that should NEVER happen — the anomaly oracle:
//
//   - HTTP 5xx        → a handler errored where it should have returned 4xx
//   - reset / EOF     → net/http recovered a handler PANIC and dropped the conn
//   - hang / timeout  → unbounded work or a deadlock
//   - reflected input → attacker text echoed unescaped into an HTML/JS response
//   - server death    → the final liveness probe fails after the barrage
//
// All of these are gated behind -fuzz. Every probe draws from a seeded PRNG and
// reports the seed, so any anomaly is reproducible with -seed.

func fuzzChecks() []check {
	return []check{
		{id: "FUZZ-QUERY", category: "fuzz", fuzz: true, run: runFuzzQuery},
		{id: "FUZZ-PATH", category: "fuzz", fuzz: true, run: runFuzzPath},
		{id: "FUZZ-BODY", category: "fuzz", fuzz: true, run: runFuzzBody},
		{id: "FUZZ-METHODS", category: "fuzz", fuzz: true, run: runFuzzMethods},
		{id: "FUZZ-SMUGGLE", category: "fuzz", fuzz: true, run: runFuzzSmuggle},
		{id: "FUZZ-WS", category: "fuzz", fuzz: true, run: runFuzzWS},
		{id: "FUZZ-LIVENESS", category: "fuzz", fuzz: true, run: runFuzzLiveness},
	}
}

// fuzzTokens is the corpus of hostile values mixed into fuzzed inputs.
var fuzzTokens = []string{
	"", " ", "\x00", "\x00\x01\x02\x03", "\n\r", "\t",
	strings.Repeat("A", 1<<16), strings.Repeat("../", 64), strings.Repeat("9", 4096),
	"%00", "%0a%0d", "%2e%2e%2f%2e%2e%2f", "..%2f..%2f", "/etc/passwd",
	"<script>alert(1)</script>", "\"'><svg/onload=alert(1)>", "javascript:alert(1)",
	"${jndi:ldap://x}", "{{7*7}}", "${{7*7}}", "%n%n%n%s%s%s", "`id`", "$(id)", "|id", ";id",
	"-1", "0", "99999999999999999999999999", "1e309", "-1e309", "NaN", "Infinity", "0x41414141",
	"2147483648", "-2147483648", "9223372036854775808", "4294967296",
	"true", "false", "null", "undefined", "[]", "{}", "[][]",
	"", "", "\U0001F600\U0001F525", "ÿÿ", "%c0%ae%c0%ae", "....//....//",
	"SELECT", "' OR '1'='1", "1;DROP TABLE", "../../../../../../proc/self/environ",
}

// fuzzToken returns a random hostile token, occasionally suffixed with a random
// number so repeated draws still vary.
func (s *Scanner) fuzzToken() string {
	t := fuzzTokens[s.randIntn(len(fuzzTokens))]
	if s.randIntn(4) == 0 {
		t += fmt.Sprintf("%d", s.randIntn(1<<30))
	}
	return t
}

// fuzzHit records one anomalous response and the request that triggered it.
type fuzzHit struct {
	kind string
	req  string
}

// classifyResult applies the anomaly oracle to one response. It returns
// (true, kind) when the response is a sign of an unexpected failure. Expected
// rejections (4xx, including 400/401/403/404/405/413/429) are not anomalies.
func classifyResult(r httpResult) (bool, string) {
	if r.err != nil {
		e := strings.ToLower(r.err.Error())
		switch {
		case strings.Contains(e, "timeout") || strings.Contains(e, "deadline exceeded"):
			return true, "hang/timeout"
		case strings.Contains(e, "eof") || strings.Contains(e, "connection reset") ||
			strings.Contains(e, "broken pipe") || strings.Contains(e, "connection refused"):
			return true, "connection dropped (possible panic/crash)"
		default:
			return false, "" // generic transport noise — not a clear server bug
		}
	}
	if r.status >= 500 {
		return true, fmt.Sprintf("HTTP %d", r.status)
	}
	return false, ""
}

// reqDesc renders a compact, reproducible request description.
func reqDesc(method, path, body string) string {
	d := method + " " + path
	if body != "" {
		d += " body=" + truncate(body, 100)
	}
	return d
}

// reflectedXSS reports whether canary appears verbatim in an HTML/JS response —
// i.e. attacker-controlled input echoed without escaping.
func reflectedXSS(r httpResult, canary string) bool {
	if canary == "" || r.body == "" {
		return false
	}
	ct := strings.ToLower(r.header.Get("Content-Type"))
	if !strings.Contains(ct, "html") && !strings.Contains(ct, "javascript") {
		return false
	}
	return strings.Contains(r.body, canary)
}

// runFuzzLoop drives gen for fuzzIter iterations, collecting up to maxHits
// anomalies, and renders a single aggregated finding.
func (s *Scanner) runFuzzLoop(id, title string, headers map[string]string, gen func() (method, path, body, canary string)) Finding {
	const maxHits = 8
	var hits []fuzzHit
	for i := 0; i < s.fuzzIter && len(hits) < maxHits; i++ {
		m, p, b, canary := gen()
		r := s.do(m, p, headers, b)
		if bad, kind := classifyResult(r); bad {
			hits = append(hits, fuzzHit{kind, reqDesc(m, p, b)})
			continue
		}
		if reflectedXSS(r, canary) {
			hits = append(hits, fuzzHit{"reflected input (possible XSS)", reqDesc(m, p, b)})
		}
	}
	return fuzzFinding(id, title, s.fuzzIter, s.seed, hits)
}

// fuzzFinding turns a hit list into a PASS (robust) or FAIL (anomalies) finding.
func fuzzFinding(id, title string, iter int, seed int64, hits []fuzzHit) Finding {
	if len(hits) == 0 {
		return finding(id, "fuzz", title, SevInfo, StatusPass,
			fmt.Sprintf("no anomalies over %d randomized requests.", iter)).
			withEvidence("seed=%d", seed)
	}
	ev := make([]string, 0, len(hits))
	for _, h := range hits {
		ev = append(ev, h.kind+" ← "+h.req)
	}
	return finding(id, "fuzz", title, SevHigh, StatusFail,
		fmt.Sprintf("found %d anomal(y/ies) the server should never produce on hostile input.", len(hits))).
		withEvidence("seed=%d | %s", seed, strings.Join(ev, " | ")).
		withRemediation("Return 4xx for malformed input; never 5xx, panic, or hang. Reproduce with -seed and the listed requests.")
}

// runFuzzQuery fuzzes the query parameters of the read API endpoints.
func runFuzzQuery(s *Scanner) []Finding {
	hdr, ok := s.apiHeaders()
	if !ok {
		return []Finding{finding("FUZZ-QUERY", "fuzz", "Query-parameter fuzzing", SevInfo, StatusSkip,
			"auth is enabled and no session is available; supply -username/-password to fuzz the API.")}
	}
	gen := func() (string, string, string, string) {
		// Alternate between the two endpoints that take parameters.
		if s.randIntn(2) == 0 {
			q := url.Values{}
			for _, k := range []string{"from", "to", "points"} {
				if s.randIntn(3) != 0 {
					q.Set(k, s.fuzzToken())
				}
			}
			return http.MethodGet, "/api/history?" + q.Encode(), "", ""
		}
		q := url.Values{}
		q.Set("lang", s.fuzzToken())
		return http.MethodGet, "/api/i18n?" + q.Encode(), "", ""
	}
	return []Finding{s.runFuzzLoop("FUZZ-QUERY", "Query-parameter fuzzing (/api/history, /api/i18n)", hdr, gen)}
}

// runFuzzPath fuzzes the request path with a canary so any reflection into the
// HTML shell is caught.
func runFuzzPath(s *Scanner) []Finding {
	gen := func() (string, string, string, string) {
		// The canary carries angle brackets so a reflection only counts when the
		// server echoes them UNescaped — an actual XSS sink, not a redirect body
		// or JSON string that safely escapes "<".
		canary := "<kx" + randToken(6) + ">"
		tok := s.fuzzToken()
		p := "/" + url.PathEscape(tok+canary)
		if s.randIntn(2) == 0 {
			p = "/" + tok + canary // unescaped variant
		}
		return http.MethodGet, p, "", canary
	}
	return []Finding{s.runFuzzLoop("FUZZ-PATH", "Path fuzzing", nil, gen)}
}

// runFuzzBody sends a curated set of malformed JSON bodies to the JSON POST
// endpoints. Login is per-IP rate-limited, so coverage there is best-effort;
// the point is that a malformed body must yield 400 — never a 5xx or a panic.
func runFuzzBody(s *Scanner) []Finding {
	bodies := []string{
		`{`, `{"username":}`, `{"username":123,"password":[]}`, `[1,2,3]`, `"x"`, `null`, `12345`,
		"\x00\x01garbage", `{"username":"` + strings.Repeat("A", 6000) + `"}`,
		`{"a":` + deepNest(2000) + `}`, strings.Repeat("[", 3000) + strings.Repeat("]", 3000),
		`{"username":"x"` + strings.Repeat(`,"x":"y"`, 5000) + `}`,
	}
	origin := s.base.Scheme + "://" + s.base.Host

	var hits []fuzzHit
	post := func(path string) {
		for _, b := range bodies {
			r := s.do(http.MethodPost, path, map[string]string{"Content-Type": "application/json", "Origin": origin}, b)
			// 429 (rate-limited) and 4xx are fine; only 5xx/reset/hang are bugs.
			if bad, kind := classifyResult(r); bad {
				hits = append(hits, fuzzHit{kind, reqDesc(http.MethodPost, path, b)})
			}
			if r.status == http.StatusTooManyRequests {
				break // further attempts won't reach the body parser
			}
		}
	}
	post("/api/login")
	if s.ollama {
		post("/api/ollama/chat")
	}
	return []Finding{fuzzFinding("FUZZ-BODY", "Malformed JSON body handling", len(bodies), s.seed, hits)}
}

// runFuzzMethods sends a matrix of HTTP methods at every known route. A 5xx or a
// dropped connection is a bug; a state-changing verb accepted on a POST-only
// endpoint is a method-enforcement gap.
func runFuzzMethods(s *Scanner) []Finding {
	routes := []string{"/", "/api/current", "/api/config", "/api/history", "/api/i18n?lang=en",
		"/api/login", "/api/logout", "/health", "/metrics", "/style.css", "/ws"}
	methods := []string{"PUT", "DELETE", "PATCH", "TRACE", "CONNECT", "PROPFIND", "MKCOL", "BREW", "FOObar"}
	postOnly := map[string]bool{"/api/login": true, "/api/logout": true, "/api/ollama/chat": true}

	hdr, _ := s.apiHeaders()
	var hits []fuzzHit
	for _, route := range routes {
		for _, m := range methods {
			r := s.do(m, route, hdr, "")
			if bad, kind := classifyResult(r); bad {
				hits = append(hits, fuzzHit{kind, reqDesc(m, route, "")})
				continue
			}
			if postOnly[route] && r.status >= 200 && r.status < 300 {
				hits = append(hits, fuzzHit{"state-changing method accepted", reqDesc(m, route, "")})
			}
		}
	}
	return []Finding{fuzzFinding("FUZZ-METHODS", "HTTP method matrix", len(routes)*len(methods), s.seed, hits)}
}

// runFuzzSmuggle sends requests with conflicting/duplicate framing headers. The
// server must reject them (400) or close — never 5xx, and never two responses.
func runFuzzSmuggle(s *Scanner) []Finding {
	host := s.base.Host
	bp := s.base.Path
	raws := []string{
		// Conflicting Content-Length values.
		fmt.Sprintf("POST %s/api/login HTTP/1.1\r\nHost: %s\r\nContent-Length: 6\r\nContent-Length: 5\r\nConnection: close\r\n\r\nhello\r\n", bp, host),
		// Content-Length + Transfer-Encoding (CL.TE).
		fmt.Sprintf("POST %s/api/login HTTP/1.1\r\nHost: %s\r\nContent-Length: 4\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n0\r\n\r\n", bp, host),
		// Obfuscated Transfer-Encoding.
		fmt.Sprintf("POST %s/api/login HTTP/1.1\r\nHost: %s\r\nTransfer-Encoding: chunked\r\nTransfer-Encoding: identity\r\nConnection: close\r\n\r\n0\r\n\r\n", bp, host),
	}
	var hits []fuzzHit
	for _, raw := range raws {
		status, _ := s.rawExchange(raw)
		switch {
		case status == 500:
			// A 400/501 rejection is the correct, safe response; only a 500 means
			// the ambiguous framing actually broke a handler.
			hits = append(hits, fuzzHit{"HTTP 500", truncate(strings.ReplaceAll(raw, "\r\n", "\\n"), 90)})
		case status >= 200 && status < 300:
			hits = append(hits, fuzzHit{"accepted ambiguous framing (smuggling risk)", truncate(strings.ReplaceAll(raw, "\r\n", "\\n"), 90)})
		}
	}
	if len(hits) == 0 {
		return []Finding{finding("FUZZ-SMUGGLE", "fuzz", "Request smuggling / desync", SevMedium, StatusPass,
			"ambiguous Content-Length / Transfer-Encoding framing is rejected (no desync, no 5xx).")}
	}
	f := fuzzFinding("FUZZ-SMUGGLE", "Request smuggling / desync", len(raws), s.seed, hits)
	return []Finding{f}
}

// runFuzzWS opens an authenticated, same-origin WebSocket and floods it with
// malformed and binary frames plus rapid pause/resume churn, then confirms the
// server is still serving new WebSocket connections and HTTP requests.
func runFuzzWS(s *Scanner) []Finding {
	var hdr http.Header
	if s.authEnabled {
		if !s.ensureSession() {
			return []Finding{finding("FUZZ-WS", "fuzz", "WebSocket protocol fuzzing", SevInfo, StatusSkip,
				"auth is enabled and no session is available to open a WebSocket.")}
		}
		hdr = http.Header{}
		hdr.Set("Cookie", "kula_session="+s.session)
	} else {
		hdr = http.Header{}
	}
	hdr.Set("Origin", s.base.Scheme+"://"+s.base.Host)

	// Several short-lived connections, each blasted with junk; a connection that
	// the server drops on bad input is fine — we only care that it never crashes.
	junk := []func(*websocket.Conn) error{
		func(c *websocket.Conn) error { return c.WriteMessage(websocket.TextMessage, []byte("not json at all")) },
		func(c *websocket.Conn) error { return c.WriteMessage(websocket.TextMessage, []byte(`{"action":`)) },
		func(c *websocket.Conn) error {
			return c.WriteMessage(websocket.TextMessage, []byte(`{"action":"`+strings.Repeat("X", 1000)+`"}`))
		},
		func(c *websocket.Conn) error {
			return c.WriteMessage(websocket.BinaryMessage, []byte{0x00, 0xff, 0x10, 0x42})
		},
		func(c *websocket.Conn) error {
			return c.WriteMessage(websocket.TextMessage, []byte(`{"action":"pause"}`))
		},
		func(c *websocket.Conn) error {
			return c.WriteMessage(websocket.TextMessage, []byte(`{"action":"resume"}`))
		},
	}
	for i := 0; i < 6; i++ {
		conn, _, err := s.wsDial("/ws", hdr)
		if err != nil || conn == nil {
			continue
		}
		for r := 0; r < 8; r++ {
			_ = conn.SetWriteDeadline(time.Now().Add(s.timeout))
			if err := junk[s.randIntn(len(junk))](conn); err != nil {
				break // server closed the connection on bad input — acceptable
			}
		}
		_ = conn.Close()
	}

	// Survival check: a fresh WebSocket must still upgrade, and HTTP must answer.
	live := s.do(http.MethodGet, "/health", nil, "")
	healthOK := live.err == nil && (live.status == http.StatusOK || live.status == http.StatusNotFound)
	conn, _, err := s.wsDial("/ws", hdr)
	if conn != nil {
		_ = conn.Close()
	}
	wsOK := err == nil

	switch {
	case !healthOK:
		return []Finding{finding("FUZZ-WS", "fuzz", "WebSocket protocol fuzzing", SevCritical, StatusFail,
			"after a burst of malformed WebSocket frames, HTTP /health stopped responding — the server was destabilized.").
			withEvidence("health err=%v status=%d", live.err, live.status).
			withRemediation("Ensure the WebSocket read loop tolerates any client input without panicking or blocking the server.")}
	case !wsOK:
		return []Finding{finding("FUZZ-WS", "fuzz", "WebSocket protocol fuzzing", SevHigh, StatusWarn,
			"HTTP still works, but a fresh WebSocket upgrade failed after the fuzz burst (possible connection-slot leak).").
			withEvidence("ws dial err=%v", err).
			withRemediation("Verify failed/garbage connections are always unregistered and their slots released.")}
	default:
		return []Finding{finding("FUZZ-WS", "fuzz", "WebSocket protocol fuzzing", SevInfo, StatusPass,
			"the server tolerated malformed/binary frames and rapid churn; HTTP and new WebSocket upgrades still work.")}
	}
}

// runFuzzLiveness is the final probe: after the whole fuzz barrage, the instance
// must still be alive and serving. It runs last in the registry.
func runFuzzLiveness(s *Scanner) []Finding {
	var lastErr error
	for i := 0; i < 3; i++ {
		r := s.do(http.MethodGet, "/health", nil, "")
		if r.err == nil && (r.status == http.StatusOK || r.status == http.StatusNotFound) {
			return []Finding{finding("FUZZ-LIVENESS", "fuzz", "Server survived the fuzz run", SevCritical, StatusPass,
				"the instance is still reachable and serving after the full fault-injection barrage.")}
		}
		lastErr = r.err
		time.Sleep(500 * time.Millisecond)
	}
	return []Finding{finding("FUZZ-LIVENESS", "fuzz", "Server survived the fuzz run", SevCritical, StatusFail,
		"the instance stopped responding to /health after fuzzing — fault injection may have crashed or hung it.").
		withEvidence("last err=%v", lastErr).
		withRemediation("Investigate the preceding fuzz findings; a monitoring daemon must never be crashable by remote input.")}
}

// deepNest builds a string of n nested single-key JSON objects, used to probe
// recursive-descent parser limits (stack exhaustion).
func deepNest(n int) string {
	return strings.Repeat(`{"a":`, n) + "1" + strings.Repeat("}", n)
}
