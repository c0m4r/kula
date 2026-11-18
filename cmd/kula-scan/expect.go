package main

import (
	"fmt"
	"strings"
)

// expectKey is a posture -expect can declare: what it means, and the
// findings that judge it, each of which a check tags with expecting(key)
// where it would contradict the posture. TestExpectKeysNameTheirFindings
// keeps ids in step with those tags.
type expectKey struct {
	key   string
	means string
	ids   []string
}

// expectKeys are the postures -expect can declare. A check tags the finding
// that would contradict one with expecting(key); when the key is declared, a
// WARN so tagged becomes a FAIL that sets exit status 1 whatever -fail-on is.
// A check whose finding has no WARN to upgrade asks s.expects(key) instead.
var expectKeys = []expectKey{
	{"auth", "web.auth is enabled", []string{"AUTH-000"}},
	{"tls", "the target is served over HTTPS with a certificate clients trust", []string{"DISC-002", "TLS-001", "TLS-004"}},
	{"hsts", "the target is served over HTTPS with Strict-Transport-Security", []string{"HDR-008"}},
	{"origin-validation", "a state change without an Origin, or from a foreign one, and a WebSocket upgrade from a foreign Origin are refused", []string{"CSRF-001", "CSRF-002", "WS-002"}},
	{"frame-protection", "responses forbid framing with X-Frame-Options: DENY and the CSP's frame-ancestors 'none'", []string{"HDR-002", "HDR-007"}},
	{"metrics-token", "/metrics requires a bearer token", []string{"PROM-001"}},
	{"host-check", "Kula refuses Host names a public DNS server could answer for: the DNS-rebinding check is on", []string{"HOST-001"}},
}

// expectKeyNames returns the -expect keys in table order.
func expectKeyNames() []string {
	names := make([]string, 0, len(expectKeys))
	for _, k := range expectKeys {
		names = append(names, k.key)
	}
	return names
}

// expectKeyOf returns the -expect key finding id judges, or "".
func expectKeyOf(id string) string {
	for _, k := range expectKeys {
		for _, kid := range k.ids {
			if strings.EqualFold(kid, id) {
				return k.key
			}
		}
	}
	return ""
}

// unjudgedExpectations returns the usage error of -expect keys this run
// cannot judge, or nil. A key is judged only by its findings (expectKey.ids),
// so one whose findings are all left out of the run would otherwise pass
// unexamined: each finding is deselected by -only or -skip, ignored by
// -ignore, or from a check of a mode (-aggressive, -fuzz) the run does not
// enable. The error names each such key, its findings and why each is out.
func (s *Scanner) unjudgedExpectations(sel *selection, ignored map[string]bool) error {
	var problems []string
	for _, k := range expectKeys {
		if !s.expects(k.key) {
			continue
		}
		var why []string
		for _, id := range k.ids {
			reason := s.notJudged(id, sel, ignored)
			if reason == "" {
				why = nil
				break
			}
			why = append(why, id+" "+reason)
		}
		if len(why) == 0 {
			continue
		}
		which := "which this run does not judge"
		if len(k.ids) > 1 {
			which = "none of which this run judges"
		}
		problems = append(problems, fmt.Sprintf("-expect %s is judged by %s, %s (%s); select its findings, or drop %s from -expect",
			k.key, strings.Join(k.ids, ", "), which, strings.Join(why, "; "), k.key))
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(problems, "\n       "))
}

// notJudged says why finding id is left out of this run, or returns "" when
// it is in: what deselects it (sel), ignores it (ignored, lowercased IDs) or
// keeps its check from running.
func (s *Scanner) notJudged(id string, sel *selection, ignored map[string]bool) string {
	entry := catalogEntryFor(id)
	switch {
	case sel != nil && len(sel.only) > 0 && !sel.only[entry.Category] && !sel.only[strings.ToLower(id)]:
		return "is not selected by -only"
	case !sel.selected(id):
		return "is skipped by -skip"
	case ignored[strings.ToLower(id)]:
		return "is ignored by -ignore"
	case !s.modeEnabled(entry.Mode):
		return fmt.Sprintf("runs only with -%s", entry.Mode)
	}
	return ""
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
		key := strings.ToLower(token)
		if !known[key] {
			unknown = append(unknown, fmt.Sprintf("%q", token))
			continue
		}
		keys = append(keys, key)
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
