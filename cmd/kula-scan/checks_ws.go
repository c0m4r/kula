package main

import (
	"net/http"
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

// foreignOrigin is the Origin the cross-origin upgrade probe sends.
const foreignOrigin = "https://evil.example"

func runWSChecks(s *Scanner) []Finding {
	var out []Finding
	sameOrigin := s.base.Scheme + "://" + s.base.Host

	// WS-001: an unauthenticated upgrade is rejected when auth is enabled.
	if s.authEnabled {
		conn, resp, err := s.wsDial("/ws", nil)
		if conn != nil {
			_ = conn.Close()
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
			"auth is enabled and no session is available; supply -username/-password to test the WebSocket origin gate."))
		return out
	}

	foreign := cloneHeader(hdr)
	foreign.Set("Origin", foreignOrigin)
	conn, resp, err := s.wsDial("/ws", foreign)
	if conn != nil {
		_ = conn.Close()
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
	cc, resp, err := s.wsDial("/ws", same)
	if cc != nil {
		_ = cc.Close()
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
