package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

var forwardedGitCommitHooks = []string{
	"applypatch-msg",
	"pre-applypatch",
	"post-applypatch",
	"pre-commit",
	"prepare-commit-msg",
	"commit-msg",
	"post-commit",
	"post-rewrite",
	"reference-transaction",
	"post-index-change",
}

func prepareGitCommitHookGuard(
	opts ToolExecutionOptions,
	workDir string,
	indexPath string,
	expectedIndexPath string,
	head string,
	hasHead bool,
	paths []string,
) (string, func(), error) {
	result := runToolProcess(opts, "GitCommit hooks path", workDir, "git", []string{"rev-parse", "--git-path", "hooks"}, defaultToolProcessTimeout)
	if err := gitCommitProcessError(result, "unable to locate configured Git hooks"); err != nil {
		return "", nil, err
	}
	originalHooksPath := strings.TrimSpace(result.combinedOutput())
	if originalHooksPath == "" {
		return "", nil, fmt.Errorf("git rev-parse returned an empty hooks path")
	}
	if !filepath.IsAbs(originalHooksPath) {
		originalHooksPath = filepath.Join(workDir, originalHooksPath)
	}
	originalHooksPath, err := filepath.Abs(originalHooksPath)
	if err != nil {
		return "", nil, fmt.Errorf("resolve Git hooks path: %w", err)
	}

	// Store wrappers beside the active worktree index: that directory is
	// visible to Git inside the sandbox and keeps temporary files out of the
	// worktree pathspec (including an explicit "." commit).
	guardBase := filepath.Dir(indexPath)
	guardDir, err := os.MkdirTemp(guardBase, ".corelay-git-commit-hooks-")
	if err != nil {
		return "", nil, fmt.Errorf("create Git hook guard: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(guardDir) }
	for _, hookName := range forwardedGitCommitHooks {
		originalHook := gitCommitOriginalHookPath(originalHooksPath, hookName)
		contents := gitCommitHookWrapper(hookName, originalHook, indexPath, expectedIndexPath, head, hasHead, paths)
		hookPath := filepath.Join(guardDir, hookName)
		if err := os.WriteFile(hookPath, []byte(contents), 0o700); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("write %s Git hook guard: %w", hookName, err)
		}
		if err := os.Chmod(hookPath, 0o700); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("enable %s Git hook guard: %w", hookName, err)
		}
	}
	return guardDir, cleanup, nil
}

func gitCommitHookWrapper(
	hookName string,
	originalHook string,
	indexPath string,
	expectedIndexPath string,
	head string,
	hasHead bool,
	paths []string,
) string {
	var script strings.Builder
	script.WriteString("#!/bin/sh\nset -u\n")
	if hookName == "pre-commit" || hookName == "prepare-commit-msg" || hookName == "commit-msg" {
		script.WriteString("original_hook=")
		script.WriteString(shellSingleQuote(filepath.ToSlash(originalHook)))
		script.WriteString("\nif [ -x \"$original_hook\" ]; then\n  \"$original_hook\" \"$@\"\n  hook_status=$?\n  if [ \"$hook_status\" -ne 0 ]; then exit \"$hook_status\"; fi\nfi\n")
		expectedHead := "no"
		if hasHead {
			expectedHead = "yes"
		}
		script.WriteString("if current_head=$(git rev-parse --verify --quiet HEAD 2>/dev/null); then\n")
		script.WriteString("  current_has_head=yes\nelse\n  current_head=''\n  current_has_head=no\nfi\n")
		script.WriteString("if [ \"$current_has_head\" != ")
		script.WriteString(shellSingleQuote(expectedHead))
		script.WriteString(" ] || [ \"$current_head\" != ")
		script.WriteString(shellSingleQuote(head))
		script.WriteString(" ]; then echo 'Git HEAD changed during selected commit preparation; retry after reviewing the new HEAD' >&2; exit 1; fi\n")
		script.WriteString("candidate_diff=$(git --literal-pathspecs diff --cached --raw --no-abbrev --no-renames")
		if hasHead {
			script.WriteByte(' ')
			script.WriteString(shellSingleQuote(head))
		}
		script.WriteString(") || { echo 'unable to inspect the active partial-commit index' >&2; exit 2; }\n")
		script.WriteString("selected_candidate_diff=$(git --literal-pathspecs diff --cached --raw --no-abbrev --no-renames")
		if hasHead {
			script.WriteByte(' ')
			script.WriteString(shellSingleQuote(head))
		}
		script.WriteString(" --")
		for _, path := range paths {
			script.WriteByte(' ')
			script.WriteString(shellSingleQuote(path))
		}
		script.WriteString(") || { echo 'unable to inspect the selected partial-commit index scope' >&2; exit 2; }\n")
		script.WriteString("if [ \"$candidate_diff\" != \"$selected_candidate_diff\" ]; then echo 'selected_scope_conflict: active commit index contains paths outside the selected scope' >&2; exit 1; fi\n")
		script.WriteString("expected_diff=$(GIT_INDEX_FILE=")
		script.WriteString(shellSingleQuote(filepath.ToSlash(expectedIndexPath)))
		script.WriteString(" git --literal-pathspecs diff --cached --raw --no-abbrev")
		if hasHead {
			script.WriteByte(' ')
			script.WriteString(shellSingleQuote(head))
		}
		script.WriteString(" --")
		for _, path := range paths {
			script.WriteByte(' ')
			script.WriteString(shellSingleQuote(path))
		}
		script.WriteString(") || { echo 'unable to verify the selected Git index scope' >&2; exit 2; }\n")
		script.WriteString("actual_diff=$(GIT_INDEX_FILE=")
		script.WriteString(shellSingleQuote(filepath.ToSlash(indexPath)))
		script.WriteString(" git --literal-pathspecs diff --cached --raw --no-abbrev")
		if hasHead {
			script.WriteByte(' ')
			script.WriteString(shellSingleQuote(head))
		}
		script.WriteString(" --")
		for _, path := range paths {
			script.WriteByte(' ')
			script.WriteString(shellSingleQuote(path))
		}
		script.WriteString(") || { echo 'unable to inspect the live Git index' >&2; exit 2; }\n")
		script.WriteString("if [ \"$expected_diff\" != \"$actual_diff\" ]; then echo 'staged_conflict: selected paths changed in the Git index during commit preparation' >&2; exit 1; fi\n")
		script.WriteString("exit 0\n")
		return script.String()
	}
	script.WriteString("original_hook=")
	script.WriteString(shellSingleQuote(filepath.ToSlash(originalHook)))
	script.WriteString("\nif [ -x \"$original_hook\" ]; then exec \"$original_hook\" \"$@\"; fi\nexit 0\n")
	return script.String()
}

func gitCommitOriginalHookPath(hooksPath, hookName string) string {
	hookPath := filepath.Join(hooksPath, hookName)
	if runtime.GOOS != "windows" {
		return hookPath
	}
	if hookHasShebang(hookPath) {
		return hookPath
	}
	exePath := hookPath + ".exe"
	if info, err := os.Stat(exePath); err == nil && !info.IsDir() {
		return exePath
	}
	return hookPath
}

func hookHasShebang(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	var prefix [2]byte
	n, err := file.Read(prefix[:])
	return err == nil && n == len(prefix) && string(prefix[:]) == "#!"
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
