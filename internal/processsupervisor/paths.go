package processsupervisor

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

func canonicalWorkspace(path string) (string, error) {
	canonical, err := canonicalDirectory(path)
	if err != nil {
		return "", failAdapter(sandbox.FailurePolicyInvalid, "canonicalize sandbox workspace")
	}
	return canonical, nil
}

func canonicalWorkingDir(workspace, requested string) (string, error) {
	candidate := requested
	if strings.TrimSpace(candidate) == "" {
		candidate = workspace
	} else if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(workspace, candidate)
	}
	absolute, err := filepath.Abs(candidate)
	if err != nil {
		return "", failAdapter(sandbox.FailureCommandInvalid, "resolve process working directory")
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", failAdapter(sandbox.FailureCommandInvalid, "canonicalize process working directory")
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return "", failAdapter(sandbox.FailureCommandInvalid, "process working directory is not a directory")
	}
	relative, err := filepath.Rel(workspace, canonical)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", failAdapter(sandbox.FailureCommandInvalid, "process working directory escapes sandbox workspace")
	}
	return filepath.Clean(canonical), nil
}

func canonicalTemporaryDir(path string) (string, error) {
	canonical, err := canonicalDirectory(path)
	if err != nil {
		return "", failAdapter(sandbox.FailureRunnerUnavailable, "canonicalize isolated temporary directory")
	}
	return canonical, nil
}

func cloneEnvironmentWithTemp(environment sandbox.EnvironmentSpec, temporary string) sandbox.EnvironmentSpec {
	cloned := sandbox.EnvironmentSpec{Inherit: append([]string(nil), environment.Inherit...), Set: make(map[string]string, len(environment.Set)+3)}
	for name, value := range environment.Set {
		cloned.Set[name] = value
	}
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		cloned.Set[name] = temporary
	}
	return cloned
}

func removeTemporaryDir(path string) func() {
	return func() {
		if strings.TrimSpace(path) != "" {
			_ = os.RemoveAll(path)
		}
	}
}
