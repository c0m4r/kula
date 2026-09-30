package tui

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
)

func TestBrailleCanvasEncodesDots(t *testing.T) {
	canvas := newBrailleCanvas(2, 1)
	canvas.set(0, 0, 0)
	canvas.set(1, 3, 0)
	canvas.set(2, 1, 0)
	canvas.set(-1, 0, 0) // out of range: ignored
	canvas.set(4, 0, 0)
	canvas.set(0, 4, 0)

	got := []rune(stripped(canvas.render([]lipgloss.Style{sAccent})[0]))
	want := []rune{0x2800 + 0x01 + 0x80, 0x2800 + 0x02}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("braille cells = %q, want %q", string(got), string(want))
	}
}

func TestBrailleCanvasLineIsContinuous(t *testing.T) {
	canvas := newBrailleCanvas(10, 2)
	canvas.line(0, 7, 19, 0, 0)
	for col := 0; col < 10; col++ {
		if canvas.bits[col] == 0 && canvas.bits[10+col] == 0 {
			t.Fatalf("diagonal line skipped column %d", col)
		}
	}
}

// plotCells returns the cells right of a chart row's y-axis rule.
func plotCells(line string) []rune {
	cells := []rune(stripped(line))
	for index, cell := range cells {
		if cell == '┤' || cell == '│' {
			return cells[index+1:]
		}
	}
	return nil
}

func testChart() lineChart {
	return lineChart{
		xs:     []float64{0, 0.5, 1},
		series: []chartSeries{{style: sAccent, values: []float64{0, 50, 100}}},
		min:    0,
		max:    100,
		label:  percentLabel,
		ticks:  []chartTick{{pos: 0, label: "12:00"}, {pos: 0.5, label: "12:30"}, {pos: 1, label: "13:00"}},
	}
}

func TestLineChartGeometryAndLabels(t *testing.T) {
	lines := testChart().render(40, 5)
	if len(lines) != 7 {
		t.Fatalf("chart has %d lines, want 5 plot rows + axis + labels", len(lines))
	}
	for index, line := range lines {
		if width := lipgloss.Width(line); width > 40 {
			t.Errorf("line %d is %d cells wide, limit 40: %q", index, width, stripped(line))
		}
	}
	plain := make([]string, len(lines))
	for index, line := range lines {
		plain[index] = stripped(line)
	}
	if !strings.HasPrefix(plain[0], "100% ┤") || !strings.HasPrefix(plain[4], "  0% ┤") ||
		!strings.HasPrefix(plain[2], " 50% ┤") {
		t.Fatalf("y labels misplaced:\n%s", strings.Join(plain, "\n"))
	}
	// The line rises from bottom-left to top-right.
	top, bottom := []rune(plain[0]), []rune(plain[4])
	if top[len(top)-1] == ' ' || bottom[len("  0% ┤")] == ' ' {
		t.Fatalf("line endpoints missing:\n%s", strings.Join(plain, "\n"))
	}
	if !strings.Contains(plain[5], "└") || strings.Count(plain[5], "┬") != 3 {
		t.Fatalf("axis should carry three ticks: %q", plain[5])
	}
	for _, label := range []string{"12:00", "12:30", "13:00"} {
		if !strings.Contains(plain[6], label) {
			t.Errorf("tick label %q missing from %q", label, plain[6])
		}
	}
}

func TestLineChartSharedLabelWidthAlignsPlots(t *testing.T) {
	narrow := testChart()
	narrow.labelWidth = 5
	wide := testChart()
	wide.label = func(value float64) string { return formatAxisNumber(value * 1e3) }
	wide.labelWidth = 5
	a, b := stripped(narrow.render(40, 3)[0]), stripped(wide.render(40, 3)[0])
	if strings.Index(a, "┤") != strings.Index(b, "┤") {
		t.Fatalf("plots misaligned:\n%q\n%q", a, b)
	}
}

func TestLineChartBreaksLeaveGaps(t *testing.T) {
	chart := lineChart{
		xs:     []float64{0, 0.1, 0.9, 1},
		breaks: []bool{false, false, true, false},
		series: []chartSeries{{style: sAccent, values: []float64{50, 50, 50, 50}}},
		min:    0, max: 100,
		label: percentLabel,
	}
	lines := chart.render(50, 3)
	plot := plotCells(lines[1])
	centre := string(plot[len(plot)/3 : 2*len(plot)/3])
	if strings.TrimSpace(centre) != "" {
		t.Fatalf("break was bridged: %q", string(plot))
	}
	if strings.TrimSpace(string(plot[:3])) == "" || strings.TrimSpace(string(plot[len(plot)-3:])) == "" {
		t.Fatalf("segments beside the break are missing: %q", string(plot))
	}
}

func TestLineChartEnvelopeSpansMinToMax(t *testing.T) {
	chart := lineChart{
		xs: []float64{0, 1},
		series: []chartSeries{{
			style:  sAccent,
			values: []float64{50, 50},
			lower:  []float64{0, 0},
			upper:  []float64{100, 100},
		}},
		min: 0, max: 100,
		label: percentLabel,
	}
	lines := chart.render(30, 4)
	for row := 0; row < 4; row++ {
		plot := string(plotCells(lines[row]))
		if strings.TrimSpace(plot) == "" || strings.Contains(strings.TrimRight(plot, " "), " ") {
			t.Fatalf("row %d not filled by the envelope: %q", row, plot)
		}
	}
}

func TestLineChartRejectsDegenerateInput(t *testing.T) {
	flat := testChart()
	flat.max = flat.min
	if lines := flat.render(40, 4); lines != nil {
		t.Fatal("zero-height scale rendered a chart")
	}
	if lines := testChart().render(10, 4); lines != nil {
		t.Fatal("chart narrower than its labels rendered")
	}
	nan := testChart()
	nan.series[0].values = []float64{math.NaN(), math.Inf(1), 50}
	if lines := nan.render(40, 4); len(lines) != 6 {
		t.Fatal("non-finite values broke rendering")
	}
}

func TestTimeTicksAlignToRoundTimes(t *testing.T) {
	from := time.Date(2026, 9, 30, 12, 7, 13, 0, time.UTC)
	to := from.Add(time.Hour)
	ticks := timeTicks(from, to, 100)
	if len(ticks) < 4 || len(ticks) > 100/9 {
		t.Fatalf("got %d ticks for 100 columns", len(ticks))
	}
	previous := -1.0
	for _, tick := range ticks {
		at, err := time.Parse("15:04", tick.label)
		if err != nil {
			t.Fatalf("tick label %q: %v", tick.label, err)
		}
		if at.Minute()%5 != 0 {
			t.Errorf("tick %q is not on a round time", tick.label)
		}
		if tick.pos < 0 || tick.pos > 1 || tick.pos <= previous {
			t.Errorf("tick %q position %v out of order", tick.label, tick.pos)
		}
		previous = tick.pos
	}
}

func TestTimeTicksShowDatesForLongRanges(t *testing.T) {
	to := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	for _, tick := range timeTicks(to.Add(-7*24*time.Hour), to, 80) {
		if strings.Contains(tick.label, ":") {
			t.Fatalf("7-day tick %q shows a clock time instead of a date", tick.label)
		}
	}
	for _, tick := range timeTicks(to.Add(-3*24*time.Hour), to, 120) {
		if len(strings.Fields(tick.label)) != 2 && !strings.Contains(tick.label, " ") {
			t.Fatalf("3-day tick %q lacks its day", tick.label)
		}
	}
	if ticks := timeTicks(to, to, 80); ticks != nil {
		t.Fatal("empty span produced ticks")
	}
}

func TestNiceScale(t *testing.T) {
	ceilings := map[float64]float64{0: 1, -5: 1, 0.7: 0.8, 1: 1, 73: 80, 101: 120, 2400: 2500}
	for input, want := range ceilings {
		if got := niceCeil(input); math.Abs(got-want) > 1e-9 {
			t.Errorf("niceCeil(%v) = %v, want %v", input, got, want)
		}
	}
	floors := map[float64]float64{0: 0, 38: 30, 5: 5, 0.37: 0.3, -3: -3}
	for input, want := range floors {
		if got := niceFloor(input); math.Abs(got-want) > 1e-9 {
			t.Errorf("niceFloor(%v) = %v, want %v", input, got, want)
		}
	}
}

func TestAreaChartFillsRowsAndWidth(t *testing.T) {
	lines := areaChart([]float64{0, 50, 100}, 10, 3, 0, 100)
	if len(lines) != 3 {
		t.Fatalf("area chart has %d rows, want 3", len(lines))
	}
	for index, line := range lines {
		if width := lipgloss.Width(line); width != 10 {
			t.Errorf("row %d width %d, want 10", index, width)
		}
	}
	last := []rune(stripped(lines[2]))
	if string(last[:7]) != "·······" || last[7] != '▁' {
		t.Fatalf("baseline should dot missing history and mark zero: %q", string(last))
	}
	for row := 0; row < 3; row++ {
		if []rune(stripped(lines[row]))[9] != '█' {
			t.Fatalf("full-scale column not filled on row %d", row)
		}
	}
}

func TestTrendChartSizes(t *testing.T) {
	values := []float64{10, 40, 90}
	if lines := trendChart(values, 30, 1, 0, 100, percentLabel); len(lines) != 1 || lipgloss.Width(lines[0]) != 30 {
		t.Fatalf("one-row trend should be a sparkline: %q", lines)
	}
	lines := trendChart(values, 30, 4, 0, 100, percentLabel)
	if len(lines) != 4 {
		t.Fatalf("four-row trend has %d rows", len(lines))
	}
	for _, line := range lines {
		if lipgloss.Width(line) > 30 {
			t.Fatalf("trend row overflows: %q", stripped(line))
		}
	}
	if !strings.HasSuffix(stripped(lines[0]), "100%") || !strings.HasSuffix(stripped(lines[3]), "0%") {
		t.Fatalf("trend scale labels missing: %q / %q", stripped(lines[0]), stripped(lines[3]))
	}
	if unlabelled := trendChart(values, 30, 3, 0, 0, nil); len(unlabelled) != 3 ||
		lipgloss.Width(unlabelled[0]) != 30 {
		t.Fatalf("unlabelled trend should use the full width: %q", unlabelled)
	}
}

func TestAxisLabelsStayShort(t *testing.T) {
	tests := map[float64]string{0: "0", 125: "125", 0.25: "0.25", 1500: "1.5k", 2.5e6: "2.5M", 1.25e6: "1.25M", 12.5: "12.5"}
	for input, want := range tests {
		got := formatAxisNumber(input)
		if got != want {
			t.Errorf("formatAxisNumber(%v) = %q, want %q", input, got, want)
		}
		if len(got) > historyLabelWidth {
			t.Errorf("label %q exceeds %d cells", got, historyLabelWidth)
		}
	}
	// Scales are nice numbers; every unit's worst case must fit the shared width.
	worst := map[historyUnit]float64{
		unitPercent: 100, unitBitRate: niceCeil(98765), unitByteRate: niceCeil(9.8e11),
		unitCount: niceCeil(98765), unitLoad: 1.25, unitCelsius: 120,
	}
	for unit, value := range worst {
		if got := formatAxis(unit, value); lipgloss.Width(got) > historyLabelWidth {
			t.Errorf("unit %d label %q exceeds %d cells", unit, got, historyLabelWidth)
		}
	}
}
