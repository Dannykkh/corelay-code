package agent

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	fileMutationExecutionProtocol       = "corelay.file-mutation.v1"
	legacyFileMutationExecutionProtocol = "aniclew.file-mutation.v1"
	fileMutationProofTTL                = time.Minute
)

// fileMutationPrecondition binds a dispatcher-approved Write/Edit to the
// exact artifact revision observed by ReadLedger. The value is deliberately
// private and is only recovered from a one-shot, random execution token.
type fileMutationPrecondition struct {
	Token            string
	ToolName         string
	CanonicalPath    string
	ExpectedRevision string
	ExpectAbsent     bool
	InputDigest      string
	ExpiresAt        time.Time
}

type fileMutationExecutionEnvelope struct {
	Protocol string          `json:"protocol"`
	Token    string          `json:"token"`
	Input    json.RawMessage `json:"input"`
}

var pendingFileMutationPreconditions sync.Map

type artifactMutationLockEntry struct {
	mu   sync.Mutex
	refs int
}

var artifactMutationLocks = struct {
	mu    sync.Mutex
	locks map[string]*artifactMutationLockEntry
}{locks: make(map[string]*artifactMutationLockEntry)}

func bindFileMutationExecutionInput(
	input json.RawMessage,
	toolName string,
	decision ReadLedgerDecision,
) (json.RawMessage, string, error) {
	if !decision.Allowed || strings.TrimSpace(decision.Path) == "" {
		return nil, "", errors.New("file mutation decision is not executable")
	}
	expectAbsent := decision.Code == ReadLedgerNewFile
	if decision.Code != ReadLedgerAllowed && !expectAbsent {
		return nil, "", fmt.Errorf("file mutation decision %q is not executable", decision.Code)
	}
	if toolName != "Write" && toolName != "Edit" {
		return nil, "", fmt.Errorf("tool %q cannot carry a file mutation precondition", toolName)
	}
	if toolName == "Edit" && expectAbsent {
		return nil, "", errors.New("Edit cannot target an absent artifact")
	}
	if !expectAbsent && strings.TrimSpace(decision.CurrentRevision) == "" {
		return nil, "", errors.New("file mutation revision is unavailable")
	}

	token, err := newFileMutationToken()
	if err != nil {
		return nil, "", err
	}
	cleanupExpiredFileMutationPreconditions(time.Now())
	proof := fileMutationPrecondition{
		Token:            token,
		ToolName:         toolName,
		CanonicalPath:    filepath.Clean(decision.Path),
		ExpectedRevision: decision.CurrentRevision,
		ExpectAbsent:     expectAbsent,
		InputDigest:      fileMutationInputDigest(toolName, input),
		ExpiresAt:        time.Now().Add(fileMutationProofTTL),
	}
	if _, loaded := pendingFileMutationPreconditions.LoadOrStore(token, proof); loaded {
		return nil, "", errors.New("file mutation token collision")
	}
	encoded, err := json.Marshal(fileMutationExecutionEnvelope{
		Protocol: fileMutationExecutionProtocol,
		Token:    token,
		Input:    append(json.RawMessage(nil), input...),
	})
	if err != nil {
		pendingFileMutationPreconditions.Delete(token)
		return nil, "", fmt.Errorf("encode file mutation execution: %w", err)
	}
	return encoded, token, nil
}

func unwrapFileMutationExecutionInput(
	input json.RawMessage,
	toolName string,
) (json.RawMessage, *fileMutationPrecondition, bool, error) {
	var probe struct {
		Protocol string `json:"protocol"`
	}
	if json.Unmarshal(input, &probe) != nil ||
		!renamedProtocolMatches(probe.Protocol, fileMutationExecutionProtocol, legacyFileMutationExecutionProtocol) {
		return input, nil, false, nil
	}
	var envelope fileMutationExecutionEnvelope
	if err := json.Unmarshal(input, &envelope); err != nil {
		return nil, nil, true, fmt.Errorf("decode file mutation execution: %w", err)
	}
	if strings.TrimSpace(envelope.Token) == "" || len(envelope.Input) == 0 {
		return nil, nil, true, errors.New("file mutation execution envelope is incomplete")
	}
	loaded, ok := pendingFileMutationPreconditions.LoadAndDelete(envelope.Token)
	if !ok {
		return nil, nil, true, errors.New("file mutation precondition is unknown or already consumed")
	}
	proof, ok := loaded.(fileMutationPrecondition)
	if !ok {
		return nil, nil, true, errors.New("file mutation precondition is invalid")
	}
	if !proof.ExpiresAt.After(time.Now()) {
		return nil, nil, true, errors.New("file mutation precondition expired")
	}
	if proof.Token != envelope.Token || proof.ToolName != toolName ||
		proof.InputDigest != fileMutationInputDigest(toolName, envelope.Input) {
		return nil, nil, true, errors.New("file mutation precondition is not bound to this tool input")
	}
	return append(json.RawMessage(nil), envelope.Input...), &proof, true, nil
}

func discardFileMutationPrecondition(token string) {
	if strings.TrimSpace(token) != "" {
		pendingFileMutationPreconditions.Delete(token)
	}
}

func cleanupExpiredFileMutationPreconditions(now time.Time) {
	pendingFileMutationPreconditions.Range(func(key, value any) bool {
		proof, ok := value.(fileMutationPrecondition)
		if !ok || !proof.ExpiresAt.After(now) {
			pendingFileMutationPreconditions.Delete(key)
		}
		return true
	})
}

func newFileMutationToken() (string, error) {
	var random [24]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("create file mutation token: %w", err)
	}
	return hex.EncodeToString(random[:]), nil
}

func fileMutationInputDigest(toolName string, input json.RawMessage) string {
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(toolName))
	_, _ = hasher.Write([]byte{0})
	_, _ = hasher.Write(input)
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil))
}

func mutationExecutionPath(
	rawPath string,
	workDir string,
	toolName string,
	proof *fileMutationPrecondition,
) (string, error) {
	resolved := resolvePath(rawPath, workDir)
	absolute, err := filepath.Abs(resolved)
	if err != nil {
		return "", fmt.Errorf("resolve mutation path: %w", err)
	}
	if proof == nil {
		return filepath.Clean(absolute), nil
	}
	if proof.ToolName != toolName {
		return "", errors.New("file mutation precondition targets a different tool")
	}
	canonical, err := canonicalizeTarget(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve mutation target: %w", err)
	}
	if !sameArtifactMutationPath(canonical, proof.CanonicalPath) {
		return "", errors.New("file mutation target changed after authorization")
	}
	return filepath.Clean(proof.CanonicalPath), nil
}

func validateArtifactPrecondition(path string, proof *fileMutationPrecondition) error {
	if proof == nil {
		return nil
	}
	if proof.ExpectAbsent {
		_, err := os.Lstat(path)
		switch {
		case os.IsNotExist(err):
			return nil
		case err != nil:
			return fmt.Errorf("inspect new mutation target: %w", err)
		default:
			return errors.New("target was created after authorization")
		}
	}
	current, err := readLedgerFileRevision(path)
	if err != nil {
		return fmt.Errorf("inspect mutation target revision: %w", err)
	}
	if current != proof.ExpectedRevision {
		return fmt.Errorf(
			"target changed after authorization (expected %s, found %s)",
			proof.ExpectedRevision,
			current,
		)
	}
	return nil
}

func validateExpectedArtifactState(path, expectedRevision string, expectAbsent bool) error {
	if expectAbsent {
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			return nil
		} else if err != nil {
			return fmt.Errorf("inspect expected absent artifact: %w", err)
		}
		return errors.New("artifact appeared concurrently")
	}
	current, err := readLedgerFileRevision(path)
	if err != nil {
		return fmt.Errorf("inspect expected artifact revision: %w", err)
	}
	if current != expectedRevision {
		return fmt.Errorf(
			"artifact changed concurrently (expected %s, found %s)",
			expectedRevision,
			current,
		)
	}
	return nil
}

func validateArtifactBytesPrecondition(data []byte, proof *fileMutationPrecondition) error {
	if proof == nil {
		return nil
	}
	if proof.ExpectAbsent {
		return errors.New("authorized new target unexpectedly exists")
	}
	current := artifactBytesRevision(data)
	if current != proof.ExpectedRevision {
		return fmt.Errorf(
			"target changed after authorization (expected %s, found %s)",
			proof.ExpectedRevision,
			current,
		)
	}
	return nil
}

func lockArtifactMutation(path string) func() {
	key := filepath.Clean(path)
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	artifactMutationLocks.mu.Lock()
	entry := artifactMutationLocks.locks[key]
	if entry == nil {
		entry = &artifactMutationLockEntry{}
		artifactMutationLocks.locks[key] = entry
	}
	entry.refs++
	artifactMutationLocks.mu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		artifactMutationLocks.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(artifactMutationLocks.locks, key)
		}
		artifactMutationLocks.mu.Unlock()
	}
}

func sameArtifactMutationPath(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

type stagedArtifact struct {
	target    string
	temporary string
	revision  string
	committed bool
}

func stageArtifact(target string, data []byte, mode os.FileMode) (*stagedArtifact, error) {
	dir := filepath.Dir(target)
	extension := filepath.Ext(target)
	pattern := ".corelay-mutation-*" + extension
	temp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, fmt.Errorf("create staged artifact: %w", err)
	}
	temporary := temp.Name()
	stage := &stagedArtifact{
		target:    target,
		temporary: temporary,
		revision:  artifactBytesRevision(data),
	}
	cleanup := func(cause error) (*stagedArtifact, error) {
		_ = temp.Close()
		_ = os.Remove(temporary)
		return nil, cause
	}
	if mode.Perm() == 0 {
		mode = 0o644
	}
	if err := temp.Chmod(mode.Perm()); err != nil {
		return cleanup(fmt.Errorf("set staged artifact mode: %w", err))
	}
	if _, err := temp.Write(data); err != nil {
		return cleanup(fmt.Errorf("write staged artifact: %w", err))
	}
	if err := temp.Sync(); err != nil {
		return cleanup(fmt.Errorf("sync staged artifact: %w", err))
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(temporary)
		return nil, fmt.Errorf("close staged artifact: %w", err)
	}
	return stage, nil
}

func (s *stagedArtifact) cleanup() {
	if s == nil || s.committed || strings.TrimSpace(s.temporary) == "" {
		return
	}
	_ = os.Remove(s.temporary)
}

// commit replaces an existing artifact only while it still has the exact
// baseline revision. New files use an atomic hard-link publish so a file that
// appeared after authorization is never overwritten.
func (s *stagedArtifact) commit(expectedRevision string, expectAbsent bool) error {
	if s == nil || s.target == "" || s.temporary == "" {
		return errors.New("staged artifact is incomplete")
	}
	if expectAbsent {
		if _, err := os.Lstat(s.target); err == nil {
			return errors.New("target was created before staged commit")
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect staged commit target: %w", err)
		}
		if err := os.Link(s.temporary, s.target); err != nil {
			return fmt.Errorf("publish new artifact without overwrite: %w", err)
		}
		if err := os.Remove(s.temporary); err == nil {
			s.committed = true
		}
		// When removal fails the deferred cleanup retries the temporary link;
		// the target itself is already a complete, fsynced hard link.
		return nil
	}
	current, err := readLedgerFileRevision(s.target)
	if err != nil {
		return fmt.Errorf("inspect staged commit revision: %w", err)
	}
	if current != expectedRevision {
		return fmt.Errorf(
			"target changed before staged commit (expected %s, found %s)",
			expectedRevision,
			current,
		)
	}
	if err := os.Rename(s.temporary, s.target); err != nil {
		return fmt.Errorf("replace artifact from staged content: %w", err)
	}
	s.committed = true
	return nil
}

func fileMutationBlocked(operation string, err error) (string, bool) {
	return fmt.Sprintf(
		"[MUTATION BLOCKED] The %s was not applied because %s. Re-read the file and retry against the current revision.",
		operation,
		strings.TrimSpace(err.Error()),
	), true
}

func stagedLintFailure(operation string, result LintResult) (string, bool) {
	if result.Failure == LintFailureSyntax {
		return fmt.Sprintf(
			"Your %s introduced a syntax error and was NOT applied:\n%s\n\nThe staged %s was rolled back before commit. Fix the content and try again.",
			operation,
			strings.TrimSpace(result.Message),
			operation,
		), true
	}
	return fmt.Sprintf(
		"The staged %s was rolled back before commit because syntax validation could not complete safely:\n%s",
		operation,
		strings.TrimSpace(result.Message),
	), true
}
