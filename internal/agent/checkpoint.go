package agent

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/Dannykkh/corelay-code/internal/config"
)

// Checkpoint / undo: before the agent's first edit in a turn the previous undo
// buffer is cleared (single-level, most-recent-turn undo); each edited file's
// prior state is backed up once. "/undo" restores them — files the agent
// created are deleted, files it modified are reverted. Backups live under
// <state dir>/undo/<hash> so the user's workspace stays clean.

const checkpointManifestVersion = 1

type checkpointPaths struct {
	stateDir string
	undoDir  string
	dir      string
	manifest string
}

func checkpointDir(workDir string) string {
	canonical, err := canonicalWorkspace(workDir)
	if err != nil {
		return ""
	}
	paths, err := checkpointPathsFor(canonical)
	if err != nil {
		return ""
	}
	return paths.dir
}

type ckptFile struct {
	Existed bool   `json:"existed"`
	Backup  string `json:"backup"` // clean relative path within the checkpoint dir
}

type ckptManifest struct {
	Version int                 `json:"version"`
	WorkDir string              `json:"workDir"`
	Files   map[string]ckptFile `json:"files"` // clean workspace-relative path -> info
}

// startCheckpoint clears the previous undo buffer to begin a new generation.
func startCheckpoint(workDir string) {
	canonical, err := canonicalWorkspace(workDir)
	if err != nil {
		return
	}
	paths, err := prepareCheckpointDirectory(canonical, true)
	if err != nil {
		return
	}
	m := ckptManifest{
		Version: checkpointManifestVersion,
		WorkDir: canonical,
		Files:   map[string]ckptFile{},
	}
	_ = checkpointAtomicWrite(paths.manifest, mustMarshalCheckpoint(m), 0o644)
}

// checkpointFile backs up a file's current (pre-edit) state once per generation.
func checkpointFile(workDir, relPath, absPath string) {
	canonicalWork, err := canonicalWorkspace(workDir)
	if err != nil {
		return
	}
	paths, err := validateCheckpointControlTree(canonicalWork)
	if err != nil {
		return
	}
	m, _, found, err := readManifestStrict(canonicalWork)
	if err != nil || !found {
		return
	}

	canonicalTarget, targetExists, _, err := resolveCheckpointCaptureTarget(canonicalWork, relPath)
	if err != nil {
		return
	}
	providedTarget, _, _, err := resolveCheckpointCaptureTarget(canonicalWork, absPath)
	if err != nil || !sameCheckpointPath(canonicalTarget, providedTarget) {
		return
	}
	if err := rejectProtectedCheckpointTarget(canonicalTarget, canonicalWork, paths.stateDir); err != nil {
		return
	}
	storedRel, err := filepath.Rel(canonicalWork, canonicalTarget)
	if err != nil {
		return
	}
	storedRel, err = cleanCheckpointRelative(storedRel)
	if err != nil {
		return
	}
	if _, done := m.Files[storedRel]; done {
		return
	}

	entry := ckptFile{}
	if targetExists {
		data, err := os.ReadFile(canonicalTarget)
		if err != nil {
			return
		}
		entry.Existed = true
		entry.Backup = hex.EncodeToString(sha1Sum(storedRel)) + ".bak"
		backupPath := filepath.Join(paths.dir, entry.Backup)
		if err := checkpointAtomicWrite(backupPath, data, 0o600); err != nil {
			return
		}
	}
	m.Files[storedRel] = entry
	_ = checkpointAtomicWrite(paths.manifest, mustMarshalCheckpoint(m), 0o644)
}

func sha1Sum(s string) []byte {
	sum := sha1.Sum([]byte(s))
	return sum[:]
}

// undoCheckpoint preserves the original helper contract. Invalid checkpoint
// state is safely treated as non-revertible; the explicit command path uses
// undoCheckpointSecure so it can tell the user that validation was refused.
func undoCheckpoint(workDir string) (reverted []string, ok bool) {
	reverted, ok, _ = undoCheckpointSecure(workDir)
	return reverted, ok
}

type undoAction struct {
	rel        string
	target     string
	backup     []byte
	delete     bool
	original   []byte
	origMode   os.FileMode
	origExists bool
}

// undoCheckpointSecure validates and preloads the complete manifest before it
// mutates a single workspace path. This is the critical all-or-nothing
// preflight: one malicious or stale entry rejects the entire undo operation.
func undoCheckpointSecure(workDir string) (reverted []string, ok bool, retErr error) {
	canonicalWork, err := canonicalWorkspace(workDir)
	if err != nil {
		return nil, false, fmt.Errorf("canonical workspace: %w", err)
	}
	m, paths, found, err := readManifestStrict(canonicalWork)
	if err != nil {
		return nil, false, err
	}
	if !found || len(m.Files) == 0 {
		return nil, false, nil
	}

	actions, err := preflightUndoManifest(canonicalWork, paths, m)
	if err != nil {
		return nil, false, err
	}
	if len(actions) == 0 {
		if err := os.RemoveAll(paths.dir); err != nil {
			return nil, false, fmt.Errorf("clear exhausted checkpoint: %w", err)
		}
		return nil, false, nil
	}

	applied := make([]undoAction, 0, len(actions))
	for _, action := range actions {
		if action.delete {
			if err := os.Remove(action.target); err != nil {
				rollbackUndoActions(applied)
				return nil, false, fmt.Errorf("delete created file %q: %w", action.rel, err)
			}
			reverted = append(reverted, action.rel+" (created → deleted)")
		} else {
			if err := checkpointAtomicWrite(action.target, action.backup, 0o644); err != nil {
				rollbackUndoActions(applied)
				return nil, false, fmt.Errorf("restore %q: %w", action.rel, err)
			}
			reverted = append(reverted, action.rel)
		}
		applied = append(applied, action)
	}

	if err := os.RemoveAll(paths.dir); err != nil {
		return reverted, true, fmt.Errorf("undo completed but checkpoint cleanup failed: %w", err)
	}
	return reverted, len(reverted) > 0, nil
}

func preflightUndoManifest(workDir string, paths checkpointPaths, m ckptManifest) ([]undoAction, error) {
	keys := make([]string, 0, len(m.Files))
	for rel := range m.Files {
		keys = append(keys, rel)
	}
	sort.Strings(keys)

	actions := make([]undoAction, 0, len(keys))
	targets := make(map[string]string, len(keys))
	backups := make(map[string]string, len(keys))
	for _, manifestRel := range keys {
		entry := m.Files[manifestRel]
		rel, err := cleanCheckpointRelative(manifestRel)
		if err != nil {
			return nil, fmt.Errorf("invalid manifest target %q: %w", manifestRel, err)
		}
		target, exists, info, err := resolveCheckpointTarget(workDir, rel)
		if err != nil {
			return nil, fmt.Errorf("resolve manifest target %q: %w", rel, err)
		}
		if err := rejectProtectedCheckpointTarget(target, workDir, paths.stateDir); err != nil {
			return nil, fmt.Errorf("protected manifest target %q: %w", rel, err)
		}
		key := checkpointPathKey(target)
		if previous, duplicate := targets[key]; duplicate {
			return nil, fmt.Errorf("manifest targets %q and %q resolve to the same path", previous, rel)
		}
		targets[key] = rel

		action := undoAction{rel: rel, target: target}
		if exists {
			if info == nil || !info.Mode().IsRegular() {
				return nil, fmt.Errorf("manifest target %q is not a regular file", rel)
			}
			action.original, err = os.ReadFile(target)
			if err != nil {
				return nil, fmt.Errorf("preload current target %q: %w", rel, err)
			}
			action.origMode = info.Mode().Perm()
			action.origExists = true
		}

		if entry.Existed {
			backupRel, err := cleanCheckpointRelative(entry.Backup)
			if err != nil {
				return nil, fmt.Errorf("invalid backup for %q: %w", rel, err)
			}
			if isCheckpointControlRelative(backupRel) {
				return nil, fmt.Errorf("backup for %q names checkpoint control state", rel)
			}
			backupPath, err := resolveCheckpointBackup(paths, backupRel)
			if err != nil {
				return nil, fmt.Errorf("resolve backup for %q: %w", rel, err)
			}
			backupKey := checkpointPathKey(backupPath)
			if previous, duplicate := backups[backupKey]; duplicate {
				return nil, fmt.Errorf("manifest entries %q and %q share one backup", previous, rel)
			}
			backups[backupKey] = rel
			action.backup, err = os.ReadFile(backupPath)
			if err != nil {
				return nil, fmt.Errorf("preload backup for %q: %w", rel, err)
			}
		} else {
			if entry.Backup != "" {
				return nil, fmt.Errorf("created-file entry %q must not name a backup", rel)
			}
			if !exists {
				continue
			}
			lexicalInfo, err := os.Lstat(filepath.Join(workDir, rel))
			if err != nil {
				return nil, fmt.Errorf("inspect created-file target %q: %w", rel, err)
			}
			if lexicalInfo.Mode()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("created-file target %q changed into a symlink", rel)
			}
			// Generated checkpoints only mark regular newly-created files. A
			// symlink or directory here means state changed after capture.
			action.delete = true
		}
		actions = append(actions, action)
	}
	return actions, nil
}

func rollbackUndoActions(actions []undoAction) {
	for index := len(actions) - 1; index >= 0; index-- {
		action := actions[index]
		if action.origExists {
			_ = checkpointAtomicWrite(action.target, action.original, action.origMode)
		} else {
			_ = os.Remove(action.target)
		}
	}
}

func readManifestStrict(workDir string) (ckptManifest, checkpointPaths, bool, error) {
	paths, err := checkpointPathsFor(workDir)
	if err != nil {
		return ckptManifest{}, checkpointPaths{}, false, err
	}
	info, err := os.Lstat(paths.manifest)
	if os.IsNotExist(err) {
		return ckptManifest{}, paths, false, nil
	}
	if err != nil {
		return ckptManifest{}, paths, false, fmt.Errorf("inspect checkpoint manifest: %w", err)
	}
	if !info.Mode().IsRegular() {
		return ckptManifest{}, paths, false, errors.New("checkpoint manifest is not a regular file")
	}
	paths, err = validateCheckpointControlTree(workDir)
	if err != nil {
		return ckptManifest{}, paths, false, err
	}

	data, err := os.ReadFile(paths.manifest)
	if err != nil {
		return ckptManifest{}, paths, false, fmt.Errorf("read checkpoint manifest: %w", err)
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return ckptManifest{}, paths, false, fmt.Errorf("invalid checkpoint manifest: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var m ckptManifest
	if err := decoder.Decode(&m); err != nil {
		return ckptManifest{}, paths, false, fmt.Errorf("decode checkpoint manifest: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return ckptManifest{}, paths, false, fmt.Errorf("decode checkpoint manifest: %w", err)
	}
	if err := validateCheckpointManifestHeader(m, workDir); err != nil {
		return ckptManifest{}, paths, false, err
	}
	return m, paths, true, nil
}

func validateCheckpointManifestHeader(m ckptManifest, workDir string) error {
	if m.Version != checkpointManifestVersion {
		return fmt.Errorf("unsupported checkpoint manifest version %d", m.Version)
	}
	if m.Files == nil {
		return errors.New("checkpoint manifest files must be an object")
	}
	if strings.TrimSpace(m.WorkDir) == "" || !filepath.IsAbs(m.WorkDir) || filepath.Clean(m.WorkDir) != m.WorkDir {
		return errors.New("checkpoint manifest workDir is not a clean absolute path")
	}
	manifestWork, err := canonicalWorkspace(m.WorkDir)
	if err != nil {
		return fmt.Errorf("canonicalize checkpoint manifest workDir: %w", err)
	}
	if !sameCheckpointPath(m.WorkDir, manifestWork) || !sameCheckpointPath(manifestWork, workDir) {
		return errors.New("checkpoint manifest is bound to a different workspace")
	}
	return nil
}

func checkpointPathsFor(canonicalWork string) (checkpointPaths, error) {
	stateDir, err := filepath.Abs(config.BaseDir())
	if err != nil {
		return checkpointPaths{}, fmt.Errorf("resolve agent state directory: %w", err)
	}
	sum := sha1.Sum([]byte(checkpointPathKey(canonicalWork)))
	dir := filepath.Join(stateDir, "undo", hex.EncodeToString(sum[:10]))
	return checkpointPaths{
		stateDir: stateDir,
		undoDir:  filepath.Join(stateDir, "undo"),
		dir:      dir,
		manifest: filepath.Join(dir, "manifest.json"),
	}, nil
}

func prepareCheckpointDirectory(canonicalWork string, clear bool) (checkpointPaths, error) {
	paths, err := checkpointPathsFor(canonicalWork)
	if err != nil {
		return paths, err
	}
	if info, err := os.Lstat(paths.stateDir); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return paths, errors.New("agent state is not a regular directory")
		}
	} else if os.IsNotExist(err) {
		if err := os.MkdirAll(paths.stateDir, 0o755); err != nil {
			return paths, fmt.Errorf("create agent state directory: %w", err)
		}
	} else {
		return paths, fmt.Errorf("inspect agent state directory: %w", err)
	}
	if info, err := os.Lstat(paths.undoDir); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return paths, errors.New("checkpoint root is not a regular directory")
		}
	} else if os.IsNotExist(err) {
		if err := os.Mkdir(paths.undoDir, 0o755); err != nil {
			return paths, fmt.Errorf("create checkpoint root: %w", err)
		}
	} else {
		return paths, fmt.Errorf("inspect checkpoint root: %w", err)
	}
	if clear {
		if info, err := os.Lstat(paths.dir); err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return paths, errors.New("checkpoint directory is not a regular directory")
			}
			if _, err := validateCheckpointControlTree(canonicalWork); err != nil {
				return paths, err
			}
			if err := os.RemoveAll(paths.dir); err != nil {
				return paths, fmt.Errorf("clear checkpoint directory: %w", err)
			}
		} else if !os.IsNotExist(err) {
			return paths, fmt.Errorf("inspect checkpoint directory: %w", err)
		}
	}
	if err := os.Mkdir(paths.dir, 0o755); err != nil && !os.IsExist(err) {
		return paths, fmt.Errorf("create checkpoint directory: %w", err)
	}
	return validateCheckpointControlTree(canonicalWork)
}

func validateCheckpointControlTree(canonicalWork string) (checkpointPaths, error) {
	paths, err := checkpointPathsFor(canonicalWork)
	if err != nil {
		return paths, err
	}
	for label, path := range map[string]string{
		"agent state":          paths.stateDir,
		"checkpoint root":      paths.undoDir,
		"checkpoint directory": paths.dir,
	} {
		info, err := os.Lstat(path)
		if err != nil {
			return paths, fmt.Errorf("inspect %s: %w", label, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return paths, fmt.Errorf("%s is not a regular directory", label)
		}
	}
	stateCanonical, err := filepath.EvalSymlinks(paths.stateDir)
	if err != nil {
		return paths, fmt.Errorf("canonicalize agent state: %w", err)
	}
	undoCanonical, err := filepath.EvalSymlinks(paths.undoDir)
	if err != nil {
		return paths, fmt.Errorf("canonicalize checkpoint root: %w", err)
	}
	dirCanonical, err := filepath.EvalSymlinks(paths.dir)
	if err != nil {
		return paths, fmt.Errorf("canonicalize checkpoint directory: %w", err)
	}
	if !directCheckpointChild(undoCanonical, stateCanonical, "undo") {
		return paths, errors.New("checkpoint root escaped agent state directory")
	}
	expectedName := filepath.Base(paths.dir)
	if !directCheckpointChild(dirCanonical, undoCanonical, expectedName) {
		return paths, errors.New("checkpoint directory escaped checkpoint root")
	}
	paths.stateDir = filepath.Clean(stateCanonical)
	paths.undoDir = filepath.Clean(undoCanonical)
	paths.dir = filepath.Clean(dirCanonical)
	paths.manifest = filepath.Join(paths.dir, "manifest.json")
	return paths, nil
}

func directCheckpointChild(target, base, name string) bool {
	rel, err := filepath.Rel(base, target)
	return err == nil && rel == name && filepath.Base(target) == name
}

func resolveCheckpointTarget(workDir, rel string) (string, bool, os.FileInfo, error) {
	cleanRel, err := cleanCheckpointRelative(rel)
	if err != nil {
		return "", false, nil, err
	}
	lexical := filepath.Join(workDir, cleanRel)
	if !pathWithin(lexical, workDir) {
		return "", false, nil, errors.New("target escaped workspace lexically")
	}
	_, statErr := os.Lstat(lexical)
	if statErr != nil && !os.IsNotExist(statErr) {
		return "", false, nil, statErr
	}
	canonical, err := canonicalizeTarget(lexical)
	if err != nil {
		return "", false, nil, err
	}
	if !pathWithin(canonical, workDir) {
		return "", false, nil, errors.New("target escaped workspace through a symlink")
	}
	if statErr == nil {
		resolvedInfo, err := os.Stat(canonical)
		if err != nil {
			return "", false, nil, err
		}
		return canonical, true, resolvedInfo, nil
	}
	return canonical, false, nil, nil
}

func resolveCheckpointCaptureTarget(workDir, supplied string) (string, bool, os.FileInfo, error) {
	if strings.TrimSpace(supplied) == "" {
		return "", false, nil, errors.New("checkpoint target is empty")
	}
	if err := validatePathSyntax(supplied); err != nil {
		return "", false, nil, err
	}
	resolved := supplied
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(workDir, resolved)
	}
	absPath, err := filepath.Abs(resolved)
	if err != nil {
		return "", false, nil, err
	}
	_, statErr := os.Lstat(absPath)
	if statErr != nil && !os.IsNotExist(statErr) {
		return "", false, nil, statErr
	}
	canonical, err := canonicalizeTarget(absPath)
	if err != nil {
		return "", false, nil, err
	}
	if !pathWithin(canonical, workDir) {
		return "", false, nil, errors.New("checkpoint target escaped workspace")
	}
	if statErr == nil {
		info, err := os.Stat(canonical)
		if err != nil {
			return "", false, nil, err
		}
		if !info.Mode().IsRegular() {
			return "", false, nil, errors.New("checkpoint target is not a regular file")
		}
		return canonical, true, info, nil
	}
	return canonical, false, nil, nil
}

func resolveCheckpointBackup(paths checkpointPaths, rel string) (string, error) {
	cleanRel, err := cleanCheckpointRelative(rel)
	if err != nil {
		return "", err
	}
	lexical := filepath.Join(paths.dir, cleanRel)
	if !pathWithin(lexical, paths.dir) {
		return "", errors.New("backup escaped checkpoint directory lexically")
	}
	info, err := os.Lstat(lexical)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", errors.New("backup is not a regular file")
	}
	canonical, err := filepath.EvalSymlinks(lexical)
	if err != nil {
		return "", err
	}
	if !pathWithin(canonical, paths.dir) {
		return "", errors.New("backup escaped checkpoint directory through a symlink")
	}
	if sameCheckpointPath(canonical, paths.manifest) {
		return "", errors.New("backup resolves to checkpoint manifest")
	}
	manifestInfo, err := os.Stat(paths.manifest)
	if err != nil {
		return "", fmt.Errorf("inspect checkpoint manifest: %w", err)
	}
	backupInfo, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if os.SameFile(manifestInfo, backupInfo) {
		return "", errors.New("backup aliases checkpoint manifest")
	}
	return canonical, nil
}

func cleanCheckpointRelative(rel string) (string, error) {
	if rel == "" || strings.IndexByte(rel, 0) >= 0 {
		return "", errors.New("path must be non-empty and contain no NUL byte")
	}
	if runtime.GOOS != "windows" && strings.Contains(rel, `\`) {
		return "", errors.New("backslash path separators are ambiguous on this platform")
	}
	portable := strings.ReplaceAll(rel, `\`, "/")
	if strings.HasPrefix(portable, "/") || strings.HasPrefix(portable, "//") || filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" {
		return "", errors.New("absolute, drive, and UNC paths are forbidden")
	}
	if len(portable) >= 2 && ((portable[0] >= 'A' && portable[0] <= 'Z') || (portable[0] >= 'a' && portable[0] <= 'z')) && portable[1] == ':' {
		return "", errors.New("drive-qualified paths are forbidden")
	}
	if strings.Contains(portable, ":") {
		return "", errors.New("colon-qualified paths are forbidden")
	}
	if pathpkg.Clean(portable) != portable || portable == "." || portable == ".." || strings.HasPrefix(portable, "../") {
		return "", errors.New("path must be clean and contain no traversal")
	}
	for _, component := range strings.Split(portable, "/") {
		if component == "" || component == "." || component == ".." {
			return "", errors.New("path contains an empty or traversal component")
		}
	}
	native := filepath.FromSlash(portable)
	if filepath.Clean(native) != native {
		return "", errors.New("path is not clean for the current platform")
	}
	if err := validatePathSyntax(native); err != nil {
		return "", err
	}
	return native, nil
}

func rejectProtectedCheckpointTarget(target, workDir, stateDir string) error {
	stateCanonical, err := canonicalizeTarget(stateDir)
	if err == nil && pathWithin(target, stateCanonical) {
		return errors.New("target is inside agent state")
	}
	rel, err := filepath.Rel(workDir, target)
	if err != nil {
		return err
	}
	portable := filepath.ToSlash(rel)
	first := strings.ToLower(strings.SplitN(portable, "/", 2)[0])
	switch first {
	case ".corelay", ".aniclew", ".claude-proxy", ".git", ".hg", ".svn":
		return fmt.Errorf("target uses protected control directory %q", first)
	}
	return nil
}

func isCheckpointControlRelative(rel string) bool {
	portable := filepath.ToSlash(rel)
	first := strings.SplitN(portable, "/", 2)[0]
	return strings.EqualFold(first, "manifest.json")
}

func sameCheckpointPath(left, right string) bool {
	rel, err := filepath.Rel(left, right)
	return err == nil && rel == "."
}

func checkpointPathKey(path string) string {
	key := filepath.Clean(path)
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	return key
}

func mustMarshalCheckpoint(m ckptManifest) []byte {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil
	}
	return data
}

func checkpointAtomicWrite(target string, data []byte, mode os.FileMode) (retErr error) {
	dir := filepath.Dir(target)
	temp, err := os.CreateTemp(dir, ".corelay-checkpoint-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer func() {
		_ = temp.Close()
		if retErr != nil {
			_ = os.Remove(tempName)
		}
	}()
	if err := temp.Chmod(mode); err != nil {
		return err
	}
	if _, err := temp.Write(data); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempName, target); err != nil {
		return err
	}
	return nil
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := scanCheckpointJSONValue(decoder); err != nil {
		return err
	}
	return requireJSONEOF(decoder)
}

func scanCheckpointJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON key %q", key)
			}
			seen[key] = struct{}{}
			if err := scanCheckpointJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("unterminated JSON object")
		}
	case '[':
		for decoder.More() {
			if err := scanCheckpointJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("unterminated JSON array")
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	if _, err := decoder.Token(); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("trailing JSON content")
}
