package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

func TestImmutableCatalogFilesystemFieldsRejectWorkspaceEscapes(t *testing.T) {
	workspace, outside := pathTestWorkspace(t)
	inside := filepath.Join(workspace, "inside.txt")
	outsideFile := filepath.Join(outside, "secret.txt")
	cfg := PermissionConfig{AutoApprove: "all"}

	tests := []struct {
		name  string
		tool  string
		input json.RawMessage
	}{
		{"image absolute", "ImageRead", pathTestInput(t, map[string]any{"file_path": outsideFile})},
		{"pdf traversal", "PDFRead", pathTestInput(t, map[string]any{"file_path": filepath.Join("..", "outside", "secret.txt")})},
		{"notebook read", "NotebookRead", pathTestInput(t, map[string]any{"file_path": outsideFile})},
		{"notebook edit disabled", "NotebookEdit", pathTestInput(t, map[string]any{"file_path": "inside.txt", "cell_index": 0, "new_source": "changed"})},
		{"lint", "Lint", pathTestInput(t, map[string]any{"path": outside})},
		{"test", "Test", pathTestInput(t, map[string]any{"path": outsideFile})},
		{"git diff", "GitDiff", pathTestInput(t, map[string]any{"file": outsideFile})},
		{"git commit second operand", "GitCommit", pathTestInput(t, map[string]any{"message": "test", "files": inside + " " + outsideFile})},
		{"git commit option injection", "GitCommit", pathTestInput(t, map[string]any{"message": "test", "files": "--all"})},
		{"git opaque argument traversal", "Git", pathTestInput(t, map[string]any{"command": "status", "args": "-- ../outside/secret.txt"})},
		{"git output option", "Git", pathTestInput(t, map[string]any{"command": "diff", "args": "--output=../outside/diff.txt"})},
		{"git embedded directory traversal", "Git", pathTestInput(t, map[string]any{"command": "apply", "args": "--directory=../../outside patch.diff"})},
		{"git unsafe patch paths", "Git", pathTestInput(t, map[string]any{"command": "apply", "args": "--unsafe-paths patch.diff"})},
		{"diff first operand", "Diff", pathTestInput(t, map[string]any{"file_a": outsideFile, "file_b": inside})},
		{"diff second operand", "Diff", pathTestInput(t, map[string]any{"file_a": inside, "file_b": outsideFile})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			allowed, reason, danger := CheckPermission(test.tool, test.input, workspace, cfg)
			if allowed {
				t.Fatalf("%s workspace escape was allowed", test.tool)
			}
			if strings.TrimSpace(reason) == "" {
				t.Fatal("denial reason is empty")
			}
			if danger != DangerDangerous {
				t.Fatalf("danger=%q, want %q", danger, DangerDangerous)
			}
		})
	}
}

func TestImmutableCatalogFilesystemFieldsAcceptCanonicalWorkspaceOperands(t *testing.T) {
	workspace, _ := pathTestWorkspace(t)
	cfg := PermissionConfig{AutoApprove: "all"}

	tests := []struct {
		tool  string
		input json.RawMessage
	}{
		{"ImageRead", pathTestInput(t, map[string]any{"file_path": "inside.txt"})},
		{"PDFRead", pathTestInput(t, map[string]any{"file_path": "inside.pdf"})},
		{"NotebookRead", pathTestInput(t, map[string]any{"file_path": "inside.ipynb"})},
		{"Lint", pathTestInput(t, map[string]any{"path": "sub"})},
		{"Test", pathTestInput(t, map[string]any{"path": "sub"})},
		{"GitDiff", pathTestInput(t, map[string]any{"file": "inside.txt", "commit": "origin/main"})},
		{"GitCommit", pathTestInput(t, map[string]any{"message": "test", "files": "inside.txt sub/inside.go"})},
		{"Git", pathTestInput(t, map[string]any{"command": "status", "args": "--short -- inside.txt"})},
		{"Diff", pathTestInput(t, map[string]any{"file_a": "inside.txt", "file_b": "sub/inside.go"})},
	}
	for _, test := range tests {
		t.Run(test.tool, func(t *testing.T) {
			if allowed, reason, _ := CheckPermission(test.tool, test.input, workspace, cfg); !allowed {
				t.Fatalf("inside operands denied: %s", reason)
			}
		})
	}
}

func TestImmutableCatalogFilesystemFieldsRejectAmbiguousJSON(t *testing.T) {
	workspace, _ := pathTestWorkspace(t)
	cfg := PermissionConfig{AutoApprove: "all"}
	tests := []struct {
		name  string
		tool  string
		input json.RawMessage
	}{
		{"image duplicate", "ImageRead", json.RawMessage(`{"file_path":"inside.txt","file_path":"../outside/secret.txt"}`)},
		{"pdf conflict", "PDFRead", json.RawMessage(`{"file_path":"inside.txt","path":"../outside/secret.txt"}`)},
		{"diff duplicate second", "Diff", json.RawMessage(`{"file_a":"inside.txt","file_b":"inside.txt","file_b":"../outside/secret.txt"}`)},
		{"diff conflicting alias", "Diff", json.RawMessage(`{"file_a":"inside.txt","fileA":"../outside/secret.txt","file_b":"inside.txt"}`)},
		{"git duplicate args", "Git", json.RawMessage(`{"command":"status","args":"--short","args":"-- ../outside/secret.txt"}`)},
		{"notebook conflicting alias", "NotebookRead", json.RawMessage(`{"file_path":"inside.txt","filePath":"../outside/secret.txt"}`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if allowed, _, _ := CheckPermission(test.tool, test.input, workspace, cfg); allowed {
				t.Fatal("ambiguous filesystem input was allowed")
			}
		})
	}
}

func TestImmutableCatalogFilesystemFieldsRejectSymlinkEscapes(t *testing.T) {
	workspace, outside := pathTestWorkspace(t)
	link := filepath.Join(workspace, "outside-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	cfg := PermissionConfig{AutoApprove: "all"}

	tests := []struct {
		tool  string
		input json.RawMessage
	}{
		{"ImageRead", pathTestInput(t, map[string]any{"file_path": "outside-link/secret.txt"})},
		{"PDFRead", pathTestInput(t, map[string]any{"file_path": "outside-link/new.pdf"})},
		{"NotebookRead", pathTestInput(t, map[string]any{"file_path": "outside-link/secret.txt"})},
		{"Lint", pathTestInput(t, map[string]any{"path": "outside-link"})},
		{"Test", pathTestInput(t, map[string]any{"path": "outside-link/secret.txt"})},
		{"GitDiff", pathTestInput(t, map[string]any{"file": "outside-link/secret.txt"})},
		{"GitCommit", pathTestInput(t, map[string]any{"message": "test", "files": "inside.txt outside-link/secret.txt"})},
		{"Diff", pathTestInput(t, map[string]any{"file_a": "inside.txt", "file_b": "outside-link/secret.txt"})},
	}
	for _, test := range tests {
		t.Run(test.tool, func(t *testing.T) {
			if allowed, _, _ := CheckPermission(test.tool, test.input, workspace, cfg); allowed {
				t.Fatal("symlink escape was allowed")
			}
		})
	}
}

func TestDirectFilesystemExecutorsFailBeforeReadingOrStartingProcess(t *testing.T) {
	workspace, outside := pathTestWorkspace(t)
	outsideFile := filepath.Join(outside, "secret.txt")
	runner := &fakeToolProcessRunner{name: "path-canary", capabilities: fakeBashCapabilities()}
	runner.run = func(_ context.Context, policy sandbox.Policy, _ sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
		t.Fatal("unsafe path reached the process runner")
		return sandbox.Result{}, sandbox.Report{RequestedEnforcement: policy.Enforcement}
	}
	opts := secureToolProcessOptions(runner)

	if output, isError := executeImageRead(pathTestInput(t, map[string]any{"file_path": outsideFile}), workspace); !isError || !strings.Contains(strings.ToLower(output), "blocked") {
		t.Fatalf("ImageRead output=%q isError=%v", output, isError)
	}
	if output, isError := executeNotebookRead(pathTestInput(t, map[string]any{"file_path": outsideFile}), workspace); !isError || !strings.Contains(strings.ToLower(output), "blocked") {
		t.Fatalf("NotebookRead output=%q isError=%v", output, isError)
	}
	processCalls := []struct {
		name string
		call func() (string, bool)
	}{
		{"PDFRead", func() (string, bool) {
			return executePDFRead(pathTestInput(t, map[string]any{"file_path": outsideFile}), workspace, opts)
		}},
		{"Lint", func() (string, bool) {
			return executeLint(pathTestInput(t, map[string]any{"path": outside}), workspace, opts)
		}},
		{"Test", func() (string, bool) {
			return executeTestWithOptions(pathTestInput(t, map[string]any{"path": outsideFile}), workspace, opts)
		}},
		{"GitDiff", func() (string, bool) {
			return executeGitDiff(pathTestInput(t, map[string]any{"file": outsideFile}), workspace, opts)
		}},
		{"GitCommit", func() (string, bool) {
			return executeGitCommit(pathTestInput(t, map[string]any{"message": "test", "files": "inside.txt " + outsideFile}), workspace, opts)
		}},
		{"Git", func() (string, bool) {
			return executeGit(pathTestInput(t, map[string]any{"command": "status", "args": "-- " + outsideFile}), workspace, opts)
		}},
		{"Diff", func() (string, bool) {
			return executeDiff(pathTestInput(t, map[string]any{"file_a": "inside.txt", "file_b": outsideFile}), workspace, opts)
		}},
	}
	for _, test := range processCalls {
		t.Run(test.name, func(t *testing.T) {
			output, isError := test.call()
			if !isError || !strings.Contains(strings.ToLower(output), "blocked") {
				t.Fatalf("output=%q isError=%v", output, isError)
			}
		})
	}
	if calls := runner.commandSnapshot(); len(calls) != 0 {
		t.Fatalf("unsafe paths started %d processes", len(calls))
	}
}

func TestNotebookEditIsAbsentAndForcedCallCannotMutateCanary(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "canary.ipynb")
	before := []byte(`{"cells":[{"cell_type":"code","source":["original"],"outputs":[]}],"metadata":{},"nbformat":4}`)
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, definition := range ExtendedToolDefs() {
		if definition.Name == "NotebookEdit" {
			t.Fatal("NotebookEdit remains in the immutable default catalog")
		}
	}
	input := pathTestInput(t, map[string]any{
		"file_path":  "canary.ipynb",
		"cell_index": 0,
		"new_source": "mutated",
	})
	output, isError, handled := ExecuteExtendedToolWithOptions("NotebookEdit", input, workspace, ToolExecutionOptions{})
	if !handled || !isError || !strings.Contains(strings.ToLower(output), "disabled") {
		t.Fatalf("output=%q isError=%v handled=%v", output, isError, handled)
	}
	if output, isError := ExecuteTool("NotebookEdit", input, workspace); !isError || !strings.Contains(strings.ToLower(output), "unknown tool") {
		t.Fatalf("forced catalog output=%q isError=%v", output, isError)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("NotebookEdit changed canary bytes: before=%q after=%q", before, after)
	}
}
