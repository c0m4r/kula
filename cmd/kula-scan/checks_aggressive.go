package main

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/gorilla/websocket"
)

// Disruptive probes. Each has real side effects on the live target — login
// lockout, consumed WebSocket slots — so they only run with -aggressive. The
// login rate-limit flood (runLoginRateCheck, which locks out login for ~5 min)
// is not among aggressiveChecks: allChecks registers it near the end of the
// run, after every probe that needs to authenticate.

func aggressiveChecks() []check {
	return []check{
		{ids: []string{"INPUT-AGG"}, run: runOversizedBodyCheck},
		{ids: []string{"RATE-OLLAMA"}, run: runOllamaRateCheck},
		{ids: []string{"WS-FLOOD"}, run: runWSFloodCheck},
	}
}

// runOversizedBodyCheck verifies the login body size cap (MaxBytesReader, 4 KiB)
// rejects an oversized payload instead of buffering it. SKIP when auth is
// disabled: Kula then answers /api/login with 404 and reads no body.
func runOversizedBodyCheck(s *Scanner) []Finding {
	if !s.authEnabled {
		return []Finding{finding("INPUT-AGG", SevInfo, StatusSkip,
			"auth is disabled; the instance has no login endpoint to send a body to.")}
	}
	big := strings.Repeat("A", 8*1024)
	body := `{"username":"x","password":"` + big + `"}`
	hdr := map[string]string{
		"Content-Type": "application/json",
		"Origin":       s.base.Scheme + "://" + s.base.Host,
	}
	resp := s.do(http.MethodPost, "/api/login", hdr, body)
	if resp.err != nil {
		return []Finding{finding("INPUT-AGG", SevInfo, StatusError,
			"could not send oversized login body: "+resp.err.Error())}
	}
	switch resp.status {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		return []Finding{finding("INPUT-AGG", SevLow, StatusPass,
			"an 8 KiB login body is rejected (the 4 KiB MaxBytesReader cap holds).").withEvidence("status=%d", resp.status)}
	case http.StatusTooManyRequests:
		return []Finding{finding("INPUT-AGG", SevInfo, StatusSkip,
			"login was already rate-limited; re-run later to test the body cap.")}
	default:
		return []Finding{finding("INPUT-AGG", SevMedium, StatusWarn,
			"an oversized login body was not rejected with 400/413").withEvidence("status=%d", resp.status).
			withRemediation("Wrap the login body in http.MaxBytesReader to bound memory use.")}
	}
}

// tally counts the outcomes of a burst of requests: the answers by status,
// and the requests that got none, with the first error. A probe judges a
// safeguard only on answers: requests that failed prove nothing either way.
type tally struct {
	total    int
	statuses map[int]int
	failed   int
	firstErr error
}

func (t *tally) add(r httpResult) {
	t.total++
	if r.err != nil {
		if t.failed == 0 {
			t.firstErr = r.err
		}
		t.failed++
		return
	}
	if t.statuses == nil {
		t.statuses = map[int]int{}
	}
	t.statuses[r.status]++
}

// String renders the tally for evidence: "5× HTTP 401, 3× HTTP 429, 1 failed (…)".
func (t *tally) String() string {
	var parts []string
	for _, code := range slices.Sorted(maps.Keys(t.statuses)) {
		parts = append(parts, fmt.Sprintf("%d× HTTP %d", t.statuses[code], code))
	}
	if t.failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed (%v)", t.failed, t.firstErr))
	}
	if len(parts) == 0 {
		return "no requests sent"
	}
	return strings.Join(parts, ", ")
}

// runLoginRateCheck fires a burst of bad logins from this IP and expects the
// rate limiter to start answering 429. NOTE: this locks out login from the
// scanner's IP for ~5 minutes. SKIP when auth is disabled: there is no login
// endpoint to throttle. A 429 is the pass; every attempt reaching the
// credential check (401) is the finding; anything else says nothing of the
// limiter and leaves the check an ERROR.
func runLoginRateCheck(s *Scanner) []Finding {
	if !s.authEnabled {
		return []Finding{finding("RATE-LOGIN", SevInfo, StatusSkip,
			"auth is disabled; the instance has no login endpoint to brute-force.")}
	}
	const burst = 8
	var t tally
	for i := 0; i < burst && s.ctx.Err() == nil; i++ {
		t.add(s.loginRaw("ratelimit-probe", "wrong-"+randToken(4)))
	}
	limited, reached := t.statuses[http.StatusTooManyRequests], t.statuses[http.StatusUnauthorized]
	switch {
	case limited > 0:
		return []Finding{finding("RATE-LOGIN", SevHigh, StatusPass,
			"the login endpoint starts returning 429 within a burst of failed attempts (brute force is throttled).").
			withEvidence("%d/%d attempts were rate-limited: %s", limited, burst, &t)}
	case reached == burst:
		return []Finding{finding("RATE-LOGIN", SevHigh, StatusFail,
			"a burst of failed logins was never rate-limited; the login endpoint is brute-forceable.").
			withEvidence("0/%d attempts returned 429: %s", burst, &t).
			withRemediation("Confirm the per-IP/per-username login limiter is active (see web auth RateLimiter).")}
	case t.failed == t.total:
		return []Finding{finding("RATE-LOGIN", SevInfo, StatusError,
			fmt.Sprintf("every login attempt of the burst failed, so the limiter was not exercised: %v", t.firstErr))}
	default:
		return []Finding{finding("RATE-LOGIN", SevInfo, StatusError,
			fmt.Sprintf("no attempt was rate-limited, but only %d of %d reached the credential check (HTTP 401), so the burst says nothing of the limiter.", reached, burst)).
			withEvidence("%s", &t)}
	}
}

// runOllamaRateCheck floods the Ollama meta endpoint and expects 429s. SKIP when
// Ollama is not enabled on the target. Behind auth it needs the session, which
// is also what reads whether Ollama is enabled (assistantEnabled).
func runOllamaRateCheck(s *Scanner) []Finding {
	hdr, ok := s.apiHeaders()
	if !ok {
		return []Finding{finding("RATE-OLLAMA", SevInfo, StatusSkip,
			"auth is enabled and no session is available to reach the Ollama endpoints: "+s.noSessionReason()+".")}
	}
	switch enabled, st, why := s.assistantEnabled(); {
	case st != StatusPass:
		return []Finding{finding("RATE-OLLAMA", SevInfo, st, why+".")}
	case !enabled:
		return []Finding{finding("RATE-OLLAMA", SevInfo, StatusSkip,
			"Ollama integration is not enabled on this instance.")}
	}
	const burst = 70 // ollamaMetaRateLimit is 60/min
	var t tally
	for i := 0; i < burst && s.ctx.Err() == nil; i++ {
		t.add(s.do(http.MethodGet, "/api/ollama/models", hdr, ""))
	}
	limited := t.statuses[http.StatusTooManyRequests]
	switch {
	case limited > 0:
		return []Finding{finding("RATE-OLLAMA", SevMedium, StatusPass,
			"the Ollama meta endpoint starts returning 429 under a burst.").withEvidence("%d/%d rate-limited: %s", limited, burst, &t)}
	case t.failed == 0:
		return []Finding{finding("RATE-OLLAMA", SevMedium, StatusWarn,
			"a burst of Ollama requests was never rate-limited.").withEvidence("0/%d returned 429: %s", burst, &t).
			withRemediation("Confirm the per-IP Ollama rate limiter is active.")}
	default:
		return []Finding{finding("RATE-OLLAMA", SevInfo, StatusError,
			fmt.Sprintf("%d of %d requests of the burst failed and none of the others was rate-limited, so the burst says nothing of the limiter.", t.failed, burst)).
			withEvidence("%s", &t)}
	}
}

// wsLimitAnswered says which of Kula's two WebSocket caps refused an upgrade
// with 429, as the error in its JSON body names it, or "" when the body does
// not say. gorilla keeps the first KiB of a refused handshake's body.
func wsLimitAnswered(resp *http.Response) string {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return ""
	}
	var answer struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &answer) != nil {
		return ""
	}
	switch {
	case strings.HasPrefix(answer.Error, "per-IP"):
		return "per-address"
	case strings.HasPrefix(answer.Error, "global"):
		return "global"
	}
	return ""
}

// runWSFloodCheck opens more concurrent WebSocket connections than the per-IP
// limit and expects the upgrade to start being rejected with 429. All opened
// connections are closed before returning. Kula answers past either of its
// caps, per address (web.max_websocket_conns_per_ip) and for all clients
// (web.max_websocket_conns), and names the one in the body.
func runWSFloodCheck(s *Scanner) []Finding {
	var hdr http.Header
	if s.authEnabled {
		if !s.ensureSession() {
			return []Finding{finding("WS-FLOOD", SevInfo, StatusSkip,
				"auth is enabled and no session is available to open WebSocket connections: "+s.noSessionReason()+".")}
		}
		hdr = http.Header{}
		hdr.Set("Cookie", "kula_session="+s.session)
	}

	const attempts = 12 // default per-IP limit is 5
	var open []*websocket.Conn
	defer func() {
		for _, c := range open {
			_ = c.Close()
		}
	}()

	limited := 0
	byLimit := map[string]int{} // 429s by the cap their body names
	var other []string          // upgrades that neither succeeded nor got a 429
	for i := 0; i < attempts && s.ctx.Err() == nil; i++ {
		dh := cloneHeader(hdr)
		dh.Set("Origin", s.base.Scheme+"://"+s.base.Host)
		conn, resp, err := s.wsDial("/ws", dh)
		switch {
		case err == nil && conn != nil:
			open = append(open, conn)
		case resp != nil && resp.StatusCode == http.StatusTooManyRequests:
			limited++
			byLimit[wsLimitAnswered(resp)]++
		case resp != nil:
			other = append(other, statusOf(resp))
		default:
			other = append(other, err.Error())
		}
	}

	switch {
	case limited > 0:
		var answered []string
		for _, limit := range []string{"per-address", "global"} {
			if n := byLimit[limit]; n > 0 {
				answered = append(answered, fmt.Sprintf("%d by the %s limit", n, limit))
			}
		}
		if n := byLimit[""]; n > 0 {
			answered = append(answered, fmt.Sprintf("%d naming no limit", n))
		}
		detail := "opening more than the per-IP limit of WebSocket connections is rejected with 429."
		if byLimit["global"] > 0 && byLimit["per-address"] == 0 {
			detail = "WebSocket upgrades past a cap are rejected with 429, but by the global limit (web.max_websocket_conns), not the per-address one: other clients hold connections, or the global limit is at or below the per-address limit."
		}
		return []Finding{finding("WS-FLOOD", SevMedium, StatusPass, detail).
			withEvidence("%d/%d upgrades rejected with 429 (%s), %d held open", limited, attempts, strings.Join(answered, ", "), len(open))}
	case len(open) == 0:
		return []Finding{finding("WS-FLOOD", SevInfo, StatusError,
			"no WebSocket upgrade succeeded and none was rejected with 429, so no connection cap could be observed.").
			withEvidence("%s", strings.Join(other, "; "))}
	case len(other) > 0:
		return []Finding{finding("WS-FLOOD", SevInfo, StatusError,
			fmt.Sprintf("%d upgrades succeeded and %d failed without a 429, so whether a cap holds is not known.", len(open), len(other))).
			withEvidence("%s", strings.Join(other, "; "))}
	default:
		return []Finding{finding("WS-FLOOD", SevMedium, StatusWarn,
			"all WebSocket upgrades succeeded; no per-IP connection cap was observed.").
			withEvidence("%d connections opened without a 429", len(open)).
			withRemediation("Set web.max_websocket_conns_per_ip to bound connections per client.")}
	}
}
