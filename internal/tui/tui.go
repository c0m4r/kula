package tui

import (
	"fmt"
	"strings"
	"time"

	"kula-szpiegula/internal/collector"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ── Styles ──────────────────────────────────────────────────────────

var (
	// Title bar
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#06b6d4")).
			Background(lipgloss.Color("#1e293b")).
			Padding(0, 1)

	// Section headers
	sectionStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#f59e0b")).
			PaddingRight(1)

	// Labels
	labelStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#94a3b8"))

	// Values
	valueStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#10b981")).
			Bold(true)

	valueAltStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#06b6d4"))

	// Warnings
	warnStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#ef4444")).
			Bold(true)

	// Dim / secondary text
	dimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#475569"))

	// Bar styles
	barLow = lipgloss.NewStyle().Foreground(lipgloss.Color("#10b981"))
	barMed = lipgloss.NewStyle().Foreground(lipgloss.Color("#f59e0b"))
	barHi  = lipgloss.NewStyle().Foreground(lipgloss.Color("#ef4444"))
	barBg  = lipgloss.NewStyle().Foreground(lipgloss.Color("#1e293b"))

	// Help line
	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#64748b")).
			Italic(true)
)

// ── Model ───────────────────────────────────────────────────────────

type tickMsg time.Time

type Model struct {
	collector      *collector.Collector
	sample         *collector.Sample
	width          int
	height         int
	interval       time.Duration
	os             string
	kernel         string
	arch           string
	showSystemInfo bool
}

func NewModel(c *collector.Collector, interval time.Duration, osName, kernel, arch string, showSystemInfo bool) Model {
	return Model{
		collector:      c,
		interval:       interval,
		width:          120,
		height:         40,
		os:             osName,
		kernel:         kernel,
		arch:           arch,
		showSystemInfo: showSystemInfo,
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		tickCmd(m.interval),
		tea.EnterAltScreen,
	)
}

func tickCmd(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case tickMsg:
		m.sample = m.collector.Collect()
		return m, tickCmd(m.interval)
	}
	return m, nil
}

func (m Model) View() string {
	if m.sample == nil {
		return titleStyle.Render(" KULA-SZPIEGULA ") + "\n\n  Collecting initial data..."
	}

	s := m.sample
	compact := m.height < 30

	// Collect lines — we'll truncate at the end to fit terminal
	var lines []string

	// ── Title bar ────────────────────────────────────────────
	sysInfo := ""
	if m.showSystemInfo {
		sysInfo = fmt.Sprintf(" │ %s │ %s │ %s", m.os, m.kernel, m.arch)
	}
	header := fmt.Sprintf(" 🔮 KULA-SZPIEGULA │ %s%s │ %s │ %s ",
		s.System.Hostname, sysInfo, s.System.UptimeHuman, s.Timestamp.Format("15:04:05"))
	lines = append(lines, titleStyle.Width(m.width).Render(header))
	lines = append(lines, "")

	barW := clamp(m.width-28, 15, 70)

	// ── CPU ──────────────────────────────────────────────────
	cpuLabel := sectionStyle.Render("CPU")
	cpuBar := renderBar(s.CPU.Total.Usage, 100, barW)
	cpuVal := colorByPct(s.CPU.Total.Usage).Render(fmt.Sprintf("%5.1f%%", s.CPU.Total.Usage))
	cpuDetail := dimStyle.Render(fmt.Sprintf(" usr:%.0f%% sys:%.0f%% io:%.0f%% irq:%.0f%% stl:%.0f%%",
		s.CPU.Total.User, s.CPU.Total.System, s.CPU.Total.IOWait,
		s.CPU.Total.IRQ+s.CPU.Total.SoftIRQ, s.CPU.Total.Steal))

	if compact {
		lines = append(lines, fmt.Sprintf(" %s %s %s%s", cpuLabel, cpuBar, cpuVal, cpuDetail))
	} else {
		lines = append(lines, fmt.Sprintf(" %s  %s %s", cpuLabel, cpuBar, cpuVal))
		lines = append(lines, "      "+cpuDetail)

	}
	lines = append(lines, "")

	// ── Load Average ─────────────────────────────────────────
	numCores := maxInt(s.CPU.NumCores, 1)
	la1Style := valueStyle
	if s.LoadAvg.Load1 > float64(numCores) {
		la1Style = warnStyle
	}
	lines = append(lines, fmt.Sprintf(" %s %s  %s  %s   %s",
		sectionStyle.Render("LOAD"),
		la1Style.Render(fmt.Sprintf("%.2f", s.LoadAvg.Load1)),
		valueAltStyle.Render(fmt.Sprintf("%.2f", s.LoadAvg.Load5)),
		dimStyle.Render(fmt.Sprintf("%.2f", s.LoadAvg.Load15)),
		dimStyle.Render(fmt.Sprintf("%d tasks, %d running", s.LoadAvg.Total, s.LoadAvg.Running))))
	lines = append(lines, "")

	// ── Memory ───────────────────────────────────────────────
	if compact {
		lines = append(lines, fmt.Sprintf(" %s  %s  %s / %s  %s",
			sectionStyle.Render("MEM"),
			colorByPct(s.Memory.UsedPercent).Render(fmt.Sprintf("%.1f%%", s.Memory.UsedPercent)),
			valueStyle.Render(fmtB(s.Memory.Used)),
			dimStyle.Render(fmtB(s.Memory.Total)),
			dimStyle.Render(fmt.Sprintf("buf:%s cch:%s shm:%s", fmtB(s.Memory.Buffers), fmtB(s.Memory.Cached), fmtB(s.Memory.Shmem)))))
		if s.Swap.Total > 0 {
			lines = append(lines, fmt.Sprintf("        %s  %s / %s",
				sectionStyle.Render("SWAP"),
				colorByPct(s.Swap.UsedPercent).Render(fmt.Sprintf("%.1f%%", s.Swap.UsedPercent)),
				dimStyle.Render(fmtB(s.Swap.Total))))
		}
	} else {
		lines = append(lines, fmt.Sprintf(" %s %s %s  %s / %s   buf:%s cch:%s shm:%s",
			sectionStyle.Render("MEM "),
			renderBar(s.Memory.UsedPercent, 100, barW),
			colorByPct(s.Memory.UsedPercent).Render(fmt.Sprintf("%5.1f%%", s.Memory.UsedPercent)),
			valueStyle.Render(fmtB(s.Memory.Used)),
			dimStyle.Render(fmtB(s.Memory.Total)),
			dimStyle.Render(fmtB(s.Memory.Buffers)),
			dimStyle.Render(fmtB(s.Memory.Cached)),
			dimStyle.Render(fmtB(s.Memory.Shmem))))
		if s.Swap.Total > 0 {
			lines = append(lines, fmt.Sprintf(" %s %s %s  %s / %s",
				sectionStyle.Render("SWAP"),
				renderBar(s.Swap.UsedPercent, 100, barW),
				colorByPct(s.Swap.UsedPercent).Render(fmt.Sprintf("%5.1f%%", s.Swap.UsedPercent)),
				valueStyle.Render(fmtB(s.Swap.Used)),
				dimStyle.Render(fmtB(s.Swap.Total))))
		}
	}
	lines = append(lines, "")

	// ── Network (only in non-compact) ────────────────────────
	if !compact {
		lines = append(lines, " "+sectionStyle.Render("NET"))
		for _, iface := range s.Network.Interfaces {
			if iface.Name == "lo" {
				continue
			}
			lines = append(lines, fmt.Sprintf("   %s  ↓ %s  ↑ %s   pkt:%s/%s  err:%d/%d  drop:%d/%d",
				labelStyle.Render(fmt.Sprintf("%-14s", iface.Name)),
				valueAltStyle.Render(fmt.Sprintf("%8.2f Mbps", iface.RxMbps)),
				valueStyle.Render(fmt.Sprintf("%8.2f Mbps", iface.TxMbps)),
				dimStyle.Render(fmt.Sprintf("%d", iface.RxPkts)),
				dimStyle.Render(fmt.Sprintf("%d", iface.TxPkts)),
				iface.RxErrs, iface.TxErrs,
				iface.RxDrop, iface.TxDrop))
		}
		lines = append(lines, fmt.Sprintf("   %s tcp:%d  udp:%d  tw:%d  estab:%d  err:%.1f/s  rst:%.1f/s",
			dimStyle.Render("sockets"),
			s.Network.Sockets.TCPInUse,
			s.Network.Sockets.UDPInUse,
			s.Network.Sockets.TCPTw,
			s.Network.TCP.CurrEstab,
			s.Network.TCP.InErrs,
			s.Network.TCP.OutRsts))
		lines = append(lines, "")
	}

	// ── Disks (only in non-compact) ──────────────────────────
	if !compact {
		lines = append(lines, " "+sectionStyle.Render("DISK"))
		for _, dev := range s.Disks.Devices {
			lines = append(lines, fmt.Sprintf("   %s  r: %s  w: %s  rIOPS:%.0f  wIOPS:%.0f",
				labelStyle.Render(fmt.Sprintf("%-10s", dev.Name)),
				valueAltStyle.Render(fmt.Sprintf("%8.1f KB/s", dev.ReadBytesPS/1024)),
				valueStyle.Render(fmt.Sprintf("%8.1f KB/s", dev.WriteBytesPS/1024)),
				dev.ReadsPerSec, dev.WritesPerSec))
		}
		for _, fs := range s.Disks.FileSystems {
			fsBarW := clamp(barW-10, 10, 30)
			lines = append(lines, fmt.Sprintf("   %s %s %s  %s / %s",
				labelStyle.Render(fmt.Sprintf("%-22s", fs.MountPoint)),
				renderBar(fs.UsedPct, 100, fsBarW),
				colorByPct(fs.UsedPct).Render(fmt.Sprintf("%5.1f%%", fs.UsedPct)),
				dimStyle.Render(fmtB(fs.Used)),
				dimStyle.Render(fmtB(fs.Total))))
		}
		lines = append(lines, "")
	}

	// ── Tasks ────────────────────────────────────────────────
	zombieStr := dimStyle.Render(fmt.Sprintf("%d", s.Process.Zombie))
	if s.Process.Zombie > 0 {
		zombieStr = warnStyle.Render(fmt.Sprintf("%d", s.Process.Zombie))
	}
	lines = append(lines, fmt.Sprintf(" %s %s total  %s run  %s sleep  %s blk  %s zombie  %s threads",
		sectionStyle.Render("TASKS"),
		valueStyle.Render(fmt.Sprintf("%d", s.Process.Total)),
		valueStyle.Render(fmt.Sprintf("%d", s.Process.Running)),
		dimStyle.Render(fmt.Sprintf("%d", s.Process.Sleeping)),
		dimStyle.Render(fmt.Sprintf("%d", s.Process.Blocked)),
		zombieStr,
		dimStyle.Render(fmt.Sprintf("%d", s.Process.Threads))))
	lines = append(lines, "")

	// ── System info ──────────────────────────────────────────
	syncIcon := warnStyle.Render("✗")
	if s.System.ClockSync {
		syncIcon = valueStyle.Render("✓")
	}
	lines = append(lines, fmt.Sprintf(" %s entropy:%s  clock:%s(%s)  users:%s  self: cpu=%s mem=%s fds=%d",
		sectionStyle.Render("SYS"),
		valueAltStyle.Render(fmt.Sprintf("%d", s.System.Entropy)),
		syncIcon,
		dimStyle.Render(s.System.ClockSource),
		valueAltStyle.Render(fmt.Sprintf("%d", s.System.UserCount)),
		valueStyle.Render(fmt.Sprintf("%.1f%%", s.Self.CPUPercent)),
		valueStyle.Render(fmtB(s.Self.MemRSS)),
		s.Self.FDs))

	// ── Footer / help ────────────────────────────────────────
	lines = append(lines, "")
	lines = append(lines, " "+helpStyle.Render("'q' quit"))

	// ── Truncate to fit terminal height ──────────────────────
	// Priority: show lines from the TOP down (CPU, load, memory first)
	maxLines := m.height
	if len(lines) > maxLines && maxLines > 0 {
		lines = lines[:maxLines]
	}

	return strings.Join(lines, "\n")
}

// ── Helpers ─────────────────────────────────────────────────────────

func renderBar(val, max float64, width int) string {
	if width <= 0 {
		return ""
	}
	pct := val / max
	if pct < 0 {
		pct = 0
	}
	if pct > 1 {
		pct = 1
	}

	filled := int(pct * float64(width))
	empty := width - filled

	style := barLow
	if pct > 0.70 {
		style = barMed
	}
	if pct > 0.90 {
		style = barHi
	}

	return dimStyle.Render("[") +
		style.Render(strings.Repeat("█", filled)) +
		barBg.Render(strings.Repeat("░", empty)) +
		dimStyle.Render("]")
}

func colorByPct(pct float64) lipgloss.Style {
	if pct > 90 {
		return warnStyle
	}
	if pct > 70 {
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#f59e0b")).Bold(true)
	}
	return valueStyle
}

func fmtB(b uint64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
		TB = GB * 1024
	)
	switch {
	case b >= TB:
		return fmt.Sprintf("%.1f TB", float64(b)/float64(TB))
	case b >= GB:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(GB))
	case b >= MB:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(MB))
	case b >= KB:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(KB))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func Run(c *collector.Collector, interval time.Duration, osName, kernel, arch string, showSystemInfo bool) error {
	m := NewModel(c, interval, osName, kernel, arch, showSystemInfo)
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

func RunHeadless(c *collector.Collector, interval time.Duration, osName, kernel, arch string, showSystemInfo bool) error {
	return Run(c, interval, osName, kernel, arch, showSystemInfo)
}
