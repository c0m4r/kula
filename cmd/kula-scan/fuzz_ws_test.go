package main

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// TestFuzzWSSaysWhatItSent: FUZZ-WS counts the connections it opened and
// the frames it sent on them, with auth on (through the session) and off.
func TestFuzzWSSaysWhatItSent(t *testing.T) {
	for _, auth := range []bool{true, false} {
		m := newKulaMock(auth, false)
		s, _ := m.serve(t, Options{Username: kmUser, Password: kmPass})
		f, _ := byID(runFuzzWS(s), "FUZZ-WS")
		want := "no anomalies: 6 connections planned, 6 opened; 48 frames planned on them, 48 sent; "
		if f.Status != StatusPass || !strings.HasPrefix(f.Detail, want) {
			t.Errorf("auth %v: FUZZ-WS = %s: %s; want a PASS starting %q", auth, f.Status, f.Detail, want)
		}
	}
}

// TestFuzzWSWithoutASession: behind auth without a session FUZZ-WS opens
// nothing and skips, saying why there is no session.
func TestFuzzWSWithoutASession(t *testing.T) {
	for _, c := range []struct {
		user, pass, why string
	}{
		{"", "", "supply -username/-password to open one"},
		{kmUser, "wrong", "the supplied credentials did not log in: the target refused them (HTTP 401)"},
	} {
		m := newKulaMock(true, false)
		s, _ := m.serve(t, Options{Username: c.user, Password: c.pass})
		f, _ := byID(runFuzzWS(s), "FUZZ-WS")
		if f.Status != StatusSkip || !strings.Contains(f.Detail, "no session is available to open a WebSocket: "+c.why) {
			t.Errorf("FUZZ-WS without a session (%q) = %s: %s", c.why, f.Status, f.Detail)
		}
		for _, r := range m.requests() {
			if r.path == "/ws" {
				t.Errorf("FUZZ-WS without a session tried to open a WebSocket")
				break
			}
		}
	}
}

// TestFuzzWSThatOpensNothingDoesNotPass: a target that refuses every
// upgrade gets an ERROR naming the refusal, or a SKIP when its connection
// limit refused them all; one that refuses most gets a WARN. An upgrade
// refused with 429 is tried once more, so a connection counts as refused
// by the limit after two refusals.
func TestFuzzWSThatOpensNothingDoesNotPass(t *testing.T) {
	shortRetryWait(t)
	for _, c := range []struct {
		name    string
		refuse  int64 // how many upgrade requests to refuse
		status  int
		want    Status
		message string
	}{
		{"forbidden", 1 << 20, http.StatusForbidden, StatusError,
			"no WebSocket connection could be opened, so no frame was sent: 6 connections planned, 0 opened; 0 frames planned on them, 0 sent; 6 upgrades refused (the first: HTTP 403)"},
		{"connection limit", 1 << 20, http.StatusTooManyRequests, StatusSkip,
			"the WebSocket connection limit refused every upgrade (retry once other connections close): 6 connections planned, 0 opened"},
		{"most refused", 8, http.StatusTooManyRequests, StatusWarn,
			"fewer than half of the planned connections opened: 6 connections planned, 2 opened; 16 frames planned on them, 16 sent; " +
				"4 upgrades refused (the first: HTTP 429, the WebSocket connection limit)"},
	} {
		m := newKulaMock(false, false)
		var upgrades atomic.Int64
		ts := quietServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/ws" && upgrades.Add(1) <= c.refuse {
				kmJSONError(w, "refused", c.status)
				return
			}
			m.ServeHTTP(w, r)
		}))
		s := fuzzScanner(t, ts.URL, 10)
		f, _ := byID(runFuzzWS(s), "FUZZ-WS")
		if f.Status != c.want || !strings.Contains(f.Detail, c.message) {
			t.Errorf("%s: FUZZ-WS = %s: %s; want %s with %q", c.name, f.Status, f.Detail, c.want, c.message)
		}
	}
}
