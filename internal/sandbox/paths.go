package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func canonicalWorkspace(path string) (string, error) {
	if path == "" {
		return "", &RunnerError{Code: FailurePolicyInvalid, Detail: "sandbox workspace is empty"}
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", &RunnerError{Code: FailurePolicyInvalid, Detail: fmt.Sprintf("resolve sandbox workspace: %v", err)}
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", &RunnerError{Code: FailurePolicyInvalid, Detail: fmt.Sprintf("canonicalize sandbox workspace: %v", err)}
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", &RunnerError{Code: FailurePolicyInvalid, Detail: fmt.Sprintf("inspect sandbox workspace: %v", err)}
	}
	if !info.IsDir() {
		return "", &RunnerError{Code: FailurePolicyInvalid, Detail: "sandbox workspace is not a directory"}
	}
	return filepath.Clean(canonical), nil
}

func canonicalWorkingDir(workspace, commandDir string) (string, error) {
	if commandDir == "" {
		return workspace, nil
	}
	resolved := commandDir
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(workspace, resolved)
	}
	absolute, err := filepath.Abs(resolved)
	if err != nil {
		return "", &RunnerError{Code: FailureCommandInvalid, Detail: fmt.Sprintf("resolve command directory: %v", err)}
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", &RunnerError{Code: FailureCommandInvalid, Detail: fmt.Sprintf("canonicalize command directory: %v", err)}
	}
	relative, err := filepath.Rel(workspace, canonical)
	if err != nil || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", &RunnerError{Code: FailureCommandInvalid, Detail: "command directory is outside the canonical workspace"}
	}
	return filepath.Clean(canonical), nil
}

func canonicalTemporaryDir(path string) (string, error) {
	base, err := filepath.Abs(os.TempDir())
	if err != nil {
		return "", &RunnerError{Code: FailureRunnerUnavailable, Detail: fmt.Sprintf("resolve system temp directory: %v", err)}
	}
	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		return "", &RunnerError{Code: FailureRunnerUnavailable, Detail: fmt.Sprintf("canonicalize system temp directory: %v", err)}
	}
	candidate, err := filepath.Abs(path)
	if err != nil {
		return "", &RunnerError{Code: FailureRunnerUnavailable, Detail: fmt.Sprintf("resolve isolated temp directory: %v", err)}
	}
	candidate, err = filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", &RunnerError{Code: FailureRunnerUnavailable, Detail: fmt.Sprintf("canonicalize isolated temp directory: %v", err)}
	}
	relative, err := filepath.Rel(base, candidate)
	if err != nil || relative == "." || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", &RunnerError{Code: FailureRunnerUnavailable, Detail: "isolated temp directory is outside the system temp root"}
	}
	info, err := os.Stat(candidate)
	if err != nil || !info.IsDir() {
		return "", &RunnerError{Code: FailureRunnerUnavailable, Detail: "isolated temp path is not a directory"}
	}
	return filepath.Clean(candidate), nil
}

func cloneEnvironmentWithTemp(spec EnvironmentSpec, tempDir string) EnvironmentSpec {
	cloned := EnvironmentSpec{
		Inherit: append([]string(nil), spec.Inherit...),
		Set:     make(map[string]string, len(spec.Set)+3),
	}
	for name, value := range spec.Set {
		cloned.Set[name] = value
	}
	cloned.Set["TMPDIR"] = tempDir
	cloned.Set["TMP"] = tempDir
	cloned.Set["TEMP"] = tempDir
	return cloned
}
