package tui

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"kula/internal/collector"
)

const (
	minTerminalWidth  = 32
	minTerminalHeight = 8
	maxBarWidth       = 48
	minBarWidth       = 8
)

type metricItem struct {
	label string
	value string
}

type healthLevel int

const (
	healthOK healthLevel = iota
	healthWatch
	healthCritical
)

func (m model) View() string {
	if m.width <= 0 || m.height <= 0 {
		return ""
	}
	if m.width < minTerminalWidth || m.height < minTerminalHeight {
		return m.renderTooSmall()
	}

	// On a very wide terminal the whole UI becomes a centred column: labels
	// stay near their values and the header's two sides stay in one glance.
	frame := m
	frame.width = m.frameWidth()
	view := frame.renderFrame()
	if frame.width == m.width {
		return view
	}
	left := strings.Repeat(" ", (m.width-frame.width)/2)
	right := strings.Repeat(" ", m.width-frame.width-len(left))
	lines := strings.Split(view, "\n")
	for index := range lines {
		lines[index] = left + lines[index] + right
	}
	return strings.Join(lines, "\n")
}

// maxFrameWidth caps line length; wider terminals get margins instead.
const maxFrameWidth = 136

func (m model) frameWidth() int {
	return min(m.width, maxFrameWidth)
}

func (m model) renderFrame() string {
	header := fitLine(m.renderHeader(), m.width)
	tabs := fitLine(m.renderTabBar(), m.width)

	// Build the active view once per frame; the footer's scroll indicator
	// and the viewport both derive from it.
	var content, footer string
	if m.showHelp {
		content = m.renderHelp(m.width, m.contentHeight())
		footer = m.renderFooterWith(0)
	} else {
		lines := m.contentLines(m.contentWidth())
		content = m.renderViewportLines(lines, m.width, m.contentHeight())
		footer = m.renderFooterWith(max(0, len(lines)-m.contentHeight()))
	}

	return strings.Join([]string{header, tabs, content, fitLine(footer, m.width)}, "\n")
}

func (m model) contentHeight() int {
	return clamp(m.height-3, 1, m.height)
}

func (m model) contentWidth() int {
	width := m.frameWidth()
	switch {
	case width >= 48:
		return width - 4
	case width >= 34:
		return width - 2
	default:
		return width
	}
}

func (m model) renderTooSmall() string {
	message := sBrand.Render("KULA") + "\n" +
		sStrong.Render("Terminal too small") + "\n" +
		sMuted.Render(fmt.Sprintf("Need %d×%d · have %d×%d",
			minTerminalWidth, minTerminalHeight, m.width, m.height))
	return fitBlock(lipgloss.Place(m.width, m.height,
		lipgloss.Center, lipgloss.Center, message), m.width, m.height)
}

func (m model) renderHeader() string {
	left := sBrand.Render("KULA")
	if m.showSystemInfo && m.sample != nil && m.sample.System.Hostname != "" {
		hostWidth := clamp(m.width/3, 10, 32)
		left += sFaint.Render(" / ") + sStrong.Render(truncatePlain(m.sample.System.Hostname, hostWidth))
	}

	if m.width >= 88 && m.sample != nil && m.sample.System.UptimeHuman != "" {
		left += sMuted.Render("  uptime ") + sText.Render(m.sample.System.UptimeHuman)
	}

	var state string
	age := m.sampleAge()
	switch {
	case m.paused:
		state = sWarn.Render("Ⅱ PAUSED")
		if !m.lastUpdated.IsZero() {
			state += sMuted.Render(" " + compactAge(age))
		}
	case m.sample == nil:
		state = sAccent.Render("◌ STARTING")
	case age > max(3*time.Second, 2*m.refreshRate):
		state = sWarn.Render("! STALE " + compactAge(age))
	default:
		state = sGood.Render("● LIVE")
		if m.width >= 108 && m.collectTime > 0 {
			state += sMuted.Render("  sample " + compactDuration(m.collectTime))
		}
	}

	right := state
	if m.width >= 52 {
		right += sFaint.Render("  ") + sText.Render(m.now.Format("15:04:05"))
	}
	return joinSides(left, right, m.width)
}

func (m model) renderTabBar() string {
	tabs := make([]string, 0, numTabs)
	for tab := tabID(0); tab < numTabs; tab++ {
		tabs = append(tabs, m.renderTab(tab))
	}

	// Prefer roomy spacing and tighten it before giving up the full list, so
	// every view stays one digit away. The bar may use the whole width: at 80
	// columns that is exactly what two-space gaps need, and gaps are what
	// keep "1 Overview  2 CPU" from reading as "Overview 2".
	for _, gap := range []string{"  ", " "} {
		full := strings.Join(tabs, gap)
		if lipgloss.Width(full) <= m.width {
			return centerLine(full, m.width)
		}
	}

	previous := (m.activeTab - 1 + numTabs) % numTabs
	next := (m.activeTab + 1) % numTabs
	compact := sFaint.Render("‹ ") + m.renderTab(previous) + "  " +
		m.renderTab(m.activeTab) + "  " + m.renderTab(next) + sFaint.Render(" ›")
	return centerLine(compact, m.width)
}

// renderTab shows the digit that selects a view in a quieter tone than its
// name, so the digit reads as a key hint attached to the name that follows.
func (m model) renderTab(tab tabID) string {
	number := fmt.Sprintf("%d", tab+1)
	if tab == m.activeTab {
		return sAccent.Render(number) + " " + sTabActive.Render(tabNames[tab])
	}
	return sTabNumber.Render(number) + " " + sTabInactive.Render(tabNames[tab])
}

func (m model) renderFooter() string {
	return m.renderFooterWith(m.maxScroll())
}

func (m model) renderFooterWith(maxScroll int) string {
	if m.showHelp {
		return joinSides(
			sKey.Render("? / esc")+" "+sMuted.Render("close help"),
			sMuted.Render("v"+m.version),
			m.width,
		)
	}

	pauseLabel := "pause"
	if m.paused {
		pauseLabel = "resume"
	}

	type hint struct {
		key  string
		text string
	}
	var hints []hint
	switch {
	case m.activeTab == tabHistory && m.width >= 104:
		hints = []hint{
			{"- +", "range"},
			{"[ ]", "pan"},
			{"n", "now"},
			{"tab", "switch"},
			{"↑↓", "scroll"},
			{"?", "keys"},
			{"q", "quit"},
		}
	case m.activeTab == tabHistory && m.width >= 66:
		hints = []hint{
			{"- +", "range"},
			{"[ ]", "pan"},
			{"n", "now"},
			{"↑↓", "scroll"},
			{"?", "help"},
			{"q", "quit"},
		}
	case m.activeTab == tabHistory:
		hints = []hint{
			{"- +", "range"},
			{"[ ]", "pan"},
			{"?", "help"},
			{"q", "quit"},
		}
	case m.width >= 104:
		hints = []hint{
			{"tab", "switch"},
			{"↑↓", "scroll"},
			{"space", pauseLabel},
			{"r", "sample now"},
			{"?", "keys"},
			{"q", "quit"},
		}
	case m.width >= 66:
		hints = []hint{
			{"tab", "switch"},
			{"↑↓", "scroll"},
			{"space", pauseLabel},
			{"r", "now"},
			{"?", "help"},
			{"q", "quit"},
		}
	default:
		hints = []hint{
			{"tab", "switch"},
			{"↑↓", "scroll"},
			{"?", "help"},
			{"q", "quit"},
		}
	}

	parts := make([]string, 0, len(hints))
	for _, hint := range hints {
		parts = append(parts, sKey.Render(hint.key)+" "+sMuted.Render(hint.text))
	}
	left := strings.Join(parts, sFaint.Render("  "))

	right := sMuted.Render("v" + m.version)
	if maxScroll > 0 {
		right = sAccent.Render(fmt.Sprintf("↑ %d/%d ↓", clamp(m.scroll, 0, maxScroll)+1, maxScroll+1))
	}
	return joinSides(left, right, m.width)
}

func (m model) renderHelp(width, height int) string {
	var rows []string
	switch {
	// Each layout is chosen so its bordered panel (rows + 4) fits the height.
	case height < 19:
		rows = []string{
			sBrand.Render("KULA KEYS"),
			sKey.Render("tab h l ← → 1-8") + sMuted.Render("  switch view"),
			sKey.Render("j k ↑ ↓") + sMuted.Render("  scroll"),
			sKey.Render("space / r") + sMuted.Render("  pause / sample"),
			sKey.Render("? esc / q") + sMuted.Render("  close / quit"),
			sKey.Render("- + [ ] n") + sMuted.Render("  history smaller/bigger, pan, now"),
		}
	case height < 29:
		rows = []string{
			sBrand.Render("KULA") + sMuted.Render("  keyboard"),
			"",
			helpRow("tab / shift+tab", "switch view"),
			helpRow("h l  /  ← →", "switch view"),
			helpRow("1 … 8", "jump to view"),
			helpRow("j k  /  ↑ ↓", "scroll"),
			helpRow("pgup / pgdown", "scroll page"),
			helpRow("g / G", "top / bottom"),
			helpRow("space", "pause / resume"),
			helpRow("r", "sample now · reload history"),
			helpRow("- / +", "history: smaller / bigger"),
			helpRow("[ / ]", "history: pan back / forward"),
			helpRow("n", "history: back to now"),
			helpRow("? / esc", "close help"),
			helpRow("q", "quit"),
		}
	default:
		rows = []string{
			sBrand.Render("KULA") + sMuted.Render("  keyboard"),
			"",
			sSection.Render("Navigate"),
			helpRow("tab / shift+tab", "next / previous view"),
			helpRow("h l  /  ← →", "next / previous view"),
			helpRow("1 … 8", "jump directly to a view"),
			"",
			sSection.Render("Move"),
			helpRow("j k  /  ↑ ↓", "scroll one line"),
			helpRow("pgup / pgdown", "scroll one page"),
			helpRow("g / G", "top / bottom"),
			"",
			sSection.Render("Live data"),
			helpRow("space", "pause / resume sampling"),
			helpRow("r", "sample immediately"),
			"",
			sSection.Render("History"),
			helpRow("- / +", "smaller / bigger range"),
			helpRow("[ / ]", "pan back / forward"),
			helpRow("n", "back to now"),
			helpRow("r", "reload stored history"),
			"",
			helpRow("q", "quit"),
		}
	}

	panelWidth := clamp(width-4, 28, 62)
	contentWidth := clamp(panelWidth-6, 1, panelWidth)
	for i := range rows {
		rows[i] = fitLine(rows[i], contentWidth)
	}
	panel := sHelpPanel.Width(contentWidth).Render(strings.Join(rows, "\n"))

	if lipgloss.Height(panel) > height || lipgloss.Width(panel) > width {
		return fitBlock(strings.Join(rows, "\n"), width, height)
	}
	return fitBlock(lipgloss.Place(width, height,
		lipgloss.Center, lipgloss.Center, panel), width, height)
}

func helpRow(key, description string) string {
	return sKey.Render(padRight(key, 19)) + sText.Render(description)
}

func (m model) renderViewportLines(lines []string, width, height int) string {
	contentWidth := m.contentWidth()
	if len(lines) == 0 {
		lines = []string{""}
	}

	scroll := clamp(m.scroll, 0, max(0, len(lines)-height))
	end := min(len(lines), scroll+height)
	visible := lines[scroll:end]

	leftMargin := (width - contentWidth) / 2
	rightMargin := width - contentWidth - leftMargin
	rendered := make([]string, 0, height)
	for _, line := range visible {
		line = fitLine(line, contentWidth)
		rendered = append(rendered,
			strings.Repeat(" ", leftMargin)+line+strings.Repeat(" ", rightMargin))
	}
	for len(rendered) < height {
		rendered = append(rendered, strings.Repeat(" ", width))
	}
	return strings.Join(rendered, "\n")
}

func (m model) contentLines(width int) []string {
	if m.activeTab == tabHistory {
		return m.historyLines(width)
	}
	if m.sample == nil {
		status := m.t.T("collecting_data")
		if !m.collecting {
			status = "No sample available"
		}
		return []string{"", centerLine(sAccent.Render("◌")+" "+sMuted.Render(status), width)}
	}

	switch m.activeTab {
	case tabOverview:
		return m.overviewLines(width)
	case tabCPU:
		return m.cpuLines(width)
	case tabMemory:
		return m.memoryLines(width)
	case tabNetwork:
		return m.networkLines(width)
	case tabDisk:
		return m.diskLines(width)
	case tabProcesses:
		return m.processLines(width)
	case tabGPU:
		return m.gpuLines(width)
	default:
		return nil
	}
}

func (m model) overviewLines(width int) []string {
	sample := m.sample
	level, findings := assessHealth(sample)
	trendWidth := width
	if width >= 66 {
		trendWidth = (width - 3) / 2
	}
	lines := []string{"", sectionLine("System status", width)}
	lines = append(lines, renderHealth(level, findings, width)...)
	lines = append(lines, "")

	rows := m.overviewTrendRows()
	cpuTile := []string{
		sSection.Render("CPU"),
		statusStyle(sample.CPU.Total.Usage).Render(fmt.Sprintf("%.1f%%", sample.CPU.Total.Usage)) +
			sMuted.Render(fmt.Sprintf("  ·  load %.2f", sample.LoadAvg.Load1)),
	}
	cpuTile = append(cpuTile, trendChart(m.histCPU.getAll(), max(8, trendWidth), rows, 0, 100, percentLabel)...)
	cpuTile = append(cpuTile, sMuted.Render(fmt.Sprintf("%d cores  ·  %s", sample.CPU.NumCores, m.trendWindow())))

	memoryTile := []string{
		sSection.Render("MEMORY"),
		statusStyle(sample.Memory.UsedPercent).Render(fmt.Sprintf("%.1f%%", sample.Memory.UsedPercent)) +
			sMuted.Render("  ·  "+fmtBytes(sample.Memory.Used)+" / "+fmtBytes(sample.Memory.Total)),
	}
	memoryTile = append(memoryTile, trendChart(m.histMem.getAll(), max(8, trendWidth), rows, 0, 100, percentLabel)...)
	memoryTile = append(memoryTile, sMuted.Render(fmtBytes(sample.Memory.Available)+" available"))

	totalRx, totalTx := networkTotals(sample.Network.Interfaces)
	trafficScale := m.trafficScale()
	trafficRows := max(1, (rows+1)/2)
	trafficTile := []string{
		sSection.Render("TRAFFIC") + sMuted.Render("  scale "+fmtBitRate(trafficScale)),
		sGood.Render("↓ "+fmtBitRate(totalRx)) + "   " + sAccent.Render("↑ "+fmtBitRate(totalTx)),
	}
	trafficTile = append(trafficTile, labelBaseline(sGood.Render("↓ "),
		trendChart(m.histNetRx.getAll(), max(7, trendWidth-2), trafficRows, 0, trafficScale, nil))...)
	trafficTile = append(trafficTile, labelBaseline(sAccent.Render("↑ "),
		trendChart(m.histNetTx.getAll(), max(7, trendWidth-2), trafficRows, 0, trafficScale, nil))...)

	diskRead, diskWrite, diskBusy := diskTotals(sample.Disks.Devices)
	storageTile := []string{
		sSection.Render("STORAGE"),
		statusStyle(diskBusy).Render(fmt.Sprintf("%.1f%% busy", diskBusy)),
	}
	storageTile = append(storageTile, trendChart(m.histDisk.getAll(), max(8, trendWidth), rows, 0, 100, percentLabel)...)
	storageTile = append(storageTile, sMuted.Render("R "+fmtByteRate(diskRead)+"  ·  W "+fmtByteRate(diskWrite)))

	if width >= 66 {
		columnWidth := (width - 3) / 2
		lines = append(lines, joinBlocks(cpuTile, memoryTile, columnWidth, 3)...)
		lines = append(lines, "")
		lines = append(lines, joinBlocks(trafficTile, storageTile, columnWidth, 3)...)
	} else {
		lines = append(lines, cpuTile...)
		lines = append(lines, "")
		lines = append(lines, memoryTile...)
		lines = append(lines, "")
		lines = append(lines, trafficTile...)
		lines = append(lines, "")
		lines = append(lines, storageTile...)
	}

	lines = append(lines, "", sectionLine("Host", width))
	hostItems := []metricItem{
		{"Uptime", fallback(sample.System.UptimeHuman, "—")},
		{"Processes", fmt.Sprintf("%d · %d running", sample.Process.Total, sample.Process.Running)},
		{"Clock", clockText(sample.System.ClockSync, sample.System.ClockSource)},
		{"Users", fmt.Sprintf("%d", sample.System.UserCount)},
	}
	lines = append(lines, metricGrid(hostItems, width, responsiveColumns(width, 1, 4))...)

	if m.showSystemInfo {
		system := strings.TrimSpace(strings.Join([]string{m.osName, m.kernelVersion, m.cpuArch}, "  ·  "))
		if system != "" {
			lines = append(lines, sMuted.Render(truncatePlain(system, width)))
		}
	}

	if len(sample.GPU) > 0 {
		gpu := sample.GPU[0]
		detail := fmt.Sprintf("%.1f%% load", gpu.LoadPct)
		if gpu.Temperature > 0 {
			detail += fmt.Sprintf("  ·  %.1f°C", gpu.Temperature)
		}
		lines = append(lines, sMuted.Render("GPU  ")+
			sText.Render(truncatePlain(gpu.Name, max(8, width-lipgloss.Width(detail)-7)))+
			sFaint.Render("  ")+statusStyle(gpu.LoadPct).Render(detail))
	}

	return lines
}

// overviewTrendRows lets the overview tiles use a tall terminal's spare rows
// while an 80×24 overview still fits without scrolling.
func (m model) overviewTrendRows() int {
	switch height := m.contentHeight(); {
	case height >= 40:
		return 4
	case height >= 32:
		return 3
	case height >= 27:
		return 2
	default:
		return 1
	}
}

// trendRows grows the detailed views' trend charts into the space a tall
// terminal would otherwise leave empty. Short terminals keep one-line
// sparklines so the numbers stay on screen.
func (m model) trendRows() int {
	switch height := m.contentHeight(); {
	case height >= 40:
		return 6
	case height >= 32:
		return 4
	case height >= 27:
		return 3
	default:
		return 1
	}
}

// trafficScale is the shared ceiling for the receive and transmit trends, so
// the two can be compared: a separate auto-scale draws a 1 kbit/s blip as
// tall as a 1 Gbit/s transfer.
func (m model) trafficScale() float64 {
	return niceCeil(max(maxFinite(m.histNetRx.getAll()), maxFinite(m.histNetTx.getAll())))
}

func percentLabel(value float64) string { return fmt.Sprintf("%.0f%%", value) }

func countLabel(value float64) string { return fmt.Sprintf("%.0f", value) }

func (m model) cpuLines(width int) []string {
	cpu := m.sample.CPU
	load := m.sample.LoadAvg
	lines := []string{"", sectionLine("CPU", width)}
	lines = append(lines, renderGauge("Total", cpu.Total.Usage,
		fmt.Sprintf("%d logical cores", cpu.NumCores), width)...)
	lines = append(lines, trendChart(m.histCPU.getAll(), width, m.trendRows(), 0, 100, percentLabel)...)
	lines = append(lines,
		sMuted.Render("Trend  "+m.trendWindow()),
		"",
		sectionLine("Time share", width),
	)

	timeShare := []metricItem{
		{"User", fmt.Sprintf("%.1f%%", cpu.Total.User)},
		{"System", fmt.Sprintf("%.1f%%", cpu.Total.System)},
		{"I/O wait", fmt.Sprintf("%.1f%%", cpu.Total.IOWait)},
		{"IRQ", fmt.Sprintf("%.1f%%", cpu.Total.IRQ)},
		{"Soft IRQ", fmt.Sprintf("%.1f%%", cpu.Total.SoftIRQ)},
		{"Steal", fmt.Sprintf("%.1f%%", cpu.Total.Steal)},
	}
	lines = append(lines, metricGrid(timeShare, width, responsiveColumns(width, 2, 3))...)

	lines = append(lines, "", sectionLine("Load average", width))
	loadItems := []metricItem{
		{"1 minute", fmt.Sprintf("%.2f", load.Load1)},
		{"5 minutes", fmt.Sprintf("%.2f", load.Load5)},
		{"15 minutes", fmt.Sprintf("%.2f", load.Load15)},
		{"Runnable", fmt.Sprintf("%d / %d", load.Running, load.Total)},
	}
	lines = append(lines, metricGrid(loadItems, width, responsiveColumns(width, 2, 4))...)
	if cpu.NumCores > 0 {
		pressure := load.Load1 / float64(cpu.NumCores) * 100
		lines = append(lines, renderGauge("Capacity", pressure, "1m load / cores", width)...)
	}

	if cpu.Temperature > 0 || len(cpu.Sensors) > 0 {
		lines = append(lines, "", sectionLine("Thermals", width))
		if cpu.Temperature > 0 {
			lines = append(lines, renderTemperature("Package", cpu.Temperature, width))
		}
		for _, sensor := range cpu.Sensors {
			lines = append(lines, renderTemperature(sensor.Name, sensor.Value, width))
		}
	}
	return lines
}

func (m model) memoryLines(width int) []string {
	memory := m.sample.Memory
	swap := m.sample.Swap
	lines := []string{"", sectionLine("Memory", width)}
	lines = append(lines, renderGauge("RAM", memory.UsedPercent,
		fmtBytes(memory.Used)+" / "+fmtBytes(memory.Total), width)...)
	lines = append(lines, trendChart(m.histMem.getAll(), width, m.trendRows(), 0, 100, percentLabel)...)
	lines = append(lines,
		sMuted.Render("Trend  "+m.trendWindow()),
		"",
		sectionLine("Breakdown", width),
	)
	items := []metricItem{
		{"Available", fmtBytes(memory.Available)},
		{"Free", fmtBytes(memory.Free)},
		{"Cached", fmtBytes(memory.Cached)},
		{"Buffers", fmtBytes(memory.Buffers)},
		{"Shared", fmtBytes(memory.Shmem)},
		{"Used", fmtBytes(memory.Used)},
	}
	lines = append(lines, metricGrid(items, width, responsiveColumns(width, 2, 3))...)

	lines = append(lines, "", sectionLine("Swap", width))
	if swap.Total == 0 {
		lines = append(lines, sMuted.Render(m.t.T("no_swap")))
		return lines
	}
	lines = append(lines, renderGauge("Swap", swap.UsedPercent,
		fmtBytes(swap.Used)+" / "+fmtBytes(swap.Total), width)...)
	lines = append(lines, sparkline(m.histSwap.getAll(), width, 0, 100))
	return lines
}

func (m model) networkLines(width int) []string {
	network := m.sample.Network
	totalRx, totalTx := networkTotals(network.Interfaces)
	scale := m.trafficScale()
	rows := max(1, m.trendRows()/2)

	lines := []string{
		"",
		sectionLine("Network", width),
		sGood.Render("↓ "+fmtBitRate(totalRx)) + sMuted.Render("   receive total"),
	}
	lines = append(lines, labelBaseline(sGood.Render("↓ "),
		trendChart(m.histNetRx.getAll(), max(1, width-2), rows, 0, scale, fmtBitRate))...)
	lines = append(lines, sAccent.Render("↑ "+fmtBitRate(totalTx))+sMuted.Render("   transmit total"))
	lines = append(lines, labelBaseline(sAccent.Render("↑ "),
		trendChart(m.histNetTx.getAll(), max(1, width-2), rows, 0, scale, fmtBitRate))...)
	lines = append(lines,
		sMuted.Render("Shared scale 0 – "+fmtBitRate(scale)+"  ·  "+m.trendWindow()),
		"",
		sectionLine("Interfaces", width),
	)

	if len(network.Interfaces) == 0 {
		lines = append(lines, sMuted.Render("No active interfaces"))
	} else if width >= 72 {
		columns := []int{14, 12, 12, 10, 10, 10}
		lines = append(lines,
			tableRow([]string{"INTERFACE", "RECEIVE", "TRANSMIT", "RX PKT/S", "TX PKT/S", "DROPS"},
				columns, true),
			sRule.Render(strings.Repeat("─", min(width, sumInts(columns)))),
		)
		for _, iface := range network.Interfaces {
			drops := iface.RxDrop + iface.TxDrop
			lines = append(lines, tableRow([]string{
				iface.Name,
				fmtBitRate(iface.RxMbps),
				fmtBitRate(iface.TxMbps),
				fmt.Sprintf("%.0f", iface.RxPPS),
				fmt.Sprintf("%.0f", iface.TxPPS),
				fmt.Sprintf("%d", drops),
			}, columns, false))
		}
	} else {
		for _, iface := range network.Interfaces {
			lines = append(lines,
				sStrong.Render(truncatePlain(iface.Name, max(6, width/3)))+
					sMuted.Render("  ↓ ")+valueStyle(fmtBitRate(iface.RxMbps), sText).Render(fmtBitRate(iface.RxMbps))+
					sMuted.Render("  ↑ ")+valueStyle(fmtBitRate(iface.TxMbps), sText).Render(fmtBitRate(iface.TxMbps)),
				sMuted.Render(fmt.Sprintf("  packets %.0f ↓  %.0f ↑  ·  drops %d",
					iface.RxPPS, iface.TxPPS, iface.RxDrop+iface.TxDrop)),
			)
		}
	}

	lines = append(lines, "", sectionLine("TCP / sockets", width))
	tcpItems := []metricItem{
		{"Established", fmt.Sprintf("%d", network.TCP.CurrEstab)},
		{"TCP in use", fmt.Sprintf("%d", network.Sockets.TCPInUse)},
		{"Time wait", fmt.Sprintf("%d", network.Sockets.TCPTw)},
		{"UDP in use", fmt.Sprintf("%d", network.Sockets.UDPInUse)},
		{"Retrans / s", fmt.Sprintf("%.2f", network.TCP.Retrans)},
		{"Input errors / s", fmt.Sprintf("%.2f", network.TCP.InErrs)},
		{"Resets / s", fmt.Sprintf("%.2f", network.TCP.OutRsts)},
	}
	lines = append(lines, metricGrid(tcpItems, width, responsiveColumns(width, 2, 4))...)
	return lines
}

// labelBaseline puts label before a chart's baseline row, where a one-row
// sparkline would carry it, and indents the rows above to match.
func labelBaseline(label string, lines []string) []string {
	indent := strings.Repeat(" ", lipgloss.Width(label))
	for index := range lines {
		if index == len(lines)-1 {
			lines[index] = label + lines[index]
		} else {
			lines[index] = indent + lines[index]
		}
	}
	return lines
}

func (m model) diskLines(width int) []string {
	disks := m.sample.Disks
	readRate, writeRate, busy := diskTotals(disks.Devices)
	lines := []string{"", sectionLine("Storage I/O", width)}
	lines = append(lines, renderGauge("Busy", busy,
		"R "+fmtByteRate(readRate)+"  ·  W "+fmtByteRate(writeRate), width)...)
	lines = append(lines, trendChart(m.histDisk.getAll(), width, m.trendRows(), 0, 100, percentLabel)...)
	lines = append(lines,
		sMuted.Render("Average device utilization  ·  "+m.trendWindow()),
		"",
		sectionLine("Block devices", width),
	)

	if len(disks.Devices) == 0 {
		lines = append(lines, sMuted.Render("No block-device activity"))
	} else if width >= 72 {
		columns := []int{13, 10, 10, 12, 12, 9}
		lines = append(lines,
			tableRow([]string{"DEVICE", "READS/S", "WRITES/S", "READ", "WRITE", "BUSY"},
				columns, true),
			sRule.Render(strings.Repeat("─", min(width, sumInts(columns)))),
		)
		for _, device := range disks.Devices {
			lines = append(lines, tableRow([]string{
				device.Name,
				fmt.Sprintf("%.1f", device.ReadsPerSec),
				fmt.Sprintf("%.1f", device.WritesPerSec),
				fmtByteRate(device.ReadBytesPS),
				fmtByteRate(device.WriteBytesPS),
				fmt.Sprintf("%.1f%%", device.Utilization),
			}, columns, false))
			if device.Temperature > 0 {
				// Indented under its device row, so the device name need not repeat.
				lines = append(lines, renderTemperature("  temperature", device.Temperature, width))
			}
		}
	} else {
		for _, device := range disks.Devices {
			lines = append(lines,
				sStrong.Render(truncatePlain(device.Name, max(6, width/4)))+
					sMuted.Render("  R ")+valueStyle(fmtByteRate(device.ReadBytesPS), sText).Render(fmtByteRate(device.ReadBytesPS))+
					sMuted.Render("  W ")+valueStyle(fmtByteRate(device.WriteBytesPS), sText).Render(fmtByteRate(device.WriteBytesPS))+
					"  "+statusStyle(device.Utilization).Render(fmt.Sprintf("%.1f%%", device.Utilization)),
			)
		}
	}

	lines = append(lines, "", sectionLine("Filesystems", width))
	if len(disks.FileSystems) == 0 {
		lines = append(lines, sMuted.Render("No filesystems reported"))
	} else {
		labelWidth := newGaugeLayout(width).labelWidth
		for _, filesystem := range disks.FileSystems {
			detail := fmtBytes(filesystem.Used) + " / " + fmtBytes(filesystem.Total)
			mount := filesystem.MountPoint
			if lipgloss.Width(mount) > labelWidth {
				// A long mount point gets its own line instead of being cut
				// to an ambiguous prefix such as "/var/lib/do".
				lines = append(lines, sText.Render(truncatePlain(mount, width)))
				mount = ""
			}
			lines = append(lines, renderGauge(mount, filesystem.UsedPct, detail, width)...)
			if width >= 76 && (filesystem.Device != "" || filesystem.FSType != "") {
				lines = append(lines, sMuted.Render("  "+
					truncatePlain(strings.TrimSpace(filesystem.Device+"  "+filesystem.FSType), width-2)))
			}
		}
	}
	return lines
}

func (m model) processLines(width int) []string {
	process := m.sample.Process
	lines := []string{
		"",
		sectionLine("Processes", width),
		sStrong.Render(fmt.Sprintf("%d total", process.Total)) +
			sMuted.Render(fmt.Sprintf("  ·  %d threads", process.Threads)),
	}
	lines = append(lines, trendChart(m.histRunning.getAll(), width, m.trendRows(), 0, 0, countLabel)...)
	lines = append(lines,
		sMuted.Render("Runnable processes  ·  "+m.trendWindow()),
		"",
		sectionLine("States", width),
	)

	states := []struct {
		name      string
		value     int
		active    lipgloss.Style
		activeBar lipgloss.Style
	}{
		{"Running", process.Running, sGood, sBarGood},
		// A neutral state: a visible fill, but no status colour.
		{"Sleeping", process.Sleeping, sText, sBarNeutral},
		{"Blocked", process.Blocked, sWarn, sBarWarn},
		{"Zombie", process.Zombie, sCrit, sBarCrit},
	}
	// The collector counts only R, S, D and Z; idle kernel threads (I) and
	// stopped tasks make up the rest, without which the bars never add up.
	other := process.Total - process.Running - process.Sleeping - process.Blocked - process.Zombie
	if other > 0 {
		states = append(states, struct {
			name      string
			value     int
			active    lipgloss.Style
			activeBar lipgloss.Style
		}{"Other", other, sText, sBarNeutral})
	}
	for _, state := range states {
		lines = append(lines, renderStateGauge(state.name, state.value,
			process.Total, width, state.active, state.activeBar))
	}

	self := m.sample.Self
	lines = append(lines, "", sectionLine("Kula process", width))
	items := []metricItem{
		{"CPU", fmt.Sprintf("%.2f%%", self.CPUPercent)},
		{"Resident memory", fmtBytes(self.MemRSS)},
		{"Open files", fmt.Sprintf("%d", self.FDs)},
	}
	lines = append(lines, metricGrid(items, width, responsiveColumns(width, 1, 3))...)
	return lines
}

func (m model) gpuLines(width int) []string {
	gpus := m.sample.GPU
	lines := []string{"", sectionLine("GPU", width)}
	if len(gpus) == 0 {
		return append(lines, "", sMuted.Render(m.t.T("no_gpus")))
	}

	for index, gpu := range gpus {
		if index > 0 {
			lines = append(lines, "")
		}
		title := fmt.Sprintf("%d  %s", gpu.Index, fallback(gpu.Name, "GPU"))
		lines = append(lines,
			sStrong.Render(truncatePlain(title, max(1, width-18)))+
				sMuted.Render("  "+fallback(gpu.Driver, "unknown driver")))
		lines = append(lines, renderGauge("Core", gpu.LoadPct, "", width)...)
		if gpu.VRAMTotal > 0 {
			lines = append(lines, renderGauge("VRAM", gpu.VRAMUsedPct,
				fmtBytes(gpu.VRAMUsed)+" / "+fmtBytes(gpu.VRAMTotal), width)...)
		}
		if gpu.Temperature > 0 {
			lines = append(lines, renderTemperature("Temperature", gpu.Temperature, width))
		}
		if gpu.PowerW > 0 {
			lines = append(lines, metricGrid([]metricItem{{"Power", fmt.Sprintf("%.1f W", gpu.PowerW)}}, width, 1)...)
		}
	}
	return lines
}

func assessHealth(sample *collector.Sample) (healthLevel, []string) {
	level := healthOK
	var findings []string

	add := func(finding string, findingLevel healthLevel) {
		findings = append(findings, finding)
		if findingLevel > level {
			level = findingLevel
		}
	}
	pressure := func(label string, value float64) {
		switch {
		case value >= 90:
			add(fmt.Sprintf("%s %.0f%%", label, value), healthCritical)
		case value >= 75:
			add(fmt.Sprintf("%s %.0f%%", label, value), healthWatch)
		}
	}

	pressure("CPU", sample.CPU.Total.Usage)
	pressure("memory", sample.Memory.UsedPercent)
	if sample.CPU.NumCores > 0 {
		loadRatio := sample.LoadAvg.Load1 / float64(sample.CPU.NumCores)
		switch {
		case loadRatio >= 1.5:
			add(fmt.Sprintf("load %.1f/core", loadRatio), healthCritical)
		case loadRatio >= 1:
			add(fmt.Sprintf("load %.1f/core", loadRatio), healthWatch)
		}
	}
	if sample.Swap.Total > 0 {
		pressure("swap", sample.Swap.UsedPercent)
	}
	for _, device := range sample.Disks.Devices {
		switch {
		case device.Utilization >= 95:
			add(fmt.Sprintf("%s %.0f%% busy", device.Name, device.Utilization), healthCritical)
		case device.Utilization >= 80:
			add(fmt.Sprintf("%s %.0f%% busy", device.Name, device.Utilization), healthWatch)
		}
	}
	for _, filesystem := range sample.Disks.FileSystems {
		// "disk /" rather than a bare "/", which reads as a separator.
		if filesystem.UsedPct >= 90 {
			add(fmt.Sprintf("disk %s %.0f%% full", filesystem.MountPoint, filesystem.UsedPct), healthCritical)
		} else if filesystem.UsedPct >= 80 {
			add(fmt.Sprintf("disk %s %.0f%% full", filesystem.MountPoint, filesystem.UsedPct), healthWatch)
		}
	}
	if sample.CPU.Temperature >= 90 {
		add(fmt.Sprintf("CPU %.0f°C", sample.CPU.Temperature), healthCritical)
	} else if sample.CPU.Temperature >= 80 {
		add(fmt.Sprintf("CPU %.0f°C", sample.CPU.Temperature), healthWatch)
	}
	for _, gpu := range sample.GPU {
		if gpu.Temperature >= 90 {
			add(fmt.Sprintf("GPU %.0f°C", gpu.Temperature), healthCritical)
		} else if gpu.Temperature >= 82 {
			add(fmt.Sprintf("GPU %.0f°C", gpu.Temperature), healthWatch)
		}
	}
	if sample.Process.Zombie > 0 {
		add(fmt.Sprintf("%d zombie", sample.Process.Zombie), healthWatch)
	}
	if sample.Process.Blocked > 0 {
		add(fmt.Sprintf("%d blocked", sample.Process.Blocked), healthWatch)
	}
	if !sample.System.ClockSync {
		add("clock not synchronized", healthWatch)
	}
	if sample.Network.TCP.Retrans >= 10 {
		add(fmt.Sprintf("%.1f TCP retrans/s", sample.Network.TCP.Retrans), healthWatch)
	}

	return level, findings
}

// renderHealth wraps findings under the badge instead of truncating them, so
// the most severe signal is never the one pushed off-screen. Past three lines
// the remainder is counted.
func renderHealth(level healthLevel, findings []string, width int) []string {
	var badge string
	switch level {
	case healthCritical:
		badge = sCrit.Render("! CRITICAL")
	case healthWatch:
		badge = sWarn.Render("! WATCH")
	default:
		badge = sGood.Render("✓ NOMINAL")
	}
	if len(findings) == 0 {
		return []string{fitLine(badge+"  "+sText.Render("No pressure or fault signals"), width)}
	}

	const maxLines = 3
	indent := lipgloss.Width(badge) + 2
	separator := sFaint.Render("  ·  ")
	lines := []string{badge + "  "}
	lineWidth := indent
	for index, finding := range findings {
		item := sText.Render(finding)
		itemWidth := lipgloss.Width(item)
		if lineWidth > indent {
			if lineWidth+5+itemWidth <= width {
				lines[len(lines)-1] += separator + item
				lineWidth += 5 + itemWidth
				continue
			}
			if len(lines) == maxLines {
				lines[len(lines)-1] += sMuted.Render(fmt.Sprintf("  +%d more", len(findings)-index))
				break
			}
			lines = append(lines, strings.Repeat(" ", indent))
			lineWidth = indent
		}
		lines[len(lines)-1] += item
		lineWidth += itemWidth
	}
	for index := range lines {
		lines[index] = fitLine(lines[index], width)
	}
	return lines
}

// gaugeLayout fixes gauge geometry from the available width alone, so every
// bar in a view starts and ends in the same columns whatever its detail text.
type gaugeLayout struct {
	labelWidth int
	barWidth   int
}

func newGaugeLayout(width int) gaugeLayout {
	labelWidth := clamp(width/5, 7, 16)
	detailBudget := 0
	if width >= 60 {
		detailBudget = clamp(width/3, 18, 30)
	}
	// label + space + bar + space + "100.0%" + gap + detail budget
	barWidth := clamp(width-labelWidth-1-1-6-2-detailBudget, 0, maxBarWidth)
	if barWidth < minBarWidth {
		barWidth = 0
	}
	return gaugeLayout{labelWidth: labelWidth, barWidth: barWidth}
}

// renderGauge renders a labelled percentage bar. A detail that does not fit
// beside the bar moves to its own line rather than being cut mid-value.
func renderGauge(label string, percent float64, detail string, width int) []string {
	layout := newGaugeLayout(width)
	line := renderMetricBarFull(padRight(label, layout.labelWidth), percent, layout.barWidth, "")
	if detail == "" {
		return []string{line}
	}
	if lipgloss.Width(line)+2+lipgloss.Width(detail) <= width {
		return []string{line + sMuted.Render("  "+detail)}
	}
	indent := layout.labelWidth + 1
	return []string{line, strings.Repeat(" ", indent) +
		sMuted.Render(truncatePlain(detail, max(1, width-indent)))}
}

func renderStateGauge(
	label string,
	count, total, width int,
	active, activeBar lipgloss.Style,
) string {
	percent := 0.0
	if total > 0 {
		percent = float64(count) / float64(total) * 100
	}
	percent = sanePercent(percent)

	// A count and its share need more room after the bar than a percentage
	// alone; take it from the bar rather than truncating the share.
	layout := newGaugeLayout(width)
	barWidth := min(layout.barWidth, width-layout.labelWidth-1-15)
	if barWidth < minBarWidth {
		barWidth = 0
	}
	filled := clamp(int(math.Round(percent/100*float64(barWidth))), 0, barWidth)

	countStyle := active
	fillStyle := activeBar
	if count == 0 {
		countStyle = sMuted
		fillStyle = sBarRest
	}

	line := sMuted.Render(padRight(label, layout.labelWidth)) + " "
	if barWidth > 0 {
		line += fillStyle.Render(strings.Repeat("━", filled)) +
			sBarRest.Render(strings.Repeat("─", barWidth-filled)) + " "
	}
	line += countStyle.Render(fmt.Sprintf("%6d", count)) +
		sMuted.Render(fmt.Sprintf("  %5.1f%%", percent))
	return line
}

// renderMetricBarFull renders a labelled gauge and falls back to a compact
// value when the available width cannot hold a meaningful bar.
func renderMetricBarFull(label string, percent float64, barWidth int, detail string) string {
	percent = sanePercent(percent)
	value := statusStyle(percent).Render(fmt.Sprintf("%5.1f%%", percent))
	line := sMuted.Render(label) + " "

	if barWidth >= minBarWidth {
		filled := int(math.Round(percent / 100 * float64(barWidth)))
		filled = clamp(filled, 0, barWidth)
		line += barStyle(percent).Render(strings.Repeat("━", filled)) +
			sBarRest.Render(strings.Repeat("─", barWidth-filled)) + " "
	}
	line += value
	if detail != "" {
		line += sMuted.Render("  " + detail)
	}
	return line
}

// renderTemperature draws a temperature as a gauge on a 0–100 °C track, in
// the same columns as the percentage gauges and coloured at the thresholds
// the health line uses, so a hot sensor reads at a glance.
func renderTemperature(label string, temperature float64, width int) string {
	style, fill := sGood, sBarGood
	switch {
	case temperature >= 90:
		style, fill = sCrit, sBarCrit
	case temperature >= 80:
		style, fill = sWarn, sBarWarn
	}
	layout := newGaugeLayout(width)
	line := sMuted.Render(padRight(label, layout.labelWidth)) + " "
	if layout.barWidth > 0 {
		filled := clamp(int(math.Round(sanePercent(temperature)/100*float64(layout.barWidth))), 0, layout.barWidth)
		line += fill.Render(strings.Repeat("━", filled)) +
			sBarRest.Render(strings.Repeat("─", layout.barWidth-filled)) + " "
	}
	return fitLine(line+style.Render(fmt.Sprintf("%5.1f°C", temperature)), width)
}

func metricGrid(items []metricItem, width, columns int) []string {
	if len(items) == 0 {
		return nil
	}
	columns = clamp(columns, 1, len(items))
	gap := 3
	cellWidth := max(1, (width-gap*(columns-1))/columns)
	lines := make([]string, 0, (len(items)+columns-1)/columns)

	// Size each grid column from its own items: values right-align so digits
	// stacked in a column line up, while a column holding one item keeps its
	// value beside its label. Labels shrink only when a cell cannot hold the
	// column's longest label beside its longest value.
	labelWidths := make([]int, columns)
	valueWidths := make([]int, columns)
	for index, item := range items {
		column := index % columns
		labelWidths[column] = max(labelWidths[column], lipgloss.Width(item.label))
		valueWidths[column] = max(valueWidths[column], lipgloss.Width(item.value))
	}
	for column := range columns {
		labelWidths[column] = clamp(labelWidths[column], 1,
			max(1, cellWidth-min(valueWidths[column], cellWidth/2)-1))
		valueWidths[column] = min(valueWidths[column], max(1, cellWidth-labelWidths[column]-1))
	}

	for start := 0; start < len(items); start += columns {
		cells := make([]string, 0, columns)
		for column := 0; column < columns; column++ {
			index := start + column
			if index >= len(items) {
				cells = append(cells, strings.Repeat(" ", cellWidth))
				continue
			}
			item := items[index]
			cell := sMuted.Render(padRight(item.label, labelWidths[column])+" ") +
				valueStyle(item.value, sStrong).Render(padLeft(item.value, valueWidths[column]))
			cells = append(cells, fitLine(cell, cellWidth))
		}
		lines = append(lines, strings.Join(cells, strings.Repeat(" ", gap)))
	}
	return lines
}

// tableRow renders one table row: the name column bold, figures in the text
// tone, and zero figures muted so activity stands out down each column.
func tableRow(values []string, widths []int, header bool) string {
	var builder strings.Builder
	for index, width := range widths {
		raw := ""
		if index < len(values) {
			raw = values[index]
		}
		value := padLeft(raw, width)
		if index == 0 {
			value = padRight(raw, width)
		}
		switch {
		case header:
			builder.WriteString(sTableHead.Render(value))
		case index == 0:
			builder.WriteString(sTableName.Render(value))
		default:
			builder.WriteString(valueStyle(raw, sText).Render(value))
		}
	}
	return builder.String()
}

// valueStyle mutes a value that reads as zero ("0", "0.00", "0.0%",
// "0 B/s") and otherwise returns style, so non-zero activity draws the eye.
func valueStyle(value string, style lipgloss.Style) lipgloss.Style {
	if isZeroValue(value) {
		return sMuted
	}
	return style
}

// isZeroValue reports whether a formatted value is a single zero quantity. A
// value with a second number, such as "0 / 1520", is not.
func isZeroValue(value string) bool {
	value = strings.TrimSpace(value)
	end := 0
	for end < len(value) && (value[end] >= '0' && value[end] <= '9' || value[end] == '.') {
		end++
	}
	if end == 0 || strings.ContainsAny(value[end:], "0123456789") {
		return false
	}
	number, err := strconv.ParseFloat(value[:end], 64)
	return err == nil && number == 0
}

func joinBlocks(left, right []string, columnWidth, gap int) []string {
	height := max(len(left), len(right))
	lines := make([]string, 0, height)
	for row := 0; row < height; row++ {
		leftLine, rightLine := "", ""
		if row < len(left) {
			leftLine = left[row]
		}
		if row < len(right) {
			rightLine = right[row]
		}
		lines = append(lines,
			fitLine(leftLine, columnWidth)+strings.Repeat(" ", gap)+fitLine(rightLine, columnWidth))
	}
	return lines
}

func sectionLine(title string, width int) string {
	title = strings.ToUpper(title)
	rendered := sSection.Render(title)
	ruleWidth := width - lipgloss.Width(rendered) - 2
	if ruleWidth <= 0 {
		return ansi.Truncate(rendered, width, "")
	}
	return rendered + sFaint.Render("  ") + sRule.Render(strings.Repeat("─", ruleWidth))
}

func sparkline(values []float64, width int, minimum, maximum float64) string {
	if width <= 0 {
		return ""
	}
	if len(values) > width {
		values = values[len(values)-width:]
	}

	if maximum <= minimum {
		minimum = 0
		maximum = 0
		for _, value := range values {
			if isFinite(value) && value > maximum {
				maximum = value
			}
		}
		if maximum <= minimum {
			maximum = 1
		}
	}

	const blocks = "▁▂▃▄▅▆▇█"
	var builder strings.Builder
	if missing := width - len(values); missing > 0 {
		builder.WriteString(sFaint.Render(strings.Repeat("·", missing)))
	}
	for _, value := range values {
		if !isFinite(value) {
			value = minimum
		}
		ratio := (value - minimum) / (maximum - minimum)
		ratio = math.Max(0, math.Min(1, ratio))
		index := int(math.Round(ratio * float64(len([]rune(blocks))-1)))
		builder.WriteRune([]rune(blocks)[index])
	}
	return sAccent.Render(builder.String())
}

func (m model) trendWindow() string {
	samples := m.histCPU.len
	if samples <= 1 {
		return "trend starting"
	}
	duration := m.histTimes.span()
	if duration <= 0 {
		duration = time.Duration(samples-1) * m.refreshRate
	}
	if duration < time.Minute {
		return fmt.Sprintf("last %ds", int(duration.Seconds()))
	}
	if duration < time.Hour {
		return fmt.Sprintf("last %dm", int(duration.Minutes()))
	}
	return fmt.Sprintf("last %.1fh", duration.Hours())
}

func (m model) sampleAge() time.Duration {
	if m.lastUpdated.IsZero() || m.now.Before(m.lastUpdated) {
		return 0
	}
	return m.now.Sub(m.lastUpdated)
}

func compactDuration(duration time.Duration) string {
	if duration < 0 {
		duration = 0
	}
	switch {
	case duration < time.Millisecond:
		return duration.Round(time.Microsecond).String()
	case duration < time.Second:
		return duration.Round(time.Millisecond).String()
	case duration < time.Minute:
		return fmt.Sprintf("%ds", int(duration.Seconds()))
	case duration < time.Hour:
		return fmt.Sprintf("%dm", int(duration.Minutes()))
	default:
		return fmt.Sprintf("%.1fh", duration.Hours())
	}
}

func compactAge(duration time.Duration) string {
	if duration < time.Second {
		return "0s"
	}
	return compactDuration(duration)
}

func networkTotals(interfaces []collector.NetInterface) (float64, float64) {
	var receive, transmit float64
	for _, iface := range interfaces {
		receive += iface.RxMbps
		transmit += iface.TxMbps
	}
	return receive, transmit
}

func diskTotals(devices []collector.DiskDevice) (float64, float64, float64) {
	var readRate, writeRate, busy float64
	for _, device := range devices {
		readRate += device.ReadBytesPS
		writeRate += device.WriteBytesPS
		busy += device.Utilization
	}
	if len(devices) > 0 {
		busy /= float64(len(devices))
	}
	return readRate, writeRate, busy
}

func clockText(synchronized bool, source string) string {
	if synchronized {
		if source != "" {
			return "synced · " + source
		}
		return "synced"
	}
	return "not synced"
}

func barStyle(percent float64) lipgloss.Style {
	switch {
	case percent >= 90:
		return sBarCrit
	case percent >= 75:
		return sBarWarn
	default:
		return sBarGood
	}
}

func statusStyle(percent float64) lipgloss.Style {
	switch {
	case percent >= 90:
		return sCrit
	case percent >= 75:
		return sWarn
	default:
		return sGood
	}
}

func fmtBytes(bytes uint64) string {
	const unit = 1024
	switch {
	case bytes >= unit*unit*unit*unit:
		return fmt.Sprintf("%.1f TiB", float64(bytes)/(unit*unit*unit*unit))
	case bytes >= unit*unit*unit:
		return fmt.Sprintf("%.1f GiB", float64(bytes)/(unit*unit*unit))
	case bytes >= unit*unit:
		return fmt.Sprintf("%.1f MiB", float64(bytes)/(unit*unit))
	case bytes >= unit:
		return fmt.Sprintf("%.1f KiB", float64(bytes)/unit)
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

func fmtByteRate(bytesPerSecond float64) string {
	if !isFinite(bytesPerSecond) || bytesPerSecond < 0 {
		bytesPerSecond = 0
	}
	switch {
	case bytesPerSecond >= 1e9:
		return fmt.Sprintf("%.1f GB/s", bytesPerSecond/1e9)
	case bytesPerSecond >= 1e6:
		return fmt.Sprintf("%.1f MB/s", bytesPerSecond/1e6)
	case bytesPerSecond >= 1e3:
		return fmt.Sprintf("%.1f kB/s", bytesPerSecond/1e3)
	default:
		return fmt.Sprintf("%.0f B/s", bytesPerSecond)
	}
}

func fmtBitRate(megabitsPerSecond float64) string {
	if !isFinite(megabitsPerSecond) || megabitsPerSecond < 0 {
		megabitsPerSecond = 0
	}
	switch {
	case megabitsPerSecond >= 1000:
		return fmt.Sprintf("%.2f Gbit/s", megabitsPerSecond/1000)
	case megabitsPerSecond >= 1:
		return fmt.Sprintf("%.1f Mbit/s", megabitsPerSecond)
	case megabitsPerSecond >= 0.001:
		return fmt.Sprintf("%.0f kbit/s", megabitsPerSecond*1000)
	default:
		return fmt.Sprintf("%.0f bit/s", megabitsPerSecond*1e6)
	}
}

func padRight(value string, width int) string {
	value = truncatePlain(value, width)
	return value + strings.Repeat(" ", max(0, width-lipgloss.Width(value)))
}

func padLeft(value string, width int) string {
	value = truncatePlain(value, width)
	return strings.Repeat(" ", max(0, width-lipgloss.Width(value))) + value
}

func truncatePlain(value string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(value) <= width {
		return value
	}

	var builder strings.Builder
	for _, character := range value {
		next := builder.String() + string(character)
		if lipgloss.Width(next) > width {
			break
		}
		builder.WriteRune(character)
	}
	return builder.String()
}

func fitLine(value string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(value) > width {
		value = ansi.Truncate(value, width, "")
	}
	return value + strings.Repeat(" ", max(0, width-lipgloss.Width(value)))
}

func fitBlock(value string, width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	lines := strings.Split(value, "\n")
	if len(lines) > height {
		lines = lines[:height]
	}
	for index := range lines {
		lines[index] = fitLine(lines[index], width)
	}
	for len(lines) < height {
		lines = append(lines, strings.Repeat(" ", width))
	}
	return strings.Join(lines, "\n")
}

func centerLine(value string, width int) string {
	if lipgloss.Width(value) >= width {
		return ansi.Truncate(value, width, "")
	}
	left := (width - lipgloss.Width(value)) / 2
	return strings.Repeat(" ", left) + value
}

func joinSides(left, right string, width int) string {
	if width <= 0 {
		return ""
	}
	rightWidth := lipgloss.Width(right)
	if rightWidth >= width {
		return ansi.Truncate(right, width, "")
	}
	left = ansi.Truncate(left, max(0, width-rightWidth-1), "")
	gap := max(1, width-lipgloss.Width(left)-rightWidth)
	return left + strings.Repeat(" ", gap) + right
}

func responsiveColumns(width, narrow, wide int) int {
	switch {
	case width >= 90:
		return wide
	case width >= 56:
		return min(wide, max(narrow, 2))
	default:
		return narrow
	}
}

func fallback(value, fallbackValue string) string {
	if strings.TrimSpace(value) == "" {
		return fallbackValue
	}
	return value
}

func sanePercent(value float64) float64 {
	if !isFinite(value) {
		return 0
	}
	return math.Max(0, math.Min(100, value))
}

func isFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func sumInts(values []int) int {
	total := 0
	for _, value := range values {
		total += value
	}
	return total
}

func clamp(value, low, high int) int {
	if high < low {
		return low
	}
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}
