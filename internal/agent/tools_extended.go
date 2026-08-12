package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/Dannykkh/corelay-code/internal/types"
)

// ExtendedToolDefs returns additional tool definitions beyond the base 6.
func ExtendedToolDefs() []types.ToolDef {
	return []types.ToolDef{
		// ── Web Tools ──
		{
			Name:        "WebSearch",
			Description: "Search the web and return structured top results with title, URL, snippet, and source. Uses the built-in DuckDuckGo provider by default; provider=ollama uses Ollama web_search when OLLAMA_API_KEY is set.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"query": {"type": "string", "description": "Search query"},
					"max_results": {"type": "integer", "description": "Maximum results to return (default 5, max 10)"},
					"provider": {"type": "string", "description": "Search provider: auto, multi, duckduckgo, google, naver, naver-web, naver-blog, bing, yahoo, ollama"},
					"providers": {"type": "array", "items": {"type": "string"}, "description": "Explicit provider list for multi-search"},
					"sort": {"type": "string", "description": "Ranking mode: relevance or latest"},
					"recency": {"type": "string", "description": "Freshness filter hint: day, week, month, year"},
					"date_from": {"type": "string", "description": "Earliest desired result date, YYYY-MM-DD"},
					"date_to": {"type": "string", "description": "Latest desired result date, YYYY-MM-DD"}
				},
				"required": ["query"]
			}`),
		},
		{
			Name:        "WebFetch",
			Description: "Fetch a webpage URL and return cleaned readable text, title, links, and metadata. Uses direct HTTP fetch by default; provider=ollama uses Ollama web_fetch when OLLAMA_API_KEY is set.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"url": {"type": "string", "description": "URL to fetch"},
					"prompt": {"type": "string", "description": "What to extract from the page"},
					"max_chars": {"type": "integer", "description": "Maximum content characters to return (default 12000, max 30000)"},
					"provider": {"type": "string", "description": "Fetch provider: auto, ollama, direct"}
				},
				"required": ["url"]
			}`),
		},
		{
			Name:        "WebResearch",
			Description: "Search the web, fetch the top pages, and return compact cited context. Use when current information or multi-source web evidence is needed.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"query": {"type": "string", "description": "Research query"},
					"max_results": {"type": "integer", "description": "Maximum search results (default 5, max 10)"},
					"fetch_top": {"type": "integer", "description": "How many top results to fetch (default 3)"},
					"max_chars": {"type": "integer", "description": "Maximum fetched content characters to return (default 12000, max 30000)"},
					"provider": {"type": "string", "description": "Provider preference: auto, multi, duckduckgo, google, naver, naver-web, naver-blog, bing, yahoo, ollama"},
					"providers": {"type": "array", "items": {"type": "string"}, "description": "Explicit provider list for multi-search"},
					"sort": {"type": "string", "description": "Ranking mode: relevance or latest"},
					"recency": {"type": "string", "description": "Freshness filter hint: day, week, month, year"},
					"date_from": {"type": "string", "description": "Earliest desired result date, YYYY-MM-DD"},
					"date_to": {"type": "string", "description": "Latest desired result date, YYYY-MM-DD"}
				},
				"required": ["query"]
			}`),
		},
		// ── Git Tools ──
		{
			Name:        "Git",
			Description: "Run git commands. Read-only commands may run automatically; mutating commands require explicit permission approval.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"command": {"type": "string", "description": "Git subcommand: status, diff, log, branch, add, commit, push, show, blame, stash"},
					"args": {"type": "string", "description": "Additional arguments"}
				},
				"required": ["command"]
			}`),
		},
		// ── Directory Listing ──
		{
			Name:        "LS",
			Description: "List directory contents with file sizes and types.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "Directory path (default: working directory)"}
				}
			}`),
		},
		// ── Task Management ──
		{
			Name:        "TaskCreate",
			Description: "Create a task to track work progress. Returns task ID.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"subject": {"type": "string", "description": "Brief task title"},
					"description": {"type": "string", "description": "What needs to be done"}
				},
				"required": ["subject"]
			}`),
		},
		{
			Name:        "TaskUpdate",
			Description: "Update a task's status: pending, in_progress, completed.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"id": {"type": "string", "description": "Task ID"},
					"status": {"type": "string", "description": "New status: pending, in_progress, completed"}
				},
				"required": ["id", "status"]
			}`),
		},
		{
			Name:        "TaskList",
			Description: "List all active tasks and their status.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {}
			}`),
		},
		// ── Notebook ──
		{
			Name:        "NotebookRead",
			Description: "Read a Jupyter notebook (.ipynb) and display its cells.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"file_path": {"type": "string", "description": "Path to .ipynb file"}
				},
				"required": ["file_path"]
			}`),
		},
	}
}

// ExecuteExtendedTool handles the extended tools.
func ExecuteExtendedTool(name string, input json.RawMessage, workDir string) (string, bool, bool) {
	return ExecuteExtendedToolWithOptions(name, input, workDir, ToolExecutionOptions{})
}

// ExecuteExtendedToolWithOptions preserves the common process execution
// contract for extended tools that start a child process.
func ExecuteExtendedToolWithOptions(name string, input json.RawMessage, workDir string, opts ToolExecutionOptions) (string, bool, bool) {
	switch name {
	case "WebSearch":
		r, e := executeWebSearch(input, workDir)
		return r, e, true
	case "WebFetch":
		r, e := executeWebFetch(input, workDir)
		return r, e, true
	case "WebResearch":
		r, e := executeWebResearch(input, workDir)
		return r, e, true
	case "Git":
		r, e := executeGit(input, workDir, opts)
		return r, e, true
	case "LS":
		r, e := executeLS(input, workDir)
		return r, e, true
	case "TaskCreate":
		r, e := executeTaskCreate(input)
		return r, e, true
	case "TaskUpdate":
		r, e := executeTaskUpdate(input)
		return r, e, true
	case "TaskList":
		r, e := executeTaskList()
		return r, e, true
	case "NotebookRead":
		r, e := executeNotebookRead(input, workDir)
		return r, e, true
	case "NotebookEdit":
		r, e := executeNotebookEdit(input, workDir)
		return r, e, true
	default:
		return "", false, false // not handled
	}
}

// WebSearch, WebFetch, and WebResearch are implemented in tool_web.go

// ── Git ──

func executeGit(input json.RawMessage, workDir string, opts ToolExecutionOptions) (string, bool) {
	paths, err := executionToolWorkspacePaths("Git", input, workDir)
	if err != nil {
		return "Git blocked: " + err.Error(), true
	}
	var args struct {
		Command string `json:"command"`
		Args    string `json:"args"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "Git blocked: invalid input: " + err.Error(), true
	}
	cmdArgs := paths.many("arguments")

	// Safety: block force push and destructive resets
	fullCmd := strings.Join(cmdArgs, " ")
	if strings.Contains(fullCmd, "--force") || strings.Contains(fullCmd, "reset --hard") {
		return "Blocked: force push and hard reset are not allowed for safety.", true
	}

	process := runToolProcess(opts, "Git", workDir, "git", cmdArgs, defaultToolProcessTimeout)
	result := process.combinedOutput()
	if process.policyOrContextFailure() || !process.Started || process.ExitCode != 0 || process.Err != nil {
		result = process.setupOrExecutionError("git command failed") + "\n[git error]"
	}
	if len(result) > 30000 {
		result = result[:30000] + "\n... (truncated)"
	}
	return result, process.policyOrContextFailure() || !process.Started || process.ExitCode != 0 || process.Err != nil
}

// ── LS ──

func executeLS(input json.RawMessage, workDir string) (string, bool) {
	paths, err := executionToolWorkspacePaths("LS", input, workDir)
	if err != nil {
		return "LS blocked: " + err.Error(), true
	}
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "LS blocked: invalid input: " + err.Error(), true
	}
	dir := paths.one("path")

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Sprintf("Error listing %s: %v", dir, err), true
	}

	var lines []string
	for _, e := range entries {
		info, _ := e.Info()
		typeStr := "file"
		size := ""
		if e.IsDir() {
			typeStr = "dir "
		} else if info != nil {
			size = formatSize(info.Size())
		}
		lines = append(lines, fmt.Sprintf("  %s %8s  %s", typeStr, size, e.Name()))
	}
	return fmt.Sprintf("%s (%d items):\n%s", dir, len(entries), strings.Join(lines, "\n")), false
}

// ── Task Management (in-memory) ──

type taskItem struct {
	ID          string `json:"id"`
	Subject     string `json:"subject"`
	Description string `json:"description"`
	Status      string `json:"status"`
}

var taskStore = make(map[string]*taskItem)
var taskCounter = 0

func executeTaskCreate(input json.RawMessage) (string, bool) {
	var args struct {
		Subject     string `json:"subject"`
		Description string `json:"description"`
	}
	json.Unmarshal(input, &args)

	taskCounter++
	id := fmt.Sprintf("task-%d", taskCounter)
	taskStore[id] = &taskItem{
		ID: id, Subject: args.Subject, Description: args.Description, Status: "pending",
	}
	return fmt.Sprintf("Task %s created: %s", id, args.Subject), false
}

func executeTaskUpdate(input json.RawMessage) (string, bool) {
	var args struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	json.Unmarshal(input, &args)

	task, ok := taskStore[args.ID]
	if !ok {
		return fmt.Sprintf("Task %s not found", args.ID), true
	}
	task.Status = args.Status
	return fmt.Sprintf("Task %s updated to: %s", args.ID, args.Status), false
}

func executeTaskList() (string, bool) {
	if len(taskStore) == 0 {
		return "No tasks.", false
	}
	var lines []string
	for _, t := range taskStore {
		icon := "⬜"
		switch t.Status {
		case "in_progress":
			icon = "🔄"
		case "completed":
			icon = "✅"
		}
		lines = append(lines, fmt.Sprintf("%s %s [%s] %s", icon, t.ID, t.Status, t.Subject))
	}
	return strings.Join(lines, "\n"), false
}

// ── Notebook (.ipynb) ──

type notebookFile struct {
	Cells    []notebookCell         `json:"cells"`
	Metadata map[string]interface{} `json:"metadata"`
	Nbformat int                    `json:"nbformat"`
}

type notebookCell struct {
	CellType string        `json:"cell_type"`
	Source   []string      `json:"source"`
	Outputs  []interface{} `json:"outputs,omitempty"`
}

func executeNotebookRead(input json.RawMessage, workDir string) (string, bool) {
	paths, err := executionToolWorkspacePaths("NotebookRead", input, workDir)
	if err != nil {
		return "Notebook read blocked: " + err.Error(), true
	}
	var args struct {
		FilePath string `json:"file_path"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "Notebook read blocked: invalid input: " + err.Error(), true
	}

	path := paths.one("file_path")
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("Error reading notebook: %v", err), true
	}

	var nb notebookFile
	if err := json.Unmarshal(data, &nb); err != nil {
		return fmt.Sprintf("Error parsing notebook: %v", err), true
	}

	var lines []string
	for i, cell := range nb.Cells {
		source := strings.Join(cell.Source, "")
		lines = append(lines, fmt.Sprintf("--- Cell %d [%s] ---\n%s", i, cell.CellType, source))
	}
	return fmt.Sprintf("Notebook: %s (%d cells)\n\n%s", args.FilePath, len(nb.Cells), strings.Join(lines, "\n\n")), false
}

func executeNotebookEdit(input json.RawMessage, workDir string) (string, bool) {
	// Forced or cached calls fail closed too. Notebook mutation must be expressed
	// through the ordinary Read + Edit tools, which bind execution to a read
	// ledger revision and use the staged transactional mutation pipeline.
	if _, err := executionToolWorkspacePaths("NotebookEdit", input, workDir); err != nil {
		return "Notebook edit blocked: " + err.Error(), true
	}
	return "Notebook edit blocked: NotebookEdit is disabled; use Read followed by Edit", true
}

// ── Helpers ──

func formatSize(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%dB", bytes)
	} else if bytes < 1024*1024 {
		return fmt.Sprintf("%.1fKB", float64(bytes)/1024)
	}
	return fmt.Sprintf("%.1fMB", float64(bytes)/1024/1024)
}
