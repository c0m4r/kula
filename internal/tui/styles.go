package tui

import "github.com/charmbracelet/lipgloss"

// The TUI deliberately avoids a painted background. Adaptive foreground
// colours preserve the operator's terminal theme and remain legible in both
// light and dark environments.
var (
	clrAccent = lipgloss.AdaptiveColor{Light: "#005F87", Dark: "#7DD3FC"}
	clrText   = lipgloss.AdaptiveColor{Light: "#1F2937", Dark: "#E5E7EB"}
	clrMuted  = lipgloss.AdaptiveColor{Light: "#64748B", Dark: "#94A3B8"}
	clrFaint  = lipgloss.AdaptiveColor{Light: "#CBD5E1", Dark: "#475569"}
	clrGood   = lipgloss.AdaptiveColor{Light: "#167545", Dark: "#4ADE80"}
	clrWarn   = lipgloss.AdaptiveColor{Light: "#9A6700", Dark: "#FBBF24"}
	clrCrit   = lipgloss.AdaptiveColor{Light: "#B42318", Dark: "#FB7185"}

	sBrand = lipgloss.NewStyle().
		Foreground(clrAccent).
		Bold(true)
	sStrong = lipgloss.NewStyle().
		Foreground(clrText).
		Bold(true)
	sText    = lipgloss.NewStyle().Foreground(clrText)
	sMuted   = lipgloss.NewStyle().Foreground(clrMuted)
	sFaint   = lipgloss.NewStyle().Foreground(clrFaint)
	sAccent  = lipgloss.NewStyle().Foreground(clrAccent)
	sSection = lipgloss.NewStyle().
			Foreground(clrAccent).
			Bold(true)
	sRule = lipgloss.NewStyle().Foreground(clrFaint)

	sTabActive = lipgloss.NewStyle().
			Foreground(clrAccent).
			Bold(true).
			Underline(true)
	sTabInactive = lipgloss.NewStyle().Foreground(clrMuted)

	sKey = lipgloss.NewStyle().
		Foreground(clrAccent).
		Bold(true)

	sGood = lipgloss.NewStyle().
		Foreground(clrGood).
		Bold(true)
	sWarn = lipgloss.NewStyle().
		Foreground(clrWarn).
		Bold(true)
	sCrit = lipgloss.NewStyle().
		Foreground(clrCrit).
		Bold(true)

	sBarGood = lipgloss.NewStyle().Foreground(clrGood)
	sBarWarn = lipgloss.NewStyle().Foreground(clrWarn)
	sBarCrit = lipgloss.NewStyle().Foreground(clrCrit)
	sBarRest = lipgloss.NewStyle().Foreground(clrFaint)

	sTableHead = lipgloss.NewStyle().
			Foreground(clrMuted).
			Bold(true)
	sTableCell = lipgloss.NewStyle().Foreground(clrText)
	sTableDim  = lipgloss.NewStyle().Foreground(clrMuted)

	sHelpPanel = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(clrFaint).
			Padding(1, 2)
)
