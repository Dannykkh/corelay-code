package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadLedgerRequiresFreshReadForExistingWrite(t *testing.T) {
	workDir := t.TempDir()
	path := filepath.Join(workDir, "existing.txt")
	writeLedgerFixture(t, path, "before")
	ledger := NewReadLedger(workDir)

	decision := ledger.CheckWrite("existing.txt")
	if decision.Allowed || decision.Code != ReadLedgerReadRequired || decision.CurrentRevision == "" {
		t.Fatalf("decision before Read = %#v", decision)
	}
	if err := ledger.RecordRead("existing.txt"); err != nil {
		t.Fatalf("RecordRead: %v", err)
	}
	decision = ledger.CheckWrite(path)
	if !decision.Allowed || decision.Code != ReadLedgerAllowed {
		t.Fatalf("decision after Read = %#v", decision)
	}
	if decision.RecordedRevision != decision.CurrentRevision {
		t.Fatalf("revision mismatch after Read: %#v", decision)
	}
}

func TestReadLedgerDetectsStaleRead(t *testing.T) {
	workDir := t.TempDir()
	path := filepath.Join(workDir, "stale.txt")
	writeLedgerFixture(t, path, "before")
	ledger := NewReadLedger(workDir)
	if err := ledger.RecordRead(path); err != nil {
		t.Fatalf("RecordRead: %v", err)
	}

	writeLedgerFixture(t, path, "external change")
	decision := ledger.CheckEdit(path)
	if decision.Allowed || decision.Code != ReadLedgerStaleRead {
		t.Fatalf("stale decision = %#v", decision)
	}
	if decision.RecordedRevision == "" || decision.CurrentRevision == "" || decision.RecordedRevision == decision.CurrentRevision {
		t.Fatalf("stale revisions not reported: %#v", decision)
	}
}

func TestReadLedgerRefreshesAfterOwnWrite(t *testing.T) {
	workDir := t.TempDir()
	path := filepath.Join(workDir, "owned.txt")
	writeLedgerFixture(t, path, "before")
	ledger := NewReadLedger(workDir)
	if err := ledger.RecordRead(path); err != nil {
		t.Fatalf("RecordRead: %v", err)
	}

	writeLedgerFixture(t, path, "agent edit")
	if decision := ledger.CheckEdit(path); decision.Code != ReadLedgerStaleRead {
		t.Fatalf("mutation before refresh should be stale: %#v", decision)
	}
	if err := ledger.RefreshAfterWrite(path); err != nil {
		t.Fatalf("RefreshAfterWrite: %v", err)
	}
	if decision := ledger.CheckEdit(path); !decision.Allowed || decision.Code != ReadLedgerAllowed {
		t.Fatalf("mutation after refresh = %#v", decision)
	}

	writeLedgerFixture(t, path, "later external edit")
	if decision := ledger.CheckEdit(path); decision.Code != ReadLedgerStaleRead {
		t.Fatalf("later external mutation was not stale: %#v", decision)
	}
}

func TestReadLedgerHandlesNewAndMissingTargets(t *testing.T) {
	workDir := t.TempDir()
	ledger := NewReadLedger(workDir)

	if decision := ledger.CheckWrite("new.txt"); !decision.Allowed || decision.Code != ReadLedgerNewFile {
		t.Fatalf("new Write decision = %#v", decision)
	}
	if decision := ledger.CheckEdit("new.txt"); decision.Allowed || decision.Code != ReadLedgerMissingTarget {
		t.Fatalf("missing Edit decision = %#v", decision)
	}

	path := filepath.Join(workDir, "new.txt")
	writeLedgerFixture(t, path, "created by agent")
	if err := ledger.RefreshAfterWrite("new.txt"); err != nil {
		t.Fatalf("RefreshAfterWrite new file: %v", err)
	}
	if decision := ledger.CheckWrite(path); !decision.Allowed || decision.Code != ReadLedgerAllowed {
		t.Fatalf("created file decision = %#v", decision)
	}
}

func TestReadLedgerForgetAndRevision(t *testing.T) {
	workDir := t.TempDir()
	path := filepath.Join(workDir, "forget.txt")
	writeLedgerFixture(t, path, "content")
	ledger := NewReadLedger(workDir)
	if err := ledger.RecordRead("forget.txt"); err != nil {
		t.Fatalf("RecordRead: %v", err)
	}
	revision, err := ledger.CurrentRevision(path)
	if err != nil || revision == "" {
		t.Fatalf("CurrentRevision = %q, %v", revision, err)
	}
	if err := ledger.Forget(path); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if decision := ledger.CheckEdit("forget.txt"); decision.Code != ReadLedgerReadRequired {
		t.Fatalf("decision after Forget = %#v", decision)
	}
}

func TestReadLedgerRejectsInvalidTargets(t *testing.T) {
	workDir := t.TempDir()
	ledger := NewReadLedger(workDir)
	if decision := ledger.CheckWrite(""); decision.Allowed || decision.Code != ReadLedgerRevisionUnavailable {
		t.Fatalf("empty path decision = %#v", decision)
	}
	if decision := ledger.CheckWrite(workDir); decision.Allowed || decision.Code != ReadLedgerRevisionUnavailable {
		t.Fatalf("directory decision = %#v", decision)
	}
}

func writeLedgerFixture(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write fixture %s: %v", path, err)
	}
}
