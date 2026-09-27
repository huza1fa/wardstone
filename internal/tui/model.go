package tui

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type view int

const (
	overviewView view = iota
	investigationsView
	approvalsView
)

type Options struct {
	Actor           string
	RefreshInterval time.Duration
	RequestTimeout  time.Duration
}

type Model struct {
	api             API
	actor           string
	refreshInterval time.Duration
	requestTimeout  time.Duration
	spinner         spinner.Model
	width           int
	height          int
	view            view
	cursors         [3]int
	snapshot        Snapshot
	loading         bool
	loaded          bool
	confirm         string
	notice          string
	lastError       string
}

type snapshotMsg Snapshot

type refreshMsg time.Time

type decisionMsg struct {
	decision string
	err      error
}

func NewModel(api API, options Options) Model {
	if options.Actor == "" {
		options.Actor = "terminal-admin"
	}
	if options.RefreshInterval <= 0 {
		options.RefreshInterval = 15 * time.Second
	}
	if options.RequestTimeout <= 0 {
		options.RequestTimeout = 8 * time.Second
	}
	indicator := spinner.New()
	indicator.Spinner = spinner.Dot
	indicator.Style = lipgloss.NewStyle().Foreground(colorAccent)
	return Model{
		api:             api,
		actor:           options.Actor,
		refreshInterval: options.RefreshInterval,
		requestTimeout:  options.RequestTimeout,
		spinner:         indicator,
		width:           100,
		height:          30,
		loading:         true,
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, m.load(), m.scheduleRefresh())
}

func (m Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.width = message.Width
		m.height = message.Height
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(message)
	case spinner.TickMsg:
		var command tea.Cmd
		m.spinner, command = m.spinner.Update(message)
		return m, command
	case snapshotMsg:
		m.snapshot = Snapshot(message)
		m.loading = false
		m.loaded = true
		m.lastError = ""
		if len(m.snapshot.Problems) > 0 {
			m.lastError = strings.Join(m.snapshot.Problems, " · ")
		}
		m.clampCursor()
		return m, nil
	case refreshMsg:
		commands := []tea.Cmd{m.scheduleRefresh()}
		if !m.loading && m.confirm == "" {
			m.loading = true
			commands = append(commands, m.load())
		}
		return m, tea.Batch(commands...)
	case decisionMsg:
		m.loading = true
		m.confirm = ""
		if message.err != nil {
			m.loading = false
			m.lastError = message.err.Error()
			m.notice = "Approval was not changed"
			return m, nil
		}
		m.notice = titleCase(message.decision) + " approval"
		return m, m.load()
	default:
		return m, nil
	}
}

func (m Model) handleKey(message tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := message.String()
	if m.confirm != "" {
		switch key {
		case "y", "Y":
			if approval, ok := m.selectedApproval(); ok {
				m.loading = true
				return m, m.decide(string(approval.ID), m.confirm)
			}
			m.confirm = ""
			return m, nil
		case "n", "N", "esc":
			m.confirm = ""
			m.notice = "Decision cancelled"
			return m, nil
		case "ctrl+c", "q":
			return m, tea.Quit
		default:
			return m, nil
		}
	}

	switch key {
	case "ctrl+c", "q":
		return m, tea.Quit
	case "tab", "right", "l":
		m.view = (m.view + 1) % 3
	case "shift+tab", "left", "h":
		m.view = (m.view + 2) % 3
	case "1":
		m.view = overviewView
	case "2":
		m.view = investigationsView
	case "3":
		m.view = approvalsView
	case "up", "k":
		if m.cursors[m.view] > 0 {
			m.cursors[m.view]--
		}
	case "down", "j":
		m.cursors[m.view]++
		m.clampCursor()
	case "r":
		if !m.loading {
			m.loading = true
			m.notice = ""
			return m, m.load()
		}
	case "a":
		if m.view == approvalsView {
			if _, ok := m.selectedApproval(); ok {
				m.confirm = "GRANTED"
			}
		}
	case "d":
		if m.view == approvalsView {
			if _, ok := m.selectedApproval(); ok {
				m.confirm = "DENIED"
			}
		}
	}
	return m, nil
}

func (m Model) View() string {
	width := max(40, m.width)
	bodyHeight := max(8, m.height-6)
	header := m.renderHeader(width)
	tabs := m.renderTabs(width)
	var body string
	switch m.view {
	case investigationsView:
		body = m.renderInvestigations(width, bodyHeight)
	case approvalsView:
		body = m.renderApprovals(width, bodyHeight)
	default:
		body = m.renderOverview(width, bodyHeight)
	}
	footer := m.renderFooter(width)
	return lipgloss.JoinVertical(lipgloss.Left, header, tabs, body, footer)
}

func (m Model) renderHeader(width int) string {
	state := m.snapshot.Overview.Status
	if state == "" {
		state = "offline"
	}
	if len(m.snapshot.Problems) > 0 && state != "offline" {
		state = "degraded"
	}
	left := logoStyle.Render("◆ WARDSTONE") + "  " + mutedStyle.Render("admin console")
	right := statusStyle(state).Render("● " + strings.ToUpper(state))
	if m.loading {
		right = m.spinner.View() + " syncing"
	}
	gap := max(1, width-lipgloss.Width(left)-lipgloss.Width(right)-2)
	return " " + left + strings.Repeat(" ", gap) + right
}

func (m Model) renderTabs(width int) string {
	labels := []string{"1  Overview", "2  Investigations", "3  Approvals"}
	parts := make([]string, len(labels))
	for index, label := range labels {
		style := lipgloss.NewStyle().Padding(0, 1).Foreground(colorMuted)
		if view(index) == m.view {
			style = style.Bold(true).Foreground(colorAccent).Background(colorHighlight)
		}
		parts[index] = style.Render(label)
	}
	return lipgloss.NewStyle().Width(width).BorderBottom(true).BorderStyle(lipgloss.NormalBorder()).BorderForeground(colorPanel).Render(strings.Join(parts, " "))
}

func (m Model) renderOverview(width, _ int) string {
	if !m.loaded {
		return emptyPanel(width, "Connecting to Wardstone…", "The console will update when the operator API responds.")
	}
	overview := m.snapshot.Overview
	mode := overview.Mode
	if mode == "" {
		mode = "unknown"
	}
	cards := []string{
		metricCard("MODE", strings.ToUpper(string(mode)), "policy boundary", max(18, (width-8)/4)),
		metricCard("ACTIVE", fmt.Sprint(overview.Investigations.Running), "investigations", max(18, (width-8)/4)),
		metricCard("PENDING", fmt.Sprint(overview.Approvals.Pending), "approvals", max(18, (width-8)/4)),
		metricCard("FAILED", fmt.Sprint(overview.Investigations.Failed), "investigations", max(18, (width-8)/4)),
	}
	var metrics string
	if width < 84 {
		metrics = lipgloss.JoinVertical(lipgloss.Left,
			lipgloss.JoinHorizontal(lipgloss.Top, cards[0], " ", cards[1]),
			lipgloss.JoinHorizontal(lipgloss.Top, cards[2], " ", cards[3]),
		)
	} else {
		metrics = lipgloss.JoinHorizontal(lipgloss.Top, cards[0], " ", cards[1], " ", cards[2], " ", cards[3])
	}

	queue := []string{titleStyle.Render("Work queue")}
	queue = append(queue,
		fmt.Sprintf("%-12s %d", statusStyle("PENDING").Render("PENDING"), overview.Jobs.Pending),
		fmt.Sprintf("%-12s %d", statusStyle("RUNNING").Render("RUNNING"), overview.Jobs.Running),
		fmt.Sprintf("%-12s %d", statusStyle("COMPLETED").Render("COMPLETED"), overview.Jobs.Completed),
		fmt.Sprintf("%-12s %d", statusStyle("DEAD").Render("DEAD"), overview.Jobs.Dead),
	)
	recent := []string{titleStyle.Render("Recent investigations")}
	if len(m.snapshot.Investigations) == 0 {
		recent = append(recent, mutedStyle.Render("No investigations yet"))
	} else {
		limit := min(5, len(m.snapshot.Investigations))
		for _, item := range m.snapshot.Investigations[:limit] {
			recent = append(recent, fmt.Sprintf("%s  %-12s  %s", statusStyle(string(item.Status)).Render("●"), shortID(string(item.ID)), truncate(item.Ticket.Summary, max(18, width-50))))
		}
	}
	columnWidth := max(28, (width-5)/2)
	queuePanel := panelStyle.Width(columnWidth).Render(strings.Join(queue, "\n"))
	recentPanel := panelStyle.Width(columnWidth).Render(strings.Join(recent, "\n"))
	below := lipgloss.JoinHorizontal(lipgloss.Top, queuePanel, " ", recentPanel)
	if width < 70 {
		panelWidth := max(26, width-6)
		below = lipgloss.JoinVertical(lipgloss.Left,
			panelStyle.Width(panelWidth).Render(strings.Join(queue, "\n")),
			panelStyle.Width(panelWidth).Render(strings.Join(recent, "\n")),
		)
	}
	return lipgloss.NewStyle().Padding(1).Render(lipgloss.JoinVertical(lipgloss.Left, metrics, "", below))
}

func (m Model) renderInvestigations(width, height int) string {
	items := m.snapshot.Investigations
	if len(items) == 0 {
		return emptyPanel(width, "No investigations", "New Jira work will appear here after it is received.")
	}
	maxRows := max(3, height-9)
	start, end := visibleRange(len(items), maxRows, m.cursors[investigationsView])
	lines := []string{mutedStyle.Render(fmt.Sprintf("   %-11s %-12s %-16s %s", "STATUS", "TICKET", "CREATED", "SUMMARY"))}
	for index := start; index < end; index++ {
		item := items[index]
		prefix := "  "
		style := lipgloss.NewStyle()
		if index == m.cursors[investigationsView] {
			prefix = "› "
			style = style.Background(colorHighlight).Foreground(colorInk).Bold(true)
		}
		line := fmt.Sprintf("%s%-11s %-12s %-16s %s", prefix, strings.ToUpper(string(item.Status)), ticketLabel(item), formatTime(item.CreatedAt), truncate(item.Ticket.Summary, max(12, width-52)))
		lines = append(lines, style.Width(max(1, width-4)).Render(line))
	}
	selected := items[m.cursors[investigationsView]]
	detail := selected.Diagnosis
	if selected.Failure != "" {
		detail = selected.Failure
	}
	if detail == "" {
		detail = "No diagnosis has been recorded yet."
	}
	detail = fmt.Sprintf("%s  %s\n%s", titleStyle.Render(shortID(string(selected.ID))), statusStyle(string(selected.Status)).Render(string(selected.Status)), truncate(detail, max(24, width-8)))
	return lipgloss.NewStyle().Padding(1).Render(strings.Join(lines, "\n") + "\n\n" + panelStyle.Width(max(20, width-6)).Render(detail))
}

func (m Model) renderApprovals(width, height int) string {
	items := m.snapshot.Approvals
	if len(items) == 0 {
		return emptyPanel(width, "Approval queue is clear", "Actions that require an operator decision will appear here.")
	}
	maxRows := max(3, height-11)
	start, end := visibleRange(len(items), maxRows, m.cursors[approvalsView])
	lines := []string{mutedStyle.Render(fmt.Sprintf("   %-10s %-24s %-12s %s", "STATUS", "CAPABILITY", "EXPIRES", "REASON"))}
	for index := start; index < end; index++ {
		item := items[index]
		prefix := "  "
		style := lipgloss.NewStyle()
		if index == m.cursors[approvalsView] {
			prefix = "› "
			style = style.Background(colorHighlight).Foreground(colorInk).Bold(true)
		}
		line := fmt.Sprintf("%s%-10s %-24s %-12s %s", prefix, strings.ToUpper(string(item.Status)), truncate(string(item.Capability), 24), relativeExpiry(item.ExpiresAt), truncate(item.Reason, max(12, width-57)))
		lines = append(lines, style.Width(max(1, width-4)).Render(line))
	}
	selected := items[m.cursors[approvalsView]]
	detail := fmt.Sprintf("%s\n%s\n%s",
		titleStyle.Render(string(selected.Capability)),
		truncate(selected.Reason, max(24, width-8)),
		mutedStyle.Render("action "+shortID(string(selected.ActionID))+"  •  investigation "+shortID(string(selected.InvestigationID))),
	)
	if m.confirm != "" {
		verb := "approve"
		if m.confirm == "DENIED" {
			verb = "deny"
		}
		detail += "\n\n" + statusStyle(m.confirm).Render("Confirm "+verb+"?  y yes  •  n cancel")
	} else {
		detail += "\n\n" + keyStyle.Render("a") + " approve   " + keyStyle.Render("d") + " deny"
	}
	return lipgloss.NewStyle().Padding(1).Render(strings.Join(lines, "\n") + "\n\n" + panelStyle.Width(max(20, width-6)).Render(detail))
}

func (m Model) renderFooter(width int) string {
	left := keyStyle.Render("tab") + mutedStyle.Render(" view  ") + keyStyle.Render("j/k") + mutedStyle.Render(" move  ") + keyStyle.Render("r") + mutedStyle.Render(" refresh  ") + keyStyle.Render("q") + mutedStyle.Render(" quit")
	right := ""
	if m.notice != "" {
		right = m.notice
	} else if m.lastError != "" {
		right = statusStyle("degraded").Render(truncate(m.lastError, max(12, width/2)))
	} else if !m.snapshot.LoadedAt.IsZero() {
		right = mutedStyle.Render("updated " + m.snapshot.LoadedAt.Local().Format("15:04:05"))
	}
	gap := max(1, width-lipgloss.Width(left)-lipgloss.Width(right)-2)
	return lipgloss.NewStyle().Width(width).BorderTop(true).BorderStyle(lipgloss.NormalBorder()).BorderForeground(colorPanel).Render(" " + left + strings.Repeat(" ", gap) + right)
}

func (m Model) load() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), m.requestTimeout)
		defer cancel()
		return snapshotMsg(m.api.Snapshot(ctx))
	}
}

func (m Model) decide(id, decision string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), m.requestTimeout)
		defer cancel()
		return decisionMsg{decision: strings.ToLower(decision), err: m.api.DecideApproval(ctx, id, decision, m.actor)}
	}
}

func (m Model) scheduleRefresh() tea.Cmd {
	return tea.Tick(m.refreshInterval, func(at time.Time) tea.Msg { return refreshMsg(at) })
}

func (m *Model) clampCursor() {
	limits := [3]int{0, len(m.snapshot.Investigations), len(m.snapshot.Approvals)}
	for index, limit := range limits {
		if limit == 0 {
			m.cursors[index] = 0
		} else if m.cursors[index] >= limit {
			m.cursors[index] = limit - 1
		}
	}
}

func (m Model) selectedApproval() (Approval, bool) {
	if len(m.snapshot.Approvals) == 0 {
		return Approval{}, false
	}
	return m.snapshot.Approvals[m.cursors[approvalsView]], true
}

func metricCard(label, value, hint string, width int) string {
	content := mutedStyle.Render(label) + "\n" + lipgloss.NewStyle().Bold(true).Foreground(colorInk).Render(value) + "\n" + mutedStyle.Render(hint)
	return panelStyle.Width(max(14, width)).Render(content)
}

func emptyPanel(width int, title, detail string) string {
	content := titleStyle.Render(title) + "\n" + mutedStyle.Render(detail)
	return lipgloss.NewStyle().Padding(2, 1).Render(panelStyle.Width(max(26, width-6)).Padding(1, 2).Render(content))
}

func ticketLabel(item Investigation) string {
	if item.Ticket.ExternalID != "" {
		return truncate(item.Ticket.ExternalID, 12)
	}
	return shortID(string(item.ID))
}

func shortID(value string) string {
	if value == "" {
		return "—"
	}
	if len(value) <= 12 {
		return value
	}
	return value[:8] + "…"
}

func truncate(value string, width int) string {
	if width < 2 {
		return ""
	}
	if utf8.RuneCountInString(value) <= width {
		return value
	}
	runes := []rune(value)
	return string(runes[:width-1]) + "…"
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return "—"
	}
	return value.Local().Format("Jan 02 15:04")
}

func relativeExpiry(value time.Time) string {
	if value.IsZero() {
		return "—"
	}
	remaining := time.Until(value)
	if remaining <= 0 {
		return "expired"
	}
	if remaining < time.Hour {
		return fmt.Sprintf("%dm", int(remaining.Minutes()))
	}
	if remaining < 24*time.Hour {
		return fmt.Sprintf("%dh", int(remaining.Hours()))
	}
	return fmt.Sprintf("%dd", int(remaining.Hours()/24))
}

func visibleRange(total, maximum, cursor int) (int, int) {
	if total <= maximum {
		return 0, total
	}
	start := max(0, cursor-maximum/2)
	end := min(total, start+maximum)
	start = max(0, end-maximum)
	return start, end
}

func titleCase(value string) string {
	if value == "" {
		return ""
	}
	return strings.ToUpper(value[:1]) + value[1:]
}
