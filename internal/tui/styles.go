package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

var (
	colorInk       = lipgloss.Color("#E8ECF3")
	colorMuted     = lipgloss.Color("#7F8A9D")
	colorPanel     = lipgloss.Color("#252B3A")
	colorAccent    = lipgloss.Color("#70D7C7")
	colorHighlight = lipgloss.Color("#312E50")
	colorWarning   = lipgloss.Color("#F5C26B")
	colorDanger    = lipgloss.Color("#FF7185")
	colorSuccess   = lipgloss.Color("#7DE2A7")

	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(colorInk)
	logoStyle  = lipgloss.NewStyle().Bold(true).Foreground(colorAccent)
	mutedStyle = lipgloss.NewStyle().Foreground(colorMuted)
	panelStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorPanel).Padding(0, 1)
	keyStyle   = lipgloss.NewStyle().Bold(true).Foreground(colorAccent)
)

func statusStyle(status string) lipgloss.Style {
	style := lipgloss.NewStyle().Bold(true)
	switch strings.ToUpper(status) {
	case "OK", "HEALTHY", "COMPLETED", "SUCCEEDED", "GRANTED", "ALLOW":
		return style.Foreground(colorSuccess)
	case "PENDING", "RUNNING", "REQUIRE_APPROVAL", "SHADOW":
		return style.Foreground(colorWarning)
	case "FAILED", "DEAD", "DENIED", "EXPIRED", "OFFLINE", "DEGRADED":
		return style.Foreground(colorDanger)
	default:
		return style.Foreground(colorMuted)
	}
}
