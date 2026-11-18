package main

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// configReads counts the requests m saw for /api/config, and of those the
// ones that carried the session.
func configReads(m *kulaMock) (all, withSession int) {
	for _, r := range m.requests() {
		if r.path != "/api/config" {
			continue
		}
		all++
		if strings.Contains(r.header.Get("Cookie"), "kula_session="+kmSession) {
			withSession++
		}
	}
	return all, withSession
}

// TestAssistantKnownFromDiscovery: with auth off discovery reads
// /api/config, and the accessor answers from it without another request.
func TestAssistantKnownFromDiscovery(t *testing.T) {
	for _, on := range []bool{true, false} {
		m := newKulaMock(false, on)
		s, _ := m.serve(t, Options{})
		before, _ := configReads(m)
		enabled, st, why := s.assistantEnabled()
		if enabled != on || st != StatusPass || why != "" {
			t.Errorf("assistant %v: assistantEnabled = %v, %s, %q", on, enabled, st, why)
		}
		if after, _ := configReads(m); after != before {
			t.Errorf("assistant %v: the accessor read /api/config again (%d reads, %d after discovery)", on, after, before)
		}
	}
}

// TestAssistantReadOnceWithTheSession: behind auth discovery cannot see the
// assistant; with the session open the accessor reads /api/config once,
// keeps the answer and the version, and the report's target_info shows them.
func TestAssistantReadOnceWithTheSession(t *testing.T) {
	m := newKulaMock(true, true)
	m.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/config" {
			return false
		}
		kmJSON(w, map[string]any{"auth_enabled": true, "ollama_enabled": true, "version": "9.8.7"})
		return true
	}
	s, _ := m.serve(t, Options{Username: kmUser, Password: kmPass})
	disc, _ := byID(s.discover(), "DISC-001")
	if s.ollama || s.ollamaKnown || !strings.Contains(disc.Detail, "ollama not visible without a session") {
		t.Fatalf("discovery behind auth: ollama %v, known %v, DISC-001 %q", s.ollama, s.ollamaKnown, disc.Detail)
	}
	if !s.ensureSession() {
		t.Fatal("no session")
	}
	for range 3 {
		if enabled, st, why := s.assistantEnabled(); !enabled || st != StatusPass {
			t.Fatalf("assistantEnabled = %v, %s, %q; want enabled", enabled, st, why)
		}
	}
	if _, withSession := configReads(m); withSession != 1 {
		t.Errorf("read /api/config with the session %d times, want once", withSession)
	}
	if info := s.targetInfo(); !info.Ollama || info.Version != "9.8.7" {
		t.Errorf("target_info = %+v, want ollama and the version the session read", info)
	}
}

// TestAssistantReadFailureIsKept: a read with the session that fails is an
// ERROR, made once.
func TestAssistantReadFailureIsKept(t *testing.T) {
	m := newKulaMock(true, true)
	m.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/config" {
			return false
		}
		http.Error(w, "boom", http.StatusInternalServerError)
		return true
	}
	s, _ := m.serve(t, Options{Username: kmUser, Password: kmPass})
	s.ensureSession()
	for range 2 {
		if _, st, why := s.assistantEnabled(); st != StatusError || !strings.Contains(why, "answered HTTP 500") {
			t.Errorf("assistantEnabled = %s %q, want an ERROR naming the 500", st, why)
		}
	}
	if _, withSession := configReads(m); withSession != 1 {
		t.Errorf("read /api/config with the session %d times, want once", withSession)
	}
}

// TestAssistantNotReadWithoutASession: without a session the accessor reads
// nothing and never logs in; a whole run without credentials neither.
func TestAssistantNotReadWithoutASession(t *testing.T) {
	// No credentials; then credentials, but no check opened the session.
	// Either way nothing is read, and no login is spent on it.
	for _, opts := range []Options{{}, {Username: kmUser, Password: kmPass}} {
		m := newKulaMock(true, true)
		s, _ := m.serve(t, opts)
		before, _ := configReads(m)
		enabled, st, why := s.assistantEnabled()
		if enabled || st != StatusSkip || !strings.Contains(why, "no session is available") {
			t.Errorf("user %q: assistantEnabled without a session = %v, %s, %q", opts.Username, enabled, st, why)
		}
		if opts.Username == "" && !strings.Contains(why, "supply -username/-password") {
			t.Errorf("no credentials: %q does not ask for them", why)
		}
		if after, _ := configReads(m); after != before || m.logins() != 0 {
			t.Errorf("user %q: the accessor read /api/config %d times and logged in %d times without a session", opts.Username, after-before, m.logins())
		}
	}

	// A whole default run without credentials.
	m := newKulaMock(true, true)
	code, report, stderr := scanJSON(t, m)
	if _, withSession := configReads(m); withSession != 0 || m.logins() != 0 {
		t.Errorf("a run without credentials read /api/config with a session %d times, logged in %d times", withSession, m.logins())
	}
	if report.TargetInfo.Ollama {
		t.Errorf("target_info says ollama with nothing read: exit %d, %+v\n%s", code, report.TargetInfo, stderr)
	}
}

// logins returns how many login attempts reached m.
func (m *kulaMock) logins() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loginAttempts
}

// TestRateOllamaRunsBehindAuth: on an authenticated instance with the
// assistant on, RATE-OLLAMA learns it with the run's one session and runs,
// and target_info shows the assistant.
func TestRateOllamaRunsBehindAuth(t *testing.T) {
	m := newKulaMock(true, true)
	var models atomic.Int64
	m.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/ollama/models" || models.Add(1) <= 60 {
			return false
		}
		kmAIError(w, "rate_limited", "rate limit exceeded", http.StatusTooManyRequests)
		return true
	}
	t.Setenv(passwordEnv, kmPass)
	code, report, stderr := scanJSON(t, m, "-aggressive", "-only", "RATE-OLLAMA", "-username", kmUser)
	if code != exitClean {
		t.Errorf("exit %d; stderr:\n%s", code, stderr)
	}
	wantStatus(t, report.Findings, "RATE-OLLAMA", StatusPass)
	if !report.TargetInfo.Ollama || report.TargetInfo.AuthEnabled == nil || !*report.TargetInfo.AuthEnabled {
		t.Errorf("target_info = %+v, want auth and ollama", report.TargetInfo)
	}
	if n := m.logins(); n != 1 {
		t.Errorf("%d logins, want one", n)
	}
	if _, withSession := configReads(m); withSession != 1 {
		t.Errorf("read /api/config with the session %d times, want once", withSession)
	}
}

// TestDefaultRunLearnsTheAssistantForTheReport: a default run that opened a
// session shows in target_info what the session can see of the assistant,
// though no check asked.
func TestDefaultRunLearnsTheAssistantForTheReport(t *testing.T) {
	m := newKulaMock(true, true)
	t.Setenv(passwordEnv, kmPass)
	_, report, _ := scanJSON(t, m, "-only", "auth", "-username", kmUser)
	if n := m.logins(); !report.TargetInfo.Ollama || n != 1 {
		t.Errorf("target_info = %+v after %d logins, want ollama after one", report.TargetInfo, n)
	}
}
