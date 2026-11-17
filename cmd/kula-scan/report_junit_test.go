package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"
)

// junitDoc decodes a JUnit report independently of the writer's types.
type junitDoc struct {
	XMLName  xml.Name        `xml:"testsuites"`
	Name     string          `xml:"name,attr"`
	Tests    int             `xml:"tests,attr"`
	Failures int             `xml:"failures,attr"`
	Errors   int             `xml:"errors,attr"`
	Skipped  int             `xml:"skipped,attr"`
	Time     string          `xml:"time,attr"`
	Suites   []junitDocSuite `xml:"testsuite"`
}

type junitDocSuite struct {
	Name       string `xml:"name,attr"`
	Tests      int    `xml:"tests,attr"`
	Failures   int    `xml:"failures,attr"`
	Errors     int    `xml:"errors,attr"`
	Skipped    int    `xml:"skipped,attr"`
	Properties []struct {
		Name  string `xml:"name,attr"`
		Value string `xml:"value,attr"`
	} `xml:"properties>property"`
	Cases []junitDocCase `xml:"testcase"`
}

type junitDocCase struct {
	Name      string           `xml:"name,attr"`
	Classname string           `xml:"classname,attr"`
	Failure   *junitDocOutcome `xml:"failure"`
	Error     *junitDocOutcome `xml:"error"`
	Skipped   *junitDocOutcome `xml:"skipped"`
	SystemOut string           `xml:"system-out"`
}

type junitDocOutcome struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Text    string `xml:",chardata"`
}

// outcome names what a JUnit consumer makes of the testcase.
func (c junitDocCase) outcome() string {
	switch {
	case c.Failure != nil:
		return "failure"
	case c.Error != nil:
		return "error"
	case c.Skipped != nil:
		return "skipped"
	default:
		return "pass"
	}
}

// parseJUnit decodes a JUnit report, failing the test unless the whole text
// is one well-formed document free of terminal controls.
func parseJUnit(t *testing.T, text string) junitDoc {
	t.Helper()
	if bad, found := firstTerminalControl(text); found {
		t.Fatalf("the JUnit report writes %q to the terminal:\n%s", bad, text)
	}
	dec := xml.NewDecoder(strings.NewReader(text))
	for {
		_, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("the JUnit report is not well-formed XML (%v):\n%s", err, text)
		}
	}
	var doc junitDoc
	if err := xml.Unmarshal([]byte(text), &doc); err != nil {
		t.Fatalf("not a JUnit report (%v):\n%s", err, text)
	}
	return doc
}

// cases returns every testcase of the report by name.
func (d junitDoc) cases() map[string]junitDocCase {
	out := map[string]junitDocCase{}
	for _, s := range d.Suites {
		for _, c := range s.Cases {
			out[c.Name] = c
		}
	}
	return out
}

// properties returns the properties of the first suite.
func (d junitDoc) properties() map[string]string {
	out := map[string]string{}
	if len(d.Suites) > 0 {
		for _, p := range d.Suites[0].Properties {
			out[p.Name] = p.Value
		}
	}
	return out
}

// checkJUnitCounts checks every count against the testcases it covers.
func checkJUnitCounts(t *testing.T, doc junitDoc) {
	t.Helper()
	var tests, failures, errs, skipped int
	for _, s := range doc.Suites {
		var sf, se, ss int
		for _, c := range s.Cases {
			switch c.outcome() {
			case "failure":
				sf++
			case "error":
				se++
			case "skipped":
				ss++
			}
		}
		if s.Tests != len(s.Cases) || s.Failures != sf || s.Errors != se || s.Skipped != ss {
			t.Errorf("suite %s counts tests=%d failures=%d errors=%d skipped=%d, holds %d/%d/%d/%d",
				s.Name, s.Tests, s.Failures, s.Errors, s.Skipped, len(s.Cases), sf, se, ss)
		}
		tests += len(s.Cases)
		failures += sf
		errs += se
		skipped += ss
	}
	if doc.Tests != tests || doc.Failures != failures || doc.Errors != errs || doc.Skipped != skipped {
		t.Errorf("root counts tests=%d failures=%d errors=%d skipped=%d, suites hold %d/%d/%d/%d",
			doc.Tests, doc.Failures, doc.Errors, doc.Skipped, tests, failures, errs, skipped)
	}
}

type formatCase struct {
	name        string
	findings    []Finding
	ignore      string // a finding ID -ignore names
	failOn      Severity
	interrupted bool
	wantCode    int
	junit       map[string]string // outcome by testcase name; the rest pass
	sarif       map[string]string // result level by rule ID; the rest are no result
	notes       int               // tool execution notifications
}

func formatCases() []formatCase {
	return []formatCase{
		{
			name: "clean",
			findings: []Finding{
				finding("DISC-001", SevInfo, StatusPass, "Kula"),
				finding("HDR-001", SevInfo, StatusPass, ""),
				finding("HDR-009", SevLow, StatusWarn, "Server: nginx").withEvidence("Server: nginx"),
				finding("TLS-001", SevInfo, StatusSkip, "plain HTTP"),
			},
			failOn:   SevHigh,
			wantCode: exitClean,
			junit:    map[string]string{"TLS-001 TLS protocol version": "skipped"},
			sarif:    map[string]string{"HDR-009": "note"},
		},
		{
			name: "gating FAIL",
			findings: []Finding{
				finding("DISC-001", SevInfo, StatusPass, "Kula"),
				finding("HDR-003", SevHigh, StatusFail, "no CSP").withRemediation("set a CSP"),
			},
			failOn:   SevHigh,
			wantCode: exitFailing,
			junit:    map[string]string{"HDR-003 Content-Security-Policy hardened": "failure"},
			sarif:    map[string]string{"HDR-003": "error"},
		},
		{
			name: "FAIL below the threshold",
			findings: []Finding{
				finding("DISC-001", SevInfo, StatusPass, "Kula"),
				finding("HDR-009", SevLow, StatusFail, "Server: nginx"),
			},
			failOn:   SevHigh,
			wantCode: exitClean,
			sarif:    map[string]string{"HDR-009": "warning"},
		},
		{
			name: "unmet expectation",
			findings: []Finding{
				finding("DISC-001", SevInfo, StatusPass, "Kula"),
				finding("AUTH-000", SevMedium, StatusFail, "web.auth is disabled").expecting("auth"),
			},
			failOn:   SevCritical,
			wantCode: exitFailing,
			junit:    map[string]string{"AUTH-000 Authentication posture": "failure"},
			sarif:    map[string]string{"AUTH-000": "error"},
		},
		{
			name: "ERROR only",
			findings: []Finding{
				finding("DISC-001", SevInfo, StatusPass, "Kula"),
				finding("AUTH-005", SevInfo, StatusError, "wrong username or password"),
			},
			failOn:   SevHigh,
			wantCode: exitInconclusive,
			junit:    map[string]string{"AUTH-005 Login with supplied credentials": "error"},
			notes:    1,
		},
		{
			name: "ignored FAIL",
			findings: []Finding{
				finding("DISC-001", SevInfo, StatusPass, "Kula"),
				finding("HDR-003", SevHigh, StatusFail, "no CSP"),
			},
			ignore:   "HDR-003",
			failOn:   SevHigh,
			wantCode: exitClean,
			junit:    map[string]string{"HDR-003 Content-Security-Policy hardened": "skipped"},
			sarif:    map[string]string{"HDR-003": "error"}, // suppressed
		},
		{
			name: "interrupted",
			findings: []Finding{
				finding("DISC-001", SevInfo, StatusPass, "Kula"),
				finding("HDR-001", SevInfo, StatusPass, ""),
			},
			failOn:      SevHigh,
			interrupted: true,
			wantCode:    exitInterrupted,
			junit:       map[string]string{"Scan completed": "error"},
		},
		{
			name:     "stopped at discovery",
			findings: []Finding{finding("DISC-001", SevInfo, StatusError, "could not reach the target")},
			failOn:   SevHigh,
			wantCode: exitUsage,
			junit:    map[string]string{"DISC-001 Target fingerprint": "error"},
			notes:    1,
		},
		{
			name:     "stopped at an ignored discovery",
			findings: []Finding{finding("DISC-001", SevInfo, StatusError, "could not reach the target")},
			ignore:   "DISC-001",
			failOn:   SevHigh,
			wantCode: exitUsage,
			junit:    map[string]string{"DISC-001 Target fingerprint": "skipped", "Scan completed": "error"},
			notes:    1,
		},
	}
}

// buildReport assembles a report the way runCLI does.
func (c formatCase) buildReport() *Report {
	findings := append([]Finding(nil), c.findings...)
	if c.ignore != "" {
		markIgnored(findings, map[string]bool{strings.ToLower(c.ignore): true})
	}
	r := newReport("http://127.0.0.1:27960/", findings)
	r.Scanner = ScannerInfo{Name: "kula-scan", Version: "9.9.9"}
	r.Started = time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	r.DurationMS = 1234
	r.Options = ReportOptions{Only: []string{}, Skip: []string{}, Ignore: []string{}, Expect: []string{"auth"},
		FailOn: strings.ToLower(c.failOn.String()), Fuzz: true, Seed: 1763372800123456789}
	if c.interrupted {
		r.Interrupted, r.NotRun = true, 4
	}
	r.ExitCode = r.exitCode(c.failOn)
	return r
}

func TestJUnitVerdictMatchesTheExitStatus(t *testing.T) {
	for _, c := range formatCases() {
		r := c.buildReport()
		if r.ExitCode != c.wantCode {
			t.Fatalf("%s: the fixture exits %d, want %d", c.name, r.ExitCode, c.wantCode)
		}
		var out bytes.Buffer
		if err := r.writeJUnit(&out, c.failOn); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		doc := parseJUnit(t, out.String())
		checkJUnitCounts(t, doc)
		if n := strings.Count(out.String(), "<properties>"); n != 1 {
			t.Errorf("%s: %d <properties> elements, want the first suite's alone", c.name, n)
		}

		// The consumer's verdict is the exit status.
		if failed := doc.Failures+doc.Errors > 0; failed != (r.ExitCode != exitClean) {
			t.Errorf("%s: %d failures and %d errors for exit %d", c.name, doc.Failures, doc.Errors, r.ExitCode)
		}
		if r.ExitCode == exitFailing && doc.Failures == 0 {
			t.Errorf("%s: exit 1 without a failure", c.name)
		}
		if r.ExitCode == exitInconclusive && (doc.Errors == 0 || doc.Failures > 0) {
			t.Errorf("%s: exit 3 with %d errors, %d failures", c.name, doc.Errors, doc.Failures)
		}

		cases := doc.cases()
		wantCases := len(c.findings)
		if _, ok := c.junit["Scan completed"]; ok {
			wantCases++
		}
		if doc.Tests != wantCases || len(cases) != wantCases {
			t.Errorf("%s: %d testcases, want %d", c.name, doc.Tests, wantCases)
		}
		for _, f := range c.findings {
			name := f.ID + " " + f.Title
			tc, ok := cases[name]
			if !ok {
				t.Errorf("%s: no testcase %q", c.name, name)
				continue
			}
			if want := "kula-scan." + f.Category; tc.Classname != want {
				t.Errorf("%s: %s classname %q, want %q", c.name, f.ID, tc.Classname, want)
			}
			if !strings.Contains(tc.SystemOut, "status: "+f.Status.String()) ||
				!strings.Contains(tc.SystemOut, "severity: "+f.Severity.String()) ||
				!strings.Contains(tc.SystemOut, f.Detail) || !strings.Contains(tc.SystemOut, f.Evidence) {
				t.Errorf("%s: %s system-out drops part of the finding:\n%s", c.name, f.ID, tc.SystemOut)
			}
		}
		for name, tc := range cases {
			want := c.junit[name]
			if want == "" {
				want = "pass"
			}
			if got := tc.outcome(); got != want {
				t.Errorf("%s: testcase %q is a %s, want a %s", c.name, name, got, want)
			}
			if tc.Failure != nil && tc.Failure.Message == "" {
				t.Errorf("%s: testcase %q fails without a message", c.name, name)
			}
		}
		if c.ignore != "" {
			tc := cases[c.ignore+" "+catalogEntryFor(c.ignore).Title]
			if tc.Skipped == nil || !strings.Contains(tc.Skipped.Message, "ignored by -ignore") {
				t.Errorf("%s: the ignored finding's testcase does not say so: %+v", c.name, tc)
			}
		}

		props := doc.properties()
		for name, want := range map[string]string{
			"exit_code":       strconv.Itoa(r.ExitCode),
			"interrupted":     strconv.FormatBool(c.interrupted),
			"target":          r.Target,
			"scanner.version": "9.9.9",
			"options.fail_on": r.Options.FailOn,
			"options.expect":  "auth",
			"options.seed":    "1763372800123456789",
			"started":         "2026-11-17T10:00:00Z",
		} {
			if props[name] != want {
				t.Errorf("%s: property %s = %q, want %q", c.name, name, props[name], want)
			}
		}
		if doc.Time != "1.234" {
			t.Errorf("%s: time %q, want 1.234", c.name, doc.Time)
		}
	}
}

// hostileText carries what a target can put in a header or an error: a
// terminal escape, a NUL, CDATA and markup terminators, a bidi control,
// invalid UTF-8 and a character XML cannot carry.
const hostileText = "\x1b[2J\x00]]></testcase></testsuite><![CDATA[&amp;\u202e\xff\uffff"

// hostileReport quotes hostileText in every field the target can reach, and
// in a finding of every status.
func hostileReport() *Report {
	findings := []Finding{
		finding("DISC-001", SevInfo, StatusPass, hostileText),
		finding("HDR-003", SevHigh, StatusFail, "detail "+hostileText).
			withEvidence("evidence %s", hostileText).withRemediation("fix " + hostileText),
		finding("HDR-009", SevLow, StatusWarn, hostileText).withEvidence("Server: %s", hostileText),
		finding("AUTH-005", SevInfo, StatusError, hostileText),
		finding("TLS-001", SevInfo, StatusSkip, hostileText),
		finding("PROM-001", SevMedium, StatusFail, hostileText),
	}
	markIgnored(findings, map[string]bool{"prom-001": true})
	r := newReport("http://t/"+hostileText, findings)
	r.TargetInfo.Version = hostileText
	r.Options = ReportOptions{Only: []string{hostileText}}
	return r
}

func TestJUnitSurvivesHostileText(t *testing.T) {
	r := hostileReport()
	var junit bytes.Buffer
	if err := r.writeJUnit(&junit, SevHigh); err != nil {
		t.Fatal(err)
	}
	doc := parseJUnit(t, junit.String())
	checkJUnitCounts(t, doc)
	escaped := `\x1b[2J\x00]]></testcase></testsuite><![CDATA[&amp;\u202e\xff`
	tc := doc.cases()["HDR-003 Content-Security-Policy hardened"]
	if tc.Failure == nil || !strings.Contains(tc.Failure.Message, escaped) || !strings.Contains(tc.Failure.Text, "evidence "+escaped) ||
		!strings.Contains(tc.Failure.Text, "fix "+escaped) || !strings.Contains(tc.SystemOut, escaped) {
		t.Errorf("the escaped text did not survive the round trip: %+v", tc)
	}
	// The properties come by way of the JSON report, which has already
	// replaced the invalid byte.
	escapedValid := strings.TrimSuffix(escaped, `\xff`) + "\ufffd"
	if props := doc.properties(); !strings.Contains(props["target_info.version"], escapedValid) ||
		!strings.Contains(props["target"], escapedValid) || !strings.Contains(props["options.only"], escapedValid) {
		t.Errorf("properties lost the escaped text: %v", props)
	}
}
