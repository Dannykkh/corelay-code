package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

const (
	tuiTranscriptLimit   = 2000
	tuiEntryBytes        = 128 << 10
	tuiAssistantBytes    = 1 << 20
	tuiHealthPollEvery   = 5
	tuiTerminalPollEvery = time.Second
)

type tuiOptions struct {
	BaseURL      string
	WorkDir      string
	Lang         string
	Provider     string
	Model        string
	AccessToken  string
	SessionID    string
	NoColor      bool
	ShowThinking bool
	Quiet        bool
}

type tuiBackend interface {
	Health(context.Context) (serverHealth, error)
	Config(context.Context) (serverConfig, error)
	Root(context.Context) (serverRoot, error)
	SetConfig(context.Context, string, string) (serverConfig, error)
	Commands(context.Context, string) ([]agent.SlashCommand, error)
	ActiveLoops(context.Context, string) ([]activeLoopInfo, error)
	ListSessions(context.Context, string) ([]agent.SessionSummary, error)
	GetSession(context.Context, string) (*agent.Session, error)
	SaveSession(context.Context, *agent.Session, *uint64) (sessionSaveResult, error)
	ForkSession(context.Context, string, uint64) (*agent.Session, error)
	ReconcileSession(context.Context, string, uint64) (*agent.Session, error)
	CloseSession(context.Context, string, uint64) (*agent.Session, error)
	DeleteSession(context.Context, string, uint64) error
	StartTurn(context.Context, agentTurnRequest) <-chan agentStreamItem
	ResolveApproval(context.Context, string, string, string) error
	CancelRun(context.Context, string) error
}

type tuiRunState string

const (
	tuiRunIdle       tuiRunState = "idle"
	tuiRunSubmitting tuiRunState = "submitting"
	tuiRunStreaming  tuiRunState = "streaming"
	tuiRunApproval   tuiRunState = "approval"
	tuiRunCancelling tuiRunState = "cancelling"
	tuiRunCompleted  tuiRunState = "completed"
	tuiRunBlocked    tuiRunState = "blocked"
	tuiRunFailed     tuiRunState = "failed"
	tuiRunUnknown    tuiRunState = "unknown"
)

type tuiConnectionState string

const (
	tuiConnecting   tuiConnectionState = "connecting"
	tuiConnected    tuiConnectionState = "connected"
	tuiAuthRequired tuiConnectionState = "auth-required"
	tuiDisconnected tuiConnectionState = "disconnected"
)

type tuiEntryKind string

const (
	tuiEntryUser      tuiEntryKind = "user"
	tuiEntryAssistant tuiEntryKind = "assistant"
	tuiEntryStatus    tuiEntryKind = "status"
	tuiEntryTool      tuiEntryKind = "tool"
	tuiEntryResult    tuiEntryKind = "result"
	tuiEntryDiff      tuiEntryKind = "diff"
	tuiEntryError     tuiEntryKind = "error"
	tuiEntrySystem    tuiEntryKind = "system"
	tuiEntryThinking  tuiEntryKind = "thinking"
)

type tuiTranscriptEntry struct {
	Kind tuiEntryKind
	Text string
	At   time.Time
}

type tuiApproval struct {
	ID            string
	RuntimeID     string
	Generation    uint64
	ToolName      string
	RedactedInput string
	DangerLevel   string
	Scope         string
	ExpiresAt     time.Time
	Resolving     bool
}

type tuiConfirmation struct {
	Action      string
	Title       string
	Description string
}

type tuiContextMeter struct {
	Estimated int
	Window    int
	Remaining int
}

type tuiModel struct {
	backend tuiBackend
	opts    tuiOptions

	width  int
	height int

	input    textinput.Model
	viewport viewport.Model

	entries            []tuiTranscriptEntry
	assistantEntry     int
	assistantText      string
	lastDiff           string
	pendingToolInput   string
	newOutputWhileAway int

	connection  tuiConnectionState
	server      serverHealth
	config      serverConfig
	root        serverRoot
	activeLoops []activeLoopInfo
	lastError   string
	lastStatus  string

	runState        tuiRunState
	runGeneration   uint64
	runStarted      time.Time
	runtimeID       string
	stream          <-chan agentStreamItem
	turnCancel      context.CancelFunc
	doneSeen        bool
	streamEndSeen   bool
	durableError    bool
	cancelRequested bool
	awaitingReload  bool
	reloadFailed    bool
	operationBusy   bool
	heartbeatChars  int64
	heartbeatMS     int64
	contextMeter    tuiContextMeter
	currentTool     string

	sessions       []agent.SessionSummary
	current        *agent.Session
	loadedMessages []chatMsg

	approval *tuiApproval
	confirm  *tuiConfirmation

	staticCommands  []tuiCommand
	dynamicCommands []tuiCommand
	paletteOpen     bool
	paletteIndex    int
	paletteMatches  []tuiCommand

	tickCount int
	quitting  bool
}

type tuiBootstrapMsg struct {
	health   serverHealth
	config   serverConfig
	root     serverRoot
	commands []agent.SlashCommand
	sessions []agent.SessionSummary
	loops    []activeLoopInfo
	session  *agent.Session
	warnings []string
	err      error
	auth     bool
}

type tuiHealthMsg struct {
	health serverHealth
	loops  []activeLoopInfo
	err    error
	auth   bool
}

type tuiTurnPreparedMsg struct {
	generation uint64
	session    *agent.Session
	stream     <-chan agentStreamItem
	cancel     context.CancelFunc
	err        error
}

type tuiStreamClosedMsg struct{}

type tuiSessionLoadedMsg struct {
	session  *agent.Session
	required bool
	err      error
}

type tuiSessionsMsg struct {
	sessions []agent.SessionSummary
	err      error
}

type tuiSessionOperationMsg struct {
	action  string
	session *agent.Session
	err     error
}

type tuiConfigMsg struct {
	config serverConfig
	err    error
}

type tuiApprovalResolvedMsg struct {
	approvalID string
	runtimeID  string
	generation uint64
	decision   string
	err        error
}

type tuiCancelMsg struct {
	runtimeID  string
	generation uint64
	err        error
}

type tuiTickMsg time.Time

func runTUI(opts tuiOptions) error {
	if strings.TrimSpace(opts.BaseURL) == "" {
		return errors.New("TUI server URL is required")
	}
	if (opts.Provider == "") != (opts.Model == "") {
		return errors.New("provider and model must be supplied together")
	}
	client := &http.Client{Timeout: 0}
	backend := newAgentStreamTransport(opts.BaseURL, opts.AccessToken, client)
	model := newTUIModel(backend, opts)
	program := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err := program.Run()
	return err
}

func newTUIModel(backend tuiBackend, opts tuiOptions) tuiModel {
	input := textinput.New()
	input.Prompt = ""
	input.Placeholder = "Ask Corelay Code or type / for commands"
	input.CharLimit = 32 << 10
	input.Width = 80
	input.Focus()

	view := viewport.New(80, 18)
	view.MouseWheelEnabled = true
	view.MouseWheelDelta = 3

	m := tuiModel{
		backend:        backend,
		opts:           opts,
		input:          input,
		viewport:       view,
		assistantEntry: -1,
		connection:     tuiConnecting,
		runState:       tuiRunIdle,
		operationBusy:  true,
		staticCommands: defaultTUICommands(),
	}
	m.appendEntry(tuiEntrySystem, "Connecting to Corelay Code…")
	return m
}

func (m tuiModel) Init() tea.Cmd {
	return tea.Batch(
		m.bootstrapCmd(),
		m.input.Focus(),
		tea.WindowSize(),
		tuiTickCmd(),
	)
}

func (m tuiModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.resize(msg.Width, msg.Height)
		return m, nil

	case tuiTickMsg:
		m.tickCount++
		commands := []tea.Cmd{tuiTickCmd()}
		if runtime.GOOS == "windows" {
			commands = append(commands, tea.WindowSize())
		}
		if m.tickCount%tuiHealthPollEvery == 0 {
			commands = append(commands, m.healthCmd())
		}
		if m.approval != nil && !m.approval.ExpiresAt.IsZero() && time.Now().After(m.approval.ExpiresAt) && !m.approval.Resolving {
			m.appendEntry(tuiEntryError, "Permission request expired; nothing was approved.")
			m.approval = nil
			m.runState = tuiRunCancelling
			commands = append(commands, m.cancelRunCmd())
		}
		return m, tea.Batch(commands...)

	case tuiBootstrapMsg:
		m.operationBusy = false
		m.applyBootstrap(msg)
		return m, nil

	case tuiHealthMsg:
		if msg.err != nil {
			if msg.auth {
				m.connection = tuiAuthRequired
			} else {
				m.connection = tuiDisconnected
			}
			m.lastError = safeTUIText(msg.err.Error(), 1024)
		} else {
			m.connection = tuiConnected
			m.server = msg.health
			m.activeLoops = append([]activeLoopInfo(nil), msg.loops...)
			m.lastError = ""
		}
		return m, nil

	case tuiTurnPreparedMsg:
		if msg.generation != m.runGeneration || m.cancelRequested ||
			m.runState == tuiRunCancelling || m.runState != tuiRunSubmitting {
			if msg.cancel != nil {
				msg.cancel()
			}
			return m, nil
		}
		if msg.err != nil {
			if msg.cancel != nil {
				msg.cancel()
			}
			m.turnCancel = nil
			message := "Turn could not start: " + safeTUIText(msg.err.Error(), 2048)
			if isTUIHTTPStatus(msg.err, http.StatusConflict) {
				m.reloadFailed = true
				message += ". The session revision changed; use /load <id> or /new before sending again."
			}
			m.finishRun(tuiRunFailed, message)
			return m, nil
		}
		if msg.session == nil || msg.stream == nil || msg.cancel == nil {
			if msg.cancel != nil {
				msg.cancel()
			}
			m.turnCancel = nil
			m.finishRun(tuiRunFailed, "Turn could not start: preparation returned incomplete state")
			return m, nil
		}
		m.current = cloneTUISession(msg.session)
		m.loadedMessages = wireMessagesFromSession(msg.session.Messages)
		m.stream = msg.stream
		m.turnCancel = msg.cancel
		m.runState = tuiRunStreaming
		m.lastStatus = "Waiting for the first model event"
		return m, waitForTUIStream(msg.stream)

	case agentStreamItem:
		commands := m.applyStreamItem(msg)
		if m.stream != nil {
			commands = append(commands, waitForTUIStream(m.stream))
		}
		return m, tea.Batch(commands...)

	case tuiStreamClosedMsg:
		m.stream = nil
		if !m.doneSeen {
			m.finishRun(tuiRunUnknown, "Agent stream closed without a typed terminal event. Reload the session before continuing.")
		} else if m.runState == tuiRunStreaming || m.runState == tuiRunCancelling || m.runState == tuiRunApproval {
			m.finishRun(tuiRunCompleted, "Run completed")
		}
		if m.current != nil {
			m.awaitingReload = true
			m.lastStatus = "Synchronizing the durable session"
			return m, m.loadSessionAfterRunCmd(m.current.ID)
		}
		return m, nil

	case tuiSessionLoadedMsg:
		m.operationBusy = false
		if msg.required {
			m.awaitingReload = false
		}
		if msg.err != nil {
			if msg.required {
				m.reloadFailed = true
			}
			m.appendEntry(tuiEntryError, "Session load failed: "+safeTUIText(msg.err.Error(), 2048))
			return m, nil
		}
		if msg.required {
			m.syncSessionAfterRun(msg.session)
		} else {
			m.loadSession(msg.session)
		}
		return m, nil

	case tuiSessionsMsg:
		m.operationBusy = false
		if msg.err != nil {
			m.appendEntry(tuiEntryError, "Session list failed: "+safeTUIText(msg.err.Error(), 2048))
			return m, nil
		}
		m.sessions = append([]agent.SessionSummary(nil), msg.sessions...)
		m.showSessionList()
		return m, nil

	case tuiSessionOperationMsg:
		m.operationBusy = false
		m.applySessionOperation(msg)
		m.operationBusy = true
		m.lastStatus = "Refreshing durable sessions"
		return m, m.listSessionsCmd()

	case tuiConfigMsg:
		m.operationBusy = false
		if msg.err != nil {
			m.appendEntry(tuiEntryError, "Model switch failed: "+safeTUIText(msg.err.Error(), 2048))
		} else {
			m.config = msg.config
			m.appendEntry(tuiEntrySystem, fmt.Sprintf("Server target changed to %s/%s", msg.config.Provider, msg.config.Model))
		}
		return m, nil

	case tuiApprovalResolvedMsg:
		if m.approval == nil || msg.generation != m.runGeneration ||
			msg.approvalID != m.approval.ID || msg.runtimeID != m.runtimeID {
			return m, nil
		}
		if msg.err != nil {
			m.appendEntry(tuiEntryError, "Permission decision was not recorded; cancelling the run.")
			m.approval = nil
			m.runState = tuiRunCancelling
			return m, m.cancelRunCmd()
		}
		decision := msg.decision
		m.approval = nil
		m.runState = tuiRunStreaming
		if decision == "allow_once" {
			m.appendEntry(tuiEntryStatus, "Permission granted once")
		} else {
			m.appendEntry(tuiEntryStatus, "Permission denied")
		}
		return m, nil

	case tuiCancelMsg:
		if msg.generation != m.runGeneration || (msg.runtimeID != "" && msg.runtimeID != m.runtimeID) {
			return m, nil
		}
		if msg.runtimeID == "" && m.runState == tuiRunCancelling {
			m.runState = tuiRunFailed
			m.lastStatus = "Run cancelled before startup completed"
			m.appendEntry(tuiEntryStatus, m.lastStatus)
			return m, nil
		}
		if msg.err != nil && m.turnCancel != nil {
			m.turnCancel()
			m.turnCancel = nil
			m.appendEntry(tuiEntryError, "Server cancellation failed; the local stream was closed.")
		} else {
			m.appendEntry(tuiEntryStatus, "Cancellation requested")
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		if m.securityReviewRestricted() {
			return m, nil
		}
		beforeBottom := m.viewport.AtBottom()
		updated, cmd := m.viewport.Update(msg)
		m.viewport = updated
		if beforeBottom && !m.viewport.AtBottom() {
			m.newOutputWhileAway = 0
		}
		return m, cmd
	}

	return m, nil
}

func (m tuiModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if m.securityReviewRestricted() {
		if m.approval != nil {
			switch key {
			case "d", "D", "esc", "enter":
				if !m.approval.Resolving {
					m.approval.Resolving = true
					return m, m.resolveApprovalCmd("deny")
				}
			case "ctrl+c":
				m.approval.Resolving = true
				m.runState = tuiRunCancelling
				return m, m.cancelRunCmd()
			case "ctrl+q":
				m.approval.Resolving = true
				m.runState = tuiRunCancelling
				m.quitting = true
				return m, tea.Sequence(m.cancelRunCmd(), tea.Quit)
			}
			return m, nil
		}
		if m.confirm != nil {
			switch key {
			case "n", "N", "esc", "enter", "ctrl+c":
				m.appendEntry(tuiEntryStatus, "Action cancelled")
				m.confirm = nil
			case "ctrl+q":
				m.confirm = nil
				m.quitting = true
				return m, tea.Quit
			}
			return m, nil
		}
	}
	if m.approval != nil {
		switch key {
		case "a", "A":
			if !m.approval.Resolving && (m.approval.ExpiresAt.IsZero() || time.Now().Before(m.approval.ExpiresAt)) {
				m.approval.Resolving = true
				return m, m.resolveApprovalCmd("allow_once")
			}
		case "d", "D", "esc", "enter":
			if !m.approval.Resolving {
				m.approval.Resolving = true
				return m, m.resolveApprovalCmd("deny")
			}
		case "ctrl+c", "ctrl+q":
			m.approval.Resolving = true
			m.runState = tuiRunCancelling
			return m, m.cancelRunCmd()
		}
		return m, nil
	}

	if m.confirm != nil {
		switch key {
		case "y", "Y":
			confirmation := *m.confirm
			m.confirm = nil
			m.operationBusy = true
			m.lastStatus = "Applying session " + confirmation.Action
			return m, m.executeConfirmed(confirmation)
		case "n", "N", "esc", "enter", "ctrl+c":
			m.appendEntry(tuiEntryStatus, "Action cancelled")
			m.confirm = nil
		}
		return m, nil
	}

	if m.awaitingReload || m.operationBusy {
		if key == "ctrl+q" {
			m.quitting = true
			return m, tea.Quit
		}
		if m.runActive() && (key == "ctrl+c" || key == "esc") {
			m.runState = tuiRunCancelling
			return m, m.cancelRunCmd()
		}
		return m, nil
	}

	if m.paletteOpen {
		switch key {
		case "esc":
			m.paletteOpen = false
			m.paletteIndex = 0
			return m, nil
		case "up":
			if len(m.paletteMatches) > 0 {
				m.paletteIndex = (m.paletteIndex - 1 + len(m.paletteMatches)) % len(m.paletteMatches)
			}
			return m, nil
		case "down":
			if len(m.paletteMatches) > 0 {
				m.paletteIndex = (m.paletteIndex + 1) % len(m.paletteMatches)
			}
			return m, nil
		case "tab":
			m.completePaletteSelection()
			return m, nil
		case "enter":
			if m.choosePaletteSelection() {
				return m.submitInput()
			}
			return m, nil
		}
	}

	switch key {
	case "ctrl+q":
		m.stopActiveTurn()
		m.quitting = true
		return m, tea.Quit
	case "ctrl+c":
		if m.runActive() {
			m.runState = tuiRunCancelling
			return m, m.cancelRunCmd()
		}
		m.quitting = true
		return m, tea.Quit
	case "esc":
		if m.runActive() {
			m.runState = tuiRunCancelling
			return m, m.cancelRunCmd()
		}
	case "ctrl+k":
		m.openPalette()
		return m, nil
	case "ctrl+n":
		if !m.runActive() {
			m.newSession()
		}
		return m, nil
	case "ctrl+o":
		if !m.runActive() {
			m.input.SetValue("/sessions")
			return m.submitInput()
		}
	case "pgup":
		m.viewport.ViewUp()
		return m, nil
	case "pgdown":
		m.viewport.ViewDown()
		if m.viewport.AtBottom() {
			m.newOutputWhileAway = 0
		}
		return m, nil
	case "ctrl+home":
		m.viewport.GotoTop()
		return m, nil
	case "ctrl+end", "end":
		m.viewport.GotoBottom()
		m.newOutputWhileAway = 0
		return m, nil
	case "enter":
		if !m.runActive() {
			return m.submitInput()
		}
		return m, nil
	}

	if m.runActive() {
		return m, nil
	}
	updated, cmd := m.input.Update(msg)
	m.input = updated
	value := strings.TrimSpace(m.input.Value())
	if strings.HasPrefix(value, "/") {
		m.openPalette()
	} else {
		m.paletteOpen = false
		m.paletteMatches = nil
		m.paletteIndex = 0
	}
	return m, cmd
}

func (m *tuiModel) applyBootstrap(msg tuiBootstrapMsg) {
	if msg.err != nil {
		if msg.auth {
			m.connection = tuiAuthRequired
			m.lastError = "Access token required. Restart with CORELAY_ACCESS_TOKEN or -token."
		} else {
			m.connection = tuiDisconnected
			m.lastError = safeTUIText(msg.err.Error(), 1024)
		}
		m.appendEntry(tuiEntryError, m.lastError)
		return
	}
	m.connection = tuiConnected
	m.server = msg.health
	m.config = msg.config
	m.root = msg.root
	m.activeLoops = append([]activeLoopInfo(nil), msg.loops...)
	m.sessions = append([]agent.SessionSummary(nil), msg.sessions...)
	m.dynamicCommands = skillTUICommands(msg.commands)
	if msg.session != nil {
		m.loadSession(msg.session)
	} else {
		m.entries = nil
		m.appendEntry(tuiEntrySystem, fmt.Sprintf("Connected to %s · target %s/%s", m.opts.BaseURL, m.config.Provider, m.config.Model))
	}
	for _, warning := range msg.warnings {
		m.appendEntry(tuiEntryError, safeTUIText(warning, 2048))
	}
}

func (m *tuiModel) applyStreamItem(item agentStreamItem) []tea.Cmd {
	if item.Err != nil {
		m.appendEntry(tuiEntryError, "Stream error: "+safeTUIText(item.Err.Error(), 2048))
		if !m.doneSeen {
			m.runState = tuiRunUnknown
		}
		return nil
	}
	if item.EOF {
		m.stream = nil
		if m.current != nil {
			m.awaitingReload = true
		}
		return []tea.Cmd{func() tea.Msg { return tuiStreamClosedMsg{} }}
	}
	return m.applyWireEvent(item.Event)
}

func (m *tuiModel) applyWireEvent(event agentWireEvent) []tea.Cmd {
	commands := []tea.Cmd{}
	if event.Type != "text" {
		m.closeAssistantSegment()
	}
	switch event.Type {
	case "heartbeat":
		var payload struct {
			ElapsedMS int64 `json:"elapsedMs"`
			Chars     int64 `json:"chars"`
		}
		if json.Unmarshal(event.Data, &payload) == nil {
			m.heartbeatMS = payload.ElapsedMS
			m.heartbeatChars = payload.Chars
		}
	case "context_plan":
		var payload struct {
			Estimated int `json:"estimatedInputTokens"`
			Window    int `json:"contextWindowTokens"`
			Remaining int `json:"remainingTokens"`
		}
		if json.Unmarshal(event.Data, &payload) == nil {
			m.contextMeter = tuiContextMeter{Estimated: payload.Estimated, Window: payload.Window, Remaining: payload.Remaining}
		}
	case "text":
		var text string
		if json.Unmarshal(event.Data, &text) == nil {
			m.appendAssistant(text)
		}
	case "thinking":
		if m.opts.ShowThinking {
			var text string
			if json.Unmarshal(event.Data, &text) == nil {
				m.appendEntry(tuiEntryThinking, text)
			}
		}
	case "status":
		var text string
		if json.Unmarshal(event.Data, &text) == nil {
			text = safeTUIText(text, 4096)
			m.lastStatus = text
			if !m.opts.Quiet {
				m.appendEntry(tuiEntryStatus, text)
			}
		}
	case "tool_start":
		m.flushPendingToolInput()
		var payload struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(event.Data, &payload) == nil {
			m.currentTool = safeTUIText(payload.Name, 128)
			m.appendEntry(tuiEntryTool, "Running "+m.currentTool)
		}
	case "tool_input":
		var payload struct {
			Input any `json:"input"`
		}
		if json.Unmarshal(event.Data, &payload) == nil {
			if raw, err := json.Marshal(payload.Input); err == nil {
				m.pendingToolInput = safeTUIText(string(raw), 2048)
			}
		}
	case "tool_result":
		m.flushPendingToolInput()
		var payload struct {
			Name    string `json:"name"`
			Result  string `json:"result"`
			IsError bool   `json:"isError"`
		}
		if json.Unmarshal(event.Data, &payload) == nil {
			kind := tuiEntryResult
			prefix := "Completed"
			if payload.IsError {
				kind = tuiEntryError
				prefix = "Failed"
			}
			m.appendEntry(kind, fmt.Sprintf("%s %s · %s", prefix, safeTUIText(payload.Name, 128), safeTUIText(payload.Result, 8192)))
			m.currentTool = ""
		}
	case "diff":
		var payload struct {
			File string `json:"file"`
			Diff string `json:"diff"`
		}
		if json.Unmarshal(event.Data, &payload) == nil {
			m.lastDiff = safeTUIText(payload.Diff, 64<<10)
			m.appendEntry(tuiEntryDiff, safeTUIText(payload.File, 512)+"\n"+m.lastDiff)
		}
	case "error":
		m.pendingToolInput = ""
		var text string
		if json.Unmarshal(event.Data, &text) == nil {
			m.appendEntry(tuiEntryError, text)
		}
	case "context_blocked":
		m.pendingToolInput = ""
		m.runState = tuiRunBlocked
		m.appendEntry(tuiEntryError, "Context planning blocked this run")
	case "session":
		var payload struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(event.Data, &payload) == nil {
			m.runtimeID = strings.TrimSpace(payload.SessionID)
		}
	case "approval_required":
		m.pendingToolInput = ""
		if m.cancelRequested || m.runState == tuiRunCancelling {
			m.appendEntry(tuiEntryStatus, "Permission request ignored because cancellation is already in progress")
			break
		}
		var payload struct {
			ID            string `json:"id"`
			SessionID     string `json:"sessionId"`
			ToolName      string `json:"toolName"`
			RedactedInput string `json:"redactedInput"`
			DangerLevel   string `json:"dangerLevel"`
			Scope         string `json:"scope"`
			ExpiresAt     string `json:"expiresAt"`
		}
		if json.Unmarshal(event.Data, &payload) != nil || strings.TrimSpace(payload.ID) == "" ||
			strings.TrimSpace(payload.ToolName) == "" || m.runtimeID == "" ||
			(payload.SessionID != "" && payload.SessionID != m.runtimeID) {
			m.appendEntry(tuiEntryError, "Invalid permission request; cancelling without approval")
			m.runState = tuiRunCancelling
			commands = append(commands, m.cancelRunCmd())
			break
		}
		expiry, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(payload.ExpiresAt))
		if err != nil || !expiry.After(time.Now()) {
			m.approval = nil
			m.appendEntry(tuiEntryError, "Permission request had a missing, invalid, or expired deadline; cancelling without approval")
			m.runState = tuiRunCancelling
			commands = append(commands, m.cancelRunCmd())
			break
		}
		m.approval = &tuiApproval{
			ID:            payload.ID,
			RuntimeID:     m.runtimeID,
			Generation:    m.runGeneration,
			ToolName:      safeTUIText(payload.ToolName, 128),
			RedactedInput: safeTUIText(payload.RedactedInput, 2048),
			DangerLevel:   safeTUIText(payload.DangerLevel, 64),
			Scope:         safeTUIText(payload.Scope, 256),
			ExpiresAt:     expiry,
		}
		m.runState = tuiRunApproval
	case "durable_session":
		var payload struct {
			SessionID         string                       `json:"sessionId"`
			Revision          uint64                       `json:"revision"`
			LifecycleStatus   agent.SessionLifecycleStatus `json:"lifecycleStatus"`
			ReconcileRequired bool                         `json:"reconcileRequired"`
		}
		if json.Unmarshal(event.Data, &payload) == nil && m.current != nil && payload.SessionID == m.current.ID {
			m.current.Revision = payload.Revision
			m.current.LifecycleStatus = payload.LifecycleStatus
			m.current.ReconcileRequired = payload.ReconcileRequired
		}
	case "durable_session_error":
		m.durableError = true
		m.appendEntry(tuiEntryError, "Durable session could not be committed; reload before continuing")
		m.runState = tuiRunBlocked
	case "done":
		m.pendingToolInput = ""
		m.doneSeen = true
		m.applyTerminal(event.Data)
	case "stream_end":
		m.pendingToolInput = ""
		m.streamEndSeen = true
		if !m.doneSeen {
			m.runState = tuiRunUnknown
			m.appendEntry(tuiEntryError, "Stream ended without a typed terminal event")
		}
	case "command":
		m.pendingToolInput = ""
	}
	return commands
}

func (m *tuiModel) applyTerminal(raw json.RawMessage) {
	var value any
	if len(raw) > 0 && string(raw) != "null" {
		_ = json.Unmarshal(raw, &value)
	}
	metadata, typed := agent.DecodeDurableRunTerminalMetadata(value)
	var envelope struct {
		Kind       string `json:"kind"`
		StopReason string `json:"stopReason"`
	}
	_ = json.Unmarshal(raw, &envelope)

	switch {
	case m.durableError:
		m.runState = tuiRunBlocked
		m.appendEntry(tuiEntryError, "Run output was not committed to the durable session")
	case typed && metadata.BlocksSuccess():
		m.runState = tuiRunBlocked
		m.appendEntry(tuiEntryError, "Run completed with a blocked or incomplete terminal state")
	case envelope.Kind == string(agent.RunTerminalCancelled):
		m.runState = tuiRunFailed
		m.appendEntry(tuiEntryStatus, "Run cancelled")
	case envelope.Kind != "" && envelope.Kind != string(agent.RunTerminalCompleted) && envelope.Kind != string(agent.RunTerminalCommand):
		m.runState = tuiRunFailed
		message := "Run ended: " + envelope.Kind
		if envelope.StopReason != "" {
			message += " · " + safeTUIText(envelope.StopReason, 512)
		}
		m.appendEntry(tuiEntryError, message)
	default:
		m.runState = tuiRunCompleted
		m.lastStatus = "Run completed"
	}
	if m.turnCancel != nil {
		m.turnCancel = nil
	}
	if m.approval != nil {
		m.approval = nil
	}
}

func (m *tuiModel) appendEntry(kind tuiEntryKind, text string) {
	text = safeTUIText(text, tuiEntryBytes)
	if strings.TrimSpace(text) == "" {
		return
	}
	if kind != tuiEntryAssistant {
		m.closeAssistantSegment()
	}
	wasBottom := m.viewport.AtBottom()
	m.entries = append(m.entries, tuiTranscriptEntry{Kind: kind, Text: text, At: time.Now()})
	if len(m.entries) > tuiTranscriptLimit {
		drop := len(m.entries) - tuiTranscriptLimit
		m.entries = append([]tuiTranscriptEntry(nil), m.entries[drop:]...)
		if m.assistantEntry >= 0 {
			m.assistantEntry -= drop
			if m.assistantEntry < 0 {
				m.assistantEntry = -1
			}
		}
	}
	m.refreshTranscript(wasBottom)
}

func (m *tuiModel) appendAssistant(chunk string) {
	chunk = safeTUIText(chunk, tuiEntryBytes)
	if chunk == "" {
		return
	}
	wasBottom := m.viewport.AtBottom()
	if m.assistantEntry < 0 || m.assistantEntry >= len(m.entries) {
		m.entries = append(m.entries, tuiTranscriptEntry{Kind: tuiEntryAssistant, At: time.Now()})
		m.assistantEntry = len(m.entries) - 1
	}
	m.assistantText = appendBoundedUTF8(m.assistantText, chunk, tuiAssistantBytes)
	m.entries[m.assistantEntry].Text = m.assistantText
	m.refreshTranscript(wasBottom)
}

func (m *tuiModel) closeAssistantSegment() {
	m.assistantEntry = -1
	m.assistantText = ""
}

func (m *tuiModel) flushPendingToolInput() {
	if m.pendingToolInput == "" {
		return
	}
	// Raw input is held until execution produces a result. If an approval event
	// intervenes this buffer is discarded and only redactedInput is rendered.
	m.appendEntry(tuiEntryStatus, "Input: "+m.pendingToolInput)
	m.pendingToolInput = ""
}

func (m *tuiModel) refreshTranscript(wasBottom bool) {
	m.viewport.SetContent(m.renderTranscript())
	if wasBottom {
		m.viewport.GotoBottom()
		m.newOutputWhileAway = 0
	} else {
		m.newOutputWhileAway++
	}
}

func (m *tuiModel) finishRun(state tuiRunState, message string) {
	m.runState = state
	m.lastStatus = message
	if state == tuiRunFailed || state == tuiRunUnknown || state == tuiRunBlocked {
		m.appendEntry(tuiEntryError, message)
	}
	m.stopActiveTurn()
}

func (m *tuiModel) stopActiveTurn() {
	if m.turnCancel != nil {
		m.turnCancel()
		m.turnCancel = nil
	}
	m.stream = nil
	m.approval = nil
}

func (m tuiModel) runActive() bool {
	if m.stream != nil {
		return true
	}
	switch m.runState {
	case tuiRunSubmitting, tuiRunStreaming, tuiRunApproval, tuiRunCancelling:
		return true
	default:
		return false
	}
}

func (m tuiModel) sessionWritable() bool {
	if m.awaitingReload || m.reloadFailed {
		return false
	}
	if m.current == nil {
		return true
	}
	if m.current.ReconcileRequired {
		return false
	}
	return m.current.LifecycleStatus == "" || m.current.LifecycleStatus == agent.SessionLifecycleActive
}

func (m *tuiModel) resize(width, height int) {
	if width <= 0 || height <= 0 {
		return
	}
	wasBottom := m.viewport.AtBottom()
	oldOffset := m.viewport.YOffset
	m.width, m.height = width, height
	geometry := calculateTUIGeometry(width, height, m.overlayHeight())
	m.viewport.Width = geometry.TranscriptContentWidth
	m.viewport.Height = geometry.TranscriptContentHeight
	m.input.Width = maxInt(8, geometry.InputContentWidth)
	m.viewport.SetContent(m.renderTranscript())
	if wasBottom {
		m.viewport.GotoBottom()
	} else {
		m.viewport.SetYOffset(oldOffset)
	}
}

func (m *tuiModel) loadSession(session *agent.Session) {
	if session == nil {
		return
	}
	m.current = cloneTUISession(session)
	m.awaitingReload = false
	m.reloadFailed = false
	m.loadedMessages = wireMessagesFromSession(session.Messages)
	m.entries = nil
	m.assistantEntry = -1
	m.assistantText = ""
	for _, message := range session.Messages {
		kind := tuiEntrySystem
		switch message.Role {
		case "user":
			kind = tuiEntryUser
		case "assistant":
			kind = tuiEntryAssistant
		case "tool":
			kind = tuiEntryResult
		}
		m.entries = append(m.entries, tuiTranscriptEntry{Kind: kind, Text: safeTUIText(message.Content, tuiEntryBytes), At: message.Timestamp})
	}
	m.appendEntry(tuiEntrySystem, fmt.Sprintf("Loaded session %s · revision %d", session.ID, session.Revision))
	if session.ReconcileRequired {
		m.appendEntry(tuiEntryError, "This session has an ambiguous side effect. Review and run /reconcile before sending another message.")
	}
	m.refreshTranscript(true)
}

func (m *tuiModel) syncSessionAfterRun(session *agent.Session) {
	if session == nil {
		m.reloadFailed = true
		m.appendEntry(tuiEntryError, "Durable session synchronization returned no session")
		return
	}
	m.current = cloneTUISession(session)
	m.loadedMessages = wireMessagesFromSession(session.Messages)
	m.awaitingReload = false
	m.reloadFailed = false
	if session.LastRunTerminal != nil && session.LastRunTerminal.BlocksSuccess() {
		m.runState = tuiRunBlocked
	}
	m.lastStatus = fmt.Sprintf("Durable session synchronized at revision %d", session.Revision)
}

func (m *tuiModel) newSession() {
	m.current = nil
	m.loadedMessages = nil
	m.entries = nil
	m.assistantEntry = -1
	m.assistantText = ""
	m.lastDiff = ""
	m.runState = tuiRunIdle
	m.awaitingReload = false
	m.reloadFailed = false
	m.appendEntry(tuiEntrySystem, "New durable session. The first message will create it.")
	m.input.Reset()
	m.input.Focus()
}

func (m *tuiModel) showSessionList() {
	if len(m.sessions) == 0 {
		m.appendEntry(tuiEntrySystem, "No saved sessions for this workspace")
		return
	}
	items := append([]agent.SessionSummary(nil), m.sessions...)
	sort.SliceStable(items, func(i, j int) bool { return items[i].UpdatedAt.After(items[j].UpdatedAt) })
	var builder strings.Builder
	builder.WriteString("Saved sessions\n")
	for _, session := range items {
		state := string(session.LifecycleStatus)
		if state == "" {
			state = "active"
		}
		fmt.Fprintf(&builder, "  %s  r%d  %-12s  %s\n", session.ID, session.Revision, state, safeTUIText(session.Title, 72))
	}
	builder.WriteString("Use /load <id> to open one.")
	m.appendEntry(tuiEntrySystem, strings.TrimRight(builder.String(), "\n"))
}

func (m *tuiModel) applySessionOperation(msg tuiSessionOperationMsg) {
	if msg.err != nil {
		message := fmt.Sprintf("%s failed: %s", msg.action, safeTUIText(msg.err.Error(), 2048))
		if isTUIHTTPStatus(msg.err, http.StatusConflict) {
			m.reloadFailed = true
			message += ". Reload the session before retrying or forking."
		}
		m.appendEntry(tuiEntryError, message)
		return
	}
	switch msg.action {
	case "delete":
		m.newSession()
		m.appendEntry(tuiEntrySystem, "Session deleted")
	default:
		if msg.session != nil {
			m.loadSession(msg.session)
			m.appendEntry(tuiEntrySystem, strings.Title(msg.action)+" completed")
		}
	}
}

func (m tuiModel) bootstrapCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		health, err := m.backend.Health(ctx)
		if err != nil {
			return tuiBootstrapMsg{err: err}
		}
		config, err := m.backend.Config(ctx)
		if err != nil {
			return tuiBootstrapMsg{health: health, err: err, auth: isUnauthorizedTUIError(err)}
		}
		var (
			loops       []activeLoopInfo
			loopsLoaded bool
		)
		if m.opts.Provider != "" && m.opts.Model != "" {
			loops, err = m.backend.ActiveLoops(ctx, "")
			if err != nil {
				return tuiBootstrapMsg{health: health, config: config, err: fmt.Errorf("verify active runs before model switch: %w", err)}
			}
			loopsLoaded = true
			if len(loops) > 0 {
				return tuiBootstrapMsg{health: health, config: config, err: errors.New("model switch is blocked while the server has active runs")}
			}
			config, err = m.backend.SetConfig(ctx, m.opts.Provider, m.opts.Model)
			if err != nil {
				return tuiBootstrapMsg{health: health, err: err, auth: isUnauthorizedTUIError(err)}
			}
		}
		root, rootErr := m.backend.Root(ctx)
		warnings := []string{}
		if rootErr != nil {
			root = serverRoot{Name: "corelaycode", Version: "unknown", Provider: config.Provider, Model: config.Model}
			warnings = append(warnings, "Server version discovery failed: "+rootErr.Error())
		}
		commands, commandsErr := m.backend.Commands(ctx, m.opts.WorkDir)
		if commandsErr != nil {
			warnings = append(warnings, "Workspace command discovery failed: "+commandsErr.Error())
		}
		sessions, sessionsErr := m.backend.ListSessions(ctx, m.opts.WorkDir)
		if sessionsErr != nil {
			warnings = append(warnings, "Durable session discovery failed: "+sessionsErr.Error())
		}
		if !loopsLoaded {
			var loopsErr error
			loops, loopsErr = m.backend.ActiveLoops(ctx, "")
			if loopsErr != nil {
				warnings = append(warnings, "Active run discovery failed: "+loopsErr.Error())
			}
		}
		var loaded *agent.Session
		if m.opts.SessionID != "" {
			loaded, err = m.backend.GetSession(ctx, m.opts.SessionID)
			if err != nil {
				return tuiBootstrapMsg{health: health, config: config, root: root, commands: commands, sessions: sessions, loops: loops, err: err}
			}
		}
		return tuiBootstrapMsg{
			health: health, config: config, root: root, commands: commands,
			sessions: sessions, loops: loops, session: loaded, warnings: warnings,
		}
	}
}

func (m tuiModel) healthCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		health, err := m.backend.Health(ctx)
		if err != nil {
			return tuiHealthMsg{err: err}
		}
		_, configErr := m.backend.Config(ctx)
		if configErr != nil {
			return tuiHealthMsg{health: health, err: configErr, auth: isUnauthorizedTUIError(configErr)}
		}
		loops, _ := m.backend.ActiveLoops(ctx, "")
		return tuiHealthMsg{health: health, loops: loops}
	}
}

func (m tuiModel) listSessionsCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		sessions, err := m.backend.ListSessions(ctx, m.opts.WorkDir)
		return tuiSessionsMsg{sessions: sessions, err: err}
	}
}

func (m tuiModel) loadSessionCmd(id string) tea.Cmd {
	return m.loadSessionCmdWithRequirement(id, false)
}

func (m tuiModel) loadSessionAfterRunCmd(id string) tea.Cmd {
	return m.loadSessionCmdWithRequirement(id, true)
}

func (m tuiModel) loadSessionCmdWithRequirement(id string, required bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		session, err := m.backend.GetSession(ctx, id)
		return tuiSessionLoadedMsg{session: session, required: required, err: err}
	}
}

func (m tuiModel) resolveApprovalCmd(decision string) tea.Cmd {
	approval := *m.approval
	return func() tea.Msg {
		deadline := time.Now().Add(15 * time.Second)
		if !approval.ExpiresAt.IsZero() && approval.ExpiresAt.Before(deadline) {
			deadline = approval.ExpiresAt
		}
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		defer cancel()
		err := m.backend.ResolveApproval(ctx, approval.ID, approval.RuntimeID, decision)
		return tuiApprovalResolvedMsg{
			approvalID: approval.ID,
			runtimeID:  approval.RuntimeID,
			generation: approval.Generation,
			decision:   decision,
			err:        err,
		}
	}
}

func (m *tuiModel) cancelRunCmd() tea.Cmd {
	if m.cancelRequested {
		return nil
	}
	m.cancelRequested = true
	runtimeID := m.runtimeID
	generation := m.runGeneration
	if runtimeID == "" {
		if m.turnCancel != nil {
			m.turnCancel()
			m.turnCancel = nil
		}
		return func() tea.Msg {
			return tuiCancelMsg{generation: generation}
		}
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := m.backend.CancelRun(ctx, runtimeID)
		return tuiCancelMsg{runtimeID: runtimeID, generation: generation, err: err}
	}
}

func waitForTUIStream(stream <-chan agentStreamItem) tea.Cmd {
	return func() tea.Msg {
		item, ok := <-stream
		if !ok {
			return tuiStreamClosedMsg{}
		}
		return item
	}
}

func tuiTickCmd() tea.Cmd {
	return tea.Tick(tuiTerminalPollEvery, func(at time.Time) tea.Msg { return tuiTickMsg(at) })
}

func wireMessagesFromSession(messages []agent.SessionMessage) []chatMsg {
	result := make([]chatMsg, 0, len(messages))
	for _, message := range messages {
		if message.Role != "user" && message.Role != "assistant" {
			continue
		}
		result = append(result, chatMsg{Role: message.Role, Content: message.Content})
	}
	return result
}

func cloneTUISession(session *agent.Session) *agent.Session {
	if session == nil {
		return nil
	}
	encoded, _ := json.Marshal(session)
	var clone agent.Session
	if json.Unmarshal(encoded, &clone) != nil {
		copySession := *session
		return &copySession
	}
	return &clone
}

func appendBoundedUTF8(current, addition string, maxBytes int) string {
	if maxBytes <= 0 || len(current) >= maxBytes {
		return current
	}
	remaining := maxBytes - len(current)
	if len(addition) <= remaining {
		return current + addition
	}
	addition = addition[:remaining]
	for !utf8.ValidString(addition) && len(addition) > 0 {
		addition = addition[:len(addition)-1]
	}
	return current + addition + "\n[output clipped]"
}

func safeTUIText(value string, maxBytes int) string {
	value = strings.ToValidUTF8(value, "")
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = stripTerminalControls(value)
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	clipped := value[:maxBytes]
	for !utf8.ValidString(clipped) && len(clipped) > 0 {
		clipped = clipped[:len(clipped)-1]
	}
	return clipped + "…"
}

func stripTerminalControls(value string) string {
	var output strings.Builder
	for i := 0; i < len(value); {
		b := value[i]
		if b == 0x1b {
			i++
			if i >= len(value) {
				break
			}
			switch value[i] {
			case '[':
				i++
				for i < len(value) {
					terminal := value[i] >= 0x40 && value[i] <= 0x7e
					i++
					if terminal {
						break
					}
				}
			case ']', 'P', '_', '^', 'X':
				i++
				for i < len(value) {
					if value[i] == 0x07 {
						i++
						break
					}
					if value[i] == 0x1b && i+1 < len(value) && value[i+1] == '\\' {
						i += 2
						break
					}
					i++
				}
			default:
				i++
			}
			continue
		}
		if b == '\r' {
			output.WriteByte('\n')
			i++
			continue
		}
		if (b < 0x20 && b != '\n' && b != '\t') || b == 0x7f {
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(value[i:])
		if size == 0 {
			break
		}
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			i += size
			continue
		}
		output.WriteString(value[i : i+size])
		i += size
	}
	return output.String()
}

func isUnauthorizedTUIError(err error) bool {
	return isTUIHTTPStatus(err, http.StatusUnauthorized)
}

func isTUIHTTPStatus(err error, status int) bool {
	if err == nil {
		return false
	}
	var httpErr *agentHTTPError
	return errors.As(err, &httpErr) && httpErr.StatusCode == status
}

func terminalSupportsTUI() bool {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("TERM")), "dumb") {
		return false
	}
	stdin, stdinErr := os.Stdin.Stat()
	stdout, stdoutErr := os.Stdout.Stat()
	return stdinErr == nil && stdoutErr == nil && stdin.Mode()&os.ModeCharDevice != 0 && stdout.Mode()&os.ModeCharDevice != 0
}

func defaultTUIWorkDir(explicit string) string {
	if strings.TrimSpace(explicit) != "" {
		if absolute, err := filepath.Abs(explicit); err == nil {
			return absolute
		}
		return explicit
	}
	working, err := os.Getwd()
	if err != nil {
		return "."
	}
	return working
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}
