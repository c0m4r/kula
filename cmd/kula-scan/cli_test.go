package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

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
