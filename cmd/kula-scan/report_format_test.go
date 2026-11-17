package main

import (
	"net/http/httptest"
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
	} {
		if got, err := resolveFormat(c.format, c.asJSON); err != nil || got != c.want {
			t.Errorf("resolveFormat(%q, %v) = %q, %v; want %q", c.format, c.asJSON, got, err, c.want)
		}
	}
}

// TestExpectAuthFailsOnceInEachFormat: a target with auth disabled scanned
// with -expect auth gives one JUnit failure, and exit 1.
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
}
