package agent

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSessionStoreIsolatesRealWorkspacesWithSameBasename(t *testing.T) {
	base := t.TempDir()
	workspaceA := filepath.Join(base, "project-a", "app")
	workspaceB := filepath.Join(base, "project-b", "app")
	for _, workspace := range []string{workspaceA, workspaceB} {
		if err := os.MkdirAll(workspace, 0o755); err != nil {
			t.Fatalf("MkdirAll(%q) = %v", workspace, err)
		}
	}
	if filepath.Base(workspaceA) != filepath.Base(workspaceB) {
		t.Fatalf("test workspaces do not share a basename: %q and %q", workspaceA, workspaceB)
	}

	store := NewSessionStore(filepath.Join(base, "state"))
	sessionA := &Session{ID: opaqueTestSessionID("a"), Workspace: workspaceA, Title: "project A"}
	sessionB := &Session{ID: opaqueTestSessionID("b"), Workspace: workspaceB, Title: "project B"}
	if err := store.Save(sessionA); err != nil {
		t.Fatalf("Save(project A) = %v", err)
	}
	if err := store.Save(sessionB); err != nil {
		t.Fatalf("Save(project B) = %v", err)
	}

	assertOnlySessionID(t, store.List(workspaceA), sessionA.ID)
	assertOnlySessionID(t, store.List(workspaceB), sessionB.ID)
	for _, want := range []*Session{sessionA, sessionB} {
		got, err := store.Get(want.ID)
		if err != nil {
			t.Fatalf("Get(%q) = %v", want.ID, err)
		}
		if got.Title != want.Title || !sameWorkspace(got.Workspace, want.Workspace) {
			t.Fatalf("Get(%q) = workspace %q title %q, want workspace %q title %q", want.ID, got.Workspace, got.Title, want.Workspace, want.Title)
		}
	}
}

func TestSessionStoreTreatsDirectorySymlinkAsCanonicalWorkspace(t *testing.T) {
	base := t.TempDir()
	canonicalWorkspace := filepath.Join(base, "real", "project")
	if err := os.MkdirAll(canonicalWorkspace, 0o755); err != nil {
		t.Fatalf("MkdirAll(canonical workspace) = %v", err)
	}
	aliasWorkspace := filepath.Join(base, "project-alias")
	if err := os.Symlink(canonicalWorkspace, aliasWorkspace); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("directory symlink unavailable: %v", err)
		}
		t.Fatalf("Symlink(directory workspace) = %v", err)
	}

	canonicalKey, canonicalPath, err := workspaceStorageKey(canonicalWorkspace)
	if err != nil {
		t.Fatalf("workspaceStorageKey(canonical) = %v", err)
	}
	aliasKey, aliasPath, err := workspaceStorageKey(aliasWorkspace)
	if err != nil {
		t.Fatalf("workspaceStorageKey(alias) = %v", err)
	}
	if aliasKey != canonicalKey || !sameStoragePath(aliasPath, canonicalPath) {
		t.Fatalf("alias key/path = %q/%q, canonical = %q/%q", aliasKey, aliasPath, canonicalKey, canonicalPath)
	}

	store := NewSessionStore(filepath.Join(base, "state"))
	sessionViaAlias := &Session{ID: opaqueTestSessionID("c"), Workspace: aliasWorkspace, Title: "saved through alias"}
	sessionViaCanonical := &Session{ID: opaqueTestSessionID("d"), Workspace: canonicalWorkspace, Title: "saved through canonical path"}
	if err := store.Save(sessionViaAlias); err != nil {
		t.Fatalf("Save(alias workspace) = %v", err)
	}
	if err := store.Save(sessionViaCanonical); err != nil {
		t.Fatalf("Save(canonical workspace) = %v", err)
	}

	for _, workspace := range []string{canonicalWorkspace, aliasWorkspace} {
		sessions := store.List(workspace)
		if len(sessions) != 2 {
			t.Fatalf("List(%q) returned %d sessions, want both canonicalized sessions: %#v", workspace, len(sessions), sessions)
		}
		seen := map[string]bool{}
		for _, session := range sessions {
			seen[session.ID] = true
		}
		if !seen[sessionViaAlias.ID] || !seen[sessionViaCanonical.ID] {
			t.Fatalf("List(%q) IDs = %#v, want %q and %q", workspace, seen, sessionViaAlias.ID, sessionViaCanonical.ID)
		}
	}

	loaded, err := store.Get(sessionViaAlias.ID)
	if err != nil {
		t.Fatalf("Get(alias-created session) = %v", err)
	}
	if !sameStoragePath(loaded.Workspace, canonicalPath) {
		t.Fatalf("persisted workspace = %q, want canonical %q", loaded.Workspace, canonicalPath)
	}
}

func TestSessionStorePreservesOriginalWhenAtomicWriteFails(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "project")
	store := NewSessionStore(t.TempDir())
	session := &Session{ID: opaqueTestSessionID("e"), Workspace: workspace, Title: "original"}
	if err := store.Save(session); err != nil {
		t.Fatalf("Save(original) = %v", err)
	}
	dir, err := store.workspaceDir(workspace)
	if err != nil {
		t.Fatalf("workspaceDir = %v", err)
	}
	path, err := store.sessionPath(dir, session.ID)
	if err != nil {
		t.Fatalf("sessionPath = %v", err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(original) = %v", err)
	}
	store.atomicWriter = func(string, string, []byte) error {
		return errors.New("injected atomic write failure")
	}
	if err := store.Save(&Session{ID: session.ID, Title: "replacement must fail"}); err == nil {
		t.Fatal("Save(replacement) = nil, want injected write failure")
	}
	afterFailure, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(after failure) = %v", err)
	}
	if !bytes.Equal(afterFailure, original) {
		t.Fatal("failed atomic write changed the original session bytes")
	}
	loaded, err := store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(after failed write) = %v", err)
	}
	if loaded.Title != "original" {
		t.Fatalf("Get(after failed write).Title = %q, want original", loaded.Title)
	}
}
