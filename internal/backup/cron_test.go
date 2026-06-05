package backup

import (
	"testing"
	"time"
)

func TestParseScheduleErrors(t *testing.T) {
	cases := []string{
		"",
		"* * * *",      // too few fields
		"* * * * * *",  // too many fields
		"60 * * * *",   // minute out of range
		"* 24 * * *",   // hour out of range
		"* * 0 * *",    // dom out of range
		"* * * 13 *",   // month out of range
		"* * * * 8",    // dow out of range
		"*/0 * * * *",  // zero step
		"5-1 * * * *",  // inverted range
		"a * * * *",    // non-numeric
		"1,,2 * * * *", // empty list term
		"1/x * * * *",  // bad step
	}
	for _, expr := range cases {
		if _, err := ParseSchedule(expr); err == nil {
			t.Errorf("ParseSchedule(%q) expected error, got nil", expr)
		}
	}
}

func TestScheduleMatches(t *testing.T) {
	mustParse := func(expr string) *Schedule {
		s, err := ParseSchedule(expr)
		if err != nil {
			t.Fatalf("ParseSchedule(%q): %v", expr, err)
		}
		return s
	}

	// Reference instants. Note 2026-06-04 is a Thursday (weekday 4).
	midnight := time.Date(2026, 6, 4, 0, 0, 0, 0, time.Local)
	noon := time.Date(2026, 6, 4, 12, 0, 0, 0, time.Local)
	min15 := time.Date(2026, 6, 4, 9, 15, 0, 0, time.Local)
	min16 := time.Date(2026, 6, 4, 9, 16, 0, 0, time.Local)
	sunday := time.Date(2026, 6, 7, 3, 0, 0, 0, time.Local) // Sunday

	cases := []struct {
		expr string
		t    time.Time
		want bool
	}{
		{"0 0 * * *", midnight, true},
		{"0 0 * * *", noon, false},
		{"* * * * *", noon, true},
		{"*/15 * * * *", min15, true},
		{"*/15 * * * *", min16, false},
		{"0 12 * * *", noon, true},
		{"0 0 4 * *", midnight, true},  // dom = 4 matches the 4th
		{"0 0 5 * *", midnight, false}, // dom = 5 does not
		{"0 3 * * 0", sunday, true},    // dow 0 = Sunday
		{"0 3 * * 7", sunday, true},    // 7 also Sunday
		{"0 0 * * 4", midnight, true},  // Thursday
		{"0 0 * * 1", midnight, false}, // Monday
		// dom and dow both restricted -> OR semantics: the 1st OR Thursday.
		{"0 0 1 * 4", midnight, true},
	}
	for _, c := range cases {
		got := mustParse(c.expr).Matches(c.t)
		if got != c.want {
			t.Errorf("%q.Matches(%s) = %v, want %v", c.expr, c.t.Format(time.RFC3339), got, c.want)
		}
	}
}
