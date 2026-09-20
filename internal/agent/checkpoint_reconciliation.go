package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type ReconciliationFileStatus string

const (
	maxReconciliationFiles     = 256
	maxReconciliationPathBytes = 1024

	ReconciliationFileMatchesPreimage     ReconciliationFileStatus = "matches_preimage"
	ReconciliationFileMatchesPostimage    ReconciliationFileStatus = "matches_postimage"
	ReconciliationFileDiverged            ReconciliationFileStatus = "diverged"
	ReconciliationFilePostimageUnrecorded ReconciliationFileStatus = "postimage_unrecorded"
	ReconciliationFileUnavailable         ReconciliationFileStatus = "unavailable"
)

type ReconciliationSideEffectJudgment string

const (
	ReconciliationJudgmentDigestEvaluated ReconciliationSideEffectJudgment = "digest_evaluated"
	ReconciliationJudgmentUnknown         ReconciliationSideEffectJudgment = "unknown"
)

// ReconciliationFileEvidence contains hashes and bounded state labels only.
// It never includes file contents or tool input/output.
type ReconciliationFileEvidence struct {
	Path            string                   `json:"path"`
	Status          ReconciliationFileStatus `json:"status"`
	PreimageDigest  string                   `json:"preimageDigest,omitempty"`
	PostimageDigest string                   `json:"postimageDigest,omitempty"`
	CurrentDigest   string                   `json:"currentDigest,omitempty"`
}

// SessionReconciliationAssessment is a non-mutating, revision-bound view of
// durable file evidence. EvidenceDigest must be recomputed before acknowledging
// reconciliation so edits made after preview invalidate the request.
type SessionReconciliationAssessment struct {
	SessionID                  string                           `json:"sessionId"`
	Revision                   uint64                           `json:"revision"`
	RunID                      string                           `json:"runId,omitempty"`
	ToolName                   string                           `json:"toolName,omitempty"`
	EvidenceDigest             string                           `json:"evidenceDigest"`
	CheckpointStatus           string                           `json:"checkpointStatus"`
	SideEffectJudgment         ReconciliationSideEffectJudgment `json:"sideEffectJudgment"`
	RecordedExecutionState     SessionSideEffectState           `json:"recordedExecutionState,omitempty"`
	ManualConfirmationRequired bool                             `json:"manualConfirmationRequired"`
	ManualConfirmationReason   string                           `json:"manualConfirmationReason,omitempty"`
	Files                      []ReconciliationFileEvidence     `json:"files"`
	MatchesPreimage            int                              `json:"matchesPreimage"`
	MatchesPostimage           int                              `json:"matchesPostimage"`
	Diverged                   int                              `json:"diverged"`
	PostimageUnrecorded        int                              `json:"postimageUnrecorded"`
	Unavailable                int                              `json:"unavailable"`
}

// NewSessionReconciliationReceipt snapshots an assessment for the atomic
// store transition. manualAcknowledged records only an acknowledgement of a
// required manual review; evidence-only reconciliations keep it false.
func NewSessionReconciliationReceipt(
	assessment SessionReconciliationAssessment,
	manualAcknowledged bool,
	at time.Time,
) (SessionReconciliationReceipt, error) {
	if assessment.ManualConfirmationRequired && !manualAcknowledged {
		return SessionReconciliationReceipt{}, fmt.Errorf("manual side-effect confirmation is required")
	}
	receipt := SessionReconciliationReceipt{
		Version:                        1,
		At:                             at.UTC(),
		RunID:                          assessment.RunID,
		EvidenceDigest:                 assessment.EvidenceDigest,
		CheckpointStatus:               assessment.CheckpointStatus,
		SideEffectJudgment:             assessment.SideEffectJudgment,
		FileCount:                      len(assessment.Files),
		MatchesPreimage:                assessment.MatchesPreimage,
		MatchesPostimage:               assessment.MatchesPostimage,
		Diverged:                       assessment.Diverged,
		PostimageUnrecorded:            assessment.PostimageUnrecorded,
		Unavailable:                    assessment.Unavailable,
		ManualConfirmationRequired:     assessment.ManualConfirmationRequired,
		ManualConfirmationAcknowledged: assessment.ManualConfirmationRequired && manualAcknowledged,
	}
	if err := ValidateSessionReconciliationReceipt(receipt); err != nil {
		return SessionReconciliationReceipt{}, err
	}
	return receipt, nil
}

// AssessSessionReconciliation reads the session's strict checkpoint manifest
// and compares current file digests with the recorded preimage/postimage. It
// does not mutate files or the session. Missing, malformed, stale, or
// mismatched evidence is reported as unavailable and requires manual review.
func AssessSessionReconciliation(session *Session) (SessionReconciliationAssessment, error) {
	fileMutationBatchMu.Lock()
	defer fileMutationBatchMu.Unlock()
	return assessSessionReconciliationLocked(session)
}

func assessSessionReconciliationLocked(session *Session) (SessionReconciliationAssessment, error) {
	if session == nil || strings.TrimSpace(session.ID) == "" || !session.ReconcileRequired || session.Interruption == nil {
		return SessionReconciliationAssessment{}, fmt.Errorf("session is not awaiting reconciliation")
	}
	if err := ValidateSessionInterruption(*session.Interruption); err != nil {
		return SessionReconciliationAssessment{}, err
	}

	marker := *session.Interruption
	assessment := SessionReconciliationAssessment{
		SessionID:              session.ID,
		Revision:               session.Revision,
		RunID:                  marker.RunID,
		ToolName:               marker.ToolName,
		CheckpointStatus:       "unavailable",
		SideEffectJudgment:     ReconciliationJudgmentUnknown,
		RecordedExecutionState: marker.SideEffectState,
		Files:                  []ReconciliationFileEvidence{},
	}
	manualReason := "The interrupted tool is not limited to checkpointed file writes."
	if reconciliationToolIsFileOnly(marker.ToolName) {
		manualReason = ""
	}

	canonicalWork, workspaceErr := canonicalWorkspace(session.Workspace)
	manifest, paths, found, manifestErr := readManifestStrict(canonicalWork, session.ID)
	if workspaceErr == nil && manifestErr == nil && found && len(manifest.Files) <= maxReconciliationFiles && marker.RunID != "" && manifest.RunID == marker.RunID &&
		manifest.SessionRevision == session.LastCommittedRevision &&
		manifest.Generation == "checkpoint_"+marker.RunID {
		assessment.CheckpointStatus = "available"
		if reconciliationToolIsFileOnly(marker.ToolName) {
			assessment.SideEffectJudgment = ReconciliationJudgmentDigestEvaluated
		}
		keys := make([]string, 0, len(manifest.Files))
		for key := range manifest.Files {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if len(key) > maxReconciliationPathBytes {
				evidence := ReconciliationFileEvidence{Path: "<path-too-long>", Status: ReconciliationFileUnavailable}
				assessment.Files = append(assessment.Files, evidence)
				assessment.Unavailable++
				continue
			}
			evidence := assessCheckpointFile(canonicalWork, paths, key, manifest.Files[key])
			assessment.Files = append(assessment.Files, evidence)
			switch evidence.Status {
			case ReconciliationFileMatchesPreimage:
				assessment.MatchesPreimage++
			case ReconciliationFileMatchesPostimage:
				assessment.MatchesPostimage++
			case ReconciliationFileDiverged:
				assessment.Diverged++
			case ReconciliationFilePostimageUnrecorded:
				assessment.PostimageUnrecorded++
			case ReconciliationFileUnavailable:
				assessment.Unavailable++
			}
		}
	} else {
		assessment.Files = append(assessment.Files, ReconciliationFileEvidence{
			Path:   "<checkpoint>",
			Status: ReconciliationFileUnavailable,
		})
		assessment.Unavailable = 1
	}

	if assessment.SideEffectJudgment == ReconciliationJudgmentUnknown {
		manualReason = "The interrupted tool cannot be assessed from file digests alone; confirm its external side effects manually."
	}
	if assessment.Diverged > 0 {
		manualReason = "Current files differ from both recorded checkpoint states; inspect them before continuing."
	}
	if assessment.PostimageUnrecorded > 0 {
		manualReason = "A file changed before its postimage was durably recorded; inspect it before continuing."
	}
	if assessment.Unavailable > 0 {
		manualReason = "Checkpoint evidence is missing, stale, corrupt, or unreadable; inspect side effects before continuing."
	}
	assessment.ManualConfirmationRequired = manualReason != ""
	assessment.ManualConfirmationReason = manualReason

	assessment.EvidenceDigest = reconciliationEvidenceDigest(*session, assessment)
	return assessment, nil
}

func reconciliationToolIsFileOnly(name string) bool {
	switch canonicalPermissionToolName(strings.TrimSpace(name)) {
	case "Write", "Edit":
		return true
	default:
		return false
	}
}

func assessCheckpointFile(workDir string, paths checkpointPaths, key string, entry ckptFile) ReconciliationFileEvidence {
	evidence := ReconciliationFileEvidence{
		Path:           filepath.ToSlash(key),
		Status:         ReconciliationFileUnavailable,
		PreimageDigest: entry.PreimageDigest,
	}
	if entry.TargetPath != "" {
		evidence.Path = "<external file>"
	}
	if !isSHA256Revision(entry.PreimageDigest) {
		return evidence
	}
	absentDigest := checkpointStateRevision(false, nil)
	if !entry.Existed {
		if entry.Backup != "" || entry.PreimageDigest != absentDigest || entry.PreimageMode != nil {
			return evidence
		}
	} else {
		if entry.PreimageMode == nil || *entry.PreimageMode > 0o777 || entry.Backup == "" {
			return evidence
		}
		backup, err := resolveCheckpointBackup(paths, entry.Backup)
		if err != nil {
			return evidence
		}
		info, err := os.Lstat(backup)
		if err != nil || !info.Mode().IsRegular() {
			return evidence
		}
		data, err := os.ReadFile(backup)
		if err != nil || checkpointStateRevision(true, data) != entry.PreimageDigest {
			return evidence
		}
	}
	if entry.PostimageCaptured {
		if !isSHA256Revision(entry.PostimageDigest) || (!entry.PostimageExisted && entry.PostimageDigest != absentDigest) {
			return evidence
		}
		evidence.PostimageDigest = entry.PostimageDigest
	} else if entry.PostimageDigest != "" || entry.PostimageExisted {
		return evidence
	}

	target, exists, _, err := resolveCheckpointEntryTarget(workDir, key, entry)
	if err != nil {
		return evidence
	}
	var current []byte
	if exists {
		current, err = os.ReadFile(target)
		if err != nil {
			return evidence
		}
	}
	currentDigest := checkpointStateRevision(exists, current)
	evidence.CurrentDigest = currentDigest
	if currentDigest == entry.PreimageDigest {
		evidence.Status = ReconciliationFileMatchesPreimage
		return evidence
	}
	if !entry.PostimageCaptured {
		evidence.Status = ReconciliationFilePostimageUnrecorded
		return evidence
	}
	if currentDigest == entry.PostimageDigest {
		evidence.Status = ReconciliationFileMatchesPostimage
		return evidence
	}
	evidence.Status = ReconciliationFileDiverged
	return evidence
}

func reconciliationEvidenceDigest(session Session, assessment SessionReconciliationAssessment) string {
	type evidenceIdentity struct {
		SessionID                  string                           `json:"sessionId"`
		Revision                   uint64                           `json:"revision"`
		RunID                      string                           `json:"runId,omitempty"`
		ToolName                   string                           `json:"toolName,omitempty"`
		ToolCallID                 string                           `json:"toolCallId,omitempty"`
		InputDigest                string                           `json:"inputDigest,omitempty"`
		CheckpointStatus           string                           `json:"checkpointStatus"`
		SideEffectJudgment         ReconciliationSideEffectJudgment `json:"sideEffectJudgment"`
		ManualConfirmationRequired bool                             `json:"manualConfirmationRequired"`
		Files                      []ReconciliationFileEvidence     `json:"files"`
	}
	identity := evidenceIdentity{
		SessionID:                  session.ID,
		Revision:                   session.Revision,
		RunID:                      assessment.RunID,
		ToolName:                   session.Interruption.ToolName,
		ToolCallID:                 session.Interruption.ToolCallID,
		InputDigest:                session.Interruption.InputDigest,
		CheckpointStatus:           assessment.CheckpointStatus,
		SideEffectJudgment:         assessment.SideEffectJudgment,
		ManualConfirmationRequired: assessment.ManualConfirmationRequired,
		Files:                      assessment.Files,
	}
	encoded, _ := json.Marshal(identity)
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}
