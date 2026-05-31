package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Bypass / evasion probes:
//   - open redirect: can a crafted path make Kula redirect to a foreign host?
//   - X-Forwarded-For rate-limit evasion: can spoofed proxy headers defeat the
//     login throttle (trust_proxy enabled without a real trusted proxy)?

// ---- open redirect (safe, non-aggressive) -----------------------------------

func redirectChecks() []check {
	return []check{{id: "REDIR-001", category: "redirect", run: runOpenRedirectCheck}}
}

// sameHostname reports whether the Location host refers to the same host as the
// scan target (ignoring port). An empty host means a relative redirect (safe).
func (s *Scanner) sameHostname(locHost string) bool {
	if locHost == "" {
		return true
	}
	strip := func(h string) string {
		if i := strings.IndexByte(h, ':'); i >= 0 {
			return h[:i]
		}
		return h
	}
	return strings.EqualFold(strip(locHost), strip(s.base.Host))
}

func runOpenRedirectCheck(s *Scanner) []Finding {
	// Paths that have historically tricked naive redirect helpers into sending
	// the browser to an attacker host.
	payloads := []string{
		"//evil.example/", "/%2f%2fevil.example/", "/%09/evil.example",
		"/https://evil.example", "https:/evil.example", "/.evil.example",
		"/%20//evil.example", "/..//evil.example", "//evil.example/%2e%2e",
		"/%5c%5cevil.example", "/%5c/evil.example",
	}
	if s.base.Path != "" {
		// The base-path redirect (bare prefix -> prefix+"/") is a known redirecting
		// code path; probe variants around it too.
		payloads = append(payloads, s.base.Path+"//evil.example", "//evil.example"+s.base.Path)
	}

	var bad []string
	for _, p := range payloads {
		r := s.do(http.MethodGet, p, nil, "")
		if r.err != nil || r.status < 300 || r.status >= 400 {
			continue
		}
		loc := r.header.Get("Location")
		if loc == "" {
			continue
		}
		u, err := url.Parse(loc)
		if err != nil {
			continue
		}
		if !s.sameHostname(u.Host) {
			bad = append(bad, fmt.Sprintf("%s -> %d Location: %s", p, r.status, loc))
		}
	}

	if len(bad) == 0 {
		return []Finding{finding("REDIR-001", "redirect", "No open redirect", SevMedium, StatusPass,
			"crafted paths do not produce a redirect to a foreign host.")}
	}
	return []Finding{finding("REDIR-001", "redirect", "Open redirect", SevHigh, StatusFail,
		"a crafted path made the server emit a redirect to an attacker-controlled host (phishing / token leakage vector).").
		withEvidence("%s", strings.Join(bad, " | ")).
		withRemediation("Only ever redirect to validated, same-host, absolute-path targets; reject '//', '\\', and scheme-bearing inputs.")}
}

// ---- X-Forwarded-For rate-limit bypass (aggressive) -------------------------

func bypassChecks() []check {
	return []check{{id: "BYPASS-XFF", category: "bypass", aggressive: true, run: runXFFBypassCheck}}
}

func (s *Scanner) randIP() string {
	return fmt.Sprintf("%d.%d.%d.%d", 1+s.randIntn(223), s.randIntn(256), s.randIntn(256), 1+s.randIntn(254))
}

// runXFFBypassCheck fires a burst of failed logins, each carrying a different
// spoofed X-Forwarded-For. If Kula keys its login rate limiter on that header
// (trust_proxy enabled with no real proxy in front), every request lands on a
// fresh key and the throttle never fires — a brute-force bypass.
func runXFFBypassCheck(s *Scanner) []Finding {
	if !s.authEnabled {
		return []Finding{finding("BYPASS-XFF", "bypass", "X-Forwarded-For rate-limit bypass", SevInfo, StatusSkip,
			"auth is disabled; the login rate limiter is not a relevant control here.")}
	}

	const burst = 12
	origin := s.base.Scheme + "://" + s.base.Host
	reached := 0 // requests that reached the credential check (401) rather than being throttled (429)
	for i := 0; i < burst; i++ {
		hdr := map[string]string{
			"Content-Type":    "application/json",
			"Origin":          origin,
			"X-Forwarded-For": s.randIP(),
		}
		r := s.do(http.MethodPost, "/api/login", hdr, `{"username":"xff-probe","password":"nope"}`)
		if r.err != nil {
			continue
		}
		if r.status == http.StatusUnauthorized {
			reached++
		}
	}

	switch {
	case reached >= burst-1:
		return []Finding{finding("BYPASS-XFF", "bypass", "X-Forwarded-For rate-limit bypass", SevHigh, StatusFail,
			"rotating a spoofed X-Forwarded-For defeated the login rate limiter — an attacker can brute-force credentials unthrottled.").
			withEvidence("%d/%d spoofed-IP attempts reached the credential check without throttling", reached, burst).
			withRemediation("Disable web.trust_proxy unless a trusted reverse proxy actually sets X-Forwarded-For; otherwise the header is attacker-controlled.")}
	case reached <= 6:
		return []Finding{finding("BYPASS-XFF", "bypass", "X-Forwarded-For rate-limit bypass", SevHigh, StatusPass,
			"spoofing X-Forwarded-For did not bypass the login throttle (the limiter keys on the real connection, not the header).").
			withEvidence("only %d/%d spoofed-IP attempts got through before throttling", reached, burst)}
	default:
		return []Finding{finding("BYPASS-XFF", "bypass", "X-Forwarded-For rate-limit bypass", SevMedium, StatusWarn,
			"results were ambiguous; the login limiter may have been partially consumed before this probe.").
			withEvidence("%d/%d spoofed-IP attempts reached the credential check", reached, burst).
			withRemediation("Re-run in isolation (-only bypass) to confirm.")}
	}
}
