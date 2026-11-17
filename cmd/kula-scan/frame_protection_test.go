package main

import (
	"net/http"
	"strings"
	"testing"
)

// kulaHeaders serves Kula with auth off as securityMiddleware answers with
// web.security.headers on, and frame protection on or off: off drops
// X-Frame-Options and the CSP's frame-ancestors, nothing else.
func kulaHeaders(frameProtection bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		csp := kulaCSP(randToken(8))
		if frameProtection {
			h.Set("X-Frame-Options", "DENY")
		} else {
			csp = strings.TrimSuffix(csp, " frame-ancestors 'none';")
		}
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", csp)
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
		h.Set("Cache-Control", "no-store")
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte("kula is healthy"))
		case "/api/auth/status":
			writeJSONResp(w, map[string]any{"auth_required": false})
		case "/api/config":
			writeJSONResp(w, map[string]any{"auth_enabled": false, "ollama_enabled": false})
		default:
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html><body>kula</body></html>"))
		}
	})
}

// TestFrameOptionsJudgement: X-Frame-Options missing beside the other
// security headers is the frame_protection opt-out, a WARN against
// frame-protection; missing with them, or not DENY, is a FAIL.
func TestFrameOptionsJudgement(t *testing.T) {
	kula := func(drop ...string) http.Header {
		h := http.Header{}
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", kulaCSP("n"))
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Permissions-Policy", "geolocation=()")
		for _, name := range drop {
			h.Del(name)
		}
		return h
	}
	sameOrigin := kula()
	sameOrigin.Set("X-Frame-Options", "SAMEORIGIN")
	for _, c := range []struct {
		name   string
		header http.Header
		want   Status
		tagged bool
	}{
		{"frame protection on", kula(), StatusPass, false},
		{"frame protection off", kula("X-Frame-Options"), StatusWarn, true},
		{"security headers off", http.Header{}, StatusFail, false},
		{"Permissions-Policy missing too", kula("X-Frame-Options", "Permissions-Policy"), StatusFail, false},
		{"no CSP either", kula("X-Frame-Options", "Content-Security-Policy"), StatusFail, false},
		{"SAMEORIGIN", sameOrigin, StatusFail, false},
	} {
		f := frameOptionsFinding(c.header)
		if f.ID != "HDR-002" || f.Status != c.want || (f.Expectation == "frame-protection") != c.tagged {
			t.Errorf("%s: %s = %s, expectation %q; want %s, tagged %v", c.name, f.ID, f.Status, f.Expectation, c.want, c.tagged)
		}
	}
}

// TestFrameProtectionOff: a default Kula with web.security.frame_protection
// off gets two WARNs and exits 0; -expect frame-protection fails the run on
// both.
func TestFrameProtectionOff(t *testing.T) {
	code, report, stderr := scanJSON(t, kulaHeaders(false), "-only", "headers")
	if code != exitClean {
		t.Errorf("frame protection off: exit %d, want 0; stderr:\n%s", code, stderr)
	}
	for _, id := range []string{"HDR-002", "HDR-007"} {
		wantStatus(t, report.Findings, id, StatusWarn)
	}
	for _, f := range report.Findings {
		if f.Status == StatusFail || f.Status == StatusError {
			t.Errorf("frame protection off: %s = %s %q", f.ID, f.Status, f.Detail)
		}
	}

	code, report, stderr = scanJSON(t, kulaHeaders(false), "-only", "headers", "-expect", "frame-protection")
	if code != exitFailing || !strings.Contains(stderr, "expected posture not met: frame-protection (HDR-002, HDR-007)") {
		t.Errorf("-expect frame-protection: exit %d, want 1 naming HDR-002 and HDR-007; stderr:\n%s", code, stderr)
	}
	if f, _ := byID(report.Findings, "HDR-002"); f.Status != StatusFail || f.Expectation != "frame-protection" {
		t.Errorf("-expect frame-protection: HDR-002 = %s, expectation %q", f.Status, f.Expectation)
	}

	// Frame protection on meets the expectation.
	if code, _, stderr := scanJSON(t, kulaHeaders(true), "-only", "headers", "-expect", "frame-protection"); code != exitClean {
		t.Errorf("frame protection on with -expect frame-protection: exit %d; stderr:\n%s", code, stderr)
	}
}
