package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// TestFormatJUnitOnStdout drives -format junit through the command line.
func TestFormatJUnitOnStdout(t *testing.T) {
	ts := httptest.NewServer(secureMock())
	defer ts.Close()
	code, stdout, stderr := runCaptured(t, "-format", "JUnit", "-only", "headers", ts.URL)
	if code != exitClean {
		t.Fatalf("exit %d; stderr:\n%s", code, stderr)
	}
	doc := parseJUnit(t, stdout)
	checkJUnitCounts(t, doc)
	if _, ok := doc.cases()["HDR-001 X-Content-Type-Options nosniff"]; !ok || doc.Failures+doc.Errors != 0 {
		t.Errorf("the JUnit report: %d failures, %d errors, cases %v", doc.Failures, doc.Errors, doc.cases())
	}
	if props := doc.properties(); props["target"] != ts.URL+"/" || props["exit_code"] != "0" {
		t.Errorf("properties %v", props)
	}
}

func TestFormatUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"-json", "-format", "junit"},
		{"-json", "-format", "Text"},
		{"-format", "yaml"},
	} {
		// No request is made: the target does not even resolve.
		code, stdout, stderr := runCaptured(t, append(args, "http://kula-scan.invalid")...)
		if code != exitUsage {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
		if !strings.Contains(stderr, strconv.Quote(args[len(args)-1])) {
			t.Errorf("%v: the error does not name the format as written:\n%s", args, stderr)
		}
		if stdout != "" {
			t.Errorf("%v: a usage error printed a report:\n%s", args, stdout)
		}
	}
	for _, c := range []struct {
		format string
		asJSON bool
		want   string
	}{
		{"", false, formatText},
		{"", true, formatJSON},
		{"json", true, formatJSON},
		{" JUnit ", false, formatJUnit},
		{"SARIF", false, formatSARIF},
	} {
		if got, err := resolveFormat(c.format, c.asJSON); err != nil || got != c.want {
			t.Errorf("resolveFormat(%q, %v) = %q, %v; want %q", c.format, c.asJSON, got, err, c.want)
		}
	}
}

// TestExpectAuthFailsOnceInEachFormat: a target with auth disabled scanned
// with -expect auth gives one JUnit failure and one SARIF error, and exit 1.
func TestExpectAuthFailsOnceInEachFormat(t *testing.T) {
	var loginHits, chatHits atomic.Int64
	ts := httptest.NewServer(authOffMock(false, &loginHits, &chatHits))
	defer ts.Close()
	args := []string{"-expect", "auth", "-only", "auth"}

	code, stdout, stderr := runCaptured(t, append(args, "-format", "junit", ts.URL)...)
	if code != exitFailing {
		t.Fatalf("exit %d, want 1; stderr:\n%s", code, stderr)
	}
	doc := parseJUnit(t, stdout)
	if doc.Failures != 1 || doc.Errors != 0 || doc.cases()["AUTH-000 Authentication posture"].Failure == nil {
		t.Errorf("JUnit: %d failures, %d errors:\n%s", doc.Failures, doc.Errors, stdout)
	}

	code, stdout, _ = runCaptured(t, append(args, "-format", "sarif", ts.URL)...)
	log := parseSARIF(t, stdout)
	var errs []string
	for _, result := range sarifList(log, "runs", 0, "results") {
		if sarifPath(result, "level") == "error" {
			errs = append(errs, sarifPath(result, "ruleId").(string))
		}
	}
	if code != exitFailing || len(errs) != 1 || errs[0] != "AUTH-000" {
		t.Errorf("SARIF: exit %d, error results %v", code, errs)
	}
}

// TestOutputFileTakesTheFormatAndStdoutTheText: -o writes the chosen format
// to the file, owner-only, while stdout gets the text report and the
// banners follow it.
func TestOutputFileTakesTheFormatAndStdoutTheText(t *testing.T) {
	ts := httptest.NewServer(secureMock())
	defer ts.Close()
	path := filepath.Join(t.TempDir(), "scan.sarif")
	args := []string{"-fuzz", "-fuzz-iter", "1", "-only", "headers,fuzz-liveness"}

	code, stdout, stderr := runCaptured(t, append(args, "-format", "sarif", "-o", path, ts.URL)...)
	if code != exitClean {
		t.Fatalf("exit %d; stderr:\n%s", code, stderr)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the report file has mode %v, want 0600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	log := parseSARIF(t, string(data))
	if sarifPath(log, "runs", 0, "invocations", 0, "exitCode") != float64(0) {
		t.Errorf("the SARIF file: %s", data)
	}
	if !strings.Contains(stdout, "HDR-001") || !strings.Contains(stdout, "Summary:") || strings.HasPrefix(stdout, "{") {
		t.Errorf("stdout does not hold the text report:\n%s", stdout)
	}
	if !strings.Contains(stderr, "WARNING: FUZZ MODE") {
		t.Errorf("the fuzz banner is missing with text on stdout:\n%s", stderr)
	}

	// Without -o, SARIF goes to stdout and the banner stays away.
	code, stdout, stderr = runCaptured(t, append(args, "-format", "sarif", ts.URL)...)
	if code != exitClean || strings.Contains(stderr, "FUZZ MODE") {
		t.Errorf("exit %d; stderr:\n%s", code, stderr)
	}
	parseSARIF(t, stdout)

	// -json with -o: JSON in the file, text on stdout.
	jsonPath := filepath.Join(t.TempDir(), "scan.json")
	_, stdout, _ = runCaptured(t, "-json", "-o", jsonPath, "-only", "headers", ts.URL)
	data, _ = os.ReadFile(jsonPath)
	if report := decodeReport(t, string(data)); len(report.Findings) == 0 || !strings.Contains(stdout, "Summary:") {
		t.Errorf("-json -o: file %s; stdout:\n%s", data, stdout)
	}
}

// TestUnwritableOutputFailsBeforeTheScan: a report file that cannot be
// written is refused before a request goes out.
func TestUnwritableOutputFailsBeforeTheScan(t *testing.T) {
	var requests atomic.Int64
	mock := secureMock()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		mock.ServeHTTP(w, r)
	}))
	defer ts.Close()
	dir := t.TempDir()
	for _, path := range []string{filepath.Join(dir, "missing", "scan.sarif"), dir} {
		code, stdout, stderr := runCaptured(t, "-format", "sarif", "-o", path, ts.URL)
		if code != exitUsage {
			t.Errorf("-o %s: exit %d, want 2", path, code)
		}
		if !strings.Contains(stderr, "-o") || stdout != "" {
			t.Errorf("-o %s: stdout %q, stderr %q", path, stdout, stderr)
		}
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("%d requests reached the target before the report file was refused", n)
	}
}

// failingWriter refuses every write.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// TestReportWriteErrorsExit2: a report that cannot be written after the scan,
// to the -o file or to stdout, is an error and exit status 2, not a clean run.
func TestReportWriteErrorsExit2(t *testing.T) {
	ts := httptest.NewServer(secureMock())
	defer ts.Close()

	if _, err := os.Stat("/dev/full"); err == nil {
		code, _, stderr := runCaptured(t, "-format", "junit", "-o", "/dev/full", "-only", "headers", ts.URL)
		if code != exitUsage || !strings.Contains(stderr, "error: -o:") {
			t.Errorf("-o /dev/full: exit %d; stderr:\n%s", code, stderr)
		}
	}

	var stderr bytes.Buffer
	code := runCLI(context.Background(), []string{"-only", "headers", ts.URL}, failingWriter{}, &stderr)
	if code != exitUsage || !strings.Contains(stderr.String(), "error writing the text report: disk full") {
		t.Errorf("a stdout that refuses writes: exit %d; stderr:\n%s", code, stderr.String())
	}
}
