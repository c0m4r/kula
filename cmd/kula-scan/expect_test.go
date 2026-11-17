package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestExpectAuthFailsWhateverFailOn(t *testing.T) {
	var loginHits, chatHits atomic.Int64
	mock := authOffMock(false, &loginHits, &chatHits)

	code, report, stderr := scanJSON(t, mock, "-fail-on", "critical")
	if code != exitClean {
		t.Fatalf("without -expect: exit %d, want 0; stderr:\n%s", code, stderr)
	}
	wantStatus(t, report.Findings, "AUTH-000", StatusWarn)
	if f, _ := byID(report.Findings, "AUTH-000"); f.Expectation != "" {
		t.Errorf("an undeclared expectation shows in the report: %q", f.Expectation)
	}

	code, report, stderr = scanJSON(t, mock, "-fail-on", "critical", "-expect", "AUTH")
	if code != exitFailing {
		t.Fatalf("-expect auth against auth off: exit %d, want 1; stderr:\n%s", code, stderr)
	}
	f, _ := byID(report.Findings, "AUTH-000")
	if f.Status != StatusFail || f.Expectation != "auth" {
		t.Errorf("AUTH-000 = %s, expectation %q; want FAIL against auth", f.Status, f.Expectation)
	}
	if !strings.Contains(stderr, "expected posture not met: auth (AUTH-000)") {
		t.Errorf("the closing line does not name the expectation:\n%s", stderr)
	}

	// -ignore still wins.
	if code, _, _ := scanJSON(t, mock, "-fail-on", "critical", "-expect", "auth", "-ignore", "AUTH-000"); code != exitClean {
		t.Errorf("-expect auth -ignore AUTH-000: exit %d, want 0", code)
	}
}

func TestUnknownExpectKeyIsAUsageError(t *testing.T) {
	code, _, stderr := runCaptured(t, "-expect", "auth,host-checks", "http://kula-scan.invalid")
	if code != exitUsage || !strings.Contains(stderr, `"host-checks"`) || !strings.Contains(stderr, "origin-validation") {
		t.Errorf("exit %d, stderr:\n%s", code, stderr)
	}
}

func TestExpectTLSOnPlainHTTP(t *testing.T) {
	code, report, _ := scanJSON(t, secureMock(), "-expect", "tls", "-only", "discovery,tls")
	if code != exitFailing {
		t.Errorf("exit %d, want 1", code)
	}
	for _, id := range []string{"DISC-002", "TLS-001"} {
		f, _ := byID(report.Findings, id)
		if f.Status != StatusFail || f.Expectation != "tls" {
			t.Errorf("%s = %s, expectation %q; want FAIL against tls", id, f.Status, f.Expectation)
		}
	}
	// Without -expect tls, plain HTTP has no TLS layer to judge.
	_, report, _ = scanJSON(t, secureMock(), "-only", "tls")
	wantStatus(t, report.Findings, "TLS-001", StatusSkip)
}

// TestExpectationTags: every -expect key is carried by the finding that
// contradicts it.
func TestExpectationTags(t *testing.T) {
	var loginHits, chatHits atomic.Int64
	weakCSP := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte("kula is healthy"))
		case "/api/auth/status":
			writeJSONResp(w, map[string]any{"auth_required": false})
		default:
			w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'nonce-"+randToken(8)+"'")
			_, _ = w.Write([]byte("<html></html>"))
		}
	})
	cases := []struct {
		key, id string
		handler http.Handler
		tls     bool
		run     func(*Scanner) []Finding
	}{
		{"auth", "AUTH-000", authOffMock(false, &loginHits, &chatHits), false, runAuthChecks},
		{"origin-validation", "CSRF-001", insecureMock(), false, runCSRFChecks},
		{"origin-validation", "CSRF-002", insecureMock(), false, runCSRFChecks},
		{"metrics-token", "PROM-001", insecureMock(), false, runMetricsChecks},
		{"host-check", "HOST-001", authOffMock(false, &loginHits, &chatHits), false, runHostCheck},
		{"frame-protection", "HDR-007", weakCSP, false, runHeaderChecks},
		{"hsts", "HDR-008", weakCSP, true, runHeaderChecks},
		{"tls", "DISC-002", secureMock(), false, func(s *Scanner) []Finding { return s.discover() }},
		{"tls", "TLS-001", secureMock(), false, runTLSChecks},
	}
	for _, c := range cases {
		var ts *httptest.Server
		if c.tls {
			ts = httptest.NewTLSServer(c.handler)
		} else {
			ts = httptest.NewServer(c.handler)
		}
		s, err := NewScanner(Options{Target: ts.URL, Timeout: 5 * time.Second, Insecure: true, Expect: []string{c.key}})
		if err != nil {
			t.Fatal(err)
		}
		s.discover()
		findings := c.run(s)
		s.applyExpectations(findings)
		ts.Close()
		f, ok := byID(findings, c.id)
		if !ok || f.Status != StatusFail || f.Expectation != c.key {
			t.Errorf("-expect %s: %s = %+v, want a FAIL against %s", c.key, c.id, f, c.key)
		}
	}
	// Each key of the table has a case.
	for _, key := range expectKeyNames() {
		found := false
		for _, c := range cases {
			found = found || c.key == key
		}
		if !found {
			t.Errorf("-expect %s has no tagged finding under test", key)
		}
	}
}

func TestApplyExpectationsOnlyJudgesDeclaredKeys(t *testing.T) {
	s, err := NewScanner(Options{Target: "http://kula-scan.invalid", Expect: []string{"hsts"}})
	if err != nil {
		t.Fatal(err)
	}
	findings := []Finding{
		finding("HDR-008", SevLow, StatusWarn, "").expecting("hsts"),
		finding("AUTH-000", SevMedium, StatusWarn, "").expecting("auth"),
		finding("HDR-008", SevLow, StatusPass, "").expecting("hsts"),
	}
	s.applyExpectations(findings)
	if findings[0].Status != StatusFail || findings[0].StatusStr != "FAIL" || findings[0].Expectation != "hsts" {
		t.Errorf("a declared WARN: %+v, want a FAIL against hsts", findings[0])
	}
	if findings[1].Status != StatusWarn || findings[1].Expectation != "" {
		t.Errorf("an undeclared WARN: %+v, want an untagged WARN", findings[1])
	}
	if findings[2].Status != StatusPass {
		t.Errorf("a declared PASS: %+v, want it unchanged", findings[2])
	}

	var out strings.Builder
	newReport("http://t", findings).writeText(&out, true)
	if !strings.Contains(out.String(), "HSTS over TLS [LOW] (expected: hsts)") {
		t.Errorf("the text report does not mark the expectation:\n%s", out.String())
	}
	// A FAIL against a declared expectation gates even below -fail-on.
	if code := newReport("t", findings[:1]).exitCode(SevCritical); code != exitFailing {
		t.Errorf("exit %d, want 1", code)
	}
}
