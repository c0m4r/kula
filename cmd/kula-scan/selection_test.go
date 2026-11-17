package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestUnknownSelectionTokensAreUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"-only", "header"},
		{"-only", "headers,CSRF-999"},
		{"-skip", "nope"},
		{"-ignore", "headers"}, // a category, not a finding ID
		{"-ignore", "AUTH-005,NOPE-1"},
	} {
		// No request is made: the target does not even resolve.
		code, stdout, stderr := runCaptured(t, append(args, "http://kula-scan.invalid")...)
		if code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
		// The token is named as written.
		bad := args[1][strings.LastIndex(args[1], ",")+1:]
		if !strings.Contains(stderr, `"`+bad+`"`) {
			t.Errorf("%v: the error does not name %q:\n%s", args, bad, stderr)
		}
		if args[0] != "-ignore" && !strings.Contains(stderr, "headers, auth, csrf") {
			t.Errorf("%v: the error does not list the categories:\n%s", args, stderr)
		}
		if stdout != "" {
			t.Errorf("%v: a usage error printed a report:\n%s", args, stdout)
		}
	}
}

// requestLog records the paths a scan requests.
type requestLog struct {
	mu    sync.Mutex
	paths map[string]int
}

func (l *requestLog) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		l.paths[r.URL.Path]++
		l.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

func (l *requestLog) count(path string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.paths[path]
}

func scanJSON(t *testing.T, handler http.Handler, args ...string) (int, Report, string) {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	code, stdout, stderr := runCaptured(t, append(append([]string{"-json"}, args...), ts.URL)...)
	return code, decodeReport(t, stdout), stderr
}

// decodeReport decodes a JSON report, restoring the typed status and
// severity the JSON carries only as strings.
func decodeReport(t *testing.T, text string) Report {
	t.Helper()
	var report Report
	if err := json.Unmarshal([]byte(text), &report); err != nil {
		t.Fatalf("not a JSON report (%v):\n%s", err, text)
	}
	for i := range report.Findings {
		f := &report.Findings[i]
		for st := StatusPass; st <= StatusError; st++ {
			if st.String() == f.StatusStr {
				f.Status = st
			}
		}
		if sev, ok := parseSeverity(f.SeverityStr); ok {
			f.Severity = sev
		}
	}
	return report
}

func reportedIDs(report Report) []string {
	ids := make([]string, 0, len(report.Findings))
	for _, f := range report.Findings {
		ids = append(ids, f.ID)
	}
	return ids
}

func TestOnlySelectsFindingIDs(t *testing.T) {
	log := &requestLog{paths: map[string]int{}}
	t.Setenv(passwordEnv, mockPass)
	code, report, _ := scanJSON(t, log.wrap(secureMock()), "-username", mockUser, "-only", "csrf-003")
	if code != 0 {
		t.Errorf("exit %d", code)
	}
	// DISC-001 always shows: it says whether the target was scanned.
	if got := strings.Join(reportedIDs(report), ","); got != "DISC-001,CSRF-003" {
		t.Errorf("-only csrf-003 reported %s", got)
	}
	wantStatus(t, report.Findings, "CSRF-003", StatusPass)
	// /style.css: the header check; /fonts/: only the traversal check.
	if log.count("/metrics") != 0 || log.count("/style.css") != 0 || log.count("/fonts/") != 0 {
		t.Errorf("-only csrf-003 ran other checks: %v", log.paths)
	}
}

func TestSkipDropsOrFiltersChecks(t *testing.T) {
	log := &requestLog{paths: map[string]int{}}
	_, report, _ := scanJSON(t, log.wrap(secureMock()), "-skip", "PROM-001,prom-002,prom-003,HDR-009,traversal")
	if log.count("/metrics") != 0 {
		t.Error("the metrics check ran with all of its IDs skipped")
	}
	if log.count("/fonts/") != 0 { // only the traversal check requests it
		t.Error("the traversal check ran with its category skipped")
	}
	for _, id := range []string{"PROM-001", "PROM-002", "PROM-003", "HDR-009", "TRAV-001"} {
		if _, ok := byID(report.Findings, id); ok {
			t.Errorf("-skip reported %s", id)
		}
	}
	// The rest of the headers check still runs.
	wantStatus(t, report.Findings, "HDR-001", StatusPass)
}

// noCSP serves the secure mock without its Content-Security-Policy on the
// UI page, which fails HDR-003 (HIGH) and nothing else.
func noCSP() http.Handler {
	next := secureMock()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w = &dropHeader{ResponseWriter: w, name: "Content-Security-Policy"}
		}
		next.ServeHTTP(w, r)
	})
}

type dropHeader struct {
	http.ResponseWriter
	name string
}

func (d *dropHeader) WriteHeader(code int) {
	d.Header().Del(d.name)
	d.ResponseWriter.WriteHeader(code)
}

func (d *dropHeader) Write(b []byte) (int, error) {
	d.Header().Del(d.name)
	return d.ResponseWriter.Write(b)
}

func TestIgnoringTheOnlyFailureExitsZero(t *testing.T) {
	t.Setenv(passwordEnv, mockPass)
	code, report, stderr := scanJSON(t, noCSP(), "-username", mockUser)
	if code != 1 {
		t.Fatalf("without -ignore: exit %d, want 1; stderr:\n%s", code, stderr)
	}
	if fails := report.failingIDs(SevInfo); len(fails) != 1 || fails[0] != "HDR-003" {
		t.Fatalf("the fixture should fail HDR-003 alone, fails %v", fails)
	}

	code, report, stderr = scanJSON(t, noCSP(), "-username", mockUser, "-ignore", "hdr-003")
	if code != 0 {
		t.Fatalf("-ignore hdr-003: exit %d, want 0; stderr:\n%s", code, stderr)
	}
	f, _ := byID(report.Findings, "HDR-003")
	if f.Status != StatusFail || !f.Ignored {
		t.Errorf("HDR-003 should stay a FAIL, marked ignored: %+v", f)
	}
	if report.Summary.Ignored != 1 || report.Summary.Fail != 0 {
		t.Errorf("summary %+v, want the FAIL counted as ignored", report.Summary)
	}
}

func TestTextReportMarksIgnoredFindings(t *testing.T) {
	findings := []Finding{finding("HDR-003", SevHigh, StatusFail, "no CSP")}
	markIgnored(findings, map[string]bool{"hdr-003": true})
	var out strings.Builder
	newReport("http://t", findings).writeText(&out, true)
	if !strings.Contains(out.String(), "HDR-003    Content-Security-Policy hardened [HIGH] (ignored)") ||
		!strings.Contains(out.String(), "1 ignored") {
		t.Errorf("the text report does not mark the ignored finding:\n%s", out.String())
	}
}
