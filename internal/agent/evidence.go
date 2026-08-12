package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	EvidenceModeQuick  = "quick"
	EvidenceModeNormal = "normal"
	EvidenceModeDeep   = "deep"

	EvidencePolicyOff      = "off"
	EvidencePolicyMeasure  = "measure"
	EvidencePolicyAdvisory = "advisory"
	EvidencePolicyBlock    = "block"

	EvidenceGateAllow      = "allow"
	EvidenceGateWarn       = "warn"
	EvidenceGateWouldBlock = "would-block"
	EvidenceGateBlock      = "block"

	EvidenceTerminalVerified          = "verified"
	EvidenceTerminalPartiallyVerified = "partially-verified"
	EvidenceTerminalUnverified        = "unverified"
	EvidenceTerminalBlocked           = "blocked"

	maxCompletionEvidenceRefs        = 256
	maxCompletionEvidenceToolNameLen = 128
	maxCompletionEvidenceInputBytes  = 1 << 20
	maxCompletionEvidenceResultBytes = 4 << 20
)

var (
	errCompletionEvidenceInvalidRunID = errors.New("completion evidence run ID is invalid")
	errCompletionEvidenceRunBound     = errors.New("completion evidence ledger is already bound to another run")
	errCompletionEvidenceNotBound     = errors.New("completion evidence ledger is not bound to a run")
	errCompletionEvidenceLimit        = errors.New("completion evidence reference limit reached")
	errCompletionEvidenceInvalidTool  = errors.New("completion evidence tool name is invalid")
	errCompletionEvidenceFieldLarge   = errors.New("completion evidence field exceeds its bound")
	errCompletionEvidenceCollision    = errors.New("completion evidence digest collision")
)

type CompletionEvidenceSource string

const (
	CompletionEvidenceSourceTool       CompletionEvidenceSource = "tool"
	CompletionEvidenceSourceAutoVerify CompletionEvidenceSource = "auto-verify"
)

// CompletionToolOutcome is the execution-bound input for completion evidence.
// Denied, synthetic, or not-executed outcomes are deliberately ignored.
type CompletionToolOutcome struct {
	ToolName  string
	Input     json.RawMessage
	Result    string
	IsError   bool
	Executed  bool
	Synthetic bool
	Denied    bool
}

// CompletionEvidenceRef contains only bounded metadata and a canonical
// digest. Raw tool input, result, command, and preview content are never kept.
type CompletionEvidenceRef struct {
	Digest    string                   `json:"digest"`
	Sequence  uint64                   `json:"sequence"`
	Source    CompletionEvidenceSource `json:"source"`
	ToolName  string                   `json:"toolName"`
	Succeeded bool                     `json:"succeeded"`
}

// CompletionEvidenceSnapshot is append ordered and receipt/prompt safe.
type CompletionEvidenceSnapshot struct {
	RunID string                  `json:"runId"`
	Refs  []CompletionEvidenceRef `json:"refs"`
}

type EvidencePolicyConfig struct {
	Policy        string `json:"policy"`
	MaxStopBlocks int    `json:"maxStopBlocks"`
}

type EvidenceRecord struct {
	Source   string `json:"source"`
	Command  string `json:"command,omitempty"`
	Status   string `json:"status"` // passed, failed, skipped
	Summary  string `json:"summary,omitempty"`
	ExitCode int    `json:"exitCode,omitempty"`
	At       string `json:"at,omitempty"`
}

type EvidenceGateResult struct {
	Decision      string   `json:"decision"`
	TerminalState string   `json:"terminalState"`
	Mode          string   `json:"mode"`
	Policy        string   `json:"policy"`
	Summary       string   `json:"summary"`
	Risks         []string `json:"risks,omitempty"`
}

type EvidenceLedger struct {
	mu sync.Mutex

	Mode          string
	Policy        string
	MaxStopBlocks int
	StopBlocks    int
	Risks         []string
	ChangedFiles  []string
	Records       []EvidenceRecord

	completionRunID           string
	completionRefs            []CompletionEvidenceRef
	completionSuccessByDigest map[string]bool
}

func DefaultEvidencePolicyConfig() EvidencePolicyConfig {
	return EvidencePolicyConfig{
		Policy:        evidencePolicyFromEnv(),
		MaxStopBlocks: 2,
	}
}

func NormalizeEvidencePolicyConfig(cfg EvidencePolicyConfig) EvidencePolicyConfig {
	policy := strings.ToLower(strings.TrimSpace(cfg.Policy))
	switch policy {
	case EvidencePolicyOff, EvidencePolicyMeasure, EvidencePolicyAdvisory, EvidencePolicyBlock:
	default:
		policy = evidencePolicyFromEnv()
	}
	if cfg.MaxStopBlocks <= 0 {
		cfg.MaxStopBlocks = 2
	}
	cfg.Policy = policy
	return cfg
}

func NewEvidenceLedger(prompt string, cfg EvidencePolicyConfig) *EvidenceLedger {
	mode, risks := ClassifyEvidenceMode(prompt)
	policy := NormalizeEvidencePolicyConfig(cfg)
	return &EvidenceLedger{
		Mode:          mode,
		Policy:        policy.Policy,
		MaxStopBlocks: policy.MaxStopBlocks,
		Risks:         risks,
	}
}

func ClassifyEvidenceMode(prompt string) (string, []string) {
	lower := strings.ToLower(prompt)
	risks := detectEvidenceRisks(lower)
	if len(risks) > 0 || containsAny(lower, deepEvidenceTriggers) {
		return EvidenceModeDeep, risks
	}
	if containsAny(lower, quickEvidenceTriggers) {
		return EvidenceModeQuick, risks
	}
	if containsAny(lower, normalEvidenceTriggers) {
		return EvidenceModeNormal, risks
	}
	return EvidenceModeQuick, risks
}

// BindCompletionRun binds this run-owned ledger to exactly one completion
// contract run. Repeating the same binding is idempotent; rebinding is denied.
func (l *EvidenceLedger) BindCompletionRun(runID string) error {
	if l == nil || !validCompletionEvidenceRunID(runID) {
		return errCompletionEvidenceInvalidRunID
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.completionRunID != "" {
		if l.completionRunID == runID {
			return nil
		}
		return errCompletionEvidenceRunBound
	}
	l.completionRunID = runID
	l.completionRefs = make([]CompletionEvidenceRef, 0, maxCompletionEvidenceRefs)
	l.completionSuccessByDigest = make(map[string]bool, maxCompletionEvidenceRefs)
	return nil
}

// ObserveCompletionToolOutcome appends evidence only after a real executor
// outcome. ReportCompletion is a claim transport and can never prove itself.
func (l *EvidenceLedger) ObserveCompletionToolOutcome(outcome CompletionToolOutcome) (CompletionEvidenceRef, error) {
	if !outcome.Executed || outcome.Synthetic || outcome.Denied || strings.EqualFold(strings.TrimSpace(outcome.ToolName), "ReportCompletion") {
		return CompletionEvidenceRef{}, nil
	}
	toolName, err := safeCompletionEvidenceToolName(outcome.ToolName)
	if err != nil {
		return CompletionEvidenceRef{}, err
	}
	return l.appendCompletionEvidence(
		CompletionEvidenceSourceTool,
		toolName,
		[]byte(outcome.Input),
		[]byte(outcome.Result),
		!outcome.IsError,
	)
}

// CompletionEvidenceSnapshot returns a defensive, bounded append-order copy.
func (l *EvidenceLedger) CompletionEvidenceSnapshot() CompletionEvidenceSnapshot {
	if l == nil {
		return CompletionEvidenceSnapshot{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return CompletionEvidenceSnapshot{
		RunID: completionReceiptText(l.completionRunID, maxCompletionRunIDBytes),
		Refs:  append([]CompletionEvidenceRef(nil), l.completionRefs...),
	}
}

// ResolveCompletionEvidence implements CompletionEvidenceResolver. Only a
// successful ref from the exact bound run can satisfy a completion claim.
func (l *EvidenceLedger) ResolveCompletionEvidence(runID, evidenceDigest string) (bool, error) {
	if l == nil {
		return false, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if runID == "" || runID != l.completionRunID {
		return false, nil
	}
	succeeded, found := l.completionSuccessByDigest[evidenceDigest]
	return found && succeeded, nil
}

func (l *EvidenceLedger) ObserveToolResult(toolName string, input json.RawMessage, output string, isError bool) {
	if l == nil {
		return
	}
	if isEditTool(toolName) && !isError {
		if path := evidencePathFromInput(input); path != "" {
			l.ObserveChangedFile(path)
		}
	}
	if toolName != "Bash" {
		return
	}
	command := evidenceCommandFromInput(input)
	if command == "" || !IsVerificationCommand(command) {
		return
	}
	status := inferEvidenceStatus(output, isError)
	l.addRecord(EvidenceRecord{
		Source:  "tool",
		Command: command,
		Status:  status,
		Summary: truncateStr(strings.TrimSpace(output), 600),
		At:      time.Now().UTC().Format(time.RFC3339Nano),
	})
}

func (l *EvidenceLedger) ObserveChangedFile(path string) {
	if l == nil {
		return
	}
	path = filepath.ToSlash(strings.TrimSpace(path))
	if path == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, existing := range l.ChangedFiles {
		if existing == path {
			return
		}
	}
	l.ChangedFiles = append(l.ChangedFiles, path)
}

func (l *EvidenceLedger) ObserveAutoVerify(output string, failed bool, ran bool) {
	if l == nil || !ran {
		return
	}
	status := "passed"
	if failed {
		status = "failed"
	}
	l.addRecord(EvidenceRecord{
		Source:  "auto-verify",
		Status:  status,
		Summary: truncateStr(strings.TrimSpace(output), 600),
		At:      time.Now().UTC().Format(time.RFC3339Nano),
	})
	_, _ = l.appendCompletionEvidence(
		CompletionEvidenceSourceAutoVerify,
		string(CompletionEvidenceSourceAutoVerify),
		nil,
		[]byte(output),
		!failed,
	)
}

func (l *EvidenceLedger) Evaluate() EvidenceGateResult {
	if l == nil {
		return EvidenceGateResult{
			Decision:      EvidenceGateAllow,
			TerminalState: EvidenceTerminalUnverified,
			Mode:          EvidenceModeQuick,
			Policy:        EvidencePolicyOff,
			Summary:       "no evidence ledger",
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.Policy == EvidencePolicyOff {
		return l.gateLocked(EvidenceGateAllow, "evidence policy disabled")
	}
	if l.Mode == EvidenceModeQuick {
		return l.gateLocked(EvidenceGateAllow, "quick task does not require completion evidence")
	}
	if len(l.ChangedFiles) == 0 {
		return l.gateLocked(EvidenceGateAllow, "no file changes observed")
	}
	if l.docsOnlyLocked() {
		return l.gateLocked(EvidenceGateAllow, "docs-only change")
	}
	if l.hasSuccessfulVerificationLocked() {
		return l.gateLocked(EvidenceGateAllow, "successful verification evidence recorded")
	}
	if l.Mode != EvidenceModeDeep {
		return l.gateLocked(EvidenceGateWarn, "changed files without successful verification evidence")
	}

	switch l.Policy {
	case EvidencePolicyBlock:
		if l.StopBlocks >= l.MaxStopBlocks {
			return l.gateLocked(EvidenceGateWarn, "evidence block budget exhausted; completion allowed with warning")
		}
		return l.gateLocked(EvidenceGateBlock, "deep change requires successful verification evidence")
	case EvidencePolicyAdvisory:
		return l.gateLocked(EvidenceGateWarn, "deep change has no successful verification evidence")
	default:
		return l.gateLocked(EvidenceGateWouldBlock, "deep change would require verification evidence in block mode")
	}
}

func (l *EvidenceLedger) MarkStopBlock() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.StopBlocks++
}

func (l *EvidenceLedger) ApplyToReceipt(v ReceiptVerification) ReceiptVerification {
	if l == nil {
		return v
	}
	gate := l.Evaluate()
	l.mu.Lock()
	records := append([]EvidenceRecord(nil), l.Records...)
	best := bestEvidenceRecordLocked(l.Records)
	l.mu.Unlock()
	if v.Status == "" || v.Status == "not-run" {
		if best.Status != "" {
			v.Status = best.Status
			v.Source = best.Source
		}
	}
	if v.Command == "" {
		v.Command = best.Command
	}
	v.Gate = gate.Decision
	v.Mode = gate.Mode
	v.TerminalState = evidenceTerminalState(gate.Decision, v.Status, records)
	if v.Summary == "" {
		v.Summary = gate.Summary
	}
	if len(records) > 0 {
		v.Evidence = records
	}
	return v
}

func bestEvidenceRecordLocked(records []EvidenceRecord) EvidenceRecord {
	var failed EvidenceRecord
	for _, record := range records {
		if record.Status == "passed" {
			return record
		}
		if failed.Status == "" && record.Status == "failed" {
			failed = record
		}
	}
	return failed
}

func (l *EvidenceLedger) gateLocked(decision, summary string) EvidenceGateResult {
	return EvidenceGateResult{
		Decision:      decision,
		TerminalState: evidenceTerminalState(decision, "", l.Records),
		Mode:          l.Mode,
		Policy:        l.Policy,
		Summary:       summary,
		Risks:         append([]string(nil), l.Risks...),
	}
}

// evidenceTerminalState deliberately separates policy disposition from
// completion confidence. An advisory policy may allow a run to finish while
// its evidence remains unverified, and a mixed pass/fail history is never
// promoted to fully verified.
func evidenceTerminalState(decision, status string, records []EvidenceRecord) string {
	if decision == EvidenceGateBlock {
		return EvidenceTerminalBlocked
	}

	hasPassed := status == "passed"
	hasFailed := status == "failed"
	for _, record := range records {
		switch record.Status {
		case "passed":
			hasPassed = true
		case "failed":
			hasFailed = true
		}
	}

	if (decision == "" || decision == EvidenceGateAllow) && hasPassed && !hasFailed {
		return EvidenceTerminalVerified
	}
	if hasPassed {
		return EvidenceTerminalPartiallyVerified
	}
	return EvidenceTerminalUnverified
}

func (l *EvidenceLedger) addRecord(record EvidenceRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Records = append(l.Records, record)
}

func (l *EvidenceLedger) appendCompletionEvidence(
	source CompletionEvidenceSource,
	toolName string,
	input []byte,
	result []byte,
	succeeded bool,
) (CompletionEvidenceRef, error) {
	if l == nil {
		return CompletionEvidenceRef{}, errCompletionEvidenceNotBound
	}
	if len(input) > maxCompletionEvidenceInputBytes || len(result) > maxCompletionEvidenceResultBytes {
		return CompletionEvidenceRef{}, errCompletionEvidenceFieldLarge
	}
	inputDigest := completionEvidenceSHA256(input)
	resultDigest := completionEvidenceSHA256(result)

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.completionRunID == "" {
		return CompletionEvidenceRef{}, errCompletionEvidenceNotBound
	}
	if len(l.completionRefs) >= maxCompletionEvidenceRefs {
		return CompletionEvidenceRef{}, errCompletionEvidenceLimit
	}
	sequence := uint64(len(l.completionRefs) + 1)
	digest := canonicalCompletionEvidenceDigest(completionEvidenceDigestInput{
		RunID:        l.completionRunID,
		Sequence:     sequence,
		Source:       source,
		ToolName:     toolName,
		InputSHA256:  inputDigest,
		ResultSHA256: resultDigest,
		Succeeded:    succeeded,
	})
	if _, exists := l.completionSuccessByDigest[digest]; exists {
		return CompletionEvidenceRef{}, errCompletionEvidenceCollision
	}
	ref := CompletionEvidenceRef{
		Digest:    digest,
		Sequence:  sequence,
		Source:    source,
		ToolName:  toolName,
		Succeeded: succeeded,
	}
	l.completionRefs = append(l.completionRefs, ref)
	l.completionSuccessByDigest[digest] = succeeded
	return ref, nil
}

type completionEvidenceDigestInput struct {
	RunID        string                   `json:"runId"`
	Sequence     uint64                   `json:"sequence"`
	Source       CompletionEvidenceSource `json:"source"`
	ToolName     string                   `json:"toolName"`
	InputSHA256  string                   `json:"inputSha256"`
	ResultSHA256 string                   `json:"resultSha256"`
	Succeeded    bool                     `json:"succeeded"`
}

func canonicalCompletionEvidenceDigest(input completionEvidenceDigestInput) string {
	encoded, _ := json.Marshal(input)
	return completionEvidenceSHA256(encoded)
}

func completionEvidenceSHA256(value []byte) string {
	digest := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func validCompletionEvidenceRunID(runID string) bool {
	return runID != "" &&
		runID == strings.TrimSpace(runID) &&
		len(runID) <= maxCompletionRunIDBytes &&
		utf8.ValidString(runID) &&
		!containsCompletionControl(runID)
}

func safeCompletionEvidenceToolName(toolName string) (string, error) {
	if toolName == "" || toolName != strings.TrimSpace(toolName) || len(toolName) > maxCompletionEvidenceToolNameLen {
		return "", errCompletionEvidenceInvalidTool
	}
	for index := 0; index < len(toolName); index++ {
		value := toolName[index]
		if (value >= 'a' && value <= 'z') ||
			(value >= 'A' && value <= 'Z') ||
			(value >= '0' && value <= '9') ||
			strings.ContainsRune("_.:/-", rune(value)) {
			continue
		}
		return "", errCompletionEvidenceInvalidTool
	}
	return toolName, nil
}

func (l *EvidenceLedger) hasSuccessfulVerificationLocked() bool {
	for _, record := range l.Records {
		if record.Status == "passed" {
			return true
		}
	}
	return false
}

func (l *EvidenceLedger) docsOnlyLocked() bool {
	if len(l.ChangedFiles) == 0 {
		return false
	}
	for _, path := range l.ChangedFiles {
		if changeKind(path) != "docs" {
			return false
		}
	}
	return true
}

func IsVerificationCommand(command string) bool {
	lower := strings.ToLower(strings.TrimSpace(command))
	if lower == "" {
		return false
	}
	return containsAny(lower, verificationCommandSignals)
}

func evidencePolicyFromEnv() string {
	policy := strings.ToLower(renamedAgentEnv("CORELAY_EVIDENCE_POLICY", "ANICLEW_EVIDENCE_POLICY"))
	switch policy {
	case EvidencePolicyOff, EvidencePolicyMeasure, EvidencePolicyAdvisory, EvidencePolicyBlock:
		return policy
	default:
		return EvidencePolicyMeasure
	}
}

func inferEvidenceStatus(output string, isError bool) string {
	lower := strings.ToLower(output)
	if isError || containsAny(lower, failedEvidenceSignals) {
		return "failed"
	}
	return "passed"
}

func evidenceCommandFromInput(input json.RawMessage) string {
	var args struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(input, &args)
	return strings.TrimSpace(args.Command)
}

func evidencePathFromInput(input json.RawMessage) string {
	var args struct {
		FilePath string `json:"file_path"`
		Path     string `json:"path"`
	}
	_ = json.Unmarshal(input, &args)
	if strings.TrimSpace(args.FilePath) != "" {
		return args.FilePath
	}
	return args.Path
}

func isEditTool(toolName string) bool {
	return toolName == "Write" || toolName == "Edit" || toolName == "MultiEdit"
}

func changeKind(path string) string {
	lower := strings.ToLower(filepath.ToSlash(path))
	ext := strings.ToLower(filepath.Ext(lower))
	base := strings.ToLower(filepath.Base(lower))
	if ext == ".md" || ext == ".mdx" || ext == ".txt" || ext == ".rst" || strings.HasPrefix(lower, "docs/") || base == "readme" || base == "readme.md" {
		return "docs"
	}
	if ext == ".go" || ext == ".rs" || ext == ".py" || ext == ".js" || ext == ".jsx" || ext == ".ts" || ext == ".tsx" || ext == ".cs" || ext == ".java" || ext == ".kt" || ext == ".swift" {
		return "code"
	}
	if ext == ".json" || ext == ".yaml" || ext == ".yml" || ext == ".toml" || ext == ".ini" || ext == ".env" {
		return "config"
	}
	if strings.Contains(lower, "test") || strings.Contains(lower, "spec") {
		return "test"
	}
	return "asset"
}

func detectEvidenceRisks(prompt string) []string {
	var risks []string
	add := func(name string, signals []string) {
		if containsAny(prompt, signals) {
			for _, risk := range risks {
				if risk == name {
					return
				}
			}
			risks = append(risks, name)
		}
	}
	add("production", []string{"production", "prod", "go-live", "launch", "deploy", "배포", "출시", "운영"})
	add("database", []string{"database", "migration", "schema", "db ", " db", "sql", "데이터베이스", "마이그레이션", "스키마"})
	add("auth-security", []string{"auth", "oauth", "jwt", "security", "secret", "token", "password", "인증", "보안", "비밀", "토큰"})
	add("remote-write", []string{"git push", "release", "publish", "server", "remote", "원격", "서버"})
	return risks
}

func containsAny(value string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

var quickEvidenceTriggers = []string{
	"quick", "brief", "simple", "just explain", "explain only", "review only", "check only",
	"no edits", "do not edit", "간단히", "빠르게", "설명만", "검토만", "방향", "확인만",
}

var normalEvidenceTriggers = []string{
	"add", "fix", "change", "update", "wire", "connect", "bug", "추가", "수정", "고쳐", "연결",
}

var deepEvidenceTriggers = []string{
	"deep", "thorough", "exhaustive", "end-to-end", "production-ready", "migration", "database",
	"auth", "security", "refactor", "large", "complex", "implement the plan", "complete", "finish",
	"전체", "완료", "구현", "배포", "데이터베이스", "마이그레이션", "보안", "리팩토링", "끝까지",
}

var verificationCommandSignals = []string{
	"go test", "cargo test", "cargo check", "pytest", "python -m pytest", "npm test", "npm run test",
	"pnpm test", "pnpm run test", "yarn test", "bun test", "mvn test", "gradle test", "rspec",
	"vitest", "jest", "playwright", "cypress", "eslint", "lint", "ruff", "mypy", "pyright",
	"tsc", "typecheck", "type-check", "npm run build", "pnpm build", "yarn build", "go vet",
	"validate", "verify", "json.tool", "py_compile",
}

var failedEvidenceSignals = []string{
	"failed", "failure", "error:", "panic:", "exit status", "tests failed", "build failed",
	"lint failed", "compilation failed", "compile error", "fatal:",
}
