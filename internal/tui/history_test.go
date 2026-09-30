package tui

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"kula/internal/collector"
	"kula/internal/config"
	"kula/internal/storage"
)

type historyQuery struct {
	from, to time.Time
	points   int
}

// fakeHistory serves a fixed set of buckets, filtered to each query window.
type fakeHistory struct {
	samples    []*storage.AggregatedSample
	retained   []storage.RetainedRange
	resolution string
	err        error
	refreshErr error
	layout     string
	queries    []historyQuery
	refreshes  int
}

func (f *fakeHistory) Refresh() error {
	f.refreshes++
	return f.refreshErr
}

func (f *fakeHistory) QueryRangeWithMeta(from, to time.Time, points int) (*storage.HistoryResult, error) {
	f.queries = append(f.queries, historyQuery{from: from, to: to, points: points})
	if f.err != nil {
		return nil, f.err
	}
	result := &storage.HistoryResult{
		Resolution:       f.resolution,
		SourceResolution: "1s",
		Downsampled:      f.resolution != "1s",
		Complete:         true,
		RequestedFrom:    from,
		RequestedTo:      to,
	}
	for _, sample := range f.samples {
		if !sample.BucketEnd.Before(from) && !sample.BucketStart.After(to) {
			result.Samples = append(result.Samples, sample)
		}
	}
	return result, nil
}

func (f *fakeHistory) RetainedRanges() ([]storage.RetainedRange, time.Duration) {
	return f.retained, time.Second
}

func (f *fakeHistory) LayoutMismatch() string {
	return f.layout
}

var historyTestNow = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

// newFakeHistory stores one 30s bucket every 30s over the last span.
func newFakeHistory(span time.Duration) *fakeHistory {
	source := &fakeHistory{resolution: "30s"}
	start := historyTestNow.Add(-span)
	for at := start; at.Before(historyTestNow); at = at.Add(30 * time.Second) {
		data := newTestSample()
		data.Timestamp = at.Add(30 * time.Second)
		data.CPU.Total.Usage = 20
		source.samples = append(source.samples, &storage.AggregatedSample{
			Timestamp:   data.Timestamp,
			Data:        data,
			BucketStart: at,
			BucketEnd:   at.Add(30 * time.Second),
		})
	}
	source.retained = []storage.RetainedRange{{From: start, To: historyTestNow}}
	return source
}

func newHistoryModel(width, height int, source HistorySource) model {
	m := newTestModel(width, height)
	m.now = historyTestNow
	m.lastUpdated = historyTestNow
	m.history = historyState{source: source, dir: "/var/lib/kula", rangeIndex: defaultHistoryRange}
	return m
}

// press delivers a key and runs any resulting history load to completion,
// the way Bubble Tea would deliver its message.
func press(t *testing.T, m model, key tea.KeyMsg) model {
	t.Helper()
	next, command := m.Update(key)
	return drain(t, next.(model), command)
}

func drain(t *testing.T, m model, command tea.Cmd) model {
	t.Helper()
	for guard := 0; command != nil && guard < 10; guard++ {
		msg := command()
		switch msg.(type) {
		case historyLoadedMsg, historyOpenedMsg:
		default:
			return m
		}
		next, following := m.Update(msg)
		m, command = next.(model), following
	}
	return m
}

func historyView(m model) string {
	return stripped(strings.Join(m.contentLines(m.contentWidth()), "\n"))
}

func TestHistoryTabLoadsOnSelectAndRendersCharts(t *testing.T) {
	source := newFakeHistory(3 * time.Hour)
	m := press(t, newHistoryModel(100, 40, source), runeKey('8'))

	if m.activeTab != tabHistory {
		t.Fatalf("8 selected tab %d, want History", m.activeTab)
	}
	if len(source.queries) != 1 || source.refreshes != 1 {
		t.Fatalf("opening History ran %d queries and %d refreshes, want 1 each",
			len(source.queries), source.refreshes)
	}
	query := source.queries[0]
	if !query.to.Equal(historyTestNow) || query.to.Sub(query.from) != time.Hour {
		t.Fatalf("default window = %s → %s, want the last hour", query.from, query.to)
	}
	if query.points < 60 {
		t.Fatalf("requested %d points, want roughly one per dot column", query.points)
	}

	view := historyView(m)
	for _, want := range []string{
		"HISTORY", "1h", "● following", "11:00 → 12:00", "30s buckets of 1s data",
		"CPU USAGE", "avg 20.0%", "LOAD AVERAGE", "MEMORY USED", "NETWORK bit/s",
		"DISK I/O B/s", "TCP CONNECTIONS", "PROCESSES", "CPU TEMPERATURE", "GPU LOAD",
		"11:20", "11:30", "11:40",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("history view missing %q", want)
		}
	}

	for _, size := range [][2]int{{32, 8}, {50, 18}, {80, 24}, {120, 35}, {180, 50}} {
		sized := m
		sized.width, sized.height = size[0], size[1]
		assertFrameFits(t, sized)
	}
	footer := stripped(m.renderFooter())
	for _, want := range []string{"- +", "range", "[ ]", "pan"} {
		if !strings.Contains(footer, want) {
			t.Errorf("history footer missing %q: %q", want, footer)
		}
	}
}

func TestHistoryKeysBelongToTheHistoryTab(t *testing.T) {
	source := newFakeHistory(3 * time.Hour)
	m := newHistoryModel(100, 40, source)
	m.activeTab = tabCPU
	next, command := m.Update(runeKey('+'))
	if command != nil || next.(model).history.rangeIndex != defaultHistoryRange || len(source.queries) != 0 {
		t.Fatal("history keys acted outside the History tab")
	}

	m = press(t, m, runeKey('8'))
	before := len(source.queries)
	press(t, m, runeKey('r'))
	if len(source.queries) != before+1 {
		t.Fatal("r on the History tab should reload stored history")
	}
}

func TestHistoryRangeKeysZoom(t *testing.T) {
	source := newFakeHistory(48 * time.Hour)
	m := press(t, newHistoryModel(100, 40, source), runeKey('8'))

	m = press(t, m, runeKey('+'))
	if got := historyRanges[m.history.rangeIndex].label; got != "3h" {
		t.Fatalf("+ selected %s, want the bigger 3h", got)
	}
	last := source.queries[len(source.queries)-1]
	if last.to.Sub(last.from) != 3*time.Hour {
		t.Fatalf("bigger range queried %s", last.to.Sub(last.from))
	}

	m = press(t, m, runeKey('-'))
	m = press(t, m, runeKey('_'))
	if got := historyRanges[m.history.rangeIndex].label; got != "15m" {
		t.Fatalf("- and _ selected %s, want the smaller 15m", got)
	}
	m = press(t, m, runeKey('='))
	if got := historyRanges[m.history.rangeIndex].label; got != "1h" {
		t.Fatalf("= selected %s, want 1h", got)
	}

	for index := 0; index < len(historyRanges)+2; index++ {
		m = press(t, m, runeKey('+'))
	}
	if m.history.rangeIndex != len(historyRanges)-1 {
		t.Fatalf("growing the range stopped at %s", historyRanges[m.history.rangeIndex].label)
	}
	queries := len(source.queries)
	m = press(t, m, runeKey('+'))
	if len(source.queries) != queries {
		t.Fatal("growing past the biggest range reloaded anyway")
	}
	for index := 0; index < len(historyRanges)+2; index++ {
		m = press(t, m, runeKey('-'))
	}
	if m.history.rangeIndex != 0 {
		t.Fatalf("shrinking the range stopped at %s", historyRanges[m.history.rangeIndex].label)
	}
}

func TestHistoryLoadsNeverOverlap(t *testing.T) {
	source := newFakeHistory(48 * time.Hour)
	m := press(t, newHistoryModel(100, 40, source), runeKey('8'))

	next, first := m.Update(runeKey('+'))
	m = next.(model)
	if first == nil || !m.history.loading {
		t.Fatal("changing the range should start a load")
	}
	next, second := m.Update(runeKey('+'))
	m = next.(model)
	if second != nil || !m.history.pending {
		t.Fatal("a second change while loading should queue, not start a parallel load")
	}

	// The superseded result must not replace the view; it releases the
	// queued load for the newest window instead.
	stale := first().(historyLoadedMsg)
	next, queued := m.Update(stale)
	m = next.(model)
	if m.history.data.request.rangeIndex == defaultHistoryRange+1 {
		t.Fatal("stale result was applied")
	}
	if queued == nil {
		t.Fatal("finishing a load did not start the queued one")
	}
	m = drain(t, m, queued)
	if got := historyRanges[m.history.data.request.rangeIndex].label; got != "6h" {
		t.Fatalf("view shows %s after two zoom steps, want 6h", got)
	}
}

func TestHistoryPanStopsAtRetentionAndResumesFollowing(t *testing.T) {
	source := newFakeHistory(3 * time.Hour)
	m := press(t, newHistoryModel(100, 40, source), runeKey('8'))

	m = press(t, m, runeKey(']'))
	if !m.history.following() {
		t.Fatal("panning forward from the live edge left following mode")
	}

	m = press(t, m, runeKey('['))
	if want := historyTestNow.Add(-30 * time.Minute); !m.history.end.Equal(want) {
		t.Fatalf("pan back ended at %s, want %s", m.history.end, want)
	}
	if view := historyView(m); !strings.Contains(view, "◂ past") || !strings.Contains(view, "11:30") {
		t.Fatalf("past window not labelled:\n%s", view)
	}

	for index := 0; index < 20; index++ {
		m = press(t, m, runeKey(','))
	}
	oldest := historyTestNow.Add(-3 * time.Hour)
	if midpoint := m.history.end.Add(-30 * time.Minute); midpoint.Before(oldest) {
		t.Fatalf("panned to %s, past the oldest retained %s", m.history.end, oldest)
	}

	for index := 0; index < 20; index++ {
		m = press(t, m, runeKey('.'))
	}
	if !m.history.following() {
		t.Fatal("panning forward to now did not resume following")
	}

	m = press(t, m, runeKey('['))
	m = press(t, m, runeKey('n'))
	if !m.history.following() {
		t.Fatal("n did not return to now")
	}
}

func TestHistoryZoomKeepsPastWindowCentred(t *testing.T) {
	source := newFakeHistory(48 * time.Hour)
	m := press(t, newHistoryModel(100, 40, source), runeKey('8'))
	m.history.end = historyTestNow.Add(-10 * time.Hour)
	centre := m.history.end.Add(-30 * time.Minute)

	m = press(t, m, runeKey('+'))
	if got := m.history.end.Add(-90 * time.Minute); !got.Equal(centre) {
		t.Fatalf("zoom moved the window centre from %s to %s", centre, got)
	}
	for index := 0; index < 3; index++ {
		m = press(t, m, runeKey('+'))
	}
	if !m.history.following() {
		t.Fatal("a window reaching past now should follow the live edge")
	}
}

func TestHistoryFollowsLiveEdgeOnlyWhenVisible(t *testing.T) {
	source := newFakeHistory(3 * time.Hour)
	m := press(t, newHistoryModel(100, 40, source), runeKey('8'))
	// The batch below runs the tick command too, which sleeps one refresh.
	m.refreshRate = time.Millisecond
	interval := m.history.followInterval(m.refreshRate)

	tick := func(m model, at time.Time) (model, int) {
		before := len(source.queries)
		next, command := m.Update(tickMsg(at))
		m = next.(model)
		if batch, ok := command().(tea.BatchMsg); ok {
			for _, command := range batch {
				if command == nil {
					continue
				}
				if loaded, ok := command().(historyLoadedMsg); ok {
					next, _ := m.Update(loaded)
					m = next.(model)
				}
			}
		}
		return m, len(source.queries) - before
	}

	m, loads := tick(m, historyTestNow.Add(time.Second))
	if loads != 0 {
		t.Fatal("reloaded before the follow interval elapsed")
	}
	m, loads = tick(m, historyTestNow.Add(interval))
	if loads != 1 {
		t.Fatalf("follow interval elapsed: %d loads, want 1", loads)
	}
	if !m.history.data.request.to.Equal(historyTestNow.Add(interval)) {
		t.Fatal("follow reload did not move the window to now")
	}

	m.paused = true
	if _, loads = tick(m, historyTestNow.Add(10*interval)); loads != 0 {
		t.Fatal("reloaded while paused")
	}
	m.paused = false

	m.activeTab = tabCPU
	if _, loads = tick(m, historyTestNow.Add(10*interval)); loads != 0 {
		t.Fatal("reloaded while another tab was visible")
	}
	m.activeTab = tabHistory

	m = press(t, m, runeKey('['))
	if _, loads = tick(m, historyTestNow.Add(100*interval)); loads != 0 {
		t.Fatal("a past window reloaded on its own")
	}
}

func TestHistoryReloadsWhenThePlotWidthChanges(t *testing.T) {
	source := newFakeHistory(3 * time.Hour)
	m := press(t, newHistoryModel(100, 40, source), runeKey('8'))
	next, command := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	drain(t, next.(model), command)
	if len(source.queries) != 2 || source.queries[1].points <= source.queries[0].points {
		t.Fatalf("resize did not request a denser window: %+v", source.queries)
	}
}

func TestHistoryStatesExplainThemselves(t *testing.T) {
	permission := fmt.Errorf("tier /var/lib/kula/tier_0.dat: %w", fs.ErrPermission)
	tests := []struct {
		name  string
		setup func() model
		wants []string
	}{
		{
			name: "unreadable storage",
			setup: func() model {
				m := newHistoryModel(100, 40, nil)
				m.history.openErr = permission
				return m
			},
			wants: []string{"History unavailable", "permission denied", "sudo kula --config", "/var/lib/kula"},
		},
		{
			name: "no history recorded",
			setup: func() model {
				source := newFakeHistory(0)
				source.retained = nil
				return press(t, newHistoryModel(100, 40, source), runeKey('8'))
			},
			wants: []string{"No stored history yet", "kula serve", "/var/lib/kula"},
		},
		{
			name: "window outside retention",
			setup: func() model {
				source := newFakeHistory(time.Hour)
				source.samples = nil
				return press(t, newHistoryModel(100, 40, source), runeKey('8'))
			},
			wants: []string{"No samples in this window", "Sep 30 11:00", "return to now"},
		},
		{
			name: "query failure",
			setup: func() model {
				source := newFakeHistory(time.Hour)
				source.err = errors.New("reading tier 0: boom")
				return press(t, newHistoryModel(100, 40, source), runeKey('8'))
			},
			wants: []string{"Could not read history", "boom"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := test.setup()
			m.activeTab = tabHistory
			view := historyView(m)
			for _, want := range test.wants {
				if !strings.Contains(view, want) {
					t.Errorf("view missing %q:\n%s", want, view)
				}
			}
			assertFrameFits(t, m)
		})
	}
}

// Storage that could not be opened is retried while the view is shown, at
// most once per historyReopenInterval, and charts once it opens.
func TestHistoryRetriesStorageThatFailedToOpen(t *testing.T) {
	permission := fmt.Errorf("tier /var/lib/kula/tier_0.dat: %w", fs.ErrPermission)
	source := newFakeHistory(time.Hour)
	opens := 0
	m := newHistoryModel(100, 40, nil)
	m.history.openErr = permission
	m.history.openedAt = historyTestNow
	m.history.open = func() (HistorySource, error) {
		opens++
		if opens == 1 {
			return nil, permission
		}
		return source, nil
	}

	m = press(t, m, runeKey('8'))
	if opens != 0 {
		t.Fatalf("reopened %d times within the retry interval", opens)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		m.now = m.now.Add(historyReopenInterval)
		command := m.refreshHistoryIfStale()
		m = drain(t, m, command)
		if opens != attempt {
			t.Fatalf("after %d intervals: %d opens", attempt, opens)
		}
	}
	if m.history.source == nil || m.history.openErr != nil {
		t.Fatalf("source not adopted (err %v)", m.history.openErr)
	}
	if view := historyView(m); !strings.Contains(view, "CPU USAGE") {
		t.Fatalf("no charts after the storage opened:\n%s", view)
	}
}

// Storage laid out for another config is charted, with a note: resolutions
// and tier choice come from this config.
func TestHistoryNotesStorageLaidOutForAnotherConfig(t *testing.T) {
	source := newFakeHistory(time.Hour)
	source.layout = "tier 0 is 250 MiB on disk but 100 MiB in this configuration"
	m := press(t, newHistoryModel(80, 40, source), runeKey('8'))
	view := historyView(m)
	for _, want := range []string{"Storage differs from this config", "pass the service's --config", source.layout, "CPU USAGE"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q:\n%s", want, view)
		}
	}
	assertFrameFits(t, m)
}

// A record written after a later one (the recording host's clock was set
// back) must not draw the line backwards.
func TestHistoryDropsBucketsOutOfTimeOrder(t *testing.T) {
	source := newFakeHistory(time.Hour)
	late := *source.samples[10]
	late.Timestamp, late.BucketStart, late.BucketEnd = source.samples[3].Timestamp, source.samples[3].BucketStart, source.samples[3].BucketEnd
	source.samples = append(source.samples[:11:11], append([]*storage.AggregatedSample{&late}, source.samples[11:]...)...)

	m := press(t, newHistoryModel(100, 40, source), runeKey('8'))
	samples := m.history.data.samples
	if len(samples) != len(source.samples)-1 {
		t.Fatalf("kept %d of %d buckets, want all but the out-of-order one", len(samples), len(source.samples))
	}
	for i := 1; i < len(samples); i++ {
		if !historyPointTime(samples[i]).After(historyPointTime(samples[i-1])) {
			t.Fatalf("bucket %d at %s follows %s", i, historyPointTime(samples[i]), historyPointTime(samples[i-1]))
		}
	}
}

func TestHistoryHidesChartsWithoutData(t *testing.T) {
	source := newFakeHistory(time.Hour)
	for _, sample := range source.samples {
		sample.Data.Swap = collector.SwapStats{}
		sample.Data.GPU = nil
		sample.Data.CPU.Temperature = 0
	}
	view := historyView(press(t, newHistoryModel(100, 40, source), runeKey('8')))
	for _, hidden := range []string{"SWAP USED", "GPU LOAD", "CPU TEMPERATURE"} {
		if strings.Contains(view, hidden) {
			t.Errorf("chart %q shown without data", hidden)
		}
	}
	if !strings.Contains(view, "CPU USAGE") {
		t.Fatal("charts with data disappeared too")
	}
}

func TestHistoryEnvelopeReportsStoredPeak(t *testing.T) {
	source := newFakeHistory(time.Hour)
	peak := source.samples[len(source.samples)/2]
	peak.Min = newTestSample()
	peak.Min.CPU.Total.Usage = 5
	peak.Max = newTestSample()
	peak.Max.CPU.Total.Usage = 95
	view := historyView(press(t, newHistoryModel(100, 40, source), runeKey('8')))
	if !strings.Contains(view, "max 95.0%") {
		t.Fatalf("peak hidden by averaging:\n%s", view)
	}
}

func TestHistoryLayoutBreaksAcrossOutages(t *testing.T) {
	source := newFakeHistory(time.Hour)
	// Drop twenty minutes of buckets: the collector was down.
	var kept []*storage.AggregatedSample
	outageFrom, outageTo := historyTestNow.Add(-40*time.Minute), historyTestNow.Add(-20*time.Minute)
	for _, sample := range source.samples {
		if sample.BucketStart.Before(outageFrom) || !sample.BucketStart.Before(outageTo) {
			kept = append(kept, sample)
		}
	}
	data := &historyData{
		request: historyRequest{from: historyTestNow.Add(-time.Hour), to: historyTestNow},
		samples: kept,
		step:    30 * time.Second,
	}
	xs, breaks := data.layout()
	gaps := 0
	for index := range breaks {
		if breaks[index] {
			gaps++
			if got := xs[index] - xs[index-1]; got < 0.3 {
				t.Errorf("break at %d spans only %.2f of the window", index, got)
			}
		}
		if xs[index] < 0 || xs[index] > 1 || (index > 0 && xs[index] <= xs[index-1]) {
			t.Fatalf("x positions not ordered within the window: %v", xs)
		}
	}
	if gaps != 1 {
		t.Fatalf("found %d gaps, want exactly the outage", gaps)
	}
}

func TestHistoryChartsARealReadOnlyStore(t *testing.T) {
	cfg := config.StorageConfig{
		Directory: t.TempDir(),
		Tiers: []config.TierConfig{
			{Resolution: time.Second, MaxBytes: 4 * 1024 * 1024},
			{Resolution: time.Minute, MaxBytes: 1024 * 1024},
			{Resolution: 5 * time.Minute, MaxBytes: 1024 * 1024},
		},
	}
	owner, err := storage.NewStore(cfg)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = owner.Close() }()
	start := historyTestNow.Add(-10 * time.Minute)
	for second := 0; second < 600; second++ {
		sample := newTestSample()
		sample.Timestamp = start.Add(time.Duration(second+1) * time.Second)
		sample.CPU.Total.Usage = 30
		if second >= 300 {
			sample.CPU.Total.Usage = 70
		}
		if err := owner.WriteSample(sample); err != nil {
			t.Fatalf("WriteSample: %v", err)
		}
	}

	reader, err := storage.OpenReadOnly(cfg)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer func() { _ = reader.Close() }()

	m := newHistoryModel(100, 40, reader)
	m.history.rangeIndex = 1 // 15m
	m = press(t, m, runeKey('8'))
	if m.history.err != nil || m.history.data == nil || len(m.history.data.samples) == 0 {
		t.Fatalf("no history from the read-only store: err=%v", m.history.err)
	}
	view := historyView(m)
	for _, want := range []string{"CPU USAGE", "avg 50.0%", "max 70.0%", "11:45 → 12:00"} {
		if !strings.Contains(view, want) {
			t.Errorf("real-store history missing %q:\n%s", want, view)
		}
	}
	if !strings.Contains(view, "data from 11:50") {
		t.Errorf("partial coverage not flagged:\n%s", view)
	}
}

func TestHistoryReloadFailureKeepsChartsAndBacksOff(t *testing.T) {
	source := newFakeHistory(3 * time.Hour)
	m := press(t, newHistoryModel(100, 40, source), runeKey('8'))
	m.refreshRate = time.Millisecond // the tick command below sleeps one refresh
	source.err = errors.New("reading tier 0: disk gone")
	m = press(t, m, runeKey('+'))

	view := historyView(m)
	if !strings.Contains(view, "Reload failed: reading tier 0: disk gone") || !strings.Contains(view, "CPU USAGE") {
		t.Fatalf("failed reload should keep the last charts and say so:\n%s", view)
	}

	queries := len(source.queries)
	if m.historyStale() {
		t.Fatal("a failed load should not be retried on the next tick")
	}
	m.now = m.now.Add(m.history.followInterval(m.refreshRate))
	if !m.historyStale() {
		t.Fatal("a failed load should be retried after the follow interval")
	}
	source.err = nil
	next, command := m.Update(tickMsg(m.now))
	m = next.(model)
	if batch, ok := command().(tea.BatchMsg); ok {
		for _, command := range batch {
			if command == nil {
				continue
			}
			if loaded, ok := command().(historyLoadedMsg); ok {
				next, _ := m.Update(loaded)
				m = next.(model)
			}
		}
	}
	if len(source.queries) != queries+1 || m.history.err != nil {
		t.Fatalf("retry did not recover: %d new queries, err %v", len(source.queries)-queries, m.history.err)
	}
	if got := historyRanges[m.history.data.request.rangeIndex].label; got != "3h" {
		t.Fatalf("recovered view shows %s, want the requested 3h", got)
	}
}

func TestHistoryTitlesAndRangeBarReadAtAGlance(t *testing.T) {
	source := newFakeHistory(3 * time.Hour)
	last := source.samples[len(source.samples)-1].Data
	last.CPU.Total.Usage = 33
	last.LoadAvg = collector.LoadAvg{Load1: 1.5, Load5: 1.25, Load15: 0.75}
	m := press(t, newHistoryModel(120, 40, source), runeKey('8'))
	view := historyView(m)

	for _, want := range []string{
		"now 33.0%", "max 33.0%",
		"━ 1m 1.50", "━ 5m 1.25", "━ 15m 0.75",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("history view missing %q:\n%s", want, view)
		}
	}
	rangeBar := stripped(m.historyRangeBar(m.contentWidth()))
	if !strings.HasPrefix(rangeBar, "-  5m") || !strings.Contains(rangeBar, "30d  +") {
		t.Fatalf("range bar should put - at the short end and + at the long end: %q", rangeBar)
	}

	m = press(t, m, runeKey('['))
	if view := historyView(m); !strings.Contains(view, "last 20.0%") || strings.Contains(view, "now ") {
		t.Fatalf("a past window should report its last value, not now:\n%s", view)
	}

	narrow := m
	narrow.width = 40
	for _, line := range strings.Split(historyView(narrow), "\n") {
		if lipgloss.Width(line) > narrow.contentWidth() {
			t.Fatalf("narrow history line overflows: %q", line)
		}
	}
}
