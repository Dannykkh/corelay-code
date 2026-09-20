package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAssessSessionReconciliationUsesCheckpointDigests(t *testing.T) {
	for _, test := range []struct {
		name       string
		current    string
		wantStatus ReconciliationFileStatus
		wantManual bool
	}{
		{name: "postimage", current: "agent edit", wantStatus: ReconciliationFileMatchesPostimage},
		{name: "preimage", current: "before", wantStatus: ReconciliationFileMatchesPreimage},
		{name: "user divergence", current: "user edit", wantStatus: ReconciliationFileDiverged, wantManual: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCheckpointSecurityFixture(t)
			owner := checkpointSecurityOwner(fixture)
			createUndoTestCheckpoint(t, fixture, owner,
				map[string]string{"edited.txt": "before"},
				map[string]string{"edited.txt": "agent edit"},
			)
			if err := os.WriteFile(filepath.Join(fixture.workDir, "edited.txt"), []byte(test.current), 0o600); err != nil {
				t.Fatal(err)
			}

			assessment, err := AssessSessionReconciliation(reconciliationTestSession(fixture, "Write"))
			if err != nil {
				t.Fatalf("AssessSessionReconciliation() error = %v", err)
			}
			if assessment.CheckpointStatus != "available" || assessment.SideEffectJudgment != ReconciliationJudgmentDigestEvaluated ||
				len(assessment.Files) != 1 || assessment.Files[0].Status != test.wantStatus ||
				assessment.ManualConfirmationRequired != test.wantManual || !isSHA256Revision(assessment.EvidenceDigest) {
				t.Fatalf("assessment = %+v", assessment)
			}
		})
	}
}

func TestAssessSessionReconciliationRequiresManualReviewForUnrecordedChanges(t *testing.T) {
	fixture := newCheckpointSecurityFixture(t)
	owner := checkpointSecurityOwner(fixture)
	if err := startCheckpoint(fixture.workDir, owner); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fixture.workDir, "edited.txt")
	writeCheckpointSecurityFile(t, path, "before")
	if err := checkpointFile(fixture.workDir, "edited.txt", path, owner); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("changed before postimage publication"), 0o600); err != nil {
		t.Fatal(err)
	}

	assessment, err := AssessSessionReconciliation(reconciliationTestSession(fixture, "Write"))
	if err != nil {
		t.Fatal(err)
	}
	if assessment.Files[0].Status != ReconciliationFilePostimageUnrecorded || !assessment.ManualConfirmationRequired || assessment.PostimageUnrecorded != 1 {
		t.Fatalf("unrecorded mutation assessment = %+v", assessment)
	}
}

func TestAssessSessionReconciliationFailsClosedForMissingMismatchedAndCorruptEvidence(t *testing.T) {
	t.Run("missing manifest", func(t *testing.T) {
		fixture := newCheckpointSecurityFixture(t)
		assessment, err := AssessSessionReconciliation(reconciliationTestSession(fixture, "Write"))
		if err != nil {
			t.Fatal(err)
		}
		assertUnavailableReconciliation(t, assessment)
	})

	t.Run("mismatched run", func(t *testing.T) {
		fixture := newCheckpointSecurityFixture(t)
		otherOwner := newCheckpointOwner(fixture.sessionID, 1, "different-run")
		createUndoTestCheckpoint(t, fixture, otherOwner,
			map[string]string{"edited.txt": "before"},
			map[string]string{"edited.txt": "agent edit"},
		)
		assessment, err := AssessSessionReconciliation(reconciliationTestSession(fixture, "Write"))
		if err != nil {
			t.Fatal(err)
		}
		assertUnavailableReconciliation(t, assessment)
	})

	t.Run("corrupt backup", func(t *testing.T) {
		fixture := newCheckpointSecurityFixture(t)
		createUndoTestCheckpoint(t, fixture, checkpointSecurityOwner(fixture),
			map[string]string{"edited.txt": "before"},
			map[string]string{"edited.txt": "agent edit"},
		)
		manifest, _, found, err := readManifestStrict(fixture.workDir, fixture.sessionID)
		if err != nil || !found {
			t.Fatalf("read manifest: found=%v err=%v", found, err)
		}
		entry := manifest.Files["edited.txt"]
		if err := os.WriteFile(filepath.Join(fixture.dir, entry.Backup), []byte("corrupt"), 0o600); err != nil {
			t.Fatal(err)
		}
		assessment, err := AssessSessionReconciliation(reconciliationTestSession(fixture, "Write"))
		if err != nil {
			t.Fatal(err)
		}
		if len(assessment.Files) != 1 || assessment.Files[0].Status != ReconciliationFileUnavailable || !assessment.ManualConfirmationRequired {
			t.Fatalf("corrupt-backup assessment = %+v", assessment)
		}
	})
}

func TestAssessSessionReconciliationSeparatesUnknownSideEffectFromManualAck(t *testing.T) {
	fixture := newCheckpointSecurityFixture(t)
	createUndoTestCheckpoint(t, fixture, checkpointSecurityOwner(fixture),
		map[string]string{"edited.txt": "before"},
		map[string]string{"edited.txt": "agent edit"},
	)
	session := reconciliationTestSession(fixture, "Bash")
	assessment, err := AssessSessionReconciliation(session)
	if err != nil {
		t.Fatal(err)
	}
	if assessment.CheckpointStatus != "available" || assessment.SideEffectJudgment != ReconciliationJudgmentUnknown ||
		!assessment.ManualConfirmationRequired || assessment.RecordedExecutionState != SessionSideEffectStarted {
		t.Fatalf("shell assessment = %+v", assessment)
	}
}

func TestReconciliationEvidenceDigestChangesWithCurrentFileStateAndDoesNotContainRawInput(t *testing.T) {
	fixture := newCheckpointSecurityFixture(t)
	createUndoTestCheckpoint(t, fixture, checkpointSecurityOwner(fixture),
		map[string]string{"edited.txt": "before"},
		map[string]string{"edited.txt": "agent edit"},
	)
	session := reconciliationTestSession(fixture, "Write")
	secret := "raw-tool-input-that-must-not-persist"
	session.Interruption.InputDigest = artifactBytesRevision([]byte(secret))
	first, err := AssessSessionReconciliation(session)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.workDir, "edited.txt"), []byte("user edit"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := AssessSessionReconciliation(session)
	if err != nil {
		t.Fatal(err)
	}
	if first.EvidenceDigest == second.EvidenceDigest {
		t.Fatal("evidence digest did not change with the current file state")
	}
	encoded, err := json.Marshal(struct {
		Interruption SessionInterruption             `json:"interruption"`
		Assessment   SessionReconciliationAssessment `json:"assessment"`
	}{*session.Interruption, first})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("reconciliation persisted raw tool input: %s", encoded)
	}
}

func reconciliationTestSession(fixture checkpointSecurityFixture, toolName string) *Session {
	return &Session{
		Version:               currentSessionVersion,
		Revision:              2,
		ID:                    fixture.sessionID,
		Workspace:             fixture.workDir,
		LifecycleStatus:       SessionLifecycleInterrupted,
		LastCommittedRevision: 1,
		ReconcileRequired:     true,
		Interruption: &SessionInterruption{
			At:              time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC),
			RunID:           "run-checkpoint-security",
			ToolName:        toolName,
			ToolCallID:      "tool-1",
			InputDigest:     artifactBytesRevision([]byte("safe digest input")),
			SideEffectState: SessionSideEffectStarted,
			Summary:         "interrupted after tool start",
		},
	}
}

func assertUnavailableReconciliation(t *testing.T, assessment SessionReconciliationAssessment) {
	t.Helper()
	if assessment.CheckpointStatus != "unavailable" || assessment.SideEffectJudgment != ReconciliationJudgmentUnknown ||
		!assessment.ManualConfirmationRequired || assessment.Unavailable == 0 {
		t.Fatalf("unavailable assessment = %+v", assessment)
	}
}
