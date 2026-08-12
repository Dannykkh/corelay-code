package agent

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	// ErrInvalidSessionID reports an ID outside the supported opaque or legacy
	// formats. Callers may use errors.Is to map it to a client error.
	ErrInvalidSessionID = errors.New("invalid session id")
	// ErrSessionNotFound reports that no persisted session has the requested ID.
	ErrSessionNotFound = errors.New("session not found")
	// ErrSessionConflict reports an ID collision or an ambiguous persisted ID.
	ErrSessionConflict = errors.New("session id conflict")
	// ErrSessionRevisionConflict reports a stale optimistic save.
	ErrSessionRevisionConflict = errors.New("session revision conflict")
	// ErrSessionRevisionOverflow reports a persisted revision that cannot be
	// incremented without wrapping.
	ErrSessionRevisionOverflow = errors.New("session revision overflow")
	// ErrSessionCorrupt reports malformed, truncated, or internally invalid
	// persisted session data.
	ErrSessionCorrupt = errors.New("session data corrupt")
	// ErrSessionVersionUnsupported reports a syntactically valid session using a
	// schema version this binary cannot safely interpret.
	ErrSessionVersionUnsupported = errors.New("session version unsupported")
	// ErrSessionParentInvalid reports inconsistent optional fork lineage fields.
	ErrSessionParentInvalid = errors.New("invalid session parent metadata")
	// ErrSessionLifecycleInvalid reports an invalid or forbidden lifecycle transition.
	ErrSessionLifecycleInvalid = errors.New("invalid session lifecycle state")
	// ErrSessionReconcileRequired prevents resume-like mutations while an
	// interrupted tool side effect still requires explicit reconciliation.
	ErrSessionReconcileRequired = errors.New("session reconciliation required")
	// ErrSessionTerminalInvalid reports malformed content-free run terminal
	// metadata. Terminal state is committed with the transcript revision it
	// describes and must never be accepted as an unvalidated side channel.
	ErrSessionTerminalInvalid = errors.New("invalid session terminal metadata")
)

var (
	legacySessionIDPattern    = regexp.MustCompile(`^sess_[0-9]{8}-[0-9]{6}$`)
	opaqueSessionIDPattern    = regexp.MustCompile(`^sess_[a-z2-7]{26}$`)
	sessionInputDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

const (
	maxSessionIDAttempts  = 16
	currentSessionVersion = 1
)

// SessionRevisionConflictError exposes both sides of an optimistic write
// conflict while remaining compatible with errors.Is.
type SessionRevisionConflictError struct {
	SessionID string
	Expected  uint64
	Current   uint64
}

func (e *SessionRevisionConflictError) Error() string {
	return fmt.Sprintf(
		"%v: session %s expected revision %d, current revision %d",
		ErrSessionRevisionConflict,
		e.SessionID,
		e.Expected,
		e.Current,
	)
}

func (e *SessionRevisionConflictError) Unwrap() error {
	return ErrSessionRevisionConflict
}

// SessionRecoveryMetadata points to the untouched source and a safe,
// content-addressed prospective quarantine path under the session store.
type SessionRecoveryMetadata struct {
	SessionID             string                 `json:"sessionId,omitempty"`
	SourcePath            string                 `json:"sourcePath"`
	QuarantinePath        string                 `json:"quarantinePath,omitempty"`
	Quarantined           bool                   `json:"quarantined"`
	Kind                  string                 `json:"kind"`
	LifecycleStatus       SessionLifecycleStatus `json:"lifecycleStatus"`
	LastCommittedRevision uint64                 `json:"lastCommittedRevision,omitempty"`
}

// SessionRecoveryError carries typed recovery information without deleting or
// replacing the persisted source bytes.
type SessionRecoveryError struct {
	Kind     error
	Cause    error
	Recovery SessionRecoveryMetadata
}

func (e *SessionRecoveryError) Error() string {
	if e.Cause == nil {
		return e.Kind.Error()
	}
	return fmt.Sprintf("%v: %v", e.Kind, e.Cause)
}

func (e *SessionRecoveryError) Unwrap() error {
	return e.Kind
}

// SessionRecoveryFromError extracts recovery metadata from a typed load error.
func SessionRecoveryFromError(err error) (SessionRecoveryMetadata, bool) {
	var recoveryErr *SessionRecoveryError
	if !errors.As(err, &recoveryErr) {
		return SessionRecoveryMetadata{}, false
	}
	return recoveryErr.Recovery, true
}

// SessionLifecycleStatus describes whether a persisted session can be safely
// continued without first reconciling an uncertain external side effect.
type SessionLifecycleStatus string

const (
	SessionLifecycleActive         SessionLifecycleStatus = "active"
	SessionLifecycleInterrupted    SessionLifecycleStatus = "interrupted"
	SessionLifecycleClosed         SessionLifecycleStatus = "closed"
	SessionLifecycleRecoveryNeeded SessionLifecycleStatus = "recovery-needed"
)

// SessionInterruption contains identifiers and a reason only. Raw tool input
// and output must not be persisted in this marker.
type SessionInterruption struct {
	At              time.Time              `json:"at"`
	Reason          string                 `json:"reason,omitempty"`
	RunID           string                 `json:"runId,omitempty"`
	ToolName        string                 `json:"toolName,omitempty"`
	ToolCallID      string                 `json:"toolCallId,omitempty"`
	InputDigest     string                 `json:"inputDigest,omitempty"`
	SideEffectState SessionSideEffectState `json:"sideEffectState,omitempty"`
	Summary         string                 `json:"summary,omitempty"`
}

// AggregateToolExecutionJournal reduces one run's content-free execution
// starts to the same schedule-independent aggregate identity used by the
// durable event observer. A single entry remains exact; multiple entries are
// represented by a sorted digest and quarantine the whole run.
func AggregateToolExecutionJournal(
	entries []ToolExecutionJournalEntry,
	at time.Time,
	reason string,
) (SessionInterruption, error) {
	if len(entries) == 0 {
		return SessionInterruption{}, fmt.Errorf("%w: execution journal is empty", ErrSessionLifecycleInvalid)
	}
	type digestState struct {
		ToolCallID  string                 `json:"toolCallId"`
		ToolName    string                 `json:"toolName"`
		InputDigest string                 `json:"inputDigest"`
		SideEffect  SessionSideEffectState `json:"sideEffectState"`
	}
	states := make([]digestState, 0, len(entries))
	runID := strings.TrimSpace(entries[0].RunID)
	for _, entry := range entries {
		toolCallID := strings.TrimSpace(entry.ID)
		toolName := strings.TrimSpace(entry.Name)
		entryRunID := strings.TrimSpace(entry.RunID)
		if toolCallID == "" || toolName == "" || entryRunID == "" {
			return SessionInterruption{}, fmt.Errorf("%w: execution journal identity is incomplete", ErrSessionLifecycleInvalid)
		}
		if entryRunID != runID {
			return SessionInterruption{}, fmt.Errorf("%w: execution journal run id changed", ErrSessionLifecycleInvalid)
		}
		state := digestState{
			ToolCallID:  toolCallID,
			ToolName:    toolName,
			InputDigest: strings.TrimSpace(entry.InputDigest),
			SideEffect:  SessionSideEffectStarted,
		}
		probe := SessionInterruption{
			RunID: runID, ToolName: state.ToolName, ToolCallID: state.ToolCallID,
			InputDigest: state.InputDigest, SideEffectState: state.SideEffect, Summary: "execution journal",
		}
		if err := ValidateSessionInterruption(probe); err != nil {
			return SessionInterruption{}, err
		}
		states = append(states, state)
	}
	sort.SliceStable(states, func(left, right int) bool {
		if states[left].ToolCallID != states[right].ToolCallID {
			return states[left].ToolCallID < states[right].ToolCallID
		}
		if states[left].InputDigest != states[right].InputDigest {
			return states[left].InputDigest < states[right].InputDigest
		}
		return states[left].ToolName < states[right].ToolName
	})
	marker := SessionInterruption{
		At: at, Reason: strings.TrimSpace(reason), RunID: runID,
		SideEffectState: SessionSideEffectStarted,
	}
	if len(states) == 1 {
		marker.ToolCallID = states[0].ToolCallID
		marker.ToolName = states[0].ToolName
		marker.InputDigest = states[0].InputDigest
		marker.Summary = "Authorized tool execution started; marker quarantines the entire run."
	} else {
		encoded, _ := json.Marshal(states)
		digest := sha256.Sum256(encoded)
		marker.ToolCallID = "multiple"
		marker.ToolName = "multiple_tools"
		marker.InputDigest = fmt.Sprintf("sha256:%x", digest)
		marker.SideEffectState = SessionSideEffectMayHaveApplied
		marker.Summary = fmt.Sprintf(
			"Agent run ended after %d tool executions began; reconcile every side effect represented by the aggregate digest before resuming.",
			len(states),
		)
	}
	if err := ValidateSessionInterruption(marker); err != nil {
		return SessionInterruption{}, err
	}
	return marker, nil
}

// SessionSideEffectState records only the bounded execution state needed to
// reconcile an interrupted tool. It deliberately carries no input or output.
type SessionSideEffectState string

const (
	SessionSideEffectUnknown        SessionSideEffectState = "unknown"
	SessionSideEffectStarted        SessionSideEffectState = "started"
	SessionSideEffectMayHaveApplied SessionSideEffectState = "may_have_applied"
	SessionSideEffectApplied        SessionSideEffectState = "applied"
)

// Session represents a saved chat conversation.
type Session struct {
	Version               int                         `json:"version,omitempty"`
	Revision              uint64                      `json:"revision,omitempty"`
	ID                    string                      `json:"id"`
	Title                 string                      `json:"title"`
	Workspace             string                      `json:"workspace"` // project directory path
	Messages              []SessionMessage            `json:"messages"`
	Provider              string                      `json:"provider"`
	Model                 string                      `json:"model"`
	CreatedAt             time.Time                   `json:"createdAt"`
	UpdatedAt             time.Time                   `json:"updatedAt"`
	Turns                 int                         `json:"turns"`
	ParentSessionID       string                      `json:"parentSessionId,omitempty"`
	ParentRevision        uint64                      `json:"parentRevision,omitempty"`
	LifecycleStatus       SessionLifecycleStatus      `json:"lifecycleStatus,omitempty"`
	LastCommittedRevision uint64                      `json:"lastCommittedRevision,omitempty"`
	ReconcileRequired     bool                        `json:"reconcileRequired,omitempty"`
	Interruption          *SessionInterruption        `json:"interruption,omitempty"`
	LastRunTerminal       *DurableRunTerminalMetadata `json:"lastRunTerminal,omitempty"`
}

// SessionMessage is a message in a session (user, assistant, or tool).
type SessionMessage struct {
	Role       string      `json:"role"` // "user", "assistant", "tool"
	Content    string      `json:"content"`
	ToolName   string      `json:"toolName,omitempty"`
	ToolInput  interface{} `json:"toolInput,omitempty"`
	ToolResult string      `json:"toolResult,omitempty"`
	IsError    bool        `json:"isError,omitempty"`
	Timestamp  time.Time   `json:"timestamp"`
}

// SessionSummary is a lightweight view for listing sessions.
type SessionSummary struct {
	Version               int                         `json:"version,omitempty"`
	Revision              uint64                      `json:"revision,omitempty"`
	ID                    string                      `json:"id"`
	Title                 string                      `json:"title"`
	Preview               string                      `json:"preview"`
	Workspace             string                      `json:"workspace"`
	Turns                 int                         `json:"turns"`
	Provider              string                      `json:"provider"`
	Model                 string                      `json:"model"`
	UpdatedAt             time.Time                   `json:"updatedAt"`
	ParentSessionID       string                      `json:"parentSessionId,omitempty"`
	ParentRevision        uint64                      `json:"parentRevision,omitempty"`
	LifecycleStatus       SessionLifecycleStatus      `json:"lifecycleStatus,omitempty"`
	LastCommittedRevision uint64                      `json:"lastCommittedRevision,omitempty"`
	ReconcileRequired     bool                        `json:"reconcileRequired,omitempty"`
	LastRunTerminal       *DurableRunTerminalMetadata `json:"lastRunTerminal,omitempty"`
}

// Workspace represents a project directory.
type Workspace struct {
	Path     string `json:"path"`
	Name     string `json:"name"`     // folder name
	Sessions int    `json:"sessions"` // number of sessions
}

// SessionStore manages session persistence.
type SessionStore struct {
	mu          *sync.RWMutex
	baseDir     string
	dir         string
	idGenerator func() (string, error)
	initErr     error
}

var sessionStoreLocks sync.Map

func NewSessionStore(baseDir string) *SessionStore {
	storageBase := filepath.Clean(baseDir)
	if absolute, err := filepath.Abs(storageBase); err == nil {
		storageBase = absolute
	}
	dir := filepath.Join(storageBase, "sessions")
	err := os.MkdirAll(dir, 0755)
	lockKey := filepath.Clean(dir)
	if absolute, absErr := filepath.Abs(lockKey); absErr == nil {
		lockKey = absolute
	}
	if resolved, resolveErr := filepath.EvalSymlinks(lockKey); resolveErr == nil {
		lockKey = filepath.Clean(resolved)
	}
	if runtime.GOOS == "windows" {
		lockKey = strings.ToLower(lockKey)
	}
	lockValue, _ := sessionStoreLocks.LoadOrStore(lockKey, &sync.RWMutex{})
	return &SessionStore{
		mu:          lockValue.(*sync.RWMutex),
		baseDir:     storageBase,
		dir:         dir,
		idGenerator: generatePersistedSessionID,
		initErr:     err,
	}
}

// OpenToolResultMemory binds a result store to one exact durable session and
// canonical workspace digest. It returns only references that appear in the
// committed transcript and whose blobs verify in that namespace.
func (s *SessionStore) OpenToolResultMemory(
	id string,
	expectedWorkspace string,
) (*SessionMemory, []ToolResultReference, error) {
	if err := ValidateSessionID(id); err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(expectedWorkspace) == "" {
		return nil, nil, fmt.Errorf("tool result workspace is required")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.initErr != nil {
		return nil, nil, fmt.Errorf("initialize session store: %w", s.initErr)
	}
	session, err := s.getLocked(id)
	if err != nil {
		return nil, nil, err
	}
	if !sameWorkspace(session.Workspace, expectedWorkspace) {
		return nil, nil, fmt.Errorf("%w: session %s belongs to another workspace", ErrSessionConflict, id)
	}
	memory, err := s.openToolResultMemoryForSessionLocked(*session)
	if err != nil {
		return nil, nil, err
	}
	return memory, memory.ReferencesForMessages(session.Messages), nil
}

func (s *SessionStore) openToolResultMemoryForSessionLocked(session Session) (*SessionMemory, error) {
	workspaceKey, _, err := workspaceStorageKey(session.Workspace)
	if err != nil {
		return nil, err
	}
	if err := ValidateSessionID(session.ID); err != nil {
		return nil, err
	}
	label := workspaceKey + ":" + session.ID
	memory, err := OpenSessionMemory(s.baseDir, label)
	if err != nil {
		return nil, fmt.Errorf("open durable tool result store: %w", err)
	}
	return memory, nil
}

// workspaceDir returns the canonical digest directory for a workspace,
// creating it if needed. Workspace paths are never embedded in directory names:
// the legacy separator replacement was lossy (for example D:\a\b and
// D:\a--b collided).
func (s *SessionStore) workspaceDir(workspace string) (string, error) {
	key, _, err := workspaceStorageKey(workspace)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(s.dir, key)
	if err := ensureLexicallyContained(s.dir, dir); err != nil {
		return "", fmt.Errorf("workspace directory: %w", err)
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("create workspace directory: %w", err)
	}
	if err := ensureResolvedDirectoryContained(s.dir, dir); err != nil {
		return "", fmt.Errorf("workspace directory: %w", err)
	}
	return dir, nil
}

func workspaceStorageKey(workspace string) (string, string, error) {
	canonical, err := canonicalWorkspacePath(workspace)
	if err != nil {
		return "", "", err
	}
	identity := canonical
	if runtime.GOOS == "windows" {
		identity = strings.ToLower(identity)
	}
	sum := sha256.Sum256([]byte(identity))
	return "ws_" + hex.EncodeToString(sum[:]), canonical, nil
}

func canonicalWorkspacePath(workspace string) (string, error) {
	if strings.TrimSpace(workspace) == "" {
		return "", fmt.Errorf("workspace path is empty")
	}
	absolute, err := filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("resolve workspace path: %w", err)
	}
	canonical := filepath.Clean(absolute)
	if resolved, resolveErr := filepath.EvalSymlinks(canonical); resolveErr == nil {
		canonical = filepath.Clean(resolved)
	}
	return canonical, nil
}

// legacyWorkspacePath reproduces the pre-digest directory name for read and
// migration compatibility. Callers must validate the Workspace field of every
// session loaded from this path because distinct workspaces may share it.
func (s *SessionStore) legacyWorkspacePath(workspace string) (string, error) {
	safe := strings.ReplaceAll(workspace, ":", "")
	safe = strings.ReplaceAll(safe, "\\", "--")
	safe = strings.ReplaceAll(safe, "/", "--")
	dir := filepath.Join(s.dir, safe)
	if err := ensureLexicallyContained(s.dir, dir); err != nil {
		return "", fmt.Errorf("legacy workspace directory: %w", err)
	}
	return dir, nil
}

// ValidateSessionID accepts IDs produced by the current cryptographic
// generator and the timestamp IDs emitted by earlier Corelay Code versions
// (then named AniClew).
func ValidateSessionID(id string) error {
	if opaqueSessionIDPattern.MatchString(id) || legacySessionIDPattern.MatchString(id) {
		return nil
	}
	return fmt.Errorf("%w: %q", ErrInvalidSessionID, id)
}

func generatePersistedSessionID() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(random)
	return "sess_" + strings.ToLower(encoded), nil
}

func ensureLexicallyContained(base, target string) error {
	baseAbs, err := filepath.Abs(base)
	if err != nil {
		return err
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(baseAbs, targetAbs)
	if err != nil {
		return err
	}
	if rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("path escapes session store")
	}
	return nil
}

func ensureResolvedDirectoryContained(base, dir string) error {
	if err := ensureLexicallyContained(base, dir); err != nil {
		return err
	}
	baseReal, err := filepath.EvalSymlinks(base)
	if err != nil {
		return err
	}
	dirReal, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	return ensureLexicallyContained(baseReal, dirReal)
}

func (s *SessionStore) sessionPath(dir, id string) (string, error) {
	if err := ValidateSessionID(id); err != nil {
		return "", err
	}
	if err := ensureResolvedDirectoryContained(s.dir, dir); err != nil {
		return "", err
	}
	path := filepath.Join(dir, id+".json")
	if err := ensureLexicallyContained(s.dir, path); err != nil {
		return "", err
	}
	return path, nil
}

type storedSession struct {
	path    string
	session Session
}

func (s *SessionStore) loadStoredSession(path, expectedID string) (Session, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return Session{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return Session{}, s.sessionRecoveryError(
			ErrSessionCorrupt,
			path,
			expectedID,
			nil,
			fmt.Errorf("session file is not regular"),
		)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Session{}, err
	}

	var envelope struct {
		Version               int    `json:"version"`
		Revision              uint64 `json:"revision"`
		LastCommittedRevision uint64 `json:"lastCommittedRevision"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return Session{}, s.sessionRecoveryError(
			ErrSessionCorrupt,
			path,
			expectedID,
			data,
			fmt.Errorf("decode session envelope: %w", err),
		)
	}
	if envelope.Version != 0 && envelope.Version != currentSessionVersion {
		return Session{}, s.sessionRecoveryError(
			ErrSessionVersionUnsupported,
			path,
			expectedID,
			data,
			fmt.Errorf("schema version %d", envelope.Version),
		)
	}

	var sess Session
	if err := json.Unmarshal(data, &sess); err != nil {
		return Session{}, s.sessionRecoveryError(
			ErrSessionCorrupt,
			path,
			expectedID,
			data,
			fmt.Errorf("decode session: %w", err),
		)
	}
	if sess.Version == 0 {
		if sess.Revision != 0 || sess.ParentSessionID != "" || sess.ParentRevision != 0 ||
			sess.LifecycleStatus != "" || sess.LastCommittedRevision != 0 ||
			sess.ReconcileRequired || sess.Interruption != nil || sess.LastRunTerminal != nil {
			return Session{}, s.sessionRecoveryError(
				ErrSessionCorrupt,
				path,
				expectedID,
				data,
				errors.New("legacy session contains versioned fields"),
			)
		}
	} else if sess.Revision == 0 {
		return Session{}, s.sessionRecoveryError(
			ErrSessionCorrupt,
			path,
			expectedID,
			data,
			errors.New("versioned session has revision zero"),
		)
	}
	if err := normalizeSessionLifecycle(&sess); err != nil {
		return Session{}, s.sessionRecoveryError(ErrSessionCorrupt, path, expectedID, data, err)
	}
	if err := validateOptionalDurableRunTerminal(sess.LastRunTerminal); err != nil {
		return Session{}, s.sessionRecoveryError(ErrSessionCorrupt, path, expectedID, data, err)
	}
	if err := validateSessionParentMetadata(sess.ID, sess.ParentSessionID, sess.ParentRevision); err != nil {
		return Session{}, s.sessionRecoveryError(ErrSessionCorrupt, path, expectedID, data, err)
	}
	if err := ValidateSessionID(sess.ID); err != nil {
		return Session{}, s.sessionRecoveryError(ErrSessionCorrupt, path, expectedID, data, err)
	}
	if expectedID != "" && sess.ID != expectedID {
		return Session{}, s.sessionRecoveryError(
			ErrSessionCorrupt,
			path,
			expectedID,
			data,
			fmt.Errorf("session %q contains mismatched id %q", expectedID, sess.ID),
		)
	}
	if strings.TrimSpace(sess.Workspace) == "" {
		return Session{}, s.sessionRecoveryError(
			ErrSessionCorrupt,
			path,
			sess.ID,
			data,
			fmt.Errorf("session %q has no workspace", sess.ID),
		)
	}
	if err := s.validateSessionWorkspacePath(path, sess.Workspace); err != nil {
		return Session{}, s.sessionRecoveryError(
			ErrSessionCorrupt,
			path,
			sess.ID,
			data,
			fmt.Errorf("session %q: %w", sess.ID, err),
		)
	}
	return sess, nil
}

func validateSessionParentMetadata(sessionID, parentID string, parentRevision uint64) error {
	if parentID == "" && parentRevision == 0 {
		return nil
	}
	if parentID == "" || parentRevision == 0 {
		return fmt.Errorf("%w: parent id and revision must be set together", ErrSessionParentInvalid)
	}
	if err := ValidateSessionID(parentID); err != nil {
		return fmt.Errorf("%w: %v", ErrSessionParentInvalid, err)
	}
	if sessionID != "" && parentID == sessionID {
		return fmt.Errorf("%w: session cannot parent itself", ErrSessionParentInvalid)
	}
	return nil
}

func normalizeSessionLifecycle(sess *Session) error {
	if sess == nil {
		return fmt.Errorf("%w: session is nil", ErrSessionLifecycleInvalid)
	}
	if sess.Version == 0 {
		sess.LifecycleStatus = SessionLifecycleActive
		return nil
	}
	if sess.LifecycleStatus == "" {
		if sess.LastCommittedRevision != 0 || sess.ReconcileRequired || sess.Interruption != nil {
			return fmt.Errorf("%w: lifecycle metadata is incomplete", ErrSessionLifecycleInvalid)
		}
		sess.LifecycleStatus = SessionLifecycleActive
		sess.LastCommittedRevision = sess.Revision
		return nil
	}
	if sess.LastCommittedRevision > sess.Revision {
		return fmt.Errorf("%w: last committed revision exceeds current revision", ErrSessionLifecycleInvalid)
	}
	switch sess.LifecycleStatus {
	case SessionLifecycleActive, SessionLifecycleClosed:
		if sess.ReconcileRequired || sess.Interruption != nil || sess.LastCommittedRevision != sess.Revision {
			return fmt.Errorf("%w: stable lifecycle metadata is inconsistent", ErrSessionLifecycleInvalid)
		}
	case SessionLifecycleInterrupted:
		if !sess.ReconcileRequired || sess.Interruption == nil || sess.Interruption.At.IsZero() ||
			sess.LastCommittedRevision >= sess.Revision {
			return fmt.Errorf("%w: interrupted lifecycle metadata is inconsistent", ErrSessionLifecycleInvalid)
		}
		if err := ValidateSessionInterruption(*sess.Interruption); err != nil {
			return err
		}
	case SessionLifecycleRecoveryNeeded:
		if !sess.ReconcileRequired {
			return fmt.Errorf("%w: recovery-needed session must require reconciliation", ErrSessionLifecycleInvalid)
		}
		if sess.Interruption != nil {
			if err := ValidateSessionInterruption(*sess.Interruption); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("%w: unknown status %q", ErrSessionLifecycleInvalid, sess.LifecycleStatus)
	}
	return nil
}

// ValidateDurableRunTerminalMetadata verifies the bounded, content-free state
// that may be committed beside a durable transcript. Completion counts must be
// internally consistent so a corrupt or forged "complete" marker fails closed.
func ValidateDurableRunTerminalMetadata(value DurableRunTerminalMetadata) error {
	if value == (DurableRunTerminalMetadata{}) {
		return fmt.Errorf("%w: terminal metadata is empty", ErrSessionTerminalInvalid)
	}
	if value.TerminalState != "" && durableRunTerminalState(value.TerminalState) == "" {
		return fmt.Errorf("%w: unknown terminal state %q", ErrSessionTerminalInvalid, value.TerminalState)
	}
	if value.CompletionStatus == "" {
		if value.CompletionRevision != 0 || value.CompletionCriteria != 0 ||
			value.CompletionSatisfied != 0 || value.CompletionBlocked != 0 {
			return fmt.Errorf("%w: completion fields require a completion status", ErrSessionTerminalInvalid)
		}
		return nil
	}
	if value.TerminalState == "" {
		return fmt.Errorf("%w: completion status requires a terminal state", ErrSessionTerminalInvalid)
	}
	if !validDurableRunCompletionCount(value.CompletionCriteria) || value.CompletionCriteria == 0 ||
		!validDurableRunCompletionCount(value.CompletionSatisfied) ||
		!validDurableRunCompletionCount(value.CompletionBlocked) ||
		value.CompletionSatisfied+value.CompletionBlocked > value.CompletionCriteria {
		return fmt.Errorf("%w: completion counts are inconsistent", ErrSessionTerminalInvalid)
	}
	switch value.CompletionStatus {
	case CompletionStatusComplete:
		if value.CompletionRevision == 0 || value.CompletionSatisfied != value.CompletionCriteria ||
			value.CompletionBlocked != 0 || value.TerminalState == EvidenceTerminalBlocked {
			return fmt.Errorf("%w: complete status is inconsistent", ErrSessionTerminalInvalid)
		}
	case CompletionStatusIncomplete:
		if value.CompletionSatisfied >= value.CompletionCriteria || value.CompletionBlocked != 0 ||
			value.TerminalState != EvidenceTerminalBlocked {
			return fmt.Errorf("%w: incomplete status is inconsistent", ErrSessionTerminalInvalid)
		}
	case CompletionStatusBlocked:
		if value.CompletionBlocked == 0 || value.TerminalState != EvidenceTerminalBlocked {
			return fmt.Errorf("%w: blocked status is inconsistent", ErrSessionTerminalInvalid)
		}
	default:
		return fmt.Errorf("%w: unknown completion status %q", ErrSessionTerminalInvalid, value.CompletionStatus)
	}
	return nil
}

func validateOptionalDurableRunTerminal(value *DurableRunTerminalMetadata) error {
	if value == nil {
		return nil
	}
	return ValidateDurableRunTerminalMetadata(*value)
}

func cloneDurableRunTerminalMetadata(value *DurableRunTerminalMetadata) *DurableRunTerminalMetadata {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func (s *SessionStore) sessionRecoveryError(
	kind error,
	sourcePath string,
	sessionID string,
	data []byte,
	cause error,
) error {
	if sessionID == "" {
		candidate := strings.TrimSuffix(filepath.Base(sourcePath), filepath.Ext(sourcePath))
		if ValidateSessionID(candidate) == nil {
			sessionID = candidate
		}
	}
	metadata := SessionRecoveryMetadata{
		SessionID:       sessionID,
		SourcePath:      sourcePath,
		Kind:            sessionRecoveryKind(kind),
		LifecycleStatus: SessionLifecycleRecoveryNeeded,
	}
	if len(data) > 0 {
		var envelope struct {
			Revision              uint64 `json:"revision"`
			LastCommittedRevision uint64 `json:"lastCommittedRevision"`
		}
		if json.Unmarshal(data, &envelope) == nil && envelope.LastCommittedRevision <= envelope.Revision {
			metadata.LastCommittedRevision = envelope.LastCommittedRevision
		}
	}
	if absolute, err := filepath.Abs(sourcePath); err == nil {
		metadata.SourcePath = absolute
	}
	if data != nil {
		metadata.QuarantinePath = s.sessionQuarantinePath(sessionID, data)
	}
	return &SessionRecoveryError{Kind: kind, Cause: cause, Recovery: metadata}
}

func sessionRecoveryKind(kind error) string {
	if errors.Is(kind, ErrSessionVersionUnsupported) {
		return "unsupported_version"
	}
	return "corrupt"
}

func (s *SessionStore) sessionQuarantinePath(sessionID string, data []byte) string {
	storeBase := filepath.Dir(s.dir)
	recoveryDir := filepath.Join(storeBase, ".session-recovery")
	if err := ensureLexicallyContained(storeBase, recoveryDir); err != nil {
		return ""
	}
	if info, err := os.Lstat(recoveryDir); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return ""
		}
		if err := ensureResolvedDirectoryContained(storeBase, recoveryDir); err != nil {
			return ""
		}
	} else if !os.IsNotExist(err) {
		return ""
	}
	if ValidateSessionID(sessionID) != nil {
		sessionID = "session_unknown"
	}
	sum := sha256.Sum256(data)
	name := fmt.Sprintf("%s-%s.quarantine", sessionID, hex.EncodeToString(sum[:8]))
	target := filepath.Join(recoveryDir, name)
	if err := ensureLexicallyContained(recoveryDir, target); err != nil {
		return ""
	}
	return target
}

func (s *SessionStore) validateSessionWorkspacePath(path, workspace string) error {
	key, _, err := workspaceStorageKey(workspace)
	if err != nil {
		return err
	}
	parent := filepath.Dir(path)
	digestDir := filepath.Join(s.dir, key)
	if sameStoragePath(parent, digestDir) {
		return nil
	}
	legacyDir, legacyErr := s.legacyWorkspacePath(workspace)
	if legacyErr == nil && sameStoragePath(parent, legacyDir) {
		return nil
	}
	return fmt.Errorf("workspace does not match storage directory")
}

func sameStoragePath(a, b string) bool {
	aAbs, aErr := filepath.Abs(a)
	bAbs, bErr := filepath.Abs(b)
	if aErr != nil || bErr != nil {
		return false
	}
	aClean := filepath.Clean(aAbs)
	bClean := filepath.Clean(bAbs)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(aClean, bClean)
	}
	return aClean == bClean
}

func (s *SessionStore) storedSessionsLocked() []storedSession {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	stored := make([]storedSession, 0)
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		dir := filepath.Join(s.dir, entry.Name())
		if err := ensureResolvedDirectoryContained(s.dir, dir); err != nil {
			continue
		}
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, file := range files {
			if file.IsDir() || file.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(file.Name(), ".json") {
				continue
			}
			path := filepath.Join(dir, file.Name())
			sess, err := s.loadStoredSession(path, "")
			if err != nil {
				continue
			}
			stored = append(stored, storedSession{path: path, session: sess})
		}
	}
	return stored
}

func sessionSummary(sess Session) SessionSummary {
	preview := ""
	for _, message := range sess.Messages {
		if message.Role == "user" {
			preview = message.Content
		}
	}
	if len(preview) > 80 {
		preview = preview[:80] + "..."
	}
	return SessionSummary{
		Version:               sess.Version,
		Revision:              sess.Revision,
		ID:                    sess.ID,
		Title:                 sess.Title,
		Preview:               preview,
		Workspace:             sess.Workspace,
		Turns:                 sess.Turns,
		Provider:              sess.Provider,
		Model:                 sess.Model,
		UpdatedAt:             sess.UpdatedAt,
		ParentSessionID:       sess.ParentSessionID,
		ParentRevision:        sess.ParentRevision,
		LifecycleStatus:       sess.LifecycleStatus,
		LastCommittedRevision: sess.LastCommittedRevision,
		ReconcileRequired:     sess.ReconcileRequired,
		LastRunTerminal:       cloneDurableRunTerminalMetadata(sess.LastRunTerminal),
	}
}

// ListWorkspaces returns all workspaces that have sessions.
func (s *SessionStore) ListWorkspaces() []Workspace {
	s.mu.RLock()
	defer s.mu.RUnlock()

	type workspaceGroup struct {
		path string
		ids  map[string]struct{}
	}
	groups := make(map[string]*workspaceGroup)
	for _, stored := range s.storedSessionsLocked() {
		key, canonical, err := workspaceStorageKey(stored.session.Workspace)
		if err != nil {
			continue
		}
		group := groups[key]
		if group == nil {
			group = &workspaceGroup{path: canonical, ids: make(map[string]struct{})}
			groups[key] = group
		}
		group.ids[stored.session.ID] = struct{}{}
	}
	workspaces := make([]Workspace, 0, len(groups))
	for _, group := range groups {
		workspaces = append(workspaces, Workspace{
			Path:     group.path,
			Name:     filepath.Base(group.path),
			Sessions: len(group.ids),
		})
	}
	sort.Slice(workspaces, func(i, j int) bool { return workspaces[i].Path < workspaces[j].Path })
	return workspaces
}

// List returns sessions for a specific workspace, sorted by updated time.
func (s *SessionStore) List(workspace string) []SessionSummary {
	s.mu.RLock()
	defer s.mu.RUnlock()

	wantedKey, _, err := workspaceStorageKey(workspace)
	if err != nil {
		return []SessionSummary{}
	}
	sessions := []SessionSummary{}
	seen := make(map[string]struct{})
	for _, stored := range s.storedSessionsLocked() {
		key, _, err := workspaceStorageKey(stored.session.Workspace)
		if err != nil || key != wantedKey {
			continue
		}
		if _, duplicate := seen[stored.session.ID]; duplicate {
			continue
		}
		seen[stored.session.ID] = struct{}{}
		sessions = append(sessions, sessionSummary(stored.session))
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt)
	})

	return sessions
}

// ListAll returns all sessions across all workspaces.
func (s *SessionStore) ListAll() []SessionSummary {
	s.mu.RLock()
	defer s.mu.RUnlock()
	all := []SessionSummary{} // non-nil for JSON `[]` rather than `null`
	seen := make(map[string]struct{})
	for _, stored := range s.storedSessionsLocked() {
		if _, duplicate := seen[stored.session.ID]; duplicate {
			continue
		}
		seen[stored.session.ID] = struct{}{}
		all = append(all, sessionSummary(stored.session))
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].UpdatedAt.After(all[j].UpdatedAt)
	})
	return all
}

// Get loads a session by ID, searching across all workspaces.
func (s *SessionStore) Get(id string) (*Session, error) {
	if err := ValidateSessionID(id); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.getLocked(id)
}

func (s *SessionStore) getLocked(id string) (*Session, error) {
	path, err := s.findSessionPathLocked(id)
	if err != nil {
		return nil, err
	}
	sess, err := s.loadStoredSession(path, id)
	if err != nil {
		return nil, fmt.Errorf("load session %q: %w", id, err)
	}
	return &sess, nil
}

func (s *SessionStore) findSessionPathLocked(id string) (string, error) {
	if err := ValidateSessionID(id); err != nil {
		return "", err
	}
	if s.initErr != nil {
		return "", fmt.Errorf("initialize session store: %w", s.initErr)
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%w: %s", ErrSessionNotFound, id)
		}
		return "", fmt.Errorf("read session store: %w", err)
	}
	var found string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(s.dir, e.Name())
		path, err := s.sessionPath(dir, id)
		if err != nil {
			return "", fmt.Errorf("resolve session %q: %w", id, err)
		}
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("inspect session %q: %w", id, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return "", fmt.Errorf("session %q is not a regular file", id)
		}
		if found != "" {
			return "", fmt.Errorf("%w: duplicate session %s", ErrSessionConflict, id)
		}
		found = path
	}
	if found == "" {
		return "", fmt.Errorf("%w: %s", ErrSessionNotFound, id)
	}
	return found, nil
}

// Save persists a session to disk under its workspace directory.
func (s *SessionStore) Save(sess *Session) error {
	return s.save(sess, nil, nil)
}

// SaveExpected persists a session only when expectedRevision matches the
// revision currently on disk. A new or legacy-unversioned session has current
// revision zero. The check and atomic replacement occur under one store lock.
func (s *SessionStore) SaveExpected(sess *Session, expectedRevision uint64) error {
	return s.save(sess, &expectedRevision, nil)
}

// CommitInterruptedRun atomically commits a successful run transcript and
// terminal while clearing the exact pre-execution marker that guarded it. The
// ordinary save path continues to reject interrupted sessions; this narrower
// transition requires both the expected revision and full marker equality.
func (s *SessionStore) CommitInterruptedRun(
	sess *Session,
	expectedRevision uint64,
	marker SessionInterruption,
) error {
	if strings.TrimSpace(marker.RunID) == "" {
		return fmt.Errorf("%w: interrupted run marker has no run id", ErrSessionLifecycleInvalid)
	}
	if err := ValidateSessionInterruption(marker); err != nil {
		return err
	}
	marker = *cloneSessionInterruption(&marker)
	return s.save(sess, &expectedRevision, &marker)
}

// Fork atomically snapshots a versioned parent at expectedParentRevision into
// a new independent session. It never mutates the parent.
func (s *SessionStore) Fork(parentID string, expectedParentRevision uint64) (*Session, error) {
	if err := ValidateSessionID(parentID); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.initErr != nil {
		return nil, fmt.Errorf("initialize session store: %w", s.initErr)
	}
	parent, err := s.getLocked(parentID)
	if err != nil {
		return nil, err
	}
	if parent.Version != currentSessionVersion {
		return nil, fmt.Errorf(
			"%w: session %s uses schema version %d and must be migrated before fork",
			ErrSessionVersionUnsupported,
			parentID,
			parent.Version,
		)
	}
	if parent.Revision != expectedParentRevision {
		return nil, &SessionRevisionConflictError{
			SessionID: parentID,
			Expected:  expectedParentRevision,
			Current:   parent.Revision,
		}
	}
	if parent.ReconcileRequired || parent.LifecycleStatus == SessionLifecycleInterrupted ||
		parent.LifecycleStatus == SessionLifecycleRecoveryNeeded {
		return nil, fmt.Errorf("%w: parent session %s", ErrSessionReconcileRequired, parentID)
	}
	id, err := s.allocateSessionIDLocked()
	if err != nil {
		return nil, err
	}
	messages, err := cloneSessionMessages(parent.Messages)
	if err != nil {
		return nil, fmt.Errorf("clone parent session %s: %w", parentID, err)
	}
	_, workspace, err := workspaceStorageKey(parent.Workspace)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	child := Session{
		Version:               currentSessionVersion,
		Revision:              1,
		ID:                    id,
		Title:                 parent.Title,
		Workspace:             workspace,
		Messages:              messages,
		Provider:              parent.Provider,
		Model:                 parent.Model,
		CreatedAt:             now,
		UpdatedAt:             now,
		Turns:                 parent.Turns,
		ParentSessionID:       parent.ID,
		ParentRevision:        parent.Revision,
		LifecycleStatus:       SessionLifecycleActive,
		LastCommittedRevision: 1,
		LastRunTerminal:       cloneDurableRunTerminalMetadata(parent.LastRunTerminal),
	}
	data, err := json.MarshalIndent(&child, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode forked session %q: %w", child.ID, err)
	}
	committedResults := committedToolResultDigests(parent.Messages)
	var childMemory *SessionMemory
	if len(committedResults) > 0 {
		parentMemory, openErr := s.openToolResultMemoryForSessionLocked(*parent)
		if openErr != nil {
			return nil, openErr
		}
		childMemory, openErr = s.openToolResultMemoryForSessionLocked(child)
		if openErr != nil {
			return nil, openErr
		}
		if cloneErr := parentMemory.CloneResultsTo(childMemory, committedResults); cloneErr != nil {
			_ = childMemory.CleanupSafe()
			return nil, fmt.Errorf("clone forked tool results: %w", cloneErr)
		}
	}
	if err := s.persistSessionLocked(child, "", data); err != nil {
		if childMemory != nil {
			_ = childMemory.CleanupSafe()
		}
		return nil, fmt.Errorf("persist forked session %q: %w", child.ID, err)
	}
	return &child, nil
}

func cloneSessionMessages(messages []SessionMessage) ([]SessionMessage, error) {
	if messages == nil {
		return nil, nil
	}
	data, err := json.Marshal(messages)
	if err != nil {
		return nil, err
	}
	var clone []SessionMessage
	if err := json.Unmarshal(data, &clone); err != nil {
		return nil, err
	}
	return clone, nil
}

// SessionResumeState is the store-level gate a future resume route must check
// before starting another agent run.
type SessionResumeState struct {
	SessionID             string                      `json:"sessionId"`
	Revision              uint64                      `json:"revision"`
	ParentSessionID       string                      `json:"parentSessionId,omitempty"`
	ParentRevision        uint64                      `json:"parentRevision,omitempty"`
	LifecycleStatus       SessionLifecycleStatus      `json:"lifecycleStatus"`
	LastCommittedRevision uint64                      `json:"lastCommittedRevision"`
	ReconcileRequired     bool                        `json:"reconcileRequired"`
	Interruption          *SessionInterruption        `json:"interruption,omitempty"`
	LastRunTerminal       *DurableRunTerminalMetadata `json:"lastRunTerminal,omitempty"`
}

func (s *SessionStore) ResumeState(id string) (SessionResumeState, error) {
	sess, err := s.Get(id)
	if err != nil {
		return SessionResumeState{}, err
	}
	return sessionResumeState(*sess), nil
}

func sessionResumeState(sess Session) SessionResumeState {
	return SessionResumeState{
		SessionID:             sess.ID,
		Revision:              sess.Revision,
		ParentSessionID:       sess.ParentSessionID,
		ParentRevision:        sess.ParentRevision,
		LifecycleStatus:       sess.LifecycleStatus,
		LastCommittedRevision: sess.LastCommittedRevision,
		ReconcileRequired:     sess.ReconcileRequired,
		Interruption:          cloneSessionInterruption(sess.Interruption),
		LastRunTerminal:       cloneDurableRunTerminalMetadata(sess.LastRunTerminal),
	}
}

// MarkInterrupted records an uncertain external side effect and fails closed:
// ordinary Save, Fork, and future resume consumers remain blocked until an
// explicit MarkReconciled call succeeds.
func (s *SessionStore) MarkInterrupted(id string, expectedRevision uint64, marker SessionInterruption) (*Session, error) {
	if marker.At.IsZero() {
		marker.At = time.Now().UTC()
	} else {
		marker.At = marker.At.UTC()
	}
	if err := ValidateSessionInterruption(marker); err != nil {
		return nil, err
	}
	return s.updateSessionLifecycle(id, expectedRevision, func(sess *Session) error {
		if sess.LifecycleStatus == SessionLifecycleClosed {
			return fmt.Errorf("%w: closed session cannot be interrupted", ErrSessionLifecycleInvalid)
		}
		if sess.ReconcileRequired {
			return fmt.Errorf("%w: session %s", ErrSessionReconcileRequired, sess.ID)
		}
		lastCommitted := sess.Revision
		sess.LifecycleStatus = SessionLifecycleInterrupted
		sess.LastCommittedRevision = lastCommitted
		sess.ReconcileRequired = true
		sess.Interruption = cloneSessionInterruption(&marker)
		return nil
	})
}

// UpdateInterruptedRun replaces one exact persisted run marker with a more
// complete marker for the same run. It uses the lifecycle CAS and never
// adopts a stale revision, which lets parallel pre-execution journals build a
// deterministic aggregate before their corresponding executors proceed.
func (s *SessionStore) UpdateInterruptedRun(
	id string,
	expectedRevision uint64,
	expectedMarker SessionInterruption,
	nextMarker SessionInterruption,
) (*Session, error) {
	if err := ValidateSessionInterruption(expectedMarker); err != nil {
		return nil, err
	}
	if nextMarker.At.IsZero() {
		nextMarker.At = expectedMarker.At
	} else {
		nextMarker.At = nextMarker.At.UTC()
	}
	if err := ValidateSessionInterruption(nextMarker); err != nil {
		return nil, err
	}
	if strings.TrimSpace(expectedMarker.RunID) == "" || expectedMarker.RunID != nextMarker.RunID {
		return nil, fmt.Errorf("%w: interrupted run id does not match", ErrSessionLifecycleInvalid)
	}
	return s.updateSessionLifecycle(id, expectedRevision, func(sess *Session) error {
		if sess.LifecycleStatus != SessionLifecycleInterrupted ||
			!sess.ReconcileRequired || sess.Interruption == nil {
			return fmt.Errorf("%w: session is not guarded by an execution marker", ErrSessionLifecycleInvalid)
		}
		if !sameSessionInterruption(sess.Interruption, &expectedMarker) {
			return fmt.Errorf("%w: interrupted run marker does not match", ErrSessionReconcileRequired)
		}
		sess.Interruption = cloneSessionInterruption(&nextMarker)
		return nil
	})
}

// ValidateSessionInterruption validates the redacted interruption envelope.
// Legacy Reason-only markers remain valid for persisted-session compatibility.
func ValidateSessionInterruption(marker SessionInterruption) error {
	if len(marker.Reason) > 1024 || len(marker.RunID) > 256 || len(marker.ToolName) > 128 ||
		len(marker.ToolCallID) > 256 || len(marker.Summary) > 1024 {
		return fmt.Errorf("%w: interruption marker is too large", ErrSessionLifecycleInvalid)
	}
	hasStructuredMetadata := marker.RunID != "" || marker.InputDigest != "" ||
		marker.SideEffectState != "" || marker.Summary != ""
	if !hasStructuredMetadata {
		return nil
	}
	if strings.TrimSpace(marker.RunID) == "" || strings.TrimSpace(marker.ToolName) == "" ||
		strings.TrimSpace(marker.Summary) == "" {
		return fmt.Errorf("%w: structured interruption metadata is incomplete", ErrSessionLifecycleInvalid)
	}
	if !sessionInputDigestPattern.MatchString(marker.InputDigest) {
		return fmt.Errorf("%w: interruption input digest must be lowercase sha256", ErrSessionLifecycleInvalid)
	}
	switch marker.SideEffectState {
	case SessionSideEffectUnknown, SessionSideEffectStarted,
		SessionSideEffectMayHaveApplied, SessionSideEffectApplied:
		return nil
	default:
		return fmt.Errorf("%w: invalid side effect state %q", ErrSessionLifecycleInvalid, marker.SideEffectState)
	}
}

// MarkReconciled explicitly acknowledges an interrupted side effect and makes
// the resulting revision safe to resume.
func (s *SessionStore) MarkReconciled(id string, expectedRevision uint64) (*Session, error) {
	return s.updateSessionLifecycle(id, expectedRevision, func(sess *Session) error {
		if !sess.ReconcileRequired ||
			(sess.LifecycleStatus != SessionLifecycleInterrupted && sess.LifecycleStatus != SessionLifecycleRecoveryNeeded) {
			return fmt.Errorf("%w: session is not awaiting reconciliation", ErrSessionLifecycleInvalid)
		}
		sess.LifecycleStatus = SessionLifecycleActive
		sess.ReconcileRequired = false
		sess.Interruption = nil
		return nil
	})
}

// Close marks an active session terminal while retaining its immutable history.
func (s *SessionStore) Close(id string, expectedRevision uint64) (*Session, error) {
	return s.updateSessionLifecycle(id, expectedRevision, func(sess *Session) error {
		if sess.ReconcileRequired {
			return fmt.Errorf("%w: session %s", ErrSessionReconcileRequired, sess.ID)
		}
		if sess.LifecycleStatus != SessionLifecycleActive {
			return fmt.Errorf("%w: only an active session can be closed", ErrSessionLifecycleInvalid)
		}
		sess.LifecycleStatus = SessionLifecycleClosed
		sess.Interruption = nil
		return nil
	})
}

func (s *SessionStore) updateSessionLifecycle(
	id string,
	expectedRevision uint64,
	transition func(*Session) error,
) (*Session, error) {
	if err := ValidateSessionID(id); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.initErr != nil {
		return nil, fmt.Errorf("initialize session store: %w", s.initErr)
	}
	path, err := s.findSessionPathLocked(id)
	if err != nil {
		return nil, err
	}
	sess, err := s.loadStoredSession(path, id)
	if err != nil {
		return nil, fmt.Errorf("load session %q: %w", id, err)
	}
	if sess.Version != currentSessionVersion {
		return nil, fmt.Errorf("%w: lifecycle updates require a versioned session", ErrSessionVersionUnsupported)
	}
	if sess.Revision != expectedRevision {
		return nil, &SessionRevisionConflictError{
			SessionID: id,
			Expected:  expectedRevision,
			Current:   sess.Revision,
		}
	}
	if sess.Revision == ^uint64(0) {
		return nil, fmt.Errorf("%w: session %s", ErrSessionRevisionOverflow, id)
	}
	if err := transition(&sess); err != nil {
		return nil, err
	}
	sess.Version = currentSessionVersion
	sess.Revision++
	if !sess.ReconcileRequired {
		sess.LastCommittedRevision = sess.Revision
	}
	sess.UpdatedAt = time.Now()
	if err := normalizeSessionLifecycle(&sess); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(&sess, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode session %q: %w", id, err)
	}
	if err := s.persistSessionLocked(sess, path, data); err != nil {
		return nil, fmt.Errorf("persist session %q: %w", id, err)
	}
	return &sess, nil
}

func (s *SessionStore) save(
	sess *Session,
	expectedRevision *uint64,
	committedInterruption *SessionInterruption,
) error {
	if sess == nil {
		return fmt.Errorf("session is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.initErr != nil {
		return fmt.Errorf("initialize session store: %w", s.initErr)
	}

	candidate := *sess
	candidate.LastRunTerminal = cloneDurableRunTerminalMetadata(sess.LastRunTerminal)
	var existingPath string
	var existing *Session
	if candidate.ID == "" {
		id, err := s.allocateSessionIDLocked()
		if err != nil {
			return err
		}
		candidate.ID = id
	} else if err := ValidateSessionID(candidate.ID); err != nil {
		return err
	}

	path, err := s.findSessionPathLocked(candidate.ID)
	switch {
	case err == nil:
		existingPath = path
		var readErr error
		existing, readErr = s.getLocked(candidate.ID)
		if readErr != nil {
			return readErr
		}
		if expectedRevision != nil && *expectedRevision != existing.Revision {
			return &SessionRevisionConflictError{
				SessionID: candidate.ID,
				Expected:  *expectedRevision,
				Current:   existing.Revision,
			}
		}
		if existing.Revision == ^uint64(0) {
			return fmt.Errorf("%w: session %s", ErrSessionRevisionOverflow, candidate.ID)
		}
		if candidate.Workspace != "" && !sameWorkspace(candidate.Workspace, existing.Workspace) {
			return fmt.Errorf("%w: session %s belongs to another workspace", ErrSessionConflict, candidate.ID)
		}
		candidate.Workspace = existing.Workspace
		if candidate.CreatedAt.IsZero() {
			candidate.CreatedAt = existing.CreatedAt
		}
		if candidate.ParentSessionID == "" && candidate.ParentRevision == 0 {
			candidate.ParentSessionID = existing.ParentSessionID
			candidate.ParentRevision = existing.ParentRevision
		} else if candidate.ParentSessionID != existing.ParentSessionID || candidate.ParentRevision != existing.ParentRevision {
			return fmt.Errorf("%w: parent metadata is immutable", ErrSessionParentInvalid)
		}
	case errors.Is(err, ErrSessionNotFound):
		// A caller-supplied valid ID may create a new session for compatibility.
		if expectedRevision != nil && *expectedRevision != 0 {
			return &SessionRevisionConflictError{
				SessionID: candidate.ID,
				Expected:  *expectedRevision,
				Current:   0,
			}
		}
	case err != nil:
		return err
	}
	if existing != nil && candidate.LastRunTerminal == nil {
		candidate.LastRunTerminal = cloneDurableRunTerminalMetadata(existing.LastRunTerminal)
	}
	if err := validateOptionalDurableRunTerminal(candidate.LastRunTerminal); err != nil {
		return err
	}
	if committedInterruption != nil {
		if err := prepareInterruptedRunCommit(&candidate, existing, committedInterruption); err != nil {
			return err
		}
	} else {
		if err := prepareSessionLifecycleForSave(&candidate, existing); err != nil {
			return err
		}
	}
	if err := validateSessionParentMetadata(candidate.ID, candidate.ParentSessionID, candidate.ParentRevision); err != nil {
		return err
	}
	candidate.Version = currentSessionVersion
	if existing == nil {
		candidate.Revision = 1
	} else {
		candidate.Revision = existing.Revision + 1
	}
	candidate.LastCommittedRevision = candidate.Revision

	if candidate.Workspace == "" {
		candidate.Workspace, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("resolve current workspace: %w", err)
		}
	}
	_, canonicalWorkspace, err := workspaceStorageKey(candidate.Workspace)
	if err != nil {
		return err
	}
	candidate.Workspace = canonicalWorkspace
	if existing == nil && candidate.ParentSessionID != "" {
		parent, err := s.getLocked(candidate.ParentSessionID)
		if err != nil {
			return fmt.Errorf("%w: load parent session: %v", ErrSessionParentInvalid, err)
		}
		if parent.Revision != candidate.ParentRevision {
			return fmt.Errorf(
				"%w: parent %s is revision %d, requested %d",
				ErrSessionParentInvalid,
				candidate.ParentSessionID,
				parent.Revision,
				candidate.ParentRevision,
			)
		}
		if !sameWorkspace(candidate.Workspace, parent.Workspace) {
			return fmt.Errorf("%w: parent belongs to another workspace", ErrSessionParentInvalid)
		}
	}
	now := time.Now()
	if candidate.CreatedAt.IsZero() {
		candidate.CreatedAt = now
	}
	candidate.UpdatedAt = now

	// Auto-generate title from first user message
	if candidate.Title == "" {
		for _, m := range candidate.Messages {
			if m.Role == "user" && m.Content != "" {
				title := m.Content
				if len(title) > 50 {
					title = title[:50] + "..."
				}
				candidate.Title = title
				break
			}
		}
		if candidate.Title == "" {
			candidate.Title = "New Chat"
		}
	}

	// Count turns
	turns := 0
	for _, m := range candidate.Messages {
		if m.Role == "user" {
			turns++
		}
	}
	candidate.Turns = turns

	data, err := json.MarshalIndent(&candidate, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session %q: %w", candidate.ID, err)
	}
	if err := s.persistSessionLocked(candidate, existingPath, data); err != nil {
		return fmt.Errorf("persist session %q: %w", candidate.ID, err)
	}
	*sess = candidate
	return nil
}

func prepareInterruptedRunCommit(
	candidate *Session,
	existing *Session,
	marker *SessionInterruption,
) error {
	if candidate == nil || existing == nil || marker == nil {
		return fmt.Errorf("%w: interrupted run commit requires existing state", ErrSessionLifecycleInvalid)
	}
	if existing.LifecycleStatus != SessionLifecycleInterrupted ||
		!existing.ReconcileRequired || existing.Interruption == nil {
		return fmt.Errorf("%w: session is not guarded by an execution marker", ErrSessionLifecycleInvalid)
	}
	if !sameSessionInterruption(existing.Interruption, marker) {
		return fmt.Errorf("%w: interrupted run marker does not match", ErrSessionReconcileRequired)
	}
	candidate.LifecycleStatus = SessionLifecycleActive
	candidate.ReconcileRequired = false
	candidate.Interruption = nil
	candidate.LastCommittedRevision = existing.LastCommittedRevision
	return nil
}

func prepareSessionLifecycleForSave(candidate *Session, existing *Session) error {
	if candidate == nil {
		return fmt.Errorf("%w: session is nil", ErrSessionLifecycleInvalid)
	}
	if existing == nil {
		if candidate.LifecycleStatus != "" && candidate.LifecycleStatus != SessionLifecycleActive {
			return fmt.Errorf("%w: new sessions must be active", ErrSessionLifecycleInvalid)
		}
		if candidate.ReconcileRequired || candidate.Interruption != nil {
			return fmt.Errorf("%w: new session cannot start interrupted", ErrSessionLifecycleInvalid)
		}
		candidate.LifecycleStatus = SessionLifecycleActive
		candidate.LastCommittedRevision = 0
		return nil
	}
	if existing.ReconcileRequired {
		return fmt.Errorf("%w: session %s", ErrSessionReconcileRequired, existing.ID)
	}
	if existing.LifecycleStatus == SessionLifecycleClosed {
		return fmt.Errorf("%w: closed session %s cannot be saved", ErrSessionLifecycleInvalid, existing.ID)
	}
	if candidate.LifecycleStatus == "" {
		if candidate.ReconcileRequired || candidate.Interruption != nil {
			return fmt.Errorf("%w: partial lifecycle update", ErrSessionLifecycleInvalid)
		}
		candidate.LifecycleStatus = existing.LifecycleStatus
		candidate.ReconcileRequired = existing.ReconcileRequired
		candidate.Interruption = cloneSessionInterruption(existing.Interruption)
	} else if candidate.LifecycleStatus != existing.LifecycleStatus ||
		candidate.ReconcileRequired != existing.ReconcileRequired ||
		!sameSessionInterruption(candidate.Interruption, existing.Interruption) {
		return fmt.Errorf("%w: lifecycle transitions require the lifecycle API", ErrSessionLifecycleInvalid)
	}
	candidate.LastCommittedRevision = existing.LastCommittedRevision
	return nil
}

func cloneSessionInterruption(value *SessionInterruption) *SessionInterruption {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func sameSessionInterruption(a, b *SessionInterruption) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.At.Equal(b.At) && a.Reason == b.Reason && a.ToolName == b.ToolName &&
		a.ToolCallID == b.ToolCallID && a.RunID == b.RunID && a.InputDigest == b.InputDigest &&
		a.SideEffectState == b.SideEffectState && a.Summary == b.Summary
}

func (s *SessionStore) allocateSessionIDLocked() (string, error) {
	generator := s.idGenerator
	if generator == nil {
		generator = generatePersistedSessionID
	}
	for attempt := 0; attempt < maxSessionIDAttempts; attempt++ {
		id, err := generator()
		if err != nil {
			return "", err
		}
		if err := ValidateSessionID(id); err != nil {
			return "", fmt.Errorf("session id generator: %w", err)
		}
		_, err = s.findSessionPathLocked(id)
		if errors.Is(err, ErrSessionNotFound) {
			return id, nil
		}
		if err != nil && !errors.Is(err, ErrSessionConflict) {
			return "", err
		}
	}
	return "", fmt.Errorf("%w: unable to allocate a unique session id", ErrSessionConflict)
}

func sameWorkspace(a, b string) bool {
	aKey, _, aErr := workspaceStorageKey(a)
	bKey, _, bErr := workspaceStorageKey(b)
	return aErr == nil && bErr == nil && aKey == bKey
}

func (s *SessionStore) persistSessionLocked(sess Session, existingPath string, data []byte) error {
	dir, err := s.workspaceDir(sess.Workspace)
	if err != nil {
		return err
	}
	targetPath, err := s.sessionPath(dir, sess.ID)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(dir, targetPath, data); err != nil {
		return err
	}
	if existingPath == "" || sameStoragePath(existingPath, targetPath) {
		return nil
	}
	if err := os.Remove(existingPath); err != nil {
		rollbackErr := os.Remove(targetPath)
		if rollbackErr != nil {
			return fmt.Errorf("remove legacy session: %v (rollback new session: %v)", err, rollbackErr)
		}
		return fmt.Errorf("remove legacy session: %w", err)
	}
	return nil
}

func writeFileAtomic(dir, target string, data []byte) (retErr error) {
	temp, err := os.CreateTemp(dir, ".session-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() {
		if temp != nil {
			_ = temp.Close()
		}
		if retErr != nil {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(0644); err != nil {
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
	temp = nil
	if err := os.Rename(tempPath, target); err != nil {
		return err
	}
	return nil
}

// SessionDeleteResult contains content-free cleanup evidence. CleanupPending
// means the transcript was deleted but a verified exact-namespace cleanup
// could not complete and must be retried by retention maintenance.
type SessionDeleteResult struct {
	ResultCount    int   `json:"resultCount"`
	TotalBytes     int64 `json:"totalBytes"`
	CleanupPending bool  `json:"cleanupPending"`
}

// Delete preserves the legacy API. New durable callers must use
// DeleteExpected so a stale client cannot delete a newer transcript.
func (s *SessionStore) Delete(id string) error {
	_, err := s.deleteSession(id, nil)
	return err
}

// DeleteExpected removes one exact persisted revision and then cleans only its
// workspace-bound result namespace. The transcript is removed first so a
// cleanup failure leaves an orphan for retention rather than a live transcript
// with broken references.
func (s *SessionStore) DeleteExpected(id string, expectedRevision uint64) (SessionDeleteResult, error) {
	return s.deleteSession(id, &expectedRevision)
}

func (s *SessionStore) deleteSession(id string, expectedRevision *uint64) (SessionDeleteResult, error) {
	if err := ValidateSessionID(id); err != nil {
		return SessionDeleteResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	path, err := s.findSessionPathLocked(id)
	if err != nil {
		return SessionDeleteResult{}, err
	}
	session, err := s.loadStoredSession(path, id)
	if err != nil {
		return SessionDeleteResult{}, fmt.Errorf("load session %q: %w", id, err)
	}
	if expectedRevision != nil && session.Revision != *expectedRevision {
		return SessionDeleteResult{}, &SessionRevisionConflictError{
			SessionID: id,
			Expected:  *expectedRevision,
			Current:   session.Revision,
		}
	}
	memory, err := s.openToolResultMemoryForSessionLocked(session)
	if err != nil {
		return SessionDeleteResult{}, err
	}
	stats := memory.Stats()
	if err := os.Remove(path); err != nil {
		return SessionDeleteResult{}, fmt.Errorf("delete session %q: %w", id, err)
	}
	result := SessionDeleteResult{ResultCount: stats.ResultCount, TotalBytes: stats.TotalBytes}
	if err := memory.CleanupSafe(); err != nil {
		result.CleanupPending = true
	}
	return result, nil
}

// Rename changes a session's title.
func (s *SessionStore) Rename(id, title string) error {
	if err := ValidateSessionID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	path, err := s.findSessionPathLocked(id)
	if err != nil {
		return err
	}
	sess, err := s.loadStoredSession(path, id)
	if err != nil {
		return fmt.Errorf("load session %q: %w", id, err)
	}
	if sess.Revision == ^uint64(0) {
		return fmt.Errorf("%w: session %s", ErrSessionRevisionOverflow, id)
	}
	if sess.ReconcileRequired {
		return fmt.Errorf("%w: session %s", ErrSessionReconcileRequired, id)
	}
	_, canonicalWorkspace, err := workspaceStorageKey(sess.Workspace)
	if err != nil {
		return err
	}
	sess.Workspace = canonicalWorkspace
	sess.Version = currentSessionVersion
	sess.Revision++
	sess.LastCommittedRevision = sess.Revision
	sess.Title = title
	sess.UpdatedAt = time.Now()
	data, err := json.MarshalIndent(&sess, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session %q: %w", id, err)
	}
	if err := s.persistSessionLocked(sess, path, data); err != nil {
		return fmt.Errorf("persist session %q: %w", id, err)
	}
	return nil
}
