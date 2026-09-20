package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestUndoCheckpointDetectsCurrentPostimageConflict(t *testing.T) {
	fixture := newCheckpointSecurityFixture(t)
	owner := checkpointSecurityOwner(fixture)
	createUndoTestCheckpoint(t, fixture, owner, map[string]string{
		"edited.txt": "before",
	}, map[string]string{
		"edited.txt": "agent edit",
	})
	if err := os.WriteFile(filepath.Join(fixture.workDir, "edited.txt"), []byte("user edit"), 0o600); err != nil {
		t.Fatal(err)
	}

	preview, err := inspectCheckpointUndo(fixture.workDir, fixture.sessionID, nil)
	if err != nil {
		t.Fatalf("inspect checkpoint: %v", err)
	}
	if len(preview) != 1 || preview[0].Status != undoEntryConflict {
		t.Fatalf("checkpoint preview = %#v; want one conflict", preview)
	}
	if reverted, ok, err := undoCheckpointSecure(fixture.workDir, fixture.sessionID); err == nil || ok || len(reverted) != 0 {
		t.Fatalf("conflicted undo = %#v, ok=%v, err=%v; want no restore", reverted, ok, err)
	}
	if content, err := os.ReadFile(filepath.Join(fixture.workDir, "edited.txt")); err != nil || string(content) != "user edit" {
		t.Fatalf("user edit = %q, err=%v; conflict was overwritten", content, err)
	}
	if _, _, found, err := readManifestStrict(fixture.workDir, fixture.sessionID); err != nil || !found {
		t.Fatalf("conflicted checkpoint was not preserved: found=%v err=%v", found, err)
	}
}

func TestUndoCheckpointSelectedRestorePreservesUnselectedConflict(t *testing.T) {
	fixture := newCheckpointSecurityFixture(t)
	owner := checkpointSecurityOwner(fixture)
	createUndoTestCheckpoint(t, fixture, owner, map[string]string{
		"safe.txt":     "safe before",
		"conflict.txt": "conflict before",
	}, map[string]string{
		"safe.txt":     "safe agent edit",
		"conflict.txt": "conflict agent edit",
	})
	if err := os.WriteFile(filepath.Join(fixture.workDir, "conflict.txt"), []byte("user edit"), 0o600); err != nil {
		t.Fatal(err)
	}

	listed := runUndoCommandTestTurn(t, fixture, "/undo --list", ExecutionModeReadOnly)
	listText := eventTextContent(listed)
	if !strings.Contains(listText, "safe.txt") || !strings.Contains(listText, "conflict.txt") ||
		!strings.Contains(listText, "복원 가능") || !strings.Contains(listText, "충돌") {
		t.Fatalf("undo list output = %q", listText)
	}
	var previewEvent Event
	for _, event := range listed {
		if event.Type == "undo_preview" {
			previewEvent = event
			break
		}
	}
	if previewEvent.Type != "undo_preview" {
		t.Fatalf("undo list did not emit structured preview: %#v", listed)
	}
	previewPayload, ok := previewEvent.Data.(map[string]any)
	if !ok || len(previewPayload["entries"].([]map[string]string)) != 2 {
		t.Fatalf("undo preview payload = %#v", previewEvent.Data)
	}

	manifest, _, found, err := readManifestStrict(fixture.workDir, fixture.sessionID)
	if err != nil || !found {
		t.Fatalf("checkpoint manifest unavailable: found=%v err=%v", found, err)
	}
	safeID := checkpointUndoEntryID(manifest.CheckpointKey, "safe.txt")
	all := runUndoCommandTestTurn(t, fixture, "/undo", ExecutionModeWorkspace)
	conflictID := checkpointUndoEntryID(manifest.CheckpointKey, "conflict.txt")
	if !strings.Contains(eventTextContent(all), conflictID) {
		t.Fatalf("whole-turn conflict output did not list conflict entry ID %q: %q", conflictID, eventTextContent(all))
	}
	if content, err := os.ReadFile(filepath.Join(fixture.workDir, "safe.txt")); err != nil || string(content) != "safe agent edit" {
		t.Fatalf("whole-turn conflict partially restored safe entry: %q err=%v", content, err)
	}

	selected := runUndoCommandTestTurn(t, fixture, "/undo --select "+safeID, ExecutionModeWorkspace)
	if !strings.Contains(eventTextContent(selected), "safe.txt") {
		t.Fatalf("selected restore output = %q", eventTextContent(selected))
	}
	var resultEvent Event
	for _, event := range selected {
		if event.Type == "undo_result" {
			resultEvent = event
			break
		}
	}
	if resultEvent.Type != "undo_result" {
		t.Fatalf("selected restore did not emit structured result: %#v", selected)
	}
	for path, want := range map[string]string{
		"safe.txt":     "safe before",
		"conflict.txt": "user edit",
	} {
		if content, err := os.ReadFile(filepath.Join(fixture.workDir, path)); err != nil || string(content) != want {
			t.Fatalf("%s = %q, err=%v; want %q", path, content, err, want)
		}
	}
	manifest, _, found, err = readManifestStrict(fixture.workDir, fixture.sessionID)
	if err != nil || !found || len(manifest.Files) != 1 {
		t.Fatalf("selective restore manifest = found:%v err:%v files:%#v", found, err, manifest.Files)
	}
	if _, remains := manifest.Files["conflict.txt"]; !remains {
		t.Fatalf("unselected conflict was pruned: %#v", manifest.Files)
	}
	selectedConflict := runUndoCommandTestTurn(t, fixture, "/undo --select "+checkpointUndoEntryID(manifest.CheckpointKey, "conflict.txt"), ExecutionModeWorkspace)
	if !strings.Contains(eventTextContent(selectedConflict), "user edit") && !strings.Contains(eventTextContent(selectedConflict), "conflict.txt") {
		t.Fatalf("selected conflict output did not identify the conflict: %q", eventTextContent(selectedConflict))
	}
	if content, err := os.ReadFile(filepath.Join(fixture.workDir, "conflict.txt")); err != nil || string(content) != "user edit" {
		t.Fatalf("selected conflict was overwritten: %q err=%v", content, err)
	}
}

func TestUndoCheckpointRejectsBackupThatDoesNotMatchPreimageDigest(t *testing.T) {
	fixture := newCheckpointSecurityFixture(t)
	owner := checkpointSecurityOwner(fixture)
	createUndoTestCheckpoint(t, fixture, owner, map[string]string{"victim.txt": "before"}, map[string]string{"victim.txt": "after"})
	manifest, paths, found, err := readManifestStrict(fixture.workDir, fixture.sessionID)
	if err != nil || !found {
		t.Fatalf("checkpoint manifest unavailable: found=%v err=%v", found, err)
	}
	entry := manifest.Files["victim.txt"]
	backupPath := filepath.Join(paths.dir, entry.Backup)
	if err := os.WriteFile(backupPath, []byte("substituted backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	if reverted, ok, err := undoCheckpointSecure(fixture.workDir, fixture.sessionID); err == nil || ok || len(reverted) != 0 {
		t.Fatalf("substituted backup undo = %#v, ok=%v, err=%v", reverted, ok, err)
	}
	if content, err := os.ReadFile(filepath.Join(fixture.workDir, "victim.txt")); err != nil || string(content) != "after" {
		t.Fatalf("backup rejection changed target: %q err=%v", content, err)
	}
}

func TestUndoCheckpointListKeepsSafeEntriesVisibleWhenBackupIsCorrupt(t *testing.T) {
	fixture := newCheckpointSecurityFixture(t)
	owner := checkpointSecurityOwner(fixture)
	createUndoTestCheckpoint(t, fixture, owner, map[string]string{
		"safe.txt":   "safe before",
		"broken.txt": "broken before",
	}, map[string]string{
		"safe.txt":   "safe agent edit",
		"broken.txt": "broken agent edit",
	})
	manifest, paths, found, err := readManifestStrict(fixture.workDir, fixture.sessionID)
	if err != nil || !found {
		t.Fatalf("checkpoint manifest unavailable: found=%v err=%v", found, err)
	}
	backupPath := filepath.Join(paths.dir, manifest.Files["broken.txt"].Backup)
	if err := os.WriteFile(backupPath, []byte("corrupt backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	listed := runUndoCommandTestTurn(t, fixture, "/undo --list", ExecutionModeReadOnly)
	listText := eventTextContent(listed)
	if !strings.Contains(listText, "safe.txt") || !strings.Contains(listText, "broken.txt") || !strings.Contains(listText, "복원 불가") {
		t.Fatalf("undo list did not isolate the corrupt entry: %q", listText)
	}
	safeID := checkpointUndoEntryID(manifest.CheckpointKey, "safe.txt")
	selected := runUndoCommandTestTurn(t, fixture, "/undo --select "+safeID, ExecutionModeWorkspace)
	if !strings.Contains(eventTextContent(selected), "safe.txt") {
		t.Fatalf("safe selective restore output = %q", eventTextContent(selected))
	}
	for path, want := range map[string]string{
		"safe.txt":   "safe before",
		"broken.txt": "broken agent edit",
	} {
		content, err := os.ReadFile(filepath.Join(fixture.workDir, path))
		if err != nil || string(content) != want {
			t.Fatalf("%s = %q, err=%v; want %q", path, content, err, want)
		}
	}
}

func TestUndoCheckpointRejectsTargetReplacedBySymlink(t *testing.T) {
	fixture := newCheckpointSecurityFixture(t)
	owner := checkpointSecurityOwner(fixture)
	createUndoTestCheckpoint(t, fixture, owner, map[string]string{"victim.txt": "before"}, map[string]string{"victim.txt": "after"})
	victim := filepath.Join(fixture.workDir, "victim.txt")
	canary := filepath.Join(fixture.workDir, "canary.txt")
	writeCheckpointSecurityFile(t, canary, "keep")
	if err := os.Remove(victim); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(canary, victim); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if reverted, ok, err := undoCheckpointSecure(fixture.workDir, fixture.sessionID); err == nil || ok || len(reverted) != 0 {
		t.Fatalf("symlink-replaced undo = %#v, ok=%v, err=%v", reverted, ok, err)
	}
	if content, err := os.ReadFile(canary); err != nil || string(content) != "keep" {
		t.Fatalf("symlink target changed: %q err=%v", content, err)
	}
}

func TestUndoCheckpointPreservesPreimagePermissions(t *testing.T) {
	fixture := newCheckpointSecurityFixture(t)
	owner := checkpointSecurityOwner(fixture)
	if err := startCheckpoint(fixture.workDir, owner); err != nil {
		t.Fatal(err)
	}
	wants := map[string]os.FileMode{
		"private.txt": 0o600,
		"executable":  0o751,
	}
	mutations := make([]committedFileMutation, 0, len(wants))
	preimageModes := make(map[string]os.FileMode, len(wants))
	for path, mode := range wants {
		absolute := filepath.Join(fixture.workDir, path)
		if err := os.WriteFile(absolute, []byte("before"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(absolute, mode); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(absolute)
		if err != nil {
			t.Fatal(err)
		}
		preimageModes[path] = info.Mode().Perm()
		if err := checkpointFile(fixture.workDir, path, absolute, owner); err != nil {
			t.Fatalf("capture %s: %v", path, err)
		}
		if err := os.WriteFile(absolute, []byte("after"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(absolute, 0o644); err != nil {
			t.Fatal(err)
		}
		revision, err := readLedgerFileRevision(absolute)
		if err != nil {
			t.Fatal(err)
		}
		mutations = append(mutations, committedFileMutation{Snapshot: fileMutationSnapshot{Path: absolute}, PostRevision: revision})
	}
	if err := recordCheckpointPostimages(fixture.workDir, owner, mutations); err != nil {
		t.Fatal(err)
	}
	if reverted, ok, err := undoCheckpointSecure(fixture.workDir, fixture.sessionID); err != nil || !ok || len(reverted) != len(wants) {
		t.Fatalf("undo = %#v, ok=%v, err=%v", reverted, ok, err)
	}
	for path, wantMode := range preimageModes {
		absolute := filepath.Join(fixture.workDir, path)
		info, err := os.Stat(absolute)
		if err != nil {
			t.Fatal(err)
		}
		content, err := os.ReadFile(absolute)
		if err != nil || string(content) != "before" {
			t.Fatalf("restored %s = %q, err=%v", path, content, err)
		}
		if info.Mode().Perm() != wantMode {
			t.Fatalf("restored %s mode = %04o, want original %04o", path, info.Mode().Perm(), wantMode)
		}
	}
}

func TestUndoCheckpointRechecksPostimageAfterInitialCheck(t *testing.T) {
	fixture := newCheckpointSecurityFixture(t)
	owner := checkpointSecurityOwner(fixture)
	createUndoTestCheckpoint(t, fixture, owner, map[string]string{"victim.txt": "before"}, map[string]string{"victim.txt": "agent edit"})
	manifest, paths, found, err := readManifestStrict(fixture.workDir, fixture.sessionID)
	if err != nil || !found {
		t.Fatalf("read checkpoint: found=%v err=%v", found, err)
	}
	actions, err := preflightUndoManifestSelected(fixture.canonical, paths, manifest, false, nil)
	if err != nil || len(actions) != 1 {
		t.Fatalf("preflight actions = %#v, err=%v", actions, err)
	}
	victim := filepath.Join(fixture.workDir, "victim.txt")
	err = checkpointAtomicWriteIfUndoStateWithHook(actions[0], fixture.canonical, actions[0].postimageExisted, actions[0].postimageDigest, actions[0].backup, actions[0].preimageMode, func() {
		if writeErr := os.WriteFile(victim, []byte("user edit after preflight"), 0o600); writeErr != nil {
			t.Errorf("write late user edit: %v", writeErr)
		}
	})
	if err == nil {
		t.Fatal("postimage change after the initial check was not rejected")
	}
	if content, err := os.ReadFile(victim); err != nil || string(content) != "user edit after preflight" {
		t.Fatalf("late user edit = %q, err=%v; undo overwrote it", content, err)
	}
}

func TestUndoCheckpointRejectsParentReplacedAfterPreflight(t *testing.T) {
	fixture := newCheckpointSecurityFixture(t)
	owner := checkpointSecurityOwner(fixture)
	createUndoTestCheckpoint(t, fixture, owner, map[string]string{"nested/victim.txt": "before"}, map[string]string{"nested/victim.txt": "agent edit"})
	manifest, paths, found, err := readManifestStrict(fixture.workDir, fixture.sessionID)
	if err != nil || !found {
		t.Fatalf("read checkpoint: found=%v err=%v", found, err)
	}
	actions, err := preflightUndoManifestSelected(fixture.canonical, paths, manifest, false, nil)
	if err != nil || len(actions) != 1 {
		t.Fatalf("preflight actions = %#v, err=%v", actions, err)
	}
	parent := filepath.Join(fixture.workDir, "nested")
	movedParent := filepath.Join(fixture.workDir, "nested-original")
	victim := filepath.Join(parent, "victim.txt")
	var hookErr error
	err = checkpointAtomicWriteIfUndoStateWithHook(actions[0], fixture.canonical, actions[0].postimageExisted, actions[0].postimageDigest, actions[0].backup, actions[0].preimageMode, func() {
		if hookErr = os.Rename(parent, movedParent); hookErr != nil {
			return
		}
		if hookErr = os.Mkdir(parent, 0o755); hookErr != nil {
			return
		}
		hookErr = os.WriteFile(victim, []byte("agent edit"), 0o600)
	})
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if err == nil {
		t.Fatal("parent directory replacement after the initial check was not rejected")
	}
	if content, err := os.ReadFile(victim); err != nil || string(content) != "agent edit" {
		t.Fatalf("replacement parent target = %q, err=%v; undo wrote through replaced parent", content, err)
	}
}

func createUndoTestCheckpoint(t *testing.T, fixture checkpointSecurityFixture, owner checkpointOwner, preimages, postimages map[string]string) map[string]ckptFile {
	t.Helper()
	if err := startCheckpoint(fixture.workDir, owner); err != nil {
		t.Fatalf("start checkpoint: %v", err)
	}
	for path, content := range preimages {
		absolute := filepath.Join(fixture.workDir, path)
		writeCheckpointSecurityFile(t, absolute, content)
		if err := checkpointFile(fixture.workDir, path, absolute, owner); err != nil {
			t.Fatalf("capture %s: %v", path, err)
		}
	}
	mutations := make([]committedFileMutation, 0, len(postimages))
	for path, content := range postimages {
		absolute := filepath.Join(fixture.workDir, path)
		if err := os.WriteFile(absolute, []byte(content), 0o600); err != nil {
			t.Fatalf("write postimage %s: %v", path, err)
		}
		revision, err := readLedgerFileRevision(absolute)
		if err != nil {
			t.Fatalf("read postimage %s: %v", path, err)
		}
		mutations = append(mutations, committedFileMutation{Snapshot: fileMutationSnapshot{Path: absolute}, PostRevision: revision})
	}
	if err := recordCheckpointPostimages(fixture.workDir, owner, mutations); err != nil {
		t.Fatalf("record postimages: %v", err)
	}
	manifest, _, found, err := readManifestStrict(fixture.workDir, owner.SessionID)
	if err != nil || !found {
		t.Fatalf("read checkpoint: found=%v err=%v", found, err)
	}
	return manifest.Files
}

func runUndoCommandTestTurn(t *testing.T, fixture checkpointSecurityFixture, command string, mode ExecutionMode) []Event {
	t.Helper()
	provider := &policyTestProvider{name: "unused", models: []types.ModelInfo{{ID: "model"}}}
	events := make(chan Event, 16)
	go RunLoopWithOptions(context.Background(), provider, "model", []types.Message{{Role: "user", Content: mustJSON(command)}}, fixture.workDir, RunOptions{
		SessionID:              "live-" + fixture.sessionID,
		DurableSessionID:       fixture.sessionID,
		SessionRevision:        1,
		ExecutionPolicyRequest: &ExecutionPolicyRequest{Mode: mode},
		EvidencePolicy:         EvidencePolicyConfig{Policy: EvidencePolicyOff},
		DisablePlugins:         true,
		DisableWorkspaceMCP:    true,
		PluginDirs:             []string{},
	}, events)
	var received []Event
	for event := range events {
		received = append(received, event)
	}
	if provider.lastRequest() != nil {
		t.Fatalf("undo command %q unexpectedly called provider", command)
	}
	return received
}

func eventTextContent(events []Event) string {
	var text strings.Builder
	for _, event := range events {
		if event.Type == "text" {
			if value, ok := event.Data.(string); ok {
				text.WriteString(value)
			}
		}
	}
	return text.String()
}
