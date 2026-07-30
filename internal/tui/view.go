package tui

import (
	"fmt"
	"math"
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

	header := fitLine(m.renderHeader(), m.width)
	tabs := fitLine(m.renderTabBar(), m.width)
	footer := fitLine(m.renderFooter(), m.width)

	var content string
	if m.showHelp {
		content = m.renderHelp(m.width, m.contentHeight())
	} else {
		content = m.renderViewport(m.width, m.contentHeight())
	}

	return strings.Join([]string{header, tabs, content, footer}, "\n")
}

func (m model) contentHeight() int {
	return clamp(m.height-3, 1, m.height)
}

func (m model) contentWidth() int {
	switch {
	case m.width >= 48:
		return m.width - 4
	case m.width >= 34:
		return m.width - 2
	default:
		return m.width
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
		left += sFaint.Render("  uptime ") + sText.Render(m.sample.System.UptimeHuman)
	}

	var state string
	age := m.sampleAge()
	switch {
	case m.paused:
		state = sWarn.Render("Ⅱ PAUSED")
		if !m.lastUpdated.IsZero() {
			state += sFaint.Render(" " + compactAge(age))
		}
	case m.sample == nil:
		state = sAccent.Render("◌ STARTING")
	case age > max(3*time.Second, 2*m.refreshRate):
		state = sWarn.Render("! STALE " + compactAge(age))
	default:
		state = sGood.Render("● LIVE")
		if m.width >= 108 && m.collectTime > 0 {
			state += sFaint.Render("  sample " + compactDuration(m.collectTime))
		}
	}

	right := state
	if m.width >= 52 {
		right += sFaint.Render("  ") + sText.Render(m.now.Format("15:04:05"))
	}
	return joinSides(left, right, m.width)
}

func (m model) renderTabBar() string {
	var tabs []string
	for tab := tabID(0); tab < numTabs; tab++ {
		label := fmt.Sprintf("%d %s", tab+1, tabNames[tab])
		if tab == m.activeTab {
			tabs = append(tabs, sTabActive.Render(label))
		} else {
			tabs = append(tabs, sTabInactive.Render(label))
		}
	}

	full := strings.Join(tabs, sFaint.Render("  "))
	if lipgloss.Width(full) <= m.width-2 {
		return centerLine(full, m.width)
	}

	previous := (m.activeTab - 1 + numTabs) % numTabs
	next := (m.activeTab + 1) % numTabs
	compact := sFaint.Render("‹ ") +
		sTabInactive.Render(fmt.Sprintf("%d %s", previous+1, tabNames[previous])) +
		sFaint.Render("  ") +
		sTabActive.Render(fmt.Sprintf("%d %s", m.activeTab+1, tabNames[m.activeTab])) +
		sFaint.Render("  ") +
		sTabInactive.Render(fmt.Sprintf("%d %s", next+1, tabNames[next])) +
		sFaint.Render(" ›")
	return centerLine(compact, m.width)
}

func (m model) renderFooter() string {
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
	if maxScroll := m.maxScroll(); maxScroll > 0 {
		right = sAccent.Render(fmt.Sprintf("↑ %d/%d ↓", m.clampScroll(m.scroll)+1, maxScroll+1))
	}
	return joinSides(left, right, m.width)
}

func (m model) renderHelp(width, height int) string {
	var rows []string
	switch {
	case height < 12:
		rows = []string{
			sBrand.Render("KULA KEYS"),
			sKey.Render("tab h l ← →") + sMuted.Render("  switch view"),
			sKey.Render("j k ↑ ↓") + sMuted.Render("  scroll"),
			sKey.Render("space / r") + sMuted.Render("  pause / sample"),
			sKey.Render("? esc / q") + sMuted.Render("  close / quit"),
		}
	case height < 20:
		rows = []string{
			sBrand.Render("KULA") + sFaint.Render("  keyboard"),
			"",
			helpRow("tab / shift+tab", "switch view"),
			helpRow("h l  /  ← →", "switch view"),
			helpRow("1 … 7", "jump to view"),
			helpRow("j k  /  ↑ ↓", "scroll"),
			helpRow("pgup / pgdown", "scroll page"),
			helpRow("g / G", "top / bottom"),
			helpRow("space", "pause / resume"),
			helpRow("r", "sample now"),
			helpRow("? / esc", "close help"),
			helpRow("q", "quit"),
		}
	default:
		rows = []string{
			sBrand.Render("KULA") + sFaint.Render("  keyboard"),
			"",
			sSection.Render("Navigate"),
			helpRow("tab / shift+tab", "next / previous view"),
			helpRow("h l  /  ← →", "next / previous view"),
			helpRow("1 … 7", "jump directly to a view"),
			"",
			sSection.Render("Move"),
			helpRow("j k  /  ↑ ↓", "scroll one line"),
			helpRow("pgup / pgdown", "scroll one page"),
			helpRow("g / G", "top / bottom"),
			"",
			sSection.Render("Live data"),
			helpRow("space", "pause / resume sampling"),
			helpRow("r", "sample immediately"),
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

func (m model) renderViewport(width, height int) string {
	contentWidth := m.contentWidth()
	lines := m.contentLines(contentWidth)
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
	lines := []string{
		"",
		sectionLine("System status", width),
		renderHealth(level, findings, width),
		"",
	}

	cpuTile := []string{
		sSection.Render("CPU"),
		statusStyle(sample.CPU.Total.Usage).Render(fmt.Sprintf("%.1f%%", sample.CPU.Total.Usage)) +
			sMuted.Render(fmt.Sprintf("  ·  load %.2f", sample.LoadAvg.Load1)),
		sparkline(m.histCPU.getAll(), max(8, trendWidth), 0, 100),
		sMuted.Render(fmt.Sprintf("%d cores  ·  %s", sample.CPU.NumCores, m.trendWindow())),
	}
	memoryTile := []string{
		sSection.Render("MEMORY"),
		statusStyle(sample.Memory.UsedPercent).Render(fmt.Sprintf("%.1f%%", sample.Memory.UsedPercent)) +
			sMuted.Render("  ·  "+fmtBytes(sample.Memory.Used)+" / "+fmtBytes(sample.Memory.Total)),
		sparkline(m.histMem.getAll(), max(8, trendWidth), 0, 100),
		sMuted.Render(fmtBytes(sample.Memory.Available) + " available"),
	}

	totalRx, totalTx := networkTotals(sample.Network.Interfaces)
	trafficTile := []string{
		sSection.Render("TRAFFIC"),
		sGood.Render("↓ "+fmtBitRate(totalRx)) +
			sFaint.Render("   ") + sAccent.Render("↑ "+fmtBitRate(totalTx)),
		sGood.Render("↓ ") + sparkline(m.histNetRx.getAll(), max(7, trendWidth-2), 0, 0),
		sAccent.Render("↑ ") + sparkline(m.histNetTx.getAll(), max(7, trendWidth-2), 0, 0),
	}
	diskRead, diskWrite, diskBusy := diskTotals(sample.Disks.Devices)
	storageTile := []string{
		sSection.Render("STORAGE"),
		statusStyle(diskBusy).Render(fmt.Sprintf("%.1f%% busy", diskBusy)),
		sparkline(m.histDisk.getAll(), max(8, trendWidth), 0, 100),
		sMuted.Render("R " + fmtByteRate(diskRead) + "  ·  W " + fmtByteRate(diskWrite)),
	}

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

func (m model) cpuLines(width int) []string {
	cpu := m.sample.CPU
	load := m.sample.LoadAvg
	lines := []string{
		"",
		sectionLine("CPU", width),
		renderGauge("Total", cpu.Total.Usage,
			fmt.Sprintf("%d logical cores", cpu.NumCores), width),
		sparkline(m.histCPU.getAll(), width, 0, 100),
		sMuted.Render("Trend  " + m.trendWindow()),
		"",
		sectionLine("Time share", width),
	}

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
		lines = append(lines, renderGauge("Capacity", pressure, "1m load / cores", width))
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
	lines := []string{
		"",
		sectionLine("Memory", width),
		renderGauge("RAM", memory.UsedPercent,
			fmtBytes(memory.Used)+" / "+fmtBytes(memory.Total), width),
		sparkline(m.histMem.getAll(), width, 0, 100),
		sMuted.Render("Trend  " + m.trendWindow()),
		"",
		sectionLine("Breakdown", width),
	}
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
	lines = append(lines,
		renderGauge("Swap", swap.UsedPercent,
			fmtBytes(swap.Used)+" / "+fmtBytes(swap.Total), width),
		sparkline(m.histSwap.getAll(), width, 0, 100),
	)
	return lines
}

func (m model) networkLines(width int) []string {
	network := m.sample.Network
	totalRx, totalTx := networkTotals(network.Interfaces)
	lines := []string{
		"",
		sectionLine("Network", width),
		sGood.Render("↓ "+fmtBitRate(totalRx)) +
			sFaint.Render("   receive total"),
		sGood.Render("↓ ") + sparkline(m.histNetRx.getAll(), max(1, width-2), 0, 0),
		sAccent.Render("↑ "+fmtBitRate(totalTx)) +
			sFaint.Render("   transmit total"),
		sAccent.Render("↑ ") + sparkline(m.histNetTx.getAll(), max(1, width-2), 0, 0),
		"",
		sectionLine("Interfaces", width),
	}

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
					sFaint.Render("  ↓ ")+sText.Render(fmtBitRate(iface.RxMbps))+
					sFaint.Render("  ↑ ")+sText.Render(fmtBitRate(iface.TxMbps)),
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

func (m model) diskLines(width int) []string {
	disks := m.sample.Disks
	readRate, writeRate, busy := diskTotals(disks.Devices)
	lines := []string{
		"",
		sectionLine("Storage I/O", width),
		renderGauge("Busy", busy,
			"R "+fmtByteRate(readRate)+"  ·  W "+fmtByteRate(writeRate), width),
		sparkline(m.histDisk.getAll(), width, 0, 100),
		sMuted.Render("Average device utilization  ·  " + m.trendWindow()),
		"",
		sectionLine("Block devices", width),
	}

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
				lines = append(lines, sMuted.Render("  ")+
					renderTemperature(device.Name+" temperature", device.Temperature, width-2))
			}
		}
	} else {
		for _, device := range disks.Devices {
			lines = append(lines,
				sStrong.Render(truncatePlain(device.Name, max(6, width/4)))+
					sFaint.Render("  R ")+sText.Render(fmtByteRate(device.ReadBytesPS))+
					sFaint.Render("  W ")+sText.Render(fmtByteRate(device.WriteBytesPS))+
					sFaint.Render("  ")+statusStyle(device.Utilization).Render(fmt.Sprintf("%.1f%%", device.Utilization)),
			)
		}
	}

	lines = append(lines, "", sectionLine("Filesystems", width))
	if len(disks.FileSystems) == 0 {
		lines = append(lines, sMuted.Render("No filesystems reported"))
	} else {
		for _, filesystem := range disks.FileSystems {
			detail := fmtBytes(filesystem.Used) + " / " + fmtBytes(filesystem.Total)
			lines = append(lines,
				renderGauge(truncatePlain(filesystem.MountPoint, max(8, width/3)),
					filesystem.UsedPct, detail, width),
			)
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
			sFaint.Render(fmt.Sprintf("  ·  %d threads", process.Threads)),
		sparkline(m.histRunning.getAll(), width, 0, 0),
		sMuted.Render("Runnable processes  ·  " + m.trendWindow()),
		"",
		sectionLine("States", width),
	}

	states := []struct {
		name      string
		value     int
		active    lipgloss.Style
		activeBar lipgloss.Style
	}{
		{"Running", process.Running, sGood, sBarGood},
		{"Sleeping", process.Sleeping, sText, sBarRest},
		{"Blocked", process.Blocked, sWarn, sBarWarn},
		{"Zombie", process.Zombie, sCrit, sBarCrit},
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
				sFaint.Render("  "+fallback(gpu.Driver, "unknown driver")),
			renderGauge("Core", gpu.LoadPct, "", width),
		)
		if gpu.VRAMTotal > 0 {
			lines = append(lines, renderGauge("VRAM", gpu.VRAMUsedPct,
				fmtBytes(gpu.VRAMUsed)+" / "+fmtBytes(gpu.VRAMTotal), width))
		}

		details := make([]metricItem, 0, 2)
		if gpu.Temperature > 0 {
			details = append(details, metricItem{"Temperature", fmt.Sprintf("%.1f°C", gpu.Temperature)})
		}
		if gpu.PowerW > 0 {
			details = append(details, metricItem{"Power", fmt.Sprintf("%.1f W", gpu.PowerW)})
		}
		if len(details) > 0 {
			lines = append(lines, metricGrid(details, width, responsiveColumns(width, 1, 2))...)
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
		if filesystem.UsedPct >= 90 {
			add(fmt.Sprintf("%s %.0f%% full", filesystem.MountPoint, filesystem.UsedPct), healthCritical)
		} else if filesystem.UsedPct >= 80 {
			add(fmt.Sprintf("%s %.0f%% full", filesystem.MountPoint, filesystem.UsedPct), healthWatch)
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

func renderHealth(level healthLevel, findings []string, width int) string {
	var badge string
	switch level {
	case healthCritical:
		badge = sCrit.Render("! CRITICAL")
	case healthWatch:
		badge = sWarn.Render("! WATCH")
	default:
		badge = sGood.Render("✓ NOMINAL")
	}

	detail := "No pressure or fault signals"
	if len(findings) > 0 {
		detail = strings.Join(findings, "  ·  ")
	}
	return fitLine(badge+sFaint.Render("  ")+sText.Render(detail), width)
}

func renderGauge(label string, percent float64, detail string, width int) string {
	percent = sanePercent(percent)
	labelWidth := clamp(width/5, 7, 16)
	label = padRight(label, labelWidth)
	percentText := fmt.Sprintf("%5.1f%%", percent)
	reserved := labelWidth + 1 + lipgloss.Width(percentText)
	if detail != "" {
		reserved += min(lipgloss.Width(detail)+2, max(0, width/3))
	}
	barWidth := clamp(width-reserved-2, 0, maxBarWidth)
	if barWidth < minBarWidth {
		barWidth = 0
	}
	return renderMetricBarFull(label, percent, barWidth, detail)
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

	labelWidth := clamp(width/5, 7, 16)
	barWidth := clamp(width-labelWidth-20, 0, maxBarWidth)
	if barWidth < minBarWidth {
		barWidth = 0
	}
	filled := clamp(int(math.Round(percent/100*float64(barWidth))), 0, barWidth)

	valueStyle := active
	fillStyle := activeBar
	if count == 0 {
		valueStyle = sMuted
		fillStyle = sBarRest
	}

	line := sMuted.Render(padRight(label, labelWidth)) + " "
	if barWidth > 0 {
		line += fillStyle.Render(strings.Repeat("━", filled)) +
			sBarRest.Render(strings.Repeat("─", barWidth-filled)) + " "
	}
	line += valueStyle.Render(fmt.Sprintf("%d", count)) +
		sFaint.Render(fmt.Sprintf("  %.1f%%", percent))
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
		line += sFaint.Render("  " + detail)
	}
	return line
}

func renderTemperature(label string, temperature float64, width int) string {
	style := sGood
	switch {
	case temperature >= 90:
		style = sCrit
	case temperature >= 80:
		style = sWarn
	}
	return fitLine(sMuted.Render(padRight(label, clamp(width/3, 10, 24)))+" "+
		style.Render(fmt.Sprintf("%.1f°C", temperature)), width)
}

func metricGrid(items []metricItem, width, columns int) []string {
	if len(items) == 0 {
		return nil
	}
	columns = clamp(columns, 1, len(items))
	gap := 3
	cellWidth := max(1, (width-gap*(columns-1))/columns)
	lines := make([]string, 0, (len(items)+columns-1)/columns)

	for start := 0; start < len(items); start += columns {
		cells := make([]string, 0, columns)
		for column := 0; column < columns; column++ {
			index := start + column
			if index >= len(items) {
				cells = append(cells, strings.Repeat(" ", cellWidth))
				continue
			}
			item := items[index]
			label := truncatePlain(item.label, max(1, cellWidth/2))
			cell := sMuted.Render(label+" ") + sStrong.Render(item.value)
			cells = append(cells, fitLine(cell, cellWidth))
		}
		lines = append(lines, strings.Join(cells, strings.Repeat(" ", gap)))
	}
	return lines
}

func tableRow(values []string, widths []int, header bool) string {
	var builder strings.Builder
	for index, width := range widths {
		value := ""
		if index < len(values) {
			value = values[index]
		}
		if index == 0 {
			value = padRight(value, width)
		} else {
			value = padLeft(value, width)
		}
		if header {
			builder.WriteString(sTableHead.Render(value))
		} else if index == 0 {
			builder.WriteString(sTableCell.Render(value))
		} else {
			builder.WriteString(sTableDim.Render(value))
		}
	}
	return builder.String()
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
