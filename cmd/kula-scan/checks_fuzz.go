package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
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

// fuzzChecks are the fault-injection probes. They run before the probes that
// exhaust the login limiter, since FUZZ-BODY spends login attempts, and
// fuzzLivenessCheck runs after those (see allChecks).
func fuzzChecks() []check {
	return []check{
		{ids: []string{"FUZZ-QUERY"}, run: runFuzzQuery},
		{ids: []string{"FUZZ-PATH"}, run: runFuzzPath},
		{ids: []string{"FUZZ-BODY"}, run: runFuzzBody},
		{ids: []string{"FUZZ-METHODS"}, run: runFuzzMethods},
		{ids: []string{"FUZZ-SMUGGLE"}, run: runFuzzSmuggle},
		{ids: []string{"FUZZ-WS"}, run: runFuzzWS},
	}
}

// fuzzLivenessCheck is the run's last probe: the target must still answer
// after everything before it.
func fuzzLivenessCheck() check {
	return check{ids: []string{"FUZZ-LIVENESS"}, run: runFuzzLiveness}
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
		why := fmt.Sprintf("%s (HTTP %d%s)", d.why, r.status, retryAfter(r))
		if d.busy {
			return judgement{outcome: outThrottled, why: why}
		}
		return judgement{outcome: outUnavailable, why: why}
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
		"no sample collected yet, right after start"},
	// handleHistory when every history slot stayed busy for historySlotWait.
	{http.StatusServiceUnavailable, "/api/history", "too many history requests in progress", "", true, true,
		"the history concurrency limit"},
	// handleLogin when no password-hashing slot freed up (the attempt is refunded).
	{http.StatusServiceUnavailable, "/api/login", "server busy", "", true, true,
		"login's password-hashing limit, server busy"},
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
		return fmt.Sprintf("the login limiter (%d attempts per address, %d per account and address, in five minutes; retry in five minutes)",
			kulaLoginAddressLimit, kulaLoginAccountLimit)
	case route == "/api/ollama/chat":
		return fmt.Sprintf("the AI chat limiter (%d requests a minute per address; retry in a minute)", kulaChatLimit)
	case strings.HasPrefix(route, "/api/ollama/"):
		return "the AI limiter (60 requests a minute per address; retry in a minute)"
	case route == "/ws":
		return "the WebSocket connection limit (retry once other connections close)"
	}
	return "a limiter (HTTP 429" + retryAfter(r) + ")"
}

// retryAfter renders r's Retry-After, if any, for a detail.
func retryAfter(r httpResult) string {
	if ra := r.header.Get("Retry-After"); ra != "" {
		return ", retry after " + ra + " s"
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
		fmt.Fprintf(&b, ", %d throttled by %s", t.throttled, t.throttledBy)
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
			fmt.Sprintf("nothing reached the code under test, a limiter turned the requests away: %s.%s", coverage, note)).
			withEvidence("seed=%d", seed)
	case t.reached == 0 && t.unavailable > 0 && t.unsent+t.failed+t.gated == 0:
		return finding(id, SevInfo, StatusSkip,
			fmt.Sprintf("nothing reached the code under test, the target answered that it is unavailable: %s.%s", coverage, note)).
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

// fuzzCanary returns a fresh reflection canary. It carries angle brackets so
// a reflection only counts when the server echoes them UNescaped — an actual
// XSS sink, not a redirect body or JSON string that safely escapes "<".
func fuzzCanary() string {
	return "<kx" + randToken(6) + ">"
}

// ---- raw requests -----------------------------------------------------------

// rawGet sends GET path (relative to the base path) exactly as written, over
// a raw connection, with headers and the -H headers, and reads the answer.
// The HTTP client escapes, refuses or normalises what a hostile path or
// query is made of (control and high bytes, bad percent-escapes, angle
// brackets); this sends it as it is.
func (s *Scanner) rawGet(path string, headers map[string]string) httpResult {
	own := http.Header{}
	for name, value := range headers {
		own.Set(name, value)
	}
	head := fmt.Sprintf("GET %s%s HTTP/1.1\r\nHost: %s\r\n%sConnection: close\r\n\r\n",
		s.base.Path, path, s.base.Host, s.rawHeaderLines(own))
	return s.rawRequest(http.MethodGet, path, head)
}

// rawRequest writes raw, a whole request, on a raw connection and reads one
// response. Unlike rawExchange it keeps what the fuzz oracle needs: the
// response headers (the reflection canary looks at the Content-Type), and
// the error as it came, so that a request the target dropped or left
// unanswered is told from one that never reached it (unsent).
func (s *Scanner) rawRequest(method, path, raw string) httpResult {
	r := httpResult{method: method, path: path}
	conn, err := s.dialRaw()
	if err == nil {
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(s.timeout))
		_, err = io.WriteString(conn, raw)
	}
	if err != nil {
		r.err, r.unsent = err, true
		s.logRaw(r)
		return r
	}
	resp, err := readResponse(conn)
	if err != nil {
		r.err = err
		s.logRaw(r)
		return r
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes))
	_ = resp.Body.Close()
	r.status, r.header, r.body = resp.StatusCode, resp.Header, string(body)
	s.logRaw(r)
	return r
}

// logRaw logs a raw request and its outcome, when -v asks for it.
func (s *Scanner) logRaw(r httpResult) {
	if !s.verbose {
		return
	}
	path := printable(truncate(r.path, 120))
	switch {
	case r.unsent:
		_, _ = fmt.Fprintf(s.log, "    raw %s %s -> not sent: %s\n", r.method, path, printable(r.err.Error()))
	case r.err != nil:
		_, _ = fmt.Fprintf(s.log, "    raw %s %s -> %s\n", r.method, path, printable(r.err.Error()))
	default:
		_, _ = fmt.Fprintf(s.log, "    raw %s %s -> %d (%d bytes)\n", r.method, path, r.status, len(r.body))
	}
}

// rawPathOddities are what the HTTP client will not send as written: bad
// and overlong percent-escapes, raw control and high bytes, backslashes and
// odd separators. FUZZ-PATH puts them around its tokens on the raw socket.
var rawPathOddities = []string{
	"", "", "", "//", "/./", "/../", "\\", "\\..\\", ";", ";x=1", "?", "??", "#",
	"%", "%z", "%zz", "%%", "%0", "%u002e", "%c0%af", "%2f", "%5c", "%00", "\x7f", "\xff", "\xc0\xae", "\t",
}

// rawQueryOddities are the same for a query string.
var rawQueryOddities = []string{
	"", "", "", "", "%", "%z", "%zz", "%%", "%0", "%00", "+", "=", "==", "?", "#", "[]", "\x00", "\xff",
}

// rawQuerySeparators join the pairs of a raw query.
var rawQuerySeparators = []string{"&", "&", "&", ";", "&&", "&amp;", "%26"}

func (s *Scanner) pick(list []string) string {
	return list[s.randIntn(len(list))]
}

// ---- FUZZ-PATH --------------------------------------------------------------

// runFuzzPath fuzzes the request path with a canary so any reflection into
// the HTML shell is caught. Half the paths are well formed (fully escaped)
// and go through the HTTP client; the other half are written raw, the token,
// oddities and canary as they are, so they reach the server as written.
func runFuzzPath(s *Scanner) []Finding {
	t := &fuzzTally{planned: s.fuzzIter}
	for i := 0; i < s.fuzzIter && !t.full() && s.ctx.Err() == nil; i++ {
		canary, tok := fuzzCanary(), s.fuzzToken()
		if s.randIntn(2) == 0 {
			p := "/" + url.PathEscape(tok+canary)
			t.record(s.do(http.MethodGet, p, nil, ""), reqDesc(http.MethodGet, p, ""), canary)
			continue
		}
		p := "/" + s.pick(rawPathOddities) + tok + s.pick(rawPathOddities) + canary
		t.record(s.rawGet(p, nil), "raw "+reqDesc(http.MethodGet, p, ""), canary)
	}
	return []Finding{t.verdict("FUZZ-PATH", s.seed, t.coverage("the server"), "")}
}

// ---- FUZZ-QUERY -------------------------------------------------------------

// historyParams are /api/history's query parameters.
var historyParams = []string{"from", "to", "points", "tier", "sections"}

// historySections are the names /api/history's sections parameter takes.
var historySections = []string{"cpu", "lavg", "mem", "swap", "net", "disk", "sys", "proc", "self", "gpu", "psu", "apps"}

// historyValue returns a value for a history parameter: a hostile token, or
// a valid or nearly valid one, so that some requests pass validation and
// reach the store.
func (s *Scanner) historyValue(key string, now time.Time) string {
	if s.randIntn(2) == 0 {
		return s.fuzzToken()
	}
	switch key {
	case "from", "to":
		switch s.randIntn(5) {
		case 0:
			return now.Format(time.RFC3339)
		case 1:
			return now.Add(-5 * time.Minute).UTC().Format(time.RFC3339Nano)
		case 2:
			return now.Add(-24 * time.Hour).In(time.FixedZone("", 14*3600)).Format(time.RFC3339)
		default:
			return s.pick([]string{"0001-01-01T00:00:00Z", "9999-12-31T23:59:59Z", "2026-02-30T00:00:00Z",
				"2026-01-01T24:00:00Z", "1970-01-01T00:00:00-12:00", "2026-01-01T00:00:00", "2026-01-01"})
		}
	case "points":
		return s.pick([]string{"0", "1", "-1", "450", "5000", "5001", "1e3", "0x10"})
	case "tier":
		return s.pick([]string{"0", "1", "2", "3", "-1", "9", "+1", "01", "4294967296"})
	default: // sections
		names := make([]string, 0, 3)
		for range 1 + s.randIntn(3) {
			names = append(names, s.pick(historySections))
		}
		return s.pick([]string{strings.Join(names, ","), strings.Join(names, ",") + "," + s.fuzzToken(),
			",,,", " cpu , mem ", "CPU", "cpu,cpu,cpu", ","})
	}
}

// historyQuery returns a well-formed history query, canary in one value.
func (s *Scanner) historyQuery(now time.Time, canary string) string {
	q := url.Values{}
	for _, key := range historyParams {
		if s.randIntn(3) != 0 {
			q.Set(key, s.historyValue(key, now))
		}
	}
	if canary != "" {
		key := s.pick(historyParams)
		q.Set(key, q.Get(key)+canary)
	}
	return q.Encode()
}

// rawHistoryQuery returns a history query as written: values unescaped,
// oddities around them, odd separators and pairs, canary in one value.
func (s *Scanner) rawHistoryQuery(now time.Time, canary string) string {
	var pairs []string
	for _, key := range historyParams {
		if s.randIntn(3) != 0 {
			pairs = append(pairs, key+"="+s.pick(rawQueryOddities)+s.historyValue(key, now)+s.pick(rawQueryOddities))
		}
	}
	if s.randIntn(3) == 0 {
		pairs = append(pairs, s.pick([]string{"from=", "=1", "tier", "tier[]=0", "points=1&points=2", "sections", "%zz=1"}))
	}
	if len(pairs) == 0 {
		pairs = append(pairs, "tier="+s.fuzzToken())
	}
	if canary != "" {
		pairs[s.randIntn(len(pairs))] += canary
	}
	var b strings.Builder
	for i, pair := range pairs {
		if i > 0 {
			b.WriteString(s.pick(rawQuerySeparators))
		}
		b.WriteString(pair)
	}
	return b.String()
}

// i18nQuery returns a lang query: well formed, or as written.
func (s *Scanner) i18nQuery(raw bool, canary string) string {
	if !raw {
		q := url.Values{}
		q.Set("lang", s.fuzzToken()+canary)
		return q.Encode()
	}
	lang := "lang=" + s.pick(rawQueryOddities) + s.fuzzToken() + s.pick(rawQueryOddities) + canary
	if s.randIntn(4) == 0 {
		lang = "lang=en" + s.pick(rawQuerySeparators) + lang
	}
	return lang
}

// runFuzzQuery fuzzes the query parameters of the read API endpoints:
// /api/history's from, to, points, tier and sections, and /api/i18n's lang.
// Half the queries are well formed and go through the HTTP client; the
// other half are written raw.
func runFuzzQuery(s *Scanner) []Finding {
	hdr, ok := s.apiHeaders()
	if !ok {
		return []Finding{finding("FUZZ-QUERY", SevInfo, StatusSkip,
			"auth is enabled and no session is available; supply -username/-password to fuzz the API.")}
	}
	now := time.Now()
	t := &fuzzTally{planned: s.fuzzIter}
	for i := 0; i < s.fuzzIter && !t.full() && s.ctx.Err() == nil; i++ {
		canary := ""
		if s.randIntn(4) == 0 {
			canary = fuzzCanary()
		}
		history, raw := s.randIntn(3) != 0, s.randIntn(2) == 0
		var path string
		switch {
		case history && raw:
			path = "/api/history?" + s.rawHistoryQuery(now, canary)
		case history:
			path = "/api/history?" + s.historyQuery(now, canary)
		default:
			path = "/api/i18n?" + s.i18nQuery(raw, canary)
		}
		if raw {
			t.record(s.rawGet(path, hdr), "raw "+reqDesc(http.MethodGet, path, ""), canary)
		} else {
			t.record(s.do(http.MethodGet, path, hdr, ""), reqDesc(http.MethodGet, path, ""), canary)
		}
	}
	return []Finding{t.verdict("FUZZ-QUERY", s.seed, t.coverage("the history and i18n handlers"), "")}
}

// ---- FUZZ-BODY --------------------------------------------------------------

// fuzzBody is one malformed body FUZZ-BODY sends, named for evidence.
type fuzzBody struct{ name, body string }

// fuzzLoginBodies is how many bodies FUZZ-BODY sends to /api/login. Each
// costs one of the address's kulaLoginAddressLimit login attempts in five
// minutes, whatever it holds, so it sends few, and leaves the probes that
// run after it the attempts they need (see allChecks).
const fuzzLoginBodies = 6

// loginFuzzBodies are the bodies for /api/login: the cases that matter for
// its decoder. None decodes into a login, so none counts against an
// account's limit or costs a password hash, and all but the oversized one
// fit the 4 KiB cap, so the decoder reads them through.
func loginFuzzBodies() []fuzzBody {
	return []fuzzBody{
		{"truncated JSON", `{"username":"kula-scan-fuzz","password":`},
		{"wrong types", `{"username":123,"password":["x"]}`},
		{"2000 levels of nesting", strings.Repeat("[", 2000) + strings.Repeat("]", 2000)},
		{"a 64 KiB array, past the 4 KiB cap", "[" + strings.Repeat("0,", 32*1024-1) + "0]"},
		{"400 keys", manyKeys(400, `"username":1`)},
		{"binary garbage", "\x00\x01\x02\x03\xff\xfe\x1b[0m{\"\x00"},
	}
}

// fuzzChatBodies is how many bodies FUZZ-BODY sends to /api/ollama/chat,
// within the chat limiter's kulaChatLimit a minute per address, with room
// to spare for the dashboard.
const fuzzChatBodies = 6

// chatFuzzBodies are the bodies for /api/ollama/chat. Every one fails Go's
// decoder, size cap or not: a body that decodes into a chat request (null,
// {}, an object of unknown keys, however deeply nested) starts a real chat
// with the model, which costs the operator and, with a slow model, reads as
// a hang.
func chatFuzzBodies() []fuzzBody {
	return []fuzzBody{
		{"truncated JSON", `{"prompt":"kula-scan fuzz","messages":[{"role":"user","content":`},
		{"wrong types", `{"prompt":123,"messages":"x","lang":[]}`},
		{"12000 levels of nesting, past the decoder's depth limit",
			`{"prompt":` + strings.Repeat("[", 12000) + strings.Repeat("]", 12000) + `}`},
		{"a 40 KiB array, past the 32 KiB cap", `{"prompt":[` + strings.Repeat("0,", 20*1024) + `0]}`},
		{"2000 keys", manyKeys(2000, `"prompt":1`)},
		{"binary garbage", "\x00\x01\x02\x03\xff\xfe\x1b[0m{\"\x00"},
	}
}

// manyKeys returns a JSON object of n keys, "k0":0 on, ending with last.
func manyKeys(n int, last string) string {
	var b strings.Builder
	b.WriteString("{")
	for i := range n {
		fmt.Fprintf(&b, `"k%d":0,`, i)
	}
	b.WriteString(last + "}")
	return b.String()
}

// runFuzzBody sends malformed JSON bodies to the endpoints that parse one:
// login, which exists with auth on (Kula answers it 404 otherwise), and the
// AI chat, with the assistant on. A malformed body must yield 400 — never a
// 5xx, a panic or a hang. A half whose endpoint does not exist or cannot be
// reached is left out, saying why; with neither, the probe is skipped.
func runFuzzBody(s *Scanner) []Finding {
	origin := s.base.Scheme + "://" + s.base.Host
	total := &fuzzTally{}
	var covered, left []string

	if s.authEnabled {
		// No session: CSRF admits a request without one on its Origin, and
		// login does not ask for one.
		hdr := map[string]string{"Content-Type": "application/json", "Origin": origin}
		login := s.postFuzzBodies("/api/login", loginFuzzBodies(), hdr)
		total.add(login)
		covered = append(covered, "login: "+login.coverage("its body parser"))
	} else {
		left = append(left, "auth is disabled, so there is no login endpoint")
	}

	if open, why := s.chatOpen(); open {
		hdr := map[string]string{"Content-Type": "application/json", "Origin": origin}
		if s.loggedIn {
			// Behind auth the chat needs the session, and CSRF its token.
			hdr["Cookie"] = "kula_session=" + s.session
			hdr["X-CSRF-Token"] = s.csrf
		}
		chat := s.postFuzzBodies("/api/ollama/chat", chatFuzzBodies(), hdr)
		total.add(chat)
		covered = append(covered, "AI chat: "+chat.coverage("its body decoder"))
	} else {
		left = append(left, why)
	}

	if len(covered) == 0 {
		return []Finding{finding("FUZZ-BODY", SevInfo, StatusSkip,
			"no endpoint that parses a JSON body could be sent one: "+strings.Join(left, "; ")+".")}
	}
	note := ""
	if len(left) > 0 {
		note = "Not covered: " + strings.Join(left, "; ") + "."
	}
	return []Finding{total.verdict("FUZZ-BODY", s.seed, strings.Join(covered, "; "), note)}
}

// postFuzzBodies posts each body to path, and stops at the first 429: the
// limiter refuses the rest too, and each further attempt would count.
func (s *Scanner) postFuzzBodies(path string, bodies []fuzzBody, headers map[string]string) *fuzzTally {
	t := &fuzzTally{planned: len(bodies)}
	for _, b := range bodies {
		if s.ctx.Err() != nil {
			break
		}
		r := s.do(http.MethodPost, path, headers, b.body)
		t.record(r, reqDesc(http.MethodPost, path, "")+" ("+b.name+")", "")
		if r.err == nil && r.status == http.StatusTooManyRequests {
			t.stopped = "once throttled"
			break
		}
	}
	return t
}

// chatOpen reports whether the AI chat can be sent bodies, or why not.
// Discovery reads ollama_enabled from /api/config, which auth hides from a
// client without a session; behind auth it is asked again with the session.
func (s *Scanner) chatOpen() (bool, string) {
	const disabled = "the AI assistant is disabled, so there is no chat endpoint"
	switch {
	case !s.authEnabled && !s.ollama:
		return false, disabled
	case !s.authEnabled:
		return true, ""
	case !s.ensureSession():
		return false, "auth is enabled and no session is available to reach the AI chat (supply -username/-password)"
	case s.ollama:
		return true, ""
	}
	r := s.do(http.MethodGet, "/api/config", s.authedHeaders(false), "")
	var cfg struct {
		OllamaEnabled bool `json:"ollama_enabled"`
	}
	if r.err == nil && r.status == http.StatusOK && json.Unmarshal([]byte(r.body), &cfg) == nil && cfg.OllamaEnabled {
		return true, ""
	}
	return false, disabled
}

// ---- FUZZ-METHODS -----------------------------------------------------------

// routeKind is which methods a route serves, as FUZZ-METHODS judges it.
type routeKind int

const (
	// routeOther is outside /api/: the page, static assets, /health and
	// /status (which Kula serves to any method), /metrics and the WebSocket.
	routeOther routeKind = iota
	// routeReadOnly is a GET-only API route: Kula's readOnly answers any
	// other method 405.
	routeReadOnly
	// routePostOnly is login, logout or the AI chat: their handlers answer
	// any other method 405 before doing anything.
	routePostOnly
)

// methodRoutes are the routes FUZZ-METHODS sends its methods to.
var methodRoutes = []struct {
	path string
	kind routeKind
}{
	{"/", routeOther},
	{"/style.css", routeOther},
	{"/health", routeOther},
	{"/status", routeOther},
	{"/metrics", routeOther},
	{"/ws", routeOther},
	{"/api/current", routeReadOnly},
	{"/api/config", routeReadOnly},
	{"/api/history", routeReadOnly},
	{"/api/i18n?lang=en", routeReadOnly},
	{"/api/system-info", routeReadOnly},
	{"/api/auth/status", routeReadOnly},
	{"/api/ollama/models", routeReadOnly},
	{"/api/ollama/context", routeReadOnly},
	{"/api/login", routePostOnly},
	{"/api/logout", routePostOnly},
	{"/api/ollama/chat", routePostOnly},
}

// unusualMethods are sent to every route, with HEAD and OPTIONS.
var unusualMethods = []string{
	http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodTrace, http.MethodConnect,
	"PROPFIND", "MKCOL", "BREW", "FOObar",
}

// methodProbe is one request of FUZZ-METHODS' matrix.
type methodProbe struct {
	method, path string
	kind         routeKind
}

// methodMatrix returns FUZZ-METHODS' requests: the unusual methods, HEAD and
// OPTIONS on every route, and POST on the GET-only API routes. It sends no
// POST to a POST-only route, the one method that does something there: POST
// /api/login spends one of the address's login attempts, POST
// /api/ollama/chat could start a chat with the model, and POST /api/logout
// would end the scan's session. Kula's handlers for those answer any other
// method 405 before doing anything (handleLogin after its 404 with auth off,
// handleLogout and handleOllamaChat first thing).
func methodMatrix() []methodProbe {
	var matrix []methodProbe
	for _, route := range methodRoutes {
		methods := append(slices.Clone(unusualMethods), http.MethodHead, http.MethodOptions)
		if route.kind == routeReadOnly {
			methods = append(methods, http.MethodPost)
		}
		for _, m := range methods {
			matrix = append(matrix, methodProbe{m, route.path, route.kind})
		}
	}
	return matrix
}

// stateChanging reports whether method is one a read-only route must refuse
// for the judgement: POST, PUT, DELETE or PATCH.
func stateChanging(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
		return true
	}
	return false
}

// runFuzzMethods sends the method matrix (methodMatrix) past Kula's gates: a
// same-origin Origin for the CSRF gate and, with a session, its cookie and
// CSRF token. The logout is sent no session, so a server that ended one on
// another method would not end the scan's. An unexpected 5xx, a dropped
// connection or a hang is an anomaly, and so is a method an API route does
// not serve answered 2xx: POST, PUT, DELETE or PATCH on a read-only route,
// any method but OPTIONS (a CORS preflight) on a POST-only one. Outside
// /api/ Kula serves its page, assets, /health and /status to any method,
// which changes nothing: those 2xx answers are counted as information only.
func runFuzzMethods(s *Scanner) []Finding {
	origin := s.base.Scheme + "://" + s.base.Host
	anonymous := map[string]string{"Origin": origin}
	session := maps.Clone(anonymous)
	var notes []string
	if s.authEnabled {
		if s.ensureSession() {
			session["Cookie"] = "kula_session=" + s.session
			session["X-CSRF-Token"] = s.csrf
		} else {
			notes = append(notes, "Without a session the API routes stop at the auth gate: "+s.noSessionReason()+".")
		}
	}

	matrix := methodMatrix()
	t := &fuzzTally{planned: len(matrix)}
	enforced, other := 0, 0
	methodHit := false
	for _, p := range matrix {
		if t.full() || s.ctx.Err() != nil {
			break
		}
		hdr := session
		if p.path == "/api/logout" {
			hdr = anonymous
		}
		r := s.do(p.method, p.path, hdr, "")
		desc := reqDesc(p.method, p.path, "")
		if t.record(r, desc, "").outcome != outReached {
			continue
		}
		accepted := r.status >= 200 && r.status <= 299
		switch {
		case p.kind == routeReadOnly && stateChanging(p.method), p.kind == routePostOnly && p.method != http.MethodOptions:
			enforced++
			if accepted {
				methodHit = true
				t.hit(fmt.Sprintf("an API route accepted a method it does not serve (HTTP %d)", r.status), desc)
			}
		case p.kind == routeOther && accepted && p.method != http.MethodHead && p.method != http.MethodOptions:
			other++
		}
	}

	if !methodHit && enforced > 0 {
		notes = append(notes, fmt.Sprintf("Method enforcement: %d requests with a method their API route does not serve "+
			"(POST, PUT, DELETE or PATCH on a read-only route; another than POST on login, logout or the AI chat) reached the handler, and none was accepted.", enforced))
	}
	if other > 0 {
		notes = append(notes, fmt.Sprintf("For information: %d requests outside /api/ with a method other than GET, HEAD or OPTIONS were answered 2xx; "+
			"Kula serves its page, static assets, /health and /status to any method, which changes nothing.", other))
	}
	f := t.verdict("FUZZ-METHODS", s.seed, t.coverage("a handler"), strings.Join(notes, " "))
	if methodHit {
		f = f.withRemediation("Answer a method a route does not serve with 405, as Kula's readOnly does for its GET-only API routes; " +
			"for the other anomalies, return 4xx, never 5xx, a panic or a hang.")
	}
	return []Finding{f}
}

// runFuzzSmuggle sends three requests to /api/login whose Content-Length and
// Transfer-Encoding headers frame them ambiguously, and says what each was
// answered. The server must refuse them, or handle them without a 2xx or a
// 500 (net/http answers two Content-Lengths 400 and two Transfer-Encodings
// 501 before routing, and reads Content-Length with chunked as chunked).
// That is all one server shows: a desync is a disagreement between a proxy
// and the server behind it on where a request ends. The requests carry no
// Origin, so with origin validation off they reach login's limiter (see
// allChecks).
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
	// What each of raws is, in order, for the detail.
	names := []string{"two Content-Lengths", "Content-Length with chunked Transfer-Encoding", "two Transfer-Encodings"}
	// The -H header lines follow the Host line of what is sent; the evidence
	// quotes the request without them, since their values are secrets.
	hostLine := "\r\nHost: " + host + "\r\n"
	extra := s.rawHeaderLines(nil)
	var hits []fuzzHit
	var lost undelivered
	var answers []string
	for i, raw := range raws {
		status, _, err := s.rawExchange(strings.Replace(raw, hostLine, hostLine+extra, 1))
		if err != nil {
			lost.add(err)
			continue
		}
		answer := fmt.Sprintf("HTTP %d", status)
		if status == 0 {
			answer = "no answer (the connection closed, or nothing came within -timeout)"
		}
		answers = append(answers, names[i]+": "+answer)
		switch {
		case status == 500:
			// A 400/501 rejection is the correct, safe response; only a 500 means
			// the ambiguous framing actually broke a handler.
			hits = append(hits, fuzzHit{"answered HTTP 500", truncate(strings.ReplaceAll(raw, "\r\n", "\\n"), 90)})
		case status >= 200 && status < 300:
			hits = append(hits, fuzzHit{fmt.Sprintf("accepted ambiguous framing (HTTP %d)", status), truncate(strings.ReplaceAll(raw, "\r\n", "\\n"), 90)})
		}
	}
	delivered := len(raws) - lost.n
	observed := fmt.Sprintf("%d of %d ambiguously framed requests to /api/login delivered", delivered, len(raws))
	if len(answers) > 0 {
		observed += ": " + strings.Join(answers, "; ")
	}
	if len(hits) == 0 && lost.n > 0 {
		detail := lost.detail(len(raws), "ambiguously framed requests to /api/login")
		if len(answers) > 0 {
			detail += "; the others: " + strings.Join(answers, "; ")
		}
		return []Finding{finding("FUZZ-SMUGGLE", SevInfo, StatusError, detail+".")}
	}
	if len(hits) == 0 {
		return []Finding{finding("FUZZ-SMUGGLE", SevMedium, StatusPass,
			observed+". None was accepted with a 2xx or answered HTTP 500. That is how this server reads the framing; "+
				"a desync is a disagreement between a proxy and the server behind it, which one server's answers cannot show.")}
	}
	t := &fuzzTally{planned: len(raws), sent: delivered}
	note := ""
	for _, h := range hits {
		t.hit(h.kind, h.req)
		if strings.HasPrefix(h.kind, "accepted") {
			note = "Behind a proxy that reads such framing otherwise, a server that accepts it would not agree with the proxy on where a request ends."
		}
	}
	return []Finding{t.verdict("FUZZ-SMUGGLE", s.seed, observed, note)}
}

// FUZZ-WS opens fuzzWSConns connections one after another, each closed
// before the next (Kula admits 5 per address by default), and sends up to
// fuzzWSFrames junk frames on each.
const (
	fuzzWSConns  = 6
	fuzzWSFrames = 8
)

// runFuzzWS opens authenticated, same-origin WebSockets and floods them with
// malformed and binary frames plus rapid pause/resume churn, then confirms the
// server is still serving new WebSocket connections and HTTP requests. It
// says how many connections it opened and how many frames it sent on them;
// when it could open none, it fuzzed nothing and does not pass.
func runFuzzWS(s *Scanner) []Finding {
	hdr := http.Header{}
	if s.authEnabled {
		if !s.ensureSession() {
			return []Finding{finding("FUZZ-WS", SevInfo, StatusSkip,
				"auth is enabled and no session is available to open a WebSocket: "+s.noSessionReason()+".")}
		}
		hdr.Set("Cookie", "kula_session="+s.session)
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
	opened, frames, refused, throttled := 0, 0, 0, 0
	firstRefusal := ""
	for i := 0; i < fuzzWSConns && s.ctx.Err() == nil; i++ {
		conn, resp, err := s.wsDial("/ws", hdr)
		if err != nil || conn == nil {
			refused++
			if resp != nil && resp.StatusCode == http.StatusTooManyRequests {
				throttled++
			}
			if firstRefusal == "" {
				firstRefusal = upgradeFailure(resp, err)
			}
			continue
		}
		opened++
		for r := 0; r < fuzzWSFrames; r++ {
			_ = conn.SetWriteDeadline(time.Now().Add(s.timeout))
			if err := junk[s.randIntn(len(junk))](conn); err != nil {
				break // server closed the connection on bad input — acceptable
			}
			frames++
		}
		_ = conn.Close()
	}
	coverage := fmt.Sprintf("%d connections planned, %d opened; %d frames planned on them, %d sent",
		fuzzWSConns, opened, opened*fuzzWSFrames, frames)
	if refused > 0 {
		coverage += fmt.Sprintf("; %d upgrades refused (the first: %s)", refused, firstRefusal)
	}
	switch {
	case opened == 0 && throttled == refused && throttled > 0:
		return []Finding{finding("FUZZ-WS", SevInfo, StatusSkip,
			"no WebSocket connection could be opened, the WebSocket connection limit refused every upgrade (retry once other connections close): "+coverage+".")}
	case opened == 0:
		return []Finding{finding("FUZZ-WS", SevInfo, StatusError,
			"no WebSocket connection could be opened, so no frame was sent: "+coverage+".")}
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
			"after a burst of malformed WebSocket frames, HTTP /health stopped responding — the server was destabilized: "+coverage+".").
			withEvidence("health err=%v status=%d", live.err, live.status).
			withRemediation("Ensure the WebSocket read loop tolerates any client input without panicking or blocking the server.")}
	case !wsOK:
		return []Finding{finding("FUZZ-WS", SevHigh, StatusWarn,
			"HTTP still works, but a fresh WebSocket upgrade failed after the fuzz burst (possible connection-slot leak): "+coverage+".").
			withEvidence("ws dial err=%v", err).
			withRemediation("Verify failed/garbage connections are always unregistered and their slots released.")}
	case opened*2 < fuzzWSConns:
		return []Finding{finding("FUZZ-WS", SevInfo, StatusWarn,
			"no anomalies, but fewer than half of the planned connections opened: "+coverage+"; HTTP and a new WebSocket upgrade still work after them.")}
	default:
		return []Finding{finding("FUZZ-WS", SevInfo, StatusPass,
			"no anomalies: "+coverage+"; the server tolerated the malformed and binary frames and the churn, and HTTP and a new WebSocket upgrade still work after them.")}
	}
}

// upgradeFailure says how a WebSocket upgrade failed, for a detail: the
// status the target refused it with, or the error.
func upgradeFailure(resp *http.Response, err error) string {
	switch {
	case resp != nil && resp.StatusCode == http.StatusTooManyRequests:
		return "HTTP 429, the WebSocket connection limit"
	case resp != nil:
		return fmt.Sprintf("HTTP %d", resp.StatusCode)
	case err != nil:
		return truncate(printable(err.Error()), 160)
	}
	return "no connection"
}

// runFuzzLiveness is the final probe: after the whole fuzz barrage, and the
// probes that run after it, the instance must still be alive and serving. It
// runs last in the registry.
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
