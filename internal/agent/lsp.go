package agent

// The LSP boundary is deliberately small and read-only. It starts one
// configured language-server process per request, synchronizes the target
// document, asks one semantic question, then shuts the process down. This
// keeps lifecycle and cancellation ownership local to a tool call and avoids
// a process-wide language-server cache leaking workspace state between runs.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Dannykkh/corelay-code/internal/config"
	"github.com/Dannykkh/corelay-code/internal/processsupervisor"
	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

const (
	defaultLSPTimeout       = 20 * time.Second
	maxLSPFrameBytes        = 2 << 20
	maxLSPHeaderBytes       = 16 << 10
	maxLSPResults           = 200
	defaultLSPResults       = 50
	maxLSPDiagnosticMessage = 2048
)

type lspInput struct {
	Operation  string `json:"operation"`
	FilePath   string `json:"file_path"`
	Line       int    `json:"line"`
	Column     int    `json:"column"`
	MaxResults int    `json:"max_results"`
}

// LSPExecutionOptions is intentionally separate from ToolExecutionOptions:
// the normal tool runner is a one-shot sandbox.Runner, while LSP requires the
// streaming processsupervisor contract. Production callers use the secure
// platform adapter selected here; tests can inject a deterministic runner.
type LSPExecutionOptions struct {
	Context         context.Context
	Runner          processsupervisor.Runner
	Policy          sandbox.Policy
	ExecutionPolicy ExecutionPolicySnapshot
	Args            []string
	CallTimeout     time.Duration
	ObserveStart    func(processsupervisor.Report)
}

type lspRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type lspMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *lspRPCError    `json:"error,omitempty"`
}

type lspLocation struct {
	URI   string   `json:"uri"`
	Range lspRange `json:"range"`
}

type lspRange struct {
	Start lspPosition `json:"start"`
	End   lspPosition `json:"end"`
}

type lspPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type lspDiagnostic struct {
	Range    lspRange `json:"range"`
	Severity int      `json:"severity,omitempty"`
	Source   string   `json:"source,omitempty"`
	Message  string   `json:"message"`
}

type lspClient struct {
	process *processsupervisor.Process
	stdin   io.WriteCloser
	stdout  *bufio.Reader
	root    string
	nextID  atomic.Int64
	closed  atomic.Bool
	timeout time.Duration

	diagnosticsMu syncDiagnostics
}

// syncDiagnostics keeps the small notification cache lock local without
// exposing a map or server-owned payload outside the client.
type syncDiagnostics struct {
	mu       chan struct{}
	uri      string
	received bool
	items    []lspDiagnostic
}

func newSyncDiagnostics() syncDiagnostics {
	lock := make(chan struct{}, 1)
	lock <- struct{}{}
	return syncDiagnostics{mu: lock}
}

func (d *syncDiagnostics) selectDocument(uri string) {
	<-d.mu
	if d.uri != uri {
		d.uri = uri
		d.received = false
		d.items = nil
	}
	d.mu <- struct{}{}
}

func (d *syncDiagnostics) set(uri string, items []lspDiagnostic) {
	if d == nil || d.mu == nil {
		return
	}
	<-d.mu
	if uri != "" && uri == d.uri {
		d.items = append([]lspDiagnostic(nil), items...)
		d.received = true
	}
	d.mu <- struct{}{}
}

func (d *syncDiagnostics) snapshot() ([]lspDiagnostic, bool) {
	if d == nil || d.mu == nil {
		return nil, false
	}
	<-d.mu
	items := append([]lspDiagnostic(nil), d.items...)
	received := d.received
	d.mu <- struct{}{}
	return items, received
}

func defaultLSPExecutionOptions(ctx context.Context, workDir string, policy *ExecutionPolicySnapshot) LSPExecutionOptions {
	if ctx == nil {
		ctx = context.Background()
	}
	if policy != nil && policy.Mode == ExecutionModeFull {
		mcp := FullModeMCPExecutionOptions(ctx, *policy)
		return LSPExecutionOptions{Context: ctx, Runner: mcp.Runner, Policy: mcp.Policy, ExecutionPolicy: mcp.ExecutionPolicy, Args: []string{"serve"}, CallTimeout: defaultLSPTimeout}
	}
	mcp := DefaultMCPExecutionOptions(ctx, workDir)
	if policy != nil {
		mcp.ExecutionPolicy = *policy
	}
	return LSPExecutionOptions{Context: ctx, Runner: mcp.Runner, Policy: mcp.Policy, ExecutionPolicy: mcp.ExecutionPolicy, Args: []string{"serve"}, CallTimeout: defaultLSPTimeout}
}

func executeLSPWithOptions(input json.RawMessage, workDir string, opts ToolExecutionOptions) (string, bool) {
	paths, err := executionToolWorkspacePathsWithPolicy("LSP", input, workDir, opts.ExecutionPolicy)
	if err != nil {
		return "LSP blocked: " + err.Error(), true
	}
	var args lspInput
	if err := json.Unmarshal(input, &args); err != nil {
		return "LSP blocked: invalid input", true
	}
	args.FilePath = paths.one("file_path")
	if err := validateLSPInput(args); err != nil {
		return "LSP blocked: " + err.Error(), true
	}
	if args.MaxResults <= 0 {
		args.MaxResults = defaultLSPResults
	}
	if args.MaxResults > maxLSPResults {
		args.MaxResults = maxLSPResults
	}

	if !strings.EqualFold(filepath.Ext(args.FilePath), ".go") {
		return lspFallback(workDir, args.FilePath, "LSP currently supports Go files only", opts.ExecutionPolicy), false
	}
	content, err := readLSPDocument(args.FilePath)
	if err != nil {
		return "LSP could not read the target document", true
	}
	executable := configuredLSPExecutable()
	if strings.TrimSpace(executable) == "" {
		return lspFallback(workDir, args.FilePath, "no configured language-server executable", opts.ExecutionPolicy), false
	}
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	lspWorkDir := workDir
	if opts.ExecutionPolicy != nil && opts.ExecutionPolicy.Mode == ExecutionModeFull {
		if workspace, workspaceErr := canonicalWorkspace(workDir); workspaceErr == nil && !pathWithin(args.FilePath, workspace) {
			// Full mode explicitly permits external targets. Start the server at
			// the external file's directory so module discovery is rooted at the
			// target instead of the selected project.
			lspWorkDir = filepath.Dir(args.FilePath)
		}
	}
	lspOpts := defaultLSPExecutionOptions(ctx, lspWorkDir, opts.ExecutionPolicy)
	if err := ctx.Err(); err != nil {
		return "LSP canceled before start", true
	}
	client, err := newLSPClient(ctx, executable, lspWorkDir, lspOpts)
	if err != nil {
		return lspFallback(workDir, args.FilePath, "language server unavailable: "+sanitizeLSPText(err.Error()), opts.ExecutionPolicy), false
	}
	defer client.close()
	if err := client.initialize(ctx); err != nil {
		return lspFallback(workDir, args.FilePath, "language server initialization failed: "+sanitizeLSPText(err.Error()), opts.ExecutionPolicy), false
	}
	uri := lspURI(args.FilePath)
	if err := client.didOpen(ctx, uri, content); err != nil {
		return lspFallback(workDir, args.FilePath, "document synchronization failed: "+sanitizeLSPText(err.Error()), opts.ExecutionPolicy), false
	}
	result, err := client.execute(ctx, args, uri)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "LSP canceled or timed out", true
		}
		return lspFallback(workDir, args.FilePath, "language server request failed: "+sanitizeLSPText(err.Error()), opts.ExecutionPolicy), false
	}
	return result, false
}

func validateLSPInput(args lspInput) error {
	switch args.Operation {
	case "definition", "references", "diagnostics":
	default:
		return errors.New("operation must be definition, references, or diagnostics")
	}
	if strings.TrimSpace(args.FilePath) == "" {
		return errors.New("file_path is required")
	}
	if args.Operation != "diagnostics" && (args.Line < 1 || args.Column < 1) {
		return errors.New("definition and references require 1-based line and column")
	}
	if args.Operation == "diagnostics" && (args.Line < 0 || args.Column < 0) {
		return errors.New("diagnostics line and column must not be negative")
	}
	return nil
}

func configuredLSPExecutable() string {
	if value := strings.TrimSpace(os.Getenv("CORELAY_LSP_EXECUTABLE")); value != "" {
		return value
	}
	if value := strings.TrimSpace(os.Getenv("ANICLEW_LSP_EXECUTABLE")); value != "" {
		return value
	}
	if cfg, _, err := config.LoadChecked(); err == nil && strings.TrimSpace(cfg.LSPExecutable) != "" {
		return strings.TrimSpace(cfg.LSPExecutable)
	}
	return "gopls"
}

// ConfiguredLSPExecutable exposes the non-secret executable selection used by
// the doctor command without starting a server or installing anything.
func ConfiguredLSPExecutable() string { return configuredLSPExecutable() }

func readLSPDocument(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8<<20 {
		return nil, errors.New("document is unavailable or too large")
	}
	return os.ReadFile(path)
}

func lspFallback(workDir, filePath, reason string, policy *ExecutionPolicySnapshot) string {
	root := filepath.Dir(filePath)
	if strings.TrimSpace(root) == "" {
		root = workDir
	}
	// The fallback is intentionally structural. Its heading prevents callers
	// from mistaking declarations for semantic references or definitions.
	mapInput := json.RawMessage(fmt.Sprintf(`{"path":%q,"include_signatures":true}`, root))
	mapOutput, mapErr := executeRepoMap(mapInput, workDir, ToolExecutionOptions{Context: context.Background(), ExecutionPolicy: policy})
	if mapErr {
		return "LSP unavailable (semantic result not available): " + reason
	}
	return "LSP unavailable (semantic result not available): " + reason + "\n\nRepoMap fallback (structural only; not semantic):\n" + mapOutput
}

func newLSPClient(ctx context.Context, executable, workDir string, opts LSPExecutionOptions) (*lspClient, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Runner == nil || opts.Policy.Enforcement == "" {
		return nil, errors.New("secure language-server process runner is not configured")
	}
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = defaultLSPTimeout
	}
	if opts.Policy.Enforcement == sandbox.EnforcementDisabled {
		if _, ok := opts.Runner.(*processsupervisor.HostRunner); !ok {
			return nil, errors.New("disabled language-server execution requires the explicit host adapter")
		}
	}
	workDir = lspCanonicalPath(workDir)
	environment, err := lspEnvironmentSpec()
	if err != nil {
		return nil, errors.New("language-server environment is invalid")
	}
	process, report := opts.Runner.Start(ctx, opts.Policy, processsupervisor.Spec{
		Executable:      executable,
		Args:            append([]string(nil), opts.Args...),
		Dir:             workDir,
		Environment:     environment,
		ExecutionPolicy: opts.ExecutionPolicy,
	})
	if opts.ObserveStart != nil {
		opts.ObserveStart(report)
	}
	if process == nil || !report.Started {
		if strings.TrimSpace(report.Detail) != "" {
			return nil, fmt.Errorf("%s", report.Detail)
		}
		return nil, errors.New("language-server process did not start")
	}
	go drainLSPStderr(process.Stderr(), process)
	return &lspClient{
		process:       process,
		stdin:         process.Stdin(),
		stdout:        bufio.NewReaderSize(process.Stdout(), 64*1024),
		root:          workDir,
		timeout:       opts.CallTimeout,
		diagnosticsMu: newSyncDiagnostics(),
	}, nil
}

// lspEnvironmentSpec keeps the child environment deny-by-default while giving
// Go language servers a writable build cache. The parent process intentionally
// does not expose ambient cache/user variables to supervised children; without
// an explicit GOCACHE, go/packages fails before semantic analysis starts.
func lspEnvironmentSpec() (sandbox.EnvironmentSpec, error) {
	environment, err := mcpEnvironmentSpec(nil)
	if err != nil {
		return sandbox.EnvironmentSpec{}, err
	}
	cacheDir := filepath.Join(os.TempDir(), "corelay-lsp-cache")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return sandbox.EnvironmentSpec{}, err
	}
	if environment.Set == nil {
		environment.Set = make(map[string]string, 1)
	}
	environment.Set["GOCACHE"] = cacheDir
	return environment, nil
}

func drainLSPStderr(reader io.Reader, process *processsupervisor.Process) {
	if reader == nil {
		return
	}
	written, _ := io.CopyN(io.Discard, reader, maxMCPDiagnosticBytes+1)
	if written <= maxMCPDiagnosticBytes || process == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = process.Stop(ctx)
	cancel()
}

func (c *lspClient) initialize(ctx context.Context) error {
	rootURI := lspURI(c.root)
	_, err := c.request(ctx, "initialize", map[string]any{
		"processId":        nil,
		"clientInfo":       map[string]string{"name": "corelay-code", "version": "1"},
		"rootUri":          rootURI,
		"workspaceFolders": []map[string]string{{"uri": rootURI, "name": filepath.Base(c.root)}},
		"capabilities": map[string]any{
			"workspace":    map[string]any{"workspaceFolders": true},
			"textDocument": map[string]any{"publishDiagnostics": map[string]any{"relatedInformation": false}},
		},
		"trace": "off",
	})
	if err != nil {
		return err
	}
	return c.notify(ctx, "initialized", map[string]any{})
}

func (c *lspClient) didOpen(ctx context.Context, uri string, content []byte) error {
	c.diagnosticsMu.selectDocument(uri)
	return c.notify(ctx, "textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{"uri": uri, "languageId": "go", "version": 1, "text": string(content)},
	})
}

func (c *lspClient) execute(ctx context.Context, args lspInput, uri string) (string, error) {
	position := lspPosition{Line: lspMaxInt(args.Line-1, 0), Character: lspMaxInt(args.Column-1, 0)}
	switch args.Operation {
	case "definition":
		result, err := c.request(ctx, "textDocument/definition", map[string]any{"textDocument": map[string]string{"uri": uri}, "position": position})
		if err != nil {
			return "", err
		}
		locations, err := decodeLSPLocations(result)
		if err != nil {
			return "", err
		}
		return renderLSPLocations("LSP definition", locations, args.MaxResults), nil
	case "references":
		result, err := c.request(ctx, "textDocument/references", map[string]any{"textDocument": map[string]string{"uri": uri}, "position": position, "context": map[string]bool{"includeDeclaration": true}})
		if err != nil {
			return "", err
		}
		locations, err := decodeLSPLocations(result)
		if err != nil {
			return "", err
		}
		return renderLSPLocations("LSP references", locations, args.MaxResults), nil
	case "diagnostics":
		c.diagnosticsMu.selectDocument(uri)
		result, requestErr := c.request(ctx, "textDocument/diagnostic", map[string]any{"textDocument": map[string]string{"uri": uri}})
		if requestErr == nil {
			var report struct {
				Items []lspDiagnostic `json:"items"`
			}
			if err := json.Unmarshal(result, &report); err == nil {
				c.diagnosticsMu.set(uri, report.Items)
			}
		}
		items, received := c.diagnosticsMu.snapshot()
		if !received && requestErr != nil {
			// Some gopls versions expose diagnostics only as publishDiagnostics
			// notifications. The request failure is therefore non-fatal when the
			// server delivered a notification payload.
			return "", requestErr
		}
		return renderLSPDiagnostics(args.FilePath, items, args.MaxResults), nil
	default:
		return "", errors.New("unsupported LSP operation")
	}
}

func (c *lspClient) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	requestCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	id := c.nextID.Add(1)
	if id <= 0 {
		return nil, errors.New("LSP request ID exhausted")
	}
	frame, err := json.Marshal(lspMessage{JSONRPC: "2.0", ID: json.RawMessage(strconv.FormatInt(id, 10)), Method: method, Params: mustJSON(params)})
	if err != nil {
		return nil, err
	}
	if _, err := decodeUniqueJSONObject(frame); err != nil || validateMCPJSONDepth(frame, defaultMCPJSONDepth) != nil {
		return nil, errors.New("invalid bounded LSP request")
	}
	if err := c.writeFrame(requestCtx, frame); err != nil {
		return nil, err
	}
	return c.readResponse(requestCtx, id)
}

func (c *lspClient) notify(ctx context.Context, method string, params any) error {
	if ctx == nil {
		ctx = context.Background()
	}
	notifyCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	frame, err := json.Marshal(lspMessage{JSONRPC: "2.0", Method: method, Params: mustJSON(params)})
	if err != nil {
		return err
	}
	if _, err := decodeUniqueJSONObject(frame); err != nil || validateMCPJSONDepth(frame, defaultMCPJSONDepth) != nil {
		return errors.New("invalid bounded LSP notification")
	}
	return c.writeFrame(notifyCtx, frame)
}

func (c *lspClient) writeFrame(ctx context.Context, frame []byte) error {
	if len(frame) > maxLSPFrameBytes {
		return errors.New("LSP request exceeds frame limit")
	}
	encoded := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(frame))
	writeDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(c.stdin, bytes.NewReader(append([]byte(encoded), frame...)))
		writeDone <- err
	}()
	select {
	case err := <-writeDone:
		return err
	case <-ctx.Done():
		c.stopAfterCancel()
		return ctx.Err()
	}
}

func (c *lspClient) readResponse(ctx context.Context, id int64) (json.RawMessage, error) {
	type response struct {
		result json.RawMessage
		err    error
	}
	responses := make(chan response, 1)
	go func() {
		for {
			frame, err := readLSPFrame(c.stdout)
			if err != nil {
				responses <- response{err: err}
				return
			}
			if _, err := decodeUniqueJSONObject(frame); err != nil || validateMCPJSONDepth(frame, defaultMCPJSONDepth) != nil {
				responses <- response{err: errors.New("invalid bounded LSP JSON envelope")}
				return
			}
			var message lspMessage
			if err := json.Unmarshal(frame, &message); err != nil || message.JSONRPC != "2.0" {
				responses <- response{err: errors.New("invalid LSP response")}
				return
			}
			if message.Method != "" {
				c.handleNotification(message)
				continue
			}
			if string(message.ID) != strconv.FormatInt(id, 10) {
				continue
			}
			if message.Error != nil {
				responses <- response{err: fmt.Errorf("LSP request failed: %s", sanitizeLSPText(message.Error.Message))}
				return
			}
			responses <- response{result: append(json.RawMessage(nil), message.Result...)}
			return
		}
	}()
	select {
	case result := <-responses:
		return result.result, result.err
	case <-ctx.Done():
		c.stopAfterCancel()
		return nil, ctx.Err()
	}
}

func (c *lspClient) stopAfterCancel() {
	if c == nil || c.process == nil {
		return
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = c.process.Stop(stopCtx)
	c.process.CloseIO()
	cancel()
}

func (c *lspClient) handleNotification(message lspMessage) {
	if message.Method != "textDocument/publishDiagnostics" {
		return
	}
	var params struct {
		URI         string          `json:"uri"`
		Diagnostics []lspDiagnostic `json:"diagnostics"`
	}
	if json.Unmarshal(message.Params, &params) == nil {
		c.diagnosticsMu.set(params.URI, params.Diagnostics)
	}
}

func (c *lspClient) close() {
	if c == nil || c.closed.Swap(true) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if c.process != nil {
		// shutdown is best effort. A canceled or crashed server is still
		// terminated by the supervisor below, and no write is attempted after
		// the process has been marked closed.
		_, _ = c.request(ctx, "shutdown", nil)
		_ = c.notify(ctx, "exit", nil)
		_ = c.process.Stop(ctx)
		c.process.CloseIO()
	}
}

func readLSPFrame(reader *bufio.Reader) ([]byte, error) {
	contentLength := -1
	headerBytes := 0
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		headerBytes += len(line)
		if headerBytes > maxLSPHeaderBytes {
			return nil, errors.New("LSP header exceeds limit")
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 || !strings.EqualFold(strings.TrimSpace(parts[0]), "Content-Length") {
			continue
		}
		value, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil || value < 0 || value > maxLSPFrameBytes {
			return nil, errors.New("invalid LSP content length")
		}
		contentLength = value
	}
	if contentLength < 0 {
		return nil, errors.New("LSP response has no content length")
	}
	frame := make([]byte, contentLength)
	if _, err := io.ReadFull(reader, frame); err != nil {
		return nil, err
	}
	return frame, nil
}

func decodeLSPLocations(raw json.RawMessage) ([]lspLocation, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var locations []lspLocation
	if err := json.Unmarshal(raw, &locations); err == nil {
		valid := len(locations) == 0
		for _, location := range locations {
			if strings.TrimSpace(location.URI) != "" {
				valid = true
				break
			}
		}
		if valid {
			return locations, nil
		}
	}
	var one lspLocation
	if err := json.Unmarshal(raw, &one); err == nil && strings.TrimSpace(one.URI) != "" {
		return []lspLocation{one}, nil
	}
	{
		var links []struct {
			TargetURI   string   `json:"targetUri"`
			TargetRange lspRange `json:"targetSelectionRange"`
		}
		if json.Unmarshal(raw, &links) != nil {
			return nil, errors.New("invalid LSP location result")
		}
		for _, link := range links {
			if strings.TrimSpace(link.TargetURI) == "" {
				continue
			}
			locations = append(locations, lspLocation{URI: link.TargetURI, Range: link.TargetRange})
		}
		return locations, nil
	}
}

func renderLSPLocations(title string, locations []lspLocation, maximum int) string {
	if len(locations) > maximum {
		locations = locations[:maximum]
	}
	sort.SliceStable(locations, func(i, j int) bool {
		return locations[i].URI < locations[j].URI || locations[i].URI == locations[j].URI && locations[i].Range.Start.Line < locations[j].Range.Start.Line
	})
	var b strings.Builder
	fmt.Fprintf(&b, "%s (semantic)\n", title)
	if len(locations) == 0 {
		b.WriteString("No semantic locations found.\n")
		return b.String()
	}
	for _, location := range locations {
		fmt.Fprintf(&b, "- %s:%d:%d\n", lspPath(location.URI), location.Range.Start.Line+1, location.Range.Start.Character+1)
	}
	return b.String()
}

func renderLSPDiagnostics(filePath string, items []lspDiagnostic, maximum int) string {
	if len(items) > maximum {
		items = items[:maximum]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "LSP diagnostics (semantic) for %s\n", filePath)
	if len(items) == 0 {
		b.WriteString("No diagnostics found.\n")
		return b.String()
	}
	for _, item := range items {
		message := sanitizeLSPText(item.Message)
		if len(message) > maxLSPDiagnosticMessage {
			message = message[:maxLSPDiagnosticMessage] + "..."
		}
		severity := lspDiagnosticSeverity(item.Severity)
		fmt.Fprintf(&b, "- %s:%d:%d [%s] %s\n", filePath, item.Range.Start.Line+1, item.Range.Start.Character+1, severity, message)
	}
	return b.String()
}

func lspDiagnosticSeverity(value int) string {
	switch value {
	case 1:
		return "error"
	case 2:
		return "warning"
	case 3:
		return "information"
	case 4:
		return "hint"
	default:
		return "unknown"
	}
}

func lspURI(path string) string {
	path = lspCanonicalPath(path)
	path = filepath.ToSlash(path)
	if runtime.GOOS == "windows" && len(path) >= 2 && path[1] == ':' {
		path = "/" + path
	}
	return (&url.URL{Scheme: "file", Path: path}).String()
}

func lspPath(uri string) string {
	parsed, err := url.Parse(uri)
	if err != nil || !strings.EqualFold(parsed.Scheme, "file") {
		return sanitizeLSPText(uri)
	}
	path := parsed.Path
	if parsed.Host != "" {
		path = "//" + parsed.Host + path
	}
	if runtime.GOOS == "windows" && strings.HasPrefix(path, "/") && len(path) > 2 && path[2] == ':' {
		path = path[1:]
	}
	return filepath.FromSlash(path)
}

func sanitizeLSPText(value string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\t' {
			return ' '
		}
		return r
	}, strings.TrimSpace(value))
}

func lspMaxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}
