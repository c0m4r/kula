package main

import (
	"fmt"
	"strings"
)

// expectKeys are the postures -expect can declare. A check tags the finding
// that would contradict one with expecting(key); when the key is declared, a
// WARN so tagged becomes a FAIL that sets exit status 1 whatever -fail-on is.
// A check whose finding has no WARN to upgrade asks s.expects(key) instead.
var expectKeys = []struct {
	key   string
	means string
}{
	{"auth", "web.auth is enabled (AUTH-000)"},
	{"tls", "the target is served over HTTPS (DISC-002, TLS-001)"},
	{"hsts", "HTTPS responses carry Strict-Transport-Security (HDR-008)"},
	{"origin-validation", "a state change without an Origin, or from a foreign one, is refused (CSRF-001, CSRF-002)"},
	{"frame-protection", "the CSP forbids framing with frame-ancestors 'none' (HDR-007)"},
	{"metrics-token", "/metrics requires a bearer token (PROM-001)"},
}

// expectKeyNames returns the -expect keys in table order.
func expectKeyNames() []string {
	names := make([]string, 0, len(expectKeys))
	for _, k := range expectKeys {
		names = append(names, k.key)
	}
	return names
}

// parseExpect parses the -expect list into known keys, or an error naming the
// keys it does not know.
func parseExpect(list string) ([]string, error) {
	known := map[string]bool{}
	for _, k := range expectKeys {
		known[k.key] = true
	}
	var keys, unknown []string
	for _, token := range splitList(list) {
		if !known[token] {
			unknown = append(unknown, fmt.Sprintf("%q", token))
			continue
		}
		keys = append(keys, token)
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("-expect: unknown key %s (keys: %s)", strings.Join(unknown, ", "), strings.Join(expectKeyNames(), ", "))
	}
	return keys, nil
}

// expecting tags f as the finding that contradicts the posture key. The tag
// matters only when -expect declares the key: applyExpectations then turns a
// WARN into a FAIL that always gates the exit status, and clears the tag
// otherwise.
func (f Finding) expecting(key string) Finding {
	f.Expectation = key
	return f
}

// expects reports whether -expect declared the posture key.
func (s *Scanner) expects(key string) bool {
	return s.expect[key]
}

// applyExpectations judges findings against the declared posture: a tagged
// WARN whose key is declared becomes a FAIL, and the tag is dropped from a
// finding whose key is not declared, so a tag in the report always names a
// declared expectation.
func (s *Scanner) applyExpectations(findings []Finding) {
	for i := range findings {
		f := &findings[i]
		switch {
		case f.Expectation == "":
		case !s.expects(f.Expectation):
			f.Expectation = ""
		case f.Status == StatusWarn:
			f.Status = StatusFail
			f.StatusStr = StatusFail.String()
		}
	}
}
