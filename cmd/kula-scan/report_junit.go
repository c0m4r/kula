package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The JUnit XML report has one testsuite per category and one testcase per
// finding, and a JUnit consumer fails it exactly when the exit status is not
// zero: a FAIL that gates the exit status is a <failure>, an ERROR not
// ignored an <error>, and an exit status no finding explains (an interrupt,
// or discovery stopping at a DISC-001 that -ignore names) an <error> in a
// run-level testcase. Every testcase's <system-out> keeps the whole finding.
//
// Everything quoted from the target goes through printable, like the text
// report, and encoding/xml escapes the rest, so the file is well-formed and
// safe to print.

type junitTestsuites struct {
	XMLName   xml.Name     `xml:"testsuites"`
	Name      string       `xml:"name,attr"`
	Tests     int          `xml:"tests,attr"`
	Failures  int          `xml:"failures,attr"`
	Errors    int          `xml:"errors,attr"`
	Skipped   int          `xml:"skipped,attr"`
	Time      string       `xml:"time,attr"`
	Timestamp string       `xml:"timestamp,attr,omitempty"`
	Suites    []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name       string           `xml:"name,attr"`
	Tests      int              `xml:"tests,attr"`
	Failures   int              `xml:"failures,attr"`
	Errors     int              `xml:"errors,attr"`
	Skipped    int              `xml:"skipped,attr"`
	Timestamp  string           `xml:"timestamp,attr,omitempty"`
	Properties *junitProperties `xml:"properties"` // the first suite's only
	Cases      []junitCase      `xml:"testcase"`
}

type junitProperties struct {
	Property []junitProperty `xml:"property"`
}

type junitProperty struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	Classname string        `xml:"classname,attr"`
	Failure   *junitOutcome `xml:"failure"`
	Error     *junitOutcome `xml:"error"`
	Skipped   *junitSkipped `xml:"skipped"`
	SystemOut *junitOutcome `xml:"system-out"`
}

type junitSkipped struct {
	Message string `xml:"message,attr,omitempty"`
}

// junitOutcome is a <failure>, <error> or <system-out>: optional message and
// type attributes around multi-line text.
type junitOutcome struct {
	Message string
	Type    string
	Text    string
}

// MarshalXML writes the text through the token encoder, which escapes markup
// and replaces what XML cannot carry as field marshalling does, but keeps
// newlines readable instead of writing them as &#xA;.
func (o junitOutcome) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	if o.Message != "" {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "message"}, Value: o.Message})
	}
	if o.Type != "" {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "type"}, Value: o.Type})
	}
	if err := e.EncodeToken(start); err != nil {
		return err
	}
	if o.Text != "" {
		if err := e.EncodeToken(xml.CharData(o.Text)); err != nil {
			return err
		}
	}
	return e.EncodeToken(start.End())
}

// add appends a testcase and counts its outcome.
func (s *junitSuite) add(c junitCase) {
	s.Cases = append(s.Cases, c)
	s.Tests++
	switch {
	case c.Failure != nil:
		s.Failures++
	case c.Error != nil:
		s.Errors++
	case c.Skipped != nil:
		s.Skipped++
	}
}

// writeJUnit renders the report as JUnit XML.
func (r *Report) writeJUnit(w io.Writer, failOn Severity) error {
	code := r.exitCode(failOn)

	var suites []*junitSuite
	byCategory := map[string]*junitSuite{}
	failing := 0
	for _, f := range r.Findings {
		suite, ok := byCategory[f.Category]
		if !ok {
			suite = &junitSuite{Name: printable(f.Category)}
			byCategory[f.Category] = suite
			suites = append(suites, suite)
		}
		c := junitCaseFor(f, failOn)
		if c.Failure != nil || c.Error != nil {
			failing++
		}
		suite.add(c)
	}

	// A run-level suite comes first when the exit status needs a testcase of
	// its own, or when there is no other suite to carry the properties.
	runCase := r.Interrupted || (code != exitClean && failing == 0)
	if runCase || len(suites) == 0 {
		run := &junitSuite{Name: "kula-scan"}
		if runCase {
			run.add(junitCase{
				Name:      "Scan completed",
				Classname: "kula-scan.run",
				Error:     &junitOutcome{Message: r.exitReason(failOn), Type: "exit " + strconv.Itoa(code)},
			})
		}
		suites = append([]*junitSuite{run}, suites...)
	}

	props, err := r.junitProperties(code)
	if err != nil {
		return err
	}
	suites[0].Properties = &junitProperties{Property: props}

	root := junitTestsuites{
		Name: "kula-scan",
		Time: strconv.FormatFloat(float64(r.DurationMS)/1000, 'f', 3, 64),
	}
	if !r.Started.IsZero() {
		root.Timestamp = r.Started.UTC().Format(time.RFC3339)
	}
	for _, s := range suites {
		s.Timestamp = root.Timestamp
		root.Tests += s.Tests
		root.Failures += s.Failures
		root.Errors += s.Errors
		root.Skipped += s.Skipped
		root.Suites = append(root.Suites, *s)
	}

	var out bytes.Buffer
	out.WriteString(xml.Header)
	enc := xml.NewEncoder(&out)
	enc.Indent("", "  ")
	if err := enc.Encode(root); err != nil {
		return err
	}
	out.WriteByte('\n')
	_, err = w.Write(out.Bytes())
	return err
}

// junitCaseFor maps a finding to a testcase. A gating FAIL fails it, an ERROR
// not ignored errors it, and SKIP or -ignore skips it; a PASS, a WARN and a
// FAIL below -fail-on pass.
func junitCaseFor(f Finding, failOn Severity) junitCase {
	c := junitCase{
		Name:      printable(f.ID + " " + f.Title),
		Classname: "kula-scan." + printable(f.Category),
		SystemOut: &junitOutcome{Text: junitRecord(f)},
	}
	message := printable(f.Detail)
	switch {
	case gates(f, failOn):
		if message == "" {
			message = printable(f.Title)
		}
		var body []string
		if f.Expectation != "" {
			body = append(body, "expected posture: "+printable(f.Expectation))
		}
		body = append(body, junitEvidence(f)...)
		c.Failure = &junitOutcome{Message: message, Type: f.Severity.String(), Text: strings.Join(body, "\n")}
	case f.Ignored:
		label := f.Status.String()
		if f.Status == StatusFail || f.Status == StatusWarn {
			label += " " + f.Severity.String()
		}
		skipped := "ignored by -ignore: " + label
		if message != "" {
			skipped += ": " + message
		}
		c.Skipped = &junitSkipped{Message: skipped}
	case f.Status == StatusError:
		if message == "" {
			message = "the probe could not complete"
		}
		c.Error = &junitOutcome{Message: message, Type: "ERROR", Text: strings.Join(junitEvidence(f), "\n")}
	case f.Status == StatusSkip:
		if message == "" {
			message = "skipped"
		}
		c.Skipped = &junitSkipped{Message: message}
	}
	return c
}

// junitEvidence returns a finding's evidence and remediation lines.
func junitEvidence(f Finding) []string {
	var lines []string
	if f.Evidence != "" {
		lines = append(lines, "evidence: "+printable(f.Evidence))
	}
	if f.Remediation != "" {
		lines = append(lines, "fix: "+printable(f.Remediation))
	}
	return lines
}

// junitRecord renders a whole finding for its testcase's <system-out>.
func junitRecord(f Finding) string {
	lines := []string{"status: " + f.Status.String(), "severity: " + f.Severity.String()}
	if f.Expectation != "" {
		lines = append(lines, "expected posture: "+printable(f.Expectation))
	}
	if f.Ignored {
		lines = append(lines, "ignored: by -ignore, never affects the exit status")
	}
	if f.Detail != "" {
		lines = append(lines, "detail: "+printable(f.Detail))
	}
	return strings.Join(append(lines, junitEvidence(f)...), "\n")
}

// junitProperties lists the run's metadata: the JSON report's fields other
// than the findings and their tally, flattened to dotted names such as
// options.fail_on, so a field the report gains shows here too. exit_code is
// the status the findings call for.
func (r *Report) junitProperties(code int) ([]junitProperty, error) {
	encoded, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(encoded))
	dec.UseNumber() // a fuzz seed does not survive a float64
	var fields map[string]any
	if err := dec.Decode(&fields); err != nil {
		return nil, err
	}
	delete(fields, "findings")
	delete(fields, "summary")
	fields["exit_code"] = json.Number(strconv.Itoa(code))

	var props []junitProperty
	var flatten func(name string, v any)
	flatten = func(name string, v any) {
		if object, ok := v.(map[string]any); ok {
			for key, sub := range object {
				if name != "" {
					key = name + "." + key
				}
				flatten(key, sub)
			}
			return
		}
		props = append(props, junitProperty{Name: printable(name), Value: printable(propertyValue(v))})
	}
	flatten("", fields)
	sort.Slice(props, func(i, j int) bool { return props[i].Name < props[j].Name })
	return props, nil
}

// propertyValue renders a decoded JSON value as a property value; a list is
// comma-separated, and null, an unknown such as target_info.auth_enabled,
// stays null.
func propertyValue(v any) string {
	switch v := v.(type) {
	case nil:
		return "null"
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			parts = append(parts, propertyValue(item))
		}
		return strings.Join(parts, ",")
	default:
		return fmt.Sprint(v)
	}
}
