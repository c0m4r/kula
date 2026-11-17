package main

import (
	"fmt"
	"io"
	"os"
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
		ew := &errWriter{w: w}
		r.writeText(ew, noColor)
		if ids := r.failingIDs(failOn); len(ids) > 0 {
			_, _ = fmt.Fprintf(ew, "\nFAILING safeguards at/above %s: %s\n", failOn, strings.Join(ids, ", "))
		}
		return ew.err
	}
}

// errWriter keeps the first error a write returns, for code that does not
// check each write.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	n, err := e.w.Write(p)
	e.err = err
	return n, err
}

// createReportFile creates or truncates the -o file. The scan opens it before
// it starts, so a path that cannot be written fails at once instead of after
// the scan. A new file is readable by its owner alone, as the report maps the
// target's weak spots; an existing one keeps its mode.
func createReportFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
}

// writeReportFile writes the report to the -o file in format, never colored,
// and closes it.
func writeReportFile(file *os.File, r *Report, format string, failOn Severity) error {
	err := r.writeFormat(file, format, failOn, true)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}
