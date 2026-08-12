package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/Dannykkh/corelay-code/internal/agent"
)

// runChat is Corelay Code's built-in terminal client. It connects to a running
// Corelay Code server's /api/agent endpoint and renders the streamed agent loop in
// the terminal — a CLI experience that needs no external tool (claude/codex)
// and makes no outbound internet call, so it works inside an air-gapped network
// where those proprietary CLIs cannot be installed.
//
//	corelaycode chat                 # full-screen TUI against http://localhost:4000
//	corelaycode chat -p "fix the bug" # one-shot
//	corelaycode chat -url http://host:4000 -workdir /path/to/project
func runChat(args []string) {
	fs := flag.NewFlagSet("chat", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage:")
		fmt.Fprintln(fs.Output(), "  corelaycode chat [flags]       Interactive TUI, with automatic line fallback")
		fmt.Fprintln(fs.Output(), "  corelaycode tui [flags]        Require the full-screen TUI")
		fmt.Fprintln(fs.Output(), "  corelaycode chat -plain        Line-oriented terminal client")
		fmt.Fprintln(fs.Output(), "  corelaycode chat -p <prompt>   One-shot terminal client")
		fmt.Fprintln(fs.Output(), "\nFlags:")
		fs.PrintDefaults()
	}
	url := fs.String("url", "http://localhost:4000", "Corelay Code server URL")
	workdir := fs.String("workdir", "", "Working directory for the agent (default: current dir)")
	lang := fs.String("lang", "auto", "Response language (auto, en, ko, ja, zh)")
	prompt := fs.String("p", "", "One-shot prompt; omit for the interactive terminal client")
	provider := fs.String("provider", "", "Optionally switch the server's provider before chatting")
	model := fs.String("model", "", "Optionally switch the server's model before chatting")
	noColor := fs.Bool("no-color", false, "Disable ANSI colors")
	showThinking := fs.Bool("show-thinking", false, "Show the model's reasoning (dimmed)")
	quiet := fs.Bool("quiet", false, "Hide status lines (project detection, iterations, etc.)")
	plain := fs.Bool("plain", false, "Use line mode (also selected automatically for non-TTY input/output)")
	forceTUI := fs.Bool("tui", false, "Require the full-screen terminal UI and an interactive terminal")
	token := fs.String("token", "", "Server access token (or CORELAY_ACCESS_TOKEN; legacy ANICLEW_ACCESS_TOKEN fallback)")
	sessionID := fs.String("session", "", "Durable session ID to open (TUI only)")
	fs.Parse(args)
	*token = resolveAccessToken(*token)
	resolvedWorkDir := defaultTUIWorkDir(*workdir)
	if *prompt == "" && !*plain && (*forceTUI || terminalSupportsTUI()) {
		if !terminalSupportsTUI() {
			fmt.Fprintln(os.Stderr, "The full-screen TUI requires an interactive terminal; use 'corelaycode chat -plain' for line mode.")
			os.Exit(2)
		}
		if err := runTUI(tuiOptions{
			BaseURL:      strings.TrimRight(*url, "/"),
			WorkDir:      resolvedWorkDir,
			Lang:         *lang,
			Provider:     *provider,
			Model:        *model,
			AccessToken:  *token,
			SessionID:    *sessionID,
			NoColor:      *noColor,
			ShowThinking: *showThinking,
			Quiet:        *quiet,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "Corelay Code TUI failed: %v\n", err)
			os.Exit(1)
		}
		return
	}

	c := &chatClient{
		base:         strings.TrimRight(*url, "/"),
		lang:         *lang,
		color:        !*noColor,
		showThinking: *showThinking,
		showStatus:   !*quiet,
		accessToken:  *token,
		http:         &http.Client{Timeout: 0}, // agent turns can be long; no client deadline
	}
	c.workDir = resolvedWorkDir

	// Verify the server is reachable early with a clear message.
	if err := c.ping(); err != nil {
		fmt.Fprintf(os.Stderr, "Cannot reach Corelay Code at %s — is the server running?\n  (%v)\n", c.base, err)
		os.Exit(1)
	}

	// Optional provider/model switch.
	if *provider != "" && *model != "" {
		if err := c.setConfig(*provider, *model); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not switch model: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "%sModel set to %s/%s%s\n", c.dim(), *provider, *model, c.rst())
		}
	}

	if *prompt != "" {
		c.runOnce(*prompt)
		return
	}
	c.repl()
}

func resolveAccessToken(flagToken string) string {
	if flagToken != "" {
		return flagToken
	}
	if token := os.Getenv("CORELAY_ACCESS_TOKEN"); token != "" {
		return token
	}
	return os.Getenv("ANICLEW_ACCESS_TOKEN")
}

type chatClient struct {
	base         string
	workDir      string
	lang         string
	color        bool
	showThinking bool
	showStatus   bool
	accessToken  string
	http         *http.Client
	messages     []chatMsg
	input        io.Reader
	approvalOut  io.Writer
	terminal     func() bool

	inputOnce    sync.Once
	inputLines   chan consoleLine
	approvalOnce sync.Once
	approvalGate chan struct{}

	// transient one-line status (spinner + elapsed + size) shown via \r while
	// the model is still working; cleared before any real output is printed.
	statusLine string
	spinIdx    int
}

type consoleLine struct {
	text string
	err  error
}

type pendingToolInput struct {
	encoded string
}

type chatTurnState struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu                 sync.Mutex
	sessionID          string
	seenApprovals      map[string]struct{}
	pendingApprovals   int
	resolvingApprovals int
	streamComplete     bool
	terminalErr        error
	wg                 sync.WaitGroup
	pendingToolInput   *pendingToolInput
}

func newChatTurnState(ctx context.Context, cancel context.CancelFunc) *chatTurnState {
	return &chatTurnState{
		ctx:           ctx,
		cancel:        cancel,
		seenApprovals: make(map[string]struct{}),
	}
}

func (t *chatTurnState) setSessionID(sessionID string) {
	t.mu.Lock()
	t.sessionID = strings.TrimSpace(sessionID)
	t.mu.Unlock()
}

func (t *chatTurnState) getSessionID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionID
}

func (t *chatTurnState) beginApproval(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, exists := t.seenApprovals[id]; exists {
		return false
	}
	t.seenApprovals[id] = struct{}{}
	t.pendingApprovals++
	return true
}

func (t *chatTurnState) finishApproval() {
	t.mu.Lock()
	if t.pendingApprovals > 0 {
		t.pendingApprovals--
	}
	t.mu.Unlock()
}

func (t *chatTurnState) approvalPending() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.pendingApprovals > 0
}

func (t *chatTurnState) beginApprovalResolution() {
	t.mu.Lock()
	t.resolvingApprovals++
	t.mu.Unlock()
}

func (t *chatTurnState) finishApprovalResolution() {
	t.mu.Lock()
	if t.resolvingApprovals > 0 {
		t.resolvingApprovals--
	}
	t.mu.Unlock()
}

func (t *chatTurnState) markStreamComplete(data any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.streamComplete = true
	if terminal, ok := agent.DecodeDurableRunTerminalMetadata(data); ok && terminal.BlocksSuccess() {
		t.terminalErr = &chatCompletionBlockedError{
			status:        string(terminal.CompletionStatus),
			terminalState: terminal.TerminalState,
		}
	}
}

func (t *chatTurnState) terminalError() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.terminalErr
}

func (t *chatTurnState) terminalObserved() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.streamComplete
}

type chatCompletionBlockedError struct {
	status        string
	terminalState string
}

func (e *chatCompletionBlockedError) Error() string {
	status := strings.TrimSpace(e.status)
	if status == "" {
		status = strings.TrimSpace(e.terminalState)
	}
	if status == "" {
		status = "blocked"
	}
	return "agent completion was not successful: " + status
}

func (t *chatTurnState) shutdown() {
	t.mu.Lock()
	waitForResolution := t.streamComplete && t.pendingApprovals > 0 &&
		t.pendingApprovals == t.resolvingApprovals
	t.mu.Unlock()
	if waitForResolution {
		t.wg.Wait()
		t.cancel()
		return
	}
	t.cancel()
	t.wg.Wait()
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

var errChatUnknownTerminal = errors.New("agent stream closed without a terminal event")

// clearStatus erases the transient \r status line if one is showing, so the
// next real output (text/thinking/tool) starts on a clean line. statusLine
// holds the VISIBLE text only (no ANSI), so rune count ≈ display width.
func (c *chatClient) clearStatus() {
	if c.statusLine == "" {
		return
	}
	fmt.Printf("\r%s\r", strings.Repeat(" ", len([]rune(c.statusLine))+2))
	c.statusLine = ""
}

type chatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ── ANSI helpers (no-ops when color is disabled) ──

func (c *chatClient) dim() string  { return c.code("\033[2m") }
func (c *chatClient) cyan() string { return c.code("\033[36m") }
func (c *chatClient) ylw() string  { return c.code("\033[33m") }
func (c *chatClient) red() string  { return c.code("\033[31m") }
func (c *chatClient) rst() string  { return c.code("\033[0m") }
func (c *chatClient) code(s string) string {
	if c.color {
		return s
	}
	return ""
}

func (c *chatClient) ping() error {
	req, err := http.NewRequest(http.MethodGet, c.base+"/api/config", nil)
	if err != nil {
		return err
	}
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func (c *chatClient) setConfig(provider, model string) error {
	body, _ := json.Marshal(map[string]string{"provider": provider, "model": model})
	req, _ := http.NewRequest("PUT", c.base+"/api/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func (c *chatClient) inputReader() io.Reader {
	if c.input != nil {
		return c.input
	}
	return os.Stdin
}

func (c *chatClient) approvalWriter() io.Writer {
	if c.approvalOut != nil {
		return c.approvalOut
	}
	return os.Stderr
}

func (c *chatClient) isTerminal() bool {
	if c.terminal != nil {
		return c.terminal()
	}
	f, ok := c.inputReader().(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// readConsoleLine owns a single background reader for stdin. Both the REPL and
// approval prompts consume from this channel, so a canceled approval cannot
// leave a blocked goroutine behind that steals the next REPL line.
func (c *chatClient) readConsoleLine(ctx context.Context) (string, error) {
	c.inputOnce.Do(func() {
		c.inputLines = make(chan consoleLine, 1)
		go func() {
			reader := bufio.NewReader(c.inputReader())
			for {
				line, err := reader.ReadString('\n')
				if len(line) > 0 {
					c.inputLines <- consoleLine{text: strings.TrimRight(line, "\r\n")}
				}
				if err != nil {
					c.inputLines <- consoleLine{err: err}
					close(c.inputLines)
					return
				}
			}
		}()
	})

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case line, ok := <-c.inputLines:
		if !ok {
			return "", io.EOF
		}
		return line.text, line.err
	}
}

func (c *chatClient) promptGate() chan struct{} {
	c.approvalOnce.Do(func() {
		c.approvalGate = make(chan struct{}, 1)
	})
	return c.approvalGate
}

func (c *chatClient) runOnce(prompt string) {
	c.messages = append(c.messages, chatMsg{Role: "user", Content: prompt})
	if _, err := c.streamTurn(); err != nil {
		fmt.Fprintf(os.Stderr, "\n%sError: %v%s\n", c.red(), err, c.rst())
		os.Exit(1)
	}
}

func (c *chatClient) repl() {
	fmt.Printf("%sCorelay Code chat — %s (workdir: %s)%s\n", c.cyan(), c.base, c.workDir, c.rst())
	fmt.Printf("%sType your message. Ctrl-C or 'exit' to quit.%s\n\n", c.dim(), c.rst())

	// Ctrl-C exits cleanly.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		<-sig
		fmt.Printf("\n%sBye.%s\n", c.dim(), c.rst())
		os.Exit(0)
	}()

	for {
		fmt.Printf("%s›%s ", c.cyan(), c.rst())
		line, err := c.readConsoleLine(context.Background())
		if err == io.EOF {
			fmt.Printf("\n%sBye.%s\n", c.dim(), c.rst())
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == "exit" || line == "quit" || line == "/exit" {
			fmt.Printf("%sBye.%s\n", c.dim(), c.rst())
			return
		}

		c.messages = append(c.messages, chatMsg{Role: "user", Content: line})
		reply, err := c.streamTurn()
		if err != nil {
			var blocked *chatCompletionBlockedError
			if errors.As(err, &blocked) {
				if reply != "" {
					c.messages = append(c.messages, chatMsg{Role: "assistant", Content: reply})
				}
				fmt.Fprintf(os.Stderr, "\n%sRun blocked: %v%s\n\n", c.red(), blocked, c.rst())
				continue
			}
			fmt.Fprintf(os.Stderr, "\n%sError: %v%s\n", c.red(), err, c.rst())
			// Drop the user turn we could not answer so history stays consistent.
			c.messages = c.messages[:len(c.messages)-1]
			continue
		}
		c.messages = append(c.messages, chatMsg{Role: "assistant", Content: reply})
		fmt.Print("\n\n")
	}
}

// streamTurn POSTs the current message history to /api/agent and renders the
// SSE event stream. Returns the assistant's accumulated text for history.
func (c *chatClient) streamTurn() (string, error) {
	turnCtx, turnCancel := context.WithCancel(context.Background())
	turn := newChatTurnState(turnCtx, turnCancel)
	defer turn.shutdown()

	var answer strings.Builder
	transport := newAgentStreamTransport(c.base, c.accessToken, c.http)
	stream := transport.StartTurn(turnCtx, agentTurnRequest{
		Messages:     c.messages,
		WorkDir:      c.workDir,
		ResponseLang: c.lang,
	})
	for item := range stream {
		switch {
		case item.Err != nil:
			c.clearStatus()
			return answer.String(), c.safeStreamError(item.Err)
		case item.EOF:
			// A clean transport EOF retains the line client's existing behavior.
			// Typed completion metadata, when present, is evaluated below.
		case strings.TrimSpace(item.Event.Type) != "":
			c.renderEvent(item.Event, &answer, turn)
		}
	}
	c.clearStatus() // erase any lingering spinner line before returning
	if terminalErr := turn.terminalError(); terminalErr != nil {
		return answer.String(), terminalErr
	}
	if !turn.terminalObserved() {
		return answer.String(), errChatUnknownTerminal
	}
	return answer.String(), nil
}

func (c *chatClient) renderEvent(ev agentWireEvent, answer *strings.Builder, turn *chatTurnState) {
	switch ev.Type {
	case "heartbeat":
		if turn.approvalPending() {
			return
		}
		// Transient proof-of-life: spinner + elapsed + output size, redrawn in
		// place via \r. The single signal that distinguishes a slow-but-working
		// local model from a hung connection (http client timeout is 0).
		var m struct {
			ElapsedMs int64 `json:"elapsedMs"`
			Chars     int64 `json:"chars"`
		}
		json.Unmarshal(ev.Data, &m)
		secs := m.ElapsedMs / 1000
		elapsed := fmt.Sprintf("%ds", secs)
		if secs >= 60 {
			elapsed = fmt.Sprintf("%dm%02ds", secs/60, secs%60)
		}
		frame := spinnerFrames[c.spinIdx%len(spinnerFrames)]
		c.spinIdx++
		visible := fmt.Sprintf("%s thinking… %s", frame, elapsed)
		if m.Chars > 0 {
			visible += fmt.Sprintf(" · %d chars", m.Chars)
		}
		c.statusLine = visible
		fmt.Printf("\r%s%s%s", c.dim(), visible, c.rst())

	case "text":
		c.clearStatus()
		var s string
		json.Unmarshal(ev.Data, &s)
		s = terminalSafe(s)
		fmt.Print(s)
		answer.WriteString(s)

	case "thinking":
		if c.showThinking {
			c.clearStatus()
			var s string
			json.Unmarshal(ev.Data, &s)
			fmt.Printf("%s%s%s", c.dim(), terminalSafe(s), c.rst())
		}

	case "status":
		if c.showStatus {
			c.clearStatus()
			var s string
			json.Unmarshal(ev.Data, &s)
			fmt.Printf("%s· %s%s\n", c.dim(), terminalSafe(s), c.rst())
		}

	case "tool_start":
		c.flushPendingToolInput(turn)
		c.clearStatus()
		var m map[string]string
		json.Unmarshal(ev.Data, &m)
		fmt.Printf("%s▸ %s%s\n", c.cyan(), terminalSafe(m["name"]), c.rst())

	case "tool_input":
		var m struct {
			Name   string `json:"name"`
			Input  any    `json:"input"`
			Danger string `json:"danger"`
		}
		json.Unmarshal(ev.Data, &m)
		if b, _ := json.Marshal(m.Input); len(b) > 0 {
			// Hold the raw input until the next tool result. If an approval frame
			// follows, it is discarded and only redactedInput is ever rendered.
			turn.pendingToolInput = &pendingToolInput{encoded: string(b)}
		}

	case "tool_result":
		c.flushPendingToolInput(turn)
		c.clearStatus()
		var m struct {
			Name    string `json:"name"`
			Result  string `json:"result"`
			IsError bool   `json:"isError"`
		}
		json.Unmarshal(ev.Data, &m)
		marker, col := "✓", c.dim()
		if m.IsError {
			marker, col = "✗", c.red()
		}
		fmt.Printf("%s  %s %s%s\n", col, marker, truncate(oneLine(terminalSafe(m.Result)), 200), c.rst())

	case "diff":
		c.clearStatus()
		var m struct {
			File string `json:"file"`
			Diff string `json:"diff"`
		}
		json.Unmarshal(ev.Data, &m)
		fmt.Printf("%s  ± %s%s\n", c.cyan(), terminalSafe(m.File), c.rst())
		for _, ln := range strings.Split(strings.TrimRight(terminalSafe(m.Diff), "\n"), "\n") {
			col := c.dim()
			if strings.HasPrefix(ln, "+ ") {
				col = c.cyan()
			} else if strings.HasPrefix(ln, "- ") {
				col = c.red()
			}
			fmt.Printf("%s  %s%s\n", col, ln, c.rst())
		}

	case "error":
		turn.pendingToolInput = nil
		c.clearStatus()
		var s string
		json.Unmarshal(ev.Data, &s)
		fmt.Printf("\n%s✗ %s%s\n", c.red(), terminalSafe(s), c.rst())

	case "session":
		var m struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(ev.Data, &m) == nil {
			turn.setSessionID(m.SessionID)
		}

	case "approval_required":
		turn.pendingToolInput = nil
		c.clearStatus()
		var m approvalRequiredEvent
		if json.Unmarshal(ev.Data, &m) != nil || strings.TrimSpace(m.ID) == "" || strings.TrimSpace(m.ToolName) == "" {
			fmt.Fprintln(c.approvalWriter(), "Permission request was invalid; canceling the run without approval.")
			turn.cancel()
			return
		}
		expiresAt, err := validApprovalExpiry(m.ExpiresAt, time.Now())
		if err != nil {
			fmt.Fprintln(c.approvalWriter(), "Permission request had an invalid or expired deadline; canceling the run without approval.")
			turn.cancel()
			return
		}
		sessionID := turn.getSessionID()
		if sessionID == "" || (m.SessionID != "" && m.SessionID != sessionID) {
			fmt.Fprintln(c.approvalWriter(), "Permission request did not match the active run; canceling without approval.")
			turn.cancel()
			return
		}
		if !turn.beginApproval(m.ID) {
			return
		}
		turn.wg.Add(1)
		go func() {
			defer turn.wg.Done()
			defer turn.finishApproval()
			c.handleApproval(turn, sessionID, m, expiresAt)
		}()

	case "done":
		turn.pendingToolInput = nil
		turn.markStreamComplete(ev.Data)
		// terminal control frame — nothing to render

	case "stream_end":
		turn.pendingToolInput = nil
		// stream_end is only a transport delimiter. A preceding done event owns
		// the run's terminal semantics; stream_end alone cannot prove success.

	case "command":
		turn.pendingToolInput = nil
		// control frames — nothing to render
	}
}

func (c *chatClient) flushPendingToolInput(turn *chatTurnState) {
	if turn.pendingToolInput == nil {
		return
	}
	c.clearStatus()
	fmt.Printf("%s  %s%s\n", c.dim(), truncate(turn.pendingToolInput.encoded, 200), c.rst())
	turn.pendingToolInput = nil
}

type approvalRequiredEvent struct {
	ID            string `json:"id"`
	SessionID     string `json:"sessionId"`
	ToolName      string `json:"toolName"`
	RedactedInput string `json:"redactedInput"`
	DangerLevel   string `json:"dangerLevel"`
	Scope         string `json:"scope"`
	ExpiresAt     string `json:"expiresAt"`
}

type approvalHTTPError struct {
	status int
}

func (e approvalHTTPError) Error() string {
	return fmt.Sprintf("HTTP %d", e.status)
}

const plainApprovalResolveTimeout = 15 * time.Second

func validApprovalExpiry(value string, now time.Time) (time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, errors.New("approval expiry is required")
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, errors.New("approval expiry is invalid")
	}
	if !expiresAt.After(now) {
		return time.Time{}, errors.New("approval has expired")
	}
	return expiresAt, nil
}

func approvalResolutionDeadline(expiresAt, now time.Time) time.Time {
	deadline := now.Add(plainApprovalResolveTimeout)
	if expiresAt.Before(deadline) {
		return expiresAt
	}
	return deadline
}

func (c *chatClient) handleApproval(
	turn *chatTurnState,
	sessionID string,
	pending approvalRequiredEvent,
	expiresAt time.Time,
) {
	if !expiresAt.After(time.Now()) {
		fmt.Fprintln(c.approvalWriter(), "Permission request expired; canceling the run without approval.")
		turn.cancel()
		return
	}
	promptCtx, cancelPrompt := context.WithDeadline(turn.ctx, expiresAt)
	defer cancelPrompt()

	decision := "deny"
	out := c.approvalWriter()
	toolName := truncate(oneLine(terminalSafe(pending.ToolName)), 80)
	if c.isTerminal() {
		gate := c.promptGate()
		select {
		case gate <- struct{}{}:
			defer func() { <-gate }()
		case <-promptCtx.Done():
			turn.cancel()
			return
		}

		fmt.Fprintf(out, "\nPermission required: %s\n", toolName)
		if pending.DangerLevel != "" || pending.Scope != "" {
			fmt.Fprintf(out, "Risk: %s  Scope: %s\n",
				truncate(oneLine(terminalSafe(pending.DangerLevel)), 40),
				truncate(oneLine(terminalSafe(pending.Scope)), 100))
		}
		fmt.Fprintf(out, "Redacted input: %s\n", truncate(oneLine(terminalSafe(pending.RedactedInput)), 400))
		fmt.Fprint(out, "Allow once? [y/N]: ")
		line, err := c.readConsoleLine(promptCtx)
		if err != nil && err != io.EOF {
			if promptCtx.Err() != nil {
				fmt.Fprintln(out, "Approval expired or the run ended; nothing was approved.")
				turn.cancel()
				return
			}
			fmt.Fprintln(out, "Input unavailable; denying.")
		}
		answer := strings.ToLower(strings.TrimSpace(line))
		if answer == "y" || answer == "yes" {
			decision = "allow_once"
		}
	} else {
		fmt.Fprintf(out, "Permission required for %s; stdin is not a TTY, denying automatically.\n", toolName)
	}

	now := time.Now()
	if !expiresAt.After(now) {
		fmt.Fprintln(out, "Approval expired or the run ended; nothing was approved.")
		turn.cancel()
		return
	}
	resolveCtx, cancelResolve := context.WithDeadline(
		turn.ctx,
		approvalResolutionDeadline(expiresAt, now),
	)
	defer cancelResolve()

	turn.beginApprovalResolution()
	err := c.resolveApproval(resolveCtx, pending.ID, sessionID, decision)
	turn.finishApprovalResolution()
	if err != nil {
		if resolveCtx.Err() != nil {
			fmt.Fprintln(out, "Approval expired or the run ended; nothing was approved.")
		} else if httpErr, ok := err.(approvalHTTPError); ok && (httpErr.status == http.StatusNotFound || httpErr.status == http.StatusConflict) {
			fmt.Fprintf(out, "Approval is no longer actionable (HTTP %d); treating it as denied.\n", httpErr.status)
		} else {
			fmt.Fprintf(out, "Approval decision could not be recorded; canceling the run without approval (%v).\n", err)
		}
		turn.cancel()
		return
	}
	if decision == "allow_once" {
		fmt.Fprintln(out, "Allowed once.")
	} else {
		fmt.Fprintln(out, "Denied.")
	}
}

func (c *chatClient) resolveApproval(ctx context.Context, approvalID, sessionID, decision string) error {
	body, err := json.Marshal(map[string]string{
		"sessionId": sessionID,
		"decision":  decision,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		c.base+"/api/approvals/"+url.PathEscape(approvalID)+"/resolve",
		bytes.NewReader(body),
	)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= http.StatusBadRequest {
		return approvalHTTPError{status: resp.StatusCode}
	}
	return nil
}

func (c *chatClient) authorize(req *http.Request) {
	if req != nil && c.accessToken != "" {
		req.Header.Set("X-Access-Token", c.accessToken)
	}
}

type plainSafeError struct {
	cause   error
	message string
}

func (e *plainSafeError) Error() string { return e.message }
func (e *plainSafeError) Unwrap() error { return e.cause }

func (c *chatClient) safeStreamError(err error) error {
	if err == nil {
		return nil
	}
	message := terminalSafe(err.Error())
	if c.accessToken != "" {
		message = strings.ReplaceAll(message, c.accessToken, "")
	}
	message = truncateUTF8Bytes(strings.TrimSpace(message), maxAgentHTTPErrorBodyBytes)
	if message == "" {
		message = "agent stream failed"
	}
	return &plainSafeError{cause: err, message: message}
}

func terminalSafe(s string) string {
	return safeTUIText(s, 0)
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
