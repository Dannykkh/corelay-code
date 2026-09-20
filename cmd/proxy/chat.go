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
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/config"
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
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage:")
		fmt.Fprintln(fs.Output(), "  corelaycode chat [flags]       Interactive TUI, with automatic line fallback")
		fmt.Fprintln(fs.Output(), "  corelaycode tui [flags]        Require the full-screen TUI")
		fmt.Fprintln(fs.Output(), "  corelaycode chat -plain        Line-oriented terminal client")
		fmt.Fprintln(fs.Output(), "  corelaycode chat -p <prompt>   One-shot terminal client")
		fmt.Fprintln(fs.Output(), "  corelaycode chat -session <id> Resume a durable session in any mode")
		fmt.Fprintln(fs.Output(), "  corelaycode chat -session <id> -fork Fork the session before the next turn")
		fmt.Fprintln(fs.Output(), "\nFlags:")
		fs.PrintDefaults()
	}
	url := fs.String("url", "http://localhost:4000", "Corelay Code server URL")
	workdir := fs.String("workdir", "", "Working directory for the agent (default: current dir)")
	modeFlag := fs.String("mode", "", "Per-run execution mode: read-only, workspace, or full (default: workspace)")
	lang := fs.String("lang", "auto", "Response language (auto, en, ko, ja, zh)")
	prompt := fs.String("p", "", "One-shot prompt; omit for the interactive terminal client")
	provider := fs.String("provider", "", "Optionally switch the server's provider before chatting")
	model := fs.String("model", "", "Optionally switch the server's model before chatting")
	noColor := fs.Bool("no-color", false, "Disable ANSI colors")
	showThinking := fs.Bool("show-thinking", false, "Show the model's reasoning (dimmed)")
	quiet := fs.Bool("quiet", false, "Hide status lines (project detection, iterations, etc.)")
	plain := fs.Bool("plain", false, "Use line mode (also selected automatically for non-TTY input/output)")
	forceTUI := fs.Bool("tui", false, "Require the full-screen terminal UI and an interactive terminal")
	formatFlag := fs.String("format", string(chatOutputHuman), "One-shot output format: human, json, or jsonl")
	token := fs.String("token", "", "Server access token (or CORELAY_ACCESS_TOKEN; legacy ANICLEW_ACCESS_TOKEN fallback)")
	sessionID := fs.String("session", "", "Durable session ID to resume (works with TUI, plain, and one-shot modes)")
	forkSession := fs.Bool("fork", false, "Fork the selected durable session before the next turn (requires -session)")
	parseErr := fs.Parse(args)
	format := chatOutputFormat(strings.ToLower(strings.TrimSpace(*formatFlag)))
	runID := newChatRunID()
	if parseErr != nil {
		format = requestedChatOutputFormat(args, format)
		if errors.Is(parseErr, flag.ErrHelp) {
			fs.SetOutput(os.Stdout)
			fs.Usage()
			return
		}
		if format == chatOutputHuman {
			fmt.Fprintf(os.Stderr, "chat: %v\n", parseErr)
			fs.SetOutput(os.Stderr)
			fs.Usage()
			os.Exit(2)
		}
		failChatUsage(format, runID, parseErr.Error())
	}
	if format != chatOutputHuman && format != chatOutputJSON && format != chatOutputJSONL {
		failChatUsage(chatOutputHuman, runID, fmt.Sprintf("invalid -format %q; expected human, json, or jsonl", *formatFlag))
	}
	if format != chatOutputHuman && strings.TrimSpace(*prompt) == "" {
		failChatUsage(format, runID, "-format json and -format jsonl require -p <prompt>")
	}
	urlExplicit := false
	fs.Visit(func(visited *flag.Flag) {
		if visited.Name == "url" {
			urlExplicit = true
		}
	})
	mode, err := parseExecutionModeFlag(*modeFlag)
	if err != nil {
		failChatUsage(format, runID, err.Error())
	}
	if *forkSession && strings.TrimSpace(*sessionID) == "" {
		failChatUsage(format, runID, "-fork requires -session <id>")
	}
	var managedLease *managedServerLease
	if !urlExplicit {
		cfg, _, configErr := config.LoadChecked()
		if configErr != nil {
			failChatStartup(format, runID, "failed", "cannot_read_configuration", fmt.Errorf("cannot read Corelay configuration: %w", configErr), "", "", fmt.Sprintf("Cannot read Corelay configuration: %v", configErr))
		}
		port := cfg.Port
		if port <= 0 {
			port = 4000
		}
		*url = fmt.Sprintf("http://127.0.0.1:%d", port)
		*token = resolveAccessTokenForURL(*token, *url)
		serverProvider, serverModel := *provider, *model
		if serverProvider == "" {
			serverProvider = cfg.DefaultProvider
		}
		if serverModel == "" {
			serverModel = cfg.DefaultModel
		}
		startupCtx, cancel := context.WithTimeout(context.Background(), managedServerStartupTimeout+5*time.Second)
		managedLease, err = ensureManagedLocalChatServer(startupCtx, *url, cfg, serverProvider, serverModel, *token, &http.Client{Timeout: 2 * time.Second})
		cancel()
		if err != nil {
			failChatStartup(format, runID, "failed", "server_unavailable", fmt.Errorf("cannot connect to or start Corelay Code: %w", err), *token, *url, fmt.Sprintf("Cannot connect to or start Corelay Code at %s: %v", *url, err))
		}
		*token = resolveAccessTokenForURL(*token, *url)
	} else {
		*token = resolveAccessTokenForURL(*token, *url)
	}
	defer func() { _ = managedLease.Close() }()
	resolvedWorkDir := defaultTUIWorkDir(*workdir)
	workDirExplicit := strings.TrimSpace(*workdir) != ""
	if *prompt == "" && !*plain && (*forceTUI || terminalSupportsTUI()) {
		if !terminalSupportsTUI() {
			fmt.Fprintln(os.Stderr, "The full-screen TUI requires an interactive terminal; use 'corelaycode chat -plain' for line mode.")
			os.Exit(2)
		}
		if err := runTUI(tuiOptions{
			BaseURL:         strings.TrimRight(*url, "/"),
			WorkDir:         resolvedWorkDir,
			Mode:            mode,
			Lang:            *lang,
			Provider:        *provider,
			Model:           *model,
			AccessToken:     *token,
			SessionID:       *sessionID,
			ForkSession:     *forkSession,
			WorkDirExplicit: workDirExplicit,
			NoColor:         *noColor,
			ShowThinking:    *showThinking,
			Quiet:           *quiet,
		}); err != nil {
			_ = managedLease.Close()
			fmt.Fprintf(os.Stderr, "Corelay Code TUI failed: %v\n", err)
			os.Exit(1)
		}
		return
	}

	c := &chatClient{
		base:            strings.TrimRight(*url, "/"),
		lang:            *lang,
		mode:            mode,
		color:           !*noColor,
		showThinking:    *showThinking,
		showStatus:      !*quiet,
		accessToken:     *token,
		http:            newChatHTTPClient(0), // agent turns can be long; no client deadline
		sessionID:       strings.TrimSpace(*sessionID),
		forkSession:     *forkSession,
		workDirExplicit: workDirExplicit,
		managedLease:    managedLease,
		format:          format,
		runID:           runID,
	}
	if format == chatOutputJSON || format == chatOutputJSONL {
		c.output = &chatOutputEmitter{format: format, out: os.Stdout, runID: runID}
	}
	c.workDir = resolvedWorkDir

	// Verify the server is reachable early with a clear message.
	if err := c.ping(); err != nil {
		_ = managedLease.Close()
		failChatStartup(format, c.runID, "failed", "server_unavailable", fmt.Errorf("cannot reach Corelay Code server: %w", err), c.accessToken, c.base, fmt.Sprintf("Cannot reach Corelay Code at %s — is the server running?\n  (%v)", c.base, err))
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
		runCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		c.runCtx = runCtx
		reply, runErr := c.runOnce(*prompt)
		interrupted := errors.Is(runCtx.Err(), context.Canceled)
		stop()
		status, exitCode := chatRunStatus(runErr, interrupted)
		c.writeOneShotResult(reply, status, exitCode, runErr)
		if c.output != nil && c.output.err != nil {
			_ = managedLease.Close()
			fmt.Fprintf(os.Stderr, "chat: output failed: %v\n", c.output.err)
			os.Exit(1)
		}
		if runErr != nil {
			_ = managedLease.Close()
			if format == chatOutputHuman {
				fmt.Fprintf(os.Stderr, "\n%sError: %v%s\n", c.red(), runErr, c.rst())
			}
			os.Exit(exitCode)
		}
		return
	}
	c.repl()
}

func isLoopbackServerURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" {
		return false
	}
	host := strings.Trim(parsed.Hostname(), "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func newChatHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: rejectChatAPIRedirect}
}

func rejectChatAPIRedirect(*http.Request, []*http.Request) error {
	return errors.New("redirects are disabled for authenticated Corelay API requests")
}

func resolveAccessTokenForURL(flagToken, serverURL string) string {
	if token := resolveAccessToken(flagToken); token != "" || !isLoopbackServerURL(serverURL) {
		return token
	}
	if cfg, exists, err := config.LoadChecked(); err == nil && exists {
		return cfg.AccessToken
	}
	return ""
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
	base             string
	workDir          string
	lang             string
	mode             agent.ExecutionMode
	color            bool
	showThinking     bool
	showStatus       bool
	accessToken      string
	http             *http.Client
	messages         []chatMsg
	managedLease     *managedServerLease
	sessionID        string
	session          *agent.Session
	forkSession      bool
	workDirExplicit  bool
	sessionAnnounced bool
	format           chatOutputFormat
	runID            string
	output           *chatOutputEmitter
	runCtx           context.Context
	terminalSummary  *chatTerminalSummary
	input            io.Reader
	approvalOut      io.Writer
	terminal         func() bool

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

func (t *chatTurnState) markStreamComplete(data any, accessToken, serverURL string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.streamComplete = true
	var terminal struct {
		Kind       string `json:"kind"`
		StopReason string `json:"stopReason"`
	}
	if encoded, err := json.Marshal(data); err == nil {
		_ = json.Unmarshal(encoded, &terminal)
	}
	terminal.Kind = normalizedRunTerminalKind(terminal.Kind)
	terminal.StopReason = sanitizeChatOutputText(terminal.StopReason, accessToken, serverURL, 128)
	if terminal.Kind == string(agent.RunTerminalCancelled) {
		t.terminalErr = &chatTerminalFailureError{kind: terminal.Kind, stopReason: terminal.StopReason}
		return
	}
	if terminal.Kind == "unknown" {
		t.terminalErr = &chatTerminalFailureError{kind: string(agent.RunTerminalFailed)}
		return
	}
	if terminal.Kind != "" && terminal.Kind != string(agent.RunTerminalCompleted) && terminal.Kind != string(agent.RunTerminalCommand) {
		t.terminalErr = &chatTerminalFailureError{kind: terminal.Kind, stopReason: terminal.StopReason}
		return
	}
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

type chatTerminalFailureError struct {
	kind       string
	stopReason string
}

func (e *chatTerminalFailureError) Error() string {
	kind := strings.TrimSpace(e.kind)
	if kind == "" {
		kind = "failed"
	}
	reason := strings.TrimSpace(e.stopReason)
	if reason == "" {
		return "agent run terminated: " + kind
	}
	return "agent run terminated: " + kind + " (" + reason + ")"
}

func (e *chatTerminalFailureError) Unwrap() error {
	if e != nil && e.kind == string(agent.RunTerminalCancelled) {
		return context.Canceled
	}
	return nil
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
	if c.format != chatOutputHuman {
		c.statusLine = ""
		return
	}
	fmt.Printf("\r%s\r", strings.Repeat(" ", len([]rune(c.statusLine))+2))
	c.statusLine = ""
}

func (c *chatClient) humanPrintf(format string, values ...any) {
	if c.format == chatOutputHuman {
		fmt.Printf(format, values...)
	}
}

func (c *chatClient) humanPrint(values ...any) {
	if c.format == chatOutputHuman {
		fmt.Print(values...)
	}
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
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
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

func (c *chatClient) runOnce(prompt string) (string, error) {
	c.messages = append(c.messages, chatMsg{Role: "user", Content: prompt})
	return c.streamTurn()
}

func chatRunStatus(err error, interrupted bool) (string, int) {
	if interrupted {
		return "cancelled", 130
	}
	if err == nil {
		return "completed", 0
	}
	var blocked *chatCompletionBlockedError
	if errors.As(err, &blocked) {
		return "blocked", 1
	}
	var terminalErr *chatTerminalFailureError
	if errors.As(err, &terminalErr) && terminalErr.kind == string(agent.RunTerminalContextBlocked) {
		return "blocked", 1
	}
	return "failed", 1
}

func requestedChatOutputFormat(args []string, parsed chatOutputFormat) chatOutputFormat {
	requested := string(parsed)
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "-format" || arg == "--format" {
			if index+1 < len(args) {
				requested = args[index+1]
				index++
			}
			continue
		}
		for _, prefix := range []string{"-format=", "--format="} {
			if strings.HasPrefix(arg, prefix) {
				requested = strings.TrimPrefix(arg, prefix)
			}
		}
	}
	format := chatOutputFormat(strings.ToLower(strings.TrimSpace(requested)))
	if format == chatOutputJSON || format == chatOutputJSONL {
		return format
	}
	return chatOutputHuman
}

func (c *chatClient) writeOneShotResult(reply, status string, exitCode int, err error) {
	if c.output == nil {
		return
	}
	revision := uint64(0)
	if c.session != nil {
		revision = c.session.Revision
	}
	result := newChatOutputResult(c.runID, status, exitCode, reply, c.sessionID, revision, c.terminalSummary, err, c.accessToken, c.base)
	if result.Error != nil && exitCode != 130 && result.Error.Code == "cancelled" {
		result.Error.Code = "run_cancelled"
	}
	c.output.result(result)
}

func failChatUsage(format chatOutputFormat, runID, message string) {
	if format == chatOutputJSON || format == chatOutputJSONL {
		emitter := &chatOutputEmitter{format: format, out: os.Stdout, runID: runID}
		emitter.result(chatOutputResult{
			SchemaVersion: chatOutputSchemaVersion,
			RunID:         runID,
			Status:        "failed",
			ExitCode:      2,
			Text:          "",
			Error:         &chatOutputError{Code: "usage_error", Message: sanitizeChatOutputText(message, "", "", maxChatOutputEventBytes)},
		})
	} else {
		fmt.Fprintf(os.Stderr, "chat: %s\n", message)
	}
	os.Exit(2)
}

func failChatStartup(format chatOutputFormat, runID, status, code string, err error, accessToken, serverURL, humanMessage string) {
	if format == chatOutputJSON || format == chatOutputJSONL {
		emitter := &chatOutputEmitter{format: format, out: os.Stdout, runID: runID}
		result := newChatOutputResult(runID, status, 1, "", "", 0, nil, err, accessToken, serverURL)
		if result.Error == nil {
			result.Error = &chatOutputError{Code: code, Message: "request could not be started"}
		} else {
			result.Error.Code = code
		}
		emitter.result(result)
	} else {
		fmt.Fprintln(os.Stderr, humanMessage)
	}
	os.Exit(1)
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
		_ = c.managedLease.Close()
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
				if c.session == nil && reply != "" {
					c.messages = append(c.messages, chatMsg{Role: "assistant", Content: reply})
				}
				fmt.Fprintf(os.Stderr, "\n%sRun blocked: %v%s\n\n", c.red(), blocked, c.rst())
				continue
			}
			fmt.Fprintf(os.Stderr, "\n%sError: %v%s\n", c.red(), err, c.rst())
			// Drop the user turn we could not answer so history stays consistent.
			if c.session != nil {
				c.messages = wireMessagesFromSession(c.session.Messages)
			} else {
				c.messages = c.messages[:len(c.messages)-1]
			}
			continue
		}
		if c.session == nil {
			c.messages = append(c.messages, chatMsg{Role: "assistant", Content: reply})
		}
		fmt.Print("\n\n")
	}
}

// streamTurn POSTs the current message history to /api/agent and renders the
// SSE event stream. Returns the assistant's accumulated text for history.
func (c *chatClient) streamTurn() (string, error) {
	if len(c.messages) == 0 || c.messages[len(c.messages)-1].Role != "user" {
		return "", errors.New("chat turn is missing its user message")
	}
	transport := newAgentStreamTransport(c.base, c.accessToken, c.http)
	prompt := c.messages[len(c.messages)-1].Content
	parentCtx := c.runCtx
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	prepareCtx, prepareCancel := context.WithTimeout(parentCtx, 8*time.Second)
	session, revision, err := c.prepareDurableTurn(prepareCtx, transport, prompt)
	prepareCancel()
	if err != nil {
		return "", err
	}
	c.messages = wireMessagesFromSession(session.Messages)
	revisionForRun := revision
	turnCtx, turnCancel := context.WithCancel(parentCtx)
	turn := newChatTurnState(turnCtx, turnCancel)
	defer turn.shutdown()

	var answer strings.Builder
	stream := transport.StartTurn(turnCtx, agentTurnRequest{
		Messages:         c.messages,
		WorkDir:          session.Workspace,
		ResponseLang:     c.lang,
		ExecutionPolicy:  requestedExecutionPolicy(c.mode),
		DurableSessionID: session.ID,
		ExpectedRevision: &revisionForRun,
	})
	var streamErr error
	for item := range stream {
		switch {
		case item.Err != nil:
			c.clearStatus()
			streamErr = c.safeStreamError(item.Err)
		case item.EOF:
			// A clean transport EOF retains the line client's existing behavior.
			// Typed completion metadata, when present, is evaluated below.
		case strings.TrimSpace(item.Event.Type) != "":
			c.renderEvent(item.Event, &answer, turn)
		}
	}
	c.clearStatus() // erase any lingering spinner line before returning
	var terminalErr error
	if runTerminalErr := turn.terminalError(); runTerminalErr != nil {
		terminalErr = runTerminalErr
	} else if !turn.terminalObserved() {
		terminalErr = errChatUnknownTerminal
	}
	refreshCtx, refreshCancel := context.WithTimeout(parentCtx, 8*time.Second)
	refreshed, refreshErr := transport.GetSession(refreshCtx, session.ID)
	refreshCancel()
	if refreshErr == nil && refreshed != nil {
		c.session = refreshed
		c.messages = wireMessagesFromSession(refreshed.Messages)
	} else if refreshErr != nil && streamErr == nil && terminalErr == nil {
		refreshErr = fmt.Errorf("durable session synchronization failed: %w", refreshErr)
	}
	if streamErr != nil {
		return answer.String(), streamErr
	}
	if terminalErr != nil {
		return answer.String(), terminalErr
	}
	if refreshErr != nil {
		return answer.String(), refreshErr
	}
	return answer.String(), nil
}

func (c *chatClient) prepareDurableTurn(
	ctx context.Context,
	transport *agentStreamTransport,
	prompt string,
) (*agent.Session, uint64, error) {
	if c.session == nil && strings.TrimSpace(c.sessionID) != "" {
		session, err := transport.GetSession(ctx, c.sessionID)
		if err != nil {
			return nil, 0, err
		}
		if err := validateDurableChatWorkspace(session, c.workDir, c.workDirExplicit); err != nil {
			return nil, 0, err
		}
		if !c.workDirExplicit && strings.TrimSpace(session.Workspace) != "" {
			c.workDir = session.Workspace
		}
		if c.forkSession {
			session, err = transport.ForkSession(ctx, session.ID, session.Revision)
			if err != nil {
				return nil, 0, err
			}
			c.forkSession = false
			c.sessionAnnounced = false
		}
		c.session = session
		c.sessionID = session.ID
	}
	if err := validateDurableChatWorkspace(c.session, c.workDir, c.workDirExplicit); err != nil {
		return nil, 0, err
	}
	session, revision, err := saveDurableChatUserTurn(
		ctx, transport, c.session, prompt, c.workDir, "", "",
	)
	if err != nil {
		if isChatHTTPStatus(err, http.StatusConflict) {
			// A concurrent writer may have advanced the canonical session. Force
			// the next line-mode turn to reload before attempting another CAS.
			c.session = nil
		}
		return nil, 0, err
	}
	c.session = session
	c.sessionID = session.ID
	c.announceDurableSession()
	return session, revision, nil
}

func isChatHTTPStatus(err error, status int) bool {
	var httpErr *agentHTTPError
	return errors.As(err, &httpErr) && httpErr.StatusCode == status
}

func (c *chatClient) announceDurableSession() {
	if c.format != chatOutputHuman || c.sessionAnnounced || c.session == nil || strings.TrimSpace(c.session.ID) == "" {
		return
	}
	fmt.Fprintf(os.Stderr, "Durable session: %s (revision %d)\n", c.session.ID, c.session.Revision)
	c.sessionAnnounced = true
}

func (c *chatClient) renderEvent(ev agentWireEvent, answer *strings.Builder, turn *chatTurnState) {
	if c.output != nil {
		if name, data, ok := normalizeChatOutputEvent(ev, c.output, c.showThinking, c.accessToken, c.base); ok {
			c.output.event(name, data)
		}
		c.runID = c.output.runID
	}
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
		c.humanPrintf("\r%s%s%s", c.dim(), visible, c.rst())

	case "text":
		c.clearStatus()
		var s string
		json.Unmarshal(ev.Data, &s)
		s = terminalSafe(s)
		c.humanPrint(s)
		answer.WriteString(s)

	case "thinking":
		if c.showThinking {
			c.clearStatus()
			var s string
			json.Unmarshal(ev.Data, &s)
			c.humanPrintf("%s%s%s", c.dim(), terminalSafe(s), c.rst())
		}

	case "status":
		if c.showStatus {
			c.clearStatus()
			var s string
			json.Unmarshal(ev.Data, &s)
			c.humanPrintf("%s· %s%s\n", c.dim(), terminalSafe(s), c.rst())
		}

	case "tool_start":
		c.flushPendingToolInput(turn)
		c.clearStatus()
		var m map[string]string
		json.Unmarshal(ev.Data, &m)
		c.humanPrintf("%s▸ %s%s\n", c.cyan(), terminalSafe(m["name"]), c.rst())

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
		c.humanPrintf("%s  %s %s%s\n", col, marker, truncate(oneLine(terminalSafe(m.Result)), 200), c.rst())

	case "diff":
		c.clearStatus()
		var m struct {
			File string `json:"file"`
			Diff string `json:"diff"`
		}
		json.Unmarshal(ev.Data, &m)
		c.humanPrintf("%s  ± %s%s\n", c.cyan(), terminalSafe(m.File), c.rst())
		for _, ln := range strings.Split(strings.TrimRight(terminalSafe(m.Diff), "\n"), "\n") {
			col := c.dim()
			if strings.HasPrefix(ln, "+ ") {
				col = c.cyan()
			} else if strings.HasPrefix(ln, "- ") {
				col = c.red()
			}
			c.humanPrintf("%s  %s%s\n", col, ln, c.rst())
		}

	case "error":
		turn.pendingToolInput = nil
		c.clearStatus()
		var s string
		json.Unmarshal(ev.Data, &s)
		c.humanPrintf("\n%s✗ %s%s\n", c.red(), terminalSafe(s), c.rst())

	case "session":
		var m struct {
			SessionID       string `json:"sessionId"`
			ExecutionPolicy struct {
				Mode     agent.ExecutionMode `json:"mode"`
				Revision uint64              `json:"revision"`
			} `json:"executionPolicy"`
		}
		if json.Unmarshal(ev.Data, &m) == nil {
			turn.setSessionID(m.SessionID)
			if c.showStatus && validEffectiveExecutionPolicy(m.ExecutionPolicy.Mode, m.ExecutionPolicy.Revision) {
				c.clearStatus()
				c.humanPrintf("%s· Execution mode: %s (revision %d)%s\n", c.dim(), m.ExecutionPolicy.Mode, m.ExecutionPolicy.Revision, c.rst())
			}
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
		c.terminalSummary = chatTerminalSummaryFromData(ev.Data, c.accessToken, c.base)
		turn.markStreamComplete(ev.Data, c.accessToken, c.base)
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
	c.humanPrintf("%s  %s%s\n", c.dim(), truncate(turn.pendingToolInput.encoded, 200), c.rst())
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
