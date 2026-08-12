package main

import (
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
)

type tuiGeometry struct {
	TooSmall                bool
	HeaderHeight            int
	MainHeight              int
	TranscriptOuterWidth    int
	TranscriptContentWidth  int
	TranscriptContentHeight int
	RailWidth               int
	OverlayHeight           int
	InputOuterHeight        int
	InputContentWidth       int
	StatusHeight            int
}

type tuiTheme struct {
	background lipgloss.Color
	surface    lipgloss.Color
	surfaceAlt lipgloss.Color
	border     lipgloss.Color
	primary    lipgloss.Color
	text       lipgloss.Color
	muted      lipgloss.Color
	healthy    lipgloss.Color
	warning    lipgloss.Color
	danger     lipgloss.Color
	diffAdd    lipgloss.Color

	noColor bool
}

func newTUITheme(noColor bool) tuiTheme {
	return tuiTheme{
		background: lipgloss.Color("#0A0C0F"),
		surface:    lipgloss.Color("#111419"),
		surfaceAlt: lipgloss.Color("#171B21"),
		border:     lipgloss.Color("#B67924"),
		primary:    lipgloss.Color("#31BDE8"),
		text:       lipgloss.Color("#D7DBE0"),
		muted:      lipgloss.Color("#7D8793"),
		healthy:    lipgloss.Color("#A6D93A"),
		warning:    lipgloss.Color("#E0A43A"),
		danger:     lipgloss.Color("#E45B62"),
		diffAdd:    lipgloss.Color("#7CC7A0"),
		noColor:    noColor,
	}
}

func (t tuiTheme) fg(color lipgloss.Color) lipgloss.Style {
	style := lipgloss.NewStyle()
	if !t.noColor {
		style = style.Foreground(color)
	}
	return style
}

func (t tuiTheme) bg(color lipgloss.Color) lipgloss.Style {
	style := lipgloss.NewStyle()
	if !t.noColor {
		style = style.Background(color)
	}
	return style
}

func (t tuiTheme) colors(foreground, background lipgloss.Color) lipgloss.Style {
	style := lipgloss.NewStyle()
	if !t.noColor {
		style = style.Foreground(foreground).Background(background)
	}
	return style
}

func (m tuiModel) View() string {
	if m.securityReviewRestricted() {
		return m.renderRestrictedSecurityReview()
	}
	if m.width < 40 || m.height < 10 {
		width := maxInt(1, m.width)
		height := maxInt(1, m.height)
		lines := []string{"Corelay Code TUI", "terminal too small", "resize to at least 40x10", "Ctrl+Q to quit"}
		if len(lines) > height {
			lines = lines[:height]
		}
		for index := range lines {
			lines[index] = fitDisplay(lines[index], width)
		}
		return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, strings.Join(lines, "\n"))
	}

	theme := newTUITheme(m.opts.NoColor)
	geometry := calculateTUIGeometry(m.width, m.height, m.overlayHeight())
	header := m.renderHeader(theme, geometry)
	transcript := m.renderTranscriptPanel(theme, geometry)
	main := transcript
	if geometry.RailWidth > 0 {
		rail := m.renderRail(theme, geometry)
		main = lipgloss.JoinHorizontal(lipgloss.Top, transcript, " ", rail)
	}

	blocks := []string{header, main}
	if geometry.OverlayHeight > 0 {
		blocks = append(blocks, m.renderOverlay(theme, geometry))
	}
	blocks = append(blocks, m.renderInput(theme, geometry), m.renderStatusBar(theme, geometry))
	content := lipgloss.JoinVertical(lipgloss.Left, blocks...)
	root := lipgloss.NewStyle().Width(m.width).Height(m.height)
	if !theme.noColor {
		root = root.Background(theme.background).Foreground(theme.text)
	}
	return root.Render(content)
}

func (m tuiModel) securityReviewRestricted() bool {
	if m.approval == nil && m.confirm == nil {
		return false
	}
	if m.width < 40 || m.height < 10 {
		return true
	}
	geometry := calculateTUIGeometry(m.width, m.height, m.overlayHeight())
	requiredHeight := 5
	if m.approval != nil {
		requiredHeight = 7
	}
	return geometry.OverlayHeight < requiredHeight
}

func (m tuiModel) renderRestrictedSecurityReview() string {
	width := maxInt(1, m.width)
	height := maxInt(1, m.height)
	action := "D/Esc/Enter deny"
	detail := "Permission details do not fit."
	if m.confirm != nil {
		action = "N/Esc/Enter cancel"
		detail = "Confirmation details do not fit."
	}
	lines := []string{
		"CORELAY CODE SAFETY LOCK",
		detail,
		"Resize to review all required details.",
		"Positive action is disabled.",
		action,
		"Ctrl+C cancel · Ctrl+Q quit",
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	for index := range lines {
		lines[index] = fitDisplay(lines[index], width)
	}
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, strings.Join(lines, "\n"))
}

func calculateTUIGeometry(width, height, overlayHeight int) tuiGeometry {
	geometry := tuiGeometry{TooSmall: width < 40 || height < 10, StatusHeight: 1, InputOuterHeight: 3}
	if geometry.TooSmall {
		return geometry
	}
	geometry.HeaderHeight = 1
	if width < 80 {
		geometry.HeaderHeight = 2
	}
	geometry.OverlayHeight = overlayHeight
	maxOverlay := maxInt(0, height-geometry.HeaderHeight-geometry.InputOuterHeight-geometry.StatusHeight-5)
	if geometry.OverlayHeight > maxOverlay {
		geometry.OverlayHeight = maxOverlay
	}
	geometry.MainHeight = height - geometry.HeaderHeight - geometry.OverlayHeight - geometry.InputOuterHeight - geometry.StatusHeight
	if geometry.MainHeight < 3 {
		geometry.MainHeight = 3
	}
	if width >= 120 && height >= 28 {
		geometry.RailWidth = 30
	}
	geometry.TranscriptOuterWidth = width
	if geometry.RailWidth > 0 {
		geometry.TranscriptOuterWidth = width - geometry.RailWidth - 1
	}
	geometry.TranscriptContentWidth = maxInt(1, geometry.TranscriptOuterWidth-2)
	geometry.TranscriptContentHeight = maxInt(1, geometry.MainHeight-2)
	geometry.InputContentWidth = maxInt(1, width-4)
	return geometry
}

func (m tuiModel) overlayHeight() int {
	switch {
	case m.approval != nil:
		return 9
	case m.confirm != nil:
		return 7
	case m.paletteOpen:
		rows := len(m.paletteMatches)
		if rows == 0 {
			rows = 1
		}
		return minInt(12, rows+3)
	default:
		return 0
	}
}

func (m tuiModel) renderHeader(theme tuiTheme, geometry tuiGeometry) string {
	brand := theme.fg(theme.text).Bold(true).Render(" CORELAY CODE TUI ")
	version := singleLineTUIText(m.root.Version, 80)
	if version == "" {
		version = "—"
	}
	brand += theme.fg(theme.muted).Render("v" + version)
	target := strings.Trim(singleLineTUIText(m.config.Provider+" / "+m.config.Model, 512), " /")
	if target == "" {
		target = "target unknown"
	}
	mode := "DIRECT"
	if m.config.RouterEnabled {
		mode = "ROUTER"
	}
	workDir := m.opts.WorkDir
	if base := filepath.Base(workDir); base != "." && base != string(filepath.Separator) && base != "" {
		workDir = base + " · " + workDir
	}
	workDir = singleLineTUIText(workDir, 1024)
	connection := strings.ToUpper(string(m.connection))

	if m.width < 80 {
		first := fitDisplay(strings.Join([]string{brand, theme.fg(theme.primary).Render(target)}, "  "), m.width)
		second := fitDisplay(fmt.Sprintf(" %s  %s  %s", mode, workDir, connection), m.width)
		return first + "\n" + theme.fg(theme.muted).Render(second)
	}

	leftWidth := minInt(28, maxInt(18, m.width/5))
	targetWidth := minInt(42, maxInt(24, m.width/3))
	modeWidth := 10
	connectionWidth := minInt(20, maxInt(13, len(connection)+2))
	workWidth := maxInt(10, m.width-leftWidth-targetWidth-modeWidth-connectionWidth)
	segments := []string{
		theme.bg(theme.surfaceAlt).Width(leftWidth).Render(fitDisplay(brand, leftWidth)),
		theme.colors(theme.primary, theme.surface).Width(targetWidth).Render(" " + fitDisplay(target, targetWidth-1)),
		theme.colors(theme.muted, theme.surfaceAlt).Width(workWidth).Render(" " + fitDisplay(workDir, workWidth-1)),
		theme.colors(theme.warning, theme.surface).Bold(true).Width(modeWidth).Align(lipgloss.Center).Render(mode),
		m.headerConnectionStyle(theme).Width(connectionWidth).Align(lipgloss.Center).Render(connection),
	}
	return fitDisplay(lipgloss.JoinHorizontal(lipgloss.Top, segments...), m.width)
}

func (m tuiModel) headerConnectionStyle(theme tuiTheme) lipgloss.Style {
	color := theme.muted
	switch m.connection {
	case tuiConnected:
		color = theme.healthy
	case tuiAuthRequired:
		color = theme.warning
	case tuiDisconnected:
		color = theme.danger
	}
	return theme.colors(color, theme.surfaceAlt).Bold(true)
}

func (m tuiModel) renderTranscriptPanel(theme tuiTheme, geometry tuiGeometry) string {
	displayViewport := m.viewport
	displayViewport.Width = geometry.TranscriptContentWidth
	displayViewport.Height = geometry.TranscriptContentHeight
	content := displayViewport.View()
	style := lipgloss.NewStyle().
		Width(geometry.TranscriptContentWidth).
		Height(geometry.TranscriptContentHeight).
		Border(lipgloss.NormalBorder()).
		Padding(0)
	if !theme.noColor {
		style = style.BorderForeground(theme.border).Background(theme.background).Foreground(theme.text)
	}
	return style.Render(content)
}

func (m tuiModel) renderRail(theme tuiTheme, geometry tuiGeometry) string {
	width := geometry.RailWidth
	heights := splitRailHeights(geometry.MainHeight)
	contextLines := []string{
		m.contextSummary(),
		renderMeter(theme, m.contextMeter.Estimated, m.contextMeter.Window, width-4),
		m.speedSummary(),
	}
	activity := []string{
		strings.ToUpper(string(m.runState)),
		fitDisplay(singleLineTUIText(m.activitySummary(), 512), width-4),
		fmt.Sprintf("ACTIVE RUNS  %d", len(m.activeLoops)),
	}
	connection := []string{
		"SERVER  " + connectionLabel(m.connection),
		"TARGET  " + fitDisplay(strings.Trim(singleLineTUIText(m.server.Provider+"/"+m.server.Model, 512), "/"), width-12),
		"URL     " + fitDisplay(singleLineTUIText(m.opts.BaseURL, 1024), width-12),
	}
	session := []string{
		m.sessionStateLabel(),
		m.sessionIdentity(width - 4),
		m.sessionRevision(),
	}
	panels := []string{
		renderRailPanel(theme, "CONTEXT", contextLines, width, heights[0]),
		renderRailPanel(theme, "ACTIVITY", activity, width, heights[1]),
		renderRailPanel(theme, "LINK", connection, width, heights[2]),
		renderRailPanel(theme, "SESSION", session, width, heights[3]),
	}
	return lipgloss.JoinVertical(lipgloss.Left, panels...)
}

func splitRailHeights(total int) [4]int {
	result := [4]int{}
	remaining := total
	for index := range result {
		parts := len(result) - index
		result[index] = remaining / parts
		if result[index] < 4 {
			result[index] = 4
		}
		remaining -= result[index]
	}
	if remaining != 0 {
		result[len(result)-1] += remaining
	}
	return result
}

func renderRailPanel(theme tuiTheme, title string, lines []string, outerWidth, outerHeight int) string {
	contentWidth := maxInt(1, outerWidth-2)
	contentHeight := maxInt(1, outerHeight-2)
	var builder strings.Builder
	titleLine := theme.fg(theme.warning).Bold(true).Render(title)
	builder.WriteString(fitDisplay(titleLine, contentWidth))
	for _, line := range lines {
		builder.WriteByte('\n')
		builder.WriteString(fitDisplay(line, contentWidth))
	}
	style := lipgloss.NewStyle().Width(contentWidth).Height(contentHeight).Border(lipgloss.NormalBorder())
	if !theme.noColor {
		style = style.BorderForeground(theme.border).Background(theme.background).Foreground(theme.text)
	}
	return style.Render(builder.String())
}

func (m tuiModel) renderOverlay(theme tuiTheme, geometry tuiGeometry) string {
	width := minInt(geometry.TranscriptOuterWidth, maxInt(40, minInt(90, m.width-2)))
	if width > m.width {
		width = m.width
	}
	var overlay string
	switch {
	case m.approval != nil:
		overlay = m.renderApproval(theme, width, geometry.OverlayHeight)
	case m.confirm != nil:
		overlay = m.renderConfirmation(theme, width, geometry.OverlayHeight)
	case m.paletteOpen:
		overlay = m.renderPalette(theme, width, geometry.OverlayHeight)
	}
	return lipgloss.NewStyle().Width(m.width).Render(overlay)
}

func (m tuiModel) renderPalette(theme tuiTheme, outerWidth, outerHeight int) string {
	contentWidth := maxInt(1, outerWidth-2)
	contentHeight := maxInt(1, outerHeight-2)
	var builder strings.Builder
	builder.WriteString(theme.fg(theme.warning).Bold(true).Render("COMMANDS"))
	builder.WriteString(theme.fg(theme.muted).Render("  ↑↓ move · Tab complete · Enter run · Esc close"))
	if len(m.paletteMatches) == 0 {
		builder.WriteString("\n")
		builder.WriteString(theme.fg(theme.muted).Render("No matching command"))
	}
	maxRows := maxInt(0, contentHeight-1)
	start := 0
	if maxRows > 0 && m.paletteIndex >= maxRows {
		start = m.paletteIndex - maxRows + 1
	}
	end := minInt(len(m.paletteMatches), start+maxRows)
	for index := start; index < end; index++ {
		command := m.paletteMatches[index]
		builder.WriteByte('\n')
		nameWidth := minInt(24, maxInt(12, contentWidth/3))
		name := fitDisplay("/"+singleLineTUIText(command.Name, 128), nameWidth)
		description := fitDisplay(singleLineTUIText(command.Description, 512), maxInt(1, contentWidth-nameWidth-2))
		line := fmt.Sprintf("%-*s  %s", nameWidth, name, description)
		if index == m.paletteIndex {
			style := theme.colors(theme.background, theme.primary).Bold(true).Width(contentWidth)
			line = style.Render(fitDisplay(line, contentWidth))
		} else {
			line = theme.fg(theme.text).Render(fitDisplay(line, contentWidth))
		}
		builder.WriteString(line)
	}
	style := lipgloss.NewStyle().Width(contentWidth).Height(contentHeight).Border(lipgloss.NormalBorder())
	if !theme.noColor {
		style = style.BorderForeground(theme.border).Background(theme.surface).Foreground(theme.text)
	}
	return style.Render(builder.String())
}

func (m tuiModel) renderApproval(theme tuiTheme, outerWidth, outerHeight int) string {
	approval := m.approval
	contentWidth := maxInt(1, outerWidth-2)
	contentHeight := maxInt(1, outerHeight-2)
	remaining := ""
	if !approval.ExpiresAt.IsZero() {
		duration := time.Until(approval.ExpiresAt).Round(time.Second)
		if duration < 0 {
			duration = 0
		}
		remaining = fmt.Sprintf(" · expires in %s", duration)
	}
	state := "A allow once · D/Esc/Enter deny"
	if approval.Resolving {
		state = "Recording fail-closed decision…"
	}
	lines := []string{
		theme.fg(theme.danger).Bold(true).Render("PERMISSION REQUIRED") + theme.fg(theme.muted).Render(remaining),
		"Tool   " + fitDisplay(singleLineTUIText(approval.ToolName, 256), contentWidth-7),
		"Risk   " + fitDisplay(strings.Trim(singleLineTUIText(approval.DangerLevel+" · "+approval.Scope, 512), " ·"), contentWidth-7),
		"Input  " + fitDisplay(singleLineTUIText(approval.RedactedInput, 4096), contentWidth-7),
		theme.fg(theme.warning).Bold(true).Render(state),
	}
	if len(lines) > contentHeight {
		lines = lines[:contentHeight]
	}
	style := lipgloss.NewStyle().Width(contentWidth).Height(contentHeight).Border(lipgloss.NormalBorder()).Padding(0)
	if !theme.noColor {
		style = style.BorderForeground(theme.danger).Background(theme.surface).Foreground(theme.text)
	}
	return style.Render(strings.Join(lines, "\n"))
}

func (m tuiModel) renderConfirmation(theme tuiTheme, outerWidth, outerHeight int) string {
	confirmation := m.confirm
	contentWidth := maxInt(1, outerWidth-2)
	contentHeight := maxInt(1, outerHeight-2)
	lines := []string{
		theme.fg(theme.warning).Bold(true).Render(singleLineTUIText(confirmation.Title, contentWidth*4)),
		fitDisplay(singleLineTUIText(confirmation.Description, 2048), contentWidth),
		theme.fg(theme.danger).Render("Y confirms · N/Esc/Enter cancels"),
	}
	if len(lines) > contentHeight {
		lines = lines[:contentHeight]
	}
	style := lipgloss.NewStyle().Width(contentWidth).Height(contentHeight).Border(lipgloss.NormalBorder())
	if !theme.noColor {
		style = style.BorderForeground(theme.warning).Background(theme.surface).Foreground(theme.text)
	}
	return style.Render(strings.Join(lines, "\n"))
}

func (m tuiModel) renderInput(theme tuiTheme, geometry tuiGeometry) string {
	content := m.input.View()
	label := " MESSAGE "
	if m.operationBusy || m.awaitingReload {
		content = theme.fg(theme.muted).Render("Synchronizing UI state. Input will unlock when the server reply is applied.")
		label = " UI BUSY "
	} else if m.runActive() {
		content = theme.fg(theme.muted).Render("Agent is running. Esc or Ctrl+C requests cancellation.")
		label = " RUN LOCKED "
	} else if !m.sessionWritable() {
		content = theme.fg(theme.warning).Render("Session requires an action. Type /reconcile, /new, or /sessions.")
		label = " SESSION LOCKED "
	}
	style := lipgloss.NewStyle().Width(maxInt(1, m.width-2)).Height(1).Border(lipgloss.NormalBorder())
	if !theme.noColor {
		style = style.BorderForeground(theme.border).Background(theme.surface).Foreground(theme.text)
	}
	return style.Render(theme.fg(theme.warning).Bold(true).Render(label) + fitDisplay(content, maxInt(1, geometry.InputContentWidth-lipgloss.Width(label))))
}

func (m tuiModel) renderStatusBar(theme tuiTheme, _ tuiGeometry) string {
	left := fmt.Sprintf(" %s · %s", strings.ToUpper(string(m.runState)), m.statusHint())
	right := "Ctrl+K commands · PgUp/PgDn scroll · Ctrl+Q quit "
	if m.newOutputWhileAway > 0 {
		right = fmt.Sprintf("%d new · End follow · ", m.newOutputWhileAway) + right
	}
	space := maxInt(1, m.width-lipgloss.Width(left)-lipgloss.Width(right))
	line := left + strings.Repeat(" ", space) + right
	style := theme.bg(theme.surfaceAlt).Width(m.width)
	if !theme.noColor {
		style = style.Foreground(theme.muted)
	}
	return style.Render(fitDisplay(line, m.width))
}

func (m tuiModel) renderTranscript() string {
	width := maxInt(10, m.viewport.Width)
	contentWidth := maxInt(1, width-8)
	theme := newTUITheme(m.opts.NoColor)
	var builder strings.Builder
	for index, entry := range m.entries {
		if index > 0 {
			builder.WriteString("\n\n")
		}
		label, labelStyle, textStyle := transcriptEntryStyle(theme, entry.Kind)
		builder.WriteString(labelStyle.Width(7).Render(label))
		text := safeTUIText(entry.Text, tuiEntryBytes)
		wrapped := textStyle.Width(contentWidth).Render(text)
		lines := strings.Split(wrapped, "\n")
		if len(lines) > 0 {
			builder.WriteString(lines[0])
		}
		for _, line := range lines[1:] {
			builder.WriteByte('\n')
			builder.WriteString(strings.Repeat(" ", 7))
			builder.WriteString(line)
		}
	}
	return builder.String()
}

func transcriptEntryStyle(theme tuiTheme, kind tuiEntryKind) (string, lipgloss.Style, lipgloss.Style) {
	label := "SYS"
	color := theme.muted
	text := theme.fg(theme.text)
	switch kind {
	case tuiEntryUser:
		label, color = "YOU", theme.warning
	case tuiEntryAssistant:
		label, color = "ANI", theme.primary
	case tuiEntryStatus:
		label, color = "RUN", theme.muted
		text = theme.fg(theme.muted)
	case tuiEntryTool:
		label, color = "TOOL", theme.primary
	case tuiEntryResult:
		label, color = "OUT", theme.healthy
	case tuiEntryDiff:
		label, color = "DIFF", theme.diffAdd
	case tuiEntryError:
		label, color = "ERROR", theme.danger
		text = theme.fg(theme.danger)
	case tuiEntryThinking:
		label, color = "THINK", theme.muted
		text = theme.fg(theme.muted).Italic(true)
	}
	return label, theme.fg(color).Bold(true), text
}

func (m tuiModel) contextSummary() string {
	if m.contextMeter.Window <= 0 {
		return "waiting for context plan"
	}
	percent := float64(m.contextMeter.Estimated) / float64(m.contextMeter.Window) * 100
	return fmt.Sprintf("%s / %s  %.0f%%", compactNumber(m.contextMeter.Estimated), compactNumber(m.contextMeter.Window), percent)
}

func (m tuiModel) speedSummary() string {
	if m.heartbeatMS <= 0 || m.heartbeatChars <= 0 {
		return "stream — chars/s"
	}
	rate := float64(m.heartbeatChars) / (float64(m.heartbeatMS) / 1000)
	return fmt.Sprintf("stream %.1f chars/s", rate)
}

func (m tuiModel) activitySummary() string {
	if m.currentTool != "" {
		return "tool · " + singleLineTUIText(m.currentTool, 256)
	}
	if m.lastStatus != "" {
		return singleLineTUIText(m.lastStatus, 512)
	}
	return "waiting for input"
}

func (m tuiModel) sessionStateLabel() string {
	if m.current == nil {
		return "EPHEMERAL · not yet saved"
	}
	state := string(m.current.LifecycleStatus)
	if state == "" {
		state = "active"
	}
	if m.current.ReconcileRequired {
		state += " · reconcile"
	}
	return strings.ToUpper(state)
}

func (m tuiModel) sessionIdentity(width int) string {
	if m.current == nil {
		return "ID  —"
	}
	return "ID  " + fitDisplay(singleLineTUIText(m.current.ID, 256), maxInt(1, width-4))
}

func (m tuiModel) sessionRevision() string {
	if m.current == nil {
		return "REV —"
	}
	return fmt.Sprintf("REV %d · TURNS %d", m.current.Revision, m.current.Turns)
}

func (m tuiModel) statusHint() string {
	switch m.connection {
	case tuiAuthRequired:
		return "access token required"
	case tuiDisconnected:
		return "offline · /connect retries"
	case tuiConnecting:
		return "connecting"
	}
	if m.runActive() && !m.runStarted.IsZero() {
		return "elapsed " + time.Since(m.runStarted).Round(time.Second).String()
	}
	if m.current != nil {
		return fmt.Sprintf("session r%d", m.current.Revision)
	}
	return "ready"
}

func renderMeter(theme tuiTheme, value, total, width int) string {
	width = maxInt(4, minInt(24, width))
	filled := 0
	if total > 0 && value > 0 {
		filled = int(math.Round(float64(width) * math.Min(1, float64(value)/float64(total))))
	}
	bar := strings.Repeat("━", filled) + strings.Repeat("─", width-filled)
	if theme.noColor {
		return bar
	}
	return theme.fg(theme.primary).Render(strings.Repeat("━", filled)) + theme.fg(theme.muted).Render(strings.Repeat("─", width-filled))
}

func connectionLabel(state tuiConnectionState) string {
	switch state {
	case tuiConnected:
		return "OK"
	case tuiAuthRequired:
		return "AUTH REQUIRED"
	case tuiDisconnected:
		return "OFFLINE"
	default:
		return "CONNECTING"
	}
}

func compactNumber(value int) string {
	switch {
	case value >= 1_000_000:
		return fmt.Sprintf("%.1fm", float64(value)/1_000_000)
	case value >= 1_000:
		return fmt.Sprintf("%.1fk", float64(value)/1_000)
	default:
		return fmt.Sprintf("%d", value)
	}
}

func fitDisplay(value string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(value) <= width {
		return value
	}
	ellipsis := "…"
	limit := maxInt(0, width-lipgloss.Width(ellipsis))
	var builder strings.Builder
	for len(value) > 0 {
		_, size := utf8.DecodeRuneInString(value)
		if size <= 0 {
			break
		}
		candidate := builder.String() + value[:size]
		if lipgloss.Width(candidate) > limit {
			break
		}
		builder.WriteString(value[:size])
		value = value[size:]
	}
	return builder.String() + ellipsis
}

func singleLineTUIText(value string, maxBytes int) string {
	value = safeTUIText(value, maxBytes)
	value = strings.NewReplacer("\n", " ", "\t", " ").Replace(value)
	return strings.Join(strings.Fields(value), " ")
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}
