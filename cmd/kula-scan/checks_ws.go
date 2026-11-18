package main

import (
	"errors"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

// WebSocket safeguard probes: the /ws upgrade must require authentication (when
// auth is enabled) and must reject cross-origin upgrades (Cross-Site WebSocket
// Hijacking, CSWSH) regardless of auth.

func wsChecks() []check {
	return []check{{
		ids: []string{"WS-000", "WS-001", "WS-002"},
		run: runWSChecks,
	}}
}

// wsRetryWait is how long wsDialRetry waits before it tries an upgrade
// refused with 429 again. It is a var so tests can shorten it.
var wsRetryWait = 500 * time.Millisecond

// wsCloseWait bounds how long closeWS waits for the target to end a
// connection; -timeout bounds it too, when shorter. wsCloseMessages bounds
// how many messages it reads past, those the target sent before it saw the
// close frame: Kula sends one a second.
const (
	wsCloseWait     = 2 * time.Second
	wsCloseMessages = 16
)

// wsDialRetry is wsDial with one more try when the target refuses the
// upgrade with 429, as Kula does past its per-address and global connection
// limits: the place may still be held by a connection the scanner has just
// closed, which the target, or a proxy in front of it, has not yet let go
// of. It waits wsRetryWait before the second try; an interrupt ends the
// wait, and the first refusal is returned.
func (s *Scanner) wsDialRetry(path string, headers http.Header) (*websocket.Conn, *http.Response, error) {
	conn, resp, err := s.wsDial(path, headers)
	if conn != nil || resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		return conn, resp, err
	}
	timer := time.NewTimer(wsRetryWait)
	defer timer.Stop()
	select {
	case <-s.ctx.Done():
		return conn, resp, err
	case <-timer.C:
	}
	return s.wsDial(path, headers)
}

// closeWS ends a WebSocket the scanner is done with as a client should: it
// sends a close frame, reads until the target has answered with its own and
// ended the connection, and closes it. Kula frees the connection's place
// under its per-address limit (web.max_websocket_conns_per_ip) before it
// ends the connection, so the next upgrade does not find that place taken.
// A connection that is merely dropped is let go of later, through a proxy
// noticeably so. The wait is bounded by wsCloseWait, or by -timeout when
// shorter, and an interrupt ends it. A connection whose reading already
// failed, past the scanner's message limit for one, is closed at once.
func (s *Scanner) closeWS(conn *websocket.Conn) {
	deadline := time.Now().Add(min(wsCloseWait, s.timeout))
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), deadline)
	_ = conn.SetReadDeadline(deadline)
	var err error
	for range wsCloseMessages {
		if _, _, err = conn.NextReader(); err != nil {
			break
		}
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		// gorilla reads nothing past the target's close frame; the socket
		// itself shows when the target ends the connection, which comes
		// next.
		raw := conn.NetConn()
		_ = raw.SetReadDeadline(deadline)
		_, _ = raw.Read(make([]byte, 64))
	}
	_ = conn.Close()
}

// foreignOrigin is the Origin the cross-origin upgrade probe sends.
const foreignOrigin = "https://evil.example"

func runWSChecks(s *Scanner) []Finding {
	var out []Finding
	sameOrigin := s.base.Scheme + "://" + s.base.Host

	// WS-001: an unauthenticated upgrade is rejected when auth is enabled.
	if s.authEnabled {
		conn, resp, err := s.wsDialRetry("/ws", nil)
		if conn != nil {
			s.closeWS(conn)
		}
		switch {
		case err == nil:
			out = append(out, finding("WS-001", SevHigh, StatusFail,
				"an unauthenticated WebSocket upgrade succeeded; live metrics stream to anyone.").
				withRemediation("Ensure /ws sits behind AuthMiddleware when auth is enabled."))
		case resp == nil:
			out = append(out, finding("WS-001", SevInfo, StatusError,
				"could not attempt an unauthenticated WebSocket upgrade: "+err.Error()))
		case resp.StatusCode == http.StatusUnauthorized:
			out = append(out, finding("WS-001", SevHigh, StatusPass,
				"the WebSocket upgrade is rejected with 401 without a session."))
		default:
			out = append(out, finding("WS-001", SevMedium, StatusWarn,
				"the unauthenticated upgrade was rejected, but not with 401.").withEvidence("status=%s", statusOf(resp)))
		}
	}

	// WS-002: a cross-origin upgrade is rejected (CSWSH). Needs to get past auth
	// first, so use a valid session when auth is on.
	var hdr http.Header
	canTestOrigin := true
	if s.authEnabled {
		if s.ensureSession() {
			hdr = http.Header{}
			hdr.Set("Cookie", "kula_session="+s.session)
		} else {
			canTestOrigin = false
		}
	}

	if !canTestOrigin {
		out = append(out, finding("WS-002", SevInfo, StatusSkip,
			"auth is enabled and no session is available to test the WebSocket origin gate: "+s.noSessionReason()+"."))
		return out
	}

	foreign := cloneHeader(hdr)
	foreign.Set("Origin", foreignOrigin)
	conn, resp, err := s.wsDialRetry("/ws", foreign)
	if conn != nil {
		s.closeWS(conn)
	}
	switch {
	case err == nil:
		out = append(out, crossOriginUpgradeFinding(s))
	case resp == nil:
		out = append(out, finding("WS-002", SevInfo, StatusError,
			"could not attempt a cross-origin WebSocket upgrade: "+err.Error()))
	case resp.StatusCode == http.StatusForbidden:
		out = append(out, finding("WS-002", SevHigh, StatusPass,
			"a cross-origin WebSocket upgrade is rejected with 403."))
	default:
		out = append(out, finding("WS-002", SevMedium, StatusWarn,
			"the cross-origin upgrade was rejected, but not with 403.").withEvidence("status=%s", statusOf(resp)))
	}

	// WS-000: positive control — a same-origin (authenticated) upgrade succeeds,
	// proving the gate is selective rather than blanket-rejecting everything.
	same := cloneHeader(hdr)
	same.Set("Origin", sameOrigin)
	cc, resp, err := s.wsDialRetry("/ws", same)
	if cc != nil {
		s.closeWS(cc)
	}
	switch {
	case err == nil:
		out = append(out, finding("WS-000", SevInfo, StatusPass,
			"a legitimate same-origin upgrade succeeds (the gate is selective, not blanket-deny)."))
	case resp == nil:
		out = append(out, finding("WS-000", SevInfo, StatusError,
			"could not attempt a same-origin WebSocket upgrade: "+err.Error()))
	default:
		out = append(out, finding("WS-000", SevInfo, StatusWarn,
			"a same-origin upgrade was rejected; the gate may be over-broad or the stream disabled.").
			withEvidence("status=%s; err=%v", statusOf(resp), err))
	}

	return out
}

// crossOriginUpgradeFinding judges a cross-origin upgrade that succeeded by
// what a page on another site can do with it. With auth off, any page opens
// the stream through its visitor's browser. With auth on, the probe got past
// auth with the scanner's own session cookie, which a browser sends with a
// cross-site WebSocket handshake only when the cookie is SameSite=None, or
// has no SameSite attribute (which some browsers treat as None). A Strict or
// Lax cookie keeps other sites out of the session, but then that attribute
// alone does: origin validation is off, which -expect origin-validation
// turns into a FAIL, as it does for CSRF-001 and CSRF-002.
func crossOriginUpgradeFinding(s *Scanner) Finding {
	if !s.authEnabled {
		return finding("WS-002", SevHigh, StatusFail,
			"a cross-origin WebSocket upgrade from "+foreignOrigin+" succeeded with auth off; any website a visitor opens can read the live stream through their browser.").
			withRemediation("Keep web.security.origin_validation enabled so CheckOrigin rejects foreign origins.")
	}
	sameSite, attribute := http.SameSiteDefaultMode, "no SameSite attribute"
	if s.loginCookie != nil {
		sameSite = s.loginCookie.SameSite
	}
	switch sameSite {
	case http.SameSiteStrictMode:
		attribute = "SameSite=Strict"
	case http.SameSiteLaxMode:
		attribute = "SameSite=Lax"
	case http.SameSiteNoneMode:
		attribute = "SameSite=None"
	}
	if sameSite == http.SameSiteStrictMode || sameSite == http.SameSiteLaxMode {
		return finding("WS-002", SevMedium, StatusWarn,
			"a cross-origin WebSocket upgrade from "+foreignOrigin+" succeeded with a session cookie: origin validation is off, and only the cookie's "+attribute+" keeps other sites from opening the live stream with a visitor's session.").
			withEvidence("session cookie: %s", attribute).
			withRemediation("Enable web.security.origin_validation so CheckOrigin rejects foreign origins.").
			expecting("origin-validation")
	}
	return finding("WS-002", SevHigh, StatusFail,
		"a cross-origin WebSocket upgrade from "+foreignOrigin+" succeeded, and the session cookie has "+attribute+", so a browser sends it cross-site: a malicious page can open the live stream with a visitor's session.").
		withEvidence("session cookie: %s", attribute).
		withRemediation("Enable web.security.origin_validation so CheckOrigin rejects foreign origins; with web.security.allowed_origins set, Kula issues SameSite=None cookies, so origin validation is the only guard.")
}
