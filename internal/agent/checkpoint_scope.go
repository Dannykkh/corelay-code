package agent

import (
	"errors"
	"strings"
	"sync"
)

// CheckpointScope binds cooperating RunLoops to one undoable run generation.
// Its private owner state prevents callers from changing identity after workers
// start, while the constructor lets server-side managers share a single scope.
type CheckpointScope struct {
	mu      sync.Mutex
	owner   checkpointOwner
	started bool
}

// NewCheckpointScope creates a checkpoint generation for a root worker manager.
// Pass the returned scope to every worker that contributes to the same turn.
func NewCheckpointScope(sessionID string, sessionRevision uint64) *CheckpointScope {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		sessionID = "ephemeral-" + newActiveRunID()
	}
	return newCheckpointScope(newCheckpointOwner(sessionID, sessionRevision, newActiveRunID()))
}

func newCheckpointScope(owner checkpointOwner) *CheckpointScope {
	return &CheckpointScope{owner: owner}
}

func (scope *CheckpointScope) ownerSnapshot() checkpointOwner {
	if scope == nil {
		return checkpointOwner{}
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	return scope.owner
}

func (scope *CheckpointScope) captureBeforeMutation(workDir, relPath, absPath string) error {
	if scope == nil {
		return errors.New("checkpoint scope is unavailable")
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if !scope.owner.valid() {
		return errors.New("checkpoint scope owner is invalid")
	}
	if !scope.started {
		if err := startCheckpoint(workDir, scope.owner); err != nil {
			return err
		}
		scope.started = true
	}
	return checkpointFile(workDir, relPath, absPath, scope.owner)
}

func (scope *CheckpointScope) recordPostimages(workDir string, mutations []committedFileMutation) error {
	if scope == nil {
		return errors.New("checkpoint scope is unavailable")
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if !scope.owner.valid() || !scope.started {
		return errors.New("checkpoint scope has no captured preimage")
	}
	return recordCheckpointPostimages(workDir, scope.owner, mutations)
}

func (scope *CheckpointScope) settleFailedBatch(workDir string, mutationPaths []string) error {
	if scope == nil {
		return errors.New("checkpoint scope is unavailable")
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if !scope.owner.valid() || !scope.started {
		return nil
	}
	return settleCheckpointPostimages(workDir, scope.owner, mutationPaths)
}
