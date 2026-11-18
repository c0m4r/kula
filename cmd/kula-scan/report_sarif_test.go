package main

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// parseSARIF decodes a SARIF report into maps, failing the test unless it is
// JSON free of terminal controls.
func parseSARIF(t *testing.T, text string) map[string]any {
	t.Helper()
	if bad, found := firstTerminalControl(text); found {
		t.Fatalf("the SARIF report writes %q to the terminal:\n%s", bad, text)
	}
	var log map[string]any
	if err := json.Unmarshal([]byte(text), &log); err != nil {
		t.Fatalf("not JSON (%v):\n%s", err, text)
	}
	return log
}

// sarifPath walks decoded JSON: a string steps into an object, an int into
// an array. It returns nil when the path does not exist.
func sarifPath(v any, path ...any) any {
	for _, step := range path {
		switch s := step.(type) {
		case string:
			object, ok := v.(map[string]any)
			if !ok {
				return nil
			}
			v = object[s]
		case int:
			list, ok := v.([]any)
			if !ok || s >= len(list) {
				return nil
			}
			v = list[s]
		}
	}
	return v
}

func sarifList(v any, path ...any) []any {
	list, _ := sarifPath(v, path...).([]any)
	return list
}

func TestSARIFVerdictMatchesTheExitStatus(t *testing.T) {
	for _, c := range formatCases() {
		r := c.buildReport()
		var out bytes.Buffer
		if err := r.writeSARIF(&out, c.failOn); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		log := parseSARIF(t, out.String())
		if log["version"] != "2.1.0" || !strings.HasSuffix(log["$schema"].(string), "/sarif-schema-2.1.0.json") {
			t.Fatalf("%s: version %v, $schema %v", c.name, log["version"], log["$schema"])
		}
		if runs := sarifList(log, "runs"); len(runs) != 1 {
			t.Fatalf("%s: %d runs, want 1", c.name, len(runs))
		}
		run := sarifPath(log, "runs", 0)
		driver := sarifPath(run, "tool", "driver")
		if sarifPath(driver, "name") != "kula-scan" || sarifPath(driver, "version") != "9.9.9" ||
			sarifPath(driver, "informationUri") != projectURL {
			t.Errorf("%s: driver %v", c.name, driver)
		}
		rules := sarifList(driver, "rules")
		if len(rules) != len(catalog) {
			t.Errorf("%s: %d rules, want the %d of the catalog", c.name, len(rules), len(catalog))
		}

		invocation := sarifPath(run, "invocations", 0)
		if code := sarifPath(invocation, "exitCode"); code != float64(r.ExitCode) {
			t.Errorf("%s: exitCode %v, want %d", c.name, code, r.ExitCode)
		}
		wantSuccess := r.ExitCode != exitUsage && r.ExitCode != exitInconclusive && r.ExitCode != exitInterrupted
		if sarifPath(invocation, "executionSuccessful") != wantSuccess {
			t.Errorf("%s: executionSuccessful %v for exit %d", c.name, sarifPath(invocation, "executionSuccessful"), r.ExitCode)
		}
		if sarifPath(invocation, "startTimeUtc") != "2026-11-17T10:00:00Z" ||
			sarifPath(invocation, "endTimeUtc") != "2026-11-17T10:00:01.234Z" {
			t.Errorf("%s: start %v, end %v", c.name, sarifPath(invocation, "startTimeUtc"), sarifPath(invocation, "endTimeUtc"))
		}
		notes := sarifList(invocation, "toolExecutionNotifications")
		if len(notes) != c.notes {
			t.Errorf("%s: %d notifications, want %d", c.name, len(notes), c.notes)
		}
		for _, note := range notes {
			id, _ := sarifPath(note, "associatedRule", "id").(string)
			want := "error"
			if strings.EqualFold(id, c.ignore) {
				want = "warning"
			}
			if sarifPath(note, "level") != want {
				t.Errorf("%s: notification for %s at level %v, want %s", c.name, id, sarifPath(note, "level"), want)
			}
		}

		results := sarifList(run, "results")
		if len(results) != len(c.sarif) {
			t.Errorf("%s: %d results, want %d", c.name, len(results), len(c.sarif))
		}
		unsuppressedErrors := 0
		for _, result := range results {
			id, _ := sarifPath(result, "ruleId").(string)
			level := sarifPath(result, "level")
			if level != c.sarif[id] {
				t.Errorf("%s: %s at level %v, want %q", c.name, id, level, c.sarif[id])
			}
			index, _ := sarifPath(result, "ruleIndex").(float64)
			if sarifPath(rules, int(index), "id") != id {
				t.Errorf("%s: %s points at rule %v", c.name, id, sarifPath(rules, int(index), "id"))
			}
			if sarifPath(result, "locations", 0, "physicalLocation", "artifactLocation", "uri") != r.Target {
				t.Errorf("%s: %s location %v", c.name, id, sarifPath(result, "locations"))
			}
			f, _ := byID(c.findings, id)
			props := sarifPath(result, "properties")
			if sarifPath(props, "severity") != f.Severity.String() || sarifPath(props, "status") != f.Status.String() ||
				sarifPath(props, "security-severity") != securitySeverity(f.Severity) {
				t.Errorf("%s: %s properties %v", c.name, id, props)
			}
			// GitHub code scanning reads the alert's severity from the rule.
			if got := sarifPath(rules, int(index), "properties", "security-severity"); got != securitySeverity(f.Severity) {
				t.Errorf("%s: rule %s security-severity %v, want %s", c.name, id, got, securitySeverity(f.Severity))
			}
			if f.Expectation != "" && sarifPath(props, "expectation") != f.Expectation {
				t.Errorf("%s: %s expectation %v, want %s", c.name, id, sarifPath(props, "expectation"), f.Expectation)
			}
			suppressions := sarifList(result, "suppressions")
			if ignored := strings.EqualFold(id, c.ignore); ignored != (len(suppressions) == 1) ||
				ignored && sarifPath(suppressions, 0, "kind") != "external" {
				t.Errorf("%s: %s suppressions %v", c.name, id, suppressions)
			}
			if level == "error" && len(suppressions) == 0 {
				unsuppressedErrors++
			}
		}
		// An error-level result that is not suppressed is what fails the run.
		gating := false
		for _, f := range r.Findings {
			gating = gating || gates(f, c.failOn)
		}
		if (unsuppressedErrors > 0) != gating {
			t.Errorf("%s: %d unsuppressed error results, gating FAIL %v", c.name, unsuppressedErrors, gating)
		}
	}
}

func TestSARIFRulesComeFromTheCatalog(t *testing.T) {
	var out bytes.Buffer
	if err := newReport("http://t/", nil).writeSARIF(&out, SevHigh); err != nil {
		t.Fatal(err)
	}
	log := parseSARIF(t, out.String())
	rules := sarifList(log, "runs", 0, "tool", "driver", "rules")
	for i, e := range catalog {
		rule := sarifPath(rules, i)
		if sarifPath(rule, "id") != e.ID || sarifPath(rule, "shortDescription", "text") != e.Title ||
			sarifPath(rule, "fullDescription", "text") != e.Description ||
			sarifPath(rule, "properties", "category") != e.Category || sarifPath(rule, "properties", "mode") != e.Mode.String() {
			t.Errorf("rule %d = %v, want %+v", i, rule, e)
		}
		if name, _ := sarifPath(rule, "name").(string); name == "" || strings.ContainsAny(name, " -'()/,") {
			t.Errorf("rule %s has name %q, want an identifier", e.ID, name)
		}
	}
	if results := sarifList(log, "runs", 0, "results"); results == nil {
		t.Error("results is null, want []")
	}
	if got := ruleName("HSTS over TLS"); got != "HSTSOverTLS" {
		t.Errorf("ruleName = %q", got)
	}
}

// TestSARIFSurvivesHostileText: SARIF goes through the JSON report's
// encoder, so both are valid JSON safe to print, and decode to the target's
// text.
func TestSARIFSurvivesHostileText(t *testing.T) {
	r := hostileReport()
	for name, write := range map[string]func(io.Writer) error{
		"JSON":  r.writeJSON,
		"SARIF": func(w io.Writer) error { return r.writeSARIF(w, SevHigh) },
	} {
		var out bytes.Buffer
		if err := write(&out); err != nil {
			t.Fatal(err)
		}
		if !json.Valid(out.Bytes()) {
			t.Fatalf("%s: not valid JSON:\n%s", name, out.String())
		}
		if bad, found := firstTerminalControl(out.String()); found {
			t.Fatalf("%s: writes %q to the terminal:\n%s", name, bad, out.String())
		}
	}
	var sarif bytes.Buffer
	if err := r.writeSARIF(&sarif, SevHigh); err != nil {
		t.Fatal(err)
	}
	log := parseSARIF(t, sarif.String())
	text, _ := sarifPath(log, "runs", 0, "results", 0, "message", "text").(string)
	// Invalid UTF-8 comes back replaced; the rest is the target's text.
	if !strings.Contains(text, "evidence \x1b[2J\x00]]></testcase>") || !strings.Contains(text, "\u202e\ufffd") {
		t.Errorf("the SARIF message does not decode to the target's text: %q", text)
	}
}
