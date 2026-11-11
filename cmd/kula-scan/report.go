package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Report bundles all findings from a scan plus rendering and exit-code logic.
type Report struct {
	Target   string    `json:"target"`
	Findings []Finding `json:"findings"`
	Summary  Summary   `json:"summary"`
}

// Summary holds per-status counts for the run.
type Summary struct {
	Pass  int `json:"pass"`
	Fail  int `json:"fail"`
	Warn  int `json:"warn"`
	Skip  int `json:"skip"`
	Error int `json:"error"`
}

// newReport tallies findings into a Report.
func newReport(target string, findings []Finding) *Report {
	r := &Report{Target: target, Findings: findings}
	for _, f := range findings {
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

	_, _ = fmt.Fprintf(w, "\n%s %s\n", color(cBold, "kula-scan — target:"), printable(r.Target))

	// Any field may quote the target (a header value, a certificate name, an
	// error message), so each is written through printable.
	for _, cat := range order {
		_, _ = fmt.Fprintf(w, "\n%s\n", color(cBold, strings.ToUpper(printable(cat))))
		for _, f := range groups[cat] {
			tag := fmt.Sprintf("%-5s", f.Status.String())
			line := fmt.Sprintf("  %s %-10s %s", color(statusColor(f.Status), tag), printable(f.ID), printable(f.Title))
			// Severity tag only matters for things that went wrong.
			if f.Status == StatusFail || f.Status == StatusWarn {
				line += " " + color(severityColor(f.Severity), "["+f.Severity.String()+"]")
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
	_, _ = fmt.Fprintf(w, "\n%s  %s  %s  %s  %s\n",
		color(cBold, "Summary:"),
		color(cGreen, fmt.Sprintf("%d pass", s.Pass)),
		color(cRed, fmt.Sprintf("%d fail", s.Fail)),
		color(cYellow, fmt.Sprintf("%d warn", s.Warn)),
		color(cGray, fmt.Sprintf("%d skip / %d error", s.Skip, s.Error)),
	)
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
	var encoded strings.Builder
	enc := json.NewEncoder(&encoded)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
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

// worstFailure returns the highest severity among FAIL findings, or -1 when
// there are no failures.
func (r *Report) worstFailure() Severity {
	worst := Severity(-1)
	for _, f := range r.Findings {
		if f.Status == StatusFail && f.Severity > worst {
			worst = f.Severity
		}
	}
	return worst
}

// exitCode returns a non-zero code when any FAIL at or above failOn exists.
// 0 = clean, 1 = failing safeguard at/above threshold, 2 reserved for usage errors.
func (r *Report) exitCode(failOn Severity) int {
	w := r.worstFailure()
	if w >= 0 && w >= failOn {
		return 1
	}
	return 0
}

// failingIDs lists the IDs of FAIL findings at or above failOn, for the closing summary line.
func (r *Report) failingIDs(failOn Severity) []string {
	var ids []string
	for _, f := range r.Findings {
		if f.Status == StatusFail && f.Severity >= failOn {
			ids = append(ids, f.ID)
		}
	}
	sort.Strings(ids)
	return ids
}
