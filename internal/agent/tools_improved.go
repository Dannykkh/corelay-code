package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

func executeBashV2WithOptions(
	input json.RawMessage,
	workDir string,
	ctx context.Context,
	runner sandbox.Runner,
	policy sandbox.Policy,
	observeReport func(sandbox.Report),
) (string, bool) {
	result := ExecuteBashDeepWithOptions(input, workDir, BashExecOptions{
		Context:       ctx,
		Runner:        runner,
		Policy:        policy,
		ObserveReport: observeReport,
	})
	return result.Output, result.IsError
}

// ── Improved Read: auto-detect binary, image info, better formatting ──

func executeReadV2(input json.RawMessage, workDir string) (string, bool) {
	var args struct {
		FilePath string `json:"file_path"`
		Offset   int    `json:"offset"`
		Limit    int    `json:"limit"`
	}
	json.Unmarshal(input, &args)

	path := resolvePath(args.FilePath, workDir)

	// File info
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}

	// Check if binary
	if isBinaryFile(path) {
		return fmt.Sprintf("Binary file: %s (%s)", args.FilePath, formatSize(info.Size())), false
	}

	// Check if image
	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".png" || ext == ".jpg" || ext == ".jpeg" || ext == ".gif" || ext == ".svg" || ext == ".webp" {
		return fmt.Sprintf("Image file: %s (%s, %s)", args.FilePath, ext, formatSize(info.Size())), false
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("Error reading %s: %v", args.FilePath, err), true
	}

	lines := strings.Split(string(data), "\n")
	totalLines := len(lines)

	// Default: read up to 2000 lines
	start := args.Offset
	if start < 0 {
		start = 0
	}
	if start > totalLines {
		start = totalLines
	}

	limit := args.Limit
	if limit <= 0 {
		limit = 2000
	}

	end := start + limit
	if end > totalLines {
		end = totalLines
	}

	var result strings.Builder
	maxLineWidth := len(fmt.Sprintf("%d", end))

	for i := start; i < end; i++ {
		fmt.Fprintf(&result, "%*d\t%s\n", maxLineWidth, i+1, lines[i])
	}

	header := fmt.Sprintf("File: %s (%d lines, %s)", args.FilePath, totalLines, formatSize(info.Size()))
	if start > 0 || end < totalLines {
		header += fmt.Sprintf(" [showing lines %d-%d]", start+1, end)
	}
	return header + "\n" + result.String(), false
}

func isBinaryFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	for _, b := range buf[:n] {
		if b == 0 {
			return true
		}
	}
	return false
}

// ── Improved Glob: recursive ** support using filepath.Walk ──

func executeGlobV2(input json.RawMessage, workDir string) (string, bool) {
	var args struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	json.Unmarshal(input, &args)

	dir := workDir
	if args.Path != "" {
		dir = resolvePath(args.Path, workDir)
	}

	var matches []string
	pattern := args.Pattern

	// Handle ** recursive patterns
	if strings.Contains(pattern, "**") {
		// Split: "src/**/*.go" → prefix="src", suffix="*.go"
		parts := strings.SplitN(pattern, "**", 2)
		prefix := strings.TrimSuffix(parts[0], "/")
		suffix := strings.TrimPrefix(parts[1], "/")

		searchDir := dir
		if prefix != "" {
			searchDir = filepath.Join(dir, prefix)
		}

		filepath.Walk(searchDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if suffix != "" {
				matched, _ := filepath.Match(suffix, filepath.Base(path))
				if !matched {
					return nil
				}
			}
			rel, _ := filepath.Rel(dir, path)
			matches = append(matches, rel)
			if len(matches) >= 200 {
				return filepath.SkipAll
			}
			return nil
		})
	} else {
		// Simple glob
		globPath := filepath.Join(dir, pattern)
		results, _ := filepath.Glob(globPath)
		for _, r := range results {
			rel, _ := filepath.Rel(dir, r)
			matches = append(matches, rel)
		}
	}

	sort.Strings(matches)

	if len(matches) == 0 {
		return "No files found matching: " + pattern, false
	}

	result := fmt.Sprintf("Found %d files matching '%s':\n", len(matches), pattern)
	for _, m := range matches {
		result += "  " + m + "\n"
	}
	return result, false
}

// ── Improved Grep: context lines, file type filter, count mode ──

func executeGrepV2(input json.RawMessage, workDir string) (string, bool) {
	return executeGrepV2WithOptions(input, workDir, ToolExecutionOptions{})
}

func executeGrepV2WithOptions(input json.RawMessage, workDir string, opts ToolExecutionOptions) (string, bool) {
	var args struct {
		Pattern    string `json:"pattern"`
		Path       string `json:"path"`
		Glob       string `json:"glob"`
		Context    int    `json:"context"`     // lines of context (-C)
		IgnoreCase bool   `json:"ignore_case"` // -i
		FilesOnly  bool   `json:"files_only"`  // only show file names
		MaxResults int    `json:"max_results"`
	}
	json.Unmarshal(input, &args)

	dir := workDir
	if args.Path != "" {
		dir = resolvePath(args.Path, workDir)
	}

	// Try ripgrep first, and only fall back when the executable cannot start.
	// A real rg exit 1 is the documented "no matches" outcome.
	cmdArgs := []string{"--no-heading", "--line-number", "--color=never"}

	if args.IgnoreCase {
		cmdArgs = append(cmdArgs, "-i")
	}
	if args.Context > 0 {
		cmdArgs = append(cmdArgs, fmt.Sprintf("-C%d", args.Context))
	}
	if args.FilesOnly {
		cmdArgs = append(cmdArgs, "-l")
	}
	if args.Glob != "" {
		cmdArgs = append(cmdArgs, "--glob", args.Glob)
	}
	cmdArgs = append(cmdArgs, args.Pattern, dir)

	process := runToolProcess(opts, "Grep", workDir, "rg", cmdArgs, defaultToolProcessTimeout)
	if process.policyOrContextFailure() {
		return "Grep failed: " + process.setupOrExecutionError("sandbox execution failed"), true
	}
	if !process.Started {
		// Fallback to grep
		grepArgs := []string{"-rn", "--color=never"}
		if args.IgnoreCase {
			grepArgs = append(grepArgs, "-i")
		}
		if args.Context > 0 {
			grepArgs = append(grepArgs, fmt.Sprintf("-C%d", args.Context))
		}
		if args.FilesOnly {
			grepArgs = append(grepArgs, "-l")
		}
		if args.Glob != "" {
			grepArgs = append(grepArgs, "--include="+args.Glob)
		}
		grepArgs = append(grepArgs, args.Pattern, dir)

		process = runToolProcess(opts, "Grep fallback", workDir, "grep", grepArgs, defaultToolProcessTimeout)
		if process.policyOrContextFailure() || !process.Started {
			return "Grep failed: " + process.setupOrExecutionError("neither rg nor grep could start"), true
		}
	}
	if process.ExitCode > 1 || (process.Err != nil && process.ExitCode == 0) {
		return "Grep failed: " + process.setupOrExecutionError("search command failed"), true
	}

	result := process.combinedOutput()

	// Relativize paths
	result = strings.ReplaceAll(result, dir+"/", "")
	result = strings.ReplaceAll(result, dir+"\\", "")

	// Count matches
	lines := strings.Split(result, "\n")
	matchCount := len(lines)
	if result == "" {
		matchCount = 0
	}

	maxResults := args.MaxResults
	if maxResults <= 0 {
		maxResults = 250
	}
	if matchCount > maxResults {
		lines = lines[:maxResults]
		result = strings.Join(lines, "\n") + fmt.Sprintf("\n... (%d more results)", matchCount-maxResults)
	}

	if matchCount == 0 {
		return "No matches found for: " + args.Pattern, false
	}

	// Make paths relative
	return fmt.Sprintf("Found %d matches for '%s':\n\n%s", matchCount, args.Pattern, result), false
}

// ── Improved Edit: multi-replace, regex replace ──

func executeEditV2(input json.RawMessage, workDir string) (string, bool) {
	runner, policy := DefaultSandboxExecution(workDir)
	return executeEditV2WithOptions(input, workDir, ToolExecutionOptions{
		SandboxRunner: runner,
		SandboxPolicy: policy,
	})
}

func executeEditV2WithOptions(input json.RawMessage, workDir string, opts ToolExecutionOptions) (string, bool) {
	var args editPolicyInput
	if err := json.Unmarshal(input, &args); err != nil {
		return fmt.Sprintf("Invalid Edit input: %v", err), true
	}

	path, err := mutationExecutionPath(args.FilePath, workDir, "Edit", opts.fileMutation)
	if err != nil {
		return fileMutationBlocked("edit", err)
	}
	unlock := lockArtifactMutation(path)
	defer unlock()

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("Error reading %s: %v", args.FilePath, err), true
	}
	if err := validateArtifactBytesPrecondition(data, opts.fileMutation); err != nil {
		return fileMutationBlocked("edit", err)
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		if err == nil {
			err = fmt.Errorf("target is not a regular file")
		}
		return fmt.Sprintf("Error inspecting %s: %v", args.FilePath, err), true
	}

	content := string(data)
	newContent, count, err := applyEditPolicy(content, args, opts.EditPolicy)
	if err != nil {
		hint := ""
		if args.OldString != "" {
			hint = closestLinesHint(content, args.OldString)
		}
		return fmt.Sprintf(
			"Error: edit policy %s could not apply to %s: %v.%s\nRe-read the file and retry with the required edit format.",
			normalizeEditPolicy(opts.EditPolicy),
			args.FilePath,
			err,
			hint,
		), true
	}

	// ── Lint gate ──
	// If the file was syntactically valid before the edit, verify the edit
	// didn't break it. A new syntax error rolls back the write and reports
	// the error, which flows back to the model via the tool_result and the
	// reflection loop in RunLoop, prompting a self-correcting retry.
	baselineRevision := artifactBytesRevision(data)
	baselineLint := lintFileWithOptions(path, baselineRevision, opts)
	if baselineLint.failedInfrastructure() {
		return fmt.Sprintf(
			"Edit was not started because the original file could not be linted safely:\n%s",
			strings.TrimSpace(baselineLint.Message),
		), true
	}
	preLintOK := baselineLint.Valid || !baselineLint.Checked

	stage, err := stageArtifact(path, []byte(newContent), info.Mode())
	if err != nil {
		return fmt.Sprintf("Error staging %s: %v", args.FilePath, err), true
	}
	defer stage.cleanup()
	if preLintOK {
		lintResult := lintFileWithOptions(stage.temporary, stage.revision, opts)
		if !lintResult.Valid {
			if stateErr := validateExpectedArtifactState(path, baselineRevision, false); stateErr != nil {
				current, revisionErr := readLedgerFileRevision(path)
				return concurrentMutationFailure("edit", baselineRevision, current, revisionErr), true
			}
			return stagedLintFailure("edit", lintResult)
		}
	}
	if err := validateArtifactPrecondition(path, opts.fileMutation); err != nil {
		return fileMutationBlocked("edit", err)
	}
	if err := stage.commit(baselineRevision, false); err != nil {
		return fileMutationBlocked("edit", err)
	}
	if current, revisionErr := readLedgerFileRevision(path); revisionErr != nil || current != stage.revision {
		return concurrentMutationFailure("edit", stage.revision, current, revisionErr), true
	}

	if count == 1 {
		return fmt.Sprintf("Edited %s (1 replacement)", args.FilePath), false
	}
	return fmt.Sprintf("Edited %s (%d replacements)", args.FilePath, count), false
}

// ── Improved Write: diff preview, backup ──

func executeWriteV2(input json.RawMessage, workDir string) (string, bool) {
	runner, policy := DefaultSandboxExecution(workDir)
	return executeWriteV2WithOptions(input, workDir, ToolExecutionOptions{
		SandboxRunner: runner,
		SandboxPolicy: policy,
	})
}

func executeWriteV2WithOptions(input json.RawMessage, workDir string, opts ToolExecutionOptions) (string, bool) {
	var args struct {
		FilePath string `json:"file_path"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return fmt.Sprintf("Invalid Write input: %v", err), true
	}

	path, err := mutationExecutionPath(args.FilePath, workDir, "Write", opts.fileMutation)
	if err != nil {
		return fileMutationBlocked("write", err)
	}
	unlock := lockArtifactMutation(path)
	defer unlock()

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Sprintf("Error creating directory for %s: %v", args.FilePath, err), true
	}
	if opts.fileMutation != nil {
		canonical, pathErr := mutationExecutionPath(args.FilePath, workDir, "Write", opts.fileMutation)
		if pathErr != nil || !sameArtifactMutationPath(canonical, path) {
			if pathErr == nil {
				pathErr = errors.New("mutation directory changed after creation")
			}
			return fileMutationBlocked("write", pathErr)
		}
	}

	// Check if file exists; keep a backup for the lint-gate rollback.
	existed := false
	var backup []byte
	if d, readErr := os.ReadFile(path); readErr == nil {
		existed = true
		backup = d
	} else if !os.IsNotExist(readErr) {
		return fmt.Sprintf("Error reading %s: %v", args.FilePath, readErr), true
	}
	if existed {
		if err := validateArtifactBytesPrecondition(backup, opts.fileMutation); err != nil {
			return fileMutationBlocked("write", err)
		}
	} else if err := validateArtifactPrecondition(path, opts.fileMutation); err != nil {
		return fileMutationBlocked("write", err)
	}
	// Gate only when we have a clean baseline: a brand-new file, or an
	// existing file that was already syntactically valid.
	preLintOK := true
	if existed {
		baselineLint := lintFileWithOptions(path, artifactBytesRevision(backup), opts)
		if baselineLint.failedInfrastructure() {
			return fmt.Sprintf(
				"Write was not started because the original file could not be linted safely:\n%s",
				strings.TrimSpace(baselineLint.Message),
			), true
		}
		preLintOK = baselineLint.Valid || !baselineLint.Checked
	}

	mode := os.FileMode(0o644)
	if existed {
		info, statErr := os.Stat(path)
		if statErr != nil || !info.Mode().IsRegular() {
			if statErr == nil {
				statErr = fmt.Errorf("target is not a regular file")
			}
			return fmt.Sprintf("Error inspecting %s: %v", args.FilePath, statErr), true
		}
		mode = info.Mode()
	}
	baselineRevision := ""
	if existed {
		baselineRevision = artifactBytesRevision(backup)
	}
	stage, err := stageArtifact(path, []byte(args.Content), mode)
	if err != nil {
		return fmt.Sprintf("Error staging %s: %v", args.FilePath, err), true
	}
	defer stage.cleanup()
	if preLintOK {
		lintResult := lintFileWithOptions(stage.temporary, stage.revision, opts)
		if !lintResult.Valid {
			if stateErr := validateExpectedArtifactState(path, baselineRevision, !existed); stateErr != nil {
				current, revisionErr := readLedgerFileRevision(path)
				return concurrentMutationFailure("write", baselineRevision, current, revisionErr), true
			}
			return stagedLintFailure("write", lintResult)
		}
	}
	if err := validateArtifactPrecondition(path, opts.fileMutation); err != nil {
		return fileMutationBlocked("write", err)
	}
	if err := stage.commit(baselineRevision, !existed); err != nil {
		return fileMutationBlocked("write", err)
	}
	if current, revisionErr := readLedgerFileRevision(path); revisionErr != nil || current != stage.revision {
		return concurrentMutationFailure("write", stage.revision, current, revisionErr), true
	}

	lines := len(strings.Split(args.Content, "\n"))
	action := "Created"
	if existed {
		action = "Updated"
	}
	return fmt.Sprintf("%s %s (%d lines, %s)", action, args.FilePath, lines, formatSize(int64(len(args.Content)))), false
}

func concurrentMutationFailure(operation, expected, actual string, revisionErr error) string {
	detail := "artifact revision became unavailable"
	if revisionErr != nil {
		detail += ": " + revisionErr.Error()
	} else {
		detail = fmt.Sprintf("expected %s, found %s", expected, actual)
	}
	return fmt.Sprintf(
		"The %s could not be validated because the file changed concurrently (%s). Rollback was skipped so another writer's content was not overwritten.",
		operation,
		detail,
	)
}

func rollbackLintFailure(
	operation string,
	path string,
	writtenRevision string,
	existed bool,
	backup []byte,
	lintResult LintResult,
) (string, bool) {
	if lintResult.Failure == LintFailureRevisionMismatch || lintResult.Failure == LintFailureArtifact {
		return concurrentMutationFailure(operation, writtenRevision, lintResult.ArtifactRevision, nil), true
	}
	rolledBack, rollbackErr := rollbackArtifactIfRevision(path, writtenRevision, existed, backup)
	if rollbackErr != nil || !rolledBack {
		detail := "rollback did not complete"
		if rollbackErr != nil {
			detail = rollbackErr.Error()
		}
		return fmt.Sprintf(
			"The %s failed lint validation: %s\n%s. Another writer's content was not overwritten.",
			operation,
			strings.TrimSpace(lintResult.Message),
			detail,
		), true
	}

	if lintResult.Failure == LintFailureSyntax {
		return fmt.Sprintf(
			"Your %s introduced a syntax error and was NOT applied:\n%s\n\n"+
				"The file is unchanged. Fix the content and try again. "+
				"Do NOT repeat the same failed %s.",
			operation,
			strings.TrimSpace(lintResult.Message),
			operation,
		), true
	}
	return fmt.Sprintf(
		"The %s was rolled back because syntax validation could not complete safely:\n%s\n\nThe file is unchanged.",
		operation,
		strings.TrimSpace(lintResult.Message),
	), true
}
