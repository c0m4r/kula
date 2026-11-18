package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// postureTarget is Kula's /health with /api/auth/status answered as mode
// says: "absent" is Kula with web.ui: false, a plain 404 for every route but
// /health and /metrics; "502" a proxy whose upstream failed; "drop" a
// connection closed without an answer; "html" a page that is not Kula's
// JSON. Every other route answers as Kula's API would without a session, so
// a check that reads it would take a 401 for Kula's.
func postureTarget(mode string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/health":
			_, _ = w.Write([]byte("kula is healthy"))
		case r.URL.Path == "/metrics":
			w.Header().Set("Content-Type", "text/plain; version=0.0.4")
			_, _ = w.Write([]byte("kula_up 1\n"))
		case mode == "absent":
			http.NotFound(w, r)
		case r.URL.Path != "/api/auth/status":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		case mode == "drop":
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
		case mode == "html":
			_, _ = w.Write([]byte("<html>sign in to the proxy</html>"))
		default:
			w.WriteHeader(http.StatusBadGateway)
		}
	})
}

// newServer serves handler for the rest of the test and returns its URL.
func newServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts.URL
}

// authEnabledOf returns target_info.auth_enabled as the JSON report writes it.
func authEnabledOf(t *testing.T, stdout string) string {
	t.Helper()
	var report struct {
		TargetInfo map[string]json.RawMessage `json:"target_info"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("not a JSON report (%v):\n%s", err, stdout)
	}
	return string(report.TargetInfo["auth_enabled"])
}

// TestUnknownAuthPostureIsNotAuthOff: an /api/auth/status that does not
// answer Kula's JSON says nothing of the auth posture. Discovery does not
// read it as auth off: the checks that need the UI or API skip and say why,
// the rest still run, -expect auth is an ERROR, never a FAIL, and the JSON
// report's auth_enabled is null. Where the API could not be read the run is
// inconclusive (AUTH-000 ERROR, exit 3); where Kula serves none (web.ui:
// false) it is not.
func TestUnknownAuthPostureIsNotAuthOff(t *testing.T) {
	for _, mode := range []string{"502", "drop", "html", "absent"} {
		t.Run(mode, func(t *testing.T) {
			absent := mode == "absent"
			wantAuth, wantExit := StatusError, exitInconclusive
			why := "so whether web.auth is enabled is unknown"
			if absent {
				wantAuth, wantExit = StatusSkip, exitClean
				why = "Kula serves no UI or API at this address (web.ui: false"
			}

			code, stdout, stderr := runCaptured(t, "-json", newServer(t, postureTarget(mode)))
			report := decodeReport(t, stdout)
			if code != wantExit {
				t.Errorf("exit %d, want %d; stderr:\n%s", code, wantExit, stderr)
			}
			disc, _ := byID(report.Findings, "DISC-001")
			if disc.Status != StatusPass || strings.Contains(disc.Detail, "authentication disabled") {
				t.Errorf("DISC-001 = %s: %s; want a PASS that does not say auth is off", disc.Status, disc.Detail)
			}
			auth, _ := byID(report.Findings, "AUTH-000")
			if auth.Status != wantAuth || !strings.Contains(auth.Detail, why) {
				t.Errorf("AUTH-000 = %s: %s; want %s saying %q", auth.Status, auth.Detail, wantAuth, why)
			}
			// The checks that need the UI or API say why they did not run.
			for _, id := range []string{"AUTH-001", "HDR-001", "CSRF-001", "CORS-001", "HOST-001", "TRAV-001", "WS-000", "INPUT-001", "SRI-001"} {
				f, _ := byID(report.Findings, id)
				if f.Status != StatusSkip || !strings.Contains(f.Detail, why) {
					t.Errorf("%s = %s: %s; want a SKIP saying %q", id, f.Status, f.Detail, why)
				}
			}
			// Those that need neither still run.
			wantStatus(t, report.Findings, "PROM-001", StatusWarn)
			wantStatus(t, report.Findings, "REDIR-001", StatusPass)
			wantStatus(t, report.Findings, "TLS-001", StatusSkip)
			if got := authEnabledOf(t, stdout); got != "null" {
				t.Errorf("target_info.auth_enabled = %s, want null", got)
			}

			// -expect auth cannot be judged: an ERROR, never a FAIL.
			code, stdout, stderr = runCaptured(t, "-json", "-expect", "auth", newServer(t, postureTarget(mode)))
			report = decodeReport(t, stdout)
			auth, _ = byID(report.Findings, "AUTH-000")
			if code != exitInconclusive || auth.Status != StatusError || !strings.HasPrefix(auth.Detail, "-expect auth cannot be judged: ") || !strings.Contains(auth.Detail, why) {
				t.Errorf("-expect auth: exit %d, AUTH-000 = %s: %s; want exit 3 and an ERROR saying it cannot be judged\nstderr:\n%s",
					code, auth.Status, auth.Detail, stderr)
			}

			// So is another expectation whose findings need the UI.
			code, stdout, _ = runCaptured(t, "-json", "-expect", "frame-protection", newServer(t, postureTarget(mode)))
			report = decodeReport(t, stdout)
			for _, id := range []string{"HDR-002", "HDR-007"} {
				f, _ := byID(report.Findings, id)
				if f.Status != StatusError || f.Expectation != "frame-protection" || !strings.Contains(f.Detail, "-expect frame-protection cannot be judged") {
					t.Errorf("-expect frame-protection: %s = %s (%q): %s; want an ERROR saying it cannot be judged", id, f.Status, f.Expectation, f.Detail)
				}
			}
			if code != exitInconclusive {
				t.Errorf("-expect frame-protection: exit %d, want %d", code, exitInconclusive)
			}
		})
	}
}

// TestUnreadableAuthPostureReportsAUTH000: a posture that could not be read
// leaves inconclusive a run whose selected checks it kept from running, and
// AUTH-000 says so even when not selected; -skip AUTH-000 silences it, and a
// run of checks that need no API is not affected.
func TestUnreadableAuthPostureReportsAUTH000(t *testing.T) {
	code, stdout, _ := runCaptured(t, "-json", "-only", "headers", newServer(t, postureTarget("502")))
	report := decodeReport(t, stdout)
	auth, _ := byID(report.Findings, "AUTH-000")
	if code != exitInconclusive || auth.Status != StatusError {
		t.Errorf("-only headers: exit %d, AUTH-000 = %s: %s; want exit 3 and an ERROR", code, auth.Status, auth.Detail)
	}
	wantStatus(t, report.Findings, "HDR-001", StatusSkip)
	if ids := reportedIDs(report); len(ids) < 2 || ids[1] != "AUTH-000" {
		t.Errorf("AUTH-000 does not follow discovery: %v", ids)
	}

	code, stdout, _ = runCaptured(t, "-json", "-only", "headers", "-skip", "AUTH-000", newServer(t, postureTarget("502")))
	report = decodeReport(t, stdout)
	if _, reported := byID(report.Findings, "AUTH-000"); reported || code != exitClean {
		t.Errorf("-skip AUTH-000: exit %d, findings %v; want exit 0 without AUTH-000", code, reportedIDs(report))
	}

	code, stdout, _ = runCaptured(t, "-json", "-only", "metrics,redirect", newServer(t, postureTarget("502")))
	report = decodeReport(t, stdout)
	if _, reported := byID(report.Findings, "AUTH-000"); reported || code != exitClean {
		t.Errorf("-only metrics,redirect: exit %d, findings %v; want exit 0 without AUTH-000", code, reportedIDs(report))
	}
}

// TestUnknownAuthPostureInEveryFormat: JUnit and SARIF carry the unknown
// auth posture as the JSON report does, as null, and a known one as it is.
func TestUnknownAuthPostureInEveryFormat(t *testing.T) {
	for mode, want := range map[string]string{"absent": "null", "502": "null", "auth on": "true"} {
		handler := postureTarget(mode)
		if mode == "auth on" {
			handler = secureMock()
		}
		target := newServer(t, handler)
		_, stdout, _ := runCaptured(t, "-format", "junit", "-only", "discovery", target)
		if got := parseJUnit(t, stdout).properties()["target_info.auth_enabled"]; got != want {
			t.Errorf("%s: JUnit property target_info.auth_enabled = %q, want %q", mode, got, want)
		}
		_, stdout, _ = runCaptured(t, "-format", "sarif", "-only", "discovery", target)
		got, _ := json.Marshal(sarifPath(parseSARIF(t, stdout), "runs", 0, "invocations", 0, "properties", "target_info", "auth_enabled"))
		if string(got) != want {
			t.Errorf("%s: SARIF target_info.auth_enabled = %s, want %s", mode, got, want)
		}
	}
}
