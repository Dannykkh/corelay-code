package agent

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/aniclew/aniclew/internal/types"
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

// AllToolDefs returns base + extended + computer use tool definitions.
func AllToolDefs(workDir string) []types.ToolDef {
	all := append(ToolDefs(workDir), ExtendedToolDefs()...)
	all = append(all, ComputerUseToolDefs()...)
	all = append(all, AdvancedToolDefs()...)

	// Add MCP tools dynamically
	for _, t := range GetMCPTools() {
		all = append(all, types.ToolDef{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}

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
	WorkerID         string
	OwnershipChecker func(workerID, filePath string) (bool, string)
}

// ExecuteTool runs a tool and returns the result text.
func ExecuteTool(name string, input json.RawMessage, workDir string) (string, bool) {
	return ExecuteToolWithOptions(name, input, workDir, ToolExecutionOptions{})
}

func ExecuteToolWithOptions(name string, input json.RawMessage, workDir string, opts ToolExecutionOptions) (string, bool) {
	// ── Air-gap: refuse internet-egress tools (defense in depth — they are
	//    already stripped from AllToolDefs, this catches forced/cached calls) ──
	if OfflineMode() && egressTools[name] {
		return fmt.Sprintf("[OFFLINE] %s is disabled in air-gap mode (ANICLEW_OFFLINE is set). No outbound internet calls are allowed.", name), true
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
	// Try extended tools first
	if result, isErr, handled := ExecuteExtendedTool(name, input, workDir); handled {
		return result, isErr
	}

	// Try Advanced tools
	if advResult, advErr, advHandled := ExecuteAdvancedTool(name, input, workDir); advHandled {
		return advResult, advErr
	}

	// Try Computer Use tools
	if cuResult, cuErr, cuHandled := ExecuteComputerUseTool(name, input, workDir); cuHandled {
		return cuResult, cuErr
	}

	// Try MCP tools
	mcpResult, mcpErr := CallMCPTool(name, input)
	if mcpResult != fmt.Sprintf("MCP tool '%s' not found", name) {
		return mcpResult, mcpErr
	}

	// Base tools (V2 improved)
	switch name {
	case "Bash":
		return executeBashV2(input, workDir)
	case "Read":
		return executeReadV2(input, workDir)
	case "Write":
		return executeWriteV2(input, workDir)
	case "Edit":
		return executeEditV2(input, workDir)
	case "Glob":
		return executeGlobV2(input, workDir)
	case "Grep":
		return executeGrepV2(input, workDir)
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

func resolvePath(path, workDir string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(workDir, path)
}
