package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoadProjectContextFollowsAncestorThenWorkspaceScope(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "parent")
	workspace := filepath.Join(parent, "workspace")
	sibling := filepath.Join(parent, "sibling")
	for _, dir := range []string{workspace, sibling, filepath.Join(workspace, "src")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeContextFile(t, filepath.Join(parent, "AGENTS.md"), "parent agents marker: canonical rule")
	writeContextFile(t, filepath.Join(parent, "CLAUDE.md"), "parent claude marker: conflicting suggestion")
	writeContextFile(t, filepath.Join(workspace, "AGENTS.md"), "workspace agents marker: canonical rule")
	writeContextFile(t, filepath.Join(workspace, "CLAUDE.md"), "workspace claude marker: conflicting suggestion")
	writeContextFile(t, filepath.Join(sibling, "AGENTS.md"), "sibling marker must not load")
	writeContextFile(t, filepath.Join(workspace, "src", "AGENTS.md"), "nested marker is path scoped")

	home := filepath.Join(root, "home")
	writeContextFile(t, filepath.Join(home, ".claude", "CLAUDE.md"), "user global marker")
	setContextHome(t, home)
	contextText := LoadProjectContext(workspace)
	markers := []string{
		"user global marker",
		"parent claude marker",
		"parent agents marker",
		"workspace claude marker",
		"workspace agents marker",
	}
	last := -1
	for _, marker := range markers {
		index := strings.Index(contextText, marker)
		if index < 0 || index <= last {
			t.Fatalf("project context does not preserve ancestor-to-workspace order for %q: %s", marker, contextText)
		}
		last = index
	}
	for _, excluded := range []string{"sibling marker", "nested marker is path scoped"} {
		if strings.Contains(contextText, excluded) {
			t.Fatalf("workspace context unexpectedly loaded %q: %s", excluded, contextText)
		}
	}
	if !strings.Contains(contextText, "AGENTS.md is authoritative over CLAUDE.md") {
		t.Fatalf("same-scope conflict contract missing: %s", contextText)
	}
	precedence := strings.Index(contextText, "## Effective Instruction Precedence")
	if precedence <= strings.Index(contextText, "workspace agents marker") {
		t.Fatalf("effective precedence summary must follow all instruction bodies: %s", contextText)
	}
}

func TestNestedProjectInstructionsAreDisclosedBeforeFileTools(t *testing.T) {
	workspace := t.TempDir()
	scope := filepath.Join(workspace, "src")
	deeper := filepath.Join(scope, "components")
	sibling := filepath.Join(workspace, "docs")
	for _, dir := range []string{deeper, sibling} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeContextFile(t, filepath.Join(scope, "AGENTS.md"), "src instructions v1")
	writeContextFile(t, filepath.Join(deeper, "CLAUDE.md"), "component instructions")
	writeContextFile(t, filepath.Join(sibling, "AGENTS.md"), "unrelated docs instructions")
	setContextHome(t, filepath.Join(t.TempDir(), "home"))

	target := filepath.Join(deeper, "Widget.tsx")
	if err := os.WriteFile(target, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	disclosure := newProjectInstructionDisclosure()
	read := dispatchTestCall("nested-read", "Read", map[string]any{"file_path": target})
	allowed, message := disclosure.CheckTool(workspace, nil, read)
	if allowed || !strings.Contains(message, "src instructions v1") || !strings.Contains(message, "component instructions") {
		t.Fatalf("first nested read disclosure allowed=%v message=%q", allowed, message)
	}
	if strings.Contains(message, "unrelated docs instructions") {
		t.Fatalf("unrelated sibling instruction leaked: %q", message)
	}
	relativeRead := dispatchTestCall("nested-read-relative", "Read", map[string]any{
		"file_path": filepath.Join("src", "components", "Widget.tsx"),
	})
	allowed, message = disclosure.CheckTool(workspace, nil, relativeRead)
	if !allowed || message != "" {
		t.Fatalf("relative alias bypassed canonical instruction disclosure: allowed=%v message=%q", allowed, message)
	}

	writeContextFile(t, filepath.Join(scope, "AGENTS.md"), "src instructions v2")
	allowed, message = disclosure.CheckTool(workspace, nil, read)
	if allowed || !strings.Contains(message, "src instructions v2") {
		t.Fatalf("changed instructions were not disclosed again: allowed=%v message=%q", allowed, message)
	}
}

func TestProjectInstructionCheckCoversNestedReadersAndTreeSearch(t *testing.T) {
	workspace := t.TempDir()
	src := filepath.Join(workspace, "src")
	components := filepath.Join(src, "components")
	models := filepath.Join(src, "models")
	docs := filepath.Join(workspace, "docs")
	github := filepath.Join(workspace, ".github")
	for _, dir := range []string{components, models, docs, github} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeContextFile(t, filepath.Join(src, "AGENTS.md"), "src tree instructions")
	writeContextFile(t, filepath.Join(components, "CLAUDE.md"), "component tree instructions")
	writeContextFile(t, filepath.Join(models, "AGENTS.md"), "model tree instructions")
	writeContextFile(t, filepath.Join(docs, "AGENTS.md"), "docs must stay outside src search")
	writeContextFile(t, filepath.Join(github, "AGENTS.md"), "hidden github instructions")
	setContextHome(t, filepath.Join(t.TempDir(), "home"))

	for _, test := range []struct {
		name  string
		input map[string]any
		want  []string
		avoid []string
	}{
		{
			name:  "notebook reader",
			input: map[string]any{"file_path": filepath.Join(components, "analysis.ipynb")},
			want:  []string{"src tree instructions", "component tree instructions"},
		},
		{
			name:  "pdf reader",
			input: map[string]any{"file_path": filepath.Join(components, "report.pdf")},
			want:  []string{"src tree instructions", "component tree instructions"},
		},
		{
			name:  "directory listing",
			input: map[string]any{"path": filepath.Join(src, "components")},
			want:  []string{"src tree instructions", "component tree instructions"},
			avoid: []string{"docs must stay outside src search"},
		},
		{
			name:  "recursive glob",
			input: map[string]any{"path": "src", "pattern": "**/*.go"},
			want:  []string{"src tree instructions", "component tree instructions"},
			avoid: []string{"docs must stay outside src search"},
		},
		{
			name:  "pattern-scoped recursive glob",
			input: map[string]any{"path": ".", "pattern": "src/**/*.go"},
			want:  []string{"src tree instructions", "component tree instructions"},
			avoid: []string{"docs must stay outside src search"},
		},
		{
			name:  "recursive grep",
			input: map[string]any{"path": "src", "pattern": "Widget"},
			want:  []string{"src tree instructions", "component tree instructions"},
			avoid: []string{"docs must stay outside src search"},
		},
		{
			name:  "pattern-scoped recursive grep",
			input: map[string]any{"path": ".", "pattern": "Widget", "glob": "src/**/*.go"},
			want:  []string{"src tree instructions", "component tree instructions"},
			avoid: []string{"docs must stay outside src search"},
		},
		{
			name:  "exclude pattern grep covers remaining workspace",
			input: map[string]any{"path": ".", "pattern": "Widget", "glob": "!src/**/*.go"},
			want:  []string{"docs must stay outside src search", "hidden github instructions"},
		},
		{
			name:  "brace-expanded grep",
			input: map[string]any{"path": "src", "pattern": "Widget", "glob": "{components,models}/**/*.go"},
			want:  []string{"src tree instructions", "component tree instructions", "model tree instructions"},
			avoid: []string{"docs must stay outside src search"},
		},
		{
			name:  "repo map",
			input: map[string]any{"path": "."},
			want:  []string{"src tree instructions", "component tree instructions", "docs must stay outside src search"},
			avoid: []string{"hidden github instructions"},
		},
		{
			name:  "recursive glob includes hidden instructions",
			input: map[string]any{"path": ".", "pattern": "**/*.go"},
			want:  []string{"hidden github instructions"},
		},
		{
			name:  "whole workspace diff",
			input: map[string]any{},
			want:  []string{"src tree instructions", "component tree instructions", "docs must stay outside src search", "hidden github instructions"},
		},
		{
			name:  "git commit selected path",
			input: map[string]any{"files": filepath.Join("src", "components", "Widget.go"), "message": "commit"},
			want:  []string{"src tree instructions", "component tree instructions"},
			avoid: []string{"docs must stay outside src search"},
		},
		{
			name:  "git commit staged scope",
			input: map[string]any{"scope": "staged", "message": "commit"},
			want:  []string{"src tree instructions", "component tree instructions", "docs must stay outside src search", "hidden github instructions"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			name := "NotebookRead"
			if test.name == "pdf reader" {
				name = "PDFRead"
			} else if test.name == "directory listing" {
				name = "LS"
			} else if strings.Contains(test.name, "glob") {
				name = "Glob"
			} else if strings.Contains(test.name, "grep") {
				name = "Grep"
			} else if strings.Contains(test.name, "diff") {
				name = "GitDiff"
			} else if strings.Contains(test.name, "commit") {
				name = "GitCommit"
			} else if test.name == "repo map" {
				name = "RepoMap"
			} else if test.name == "whole workspace diff" {
				name = "GitDiff"
			}
			call := dispatchTestCall("scope-"+strings.ReplaceAll(test.name, " ", "-"), name, test.input)
			allowed, message := newProjectInstructionDisclosure().CheckTool(workspace, nil, call)
			if allowed {
				t.Fatalf("%s did not disclose nested instructions: allowed=%v message=%q input=%#v", name, allowed, message, test.input)
			}
			for _, marker := range test.want {
				if !strings.Contains(message, marker) {
					t.Errorf("%s disclosure missing %q: %s", name, marker, message)
				}
			}
			for _, marker := range test.avoid {
				if strings.Contains(message, marker) {
					t.Errorf("%s disclosure included unrelated %q: %s", name, marker, message)
				}
			}
		})
	}
}

func TestProjectInstructionDisclosureRunsBeforeMutationAndCannotBeSkippedByAlias(t *testing.T) {
	workspace := t.TempDir()
	scope := filepath.Join(workspace, "src")
	if err := os.MkdirAll(scope, 0o700); err != nil {
		t.Fatal(err)
	}
	writeContextFile(t, filepath.Join(scope, "AGENTS.md"), "write requires generated header")
	setContextHome(t, filepath.Join(t.TempDir(), "home"))
	disclosure := newProjectInstructionDisclosure()
	call := dispatchTestCall("nested-write", "Write", map[string]any{
		"file_path": filepath.Join(scope, "generated.txt"),
		"content":   "generated",
	})
	executed := 0
	opts := toolDispatchOptions{
		Context:          context.Background(),
		WorkDir:          workspace,
		AllowedTools:     dispatchAllowedTools("Write", "Read"),
		PermissionConfig: dispatchPermissionConfig("all"),
		SnapshotDecision: func(toolUseBlock) string { return "allow" },
		InstructionCheck: func(call toolUseBlock) (bool, string) {
			return disclosure.CheckTool(workspace, nil, call)
		},
		InstructionBegin:  disclosure.BeginBatch,
		InstructionFinish: disclosure.FinishBatch,
		Execute: func(call toolUseBlock) (string, bool) {
			executed++
			return ExecuteTool(call.Name, call.Input, workspace)
		},
	}
	read := dispatchTestCall("same-batch-read", "Read", map[string]any{"file_path": filepath.Join(scope, "generated.txt")})
	first := dispatchToolCalls([]toolUseBlock{call, read}, opts)
	if len(first) != 2 || !first[0].Synthetic || first[0].Executed || !strings.Contains(first[0].Content, "write requires generated header") ||
		!first[1].Synthetic || first[1].Executed || !strings.Contains(first[1].Content, "another call in the same tool batch") {
		t.Fatalf("first dispatch did not disclose instructions before execution: %#v", first)
	}
	if executed != 0 {
		t.Fatalf("first dispatch executed before instruction review: %d", executed)
	}
	if _, err := os.Stat(filepath.Join(scope, "generated.txt")); !os.IsNotExist(err) {
		t.Fatalf("first dispatch created the target before disclosure: stat error=%v", err)
	}

	alias := filepath.Join(workspace, "source-alias")
	if err := os.Symlink(scope, alias); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("directory symlink is unavailable: %v", err)
		}
		t.Fatal(err)
	}
	call = dispatchTestCall("nested-write-alias", "Write", map[string]any{
		"file_path": filepath.Join(alias, "generated.txt"),
		"content":   "generated",
	})
	second := dispatchToolCalls([]toolUseBlock{call}, opts)
	if len(second) != 1 || !second[0].Executed || second[0].IsError {
		t.Fatalf("symlink alias bypassed disclosure or failed after review: %#v", second)
	}
	if executed != 1 {
		t.Fatalf("reviewed mutation execution count=%d, want 1", executed)
	}
	content, err := os.ReadFile(filepath.Join(scope, "generated.txt"))
	if err != nil || string(content) != "generated" {
		t.Fatalf("reviewed mutation content=%q err=%v", content, err)
	}
}

func TestProjectInstructionsCanonicalizeWorkspaceSymlinkAndCase(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "Project")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	writeContextFile(t, filepath.Join(root, "AGENTS.md"), "canonical parent marker")
	writeContextFile(t, filepath.Join(workspace, "AGENTS.md"), "canonical workspace marker")
	setContextHome(t, filepath.Join(t.TempDir(), "home"))

	alias := filepath.Join(root, "project-alias")
	if err := os.Symlink(workspace, alias); err != nil {
		if runtime.GOOS != "windows" {
			t.Fatal(err)
		}
		t.Logf("directory symlink unavailable; case normalization checks will continue: %v", err)
	} else if got, want := LoadProjectContext(alias), LoadProjectContext(workspace); got != want {
		t.Fatalf("workspace symlink changed instruction resolution\naliased: %s\ncanonical: %s", got, want)
	}

	caseVariant := filepath.Join(root, "pROJECT")
	workspaceInfo, err := os.Stat(workspace)
	if err != nil {
		t.Fatal(err)
	}
	caseInfo, err := os.Stat(caseVariant)
	if err == nil && os.SameFile(workspaceInfo, caseInfo) {
		if got, want := LoadProjectContext(caseVariant), LoadProjectContext(workspace); got != want {
			t.Fatalf("case-insensitive filesystem changed instruction resolution\ncase variant: %s\ncanonical: %s", got, want)
		}
	} else {
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if got := LoadProjectContext(caseVariant); strings.Contains(got, "canonical workspace marker") {
			t.Fatalf("case-sensitive filesystem treated different casing as the workspace: %s", got)
		}
	}
}

func TestScopedProjectInstructionsIgnoreExternalFullModeTargets(t *testing.T) {
	workspace := t.TempDir()
	external := t.TempDir()
	writeContextFile(t, filepath.Join(external, "AGENTS.md"), "external instructions stay outside project scope")
	if files := scopedProjectInstructionFiles(workspace, filepath.Join(external, "file.txt")); len(files) != 0 {
		t.Fatalf("external target instructions were loaded into workspace scope: %#v", files)
	}
	call := dispatchTestCall("external-read", "Read", map[string]any{"file_path": filepath.Join(external, "file.txt")})
	full := &ExecutionPolicySnapshot{Mode: ExecutionModeFull}
	allowed, message := newProjectInstructionDisclosure().CheckTool(workspace, full, call)
	if !allowed || message != "" {
		t.Fatalf("external full-mode target caused external instruction lookup: allowed=%v message=%q", allowed, message)
	}
	for _, tool := range []struct {
		name  string
		input map[string]any
	}{
		{name: "Glob", input: map[string]any{"path": external, "pattern": "**/*.go"}},
		{name: "Grep", input: map[string]any{"path": external, "pattern": "needle"}},
		{name: "RepoMap", input: map[string]any{"path": external}},
		{name: "GitDiff", input: map[string]any{"file": external}},
	} {
		call := dispatchTestCall("external-"+tool.name, tool.name, tool.input)
		allowed, message = newProjectInstructionDisclosure().CheckTool(workspace, full, call)
		if !allowed || message != "" {
			t.Errorf("external full-mode %s caused external instruction lookup: allowed=%v message=%q", tool.name, allowed, message)
		}
	}
}

func TestRecursiveProjectInstructionDiscoveryFailsClosedAtBound(t *testing.T) {
	workspace := t.TempDir()
	for index := 0; index <= maxTreeInstructionFiles; index++ {
		scope := filepath.Join(workspace, fmt.Sprintf("scope-%02d", index))
		if err := os.MkdirAll(scope, 0o700); err != nil {
			t.Fatal(err)
		}
		writeContextFile(t, filepath.Join(scope, "AGENTS.md"), "nested rule")
	}
	call := dispatchTestCall("large-grep", "Grep", map[string]any{"path": workspace, "pattern": "needle"})
	allowed, message := newProjectInstructionDisclosure().CheckTool(workspace, nil, call)
	if allowed || !strings.Contains(message, "narrow the directory") {
		t.Fatalf("large recursive instruction set did not fail closed: allowed=%v message=%q", allowed, message)
	}
}

func TestGitDiffDirectoryInstructionDiscoveryFailsClosedAtBound(t *testing.T) {
	workspace := t.TempDir()
	for index := 0; index <= maxTreeInstructionFiles; index++ {
		scope := filepath.Join(workspace, fmt.Sprintf("scope-%02d", index))
		if err := os.MkdirAll(scope, 0o700); err != nil {
			t.Fatal(err)
		}
		writeContextFile(t, filepath.Join(scope, "AGENTS.md"), "nested rule")
	}
	call := dispatchTestCall("large-gitdiff-directory", "GitDiff", map[string]any{"file": workspace})
	allowed, message := newProjectInstructionDisclosure().CheckTool(workspace, nil, call)
	if allowed || !strings.Contains(message, "requested diff path") {
		t.Fatalf("directory GitDiff discovery did not fail closed at bound: allowed=%v message=%q", allowed, message)
	}
}

func TestGitDiffRejectsWildcardPathspecBeforeNestedScopeCanBeSkipped(t *testing.T) {
	workspace := t.TempDir()
	writeContextFile(t, filepath.Join(workspace, "src", "components", "AGENTS.md"), "nested diff instructions")
	for _, pathspec := range []string{"src/**", ":(exclude)src/components"} {
		input := dispatchTestCall("gitdiff-pathspec", "GitDiff", map[string]any{"file": pathspec}).Input
		allowed, reason, _ := CheckPermission("GitDiff", input, workspace, PermissionConfig{AutoApprove: "all"})
		if allowed || !strings.Contains(reason, "literal file or directory paths only") {
			t.Errorf("GitDiff pathspec %q was not rejected before nested instructions could be skipped: allowed=%v reason=%q", pathspec, allowed, reason)
		}
		result, isError := ExecuteTool("GitDiff", input, workspace)
		if !isError || !strings.Contains(result, "literal file or directory paths only") {
			t.Errorf("direct GitDiff pathspec %q did not fail safely: isError=%v result=%q", pathspec, isError, result)
		}
	}
}

func writeContextFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func setContextHome(t *testing.T, home string) {
	t.Helper()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
	} else {
		t.Setenv("HOME", home)
	}
}
