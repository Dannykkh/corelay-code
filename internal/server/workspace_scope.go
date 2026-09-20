package server

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Dannykkh/corelay-code/internal/config"
)

var errWorkspaceNotRegistered = errors.New("workspace is not a configured project")

// requestProjectWorkspace scopes UI file requests to the server's configured
// workspace or one of its registered projects. The project picker sends this
// path explicitly so changing projects does not mutate process-wide defaults.
func (s *Server) requestProjectWorkspace(r *http.Request, explicit string) (string, error) {
	requested := strings.TrimSpace(explicit)
	if requested == "" && r != nil {
		requested = strings.TrimSpace(r.URL.Query().Get("workDir"))
	}
	if requested == "" {
		return s.requestWorkDir(r, ""), nil
	}

	requestedCanonical, err := canonicalExistingDirectory(requested)
	if err != nil {
		return "", fmt.Errorf("invalid workspace")
	}

	allowed := make([]string, 0)
	cfg := config.Load()
	allowed = append(allowed, cfg.WorkDir)
	s.mu.RLock()
	serverWorkDir := s.workDir
	s.mu.RUnlock()
	if serverWorkDir == "" {
		serverWorkDir, _ = os.Getwd()
	}
	allowed = append(allowed, serverWorkDir)
	for _, project := range cfg.Projects {
		allowed = append(allowed, project.Path)
	}
	for _, candidate := range allowed {
		candidateCanonical, err := canonicalExistingDirectory(candidate)
		if err == nil && sameCanonicalPath(requestedCanonical, candidateCanonical) {
			return requestedCanonical, nil
		}
	}
	return "", errWorkspaceNotRegistered
}

func writeProjectWorkspaceScopeError(w http.ResponseWriter, err error) {
	if errors.Is(err, errWorkspaceNotRegistered) {
		writeSessionAPIError(w, http.StatusForbidden, "workspace_not_registered", "workspace must match the server default or a registered project", nil, nil)
		return
	}
	writeSessionAPIError(w, http.StatusBadRequest, "invalid_workspace", "workspace path is invalid", nil, nil)
}

func canonicalExistingDirectory(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", os.ErrNotExist
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("not a directory")
	}
	return filepath.Clean(resolved), nil
}

func sameCanonicalPath(left, right string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
	}
	return filepath.Clean(left) == filepath.Clean(right)
}

func workspacePathContains(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	if err != nil || filepath.IsAbs(rel) || rel == ".." {
		return false
	}
	return !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// resolveWorkspaceFile maps a UI-relative path beneath a canonical project
// root. Symlinks are resolved before checking containment so a link inside a
// project cannot expose or modify a file outside it.
func resolveWorkspaceFile(root, relative string, allowMissing bool) (string, error) {
	if strings.TrimSpace(relative) == "" {
		return "", fmt.Errorf("path required")
	}
	relative = filepath.FromSlash(relative)
	if filepath.IsAbs(relative) || filepath.VolumeName(relative) != "" {
		return "", fmt.Errorf("path outside workspace")
	}

	canonicalRoot, err := canonicalExistingDirectory(root)
	if err != nil {
		return "", err
	}
	candidate := filepath.Clean(filepath.Join(canonicalRoot, relative))
	if !workspacePathContains(canonicalRoot, candidate) || candidate == canonicalRoot {
		return "", fmt.Errorf("path outside workspace")
	}

	resolved, err := filepath.EvalSymlinks(candidate)
	if err == nil {
		resolved, err = filepath.Abs(resolved)
		if err != nil || !workspacePathContains(canonicalRoot, filepath.Clean(resolved)) {
			return "", fmt.Errorf("path outside workspace")
		}
		return filepath.Clean(resolved), nil
	}
	if !allowMissing || !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	// For a new file, resolve its nearest existing ancestor. This catches both
	// lexical traversal and missing descendants below a symlink that points out.
	ancestor := filepath.Dir(candidate)
	missing := []string{filepath.Base(candidate)}
	for {
		resolvedAncestor, resolveErr := filepath.EvalSymlinks(ancestor)
		if resolveErr == nil {
			resolvedAncestor, resolveErr = filepath.Abs(resolvedAncestor)
			if resolveErr != nil || !workspacePathContains(canonicalRoot, filepath.Clean(resolvedAncestor)) {
				return "", fmt.Errorf("path outside workspace")
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolvedAncestor = filepath.Join(resolvedAncestor, missing[i])
			}
			if !workspacePathContains(canonicalRoot, filepath.Clean(resolvedAncestor)) {
				return "", fmt.Errorf("path outside workspace")
			}
			return filepath.Clean(resolvedAncestor), nil
		}
		if !errors.Is(resolveErr, os.ErrNotExist) {
			return "", resolveErr
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", resolveErr
		}
		missing = append(missing, filepath.Base(ancestor))
		ancestor = parent
	}
}
