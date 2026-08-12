package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type ReadLedgerCode string

const (
	ReadLedgerAllowed             ReadLedgerCode = "allowed"
	ReadLedgerNewFile             ReadLedgerCode = "new_file"
	ReadLedgerReadRequired        ReadLedgerCode = "read_required"
	ReadLedgerStaleRead           ReadLedgerCode = "stale_read"
	ReadLedgerMissingTarget       ReadLedgerCode = "missing_target"
	ReadLedgerRevisionUnavailable ReadLedgerCode = "revision_unavailable"
)

// ReadLedgerDecision is a typed pre-mutation result. CurrentRevision is
// returned when available so the execution pipeline can bind a prepared edit
// to the exact artifact state it checked.
type ReadLedgerDecision struct {
	Allowed          bool
	Code             ReadLedgerCode
	Path             string
	RecordedRevision string
	CurrentRevision  string
	Detail           string
}

// ReadLedger stores successful Read evidence for one run. It never stores file
// contents. A content digest is enough to reject writes after an external
// change and to refresh evidence after an agent-owned mutation.
type ReadLedger struct {
	workDir string
	mu      sync.RWMutex
	reads   map[string]string
}

func NewReadLedger(workDir string) *ReadLedger {
	if strings.TrimSpace(workDir) == "" {
		workDir = "."
	}
	if absolute, err := filepath.Abs(workDir); err == nil {
		workDir = filepath.Clean(absolute)
	}
	return &ReadLedger{
		workDir: workDir,
		reads:   make(map[string]string),
	}
}

// RecordRead records the current revision after a successful Read operation.
// Callers must not invoke it for a failed or partial read.
func (l *ReadLedger) RecordRead(filePath string) error {
	canonical, err := l.canonicalPath(filePath)
	if err != nil {
		return err
	}
	revision, err := readLedgerFileRevision(canonical)
	if err != nil {
		l.mu.Lock()
		delete(l.reads, canonical)
		l.mu.Unlock()
		return err
	}
	l.mu.Lock()
	l.reads[canonical] = revision
	l.mu.Unlock()
	return nil
}

// CheckWrite enforces read-before-write for an existing file while allowing a
// genuinely new file to be created.
func (l *ReadLedger) CheckWrite(filePath string) ReadLedgerDecision {
	return l.checkMutation(filePath, true)
}

// CheckEdit requires the target to exist in addition to requiring fresh Read
// evidence.
func (l *ReadLedger) CheckEdit(filePath string) ReadLedgerDecision {
	return l.checkMutation(filePath, false)
}

// RefreshAfterWrite advances the ledger only after a successful agent-owned
// Write or Edit. This permits a subsequent edit without a redundant Read while
// still detecting modifications made by another actor afterward.
func (l *ReadLedger) RefreshAfterWrite(filePath string) error {
	return l.RecordRead(filePath)
}

// CurrentRevision returns the content revision used by both the read ledger
// and ActionFingerprintInput.TargetRevision.
func (l *ReadLedger) CurrentRevision(filePath string) (string, error) {
	canonical, err := l.canonicalPath(filePath)
	if err != nil {
		return "", err
	}
	return readLedgerFileRevision(canonical)
}

// Forget removes prior evidence, for example after a failed mutation whose
// side effects could not be reconciled.
func (l *ReadLedger) Forget(filePath string) error {
	canonical, err := l.canonicalPath(filePath)
	if err != nil {
		return err
	}
	l.mu.Lock()
	delete(l.reads, canonical)
	l.mu.Unlock()
	return nil
}

func (l *ReadLedger) checkMutation(filePath string, allowCreate bool) ReadLedgerDecision {
	canonical, err := l.canonicalPath(filePath)
	if err != nil {
		return ReadLedgerDecision{
			Code:   ReadLedgerRevisionUnavailable,
			Detail: err.Error(),
		}
	}
	decision := ReadLedgerDecision{Path: canonical}
	revision, err := readLedgerFileRevision(canonical)
	if err != nil {
		if os.IsNotExist(err) {
			if allowCreate {
				decision.Allowed = true
				decision.Code = ReadLedgerNewFile
				return decision
			}
			decision.Code = ReadLedgerMissingTarget
			decision.Detail = err.Error()
			return decision
		}
		decision.Code = ReadLedgerRevisionUnavailable
		decision.Detail = err.Error()
		return decision
	}
	decision.CurrentRevision = revision

	l.mu.RLock()
	recorded, ok := l.reads[canonical]
	l.mu.RUnlock()
	decision.RecordedRevision = recorded
	if !ok {
		decision.Code = ReadLedgerReadRequired
		return decision
	}
	if recorded != revision {
		decision.Code = ReadLedgerStaleRead
		return decision
	}
	decision.Allowed = true
	decision.Code = ReadLedgerAllowed
	return decision
}

func (l *ReadLedger) canonicalPath(filePath string) (string, error) {
	if strings.TrimSpace(filePath) == "" {
		return "", fmt.Errorf("read ledger path is empty")
	}
	workspace, err := canonicalWorkspace(l.workDir)
	if err != nil {
		return "", fmt.Errorf("resolve read ledger workspace: %w", err)
	}
	resolved := resolvePath(filePath, l.workDir)
	absolute, err := filepath.Abs(resolved)
	if err != nil {
		return "", fmt.Errorf("resolve read ledger path: %w", err)
	}
	canonical, err := canonicalizeTarget(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve read ledger target: %w", err)
	}
	if !pathWithin(canonical, workspace) {
		return "", fmt.Errorf("read ledger target is outside the workspace")
	}
	return filepath.Clean(canonical), nil
}

func readLedgerFileRevision(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("read ledger target is not a regular file: %s", filePath)
	}

	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), nil
}
