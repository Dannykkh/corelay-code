package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Dannykkh/corelay-code/internal/agent"
	tea "github.com/charmbracelet/bubbletea"
)

type tuiCommand struct {
	Name        string
	Description string
	NeedsArgs   bool
	Local       bool
	Skill       bool
}

func defaultTUICommands() []tuiCommand {
	return []tuiCommand{
		{Name: "help", Description: "Show TUI, session, and agent commands", Local: true},
		{Name: "new", Description: "Detach the current session and start a new chat", Local: true},
		{Name: "sessions", Description: "List durable sessions in this workspace", Local: true},
		{Name: "load", Description: "Load a durable session by ID", NeedsArgs: true, Local: true},
		{Name: "fork", Description: "Fork the current session at its exact revision", Local: true},
		{Name: "reconcile", Description: "Acknowledge and clear an interrupted-run guard", Local: true},
		{Name: "close", Description: "Close the current durable session", Local: true},
		{Name: "delete", Description: "Delete the current durable session and result store", Local: true},
		{Name: "rename", Description: "Rename the current durable session", NeedsArgs: true, Local: true},
		{Name: "save", Description: "Show the current autosave revision", Local: true},
		{Name: "diff", Description: "Show the most recent diff from this TUI run", Local: true},
		{Name: "model", Description: "Change the server-wide provider and model", NeedsArgs: true, Local: true},
		{Name: "lang", Description: "Set response language for this TUI", NeedsArgs: true, Local: true},
		{Name: "connect", Description: "Retry server discovery without losing the draft", Local: true},
		{Name: "version", Description: "Show server and TUI version information", Local: true},
		{Name: "clear", Description: "Start a new chat without deleting saved history", Local: true},
		{Name: "cancel", Description: "Cancel the active runtime session", Local: true},
		{Name: "quit", Description: "Exit and restore the terminal", Local: true},
		{Name: "plan", Description: "Run the next task in read-only plan mode"},
		{Name: "compact", Description: "Compact the current agent context"},
		{Name: "undo", Description: "Restore the most recent agent edit checkpoint"},
	}
}

func skillTUICommands(commands []agent.SlashCommand) []tuiCommand {
	result := make([]tuiCommand, 0, len(commands))
	seen := make(map[string]struct{}, len(commands))
	for _, command := range commands {
		name := strings.ToLower(strings.TrimSpace(command.Name))
		if name == "" {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, tuiCommand{
			Name:        name,
			Description: safeTUIText(command.Description, 160),
			Skill:       true,
		})
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func (m *tuiModel) openPalette() {
	m.paletteOpen = true
	m.refreshPaletteMatches()
}

func (m *tuiModel) refreshPaletteMatches() {
	value := strings.TrimSpace(m.input.Value())
	query := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(value, "/")))
	if index := strings.IndexAny(query, " \t"); index >= 0 {
		query = query[:index]
	}

	all := make([]tuiCommand, 0, len(m.staticCommands)+len(m.dynamicCommands))
	// Skills come first because the kernel resolves exact skill-name collisions
	// before its built-ins. The palette must preserve that meaning.
	all = append(all, m.dynamicCommands...)
	all = append(all, m.staticCommands...)
	type scored struct {
		command tuiCommand
		score   int
		order   int
	}
	matches := make([]scored, 0, len(all))
	seen := make(map[string]struct{}, len(all))
	for index, command := range all {
		name := strings.ToLower(command.Name)
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		description := strings.ToLower(command.Description)
		score := 0
		switch {
		case query == "":
			score = 1
		case name == query:
			score = 100
		case strings.HasPrefix(name, query):
			score = 80
		case strings.Contains(name, query):
			score = 60
		case strings.Contains(description, query):
			score = 20
		default:
			continue
		}
		matches = append(matches, scored{command: command, score: score, order: index})
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		return matches[i].order < matches[j].order
	})
	if len(matches) > 10 {
		matches = matches[:10]
	}
	m.paletteMatches = make([]tuiCommand, len(matches))
	for index := range matches {
		m.paletteMatches[index] = matches[index].command
	}
	if len(m.paletteMatches) == 0 {
		m.paletteIndex = 0
	} else if m.paletteIndex >= len(m.paletteMatches) {
		m.paletteIndex = len(m.paletteMatches) - 1
	}
}

func (m *tuiModel) completePaletteSelection() {
	if len(m.paletteMatches) == 0 {
		return
	}
	command := m.paletteMatches[m.paletteIndex]
	value := "/" + command.Name
	if command.NeedsArgs {
		value += " "
	}
	m.input.SetValue(value)
	m.input.CursorEnd()
	m.paletteOpen = command.NeedsArgs
	m.refreshPaletteMatches()
}

func (m *tuiModel) choosePaletteSelection() bool {
	if len(m.paletteMatches) == 0 {
		return false
	}
	command := m.paletteMatches[m.paletteIndex]
	current := strings.TrimSpace(m.input.Value())
	name, args := splitTUICommand(current)
	if name != command.Name || command.NeedsArgs && args == "" {
		m.completePaletteSelection()
		return !command.NeedsArgs
	}
	m.paletteOpen = false
	return true
}

func (m tuiModel) allCommands() []tuiCommand {
	commands := make([]tuiCommand, 0, len(m.dynamicCommands)+len(m.staticCommands))
	commands = append(commands, m.dynamicCommands...)
	commands = append(commands, m.staticCommands...)
	return commands
}

func (m tuiModel) commandNamed(name string) (tuiCommand, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, command := range m.dynamicCommands {
		if command.Name == name {
			return command, true
		}
	}
	for _, command := range m.staticCommands {
		if command.Name == name {
			return command, true
		}
	}
	return tuiCommand{}, false
}

func (m tuiModel) submitInput() (tea.Model, tea.Cmd) {
	value := strings.TrimSpace(m.input.Value())
	if value == "" {
		return m, nil
	}
	if strings.HasPrefix(value, "/") {
		name, args := splitTUICommand(value)
		if command, ok := m.commandNamed(name); ok && command.Local {
			m.input.Reset()
			m.paletteOpen = false
			m.paletteMatches = nil
			return m.executeLocalCommand(name, args)
		}
		// Dynamic skill commands and kernel built-ins are sent unchanged so the
		// server remains the only source of truth for slash-command semantics.
	}
	if !m.sessionWritable() {
		m.appendEntry(tuiEntryError, "This session is read-only. Reconcile an interrupted session or start a new one before sending.")
		return m, nil
	}

	prompt := safeTUIText(value, 32<<10)
	if strings.TrimSpace(prompt) == "" {
		return m, nil
	}
	m.input.Reset()
	m.paletteOpen = false
	m.paletteMatches = nil
	m.assistantEntry = -1
	m.assistantText = ""
	m.pendingToolInput = ""
	m.doneSeen = false
	m.streamEndSeen = false
	m.durableError = false
	m.cancelRequested = false
	m.runtimeID = ""
	m.heartbeatChars = 0
	m.heartbeatMS = 0
	m.contextMeter = tuiContextMeter{}
	m.currentTool = ""
	m.runStarted = time.Now()
	m.runGeneration++
	m.runState = tuiRunSubmitting
	m.lastStatus = "Saving the durable user turn"
	m.appendEntry(tuiEntryUser, prompt)

	ctx, rawCancel := context.WithCancel(context.Background())
	cancel := onceTUICancel(rawCancel)
	m.turnCancel = cancel
	return m, m.prepareTurnCmd(ctx, prompt, cancel)
}

func onceTUICancel(cancel context.CancelFunc) context.CancelFunc {
	var once sync.Once
	return func() {
		once.Do(cancel)
	}
}

func splitTUICommand(value string) (string, string) {
	value = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), "/"))
	parts := strings.SplitN(value, " ", 2)
	if len(parts) == 0 {
		return "", ""
	}
	name := strings.ToLower(strings.TrimSpace(parts[0]))
	if len(parts) == 1 {
		return name, ""
	}
	return name, strings.TrimSpace(parts[1])
}

func (m tuiModel) executeLocalCommand(name, args string) (tea.Model, tea.Cmd) {
	switch name {
	case "help":
		m.showHelp()
	case "new", "clear":
		m.newSession()
	case "sessions":
		m.operationBusy = true
		m.lastStatus = "Loading durable sessions"
		return m, m.listSessionsCmd()
	case "load":
		if args == "" {
			m.appendEntry(tuiEntryError, "Usage: /load <session-id>")
			break
		}
		m.operationBusy = true
		m.lastStatus = "Loading durable session"
		return m, m.loadSessionCmd(args)
	case "fork":
		if m.current == nil {
			m.appendEntry(tuiEntryError, "No durable session is loaded")
			break
		}
		m.operationBusy = true
		m.lastStatus = "Forking durable session"
		return m, m.sessionOperationCmd("fork", m.current.ID, m.current.Revision, "")
	case "reconcile":
		if m.current == nil || !m.current.ReconcileRequired {
			m.appendEntry(tuiEntryError, "The current session does not require reconciliation")
			break
		}
		detail := "Review the external side effect before continuing."
		if m.current.Interruption != nil {
			detail = fmt.Sprintf("%s · tool=%s · state=%s · digest=%s",
				safeTUIText(m.current.Interruption.Summary, 240),
				safeTUIText(m.current.Interruption.ToolName, 80),
				m.current.Interruption.SideEffectState,
				safeTUIText(m.current.Interruption.InputDigest, 90),
			)
		}
		m.confirm = &tuiConfirmation{Action: "reconcile", Title: "Mark this interrupted run reconciled?", Description: detail}
	case "close":
		if m.current == nil {
			m.appendEntry(tuiEntryError, "No durable session is loaded")
			break
		}
		m.confirm = &tuiConfirmation{Action: "close", Title: "Close this durable session?", Description: "Closed sessions remain readable but cannot accept another turn."}
	case "delete":
		if m.current == nil {
			m.appendEntry(tuiEntryError, "No durable session is loaded")
			break
		}
		m.confirm = &tuiConfirmation{Action: "delete", Title: "Delete this durable session?", Description: "The transcript and its tool-result store will be removed."}
	case "rename":
		if m.current == nil || args == "" {
			m.appendEntry(tuiEntryError, "Usage: /rename <title> with a loaded session")
			break
		}
		m.operationBusy = true
		m.lastStatus = "Renaming durable session"
		return m, m.sessionOperationCmd("rename", m.current.ID, m.current.Revision, args)
	case "save":
		if m.current == nil {
			m.appendEntry(tuiEntrySystem, "The first sent message will create a durable session")
		} else {
			m.appendEntry(tuiEntrySystem, fmt.Sprintf("Autosaved session %s at revision %d", m.current.ID, m.current.Revision))
		}
	case "diff":
		if m.lastDiff == "" {
			m.appendEntry(tuiEntrySystem, "No diff has been emitted in this TUI run")
		} else {
			m.appendEntry(tuiEntryDiff, m.lastDiff)
		}
	case "model":
		parts := strings.Fields(args)
		if len(parts) != 2 {
			m.appendEntry(tuiEntryError, "Usage: /model <provider> <model>. This changes the server-wide target.")
			break
		}
		if len(m.activeLoops) > 0 {
			m.appendEntry(tuiEntryError, "Model changes are blocked while the server has active runs")
			break
		}
		m.operationBusy = true
		m.lastStatus = "Changing the server-wide model target"
		return m, m.setConfigCmd(parts[0], parts[1])
	case "lang":
		lang := strings.ToLower(strings.TrimSpace(args))
		switch lang {
		case "auto", "en", "ko", "ja", "zh":
			m.opts.Lang = lang
			m.appendEntry(tuiEntrySystem, "Response language set to "+lang)
		default:
			m.appendEntry(tuiEntryError, "Usage: /lang <auto|en|ko|ja|zh>")
		}
	case "connect":
		m.connection = tuiConnecting
		m.operationBusy = true
		m.appendEntry(tuiEntryStatus, "Retrying server discovery")
		return m, m.bootstrapCmd()
	case "version":
		version := m.root.Version
		if version == "" {
			version = "unknown"
		}
		m.appendEntry(tuiEntrySystem, fmt.Sprintf("Corelay Code server %s · TUI 1 · %s", version, m.opts.BaseURL))
	case "cancel":
		if !m.runActive() {
			m.appendEntry(tuiEntrySystem, "No run is active")
			break
		}
		m.runState = tuiRunCancelling
		return m, m.cancelRunCmd()
	case "quit":
		m.stopActiveTurn()
		m.quitting = true
		return m, tea.Quit
	}
	m.input.Reset()
	m.paletteOpen = false
	m.input.Focus()
	return m, nil
}

func (m *tuiModel) showHelp() {
	var builder strings.Builder
	builder.WriteString("TUI keys\n")
	builder.WriteString("  Enter send/select · Ctrl+K commands · Ctrl+O sessions · Ctrl+N new\n")
	builder.WriteString("  PageUp/PageDown scroll · End follow output · Esc/Ctrl+C cancel · Ctrl+Q quit\n")
	builder.WriteString("  During approval: A allows once; D, Esc, or Enter denies. Idle Ctrl+C quits.\n\n")
	builder.WriteString("Commands\n")
	for _, command := range m.staticCommands {
		fmt.Fprintf(&builder, "  /%-12s %s\n", command.Name, command.Description)
	}
	if len(m.dynamicCommands) > 0 {
		builder.WriteString("\nSkill commands\n")
		for _, command := range m.dynamicCommands {
			fmt.Fprintf(&builder, "  /%-12s %s\n", command.Name, command.Description)
		}
	}
	m.appendEntry(tuiEntrySystem, strings.TrimRight(builder.String(), "\n"))
}

func (m tuiModel) prepareTurnCmd(ctx context.Context, prompt string, cancel context.CancelFunc) tea.Cmd {
	generation := m.runGeneration
	current := cloneTUISession(m.current)
	provider := m.config.Provider
	model := m.config.Model
	workDir := m.opts.WorkDir
	lang := m.opts.Lang
	return func() tea.Msg {
		session := current
		if session == nil {
			session = &agent.Session{
				Workspace:       workDir,
				Title:           titleFromPrompt(prompt),
				Provider:        provider,
				Model:           model,
				LifecycleStatus: agent.SessionLifecycleActive,
			}
		}
		session.Messages = append(session.Messages, agent.SessionMessage{
			Role:      "user",
			Content:   prompt,
			Timestamp: time.Now().UTC(),
		})
		session.Turns++
		var expected *uint64
		if session.ID != "" {
			revision := session.Revision
			expected = &revision
		}
		result, err := m.backend.SaveSession(ctx, session, expected)
		if err != nil {
			return tuiTurnPreparedMsg{generation: generation, cancel: cancel, err: err}
		}
		session.ID = result.ID
		session.Version = result.Version
		session.Revision = result.Revision
		revision := result.Revision
		stream := m.backend.StartTurn(ctx, agentTurnRequest{
			Messages:         wireMessagesFromSession(session.Messages),
			WorkDir:          workDir,
			ResponseLang:     lang,
			DurableSessionID: session.ID,
			ExpectedRevision: &revision,
		})
		return tuiTurnPreparedMsg{generation: generation, session: session, stream: stream, cancel: cancel}
	}
}

func (m tuiModel) sessionOperationCmd(action, id string, revision uint64, value string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		var (
			session *agent.Session
			err     error
		)
		switch action {
		case "fork":
			session, err = m.backend.ForkSession(ctx, id, revision)
		case "reconcile":
			session, err = m.backend.ReconcileSession(ctx, id, revision)
		case "close":
			session, err = m.backend.CloseSession(ctx, id, revision)
		case "delete":
			err = m.backend.DeleteSession(ctx, id, revision)
		case "rename":
			session = cloneTUISession(m.current)
			if session == nil || session.ID != id || session.Revision != revision {
				err = fmt.Errorf("session changed before rename")
				break
			}
			session.Title = safeTUIText(value, 256)
			result, saveErr := m.backend.SaveSession(ctx, session, &revision)
			if saveErr != nil {
				err = saveErr
				break
			}
			session.Revision = result.Revision
			session.Version = result.Version
		}
		return tuiSessionOperationMsg{action: action, session: session, err: err}
	}
}

func (m tuiModel) executeConfirmed(confirmation tuiConfirmation) tea.Cmd {
	if m.current == nil {
		return func() tea.Msg {
			return tuiSessionOperationMsg{action: confirmation.Action, err: fmt.Errorf("session is not loaded")}
		}
	}
	return m.sessionOperationCmd(confirmation.Action, m.current.ID, m.current.Revision, "")
}

func (m tuiModel) setConfigCmd(provider, model string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		loops, err := m.backend.ActiveLoops(ctx, "")
		if err != nil {
			return tuiConfigMsg{err: fmt.Errorf("verify active runs before model switch: %w", err)}
		}
		if len(loops) > 0 {
			return tuiConfigMsg{err: fmt.Errorf("model switch is blocked while the server has %d active run(s)", len(loops))}
		}
		config, err := m.backend.SetConfig(ctx, provider, model)
		return tuiConfigMsg{config: config, err: err}
	}
}

func titleFromPrompt(prompt string) string {
	title := strings.TrimSpace(strings.SplitN(prompt, "\n", 2)[0])
	return safeTUIText(title, 72)
}
