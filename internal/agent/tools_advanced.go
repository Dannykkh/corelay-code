package agent

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Dannykkh/corelay-code/internal/types"
)

// AdvancedToolDefs returns high-level tools.
func AdvancedToolDefs() []types.ToolDef {
	return []types.ToolDef{
		{
			Name:        "ImageRead",
			Description: "Read an image file and return its metadata. For vision-capable models, the image content is included.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"file_path": {"type": "string", "description": "Path to image file (png, jpg, svg, webp)"}
				},
				"required": ["file_path"]
			}`),
		},
		{
			Name:        "PDFRead",
			Description: "Read a PDF file and extract its text content.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"file_path": {"type": "string", "description": "Path to PDF file"},
					"pages": {"type": "string", "description": "Page range, e.g. '1-5' or '3'"}
				},
				"required": ["file_path"]
			}`),
		},
		{
			Name:        "Lint",
			Description: "Run linter/formatter on files. Auto-detects project type (go vet, eslint, ruff, cargo clippy, etc.).",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "File or directory to lint (default: current directory)"},
					"fix": {"type": "boolean", "description": "Auto-fix issues if possible"}
				}
			}`),
		},
		{
			Name:        "Test",
			Description: "Run tests. Auto-detects test framework (go test, jest, pytest, cargo test, etc.).",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "Test file or directory"},
					"filter": {"type": "string", "description": "Test name filter/pattern"}
				}
			}`),
		},
		{
			Name:        "GitDiff",
			Description: "Show git diff with context. More user-friendly than raw git diff.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"staged": {"type": "boolean", "description": "Show staged changes only"},
					"file": {"type": "string", "description": "Specific file to diff"},
					"commit": {"type": "string", "description": "Compare with specific commit"}
				}
			}`),
		},
		{
			Name:        "GitCommit",
			Description: "Stage and commit changes with a message. Shows diff before committing.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"message": {"type": "string", "description": "Commit message"},
					"files": {"type": "string", "description": "Files to stage (space-separated, or '.' for all)"},
					"amend": {"type": "boolean", "description": "Amend the last commit"}
				},
				"required": ["message"]
			}`),
		},
		{
			Name:        "HTTPRequest",
			Description: "Make an HTTP request (GET, POST, PUT, DELETE). Useful for testing APIs.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"method": {"type": "string", "description": "HTTP method: GET, POST, PUT, DELETE"},
					"url": {"type": "string", "description": "URL to request"},
					"body": {"type": "string", "description": "Request body (JSON string)"},
					"headers": {"type": "object", "description": "Request headers"}
				},
				"required": ["url"]
			}`),
		},
		{
			Name:        "Diff",
			Description: "Compare two files and show differences.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"file_a": {"type": "string", "description": "First file path"},
					"file_b": {"type": "string", "description": "Second file path"}
				},
				"required": ["file_a", "file_b"]
			}`),
		},
	}
}

// ExecuteAdvancedTool handles advanced tools.
func ExecuteAdvancedTool(name string, input json.RawMessage, workDir string) (string, bool, bool) {
	return ExecuteAdvancedToolWithOptions(name, input, workDir, ToolExecutionOptions{})
}

// ExecuteAdvancedToolWithOptions binds every one-shot process to the caller's
// sandbox runner, immutable policy, context, and report observer.
func ExecuteAdvancedToolWithOptions(name string, input json.RawMessage, workDir string, opts ToolExecutionOptions) (string, bool, bool) {
	switch name {
	case "ImageRead":
		r, e := executeImageRead(input, workDir)
		return r, e, true
	case "PDFRead":
		r, e := executePDFRead(input, workDir, opts)
		return r, e, true
	case "Lint":
		r, e := executeLint(input, workDir, opts)
		return r, e, true
	case "Test":
		r, e := executeTestWithOptions(input, workDir, opts)
		return r, e, true
	case "GitDiff":
		r, e := executeGitDiff(input, workDir, opts)
		return r, e, true
	case "GitCommit":
		r, e := executeGitCommit(input, workDir, opts)
		return r, e, true
	case "HTTPRequest":
		r, e := executeHTTPRequest(input, workDir, opts)
		return r, e, true
	case "Diff":
		r, e := executeDiff(input, workDir, opts)
		return r, e, true
	default:
		return "", false, false
	}
}

// ── Image Read ──

func executeImageRead(input json.RawMessage, workDir string) (string, bool) {
	paths, err := executionToolWorkspacePaths("ImageRead", input, workDir)
	if err != nil {
		return "Image read blocked: " + err.Error(), true
	}
	var args struct {
		FilePath string `json:"file_path"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "Image read blocked: invalid input: " + err.Error(), true
	}

	path := paths.one("file_path")
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}

	ext := strings.ToLower(filepath.Ext(path))
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("Error reading: %v", err), true
	}

	b64 := base64.StdEncoding.EncodeToString(data)
	return fmt.Sprintf("Image: %s\nType: %s\nSize: %s\nBase64 length: %d chars\n\n[Image data available for vision models]",
		args.FilePath, ext, formatSize(info.Size()), len(b64)), false
}

// ── PDF Read ──

func executePDFRead(input json.RawMessage, workDir string, opts ToolExecutionOptions) (string, bool) {
	paths, err := executionToolWorkspacePaths("PDFRead", input, workDir)
	if err != nil {
		return "PDF read blocked: " + err.Error(), true
	}
	var args struct {
		FilePath string `json:"file_path"`
		Pages    string `json:"pages"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "PDF read blocked: invalid input: " + err.Error(), true
	}

	path := paths.one("file_path")

	// Try pdftotext first
	cmdArgs := []string{path, "-"}
	if args.Pages != "" {
		parts := strings.Split(args.Pages, "-")
		if len(parts) == 2 {
			cmdArgs = []string{"-f", parts[0], "-l", parts[1], path, "-"}
		} else {
			cmdArgs = []string{"-f", args.Pages, "-l", args.Pages, path, "-"}
		}
	}

	process := runToolProcess(opts, "PDFRead", workDir, "pdftotext", cmdArgs, defaultToolProcessTimeout)
	if process.policyOrContextFailure() {
		return "PDF read failed: " + process.setupOrExecutionError("sandbox execution failed"), true
	}
	if !process.Started || process.ExitCode != 0 || process.Err != nil {
		// Fallback uses a fixed Python program plus a separate path argument. No
		// shell interpolation is involved, so quotes in the filename stay data.
		python, probe := resolvePythonExecutableWithOptions([]string{"python3", "python", "py"}, workDir, opts)
		if python == "" {
			return "PDF reading requires 'pdftotext' or 'PyPDF2': " + probe.setupOrExecutionError("no usable Python interpreter"), true
		}
		const extractPDF = "import PyPDF2, sys; r=PyPDF2.PdfReader(sys.argv[1]); print('\\n'.join((p.extract_text() or '') for p in r.pages))"
		process = runToolProcess(opts, "PDFRead Python fallback", workDir, python, []string{"-c", extractPDF, path}, defaultToolProcessTimeout)
		if process.policyOrContextFailure() || !process.Started || process.ExitCode != 0 || process.Err != nil {
			return "PDF reading requires 'pdftotext' or 'PyPDF2': " + process.setupOrExecutionError("Python PDF extraction failed"), true
		}
	}

	result := process.combinedOutput()
	if len(result) > 50000 {
		result = result[:50000] + "\n... (truncated)"
	}
	return fmt.Sprintf("PDF: %s\n\n%s", args.FilePath, result), false
}

// ── Auto Lint ──

func executeLint(input json.RawMessage, workDir string, opts ToolExecutionOptions) (string, bool) {
	paths, err := executionToolWorkspacePaths("Lint", input, workDir)
	if err != nil {
		return "Lint blocked: " + err.Error(), true
	}
	var args struct {
		Path string `json:"path"`
		Fix  bool   `json:"fix"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "Lint blocked: invalid input: " + err.Error(), true
	}
	target := "."
	if args.Path != "" {
		target = paths.one("path")
	}

	project := DetectProject(workDir)
	var command string
	var commandArgs []string

	switch project.Type {
	case "go":
		if args.Fix {
			format := runToolProcess(opts, "Lint gofmt", workDir, "gofmt", []string{"-w", target}, defaultToolProcessTimeout)
			if format.policyOrContextFailure() || !format.Started || format.ExitCode != 0 || format.Err != nil {
				return format.setupOrExecutionError("gofmt failed"), true
			}
			command = "go"
			commandArgs = []string{"vet", lintGoTarget(args.Path, target)}
		} else {
			command = "go"
			commandArgs = []string{"vet", lintGoTarget(args.Path, target)}
		}
	case "node":
		command = "npx"
		if args.Fix {
			commandArgs = []string{"eslint", "--fix", target}
		} else {
			commandArgs = []string{"eslint", target}
		}
	case "python":
		command = "ruff"
		if args.Fix {
			commandArgs = []string{"check", "--fix", target}
		} else {
			commandArgs = []string{"check", target}
		}
	case "rust":
		command = "cargo"
		commandArgs = []string{"clippy"}
	default:
		return fmt.Sprintf("No linter configured for project type: %s", project.Type), false
	}

	process := runToolProcess(opts, "Lint", workDir, command, commandArgs, defaultToolProcessTimeout)
	result := process.combinedOutput()

	if process.policyOrContextFailure() || !process.Started {
		return process.setupOrExecutionError("linter did not start"), true
	}
	if process.ExitCode != 0 || process.Err != nil {
		return process.setupOrExecutionError("lint found issues"), true
	}
	if result == "" {
		return "No lint issues found. ✅", false
	}
	return result, false
}

func lintGoTarget(rawPath, canonical string) string {
	if strings.TrimSpace(rawPath) == "" {
		return "./..."
	}
	return canonical
}

// ── Auto Test ──

var pythonVersionRe = regexp.MustCompile(`(?i)^python \d+\.\d+`)

// looksLikePythonVersion reports whether `--version` output came from a real
// interpreter. Windows ships a python3.exe App Execution Alias that resolves on
// PATH and exits 0, but prints a bare "Python" and runs nothing — so presence on
// PATH is not evidence that the name works.
func looksLikePythonVersion(out []byte) bool {
	return pythonVersionRe.Match(bytes.TrimSpace(out))
}

var (
	pythonExecOnce sync.Once
	pythonExecName string
)

// pythonExecutable is retained for callers that do not have a tool execution
// context. Model-facing routes use resolvePythonExecutableWithOptions so the
// probe shares their sandbox and cancellation context.
func pythonExecutable() string {
	pythonExecOnce.Do(func() {
		pythonExecName = resolvePythonExecutable([]string{"python3", "python", "py"})
	})
	return pythonExecName
}

func resolvePythonExecutable(candidates []string) string {
	workDir, err := os.Getwd()
	if err != nil {
		workDir = "."
	}
	runner, policy := DefaultSandboxExecution(workDir)
	name, _ := resolvePythonExecutableWithOptions(candidates, workDir, ToolExecutionOptions{
		SandboxRunner: runner,
		SandboxPolicy: policy,
	})
	if name != "" {
		return name
	}
	// Nothing verified; return the conventional name so the failure surfaces as
	// a normal exec error instead of an empty command.
	return "python3"
}

func resolvePythonExecutableWithOptions(
	candidates []string,
	workDir string,
	opts ToolExecutionOptions,
) (string, toolProcessResult) {
	var last toolProcessResult
	for _, name := range candidates {
		last = runToolProcess(opts, "Python interpreter probe", workDir, name, []string{"--version"}, 10*time.Second)
		if last.policyOrContextFailure() {
			return "", last
		}
		if last.Started && last.ExitCode == 0 && last.Err == nil && looksLikePythonVersion([]byte(last.combinedOutput())) {
			return name, last
		}
	}
	return "", last
}

// executeTest is the internal auto-verify compatibility route. It still uses
// an explicit secure platform default; model tool calls use the option-bearing
// variant below so their runner, policy, context, and report sink are retained.
func executeTest(input json.RawMessage, workDir string) (string, bool) {
	runner, policy := DefaultSandboxExecution(workDir)
	return executeTestWithOptions(input, workDir, ToolExecutionOptions{
		SandboxRunner: runner,
		SandboxPolicy: policy,
	})
}

func executeTestWithOptions(input json.RawMessage, workDir string, opts ToolExecutionOptions) (string, bool) {
	paths, err := executionToolWorkspacePaths("Test", input, workDir)
	if err != nil {
		return "Test blocked: " + err.Error(), true
	}
	var args struct {
		Path   string `json:"path"`
		Filter string `json:"filter"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "Test blocked: invalid input: " + err.Error(), true
	}

	project := DetectProject(workDir)
	var command string
	var commandArgs []string

	switch project.Type {
	case "go":
		testArgs := []string{"test"}
		if args.Filter != "" {
			testArgs = append(testArgs, "-run", args.Filter)
		}
		if args.Path != "" {
			testArgs = append(testArgs, paths.one("path"))
		} else {
			testArgs = append(testArgs, "./...")
		}
		command = "go"
		commandArgs = testArgs
	case "node":
		testArgs := []string{"test"}
		if args.Filter != "" {
			testArgs = append(testArgs, "--", "--grep", args.Filter)
		}
		command = "npm"
		commandArgs = testArgs
	case "python":
		testArgs := []string{"-m", "pytest", "-v"}
		if args.Filter != "" {
			testArgs = append(testArgs, "-k", args.Filter)
		}
		if args.Path != "" {
			testArgs = append(testArgs, paths.one("path"))
		}
		python, probe := resolvePythonExecutableWithOptions([]string{"python3", "python", "py"}, workDir, opts)
		if python == "" {
			return "Python test runner unavailable: " + probe.setupOrExecutionError("no usable Python interpreter"), true
		}
		command = python
		commandArgs = testArgs
	case "rust":
		testArgs := []string{"test"}
		if args.Filter != "" {
			testArgs = append(testArgs, "--", args.Filter)
		}
		command = "cargo"
		commandArgs = testArgs
	default:
		// Reported as an error on purpose: nothing ran. Returning this as a
		// success let a model treat "no runner configured" as a passing test
		// run and stop there.
		return fmt.Sprintf("No test runner configured for project type %q. Run the test command directly with Bash.", project.Type), true
	}

	process := runToolProcess(opts, "Test", workDir, command, commandArgs, defaultToolProcessTimeout)
	result := process.combinedOutput()
	if process.policyOrContextFailure() || !process.Started {
		return process.setupOrExecutionError("test process did not start"), true
	}
	if result == "" {
		result = "No test output (possibly no test files)"
	}
	if len(result) > 50000 {
		result = result[:50000] + "\n... (truncated)"
	}
	if process.ExitCode != 0 || process.Err != nil {
		return result + "\n[tests failed]", true
	}
	return result, false
}

// ── Git Diff (formatted) ──

func executeGitDiff(input json.RawMessage, workDir string, opts ToolExecutionOptions) (string, bool) {
	paths, err := executionToolWorkspacePaths("GitDiff", input, workDir)
	if err != nil {
		return "Git diff blocked: " + err.Error(), true
	}
	var args struct {
		Staged bool   `json:"staged"`
		File   string `json:"file"`
		Commit string `json:"commit"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "Git diff blocked: invalid input: " + err.Error(), true
	}

	cmdArgs := []string{"diff", "--stat"}
	if args.Staged {
		cmdArgs = append(cmdArgs, "--cached")
	}
	if args.Commit != "" {
		cmdArgs = append(cmdArgs, args.Commit)
	}
	if args.File != "" {
		cmdArgs = append(cmdArgs, "--", paths.one("file"))
	}

	// Get stat
	statProcess := runToolProcess(opts, "GitDiff stat", workDir, "git", cmdArgs, defaultToolProcessTimeout)
	if statProcess.policyOrContextFailure() || !statProcess.Started || statProcess.ExitCode != 0 || statProcess.Err != nil {
		return "Git diff failed: " + statProcess.setupOrExecutionError("git diff --stat failed"), true
	}

	// Get full diff
	diffArgs := append([]string{"diff", "--color=never"}, cmdArgs[2:]...)
	diffProcess := runToolProcess(opts, "GitDiff", workDir, "git", diffArgs, defaultToolProcessTimeout)
	if diffProcess.policyOrContextFailure() || !diffProcess.Started || diffProcess.ExitCode != 0 || diffProcess.Err != nil {
		return "Git diff failed: " + diffProcess.setupOrExecutionError("git diff failed"), true
	}

	result := "Diff Summary:\n" + statProcess.combinedOutput() + "\n" + diffProcess.combinedOutput()
	if len(result) > 30000 {
		result = result[:30000] + "\n... (truncated)"
	}
	return result, false
}

// ── Git Commit ──

func executeGitCommit(input json.RawMessage, workDir string, opts ToolExecutionOptions) (string, bool) {
	paths, err := executionToolWorkspacePaths("GitCommit", input, workDir)
	if err != nil {
		return "Git commit blocked: " + err.Error(), true
	}
	var args struct {
		Message string `json:"message"`
		Files   string `json:"files"`
		Amend   bool   `json:"amend"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "Git commit blocked: invalid input: " + err.Error(), true
	}

	// Stage files
	if args.Files != "" {
		files := paths.many("files")
		add := runToolProcess(
			opts,
			"GitCommit stage",
			workDir,
			"git",
			append([]string{"add", "--"}, files...),
			defaultToolProcessTimeout,
		)
		if add.policyOrContextFailure() || !add.Started || add.ExitCode != 0 || add.Err != nil {
			return "Stage failed: " + add.setupOrExecutionError("git add failed"), true
		}
	}

	// Commit
	commitArgs := []string{"commit", "-m", args.Message}
	if args.Amend {
		commitArgs = append(commitArgs, "--amend")
	}

	commit := runToolProcess(opts, "GitCommit", workDir, "git", commitArgs, defaultToolProcessTimeout)
	if commit.policyOrContextFailure() || !commit.Started || commit.ExitCode != 0 || commit.Err != nil {
		return commit.setupOrExecutionError("git commit failed"), true
	}
	return commit.combinedOutput(), false
}

// ── HTTP Request ──

func executeHTTPRequest(input json.RawMessage, workDir string, opts ToolExecutionOptions) (string, bool) {
	var args struct {
		Method  string            `json:"method"`
		URL     string            `json:"url"`
		Body    string            `json:"body"`
		Headers map[string]string `json:"headers"`
	}
	json.Unmarshal(input, &args)

	if args.Method == "" {
		args.Method = "GET"
	}

	curlArgs := []string{"-s", "-w", "\n\nHTTP %{http_code} | %{time_total}s", "-X", args.Method}
	headerNames := make([]string, 0, len(args.Headers))
	for name := range args.Headers {
		headerNames = append(headerNames, name)
	}
	sort.Strings(headerNames)
	for _, k := range headerNames {
		v := args.Headers[k]
		curlArgs = append(curlArgs, "-H", fmt.Sprintf("%s: %s", k, v))
	}
	if args.Body != "" {
		curlArgs = append(curlArgs, "-d", args.Body, "-H", "Content-Type: application/json")
	}
	curlArgs = append(curlArgs, args.URL)

	process := runToolProcess(opts, "HTTPRequest", workDir, "curl", curlArgs, defaultToolProcessTimeout)
	result := process.combinedOutput()
	if process.policyOrContextFailure() || !process.Started || process.ExitCode != 0 || process.Err != nil {
		return process.setupOrExecutionError("HTTP request failed") + "\n[request failed]", true
	}
	if len(result) > 20000 {
		result = result[:20000] + "\n... (truncated)"
	}
	return result, false
}

// ── File Diff ──

func executeDiff(input json.RawMessage, workDir string, opts ToolExecutionOptions) (string, bool) {
	paths, err := executionToolWorkspacePaths("Diff", input, workDir)
	if err != nil {
		return "Diff blocked: " + err.Error(), true
	}
	var args struct {
		FileA string `json:"file_a"`
		FileB string `json:"file_b"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "Diff blocked: invalid input: " + err.Error(), true
	}

	pathA := paths.one("file_a")
	pathB := paths.one("file_b")

	process := runToolProcess(opts, "Diff", workDir, "diff", []string{"-u", pathA, pathB}, defaultToolProcessTimeout)
	if process.policyOrContextFailure() || !process.Started {
		return "Diff failed: " + process.setupOrExecutionError("diff did not start"), true
	}
	// diff exit 1 means the files differ; values above 1 are execution errors.
	if process.ExitCode > 1 || (process.Err != nil && process.ExitCode == 0) {
		return "Diff failed: " + process.setupOrExecutionError("diff command failed"), true
	}
	result := process.combinedOutput()
	if result == "" {
		return "Files are identical.", false
	}
	return result, false
}
