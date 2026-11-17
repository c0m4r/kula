package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// kulaReadOnlyRoutes are the API routes Kula serves through readOnly: GET
// and HEAD, and a JSON 405 to any other method.
var kulaReadOnlyRoutes = map[string]bool{
	"/api/current": true, "/api/config": true, "/api/history": true, "/api/i18n": true, "/api/system-info": true,
	"/api/auth/status": true, "/api/ollama/models": true, "/api/ollama/context": true,
}

// kulaMethodAllowed answers 405, as Kula does, a method the API route of r
// does not serve: anything but GET and HEAD on a read-only route, anything
// but POST on the AI chat. It reports whether the method is served.
// secureMock calls it behind its auth gate, where Kula's readOnly sits.
func kulaMethodAllowed(w http.ResponseWriter, r *http.Request) bool {
	var allow string
	switch {
	case kulaReadOnlyRoutes[r.URL.Path] && r.Method != http.MethodGet && r.Method != http.MethodHead:
		allow = "GET, HEAD"
	case r.URL.Path == "/api/ollama/chat" && r.Method != http.MethodPost:
		allow = http.MethodPost
	default:
		return true
	}
	w.Header().Set("Allow", allow)
	kmJSONError(w, "method not allowed", http.StatusMethodNotAllowed)
	return false
}

// counts returns how many login attempts and chat bodies reached the mock's
// handlers.
func (m *kulaMock) counts() [2]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return [2]int{m.loginAttempts, len(m.chatBodies)}
}

// TestFuzzMethodsReachTheHandlers: with a session (auth on) or a
// same-origin Origin (auth off), every request of the matrix gets past the
// gates to a handler; the read-only and POST-only API routes refuse what
// they do not serve, which is said; and the page, assets, /health and
// /status answering 200 to DELETE and the other methods is information,
// never a hit.
func TestFuzzMethodsReachTheHandlers(t *testing.T) {
	n := len(methodMatrix())
	// 17 routes get 9 unusual methods, HEAD and OPTIONS; the 8 read-only API
	// routes get POST too.
	if n != 17*11+8 {
		t.Fatalf("the matrix has %d requests, want %d", n, 17*11+8)
	}
	for _, auth := range []bool{true, false} {
		m := newKulaMock(auth, true)
		s, _ := m.serve(t, Options{Username: kmUser, Password: kmPass})
		f, _ := byID(runFuzzMethods(s), "FUZZ-METHODS")
		want := fmt.Sprintf("no anomalies: %d planned, %d sent: %d reached a handler.", n, n, n)
		if f.Status != StatusPass || !strings.HasPrefix(f.Detail, want) {
			t.Errorf("auth %v: FUZZ-METHODS = %s: %s; want a PASS starting %q", auth, f.Status, f.Detail, want)
		}
		// 8 read-only routes × POST, PUT, DELETE, PATCH, and 3 POST-only
		// routes × 10 methods (all but POST and OPTIONS).
		if !strings.Contains(f.Detail, "Method enforcement: 62 requests with a method their API route does not serve") {
			t.Errorf("auth %v: FUZZ-METHODS does not say the API routes enforced their methods: %s", auth, f.Detail)
		}
		// /, /style.css, /health and /status × the 9 unusual methods.
		if !strings.Contains(f.Detail, "For information: 36 requests outside /api/ with a method other than GET, HEAD or OPTIONS were answered 2xx") {
			t.Errorf("auth %v: FUZZ-METHODS does not count the non-API 2xx answers: %s", auth, f.Detail)
		}
		deleted := false
		for _, r := range m.requests() {
			deleted = deleted || r.method == http.MethodDelete && r.path == "/style.css"
		}
		if !deleted {
			t.Errorf("auth %v: no DELETE /style.css was sent", auth)
		}
	}

	// The secure mock, with Kula's method rules, passes too: its HEAD to the
	// disabled AI routes are answered unavailable, its HEAD to the
	// token-guarded /metrics stops at that gate.
	ts := httptest.NewServer(secureMock())
	defer ts.Close()
	s := scannerForFuzz(t, ts, mockUser, mockPass)
	f, _ := byID(runFuzzMethods(s), "FUZZ-METHODS")
	if f.Status != StatusPass || !strings.Contains(f.Detail, fmt.Sprintf("%d planned, %d sent: %d reached a handler", n, n, n-3)) {
		t.Errorf("FUZZ-METHODS against the secure mock = %s: %s", f.Status, f.Detail)
	}
}

// TestFuzzMethodsFailsOnAnUnenforcedMethod: a read-only API route that
// answers POST, PUT, DELETE or PATCH with 200, or a POST-only route that
// answers another method with 200, is a hit; a CORS preflight answered 204
// is not.
func TestFuzzMethodsFailsOnAnUnenforcedMethod(t *testing.T) {
	m := newKulaMock(false, false)
	m.laxMethods = true
	s, _ := m.serve(t, Options{})
	f, _ := byID(runFuzzMethods(s), "FUZZ-METHODS")
	if f.Status != StatusFail || !strings.Contains(f.Evidence, "an API route accepted a method it does not serve (HTTP 200) ← POST /api/current") {
		t.Errorf("FUZZ-METHODS against read-only routes that answer POST = %s: %s | %s", f.Status, f.Detail, f.Evidence)
	}
	if !strings.Contains(f.Remediation, "405") {
		t.Errorf("the remediation does not say to answer 405: %q", f.Remediation)
	}

	m = newKulaMock(false, false)
	m.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/logout" {
			return false
		}
		kmJSON(w, map[string]string{"status": "logged out"})
		return true
	}
	s, _ = m.serve(t, Options{})
	f, _ = byID(runFuzzMethods(s), "FUZZ-METHODS")
	if f.Status != StatusFail || !strings.Contains(f.Evidence, "← PUT /api/logout") {
		t.Errorf("FUZZ-METHODS against a logout that answers PUT = %s: %s | %s", f.Status, f.Detail, f.Evidence)
	}

	// Kula with its own origin allow-listed answers a preflight 204.
	m = newKulaMock(true, false)
	m.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodOptions {
			return false
		}
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	s, _ = m.serve(t, Options{Username: kmUser, Password: kmPass})
	wantStatus(t, runFuzzMethods(s), "FUZZ-METHODS", StatusPass)
}

// TestFuzzMethodsNeverChangesState: the matrix sends no POST to login (it
// would spend a login attempt), to the AI chat (it could start a chat) or
// to logout, and nothing it sends to logout carries the session; the
// session is still valid after it.
func TestFuzzMethodsNeverChangesState(t *testing.T) {
	for _, auth := range []bool{true, false} {
		m := newKulaMock(auth, true)
		s, _ := m.serve(t, Options{Username: kmUser, Password: kmPass})
		if auth && !s.ensureSession() {
			t.Fatal("no session")
		}
		before := len(m.requests())
		attempts := m.counts()
		wantStatus(t, runFuzzMethods(s), "FUZZ-METHODS", StatusPass)

		sent := m.requests()[before:]
		if len(sent) != len(methodMatrix()) {
			t.Errorf("auth %v: %d requests sent, want the %d of the matrix", auth, len(sent), len(methodMatrix()))
		}
		for _, r := range sent {
			switch {
			case r.method == http.MethodPost && (r.path == "/api/login" || r.path == "/api/ollama/chat" || r.path == "/api/logout"):
				t.Errorf("auth %v: the matrix sent POST %s", auth, r.path)
			case r.path == "/api/logout" && r.header.Get("Cookie") != "":
				t.Errorf("auth %v: the matrix sent %s /api/logout with the Cookie %q", auth, r.method, r.header.Get("Cookie"))
			}
		}
		if after := m.counts(); after != attempts {
			t.Errorf("auth %v: login attempts and chat bodies went from %v to %v", auth, attempts, after)
		}
		if auth {
			r := s.do(http.MethodGet, "/api/auth/status", s.authedHeaders(false), "")
			if !strings.Contains(r.body, `"authenticated":true`) {
				t.Errorf("the session no longer authenticates after the matrix: %s", r.body)
			}
		}
	}
}

// TestFuzzMethodsWithoutASession: behind auth without a session the API
// routes stop at the auth gate, and the finding says why there is no
// session.
func TestFuzzMethodsWithoutASession(t *testing.T) {
	for _, c := range []struct {
		user, pass, why string
	}{
		{"", "", "supply -username/-password to open one"},
		{kmUser, "wrong", "the supplied credentials did not log in"},
	} {
		m := newKulaMock(true, false)
		s, _ := m.serve(t, Options{Username: c.user, Password: c.pass})
		f, _ := byID(runFuzzMethods(s), "FUZZ-METHODS")
		if f.Status != StatusWarn || !strings.Contains(f.Detail, "Without a session the API routes stop at the auth gate: "+c.why) {
			t.Errorf("FUZZ-METHODS without a session (%q) = %s: %s", c.why, f.Status, f.Detail)
		}
	}
}
