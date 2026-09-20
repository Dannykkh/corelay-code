package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

func TestGitCommitSpecifiedPathPreservesOtherStagedChanges(t *testing.T) {
	repo := newGitCommitFixture(t)
	writeGitCommitFile(t, repo, "A.txt", "A updated\n")
	writeGitCommitFile(t, repo, "B.txt", "B staged update\n")
	runGitCommitFixture(t, repo, "add", "--", "B.txt")

	input, err := json.Marshal(map[string]any{"message": "commit A only", "files": "A.txt"})
	if err != nil {
		t.Fatal(err)
	}
	output, isError := ExecuteToolWithOptions("GitCommit", input, repo, gitCommitExecutorOptions())
	if isError {
		t.Fatalf("GitCommit failed: %s", output)
	}

	committed := runGitCommitFixture(t, repo, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD")
	if strings.TrimSpace(committed) != "A.txt" {
		t.Fatalf("new commit contains %q, want only A.txt; output=%s", strings.TrimSpace(committed), output)
	}
	staged := strings.Fields(runGitCommitFixture(t, repo, "diff", "--cached", "--name-only"))
	if len(staged) != 1 || staged[0] != "B.txt" {
		t.Fatalf("other staged changes after commit = %#v, want [B.txt]", staged)
	}
	if output := runGitCommitFixture(t, repo, "diff", "--cached", "--quiet", "HEAD", "--", "A.txt"); strings.TrimSpace(output) != "" {
		t.Fatalf("selected A remains staged after commit: %q", output)
	}
}

func TestGitCommitSelectedPathFromNestedWorkspaceUsesRepositoryRelativeHookScope(t *testing.T) {
	repo := newGitCommitFixture(t)
	writeGitCommitFile(t, repo, filepath.Join("nested", "A.txt"), "nested A base\n")
	runGitCommitFixture(t, repo, "add", "--", "nested/A.txt")
	runGitCommitFixture(t, repo, "commit", "-m", "add nested A")
	writeGitCommitFile(t, repo, filepath.Join("nested", "A.txt"), "nested A update\n")
	workspace := filepath.Join(repo, "nested")

	input, _ := json.Marshal(map[string]any{"message": "commit nested A", "files": "A.txt"})
	output, isError := ExecuteToolWithOptions("GitCommit", input, workspace, gitCommitExecutorOptions())
	if isError {
		t.Fatalf("GitCommit from nested workspace failed: %s", output)
	}
	if got := strings.TrimSpace(runGitCommitFixture(t, repo, "show", "HEAD:nested/A.txt")); got != "nested A update" {
		t.Fatalf("nested selected path commit produced %q", got)
	}
	if got := strings.TrimSpace(runGitCommitFixture(t, repo, "show", "HEAD:A.txt")); got != "A base" {
		t.Fatalf("nested selected path commit changed root A: %q", got)
	}
}

func TestGitCommitOnlyUsesHeadAdvancedBeforeInvocation(t *testing.T) {
	repo := newGitCommitFixture(t)
	writeGitCommitFile(t, repo, "C.txt", "C committed before selected operation\n")
	runGitCommitFixture(t, repo, "add", "--", "C.txt")
	runGitCommitFixture(t, repo, "commit", "-m", "advance HEAD")
	writeGitCommitFile(t, repo, "A.txt", "A updated\n")
	input, _ := json.Marshal(map[string]any{"message": "commit A only", "files": "A.txt"})
	output, isError := ExecuteToolWithOptions("GitCommit", input, repo, gitCommitExecutorOptions())
	if isError {
		t.Fatalf("GitCommit after HEAD advanced failed: %s", output)
	}
	if got := strings.TrimSpace(runGitCommitFixture(t, repo, "show", "HEAD:C.txt")); got != "C committed before selected operation" {
		t.Fatalf("selected commit lost a change already in HEAD: C.txt = %q", got)
	}
}

func TestGitCommitRejectsHeadChangedAfterSnapshot(t *testing.T) {
	repo := newGitCommitFixture(t)
	writeGitCommitFile(t, repo, "A.txt", "A selected change\n")
	oldHead := strings.TrimSpace(runGitCommitFixture(t, repo, "rev-parse", "HEAD"))
	options := gitCommitExecutorOptions()
	runner := options.SandboxRunner.(*fakeToolProcessRunner)
	originalRun := runner.run
	injected := false
	runner.run = func(ctx context.Context, policy sandbox.Policy, command sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
		isCommit := false
		for _, arg := range command.Args {
			if arg == "commit" {
				isCommit = true
				break
			}
		}
		if isCommit && !injected {
			injected = true
			writeGitCommitFile(t, repo, "C.txt", "external commit remains in HEAD\n")
			runGitCommitFixture(t, repo, "add", "--", "C.txt")
			runGitCommitFixture(t, repo, "commit", "-m", "external commit during preparation")
		}
		return originalRun(ctx, policy, command)
	}

	input, _ := json.Marshal(map[string]any{"message": "must detect moved HEAD", "files": "A.txt"})
	output, isError := ExecuteToolWithOptions("GitCommit", input, repo, options)
	if !isError || !strings.Contains(output, "Git HEAD changed during selected commit preparation") {
		t.Fatalf("moved HEAD was not rejected: isError=%v output=%q", isError, output)
	}
	if got := strings.TrimSpace(runGitCommitFixture(t, repo, "rev-parse", "HEAD")); got == oldHead {
		t.Fatalf("test did not advance HEAD externally: %s", got)
	}
	if got := strings.TrimSpace(runGitCommitFixture(t, repo, "show", "HEAD:C.txt")); got != "external commit remains in HEAD" {
		t.Fatalf("external HEAD change was lost: C.txt = %q", got)
	}
}

func TestGitCommitChecksHeadAfterConfiguredPreCommitHook(t *testing.T) {
	repo := newGitCommitFixture(t)
	writeGitCommitFile(t, repo, "A.txt", "A selected change\n")
	oldHead := strings.TrimSpace(runGitCommitFixture(t, repo, "rev-parse", "HEAD"))
	hookDir := filepath.Join(repo, "custom-hooks")
	if err := os.MkdirAll(hookDir, 0o700); err != nil {
		t.Fatal(err)
	}
	hook := "#!/bin/sh\n" +
		"index_path=$(git rev-parse --git-path index) || exit 2\n" +
		"printf 'committed by configured hook\\n' > C.txt\n" +
		"GIT_INDEX_FILE=\"$index_path\" git add -- C.txt || exit 2\n" +
		"GIT_INDEX_FILE=\"$index_path\" git -c core.hooksPath=/dev/null commit -m 'hook commit' -- C.txt\n"
	if err := os.WriteFile(filepath.Join(hookDir, "pre-commit"), []byte(hook), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(hookDir, "pre-commit"), 0o700); err != nil {
		t.Fatal(err)
	}
	runGitCommitFixture(t, repo, "config", "core.hooksPath", hookDir)

	input, _ := json.Marshal(map[string]any{"message": "outer selected commit", "files": "A.txt"})
	output, isError := ExecuteToolWithOptions("GitCommit", input, repo, gitCommitExecutorOptions())
	if !isError || !strings.Contains(output, "Git HEAD changed during selected commit preparation") {
		t.Fatalf("HEAD change from configured pre-commit hook was not rejected: isError=%v output=%q", isError, output)
	}
	if got := strings.TrimSpace(runGitCommitFixture(t, repo, "show", "HEAD:C.txt")); got != "committed by configured hook" {
		t.Fatalf("configured hook's HEAD update was lost: C.txt = %q", got)
	}
	if got := strings.TrimSpace(runGitCommitFixture(t, repo, "show", "HEAD:A.txt")); got != "A base" {
		t.Fatalf("outer selected commit unexpectedly changed A in HEAD: %q", got)
	}
	if got := strings.TrimSpace(runGitCommitFixture(t, repo, "rev-parse", "HEAD")); got == oldHead {
		t.Fatalf("test hook did not advance HEAD from %s", oldHead)
	}
}

func TestGitCommitRejectsPreCommitHookAddingOutsidePathToCandidateIndex(t *testing.T) {
	repo := newGitCommitFixture(t)
	writeGitCommitFile(t, repo, "A.txt", "A selected change\n")
	writeGitCommitFile(t, repo, "B.txt", "B hook-added change\n")
	oldHead := strings.TrimSpace(runGitCommitFixture(t, repo, "rev-parse", "HEAD"))
	hookDir := filepath.Join(repo, "custom-hooks")
	if err := os.MkdirAll(hookDir, 0o700); err != nil {
		t.Fatal(err)
	}
	hook := "#!/bin/sh\n" + "git add -- B.txt\n"
	if err := os.WriteFile(filepath.Join(hookDir, "pre-commit"), []byte(hook), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(hookDir, "pre-commit"), 0o700); err != nil {
		t.Fatal(err)
	}
	runGitCommitFixture(t, repo, "config", "core.hooksPath", hookDir)

	input, _ := json.Marshal(map[string]any{"message": "must reject hook-added B", "files": "A.txt"})
	output, isError := ExecuteToolWithOptions("GitCommit", input, repo, gitCommitExecutorOptions())
	if !isError || !strings.Contains(output, "selected_scope_conflict") {
		t.Fatalf("candidate index change outside selected scope was not rejected: isError=%v output=%q", isError, output)
	}
	if got := strings.TrimSpace(runGitCommitFixture(t, repo, "rev-parse", "HEAD")); got != oldHead {
		t.Fatalf("hook-added candidate path changed HEAD from %s to %s", oldHead, got)
	}
	if got := strings.TrimSpace(runGitCommitFixture(t, repo, "show", "HEAD:B.txt")); got != "B base" {
		t.Fatalf("hook-added B unexpectedly reached HEAD: %q", got)
	}
	if got := strings.TrimSpace(runGitCommitFixture(t, repo, "diff", "--cached", "--name-only")); got != "" {
		t.Fatalf("candidate-only hook staging leaked into the real index: %q", got)
	}
	worktree, err := os.ReadFile(filepath.Join(repo, "B.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(worktree)); got != "B hook-added change" {
		t.Fatalf("rejected hook changed the worktree: %q", got)
	}
}

func TestGitCommitOriginalHookPathFindsWindowsExe(t *testing.T) {
	hooks := t.TempDir()
	base := filepath.Join(hooks, "pre-commit")
	exe := base + ".exe"
	if err := os.WriteFile(exe, []byte("MZ"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := gitCommitOriginalHookPath(hooks, "pre-commit"); runtime.GOOS == "windows" && got != exe {
		t.Fatalf("Windows hook selection = %q, want %q", got, exe)
	} else if runtime.GOOS != "windows" && got != base {
		t.Fatalf("non-Windows hook selection = %q, want %q", got, base)
	}
	if err := os.WriteFile(base, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := gitCommitOriginalHookPath(hooks, "pre-commit"); got != base {
		t.Fatalf("shebang hook should take precedence over .exe: got %q, want %q", got, base)
	}
}

func TestGitCommitHookGuardIsCreatedInGitMetadataDirectory(t *testing.T) {
	repo := newGitCommitFixture(t)
	workspace := filepath.Join(repo, "nested")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	options := gitCommitExecutorOptions()
	indexPath, err := gitCommitIndexPath(options, workspace)
	if err != nil {
		t.Fatal(err)
	}
	head, hasHead, err := gitCommitOptionalHead(options, workspace)
	if err != nil {
		t.Fatal(err)
	}
	hooksPath, cleanup, err := prepareGitCommitHookGuard(options, workspace, indexPath, indexPath, head, hasHead, []string{"nested/A.txt"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if !pathWithin(hooksPath, filepath.Dir(indexPath)) {
		t.Fatalf("Git hook guard path %q is outside the active index directory %q", hooksPath, filepath.Dir(indexPath))
	}
}

func TestGitCommitSelectedPathsSupportUnbornHead(t *testing.T) {
	repo := t.TempDir()
	runGitCommitFixture(t, repo, "init", "-q")
	runGitCommitFixture(t, repo, "config", "user.name", "Corelay Test")
	runGitCommitFixture(t, repo, "config", "user.email", "corelay-test@example.invalid")
	writeGitCommitFile(t, repo, "A.txt", "A root commit\n")
	writeGitCommitFile(t, repo, "B.txt", "B staged root content\n")
	runGitCommitFixture(t, repo, "add", "--", "B.txt")
	input, _ := json.Marshal(map[string]any{"message": "root commit A only", "files": "A.txt"})
	output, isError := ExecuteToolWithOptions("GitCommit", input, repo, gitCommitExecutorOptions())
	if isError {
		t.Fatalf("GitCommit failed for an unborn HEAD: %s", output)
	}
	if got := strings.TrimSpace(runGitCommitFixture(t, repo, "ls-tree", "--name-only", "HEAD")); got != "A.txt" {
		t.Fatalf("root commit contains %q, want only A.txt", got)
	}
	if got := strings.TrimSpace(runGitCommitFixture(t, repo, "diff", "--cached", "--name-only")); got != "B.txt" {
		t.Fatalf("staged paths after root commit = %q, want B.txt", got)
	}
}

func TestGitCommitRequiresExplicitStagedScopeWhenFilesAreOmitted(t *testing.T) {
	repo := newGitCommitFixture(t)
	writeGitCommitFile(t, repo, "B.txt", "B staged update\n")
	runGitCommitFixture(t, repo, "add", "--", "B.txt")
	oldHead := strings.TrimSpace(runGitCommitFixture(t, repo, "rev-parse", "HEAD"))

	implicit, _ := json.Marshal(map[string]any{"message": "must not commit implicitly"})
	output, isError := ExecuteToolWithOptions("GitCommit", implicit, repo, gitCommitExecutorOptions())
	if !isError || !strings.Contains(output, "scope=staged") {
		t.Fatalf("GitCommit without files or scope was not rejected: isError=%v output=%q", isError, output)
	}
	if got := strings.TrimSpace(runGitCommitFixture(t, repo, "rev-parse", "HEAD")); got != oldHead {
		t.Fatalf("implicit commit changed HEAD from %s to %s", oldHead, got)
	}
	if got := strings.TrimSpace(runGitCommitFixture(t, repo, "diff", "--cached", "--name-only")); got != "B.txt" {
		t.Fatalf("implicit commit changed staged index: %q", got)
	}

	explicit, _ := json.Marshal(map[string]any{"message": "commit staged scope", "scope": "staged"})
	output, isError = ExecuteToolWithOptions("GitCommit", explicit, repo, gitCommitExecutorOptions())
	if isError {
		t.Fatalf("explicit staged-scope commit failed: %s", output)
	}
	committed := strings.TrimSpace(runGitCommitFixture(t, repo, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD"))
	if committed != "B.txt" {
		t.Fatalf("scope=staged committed %q, want B.txt", committed)
	}
}

func TestGitCommitStagedScopeRequiresRepositoryRootWorkspace(t *testing.T) {
	repo := newGitCommitFixture(t)
	projectA := filepath.Join(repo, "projectA")
	projectB := filepath.Join(repo, "projectB")
	if err := os.MkdirAll(projectA, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projectB, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectA, "AGENTS.md"), []byte("workspace A instructions\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectB, "AGENTS.md"), []byte("workspace B instructions\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeGitCommitFile(t, repo, filepath.Join("projectB", "change.txt"), "staged in sibling project\n")
	runGitCommitFixture(t, repo, "add", "--", "projectB/change.txt")
	oldHead := strings.TrimSpace(runGitCommitFixture(t, repo, "rev-parse", "HEAD"))
	input, _ := json.Marshal(map[string]any{"message": "must not commit sibling project", "scope": "staged"})
	output, isError := ExecuteToolWithOptions("GitCommit", input, projectA, gitCommitExecutorOptions())
	if !isError || !strings.Contains(output, "Git repository root") {
		t.Fatalf("nested workspace staged scope was not rejected: isError=%v output=%q", isError, output)
	}
	if got := strings.TrimSpace(runGitCommitFixture(t, repo, "rev-parse", "HEAD")); got != oldHead {
		t.Fatalf("rejected nested staged scope changed HEAD from %s to %s", oldHead, got)
	}
	if got := strings.TrimSpace(runGitCommitFixture(t, repo, "diff", "--cached", "--name-only")); got != "projectB/change.txt" {
		t.Fatalf("rejected nested staged scope changed staged sibling path: %q", got)
	}
}

func TestGitCommitRejectsStagedChangesOnSelectedPaths(t *testing.T) {
	repo := newGitCommitFixture(t)
	writeGitCommitFile(t, repo, "A.txt", "A staged version\n")
	runGitCommitFixture(t, repo, "add", "--", "A.txt")
	writeGitCommitFile(t, repo, "A.txt", "A remaining worktree version\n")
	oldHead := strings.TrimSpace(runGitCommitFixture(t, repo, "rev-parse", "HEAD"))

	input, _ := json.Marshal(map[string]any{"message": "must reject partial stage", "files": "A.txt"})
	output, isError := ExecuteToolWithOptions("GitCommit", input, repo, gitCommitExecutorOptions())
	if !isError || !strings.Contains(output, "staged_conflict") {
		t.Fatalf("selected staged changes were not rejected: isError=%v output=%q", isError, output)
	}
	if got := strings.TrimSpace(runGitCommitFixture(t, repo, "rev-parse", "HEAD")); got != oldHead {
		t.Fatalf("staged-conflict rejection changed HEAD from %s to %s", oldHead, got)
	}
	if got := strings.TrimSpace(runGitCommitFixture(t, repo, "diff", "--cached", "--name-only")); got != "A.txt" {
		t.Fatalf("staged-conflict rejection changed staged paths: %q", got)
	}
	if got := strings.TrimSpace(runGitCommitFixture(t, repo, "diff", "--name-only")); got != "A.txt" {
		t.Fatalf("staged-conflict rejection changed worktree paths: %q", got)
	}
}

func TestGitCommitRejectsSelectedStageCreatedAfterPreflight(t *testing.T) {
	repo := newGitCommitFixture(t)
	writeGitCommitFile(t, repo, "A.txt", "A worktree value selected for commit\n")
	oldHead := strings.TrimSpace(runGitCommitFixture(t, repo, "rev-parse", "HEAD"))
	options := gitCommitExecutorOptions()
	runner := options.SandboxRunner.(*fakeToolProcessRunner)
	originalRun := runner.run
	injected := false
	runner.run = func(ctx context.Context, policy sandbox.Policy, command sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
		isCommit := false
		for _, arg := range command.Args {
			if arg == "commit" {
				isCommit = true
				break
			}
		}
		if isCommit && !injected {
			injected = true
			writeGitCommitFile(t, repo, "A.txt", "A concurrent staged value\n")
			runGitCommitFixture(t, repo, "add", "--", "A.txt")
			writeGitCommitFile(t, repo, "A.txt", "A worktree value selected for commit\n")
		}
		return originalRun(ctx, policy, command)
	}

	input, _ := json.Marshal(map[string]any{"message": "race must abort", "files": "A.txt"})
	output, isError := ExecuteToolWithOptions("GitCommit", input, repo, options)
	if !isError || !strings.Contains(output, "staged_conflict") {
		t.Fatalf("concurrent selected staging was not rejected: isError=%v output=%q", isError, output)
	}
	if got := strings.TrimSpace(runGitCommitFixture(t, repo, "rev-parse", "HEAD")); got != oldHead {
		t.Fatalf("rejected concurrent-stage commit changed HEAD from %s to %s", oldHead, got)
	}
	if got := strings.TrimSpace(runGitCommitFixture(t, repo, "show", ":A.txt")); got != "A concurrent staged value" {
		t.Fatalf("concurrent staged data was not preserved: %q", got)
	}
	worktree, err := os.ReadFile(filepath.Join(repo, "A.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(worktree)); got != "A worktree value selected for commit" {
		t.Fatalf("worktree data changed after rejecting the stage race: %q", got)
	}
}

func TestGitCommitRejectsBusyGitIndexWithoutChangingIt(t *testing.T) {
	repo := newGitCommitFixture(t)
	writeGitCommitFile(t, repo, "A.txt", "A updated\n")
	oldHead := strings.TrimSpace(runGitCommitFixture(t, repo, "rev-parse", "HEAD"))
	indexLock := filepath.Join(repo, ".git", "index.lock")
	lockContents := []byte("owned by another Git process\n")
	if err := os.WriteFile(indexLock, lockContents, 0o600); err != nil {
		t.Fatal(err)
	}

	input, _ := json.Marshal(map[string]any{"message": "must respect existing index lock", "files": "A.txt"})
	output, isError := ExecuteToolWithOptions("GitCommit", input, repo, gitCommitExecutorOptions())
	if !isError || !strings.Contains(output, "index_busy") {
		t.Fatalf("GitCommit did not reject an existing index lock: isError=%v output=%q", isError, output)
	}
	if got := strings.TrimSpace(runGitCommitFixture(t, repo, "rev-parse", "HEAD")); got != oldHead {
		t.Fatalf("busy-index rejection changed HEAD from %s to %s", oldHead, got)
	}
	gotLockContents, err := os.ReadFile(indexLock)
	if err != nil {
		t.Fatalf("busy index lock was removed: %v", err)
	}
	if !bytes.Equal(gotLockContents, lockContents) {
		t.Fatalf("busy index lock was modified: %q", gotLockContents)
	}
}

func TestGitCommitAmendSelectedPathPreservesOtherStagedChanges(t *testing.T) {
	repo := newGitCommitFixture(t)
	runGitCommitFixture(t, repo, "commit", "--allow-empty", "-m", "parent")
	oldHead := strings.TrimSpace(runGitCommitFixture(t, repo, "rev-parse", "HEAD"))
	oldParent := strings.TrimSpace(runGitCommitFixture(t, repo, "rev-parse", "HEAD^"))
	writeGitCommitFile(t, repo, "A.txt", "A amended\n")
	writeGitCommitFile(t, repo, "B.txt", "B staged but excluded\n")
	runGitCommitFixture(t, repo, "add", "--", "B.txt")

	input, _ := json.Marshal(map[string]any{"message": "amend A only", "files": "A.txt", "amend": true})
	output, isError := ExecuteToolWithOptions("GitCommit", input, repo, gitCommitExecutorOptions())
	if isError {
		t.Fatalf("GitCommit amend failed: %s", output)
	}
	if got := strings.TrimSpace(runGitCommitFixture(t, repo, "rev-parse", "HEAD^")); got != oldParent {
		t.Fatalf("amend parent = %s, want previous parent %s (old HEAD %s)", got, oldParent, oldHead)
	}
	if got := strings.TrimSpace(runGitCommitFixture(t, repo, "show", "HEAD:A.txt")); got != "A amended" {
		t.Fatalf("amended A contents = %q", got)
	}
	if got := strings.TrimSpace(runGitCommitFixture(t, repo, "show", "HEAD:B.txt")); got != "B base" {
		t.Fatalf("amend unexpectedly included B worktree contents: %q", got)
	}
	if got := strings.TrimSpace(runGitCommitFixture(t, repo, "diff", "--cached", "--name-only")); got != "B.txt" {
		t.Fatalf("staged B after amend = %q, want B.txt", got)
	}
}

func TestGitCommitSelectedPathsSupportNewFilesDeletesAndRenames(t *testing.T) {
	repo := newGitCommitFixture(t)
	if err := os.Remove(filepath.Join(repo, "A.txt")); err != nil {
		t.Fatal(err)
	}
	writeGitCommitFile(t, repo, "added.txt", "new file\n")
	if err := os.MkdirAll(filepath.Join(repo, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(repo, "B.txt"), filepath.Join(repo, "nested", "renamed.txt")); err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(map[string]any{
		"message": "commit selected add delete rename",
		"files":   "A.txt B.txt added.txt nested/renamed.txt",
	})
	output, isError := ExecuteToolWithOptions("GitCommit", input, repo, gitCommitExecutorOptions())
	if isError {
		t.Fatalf("GitCommit add/delete/rename failed: %s", output)
	}
	status := runGitCommitFixture(t, repo, "diff-tree", "--no-commit-id", "--name-status", "--no-renames", "-r", "HEAD")
	for _, expected := range []string{"D\tA.txt", "D\tB.txt", "A\tadded.txt", "A\tnested/renamed.txt"} {
		if !strings.Contains(status, expected) {
			t.Errorf("commit tree changes missing %q: %s", expected, status)
		}
	}
}

func TestGitCommitFailureLeavesStagedAndWorkingTreeChangesUntouched(t *testing.T) {
	repo := newGitCommitFixture(t)
	writeGitCommitFile(t, repo, "A.txt", "A worktree update\n")
	writeGitCommitFile(t, repo, "new.txt", "new file remains untracked\n")
	writeGitCommitFile(t, repo, "B.txt", "B staged update\n")
	runGitCommitFixture(t, repo, "add", "--", "B.txt")
	oldHead := strings.TrimSpace(runGitCommitFixture(t, repo, "rev-parse", "HEAD"))
	hookDir := filepath.Join(repo, ".git", "hooks-failing")
	if err := os.MkdirAll(hookDir, 0o700); err != nil {
		t.Fatal(err)
	}
	hookPath := filepath.Join(hookDir, "pre-commit")
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(hookPath, 0o700); err != nil {
		t.Fatal(err)
	}
	runGitCommitFixture(t, repo, "config", "core.hooksPath", hookDir)

	input, _ := json.Marshal(map[string]any{"message": "hook failure", "files": "A.txt new.txt"})
	output, isError := ExecuteToolWithOptions("GitCommit", input, repo, gitCommitExecutorOptions())
	if !isError {
		t.Fatalf("commit with failing pre-commit hook succeeded: %s", output)
	}
	if got := strings.TrimSpace(runGitCommitFixture(t, repo, "rev-parse", "HEAD")); got != oldHead {
		t.Fatalf("failed commit changed HEAD from %s to %s", oldHead, got)
	}
	if got := strings.TrimSpace(runGitCommitFixture(t, repo, "diff", "--cached", "--name-only")); got != "B.txt" {
		t.Fatalf("failed commit changed staged paths: %q", got)
	}
	if got := strings.TrimSpace(runGitCommitFixture(t, repo, "diff", "--name-only")); got != "A.txt" {
		t.Fatalf("failed commit changed worktree paths: %q", got)
	}
	if got := strings.TrimSpace(runGitCommitFixture(t, repo, "ls-files", "--others", "--exclude-standard")); got != "new.txt" {
		t.Fatalf("failed commit did not restore selected untracked file: %q", got)
	}
}

func TestGitCommitCancellationDuringExecutionAllowsGitToFinish(t *testing.T) {
	repo := newGitCommitFixture(t)
	writeGitCommitFile(t, repo, "A.txt", "A committed before cancellation\n")
	oldHead := strings.TrimSpace(runGitCommitFixture(t, repo, "rev-parse", "HEAD"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	options := gitCommitExecutorOptions()
	options.Context = ctx
	runner := options.SandboxRunner.(*fakeToolProcessRunner)
	originalRun := runner.run
	runner.run = func(ctx context.Context, policy sandbox.Policy, command sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
		for _, arg := range command.Args {
			if arg == "commit" {
				cancel()
				break
			}
		}
		return originalRun(ctx, policy, command)
	}

	input, _ := json.Marshal(map[string]any{"message": "cancel during commit", "files": "A.txt"})
	output, isError := ExecuteToolWithOptions("GitCommit", input, repo, options)
	if isError {
		t.Fatalf("GitCommit did not finish after caller cancellation: %s", output)
	}
	if ctx.Err() == nil {
		t.Fatal("test did not cancel caller context during GitCommit")
	}
	if got := strings.TrimSpace(runGitCommitFixture(t, repo, "rev-parse", "HEAD")); got == oldHead {
		t.Fatalf("HEAD did not advance after simulated post-commit cancellation: %s", got)
	}
	if got := strings.TrimSpace(runGitCommitFixture(t, repo, "diff", "--cached", "--quiet", "HEAD", "--", "A.txt")); got != "" {
		t.Fatalf("selected index is not reconciled after cancellation: %q", got)
	}
	if !strings.Contains(output, "cancel during commit") {
		t.Fatalf("successful commit output was not returned: %q", output)
	}
}

func TestGitCommitCancellationBeforeCommitRollsBackNewFileStaging(t *testing.T) {
	repo := newGitCommitFixture(t)
	writeGitCommitFile(t, repo, "B.txt", "B staged update\n")
	writeGitCommitFile(t, repo, "new.txt", "new file remains untracked\n")
	runGitCommitFixture(t, repo, "add", "--", "B.txt")
	oldHead := strings.TrimSpace(runGitCommitFixture(t, repo, "rev-parse", "HEAD"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	options := gitCommitExecutorOptions()
	options.Context = ctx
	runner := options.SandboxRunner.(*fakeToolProcessRunner)
	originalRun := runner.run
	runner.run = func(runCtx context.Context, policy sandbox.Policy, command sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
		result, report := originalRun(runCtx, policy, command)
		if len(command.Args) > 1 && command.Args[1] == "add" {
			cancel()
		}
		return result, report
	}

	input, _ := json.Marshal(map[string]any{"message": "cancel before commit", "files": "new.txt"})
	output, isError := ExecuteToolWithOptions("GitCommit", input, repo, options)
	if !isError || !strings.Contains(output, "canceled") {
		t.Fatalf("canceled GitCommit was not stopped before commit: isError=%v output=%q", isError, output)
	}
	if got := strings.TrimSpace(runGitCommitFixture(t, repo, "rev-parse", "HEAD")); got != oldHead {
		t.Fatalf("pre-commit cancellation changed HEAD from %s to %s", oldHead, got)
	}
	if got := strings.TrimSpace(runGitCommitFixture(t, repo, "diff", "--cached", "--name-only")); got != "B.txt" {
		t.Fatalf("pre-commit cancellation changed staged paths: %q", got)
	}
	if got := strings.TrimSpace(runGitCommitFixture(t, repo, "ls-files", "--others", "--exclude-standard")); got != "new.txt" {
		t.Fatalf("pre-commit cancellation did not restore new file to untracked state: %q", got)
	}
}

func newGitCommitFixture(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git is unavailable: %v", err)
	}
	repo := t.TempDir()
	runGitCommitFixture(t, repo, "init", "-q")
	runGitCommitFixture(t, repo, "config", "user.name", "Corelay Test")
	runGitCommitFixture(t, repo, "config", "user.email", "corelay-test@example.invalid")
	runGitCommitFixture(t, repo, "config", "commit.gpgsign", "false")
	writeGitCommitFile(t, repo, "A.txt", "A base\n")
	writeGitCommitFile(t, repo, "B.txt", "B base\n")
	runGitCommitFixture(t, repo, "add", "--", "A.txt", "B.txt")
	runGitCommitFixture(t, repo, "commit", "-m", "initial")
	return repo
}

func runGitCommitFixture(t *testing.T, repo string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = repo
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

func writeGitCommitFile(t *testing.T, repo, name, content string) {
	t.Helper()
	path := filepath.Join(repo, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll(%q) = %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) = %v", path, err)
	}
}

func gitCommitExecutorOptions() ToolExecutionOptions {
	runner := &fakeToolProcessRunner{name: "git-commit-fixture", capabilities: fakeBashCapabilities()}
	runner.run = func(ctx context.Context, policy sandbox.Policy, command sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
		report := sandbox.Report{
			Runner: runner.Name(), RequestedEnforcement: policy.Enforcement,
			EffectiveEnforcement: policy.Enforcement, Capabilities: runner.Capabilities(), Started: true,
		}
		process := exec.CommandContext(ctx, command.Path, command.Args...)
		process.Dir = command.Dir
		process.Env = gitCommitCommandEnvironment(command.Environment)
		var stdout, stderr bytes.Buffer
		process.Stdout = &stdout
		process.Stderr = &stderr
		result := sandbox.Result{Started: true}
		if err := process.Run(); err != nil {
			var exitError *exec.ExitError
			if errors.As(err, &exitError) {
				result.ExitCode = exitError.ExitCode()
			} else {
				result.Err = err
				result.Started = false
			}
		}
		result.Stdout = stdout.Bytes()
		result.Stderr = stderr.Bytes()
		return result, report
	}
	return secureToolProcessOptions(runner)
}

func gitCommitCommandEnvironment(environment sandbox.EnvironmentSpec) []string {
	values := make([]string, 0, len(environment.Inherit)+len(environment.Set))
	for _, name := range environment.Inherit {
		if value, ok := os.LookupEnv(name); ok {
			values = append(values, name+"="+value)
		}
	}
	for name, value := range environment.Set {
		values = append(values, name+"="+value)
	}
	return values
}
