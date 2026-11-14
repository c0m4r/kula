package main

import (
	"net/http"
	"strings"

	"github.com/gorilla/websocket"
)

// Disruptive probes. Each has real side effects on the live target — login
// lockout, consumed WebSocket slots — so they only run with -aggressive. They
// are ordered so the login rate-limit flood (which locks out login for ~5 min)
// runs last, after every probe that needs to authenticate.

func aggressiveChecks() []check {
	return []check{
		{id: "INPUT-AGG", category: "input", aggressive: true, run: runOversizedBodyCheck},
		{id: "RATE-OLLAMA", category: "rate", aggressive: true, run: runOllamaRateCheck},
		{id: "WS-FLOOD", category: "ws", aggressive: true, run: runWSFloodCheck},
		{id: "RATE-LOGIN", category: "rate", aggressive: true, run: runLoginRateCheck},
	}
}

// runOversizedBodyCheck verifies the login body size cap (MaxBytesReader, 4 KiB)
// rejects an oversized payload instead of buffering it. SKIP when auth is
// disabled: Kula then answers /api/login with 404 and reads no body.
func runOversizedBodyCheck(s *Scanner) []Finding {
	if !s.authEnabled {
		return []Finding{finding("INPUT-AGG", "input", "Login body size cap", SevInfo, StatusSkip,
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
		return []Finding{finding("INPUT-AGG", "input", "Login body size cap", SevInfo, StatusError,
			"could not send oversized login body: "+resp.err.Error())}
	}
	switch resp.status {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		return []Finding{finding("INPUT-AGG", "input", "Login body size cap", SevLow, StatusPass,
			"an 8 KiB login body is rejected (the 4 KiB MaxBytesReader cap holds).").withEvidence("status=%d", resp.status)}
	case http.StatusTooManyRequests:
		return []Finding{finding("INPUT-AGG", "input", "Login body size cap", SevInfo, StatusSkip,
			"login was already rate-limited; re-run later to test the body cap.")}
	default:
		return []Finding{finding("INPUT-AGG", "input", "Login body size cap", SevMedium, StatusWarn,
			"an oversized login body was not rejected with 400/413").withEvidence("status=%d", resp.status).
			withRemediation("Wrap the login body in http.MaxBytesReader to bound memory use.")}
	}
}

// runLoginRateCheck fires a burst of bad logins from this IP and expects the
// rate limiter to start answering 429. NOTE: this locks out login from the
// scanner's IP for ~5 minutes. SKIP when auth is disabled: there is no login
// endpoint to throttle.
func runLoginRateCheck(s *Scanner) []Finding {
	if !s.authEnabled {
		return []Finding{finding("RATE-LOGIN", "rate", "Login brute-force rate limiting", SevInfo, StatusSkip,
			"auth is disabled; the instance has no login endpoint to brute-force.")}
	}
	const burst = 8
	limited := 0
	for i := 0; i < burst; i++ {
		r := s.loginRaw("ratelimit-probe", "wrong-"+randToken(4))
		if r.err != nil {
			continue
		}
		if r.status == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited > 0 {
		return []Finding{finding("RATE-LOGIN", "rate", "Login brute-force rate limiting", SevHigh, StatusPass,
			"the login endpoint starts returning 429 within a burst of failed attempts (brute force is throttled).").
			withEvidence("%d/%d attempts were rate-limited", limited, burst)}
	}
	return []Finding{finding("RATE-LOGIN", "rate", "Login brute-force rate limiting", SevHigh, StatusFail,
		"a burst of failed logins was never rate-limited; the login endpoint is brute-forceable.").
		withEvidence("0/%d attempts returned 429", burst).
		withRemediation("Confirm the per-IP/per-username login limiter is active (see web auth RateLimiter).")}
}

// runOllamaRateCheck floods the Ollama meta endpoint and expects 429s. SKIP when
// Ollama is not enabled on the target.
func runOllamaRateCheck(s *Scanner) []Finding {
	if !s.ollama {
		return []Finding{finding("RATE-OLLAMA", "rate", "Ollama rate limiting", SevInfo, StatusSkip,
			"Ollama integration is not enabled on this instance.")}
	}
	hdr, ok := s.apiHeaders()
	if !ok {
		return []Finding{finding("RATE-OLLAMA", "rate", "Ollama rate limiting", SevInfo, StatusSkip,
			"auth is enabled and no session is available to reach the Ollama endpoints.")}
	}
	const burst = 70 // ollamaMetaRateLimit is 60/min
	limited := 0
	for i := 0; i < burst; i++ {
		r := s.do(http.MethodGet, "/api/ollama/models", hdr, "")
		if r.err == nil && r.status == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited > 0 {
		return []Finding{finding("RATE-OLLAMA", "rate", "Ollama rate limiting", SevMedium, StatusPass,
			"the Ollama meta endpoint starts returning 429 under a burst.").withEvidence("%d/%d rate-limited", limited, burst)}
	}
	return []Finding{finding("RATE-OLLAMA", "rate", "Ollama rate limiting", SevMedium, StatusWarn,
		"a burst of Ollama requests was never rate-limited.").withEvidence("0/%d returned 429", burst).
		withRemediation("Confirm the per-IP Ollama rate limiter is active.")}
}

// runWSFloodCheck opens more concurrent WebSocket connections than the per-IP
// limit and expects the upgrade to start being rejected with 429. All opened
// connections are closed before returning.
func runWSFloodCheck(s *Scanner) []Finding {
	var hdr http.Header
	if s.authEnabled {
		if !s.ensureSession() {
			return []Finding{finding("WS-FLOOD", "ws", "WebSocket per-IP connection limit", SevInfo, StatusSkip,
				"auth is enabled and no session is available to open WebSocket connections.")}
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
	for i := 0; i < attempts; i++ {
		dh := cloneHeader(hdr)
		dh.Set("Origin", s.base.Scheme+"://"+s.base.Host)
		conn, resp, err := s.wsDial("/ws", dh)
		if err == nil && conn != nil {
			open = append(open, conn)
			continue
		}
		if resp != nil && resp.StatusCode == http.StatusTooManyRequests {
			limited++
		}
	}

	if limited > 0 {
		return []Finding{finding("WS-FLOOD", "ws", "WebSocket per-IP connection limit", SevMedium, StatusPass,
			"opening more than the per-IP limit of WebSocket connections is rejected with 429.").
			withEvidence("%d/%d upgrades rejected with 429, %d held open", limited, attempts, len(open))}
	}
	return []Finding{finding("WS-FLOOD", "ws", "WebSocket per-IP connection limit", SevMedium, StatusWarn,
		"all WebSocket upgrades succeeded; no per-IP connection cap was observed.").
		withEvidence("%d connections opened without a 429", len(open)).
		withRemediation("Set web.max_websocket_conns_per_ip to bound connections per client.")}
}
