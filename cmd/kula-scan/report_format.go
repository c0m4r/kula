package main

import (
	"fmt"
	"io"
	"slices"
	"strings"
)

// Report formats -format accepts.
const (
	formatText  = "text"
	formatJSON  = "json"
	formatJUnit = "junit"
	formatSARIF = "sarif"
)

var reportFormats = []string{formatText, formatJSON, formatJUnit, formatSARIF}

// resolveFormat returns the report format -format and -json ask for. An
// empty -format means text; -json means -format json and conflicts with any
// other -format.
func resolveFormat(format string, asJSON bool) (string, error) {
	name := strings.ToLower(strings.TrimSpace(format))
	if name != "" && !slices.Contains(reportFormats, name) {
		return "", fmt.Errorf("invalid -format %q (want %s)", format, strings.Join(reportFormats, "|"))
	}
	switch {
	case asJSON && name != "" && name != formatJSON:
		return "", fmt.Errorf("-json means -format json; it conflicts with -format %q", format)
	case asJSON:
		return formatJSON, nil
	case name == "":
		return formatText, nil
	default:
		return name, nil
	}
}

// writeFormat writes the report to w in format. The text report closes with
// the failing safeguards, and is colored unless noColor is set.
func (r *Report) writeFormat(w io.Writer, format string, failOn Severity, noColor bool) error {
	switch format {
	case formatJSON:
		return r.writeJSON(w)
	case formatJUnit:
		return r.writeJUnit(w, failOn)
	case formatSARIF:
		return r.writeSARIF(w, failOn)
	default:
		r.writeText(w, noColor)
		if ids := r.failingIDs(failOn); len(ids) > 0 {
			_, _ = fmt.Fprintf(w, "\nFAILING safeguards at/above %s: %s\n", failOn, strings.Join(ids, ", "))
		}
		return nil
	}
}
