package main

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// Bypass / evasion probes:
//   - open redirect: can a crafted path make Kula redirect to a foreign host?
//   - X-Forwarded-For rate-limit evasion: can spoofed proxy headers defeat the
//     login throttle (trust_proxy enabled without a real trusted proxy)?

// ---- open redirect (safe, non-aggressive) -----------------------------------

func redirectChecks() []check {
	return []check{{ids: []string{"REDIR-001"}, run: runOpenRedirectCheck}}
}

// sameHostname reports whether host, a host name without a port, names the
// scan target's host (ignoring port, case and a trailing dot).
func (s *Scanner) sameHostname(host string) bool {
	norm := func(h string) string {
		return strings.TrimSuffix(strings.ToLower(strings.Trim(h, "[]")), ".")
	}
	return norm(host) == norm(s.base.Hostname())
}

// locationHost returns the host name a browser goes to for a Location header
// answering a request on the target, and offTarget true when it names a host
// at all: false means a path on the target itself. A browser strips leading
// and trailing control characters and spaces, drops tabs and newlines, and
// reads a backslash as a slash in an http(s) URL, which Go's url.Parse does
// not; so "/\host" is a host to a browser, not a path. A scheme other than
// http(s) yields that scheme in place of a host.
func locationHost(loc, baseScheme string) (host string, offTarget bool) {
	loc = strings.TrimFunc(loc, func(r rune) bool { return r <= ' ' })
	loc = strings.Map(func(r rune) rune {
		switch r {
		case '\t', '\n', '\r':
			return -1
		case '\\':
			return '/'
		}
		return r
	}, loc)

	rest := loc
	if i := strings.IndexByte(loc, ':'); i > 0 && isScheme(loc[:i]) {
		scheme := strings.ToLower(loc[:i])
		if scheme != "http" && scheme != "https" {
			return scheme + ":", true
		}
		rest = loc[i+1:]
		slashes := len(rest) - len(strings.TrimLeft(rest, "/"))
		if scheme == strings.ToLower(baseScheme) && slashes < 2 {
			return "", false // "http:path" is a path relative to an http page
		}
	} else if !strings.HasPrefix(loc, "//") {
		return "", false // a path, query or fragment on the target
	}

	authority := strings.TrimLeft(rest, "/")
	if i := strings.IndexAny(authority, "/?#"); i >= 0 {
		authority = authority[:i]
	}
	if i := strings.LastIndexByte(authority, '@'); i >= 0 {
		authority = authority[i+1:]
	}
	host = authority
	if h, _, err := net.SplitHostPort(authority); err == nil {
		host = h
	}
	if host == "" {
		return "", false // no host: not a URL a browser follows
	}
	return host, true
}

// isScheme reports whether s is a URL scheme: a letter, then letters, digits,
// '+', '-' or '.'.
func isScheme(s string) bool {
	for i, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case i > 0 && (c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'):
		default:
			return false
		}
	}
	return s != ""
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
	failed := 0
	for _, p := range payloads {
		r := s.do(http.MethodGet, p, nil, "")
		if r.err != nil {
			failed++
			continue
		}
		if r.status < 300 || r.status >= 400 {
			continue
		}
		loc := r.header.Get("Location")
		if loc == "" {
			continue
		}
		if host, off := locationHost(loc, s.base.Scheme); off && !s.sameHostname(host) {
			bad = append(bad, fmt.Sprintf("%s -> %d Location: %q (a browser goes to %s)", p, r.status, loc, host))
		}
	}

	switch {
	case len(bad) > 0:
		return []Finding{finding("REDIR-001", SevHigh, StatusFail,
			"a crafted path made the server emit a redirect to an attacker-controlled host (phishing / token leakage vector).").
			withEvidence("%s", strings.Join(bad, " | ")).
			withRemediation("Only ever redirect to validated, same-host, absolute-path targets; reject '//', '\\', and scheme-bearing inputs.")}
	case failed == len(payloads):
		return []Finding{finding("REDIR-001", SevInfo, StatusError,
			"could not send any of the crafted paths.")}
	default:
		detail := "crafted paths do not produce a redirect to a foreign host."
		if failed > 0 {
			detail = fmt.Sprintf("the crafted paths that reached the target produce no redirect to a foreign host; %d of %d requests failed.", failed, len(payloads))
		}
		return []Finding{finding("REDIR-001", SevMedium, StatusPass, detail)}
	}
}

// ---- X-Forwarded-For rate-limit bypass (aggressive) -------------------------

func bypassChecks() []check {
	return []check{{ids: []string{"BYPASS-XFF"}, run: runXFFBypassCheck}}
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
		return []Finding{finding("BYPASS-XFF", SevInfo, StatusSkip,
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
		return []Finding{finding("BYPASS-XFF", SevHigh, StatusFail,
			"rotating a spoofed X-Forwarded-For defeated the login rate limiter — an attacker can brute-force credentials unthrottled.").
			withEvidence("%d/%d spoofed-IP attempts reached the credential check without throttling", reached, burst).
			withRemediation("Disable web.trust_proxy unless a trusted reverse proxy actually sets X-Forwarded-For; otherwise the header is attacker-controlled.")}
	case reached <= 6:
		return []Finding{finding("BYPASS-XFF", SevHigh, StatusPass,
			"spoofing X-Forwarded-For did not bypass the login throttle (the limiter keys on the real connection, not the header).").
			withEvidence("only %d/%d spoofed-IP attempts got through before throttling", reached, burst)}
	default:
		return []Finding{finding("BYPASS-XFF", SevMedium, StatusWarn,
			"results were ambiguous; the login limiter may have been partially consumed before this probe.").
			withEvidence("%d/%d spoofed-IP attempts reached the credential check", reached, burst).
			withRemediation("Re-run in isolation (-only bypass) to confirm.")}
	}
}
