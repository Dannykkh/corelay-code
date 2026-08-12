package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Dispatcher batches are serialized so two Corelay Code runs cannot interleave
// commits across different files while one model turn is being applied.
// Individual direct tool calls still use the narrower per-artifact lock.
var fileMutationBatchMu sync.Mutex

type fileMutationSnapshot struct {
	Path     string
	Existed  bool
	Data     []byte
	Mode     os.FileMode
	Revision string
}

type committedFileMutation struct {
	ResultIndex  int
	Snapshot     fileMutationSnapshot
	PostRevision string
}

func isFileMutationTool(name string) bool {
	return name == "Write" || name == "Edit"
}

func hasPreparedFileMutations(calls []preparedToolCall) bool {
	for _, call := range calls {
		if isFileMutationTool(call.tool.Name) {
			return true
		}
	}
	return false
}

// preflightFileMutationBatch validates every mutation target after concurrent
// reads have finished but before the first mutation starts. If one target is
// stale, every mutation in the turn is blocked and no partial batch begins.
func preflightFileMutationBatch(
	calls []preparedToolCall,
	opts toolDispatchOptions,
) map[int]string {
	if !opts.ReadBeforeWrite {
		return nil
	}
	failures := make(map[int]string)
	for _, prepared := range calls {
		call := prepared.tool
		if !isFileMutationTool(call.Name) {
			continue
		}
		if opts.ReadLedger == nil {
			failures[prepared.index] = "Read-before-write ledger is unavailable"
			continue
		}
		filePath := dispatchFilePath(call.Input)
		var decision ReadLedgerDecision
		if call.Name == "Write" {
			decision = opts.ReadLedger.CheckWrite(filePath)
		} else {
			decision = opts.ReadLedger.CheckEdit(filePath)
		}
		if !decision.Allowed {
			failures[prepared.index] = readLedgerDenialReason(decision)
		}
	}
	if len(failures) == 0 {
		return nil
	}
	blocked := make(map[int]string)
	for _, prepared := range calls {
		if !isFileMutationTool(prepared.tool.Name) {
			continue
		}
		if reason := failures[prepared.index]; reason != "" {
			blocked[prepared.index] = reason
		} else {
			blocked[prepared.index] = "File mutation batch blocked because another target failed preflight"
		}
	}
	return blocked
}

func captureFileMutationSnapshot(
	call toolUseBlock,
	opts toolDispatchOptions,
) (fileMutationSnapshot, error) {
	rawPath := dispatchFilePath(call.Input)
	if strings.TrimSpace(rawPath) == "" {
		return fileMutationSnapshot{}, fmt.Errorf("mutation path is empty")
	}
	var path string
	var err error
	if opts.ReadLedger != nil {
		path, err = opts.ReadLedger.canonicalPath(rawPath)
	} else {
		workspace, workspaceErr := canonicalWorkspace(opts.WorkDir)
		if workspaceErr != nil {
			return fileMutationSnapshot{}, workspaceErr
		}
		path, err = canonicalPathWithinWorkspace(
			rawPath,
			opts.WorkDir,
			workspace,
			DefaultPermissionConfig(),
		)
	}
	if err != nil {
		return fileMutationSnapshot{}, err
	}
	snapshot := fileMutationSnapshot{Path: filepath.Clean(path), Mode: 0o644}
	data, err := os.ReadFile(snapshot.Path)
	if os.IsNotExist(err) {
		if call.Name == "Edit" {
			return fileMutationSnapshot{}, fmt.Errorf("Edit target does not exist")
		}
		return snapshot, nil
	}
	if err != nil {
		return fileMutationSnapshot{}, fmt.Errorf("read mutation snapshot: %w", err)
	}
	info, err := os.Stat(snapshot.Path)
	if err != nil {
		return fileMutationSnapshot{}, fmt.Errorf("inspect mutation snapshot: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fileMutationSnapshot{}, fmt.Errorf("mutation snapshot target is not a regular file")
	}
	snapshot.Existed = true
	snapshot.Data = append([]byte(nil), data...)
	snapshot.Mode = info.Mode()
	snapshot.Revision = artifactBytesRevision(data)
	return snapshot, nil
}

func committedMutationRevision(snapshot fileMutationSnapshot) (string, error) {
	current, err := readLedgerFileRevision(snapshot.Path)
	if err != nil {
		return "", fmt.Errorf("capture committed mutation revision: %w", err)
	}
	return current, nil
}

func rollbackCommittedFileMutation(mutation committedFileMutation) error {
	snapshot := mutation.Snapshot
	unlock := lockArtifactMutation(snapshot.Path)
	defer unlock()
	if err := validateExpectedArtifactState(snapshot.Path, mutation.PostRevision, false); err != nil {
		return err
	}
	if !snapshot.Existed {
		if err := os.Remove(snapshot.Path); err != nil {
			return fmt.Errorf("remove rolled-back artifact: %w", err)
		}
		return nil
	}
	stage, err := stageArtifact(snapshot.Path, snapshot.Data, snapshot.Mode)
	if err != nil {
		return err
	}
	defer stage.cleanup()
	if err := stage.commit(mutation.PostRevision, false); err != nil {
		return err
	}
	current, err := readLedgerFileRevision(snapshot.Path)
	if err != nil {
		return err
	}
	if current != snapshot.Revision {
		return fmt.Errorf(
			"rolled-back artifact revision mismatch (expected %s, found %s)",
			snapshot.Revision,
			current,
		)
	}
	return nil
}

func rollbackFileMutationBatch(
	journal []committedFileMutation,
	results []toolDispatchResult,
	ledger *ReadLedger,
) []error {
	errorsFound := make([]error, 0)
	for index := len(journal) - 1; index >= 0; index-- {
		mutation := journal[index]
		err := rollbackCommittedFileMutation(mutation)
		result := &results[mutation.ResultIndex]
		result.IsError = true
		if err != nil {
			errorsFound = append(errorsFound, fmt.Errorf("%s: %w", mutation.Snapshot.Path, err))
			result.Content = "[TRANSACTION INCOMPLETE] A later file mutation failed, and this earlier change could not be rolled back safely: " + err.Error()
			result.Display = result.Content
			if ledger != nil {
				_ = ledger.Forget(mutation.Snapshot.Path)
			}
			continue
		}
		result.Content = "[TRANSACTION ROLLED BACK] A later file mutation failed, so this earlier change was restored to its pre-turn state."
		result.Display = result.Content
		if ledger != nil {
			if mutation.Snapshot.Existed {
				_ = ledger.RefreshAfterWrite(mutation.Snapshot.Path)
			} else {
				_ = ledger.Forget(mutation.Snapshot.Path)
			}
		}
	}
	return errorsFound
}
