package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func opaqueTestSessionID(letter string) string {
	return "sess_" + strings.Repeat(letter, 26)
}

func TestValidateSessionIDSupportsOpaqueAndLegacyFormats(t *testing.T) {
	t.Parallel()

	valid := []string{
		opaqueTestSessionID("a"),
		"sess_20260811-120000",
	}
	for _, id := range valid {
		if err := ValidateSessionID(id); err != nil {
			t.Errorf("ValidateSessionID(%q) = %v, want nil", id, err)
		}
	}

	invalid := []string{
		"",
		"sess_",
		"sess_short",
		"sess_20260811-120000.json",
		"../sess_20260811-120000",
		"sess_20260811-120000/../../outside",
		"sess_ABCDEFGHIJKLMNOPQRSTUVWXYZ",
	}
	for _, id := range invalid {
		err := ValidateSessionID(id)
		if !errors.Is(err, ErrInvalidSessionID) {
			t.Errorf("ValidateSessionID(%q) = %v, want ErrInvalidSessionID", id, err)
		}
	}
}

func TestSessionStoreCRUDUsesOpaqueIDAndAtomicReplacement(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	store := NewSessionStore(base)

	sess := &Session{
		Workspace: workspace,
		Messages: []SessionMessage{
			{Role: "user", Content: "first question"},
			{Role: "assistant", Content: "first answer"},
		},
	}
	if err := store.Save(sess); err != nil {
		t.Fatalf("Save(new) = %v", err)
	}
	if !opaqueSessionIDPattern.MatchString(sess.ID) {
		t.Fatalf("generated ID = %q, want opaque format", sess.ID)
	}
	createdAt := sess.CreatedAt

	updated := &Session{
		ID:    sess.ID,
		Title: "updated title",
		Messages: []SessionMessage{
			{Role: "user", Content: "updated question"},
		},
	}
	if err := store.Save(updated); err != nil {
		t.Fatalf("Save(update) = %v", err)
	}
	if updated.Workspace != workspace {
		t.Fatalf("update workspace = %q, want %q", updated.Workspace, workspace)
	}
	if !updated.CreatedAt.Equal(createdAt) {
		t.Fatalf("update changed CreatedAt: got %s want %s", updated.CreatedAt, createdAt)
	}

	badUpdate := &Session{
		ID:    sess.ID,
		Title: "must not be persisted",
		Messages: []SessionMessage{
			{Role: "user", Content: "bad", ToolInput: make(chan int)},
		},
	}
	if err := store.Save(badUpdate); err == nil {
		t.Fatal("Save(unencodable update) = nil, want error")
	}
	got, err := store.Get(sess.ID)
	if err != nil {
		t.Fatalf("Get(after failed update) = %v", err)
	}
	if got.Title != "updated title" {
		t.Fatalf("failed update replaced persisted session: title=%q", got.Title)
	}

	if err := store.Rename(sess.ID, "renamed"); err != nil {
		t.Fatalf("Rename = %v", err)
	}
	got, err = store.Get(sess.ID)
	if err != nil {
		t.Fatalf("Get(after rename) = %v", err)
	}
	if got.Title != "renamed" {
		t.Fatalf("renamed title = %q", got.Title)
	}

	if err := filepath.Walk(filepath.Join(base, "sessions"), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if strings.Contains(info.Name(), ".session-") && strings.HasSuffix(info.Name(), ".tmp") {
			t.Errorf("temporary session file remains: %s", path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk session store: %v", err)
	}

	if err := store.Delete(sess.ID); err != nil {
		t.Fatalf("Delete = %v", err)
	}
	if _, err := store.Get(sess.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Get(after delete) = %v, want ErrSessionNotFound", err)
	}
	if err := store.Delete(sess.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Delete(missing) = %v, want ErrSessionNotFound", err)
	}
}

func TestSessionStoreRejectsTraversalForEveryIDOperation(t *testing.T) {
	base := t.TempDir()
	store := NewSessionStore(base)
	maliciousID := "../outside"

	if err := store.Save(&Session{ID: maliciousID, Workspace: base}); !errors.Is(err, ErrInvalidSessionID) {
		t.Fatalf("Save(traversal) = %v, want ErrInvalidSessionID", err)
	}
	if _, err := store.Get(maliciousID); !errors.Is(err, ErrInvalidSessionID) {
		t.Fatalf("Get(traversal) = %v, want ErrInvalidSessionID", err)
	}
	if err := store.Delete(maliciousID); !errors.Is(err, ErrInvalidSessionID) {
		t.Fatalf("Delete(traversal) = %v, want ErrInvalidSessionID", err)
	}
	if err := store.Rename(maliciousID, "renamed"); !errors.Is(err, ErrInvalidSessionID) {
		t.Fatalf("Rename(traversal) = %v, want ErrInvalidSessionID", err)
	}
	if _, err := os.Stat(filepath.Join(base, "outside.json")); !os.IsNotExist(err) {
		t.Fatalf("traversal target exists or stat failed unexpectedly: %v", err)
	}

	validID := opaqueTestSessionID("c")
	if err := store.Save(&Session{ID: validID, Workspace: ".."}); err != nil {
		t.Fatalf("Save(relative workspace) = %v", err)
	}
	dir, err := store.workspaceDir("..")
	if err != nil {
		t.Fatalf("workspaceDir(relative workspace) = %v", err)
	}
	if !strings.HasPrefix(filepath.Base(dir), "ws_") {
		t.Fatalf("relative workspace directory = %q, want digest key", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, validID+".json")); err != nil {
		t.Fatalf("relative workspace session is not contained in digest directory: %v", err)
	}
}

func TestSessionStoreRetriesGeneratedIDCollisionAndRejectsWorkspaceMove(t *testing.T) {
	base := t.TempDir()
	store := NewSessionStore(base)
	collisionID := opaqueTestSessionID("d")
	freshID := opaqueTestSessionID("e")
	workspaceA := filepath.Join(base, "workspace-a")
	workspaceB := filepath.Join(base, "workspace-b")

	if err := store.Save(&Session{ID: collisionID, Workspace: workspaceA}); err != nil {
		t.Fatalf("seed collision session: %v", err)
	}
	calls := 0
	store.idGenerator = func() (string, error) {
		calls++
		if calls == 1 {
			return collisionID, nil
		}
		return freshID, nil
	}
	generated := &Session{Workspace: workspaceB}
	if err := store.Save(generated); err != nil {
		t.Fatalf("Save(after generated collision) = %v", err)
	}
	if generated.ID != freshID || calls != 2 {
		t.Fatalf("generated ID=%q calls=%d, want %q and 2", generated.ID, calls, freshID)
	}

	err := store.Save(&Session{ID: collisionID, Workspace: workspaceB})
	if !errors.Is(err, ErrSessionConflict) {
		t.Fatalf("Save(existing ID in another workspace) = %v, want ErrSessionConflict", err)
	}
}

func TestSessionStoreWorkspaceDigestIsolatesLegacyNameCollisions(t *testing.T) {
	base := t.TempDir()
	store := NewSessionStore(base)
	workspaceA := `D:\a\b`
	workspaceB := `D:\a--b`

	legacyA, err := store.legacyWorkspacePath(workspaceA)
	if err != nil {
		t.Fatalf("legacyWorkspacePath(A) = %v", err)
	}
	legacyB, err := store.legacyWorkspacePath(workspaceB)
	if err != nil {
		t.Fatalf("legacyWorkspacePath(B) = %v", err)
	}
	if !sameStoragePath(legacyA, legacyB) {
		t.Fatalf("test setup no longer reproduces legacy collision: %q vs %q", legacyA, legacyB)
	}

	dirA, err := store.workspaceDir(workspaceA)
	if err != nil {
		t.Fatalf("workspaceDir(A) = %v", err)
	}
	dirB, err := store.workspaceDir(workspaceB)
	if err != nil {
		t.Fatalf("workspaceDir(B) = %v", err)
	}
	if sameStoragePath(dirA, dirB) {
		t.Fatalf("digest directories collided: %q", dirA)
	}

	sessionA := &Session{ID: opaqueTestSessionID("f"), Workspace: workspaceA, Title: "A"}
	sessionB := &Session{ID: opaqueTestSessionID("2"), Workspace: workspaceB, Title: "B"}
	if err := store.Save(sessionA); err != nil {
		t.Fatalf("Save(A) = %v", err)
	}
	if err := store.Save(sessionB); err != nil {
		t.Fatalf("Save(B) = %v", err)
	}
	assertOnlySessionID(t, store.List(workspaceA), sessionA.ID)
	assertOnlySessionID(t, store.List(workspaceB), sessionB.ID)
}

func TestSessionStoreCanonicalWorkspaceKeyRelativeAndCaseRules(t *testing.T) {
	workspace, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd = %v", err)
	}
	absoluteKey, _, err := workspaceStorageKey(workspace)
	if err != nil {
		t.Fatalf("workspaceStorageKey(absolute) = %v", err)
	}
	relativeKey, _, err := workspaceStorageKey(".")
	if err != nil {
		t.Fatalf("workspaceStorageKey(relative) = %v", err)
	}
	if relativeKey != absoluteKey {
		t.Fatalf("relative key=%q absolute key=%q", relativeKey, absoluteKey)
	}

	caseVariant := strings.ToUpper(workspace)
	caseKey, _, err := workspaceStorageKey(caseVariant)
	if err != nil {
		t.Fatalf("workspaceStorageKey(case variant) = %v", err)
	}
	if runtime.GOOS == "windows" && caseKey != absoluteKey {
		t.Fatalf("Windows case variant key=%q absolute key=%q", caseKey, absoluteKey)
	}
	if runtime.GOOS != "windows" && caseVariant != workspace && caseKey == absoluteKey {
		t.Fatalf("case-sensitive platform collapsed %q and %q", workspace, caseVariant)
	}
}

func TestSessionStoreLegacyCollisionFiltersAndMigratesByEmbeddedWorkspace(t *testing.T) {
	base := t.TempDir()
	store := NewSessionStore(base)
	workspaceA := `D:\a\b`
	workspaceB := `D:\a--b`
	legacyDir, err := store.legacyWorkspacePath(workspaceA)
	if err != nil {
		t.Fatalf("legacyWorkspacePath = %v", err)
	}
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(legacy) = %v", err)
	}

	sessionA := Session{ID: opaqueTestSessionID("3"), Workspace: workspaceA, Title: "legacy A"}
	sessionB := Session{ID: opaqueTestSessionID("4"), Workspace: workspaceB, Title: "legacy B"}
	writeLegacySession(t, legacyDir, sessionA)
	writeLegacySession(t, legacyDir, sessionB)

	assertOnlySessionID(t, store.List(workspaceA), sessionA.ID)
	assertOnlySessionID(t, store.List(workspaceB), sessionB.ID)
	workspaces := store.ListWorkspaces()
	if len(workspaces) != 2 || workspaces[0].Sessions != 1 || workspaces[1].Sessions != 1 {
		t.Fatalf("ListWorkspaces(legacy collision) = %#v, want two isolated workspaces", workspaces)
	}

	loaded, err := store.Get(sessionA.ID)
	if err != nil {
		t.Fatalf("Get(legacy A) = %v", err)
	}
	loaded.Title = "migrated A"
	if err := store.Save(loaded); err != nil {
		t.Fatalf("Save(migrate legacy A) = %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacyDir, sessionA.ID+".json")); !os.IsNotExist(err) {
		t.Fatalf("legacy A remains after migration: %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacyDir, sessionB.ID+".json")); err != nil {
		t.Fatalf("migration removed colliding workspace B: %v", err)
	}
	digestDir, err := store.workspaceDir(workspaceA)
	if err != nil {
		t.Fatalf("workspaceDir(A) = %v", err)
	}
	if _, err := os.Stat(filepath.Join(digestDir, sessionA.ID+".json")); err != nil {
		t.Fatalf("migrated A missing from digest directory: %v", err)
	}
	assertOnlySessionID(t, store.List(workspaceA), sessionA.ID)
	assertOnlySessionID(t, store.List(workspaceB), sessionB.ID)
}

func TestSessionStoreRejectsSessionWhoseWorkspaceDoesNotMatchDirectory(t *testing.T) {
	base := t.TempDir()
	store := NewSessionStore(base)
	workspaceA := filepath.Join(base, "workspace-a")
	workspaceB := filepath.Join(base, "workspace-b")
	dirA, err := store.workspaceDir(workspaceA)
	if err != nil {
		t.Fatalf("workspaceDir(A) = %v", err)
	}
	corrupt := Session{ID: opaqueTestSessionID("5"), Workspace: workspaceB}
	writeLegacySession(t, dirA, corrupt)

	if got := store.List(workspaceA); len(got) != 0 {
		t.Fatalf("List(A) included mismatched session: %#v", got)
	}
	if got := store.List(workspaceB); len(got) != 0 {
		t.Fatalf("List(B) included session stored under A: %#v", got)
	}
	if _, err := store.Get(corrupt.ID); err == nil {
		t.Fatal("Get(mismatched workspace directory) = nil error")
	}
}

func TestSessionStoreRevisionIsMonotonicAndClientValuesAreIgnored(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	store := NewSessionStore(base)
	sess := &Session{Workspace: workspace, Version: 99, Revision: 99, Title: "created"}

	if err := store.Save(sess); err != nil {
		t.Fatalf("Save(create) = %v", err)
	}
	if sess.Version != currentSessionVersion || sess.Revision != 1 {
		t.Fatalf("created schema = v%d/r%d, want v%d/r1", sess.Version, sess.Revision, currentSessionVersion)
	}

	update := &Session{ID: sess.ID, Title: "lower", Revision: 0}
	if err := store.Save(update); err != nil {
		t.Fatalf("Save(lower client revision) = %v", err)
	}
	if update.Revision != 2 {
		t.Fatalf("lower client revision produced r%d, want r2", update.Revision)
	}

	update.Title = "jump"
	update.Revision = 9999
	if err := store.Save(update); err != nil {
		t.Fatalf("Save(jump client revision) = %v", err)
	}
	if update.Revision != 3 {
		t.Fatalf("client jump produced r%d, want r3", update.Revision)
	}

	if err := store.Rename(sess.ID, "renamed"); err != nil {
		t.Fatalf("Rename = %v", err)
	}
	got, err := store.Get(sess.ID)
	if err != nil {
		t.Fatalf("Get = %v", err)
	}
	if got.Version != currentSessionVersion || got.Revision != 4 || got.Title != "renamed" {
		t.Fatalf("renamed session = v%d/r%d title=%q, want v%d/r4 renamed", got.Version, got.Revision, got.Title, currentSessionVersion)
	}
	summaries := store.List(workspace)
	if len(summaries) != 1 || summaries[0].Revision != 4 || summaries[0].Version != currentSessionVersion {
		t.Fatalf("List revision summary = %#v", summaries)
	}
}

func TestSessionStoreSaveExpectedRejectsStaleRevision(t *testing.T) {
	base := t.TempDir()
	store := NewSessionStore(base)
	sess := &Session{Workspace: filepath.Join(base, "workspace"), Title: "r1"}
	if err := store.SaveExpected(sess, 0); err != nil {
		t.Fatalf("SaveExpected(create) = %v", err)
	}
	first, err := store.Get(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	stale := *first
	first.Title = "winner"
	if err := store.SaveExpected(first, 1); err != nil {
		t.Fatalf("SaveExpected(r1) = %v", err)
	}
	stale.Title = "stale"
	err = store.SaveExpected(&stale, 1)
	if !errors.Is(err, ErrSessionRevisionConflict) {
		t.Fatalf("SaveExpected(stale) = %v, want ErrSessionRevisionConflict", err)
	}
	var conflict *SessionRevisionConflictError
	if !errors.As(err, &conflict) || conflict.Expected != 1 || conflict.Current != 2 || conflict.SessionID != sess.ID {
		t.Fatalf("typed conflict = %#v, %v", conflict, err)
	}
	got, err := store.Get(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != 2 || got.Title != "winner" {
		t.Fatalf("stale write changed session: r%d title=%q", got.Revision, got.Title)
	}

	newSession := &Session{Workspace: filepath.Join(base, "workspace")}
	if err := store.SaveExpected(newSession, 4); !errors.Is(err, ErrSessionRevisionConflict) {
		t.Fatalf("SaveExpected(new, nonzero) = %v, want conflict", err)
	}
}

func TestSessionStoreConcurrentExpectedRevisionHasOneWinnerAcrossStores(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	storeA := NewSessionStore(base)
	storeB := NewSessionStore(base)
	seed := &Session{Workspace: workspace, Title: "seed"}
	if err := storeA.Save(seed); err != nil {
		t.Fatal(err)
	}
	left, err := storeA.Get(seed.ID)
	if err != nil {
		t.Fatal(err)
	}
	right, err := storeB.Get(seed.ID)
	if err != nil {
		t.Fatal(err)
	}
	left.Title = "left"
	right.Title = "right"

	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, update := range []struct {
		store *SessionStore
		sess  *Session
	}{{storeA, left}, {storeB, right}} {
		wg.Add(1)
		go func(updateStore *SessionStore, updateSession *Session) {
			defer wg.Done()
			<-start
			results <- updateStore.SaveExpected(updateSession, 1)
		}(update.store, update.sess)
	}
	close(start)
	wg.Wait()
	close(results)

	successes := 0
	conflicts := 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrSessionRevisionConflict):
			conflicts++
		default:
			t.Fatalf("concurrent SaveExpected = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d, want 1/1", successes, conflicts)
	}
	got, err := storeA.Get(seed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != 2 || (got.Title != "left" && got.Title != "right") {
		t.Fatalf("concurrent result = r%d title=%q", got.Revision, got.Title)
	}
}

func TestSessionStoreMigratesLegacySessionOnNextSave(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	store := NewSessionStore(base)
	dir, err := store.workspaceDir(workspace)
	if err != nil {
		t.Fatal(err)
	}
	legacy := Session{ID: opaqueTestSessionID("6"), Workspace: workspace, Title: "legacy"}
	writeLegacySession(t, dir, legacy)

	loaded, err := store.Get(legacy.ID)
	if err != nil {
		t.Fatalf("Get(legacy) = %v", err)
	}
	if loaded.Version != 0 || loaded.Revision != 0 {
		t.Fatalf("legacy loaded as v%d/r%d, want v0/r0", loaded.Version, loaded.Revision)
	}
	if loaded.LastRunTerminal != nil {
		t.Fatalf("legacy session invented terminal metadata: %#v", loaded.LastRunTerminal)
	}
	loaded.Title = "migrated"
	if err := store.SaveExpected(loaded, 0); err != nil {
		t.Fatalf("SaveExpected(legacy) = %v", err)
	}
	if loaded.Version != currentSessionVersion || loaded.Revision != 1 {
		t.Fatalf("migrated session = v%d/r%d", loaded.Version, loaded.Revision)
	}
	reloaded, err := store.Get(legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Version != currentSessionVersion || reloaded.Revision != 1 || reloaded.Title != "migrated" {
		t.Fatalf("reloaded migration = %#v", reloaded)
	}
}

func TestSessionStoreTerminalMetadataValidationAndRoundTrip(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	store := NewSessionStore(base)
	seed := &Session{Workspace: workspace, Messages: []SessionMessage{{Role: "user", Content: "run"}}}
	if err := store.Save(seed); err != nil {
		t.Fatal(err)
	}

	valid := DurableRunTerminalMetadata{
		TerminalState: EvidenceTerminalBlocked, CompletionStatus: CompletionStatusBlocked,
		CompletionRevision: 1, CompletionCriteria: 3, CompletionSatisfied: 1, CompletionBlocked: 1,
	}
	update, err := store.Get(seed.ID)
	if err != nil {
		t.Fatal(err)
	}
	update.LastRunTerminal = &valid
	if err := store.SaveExpected(update, update.Revision); err != nil {
		t.Fatalf("SaveExpected(valid terminal) = %v", err)
	}
	valid.CompletionSatisfied = 99
	loaded, err := store.Get(seed.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := DurableRunTerminalMetadata{
		TerminalState: EvidenceTerminalBlocked, CompletionStatus: CompletionStatusBlocked,
		CompletionRevision: 1, CompletionCriteria: 3, CompletionSatisfied: 1, CompletionBlocked: 1,
	}
	if loaded.LastRunTerminal == nil || *loaded.LastRunTerminal != want {
		t.Fatalf("loaded terminal = %#v, want %#v", loaded.LastRunTerminal, want)
	}
	resume, err := store.ResumeState(seed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resume.LastRunTerminal == nil || *resume.LastRunTerminal != want {
		t.Fatalf("resume terminal = %#v, want %#v", resume.LastRunTerminal, want)
	}
	resume.LastRunTerminal.CompletionBlocked = 99
	summaries := store.List(workspace)
	if len(summaries) != 1 || summaries[0].LastRunTerminal == nil || *summaries[0].LastRunTerminal != want {
		t.Fatalf("summary terminal = %#v", summaries)
	}

	invalid := []DurableRunTerminalMetadata{
		{},
		{TerminalState: "success"},
		{TerminalState: EvidenceTerminalVerified, CompletionRevision: 1},
		{TerminalState: EvidenceTerminalVerified, CompletionStatus: CompletionStatusComplete, CompletionRevision: 1, CompletionCriteria: maxCompletionCriteria + 1, CompletionSatisfied: maxCompletionCriteria + 1},
		{TerminalState: EvidenceTerminalVerified, CompletionStatus: CompletionStatusComplete, CompletionRevision: 1, CompletionCriteria: 2, CompletionSatisfied: 1},
		{TerminalState: EvidenceTerminalVerified, CompletionStatus: CompletionStatusIncomplete, CompletionCriteria: 2, CompletionSatisfied: 1},
		{TerminalState: EvidenceTerminalBlocked, CompletionStatus: CompletionStatusBlocked, CompletionRevision: 1, CompletionCriteria: 1},
	}
	for _, terminal := range invalid {
		candidate, err := store.Get(seed.ID)
		if err != nil {
			t.Fatal(err)
		}
		candidate.LastRunTerminal = &terminal
		if err := store.SaveExpected(candidate, candidate.Revision); !errors.Is(err, ErrSessionTerminalInvalid) {
			t.Errorf("SaveExpected(%#v) = %v, want ErrSessionTerminalInvalid", terminal, err)
		}
	}
	after, err := store.Get(seed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != 2 || after.LastRunTerminal == nil || *after.LastRunTerminal != want {
		t.Fatalf("invalid terminal attempts changed session: %#v", after)
	}
}

func TestSessionStoreRejectsCorruptPersistedTerminalMetadata(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	store := NewSessionStore(base)
	sess := &Session{Workspace: workspace, Title: "terminal canary"}
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}
	path := sessionTestPersistedPath(t, store, workspace, sess.ID)
	loaded, err := store.Get(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	loaded.LastRunTerminal = &DurableRunTerminalMetadata{
		TerminalState: EvidenceTerminalVerified, CompletionStatus: CompletionStatusComplete,
		CompletionRevision: 1, CompletionCriteria: 2, CompletionSatisfied: 1,
	}
	data, err := json.MarshalIndent(loaded, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = store.Get(sess.ID)
	if !errors.Is(err, ErrSessionCorrupt) {
		t.Fatalf("Get(corrupt terminal) = %v, want ErrSessionCorrupt", err)
	}
	if metadata, ok := SessionRecoveryFromError(err); !ok || metadata.SessionID != sess.ID || !sameStoragePath(metadata.SourcePath, path) {
		t.Fatalf("terminal recovery metadata = %#v, %v", metadata, ok)
	}
}

func TestSessionStoreRevisionOverflowPreservesOriginal(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	store := NewSessionStore(base)
	sess := &Session{Workspace: workspace, Title: "canary"}
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}
	path := sessionTestPersistedPath(t, store, workspace, sess.ID)
	loaded, err := store.Get(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	loaded.Revision = ^uint64(0)
	loaded.LastCommittedRevision = loaded.Revision
	data, err := json.MarshalIndent(loaded, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	loaded.Title = "must not persist"
	if err := store.SaveExpected(loaded, ^uint64(0)); !errors.Is(err, ErrSessionRevisionOverflow) {
		t.Fatalf("SaveExpected(overflow) = %v", err)
	}
	if err := store.Rename(sess.ID, "must not rename"); !errors.Is(err, ErrSessionRevisionOverflow) {
		t.Fatalf("Rename(overflow) = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("overflow attempt changed persisted canary")
	}
}

func TestSessionStoreCorruptAndUnsupportedRecoveryDoesNotBreakList(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	store := NewSessionStore(base)
	normal := &Session{Workspace: workspace, Title: "normal"}
	if err := store.Save(normal); err != nil {
		t.Fatal(err)
	}
	dir, err := store.workspaceDir(workspace)
	if err != nil {
		t.Fatal(err)
	}

	unsupportedID := opaqueTestSessionID("7")
	unsupported := Session{
		Version:   currentSessionVersion + 1,
		Revision:  1,
		ID:        unsupportedID,
		Workspace: workspace,
		Title:     "future",
	}
	unsupportedBytes, err := json.Marshal(unsupported)
	if err != nil {
		t.Fatal(err)
	}
	truncatedID := opaqueTestSessionID("a")
	semanticID := opaqueTestSessionID("b")
	semantic := Session{Version: currentSessionVersion, Revision: 0, ID: semanticID, Workspace: workspace}
	semanticBytes, err := json.Marshal(semantic)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		id   string
		data []byte
		kind error
	}{
		{unsupportedID, unsupportedBytes, ErrSessionVersionUnsupported},
		{truncatedID, []byte(`{"version":1,"revision":1`), ErrSessionCorrupt},
		{semanticID, semanticBytes, ErrSessionCorrupt},
	}
	for _, test := range cases {
		path := filepath.Join(dir, test.id+".json")
		if err := os.WriteFile(path, test.data, 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := store.Get(test.id)
		if !errors.Is(err, test.kind) {
			t.Errorf("Get(%s) = %v, want %v", test.id, err, test.kind)
		}
		metadata, ok := SessionRecoveryFromError(err)
		if !ok || metadata.SessionID != test.id || !sameStoragePath(metadata.SourcePath, path) {
			t.Errorf("recovery metadata for %s = %#v, %v", test.id, metadata, ok)
		}
		if metadata.QuarantinePath == "" || metadata.Quarantined {
			t.Errorf("quarantine metadata for %s = %#v", test.id, metadata)
		}
		recoveryRoot := filepath.Join(base, ".session-recovery")
		if err := ensureLexicallyContained(recoveryRoot, metadata.QuarantinePath); err != nil {
			t.Errorf("quarantine path escaped recovery root: %v", err)
		}
		if _, statErr := os.Stat(metadata.QuarantinePath); !os.IsNotExist(statErr) {
			t.Errorf("prospective quarantine path was unexpectedly written: %v", statErr)
		}
		after, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(after, test.data) {
			t.Errorf("source %s changed: %q, %v", test.id, after, readErr)
		}
		attempt := &Session{ID: test.id, Workspace: workspace, Title: "overwrite attempt"}
		if saveErr := store.Save(attempt); !errors.Is(saveErr, test.kind) {
			t.Errorf("Save(over corrupt %s) = %v, want %v", test.id, saveErr, test.kind)
		}
		afterSave, _ := os.ReadFile(path)
		if !bytes.Equal(afterSave, test.data) {
			t.Errorf("Save overwrote corrupt source %s", test.id)
		}
	}

	listed := store.List(workspace)
	if len(listed) != 1 || listed[0].ID != normal.ID {
		t.Fatalf("List with corrupt siblings = %#v, want only normal", listed)
	}
	all := store.ListAll()
	if len(all) != 1 || all[0].ID != normal.ID {
		t.Fatalf("ListAll with corrupt siblings = %#v, want only normal", all)
	}
}

func TestSessionStoreValidatesAndPreservesParentMetadata(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	store := NewSessionStore(base)
	parent := &Session{Workspace: workspace, Title: "parent"}
	if err := store.Save(parent); err != nil {
		t.Fatal(err)
	}
	child := &Session{
		Workspace:       workspace,
		Title:           "child",
		ParentSessionID: parent.ID,
		ParentRevision:  parent.Revision,
	}
	if err := store.Save(child); err != nil {
		t.Fatalf("Save(child) = %v", err)
	}
	loaded, err := store.Get(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	loaded.ParentSessionID = ""
	loaded.ParentRevision = 0
	if err := store.Save(loaded); err != nil {
		t.Fatalf("Save(old client omitting parent) = %v", err)
	}
	if loaded.ParentSessionID != parent.ID || loaded.ParentRevision != parent.Revision {
		t.Fatalf("parent metadata was not preserved: %#v", loaded)
	}

	otherParent := &Session{Workspace: workspace, Title: "other"}
	if err := store.Save(otherParent); err != nil {
		t.Fatal(err)
	}
	loaded.ParentSessionID = otherParent.ID
	loaded.ParentRevision = otherParent.Revision
	if err := store.Save(loaded); !errors.Is(err, ErrSessionParentInvalid) {
		t.Fatalf("Save(change parent) = %v, want ErrSessionParentInvalid", err)
	}

	invalid := []*Session{
		{Workspace: workspace, ParentSessionID: parent.ID},
		{Workspace: workspace, ParentRevision: 1},
		{Workspace: workspace, ParentSessionID: "not-a-session", ParentRevision: 1},
		{ID: opaqueTestSessionID("c"), Workspace: workspace, ParentSessionID: opaqueTestSessionID("c"), ParentRevision: 1},
		{Workspace: workspace, ParentSessionID: parent.ID, ParentRevision: parent.Revision + 1},
	}
	for index, candidate := range invalid {
		if err := store.Save(candidate); !errors.Is(err, ErrSessionParentInvalid) {
			t.Errorf("Save(invalid parent %d) = %v", index, err)
		}
	}
}

func sessionTestPersistedPath(t *testing.T, store *SessionStore, workspace, id string) string {
	t.Helper()
	dir, err := store.workspaceDir(workspace)
	if err != nil {
		t.Fatalf("workspaceDir = %v", err)
	}
	path, err := store.sessionPath(dir, id)
	if err != nil {
		t.Fatalf("sessionPath = %v", err)
	}
	return path
}

func writeLegacySession(t *testing.T, dir string, sess Session) {
	t.Helper()
	data, err := json.Marshal(&sess)
	if err != nil {
		t.Fatalf("Marshal(session) = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, sess.ID+".json"), data, 0o644); err != nil {
		t.Fatalf("WriteFile(session) = %v", err)
	}
}

func assertOnlySessionID(t *testing.T, sessions []SessionSummary, id string) {
	t.Helper()
	if len(sessions) != 1 || sessions[0].ID != id {
		t.Fatalf("sessions = %#v, want only %q", sessions, id)
	}
}
