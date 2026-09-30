package tui

import (
	"math"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// brailleCanvas plots dots on a grid two dots wide and four dots tall per
// terminal cell, which gives line charts eight times the resolution of a
// character grid. Each cell carries the style of the last layer drawn in it.
type brailleCanvas struct {
	cols, rows int
	bits       []uint8
	styles     []int
}

// brailleDots maps a dot's (row, column) inside a cell to its Unicode bit.
var brailleDots = [4][2]uint8{
	{0x01, 0x08},
	{0x02, 0x10},
	{0x04, 0x20},
	{0x40, 0x80},
}

func newBrailleCanvas(cols, rows int) *brailleCanvas {
	canvas := &brailleCanvas{
		cols:   max(0, cols),
		rows:   max(0, rows),
		bits:   make([]uint8, max(0, cols*rows)),
		styles: make([]int, max(0, cols*rows)),
	}
	for index := range canvas.styles {
		canvas.styles[index] = -1
	}
	return canvas
}

func (c *brailleCanvas) dotWidth() int  { return c.cols * 2 }
func (c *brailleCanvas) dotHeight() int { return c.rows * 4 }

func (c *brailleCanvas) set(x, y, style int) {
	if x < 0 || y < 0 || x >= c.dotWidth() || y >= c.dotHeight() {
		return
	}
	cell := (y/4)*c.cols + x/2
	c.bits[cell] |= brailleDots[y%4][x%2]
	c.styles[cell] = style
}

func (c *brailleCanvas) vline(x, y0, y1, style int) {
	if y0 > y1 {
		y0, y1 = y1, y0
	}
	for y := y0; y <= y1; y++ {
		c.set(x, y, style)
	}
}

// line draws a Bresenham segment between two dots, inclusive.
func (c *brailleCanvas) line(x0, y0, x1, y1, style int) {
	dx := x1 - x0
	if dx < 0 {
		dx = -dx
	}
	dy := y1 - y0
	if dy > 0 {
		dy = -dy
	}
	sx, sy := 1, 1
	if x0 > x1 {
		sx = -1
	}
	if y0 > y1 {
		sy = -1
	}
	err := dx + dy
	for {
		c.set(x0, y0, style)
		if x0 == x1 && y0 == y1 {
			return
		}
		doubled := 2 * err
		if doubled >= dy {
			err += dy
			x0 += sx
		}
		if doubled <= dx {
			err += dx
			y0 += sy
		}
	}
}

// render returns one string per cell row. Runs of cells sharing a style are
// rendered together, which keeps escape sequences to a minimum.
func (c *brailleCanvas) render(palette []lipgloss.Style) []string {
	lines := make([]string, c.rows)
	for row := 0; row < c.rows; row++ {
		var line, run strings.Builder
		runStyle := -1
		flush := func() {
			if run.Len() == 0 {
				return
			}
			if runStyle >= 0 && runStyle < len(palette) {
				line.WriteString(palette[runStyle].Render(run.String()))
			} else {
				line.WriteString(run.String())
			}
			run.Reset()
		}
		for col := 0; col < c.cols; col++ {
			cell := row*c.cols + col
			if style := c.styles[cell]; style != runStyle {
				flush()
				runStyle = style
			}
			if c.bits[cell] == 0 {
				run.WriteByte(' ')
			} else {
				run.WriteRune(rune(0x2800 + int(c.bits[cell])))
			}
		}
		flush()
		lines[row] = line.String()
	}
	return lines
}

// chartSeries is one plotted line. lower and upper, when both are set, draw a
// min/max envelope behind the line.
type chartSeries struct {
	style        lipgloss.Style
	values       []float64
	lower, upper []float64
}

type chartTick struct {
	pos   float64
	label string
}

// lineChart is a time-series chart: xs places each point across the plot
// (0 is the left edge, 1 the right), and breaks[i] leaves a gap instead of
// joining point i to the one before it.
type lineChart struct {
	xs       []float64
	breaks   []bool
	series   []chartSeries
	min, max float64
	label    func(float64) string
	ticks    []chartTick
	// labelWidth is the minimum y-label width. Stacked charts share one so
	// their plots, and therefore their time axes, line up column for column.
	labelWidth int
}

// render draws the chart width cells wide with rows plot rows, followed by
// the time axis and its labels.
func (c lineChart) render(width, rows int) []string {
	if rows < 1 || width < 12 || !(c.max > c.min) {
		return nil
	}
	label := c.label
	if label == nil {
		label = formatAxisNumber
	}

	dotHeight := rows * 4
	midRow := -1
	middle := (c.min + c.max) / 2
	if rows >= 5 {
		midRow = int(math.Round(float64(dotHeight-1)/2)) / 4
	}
	top, bottom, mid := label(c.max), label(c.min), label(middle)
	labelWidth := max(c.labelWidth, lipgloss.Width(top), lipgloss.Width(bottom))
	if midRow >= 0 {
		labelWidth = max(labelWidth, lipgloss.Width(mid))
	}
	plotCols := width - labelWidth - 2
	if plotCols < 8 {
		return nil
	}

	canvas := newBrailleCanvas(plotCols, rows)
	dotWidth := canvas.dotWidth()
	xDot := func(pos float64) int {
		if !isFinite(pos) {
			pos = 0
		}
		return clamp(int(math.Round(pos*float64(dotWidth-1))), 0, dotWidth-1)
	}
	yDot := func(value float64) int {
		ratio := (c.max - value) / (c.max - c.min)
		return clamp(int(math.Round(ratio*float64(dotHeight-1))), 0, dotHeight-1)
	}

	palette := []lipgloss.Style{sChartBand}
	for _, series := range c.series {
		if series.lower != nil && series.upper != nil {
			c.drawBand(canvas, series, xDot, yDot)
		}
	}
	// Draw in reverse so the first series, the primary one, ends up on top.
	for index := len(c.series) - 1; index >= 0; index-- {
		palette = append(palette, c.series[index].style)
		c.drawLine(canvas, c.series[index].values, len(palette)-1, xDot, yDot)
	}
	plot := canvas.render(palette)

	lines := make([]string, 0, rows+2)
	for row := 0; row < rows; row++ {
		text, axis := "", "│"
		switch row {
		case 0:
			text, axis = top, "┤"
		case rows - 1:
			text, axis = bottom, "┤"
		case midRow:
			text, axis = mid, "┤"
		}
		lines = append(lines, sMuted.Render(padLeft(text, labelWidth))+" "+sRule.Render(axis)+plot[row])
	}

	axis := []rune(strings.Repeat("─", plotCols))
	labels := []rune(strings.Repeat(" ", plotCols))
	lastEnd := -2
	for _, tick := range c.ticks {
		text := []rune(tick.label)
		col := xDot(tick.pos) / 2
		start := clamp(col-len(text)/2, 0, max(0, plotCols-len(text)))
		if start <= lastEnd+1 || start+len(text) > plotCols {
			continue
		}
		copy(labels[start:], text)
		axis[col] = '┬'
		lastEnd = start + len(text) - 1
	}
	lines = append(lines,
		strings.Repeat(" ", labelWidth+1)+sRule.Render("└"+string(axis)),
		strings.Repeat(" ", labelWidth+2)+sMuted.Render(strings.TrimRight(string(labels), " ")),
	)
	return lines
}

func (c lineChart) drawLine(canvas *brailleCanvas, values []float64, style int, xDot func(float64) int, yDot func(float64) int) {
	previousX, previousY, havePrevious := 0, 0, false
	for index, value := range values {
		if index >= len(c.xs) || !isFinite(value) {
			havePrevious = false
			continue
		}
		x, y := xDot(c.xs[index]), yDot(value)
		if havePrevious && !c.brokenAt(index) {
			canvas.line(previousX, previousY, x, y, style)
		} else {
			canvas.set(x, y, style)
		}
		previousX, previousY, havePrevious = x, y, true
	}
}

// drawBand fills every dot column between the envelope's edges, interpolating
// across the columns that lie between two buckets.
func (c lineChart) drawBand(canvas *brailleCanvas, series chartSeries, xDot func(float64) int, yDot func(float64) int) {
	previous := -1
	for index := range c.xs {
		if index >= len(series.lower) || index >= len(series.upper) {
			break
		}
		low, high := series.lower[index], series.upper[index]
		if !isFinite(low) || !isFinite(high) {
			previous = -1
			continue
		}
		x := xDot(c.xs[index])
		if previous >= 0 && !c.brokenAt(index) {
			previousX := xDot(c.xs[previous])
			for column := previousX + 1; column < x; column++ {
				ratio := float64(column-previousX) / float64(x-previousX)
				interpolatedLow := series.lower[previous] + (low-series.lower[previous])*ratio
				interpolatedHigh := series.upper[previous] + (high-series.upper[previous])*ratio
				canvas.vline(column, yDot(interpolatedHigh), yDot(interpolatedLow), 0)
			}
		}
		canvas.vline(x, yDot(high), yDot(low), 0)
		previous = index
	}
}

func (c lineChart) brokenAt(index int) bool {
	return index < len(c.breaks) && c.breaks[index]
}

// areaChart renders values as a filled chart rows tall using eighth blocks,
// the multi-row form of sparkline. Missing history on the left is dotted.
func areaChart(values []float64, width, rows int, minimum, maximum float64) []string {
	if width <= 0 || rows <= 0 {
		return nil
	}
	if len(values) > width {
		values = values[len(values)-width:]
	}
	if !(maximum > minimum) {
		minimum, maximum = 0, niceCeil(maxFinite(values))
	}

	const blocks = "▁▂▃▄▅▆▇█"
	glyphs := []rune(blocks)
	missing := width - len(values)
	lines := make([]string, rows)
	for row := 0; row < rows; row++ {
		var builder strings.Builder
		for _, value := range values {
			if !isFinite(value) {
				value = minimum
			}
			ratio := math.Max(0, math.Min(1, (value-minimum)/(maximum-minimum)))
			level := int(math.Round(ratio * float64(rows*8)))
			cellLevel := level - (rows-1-row)*8
			switch {
			case cellLevel >= 8:
				builder.WriteRune(glyphs[7])
			case cellLevel > 0:
				builder.WriteRune(glyphs[cellLevel-1])
			case row == rows-1:
				builder.WriteRune(glyphs[0])
			default:
				builder.WriteByte(' ')
			}
		}
		prefix := strings.Repeat(" ", max(0, missing))
		if row == rows-1 {
			prefix = sFaint.Render(strings.Repeat("·", max(0, missing)))
		}
		lines[row] = prefix + sAccent.Render(builder.String())
	}
	return lines
}

// niceCeil rounds a positive value up to a readable axis maximum.
func niceCeil(value float64) float64 {
	if !isFinite(value) || value <= 0 {
		return 1
	}
	magnitude := math.Pow(10, math.Floor(math.Log10(value)))
	for _, factor := range []float64{1, 1.2, 1.5, 2, 2.5, 3, 4, 5, 6, 8, 10} {
		if value <= factor*magnitude*(1+1e-9) {
			return factor * magnitude
		}
	}
	return 10 * magnitude
}

// niceFloor rounds a value down to a readable axis minimum.
func niceFloor(value float64) float64 {
	switch {
	case !isFinite(value) || value == 0:
		return 0
	case value < 0:
		return -niceCeil(-value)
	}
	magnitude := math.Pow(10, math.Floor(math.Log10(value)))
	return math.Floor(value/magnitude) * magnitude
}

func maxFinite(values []float64) float64 {
	maximum := math.Inf(-1)
	for _, value := range values {
		if isFinite(value) && value > maximum {
			maximum = value
		}
	}
	if math.IsInf(maximum, -1) {
		return 0
	}
	return maximum
}

var tickIntervals = []time.Duration{
	10 * time.Second, 15 * time.Second, 30 * time.Second,
	time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute,
	15 * time.Minute, 30 * time.Minute,
	time.Hour, 2 * time.Hour, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour,
	24 * time.Hour, 48 * time.Hour, 7 * 24 * time.Hour,
}

// timeTicks places axis labels on round local times, as many as fit across a
// plot plotCols cells wide with room between labels.
func timeTicks(from, to time.Time, plotCols int) []chartTick {
	span := to.Sub(from)
	if span <= 0 || plotCols < 8 {
		return nil
	}

	interval := tickIntervals[len(tickIntervals)-1]
	layout := tickLayout(interval, span)
	for _, candidate := range tickIntervals {
		candidateLayout := tickLayout(candidate, span)
		labelWidth := len(from.Format(candidateLayout))
		if float64(span)/float64(candidate) <= float64(plotCols)/float64(labelWidth+4) {
			interval, layout = candidate, candidateLayout
			break
		}
	}

	var tick time.Time
	if interval >= 24*time.Hour {
		year, month, day := from.Date()
		tick = time.Date(year, month, day, 0, 0, 0, 0, from.Location())
	} else {
		_, offset := from.Zone()
		shift := time.Duration(offset) * time.Second
		tick = from.Add(shift).Truncate(interval).Add(-shift)
	}
	days := int(interval / (24 * time.Hour))
	var ticks []chartTick
	for guard := 0; !tick.After(to) && guard < 1000; guard++ {
		if !tick.Before(from) {
			ticks = append(ticks, chartTick{
				pos:   float64(tick.Sub(from)) / float64(span),
				label: tick.Format(layout),
			})
		}
		if days > 0 {
			tick = tick.AddDate(0, 0, days)
		} else {
			tick = tick.Add(interval)
		}
	}
	return ticks
}

func tickLayout(interval, span time.Duration) string {
	switch {
	case interval < time.Minute:
		return "15:04:05"
	case interval >= 24*time.Hour:
		return "Jan 2"
	case span > 24*time.Hour:
		return "Mon 15:04"
	default:
		return "15:04"
	}
}

// trendChart draws a live trend: a one-line sparkline on short terminals and
// a taller area chart when there is room, labelled with its scale unless
// format is nil. A maximum at or below the minimum scales to the data.
func trendChart(values []float64, width, rows int, minimum, maximum float64, format func(float64) string) []string {
	if rows <= 1 {
		return []string{sparkline(values, width, minimum, maximum)}
	}
	if !(maximum > minimum) {
		minimum, maximum = 0, niceCeil(maxFinite(values))
	}
	if format == nil {
		return areaChart(values, width, rows, minimum, maximum)
	}
	top, bottom := format(maximum), format(minimum)
	labelWidth := max(lipgloss.Width(top), lipgloss.Width(bottom))
	plotWidth := width - labelWidth - 1
	if plotWidth < 8 {
		return []string{sparkline(values, width, minimum, maximum)}
	}
	lines := areaChart(values, plotWidth, rows, minimum, maximum)
	lines[0] += " " + sMuted.Render(top)
	lines[rows-1] += " " + sMuted.Render(bottom)
	return lines
}
