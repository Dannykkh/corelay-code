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

const checkpointManifestVersion = 4

type checkpointPaths struct {
	stateDir      string
	undoDir       string
	dir           string
	manifest      string
	sessionDigest string
}

func checkpointDir(workDir string, sessionID ...string) string {
	canonical, err := canonicalWorkspace(workDir)
	if err != nil {
		return ""
	}
	paths, err := checkpointPathsFor(canonical, checkpointSessionID(sessionID...))
	if err != nil {
		return ""
	}
	return paths.dir
}

type ckptFile struct {
	Existed           bool    `json:"existed"`
	Backup            string  `json:"backup"`               // clean relative path within the checkpoint dir
	TargetPath        string  `json:"targetPath,omitempty"` // canonical absolute path for full-mode external targets
	PreimageDigest    string  `json:"preimageDigest"`
	PreimageMode      *uint32 `json:"preimageMode,omitempty"`
	PostimageDigest   string  `json:"postimageDigest,omitempty"`
	PostimageExisted  bool    `json:"postimageExisted,omitempty"`
	PostimageCaptured bool    `json:"postimageCaptured,omitempty"`
}

type ckptManifest struct {
	Version         int                 `json:"version"`
	WorkDir         string              `json:"workDir"`
	SessionDigest   string              `json:"sessionDigest"`
	SessionRevision uint64              `json:"sessionRevision,omitempty"`
	RunID           string              `json:"runId"`
	Generation      string              `json:"generation"`
	CheckpointKey   string              `json:"checkpointKey"`
	Files           map[string]ckptFile `json:"files"` // clean workspace-relative path -> info
}

type checkpointOwner struct {
	SessionID       string
	SessionRevision uint64
	RunID           string
	Generation      string
}

func checkpointSessionID(sessionID ...string) string {
	if len(sessionID) == 0 {
		return ""
	}
	return strings.TrimSpace(sessionID[0])
}

func checkpointSessionDigest(sessionID string) string {
	return artifactBytesRevision([]byte(strings.TrimSpace(sessionID)))
}

func newCheckpointOwner(sessionID string, sessionRevision uint64, runID string) checkpointOwner {
	runID = strings.TrimSpace(runID)
	return checkpointOwner{
		SessionID:       strings.TrimSpace(sessionID),
		SessionRevision: sessionRevision,
		RunID:           runID,
		Generation:      "checkpoint_" + runID,
	}
}

func (owner checkpointOwner) valid() bool {
	return strings.TrimSpace(owner.RunID) != "" && strings.TrimSpace(owner.Generation) != ""
}

func checkpointManifestMatchesOwner(manifest ckptManifest, owner checkpointOwner) bool {
	return owner.valid() && manifest.SessionDigest == checkpointSessionDigest(owner.SessionID) &&
		manifest.SessionRevision == owner.SessionRevision && manifest.RunID == owner.RunID &&
		manifest.Generation == owner.Generation &&
		manifest.CheckpointKey == checkpointManifestKey(manifest.WorkDir, manifest.SessionDigest, manifest.RunID, manifest.Generation)
}

func checkpointManifestKey(workDir, sessionDigest, runID, generation string) string {
	identity := strings.Join([]string{
		checkpointPathKey(workDir),
		sessionDigest,
		strings.TrimSpace(runID),
		strings.TrimSpace(generation),
	}, "\x00")
	return artifactBytesRevision([]byte(identity))
}

// startCheckpoint clears this session's previous undo buffer to begin a new run generation.
func startCheckpoint(workDir string, owner checkpointOwner) error {
	if !owner.valid() {
		return errors.New("checkpoint owner is invalid")
	}
	canonical, err := canonicalWorkspace(workDir)
	if err != nil {
		return fmt.Errorf("canonicalize checkpoint workspace: %w", err)
	}
	paths, err := prepareCheckpointDirectory(canonical, owner.SessionID, true)
	if err != nil {
		return err
	}
	m := ckptManifest{
		Version:         checkpointManifestVersion,
		WorkDir:         canonical,
		SessionDigest:   paths.sessionDigest,
		SessionRevision: owner.SessionRevision,
		RunID:           owner.RunID,
		Generation:      owner.Generation,
		CheckpointKey:   checkpointManifestKey(canonical, paths.sessionDigest, owner.RunID, owner.Generation),
		Files:           map[string]ckptFile{},
	}
	if err := checkpointAtomicWrite(paths.manifest, mustMarshalCheckpoint(m), 0o644); err != nil {
		return fmt.Errorf("write checkpoint manifest: %w", err)
	}
	return nil
}

// checkpointFile backs up a file's current (pre-edit) state once per generation.
func checkpointFile(workDir, relPath, absPath string, owner checkpointOwner) error {
	if !owner.valid() {
		return errors.New("checkpoint owner is invalid")
	}
	canonicalWork, err := canonicalWorkspace(workDir)
	if err != nil {
		return fmt.Errorf("canonicalize checkpoint workspace: %w", err)
	}
	paths, err := validateCheckpointControlTree(canonicalWork, owner.SessionID)
	if err != nil {
		return err
	}
	m, _, found, err := readManifestStrict(canonicalWork, owner.SessionID)
	if err != nil || !found {
		if err != nil {
			return err
		}
		return errors.New("checkpoint manifest is unavailable")
	}
	if !checkpointManifestMatchesOwner(m, owner) {
		return errors.New("checkpoint manifest belongs to a different session or run")
	}

	canonicalTarget, targetExists, targetInfo, err := resolveCheckpointCaptureTarget(canonicalWork, relPath)
	if err != nil {
		return err
	}
	providedTarget, _, _, err := resolveCheckpointCaptureTarget(canonicalWork, absPath)
	if err != nil || !sameCheckpointPath(canonicalTarget, providedTarget) {
		if err != nil {
			return err
		}
		return errors.New("checkpoint target does not match supplied path")
	}
	if err := rejectProtectedCheckpointTarget(canonicalTarget, canonicalWork, paths.stateDir); err != nil {
		return err
	}
	storedRel, err := checkpointTargetKey(canonicalWork, canonicalTarget)
	if err != nil {
		return err
	}
	if _, done := m.Files[storedRel]; done {
		return nil
	}

	entry := ckptFile{PreimageDigest: checkpointStateRevision(targetExists, nil)}
	if !pathWithin(canonicalTarget, canonicalWork) {
		entry.TargetPath = canonicalTarget
	}
	if targetExists {
		if targetInfo == nil || !targetInfo.Mode().IsRegular() {
			return errors.New("checkpoint preimage metadata is unavailable")
		}
		data, err := os.ReadFile(canonicalTarget)
		if err != nil {
			return fmt.Errorf("read checkpoint preimage: %w", err)
		}
		entry.Existed = true
		entry.PreimageDigest = checkpointStateRevision(true, data)
		preimageMode := uint32(targetInfo.Mode().Perm())
		entry.PreimageMode = &preimageMode
		entry.Backup = hex.EncodeToString(sha1Sum(storedRel)) + ".bak"
		backupPath := filepath.Join(paths.dir, entry.Backup)
		if err := checkpointAtomicWrite(backupPath, data, 0o600); err != nil {
			return fmt.Errorf("write checkpoint preimage: %w", err)
		}
	}
	m.Files[storedRel] = entry
	if err := checkpointAtomicWrite(paths.manifest, mustMarshalCheckpoint(m), 0o644); err != nil {
		return fmt.Errorf("write checkpoint manifest: %w", err)
	}
	return nil
}

func checkpointStateRevision(exists bool, data []byte) string {
	if exists {
		return artifactBytesRevision(data)
	}
	return artifactBytesRevision([]byte("corelay-checkpoint-absent-v1"))
}

func isSHA256Revision(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	decoded, err := hex.DecodeString(value[len("sha256:"):])
	return err == nil && len(decoded) == 32 && value == strings.ToLower(value)
}

func recordCheckpointPostimages(workDir string, owner checkpointOwner, mutations []committedFileMutation) error {
	if !owner.valid() || len(mutations) == 0 {
		return errors.New("checkpoint postimage update is invalid")
	}
	canonicalWork, err := canonicalWorkspace(workDir)
	if err != nil {
		return fmt.Errorf("canonicalize checkpoint workspace: %w", err)
	}
	paths, err := validateCheckpointControlTree(canonicalWork, owner.SessionID)
	if err != nil {
		return err
	}
	manifest, _, found, err := readManifestStrict(canonicalWork, owner.SessionID)
	if err != nil {
		return err
	}
	if !found || !checkpointManifestMatchesOwner(manifest, owner) {
		return errors.New("checkpoint manifest belongs to a different session or run")
	}
	updated := make(map[string]ckptFile, len(manifest.Files))
	for rel, entry := range manifest.Files {
		updated[rel] = entry
	}
	type finalMutationPostimage struct {
		path     string
		revision string
	}
	finalPostimages := make(map[string]finalMutationPostimage, len(mutations))
	for _, mutation := range mutations {
		if !isSHA256Revision(mutation.PostRevision) {
			return errors.New("mutation postimage digest is invalid")
		}
		path, err := filepath.Abs(mutation.Snapshot.Path)
		if err != nil {
			return fmt.Errorf("resolve checkpoint postimage: %w", err)
		}
		path = filepath.Clean(path)
		canonicalTarget, err := canonicalizeTarget(path)
		if err != nil {
			return fmt.Errorf("canonicalize checkpoint postimage: %w", err)
		}
		rel, err := checkpointTargetKey(canonicalWork, canonicalTarget)
		if err != nil {
			return fmt.Errorf("invalid checkpoint postimage path: %w", err)
		}
		finalPostimages[rel] = finalMutationPostimage{path: canonicalTarget, revision: mutation.PostRevision}
	}
	keys := make([]string, 0, len(finalPostimages))
	for rel := range finalPostimages {
		keys = append(keys, rel)
	}
	sort.Strings(keys)
	for _, rel := range keys {
		postimage := finalPostimages[rel]
		entry, ok := updated[rel]
		if !ok {
			return errors.New("checkpoint preimage is missing for a successful mutation")
		}
		target, exists, info, err := resolveCheckpointEntryTarget(canonicalWork, rel, entry)
		if err != nil || !exists || info == nil || !info.Mode().IsRegular() {
			if err != nil {
				return fmt.Errorf("resolve checkpoint postimage target: %w", err)
			}
			return errors.New("checkpoint postimage target is not a regular file")
		}
		if !sameCheckpointPath(target, postimage.path) {
			return errors.New("checkpoint postimage target does not match the mutation path")
		}
		if err := rejectProtectedCheckpointTarget(target, canonicalWork, paths.stateDir); err != nil {
			return err
		}
		currentRevision, err := readLedgerFileRevision(target)
		if err != nil {
			return fmt.Errorf("read checkpoint postimage: %w", err)
		}
		if currentRevision != postimage.revision {
			return errors.New("checkpoint postimage changed before it could be recorded")
		}
		entry.PostimageDigest = postimage.revision
		entry.PostimageExisted = true
		entry.PostimageCaptured = true
		updated[rel] = entry
	}
	manifest.Files = updated
	if err := checkpointAtomicWrite(paths.manifest, mustMarshalCheckpoint(manifest), 0o644); err != nil {
		return fmt.Errorf("write checkpoint postimage manifest: %w", err)
	}
	return nil
}

// settleCheckpointPostimages settles a failed mutation batch only when the
// target is back at its captured preimage. Entries from earlier successful
// batches remain unchanged; unexpected concurrent state stays unrecorded.
func settleCheckpointPostimages(workDir string, owner checkpointOwner, mutationPaths []string) error {
	if !owner.valid() || len(mutationPaths) == 0 {
		return errors.New("checkpoint settlement is invalid")
	}
	canonicalWork, err := canonicalWorkspace(workDir)
	if err != nil {
		return fmt.Errorf("canonicalize checkpoint workspace: %w", err)
	}
	paths, err := validateCheckpointControlTree(canonicalWork, owner.SessionID)
	if err != nil {
		return err
	}
	manifest, _, found, err := readManifestStrict(canonicalWork, owner.SessionID)
	if err != nil {
		return err
	}
	if !found || !checkpointManifestMatchesOwner(manifest, owner) {
		return errors.New("checkpoint manifest belongs to a different session or run")
	}
	updated := make(map[string]ckptFile, len(manifest.Files))
	for rel, entry := range manifest.Files {
		updated[rel] = entry
	}
	seen := make(map[string]struct{}, len(mutationPaths))
	for _, mutationPath := range mutationPaths {
		target, exists, info, err := resolveCheckpointCaptureTarget(canonicalWork, mutationPath)
		if err != nil {
			return fmt.Errorf("resolve checkpoint settlement target: %w", err)
		}
		if exists && (info == nil || !info.Mode().IsRegular()) {
			return errors.New("checkpoint settlement target is not a regular file")
		}
		if err := rejectProtectedCheckpointTarget(target, canonicalWork, paths.stateDir); err != nil {
			return err
		}
		rel, err := checkpointTargetKey(canonicalWork, target)
		if err != nil {
			return fmt.Errorf("invalid checkpoint settlement path: %w", err)
		}
		if _, duplicate := seen[rel]; duplicate {
			continue
		}
		seen[rel] = struct{}{}
		entry, ok := updated[rel]
		if !ok {
			return fmt.Errorf("checkpoint preimage is missing for failed mutation %q", rel)
		}
		if entry.PostimageCaptured {
			continue
		}
		var revision string
		if exists {
			data, readErr := os.ReadFile(target)
			if readErr != nil {
				return fmt.Errorf("read checkpoint settlement postimage: %w", readErr)
			}
			revision = checkpointStateRevision(true, data)
		} else {
			revision = checkpointStateRevision(false, nil)
		}
		if exists != entry.Existed || revision != entry.PreimageDigest {
			return fmt.Errorf("checkpoint settlement target %q was not restored to its preimage", rel)
		}
		entry.PostimageDigest = revision
		entry.PostimageExisted = exists
		entry.PostimageCaptured = true
		updated[rel] = entry
	}
	manifest.Files = updated
	if err := checkpointAtomicWrite(paths.manifest, mustMarshalCheckpoint(manifest), 0o644); err != nil {
		return fmt.Errorf("write checkpoint settlement manifest: %w", err)
	}
	return nil
}

func sha1Sum(s string) []byte {
	sum := sha1.Sum([]byte(s))
	return sum[:]
}

// undoCheckpoint preserves the original helper contract. Invalid checkpoint
// state is safely treated as non-revertible; the explicit command path uses
// undoCheckpointSecure so it can tell the user that validation was refused.
func undoCheckpoint(workDir string, sessionID ...string) (reverted []string, ok bool) {
	reverted, ok, _ = undoCheckpointSecure(workDir, sessionID...)
	return reverted, ok
}

func readManifestStrict(workDir, sessionID string) (ckptManifest, checkpointPaths, bool, error) {
	canonicalWork, err := canonicalWorkspace(workDir)
	if err != nil {
		return ckptManifest{}, checkpointPaths{}, false, fmt.Errorf("canonicalize checkpoint workspace: %w", err)
	}
	paths, err := checkpointPathsFor(canonicalWork, sessionID)
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
	paths, err = validateCheckpointControlTree(canonicalWork, sessionID)
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
	if err := validateCheckpointManifestHeader(m, canonicalWork, paths); err != nil {
		return ckptManifest{}, paths, false, err
	}
	return m, paths, true, nil
}

func validateCheckpointManifestHeader(m ckptManifest, workDir string, paths checkpointPaths) error {
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
	if m.SessionDigest != paths.sessionDigest || !isSHA256Revision(m.SessionDigest) {
		return errors.New("checkpoint manifest is bound to a different session")
	}
	if strings.TrimSpace(m.RunID) == "" || strings.TrimSpace(m.Generation) == "" {
		return errors.New("checkpoint manifest run identity is incomplete")
	}
	if !isSHA256Revision(m.CheckpointKey) || m.CheckpointKey != checkpointManifestKey(workDir, m.SessionDigest, m.RunID, m.Generation) {
		return errors.New("checkpoint manifest identity key is invalid")
	}
	return nil
}

func checkpointPathsFor(canonicalWork, sessionID string) (checkpointPaths, error) {
	stateDir, err := filepath.Abs(config.BaseDir())
	if err != nil {
		return checkpointPaths{}, fmt.Errorf("resolve agent state directory: %w", err)
	}
	sessionDigest := checkpointSessionDigest(sessionID)
	sum := sha1.Sum([]byte(checkpointPathKey(canonicalWork) + "\x00" + sessionDigest))
	dir := filepath.Join(stateDir, "undo", hex.EncodeToString(sum[:10]))
	return checkpointPaths{
		stateDir:      stateDir,
		undoDir:       filepath.Join(stateDir, "undo"),
		dir:           dir,
		manifest:      filepath.Join(dir, "manifest.json"),
		sessionDigest: sessionDigest,
	}, nil
}

func prepareCheckpointDirectory(canonicalWork, sessionID string, clear bool) (checkpointPaths, error) {
	paths, err := checkpointPathsFor(canonicalWork, sessionID)
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
			if _, err := validateCheckpointControlTree(canonicalWork, sessionID); err != nil {
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
	return validateCheckpointControlTree(canonicalWork, sessionID)
}

func validateCheckpointControlTree(canonicalWork, sessionID string) (checkpointPaths, error) {
	paths, err := checkpointPathsFor(canonicalWork, sessionID)
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
	if !sameCheckpointPath(lexical, canonical) {
		return "", false, nil, errors.New("checkpoint target changed through a symlink")
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

const externalCheckpointKeyPrefix = "@external:"

func checkpointTargetKey(workDir, target string) (string, error) {
	canonical, err := canonicalizeTarget(target)
	if err != nil {
		return "", err
	}
	if pathWithin(canonical, workDir) {
		rel, err := filepath.Rel(workDir, canonical)
		if err != nil {
			return "", err
		}
		cleanRel, err := cleanCheckpointRelative(rel)
		if err != nil {
			return "", err
		}
		return cleanRel, nil
	}
	digest := strings.TrimPrefix(artifactBytesRevision([]byte(checkpointPathKey(canonical))), "sha256:")
	return externalCheckpointKeyPrefix + digest, nil
}

func isExternalCheckpointKey(key string) bool {
	if !strings.HasPrefix(key, externalCheckpointKeyPrefix) {
		return false
	}
	digest := strings.TrimPrefix(key, externalCheckpointKeyPrefix)
	if len(digest) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(digest)
	return err == nil && len(decoded) == 32 && digest == strings.ToLower(digest)
}

func resolveCheckpointEntryTarget(workDir, key string, entry ckptFile) (string, bool, os.FileInfo, error) {
	if entry.TargetPath == "" {
		if strings.HasPrefix(key, externalCheckpointKeyPrefix) {
			return "", false, nil, errors.New("external checkpoint target path is missing")
		}
		return resolveCheckpointTarget(workDir, key)
	}
	if !isExternalCheckpointKey(key) {
		return "", false, nil, errors.New("external checkpoint key is invalid")
	}
	target, exists, info, err := resolveCheckpointExternalTarget(workDir, entry.TargetPath)
	if err != nil {
		return "", false, nil, err
	}
	derivedKey, err := checkpointTargetKey(workDir, target)
	if err != nil || derivedKey != key {
		if err != nil {
			return "", false, nil, err
		}
		return "", false, nil, errors.New("external checkpoint target does not match its key")
	}
	return target, exists, info, nil
}

func resolveCheckpointExternalTarget(workDir, supplied string) (string, bool, os.FileInfo, error) {
	if strings.TrimSpace(supplied) == "" || !filepath.IsAbs(supplied) || filepath.Clean(supplied) != supplied {
		return "", false, nil, errors.New("external checkpoint target is not a clean absolute path")
	}
	if err := validatePathSyntax(supplied); err != nil {
		return "", false, nil, err
	}
	_, statErr := os.Lstat(supplied)
	if statErr != nil && !os.IsNotExist(statErr) {
		return "", false, nil, statErr
	}
	canonical, err := canonicalizeTarget(supplied)
	if err != nil {
		return "", false, nil, err
	}
	if !sameCheckpointPath(supplied, canonical) {
		return "", false, nil, errors.New("external checkpoint target changed through a symlink")
	}
	if pathWithin(canonical, workDir) {
		return "", false, nil, errors.New("external checkpoint target resolves inside the workspace")
	}
	if statErr == nil {
		info, err := os.Stat(canonical)
		if err != nil {
			return "", false, nil, err
		}
		if !info.Mode().IsRegular() {
			return "", false, nil, errors.New("external checkpoint target is not a regular file")
		}
		return canonical, true, info, nil
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
	for current := filepath.Clean(target); ; current = filepath.Dir(current) {
		component := strings.ToLower(filepath.Base(current))
		switch component {
		case ".corelay", ".aniclew", ".claude-proxy", ".git", ".hg", ".svn":
			return fmt.Errorf("target uses protected control directory %q", component)
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
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
