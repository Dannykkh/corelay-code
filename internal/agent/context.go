package agent

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unicode"
)

// LoadProjectContext reads applicable ancestor/workspace instructions plus the
// selected workspace's safe .claude settings and README. Nested target scopes
// are loaded by projectInstructionDisclosure immediately before path tools.
func LoadProjectContext(workDir string) string {
	var parts []string

	// User-level CLAUDE instructions are defaults. Put them before project
	// scopes so the more specific, repository-owned constraints follow them.
	home, _ := os.UserHomeDir()
	if home != "" {
		content := readFileIfExists(filepath.Join(home, ".claude", "CLAUDE.md"))
		if content != "" {
			parts = append(parts, "## User Instructions (~/.claude/CLAUDE.md)\n"+content)
		}
	}

	workspace := canonicalProjectDirectory(workDir)
	if workspace != "" {
		var instructionFiles []projectInstructionFile
		for _, scope := range directoryChain(workspace) {
			instructionFiles = append(instructionFiles, projectInstructionFilesAt(scope)...)
		}
		if len(instructionFiles) > 0 {
			parts = append(parts, renderInstructionContract())
			parts = append(parts, renderProjectInstructionFiles(instructionFiles...))
			parts = append(parts, renderInstructionPrecedenceSummary())
		}
	}

	if workspace != "" {
		// .claude/settings.json (project-level, non-secret allowlist)
		settings := safeProjectSettings(readFileIfExists(filepath.Join(workspace, ".claude", "settings.json")))
		if settings != "" {
			parts = append(parts, "## Project Settings (non-secret allowlist)\n```json\n"+settings+"\n```")
		}

		// README.md summary (first 50 lines for project context)
		for _, name := range []string{"README.md", "readme.md"} {
			content := readFileIfExists(filepath.Join(workspace, name))
			if content != "" {
				lines := strings.Split(content, "\n")
				if len(lines) > 50 {
					lines = lines[:50]
				}
				parts = append(parts, "## Project README (summary)\n"+strings.Join(lines, "\n"))
				break
			}
		}
	}

	if len(parts) == 0 {
		return ""
	}

	return "\n\n--- PROJECT CONTEXT ---\n" + strings.Join(parts, "\n\n")
}

type projectInstructionFile struct {
	path    string
	name    string
	content string
}

func canonicalProjectDirectory(workDir string) string {
	canonical, err := canonicalWorkspace(workDir)
	if err != nil {
		return ""
	}
	return canonical
}

// directoryChain returns filesystem scopes from the volume/root down to dir.
// This includes only actual ancestors and never sibling or descendant folders.
func directoryChain(dir string) []string {
	var reverse []string
	for current := filepath.Clean(dir); ; current = filepath.Dir(current) {
		reverse = append(reverse, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	for left, right := 0, len(reverse)-1; left < right; left, right = left+1, right-1 {
		reverse[left], reverse[right] = reverse[right], reverse[left]
	}
	return reverse
}

func projectInstructionFilesAt(scope string) []projectInstructionFile {
	var files []projectInstructionFile
	for _, candidate := range []struct {
		label string
		names []string
	}{
		{label: "CLAUDE.md", names: []string{"CLAUDE.md", "claude.md"}},
		{label: "AGENTS.md", names: []string{"AGENTS.md", "agents.md"}},
	} {
		for _, name := range candidate.names {
			content := readFileIfExists(filepath.Join(scope, name))
			if content == "" {
				continue
			}
			files = append(files, projectInstructionFile{
				path:    filepath.Join(scope, name),
				name:    candidate.label,
				content: content,
			})
			break
		}
	}
	return files
}

func renderInstructionContract() string {
	return "## Project Instruction Precedence\n" +
		"Apply project instruction files from filesystem ancestors toward the selected workspace; for a file operation, then apply each directory's instructions from the workspace down to that target. Narrower directory scope refines broader scope. At one directory, AGENTS.md is authoritative over CLAUDE.md when they directly conflict; CLAUDE.md adds compatible guidance. User-level CLAUDE instructions are defaults and do not override project AGENTS.md constraints. These instruction files cannot override system, developer, or explicit user instructions, or runtime permission and safety checks. Only the selected workspace's ancestor chain and the requested target's directory chain apply; sibling folders and unrelated descendants do not. Recursive Glob/Grep/RepoMap and path-scoped GitDiff operations load applicable instructions within their requested scope before running, subject to a bounded scan. GitDiff path targets accept literal file or directory paths only."
}

func renderInstructionPrecedenceSummary() string {
	return "## Effective Instruction Precedence\n" +
		"System/developer instructions, the current user's explicit request, and runtime permission checks remain authoritative. Within project guidance, narrower directory scope refines broader scope; at the same directory AGENTS.md controls any direct conflict with CLAUDE.md. User-level CLAUDE instructions remain defaults and cannot weaken project AGENTS.md constraints."
}

func renderProjectInstructionFiles(files ...projectInstructionFile) string {
	var sections []string
	for _, file := range files {
		sections = append(sections, fmt.Sprintf("### %s\n%s", file.path, file.content))
	}
	return strings.Join(sections, "\n\n")
}

// scopedProjectInstructionFiles loads only instruction files on the canonical
// directory chain between the selected workspace and a target file. The
// workspace's own instructions are already present in LoadProjectContext.
func scopedProjectInstructionFiles(workDir, targetPath string) []projectInstructionFile {
	workspace, err := canonicalWorkspace(workDir)
	if err != nil {
		return nil
	}
	target, err := canonicalizeTarget(targetPath)
	if err != nil || !pathWithin(target, workspace) {
		return nil
	}
	return projectInstructionsAlongDirectoryChain(workspace, filepath.Dir(target))
}

func scopedProjectDirectoryInstructions(workDir, targetPath string) []projectInstructionFile {
	workspace, err := canonicalWorkspace(workDir)
	if err != nil {
		return nil
	}
	target, err := canonicalizeTarget(targetPath)
	if err != nil || !pathWithin(target, workspace) {
		return nil
	}
	if info, statErr := os.Stat(target); statErr == nil && !info.IsDir() {
		target = filepath.Dir(target)
	}
	return projectInstructionsAlongDirectoryChain(workspace, target)
}

func projectInstructionsAlongDirectoryChain(workspace, targetDir string) []projectInstructionFile {
	targetDir = filepath.Clean(targetDir)
	if !pathWithin(targetDir, workspace) {
		return nil
	}
	if sameInstructionPath(targetDir, workspace) {
		return nil
	}
	if _, err := filepath.Rel(workspace, targetDir); err != nil {
		return nil
	}
	var scopes []string
	for current := targetDir; pathWithin(current, workspace) && !sameInstructionPath(current, workspace); current = filepath.Dir(current) {
		scopes = append(scopes, current)
	}
	for left, right := 0, len(scopes)-1; left < right; left, right = left+1, right-1 {
		scopes[left], scopes[right] = scopes[right], scopes[left]
	}
	var files []projectInstructionFile
	for _, scope := range scopes {
		files = append(files, projectInstructionFilesAt(scope)...)
	}
	return files
}

func scopedProjectFileOrDirectoryInstructions(workDir, targetPath string) ([]projectInstructionFile, bool) {
	workspace, err := canonicalWorkspace(workDir)
	if err != nil {
		return nil, false
	}
	target, err := canonicalizeTarget(targetPath)
	if err != nil {
		return nil, false
	}
	if !pathWithin(target, workspace) {
		// Explicit full mode may diff external paths. Such a target has no
		// instruction chain in this selected workspace.
		return nil, true
	}
	relative, err := filepath.Rel(workspace, target)
	if err != nil {
		return nil, false
	}
	if strings.HasPrefix(relative, ":") {
		// Git pathspec magic can invert or otherwise broaden the requested set.
		// Inspect the selected workspace under the same bounded discovery limit.
		return scopedProjectTreeInstructions(workDir, workspace, nil)
	}
	if strings.ContainsAny(relative, "*?[") {
		root := filepath.Join(workspace, instructionGlobLiteralRoot(filepath.ToSlash(relative)))
		return scopedProjectTreeInstructions(workDir, root, nil)
	}
	if info, statErr := os.Stat(target); statErr == nil && info.IsDir() {
		files, complete := scopedProjectTreeInstructions(workDir, target, nil)
		return files, complete
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return nil, false
	}
	return projectInstructionsAlongDirectoryChain(workspace, filepath.Dir(target)), true
}

func scopedGlobInstructions(workDir, base, pattern string) ([]projectInstructionFile, bool) {
	if strings.Contains(pattern, "**") {
		return scopedProjectTreeInstructions(workDir, filepath.Join(base, globLiteralRoot(pattern)), nil)
	}

	files := scopedProjectDirectoryInstructions(workDir, base)
	matches, err := filepath.Glob(filepath.Join(base, filepath.FromSlash(pattern)))
	if err != nil || len(matches) > maxTreeWalkEntries {
		return nil, false
	}
	for _, match := range matches {
		files = append(files, scopedProjectDirectoryInstructions(workDir, filepath.Dir(match))...)
		info, statErr := os.Lstat(match)
		if statErr == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			files = append(files, scopedProjectDirectoryInstructions(workDir, match)...)
		}
	}
	files = uniqueProjectInstructionFiles(files)
	if len(files) > maxTreeInstructionFiles {
		return nil, false
	}
	totalBytes := 0
	for _, file := range files {
		totalBytes += len(file.content)
	}
	return files, totalBytes <= maxTreeInstructionBytes
}

func scopedGrepInstructions(workDir, base, pattern string) ([]projectInstructionFile, bool) {
	if pattern == "" {
		return scopedProjectTreeInstructions(workDir, base, nil)
	}
	normalized := strings.ReplaceAll(pattern, `\`, "/")
	if strings.HasPrefix(normalized, "!") {
		// ripgrep treats a leading ! as an exclusion, so matching results may
		// come from any path outside the named exclusion.
		return scopedProjectTreeInstructions(workDir, base, nil)
	}
	if strings.ContainsAny(normalized, "{}") {
		// ripgrep expands brace globs, while filepath.Glob treats braces as
		// literals. Scan only the common literal prefix before alternatives so
		// every reachable nested instruction is disclosed without pulling in
		// unrelated sibling trees beyond that prefix.
		root := filepath.Join(base, instructionGlobLiteralRoot(normalized))
		return scopedProjectTreeInstructions(workDir, root, nil)
	}
	if !strings.Contains(normalized, "/") || strings.Contains(normalized, "**") {
		return scopedProjectTreeInstructions(workDir, filepath.Join(base, globLiteralRoot(normalized)), nil)
	}

	root := filepath.Join(base, globLiteralRoot(normalized))
	files := scopedProjectDirectoryInstructions(workDir, root)
	matches, err := filepath.Glob(filepath.Join(base, filepath.FromSlash(normalized)))
	if err != nil || len(matches) > maxTreeWalkEntries {
		return nil, false
	}
	for _, match := range matches {
		files = append(files, scopedProjectDirectoryInstructions(workDir, filepath.Dir(match))...)
		info, statErr := os.Lstat(match)
		if statErr == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			files = append(files, scopedProjectDirectoryInstructions(workDir, match)...)
		}
	}
	files = uniqueProjectInstructionFiles(files)
	if len(files) > maxTreeInstructionFiles {
		return nil, false
	}
	totalBytes := 0
	for _, file := range files {
		totalBytes += len(file.content)
	}
	return files, totalBytes <= maxTreeInstructionBytes
}

func instructionGlobLiteralRoot(pattern string) string {
	meta := strings.IndexAny(pattern, "*?[{")
	if meta < 0 {
		return pattern
	}
	return filepath.Dir(pattern[:meta])
}

const (
	maxTreeInstructionFiles = 64
	maxTreeInstructionBytes = 128 * 1024
	maxTreeWalkEntries      = 20000
)

// scopedProjectTreeInstructions resolves every instruction file under a
// recursive search root before Glob, Grep, GitDiff, or RepoMap can traverse it. The
// caller must block the tool if discovery was incomplete, so a partial walk
// cannot silently expose files governed by instructions the model has not seen.
func scopedProjectTreeInstructions(workDir, targetPath string, skippedDirectories map[string]bool) ([]projectInstructionFile, bool) {
	workspace, err := canonicalWorkspace(workDir)
	if err != nil {
		return nil, false
	}
	base, err := canonicalizeTarget(targetPath)
	if err != nil {
		return nil, false
	}
	if !pathWithin(base, workspace) {
		// Explicit full mode may search outside the selected project. Such a
		// target has no project-scoped instruction chain to disclose.
		return nil, true
	}
	info, statErr := os.Stat(base)
	if statErr == nil && !info.IsDir() {
		return projectInstructionsAlongDirectoryChain(workspace, filepath.Dir(base)), true
	}
	if statErr != nil && !os.IsNotExist(statErr) {
		return nil, false
	}
	if !pathWithin(base, workspace) {
		return nil, false
	}

	files := projectInstructionsAlongDirectoryChain(workspace, base)
	files = uniqueProjectInstructionFiles(files)
	seen := make(map[string]struct{}, len(files))
	totalBytes := 0
	for _, file := range files {
		seen[projectInstructionPathKey(file.path)] = struct{}{}
		totalBytes += len(file.content)
	}
	if len(files) > maxTreeInstructionFiles || totalBytes > maxTreeInstructionBytes {
		return nil, false
	}

	if os.IsNotExist(statErr) {
		return files, true
	}
	if statErr != nil || !info.IsDir() {
		return nil, false
	}

	visited := 0
	complete := true
	walkErr := filepath.WalkDir(base, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			complete = false
			return filepath.SkipAll
		}
		visited++
		if visited > maxTreeWalkEntries {
			complete = false
			return filepath.SkipAll
		}
		if !entry.IsDir() || sameInstructionPath(path, base) || sameInstructionPath(path, workspace) {
			return nil
		}
		name := strings.ToLower(entry.Name())
		if (skippedDirectories != nil && strings.HasPrefix(name, ".")) || skippedDirectories[name] {
			return filepath.SkipDir
		}
		for _, file := range projectInstructionFilesAt(path) {
			key := projectInstructionPathKey(file.path)
			if _, exists := seen[key]; exists {
				continue
			}
			if len(files) >= maxTreeInstructionFiles || totalBytes+len(file.content) > maxTreeInstructionBytes {
				complete = false
				return filepath.SkipAll
			}
			seen[key] = struct{}{}
			files = append(files, file)
			totalBytes += len(file.content)
		}
		return nil
	})
	if walkErr != nil || !complete {
		return nil, false
	}
	return files, true
}

func uniqueProjectInstructionFiles(files []projectInstructionFile) []projectInstructionFile {
	unique := make([]projectInstructionFile, 0, len(files))
	seen := make(map[string]struct{}, len(files))
	for _, file := range files {
		key := projectInstructionPathKey(file.path)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, file)
	}
	return unique
}

func projectInstructionPathKey(path string) string {
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	return path
}

func sameInstructionPath(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// projectInstructionDisclosure returns a synthetic tool result the first time
// each applicable nested instruction snapshot is encountered in a run. The
// dispatcher uses it before Read, Write, and Edit execution, so the model sees
// target-scope guidance before touching the file. Changed instructions disclose
// again because their content fingerprint changes.
type projectInstructionDisclosure struct {
	mu        sync.Mutex
	disclosed map[string]struct{}
	pending   map[string]struct{}
	inBatch   bool
}

func newProjectInstructionDisclosure() *projectInstructionDisclosure {
	return &projectInstructionDisclosure{disclosed: make(map[string]struct{})}
}

func (d *projectInstructionDisclosure) BeginBatch() {
	d.mu.Lock()
	d.inBatch = true
	d.pending = make(map[string]struct{})
	d.mu.Unlock()
}

func (d *projectInstructionDisclosure) FinishBatch() {
	d.mu.Lock()
	for key := range d.pending {
		d.disclosed[key] = struct{}{}
	}
	d.pending = nil
	d.inBatch = false
	d.mu.Unlock()
}

func (d *projectInstructionDisclosure) Check(workDir, targetPath string) (bool, string) {
	files := scopedProjectInstructionFiles(workDir, targetPath)
	return d.checkFiles(files)
}

func (d *projectInstructionDisclosure) checkFiles(files []projectInstructionFile) (bool, string) {
	if len(files) == 0 {
		return true, ""
	}

	var fresh []projectInstructionFile
	repeatedInBatch := false
	d.mu.Lock()
	for _, file := range files {
		key := instructionDisclosureKey(file)
		if _, ok := d.disclosed[key]; ok {
			continue
		}
		if d.inBatch {
			if _, ok := d.pending[key]; ok {
				repeatedInBatch = true
				continue
			}
			d.pending[key] = struct{}{}
		} else {
			d.disclosed[key] = struct{}{}
		}
		fresh = append(fresh, file)
	}
	d.mu.Unlock()
	if len(fresh) == 0 {
		if repeatedInBatch {
			return false, "This tool call was not executed because applicable project instructions were disclosed by another call in the same tool batch. Review that result and retry in a later turn if the operation follows the instructions."
		}
		return true, ""
	}
	message := "Project instruction files apply to this target. This tool call was not executed. Review the instructions below, then retry only if the requested operation follows them.\n\n" + renderInstructionContract() + "\n\n" + renderProjectInstructionFiles(fresh...) + "\n\n" + renderInstructionPrecedenceSummary()
	if repeatedInBatch {
		message += "\n\nOther applicable instructions were disclosed by another tool result in the same batch; review that result too."
	}
	return false, message
}

func (d *projectInstructionDisclosure) CheckTool(
	workDir string,
	policy *ExecutionPolicySnapshot,
	call toolUseBlock,
) (bool, string) {
	name := canonicalPermissionToolName(call.Name)
	paths, err := executionToolWorkspacePathsWithPolicy(name, call.Input, workDir, policy)
	if err != nil {
		// The ordinary permission/path boundary reports malformed or disallowed
		// targets. Instruction lookup must not mask it.
		return true, ""
	}
	var files []projectInstructionFile
	switch name {
	case "Read", "Write", "Edit", "NotebookRead", "ImageRead", "PDFRead", "LSP":
		for _, target := range paths.many("file_path") {
			files = append(files, scopedProjectInstructionFiles(workDir, target)...)
		}
	case "Diff":
		for _, key := range []string{"file_a", "file_b"} {
			for _, target := range paths.many(key) {
				files = append(files, scopedProjectInstructionFiles(workDir, target)...)
			}
		}
	case "GitDiff":
		targets := paths.many("file")
		if len(targets) == 0 {
			var complete bool
			files, complete = scopedProjectTreeInstructions(workDir, workDir, nil)
			if !complete {
				return false, "Project instruction discovery could not safely cover the workspace diff. This tool call was not executed; narrow the diff to a path and retry."
			}
		} else {
			for _, target := range targets {
				var complete bool
				var targetFiles []projectInstructionFile
				targetFiles, complete = scopedProjectFileOrDirectoryInstructions(workDir, target)
				if !complete {
					return false, "Project instruction discovery could not safely cover the requested diff path. This tool call was not executed; narrow the diff to a smaller path and retry."
				}
				files = append(files, targetFiles...)
			}
		}
	case "GitCommit":
		targets := paths.many("files")
		if len(targets) == 0 {
			var complete bool
			files, complete = scopedProjectTreeInstructions(workDir, workDir, nil)
			if !complete {
				return false, "Project instruction discovery could not safely cover the staged commit scope. This tool call was not executed; select explicit files or narrow the staged index and retry."
			}
		} else {
			for _, target := range targets {
				targetFiles, complete := scopedProjectFileOrDirectoryInstructions(workDir, target)
				if !complete {
					return false, "Project instruction discovery could not safely cover the requested commit path. This tool call was not executed; select a smaller path and retry."
				}
				files = append(files, targetFiles...)
			}
		}
	case "LS":
		files = scopedProjectDirectoryInstructions(workDir, paths.one("path"))
	case "Glob":
		var pattern struct {
			Pattern string `json:"pattern"`
		}
		_ = json.Unmarshal(call.Input, &pattern)
		var complete bool
		files, complete = scopedGlobInstructions(workDir, paths.one("path"), pattern.Pattern)
		if !complete {
			return false, "Project instruction discovery could not safely cover the requested glob. This tool call was not executed; narrow the directory or pattern and retry."
		}
	case "Grep":
		var pattern struct {
			Glob string `json:"glob"`
		}
		_ = json.Unmarshal(call.Input, &pattern)
		var complete bool
		files, complete = scopedGrepInstructions(workDir, paths.one("path"), pattern.Glob)
		if !complete {
			return false, "Project instruction discovery could not safely cover the requested search. This tool call was not executed; narrow the directory or glob and retry."
		}
	case "RepoMap":
		var complete bool
		files, complete = scopedProjectTreeInstructions(workDir, paths.one("path"), repoMapSkippedDirectories)
		if !complete {
			return false, "Project instruction discovery could not safely cover the requested search tree. This tool call was not executed; narrow the directory scope and retry."
		}
	default:
		return true, ""
	}
	return d.checkFiles(uniqueProjectInstructionFiles(files))
}

func instructionDisclosureKey(file projectInstructionFile) string {
	return fmt.Sprintf("%s\x00%x", projectInstructionPathKey(file.path), sha256.Sum256([]byte(file.content)))
}

// safeProjectSettings copies only non-secret fields with an explicit prompt
// contract. Provider credentials, environment values, hooks, and MCP server
// configuration stay in their dedicated runtime loaders.
func safeProjectSettings(raw string) string {
	var source map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &source) != nil {
		return ""
	}
	filtered := make(map[string]json.RawMessage)
	if model := source["model"]; len(model) > 0 {
		var value string
		if json.Unmarshal(model, &value) == nil && len(value) <= 128 {
			filtered["model"] = model
		}
	}
	if permissions := source["permissions"]; len(permissions) > 0 {
		var rules map[string][]string
		if json.Unmarshal(permissions, &rules) == nil {
			allowed := make(map[string][]string)
			for _, key := range []string{"allow", "deny", "ask"} {
				for _, rule := range rules[key] {
					if len(rule) > 0 && len(rule) <= 512 {
						allowed[key] = append(allowed[key], redactSensitiveString(rule))
					}
				}
			}
			if encoded, err := json.Marshal(allowed); err == nil && len(allowed) > 0 {
				filtered["permissions"] = encoded
			}
		}
	}
	if len(filtered) == 0 {
		return ""
	}
	encoded, err := json.MarshalIndent(filtered, "", "  ")
	if err != nil {
		return ""
	}
	return string(encoded)
}

// LoadSkills reads skills using default "all" source.
func LoadSkills(workDir string) []SkillInfo {
	return LoadSkillsWithSource(workDir, "all", nil)
}

// LoadSkillsWithConfig for backward compatibility.
func LoadSkillsWithConfig(workDir string, extraDirs []string) []SkillInfo {
	return LoadSkillsWithSource(workDir, "all", extraDirs)
}

// LoadSkillsWithSource reads skills filtered by source.
// source: "claude", "codex", "gemini", "all", "none"
func LoadSkillsWithSource(workDir, source string, extraDirs []string) []SkillInfo {
	return LoadSkillsWithRoots(workDir, source, nil, extraDirs)
}

// LoadSkillsWithRoots reads project vendor roots, project custom roots,
// selected user-level sources, and global custom roots in precedence order.
// Bare skill names retain first-wins behavior; case-insensitive shadowed
// names are attached to the winner as diagnostics and namespaced alternatives.
func LoadSkillsWithRoots(workDir, source string, projectDirs, extraDirs []string) []SkillInfo {
	descriptors := SkillDescriptorRoots(workDir, source, projectDirs, extraDirs)
	skills := make([]SkillInfo, 0, len(descriptors))
	for _, descriptor := range descriptors {
		content, err := LoadSkillBody(descriptor)
		if err != nil {
			continue
		}
		skill := SkillInfo{
			Name: descriptor.Name, Content: content, Path: descriptor.Path,
			Source: descriptor.Source, Namespace: descriptor.Namespace,
			Aliases: append([]string(nil), descriptor.Aliases...),
		}
		for _, shadow := range descriptor.Shadowed {
			shadowDescriptor := descriptorFromOrigin(shadow)
			shadowContent, shadowErr := LoadSkillBody(shadowDescriptor)
			if shadowErr == nil {
				skill.Shadowed = append(skill.Shadowed, SkillOrigin{
					ID: shadow.ID, Name: shadow.Name, Description: shadow.Description,
					Source: shadow.Source, Namespace: shadow.Namespace,
					Path: shadow.Path, ModuleRoot: shadow.ModuleRoot,
					Aliases: append([]string(nil), shadow.Aliases...),
					Digest:  shadow.Digest, Size: shadow.Size, Content: shadowContent,
				})
			}
		}
		skills = append(skills, skill)
	}
	return skills
}

// skillNameFoldKey uses Unicode simple-fold equivalence, matching the
// case-insensitive comparison used when slash commands are resolved.
func skillNameFoldKey(name string) string {
	var folded strings.Builder
	for _, r := range name {
		representative := r
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			if next < representative {
				representative = next
			}
		}
		folded.WriteRune(representative)
	}
	return folded.String()
}

type SkillInfo struct {
	Name      string        `json:"name"`
	Content   string        `json:"content"`
	Path      string        `json:"path"`
	Source    string        `json:"source,omitempty"`
	Namespace string        `json:"namespace,omitempty"`
	Aliases   []string      `json:"aliases,omitempty"`
	Shadowed  []SkillOrigin `json:"shadowed,omitempty"`
}

// SkillOrigin identifies a losing copy hidden by the compatibility first-wins
// rule. Content stays internal so a namespaced slash command can execute that
// exact copy without returning hidden skill bodies from metadata APIs.
type SkillOrigin struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Source      string   `json:"source"`
	Namespace   string   `json:"namespace"`
	Path        string   `json:"path"`
	ModuleRoot  string   `json:"module_root,omitempty"`
	Aliases     []string `json:"aliases,omitempty"`
	Digest      string   `json:"digest"`
	Size        int64    `json:"size"`
	Content     string   `json:"-"`
}

// SkillDescription extracts a one-line summary from a SKILL.md body so the
// agent loop can advertise skills by name + description instead of inlining
// every skill's full content. It prefers the YAML frontmatter `description:`
// field, then falls back to the first non-empty, non-heading prose line.
// The result is collapsed to a single line and truncated to keep the system
// prompt small — critical for local models with a 32K-ish context window.
func SkillDescription(content string) string {
	const maxLen = 200

	clean := func(s string) string {
		s = strings.TrimSpace(s)
		s = strings.Trim(s, "\"'")
		s = strings.Join(strings.Fields(s), " ") // collapse whitespace/newlines
		if runes := []rune(s); len(runes) > maxLen {
			s = strings.TrimSpace(string(runes[:maxLen])) + "…"
		}
		return s
	}

	lines := strings.Split(content, "\n")

	// 1. YAML frontmatter `description:` (may be empty → multi-line block scalar)
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		for i := 1; i < len(lines); i++ {
			line := strings.TrimSpace(lines[i])
			if line == "---" {
				break
			}
			if rest, ok := strings.CutPrefix(line, "description:"); ok {
				// A bare block-scalar indicator (|, >, |-, >+, …) is not the
				// description itself — fall through to read the indented block.
				trimmed := strings.TrimSpace(rest)
				isBlockScalar := trimmed != "" && (trimmed[0] == '|' || trimmed[0] == '>')
				if v := clean(rest); v != "" && !isBlockScalar {
					return v
				}
				// Block scalar (description: | or >): take following indented lines.
				var block []string
				for j := i + 1; j < len(lines); j++ {
					l := lines[j]
					if strings.TrimSpace(l) == "---" || (l != "" && !strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "\t")) {
						break
					}
					block = append(block, strings.TrimSpace(l))
				}
				if v := clean(strings.Join(block, " ")); v != "" {
					return v
				}
			}
		}
	}

	// 2. First non-empty, non-heading, non-frontmatter prose line.
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" || t == "---" || strings.HasPrefix(t, "#") {
			continue
		}
		if v := clean(t); v != "" {
			return v
		}
	}
	return ""
}

// LoadMCPConfig reads MCP configuration from multiple sources.
// Priority: .mcp.json > .claude/settings.json > ~/.claude/settings.json
func LoadMCPConfig(workDir string) string {
	// 1. Project .mcp.json
	for _, name := range []string{".mcp.json", "mcp.json"} {
		content := readFileIfExists(filepath.Join(workDir, name))
		if content != "" {
			return content
		}
	}

	// 2. Project .claude/settings.json → extract mcpServers
	mcpFromSettings := extractMCPFromSettings(filepath.Join(workDir, ".claude", "settings.json"))
	if mcpFromSettings != "" {
		return mcpFromSettings
	}

	// 3. Global ~/.claude/settings.json
	home, _ := os.UserHomeDir()
	if home != "" {
		mcpFromSettings = extractMCPFromSettings(filepath.Join(home, ".claude", "settings.json"))
		if mcpFromSettings != "" {
			return mcpFromSettings
		}
	}

	return ""
}

// extractMCPFromSettings extracts mcpServers from a Claude settings.json file.
func extractMCPFromSettings(path string) string {
	content := readFileIfExists(path)
	if content == "" {
		return ""
	}

	var settings map[string]interface{}
	if json.Unmarshal([]byte(content), &settings) != nil {
		return ""
	}

	// Look for mcpServers key
	mcpServers, ok := settings["mcpServers"]
	if !ok {
		return ""
	}

	// Wrap in expected format
	wrapped := map[string]interface{}{
		"mcpServers": mcpServers,
	}
	data, _ := json.Marshal(wrapped)
	return string(data)
}

func readFileIfExists(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	content := strings.TrimSpace(string(data))
	// Limit size to prevent huge files from flooding the prompt
	if len(content) > 20000 {
		content = content[:20000] + "\n... (truncated)"
	}
	return content
}
