package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestUnreachableTargetExits2(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	target := "http://" + listener.Addr().String()
	_ = listener.Close() // nothing listens there now

	code, stdout, stderr := runCaptured(t, "-timeout", "2s", target)
	if code != exitUsage {
		t.Errorf("exit %d, want 2", code)
	}
	if !strings.Contains(stdout, "ERROR DISC-001") || !strings.Contains(stdout, "could not reach") {
		t.Errorf("the report does not show the DISC-001 error:\n%s", stdout)
	}
	if strings.Contains(stdout, "HDR-001") {
		t.Errorf("checks ran against an unreachable target:\n%s", stdout)
	}
	if !strings.Contains(stderr, "kula-scan: exit 2: the scan stopped at discovery (DISC-001)") {
		t.Errorf("no closing line explains the exit:\n%s", stderr)
	}
}

func TestNonKulaTargetExits2(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"404 everywhere": http.NotFound,
		"a page for everything": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("<html>hello</html>"))
		},
		"another JSON API": func(w http.ResponseWriter, r *http.Request) {
			writeJSONResp(w, map[string]any{"status": "ok", "error": "host not allowed"})
		},
	} {
		code, report, stderr := scanJSON(t, handler)
		if code != exitUsage {
			t.Errorf("%s: exit %d, want 2", name, code)
		}
		f, _ := byID(report.Findings, "DISC-001")
		if f.Status != StatusError || !strings.Contains(f.Detail, "does not look like Kula") {
			t.Errorf("%s: DISC-001 = %s %q", name, f.Status, f.Detail)
		}
		if len(report.Findings) != 1 {
			t.Errorf("%s: checks ran against a target that is not Kula: %v", name, reportedIDs(report))
		}
		if !strings.Contains(stderr, "exit 2") {
			t.Errorf("%s: no closing line explains the exit:\n%s", name, stderr)
		}
	}
}

// TestKulaFingerprints: any one of Kula's three tells is enough.
func TestKulaFingerprints(t *testing.T) {
	refuse := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"host not allowed: add it to web.security.allowed_hosts"}`))
	}
	for name, handler := range map[string]http.HandlerFunc{
		"/health": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/health" {
				_, _ = w.Write([]byte("kula is healthy"))
				return
			}
			http.NotFound(w, r)
		},
		"/api/auth/status": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/auth/status" {
				writeJSONResp(w, map[string]any{"auth_required": true, "authenticated": false})
				return
			}
			http.NotFound(w, r)
		},
		"host check": func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				refuse(w, r)
				return
			}
			http.NotFound(w, r)
		},
	} {
		ts := httptest.NewServer(handler)
		s, err := NewScanner(Options{Target: ts.URL, Timeout: 5 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		findings := s.discover()
		ts.Close()
		f, _ := byID(findings, "DISC-001")
		if !s.kula || f.Status != StatusPass {
			t.Errorf("%s: not taken for Kula: %s %q", name, f.Status, f.Detail)
		}
		if name == "/api/auth/status" && !s.authEnabled {
			t.Errorf("%s: auth_required=true not read", name)
		}
		if name == "host check" && (s.authEnabled || !strings.Contains(f.Detail, "refuses this Host name")) {
			t.Errorf("%s: authEnabled=%v, detail %q", name, s.authEnabled, f.Detail)
		}
	}
}

func TestDiscoveryReadsTheVersion(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/status":
			writeJSONResp(w, map[string]any{"auth_required": false})
		case "/api/config":
			writeJSONResp(w, map[string]any{"ollama_enabled": true, "version": "9.8.7"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	s, err := NewScanner(Options{Target: ts.URL, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	f, _ := byID(s.discover(), "DISC-001")
	if s.version != "9.8.7" || !s.ollama || !strings.Contains(f.Detail, "version 9.8.7") {
		t.Errorf("version %q, ollama %v, detail %q", s.version, s.ollama, f.Detail)
	}
}

func TestRejectedCredentialsAreInconclusive(t *testing.T) {
	code, report, stderr := scanJSON(t, secureMock(), "-username", mockUser, "-password", "wrong")
	if code != exitInconclusive {
		t.Errorf("exit %d, want 3", code)
	}
	f, _ := byID(report.Findings, "AUTH-005")
	if f.Status != StatusError || !strings.Contains(f.Detail, "wrong username or password") {
		t.Errorf("AUTH-005 = %s %q, want an ERROR naming wrong credentials", f.Status, f.Detail)
	}
	if !strings.Contains(stderr, "exit 3: inconclusive") || !strings.Contains(stderr, "AUTH-005") {
		t.Errorf("no closing line explains the exit:\n%s", stderr)
	}
}

func TestThrottledLoginIsNamed(t *testing.T) {
	mock := secureMock()
	throttled := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/login" && r.Method == http.MethodPost {
			http.Error(w, `{"error":"too many requests"}`, http.StatusTooManyRequests)
			return
		}
		mock.ServeHTTP(w, r)
	})
	code, report, _ := scanJSON(t, throttled, "-username", mockUser, "-password", mockPass, "-only", "auth")
	if code != exitInconclusive {
		t.Errorf("exit %d, want 3", code)
	}
	f, _ := byID(report.Findings, "AUTH-005")
	if f.Status != StatusError || !strings.Contains(f.Detail, "rate-limited") || !strings.Contains(f.Detail, "five minutes") {
		t.Errorf("AUTH-005 = %s %q, want an ERROR naming the rate limit", f.Status, f.Detail)
	}
}

// dropConnection answers a request by closing the connection.
func dropConnection(w http.ResponseWriter) {
	if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
		_ = conn.Close()
	}
}

// TestRequestErrorsAreErrorsNotFailures: a probe whose request failed has
// learnt nothing about the safeguard.
func TestRequestErrorsAreErrorsNotFailures(t *testing.T) {
	mock := secureMock()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/logout" && r.Header.Get("X-CSRF-Token") == "":
			dropConnection(w)
		case r.URL.Path == "/metrics" && r.Header.Get("Authorization") != "":
			dropConnection(w)
		default:
			mock.ServeHTTP(w, r)
		}
	}))
	defer ts.Close()
	s := scannerFor(t, ts, mockUser, mockPass)

	wantStatus(t, runCSRFChecks(s), "CSRF-003", StatusError)
	wantStatus(t, runMetricsChecks(s), "PROM-002", StatusError)
}

func TestFailureWinsOverError(t *testing.T) {
	r := newReport("t", []Finding{
		finding("HDR-003", SevHigh, StatusFail, ""),
		finding("CSRF-003", SevInfo, StatusError, ""),
	})
	if code := r.exitCode(SevHigh); code != exitFailing {
		t.Errorf("a HIGH FAIL and an ERROR exit %d, want 1", code)
	}
	if code := r.exitCode(SevCritical); code != exitInconclusive {
		t.Errorf("an ERROR with the FAIL below -fail-on exits %d, want 3", code)
	}
	markIgnored(r.Findings, map[string]bool{"csrf-003": true})
	if code := newReport("t", r.Findings).exitCode(SevCritical); code != exitClean {
		t.Errorf("an ignored ERROR exits %d, want 0", code)
	}
}
