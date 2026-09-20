package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestRunLoopCheckpointStoresSessionRunAndCommittedDigests(t *testing.T) {
	configDir := isolateEvidenceLoopTest(t)
	t.Setenv("ANICLEW_CONFIG_DIR", configDir)
	workDir := t.TempDir()
	target := filepath.Join(workDir, "checkpoint-target.txt")
	writeCheckpointSecurityFile(t, target, "before")

	firstSession := "durable-checkpoint-session-a"
	firstEvents := runCheckpointTestTurn(t, workDir, firstSession, 31, "first", "run first turn")
	if _, _, exists, err := readManifestStrict(workDir, firstSession); err != nil || !exists {
		t.Fatalf("first checkpoint manifest unavailable: exists=%v err=%v\nevents:\n%s", exists, err, eventDump(firstEvents))
	}
	firstManifest, _, _, err := readManifestStrict(workDir, firstSession)
	if err != nil {
		t.Fatalf("read first checkpoint manifest: %v", err)
	}
	if firstManifest.SessionDigest != checkpointSessionDigest(firstSession) || firstManifest.SessionRevision != 31 || firstManifest.RunID == "" ||
		firstManifest.Generation != "checkpoint_"+firstManifest.RunID ||
		firstManifest.CheckpointKey != checkpointManifestKey(firstManifest.WorkDir, firstManifest.SessionDigest, firstManifest.RunID, firstManifest.Generation) {
		t.Fatalf("first checkpoint identity = %+v", firstManifest)
	}
	firstEntry, ok := firstManifest.Files["checkpoint-target.txt"]
	if !ok {
		t.Fatalf("first checkpoint files = %+v", firstManifest.Files)
	}
	if firstEntry.PreimageDigest != artifactBytesRevision([]byte("before")) ||
		firstEntry.PostimageDigest != artifactBytesRevision([]byte("first-final")) || !firstEntry.PostimageCaptured {
		t.Fatalf("first checkpoint digests = %+v", firstEntry)
	}

	secondSession := "durable-checkpoint-session-b"
	secondEvents := runCheckpointTestTurn(t, workDir, secondSession, 32, "second", "run second turn")
	secondManifest, _, found, err := readManifestStrict(workDir, secondSession)
	if err != nil || !found {
		t.Fatalf("second checkpoint manifest unavailable: found=%v err=%v\nevents:\n%s", found, err, eventDump(secondEvents))
	}
	secondEntry := secondManifest.Files["checkpoint-target.txt"]
	if secondManifest.SessionDigest != checkpointSessionDigest(secondSession) || secondManifest.SessionRevision != 32 ||
		secondManifest.RunID == "" || secondManifest.RunID == firstManifest.RunID ||
		secondEntry.PreimageDigest != artifactBytesRevision([]byte("first-final")) ||
		secondEntry.PostimageDigest != artifactBytesRevision([]byte("second-final")) || !secondEntry.PostimageCaptured {
		t.Fatalf("second checkpoint identity/entry = %+v / %+v", secondManifest, secondEntry)
	}

	firstManifestAfterSecond, _, found, err := readManifestStrict(workDir, firstSession)
	if err != nil || !found {
		t.Fatalf("first session checkpoint was overwritten: found=%v err=%v", found, err)
	}
	if firstManifestAfterSecond.RunID != firstManifest.RunID ||
		firstManifestAfterSecond.Files["checkpoint-target.txt"].PostimageDigest != artifactBytesRevision([]byte("first-final")) {
		t.Fatalf("first session checkpoint changed after second session: %+v", firstManifestAfterSecond)
	}
	thirdEvents := runCheckpointTestTurn(t, workDir, firstSession, 33, "third", "run another first-session turn")
	thirdManifest, _, found, err := readManifestStrict(workDir, firstSession)
	if err != nil || !found {
		t.Fatalf("new first-session generation unavailable: found=%v err=%v\nevents:\n%s", found, err, eventDump(thirdEvents))
	}
	if thirdManifest.RunID == firstManifest.RunID || thirdManifest.CheckpointKey == firstManifest.CheckpointKey ||
		thirdManifest.CheckpointKey != checkpointManifestKey(thirdManifest.WorkDir, thirdManifest.SessionDigest, thirdManifest.RunID, thirdManifest.Generation) {
		t.Fatalf("new first-session checkpoint key was not generation-bound: old=%+v new=%+v", firstManifest, thirdManifest)
	}
	secondManifestAfterThird, _, found, err := readManifestStrict(workDir, secondSession)
	if err != nil || !found || secondManifestAfterThird.RunID != secondManifest.RunID {
		t.Fatalf("first-session new generation overwrote second session: found=%v err=%v manifest=%+v", found, err, secondManifestAfterThird)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "third-final" {
		t.Fatalf("target = %q, err = %v; want latest third-final", data, err)
	}

}

func TestRunLoopEphemeralCheckpointsAreIsolatedByLiveSession(t *testing.T) {
	configDir := isolateEvidenceLoopTest(t)
	t.Setenv("ANICLEW_CONFIG_DIR", configDir)
	workDir := t.TempDir()
	target := filepath.Join(workDir, "checkpoint-target.txt")
	writeCheckpointSecurityFile(t, target, "before")
	firstEvents := runEphemeralCheckpointTestTurn(t, workDir, "live-session-a", 0, "first", "run first ephemeral turn")
	firstManifest, _, found, err := readManifestStrict(workDir, "live-session-a")
	if err != nil || !found {
		t.Fatalf("first ephemeral checkpoint unavailable: found=%v err=%v\nevents:\n%s", found, err, eventDump(firstEvents))
	}
	secondEvents := runEphemeralCheckpointTestTurn(t, workDir, "live-session-b", 0, "second", "run second ephemeral turn")
	secondManifest, _, found, err := readManifestStrict(workDir, "live-session-b")
	if err != nil || !found {
		t.Fatalf("second ephemeral checkpoint unavailable: found=%v err=%v\nevents:\n%s", found, err, eventDump(secondEvents))
	}
	firstAfterSecond, _, found, err := readManifestStrict(workDir, "live-session-a")
	if err != nil || !found || firstAfterSecond.RunID != firstManifest.RunID {
		t.Fatalf("second ephemeral session overwrote first checkpoint: found=%v err=%v first=%+v second=%+v", found, err, firstManifest, firstAfterSecond)
	}
	if secondManifest.SessionDigest != checkpointSessionDigest("live-session-b") {
		t.Fatalf("ephemeral session digest = %q", secondManifest.SessionDigest)
	}
}

func TestCheckpointPostimageUsesFinalRevisionForRepeatedPathBatch(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CORELAY_CONFIG_DIR", stateDir)
	t.Setenv("ANICLEW_CONFIG_DIR", "")
	workDir := t.TempDir()
	target := filepath.Join(workDir, "same-path.txt")
	writeCheckpointSecurityFile(t, target, "before")
	owner := newCheckpointOwner("durable-batch-session", 8, "run_same_path_batch")
	if err := startCheckpoint(workDir, owner); err != nil {
		t.Fatalf("start checkpoint: %v", err)
	}
	if err := checkpointFile(workDir, "same-path.txt", target, owner); err != nil {
		t.Fatalf("capture preimage: %v", err)
	}
	ledger := NewReadLedger(workDir)
	if err := ledger.RecordRead(target); err != nil {
		t.Fatalf("record initial read: %v", err)
	}
	calls := []toolUseBlock{
		dispatchTestCall("same-path-first", "Write", map[string]any{"file_path": "same-path.txt", "content": "intermediate"}),
		dispatchTestCall("same-path-second", "Write", map[string]any{"file_path": "same-path.txt", "content": "final"}),
	}
	results := dispatchToolCalls(calls, toolDispatchOptions{
		Context:          context.Background(),
		WorkDir:          workDir,
		AllowedTools:     toolCatalogNames([]types.ToolDef{currentBuiltInToolDefinitions()["Write"]}),
		PermissionConfig: dispatchPermissionConfig("moderate"),
		ReadBeforeWrite:  true,
		ReadLedger:       ledger,
		SnapshotDecision: func(toolUseBlock) string { return "allow" },
		BeforeExecute: func(call toolUseBlock) (toolMutationPreview, error) {
			path := editFilePath(call.Input)
			return toolMutationPreview{File: path, CheckpointCaptured: true}, checkpointFile(workDir, path, resolvePath(path, workDir), owner)
		},
		MutationBatchCommitted: func(mutations []committedFileMutation) error {
			return recordCheckpointPostimages(workDir, owner, mutations)
		},
		Execute: func(call toolUseBlock) (string, bool) {
			return ExecuteTool(call.Name, call.Input, workDir)
		},
	})
	if len(results) != 2 || results[0].IsError || results[1].IsError {
		t.Fatalf("same-path batch results = %#v", results)
	}
	manifest, _, found, err := readManifestStrict(workDir, owner.SessionID)
	if err != nil || !found {
		t.Fatalf("checkpoint manifest unavailable: found=%v err=%v", found, err)
	}
	entry := manifest.Files["same-path.txt"]
	if entry.PreimageDigest != artifactBytesRevision([]byte("before")) ||
		entry.PostimageDigest != artifactBytesRevision([]byte("final")) || !entry.PostimageCaptured {
		t.Fatalf("same-path batch checkpoint entry = %+v", entry)
	}
}

func TestCheckpointCapturesExternalFullModeMutationTarget(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CORELAY_CONFIG_DIR", stateDir)
	t.Setenv("ANICLEW_CONFIG_DIR", "")
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	externalPath := filepath.Join(root, "external", "target.txt")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeCheckpointSecurityFile(t, externalPath, "before external")
	owner := newCheckpointOwner("durable-external-session", 12, "run_external_checkpoint")
	if err := startCheckpoint(workDir, owner); err != nil {
		t.Fatalf("start checkpoint: %v", err)
	}
	if err := checkpointFile(workDir, externalPath, externalPath, owner); err != nil {
		t.Fatalf("capture external preimage: %v", err)
	}
	writeCheckpointSecurityFile(t, externalPath, "after external")
	revision, err := readLedgerFileRevision(externalPath)
	if err != nil {
		t.Fatalf("read external postimage: %v", err)
	}
	if err := recordCheckpointPostimages(workDir, owner, []committedFileMutation{{
		Snapshot: fileMutationSnapshot{Path: externalPath}, PostRevision: revision,
	}}); err != nil {
		t.Fatalf("record external postimage: %v", err)
	}
	manifest, _, found, err := readManifestStrict(workDir, owner.SessionID)
	if err != nil || !found {
		t.Fatalf("external checkpoint manifest unavailable: found=%v err=%v", found, err)
	}
	key, err := checkpointTargetKey(workDir, externalPath)
	if err != nil {
		t.Fatalf("external checkpoint key: %v", err)
	}
	canonicalExternal, err := canonicalizeTarget(externalPath)
	if err != nil {
		t.Fatalf("canonicalize external target: %v", err)
	}
	entry := manifest.Files[key]
	if key == "" || entry.TargetPath == "" || !sameCheckpointPath(entry.TargetPath, canonicalExternal) ||
		entry.PreimageDigest != artifactBytesRevision([]byte("before external")) ||
		entry.PostimageDigest != artifactBytesRevision([]byte("after external")) || !entry.PostimageCaptured {
		t.Fatalf("external checkpoint entry = %+v, key=%q", entry, key)
	}
}

func TestUndoCheckpointRequiresCurrentFullModeForExternalTarget(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CORELAY_CONFIG_DIR", stateDir)
	t.Setenv("ANICLEW_CONFIG_DIR", "")
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	target := filepath.Join(root, "external", "policy-target.txt")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeCheckpointSecurityFile(t, target, "before")
	owner := newCheckpointOwner("external-policy-session", 15, "run_external_policy")
	if err := startCheckpoint(workDir, owner); err != nil {
		t.Fatalf("start checkpoint: %v", err)
	}
	if err := checkpointFile(workDir, target, target, owner); err != nil {
		t.Fatalf("capture external preimage: %v", err)
	}
	writeCheckpointSecurityFile(t, target, "after")
	revision, err := readLedgerFileRevision(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := recordCheckpointPostimages(workDir, owner, []committedFileMutation{{
		Snapshot: fileMutationSnapshot{Path: target}, PostRevision: revision,
	}}); err != nil {
		t.Fatalf("record external postimage: %v", err)
	}
	workspacePolicy := &ExecutionPolicySnapshot{Mode: ExecutionModeWorkspace}
	if _, ok, err := undoCheckpointSecureWithPolicy(workDir, owner.SessionID, workspacePolicy); err == nil || ok {
		t.Fatalf("workspace-mode undo restored an external target: ok=%v err=%v", ok, err)
	}
	if content, err := os.ReadFile(target); err != nil || string(content) != "after" {
		t.Fatalf("workspace-mode refusal changed target: %q, err=%v", content, err)
	}
	fullPolicy := &ExecutionPolicySnapshot{Mode: ExecutionModeFull, Revision: 4, FullSelectionRevision: 4, Source: "user-selected"}
	if reverted, ok, err := undoCheckpointSecureWithPolicy(workDir, owner.SessionID, fullPolicy); err != nil || !ok || len(reverted) != 1 {
		t.Fatalf("full-mode undo = %#v, ok=%v, err=%v", reverted, ok, err)
	}
	if content, err := os.ReadFile(target); err != nil || string(content) != "before" {
		t.Fatalf("full-mode undo content = %q, err=%v", content, err)
	}
}

func TestCheckpointRejectsExternalRepositoryControlTargetAboveNestedWorkspace(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CORELAY_CONFIG_DIR", stateDir)
	t.Setenv("ANICLEW_CONFIG_DIR", "")
	root := t.TempDir()
	workDir := filepath.Join(root, "repository", "nested")
	protectedTarget := filepath.Join(root, "repository", ".git", "config")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeCheckpointSecurityFile(t, protectedTarget, "git config")
	owner := newCheckpointOwner("durable-nested-repository-session", 13, "run_nested_repository_checkpoint")
	if err := startCheckpoint(workDir, owner); err != nil {
		t.Fatalf("start checkpoint: %v", err)
	}
	if err := checkpointFile(workDir, protectedTarget, protectedTarget, owner); err == nil || !strings.Contains(err.Error(), `protected control directory ".git"`) {
		t.Fatalf("external repository-control capture error = %v", err)
	}
	manifest, _, found, err := readManifestStrict(workDir, owner.SessionID)
	if err != nil || !found {
		t.Fatalf("checkpoint manifest unavailable: found=%v err=%v", found, err)
	}
	if len(manifest.Files) != 0 {
		t.Fatalf("protected target entered checkpoint manifest: %+v", manifest.Files)
	}
}

func TestCheckpointScopeSerializesConcurrentWorkerCaptures(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CORELAY_CONFIG_DIR", stateDir)
	t.Setenv("ANICLEW_CONFIG_DIR", "")
	workDir := t.TempDir()
	owner := newCheckpointOwner("parallel-checkpoint-session", 14, "run_parallel_checkpoint")
	scope := newCheckpointScope(owner)
	files := map[string]string{"worker-a.txt": "after-a", "worker-b.txt": "after-b"}
	for name := range files {
		writeCheckpointSecurityFile(t, filepath.Join(workDir, name), "before-"+name)
	}
	start := make(chan struct{})
	errs := make(chan error, len(files))
	var workers sync.WaitGroup
	for name, after := range files {
		name, after := name, after
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			path := filepath.Join(workDir, name)
			if err := scope.captureBeforeMutation(workDir, name, path); err != nil {
				errs <- err
				return
			}
			if err := os.WriteFile(path, []byte(after), 0o600); err != nil {
				errs <- err
				return
			}
			revision, err := readLedgerFileRevision(path)
			if err != nil {
				errs <- err
				return
			}
			errs <- scope.recordPostimages(workDir, []committedFileMutation{{
				Snapshot: fileMutationSnapshot{Path: path}, PostRevision: revision,
			}})
		}()
	}
	close(start)
	workers.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("parallel checkpoint worker: %v", err)
		}
	}
	manifest, _, found, err := readManifestStrict(workDir, owner.SessionID)
	if err != nil || !found {
		t.Fatalf("parallel checkpoint manifest unavailable: found=%v err=%v", found, err)
	}
	for name, after := range files {
		entry, ok := manifest.Files[name]
		if !ok || entry.PreimageDigest != artifactBytesRevision([]byte("before-"+name)) ||
			entry.PostimageDigest != artifactBytesRevision([]byte(after)) || !entry.PostimageCaptured {
			t.Fatalf("parallel checkpoint entry %q = %+v", name, entry)
		}
	}
}

func TestFailedMutationBatchSettlesCheckpointAndPreservesPriorEntries(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CORELAY_CONFIG_DIR", stateDir)
	t.Setenv("ANICLEW_CONFIG_DIR", "")
	workDir := t.TempDir()
	targetA := filepath.Join(workDir, "successful.txt")
	targetB := filepath.Join(workDir, "failed.txt")
	writeCheckpointSecurityFile(t, targetA, "before-a")
	writeCheckpointSecurityFile(t, targetB, "before-b")
	owner := newCheckpointOwner("settled-checkpoint-session", 16, "run_settled_checkpoint")
	scope := newCheckpointScope(owner)
	ledger := NewReadLedger(workDir)
	if err := ledger.RecordRead(targetA); err != nil {
		t.Fatal(err)
	}
	if err := ledger.RecordRead(targetB); err != nil {
		t.Fatal(err)
	}
	dispatch := func(call toolUseBlock, execute func(toolUseBlock) (string, bool)) []toolDispatchResult {
		return dispatchToolCalls([]toolUseBlock{call}, toolDispatchOptions{
			Context:          context.Background(),
			WorkDir:          workDir,
			AllowedTools:     dispatchAllowedTools("Write"),
			PermissionConfig: dispatchPermissionConfig("moderate"),
			ReadBeforeWrite:  true,
			ReadLedger:       ledger,
			SnapshotDecision: func(toolUseBlock) string { return "allow" },
			BeforeExecute: func(call toolUseBlock) (toolMutationPreview, error) {
				path := editFilePath(call.Input)
				err := scope.captureBeforeMutation(workDir, path, resolvePath(path, workDir))
				return toolMutationPreview{File: path, CheckpointCaptured: err == nil}, err
			},
			MutationBatchCommitted: func(mutations []committedFileMutation) error {
				return scope.recordPostimages(workDir, mutations)
			},
			MutationBatchSettled: func(paths []string) error {
				return scope.settleFailedBatch(workDir, paths)
			},
			Execute: execute,
		})
	}
	first := dispatch(dispatchTestCall("successful-write", "Write", map[string]any{
		"file_path": "successful.txt", "content": "after-a",
	}), func(call toolUseBlock) (string, bool) { return ExecuteTool(call.Name, call.Input, workDir) })
	if len(first) != 1 || first[0].IsError {
		t.Fatalf("first mutation = %#v", first)
	}
	second := dispatch(dispatchTestCall("failed-write", "Write", map[string]any{
		"file_path": "failed.txt", "content": "after-b",
	}), func(toolUseBlock) (string, bool) { return "simulated write failure", true })
	if len(second) != 1 || !second[0].IsError {
		t.Fatalf("failed mutation = %#v", second)
	}
	manifest, _, found, err := readManifestStrict(workDir, owner.SessionID)
	if err != nil || !found {
		t.Fatalf("settled checkpoint unavailable: found=%v err=%v", found, err)
	}
	entryA, okA := manifest.Files["successful.txt"]
	entryB, okB := manifest.Files["failed.txt"]
	if !okA || !entryA.PostimageCaptured || entryA.PostimageDigest != artifactBytesRevision([]byte("after-a")) ||
		!okB || !entryB.PostimageCaptured || entryB.PostimageDigest != artifactBytesRevision([]byte("before-b")) {
		t.Fatalf("checkpoint entries after failed later batch: a=%+v b=%+v", entryA, entryB)
	}
	if reverted, ok, err := undoCheckpointSecure(workDir, owner.SessionID); err != nil || !ok || len(reverted) != 1 {
		t.Fatalf("undo after failed batch = %#v, ok=%v, err=%v; only the earlier committed file should be restored", reverted, ok, err)
	}
	for path, want := range map[string]string{targetA: "before-a", targetB: "before-b"} {
		if content, err := os.ReadFile(path); err != nil || string(content) != want {
			t.Fatalf("restored %s = %q, err=%v; want %q", path, content, err, want)
		}
	}
}

func TestFailedMutationBatchDoesNotSettleAbsentSentinelContentFile(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CORELAY_CONFIG_DIR", stateDir)
	t.Setenv("ANICLEW_CONFIG_DIR", "")
	workDir := t.TempDir()
	target := filepath.Join(workDir, "sentinel.txt")
	owner := newCheckpointOwner("settled-absent-sentinel-session", 1, "run_settled_absent_sentinel")
	scope := newCheckpointScope(owner)
	if err := scope.captureBeforeMutation(workDir, "sentinel.txt", target); err != nil {
		t.Fatalf("capture absent preimage: %v", err)
	}
	if err := os.WriteFile(target, []byte("corelay-checkpoint-absent-v1"), 0o600); err != nil {
		t.Fatalf("write unrolled-back sentinel file: %v", err)
	}
	if err := scope.settleFailedBatch(workDir, []string{"sentinel.txt"}); err == nil {
		t.Fatal("settlement accepted an existing file as an absent preimage")
	}
	manifest, _, found, err := readManifestStrict(workDir, owner.SessionID)
	if err != nil || !found {
		t.Fatalf("checkpoint manifest unavailable: found=%v err=%v", found, err)
	}
	entry, ok := manifest.Files["sentinel.txt"]
	if !ok || entry.PostimageCaptured {
		t.Fatalf("unsafe checkpoint settlement captured sentinel state: %+v", entry)
	}
	if content, err := os.ReadFile(target); err != nil || string(content) != "corelay-checkpoint-absent-v1" {
		t.Fatalf("sentinel file changed after refused settlement: content=%q err=%v", content, err)
	}
}

func TestCheckpointSettlementRejectsUncapturedMutationPath(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CORELAY_CONFIG_DIR", stateDir)
	t.Setenv("ANICLEW_CONFIG_DIR", "")
	workDir := t.TempDir()
	owner := newCheckpointOwner("settled-missing-preimage-session", 1, "run_settled_missing_preimage")
	scope := newCheckpointScope(owner)
	capturedPath := filepath.Join(workDir, "captured.txt")
	if err := scope.captureBeforeMutation(workDir, "captured.txt", capturedPath); err != nil {
		t.Fatalf("capture mutation preimage: %v", err)
	}
	if err := scope.settleFailedBatch(workDir, []string{"uncaptured.txt"}); err == nil {
		t.Fatal("settlement accepted a mutation without a captured preimage")
	}
}

func TestSubAgentWorkersShareOneCheckpointGeneration(t *testing.T) {
	configDir := isolateEvidenceLoopTest(t)
	t.Setenv("ANICLEW_CONFIG_DIR", configDir)
	t.Setenv("CORELAY_MEMORY", "off")
	t.Setenv("CORELAY_AUTOSKILL", "off")
	workDir := t.TempDir()
	checkpointWorkerFixtures(t, workDir)
	provider := newConcurrentCheckpointWorkerProvider()
	runner := &fakeBashRunner{name: "checkpoint-subagents", capabilities: fakeBashCapabilities()}
	scope := newCheckpointScope(newCheckpointOwner("subagent-checkpoint-session", 2, "run_subagent_group"))
	manager := NewSubAgentManagerWithOptions(provider, "fake-model", workDir, SubAgentManagerOptions{
		SessionID:       "subagent-live-session",
		SessionRevision: 2,
		CheckpointScope: scope,
		SandboxRunner:   runner,
		SandboxPolicy:   fakeBashPolicy(sandbox.EnforcementRequired),
		DisablePlugins:  true,
		PluginDirs:      []string{},
	})
	tasks := manager.SpawnMultiple([]struct {
		Name        string
		Instruction string
		Files       []string
	}{
		{Name: "worker-a", Instruction: "Update worker-a.txt", Files: []string{"worker-a.txt"}},
		{Name: "worker-b", Instruction: "Update worker-b.txt", Files: []string{"worker-b.txt"}},
	})
	manager.Wait(20 * time.Second)
	for _, task := range tasks {
		if current := manager.GetTask(task.ID); current == nil || current.Status != "completed" {
			t.Fatalf("sub-agent task %q = %#v", task.ID, current)
		}
	}
	assertCheckpointScopeEntries(t, workDir, scope, map[string]string{"worker-a.txt": "after-worker-a", "worker-b.txt": "after-worker-b"})
}

func TestTeamWorkersShareOneCheckpointGeneration(t *testing.T) {
	configDir := isolateEvidenceLoopTest(t)
	t.Setenv("ANICLEW_CONFIG_DIR", configDir)
	t.Setenv("CORELAY_MEMORY", "off")
	t.Setenv("CORELAY_AUTOSKILL", "off")
	workDir := t.TempDir()
	checkpointWorkerFixtures(t, workDir)
	provider := newConcurrentCheckpointWorkerProvider()
	runner := &fakeBashRunner{name: "checkpoint-team", capabilities: fakeBashCapabilities()}
	scope := newCheckpointScope(newCheckpointOwner("team-checkpoint-session", 3, "run_team_group"))
	team := NewTeam(provider, "fake-model", workDir, t.TempDir(), TeamConfig{
		Name:            "checkpoint-workers",
		MaxWaveSize:     2,
		Capacity:        CapacityConfig{MaxParallelTasks: 2, ModelSlots: 2, ToolSlots: 2, FileScopeLock: true, FileScopeLockConfigured: true},
		SessionID:       "team-live-session",
		SessionRevision: 3,
		CheckpointScope: scope,
		SandboxRunner:   runner,
		SandboxPolicy:   fakeBashPolicy(sandbox.EnforcementRequired),
		DisablePlugins:  true,
		PluginDirs:      []string{},
	})
	team.AddTask(TeamTask{ID: "worker-a", Name: "worker-a", Description: "Update worker-a.txt", Files: []string{"worker-a.txt"}})
	team.AddTask(TeamTask{ID: "worker-b", Name: "worker-b", Description: "Update worker-b.txt", Files: []string{"worker-b.txt"}})
	if err := team.ExecuteWaves(context.Background(), make(chan Event, 64)); err != nil {
		t.Fatalf("execute team workers: %v", err)
	}
	for _, task := range team.GetTasks() {
		if task.Status != "completed" {
			t.Fatalf("team task %q status = %q result=%q", task.ID, task.Status, task.Result)
		}
	}
	assertCheckpointScopeEntries(t, workDir, scope, map[string]string{"worker-a.txt": "after-worker-a", "worker-b.txt": "after-worker-b"})
}

func checkpointWorkerFixtures(t *testing.T, workDir string) {
	t.Helper()
	for _, name := range []string{"worker-a.txt", "worker-b.txt"} {
		writeCheckpointSecurityFile(t, filepath.Join(workDir, name), "before-"+name)
	}
}

func assertCheckpointScopeEntries(t *testing.T, workDir string, scope *CheckpointScope, expected map[string]string) {
	t.Helper()
	owner := scope.ownerSnapshot()
	manifest, _, found, err := readManifestStrict(workDir, owner.SessionID)
	if err != nil || !found {
		t.Fatalf("shared checkpoint manifest unavailable: found=%v err=%v", found, err)
	}
	if !checkpointManifestMatchesOwner(manifest, owner) {
		t.Fatalf("shared checkpoint owner mismatch: manifest=%+v owner=%+v", manifest, owner)
	}
	for name, after := range expected {
		entry, ok := manifest.Files[name]
		if !ok || entry.PreimageDigest != artifactBytesRevision([]byte("before-"+name)) ||
			entry.PostimageDigest != artifactBytesRevision([]byte(after)) || !entry.PostimageCaptured {
			t.Fatalf("shared checkpoint entry %q = %+v", name, entry)
		}
	}
}

type concurrentCheckpointWorkerProvider struct {
	mu     sync.Mutex
	phases map[string]int
}

func newConcurrentCheckpointWorkerProvider() *concurrentCheckpointWorkerProvider {
	return &concurrentCheckpointWorkerProvider{phases: make(map[string]int)}
}

func (*concurrentCheckpointWorkerProvider) Name() string              { return "scripted" }
func (*concurrentCheckpointWorkerProvider) DisplayName() string       { return "Scripted" }
func (*concurrentCheckpointWorkerProvider) Models() []types.ModelInfo { return nil }
func (*concurrentCheckpointWorkerProvider) Validate() error           { return nil }

func (p *concurrentCheckpointWorkerProvider) StreamMessage(ctx context.Context, request *types.MessagesRequest, options *types.StreamOptions) (<-chan types.SSEEvent, error) {
	var requestText strings.Builder
	for _, message := range request.Messages {
		requestText.Write(message.Content)
	}
	path := ""
	for _, candidate := range []string{"worker-a.txt", "worker-b.txt"} {
		if strings.Contains(requestText.String(), candidate) {
			path = candidate
			break
		}
	}
	p.mu.Lock()
	phase := p.phases[path]
	p.phases[path] = phase + 1
	p.mu.Unlock()
	var step scriptedLoopStep
	switch phase {
	case 0:
		step = toolUseStep("checkpoint-"+path+"-read", "Read", map[string]string{"file_path": path})
	case 1:
		step = toolUseStep("checkpoint-"+path+"-write", "Write", map[string]string{
			"file_path": path,
			"content":   "after-" + strings.TrimSuffix(path, ".txt"),
		})
	default:
		step = textStep("worker completed")
	}
	return (&scriptedLoopProvider{steps: []scriptedLoopStep{step}}).StreamMessage(ctx, request, options)
}

func runCheckpointTestTurn(t *testing.T, workDir, sessionID string, revision uint64, content, prompt string) []Event {
	return runCheckpointTestTurnWithIDs(t, workDir, "kernel-"+sessionID, sessionID, revision, content, prompt)
}

func runEphemeralCheckpointTestTurn(t *testing.T, workDir, liveSessionID string, revision uint64, content, prompt string) []Event {
	return runCheckpointTestTurnWithIDs(t, workDir, liveSessionID, "", revision, content, prompt)
}

func runCheckpointTestTurnWithIDs(t *testing.T, workDir, liveSessionID, durableSessionID string, revision uint64, content, prompt string) []Event {
	t.Helper()
	provider := &scriptedLoopProvider{steps: []scriptedLoopStep{
		toolUseStep("toolu_checkpoint_read", "Read", map[string]string{
			"file_path": "checkpoint-target.txt",
		}),
		toolUseStep("toolu_checkpoint_write", "Write", map[string]string{
			"file_path": "checkpoint-target.txt",
			"content":   content,
		}),
		toolUseStep("toolu_checkpoint_write_again", "Write", map[string]string{
			"file_path": "checkpoint-target.txt",
			"content":   content + "-final",
		}),
		textStep("done"),
	}}
	userContent, err := json.Marshal(prompt)
	if err != nil {
		t.Fatal(err)
	}
	eventCh := make(chan Event, 64)
	capabilities := fakeBashCapabilities()
	capabilities.FilesystemIsolation = true
	runner := &fakeBashRunner{name: "checkpoint-runloop", capabilities: capabilities}
	go RunLoopWithOptions(context.Background(), provider, "fake-model", []types.Message{{Role: "user", Content: userContent}}, workDir, RunOptions{
		SessionID:           liveSessionID,
		DurableSessionID:    durableSessionID,
		SessionRevision:     revision,
		EvidencePolicy:      EvidencePolicyConfig{Policy: EvidencePolicyOff},
		SandboxRunner:       runner,
		SandboxPolicy:       fakeBashPolicy(sandbox.EnforcementRequired),
		DisablePlugins:      true,
		DisableWorkspaceMCP: true,
		PluginDirs:          []string{},
	}, eventCh)
	var events []Event
	for event := range eventCh {
		events = append(events, event)
	}
	return events
}
