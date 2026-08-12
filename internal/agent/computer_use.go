package agent

import (
	"context"
	"crypto/hmac"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Dannykkh/corelay-code/internal/types"
)

const (
	hostInteractionExecutionProtocol       = "corelay.host-interaction.v1"
	legacyHostInteractionExecutionProtocol = "aniclew.host-interaction.v1"
)

// HostActionKind is a semantic host interaction. It is deliberately not a
// process or filesystem sandbox capability: implementations must truthfully
// report the desktop controls they provide.
type HostActionKind string

const (
	HostActionScreenshot  HostActionKind = "screenshot"
	HostActionMouseClick  HostActionKind = "mouse_click"
	HostActionTypeText    HostActionKind = "type_text"
	HostActionOpenApp     HostActionKind = "open_app"
	HostActionListWindows HostActionKind = "list_windows"
	HostActionFileManager HostActionKind = "file_manager"
	HostActionClipboard   HostActionKind = "clipboard"
)

// HostInteractionCapabilities is an immutable value snapshot supplied by a
// driver. Platform must be a GOOS value; the booleans describe only desktop
// interaction and do not imply process, network, or filesystem isolation.
type HostInteractionCapabilities struct {
	Platform       string `json:"platform"`
	WindowControl  bool   `json:"windowControl"`
	InputControl   bool   `json:"inputControl"`
	Screenshot     bool   `json:"screenshot"`
	Clipboard      bool   `json:"clipboard"`
	WorkspaceFiles bool   `json:"workspaceFiles"`
}

// HostInteractionPolicy selects the exact host capabilities allowed for one
// call. The zero value is invalid and never falls back to direct host access.
type HostInteractionPolicy struct {
	Enabled     bool
	Allowed     HostInteractionCapabilities
	CallTimeout time.Duration
}

// HostInteractionRequest is passed only to the explicitly configured driver.
// It may contain sensitive input, so implementations must never log it.
type HostInteractionRequest struct {
	Action          HostActionKind
	WorkDir         string
	Region          string
	X               int
	Y               int
	Button          string
	Text            string
	Key             string
	Target          string
	FileAction      string
	Source          string
	Destination     string
	Pattern         string
	ClipboardAction string
}

// HostInteractionResponse is an ephemeral driver response. Payload is digested
// and discarded at this boundary; it is never copied into reports or receipts.
type HostInteractionResponse struct {
	Payload []byte
	Width   int
	Height  int
}

// HostInteractionDriver is the only host-control execution boundary. Execute
// must honor ctx, must not spawn an implicit or detached fallback process, and
// must remove any temporary screenshot artifact before returning on success or
// failure. Screenshot bytes are returned in memory so paths cannot escape into
// tool results or recorder reports.
type HostInteractionDriver interface {
	Name() string
	Capabilities() HostInteractionCapabilities
	Execute(context.Context, HostInteractionRequest) (HostInteractionResponse, error)
}

type HostInteractionFailureCode string

const (
	HostFailureNone                  HostInteractionFailureCode = ""
	HostFailureConfigurationInvalid  HostInteractionFailureCode = "host_configuration_invalid"
	HostFailureApprovalRequired      HostInteractionFailureCode = "host_approval_required"
	HostFailureApprovalInvalid       HostInteractionFailureCode = "host_approval_invalid"
	HostFailureUnsupportedPlatform   HostInteractionFailureCode = "host_platform_unsupported"
	HostFailureCapabilityUnavailable HostInteractionFailureCode = "host_capability_unavailable"
	HostFailureInputInvalid          HostInteractionFailureCode = "host_input_invalid"
	HostFailureCanceled              HostInteractionFailureCode = "host_canceled"
	HostFailureTimedOut              HostInteractionFailureCode = "host_timeout"
	HostFailureExecutionFailed       HostInteractionFailureCode = "host_execution_failed"
)

// HostInteractionError is safe to surface. Detail is selected by this package
// and never includes raw driver errors, user input, screenshots, or secrets.
type HostInteractionError struct {
	Code   HostInteractionFailureCode
	Action HostActionKind
	Detail string
}

func (e *HostInteractionError) Error() string {
	if e == nil {
		return ""
	}
	if strings.TrimSpace(e.Detail) == "" {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Detail
}

// HostInteractionReport is the recorder-safe host boundary. It intentionally
// contains only action metadata and digests, never raw input or output.
type HostInteractionReport struct {
	Driver       string                     `json:"driver,omitempty"`
	Platform     string                     `json:"platform,omitempty"`
	Action       HostActionKind             `json:"action"`
	InputDigest  string                     `json:"inputDigest"`
	OutputDigest string                     `json:"outputDigest,omitempty"`
	OutputBytes  int                        `json:"outputBytes,omitempty"`
	Width        int                        `json:"width,omitempty"`
	Height       int                        `json:"height,omitempty"`
	Started      bool                       `json:"started"`
	Failure      HostInteractionFailureCode `json:"failure,omitempty"`
	Duration     time.Duration              `json:"duration"`
}

type HostInteractionExecutionOptions struct {
	Context           context.Context
	Driver            HostInteractionDriver
	Policy            HostInteractionPolicy
	ExpectedSessionID string
	ExpectedRunID     string
	ObserveReport     func(HostInteractionReport)
	approval          hostInteractionApprovalProof
}

type hostInteractionApprovalProof struct {
	ApprovalID  string `json:"approval_id"`
	SessionID   string `json:"session_id"`
	RunID       string `json:"run_id"`
	ToolName    string `json:"tool_name"`
	InputDigest string `json:"input_digest"`
	ExpiresAt   int64  `json:"expires_at"`
	Signature   string `json:"signature"`
}

type hostInteractionExecutionEnvelope struct {
	Protocol string                       `json:"protocol"`
	Approval hostInteractionApprovalProof `json:"approval"`
	Input    json.RawMessage              `json:"input"`
}

var hostInteractionProofKey struct {
	once sync.Once
	key  []byte
	err  error
}

var usedHostInteractionApprovals sync.Map

// ComputerUseToolDefs returns tools for desktop/browser control.
func ComputerUseToolDefs() []types.ToolDef {
	return []types.ToolDef{
		{
			Name:        "Screenshot",
			Description: "Take a screenshot of the current screen or a specific window. Returns privacy-safe image metadata.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"region": {"type": "string", "description": "Optional: 'full' (default), 'active' (active window only)"}
				}
			}`),
		},
		{
			Name:        "MouseClick",
			Description: "Click at a specific screen coordinate.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"x": {"type": "integer", "description": "X coordinate"},
					"y": {"type": "integer", "description": "Y coordinate"},
					"button": {"type": "string", "description": "'left' (default), 'right', 'double'"}
				},
				"required": ["x", "y"]
			}`),
		},
		{
			Name:        "TypeText",
			Description: "Type text at the current cursor position.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"text": {"type": "string", "description": "Text to type"},
					"key": {"type": "string", "description": "Special key: 'enter', 'tab', 'escape', 'backspace', 'ctrl+c', 'ctrl+v', etc."}
				}
			}`),
		},
		{
			Name:        "OpenApp",
			Description: "Open an application or URL on the desktop.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"target": {"type": "string", "description": "App name, file path, or URL to open"}
				},
				"required": ["target"]
			}`),
		},
		{
			Name:        "ListWindows",
			Description: "List all open windows/applications.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {}
			}`),
		},
		{
			Name:        "FileManager",
			Description: "Manage workspace files through an explicitly approved host interaction driver.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"action": {"type": "string", "description": "'copy', 'move', 'rename', 'delete', 'organize'"},
					"source": {"type": "string", "description": "Source path or glob pattern"},
					"destination": {"type": "string", "description": "Destination path"},
					"pattern": {"type": "string", "description": "For organize: group by 'extension', 'date', 'name'"}
				},
				"required": ["action", "source"]
			}`),
		},
		{
			Name:        "Clipboard",
			Description: "Read or write the system clipboard through an explicitly approved host interaction driver.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"action": {"type": "string", "description": "'read' or 'write'"},
					"text": {"type": "string", "description": "Text to write (for 'write' action)"}
				},
				"required": ["action"]
			}`),
		},
	}
}

func availableComputerUseToolDefs(
	driver HostInteractionDriver,
	policy HostInteractionPolicy,
) []types.ToolDef {
	if driver == nil || !policy.Enabled || policy.CallTimeout <= 0 ||
		policy.Allowed.Platform != runtime.GOOS || strings.TrimSpace(driver.Name()) == "" {
		return nil
	}
	driverCapabilities := driver.Capabilities()
	if driverCapabilities.Platform != runtime.GOOS {
		return nil
	}
	definitions := ComputerUseToolDefs()
	available := make([]types.ToolDef, 0, len(definitions))
	for _, definition := range definitions {
		action, ok := hostActionForTool(definition.Name)
		if !ok || action == HostActionFileManager {
			continue
		}
		required := requiredHostCapabilities(action)
		if hostCapabilitiesContain(policy.Allowed, required) &&
			hostCapabilitiesContain(driverCapabilities, required) {
			available = append(available, definition)
		}
	}
	return available
}

// ExecuteComputerUseTool is the compatibility entry point. Its zero
// configuration intentionally fails closed; direct host execution is no
// longer an implicit fallback.
func ExecuteComputerUseTool(name string, input json.RawMessage, workDir string) (string, bool, bool) {
	return ExecuteComputerUseToolWithOptions(name, input, workDir, HostInteractionExecutionOptions{})
}

// ExecuteComputerUseToolWithOptions validates an operator approval proof and
// exact capability snapshot before invoking the host driver.
func ExecuteComputerUseToolWithOptions(
	name string,
	input json.RawMessage,
	workDir string,
	opts HostInteractionExecutionOptions,
) (string, bool, bool) {
	action, handled := hostActionForTool(name)
	if !handled {
		return "", false, false
	}
	startedAt := time.Now()
	report := HostInteractionReport{
		Action:      action,
		InputDigest: hostInteractionInputDigest(name, input),
		Platform:    runtime.GOOS,
	}
	finish := func(response HostInteractionResponse, failure HostInteractionFailureCode, detail string) (string, bool, bool) {
		report.Failure = failure
		report.Duration = time.Since(startedAt)
		if len(response.Payload) > 0 {
			digest := sha256.Sum256(response.Payload)
			report.OutputDigest = "sha256:" + hex.EncodeToString(digest[:])
			report.OutputBytes = len(response.Payload)
		}
		report.Width = response.Width
		report.Height = response.Height
		if opts.ObserveReport != nil {
			opts.ObserveReport(report)
		}
		if failure != HostFailureNone {
			return (&HostInteractionError{Code: failure, Action: action, Detail: detail}).Error(), true, true
		}
		result := fmt.Sprintf("Host interaction completed: action=%s", action)
		if report.OutputDigest != "" {
			result += fmt.Sprintf(" output_digest=%s output_bytes=%d", report.OutputDigest, report.OutputBytes)
		}
		if response.Width > 0 && response.Height > 0 {
			result += fmt.Sprintf(" dimensions=%dx%d", response.Width, response.Height)
		}
		return result, false, true
	}

	request, err := parseHostInteractionRequest(name, input, workDir)
	if err != nil {
		return finish(HostInteractionResponse{}, HostFailureInputInvalid, "host action input is invalid")
	}
	if opts.Context == nil {
		return finish(HostInteractionResponse{}, HostFailureConfigurationInvalid, "host interaction context is not configured")
	}
	if opts.Driver == nil {
		return finish(HostInteractionResponse{}, HostFailureConfigurationInvalid, "host interaction driver is not configured")
	}
	if !opts.Policy.Enabled || opts.Policy.CallTimeout <= 0 || strings.TrimSpace(opts.Policy.Allowed.Platform) == "" {
		return finish(HostInteractionResponse{}, HostFailureConfigurationInvalid, "host interaction policy is not configured")
	}
	if opts.Policy.Allowed.Platform != runtime.GOOS {
		return finish(HostInteractionResponse{}, HostFailureUnsupportedPlatform, "host interaction policy does not support this platform")
	}

	driverCapabilities := opts.Driver.Capabilities()
	report.Driver = strings.TrimSpace(opts.Driver.Name())
	if strings.TrimSpace(report.Driver) == "" {
		return finish(HostInteractionResponse{}, HostFailureConfigurationInvalid, "host interaction driver identity is empty")
	}
	if driverCapabilities.Platform != runtime.GOOS {
		return finish(HostInteractionResponse{}, HostFailureUnsupportedPlatform, "host interaction driver does not support this platform")
	}
	required := requiredHostCapabilities(action)
	if !hostCapabilitiesContain(opts.Policy.Allowed, required) {
		return finish(HostInteractionResponse{}, HostFailureCapabilityUnavailable, "host interaction policy does not allow the required capability")
	}
	if !hostCapabilitiesContain(driverCapabilities, required) {
		return finish(HostInteractionResponse{}, HostFailureCapabilityUnavailable, "host interaction driver does not provide the required capability")
	}
	if action == HostActionFileManager {
		return finish(HostInteractionResponse{}, HostFailureCapabilityUnavailable, "FileManager host execution is disabled; use workspace-scoped file tools")
	}
	if strings.TrimSpace(opts.ExpectedSessionID) == "" || strings.TrimSpace(opts.ExpectedRunID) == "" {
		return finish(HostInteractionResponse{}, HostFailureConfigurationInvalid, "host approval execution binding is not configured")
	}
	if strings.TrimSpace(opts.approval.ApprovalID) == "" {
		return finish(HostInteractionResponse{}, HostFailureApprovalRequired, "explicit operator approval is required")
	}
	if err := validateHostInteractionApproval(
		opts.approval,
		name,
		input,
		opts.ExpectedSessionID,
		opts.ExpectedRunID,
	); err != nil {
		return finish(HostInteractionResponse{}, HostFailureApprovalInvalid, "operator approval proof is invalid")
	}

	if err := opts.Context.Err(); err != nil {
		return finish(HostInteractionResponse{}, hostContextFailure(err), hostContextFailureDetail(err))
	}
	ctx, cancel := context.WithTimeout(opts.Context, opts.Policy.CallTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return finish(HostInteractionResponse{}, hostContextFailure(err), hostContextFailureDetail(err))
	}

	report.Started = true
	response, driverErr := opts.Driver.Execute(ctx, request)
	if err := ctx.Err(); err != nil {
		return finish(HostInteractionResponse{}, hostContextFailure(err), hostContextFailureDetail(err))
	}
	if driverErr != nil {
		return finish(HostInteractionResponse{}, HostFailureExecutionFailed, "host interaction driver failed")
	}
	if len(response.Payload) > maxHostInteractionOutput(action) {
		return finish(HostInteractionResponse{}, HostFailureExecutionFailed, "host interaction output exceeded the safe bound")
	}
	if action == HostActionScreenshot && (response.Width <= 0 || response.Height <= 0 || len(response.Payload) == 0) {
		return finish(HostInteractionResponse{}, HostFailureExecutionFailed, "screenshot driver returned invalid metadata")
	}
	return finish(response, HostFailureNone, "")
}

func hostActionForTool(name string) (HostActionKind, bool) {
	switch name {
	case "Screenshot":
		return HostActionScreenshot, true
	case "MouseClick":
		return HostActionMouseClick, true
	case "TypeText":
		return HostActionTypeText, true
	case "OpenApp":
		return HostActionOpenApp, true
	case "ListWindows":
		return HostActionListWindows, true
	case "FileManager":
		return HostActionFileManager, true
	case "Clipboard":
		return HostActionClipboard, true
	default:
		return "", false
	}
}

func isHostInteractionTool(name string) bool {
	_, ok := hostActionForTool(name)
	return ok
}

func requiredHostCapabilities(action HostActionKind) HostInteractionCapabilities {
	required := HostInteractionCapabilities{Platform: runtime.GOOS}
	switch action {
	case HostActionScreenshot:
		required.WindowControl = true
		required.Screenshot = true
	case HostActionMouseClick, HostActionTypeText:
		required.WindowControl = true
		required.InputControl = true
	case HostActionOpenApp, HostActionListWindows:
		required.WindowControl = true
	case HostActionClipboard:
		required.Clipboard = true
	case HostActionFileManager:
		required.WorkspaceFiles = true
	}
	return required
}

func hostCapabilitiesContain(actual, required HostInteractionCapabilities) bool {
	return actual.Platform == required.Platform &&
		(!required.WindowControl || actual.WindowControl) &&
		(!required.InputControl || actual.InputControl) &&
		(!required.Screenshot || actual.Screenshot) &&
		(!required.Clipboard || actual.Clipboard) &&
		(!required.WorkspaceFiles || actual.WorkspaceFiles)
}

func parseHostInteractionRequest(name string, input json.RawMessage, workDir string) (HostInteractionRequest, error) {
	request := HostInteractionRequest{WorkDir: workDir}
	request.Action, _ = hostActionForTool(name)
	if len(input) > 256*1024 {
		return HostInteractionRequest{}, errors.New("host interaction input exceeds the safe bound")
	}
	object, err := decodeUniqueJSONObject(input)
	if err != nil {
		return HostInteractionRequest{}, err
	}
	switch name {
	case "Screenshot":
		var args struct {
			Region string `json:"region"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return HostInteractionRequest{}, err
		}
		if args.Region == "" {
			args.Region = "full"
		}
		if args.Region != "full" && args.Region != "active" {
			return HostInteractionRequest{}, errors.New("invalid screenshot region")
		}
		request.Region = args.Region

	case "MouseClick":
		if _, hasX := object["x"]; !hasX {
			return HostInteractionRequest{}, errors.New("x is required")
		}
		if _, hasY := object["y"]; !hasY {
			return HostInteractionRequest{}, errors.New("y is required")
		}
		var args struct {
			X      int    `json:"x"`
			Y      int    `json:"y"`
			Button string `json:"button"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return HostInteractionRequest{}, err
		}
		if args.Button == "" {
			args.Button = "left"
		}
		if args.Button != "left" && args.Button != "right" && args.Button != "double" {
			return HostInteractionRequest{}, errors.New("invalid mouse button")
		}
		request.X, request.Y, request.Button = args.X, args.Y, args.Button

	case "TypeText":
		var args struct {
			Text string `json:"text"`
			Key  string `json:"key"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return HostInteractionRequest{}, err
		}
		if (args.Text == "") == (args.Key == "") {
			return HostInteractionRequest{}, errors.New("exactly one text or key value is required")
		}
		if len(args.Text) > 64*1024 || len(args.Key) > 128 {
			return HostInteractionRequest{}, errors.New("host input exceeds the safe bound")
		}
		request.Text, request.Key = args.Text, args.Key

	case "OpenApp":
		var args struct {
			Target string `json:"target"`
		}
		if err := json.Unmarshal(input, &args); err != nil || strings.TrimSpace(args.Target) == "" || len(args.Target) > 8*1024 {
			return HostInteractionRequest{}, errors.New("target is required")
		}
		request.Target = args.Target

	case "ListWindows":
		var args map[string]json.RawMessage
		if err := json.Unmarshal(input, &args); err != nil || args == nil {
			return HostInteractionRequest{}, errors.New("input must be an object")
		}

	case "FileManager":
		var args struct {
			Action      string `json:"action"`
			Source      string `json:"source"`
			Destination string `json:"destination"`
			Pattern     string `json:"pattern"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return HostInteractionRequest{}, err
		}
		switch args.Action {
		case "copy", "move", "rename":
			if strings.TrimSpace(args.Destination) == "" {
				return HostInteractionRequest{}, errors.New("destination is required")
			}
		case "delete":
		case "organize":
			if args.Pattern != "extension" && args.Pattern != "date" && args.Pattern != "name" {
				return HostInteractionRequest{}, errors.New("invalid organize pattern")
			}
		default:
			return HostInteractionRequest{}, errors.New("invalid file action")
		}
		workspace, err := canonicalWorkspace(workDir)
		if err != nil {
			return HostInteractionRequest{}, err
		}
		source, err := canonicalPathWithinWorkspace(args.Source, workDir, workspace, DefaultPermissionConfig())
		if err != nil {
			return HostInteractionRequest{}, err
		}
		if args.Action == "delete" && source == workspace {
			return HostInteractionRequest{}, errors.New("workspace deletion is not allowed")
		}
		destination := ""
		if args.Destination != "" {
			destination, err = canonicalPathWithinWorkspace(args.Destination, workDir, workspace, DefaultPermissionConfig())
			if err != nil {
				return HostInteractionRequest{}, err
			}
		}
		request.WorkDir = workspace
		request.FileAction = args.Action
		request.Source = source
		request.Destination = destination
		request.Pattern = args.Pattern

	case "Clipboard":
		var args struct {
			Action string `json:"action"`
			Text   string `json:"text"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return HostInteractionRequest{}, err
		}
		if args.Action != "read" && args.Action != "write" {
			return HostInteractionRequest{}, errors.New("invalid clipboard action")
		}
		if len(args.Text) > 64*1024 {
			return HostInteractionRequest{}, errors.New("clipboard input exceeds the safe bound")
		}
		request.ClipboardAction, request.Text = args.Action, args.Text
	}
	return request, nil
}

func maxHostInteractionOutput(action HostActionKind) int {
	switch action {
	case HostActionScreenshot:
		return 32 * 1024 * 1024
	case HostActionClipboard:
		return 64 * 1024
	default:
		return 1024 * 1024
	}
}

func hostInteractionInputDigest(name string, input json.RawMessage) string {
	digest := sha256.Sum256(append([]byte(name+"\x00"), input...))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func ensureHostInteractionProofKey() ([]byte, error) {
	hostInteractionProofKey.once.Do(func() {
		key := make([]byte, 32)
		if _, err := cryptorand.Read(key); err != nil {
			hostInteractionProofKey.err = fmt.Errorf("create host approval proof key: %w", err)
			return
		}
		hostInteractionProofKey.key = key
	})
	if hostInteractionProofKey.err != nil {
		return nil, hostInteractionProofKey.err
	}
	if len(hostInteractionProofKey.key) != 32 {
		return nil, errors.New("host approval proof key is unavailable")
	}
	return hostInteractionProofKey.key, nil
}

func mintHostInteractionApproval(
	approvalID string,
	sessionID string,
	runID string,
	toolName string,
	input json.RawMessage,
	expiresAt time.Time,
) (hostInteractionApprovalProof, error) {
	proof := hostInteractionApprovalProof{
		ApprovalID:  strings.TrimSpace(approvalID),
		SessionID:   strings.TrimSpace(sessionID),
		RunID:       strings.TrimSpace(runID),
		ToolName:    toolName,
		InputDigest: hostInteractionInputDigest(toolName, input),
		ExpiresAt:   expiresAt.UnixNano(),
	}
	if proof.ApprovalID == "" || proof.SessionID == "" || proof.RunID == "" ||
		!isHostInteractionTool(toolName) || expiresAt.IsZero() || !expiresAt.After(time.Now()) {
		return hostInteractionApprovalProof{}, errors.New("host approval metadata is incomplete")
	}
	signature, err := signHostInteractionApproval(proof)
	if err != nil {
		return hostInteractionApprovalProof{}, err
	}
	proof.Signature = signature
	return proof, nil
}

func signHostInteractionApproval(proof hostInteractionApprovalProof) (string, error) {
	key, err := ensureHostInteractionProofKey()
	if err != nil {
		return "", err
	}
	proof.Signature = ""
	encoded, err := json.Marshal(proof)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(encoded)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func validateHostInteractionApproval(
	proof hostInteractionApprovalProof,
	name string,
	input json.RawMessage,
	expectedSessionID string,
	expectedRunID string,
) error {
	if proof.SessionID != expectedSessionID || proof.RunID != expectedRunID {
		return errors.New("host approval is not bound to the active session and run")
	}
	if proof.ToolName != name || proof.InputDigest != hostInteractionInputDigest(name, input) {
		return errors.New("host approval is not bound to this action")
	}
	if proof.ApprovalID == "" || proof.SessionID == "" || proof.RunID == "" || proof.Signature == "" {
		return errors.New("host approval metadata is incomplete")
	}
	if proof.ExpiresAt <= 0 || !time.Unix(0, proof.ExpiresAt).After(time.Now()) {
		return errors.New("host approval proof expired")
	}
	expected, err := signHostInteractionApproval(proof)
	if err != nil {
		return err
	}
	provided, err := hex.DecodeString(proof.Signature)
	if err != nil || !hmac.Equal(provided, mustDecodeHex(expected)) {
		return errors.New("host approval signature does not match")
	}
	cleanupExpiredHostInteractionApprovals(time.Now())
	if _, reused := usedHostInteractionApprovals.LoadOrStore(proof.Signature, proof.ExpiresAt); reused {
		return errors.New("host approval proof was already consumed")
	}
	return nil
}

func cleanupExpiredHostInteractionApprovals(now time.Time) {
	cutoff := now.UnixNano()
	usedHostInteractionApprovals.Range(func(key, value interface{}) bool {
		expiresAt, ok := value.(int64)
		if !ok || expiresAt <= cutoff {
			usedHostInteractionApprovals.Delete(key)
		}
		return true
	})
}

func mustDecodeHex(value string) []byte {
	decoded, _ := hex.DecodeString(value)
	return decoded
}

func bindHostInteractionExecutionInput(
	input json.RawMessage,
	approval hostInteractionApprovalProof,
) (json.RawMessage, error) {
	if strings.TrimSpace(approval.Signature) == "" {
		return nil, errors.New("host approval proof is empty")
	}
	encoded, err := json.Marshal(hostInteractionExecutionEnvelope{
		Protocol: hostInteractionExecutionProtocol,
		Approval: approval,
		Input:    append(json.RawMessage(nil), input...),
	})
	if err != nil {
		return nil, fmt.Errorf("encode host interaction execution: %w", err)
	}
	return encoded, nil
}

func unwrapHostInteractionExecutionInput(
	input json.RawMessage,
) (json.RawMessage, hostInteractionApprovalProof, bool, error) {
	var probe struct {
		Protocol string `json:"protocol"`
	}
	if json.Unmarshal(input, &probe) != nil ||
		!renamedProtocolMatches(probe.Protocol, hostInteractionExecutionProtocol, legacyHostInteractionExecutionProtocol) {
		return input, hostInteractionApprovalProof{}, false, nil
	}
	var envelope hostInteractionExecutionEnvelope
	if err := json.Unmarshal(input, &envelope); err != nil {
		return nil, hostInteractionApprovalProof{}, true, fmt.Errorf("decode host interaction execution: %w", err)
	}
	if len(envelope.Input) == 0 || strings.TrimSpace(envelope.Approval.Signature) == "" {
		return nil, hostInteractionApprovalProof{}, true, errors.New("host interaction execution envelope is incomplete")
	}
	return append(json.RawMessage(nil), envelope.Input...), envelope.Approval, true, nil
}

func hostContextFailure(err error) HostInteractionFailureCode {
	if errors.Is(err, context.DeadlineExceeded) {
		return HostFailureTimedOut
	}
	return HostFailureCanceled
}

func hostContextFailureDetail(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "host interaction deadline elapsed"
	}
	return "host interaction was canceled"
}
