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
		case resp != nil && resp.StatusCode == http.StatusUnauthorized:
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
	foreign.Set("Origin", "https://evil.example")
	conn, resp, err := s.wsDial("/ws", foreign)
	if conn != nil {
		_ = conn.Close()
	}
	switch {
	case err == nil:
		out = append(out, finding("WS-002", SevHigh, StatusFail,
			"a cross-origin WebSocket upgrade from https://evil.example succeeded; a malicious page can hijack the live stream.").
			withRemediation("Keep web.security.origin_validation enabled so CheckOrigin rejects foreign origins."))
	case resp != nil && resp.StatusCode == http.StatusForbidden:
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
	cc, _, err := s.wsDial("/ws", same)
	if cc != nil {
		_ = cc.Close()
	}
	if err == nil {
		out = append(out, finding("WS-000", SevInfo, StatusPass,
			"a legitimate same-origin upgrade succeeds (the gate is selective, not blanket-deny)."))
	} else {
		out = append(out, finding("WS-000", SevInfo, StatusWarn,
			"a same-origin upgrade was rejected; the gate may be over-broad or the stream disabled.").
			withEvidence("err=%v", err))
	}

	return out
}
