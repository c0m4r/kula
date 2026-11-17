package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// Blind fault-injection probes. Unlike the safeguard checks (which assert a
// known defense), these throw malformed and extreme inputs at the surface and
// watch for things that should NEVER happen — the anomaly oracle (classify):
//
//   - HTTP 5xx        → a handler errored where it should have returned 4xx,
//     except the 5xx answers Kula gives on purpose (deliberate5xx)
//   - reset / EOF     → net/http recovered a handler PANIC and dropped the conn
//   - hang / timeout  → unbounded work or a deadlock
//   - reflected input → attacker text echoed unescaped into an HTML/JS response
//   - server death    → the final liveness probe fails after the barrage
//
// Each probe also counts what became of its requests (fuzzTally): how many
// it planned, sent, and saw reach the code under test, and how many a
// limiter throttled or the client could not send. A finding states those
// numbers and is judged by them: a probe whose requests never reached the
// code it tests does not PASS.
//
// All of these are gated behind -fuzz. Every probe draws from a seeded PRNG and
// reports the seed, so any anomaly is reproducible with -seed.

func fuzzChecks() []check {
	return []check{
		{ids: []string{"FUZZ-QUERY"}, run: runFuzzQuery},
		{ids: []string{"FUZZ-PATH"}, run: runFuzzPath},
		{ids: []string{"FUZZ-BODY"}, run: runFuzzBody},
		{ids: []string{"FUZZ-METHODS"}, run: runFuzzMethods},
		{ids: []string{"FUZZ-SMUGGLE"}, run: runFuzzSmuggle},
		{ids: []string{"FUZZ-WS"}, run: runFuzzWS},
		{ids: []string{"FUZZ-LIVENESS"}, run: runFuzzLiveness},
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

// ---- the oracle -------------------------------------------------------------

// outcome is how one fuzz request ended, as the oracle judges it.
type outcome int

const (
	// outReached: the request got past the gates to the code under test (a
	// handler, or net/http's own request parser) and was answered.
	outReached outcome = iota
	// outGated: Kula's auth, CSRF or Host gate answered it before the code
	// under test.
	outGated
	// outThrottled: a limiter refused it (429), or Kula answered that it is
	// busy (a deliberate 503 with Retry-After).
	outThrottled
	// outUnavailable: Kula answered on purpose that the feature is off or
	// not ready (a deliberate 503 or 502).
	outUnavailable
	// outUnsent: the client could not build the request or found no
	// connection to send it on; the target saw none of it.
	outUnsent
	// outFailed: it was sent, but ended without an answer and without a
	// sign of a server fault.
	outFailed
	// outAnomaly: a sign of a server fault: an unexpected 5xx, a dropped
	// connection, a hang.
	outAnomaly
)

// The transport anomalies.
const (
	kindHang    = "hang/timeout (no answer within -timeout)"
	kindDropped = "connection dropped (possible panic/crash)"
)

// judgement is the oracle's verdict on one request.
type judgement struct {
	outcome outcome
	why     string // outAnomaly: what happened; outThrottled, outUnavailable: who said so
	parser  bool   // outReached: net/http's parser refused it before routing
	refused bool   // outUnsent: the target refused the connection
}

// classify applies the anomaly oracle to one request's result. Expected
// rejections (4xx) are answers like any other; a 5xx is an anomaly unless it
// is one Kula gives on purpose (deliberate5xx) or net/http's own refusal of a
// request it cannot parse (parserRefusal).
func classify(r httpResult) judgement {
	if r.err != nil {
		return classifyError(r)
	}
	switch {
	case r.status == http.StatusTooManyRequests:
		return judgement{outcome: outThrottled, why: throttledBy(r)}
	case parserRefusal(r):
		return judgement{outcome: outReached, parser: true}
	}
	if d, ok := deliberateAnswerFor(r); ok {
		if d.busy {
			return judgement{outcome: outThrottled, why: d.why + retryAfter(r)}
		}
		return judgement{outcome: outUnavailable, why: d.why}
	}
	switch {
	case r.status >= 500:
		return judgement{outcome: outAnomaly, why: fmt.Sprintf("HTTP %d", r.status)}
	case gateRefusal(r):
		return judgement{outcome: outGated}
	}
	return judgement{outcome: outReached}
}

// classifyError judges a request that ended in an error, by the error's type
// where the standard library keeps one and by its words otherwise. A
// request that never left the client is not sent: never an anomaly, never a
// pass. A refused connection is not sent either, but tells the caller the
// target refused it (see fuzzTally.record).
func classifyError(r httpResult) judgement {
	err := r.err
	var netErr net.Error
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return judgement{outcome: outUnsent, refused: true}
	case r.unsent, errors.Is(err, context.Canceled):
		return judgement{outcome: outUnsent}
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return judgement{outcome: outAnomaly, why: kindHang}
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF),
		errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return judgement{outcome: outAnomaly, why: kindDropped}
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "connection refused"):
		return judgement{outcome: outUnsent, refused: true}
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline exceeded"):
		return judgement{outcome: outAnomaly, why: kindHang}
	case strings.Contains(msg, "eof"), strings.Contains(msg, "connection reset"), strings.Contains(msg, "broken pipe"):
		return judgement{outcome: outAnomaly, why: kindDropped}
	}
	return judgement{outcome: outFailed}
}

// classifyResult reports whether r is an anomaly, and which.
func classifyResult(r httpResult) (bool, string) {
	j := classify(r)
	return j.outcome == outAnomaly, j.why
}

// deliberateAnswer is a 5xx Kula gives on purpose.
type deliberateAnswer struct {
	status     int
	route      string // the route that gives it; one ending in "/" covers its subtree
	err, code  string // Kula's JSON error text, and the AI routes' error code
	retryAfter bool   // Kula sends Retry-After with it
	busy       bool   // a busy answer, worth a retry (throttled); else unavailable
	why        string
}

// deliberate5xx are the 5xx answers internal/web gives on purpose. Each is
// matched as narrowly as its response allows: status and route, Kula's JSON
// error text and code, and Retry-After where Kula sends one. A HEAD answer
// carries no body, so there a JSON Content-Type stands in for the text.
var deliberate5xx = []deliberateAnswer{
	// handleCurrent before the first sample, right after start.
	{http.StatusServiceUnavailable, "/api/current", "no data yet", "", false, false,
		"no sample collected yet (right after start)"},
	// handleHistory when every history slot stayed busy for historySlotWait.
	{http.StatusServiceUnavailable, "/api/history", "too many history requests in progress", "", true, true,
		"history queries at their concurrency limit"},
	// handleLogin when no password-hashing slot freed up (the attempt is refunded).
	{http.StatusServiceUnavailable, "/api/login", "server busy", "", true, true,
		"login busy hashing passwords"},
	// The AI routes with the assistant disabled.
	{http.StatusServiceUnavailable, "/api/ollama/", "ollama is not enabled", "disabled", false, false,
		"the AI assistant is disabled"},
	// handleOllamaModels when the AI backend does not answer.
	{http.StatusBadGateway, "/api/ollama/models", "AI backend unavailable", "backend_unavailable", false, false,
		"the AI backend is unreachable"},
}

// deliberateAnswerFor returns the deliberate 5xx r is, if it is one.
func deliberateAnswerFor(r httpResult) (deliberateAnswer, bool) {
	route := routeOf(r.path)
	for _, d := range deliberate5xx {
		if r.status != d.status || !routeMatches(d.route, route) {
			continue
		}
		if d.retryAfter && r.header.Get("Retry-After") == "" {
			continue
		}
		if r.body == "" {
			if r.method == http.MethodHead && strings.HasPrefix(r.header.Get("Content-Type"), "application/json") {
				return d, true
			}
			continue
		}
		if e, ok := kulaErrorOf(r.body); ok && e.Error == d.err && e.Code == d.code {
			return d, true
		}
	}
	return deliberateAnswer{}, false
}

// routeOf returns the route a request path names: the path without its query.
func routeOf(path string) string {
	route, _, _ := strings.Cut(path, "?")
	return route
}

// routeMatches reports whether route is pattern, or under it when pattern
// ends in a slash.
func routeMatches(pattern, route string) bool {
	if strings.HasSuffix(pattern, "/") {
		return strings.HasPrefix(route, pattern)
	}
	return route == pattern
}

// kulaError is the JSON error body Kula answers with: jsonError's
// {"error": …}, and the AI routes' {"error": …, "code": …}.
type kulaError struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func kulaErrorOf(body string) (kulaError, bool) {
	var e kulaError
	err := json.Unmarshal([]byte(body), &e)
	return e, err == nil && e.Error != ""
}

// parserRefusal reports whether r is net/http's own answer to a request its
// parser refused before any handler ran: a text/plain 400, 431 or 505 whose
// body repeats its status, or the 501 for a Transfer-Encoding it does not
// implement. Raw requests with broken request lines, escapes or framing get
// these.
func parserRefusal(r httpResult) bool {
	if !strings.HasPrefix(r.header.Get("Content-Type"), "text/plain") {
		return false
	}
	switch r.status {
	case http.StatusBadRequest, http.StatusRequestHeaderFieldsTooLarge, http.StatusHTTPVersionNotSupported:
		return strings.HasPrefix(r.body, fmt.Sprintf("%d %s", r.status, http.StatusText(r.status)))
	case http.StatusNotImplemented:
		return r.body == "Unsupported transfer encoding"
	}
	return false
}

// gateRefusal reports whether r is the answer of one of Kula's gates rather
// than of the code under test: the auth gate's 401, the CSRF gate's 403 for
// a missing or foreign Origin or a missing token, or the Host check's 403. A
// HEAD answer has no body to tell by; a 401 or 403 to one is a gate's.
func gateRefusal(r httpResult) bool {
	if r.status != http.StatusUnauthorized && r.status != http.StatusForbidden {
		return false
	}
	if r.body == "" {
		return r.method == http.MethodHead
	}
	e, ok := kulaErrorOf(r.body)
	switch {
	case !ok:
		return false
	case r.status == http.StatusUnauthorized:
		return e.Error == "unauthorized"
	}
	return e.Error == "invalid origin" || e.Error == "invalid csrf token" || strings.HasPrefix(e.Error, "host not allowed")
}

// Kula's limits on the routes the fuzz probes post to. Every POST to
// /api/login counts against kulaLoginAddressLimit per client address over
// five minutes (loginIPAttemptLimit in internal/web/auth.go), whatever its
// body; one whose body decodes also counts against kulaLoginAccountLimit for
// the username it names (loginUserAttemptLimit). The AI chat admits
// kulaChatLimit requests a minute per address (ollamaChatRateLimit).
const (
	kulaLoginAddressLimit = 15
	kulaLoginAccountLimit = 5
	kulaChatLimit         = 10
)

// throttledBy names the limiter a 429 came from, and when to retry.
func throttledBy(r httpResult) string {
	route := routeOf(r.path)
	switch {
	case route == "/api/login":
		return fmt.Sprintf("the login limiter (%d attempts per address, %d per account and address, in five minutes); retry in five minutes",
			kulaLoginAddressLimit, kulaLoginAccountLimit)
	case route == "/api/ollama/chat":
		return fmt.Sprintf("the AI chat limiter (%d requests a minute per address); retry in a minute", kulaChatLimit)
	case strings.HasPrefix(route, "/api/ollama/"):
		return "the AI limiter (60 requests a minute per address); retry in a minute"
	case route == "/ws":
		return "the WebSocket connection limit; retry once other connections close"
	}
	return "HTTP 429" + retryAfter(r)
}

// retryAfter renders r's Retry-After, if any, for a detail.
func retryAfter(r httpResult) string {
	if ra := r.header.Get("Retry-After"); ra != "" {
		return "; retry after " + ra + " s"
	}
	return ""
}

// ---- coverage ---------------------------------------------------------------

// maxFuzzHits bounds the anomalies a probe collects; it stops there, since a
// target that hangs on every request would take -timeout each.
const maxFuzzHits = 8

// fuzzTally counts what became of a probe's requests.
type fuzzTally struct {
	planned     int // the requests the probe meant to send
	sent        int // written to the target
	reached     int // answered past the gates (outReached)
	parser      int // of reached: refused by net/http's parser before routing
	gated       int
	throttled   int
	unavailable int
	unsent      int
	failed      int
	anomalies   int
	hits        []fuzzHit // the first maxFuzzHits anomalies

	firstUnsent                error
	throttledBy, unavailableBy string
	stopped                    string // why the probe sent fewer than planned
	lastAnswered               string // the last request the target answered
	stoppedListening           bool   // that anomaly is reported once
}

// hit records an anomaly.
func (t *fuzzTally) hit(kind, req string) {
	t.anomalies++
	if len(t.hits) < maxFuzzHits {
		t.hits = append(t.hits, fuzzHit{kind, req})
	}
}

// full reports whether the probe has collected all the anomalies it reports.
func (t *fuzzTally) full() bool {
	if t.anomalies >= maxFuzzHits {
		t.stopped = fmt.Sprintf("after %d anomalies", maxFuzzHits)
		return true
	}
	return false
}

// record judges one request and counts it. desc names the request in
// evidence; canary, when set, must not come back unescaped in an HTML or
// JavaScript answer.
func (t *fuzzTally) record(r httpResult, desc, canary string) judgement {
	j := classify(r)
	if j.outcome != outUnsent {
		t.sent++
	}
	switch j.outcome {
	case outReached:
		t.reached++
		if j.parser {
			t.parser++
		}
		if reflectedXSS(r, canary) {
			t.hit("reflected input (possible XSS)", desc)
		}
	case outGated:
		t.gated++
	case outThrottled:
		t.throttled++
		if t.throttledBy == "" {
			t.throttledBy = j.why
		}
	case outUnavailable:
		t.unavailable++
		if t.unavailableBy == "" {
			t.unavailableBy = j.why
		}
	case outUnsent:
		t.unsent++
		if t.firstUnsent == nil {
			t.firstUnsent = r.err
		}
		// The target took this probe's connections before: one it now
		// refuses means it went away.
		if j.refused && t.lastAnswered != "" && !t.stoppedListening {
			t.stoppedListening = true
			t.hit("the target stopped accepting connections (crashed?)", "after "+t.lastAnswered)
		}
	case outFailed:
		t.failed++
	case outAnomaly:
		t.hit(j.why, desc)
	}
	if r.err == nil {
		t.lastAnswered = desc
	}
	return j
}

// add counts other's requests into t.
func (t *fuzzTally) add(other *fuzzTally) {
	t.planned += other.planned
	t.sent += other.sent
	t.reached += other.reached
	t.parser += other.parser
	t.gated += other.gated
	t.throttled += other.throttled
	t.unavailable += other.unavailable
	t.unsent += other.unsent
	t.failed += other.failed
	for _, h := range other.hits {
		t.hit(h.kind, h.req)
	}
	t.anomalies += other.anomalies - len(other.hits)
	if t.firstUnsent == nil {
		t.firstUnsent = other.firstUnsent
	}
	if t.throttledBy == "" {
		t.throttledBy = other.throttledBy
	}
	if t.unavailableBy == "" {
		t.unavailableBy = other.unavailableBy
	}
}

// coverage says what became of the requests; target names the code under
// test ("the history and i18n handlers").
func (t *fuzzTally) coverage(target string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d planned, %d sent: %d reached %s", t.planned, t.sent, t.reached, target)
	if t.parser > 0 {
		fmt.Fprintf(&b, " (%d of them refused by the HTTP parser before routing)", t.parser)
	}
	if t.gated > 0 {
		fmt.Fprintf(&b, ", %d stopped at the auth or CSRF gate", t.gated)
	}
	if t.throttled > 0 {
		fmt.Fprintf(&b, ", %d throttled", t.throttled)
	}
	if t.unavailable > 0 {
		fmt.Fprintf(&b, ", %d answered unavailable (%s)", t.unavailable, t.unavailableBy)
	}
	if t.failed > 0 {
		fmt.Fprintf(&b, ", %d ended without an answer", t.failed)
	}
	if t.unsent > 0 {
		fmt.Fprintf(&b, "; %d not sent (%s)", t.unsent, truncate(printable(fmt.Sprint(t.firstUnsent)), 160))
	}
	if rest := t.planned - t.sent - t.unsent; rest > 0 {
		why := t.stopped
		if why == "" {
			why = "once throttled"
		}
		fmt.Fprintf(&b, "; %d not sent %s", rest, why)
	}
	return b.String()
}

// verdict turns a tally into the probe's finding: an anomaly FAILs it. When
// nothing reached the code under test it SKIPs if a limiter or a disabled
// feature turned the requests away, and is an ERROR otherwise. When some did
// it PASSes, saying how many, or WARNs when fewer than half of the planned
// requests did. coverage says what was sent (see coverage); note, when set,
// is a further sentence.
func (t *fuzzTally) verdict(id string, seed int64, coverage, note string) Finding {
	if note != "" {
		note = " " + note
	}
	switch {
	case t.anomalies > 0:
		ev := make([]string, 0, len(t.hits))
		for _, h := range t.hits {
			ev = append(ev, h.kind+" ← "+h.req)
		}
		noun := "anomaly"
		if t.anomalies > 1 {
			noun = "anomalies"
		}
		return finding(id, SevHigh, StatusFail,
			fmt.Sprintf("found %d %s the server should never produce on hostile input; %s.%s", t.anomalies, noun, coverage, note)).
			withEvidence("seed=%d | %s", seed, strings.Join(ev, " | ")).
			withRemediation("Return 4xx for malformed input; never 5xx, panic, or hang. Reproduce with -seed and the listed requests.")
	case t.reached == 0 && t.throttled > 0:
		return finding(id, SevInfo, StatusSkip,
			fmt.Sprintf("nothing reached the code under test: %s. Turned away by %s.%s", coverage, t.throttledBy, note)).
			withEvidence("seed=%d", seed)
	case t.reached == 0 && t.unavailable > 0 && t.unsent+t.failed+t.gated == 0:
		return finding(id, SevInfo, StatusSkip,
			fmt.Sprintf("nothing reached the code under test: %s. The target answered that %s.%s", coverage, t.unavailableBy, note)).
			withEvidence("seed=%d", seed)
	case t.reached == 0:
		why := ""
		if t.gated > 0 {
			why = " Kula's auth or CSRF gate answered them; the session may have ended."
		}
		return finding(id, SevInfo, StatusError,
			fmt.Sprintf("nothing reached the code under test: %s.%s%s", coverage, why, note)).
			withEvidence("seed=%d", seed)
	case t.reached*2 < t.planned:
		return finding(id, SevInfo, StatusWarn,
			fmt.Sprintf("no anomalies, but fewer than half of the planned requests reached the code under test: %s.%s", coverage, note)).
			withEvidence("seed=%d", seed)
	default:
		return finding(id, SevInfo, StatusPass, fmt.Sprintf("no anomalies: %s.%s", coverage, note)).
			withEvidence("seed=%d", seed)
	}
}

// reqDesc renders a compact, reproducible request description.
func reqDesc(method, path, body string) string {
	d := method + " " + truncate(path, 120)
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

// runFuzzLoop drives gen for fuzzIter iterations, stopping early at
// maxFuzzHits anomalies, and renders a single aggregated finding. target
// names the code under test.
func (s *Scanner) runFuzzLoop(id, target string, headers map[string]string, gen func() (method, path, body, canary string)) Finding {
	t := &fuzzTally{planned: s.fuzzIter}
	for i := 0; i < s.fuzzIter && !t.full() && s.ctx.Err() == nil; i++ {
		m, p, b, canary := gen()
		t.record(s.do(m, p, headers, b), reqDesc(m, p, b), canary)
	}
	return t.verdict(id, s.seed, t.coverage(target), "")
}

// runFuzzQuery fuzzes the query parameters of the read API endpoints.
func runFuzzQuery(s *Scanner) []Finding {
	hdr, ok := s.apiHeaders()
	if !ok {
		return []Finding{finding("FUZZ-QUERY", SevInfo, StatusSkip,
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
	return []Finding{s.runFuzzLoop("FUZZ-QUERY", "the history and i18n handlers", hdr, gen)}
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
	return []Finding{s.runFuzzLoop("FUZZ-PATH", "the server", nil, gen)}
}

// runFuzzBody sends a curated set of malformed JSON bodies to the JSON POST
// endpoints. Login is per-IP rate-limited, so coverage there is best-effort;
// the point is that a malformed body must yield 400 — never a 5xx or a panic.
// Login exists only with auth enabled (Kula answers it with 404 otherwise) and
// the Ollama chat only with Ollama enabled; with neither the probe is skipped.
func runFuzzBody(s *Scanner) []Finding {
	if !s.authEnabled && !s.ollama {
		return []Finding{finding("FUZZ-BODY", SevInfo, StatusSkip,
			"auth and Ollama are disabled; the instance has no endpoint that parses a JSON body.")}
	}
	bodies := []string{
		`{`, `{"username":}`, `{"username":123,"password":[]}`, `[1,2,3]`, `"x"`, `null`, `12345`,
		"\x00\x01garbage", `{"username":"` + strings.Repeat("A", 6000) + `"}`,
		`{"a":` + deepNest(2000) + `}`, strings.Repeat("[", 3000) + strings.Repeat("]", 3000),
		`{"username":"x"` + strings.Repeat(`,"x":"y"`, 5000) + `}`,
	}
	origin := s.base.Scheme + "://" + s.base.Host

	total := &fuzzTally{}
	post := func(path string) {
		t := &fuzzTally{planned: len(bodies)}
		for _, b := range bodies {
			r := s.do(http.MethodPost, path, map[string]string{"Content-Type": "application/json", "Origin": origin}, b)
			t.record(r, reqDesc(http.MethodPost, path, b), "")
			if r.status == http.StatusTooManyRequests {
				break // further attempts won't reach the body parser
			}
		}
		total.add(t)
	}
	if s.authEnabled {
		post("/api/login")
	}
	if s.ollama {
		post("/api/ollama/chat")
	}
	return []Finding{total.verdict("FUZZ-BODY", s.seed, total.coverage("the JSON body parsers"), "")}
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
	t := &fuzzTally{planned: len(routes) * len(methods)}
	for _, route := range routes {
		for _, m := range methods {
			r := s.do(m, route, hdr, "")
			if t.record(r, reqDesc(m, route, ""), "").outcome == outAnomaly {
				continue
			}
			if postOnly[route] && r.status >= 200 && r.status < 300 {
				t.hit("state-changing method accepted", reqDesc(m, route, ""))
			}
		}
	}
	return []Finding{t.verdict("FUZZ-METHODS", s.seed, t.coverage("a handler"), "")}
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
	// The -H header lines follow the Host line of what is sent; the evidence
	// quotes the request without them, since their values are secrets.
	hostLine := "\r\nHost: " + host + "\r\n"
	extra := s.rawHeaderLines(nil)
	var hits []fuzzHit
	var lost undelivered
	for _, raw := range raws {
		status, _, err := s.rawExchange(strings.Replace(raw, hostLine, hostLine+extra, 1))
		if err != nil {
			lost.add(err)
			continue
		}
		switch {
		case status == 500:
			// A 400/501 rejection is the correct, safe response; only a 500 means
			// the ambiguous framing actually broke a handler.
			hits = append(hits, fuzzHit{"HTTP 500", truncate(strings.ReplaceAll(raw, "\r\n", "\\n"), 90)})
		case status >= 200 && status < 300:
			hits = append(hits, fuzzHit{"accepted ambiguous framing (smuggling risk)", truncate(strings.ReplaceAll(raw, "\r\n", "\\n"), 90)})
		}
	}
	if len(hits) == 0 && lost.n > 0 {
		return []Finding{finding("FUZZ-SMUGGLE", SevInfo, StatusError, lost.detail(len(raws), "smuggling requests")+".")}
	}
	if len(hits) == 0 {
		return []Finding{finding("FUZZ-SMUGGLE", SevMedium, StatusPass,
			"ambiguous Content-Length / Transfer-Encoding framing is rejected (no desync, no 5xx).")}
	}
	t := &fuzzTally{planned: len(raws), sent: len(raws) - lost.n}
	for _, h := range hits {
		t.hit(h.kind, h.req)
	}
	return []Finding{t.verdict("FUZZ-SMUGGLE", s.seed, t.coverage("the server"), "")}
}

// runFuzzWS opens an authenticated, same-origin WebSocket and floods it with
// malformed and binary frames plus rapid pause/resume churn, then confirms the
// server is still serving new WebSocket connections and HTTP requests.
func runFuzzWS(s *Scanner) []Finding {
	var hdr http.Header
	if s.authEnabled {
		if !s.ensureSession() {
			return []Finding{finding("FUZZ-WS", SevInfo, StatusSkip,
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
		return []Finding{finding("FUZZ-WS", SevCritical, StatusFail,
			"after a burst of malformed WebSocket frames, HTTP /health stopped responding — the server was destabilized.").
			withEvidence("health err=%v status=%d", live.err, live.status).
			withRemediation("Ensure the WebSocket read loop tolerates any client input without panicking or blocking the server.")}
	case !wsOK:
		return []Finding{finding("FUZZ-WS", SevHigh, StatusWarn,
			"HTTP still works, but a fresh WebSocket upgrade failed after the fuzz burst (possible connection-slot leak).").
			withEvidence("ws dial err=%v", err).
			withRemediation("Verify failed/garbage connections are always unregistered and their slots released.")}
	default:
		return []Finding{finding("FUZZ-WS", SevInfo, StatusPass,
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
			return []Finding{finding("FUZZ-LIVENESS", SevCritical, StatusPass,
				"the instance is still reachable and serving after the full fault-injection barrage.")}
		}
		lastErr = r.err
		time.Sleep(500 * time.Millisecond)
	}
	return []Finding{finding("FUZZ-LIVENESS", SevCritical, StatusFail,
		"the instance stopped responding to /health after fuzzing — fault injection may have crashed or hung it.").
		withEvidence("last err=%v", lastErr).
		withRemediation("Investigate the preceding fuzz findings; a monitoring daemon must never be crashable by remote input.")}
}

// deepNest builds a string of n nested single-key JSON objects, used to probe
// recursive-descent parser limits (stack exhaustion).
func deepNest(n int) string {
	return strings.Repeat(`{"a":`, n) + "1" + strings.Repeat("}", n)
}
