package observability

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRecordRunCheckedReportsPersistenceFailure(t *testing.T) {
	root := t.TempDir()
	tkr := NewTracker(root)
	blocked := filepath.Join(root, "file-not-directory")
	if err := os.WriteFile(blocked, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	tkr.runDir = blocked
	if tkr.RecordRunChecked(RunTrace{ID: "failed-store", Kind: "agent"}) == nil {
		t.Fatal("persistence reported success")
	}
	runs := tkr.RecentRuns(1)
	if len(runs) != 1 || runs[0].Metadata["persistence"] != "failed" {
		t.Fatal("missing failure marker")
	}
}
