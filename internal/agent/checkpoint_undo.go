package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type undoAction struct {
	manifestKey      string
	rel              string
	target           string
	targetEntry      ckptFile
	parentInfo       os.FileInfo
	backup           []byte
	delete           bool
	original         []byte
	origMode         os.FileMode
	origExists       bool
	preimageDigest   string
	preimageExisted  bool
	preimageMode     os.FileMode
	postimageDigest  string
	postimageExisted bool
	status           undoEntryStatus
	preview          undoPreview
}

type undoEntryStatus string

const (
	undoEntryRestorable      undoEntryStatus = "restorable"
	undoEntryConflict        undoEntryStatus = "conflict"
	undoEntryAlreadyRestored undoEntryStatus = "already_restored"
	undoEntryExternalBlocked undoEntryStatus = "external_requires_full"
	undoEntryUnavailable     undoEntryStatus = "unavailable"
)

type undoPreview struct {
	ID     string
	Path   string
	Status undoEntryStatus
}

func undoCheckpointSecure(workDir string, sessionID ...string) (reverted []string, ok bool, retErr error) {
	return undoCheckpointSecureWithPolicy(workDir, checkpointSessionID(sessionID...), nil)
}

func undoCheckpointSecureWithPolicy(
	workDir string,
	sessionID string,
	policy *ExecutionPolicySnapshot,
) (reverted []string, ok bool, retErr error) {
	reverted, _, ok, retErr = undoCheckpointSelectedWithPolicy(workDir, sessionID, policy, nil)
	return reverted, ok, retErr
}

func inspectCheckpointUndo(workDir, sessionID string, policy *ExecutionPolicySnapshot) ([]undoPreview, error) {
	fileMutationBatchMu.Lock()
	defer fileMutationBatchMu.Unlock()
	canonicalWork, err := canonicalWorkspace(workDir)
	if err != nil {
		return nil, fmt.Errorf("canonical workspace: %w", err)
	}
	manifest, paths, found, err := readManifestStrict(canonicalWork, strings.TrimSpace(sessionID))
	if err != nil {
		return nil, err
	}
	if !found || len(manifest.Files) == 0 {
		return nil, nil
	}
	if _, err := checkpointUndoEntryIndex(manifest); err != nil {
		return nil, err
	}
	allowExternal := policy != nil && policy.Mode == ExecutionModeFull
	actions, err := preflightUndoManifestSelected(canonicalWork, paths, manifest, allowExternal, nil)
	if err != nil {
		// A single unusable entry should not hide other safe entries from the
		// selective-restore UI. Cross-entry aliases remain a manifest-level error.
		if strings.Contains(err.Error(), "manifest targets") || strings.Contains(err.Error(), "share one backup") {
			return nil, err
		}
		keys := make([]string, 0, len(manifest.Files))
		for key := range manifest.Files {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		previews := make([]undoPreview, 0, len(keys))
		for _, key := range keys {
			selected := map[string]struct{}{key: {}}
			entryActions, entryErr := preflightUndoManifestSelected(canonicalWork, paths, manifest, allowExternal, selected)
			if entryErr != nil {
				entry := manifest.Files[key]
				path := checkpointUndoDisplayPath(key, entry, allowExternal)
				if entry.TargetPath == "" {
					if _, cleanErr := cleanCheckpointRelative(key); cleanErr != nil {
						path = "<invalid checkpoint target>"
					}
				}
				previews = append(previews, undoPreview{
					ID:     checkpointUndoEntryID(manifest.CheckpointKey, key),
					Path:   path,
					Status: undoEntryUnavailable,
				})
				continue
			}
			previews = append(previews, entryActions[0].preview)
		}
		return previews, nil
	}
	previews := make([]undoPreview, 0, len(actions))
	for _, action := range actions {
		previews = append(previews, action.preview)
	}
	return previews, nil
}

// undoCheckpointSelectedWithPolicy applies an all-or-nothing restore to the
// selected checkpoint entries. A nil selection means the complete turn.
func undoCheckpointSelectedWithPolicy(
	workDir string,
	sessionID string,
	policy *ExecutionPolicySnapshot,
	selectedIDs []string,
) (reverted []string, previews []undoPreview, ok bool, retErr error) {
	// Serialize undo with dispatcher file-mutation batches so another Corelay
	// run cannot apply its own writes between the final state check and rename.
	fileMutationBatchMu.Lock()
	defer fileMutationBatchMu.Unlock()

	canonicalWork, err := canonicalWorkspace(workDir)
	if err != nil {
		return nil, nil, false, fmt.Errorf("canonical workspace: %w", err)
	}
	manifest, paths, found, err := readManifestStrict(canonicalWork, strings.TrimSpace(sessionID))
	if err != nil {
		return nil, nil, false, err
	}
	if !found || len(manifest.Files) == 0 {
		return nil, nil, false, nil
	}
	selectedKeys, err := checkpointUndoSelection(manifest, selectedIDs)
	if err != nil {
		return nil, nil, false, err
	}
	allowExternal := policy != nil && policy.Mode == ExecutionModeFull
	actions, err := preflightUndoManifestSelected(canonicalWork, paths, manifest, allowExternal, selectedKeys)
	if err != nil {
		return nil, nil, false, err
	}
	previews = make([]undoPreview, 0, len(actions))
	blocked := false
	for _, action := range actions {
		previews = append(previews, action.preview)
		blocked = blocked || action.status == undoEntryConflict || action.status == undoEntryExternalBlocked || action.status == undoEntryUnavailable
	}
	if blocked {
		return nil, previews, false, checkpointUndoConflictError(previews)
	}

	remaining := make(map[string]ckptFile, len(manifest.Files))
	for key, entry := range manifest.Files {
		remaining[key] = entry
	}
	applied := make([]undoAction, 0, len(actions))
	for _, action := range actions {
		if action.status == undoEntryAlreadyRestored {
			delete(remaining, action.manifestKey)
			continue
		}
		if action.delete {
			if err := checkpointRemoveIfUndoState(action, canonicalWork, action.postimageExisted, action.postimageDigest); err != nil {
				rollbackErr := rollbackUndoActions(applied, canonicalWork)
				return nil, previews, false, undoApplyError(fmt.Errorf("delete created file %q: %w", action.rel, err), rollbackErr)
			}
			reverted = append(reverted, action.rel+" (created → deleted)")
		} else {
			if err := checkpointAtomicWriteIfUndoState(action, canonicalWork, action.postimageExisted, action.postimageDigest, action.backup, action.preimageMode); err != nil {
				rollbackErr := rollbackUndoActions(applied, canonicalWork)
				return nil, previews, false, undoApplyError(fmt.Errorf("restore %q: %w", action.rel, err), rollbackErr)
			}
			reverted = append(reverted, action.rel)
		}
		applied = append(applied, action)
		delete(remaining, action.manifestKey)
	}

	manifest.Files = remaining
	if err := checkpointAtomicWrite(paths.manifest, mustMarshalCheckpoint(manifest), 0o644); err != nil {
		rollbackErr := rollbackUndoActions(applied, canonicalWork)
		return nil, previews, false, undoApplyError(fmt.Errorf("publish restored checkpoint entries: %w", err), rollbackErr)
	}
	if len(remaining) == 0 {
		if err := os.RemoveAll(paths.dir); err != nil {
			return reverted, previews, len(reverted) > 0, fmt.Errorf("undo completed but checkpoint cleanup failed: %w", err)
		}
	}
	return reverted, previews, len(reverted) > 0, nil
}

func checkpointUndoEntryIndex(manifest ckptManifest) (map[string]string, error) {
	ids := make(map[string]string, len(manifest.Files))
	for key := range manifest.Files {
		id := checkpointUndoEntryID(manifest.CheckpointKey, key)
		if previous, duplicate := ids[id]; duplicate {
			return nil, fmt.Errorf("checkpoint entry ID collision between %q and %q", previous, key)
		}
		ids[id] = key
	}
	return ids, nil
}

func checkpointUndoSelection(manifest ckptManifest, selectedIDs []string) (map[string]struct{}, error) {
	if selectedIDs == nil {
		return nil, nil
	}
	if len(selectedIDs) == 0 {
		return nil, errors.New("select at least one checkpoint entry ID")
	}
	ids, err := checkpointUndoEntryIndex(manifest)
	if err != nil {
		return nil, err
	}
	selected := make(map[string]struct{}, len(selectedIDs))
	for _, id := range selectedIDs {
		key, ok := ids[id]
		if !ok {
			return nil, fmt.Errorf("checkpoint entry ID %q is unknown or stale; run /undo --list again", id)
		}
		if _, duplicate := selected[key]; duplicate {
			return nil, fmt.Errorf("checkpoint entry ID %q was selected more than once", id)
		}
		selected[key] = struct{}{}
	}
	return selected, nil
}

func checkpointUndoEntryID(checkpointKey, manifestKey string) string {
	revision := artifactBytesRevision([]byte(checkpointKey + "\x00" + manifestKey))
	return strings.TrimPrefix(revision, "sha256:")[:12]
}

func checkpointUndoConflictError(previews []undoPreview) error {
	blocked := make([]string, 0, len(previews))
	for _, preview := range previews {
		if preview.Status == undoEntryConflict || preview.Status == undoEntryExternalBlocked || preview.Status == undoEntryUnavailable {
			blocked = append(blocked, preview.ID+" ("+preview.Path+")")
		}
	}
	return fmt.Errorf("no files were restored because these checkpoint entries are conflicted or unavailable: %s; inspect with /undo --list and select safe entries with /undo --select <id>", strings.Join(blocked, ", "))
}

func checkpointUndoDisplayPath(manifestKey string, entry ckptFile, allowExternal bool) string {
	if entry.TargetPath != "" {
		if !allowExternal {
			return "<external file; requires full mode>"
		}
		return entry.TargetPath
	}
	return filepath.ToSlash(manifestKey)
}

func preflightUndoManifestSelected(
	workDir string,
	paths checkpointPaths,
	manifest ckptManifest,
	allowExternal bool,
	selectedKeys map[string]struct{},
) ([]undoAction, error) {
	keys := make([]string, 0, len(manifest.Files))
	for key := range manifest.Files {
		if selectedKeys != nil {
			if _, selected := selectedKeys[key]; !selected {
				continue
			}
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	actions := make([]undoAction, 0, len(keys))
	targets := make(map[string]string)
	backups := make(map[string]string)
	absentDigest := checkpointStateRevision(false, nil)
	for _, manifestKey := range keys {
		entry := manifest.Files[manifestKey]
		if !isSHA256Revision(entry.PreimageDigest) {
			return nil, fmt.Errorf("checkpoint preimage digest for %q is invalid", manifestKey)
		}
		if !entry.PostimageCaptured || !isSHA256Revision(entry.PostimageDigest) {
			return nil, fmt.Errorf("checkpoint postimage for %q was not recorded", manifestKey)
		}
		if !entry.PostimageExisted && entry.PostimageDigest != absentDigest {
			return nil, fmt.Errorf("checkpoint postimage state for %q is inconsistent", manifestKey)
		}
		if !entry.Existed && (entry.Backup != "" || entry.PreimageDigest != absentDigest) {
			return nil, fmt.Errorf("created-file preimage for %q is inconsistent", manifestKey)
		}
		if entry.Existed {
			if entry.PreimageMode == nil || *entry.PreimageMode > 0o777 {
				return nil, fmt.Errorf("checkpoint preimage mode for %q is invalid", manifestKey)
			}
		} else if entry.PreimageMode != nil {
			return nil, fmt.Errorf("created-file preimage mode for %q is inconsistent", manifestKey)
		}
		var preimageMode os.FileMode
		if entry.PreimageMode != nil {
			preimageMode = os.FileMode(*entry.PreimageMode)
		}
		if entry.TargetPath != "" && (!isExternalCheckpointKey(manifestKey) || !filepath.IsAbs(entry.TargetPath) || filepath.Clean(entry.TargetPath) != entry.TargetPath || pathWithin(entry.TargetPath, workDir)) {
			return nil, fmt.Errorf("external checkpoint target %q is invalid", manifestKey)
		}
		action := undoAction{
			manifestKey:      manifestKey,
			targetEntry:      entry,
			preimageDigest:   entry.PreimageDigest,
			preimageExisted:  entry.Existed,
			preimageMode:     preimageMode,
			postimageDigest:  entry.PostimageDigest,
			postimageExisted: entry.PostimageExisted,
			status:           undoEntryConflict,
			preview: undoPreview{
				ID:     checkpointUndoEntryID(manifest.CheckpointKey, manifestKey),
				Path:   checkpointUndoDisplayPath(manifestKey, entry, allowExternal),
				Status: undoEntryConflict,
			},
		}
		if entry.TargetPath != "" && !allowExternal {
			action.status = undoEntryExternalBlocked
			action.preview.Status = undoEntryExternalBlocked
			actions = append(actions, action)
			continue
		}

		rel := manifestKey
		if entry.TargetPath == "" {
			var err error
			rel, err = cleanCheckpointRelative(manifestKey)
			if err != nil {
				return nil, fmt.Errorf("invalid manifest target %q: %w", manifestKey, err)
			}
		}
		target, exists, info, err := resolveCheckpointEntryTarget(workDir, rel, entry)
		if err != nil {
			return nil, fmt.Errorf("resolve manifest target %q: %w", rel, err)
		}
		if err := rejectProtectedCheckpointTarget(target, workDir, paths.stateDir); err != nil {
			return nil, fmt.Errorf("protected manifest target %q: %w", rel, err)
		}
		targetKey := checkpointPathKey(target)
		if previous, duplicate := targets[targetKey]; duplicate {
			return nil, fmt.Errorf("manifest targets %q and %q resolve to the same path", previous, rel)
		}
		targets[targetKey] = rel
		action.rel = rel
		action.target = target
		if exists && (info == nil || !info.Mode().IsRegular()) {
			actions = append(actions, action)
			continue
		}
		parentInfo, err := os.Stat(filepath.Dir(target))
		if err != nil || !parentInfo.IsDir() {
			if err == nil {
				err = errors.New("target parent is not a directory")
			}
			return nil, fmt.Errorf("inspect parent for %q: %w", rel, err)
		}
		action.parentInfo = parentInfo
		if exists {
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
			if checkpointStateRevision(true, action.backup) != entry.PreimageDigest {
				return nil, fmt.Errorf("checkpoint backup for %q does not match its preimage digest", rel)
			}
		}

		currentDigest := checkpointStateRevision(exists, action.original)
		switch {
		case exists == entry.Existed && currentDigest == entry.PreimageDigest:
			action.status = undoEntryAlreadyRestored
		case exists == entry.PostimageExisted && currentDigest == entry.PostimageDigest:
			action.status = undoEntryRestorable
			action.delete = !entry.Existed
		default:
			action.status = undoEntryConflict
		}
		action.preview.Status = action.status
		actions = append(actions, action)
	}
	return actions, nil
}

func undoActionStateMatches(action undoAction, exists bool, revision string) bool {
	info, err := os.Lstat(action.target)
	actualExists := err == nil
	if (err != nil && !os.IsNotExist(err)) || actualExists != exists {
		return false
	}
	if actualExists && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return false
	}
	var data []byte
	if actualExists {
		data, err = os.ReadFile(action.target)
		if err != nil {
			return false
		}
	}
	return checkpointStateRevision(actualExists, data) == revision
}

func rollbackUndoActions(actions []undoAction, workDir string) error {
	var failures []string
	for index := len(actions) - 1; index >= 0; index-- {
		action := actions[index]
		if undoActionStateMatches(action, action.postimageExisted, action.postimageDigest) {
			continue
		}
		if !undoActionStateMatches(action, action.preimageExisted, action.preimageDigest) {
			failures = append(failures, fmt.Sprintf("%s changed while rolling back", action.rel))
			continue
		}
		if action.origExists {
			if err := checkpointAtomicWriteIfUndoState(action, workDir, action.preimageExisted, action.preimageDigest, action.original, action.origMode); err != nil {
				failures = append(failures, fmt.Sprintf("%s: %v", action.rel, err))
			}
		} else if err := checkpointRemoveIfUndoState(action, workDir, action.preimageExisted, action.preimageDigest); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", action.rel, err))
		}
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}

// checkpointAtomicWriteIfUndoState stages the replacement first, then repeats
// target, parent identity, existence, and digest validation immediately before
// the rename. This narrows the path-based TOCTOU window while the dispatcher
// mutex serializes cooperating Corelay mutations.
func checkpointAtomicWriteIfUndoState(
	action undoAction,
	workDir string,
	expectedExists bool,
	expectedDigest string,
	data []byte,
	mode os.FileMode,
) (retErr error) {
	return checkpointAtomicWriteIfUndoStateWithHook(action, workDir, expectedExists, expectedDigest, data, mode, nil)
}

func checkpointAtomicWriteIfUndoStateWithHook(
	action undoAction,
	workDir string,
	expectedExists bool,
	expectedDigest string,
	data []byte,
	mode os.FileMode,
	afterInitialCheck func(),
) (retErr error) {
	if err := verifyUndoActionState(action, workDir, expectedExists, expectedDigest); err != nil {
		return err
	}
	if afterInitialCheck != nil {
		afterInitialCheck()
	}
	temp, err := os.CreateTemp(filepath.Dir(action.target), ".corelay-checkpoint-*")
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
	if _, err := temp.Write(data); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Chmod(mode); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := verifyUndoActionState(action, workDir, expectedExists, expectedDigest); err != nil {
		return err
	}
	if err := os.Rename(tempName, action.target); err != nil {
		return err
	}
	return nil
}

func checkpointRemoveIfUndoState(action undoAction, workDir string, expectedExists bool, expectedDigest string) error {
	if err := verifyUndoActionState(action, workDir, expectedExists, expectedDigest); err != nil {
		return err
	}
	if err := os.Remove(action.target); err != nil {
		return err
	}
	return nil
}

func verifyUndoActionState(action undoAction, workDir string, expectedExists bool, expectedDigest string) error {
	target, _, _, err := resolveCheckpointEntryTarget(workDir, action.manifestKey, action.targetEntry)
	if err != nil {
		return fmt.Errorf("target path changed after undo preflight: %w", err)
	}
	if !sameCheckpointPath(target, action.target) {
		return errors.New("target path changed after undo preflight")
	}
	parentInfo, err := os.Stat(filepath.Dir(action.target))
	if err != nil || action.parentInfo == nil || !os.SameFile(parentInfo, action.parentInfo) {
		if err != nil {
			return fmt.Errorf("target parent changed after undo preflight: %w", err)
		}
		return errors.New("target parent changed after undo preflight")
	}
	if !undoActionStateMatches(action, expectedExists, expectedDigest) {
		return errors.New("target changed after undo preflight")
	}
	return nil
}

func undoApplyError(operationErr, rollbackErr error) error {
	if rollbackErr != nil {
		return fmt.Errorf("%w; rollback was incomplete: %v", operationErr, rollbackErr)
	}
	return operationErr
}
