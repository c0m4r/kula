package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"kula/internal/collector"
	"kula/internal/i18n"
)

const (
	defaultHistoryLen = 120
	minRefreshRate    = 100 * time.Millisecond
)

// metricRing is a fixed-capacity circular buffer used by the trend charts.
type metricRing struct {
	buf []float64
	pos int
	len int
}

type timestampRing struct {
	buf []time.Time
	pos int
	len int
}

func newRing(capacity ...int) metricRing {
	size := defaultHistoryLen
	if len(capacity) > 0 && capacity[0] > 0 {
		size = capacity[0]
	}
	return metricRing{buf: make([]float64, size)}
}

func (r *metricRing) push(v float64) {
	if len(r.buf) == 0 {
		return
	}
	r.buf[r.pos] = v
	r.pos = (r.pos + 1) % len(r.buf)
	if r.len < len(r.buf) {
		r.len++
	}
}

// getAll returns values in chronological order, oldest first.
func (r *metricRing) getAll() []float64 {
	if r.len == 0 {
		return nil
	}
	if r.len < len(r.buf) {
		return r.buf[:r.len]
	}

	values := make([]float64, r.len)
	copy(values, r.buf[r.pos:])
	copy(values[len(r.buf)-r.pos:], r.buf[:r.pos])
	return values
}

func newTimestampRing(capacity int) timestampRing {
	return timestampRing{buf: make([]time.Time, capacity)}
}

func (r *timestampRing) push(timestamp time.Time) {
	if len(r.buf) == 0 {
		return
	}
	r.buf[r.pos] = timestamp
	r.pos = (r.pos + 1) % len(r.buf)
	if r.len < len(r.buf) {
		r.len++
	}
}

func (r *timestampRing) span() time.Duration {
	if r.len < 2 {
		return 0
	}

	oldestIndex := 0
	if r.len == len(r.buf) {
		oldestIndex = r.pos
	}
	newestIndex := (r.pos - 1 + len(r.buf)) % len(r.buf)
	oldest := r.buf[oldestIndex]
	newest := r.buf[newestIndex]
	if oldest.IsZero() || newest.Before(oldest) {
		return 0
	}
	return newest.Sub(oldest)
}

type tabID int

const (
	tabOverview tabID = iota
	tabCPU
	tabMemory
	tabNetwork
	tabDisk
	tabProcesses
	tabGPU
	numTabs
)

var tabNames = [numTabs]string{
	"Overview", "CPU", "Memory", "Network", "Storage", "Processes", "GPU",
}

type tickMsg time.Time

type sampleMsg struct {
	sample      *collector.Sample
	finishedAt  time.Time
	collectTime time.Duration
}

type model struct {
	coll           *collector.Collector
	refreshRate    time.Duration
	osName         string
	kernelVersion  string
	cpuArch        string
	version        string
	showSystemInfo bool

	activeTab tabID
	width     int
	height    int
	scroll    int
	sample    *collector.Sample
	now       time.Time

	paused      bool
	collecting  bool
	showHelp    bool
	lastUpdated time.Time
	collectTime time.Duration

	histCPU     metricRing
	histMem     metricRing
	histSwap    metricRing
	histNetRx   metricRing
	histNetTx   metricRing
	histDisk    metricRing
	histRunning metricRing
	histTimes   timestampRing
	t           *i18n.Translator
}

// RunHeadless launches Kula's full-screen real-time terminal monitor.
func RunHeadless(
	coll *collector.Collector,
	refreshRate time.Duration,
	osName, kernelVersion, cpuArch, version string,
	showSystemInfo bool,
) error {
	if refreshRate <= 0 {
		refreshRate = time.Second
	} else if refreshRate < minRefreshRate {
		refreshRate = minRefreshRate
	}

	// Keep roughly two minutes of trends, independent of the configured
	// refresh interval, while bounding redraw and memory costs.
	historySize := int((2 * time.Minute) / refreshRate)
	historySize = clamp(historySize, 30, 240)

	m := model{
		coll:           coll,
		refreshRate:    refreshRate,
		osName:         osName,
		kernelVersion:  kernelVersion,
		cpuArch:        cpuArch,
		version:        version,
		showSystemInfo: showSystemInfo,
		now:            time.Now(),
		collecting:     coll != nil,
		t:              i18n.NewTranslator(""),
		histCPU:        newRing(historySize),
		histMem:        newRing(historySize),
		histSwap:       newRing(historySize),
		histNetRx:      newRing(historySize),
		histNetTx:      newRing(historySize),
		histDisk:       newRing(historySize),
		histRunning:    newRing(historySize),
		histTimes:      newTimestampRing(historySize),
	}

	program := tea.NewProgram(m, tea.WithAltScreen())
	_, err := program.Run()
	return err
}

func (m *model) pushSample(s *collector.Sample) {
	if s == nil {
		return
	}

	m.histCPU.push(s.CPU.Total.Usage)
	m.histMem.push(s.Memory.UsedPercent)
	m.histSwap.push(s.Swap.UsedPercent)
	m.histTimes.push(s.Timestamp)

	var totalRx, totalTx float64
	for _, iface := range s.Network.Interfaces {
		totalRx += iface.RxMbps
		totalTx += iface.TxMbps
	}
	m.histNetRx.push(totalRx)
	m.histNetTx.push(totalTx)

	var totalUtil float64
	for _, device := range s.Disks.Devices {
		totalUtil += device.Utilization
	}
	if len(s.Disks.Devices) > 0 {
		totalUtil /= float64(len(s.Disks.Devices))
	}
	m.histDisk.push(totalUtil)
	m.histRunning.push(float64(s.Process.Running))
}

func doTick(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func collectSample(coll *collector.Collector) tea.Cmd {
	return func() tea.Msg {
		started := time.Now()
		sample := coll.Collect()
		finished := time.Now()
		return sampleMsg{
			sample:      sample,
			finishedAt:  finished,
			collectTime: finished.Sub(started),
		}
	}
}

func (m model) Init() tea.Cmd {
	commands := []tea.Cmd{doTick(m.refreshRate)}
	if m.collecting && m.coll != nil {
		commands = append(commands, collectSample(m.coll))
	}
	return tea.Batch(commands...)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		if msg.Width > 0 && msg.Height > 0 {
			m.width = msg.Width
			m.height = msg.Height
			m.scroll = m.clampScroll(m.scroll)
		}

	case tickMsg:
		m.now = time.Time(msg)
		commands := []tea.Cmd{doTick(m.refreshRate)}
		if !m.paused && !m.collecting && m.coll != nil {
			m.collecting = true
			commands = append(commands, collectSample(m.coll))
		}
		return m, tea.Batch(commands...)

	case sampleMsg:
		m.collecting = false
		m.now = msg.finishedAt
		m.lastUpdated = msg.finishedAt
		m.collectTime = msg.collectTime
		m.sample = msg.sample
		m.pushSample(msg.sample)
		m.scroll = m.clampScroll(m.scroll)

	case tea.KeyMsg:
		if m.showHelp {
			switch msg.String() {
			case "?", "esc":
				m.showHelp = false
			case "q", "Q", "ctrl+c":
				return m, tea.Quit
			}
			return m, nil
		}

		switch msg.String() {
		case "q", "Q", "ctrl+c":
			return m, tea.Quit
		case "?":
			m.showHelp = true
		case " ":
			m.paused = !m.paused
			if !m.paused && !m.collecting && m.coll != nil {
				m.collecting = true
				return m, collectSample(m.coll)
			}
		case "r":
			if !m.collecting && m.coll != nil {
				m.collecting = true
				return m, collectSample(m.coll)
			}
		case "tab", "right", "l":
			m.selectTab((m.activeTab + 1) % numTabs)
		case "shift+tab", "left", "h":
			m.selectTab((m.activeTab - 1 + numTabs) % numTabs)
		case "up", "k":
			m.scroll = m.clampScroll(m.scroll - 1)
		case "down", "j":
			m.scroll = m.clampScroll(m.scroll + 1)
		case "pgup", "ctrl+u":
			m.scroll = m.clampScroll(m.scroll - m.pageStep())
		case "pgdown", "ctrl+d":
			m.scroll = m.clampScroll(m.scroll + m.pageStep())
		case "home", "g":
			m.scroll = 0
		case "end", "G":
			m.scroll = m.maxScroll()
		case "1", "2", "3", "4", "5", "6", "7":
			m.selectTab(tabID(msg.String()[0] - '1'))
		}
	}

	return m, nil
}

func (m *model) selectTab(tab tabID) {
	if tab >= 0 && tab < numTabs && tab != m.activeTab {
		m.activeTab = tab
		m.scroll = 0
	}
}

func (m model) pageStep() int {
	step := m.contentHeight() - 2
	if step < 1 {
		return 1
	}
	return step
}

func (m model) maxScroll() int {
	if m.width <= 0 || m.height <= 0 || m.showHelp || m.sample == nil {
		return 0
	}
	overflow := len(m.contentLines(m.contentWidth())) - m.contentHeight()
	if overflow < 0 {
		return 0
	}
	return overflow
}

func (m model) clampScroll(scroll int) int {
	return clamp(scroll, 0, m.maxScroll())
}
