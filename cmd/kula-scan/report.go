package main

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Report bundles all findings from a scan plus rendering and exit-code logic.
// It carries everything an output format needs: the scan's metadata, the
// findings, their tally and the exit status.
type Report struct {
	Scanner    ScannerInfo   `json:"scanner"`
	Target     string        `json:"target"`
	Started    time.Time     `json:"started"` // UTC, whole seconds: RFC 3339 in JSON
	DurationMS int64         `json:"duration_ms"`
	Options    ReportOptions `json:"options"`
	TargetInfo TargetInfo    `json:"target_info"`
	Findings   []Finding     `json:"findings"`
	Summary    Summary       `json:"summary"`
	// Interrupted is set when a signal cut the run short; NotRun counts the
	// checks that did not run (or were cut short, their findings dropped).
	Interrupted bool `json:"interrupted"`
	NotRun      int  `json:"not_run,omitempty"`
	ExitCode    int  `json:"exit_code"`
}

// ScannerInfo names the program that wrote the report.
type ScannerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ReportOptions records the options that shaped the run. The lists hold the
// canonical spelling of what was given (categories lower case, finding IDs
// as the catalog writes them) and are empty, not absent, when unset.
type ReportOptions struct {
	Aggressive bool     `json:"aggressive"`
	Fuzz       bool     `json:"fuzz"`
	Seed       int64    `json:"seed,omitempty"` // the fuzz seed, when fuzzing
	Only       []string `json:"only"`
	Skip       []string `json:"skip"`
	Ignore     []string `json:"ignore"`
	Expect     []string `json:"expect"`
	FailOn     string   `json:"fail_on"`
	Connect    string   `json:"connect,omitempty"` // -connect: where connections went instead of the URL's address
	Unix       string   `json:"unix,omitempty"`    // -unix: the socket connections went to
	Headers    []string `json:"headers,omitempty"` // the -H header names; never their values
}

// TargetInfo is what discovery learnt about the target, and behind auth what
// the run's session read of /api/config: Ollama and Version stay unset when
// auth hid it and the run opened no session. AuthEnabled is nil, null in
// JSON, when the auth posture is unknown: Kula serves no UI or API at the
// address (web.ui: false), or /api/auth/status could not be read; AUTH-000
// says which.
type TargetInfo struct {
	HTTPS       bool   `json:"https"`
	AuthEnabled *bool  `json:"auth_enabled"`
	Ollama      bool   `json:"ollama"`
	Version     string `json:"version,omitempty"` // when /api/config discloses it
}

// Summary holds per-status counts for the run. An ignored finding counts
// under Ignored only, not under its status.
type Summary struct {
	Pass    int `json:"pass"`
	Fail    int `json:"fail"`
	Warn    int `json:"warn"`
	Skip    int `json:"skip"`
	Error   int `json:"error"`
	Ignored int `json:"ignored"`
}

// newReport tallies findings into a Report.
func newReport(target string, findings []Finding) *Report {
	r := &Report{Target: target, Findings: findings}
	for _, f := range findings {
		if f.Ignored {
			r.Summary.Ignored++
			continue
		}
		switch f.Status {
		case StatusPass:
			r.Summary.Pass++
		case StatusFail:
			r.Summary.Fail++
		case StatusWarn:
			r.Summary.Warn++
		case StatusSkip:
			r.Summary.Skip++
		case StatusError:
			r.Summary.Error++
		}
	}
	return r
}

// ANSI colors; disabled when noColor is set.
const (
	cReset  = "\033[0m"
	cRed    = "\033[31m"
	cGreen  = "\033[32m"
	cYellow = "\033[33m"
	cBlue   = "\033[34m"
	cGray   = "\033[90m"
	cBold   = "\033[1m"
)

func statusColor(st Status) string {
	switch st {
	case StatusPass:
		return cGreen
	case StatusFail:
		return cRed
	case StatusWarn:
		return cYellow
	case StatusSkip:
		return cGray
	default:
		return cBlue
	}
}

// writeText renders the report as a grouped, optionally colored, human report.
func (r *Report) writeText(w io.Writer, noColor bool) {
	color := func(c, s string) string {
		if noColor {
			return s
		}
		return c + s + cReset
	}

	// Group findings by category, preserving first-seen order.
	var order []string
	groups := map[string][]Finding{}
	for _, f := range r.Findings {
		if _, ok := groups[f.Category]; !ok {
			order = append(order, f.Category)
		}
		groups[f.Category] = append(groups[f.Category], f)
	}

	name := "kula-scan"
	if r.Scanner.Version != "" {
		name += " " + r.Scanner.Version
	}
	_, _ = fmt.Fprintf(w, "\n%s %s\n", color(cBold, name+" — target:"), printable(r.Target))

	// The ID column is as wide as the longest ID, ten at least, so the
	// titles line up.
	idWidth := 10
	for _, f := range r.Findings {
		idWidth = max(idWidth, utf8.RuneCountInString(printable(f.ID)))
	}

	// Any field may quote the target (a header value, a certificate name, an
	// error message), so each is written through printable.
	for _, cat := range order {
		_, _ = fmt.Fprintf(w, "\n%s\n", color(cBold, strings.ToUpper(printable(cat))))
		for _, f := range groups[cat] {
			tag := fmt.Sprintf("%-5s", f.Status.String())
			line := fmt.Sprintf("  %s %-*s %s", color(statusColor(f.Status), tag), idWidth, printable(f.ID), printable(f.Title))
			// Severity tag only matters for things that went wrong.
			if f.Status == StatusFail || f.Status == StatusWarn {
				line += " " + color(severityColor(f.Severity), "["+f.Severity.String()+"]")
			}
			if f.Expectation != "" && f.Status == StatusFail {
				line += " " + color(cRed, "(expected: "+printable(f.Expectation)+")")
			}
			if f.Ignored {
				line += " " + color(cGray, "(ignored)")
			}
			_, _ = fmt.Fprintln(w, line)
			if f.Detail != "" {
				_, _ = fmt.Fprintf(w, "        %s\n", color(cGray, printable(f.Detail)))
			}
			if f.Evidence != "" {
				_, _ = fmt.Fprintf(w, "        %s %s\n", color(cGray, "evidence:"), printable(f.Evidence))
			}
			if f.Remediation != "" && (f.Status == StatusFail || f.Status == StatusWarn) {
				_, _ = fmt.Fprintf(w, "        %s %s\n", color(cBlue, "fix:"), printable(f.Remediation))
			}
		}
	}

	s := r.Summary
	ignored := ""
	if s.Ignored > 0 {
		ignored = "  " + color(cGray, fmt.Sprintf("%d ignored", s.Ignored))
	}
	_, _ = fmt.Fprintf(w, "\n%s  %s  %s  %s  %s%s\n",
		color(cBold, "Summary:"),
		color(cGreen, fmt.Sprintf("%d pass", s.Pass)),
		color(cRed, fmt.Sprintf("%d fail", s.Fail)),
		color(cYellow, fmt.Sprintf("%d warn", s.Warn)),
		color(cGray, fmt.Sprintf("%d skip / %d error", s.Skip, s.Error)),
		ignored,
	)
	if r.Interrupted {
		_, _ = fmt.Fprintf(w, "%s %s\n", color(cYellow+cBold, "Interrupted:"), notRunText(r.NotRun))
	}
}

// notRunText says how many checks an interrupted run did not run.
func notRunText(n int) string {
	if n == 1 {
		return "1 check did not run."
	}
	return fmt.Sprintf("%d checks did not run.", n)
}

func severityColor(sev Severity) string {
	switch sev {
	case SevCritical, SevHigh:
		return cRed
	case SevMedium:
		return cYellow
	default:
		return cGray
	}
}

// writeJSON renders the report as indented JSON.
func (r *Report) writeJSON(w io.Writer) error {
	return writeTerminalSafeJSON(w, r)
}

// writeTerminalSafeJSON writes v as indented JSON that is safe to print to a
// terminal. The JSON and SARIF reports both go through it.
func writeTerminalSafeJSON(w io.Writer, v any) error {
	var encoded strings.Builder
	enc := json.NewEncoder(&encoded)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return err
	}
	// The encoder escapes C0 controls but writes DEL, C1 and bidi controls
	// raw. Escape those too, so the JSON is as safe to print to a terminal as
	// the text report. Outside strings JSON is ASCII, so only string contents
	// change, and they decode to the same text.
	var out strings.Builder
	for _, r := range encoded.String() {
		if r >= 0x7f && terminalControl(r) {
			fmt.Fprintf(&out, `\u%04x`, r)
		} else {
			out.WriteRune(r)
		}
	}
	_, err := io.WriteString(w, out.String())
	return err
}

// worstFailure returns the highest severity among FAIL findings that are not
// ignored, or -1 when there are none.
func (r *Report) worstFailure() Severity {
	worst := Severity(-1)
	for _, f := range r.Findings {
		if f.Status == StatusFail && !f.Ignored && f.Severity > worst {
			worst = f.Severity
		}
	}
	return worst
}

// Exit statuses.
const (
	exitClean        = 0
	exitFailing      = 1   // a FAIL at or above -fail-on, or a declared expectation not met
	exitUsage        = 2   // a usage error, or a target that is unreachable or not Kula
	exitInconclusive = 3   // no gating FAIL, but a probe could not complete
	exitInterrupted  = 130 // a signal cut the run short (128 + SIGINT)
)

// exitCode returns the exit status the report calls for. Ignored findings
// count for nothing; a failure wins over an error, and an interrupt over
// everything, since the report is partial.
func (r *Report) exitCode(failOn Severity) int {
	switch {
	case r.Interrupted:
		return exitInterrupted
	case r.stoppedAtDiscovery():
		return exitUsage
	case r.worstFailure() >= failOn, len(r.unmetExpectations()) > 0:
		return exitFailing
	case len(r.erroredIDs()) > 0:
		return exitInconclusive
	default:
		return exitClean
	}
}

// stoppedAtDiscovery reports whether discovery found the target unreachable
// or not Kula, which ends the scan: DISC-001 is then an ERROR.
func (r *Report) stoppedAtDiscovery() bool {
	for _, f := range r.Findings {
		if f.ID == "DISC-001" && f.Status == StatusError {
			return true
		}
	}
	return false
}

// unmetExpectations lists, as "key (ID, ID)", each -expect key that a FAIL
// finding not ignored contradicts. These fail the run whatever -fail-on is.
func (r *Report) unmetExpectations() []string {
	byKey := map[string][]string{}
	var keys []string
	for _, f := range r.Findings {
		if !contradictsExpectation(f) {
			continue
		}
		if _, seen := byKey[f.Expectation]; !seen {
			keys = append(keys, f.Expectation)
		}
		byKey[f.Expectation] = append(byKey[f.Expectation], f.ID)
	}
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+" ("+strings.Join(byKey[key], ", ")+")")
	}
	return out
}

// erroredIDs lists the IDs of ERROR findings that are not ignored.
func (r *Report) erroredIDs() []string {
	var ids []string
	for _, f := range r.Findings {
		if f.Status == StatusError && !f.Ignored {
			ids = append(ids, f.ID)
		}
	}
	sort.Strings(ids)
	return slices.Compact(ids)
}

// exitReason explains a non-zero exit status in one line, or returns "" for
// a clean exit.
func (r *Report) exitReason(failOn Severity) string {
	code := r.exitCode(failOn)
	var why string
	switch code {
	case exitInterrupted:
		why = "interrupted; " + notRunText(r.NotRun)
	case exitUsage:
		for _, f := range r.Findings {
			if f.ID == "DISC-001" {
				why = "the scan stopped at discovery (DISC-001): " + f.Detail
			}
		}
	case exitFailing:
		var parts []string
		if ids := r.failingIDs(failOn); len(ids) > 0 {
			parts = append(parts, fmt.Sprintf("safeguards FAIL at/above %s: %s", failOn, strings.Join(ids, ", ")))
		}
		if unmet := r.unmetExpectations(); len(unmet) > 0 {
			parts = append(parts, "expected posture not met: "+strings.Join(unmet, ", "))
		}
		why = strings.Join(parts, "; ")
	case exitInconclusive:
		why = "inconclusive: no failing safeguard, but these probes could not complete: " + strings.Join(r.erroredIDs(), ", ")
	default:
		return ""
	}
	return fmt.Sprintf("kula-scan: exit %d: %s", code, printable(why))
}

// failingIDs lists the IDs of FAIL findings at or above failOn that are not
// ignored, for the closing summary line.
func (r *Report) failingIDs(failOn Severity) []string {
	var ids []string
	for _, f := range r.Findings {
		if failsAt(f, failOn) {
			ids = append(ids, f.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

// failsAt reports whether f is a FAIL at or above failOn that is not
// ignored: one of failingIDs.
func failsAt(f Finding, failOn Severity) bool {
	return f.Status == StatusFail && !f.Ignored && f.Severity >= failOn
}

// contradictsExpectation reports whether f is a FAIL not ignored against a
// declared -expect key: one of unmetExpectations.
func contradictsExpectation(f Finding) bool {
	return f.Status == StatusFail && !f.Ignored && f.Expectation != ""
}

// gates reports whether f calls for exit status 1, as exitCode counts it: a
// failing safeguard or an unmet expectation.
func gates(f Finding, failOn Severity) bool {
	return failsAt(f, failOn) || contradictsExpectation(f)
}
