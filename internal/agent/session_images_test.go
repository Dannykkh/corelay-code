package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionImageOwnershipForkAndDeleteLifecycle(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	store := NewSessionStore(base)
	imageBytes := sessionImagePNGFixture(t)

	parent := &Session{Workspace: workspace, Messages: []SessionMessage{{Role: "user", Content: "inspect"}}}
	if err := store.Save(parent); err != nil {
		t.Fatal(err)
	}
	parentMemory, _, err := store.OpenToolResultMemory(parent.ID, workspace)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := parentMemory.StoreImage("image/png", imageBytes)
	if err != nil {
		t.Fatal(err)
	}
	parent.Messages[0].Attachments = []SessionImageReference{reference}
	if err := store.SaveExpected(parent, parent.Revision); err != nil {
		t.Fatalf("save owned image reference: %v", err)
	}
	transcript, err := json.Marshal(parent.Messages)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(transcript, imageBytes) {
		t.Fatalf("image bytes leaked into transcript: %s", transcript)
	}

	foreign := &Session{Workspace: workspace, Messages: []SessionMessage{{
		Role: "user", Content: "foreign", Attachments: []SessionImageReference{reference},
	}}}
	if err := store.Save(foreign); !errors.Is(err, ErrSessionImageReferenceInvalid) {
		t.Fatalf("save foreign session reference = %v, want ownership rejection", err)
	}

	child, err := store.Fork(parent.ID, parent.Revision)
	if err != nil {
		t.Fatalf("fork image session: %v", err)
	}
	if len(child.Messages[0].Attachments) != 1 || child.Messages[0].Attachments[0] != reference {
		t.Fatalf("forked refs = %+v, want parent ref %+v", child.Messages[0].Attachments, reference)
	}
	childMemory, _, err := store.OpenToolResultMemory(child.ID, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := childMemory.LoadImage(reference); err != nil || !bytes.Equal(got, imageBytes) {
		t.Fatalf("child image load = %d bytes, %v", len(got), err)
	}

	parentDeleted, err := store.DeleteExpected(parent.ID, parent.Revision)
	if err != nil {
		t.Fatalf("delete parent session: %v", err)
	}
	if parentDeleted.CleanupPending || parentDeleted.ImageCount != 1 || parentDeleted.ImageBytes != int64(len(imageBytes)) {
		t.Fatalf("parent image cleanup result = %+v", parentDeleted)
	}
	if got, err := childMemory.LoadImage(reference); err != nil || !bytes.Equal(got, imageBytes) {
		t.Fatalf("child image after parent delete = %d bytes, %v", len(got), err)
	}
	childDeleted, err := store.DeleteExpected(child.ID, child.Revision)
	if err != nil {
		t.Fatalf("delete child session: %v", err)
	}
	if childDeleted.CleanupPending || childDeleted.ImageCount != 1 || childDeleted.ImageBytes != int64(len(imageBytes)) {
		t.Fatalf("child image cleanup result = %+v", childDeleted)
	}
}

func TestSessionImageReferenceFailsClosedWhenBlobIsCorrupt(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	store := NewSessionStore(base)
	imageBytes := sessionImagePNGFixture(t)
	session := &Session{Workspace: workspace, Messages: []SessionMessage{{Role: "user", Content: "inspect"}}}
	if err := store.Save(session); err != nil {
		t.Fatal(err)
	}
	memory, _, err := store.OpenToolResultMemory(session.ID, workspace)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := memory.StoreImage("image/png", imageBytes)
	if err != nil {
		t.Fatal(err)
	}
	session.Messages[0].Attachments = []SessionImageReference{reference}
	if err := store.SaveExpected(session, session.Revision); err != nil {
		t.Fatal(err)
	}

	path, err := memory.imageBlobPath(strings.TrimPrefix(reference.Digest, "sha256:"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.LoadImage(reference); err == nil {
		t.Fatal("LoadImage accepted a corrupt content-addressed blob")
	}
	loaded, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveExpected(loaded, loaded.Revision); !errors.Is(err, ErrSessionImageReferenceInvalid) {
		t.Fatalf("SaveExpected accepted a corrupt referenced image: %v", err)
	}
}

func TestSessionStoreMigratesVersionThreeBeforeImageSchema(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	store := NewSessionStore(base)
	dir, err := store.workspaceDir(workspace)
	if err != nil {
		t.Fatal(err)
	}
	legacy := Session{
		Version: planRevisionSessionVersion, Revision: 7,
		ID: opaqueTestSessionID("j"), Workspace: workspace,
		WorkstreamID: "ws-roadmap", PlanID: "plan-01", PlanRevision: 3, StageID: "stage-02",
		LifecycleStatus: SessionLifecycleActive, LastCommittedRevision: 7,
		Messages: []SessionMessage{{Role: "user", Content: "pre-image schema"}},
	}
	writeLegacySession(t, dir, legacy)
	loaded, err := store.Get(legacy.ID)
	if err != nil {
		t.Fatalf("Get(v3) = %v", err)
	}
	if loaded.Version != planRevisionSessionVersion || loaded.PlanRevision != 3 {
		t.Fatalf("loaded v3 session = v%d plan revision %d", loaded.Version, loaded.PlanRevision)
	}
	loaded.Title = "migrated to image schema"
	if err := store.SaveExpected(loaded, loaded.Revision); err != nil {
		t.Fatalf("SaveExpected(v3→v4) = %v", err)
	}
	if loaded.Version != currentSessionVersion || loaded.Revision != 8 {
		t.Fatalf("migrated session = v%d/r%d", loaded.Version, loaded.Revision)
	}
}

func sessionImagePNGFixture(t *testing.T) []byte {
	t.Helper()
	fixture := image.NewRGBA(image.Rect(0, 0, 2, 2))
	fixture.Set(0, 0, color.RGBA{R: 210, G: 80, B: 30, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, fixture); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}
