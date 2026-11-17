package main

import (
	"strings"
	"testing"
)

// TestFuzzProbesSayWhyThereIsNoSession: a fuzz probe that needs a session
// and has none asks for credentials only when none were supplied; when the
// supplied ones were refused, it says so.
func TestFuzzProbesSayWhyThereIsNoSession(t *testing.T) {
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
			{"FUZZ-QUERY", runFuzzQuery},
			{"FUZZ-BODY", runFuzzBody}, // its AI chat half
			{"FUZZ-METHODS", runFuzzMethods},
			{"FUZZ-WS", runFuzzWS},
		} {
			m := newKulaMock(true, true)
			s, _ := m.serve(t, Options{Username: c.user, Password: c.pass})
			f, _ := byID(probe.run(s), probe.id)
			if !strings.Contains(f.Detail, c.want) || strings.Contains(f.Detail, c.not) {
				t.Errorf("%s with credentials %q/%q: %s; want it to say %q", probe.id, c.user, c.pass, f.Detail, c.want)
			}
		}
	}
}
