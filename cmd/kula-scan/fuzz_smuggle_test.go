package main

import (
	"net/http"
	"strings"
	"testing"
)

// TestFuzzSmuggleSaysWhatItObserved: FUZZ-SMUGGLE reports how many of its
// requests were answered or refused, and how, and claims nothing about a
// desync, which one server cannot show. net/http refuses two of them before
// routing; Kula's CSRF gate refuses the third, which carries no Origin.
func TestFuzzSmuggleSaysWhatItObserved(t *testing.T) {
	m := newKulaMock(true, false)
	s, _ := m.serve(t, Options{})
	f, _ := byID(runFuzzSmuggle(s), "FUZZ-SMUGGLE")
	want := "3 of 3 ambiguously framed requests to /api/login answered or refused: two Content-Lengths: HTTP 400; " +
		"Content-Length with chunked Transfer-Encoding: HTTP 403; two Transfer-Encodings: HTTP 501. None was accepted with a 2xx or answered HTTP 500."
	if f.Status != StatusPass || !strings.HasPrefix(f.Detail, want) {
		t.Errorf("FUZZ-SMUGGLE = %s: %s; want a PASS starting %q", f.Status, f.Detail, want)
	}
	if f.Title != "Ambiguous request framing" || strings.Contains(f.Detail, "no desync") {
		t.Errorf("FUZZ-SMUGGLE claims more than it observes: %q: %s", f.Title, f.Detail)
	}

	// With origin validation off the third reaches login, which refuses its
	// empty body.
	m = newKulaMock(true, false)
	m.noOriginCheck = true
	s, _ = m.serve(t, Options{})
	f, _ = byID(runFuzzSmuggle(s), "FUZZ-SMUGGLE")
	if f.Status != StatusPass || !strings.Contains(f.Detail, "Content-Length with chunked Transfer-Encoding: HTTP 400") || m.counts()[0] != 1 {
		t.Errorf("FUZZ-SMUGGLE without the Origin check = %s: %s; %d login attempts, want 1", f.Status, f.Detail, m.counts()[0])
	}

	// A login that accepts what reaches it.
	m = newKulaMock(true, false)
	m.noOriginCheck = true
	m.override = func(w http.ResponseWriter, r *http.Request) bool {
		kmJSON(w, map[string]string{"status": "logged in"})
		return true
	}
	s, _ = m.serve(t, Options{})
	f, _ = byID(runFuzzSmuggle(s), "FUZZ-SMUGGLE")
	if f.Status != StatusFail || !strings.Contains(f.Evidence, "accepted ambiguous framing (HTTP 200)") ||
		!strings.Contains(f.Detail, "Content-Length with chunked Transfer-Encoding: HTTP 200") ||
		!strings.Contains(f.Detail, "would not agree with the proxy on where a request ends") {
		t.Errorf("FUZZ-SMUGGLE against a login that accepts it = %s: %s | %s", f.Status, f.Detail, f.Evidence)
	}
}
