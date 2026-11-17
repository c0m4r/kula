package main

import (
	"strings"
	"testing"
)

// TestWebSocketProbesSayWhyThereIsNoSession: a WebSocket probe or a
// limiter probe that needs a session and has none asks for credentials only
// when none were supplied; when the supplied ones were refused, it says so.
func TestWebSocketProbesSayWhyThereIsNoSession(t *testing.T) {
	for _, c := range []struct {
		user, pass, want, not string
	}{
		{"", "", "supply -username/-password to open one", "did not log in"},
		{kmUser, "wrong", "the supplied credentials did not log in: the target refused them (HTTP 401)", "supply -username/-password"},
	} {
		for _, probe := range []struct {
			id  string
			run func(*Scanner) []Finding
		}{
			{"WS-002", runWSChecks},
			{"WS-MSGBOMB", runWSMsgBombCheck},
			{"WS-FLOOD", runWSFloodCheck},
			{"RATE-OLLAMA", runOllamaRateCheck},
		} {
			m := newKulaMock(true, true)
			s, _ := m.serve(t, Options{Username: c.user, Password: c.pass})
			f, _ := byID(probe.run(s), probe.id)
			if f.Status != StatusSkip || !strings.Contains(f.Detail, c.want) || strings.Contains(f.Detail, c.not) {
				t.Errorf("%s with credentials %q/%q: %s %q; want a SKIP saying %q", probe.id, c.user, c.pass, f.Status, f.Detail, c.want)
			}
		}
	}
}
