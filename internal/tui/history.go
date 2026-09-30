package tui

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"kula/internal/collector"
	"kula/internal/storage"
)

// HistorySource supplies the stored samples the History view charts. A store
// opened with storage.OpenReadOnly satisfies it; Refresh lets the view follow
// what a separately running `kula serve` keeps writing.
type HistorySource interface {
	Refresh() error
	QueryRangeWithMeta(from, to time.Time, targetPoints int) (*storage.HistoryResult, error)
	RetainedRanges() ([]storage.RetainedRange, time.Duration)
}

type historyRange struct {
	label string
	span  time.Duration
}

var historyRanges = []historyRange{
	{"5m", 5 * time.Minute},
	{"15m", 15 * time.Minute},
	{"1h", time.Hour},
	{"3h", 3 * time.Hour},
	{"6h", 6 * time.Hour},
	{"12h", 12 * time.Hour},
	{"24h", 24 * time.Hour},
	{"3d", 3 * 24 * time.Hour},
	{"7d", 7 * 24 * time.Hour},
	{"30d", 30 * 24 * time.Hour},
}

const defaultHistoryRange = 2 // 1h

type historyState struct {
	source  HistorySource
	dir     string
	openErr error

	rangeIndex int
	// end is the right edge of the window; zero follows the live edge.
	end time.Time

	requestID int
	loading   bool
	pending   bool
	loadedAt  time.Time

	data *historyData
	err  error
}

// historyRequest is one window query; the view compares it to the loaded
// data to tell a refresh of the same view from a new one.
type historyRequest struct {
	from, to   time.Time
	rangeIndex int
	end        time.Time
	points     int
}

type historyData struct {
	request     historyRequest
	samples     []*storage.AggregatedSample
	step        time.Duration
	resolution  string
	sourceStep  string
	tier        int
	downsampled bool
	complete    bool
	actualFrom  *time.Time
	retained    []storage.RetainedRange
}

type historyLoadedMsg struct {
	id   int
	data *historyData
	err  error
}

func (h historyState) following() bool {
	return h.end.IsZero()
}

func (h historyState) span() time.Duration {
	return historyRanges[clamp(h.rangeIndex, 0, len(historyRanges)-1)].span
}

// followInterval paces reloads at the live edge: often enough that the right
// edge keeps up, rarely enough that wide windows are not re-read every tick.
func (h historyState) followInterval(refreshRate time.Duration) time.Duration {
	interval := h.span() / 300
	interval = max(interval, 2*time.Second, refreshRate)
	return min(interval, time.Minute)
}

func (m model) historyRequest() historyRequest {
	to := m.history.end
	if to.IsZero() {
		to = m.now
	}
	return historyRequest{
		from:       to.Add(-m.history.span()),
		to:         to,
		rangeIndex: m.history.rangeIndex,
		end:        m.history.end,
		points:     m.historyPoints(),
	}
}

// historyPoints asks for about one bucket per braille dot column.
func (m model) historyPoints() int {
	return clamp((m.contentWidth()-historyAxisReserve)*2, 60, 2000)
}

// historyLabelWidth fits every y-axis label formatAxis produces; with the
// space and rule after it, historyAxisReserve cells precede each plot.
const (
	historyLabelWidth  = 5
	historyAxisReserve = historyLabelWidth + 2
)

// requestHistory starts a load for the current window, or queues one behind
// a load already in flight so at most one query runs at a time.
func (m *model) requestHistory() tea.Cmd {
	m.history.requestID++
	return m.startHistoryLoad()
}

func (m *model) startHistoryLoad() tea.Cmd {
	if m.history.source == nil {
		return nil
	}
	if m.history.loading {
		m.history.pending = true
		return nil
	}
	m.history.loading = true
	m.history.pending = false
	return loadHistory(m.history.source, m.history.requestID, m.historyRequest())
}

func loadHistory(source HistorySource, id int, request historyRequest) tea.Cmd {
	return func() tea.Msg {
		refreshErr := source.Refresh()
		result, err := source.QueryRangeWithMeta(request.from, request.to, request.points)
		if err != nil {
			return historyLoadedMsg{id: id, err: err}
		}
		retained, _ := source.RetainedRanges()
		step, _ := time.ParseDuration(result.Resolution)
		data := &historyData{
			request:     request,
			samples:     result.Samples,
			step:        step,
			resolution:  result.Resolution,
			sourceStep:  result.SourceResolution,
			tier:        result.Tier,
			downsampled: result.Downsampled,
			complete:    result.Complete,
			actualFrom:  result.ActualFrom,
			retained:    retained,
		}
		if len(data.samples) == 0 && refreshErr != nil {
			return historyLoadedMsg{id: id, data: data, err: refreshErr}
		}
		return historyLoadedMsg{id: id, data: data}
	}
}

func (m *model) applyHistory(msg historyLoadedMsg) tea.Cmd {
	m.history.loading = false
	if msg.id == m.history.requestID {
		m.history.err = msg.err
		if msg.data != nil {
			m.history.data = msg.data
		}
		m.history.loadedAt = m.now
	}
	if m.history.pending {
		return m.startHistoryLoad()
	}
	return nil
}

// historyStale reports whether the History view should reload before it is
// shown: nothing loaded yet, the window or plot width changed, or the live
// edge moved on.
func (m model) historyStale() bool {
	if m.history.source == nil {
		return false
	}
	dueAgain := !m.paused && m.now.Sub(m.history.loadedAt) >= m.history.followInterval(m.refreshRate)
	data := m.history.data
	switch {
	case m.history.err != nil:
		// Retry a failure on the follow cadence, not on every tick.
		return dueAgain
	case data == nil:
		return true
	}
	request := m.historyRequest()
	if data.request.rangeIndex != request.rangeIndex ||
		!data.request.end.Equal(request.end) ||
		data.request.points != request.points {
		return true
	}
	// A past window is fixed; only the live edge moves on.
	return m.history.following() && dueAgain
}

func (m *model) refreshHistoryIfStale() tea.Cmd {
	if m.activeTab != tabHistory || m.history.loading || !m.historyStale() {
		return nil
	}
	return m.requestHistory()
}

// handleHistoryKey applies the History view's own keys. It reports false for
// keys the view does not use, which then fall through to the global bindings.
func (m *model) handleHistoryKey(key string) (tea.Cmd, bool) {
	switch key {
	// - shrinks the window and + grows it; _ and = are the same keys without
	// or with shift, so either works.
	case "-", "_":
		return m.zoomHistory(-1), true
	case "+", "=":
		return m.zoomHistory(1), true
	case "[", ",", "<":
		return m.panHistory(-1), true
	case "]", ".", ">":
		return m.panHistory(1), true
	case "n":
		m.history.end = time.Time{}
		return m.requestHistory(), true
	case "r":
		return m.requestHistory(), true
	}
	return nil, false
}

// zoomHistory steps through the range presets, keeping the centre of a past
// window in view and returning to the live edge when it would pass now.
func (m *model) zoomHistory(direction int) tea.Cmd {
	next := clamp(m.history.rangeIndex+direction, 0, len(historyRanges)-1)
	if next == m.history.rangeIndex {
		return nil
	}
	if !m.history.following() {
		centre := m.history.end.Add(-m.history.span() / 2)
		m.history.end = centre.Add(historyRanges[next].span / 2)
		if !m.history.end.Before(m.now) {
			m.history.end = time.Time{}
		}
	}
	m.history.rangeIndex = next
	return m.requestHistory()
}

// panHistory moves the window by half its span. It stops at the oldest
// retained sample going back and resumes following at the live edge.
func (m *model) panHistory(direction int) tea.Cmd {
	span := m.history.span()
	end := m.history.end
	if end.IsZero() {
		if direction > 0 {
			return nil
		}
		end = m.now
	}
	end = end.Add(time.Duration(direction) * span / 2)

	if direction < 0 {
		oldest, ok := m.historyOldest()
		if !ok || !end.Add(-span/2).After(oldest) {
			return nil
		}
	}
	if !end.Before(m.now) {
		end = time.Time{}
	}
	m.history.end = end
	return m.requestHistory()
}

func (m model) historyOldest() (time.Time, bool) {
	if m.history.data == nil {
		return time.Time{}, false
	}
	var oldest time.Time
	for _, retained := range m.history.data.retained {
		if oldest.IsZero() || retained.From.Before(oldest) {
			oldest = retained.From
		}
	}
	return oldest, !oldest.IsZero()
}

type historyUnit int

const (
	unitPercent historyUnit = iota
	unitBitRate
	unitByteRate
	unitCount
	unitLoad
	unitCelsius
)

type historyMetric struct {
	label string
	style lipgloss.Style
	value func(*collector.Sample) (float64, bool)
}

type historyChartDef struct {
	title   string
	unit    historyUnit
	metrics []historyMetric
	// envelope draws the stored per-bucket min/max behind the first metric.
	// Only direct fields qualify: the extremes of a sum or average across
	// devices are not the sum or average of their extremes.
	envelope bool
}

var historyCharts = []historyChartDef{
	{
		title: "CPU usage", unit: unitPercent, envelope: true,
		metrics: []historyMetric{{"usage", sAccent, func(s *collector.Sample) (float64, bool) {
			return s.CPU.Total.Usage, true
		}}},
	},
	{
		title: "Load average", unit: unitLoad,
		metrics: []historyMetric{
			{"1m", sAccent, func(s *collector.Sample) (float64, bool) { return s.LoadAvg.Load1, true }},
			{"5m", sSeriesAlt, func(s *collector.Sample) (float64, bool) { return s.LoadAvg.Load5, true }},
			{"15m", sMuted, func(s *collector.Sample) (float64, bool) { return s.LoadAvg.Load15, true }},
		},
	},
	{
		title: "Memory used", unit: unitPercent, envelope: true,
		metrics: []historyMetric{{"used", sAccent, func(s *collector.Sample) (float64, bool) {
			return s.Memory.UsedPercent, s.Memory.Total > 0
		}}},
	},
	{
		title: "Swap used", unit: unitPercent, envelope: true,
		metrics: []historyMetric{{"used", sAccent, func(s *collector.Sample) (float64, bool) {
			return s.Swap.UsedPercent, s.Swap.Total > 0
		}}},
	},
	{
		title: "Network", unit: unitBitRate,
		metrics: []historyMetric{
			{"↓ receive", sGood, func(s *collector.Sample) (float64, bool) {
				receive, _ := networkTotals(s.Network.Interfaces)
				return receive, len(s.Network.Interfaces) > 0
			}},
			{"↑ transmit", sAccent, func(s *collector.Sample) (float64, bool) {
				_, transmit := networkTotals(s.Network.Interfaces)
				return transmit, len(s.Network.Interfaces) > 0
			}},
		},
	},
	{
		title: "Disk I/O", unit: unitByteRate,
		metrics: []historyMetric{
			{"read", sGood, func(s *collector.Sample) (float64, bool) {
				read, _, _ := diskTotals(s.Disks.Devices)
				return read, len(s.Disks.Devices) > 0
			}},
			{"write", sAccent, func(s *collector.Sample) (float64, bool) {
				_, write, _ := diskTotals(s.Disks.Devices)
				return write, len(s.Disks.Devices) > 0
			}},
		},
	},
	{
		title: "Disk busy", unit: unitPercent,
		metrics: []historyMetric{{"average", sAccent, func(s *collector.Sample) (float64, bool) {
			_, _, busy := diskTotals(s.Disks.Devices)
			return busy, len(s.Disks.Devices) > 0
		}}},
	},
	{
		title: "TCP connections", unit: unitCount, envelope: true,
		metrics: []historyMetric{{"established", sAccent, func(s *collector.Sample) (float64, bool) {
			return float64(s.Network.TCP.CurrEstab), true
		}}},
	},
	{
		title: "Processes", unit: unitCount,
		metrics: []historyMetric{
			{"running", sAccent, func(s *collector.Sample) (float64, bool) { return float64(s.Process.Running), true }},
			{"blocked", sWarn, func(s *collector.Sample) (float64, bool) { return float64(s.Process.Blocked), true }},
		},
	},
	{
		title: "CPU temperature", unit: unitCelsius, envelope: true,
		metrics: []historyMetric{{"package", sAccent, func(s *collector.Sample) (float64, bool) {
			return s.CPU.Temperature, s.CPU.Temperature > 0
		}}},
	},
	{
		title: "GPU load", unit: unitPercent, envelope: true,
		metrics: []historyMetric{{"load", sAccent, func(s *collector.Sample) (float64, bool) {
			if len(s.GPU) == 0 {
				return 0, false
			}
			return s.GPU[0].LoadPct, true
		}}},
	},
}

// historyChartRows sizes plots so about two charts share a screen.
func (m model) historyChartRows() int {
	return clamp((m.contentHeight()-4)/2-4, 3, 10)
}

func (m model) historyLines(width int) []string {
	lines := []string{
		"",
		sectionLine("History", width),
		m.historyRangeBar(width),
		m.historyWindowLine(width),
		"",
	}

	history := m.history
	switch {
	case history.source == nil:
		return append(lines, historyProblem("History unavailable", history.dir, history.openErr, width)...)
	case history.data == nil && history.err == nil:
		return append(lines, centerLine(sAccent.Render("◌")+" "+sMuted.Render("Loading history…"), width))
	case history.data == nil || (history.err != nil && len(history.data.samples) == 0):
		return append(lines, historyProblem("Could not read history", history.dir, history.err, width)...)
	case len(history.data.samples) == 0:
		return append(lines, m.historyEmpty(width)...)
	case history.err != nil:
		// Keep the last good charts, but say they are not current.
		lines = append(lines,
			sWarn.Render("! ")+sMuted.Render(truncatePlain("Reload failed: "+history.err.Error(), max(1, width-2))),
			"")
	}

	rendered := 0
	for _, def := range historyCharts {
		chartLines := m.renderHistoryChart(def, width)
		if len(chartLines) == 0 {
			continue
		}
		if rendered > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, chartLines...)
		rendered++
	}
	if rendered == 0 {
		lines = append(lines, sMuted.Render("No chartable values in this window."))
	}
	return lines
}

func (m model) historyRangeBar(width int) string {
	var state string
	switch {
	case m.history.source == nil:
		state = sMuted.Render("unavailable")
	case !m.history.following():
		state = sWarn.Render("◂ past") + "  " + sKey.Render("n") + sMuted.Render(" now")
	case m.paused:
		state = sWarn.Render("Ⅱ paused")
	default:
		state = sGood.Render("● following")
	}
	if m.history.loading && m.historyChanging() {
		state = sAccent.Render("◌ loading") + "  " + state
	}

	parts := make([]string, 0, len(historyRanges))
	for index, preset := range historyRanges {
		if index == m.history.rangeIndex {
			parts = append(parts, sTabActive.Render(preset.label))
		} else {
			parts = append(parts, sMuted.Render(preset.label))
		}
	}
	// The keys bracket the presets at the end each one moves towards.
	for _, gap := range []string{"  ", " "} {
		full := sKey.Render("-") + gap + strings.Join(parts, gap) + gap + sKey.Render("+")
		if lipgloss.Width(full)+lipgloss.Width(state)+2 <= width {
			return joinSides(full, state, width)
		}
	}
	compact := sKey.Render("-") + " " + sTabActive.Render(historyRanges[m.history.rangeIndex].label) +
		" " + sKey.Render("+")
	return joinSides(compact, state, width)
}

// historyChanging reports whether the in-flight load replaces the view with a
// different window rather than refreshing the one on screen.
func (m model) historyChanging() bool {
	if m.history.data == nil {
		return true
	}
	request := m.historyRequest()
	loaded := m.history.data.request
	return loaded.rangeIndex != request.rangeIndex || !loaded.end.Equal(request.end)
}

func (m model) historyWindowLine(width int) string {
	data := m.history.data
	if m.history.source == nil || data == nil {
		return ""
	}
	request := data.request
	layout := "15:04"
	if request.to.Sub(request.from) >= 24*time.Hour || request.from.YearDay() != request.to.YearDay() {
		layout = "Jan 2 15:04"
	}
	text := sText.Render(request.from.Format(layout) + " → " + request.to.Format(layout))

	separator := sFaint.Render("  ·  ")
	// Segments in priority order; a narrow view keeps the window and the
	// coverage warning and drops the bucket description.
	if len(data.samples) > 0 && !data.complete && data.actualFrom != nil &&
		data.actualFrom.After(request.from.Add(max(data.step, time.Second))) {
		warning := separator +
			sWarn.Render("data from "+data.actualFrom.In(request.from.Location()).Format(layout))
		if lipgloss.Width(text+warning) <= width {
			text += warning
		}
	}
	var source string
	switch {
	case len(data.samples) == 0:
	case data.downsampled:
		source = fmt.Sprintf("%s buckets of %s data", data.resolution, data.sourceStep)
	default:
		source = fmt.Sprintf("%s samples", data.sourceStep)
	}
	if source != "" && lipgloss.Width(text+separator+source) <= width {
		// The window line reads left to right: window, then how it was
		// sampled, then any warning.
		if index := strings.Index(text, separator); index >= 0 {
			text = text[:index] + separator + sMuted.Render(source) + text[index:]
		} else {
			text += separator + sMuted.Render(source)
		}
	}
	return text
}

func historyProblem(title, dir string, err error, width int) []string {
	lines := []string{sWarn.Render(title)}
	if err != nil {
		lines = append(lines, sMuted.Render(truncatePlain(err.Error(), width)))
	}
	return append(lines, historyHints(dir, err, width)...)
}

func (m model) historyEmpty(width int) []string {
	data := m.history.data
	if len(data.retained) == 0 {
		lines := []string{sStrong.Render("No stored history yet")}
		return append(lines, historyHints(m.history.dir, nil, width)...)
	}

	oldest, _ := m.historyOldest()
	var newest time.Time
	for _, retained := range data.retained {
		if retained.To.After(newest) {
			newest = retained.To
		}
	}
	// Stored timestamps decode in the local zone; show them in the window's.
	location := data.request.from.Location()
	return []string{
		sStrong.Render("No samples in this window"),
		sMuted.Render("Stored history covers ") +
			sText.Render(oldest.In(location).Format("Jan 2 15:04")+" → "+
				newest.In(location).Format("Jan 2 15:04")),
		sKey.Render("n") + sMuted.Render(" return to now  ·  ") +
			sKey.Render("+") + sMuted.Render(" bigger range"),
	}
}

func historyHints(dir string, err error, width int) []string {
	var lines []string
	if dir != "" {
		lines = append(lines, sMuted.Render("Reading ")+sText.Render(truncatePlain(dir, max(1, width-8))))
	}
	lines = append(lines, "",
		sMuted.Render(truncatePlain("History is recorded by `kula serve`; the TUI charts it read-only.", width)))
	if errors.Is(err, fs.ErrPermission) {
		lines = append(lines, sMuted.Render(truncatePlain(
			"Run the TUI as the service user, e.g. sudo kula --config /etc/kula/config.yaml tui", width)))
	} else {
		lines = append(lines, sMuted.Render(truncatePlain(
			"Start the service, or pass --config with the file it uses, to chart its data.", width)))
	}
	return lines
}

// renderHistoryChart plots one chart definition over the loaded window. A
// chart with no values in the window (no swap, no GPU…) renders nothing.
func (m model) renderHistoryChart(def historyChartDef, width int) []string {
	data := m.history.data
	request := data.request
	span := request.to.Sub(request.from)
	if span <= 0 {
		return nil
	}

	count := len(data.samples)
	xs, breaks := data.layout()

	series := make([]chartSeries, len(def.metrics))
	hasValue := false
	low, high := math.Inf(1), math.Inf(-1)
	for metricIndex, metric := range def.metrics {
		values := make([]float64, count)
		var lower, upper []float64
		envelope := def.envelope && metricIndex == 0
		if envelope {
			lower = make([]float64, count)
			upper = make([]float64, count)
		}
		for index, sample := range data.samples {
			values[index] = sampleValue(metric, sample.Data)
			if isFinite(values[index]) {
				hasValue = true
				low, high = math.Min(low, values[index]), math.Max(high, values[index])
			}
			if envelope {
				lower[index] = sampleValue(metric, sample.Min)
				upper[index] = sampleValue(metric, sample.Max)
				if isFinite(lower[index]) && isFinite(upper[index]) {
					low, high = math.Min(low, lower[index]), math.Max(high, upper[index])
				}
			}
		}
		if envelope && !anyFinite(lower) {
			lower, upper = nil, nil
		}
		series[metricIndex] = chartSeries{style: metric.style, values: values, lower: lower, upper: upper}
	}
	if !hasValue {
		return nil
	}

	minimum, maximum := historyScale(def.unit, low, high)
	plotCols := width - historyAxisReserve
	chart := lineChart{
		xs:     xs,
		breaks: breaks,
		series: series,
		min:    minimum,
		max:    maximum,
		label:  func(value float64) string { return formatAxis(def.unit, value) },
		ticks:  timeTicks(request.from, request.to, plotCols),

		labelWidth: historyLabelWidth,
	}
	body := chart.render(width, m.historyChartRows())
	if body == nil {
		return nil
	}
	title := historyChartTitle(def, series, width, request.end.IsZero())
	return append([]string{title}, body...)
}

// layout positions each bucket across the window (0 at the left edge, 1 at
// the right) and marks a break where buckets are missing, so an outage shows
// as a gap instead of a straight line bridging it.
func (d *historyData) layout() ([]float64, []bool) {
	span := d.request.to.Sub(d.request.from)
	xs := make([]float64, len(d.samples))
	breaks := make([]bool, len(d.samples))
	if span <= 0 {
		return xs, breaks
	}
	// Raw samples jitter around their interval; allow a missed tick or two.
	limit := max(2*d.step, 2*time.Second) + d.step/2
	var previous time.Time
	for index, sample := range d.samples {
		at := historyPointTime(sample)
		xs[index] = math.Max(0, math.Min(1, float64(at.Sub(d.request.from))/float64(span)))
		if index > 0 && at.Sub(previous) > limit {
			breaks[index] = true
		}
		previous = at
	}
	return xs, breaks
}

// historyPointTime places a bucket at its midpoint, so a coarse bucket does
// not appear shifted right by half its width.
func historyPointTime(sample *storage.AggregatedSample) time.Time {
	if !sample.BucketStart.IsZero() && sample.BucketEnd.After(sample.BucketStart) {
		return sample.BucketStart.Add(sample.BucketEnd.Sub(sample.BucketStart) / 2)
	}
	return sample.Timestamp
}

func sampleValue(metric historyMetric, sample *collector.Sample) float64 {
	if sample == nil {
		return math.NaN()
	}
	value, ok := metric.value(sample)
	if !ok || !isFinite(value) {
		return math.NaN()
	}
	return value
}

func anyFinite(values []float64) bool {
	for _, value := range values {
		if isFinite(value) {
			return true
		}
	}
	return false
}

// historyScale fixes percentages to 0–100 so charts compare at a glance and
// rounds everything else to a readable range around the data.
func historyScale(unit historyUnit, low, high float64) (float64, float64) {
	switch unit {
	case unitPercent:
		return 0, 100
	case unitCelsius:
		minimum := niceFloor(low - 5)
		if minimum < 0 {
			minimum = 0
		}
		maximum := niceCeil(high + 2)
		if maximum <= minimum {
			maximum = minimum + 10
		}
		return minimum, maximum
	default:
		return 0, niceCeil(high)
	}
}

// historyChartTitle heads a chart with its latest reading and summary. A
// single-metric chart shows "now" (or "last" for a past window), avg and max
// on the right; a multi-series legend carries each line's latest value, so
// lines can be told apart without matching colours. When space runs short
// the summary goes first, then the legend values, then the legend.
func historyChartTitle(def historyChartDef, series []chartSeries, width int, following bool) string {
	title := sSection.Render(strings.ToUpper(def.title))
	if unit := unitName(def.unit); unit != "" {
		title += sMuted.Render(" " + unit)
	}

	primary := series[0]
	var sum float64
	count := 0
	peak := math.Inf(-1)
	for index, value := range primary.values {
		if !isFinite(value) {
			continue
		}
		sum += value
		count++
		high := value
		if primary.upper != nil && isFinite(primary.upper[index]) {
			high = math.Max(high, primary.upper[index])
		}
		peak = math.Max(peak, high)
	}
	if count == 0 {
		return fitLine(title, width)
	}

	reading := "last "
	if following {
		reading = "now "
	}
	stat := func(label, value string, style lipgloss.Style) string {
		return sMuted.Render(label) + style.Render(value)
	}
	maxStat := stat("max ", formatValue(def.unit, peak), sStrong)
	avgStat := stat("avg ", formatValue(def.unit, sum/float64(count)), sText)

	var candidates [][2]string
	if len(def.metrics) == 1 {
		nowStat := stat(reading, formatValue(def.unit, lastFinite(primary.values)), sStrong)
		candidates = [][2]string{
			{title, nowStat + "  " + avgStat + "  " + maxStat},
			{title, nowStat + "  " + maxStat},
			{title, nowStat},
		}
	} else {
		legend, bare := title, title
		for index, metric := range def.metrics {
			key := "  " + series[index].style.Render("━") + " " + sMuted.Render(metric.label)
			bare += key
			if last := lastFinite(series[index].values); isFinite(last) {
				key += " " + sText.Render(formatValue(def.unit, last))
			}
			legend += key
		}
		primaryMax := sMuted.Render(def.metrics[0].label+" ") + maxStat
		candidates = [][2]string{{legend, primaryMax}, {legend, ""}, {bare, ""}}
	}
	for _, candidate := range candidates {
		left, right := candidate[0], candidate[1]
		if lipgloss.Width(left)+lipgloss.Width(right)+2 <= width {
			return joinSides(left, right, width)
		}
	}
	return fitLine(title, width)
}

func lastFinite(values []float64) float64 {
	for index := len(values) - 1; index >= 0; index-- {
		if isFinite(values[index]) {
			return values[index]
		}
	}
	return math.NaN()
}

func formatValue(unit historyUnit, value float64) string {
	switch unit {
	case unitPercent:
		return fmt.Sprintf("%.1f%%", value)
	case unitBitRate:
		return fmtBitRate(value)
	case unitByteRate:
		return fmtByteRate(value)
	case unitCount:
		if value < 10 && value != math.Trunc(value) {
			return fmt.Sprintf("%.1f", value)
		}
		return fmt.Sprintf("%.0f", value)
	case unitLoad:
		return fmt.Sprintf("%.2f", value)
	case unitCelsius:
		return fmt.Sprintf("%.1f°C", value)
	default:
		return formatAxisNumber(value)
	}
}

// unitName is shown beside a chart title for units the axis labels omit.
func unitName(unit historyUnit) string {
	switch unit {
	case unitBitRate:
		return "bit/s"
	case unitByteRate:
		return "B/s"
	default:
		return ""
	}
}

// formatAxis keeps y-axis labels within historyLabelWidth cells; rate units
// move to the chart title (see unitName).
func formatAxis(unit historyUnit, value float64) string {
	switch unit {
	case unitPercent:
		return fmt.Sprintf("%.0f%%", value)
	case unitBitRate:
		return formatAxisNumber(value * 1e6)
	case unitCelsius:
		return fmt.Sprintf("%.0f°C", value)
	default:
		return formatAxisNumber(value)
	}
}

// formatAxisNumber renders a value with an SI suffix in at most five cells.
func formatAxisNumber(value float64) string {
	if !isFinite(value) {
		return "—"
	}
	magnitude := math.Abs(value)
	for _, unit := range []struct {
		scale  float64
		suffix string
	}{{1e12, "T"}, {1e9, "G"}, {1e6, "M"}, {1e3, "k"}} {
		if magnitude >= unit.scale {
			return trimNumber(value/unit.scale) + unit.suffix
		}
	}
	return trimNumber(value)
}

func trimNumber(value float64) string {
	magnitude := math.Abs(value)
	var text string
	switch {
	case magnitude == 0:
		return "0"
	case magnitude >= 100:
		text = fmt.Sprintf("%.0f", value)
	case magnitude >= 10:
		text = fmt.Sprintf("%.1f", value)
	default:
		text = fmt.Sprintf("%.2f", value)
	}
	if strings.Contains(text, ".") {
		text = strings.TrimRight(strings.TrimRight(text, "0"), ".")
	}
	return text
}
