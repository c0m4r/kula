package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestTextReportAlignsTitles: the ID column is as wide as the longest ID,
// so every title, in every category, starts in the same column; an ID or a
// title the target wrote is still escaped.
func TestTextReportAlignsTitles(t *testing.T) {
	findings := []Finding{
		finding("HDR-001", SevMedium, StatusPass, ""),
		finding("DOS-SLOWLORIS", SevMedium, StatusWarn, "detail"),
		finding("FUZZ-LIVENESS", SevHigh, StatusPass, ""),
		finding("WS-000", SevInfo, StatusSkip, ""),
		finding("X\x1b[2J", SevHigh, StatusFail, ""), // uncatalogued: titled with the ID itself
	}
	findings[0].Title = "title \u009b2J"

	for _, noColor := range []bool{true, false} {
		var out bytes.Buffer
		newReport("http://t", findings).writeText(&out, noColor)
		text := out.String()
		if bad, found := firstTerminalControl(text); found {
			t.Fatalf("noColor=%v: the report writes %q to the terminal:\n%s", noColor, bad, text)
		}
		if !noColor {
			continue
		}
		// "  STATUS ID… TITLE": the IDs start in column 8, the titles in
		// column 8 + the widest ID + 1.
		const idColumn = len("  PASS  ")
		width := len("DOS-SLOWLORIS")
		for _, f := range findings {
			id, title := printable(f.ID), printable(f.Title)
			line := ""
			for l := range strings.SplitSeq(text, "\n") {
				if strings.HasPrefix(l, "  "+f.Status.String()) && strings.HasPrefix(l[idColumn:], id+" ") {
					line = l
				}
			}
			if line == "" {
				t.Fatalf("no line for %s:\n%s", id, text)
			}
			if got := line[idColumn+width+1:]; !strings.HasPrefix(got, title) {
				t.Errorf("%s: the title does not start in column %d: %q\n%s", id, idColumn+width+1, line, text)
			}
		}
	}
}
