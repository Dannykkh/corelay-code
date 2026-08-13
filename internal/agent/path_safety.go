package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// canonicalPermissionToolName keeps permission checks aligned with the
// explicit aliases accepted by tool-call recovery. Aliases remain exact: an
// unknown spelling is not promoted into a known tool at the safety boundary.
func canonicalPermissionToolName(name string) string {
	switch name {
	case "read_file":
		return "Read"
	case "write_file":
		return "Write"
	case "edit_file":
		return "Edit"
	case "list_files":
		return "LS"
	case "search_files":
		return "Grep"
	case "glob_files":
		return "Glob"
	default:
		return name
	}
}

// checkToolWorkspacePaths validates every filesystem-bearing field before a
// tool call is approved. Unknown tools do not acquire filesystem semantics
// here; the immutable tool catalog rejects those separately.
func checkToolWorkspacePaths(toolName string, input json.RawMessage, workDir string, cfg PermissionConfig) (bool, string) {
	_, handled, err := resolveToolWorkspacePaths(toolName, input, workDir, cfg)
	if !handled {
		return true, ""
	}
	if err != nil {
		return false, err.Error()
	}
	return true, ""
}

// resolvedToolWorkspacePaths is the single path authorization product shared
// by permission checks and direct built-in executors. Executors consume the
// canonical values instead of resolving the model-provided strings a second
// time after authorization.
type resolvedToolWorkspacePaths struct {
	values map[string][]string
}

func (r resolvedToolWorkspacePaths) one(name string) string {
	values := r.values[name]
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func (r resolvedToolWorkspacePaths) many(name string) []string {
	return append([]string(nil), r.values[name]...)
}

// executionToolWorkspacePaths is defense in depth for forced/direct executor
// calls that did not traverse the permission broker. Configured blocked-path
// policy remains the broker's responsibility; the immutable workspace and
// unambiguous-JSON boundary is non-optional here.
func executionToolWorkspacePaths(toolName string, input json.RawMessage, workDir string) (resolvedToolWorkspacePaths, error) {
	resolved, handled, err := resolveToolWorkspacePaths(toolName, input, workDir, PermissionConfig{})
	if !handled {
		return resolvedToolWorkspacePaths{}, fmt.Errorf("tool %q has no filesystem path contract", toolName)
	}
	return resolved, err
}

// resolveToolWorkspacePaths validates every filesystem-bearing field in the
// immutable built-in catalog. It rejects ambiguous JSON before extracting any
// field, canonicalizes every operand (including every item of a multi-path
// call), and returns only canonical paths for execution.
func resolveToolWorkspacePaths(
	toolName string,
	input json.RawMessage,
	workDir string,
	cfg PermissionConfig,
) (resolvedToolWorkspacePaths, bool, error) {
	toolName = canonicalPermissionToolName(toolName)
	switch toolName {
	case "Read", "Write", "Edit", "Glob", "Grep", "LS", "RepoMap",
		"NotebookRead", "NotebookEdit", "ImageRead", "PDFRead",
		"Lint", "Test", "Git", "GitDiff", "GitCommit", "Diff":
	default:
		return resolvedToolWorkspacePaths{}, false, nil
	}

	object, err := decodeUniqueJSONObject(input)
	if err != nil {
		return resolvedToolWorkspacePaths{}, true, fmt.Errorf("Invalid tool path input: %w", err)
	}
	workspace, err := canonicalWorkspace(workDir)
	if err != nil {
		return resolvedToolWorkspacePaths{}, true, fmt.Errorf("Unsafe workspace path: %w", err)
	}
	resolved := resolvedToolWorkspacePaths{values: make(map[string][]string)}
	canonicalize := func(name, raw string) error {
		canonical, err := canonicalPathWithinWorkspace(raw, workDir, workspace, cfg)
		if err != nil {
			return err
		}
		resolved.values[name] = append(resolved.values[name], canonical)
		return nil
	}

	switch toolName {
	case "Read", "Write", "Edit", "NotebookRead", "ImageRead", "PDFRead":
		path, err := oneStringField(object, true, "file_path", "filePath", "path")
		if err != nil {
			return resolved, true, fmt.Errorf("Invalid file path: %w", err)
		}
		if err := canonicalize("file_path", path); err != nil {
			return resolved, true, err
		}

	case "NotebookEdit":
		// NotebookEdit historically wrote bytes directly, bypassing the read
		// ledger, revision proof, staging, lint, rollback, and ownership checks.
		// It is retained as a forced-call tombstone only; callers must use the
		// ordinary Read + Edit mutation pipeline.
		path, err := oneStringField(object, true, "file_path", "filePath", "path")
		if err != nil {
			return resolved, true, fmt.Errorf("Invalid file path: %w", err)
		}
		if err := canonicalize("file_path", path); err != nil {
			return resolved, true, err
		}
		return resolved, true, errors.New("NotebookEdit is disabled; use Read followed by Edit so the mutation pipeline can verify the artifact revision")

	case "LS", "RepoMap", "Lint", "Test":
		path, err := oneStringField(object, false, "path")
		if err != nil {
			return resolved, true, fmt.Errorf("Invalid path: %w", err)
		}
		if path == "" {
			path = "."
		}
		if err := canonicalize("path", path); err != nil {
			return resolved, true, err
		}

	case "Glob":
		basePath, err := oneStringField(object, false, "path")
		if err != nil {
			return resolved, true, fmt.Errorf("Invalid glob base path: %w", err)
		}
		if basePath == "" {
			basePath = "."
		}
		base, err := canonicalPathWithinWorkspace(basePath, workDir, workspace, cfg)
		if err != nil {
			return resolved, true, err
		}
		resolved.values["path"] = []string{base}
		pattern, err := oneStringField(object, true, "pattern")
		if err != nil {
			return resolved, true, fmt.Errorf("Invalid glob pattern: %w", err)
		}
		if err := checkGlobPattern(pattern, base, workspace, cfg); err != nil {
			return resolved, true, err
		}

	case "Grep":
		basePath, err := oneStringField(object, false, "path")
		if err != nil {
			return resolved, true, fmt.Errorf("Invalid grep path: %w", err)
		}
		if basePath == "" {
			basePath = "."
		}
		base, err := canonicalPathWithinWorkspace(basePath, workDir, workspace, cfg)
		if err != nil {
			return resolved, true, err
		}
		resolved.values["path"] = []string{base}
		pattern, err := oneStringField(object, false, "glob")
		if err != nil {
			return resolved, true, fmt.Errorf("Invalid grep glob: %w", err)
		}
		if pattern != "" {
			if err := checkGlobPattern(pattern, base, workspace, cfg); err != nil {
				return resolved, true, err
			}
		}

	case "Diff":
		for _, field := range []struct {
			name string
			keys []string
		}{
			{name: "file_a", keys: []string{"file_a", "fileA"}},
			{name: "file_b", keys: []string{"file_b", "fileB"}},
		} {
			path, err := oneStringField(object, true, field.keys...)
			if err != nil {
				return resolved, true, fmt.Errorf("Invalid %s path: %w", field.name, err)
			}
			if err := canonicalize(field.name, path); err != nil {
				return resolved, true, err
			}
		}

	case "GitDiff":
		path, err := oneStringField(object, false, "file")
		if err != nil {
			return resolved, true, fmt.Errorf("Invalid git diff path: %w", err)
		}
		if path != "" {
			if err := canonicalize("file", path); err != nil {
				return resolved, true, err
			}
		}
		commit, err := oneStringField(object, false, "commit")
		if err != nil {
			return resolved, true, fmt.Errorf("Invalid git revision: %w", err)
		}
		if err := validateGitRevision(commit); err != nil {
			return resolved, true, err
		}

	case "GitCommit":
		files, err := oneStringField(object, false, "files")
		if err != nil {
			return resolved, true, fmt.Errorf("Invalid git commit paths: %w", err)
		}
		for _, path := range strings.Fields(files) {
			if strings.HasPrefix(path, "-") || strings.HasPrefix(path, ":(") || strings.HasPrefix(path, ":!") || strings.HasPrefix(path, ":^") {
				return resolved, true, fmt.Errorf("Invalid git commit path %q: options and pathspec magic are not accepted", path)
			}
			if err := canonicalize("files", path); err != nil {
				return resolved, true, err
			}
		}

	case "Git":
		arguments, err := resolveGitToolArguments(object, workDir, workspace, cfg)
		if err != nil {
			return resolved, true, err
		}
		resolved.values["arguments"] = arguments
	}

	return resolved, true, nil
}

func validateGitRevision(revision string) error {
	if revision == "" {
		return nil
	}
	if strings.HasPrefix(revision, "-") || filepath.IsAbs(revision) || hasParentTraversalComponent(revision) || strings.IndexFunc(revision, func(r rune) bool {
		return r == '\\' || r == 0 || r == ' ' || r == '\t' || r == '\r' || r == '\n'
	}) >= 0 {
		return errors.New("Invalid git revision: revisions must be a single non-option name")
	}
	return nil
}

func resolveGitToolArguments(
	object map[string]json.RawMessage,
	workDir string,
	workspace string,
	cfg PermissionConfig,
) ([]string, error) {
	command, err := oneStringField(object, true, "command")
	if err != nil {
		return nil, fmt.Errorf("Invalid git command: %w", err)
	}
	if strings.HasPrefix(command, "-") || strings.IndexFunc(command, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && r != '-'
	}) >= 0 {
		return nil, errors.New("Invalid git command: expected a subcommand name, not a global option or path")
	}
	args, err := oneStringField(object, false, "args")
	if err != nil {
		return nil, fmt.Errorf("Invalid git arguments: %w", err)
	}
	arguments := strings.Fields(args)
	resolved := make([]string, 0, len(arguments)+1)
	resolved = append(resolved, command)
	afterSeparator := false
	for _, argument := range arguments {
		lower := strings.ToLower(argument)
		if argument == "--" {
			afterSeparator = true
			resolved = append(resolved, argument)
			continue
		}
		if unsafeGitFilesystemOption(lower) {
			return nil, fmt.Errorf("Invalid git arguments: filesystem escape option %q is not accepted", argument)
		}
		pathOperand := afterSeparator || gitCommandUsesPositionalPaths(command)
		externalForm := filepath.IsAbs(argument) || hasParentTraversalComponent(argument)
		if externalForm {
			pathOperand = true
		}
		if externalForm && strings.HasPrefix(argument, "-") {
			return nil, fmt.Errorf("Invalid git arguments: path-bearing option %q is not accepted", argument)
		}
		if pathOperand && (strings.HasPrefix(argument, ":(") || strings.HasPrefix(argument, ":!") || strings.HasPrefix(argument, ":^")) {
			return nil, fmt.Errorf("Invalid git arguments: pathspec magic %q is not accepted", argument)
		}
		if pathOperand && (!strings.HasPrefix(argument, "-") || afterSeparator) {
			canonical, err := canonicalPathWithinWorkspace(argument, workDir, workspace, cfg)
			if err != nil {
				return nil, err
			}
			resolved = append(resolved, canonical)
			continue
		}
		resolved = append(resolved, argument)
	}
	return resolved, nil
}

func unsafeGitFilesystemOption(argument string) bool {
	for _, option := range []string{
		"-c", "--config-env", "--exec-path", "--git-dir", "--work-tree",
		"--namespace", "--super-prefix", "--pathspec-from-file", "--output",
		"--global", "--system", "--file", "--object-directory", "--directory",
		"--prefix", "--unsafe-paths",
	} {
		if argument == option || strings.HasPrefix(argument, option+"=") {
			return true
		}
	}
	return false
}

func gitCommandUsesPositionalPaths(command string) bool {
	switch strings.ToLower(command) {
	case "add", "rm", "mv":
		return true
	default:
		return false
	}
}

// decodeUniqueJSONObject rejects malformed, trailing, non-object, and
// duplicate-key JSON. Accepting the decoder's usual "last key wins" behavior
// would make a path-bearing request ambiguous at the authorization boundary.
func decodeUniqueJSONObject(input json.RawMessage) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(input))
	opening, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '{' {
		return nil, errors.New("input must be a JSON object")
	}

	object := make(map[string]json.RawMessage)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, errors.New("object key must be a string")
		}
		if _, exists := object[key]; exists {
			return nil, fmt.Errorf("duplicate field %q", key)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		object[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("input contains trailing JSON")
		}
		return nil, err
	}
	return object, nil
}

// oneStringField extracts synonymous path fields without choosing between
// conflicting values. Empty or non-string values are invalid when provided.
func oneStringField(object map[string]json.RawMessage, required bool, keys ...string) (string, error) {
	var value string
	found := false
	for _, key := range keys {
		raw, exists := object[key]
		if !exists {
			continue
		}
		var candidate string
		if err := json.Unmarshal(raw, &candidate); err != nil {
			return "", fmt.Errorf("field %q must be a string", key)
		}
		if strings.TrimSpace(candidate) == "" {
			return "", fmt.Errorf("field %q must not be empty", key)
		}
		if found && candidate != value {
			return "", fmt.Errorf("conflicting path fields")
		}
		value = candidate
		found = true
	}
	if required && !found {
		return "", fmt.Errorf("missing field %q", keys[0])
	}
	return value, nil
}

func canonicalWorkspace(workDir string) (string, error) {
	if strings.TrimSpace(workDir) == "" {
		return "", errors.New("working directory is empty")
	}
	if err := validatePathSyntax(workDir); err != nil {
		return "", err
	}
	absWork, err := filepath.Abs(workDir)
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(absWork)
	if err != nil {
		return "", fmt.Errorf("resolve working directory links: %w", err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("inspect working directory: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("working directory is not a directory")
	}
	return filepath.Clean(canonical), nil
}

// canonicalPathWithinWorkspace resolves existing targets through all symlinks.
// For a new target it resolves the nearest existing parent, then appends only
// the missing lexical components. A broken symlink or inaccessible ancestor is
// an error, never permission to defer the decision to the tool.
func canonicalPathWithinWorkspace(rawPath, baseDir, workspace string, cfg PermissionConfig) (string, error) {
	if strings.TrimSpace(rawPath) == "" {
		return "", errors.New("path is empty")
	}
	if blocked := matchingBlockedPath(rawPath, cfg.BlockedPaths); blocked != "" {
		return "", fmt.Errorf("Blocked path: %s", blocked)
	}
	if err := validatePathSyntax(rawPath); err != nil {
		return "", fmt.Errorf("unresolvable path: %w", err)
	}

	resolved := rawPath
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(baseDir, resolved)
	}
	absPath, err := filepath.Abs(resolved)
	if err != nil {
		return "", fmt.Errorf("unresolvable path: %w", err)
	}
	canonical, err := canonicalizeTarget(absPath)
	if err != nil {
		return "", fmt.Errorf("unresolvable path: %w", err)
	}
	if blocked := matchingBlockedPath(canonical, cfg.BlockedPaths); blocked != "" {
		return "", fmt.Errorf("Blocked path: %s", blocked)
	}
	if !pathWithin(canonical, workspace) {
		return "", fmt.Errorf("Path outside workspace: %s", rawPath)
	}
	return canonical, nil
}

func canonicalizeTarget(target string) (string, error) {
	current := filepath.Clean(target)
	missing := make([]string, 0, 4)

	for {
		_, err := os.Lstat(current)
		switch {
		case err == nil:
			canonical, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", fmt.Errorf("resolve links for %q: %w", current, err)
			}
			if len(missing) > 0 {
				resolvedInfo, err := os.Stat(canonical)
				if err != nil {
					return "", fmt.Errorf("inspect parent %q: %w", canonical, err)
				}
				if !resolvedInfo.IsDir() {
					return "", fmt.Errorf("nearest existing parent %q is not a directory", canonical)
				}
			}
			for index := len(missing) - 1; index >= 0; index-- {
				canonical = filepath.Join(canonical, missing[index])
			}
			return filepath.Clean(canonical), nil

		case !os.IsNotExist(err):
			return "", fmt.Errorf("inspect %q: %w", current, err)

		default:
			parent := filepath.Dir(current)
			if parent == current {
				return "", fmt.Errorf("no existing parent for %q", target)
			}
			missing = append(missing, filepath.Base(current))
			current = parent
		}
	}
}

func checkGlobPattern(pattern, baseDir, workspace string, cfg PermissionConfig) error {
	if strings.TrimSpace(pattern) == "" {
		return errors.New("glob pattern is empty")
	}
	if err := validatePathSyntax(pattern); err != nil {
		return fmt.Errorf("invalid glob pattern: %w", err)
	}
	if blocked := matchingBlockedPath(pattern, cfg.BlockedPaths); blocked != "" {
		return fmt.Errorf("Blocked path: %s", blocked)
	}
	if hasParentTraversalComponent(pattern) {
		return errors.New("glob pattern contains parent traversal")
	}
	if _, err := filepath.Match(pattern, ""); err != nil {
		return fmt.Errorf("invalid glob pattern: %w", err)
	}

	root := globLiteralRoot(pattern)
	if _, err := canonicalPathWithinWorkspace(root, baseDir, workspace, cfg); err != nil {
		return err
	}
	return nil
}

func globLiteralRoot(pattern string) string {
	meta := strings.IndexAny(pattern, "*?[")
	if meta < 0 {
		return pattern
	}
	return filepath.Dir(pattern[:meta])
}

func hasParentTraversalComponent(path string) bool {
	for _, component := range strings.FieldsFunc(path, func(r rune) bool {
		return r == '/' || r == '\\'
	}) {
		if component == ".." {
			return true
		}
	}
	return false
}

func matchingBlockedPath(path string, blockedPaths []string) string {
	candidate := path
	if runtime.GOOS == "windows" {
		candidate = strings.ToLower(filepath.ToSlash(candidate))
	}
	for _, blocked := range blockedPaths {
		needle := blocked
		if runtime.GOOS == "windows" {
			needle = strings.ToLower(filepath.ToSlash(needle))
		}
		if strings.Contains(candidate, needle) {
			return blocked
		}
	}
	return ""
}

func validatePathSyntax(path string) error {
	if strings.IndexByte(path, 0) >= 0 {
		return errors.New("path contains a NUL byte")
	}
	if runtime.GOOS != "windows" {
		return nil
	}

	normalized := strings.ReplaceAll(path, "/", `\`)
	lower := strings.ToLower(normalized)
	if strings.HasPrefix(lower, `\\?\`) || strings.HasPrefix(lower, `\\.\`) {
		return errors.New("Windows device paths are not accepted")
	}
	volume := filepath.VolumeName(path)
	if volume != "" && !filepath.IsAbs(path) {
		return errors.New("drive-relative paths are ambiguous")
	}
	if volume == "" && len(path) > 0 && os.IsPathSeparator(path[0]) {
		return errors.New("root-relative paths are ambiguous")
	}
	if strings.Contains(path[len(volume):], ":") {
		return errors.New("alternate data stream paths are not accepted")
	}
	for _, component := range strings.FieldsFunc(path[len(volume):], func(r rune) bool {
		return r == '/' || r == '\\'
	}) {
		if component == "." || component == ".." {
			continue
		}
		if strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") {
			return errors.New("Windows paths with trailing dots or spaces are ambiguous")
		}
		base := component
		if dot := strings.IndexByte(base, '.'); dot >= 0 {
			base = base[:dot]
		}
		switch strings.ToUpper(base) {
		case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
			"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9", "CONIN$", "CONOUT$":
			return fmt.Errorf("Windows reserved device name %q is not accepted", component)
		}
	}
	return nil
}
