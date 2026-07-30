package tui

import (
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"kula/internal/collector"
	"kula/internal/i18n"
)

func newTestSample() *collector.Sample {
	return &collector.Sample{
		Timestamp: time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC),
		CPU: collector.CPUStats{
			Total: collector.CPUCoreStats{
				User:    10.5,
				System:  5.2,
				IOWait:  0.3,
				IRQ:     0.1,
				SoftIRQ: 0.2,
				Steal:   0,
				Usage:   16.3,
			},
			NumCores:    4,
			Temperature: 55,
			Sensors:     []collector.CPUTempSensor{{Name: "core0", Value: 54}},
		},
		LoadAvg: collector.LoadAvg{
			Load1: 0.5, Load5: 0.8, Load15: 1, Running: 2, Total: 200,
		},
		Memory: collector.MemoryStats{
			Total:       16 * 1024 * 1024 * 1024,
			Used:        8 * 1024 * 1024 * 1024,
			Free:        4 * 1024 * 1024 * 1024,
			Available:   5 * 1024 * 1024 * 1024,
			Cached:      2 * 1024 * 1024 * 1024,
			Buffers:     512 * 1024 * 1024,
			Shmem:       256 * 1024 * 1024,
			UsedPercent: 50,
		},
		Swap: collector.SwapStats{
			Total:       4 * 1024 * 1024 * 1024,
			Used:        1 * 1024 * 1024 * 1024,
			Free:        3 * 1024 * 1024 * 1024,
			UsedPercent: 25,
		},
		Network: collector.NetworkStats{
			Interfaces: []collector.NetInterface{{
				Name: "eth0", RxMbps: 10.5, TxMbps: 2.3,
				RxPPS: 500, TxPPS: 100, RxDrop: 1,
			}},
			TCP: collector.TCPStats{
				CurrEstab: 42, InErrs: 0.01, OutRsts: 0.05, Retrans: 0.2,
			},
			Sockets: collector.SocketStats{TCPInUse: 30, TCPTw: 5, UDPInUse: 10},
		},
		Disks: collector.DiskStats{
			Devices: []collector.DiskDevice{{
				Name: "nvme0n1", ReadsPerSec: 10, WritesPerSec: 5,
				ReadBytesPS: 2e6, WriteBytesPS: 1e6,
				Utilization: 30, Temperature: 38,
			}},
			FileSystems: []collector.FileSystemInfo{
				{
					Device: "/dev/nvme0n1p2", MountPoint: "/", FSType: "ext4",
					Total: 100e9, Used: 40e9, UsedPct: 40,
				},
				{
					Device: "/dev/nvme0n1p3", MountPoint: "/home", FSType: "ext4",
					Total: 500e9, Used: 200e9, UsedPct: 40,
				},
			},
		},
		System: collector.SystemStats{
			Hostname: "testhost", Uptime: 3600, UptimeHuman: "1h 0m",
			ClockSync: true, ClockSource: "ntp", Entropy: 3500, UserCount: 2,
		},
		Process: collector.ProcessStats{
			Total: 200, Running: 2, Sleeping: 198, Threads: 800,
		},
		Self: collector.SelfStats{
			CPUPercent: 0.5, MemRSS: 10 * 1024 * 1024, FDs: 15,
		},
		GPU: []collector.GPUStats{{
			Index: 0, Name: "NVIDIA RTX 4090", Driver: "nvidia",
			LoadPct: 45, Temperature: 55,
			VRAMUsed: 4 * 1024 * 1024 * 1024, VRAMTotal: 24 * 1024 * 1024 * 1024,
			VRAMUsedPct: 16.7, PowerW: 120,
		}},
	}
}

func newTestModel(width, height int) model {
	sample := newTestSample()
	m := model{
		width:          width,
		height:         height,
		sample:         sample,
		now:            time.Date(2026, 7, 30, 14, 30, 0, 0, time.UTC),
		lastUpdated:    time.Date(2026, 7, 30, 14, 30, 0, 0, time.UTC),
		refreshRate:    time.Second,
		osName:         "Test Linux",
		kernelVersion:  "6.1.0-test",
		cpuArch:        "amd64",
		version:        "1.0.0",
		showSystemInfo: true,
		t:              i18n.NewTranslator("en"),
		histCPU:        newRing(),
		histMem:        newRing(),
		histSwap:       newRing(),
		histNetRx:      newRing(),
		histNetTx:      newRing(),
		histDisk:       newRing(),
		histRunning:    newRing(),
		histTimes:      newTimestampRing(defaultHistoryLen),
	}
	m.pushSample(sample)
	return m
}

func runeKey(character rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{character}}
}

func stripped(value string) string {
	return ansi.Strip(value)
}

func assertFrameFits(t *testing.T, m model) {
	t.Helper()
	view := m.View()
	lines := strings.Split(view, "\n")
	if len(lines) != m.height {
		t.Fatalf("frame has %d lines, want %d", len(lines), m.height)
	}
	for index, line := range lines {
		if got := lipgloss.Width(line); got != m.width {
			t.Errorf("line %d has width %d, want %d: %q", index+1, got, m.width, stripped(line))
		}
	}
}

func TestMetricRingChronologyAndCapacity(t *testing.T) {
	ring := newRing(3)
	for _, value := range []float64{1, 2, 3, 4, 5} {
		ring.push(value)
	}
	got := ring.getAll()
	want := []float64{3, 4, 5}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("history[%d] = %.0f, want %.0f", index, got[index], want[index])
		}
	}
}

func TestMetricRingZeroCapacityIsSafe(t *testing.T) {
	ring := metricRing{}
	ring.push(1)
	if values := ring.getAll(); values != nil {
		t.Fatalf("zero-capacity history = %v, want nil", values)
	}
}

func TestPushSamplePopulatesAllTrends(t *testing.T) {
	m := newTestModel(80, 24)
	sample := newTestSample()
	sample.Network.Interfaces = append(sample.Network.Interfaces,
		collector.NetInterface{RxMbps: 1.5, TxMbps: 0.7})
	sample.Disks.Devices = append(sample.Disks.Devices,
		collector.DiskDevice{Utilization: 50})

	before := m.histCPU.len
	m.pushSample(sample)
	if m.histCPU.len != before+1 || m.histMem.len != before+1 ||
		m.histNetRx.len != before+1 || m.histDisk.len != before+1 {
		t.Fatal("pushSample did not advance every trend")
	}
	rx := m.histNetRx.getAll()
	if rx[len(rx)-1] != 12 {
		t.Fatalf("aggregate receive = %.1f, want 12.0", rx[len(rx)-1])
	}
	disk := m.histDisk.getAll()
	if disk[len(disk)-1] != 40 {
		t.Fatalf("average disk utilization = %.1f, want 40.0", disk[len(disk)-1])
	}

	m.pushSample(nil)
}

func TestViewFillsRepresentativeTerminalSizes(t *testing.T) {
	sizes := [][2]int{
		{32, 8},
		{40, 12},
		{50, 18},
		{80, 24},
		{120, 35},
		{180, 50},
	}
	for _, size := range sizes {
		t.Run(strings.Join([]string{
			strconv.Itoa(size[0]), "x", strconv.Itoa(size[1]),
		}, ""), func(t *testing.T) {
			for tab := tabID(0); tab < numTabs; tab++ {
				m := newTestModel(size[0], size[1])
				m.activeTab = tab
				assertFrameFits(t, m)
			}
		})
	}
}

func TestViewTooSmallIsUsefulAndBounded(t *testing.T) {
	for _, size := range [][2]int{{20, 5}, {31, 7}, {12, 2}} {
		m := newTestModel(size[0], size[1])
		assertFrameFits(t, m)
		if size[0] >= 20 && size[1] >= 5 &&
			!strings.Contains(stripped(m.View()), "Terminal too small") {
			t.Errorf("%dx%d fallback lacks guidance", size[0], size[1])
		}
	}
}

func TestViewBeforeWindowSizeIsEmpty(t *testing.T) {
	m := newTestModel(0, 0)
	if view := m.View(); view != "" {
		t.Fatalf("pre-size View = %q, want empty", view)
	}
}

func TestOverviewFitsStandardTerminalWithoutScrolling(t *testing.T) {
	m := newTestModel(80, 24)
	if got := m.maxScroll(); got != 0 {
		t.Fatalf("80x24 overview needs %d lines of scrolling", got)
	}
	view := stripped(m.View())
	for _, want := range []string{
		"SYSTEM STATUS", "CPU", "MEMORY", "TRAFFIC", "STORAGE", "HOST",
		"testhost", "NVIDIA",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("overview missing %q", want)
		}
	}
}

func TestNavigationAlwaysExposesViews(t *testing.T) {
	wide := newTestModel(80, 24)
	wideTabs := stripped(wide.renderTabBar())
	for _, want := range []string{"1 Overview", "2 CPU", "7 GPU"} {
		if !strings.Contains(wideTabs, want) {
			t.Errorf("standard tab bar missing %q", want)
		}
	}

	narrow := newTestModel(40, 18)
	narrow.activeTab = tabNetwork
	narrowTabs := stripped(narrow.renderTabBar())
	for _, want := range []string{"3 Memory", "4 Network", "5 Storage"} {
		if !strings.Contains(narrowTabs, want) {
			t.Errorf("compact tab bar missing %q", want)
		}
	}
	if lipgloss.Width(narrow.renderTabBar()) > narrow.width {
		t.Fatal("compact tab bar overflows")
	}
}

func TestKeyboardTabNavigationAndDirectJump(t *testing.T) {
	m := newTestModel(80, 24)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if got := next.(model).activeTab; got != tabCPU {
		t.Fatalf("Tab selected %d, want CPU", got)
	}

	previous, _ := next.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	if got := previous.(model).activeTab; got != tabOverview {
		t.Fatalf("Shift+Tab selected %d, want Overview", got)
	}

	m.activeTab = tabGPU
	wrapped, _ := m.Update(tea.KeyMsg{Type: tea.KeyRight})
	if got := wrapped.(model).activeTab; got != tabOverview {
		t.Fatalf("right-arrow wrap selected %d, want Overview", got)
	}

	for key := '1'; key <= '7'; key++ {
		jumped, _ := m.Update(runeKey(key))
		if got, want := jumped.(model).activeTab, tabID(key-'1'); got != want {
			t.Errorf("%c selected %d, want %d", key, got, want)
		}
	}
}

func TestSelectingViewResetsScroll(t *testing.T) {
	m := newTestModel(80, 24)
	m.scroll = 9
	m.selectTab(tabMemory)
	if m.activeTab != tabMemory || m.scroll != 0 {
		t.Fatalf("selectTab left tab=%d scroll=%d", m.activeTab, m.scroll)
	}
}

func TestLongViewsScrollAndClamp(t *testing.T) {
	m := newTestModel(80, 16)
	m.activeTab = tabDisk
	for index := 0; index < 20; index++ {
		m.sample.Disks.FileSystems = append(m.sample.Disks.FileSystems,
			collector.FileSystemInfo{
				MountPoint: "/srv/volume-" + strconv.Itoa(index),
				Total:      100e9,
				Used:       50e9,
				UsedPct:    50,
			})
	}
	if m.maxScroll() <= 0 {
		t.Fatal("long storage view unexpectedly fits without scrolling")
	}

	down, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if got := down.(model).scroll; got != 1 {
		t.Fatalf("down scroll = %d, want 1", got)
	}

	bottom, _ := down.Update(runeKey('G'))
	if got, want := bottom.(model).scroll, bottom.(model).maxScroll(); got != want {
		t.Fatalf("bottom scroll = %d, want %d", got, want)
	}

	top, _ := bottom.Update(runeKey('g'))
	if got := top.(model).scroll; got != 0 {
		t.Fatalf("top scroll = %d, want 0", got)
	}

	m.scroll = 10_000
	m.width = 120
	m.height = 40
	resized, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if got, maxScroll := resized.(model).scroll, resized.(model).maxScroll(); got > maxScroll {
		t.Fatalf("resize left scroll %d beyond max %d", got, maxScroll)
	}
}

func TestFooterReportsScrollPosition(t *testing.T) {
	m := newTestModel(80, 12)
	m.activeTab = tabCPU
	if m.maxScroll() == 0 {
		t.Fatal("test needs overflowing content")
	}
	if footer := stripped(m.renderFooter()); !strings.Contains(footer, "1/") {
		t.Fatalf("footer lacks scroll position: %q", footer)
	}
}

func TestPauseHelpAndQuitControls(t *testing.T) {
	m := newTestModel(80, 24)
	paused, command := m.Update(runeKey(' '))
	if command != nil {
		t.Fatal("pausing without a collector should not start a command")
	}
	pm := paused.(model)
	if !pm.paused || !strings.Contains(stripped(pm.renderHeader()), "PAUSED") {
		t.Fatal("space did not expose paused state")
	}

	help, _ := pm.Update(runeKey('?'))
	hm := help.(model)
	if !hm.showHelp || !strings.Contains(stripped(hm.View()), "keyboard") {
		t.Fatal("? did not open keyboard help")
	}

	closed, _ := hm.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if closed.(model).showHelp {
		t.Fatal("escape did not close help")
	}

	hm.showHelp = true
	_, quit := hm.Update(runeKey('q'))
	if quit == nil {
		t.Fatal("q from help should quit")
	}
}

func TestHelpRemainsCompleteInSmallTerminals(t *testing.T) {
	for _, size := range [][2]int{{32, 8}, {50, 18}, {80, 24}} {
		m := newTestModel(size[0], size[1])
		m.showHelp = true
		assertFrameFits(t, m)
		view := stripped(m.View())
		for _, want := range []string{"KULA", "q"} {
			if !strings.Contains(view, want) {
				t.Errorf("%dx%d help missing %q", size[0], size[1], want)
			}
		}
	}
}

func TestHeaderShowsPausedAgeAndStaleSamples(t *testing.T) {
	m := newTestModel(80, 24)
	m.now = m.lastUpdated.Add(5 * time.Second)
	if header := stripped(m.renderHeader()); !strings.Contains(header, "STALE 5s") {
		t.Fatalf("stale header = %q", header)
	}

	m.paused = true
	if header := stripped(m.renderHeader()); !strings.Contains(header, "PAUSED 5s") {
		t.Fatalf("paused header = %q", header)
	}
}

func TestSampleMessageUpdatesStateAndHistory(t *testing.T) {
	m := newTestModel(80, 24)
	m.sample = nil
	m.collecting = true
	before := m.histCPU.len
	finished := time.Date(2026, 7, 30, 15, 0, 0, 0, time.UTC)

	next, command := m.Update(sampleMsg{
		sample:      newTestSample(),
		finishedAt:  finished,
		collectTime: 25 * time.Millisecond,
	})
	if command != nil {
		t.Fatal("sample completion should not create a second timer")
	}
	got := next.(model)
	if got.collecting || got.sample == nil || got.lastUpdated != finished ||
		got.collectTime != 25*time.Millisecond {
		t.Fatalf("sample state not applied: %+v", got)
	}
	if got.histCPU.len != before+1 {
		t.Fatalf("history length = %d, want %d", got.histCPU.len, before+1)
	}
}

func TestTickKeepsTimerAliveWhilePaused(t *testing.T) {
	m := newTestModel(80, 24)
	m.paused = true
	now := time.Date(2026, 7, 30, 15, 1, 0, 0, time.UTC)
	next, command := m.Update(tickMsg(now))
	if command == nil {
		t.Fatal("tick should schedule the next timer")
	}
	if got := next.(model).now; got != now {
		t.Fatalf("clock = %v, want %v", got, now)
	}
}

func TestLoadingState(t *testing.T) {
	m := newTestModel(80, 24)
	m.sample = nil
	m.collecting = true
	view := stripped(m.View())
	if !strings.Contains(view, "Collecting data") ||
		!strings.Contains(view, "STARTING") {
		t.Fatalf("loading frame lacks feedback: %q", view)
	}
	assertFrameFits(t, m)
}

func TestSystemInfoCanBeHidden(t *testing.T) {
	m := newTestModel(80, 24)
	m.showSystemInfo = false
	view := stripped(m.View())
	for _, hidden := range []string{"testhost", "Test Linux", "6.1.0-test", "amd64"} {
		if strings.Contains(view, hidden) {
			t.Errorf("hidden system info leaked %q", hidden)
		}
	}
}

func TestEveryDetailedViewContainsOperationalData(t *testing.T) {
	m := newTestModel(100, 40)
	tests := []struct {
		tab   tabID
		wants []string
	}{
		{tabCPU, []string{"TIME SHARE", "I/O wait", "LOAD AVERAGE", "THERMALS"}},
		{tabMemory, []string{"MEMORY", "Available", "Cached", "SWAP"}},
		{tabNetwork, []string{"NETWORK", "eth0", "TCP / SOCKETS", "Retrans"}},
		{tabDisk, []string{"STORAGE I/O", "nvme0n1", "FILESYSTEMS", "/home"}},
		{tabProcesses, []string{"PROCESSES", "Running", "Zombie", "KULA PROCESS"}},
		{tabGPU, []string{"NVIDIA RTX 4090", "Core", "VRAM", "Temperature", "Power"}},
	}
	for _, test := range tests {
		m.activeTab = test.tab
		content := stripped(strings.Join(m.contentLines(m.contentWidth()), "\n"))
		for _, want := range test.wants {
			if !strings.Contains(content, want) {
				t.Errorf("%s view missing %q", tabNames[test.tab], want)
			}
		}
	}
}

func TestEmptyHardwareStatesAreExplicit(t *testing.T) {
	m := newTestModel(80, 24)
	m.sample.GPU = nil
	m.activeTab = tabGPU
	if content := stripped(strings.Join(m.contentLines(m.contentWidth()), "\n")); !strings.Contains(content, "No GPUs detected") {
		t.Fatalf("empty GPU state = %q", content)
	}

	m.sample.Network.Interfaces = nil
	m.activeTab = tabNetwork
	if content := stripped(strings.Join(m.contentLines(m.contentWidth()), "\n")); !strings.Contains(content, "No active interfaces") {
		t.Fatalf("empty network state = %q", content)
	}
}

func TestHealthAssessment(t *testing.T) {
	sample := newTestSample()
	level, findings := assessHealth(sample)
	if level != healthOK || len(findings) != 0 {
		t.Fatalf("nominal sample assessed level=%d findings=%v", level, findings)
	}

	sample.Memory.UsedPercent = 82
	sample.Process.Zombie = 1
	level, findings = assessHealth(sample)
	if level != healthWatch || len(findings) < 2 {
		t.Fatalf("watch sample assessed level=%d findings=%v", level, findings)
	}

	sample.Disks.FileSystems[0].UsedPct = 95
	level, findings = assessHealth(sample)
	if level != healthCritical {
		t.Fatalf("critical filesystem assessed level=%d findings=%v", level, findings)
	}

	sample = newTestSample()
	sample.LoadAvg.Load1 = 7
	level, findings = assessHealth(sample)
	if level != healthCritical {
		t.Fatalf("critical load assessed level=%d findings=%v", level, findings)
	}

	sample = newTestSample()
	sample.Disks.Devices[0].Utilization = 85
	level, findings = assessHealth(sample)
	if level != healthWatch {
		t.Fatalf("busy disk assessed level=%d findings=%v", level, findings)
	}
}

func TestSparklineWidthAndInvalidValues(t *testing.T) {
	values := []float64{0, 25, 50, 75, 100, math.NaN(), math.Inf(1)}
	for _, width := range []int{1, 4, 12} {
		line := sparkline(values, width, 0, 100)
		if got := lipgloss.Width(line); got != width {
			t.Errorf("sparkline width = %d, want %d (%q)", got, width, stripped(line))
		}
	}
}

func TestGaugeClampsPercent(t *testing.T) {
	for _, value := range []float64{-10, 0, 50, 110, math.NaN()} {
		line := stripped(renderGauge("CPU", value, "", 60))
		if strings.Contains(line, "NaN") {
			t.Errorf("gauge exposed invalid value: %q", line)
		}
	}
	if line := stripped(renderGauge("CPU", 110, "", 60)); !strings.Contains(line, "100.0%") {
		t.Fatalf("high gauge not clamped: %q", line)
	}
}

func TestFormattingHelpers(t *testing.T) {
	byteTests := map[uint64]string{
		0:                         "0 B",
		1024:                      "1.0 KiB",
		1536:                      "1.5 KiB",
		1024 * 1024:               "1.0 MiB",
		1024 * 1024 * 1024:        "1.0 GiB",
		1024 * 1024 * 1024 * 1024: "1.0 TiB",
	}
	for input, want := range byteTests {
		if got := fmtBytes(input); got != want {
			t.Errorf("fmtBytes(%d) = %q, want %q", input, got, want)
		}
	}

	rateTests := map[float64]string{
		0:      "0 bit/s",
		0.0005: "500 bit/s",
		0.5:    "500 kbit/s",
		1:      "1.0 Mbit/s",
		1200:   "1.20 Gbit/s",
	}
	for input, want := range rateTests {
		if got := fmtBitRate(input); got != want {
			t.Errorf("fmtBitRate(%v) = %q, want %q", input, got, want)
		}
	}
}

func TestDisplayWidthHelpersHandleUnicode(t *testing.T) {
	value := "磁盘-alpha"
	for _, width := range []int{2, 5, 12} {
		right := padRight(value, width)
		left := padLeft(value, width)
		if lipgloss.Width(right) != width || lipgloss.Width(left) != width {
			t.Errorf("unicode padding width %d: left=%d right=%d",
				width, lipgloss.Width(left), lipgloss.Width(right))
		}
	}

	styled := sCrit.Render("critical")
	if got := lipgloss.Width(fitLine(styled, 15)); got != 15 {
		t.Fatalf("styled fit width = %d, want 15", got)
	}
}

func TestTrendWindowUsesConfiguredRefresh(t *testing.T) {
	m := newTestModel(80, 24)
	m.refreshRate = 2 * time.Second
	m.histCPU = newRing()
	m.histCPU.push(1)
	m.histCPU.push(2)
	m.histCPU.push(3)
	if got := m.trendWindow(); got != "last 4s" {
		t.Fatalf("trend window = %q, want last 4s", got)
	}
}

func TestTrendWindowUsesActualSampleSpan(t *testing.T) {
	m := newTestModel(80, 24)
	m.histCPU = newRing()
	m.histTimes = newTimestampRing(defaultHistoryLen)
	start := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	m.histCPU.push(1)
	m.histTimes.push(start)
	m.histCPU.push(2)
	m.histTimes.push(start.Add(9 * time.Second))
	if got := m.trendWindow(); got != "last 9s" {
		t.Fatalf("trend window = %q, want last 9s", got)
	}
}
