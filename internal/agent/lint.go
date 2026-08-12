package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

type LintFailureCode string

const (
	LintFailureNone             LintFailureCode = ""
	LintFailureSyntax           LintFailureCode = "syntax_error"
	LintFailureSandbox          LintFailureCode = "sandbox_failure"
	LintFailureRevisionMismatch LintFailureCode = "artifact_revision_mismatch"
	LintFailureArtifact         LintFailureCode = "artifact_unavailable"
)

// LintResult keeps syntax failure distinct from sandbox and artifact failures.
// Checked=false means the extension has no configured syntax checker.
type LintResult struct {
	Checked          bool
	Valid            bool
	Failure          LintFailureCode
	Message          string
	ArtifactRevision string
	SandboxReport    sandbox.Report
}

func (r LintResult) failedInfrastructure() bool {
	return r.Failure == LintFailureSandbox ||
		r.Failure == LintFailureRevisionMismatch ||
		r.Failure == LintFailureArtifact
}

// lintFile is the compatibility entry point used by focused syntax tests. It
// composes the platform secure default explicitly; it never falls back to a
// host exec path when the adapter or executable is unavailable.
func lintFile(path string) string {
	revision, err := readLedgerFileRevision(path)
	if err != nil {
		return err.Error()
	}
	runner, policy := DefaultSandboxExecution(filepath.Dir(path))
	result := lintFileWithOptions(path, revision, ToolExecutionOptions{
		SandboxRunner: runner,
		SandboxPolicy: policy,
	})
	if result.Valid || !result.Checked {
		return ""
	}
	return result.Message
}

// lintFileWithOptions verifies that the exact expected artifact revision is
// checked. The revision is compared both before and after a child process so a
// concurrent writer cannot make a lint result apply to different bytes.
func lintFileWithOptions(path, expectedRevision string, opts ToolExecutionOptions) LintResult {
	ext := strings.ToLower(filepath.Ext(path))
	if ext != ".json" && ext != ".go" && ext != ".py" &&
		ext != ".js" && ext != ".mjs" && ext != ".cjs" && ext != ".jsx" {
		return LintResult{Valid: true}
	}

	before, err := readLedgerFileRevision(path)
	if err != nil {
		return LintResult{
			Checked: true,
			Failure: LintFailureArtifact,
			Message: "inspect lint artifact: " + err.Error(),
		}
	}
	if before != expectedRevision {
		return lintRevisionMismatch(expectedRevision, before)
	}

	if ext == ".json" {
		data, err := os.ReadFile(path)
		if err != nil {
			return LintResult{Checked: true, Failure: LintFailureArtifact, Message: "read lint artifact: " + err.Error()}
		}
		result := LintResult{
			Checked:          true,
			Valid:            json.Valid(data),
			ArtifactRevision: artifactBytesRevision(data),
		}
		if !result.Valid {
			result.Failure = LintFailureSyntax
			result.Message = "invalid JSON syntax"
		}
		return verifyLintArtifactUnchanged(path, expectedRevision, result)
	}

	command, arguments := lintCommand(ext, path)
	process := runToolProcess(
		opts,
		"syntax lint",
		filepath.Dir(path),
		command,
		arguments,
		defaultToolProcessTimeout,
	)
	result := LintResult{
		Checked:          true,
		ArtifactRevision: before,
		SandboxReport:    process.Report,
	}
	result = verifyLintArtifactUnchanged(path, expectedRevision, result)
	if result.Failure == LintFailureRevisionMismatch || result.Failure == LintFailureArtifact {
		return result
	}
	if process.policyOrContextFailure() || !process.Started {
		result.Failure = LintFailureSandbox
		result.Message = process.setupOrExecutionError("syntax checker could not run")
		return result
	}
	if process.ExitCode != 0 || process.Err != nil {
		result.Failure = LintFailureSyntax
		result.Message = process.setupOrExecutionError(
			fmt.Sprintf("syntax checker exited with code %d", process.ExitCode),
		)
		return result
	}
	result.Valid = true
	result.Failure = LintFailureNone
	return result
}

func lintCommand(extension, path string) (string, []string) {
	switch extension {
	case ".go":
		return "gofmt", []string{"-e", path}
	case ".py":
		// ast.parse performs a syntax-only check without creating __pycache__.
		const parsePython = "import ast, pathlib, sys; ast.parse(pathlib.Path(sys.argv[1]).read_text(encoding='utf-8'), filename=sys.argv[1])"
		return "python3", []string{"-c", parsePython, path}
	default:
		return "node", []string{"--check", path}
	}
}

func verifyLintArtifactUnchanged(path, expected string, result LintResult) LintResult {
	after, err := readLedgerFileRevision(path)
	if err != nil {
		result.Valid = false
		result.Failure = LintFailureArtifact
		result.Message = "re-read lint artifact: " + err.Error()
		return result
	}
	result.ArtifactRevision = after
	if after != expected {
		mismatch := lintRevisionMismatch(expected, after)
		mismatch.SandboxReport = result.SandboxReport
		return mismatch
	}
	return result
}

func lintRevisionMismatch(expected, actual string) LintResult {
	return LintResult{
		Checked:          true,
		Failure:          LintFailureRevisionMismatch,
		Message:          fmt.Sprintf("lint artifact changed concurrently (expected %s, found %s)", expected, actual),
		ArtifactRevision: actual,
	}
}

func artifactBytesRevision(data []byte) string {
	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:])
}

// rollbackArtifactIfRevision restores only bytes still owned by this
// mutation. A mismatched revision is never overwritten or removed.
func rollbackArtifactIfRevision(
	path string,
	expectedRevision string,
	existed bool,
	backup []byte,
) (bool, error) {
	current, err := readLedgerFileRevision(path)
	if err != nil {
		return false, fmt.Errorf("inspect rollback artifact: %w", err)
	}
	if current != expectedRevision {
		return false, fmt.Errorf(
			"rollback skipped because artifact changed concurrently (expected %s, found %s)",
			expectedRevision,
			current,
		)
	}
	if existed {
		if err := os.WriteFile(path, backup, 0o644); err != nil {
			return false, fmt.Errorf("restore rollback artifact: %w", err)
		}
		return true, nil
	}
	if err := os.Remove(path); err != nil {
		return false, fmt.Errorf("remove rollback artifact: %w", err)
	}
	return true, nil
}
