package agent

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/aniclew/aniclew/internal/types"
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
	switch name {
	case "ImageRead":
		r, e := executeImageRead(input, workDir)
		return r, e, true
	case "PDFRead":
		r, e := executePDFRead(input, workDir)
		return r, e, true
	case "Lint":
		r, e := executeLint(input, workDir)
		return r, e, true
	case "Test":
		r, e := executeTest(input, workDir)
		return r, e, true
	case "GitDiff":
		r, e := executeGitDiff(input, workDir)
		return r, e, true
	case "GitCommit":
		r, e := executeGitCommit(input, workDir)
		return r, e, true
	case "HTTPRequest":
		r, e := executeHTTPRequest(input)
		return r, e, true
	case "Diff":
		r, e := executeDiff(input, workDir)
		return r, e, true
	default:
		return "", false, false
	}
}

// ── Image Read ──

func executeImageRead(input json.RawMessage, workDir string) (string, bool) {
	var args struct{ FilePath string `json:"file_path"` }
	json.Unmarshal(input, &args)

	path := resolvePath(args.FilePath, workDir)
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

func executePDFRead(input json.RawMessage, workDir string) (string, bool) {
	var args struct {
		FilePath string `json:"file_path"`
		Pages    string `json:"pages"`
	}
	json.Unmarshal(input, &args)

	path := resolvePath(args.FilePath, workDir)

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

	cmd := exec.Command("pdftotext", cmdArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Fallback: try python
		pyCmd := fmt.Sprintf("python3 -c \"import PyPDF2; r=PyPDF2.PdfReader('%s'); print('\\n'.join(p.extract_text() for p in r.pages))\"", path)
		cmd2 := exec.Command("bash", "-c", pyCmd)
		out2, err2 := cmd2.CombinedOutput()
		if err2 != nil {
			return fmt.Sprintf("PDF reading requires 'pdftotext' or 'PyPDF2'. Install: apt install poppler-utils"), true
		}
		out = out2
	}

	result := string(out)
	if len(result) > 50000 {
		result = result[:50000] + "\n... (truncated)"
	}
	return fmt.Sprintf("PDF: %s\n\n%s", args.FilePath, result), false
}

// ── Auto Lint ──

func executeLint(input json.RawMessage, workDir string) (string, bool) {
	var args struct {
		Path string `json:"path"`
		Fix  bool   `json:"fix"`
	}
	json.Unmarshal(input, &args)

	project := DetectProject(workDir)
	var cmd *exec.Cmd

	switch project.Type {
	case "go":
		if args.Fix {
			cmd = exec.Command("bash", "-c", "gofmt -w . && go vet ./...")
		} else {
			cmd = exec.Command("go", "vet", "./...")
		}
	case "node":
		if args.Fix {
			cmd = exec.Command("npx", "eslint", "--fix", ".")
		} else {
			cmd = exec.Command("npx", "eslint", ".")
		}
	case "python":
		if args.Fix {
			cmd = exec.Command("ruff", "check", "--fix", ".")
		} else {
			cmd = exec.Command("ruff", "check", ".")
		}
	case "rust":
		cmd = exec.Command("cargo", "clippy")
	default:
		return fmt.Sprintf("No linter configured for project type: %s", project.Type), false
	}

	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	result := strings.TrimSpace(string(out))

	if err != nil && result != "" {
		return result, true // lint found issues
	}
	if result == "" {
		return "No lint issues found. ✅", false
	}
	return result, false
}

// ── Auto Test ──

// pythonExecutable prefers python3 but falls back to python. Windows installs
// ship python.exe with no python3 alias, so hardcoding python3 made every Test
// call fail there with "executable file not found".
func pythonExecutable() string {
	for _, name := range []string{"python3", "python"} {
		if _, err := exec.LookPath(name); err == nil {
			return name
		}
	}
	return "python3"
}

func executeTest(input json.RawMessage, workDir string) (string, bool) {
	var args struct {
		Path   string `json:"path"`
		Filter string `json:"filter"`
	}
	json.Unmarshal(input, &args)

	project := DetectProject(workDir)
	var cmd *exec.Cmd

	switch project.Type {
	case "go":
		testArgs := []string{"test"}
		if args.Filter != "" {
			testArgs = append(testArgs, "-run", args.Filter)
		}
		if args.Path != "" {
			testArgs = append(testArgs, args.Path)
		} else {
			testArgs = append(testArgs, "./...")
		}
		cmd = exec.Command("go", testArgs...)
	case "node":
		testArgs := []string{"test"}
		if args.Filter != "" {
			testArgs = append(testArgs, "--", "--grep", args.Filter)
		}
		cmd = exec.Command("npm", testArgs...)
	case "python":
		testArgs := []string{"-m", "pytest", "-v"}
		if args.Filter != "" {
			testArgs = append(testArgs, "-k", args.Filter)
		}
		if args.Path != "" {
			testArgs = append(testArgs, args.Path)
		}
		cmd = exec.Command(pythonExecutable(), testArgs...)
	case "rust":
		testArgs := []string{"test"}
		if args.Filter != "" {
			testArgs = append(testArgs, "--", args.Filter)
		}
		cmd = exec.Command("cargo", testArgs...)
	default:
		// Reported as an error on purpose: nothing ran. Returning this as a
		// success let a model treat "no runner configured" as a passing test
		// run and stop there.
		return fmt.Sprintf("No test runner configured for project type %q. Run the test command directly with Bash.", project.Type), true
	}

	cmd.Dir = workDir
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	result := strings.TrimSpace(string(out))
	if result == "" {
		result = "No test output (possibly no test files)"
	}
	if len(result) > 50000 {
		result = result[:50000] + "\n... (truncated)"
	}
	if err != nil && result != "No test output (possibly no test files)" {
		return result + "\n[tests failed]", true
	}
	return result, false
}

// ── Git Diff (formatted) ──

func executeGitDiff(input json.RawMessage, workDir string) (string, bool) {
	var args struct {
		Staged bool   `json:"staged"`
		File   string `json:"file"`
		Commit string `json:"commit"`
	}
	json.Unmarshal(input, &args)

	cmdArgs := []string{"diff", "--stat"}
	if args.Staged {
		cmdArgs = append(cmdArgs, "--cached")
	}
	if args.Commit != "" {
		cmdArgs = append(cmdArgs, args.Commit)
	}
	if args.File != "" {
		cmdArgs = append(cmdArgs, "--", args.File)
	}

	// Get stat
	statCmd := exec.Command("git", cmdArgs...)
	statCmd.Dir = workDir
	stat, _ := statCmd.CombinedOutput()

	// Get full diff
	cmdArgs[1] = "diff"
	diffCmd := exec.Command("git", append(cmdArgs[1:], "--color=never")...)
	diffCmd.Dir = workDir
	diff, _ := diffCmd.CombinedOutput()

	result := "Diff Summary:\n" + string(stat) + "\n" + string(diff)
	if len(result) > 30000 {
		result = result[:30000] + "\n... (truncated)"
	}
	return result, false
}

// ── Git Commit ──

func executeGitCommit(input json.RawMessage, workDir string) (string, bool) {
	var args struct {
		Message string `json:"message"`
		Files   string `json:"files"`
		Amend   bool   `json:"amend"`
	}
	json.Unmarshal(input, &args)

	// Stage files
	if args.Files != "" {
		addCmd := exec.Command("git", append([]string{"add"}, strings.Fields(args.Files)...)...)
		addCmd.Dir = workDir
		if out, err := addCmd.CombinedOutput(); err != nil {
			return fmt.Sprintf("Stage failed: %s", out), true
		}
	}

	// Commit
	commitArgs := []string{"commit", "-m", args.Message}
	if args.Amend {
		commitArgs = append(commitArgs, "--amend")
	}

	cmd := exec.Command("git", commitArgs...)
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), true
	}
	return string(out), false
}

// ── HTTP Request ──

func executeHTTPRequest(input json.RawMessage) (string, bool) {
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
	for k, v := range args.Headers {
		curlArgs = append(curlArgs, "-H", fmt.Sprintf("%s: %s", k, v))
	}
	if args.Body != "" {
		curlArgs = append(curlArgs, "-d", args.Body, "-H", "Content-Type: application/json")
	}
	curlArgs = append(curlArgs, args.URL)

	cmd := exec.Command("curl", curlArgs...)
	out, err := cmd.CombinedOutput()
	result := string(out)
	if err != nil {
		return result + "\n[request failed]", true
	}
	if len(result) > 20000 {
		result = result[:20000] + "\n... (truncated)"
	}
	return result, false
}

// ── File Diff ──

func executeDiff(input json.RawMessage, workDir string) (string, bool) {
	var args struct {
		FileA string `json:"file_a"`
		FileB string `json:"file_b"`
	}
	json.Unmarshal(input, &args)

	pathA := resolvePath(args.FileA, workDir)
	pathB := resolvePath(args.FileB, workDir)

	cmd := exec.Command("diff", "-u", pathA, pathB)
	out, _ := cmd.CombinedOutput()
	result := string(out)
	if result == "" {
		return "Files are identical.", false
	}
	return result, false
}
