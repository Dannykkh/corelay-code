package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Dannykkh/corelay-code/internal/harness"
	"github.com/Dannykkh/corelay-code/internal/sandbox"
	"github.com/Dannykkh/corelay-code/internal/types"
)

// ToolDefs returns the tool definitions sent to the LLM.
func ToolDefs(workDir string) []types.ToolDef {
	return []types.ToolDef{
		{
			Name:        "Bash",
			Description: "Execute a bash command and return its output. Use for running tests, installing packages, git operations, etc.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"command": {"type": "string", "description": "The bash command to execute"}
				},
				"required": ["command"]
			}`),
		},
		{
			Name:        "Read",
			Description: "Read a file from the filesystem. Returns the file contents with line numbers.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"file_path": {"type": "string", "description": "Absolute or relative path to the file"},
					"offset": {"type": "integer", "description": "Line number to start reading from (0-based)"},
					"limit": {"type": "integer", "description": "Number of lines to read"}
				},
				"required": ["file_path"]
			}`),
		},
		{
			Name:        "Write",
			Description: "Create a new file or overwrite an existing file with the given content.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"file_path": {"type": "string", "description": "Absolute or relative path to write"},
					"content": {"type": "string", "description": "The full content to write"}
				},
				"required": ["file_path", "content"]
			}`),
		},
		{
			Name:        "Edit",
			Description: "Replace a string in a file. Supports exact match, replace_all, and regex mode.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"file_path": {"type": "string", "description": "Path to the file to edit"},
					"old_string": {"type": "string", "description": "The string to find and replace"},
					"new_string": {"type": "string", "description": "The replacement string"},
					"replace_all": {"type": "boolean", "description": "Replace all occurrences (default: false, first only)"},
					"regex": {"type": "boolean", "description": "Treat old_string as regex pattern"}
				},
				"required": ["file_path", "old_string", "new_string"]
			}`),
		},
		{
			Name:        "Glob",
			Description: "Find files matching a glob pattern. Supports recursive '**' patterns (e.g., '**/*.go', 'src/**/*.ts').",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"pattern": {"type": "string", "description": "Glob pattern to match (supports **)"},
					"path": {"type": "string", "description": "Directory to search in (default: working directory)"}
				},
				"required": ["pattern"]
			}`),
		},
		{
			Name:        "Grep",
			Description: "Search file contents using regex. Supports ripgrep features: context lines, case-insensitive, file type filter.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"pattern": {"type": "string", "description": "Regex pattern to search for"},
					"path": {"type": "string", "description": "File or directory to search in"},
					"glob": {"type": "string", "description": "Only search files matching this glob (e.g., '*.go')"},
					"context": {"type": "integer", "description": "Lines of context around each match"},
					"ignore_case": {"type": "boolean", "description": "Case-insensitive search"},
					"files_only": {"type": "boolean", "description": "Only show matching file names"}
				},
				"required": ["pattern"]
			}`),
		},
	}
}

const loadToolResultToolName = "LoadToolResult"

func loadToolResultDefinition() types.ToolDef {
	return types.ToolDef{
		Name: loadToolResultToolName,
		Description: "Read one bounded chunk of a large tool result referenced by result ID. " +
			"Use offset and limit to page; returned content is redacted and never exceeds the safe chunk limit.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"id": {"type": "string", "description": "The result_<sha256> ID from a stored tool result"},
				"offset": {"type": "integer", "minimum": 0, "description": "Byte offset in the stored result (default 0)"},
				"limit": {"type": "integer", "minimum": 1, "maximum": 32768, "description": "Maximum source bytes to read (default 8192)"}
			},
			"required": ["id"],
			"additionalProperties": false
		}`),
	}
}

// AllToolDefs returns the production catalog for the currently configured
// composition. No host interaction driver exists at this legacy entry point,
// so desktop tools are not advertised merely to fail after consuming a model
// tool budget and an operator approval.
func AllToolDefs(workDir string) []types.ToolDef {
	return AllToolDefsWithHostExecution(workDir, nil, HostInteractionPolicy{})
}

// StaticToolDefs returns the catalog that does not depend on process-global
// MCP state. RunLoop composes run-owned MCP definitions onto this base so one
// session can never inherit another session's connected clients.
func StaticToolDefs(workDir string) []types.ToolDef {
	return staticToolDefsWithHostExecution(workDir, nil, HostInteractionPolicy{})
}

func staticToolDefsWithResultReader(workDir string, reader ToolResultReader) []types.ToolDef {
	all := StaticToolDefs(workDir)
	if reader != nil {
		all = append(all, loadToolResultDefinition())
	}
	return all
}

// AllToolDefsWithHostExecution retains the existing catalog composition point
// while adding only the desktop actions the concrete driver and immutable
// policy truthfully support. FileManager remains unavailable because workspace
// mutations belong to the normal permission/read-ledger/sandbox pipeline.
func AllToolDefsWithHostExecution(
	workDir string,
	hostDriver HostInteractionDriver,
	hostPolicy HostInteractionPolicy,
) []types.ToolDef {
	all := staticToolDefsWithHostExecution(workDir, hostDriver, hostPolicy)

	// Add MCP tools dynamically for legacy callers. Production RunLoop uses a
	// run-owned MCPRuntime and never reaches this global compatibility view.
	for _, t := range GetMCPTools() {
		all = append(all, types.ToolDef{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}

	return filterEgressTools(all)
}

func staticToolDefsWithHostExecution(
	workDir string,
	hostDriver HostInteractionDriver,
	hostPolicy HostInteractionPolicy,
) []types.ToolDef {
	all := append(ToolDefs(workDir), ExtendedToolDefs()...)
	all = append(all, availableComputerUseToolDefs(hostDriver, hostPolicy)...)
	all = append(all, AdvancedToolDefs()...)

	// In air-gap mode, never advertise internet-egress tools.
	return filterEgressTools(all)
}

// FileOwnershipChecker is the legacy fallback ownership guard. Team workers use
// explicit ToolExecutionOptions so parallel workers do not share worker IDs.
// Returns (allowed, reason). If nil, all writes are allowed.
var FileOwnershipChecker func(workerID, filePath string) (bool, string)

// activeWorkerID is the legacy fallback worker identifier.
var activeWorkerID string

type ToolExecutionOptions struct {
	WorkerID          string
	OwnershipChecker  func(workerID, filePath string) (bool, string)
	Context           context.Context
	SandboxRunner     sandbox.Runner
	SandboxPolicy     sandbox.Policy
	ObserveSandbox    func(sandbox.Report)
	EditPolicy        harness.EditPolicy
	HostDriver        HostInteractionDriver
	HostPolicy        HostInteractionPolicy
	ExpectedSessionID string
	ExpectedRunID     string
	ObserveHost       func(HostInteractionReport)
	ToolResultReader  ToolResultReader
	// CompletionContract and CompletionEvidenceResolver are run-owned strict
	// completion state. ReportCompletion additionally requires the private
	// catalog proof minted from the dispatcher's identity-bound envelope.
	CompletionContract         *CompletionContract
	CompletionEvidenceResolver CompletionEvidenceResolver
	hostApproval               hostInteractionApprovalProof
	pluginApproval             pluginApprovalProof
	fileMutation               *fileMutationPrecondition
	completionReportProof      *completionReportCatalogProof
}

// ExecuteTool runs a tool and returns the result text.
func ExecuteTool(name string, input json.RawMessage, workDir string) (string, bool) {
	legacy := legacyUnconfinedBashOptions(nil)
	return ExecuteToolWithOptions(name, input, workDir, ToolExecutionOptions{
		Context:       legacy.Context,
		SandboxRunner: legacy.Runner,
		SandboxPolicy: legacy.Policy,
	})
}

func ExecuteToolWithOptions(name string, input json.RawMessage, workDir string, opts ToolExecutionOptions) (string, bool) {
	pluginInput, pluginApproval, pluginApprovalBound, err := unwrapPluginApprovalExecutionInput(input)
	if err != nil {
		return "[PLUGIN BLOCKED] " + err.Error(), true
	}
	if pluginApprovalBound {
		input = pluginInput
		opts.pluginApproval = pluginApproval
	}
	hostInput, hostApproval, hostBound, err := unwrapHostInteractionExecutionInput(input)
	if err != nil {
		return "[HOST INTERACTION BLOCKED] " + err.Error(), true
	}
	if hostBound {
		if !isHostInteractionTool(name) {
			return "[HOST INTERACTION BLOCKED] approval proof was attached to a non-host tool", true
		}
		input = hostInput
		opts.hostApproval = hostApproval
	}
	mutationInput, mutationPrecondition, mutationBound, err := unwrapFileMutationExecutionInput(input, name)
	if err != nil {
		return "[MUTATION BLOCKED] " + err.Error(), true
	}
	if mutationBound {
		if name != "Write" && name != "Edit" {
			return "[MUTATION BLOCKED] file mutation precondition was attached to a non-mutation tool", true
		}
		input = mutationInput
		opts.fileMutation = mutationPrecondition
	}
	boundInput, expectedIdentity, identityBound, err := unwrapBoundToolExecutionInput(input)
	if err != nil {
		return "[IDENTITY BLOCKED] " + err.Error(), true
	}
	if identityBound {
		if expectedIdentity.ToolName != name {
			return fmt.Sprintf(
				"[IDENTITY BLOCKED] bound tool %q cannot execute as %q",
				expectedIdentity.ToolName,
				name,
			), true
		}
		input = boundInput
		if name == reportCompletionToolName {
			proof, valid := newCompletionReportCatalogProof(expectedIdentity, opts.ExpectedRunID)
			if !valid {
				return completionReportReceiptJSON(nil, 0), true
			}
			opts.completionReportProof = &proof
		}
	}
	if pluginApprovalBound && (!identityBound || expectedIdentity.Kind != toolExecutorPlugin) {
		return "[PLUGIN BLOCKED] plugin approval proof was attached to a non-plugin executor", true
	}

	// ── Air-gap: refuse internet-egress tools (defense in depth — they are
	//    already stripped from AllToolDefs, this catches forced/cached calls) ──
	if OfflineMode() && egressTools[name] {
		return fmt.Sprintf("[OFFLINE] %s is disabled in air-gap mode (CORELAY_OFFLINE is set). No outbound internet calls are allowed.", name), true
	}

	// ── File ownership enforcement for write tools ──
	ownershipChecker := opts.OwnershipChecker
	if ownershipChecker == nil {
		ownershipChecker = FileOwnershipChecker
	}
	workerID := strings.TrimSpace(opts.WorkerID)
	if workerID == "" {
		workerID = activeWorkerID
	}
	if ownershipChecker != nil && workerID != "" {
		if name == "Write" || name == "Edit" {
			var args struct {
				FilePath string `json:"file_path"`
			}
			json.Unmarshal(input, &args)
			if args.FilePath != "" {
				allowed, reason := ownershipChecker(workerID, args.FilePath)
				if !allowed {
					return fmt.Sprintf("[OWNERSHIP BLOCKED] %s", reason), true
				}
			}
		}
	}

	if identityBound {
		return executeIdentityBoundTool(name, input, workDir, expectedIdentity, opts)
	}
	return executeLegacyTool(name, input, workDir, opts)
}

func executeIdentityBoundTool(
	name string,
	input json.RawMessage,
	workDir string,
	expected toolExecutorIdentity,
	opts ToolExecutionOptions,
) (string, bool) {
	switch expected.Kind {
	case toolExecutorBuiltIn:
		// A collision that appeared after approval still fails closed. Once this
		// check completes, dispatch goes directly to the selected built-in and no
		// later MCP mutation can redirect it.
		if catalogUsesProcessGlobalMCP(expected.CatalogMarker) && mcpToolNameClaimed(name) {
			return fmt.Sprintf("[IDENTITY BLOCKED] MCP executor collides with built-in tool %q", name), true
		}
		return executeBuiltInTool(name, input, workDir, opts)
	case toolExecutorMCP:
		return callMCPToolBoundWithContext(opts.Context, name, input, expected)
	case toolExecutorPlugin:
		return executeBoundPluginTool(name, input, workDir, expected, opts)
	case toolExecutorOther:
		return fmt.Sprintf("[IDENTITY BLOCKED] tool %q has no identity-bound executor", name), true
	default:
		return fmt.Sprintf("[IDENTITY BLOCKED] tool %q has invalid executor provenance", name), true
	}
}

func executeLegacyTool(name string, input json.RawMessage, workDir string, opts ToolExecutionOptions) (string, bool) {
	if _, builtIn := currentBuiltInToolDefinitions()[name]; builtIn {
		if mcpToolNameClaimed(name) {
			return fmt.Sprintf("[IDENTITY BLOCKED] MCP executor collides with built-in tool %q", name), true
		}
		return executeBuiltInTool(name, input, workDir, opts)
	}

	mcpResult, mcpErr := CallMCPTool(name, input)
	if mcpResult != fmt.Sprintf("MCP tool '%s' not found", name) {
		return mcpResult, mcpErr
	}
	return fmt.Sprintf("Unknown tool: %s", name), true
}

func executeBuiltInTool(name string, input json.RawMessage, workDir string, opts ToolExecutionOptions) (string, bool) {
	if result, isErr, handled := ExecuteExtendedToolWithOptions(name, input, workDir, opts); handled {
		return result, isErr
	}
	if result, isErr, handled := ExecuteAdvancedToolWithOptions(name, input, workDir, opts); handled {
		return result, isErr
	}
	if result, isErr, handled := ExecuteComputerUseToolWithOptions(name, input, workDir, HostInteractionExecutionOptions{
		Context:           opts.Context,
		Driver:            opts.HostDriver,
		Policy:            opts.HostPolicy,
		ExpectedSessionID: opts.ExpectedSessionID,
		ExpectedRunID:     opts.ExpectedRunID,
		ObserveReport:     opts.ObserveHost,
		approval:          opts.hostApproval,
	}); handled {
		return result, isErr
	}
	return executeBaseTool(name, input, workDir, opts)
}

func executeBaseTool(name string, input json.RawMessage, workDir string, opts ToolExecutionOptions) (string, bool) {
	switch name {
	case "Bash":
		return executeBashV2WithOptions(
			input,
			workDir,
			opts.Context,
			opts.SandboxRunner,
			opts.SandboxPolicy,
			opts.ObserveSandbox,
		)
	case "Read":
		return executeReadV2(input, workDir)
	case "Write":
		return executeWriteV2WithOptions(input, workDir, opts)
	case "Edit":
		return executeEditV2WithOptions(input, workDir, opts)
	case "Glob":
		return executeGlobV2(input, workDir)
	case "Grep":
		return executeGrepV2WithOptions(input, workDir, opts)
	case loadToolResultToolName:
		return executeLoadToolResult(input, opts.ToolResultReader)
	case reportCompletionToolName:
		return executeReportCompletion(input, opts)
	case "WebFetch":
		return executeWebFetch(input, workDir)
	case "WebSearch":
		return executeWebSearch(input, workDir)
	case "WebResearch":
		return executeWebResearch(input, workDir)
	default:
		return fmt.Sprintf("Unknown tool: %s", name), true
	}
}

func executeLoadToolResult(input json.RawMessage, reader ToolResultReader) (string, bool) {
	if reader == nil {
		return "Stored tool results are unavailable for this run", true
	}
	var args struct {
		ID     string `json:"id"`
		Offset int64  `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(input, &args); err != nil || strings.TrimSpace(args.ID) == "" {
		return "Invalid stored tool result request", true
	}
	chunk, err := reader.ReadResult(args.ID, args.Offset, args.Limit)
	if err != nil {
		return "Stored tool result is unavailable or the requested range is invalid", true
	}
	encoded, err := json.Marshal(chunk)
	if err != nil {
		return "Stored tool result could not be encoded", true
	}
	return string(encoded), false
}

func resolvePath(path, workDir string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(workDir, path)
}
