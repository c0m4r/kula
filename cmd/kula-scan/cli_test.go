package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"kula"
)

// runCaptured runs the command line with its output captured.
func runCaptured(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	return runCapturedContext(t, context.Background(), args...)
}

// runCapturedContext runs the command line under ctx with its output captured.
func runCapturedContext(t *testing.T, ctx context.Context, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = runCLI(ctx, args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestVerboseJSONKeepsStdoutJSON(t *testing.T) {
	ts := httptest.NewServer(secureMock())
	defer ts.Close()

	code, stdout, stderr := runCaptured(t, "-v", "-json", "-only", "headers,metrics", ts.URL)
	if code != 0 {
		t.Fatalf("exit %d; stderr:\n%s", code, stderr)
	}
	var report Report
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("-v -json printed something other than JSON: %v\n%s", err, stdout)
	}
	if _, ok := byID(report.Findings, "HDR-001"); !ok {
		t.Errorf("the JSON report has no HDR-001: %s", stdout)
	}
	if !strings.Contains(stderr, "GET /metrics -> 401") {
		t.Errorf("-v did not log the requests to stderr:\n%s", stderr)
	}
	// -v also turns on progress, one line per check.
	if !strings.Contains(stderr, "] headers: HDR-000") || !strings.Contains(stderr, "] metrics: PROM-001, PROM-002") {
		t.Errorf("-v did not log progress lines to stderr:\n%s", stderr)
	}
}

func TestQuietWhenStderrIsNotATerminal(t *testing.T) {
	ts := httptest.NewServer(secureMock())
	defer ts.Close()

	code, stdout, stderr := runCaptured(t, "-only", "headers", ts.URL)
	if code != 0 {
		t.Fatalf("exit %d; stderr:\n%s", code, stderr)
	}
	if stderr != "" {
		t.Errorf("a clean run without -v wrote to a stderr that is not a terminal:\n%s", stderr)
	}
	if !strings.Contains(stdout, "HDR-001") {
		t.Errorf("the text report is missing:\n%s", stdout)
	}
}

func TestJSONReportCarriesItsMetadata(t *testing.T) {
	ts := httptest.NewServer(secureMock())
	defer ts.Close()

	before := time.Now().UTC().Truncate(time.Second)
	// HDR-008 is skipped: over plain HTTP it fails -expect hsts. Each
	// expectation keeps a finding that judges it.
	code, stdout, stderr := runCaptured(t, "-json", "-fuzz", "-fuzz-iter", "3", "-seed", "42",
		"-only", "headers,auth-000,fuzz-liveness", "-skip", "hdr-009,hdr-008", "-ignore", "hdr-003", "-expect", "frame-protection,auth",
		"-fail-on", "MEDIUM", ts.URL)
	if code != exitClean {
		t.Fatalf("exit %d; stderr:\n%s", code, stderr)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"scanner", "target", "started", "duration_ms", "options", "target_info",
		"findings", "summary", "interrupted", "exit_code"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("the JSON report has no %q", key)
		}
	}
	var started string
	_ = json.Unmarshal(raw["started"], &started)
	if at, err := time.Parse(time.RFC3339, started); err != nil || at.Before(before) {
		t.Errorf("started = %q, want RFC 3339 no earlier than %s", started, before)
	}

	report := decodeReport(t, stdout)
	if report.Scanner != (ScannerInfo{Name: "kula-scan", Version: kula.Version}) {
		t.Errorf("scanner = %+v", report.Scanner)
	}
	want := ReportOptions{
		Aggressive: false, Fuzz: true, Seed: 42,
		Only: []string{"headers", "AUTH-000", "FUZZ-LIVENESS"}, Skip: []string{"HDR-009", "HDR-008"},
		Ignore: []string{"HDR-003"}, Expect: []string{"frame-protection", "auth"}, FailOn: "medium",
	}
	if !reflect.DeepEqual(report.Options, want) {
		t.Errorf("options = %+v, want %+v", report.Options, want)
	}
	if report.TargetInfo != (TargetInfo{HTTPS: false, AuthEnabled: true}) {
		t.Errorf("target_info = %+v", report.TargetInfo)
	}
	if report.ExitCode != code || report.Interrupted || report.DurationMS < 0 {
		t.Errorf("exit_code %d, interrupted %v, duration_ms %d", report.ExitCode, report.Interrupted, report.DurationMS)
	}

	// Unset lists are empty, not null, and the seed is absent without -fuzz.
	_, stdout, _ = runCaptured(t, "-json", "-only", "headers", ts.URL)
	_ = json.Unmarshal([]byte(stdout), &raw)
	var options map[string]json.RawMessage
	_ = json.Unmarshal(raw["options"], &options)
	if string(options["skip"]) != "[]" || string(options["expect"]) != "[]" {
		t.Errorf("unset lists encode as %s and %s, want []", options["skip"], options["expect"])
	}
	if _, ok := options["seed"]; ok {
		t.Errorf("the seed is reported without -fuzz: %s", raw["options"])
	}
}

func TestVersionFlag(t *testing.T) {
	code, stdout, _ := runCaptured(t, "-version")
	if code != 0 || stdout != "kula-scan "+kula.Version+"\n" {
		t.Errorf("-version: exit %d, output %q", code, stdout)
	}
}

func TestListNeedsNoTarget(t *testing.T) {
	code, stdout, _ := runCaptured(t, "-list")
	if code != 0 || !strings.Contains(stdout, "DOS-SLOWLORIS") || !strings.Contains(stdout, "aggressive") {
		t.Errorf("-list: exit %d, output:\n%s", code, stdout)
	}
}
