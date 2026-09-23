package agent

import (
	"errors"
	"fmt"

	"github.com/Dannykkh/corelay-code/internal/approval"
	"github.com/Dannykkh/corelay-code/internal/capabilityprofile"
	"github.com/Dannykkh/corelay-code/internal/harness"
	"github.com/Dannykkh/corelay-code/internal/hooks"
	"github.com/Dannykkh/corelay-code/internal/processsupervisor"
	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

const (
	defaultToolResultReadBytes = 8 << 10
	maxToolResultReadBytes     = 32 << 10
)

// ErrToolResultPersistence is returned by checked durable result stores when
// a large result cannot be published. The error is deliberately content-free:
// callers must never surface filesystem details or the unstored result.
var ErrToolResultPersistence = errors.New("tool result persistence failed")

// ToolResultStoreDisposition distinguishes a genuinely inline result from a
// successfully persisted replacement. The historical ToolResultStore bool
// cannot distinguish an inline result from a failed large-result write.
type ToolResultStoreDisposition uint8

const (
	ToolResultInline ToolResultStoreDisposition = iota
	ToolResultPersisted
)

// CheckedToolResultStore is the fail-closed write side used by production
// durable stores. Implementations must return ErrToolResultPersistence for a
// large result that could not be published and must not include raw content in
// the error.
type CheckedToolResultStore interface {
	StoreResultChecked(toolName string, result string) (string, ToolResultStoreDisposition, error)
}

// ToolResultChunk is one bounded, redacted view of a durable tool result.
// Offsets and sizes count bytes in the fully redacted view, so pagination can
// never begin inside an unredacted secret. Content never contains a storage
// path or other filesystem identity.
type ToolResultChunk struct {
	ID         string `json:"id"`
	Content    string `json:"content"`
	Offset     int64  `json:"offset"`
	NextOffset int64  `json:"nextOffset"`
	TotalBytes int64  `json:"totalBytes"`
	EOF        bool   `json:"eof"`
}

// ToolResultReader is the optional read side of ToolResultStore. Reads are
// deliberately paged so a model cannot pull a complete multi-megabyte result
// back into one provider request.
type ToolResultReader interface {
	ReadResult(id string, offset int64, limit int) (ToolResultChunk, error)
}

// ToolResultReference is content-free metadata for one result that is both
// referenced by the committed transcript and present in the exact session
// store. Digest is always a lowercase sha256 value.
type ToolResultReference struct {
	ID     string `json:"id"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// ToolExecutionJournalEntry is the complete content-free identity persisted
// immediately before an authorized tool executor is invoked. Raw input,
// bound proof material, output, and filesystem identity must never be added
// to this boundary.
type ToolExecutionJournalEntry struct {
	ID          string
	Name        string
	InputDigest string
	RunID       string
}

// ToolExecutionJournal synchronously records an authorized execution intent.
// Returning an error prevents both the execution-start event and the executor
// call, so a persistence failure cannot open an unjournaled side-effect gap.
type ToolExecutionJournal func(ToolExecutionJournalEntry) error

type RunOptions struct {
	// ShadowJudgment observes repeated failures without changing execution.
	// Nil keeps the current path; only trusted harnesses may inject a judge.
	ShadowJudgment *ShadowJudgmentConfig
	// ExecutionPolicyRequest is the root run's user-selected mode. RunLoop
	// resolves it once against the compatibility default and runtime facts.
	// Child loops must receive a derived ExecutionPolicy snapshot instead.
	ExecutionPolicyRequest *ExecutionPolicyRequest
	// ExecutionPolicy is the exact authority snapshot inherited by a child run.
	// Nil on a root run resolves once to the workspace compatibility default.
	ExecutionPolicy *ExecutionPolicySnapshot
	// SessionID identifies the active run whose approvals must be bound to the
	// same client session. It is distinct from durable transcript IDs.
	SessionID string
	// DurableSessionID scopes persisted run artifacts such as checkpoints to
	// the transcript session. It is separate from SessionID, which owns live
	// approvals and may be ephemeral.
	DurableSessionID string
	// CheckpointScope lets cooperating workers contribute to one turn's undo
	// generation without clearing or racing each other's manifest.
	CheckpointScope *CheckpointScope
	// SessionRevision is the exact durable transcript revision being executed.
	// Ephemeral runs use zero; full-mode grants still bind their run identity.
	SessionRevision uint64
	// ApprovalRequester is optional until the common safety pipeline requests
	// an explicit user decision. A nil requester must never imply approval.
	ApprovalRequester approval.Requester
	ResponseLang      string
	// SkillSource and SkillDirs are the resolved per-run skill catalog policy.
	// An empty source retains the legacy "all" behavior for direct callers.
	SkillSource       string
	SkillDirs         []string
	ProjectSkillDirs  []string
	WorkstreamContext string
	CompactionContext CompactionContext
	Recorder          RunRecorder
	WorkerID          string
	OwnershipChecker  func(workerID, filePath string) (bool, string)
	EvidencePolicy    EvidencePolicyConfig
	// CompletionEvidenceNotRequired contains exact Definition of Done text for
	// criteria whose completion can be asserted without a run evidence digest.
	// Consumers must use CompletionEvidenceNotRequiredCriteria so the mutable
	// caller-owned slice never becomes contract state by alias.
	CompletionEvidenceNotRequired []string
	// RunMode optionally controls run-local phase transitions while the existing
	// RunLoop remains the sole owner of provider calls, tools, context, hooks,
	// evidence, receipts, and terminal events.
	RunMode RunMode
	// IterationLimit is a kernel-owned compatibility override for bounded modes
	// such as Chronos and SubAgent. Zero uses HarnessProfile.MaxIterations.
	IterationLimit int
	// HarnessProfile is an optional immutable run-level policy override. It is
	// the highest-precedence input and bypasses all lower-resolution sources.
	HarnessProfile *harness.HarnessProfile
	// CapabilityProfile is an optional exact-target, automatically eligible
	// empirical selection. It is ignored when HarnessProfile is explicit and is
	// rechecked for expiry and exact provider/model identity at run start.
	CapabilityProfile *capabilityprofile.AutomaticSelection
	// EvaluatedLessons is a bounded profiler injection, never model-supplied text.
	EvaluatedLessons capabilityprofile.LessonPolicy
	// PlanAnchor is optional durable plan state. Rendering remains disabled
	// unless the resolved HarnessProfile selects a non-off mode.
	PlanAnchor *PlanAnchor
	// PlanBinding pins a canonical Workstream Plan stage to this execution.
	// Plan-bound runs force strict completion evidence regardless of the
	// provider's ordinary plan-anchor profile.
	PlanBinding *PlanExecutionBinding
	// TokenEstimator can provide an exact provider tokenizer. When nil, the
	// request planner uses its deterministic protocol-aware conservative model.
	TokenEstimator TokenEstimator
	// ContextReducer is an optional semantic reducer for recalled memory and
	// first-turn RAG. Its output is accepted only when it actually shrinks.
	ContextReducer OptionalContextReducer
	// ToolResultStore is the durable reference seam for successful large tool
	// results and the historical context-planner fallback. SessionMemory
	// satisfies this interface.
	ToolResultStore ToolResultStore
	// ToolResultReader enables the bounded LoadToolResult catalog entry. A nil
	// reader means that tool must not be advertised.
	ToolResultReader ToolResultReader
	// ToolResultReferences contains only committed-transcript references that
	// were verified in this reader's exact session/workspace namespace.
	ToolResultReferences []ToolResultReference
	// PreExecutionJournal is the optional synchronous durability boundary for
	// authorized tool execution. It runs after all identity/proof binding and
	// immediately before the lifecycle event and executor call.
	PreExecutionJournal ToolExecutionJournal
	// HookRegistry is an optional run-owned project-hook composition seam. When
	// nil the loop creates the secure default registry. A supplied registry is
	// still loaded from this run's canonical workspace before use.
	HookRegistry *hooks.Registry
	// SandboxRunner and SandboxPolicy are resolved once per run. Supplying only
	// one is invalid; supplying neither selects DefaultSandboxExecution.
	SandboxRunner sandbox.Runner
	SandboxPolicy sandbox.Policy
	// MCPExecution optionally overrides the long-lived subprocess runner,
	// policy, and framing limits. Its Context is always rebound to this run so
	// an injected background context cannot outlive cancellation.
	MCPExecution *MCPExecutionOptions
	// MCPServers is an optional exact run-owned stdio server set. A non-nil
	// slice, including an empty one, is authoritative and is never merged with
	// workspace or process-global MCP state. Raw specs remain in memory only.
	MCPServers []MCPServerSpec
	// MCPRuntimeFactory is an optional composition/test seam. Nil selects
	// NewMCPRuntime. When MCPRuntime is nil, the returned runtime is connected
	// before the first provider request and closed exactly once when this run
	// exits.
	MCPRuntimeFactory MCPRuntimeFactory
	// MCPRuntime is an optional already-connected runtime borrowed from a
	// protocol session. Borrowed runtimes are never closed by RunLoop; their
	// owner is responsible for cancellation and exactly-once shutdown.
	MCPRuntime MCPRuntime
	// DisableWorkspaceMCP prevents this run from loading or starting commands
	// from workspace .mcp.json files. Protocol adapters that receive their own
	// client-granted MCP definitions must set this; silently mixing workspace
	// and client authority is unsafe.
	DisableWorkspaceMCP bool
	// PluginDirs is an explicit executable-plugin discovery seam. Nil selects
	// DefaultPluginDirs(workDir); a non-nil empty slice disables discovery.
	// Every executable must still remain inside its plugin directory.
	PluginDirs []string
	// PluginExecution optionally supplies a plugin-specific secure runner and
	// policy. Nil reuses the run runner but derives a stricter Required policy
	// with filesystem/network isolation and deny-by-default environment.
	PluginExecution *PluginExecutionOptions
	// DisablePlugins prevents both default and explicit plugin discovery.
	DisablePlugins bool
}

// PlanExecutionBinding is the content-free identity shared by Plan-bound
// execution and its durable receipt.
type PlanExecutionBinding struct {
	WorkstreamID      string `json:"workstreamId"`
	PlanID            string `json:"planId"`
	PlanRevision      uint64 `json:"planRevision"`
	PlanStateRevision uint64 `json:"planStateRevision,omitempty"`
	StageID           string `json:"stageId"`
	RunID             string `json:"runId"`
}

// CompactionContext carries typed durable workflow state into the common
// structured-compaction path. It is separate from rendered prompt text and
// from PlanExecutionBinding so Team workers can retain workflow context
// without claiming ownership of the parent Plan stage execution.
type CompactionContext struct {
	WorkstreamID            string
	WorkstreamObjective     string
	Constraints             []string
	Decisions               []string
	LastVerificationStatus  string
	LastVerificationSource  string
	LastVerificationSummary string
	PlanID                  string
	PlanRevision            uint64
	PlanStateRevision       uint64
	StageID                 string
	PlanEvidence            []CompactionPlanEvidence
}

// CompactionPlanEvidence contains only durable digest/status references.
type CompactionPlanEvidence struct {
	StageID             string `json:"stageId"`
	StageStatus         string `json:"stageStatus,omitempty"`
	AttemptStatus       string `json:"attemptStatus,omitempty"`
	AttemptPlanRevision uint64 `json:"attemptPlanRevision,omitempty"`
	ReceiptDigest       string `json:"receiptDigest,omitempty"`
	CriteriaDigest      string `json:"criteriaDigest,omitempty"`
	VerificationStatus  string `json:"verificationStatus,omitempty"`
	CompletionStatus    string `json:"completionStatus,omitempty"`
}

func cloneCompactionContext(value CompactionContext) CompactionContext {
	value.Constraints = append([]string(nil), value.Constraints...)
	value.Decisions = append([]string(nil), value.Decisions...)
	value.PlanEvidence = append([]CompactionPlanEvidence(nil), value.PlanEvidence...)
	return value
}

func compactionContextForRun(value CompactionContext, binding *PlanExecutionBinding) CompactionContext {
	value = cloneCompactionContext(value)
	if binding != nil {
		value.WorkstreamID = binding.WorkstreamID
		value.PlanID = binding.PlanID
		value.PlanRevision = binding.PlanRevision
		value.PlanStateRevision = binding.PlanStateRevision
		value.StageID = binding.StageID
	}
	return value
}

// CompletionEvidenceNotRequiredCriteria returns a defensive copy suitable for
// CompletionContractSpec. A nil input remains nil for default semantic parity.
func (o RunOptions) CompletionEvidenceNotRequiredCriteria() []string {
	if o.CompletionEvidenceNotRequired == nil {
		return nil
	}
	return append([]string(nil), o.CompletionEvidenceNotRequired...)
}

type RunRecorder interface {
	RunStarted()
	ReceiptWritten(path string, receipt AgentReceipt)
	RunCompleted(summary RunSummary)
}

type RunSpanRecorder interface {
	RunSpanStarted(id string, name string, data map[string]string)
	RunSpanCompleted(id string, status string, data map[string]string)
}

type RunFailureRecorder interface {
	RunFailed(message string)
}

// RunSandboxRecorder is an optional typed evidence seam. Reports contain no
// command arguments, environment values, or output.
type RunSandboxRecorder interface {
	SandboxReported(record SandboxExecutionRecord)
}

// RunContextRecorder receives redacted typed budget/compaction records. The
// records contain counts, bounded state, and digests but no raw prompt, tool
// input, command, credential, or tool-result payload.
type RunContextRecorder interface {
	ContextPlanned(plan ContextPlan)
	CompactionRecorded(snapshot CompactionSnapshot)
}

// RunHookRecorder receives bounded, redacted hook evidence as each hook
// finishes. Raw commands, environment values, process errors, and tool-result
// payloads are excluded by the hooks package before this seam is called.
type RunHookRecorder interface {
	HookRecorded(result hooks.HookResult)
}

// RunMCPRecorder receives the redacted capability report emitted when each
// long-lived MCP process is started. Reports exclude argv, environment values,
// stdout, and stderr.
type RunMCPRecorder interface {
	MCPReported(report processsupervisor.Report)
}

func startRunSpan(recorder RunRecorder, id string, name string, data map[string]string) func(status string, data map[string]string) {
	spanRecorder, ok := recorder.(RunSpanRecorder)
	if !ok || spanRecorder == nil {
		return func(string, map[string]string) {}
	}
	spanRecorder.RunSpanStarted(id, name, data)
	return func(status string, data map[string]string) {
		spanRecorder.RunSpanCompleted(id, status, data)
	}
}

func failRun(recorder RunRecorder, message string) {
	failureRecorder, ok := recorder.(RunFailureRecorder)
	if ok && failureRecorder != nil {
		failureRecorder.RunFailed(message)
	}
}

func recordSandboxExecution(recorder RunRecorder, record SandboxExecutionRecord) {
	if recorder == nil {
		return
	}
	if typed, ok := recorder.(RunSandboxRecorder); ok && typed != nil {
		typed.SandboxReported(record)
	}
	finish := startRunSpan(recorder, "sandbox_"+record.ToolID, "tool.sandbox", map[string]string{
		"tool":      record.ToolName,
		"runner":    record.Report.Runner,
		"requested": string(record.Report.RequestedEnforcement),
		"effective": string(record.Report.EffectiveEnforcement),
		"started":   fmt.Sprintf("%t", record.Report.Started),
	})
	status := "ok"
	if record.Report.Failure != sandbox.FailureNone {
		status = "error"
	}
	finish(status, map[string]string{"failure": string(record.Report.Failure)})
}

func recordContextPlan(recorder RunRecorder, plan ContextPlan) {
	if recorder == nil {
		return
	}
	if typed, ok := recorder.(RunContextRecorder); ok && typed != nil {
		typed.ContextPlanned(plan)
	}
}

func recordCompaction(recorder RunRecorder, snapshot CompactionSnapshot) {
	if recorder == nil {
		return
	}
	if typed, ok := recorder.(RunContextRecorder); ok && typed != nil {
		typed.CompactionRecorded(snapshot)
	}
}

func recordHookResults(recorder RunRecorder, results []hooks.HookResult) {
	if recorder == nil || len(results) == 0 {
		return
	}
	typed, ok := recorder.(RunHookRecorder)
	if !ok || typed == nil {
		return
	}
	for _, result := range results {
		typed.HookRecorded(result)
	}
}

func recordMCPExecution(recorder RunRecorder, report processsupervisor.Report) {
	if recorder == nil {
		return
	}
	if typed, ok := recorder.(RunMCPRecorder); ok && typed != nil {
		typed.MCPReported(report)
	}
}

type RunSummary struct {
	Provider      string
	Model         string
	ProjectType   string
	PlanMode      bool
	Iterations    int
	EditedFiles   []string
	Verification  ReceiptVerification
	ReceiptPath   string
	Sandbox       []SandboxExecutionRecord
	Hooks         []hooks.HookResult
	MCP           []processsupervisor.Report
	MCPGeneration string
	Completion    *CompletionContractSnapshot
	RunMode       *RunModeSnapshot
	Recovery      *RunGuardSnapshot
}

type SandboxExecutionRecord struct {
	ToolID   string         `json:"toolId"`
	ToolName string         `json:"toolName"`
	Report   sandbox.Report `json:"report"`
}
