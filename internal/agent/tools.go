package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

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

// FileOwnershipChecker is set by Team to enforce file ownership at execution time.
// Returns (allowed, reason). If nil, all writes are allowed.
var FileOwnershipChecker func(workerID, filePath string) (bool, string)

// activeWorkerID is set per-goroutine to identify the current worker.
var activeWorkerID string

// ExecuteTool runs a tool and returns the result text.
func ExecuteTool(name string, input json.RawMessage, workDir string) (string, bool) {
	// ── Air-gap: refuse internet-egress tools (defense in depth — they are
	//    already stripped from AllToolDefs, this catches forced/cached calls) ──
	if OfflineMode() && egressTools[name] {
		return fmt.Sprintf("[OFFLINE] %s is disabled in air-gap mode (ANICLEW_OFFLINE is set). No outbound internet calls are allowed.", name), true
	}

	// ── File ownership enforcement for write tools ──
	if FileOwnershipChecker != nil && activeWorkerID != "" {
		if name == "Write" || name == "Edit" {
			var args struct {
				FilePath string `json:"file_path"`
			}
			json.Unmarshal(input, &args)
			if args.FilePath != "" {
				allowed, reason := FileOwnershipChecker(activeWorkerID, args.FilePath)
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

// ── Tool implementations ──

func executeBash(input json.RawMessage, workDir string) (string, bool) {
	var args struct {
		Command string `json:"command"`
	}
	json.Unmarshal(input, &args)

	ctx := exec.Command("bash", "-c", args.Command)
	ctx.Dir = workDir
	ctx.Env = os.Environ()

	// Timeout
	timer := time.AfterFunc(30*time.Second, func() { ctx.Process.Kill() })
	defer timer.Stop()

	out, err := ctx.CombinedOutput()
	result := string(out)
	if err != nil {
		return result + "\n[exit: " + err.Error() + "]", true
	}
	// Truncate large output
	if len(result) > 50000 {
		result = result[:50000] + "\n... (truncated)"
	}
	return result, false
}

func executeRead(input json.RawMessage, workDir string) (string, bool) {
	var args struct {
		FilePath string `json:"file_path"`
		Offset   int    `json:"offset"`
		Limit    int    `json:"limit"`
	}
	json.Unmarshal(input, &args)

	path := resolvePath(args.FilePath, workDir)
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("Error reading %s: %v", args.FilePath, err), true
	}

	lines := strings.Split(string(data), "\n")

	start := args.Offset
	if start < 0 {
		start = 0
	}
	if start > len(lines) {
		start = len(lines)
	}

	end := len(lines)
	if args.Limit > 0 && start+args.Limit < end {
		end = start + args.Limit
	}

	var result strings.Builder
	for i := start; i < end; i++ {
		fmt.Fprintf(&result, "%d\t%s\n", i+1, lines[i])
	}
	return result.String(), false
}

func executeWrite(input json.RawMessage, workDir string) (string, bool) {
	var args struct {
		FilePath string `json:"file_path"`
		Content  string `json:"content"`
	}
	json.Unmarshal(input, &args)

	path := resolvePath(args.FilePath, workDir)
	dir := filepath.Dir(path)
	os.MkdirAll(dir, 0755)

	err := os.WriteFile(path, []byte(args.Content), 0644)
	if err != nil {
		return fmt.Sprintf("Error writing %s: %v", args.FilePath, err), true
	}
	return fmt.Sprintf("File written: %s (%d bytes)", args.FilePath, len(args.Content)), false
}

func executeEdit(input json.RawMessage, workDir string) (string, bool) {
	var args struct {
		FilePath  string `json:"file_path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}
	json.Unmarshal(input, &args)

	path := resolvePath(args.FilePath, workDir)
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("Error reading %s: %v", args.FilePath, err), true
	}

	content := string(data)
	if !strings.Contains(content, args.OldString) {
		return "Error: old_string not found in file", true
	}

	newContent := strings.Replace(content, args.OldString, args.NewString, 1)
	err = os.WriteFile(path, []byte(newContent), 0644)
	if err != nil {
		return fmt.Sprintf("Error writing %s: %v", args.FilePath, err), true
	}
	return fmt.Sprintf("File edited: %s", args.FilePath), false
}

func executeGlob(input json.RawMessage, workDir string) (string, bool) {
	var args struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	json.Unmarshal(input, &args)

	dir := workDir
	if args.Path != "" {
		dir = resolvePath(args.Path, workDir)
	}

	// Use find + glob via bash for recursive patterns
	cmd := exec.Command("bash", "-c", fmt.Sprintf("find %s -name '%s' 2>/dev/null | head -50", dir, filepath.Base(args.Pattern)))
	out, _ := cmd.CombinedOutput()
	result := strings.TrimSpace(string(out))
	if result == "" {
		return "No files found matching pattern: " + args.Pattern, false
	}
	return result, false
}

func executeGrep(input json.RawMessage, workDir string) (string, bool) {
	var args struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
		Glob    string `json:"glob"`
	}
	json.Unmarshal(input, &args)

	dir := workDir
	if args.Path != "" {
		dir = resolvePath(args.Path, workDir)
	}

	cmdArgs := []string{"-rn", "--color=never"}
	if args.Glob != "" {
		cmdArgs = append(cmdArgs, "--include="+args.Glob)
	}
	cmdArgs = append(cmdArgs, args.Pattern, dir)

	cmd := exec.Command("grep", cmdArgs...)
	out, _ := cmd.CombinedOutput()
	result := strings.TrimSpace(string(out))
	if len(result) > 30000 {
		result = result[:30000] + "\n... (truncated)"
	}
	if result == "" {
		return "No matches found", false
	}
	return result, false
}

func resolvePath(path, workDir string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(workDir, path)
}
