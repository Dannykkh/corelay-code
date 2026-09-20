package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecutionToolWorkspacePathsWithPolicyAllowsFullOnlyOutsideWorkspace(t *testing.T) {
	workspace := t.TempDir()
	external := t.TempDir()
	externalFile := filepath.Join(external, "external.txt")
	if err := os.WriteFile(externalFile, []byte("external contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(map[string]string{"file_path": externalFile})
	if err != nil {
		t.Fatal(err)
	}
	full := &ExecutionPolicySnapshot{Mode: ExecutionModeFull, Revision: 12}

	resolved, err := executionToolWorkspacePathsWithPolicy("Read", input, workspace, full)
	if err != nil {
		t.Fatalf("full mode rejected external path: %v", err)
	}
	canonical, err := filepath.EvalSymlinks(externalFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := resolved.one("file_path"); got != canonical {
		t.Fatalf("canonical full-mode path = %q, want %q", got, canonical)
	}

	for name, policy := range map[string]*ExecutionPolicySnapshot{
		"workspace":      {Mode: ExecutionModeWorkspace, Revision: 12},
		"read-only":      {Mode: ExecutionModeReadOnly, Revision: 12},
		"legacy wrapper": nil,
	} {
		t.Run(name, func(t *testing.T) {
			var err error
			if policy == nil {
				_, err = executionToolWorkspacePaths("Read", input, workspace)
			} else {
				_, err = executionToolWorkspacePathsWithPolicy("Read", input, workspace, policy)
			}
			if err == nil || !strings.Contains(err.Error(), "outside workspace") {
				t.Fatalf("external path error = %v, want workspace containment rejection", err)
			}
		})
	}
}

func TestExecutionToolWorkspacePathsFullRetainsSyntaxSymlinkAndBlockedPathChecks(t *testing.T) {
	workspace := t.TempDir()
	external := t.TempDir()
	full := &ExecutionPolicySnapshot{Mode: ExecutionModeFull, Revision: 13}
	policyInput := func(path string) json.RawMessage {
		t.Helper()
		input, err := json.Marshal(map[string]string{"file_path": path})
		if err != nil {
			t.Fatal(err)
		}
		return input
	}

	if _, err := executionToolWorkspacePathsWithPolicy("Read", policyInput("bad\x00path"), workspace, full); err == nil {
		t.Fatal("full mode accepted a path containing NUL")
	}
	if _, err := executionToolWorkspacePathsWithPolicy(
		"Read",
		json.RawMessage(`{"file_path":"inside","file_path":"external"}`),
		workspace,
		full,
	); err == nil {
		t.Fatal("full mode accepted ambiguous duplicate-key JSON")
	}

	externalFile := filepath.Join(external, "secret.txt")
	if err := os.WriteFile(externalFile, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := resolveToolWorkspacePaths(
		"Read",
		policyInput(externalFile),
		workspace,
		PermissionConfig{AllowExternalPaths: true, BlockedPaths: []string{"secret.txt"}},
	)
	if err == nil || !strings.Contains(err.Error(), "Blocked path") {
		t.Fatalf("full mode blocked-path result = %v, want blocked path rejection", err)
	}

	danglingLink := filepath.Join(external, "dangling.txt")
	if err := os.Symlink(filepath.Join(external, "missing-target.txt"), danglingLink); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	if _, err := executionToolWorkspacePathsWithPolicy("Read", policyInput(danglingLink), workspace, full); err == nil {
		t.Fatal("full mode accepted a broken symlink")
	}
}

func TestFullModeFilesystemBuiltinsAcceptExternalReadPaths(t *testing.T) {
	workspace := t.TempDir()
	external := t.TempDir()
	full := &ExecutionPolicySnapshot{Mode: ExecutionModeFull, Revision: 21}
	workspacePolicy := &ExecutionPolicySnapshot{Mode: ExecutionModeWorkspace, Revision: 21}
	write := func(path, contents string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	input := func(value any) json.RawMessage {
		t.Helper()
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}

	canary := filepath.Join(external, "external-canary.txt")
	write(canary, "external contents")
	imageCanary := filepath.Join(external, "external-canary.png")
	if err := os.WriteFile(imageCanary, imageReadPNGFixture(t), 0o600); err != nil {
		t.Fatal(err)
	}
	directWrite := filepath.Join(external, "full-mode-created.txt")
	notebook := filepath.Join(external, "external.ipynb")
	write(notebook, `{"cells":[{"cell_type":"code","source":["external-cell-value"],"outputs":[]}],"metadata":{},"nbformat":4}`)
	write(filepath.Join(external, "external.go"), "package external\nfunc ExternalSymbol() {}\n")

	cases := []struct {
		name   string
		invoke func(ToolExecutionOptions) (string, bool, bool)
		want   string
	}{
		{
			name: "direct Read",
			invoke: func(opts ToolExecutionOptions) (string, bool, bool) {
				output, isErr := ExecuteToolWithOptions("Read", input(map[string]string{"file_path": canary}), workspace, opts)
				return output, isErr, true
			},
			want: "external contents",
		},
		{
			name: "direct Glob",
			invoke: func(opts ToolExecutionOptions) (string, bool, bool) {
				output, isErr := ExecuteToolWithOptions("Glob", input(map[string]string{"pattern": "*.txt", "path": external}), workspace, opts)
				return output, isErr, true
			},
			want: "external-canary.txt",
		},
		{
			name: "direct Write",
			invoke: func(opts ToolExecutionOptions) (string, bool, bool) {
				output, isErr := ExecuteToolWithOptions("Write", input(map[string]string{"file_path": directWrite, "content": "full external write"}), workspace, opts)
				return output, isErr, true
			},
			want: "full-mode-created.txt",
		},
		{
			name: "LS",
			invoke: func(opts ToolExecutionOptions) (string, bool, bool) {
				return ExecuteExtendedToolWithOptions("LS", input(map[string]string{"path": external}), workspace, opts)
			},
			want: "external-canary.txt",
		},
		{
			name: "ImageRead",
			invoke: func(opts ToolExecutionOptions) (string, bool, bool) {
				sink, err := newImageReadPayloadSink(nil)
				if err != nil {
					t.Fatal(err)
				}
				opts.imageReadSink = sink
				opts.ToolCallID = "external-image-call"
				return ExecuteAdvancedToolWithOptions("ImageRead", input(map[string]string{"file_path": imageCanary}), workspace, opts)
			},
			want: "Image:",
		},
		{
			name: "NotebookRead",
			invoke: func(opts ToolExecutionOptions) (string, bool, bool) {
				return ExecuteExtendedToolWithOptions("NotebookRead", input(map[string]string{"file_path": notebook}), workspace, opts)
			},
			want: "external-cell-value",
		},
		{
			name: "RepoMap",
			invoke: func(opts ToolExecutionOptions) (string, bool, bool) {
				return ExecuteExtendedToolWithOptions("RepoMap", input(map[string]string{"path": external}), workspace, opts)
			},
			want: "ExternalSymbol",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			fullOutput, fullError, handled := test.invoke(ToolExecutionOptions{ExecutionPolicy: full})
			if !handled || fullError || !strings.Contains(fullOutput, test.want) {
				t.Fatalf("full result handled=%t error=%t output=%q; want output containing %q", handled, fullError, fullOutput, test.want)
			}
			workspaceOutput, workspaceError, handled := test.invoke(ToolExecutionOptions{ExecutionPolicy: workspacePolicy})
			if !handled || !workspaceError || !strings.Contains(strings.ToLower(workspaceOutput), "blocked") {
				t.Fatalf("workspace result handled=%t error=%t output=%q; want blocked", handled, workspaceError, workspaceOutput)
			}
		})
	}
	if data, err := os.ReadFile(directWrite); err != nil || string(data) != "full external write" {
		t.Fatalf("direct full-mode Write contents/error = %q/%v", data, err)
	}
	protected := filepath.Join(external, ".env")
	write(protected, "protected")
	output, isErr := ExecuteToolWithOptions("Read", input(map[string]string{"file_path": protected}), workspace, ToolExecutionOptions{ExecutionPolicy: full})
	if !isErr || !strings.Contains(output, "Blocked path") {
		t.Fatalf("direct full-mode Read of default blocked path = %q (error=%t), want blocked", output, isErr)
	}
}
