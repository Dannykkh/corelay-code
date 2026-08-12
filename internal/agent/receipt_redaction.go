package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

const maxReceiptStringBytes = 64 * 1024

type ReceiptArtifactStatus string

const (
	ReceiptArtifactOK               ReceiptArtifactStatus = "ok"
	ReceiptArtifactMissing          ReceiptArtifactStatus = "missing"
	ReceiptArtifactOutsideWorkspace ReceiptArtifactStatus = "outside_workspace"
	ReceiptArtifactSymlink          ReceiptArtifactStatus = "symlink"
	ReceiptArtifactNonRegular       ReceiptArtifactStatus = "non_regular"
	ReceiptArtifactUnreadable       ReceiptArtifactStatus = "unreadable"
	ReceiptArtifactChanged          ReceiptArtifactStatus = "changed_during_hash"
	ReceiptArtifactPattern          ReceiptArtifactStatus = "pattern"
	ReceiptArtifactInvalid          ReceiptArtifactStatus = "invalid"
)

// ReceiptArtifact records integrity metadata only. File contents are never
// copied into a receipt.
type ReceiptArtifact struct {
	Path   string                `json:"path"`
	Status ReceiptArtifactStatus `json:"status"`
	Size   *int64                `json:"size,omitempty"`
	SHA256 string                `json:"sha256,omitempty"`
}

type receiptEvidencePayload struct {
	Source        string `json:"source"`
	Command       string `json:"command,omitempty"`
	CommandDigest string `json:"commandDigest,omitempty"`
	Status        string `json:"status"`
	Summary       string `json:"summary,omitempty"`
	ExitCode      int    `json:"exitCode,omitempty"`
	At            string `json:"at,omitempty"`
}

type receiptVerificationPayload struct {
	Status        string                   `json:"status"`
	TerminalState string                   `json:"terminalState"`
	Source        string                   `json:"source"`
	Command       string                   `json:"command,omitempty"`
	CommandDigest string                   `json:"commandDigest,omitempty"`
	Summary       string                   `json:"summary,omitempty"`
	Gate          string                   `json:"gate,omitempty"`
	Mode          string                   `json:"mode,omitempty"`
	Evidence      []receiptEvidencePayload `json:"evidence,omitempty"`
}

type agentReceiptPayload struct {
	Version      int                         `json:"version"`
	CreatedAt    string                      `json:"createdAt"`
	WorkDir      string                      `json:"workDir"`
	Provider     string                      `json:"provider"`
	Model        string                      `json:"model"`
	ProjectType  string                      `json:"projectType"`
	PlanMode     bool                        `json:"planMode"`
	Iterations   int                         `json:"iterations"`
	EditedFiles  []string                    `json:"editedFiles"`
	Artifacts    []ReceiptArtifact           `json:"artifacts,omitempty"`
	Verification receiptVerificationPayload  `json:"verification"`
	Completion   *CompletionContractSnapshot `json:"completion,omitempty"`
	Recovery     *RunGuardSnapshot           `json:"recovery,omitempty"`
}

type teamRunReceiptPayload struct {
	Version             int                        `json:"version"`
	Kind                string                     `json:"kind"`
	CreatedAt           string                     `json:"createdAt"`
	WorkDir             string                     `json:"workDir"`
	Status              string                     `json:"status"`
	TeamName            string                     `json:"teamName"`
	PlanName            string                     `json:"planName,omitempty"`
	PlanVersion         int                        `json:"planVersion,omitempty"`
	Objective           string                     `json:"objective,omitempty"`
	Provider            string                     `json:"provider"`
	Model               string                     `json:"model"`
	Capacity            CapacityConfig             `json:"capacity"`
	VerifyCommand       string                     `json:"verifyCommand,omitempty"`
	VerifyCommandDigest string                     `json:"verifyCommandDigest,omitempty"`
	TaskCount           int                        `json:"taskCount"`
	Completed           int                        `json:"completed"`
	Failed              int                        `json:"failed"`
	ToolCalls           int                        `json:"toolCalls"`
	Verification        receiptVerificationPayload `json:"verification"`
	Tasks               []teamTaskReceiptPayload   `json:"tasks"`
}

type teamTaskReceiptPayload struct {
	ID            string             `json:"id"`
	Name          string             `json:"name"`
	Description   string             `json:"description,omitempty"`
	Kind          string             `json:"kind,omitempty"`
	Role          string             `json:"role,omitempty"`
	Provider      string             `json:"provider,omitempty"`
	Model         string             `json:"model,omitempty"`
	ReadOnly      bool               `json:"readOnly,omitempty"`
	Resources     AgentTaskResources `json:"resources,omitempty"`
	Status        string             `json:"status"`
	AssignedTo    string             `json:"assignedTo,omitempty"`
	Files         []string           `json:"files,omitempty"`
	Artifacts     []ReceiptArtifact  `json:"artifacts,omitempty"`
	DependsOn     []string           `json:"dependsOn,omitempty"`
	Wave          int                `json:"wave"`
	ToolCalls     int                `json:"toolCalls"`
	OutputPath    string             `json:"outputPath,omitempty"`
	ResultSummary string             `json:"resultSummary,omitempty"`
	StartedAt     string             `json:"startedAt,omitempty"`
	FinishedAt    string             `json:"finishedAt,omitempty"`
}

// marshalSanitizedReceipt is the only JSON boundary used by receipt writers.
// It constructs a deep, sanitized copy and never mutates or serializes the raw
// input receipt.
func marshalSanitizedReceipt(workDir string, receipt any) ([]byte, error) {
	var payload any
	switch value := receipt.(type) {
	case AgentReceipt:
		payload = sanitizeAgentReceipt(workDir, value)
	case TeamRunReceipt:
		payload = sanitizeTeamRunReceipt(workDir, value)
	default:
		return nil, fmt.Errorf("unsupported receipt type %T", receipt)
	}
	return json.MarshalIndent(payload, "", "  ")
}

// MarshalJSON routes every AgentReceipt serialization through the same
// sanitizing boundary used by the durable writer.
func (receipt AgentReceipt) MarshalJSON() ([]byte, error) {
	return marshalSanitizedReceipt(receipt.WorkDir, receipt)
}

// MarshalJSON routes every TeamRunReceipt serialization through the same
// sanitizing boundary used by the durable writer.
func (receipt TeamRunReceipt) MarshalJSON() ([]byte, error) {
	return marshalSanitizedReceipt(receipt.WorkDir, receipt)
}

func sanitizeAgentReceipt(workDir string, receipt AgentReceipt) agentReceiptPayload {
	artifacts := collectReceiptArtifacts(workDir, receipt.EditedFiles)
	editedFiles := make([]string, 0, len(artifacts))
	for _, artifact := range artifacts {
		editedFiles = append(editedFiles, artifact.Path)
	}
	completion := sanitizeCompletionContractSnapshot(receipt.Completion)
	recovery := sanitizeRunGuardSnapshot(receipt.Recovery)
	verification := sanitizeReceiptVerification(receipt.Verification)
	if completion != nil && (completion.Status == CompletionStatusIncomplete || completion.Status == CompletionStatusBlocked) {
		verification.TerminalState = EvidenceTerminalBlocked
	}
	return agentReceiptPayload{
		Version:      receipt.Version,
		CreatedAt:    sanitizeReceiptString(receipt.CreatedAt),
		WorkDir:      sanitizeReceiptString(receipt.WorkDir),
		Provider:     sanitizeReceiptString(receipt.Provider),
		Model:        sanitizeReceiptString(receipt.Model),
		ProjectType:  sanitizeReceiptString(receipt.ProjectType),
		PlanMode:     receipt.PlanMode,
		Iterations:   receipt.Iterations,
		EditedFiles:  editedFiles,
		Artifacts:    artifacts,
		Verification: verification,
		Completion:   completion,
		Recovery:     recovery,
	}
}

func sanitizeRunGuardSnapshot(value *RunGuardSnapshot) *RunGuardSnapshot {
	if value == nil {
		return nil
	}
	result := &RunGuardSnapshot{
		RepeatLimit:           value.RepeatLimit,
		Observations:          value.Observations,
		Denied:                value.Denied,
		LastDeniedOccurrences: value.LastDeniedOccurrences,
	}
	if result.RepeatLimit < 0 {
		result.RepeatLimit = 0
	}
	if result.Observations < 0 {
		result.Observations = 0
	}
	if result.Denied < 0 || result.Denied > result.Observations {
		result.Denied = 0
	}
	if result.LastDeniedOccurrences < 0 {
		result.LastDeniedOccurrences = 0
	}
	if value.LastDeniedReason == RunGuardRepeatedAction {
		result.LastDeniedReason = value.LastDeniedReason
	}
	if len(value.LastDeniedFingerprint) == sha256.Size*2 {
		if _, err := hex.DecodeString(value.LastDeniedFingerprint); err == nil {
			result.LastDeniedFingerprint = value.LastDeniedFingerprint
		}
	}
	if result.Denied == 0 {
		result.LastDeniedReason = ""
		result.LastDeniedFingerprint = ""
		result.LastDeniedOccurrences = 0
	}
	return result
}

// sanitizeCompletionContractSnapshot constructs a bounded content-safe copy.
// Snapshot fields are public for transport use, so this boundary re-applies
// redaction and limits instead of trusting their original constructor.
func sanitizeCompletionContractSnapshot(value *CompletionContractSnapshot) *CompletionContractSnapshot {
	if value == nil {
		return nil
	}
	criteriaCount := len(value.Criteria)
	if criteriaCount > maxCompletionCriteria {
		criteriaCount = maxCompletionCriteria
	}
	result := &CompletionContractSnapshot{
		Version:      value.Version,
		RunID:        completionReceiptText(value.RunID, maxCompletionRunIDBytes),
		PlanRevision: value.PlanRevision,
		Revision:     value.Revision,
		Status:       sanitizeCompletionContractStatus(value.Status),
		Criteria:     make([]CompletionCriterionSnapshot, 0, criteriaCount),
	}
	for _, criterion := range value.Criteria[:criteriaCount] {
		evidenceDigest := criterion.EvidenceDigest
		if !validCompletionEvidenceDigest(evidenceDigest) {
			evidenceDigest = ""
		}
		result.Criteria = append(result.Criteria, CompletionCriterionSnapshot{
			ID:               completionReceiptText(criterion.ID, maxCompletionCriterionIDBytes),
			Text:             completionReceiptText(criterion.Text, maxCompletionCriterionTextBytes),
			EvidenceRequired: criterion.EvidenceRequired,
			State:            sanitizeCompletionClaimState(criterion.State),
			EvidenceDigest:   evidenceDigest,
			Summary:          completionReceiptText(criterion.Summary, maxCompletionSummaryBytes),
			Assertion:        completionReceiptText(criterion.Assertion, maxCompletionSummaryBytes),
			ClaimRevision:    criterion.ClaimRevision,
		})
	}
	return result
}

func sanitizeCompletionContractStatus(value CompletionContractStatus) CompletionContractStatus {
	switch value {
	case CompletionStatusComplete, CompletionStatusIncomplete, CompletionStatusBlocked:
		return value
	default:
		return CompletionStatusIncomplete
	}
}

func sanitizeCompletionClaimState(value CompletionClaimState) CompletionClaimState {
	switch value {
	case CompletionClaimPending, CompletionClaimSatisfied, CompletionClaimBlocked:
		return value
	default:
		return CompletionClaimPending
	}
}

func sanitizeTeamRunReceipt(workDir string, receipt TeamRunReceipt) teamRunReceiptPayload {
	tasks := make([]teamTaskReceiptPayload, 0, len(receipt.Tasks))
	for _, task := range receipt.Tasks {
		tasks = append(tasks, teamTaskReceiptPayload{
			ID:            sanitizeReceiptString(task.ID),
			Name:          sanitizeReceiptString(task.Name),
			Description:   sanitizeReceiptString(task.Description),
			Kind:          sanitizeReceiptString(string(task.Kind)),
			Role:          sanitizeReceiptString(task.Role),
			Provider:      sanitizeReceiptString(task.Provider),
			Model:         sanitizeReceiptString(task.Model),
			ReadOnly:      task.ReadOnly,
			Resources:     task.Resources,
			Status:        sanitizeReceiptString(task.Status),
			AssignedTo:    sanitizeReceiptString(task.AssignedTo),
			Files:         sanitizeReceiptStrings(task.Files),
			Artifacts:     collectReceiptArtifacts(workDir, task.Files),
			DependsOn:     sanitizeReceiptStrings(task.DependsOn),
			Wave:          task.Wave,
			ToolCalls:     task.ToolCalls,
			OutputPath:    sanitizeReceiptString(task.OutputPath),
			ResultSummary: sanitizeReceiptString(task.ResultSummary),
			StartedAt:     sanitizeReceiptString(task.StartedAt),
			FinishedAt:    sanitizeReceiptString(task.FinishedAt),
		})
	}
	return teamRunReceiptPayload{
		Version:             receipt.Version,
		Kind:                sanitizeReceiptString(receipt.Kind),
		CreatedAt:           sanitizeReceiptString(receipt.CreatedAt),
		WorkDir:             sanitizeReceiptString(receipt.WorkDir),
		Status:              sanitizeReceiptString(receipt.Status),
		TeamName:            sanitizeReceiptString(receipt.TeamName),
		PlanName:            sanitizeReceiptString(receipt.PlanName),
		PlanVersion:         receipt.PlanVersion,
		Objective:           sanitizeReceiptString(receipt.Objective),
		Provider:            sanitizeReceiptString(receipt.Provider),
		Model:               sanitizeReceiptString(receipt.Model),
		Capacity:            receipt.Capacity,
		VerifyCommand:       sanitizeReceiptString(receipt.VerifyCommand),
		VerifyCommandDigest: sha256String(receipt.VerifyCommand),
		TaskCount:           receipt.TaskCount,
		Completed:           receipt.Completed,
		Failed:              receipt.Failed,
		ToolCalls:           receipt.ToolCalls,
		Verification:        sanitizeReceiptVerification(receipt.Verification),
		Tasks:               tasks,
	}
}

func sanitizeReceiptVerification(value ReceiptVerification) receiptVerificationPayload {
	evidence := make([]receiptEvidencePayload, 0, len(value.Evidence))
	for _, record := range value.Evidence {
		evidence = append(evidence, receiptEvidencePayload{
			Source:        sanitizeReceiptString(record.Source),
			Command:       sanitizeReceiptString(record.Command),
			CommandDigest: sha256String(record.Command),
			Status:        sanitizeReceiptString(record.Status),
			Summary:       sanitizeReceiptString(record.Summary),
			ExitCode:      record.ExitCode,
			At:            sanitizeReceiptString(record.At),
		})
	}
	return receiptVerificationPayload{
		Status:        sanitizeReceiptString(value.Status),
		TerminalState: sanitizeReceiptString(evidenceTerminalState(value.Gate, value.Status, value.Evidence)),
		Source:        sanitizeReceiptString(value.Source),
		Command:       sanitizeReceiptString(value.Command),
		CommandDigest: sha256String(value.Command),
		Summary:       sanitizeReceiptString(value.Summary),
		Gate:          sanitizeReceiptString(value.Gate),
		Mode:          sanitizeReceiptString(value.Mode),
		Evidence:      evidence,
	}
}

func sanitizeReceiptStrings(values []string) []string {
	if values == nil {
		return nil
	}
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = sanitizeReceiptString(value)
	}
	return result
}

var (
	receiptPEMPattern            = regexp.MustCompile(`(?s)-----BEGIN[ \t]+(?:[A-Z0-9]+[ \t]+)*PRIVATE KEY-----.*?-----END[ \t]+(?:[A-Z0-9]+[ \t]+)*PRIVATE KEY-----`)
	receiptPEMBeginPattern       = regexp.MustCompile(`-----BEGIN[ \t]+(?:[A-Z0-9]+[ \t]+)*PRIVATE KEY-----`)
	receiptURLCredentialsPattern = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^/\s:@]+:[^/\s@]+@`)
	receiptAuthorizationPattern  = regexp.MustCompile(`(?i)\b(authorization(?:"|')?\s*[:=]\s*(?:"|')?)(?:bearer|basic)\s+[A-Za-z0-9._~+/=-]{4,}`)
	receiptBearerPattern         = regexp.MustCompile(`(?i)\b(bearer\s+)[A-Za-z0-9._~+/=-]{8,}`)
	receiptAssignmentPattern     = regexp.MustCompile(`(?i)((?:"|')?\b(?:[a-z0-9]+[._-])*(?:api[_-]?key|apikey|access[_-]?key|accesskey|client[_-]?secret|clientsecret|private[_-]?key|privatekey|password|passwd|pwd|auth[_-]?token|authtoken|refresh[_-]?token|refreshtoken|access[_-]?token|accesstoken|token|secret)(?:[._-][a-z0-9]+)*(?:"|')?\s*[:=]\s*)(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s,;}\]]+)`)
	receiptOpenAIKeyPattern      = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`)
	receiptGitTokenPattern       = regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_-]{20,})\b`)
	receiptAWSKeyPattern         = regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`)
	receiptSlackTokenPattern     = regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`)
)

func sanitizeReceiptString(value string) string {
	if value == "" {
		return ""
	}
	if len(value) > maxReceiptStringBytes {
		return "[REDACTED: oversized receipt field]"
	}
	return redactSensitiveString(value)
}

// redactSensitiveString applies the shared secret corpus without imposing a
// receipt-sized field limit. Callers must establish their own byte bound
// before using it for larger durable transcript fields.
func redactSensitiveString(value string) string {
	if value == "" {
		return ""
	}
	if !utf8.ValidString(value) {
		value = strings.ToValidUTF8(value, "�")
	}
	value = receiptPEMPattern.ReplaceAllString(value, "[REDACTED PRIVATE KEY]")
	// A bounded transcript can end after BEGIN but before END. Treat that tail
	// as sensitive instead of requiring a complete PEM block to match.
	if location := receiptPEMBeginPattern.FindStringIndex(value); location != nil {
		value = value[:location[0]] + "[REDACTED PRIVATE KEY]"
	}
	value = receiptURLCredentialsPattern.ReplaceAllString(value, `$1[REDACTED]@`)
	value = receiptAuthorizationPattern.ReplaceAllString(value, `$1[REDACTED]`)
	value = receiptBearerPattern.ReplaceAllString(value, `$1[REDACTED]`)
	value = receiptAssignmentPattern.ReplaceAllString(value, `$1"[REDACTED]"`)
	value = receiptOpenAIKeyPattern.ReplaceAllString(value, "[REDACTED]")
	value = receiptGitTokenPattern.ReplaceAllString(value, "[REDACTED]")
	value = receiptAWSKeyPattern.ReplaceAllString(value, "[REDACTED]")
	value = receiptSlackTokenPattern.ReplaceAllString(value, "[REDACTED]")
	return value
}

func sha256String(value string) string {
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func collectReceiptArtifacts(workDir string, paths []string) []ReceiptArtifact {
	if paths == nil {
		return nil
	}
	artifacts := make([]ReceiptArtifact, 0, len(paths))
	for _, path := range paths {
		artifacts = append(artifacts, buildReceiptArtifact(workDir, path))
	}
	return artifacts
}

func buildReceiptArtifact(workDir, requestedPath string) ReceiptArtifact {
	requestedPath = strings.TrimSpace(requestedPath)
	if requestedPath == "" {
		return ReceiptArtifact{Status: ReceiptArtifactInvalid}
	}
	if strings.ContainsAny(requestedPath, "*?[") {
		return ReceiptArtifact{
			Path:   sanitizeReceiptString(filepath.ToSlash(requestedPath)),
			Status: ReceiptArtifactPattern,
		}
	}

	root, err := canonicalWorkspacePath(workDir)
	if err != nil {
		return ReceiptArtifact{Path: "[invalid-workspace]", Status: ReceiptArtifactInvalid}
	}
	candidate := requestedPath
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(root, filepath.FromSlash(candidate))
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil {
		return ReceiptArtifact{Path: "[invalid-path]", Status: ReceiptArtifactInvalid}
	}
	candidate = filepath.Clean(candidate)
	rel, inside := receiptRelativePath(root, candidate)
	if !inside {
		return ReceiptArtifact{Path: "[outside-workspace]", Status: ReceiptArtifactOutsideWorkspace}
	}
	artifact := ReceiptArtifact{Path: sanitizeReceiptString(filepath.ToSlash(rel))}

	info, linkFound, inspectErr := inspectReceiptArtifactPath(root, candidate)
	if linkFound {
		artifact.Status = ReceiptArtifactSymlink
		return artifact
	}
	if inspectErr != nil {
		if os.IsNotExist(inspectErr) {
			artifact.Status = ReceiptArtifactMissing
		} else {
			artifact.Status = ReceiptArtifactUnreadable
		}
		return artifact
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		artifact.Status = ReceiptArtifactUnreadable
		return artifact
	}
	if _, inside := receiptRelativePath(root, resolved); !inside {
		artifact.Status = ReceiptArtifactSymlink
		return artifact
	}
	if !info.Mode().IsRegular() {
		artifact.Status = ReceiptArtifactNonRegular
		return artifact
	}

	file, err := os.Open(candidate)
	if err != nil {
		artifact.Status = ReceiptArtifactUnreadable
		return artifact
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		artifact.Status = ReceiptArtifactChanged
		return artifact
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		artifact.Status = ReceiptArtifactUnreadable
		return artifact
	}
	afterInfo, err := file.Stat()
	if err != nil || !os.SameFile(openedInfo, afterInfo) || openedInfo.Size() != afterInfo.Size() || !openedInfo.ModTime().Equal(afterInfo.ModTime()) {
		artifact.Status = ReceiptArtifactChanged
		return artifact
	}
	size := afterInfo.Size()
	artifact.Status = ReceiptArtifactOK
	artifact.Size = &size
	artifact.SHA256 = "sha256:" + hex.EncodeToString(hash.Sum(nil))
	return artifact
}

func inspectReceiptArtifactPath(root, candidate string) (os.FileInfo, bool, error) {
	rel, inside := receiptRelativePath(root, candidate)
	if !inside {
		return nil, false, fmt.Errorf("artifact escapes workspace")
	}
	current := root
	parts := strings.Split(filepath.Clean(rel), string(os.PathSeparator))
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return nil, false, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return info, true, nil
		}
	}
	info, err := os.Lstat(candidate)
	return info, false, err
}

func receiptRelativePath(root, candidate string) (string, bool) {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", false
	}
	return rel, true
}
