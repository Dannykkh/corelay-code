package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Dannykkh/corelay-code/internal/protocol"
	"github.com/Dannykkh/corelay-code/internal/types"
)

// AdvancedToolDefs returns high-level tools.
func AdvancedToolDefs() []types.ToolDef {
	return []types.ToolDef{
		{
			Name:        "ImageRead",
			Description: "Read a PNG, JPEG, GIF, or WebP file and attach its validated image content to the next model request. Unsupported image formats are rejected.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"file_path": {"type": "string", "description": "Path to image file (png, jpg/jpeg, gif, or webp)"}
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
			Description: "Show git diff with context. More user-friendly than raw git diff. The optional file target must be a literal file or directory path; wildcard and Git pathspec syntax are not supported.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"staged": {"type": "boolean", "description": "Show staged changes only"},
					"file": {"type": "string", "description": "Specific literal file or directory path to diff; wildcard and Git pathspec syntax are rejected"},
					"commit": {"type": "string", "description": "Compare with specific commit"}
				}
			}`),
		},
		{
			Name:        "GitCommit",
			Description: "Commit selected literal workspace paths from the working tree with a message. Unrelated staged changes stay staged. Partial staging on selected paths is rejected. To commit the entire existing staged index, omit files and explicitly set scope=staged.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"message": {"type": "string", "description": "Commit message"},
					"files": {"type": "string", "description": "Literal workspace paths separated by spaces; glob and Git pathspec syntax are not accepted"},
					"scope": {"type": "string", "enum": ["staged"], "description": "Explicitly commit the entire existing staged index; required when files is omitted"},
					"amend": {"type": "boolean", "description": "Explicitly amend the last commit; selected-path and staged-index safeguards still apply"}
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
		r, e := executeImageReadWithOptions(input, workDir, opts)
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
	return executeImageReadWithOptions(input, workDir, ToolExecutionOptions{})
}

func executeImageReadWithOptions(input json.RawMessage, workDir string, opts ToolExecutionOptions) (string, bool) {
	paths, err := executionToolWorkspacePathsWithPolicy("ImageRead", input, workDir, opts.ExecutionPolicy)
	if err != nil {
		return "Image read blocked: " + err.Error(), true
	}
	var args struct {
		FilePath string `json:"file_path"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "Image read blocked: invalid input: " + err.Error(), true
	}

	if opts.imageReadSink == nil || opts.ToolCallID == "" {
		return "Image read blocked: image payload transport is unavailable for this run", true
	}
	path := paths.one("file_path")
	file, err := os.Open(path)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "Image read blocked: file metadata is unavailable", true
	}
	if !info.Mode().IsRegular() {
		return "Image read blocked: target is not a regular file", true
	}
	data, err := io.ReadAll(io.LimitReader(file, protocol.MaxImageBytes+1))
	if err != nil {
		return "Image read failed: file contents are unavailable", true
	}
	if len(data) > protocol.MaxImageBytes {
		return "Image read blocked: image exceeds the 4 MiB file limit", true
	}
	block, err := opts.imageReadSink.store(opts.ToolCallID, data)
	if err != nil {
		_, _, detail := protocol.ErrorDetails(err)
		return "Image read blocked: " + detail, true
	}
	mediaType := ""
	if block.Source != nil {
		mediaType = block.Source.MediaType
	}
	return fmt.Sprintf("Image: %s\nMedia type: %s\nSize: %s\nValidated image content is attached to the next model request.",
		args.FilePath, mediaType, formatSize(info.Size())), false
}

// ── PDF Read ──

func executePDFRead(input json.RawMessage, workDir string, opts ToolExecutionOptions) (string, bool) {
	paths, err := executionToolWorkspacePathsWithPolicy("PDFRead", input, workDir, opts.ExecutionPolicy)
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
	paths, err := executionToolWorkspacePathsWithPolicy("Lint", input, workDir, opts.ExecutionPolicy)
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
	paths, err := executionToolWorkspacePathsWithPolicy("Test", input, workDir, opts.ExecutionPolicy)
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
	paths, err := executionToolWorkspacePathsWithPolicy("GitDiff", input, workDir, opts.ExecutionPolicy)
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
	paths, err := executionToolWorkspacePathsWithPolicy("GitCommit", input, workDir, opts.ExecutionPolicy)
	if err != nil {
		return "Git commit blocked: " + err.Error(), true
	}
	var args struct {
		Message string `json:"message"`
		Files   string `json:"files"`
		Scope   string `json:"scope"`
		Amend   bool   `json:"amend"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "Git commit blocked: invalid input: " + err.Error(), true
	}

	files := paths.many("files")
	if len(files) > 0 {
		relativeFiles, pathErr := gitCommitRelativePaths(workDir, files)
		if pathErr != nil {
			return "Git commit blocked: " + pathErr.Error(), true
		}
		output, commitErr := executeGitCommitSelectedPaths(workDir, relativeFiles, args.Message, args.Amend, opts)
		if commitErr != nil {
			return commitErr.Error(), true
		}
		return output, false
	}
	if strings.TrimSpace(args.Scope) != "staged" {
		return "Git commit blocked: committing the existing index requires explicit scope=staged.", true
	}
	if err := requireGitCommitWorkspaceRoot(opts, workDir); err != nil {
		return "Git commit blocked: " + err.Error(), true
	}
	commitArgs := []string{"--literal-pathspecs", "commit"}
	if args.Amend {
		commitArgs = append(commitArgs, "--amend")
	}
	commitArgs = append(commitArgs, "-m", args.Message)
	output, commitErr := executeGitCommitCommand(workDir, commitArgs, nil, "", "", false, nil, opts)
	if commitErr != nil {
		return commitErr.Error(), true
	}
	return output, false
}

func requireGitCommitWorkspaceRoot(opts ToolExecutionOptions, workDir string) error {
	result := runToolProcess(opts, "GitCommit repository root", workDir, "git", []string{"rev-parse", "--show-toplevel"}, defaultToolProcessTimeout)
	if err := gitCommitProcessError(result, "unable to identify the Git repository root"); err != nil {
		return err
	}
	root := strings.TrimSpace(result.combinedOutput())
	if root == "" {
		return fmt.Errorf("Git returned an empty repository root")
	}
	root, err := canonicalizeTarget(root)
	if err != nil {
		return fmt.Errorf("resolve Git repository root: %w", err)
	}
	workspace, err := canonicalWorkspace(workDir)
	if err != nil {
		return fmt.Errorf("resolve workspace: %w", err)
	}
	if !pathWithin(root, workspace) || !pathWithin(workspace, root) {
		return fmt.Errorf("scope=staged affects the entire Git index and is allowed only when the selected workspace is the Git repository root")
	}
	return nil
}

func executeGitCommitSelectedPaths(workDir string, paths []string, message string, amend bool, opts ToolExecutionOptions) (string, error) {
	transaction, err := beginGitCommitIndexTransaction(opts, workDir)
	if err != nil {
		return "", fmt.Errorf("Git commit preflight failed: %w", err)
	}
	defer transaction.close()
	if !transaction.indexExisted {
		initialize := runToolProcessWithGitIndex(opts, "GitCommit empty-index initialization", workDir, []string{"read-tree", "--empty"}, defaultToolProcessTimeout, transaction.snapshotPath)
		if err := gitCommitProcessError(initialize, "unable to initialize the Git index snapshot"); err != nil {
			return "", fmt.Errorf("Git commit preflight failed: %w", err)
		}
	}
	head, hasHead, err := gitCommitOptionalHead(opts, workDir)
	if err != nil {
		return "", fmt.Errorf("Git commit preflight failed: %w", err)
	}
	preflightArgs := []string{"diff", "--cached", "--quiet"}
	if hasHead {
		preflightArgs = append(preflightArgs, head)
	}
	preflightArgs = append(preflightArgs, "--")
	preflightArgs = append(preflightArgs, paths...)
	preflight := runToolProcessWithGitIndex(opts, "GitCommit staged-conflict check", workDir, preflightArgs, defaultToolProcessTimeout, transaction.snapshotPath)
	if preflight.policyOrContextFailure() || !preflight.Started || preflight.Err != nil {
		return "", fmt.Errorf("Git commit preflight failed: %s", preflight.setupOrExecutionError("unable to inspect staged changes"))
	}
	switch preflight.ExitCode {
	case 0:
		// No selected path has staged changes relative to the current base.
	case 1:
		return "", fmt.Errorf("Git commit blocked: staged_conflict: selected paths already contain staged changes; commit or unstage those paths first")
	default:
		return "", fmt.Errorf("Git commit preflight failed: %s", preflight.setupOrExecutionError("unable to inspect staged changes"))
	}
	untracked, err := gitCommitUntrackedPaths(opts, workDir, paths, transaction.snapshotPath)
	if err != nil {
		return "", fmt.Errorf("Git commit preflight failed: %w", err)
	}
	if len(untracked) > 0 {
		addArgs := append([]string{"--literal-pathspecs", "add", "--all", "--"}, untracked...)
		add := runToolProcessWithGitIndex(opts, "GitCommit untracked-path staging", workDir, addArgs, defaultToolProcessTimeout, transaction.snapshotPath)
		if err := gitCommitProcessError(add, "git add failed"); err != nil {
			return "", fmt.Errorf("Git commit preparation failed: %w", err)
		}
	}
	hookPaths, err := gitCommitRepositoryRelativePaths(opts, workDir, paths)
	if err != nil {
		return "", fmt.Errorf("Git commit preflight failed: %w", err)
	}
	hooksPath, cleanupHooks, err := prepareGitCommitHookGuard(opts, workDir, transaction.indexPath, transaction.snapshotPath, head, hasHead, hookPaths)
	if err != nil {
		return "", fmt.Errorf("Git commit preflight failed: %w", err)
	}
	defer cleanupHooks()
	if len(untracked) > 0 {
		if err := transaction.publishSnapshot(); err != nil {
			return "", fmt.Errorf("Git commit preflight failed: %w", err)
		}
	} else if err := transaction.releaseLock(); err != nil {
		return "", fmt.Errorf("Git commit preflight failed: release index lock: %w", err)
	}

	// Configured hook commands run before the traditional hook-directory entry.
	// Serialize commit hooks so the guard runs after every hook has had its turn.
	commitArgs := []string{
		"-c", "core.hooksPath=" + hooksPath,
		"-c", "hook.jobs=1",
		"-c", "hook.pre-commit.jobs=1",
		"-c", "hook.prepare-commit-msg.jobs=1",
		"-c", "hook.commit-msg.jobs=1",
		"--literal-pathspecs", "commit",
	}
	if amend {
		commitArgs = append(commitArgs, "--amend")
	}
	commitArgs = append(commitArgs, "--only", "-m", message, "--")
	commitArgs = append(commitArgs, paths...)
	return executeGitCommitCommand(workDir, commitArgs, paths, transaction.snapshotPath, head, hasHead, untracked, opts)
}

func executeGitCommitCommand(
	workDir string,
	commitArgs []string,
	indexPaths []string,
	expectedIndexPath string,
	expectedHead string,
	hasExpectedHead bool,
	rollbackPaths []string,
	opts ToolExecutionOptions,
) (string, error) {
	if expectedIndexPath == "" {
		var err error
		expectedHead, hasExpectedHead, err = gitCommitOptionalHead(opts, workDir)
		if err != nil {
			return "", fmt.Errorf("Git commit preflight failed: %w", err)
		}
	}
	if opts.Context != nil && opts.Context.Err() != nil {
		cause := fmt.Errorf("Git commit canceled before execution: %w", opts.Context.Err())
		if len(rollbackPaths) > 0 {
			if rollbackErr := rollbackGitCommitAddedPaths(withoutToolContextCancellation(opts), workDir, expectedIndexPath, expectedHead, hasExpectedHead, rollbackPaths); rollbackErr != nil {
				return "", fmt.Errorf("%w; selected-path index cleanup failed: %v", cause, rollbackErr)
			}
		}
		return "", cause
	}
	// Once the commit process starts, let Git finish its index/ref lock protocol
	// even if the caller cancels. Stopping it mid-transaction can leave an
	// ambiguous result; the bounded process timeout still applies.
	commitOpts := withoutToolContextCancellation(opts)
	commit := runToolProcess(commitOpts, "GitCommit", workDir, "git", commitArgs, defaultToolProcessTimeout)
	if err := gitCommitProcessError(commit, "git commit failed"); err != nil {
		// Git commits update the index and HEAD through Git's own lock protocol.
		// Preserve the stage if HEAD or the selected index moved after preparation.
		committedHead, hasCommittedHead, stateErr := gitCommitOptionalHead(commitOpts, workDir)
		headChanged := stateErr == nil && (hasExpectedHead != hasCommittedHead || (hasExpectedHead && hasCommittedHead && committedHead != expectedHead))
		if len(rollbackPaths) > 0 && !headChanged && stateErr == nil {
			if rollbackErr := rollbackGitCommitAddedPaths(commitOpts, workDir, expectedIndexPath, expectedHead, hasExpectedHead, rollbackPaths); rollbackErr != nil {
				return "", fmt.Errorf("%w; selected-path index cleanup failed: %v", err, rollbackErr)
			}
		}
		if headChanged {
			return "", fmt.Errorf("%w; HEAD advanced to %s, so selected-path index staging was preserved for review", err, committedHead)
		} else if stateErr != nil && len(rollbackPaths) > 0 {
			return "", fmt.Errorf("%w; HEAD could not be inspected, so selected-path index staging was preserved: %v", err, stateErr)
		}
		return "", err
	}
	if err := verifyGitCommitIndex(commitOpts, workDir, indexPaths); err != nil {
		return "", fmt.Errorf("Git commit completed but index entries do not match HEAD: %w", err)
	}
	return commit.combinedOutput(), nil
}

func gitCommitUntrackedPaths(opts ToolExecutionOptions, workDir string, paths []string, indexPath string) ([]string, error) {
	args := append([]string{"--literal-pathspecs", "ls-files", "--others", "--exclude-standard", "-z", "--"}, paths...)
	result := runToolProcessWithGitIndex(opts, "GitCommit untracked-path check", workDir, args, defaultToolProcessTimeout, indexPath)
	if result.policyOrContextFailure() || !result.Started || result.Err != nil || result.ExitCode != 0 {
		return nil, fmt.Errorf("%s", result.setupOrExecutionError("unable to inspect untracked selected paths"))
	}
	pathsFound := strings.Split(string(result.Stdout), "\x00")
	untracked := make([]string, 0, len(pathsFound))
	for _, path := range pathsFound {
		if path != "" {
			untracked = append(untracked, path)
		}
	}
	return untracked, nil
}

func rollbackGitCommitAddedPaths(
	opts ToolExecutionOptions,
	workDir string,
	expectedIndexPath string,
	expectedHead string,
	hasExpectedHead bool,
	paths []string,
) error {
	if len(paths) == 0 {
		return nil
	}
	transaction, err := beginGitCommitIndexTransaction(opts, workDir)
	if err != nil {
		return err
	}
	defer transaction.close()
	currentHead, hasCurrentHead, err := gitCommitOptionalHead(opts, workDir)
	if err != nil {
		return err
	}
	if hasCurrentHead != hasExpectedHead || (hasExpectedHead && currentHead != expectedHead) {
		return fmt.Errorf("HEAD changed; selected staging was preserved")
	}
	currentDiff, err := gitCommitRawCachedDiff(opts, workDir, transaction.snapshotPath, expectedHead, hasExpectedHead, paths)
	if err != nil {
		return err
	}
	expectedDiff, err := gitCommitRawCachedDiff(opts, workDir, expectedIndexPath, expectedHead, hasExpectedHead, paths)
	if err != nil {
		return err
	}
	if currentDiff != expectedDiff {
		return fmt.Errorf("selected index changed; staged data was preserved")
	}
	var args []string
	if hasCurrentHead {
		args = append([]string{"--literal-pathspecs", "reset", "--quiet", currentHead, "--"}, paths...)
	} else {
		args = append([]string{"--literal-pathspecs", "rm", "--cached", "--ignore-unmatch", "-r", "--"}, paths...)
	}
	result := runToolProcessWithGitIndex(opts, "GitCommit untracked-path rollback", workDir, args, defaultToolProcessTimeout, transaction.snapshotPath)
	if err := gitCommitProcessError(result, "unable to rollback selected-path staging"); err != nil {
		return err
	}
	verify := []string{"diff", "--cached", "--quiet"}
	if hasCurrentHead {
		verify = append(verify, currentHead)
	}
	verify = append(verify, "--")
	verify = append(verify, paths...)
	check := runToolProcessWithGitIndex(opts, "GitCommit rollback verification", workDir, verify, defaultToolProcessTimeout, transaction.snapshotPath)
	if check.policyOrContextFailure() || !check.Started || check.Err != nil || check.ExitCode != 0 {
		return fmt.Errorf("%s", check.setupOrExecutionError("selected paths remain staged after rollback"))
	}
	return transaction.publishSnapshot()
}

func gitCommitRawCachedDiff(opts ToolExecutionOptions, workDir, indexPath, head string, hasHead bool, paths []string) (string, error) {
	args := []string{"--literal-pathspecs", "diff", "--cached", "--raw", "--no-abbrev"}
	if hasHead {
		args = append(args, head)
	}
	args = append(args, "--")
	args = append(args, paths...)
	result := runToolProcessWithGitIndex(opts, "GitCommit index snapshot comparison", workDir, args, defaultToolProcessTimeout, indexPath)
	if result.policyOrContextFailure() || !result.Started || result.Err != nil || result.ExitCode != 0 {
		return "", fmt.Errorf("%s", result.setupOrExecutionError("unable to compare selected Git index entries"))
	}
	return string(result.Stdout), nil
}

func gitCommitProcessError(result toolProcessResult, fallback string) error {
	if result.policyOrContextFailure() || !result.Started || result.Err != nil || result.ExitCode != 0 {
		return fmt.Errorf("%s", result.setupOrExecutionError(fallback))
	}
	return nil
}

func gitCommitOptionalHead(opts ToolExecutionOptions, workDir string) (string, bool, error) {
	result := runToolProcess(opts, "GitCommit HEAD snapshot", workDir, "git", []string{"rev-parse", "--verify", "--quiet", "HEAD"}, defaultToolProcessTimeout)
	if result.policyOrContextFailure() || !result.Started || result.Err != nil {
		return "", false, fmt.Errorf("unable to read HEAD: %s", result.setupOrExecutionError("git rev-parse failed"))
	}
	if result.ExitCode == 1 {
		return "", false, nil
	}
	if result.ExitCode != 0 {
		return "", false, fmt.Errorf("unable to read HEAD: %s", result.setupOrExecutionError("git rev-parse failed"))
	}
	head := strings.TrimSpace(result.combinedOutput())
	if head == "" {
		return "", false, fmt.Errorf("git rev-parse returned an empty HEAD")
	}
	return head, true, nil
}

func verifyGitCommitIndex(opts ToolExecutionOptions, workDir string, paths []string) error {
	verifyArgs := []string{"diff", "--cached", "--quiet", "HEAD"}
	if len(paths) > 0 {
		verifyArgs = append(verifyArgs, "--")
		verifyArgs = append(verifyArgs, paths...)
	}
	verify := runToolProcess(opts, "GitCommit selected-index verification", workDir, "git", verifyArgs, defaultToolProcessTimeout)
	if verify.policyOrContextFailure() || !verify.Started || verify.Err != nil || verify.ExitCode != 0 {
		return fmt.Errorf("%s", verify.setupOrExecutionError("selected paths remain staged relative to HEAD"))
	}
	return nil
}

func withoutToolContextCancellation(opts ToolExecutionOptions) ToolExecutionOptions {
	if opts.Context != nil {
		opts.Context = context.WithoutCancel(opts.Context)
	}
	return opts
}

func gitCommitRelativePaths(workDir string, paths []string) ([]string, error) {
	workspace, err := canonicalWorkspace(workDir)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace: %w", err)
	}
	relative := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		canonical, err := canonicalizeTarget(path)
		if err != nil {
			return nil, fmt.Errorf("resolve selected path: %w", err)
		}
		if !pathWithin(canonical, workspace) {
			return nil, fmt.Errorf("selected path is outside workspace: %s", path)
		}
		local, err := filepath.Rel(workspace, canonical)
		if err != nil {
			return nil, fmt.Errorf("resolve selected path relative to workspace: %w", err)
		}
		local = filepath.ToSlash(local)
		if _, exists := seen[local]; exists {
			continue
		}
		seen[local] = struct{}{}
		relative = append(relative, local)
	}
	if len(relative) == 0 {
		return nil, fmt.Errorf("no selected paths were provided")
	}
	return relative, nil
}

func gitCommitRepositoryRelativePaths(opts ToolExecutionOptions, workDir string, paths []string) ([]string, error) {
	result := runToolProcess(opts, "GitCommit repository-relative hook paths", workDir, "git", []string{"rev-parse", "--show-toplevel"}, defaultToolProcessTimeout)
	if err := gitCommitProcessError(result, "unable to identify the Git repository root for hook scope checks"); err != nil {
		return nil, err
	}
	root := strings.TrimSpace(result.combinedOutput())
	if root == "" {
		return nil, fmt.Errorf("Git returned an empty repository root for hook scope checks")
	}
	root, err := canonicalizeTarget(root)
	if err != nil {
		return nil, fmt.Errorf("resolve Git repository root for hook scope checks: %w", err)
	}
	workspace, err := canonicalWorkspace(workDir)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace for hook scope checks: %w", err)
	}
	relative := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, selected := range paths {
		absolute, err := canonicalizeTarget(filepath.Join(workspace, filepath.FromSlash(selected)))
		if err != nil {
			return nil, fmt.Errorf("resolve selected path for hook scope check: %w", err)
		}
		if !pathWithin(absolute, root) {
			return nil, fmt.Errorf("selected path is outside the Git repository: %s", selected)
		}
		local, err := filepath.Rel(root, absolute)
		if err != nil {
			return nil, fmt.Errorf("resolve selected path relative to Git root: %w", err)
		}
		local = filepath.ToSlash(local)
		if _, exists := seen[local]; exists {
			continue
		}
		seen[local] = struct{}{}
		relative = append(relative, local)
	}
	return relative, nil
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
	paths, err := executionToolWorkspacePathsWithPolicy("Diff", input, workDir, opts.ExecutionPolicy)
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
