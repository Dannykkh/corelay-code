package agent

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestRunLoopBlocksUnresolvedOverflowWithoutProviderCall(t *testing.T) {
	t.Setenv("CORELAY_CONFIG_DIR", t.TempDir())
	t.Setenv("CORELAY_MEMORY", "off")
	t.Setenv("CORELAY_AUTOSKILL", "off")
	t.Setenv("CORELAY_AUTOVERIFY", "off")
	t.Setenv("CORELAY_MAX_TOOLS", "30")

	profile := phase2Profile("blocked-overflow", 16_384, 2_048)
	estimator := &phase2TokenEstimator{estimate: func(ContextEstimateRequest) TokenEstimate {
		return TokenEstimate{
			InputTokens: 1_000_000,
			Source:      "forced-overflow",
			Confidence:  "exact",
		}
	}}
	provider := &phase2NoCallProvider{}
	recorder := &phase2ContextRecorder{}
	workDir := t.TempDir()
	events := make(chan Event, 512)
	done := make(chan struct{})
	go func() {
		RunLoopWithOptions(
			context.Background(),
			provider,
			"model",
			[]types.Message{{Role: "user", Content: mustJSON("implement a small change")}},
			workDir,
			RunOptions{
				HarnessProfile: &profile,
				TokenEstimator: estimator,
				Recorder:       recorder,
			},
			events,
		)
		close(done)
	}()

	foundBlocked := false
	for event := range events {
		if event.Type != "context_blocked" {
			continue
		}
		blocked, ok := event.Data.(ContextBlockedEvent)
		if !ok {
			t.Fatalf("context_blocked payload type = %T", event.Data)
		}
		if blocked.Code != "context_overflow_after_compaction" || !blocked.Plan.Blocked {
			t.Fatalf("context_blocked payload = %#v", blocked)
		}
		foundBlocked = true
	}
	<-done
	if !foundBlocked {
		t.Fatal("context_blocked event was not emitted")
	}
	if got := provider.Calls(); got != 0 {
		t.Fatalf("provider StreamMessage calls = %d, want 0", got)
	}
	if recorder.failureCount() != 1 {
		t.Fatalf("RunFailed calls = %d, want 1", recorder.failureCount())
	}
	if len(recorder.snapshotsCopy()) != 1 {
		t.Fatalf("compaction snapshots = %d, want 1", len(recorder.snapshotsCopy()))
	}
}

func TestRunLoopCompactionCallerPreservesUserAndWorkstreamState(t *testing.T) {
	isolateEvidenceLoopTest(t)
	t.Setenv("CORELAY_MAX_TOOLS", "30")
	workDir := t.TempDir()
	anchor, err := NewPlanAnchor(PlanAnchorSpec{
		Objective:        "Canonical plan objective",
		CurrentStep:      "verify the current stage",
		RemainingSteps:   []string{"record evidence"},
		DefinitionOfDone: []string{"preserve the authorization contract"},
		Revision:         7,
	})
	if err != nil {
		t.Fatal(err)
	}
	const trigger = "COMPACTION-TRIGGER-EVIDENCE"
	estimator := &phase2TokenEstimator{estimate: func(input ContextEstimateRequest) TokenEstimate {
		tooLarge := false
		for _, message := range input.Request.Messages {
			if strings.Contains(string(message.Content), trigger) && len(input.Request.Messages) >= 17 {
				tooLarge = true
				break
			}
		}
		tokens := 100
		if tooLarge {
			tokens = 1_000_000
		}
		return TokenEstimate{InputTokens: tokens, Source: "fixture", Confidence: "exact"}
	}}
	provider := &completionLoopProvider{steps: []completionLoopStep{
		func(request *types.MessagesRequest) (scriptedLoopStep, error) {
			if !requestHasTool(request, "Bash") {
				return scriptedLoopStep{}, fmt.Errorf("initial request lacks Bash: %v", toolDefNames(request.Tools))
			}
			return toolUseStep("verify-evidence", "Bash", map[string]string{"command": "echo verify ok"}), nil
		},
		func(*types.MessagesRequest) (scriptedLoopStep, error) {
			return toolUseStep("nav-a", "LS", map[string]string{"path": "."}), nil
		},
		func(*types.MessagesRequest) (scriptedLoopStep, error) {
			return toolUseStep("nav-b", "LS", map[string]string{"path": "."}), nil
		},
		func(*types.MessagesRequest) (scriptedLoopStep, error) {
			return toolUseStep("nav-c", "LS", map[string]string{"path": "."}), nil
		},
		func(*types.MessagesRequest) (scriptedLoopStep, error) {
			return toolUseStep("nav-d", "LS", map[string]string{"path": "."}), nil
		},
		func(request *types.MessagesRequest) (scriptedLoopStep, error) {
			if len(request.Messages) != 1 || !strings.Contains(string(request.Messages[0].Content), "Write a concise narrative") {
				return scriptedLoopStep{}, fmt.Errorf("expected structured-compaction narrative call")
			}
			return textStep("The structured state preserves current user directions and verification status."), nil
		},
		func(*types.MessagesRequest) (scriptedLoopStep, error) {
			return textStep("The current verifier is in the selected workflow stage."), nil
		},
	}}
	recorder := &phase2ContextRecorder{}
	var contextPlans []ContextPlan
	runner := &fakeBashRunner{name: "compaction-fixture", capabilities: fakeBashCapabilities()}
	runner.capabilities.FilesystemIsolation = true
	runner.run = func(_ context.Context, policy sandbox.Policy, _ sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
		report := fakeBashReport(runner, policy)
		report.Started = true
		report.EffectiveEnforcement = policy.Enforcement
		return sandbox.Result{
			Started:  true,
			ExitCode: 0,
			Stdout:   []byte("PASS " + trigger),
		}, report
	}
	profile := phase2Profile("compaction-caller-state", 65_536, 4_096)
	longCorrection := "Correction: focus on the current token verifier, not the legacy handler. " +
		strings.Repeat("중간 지시 압축 대상🙂 ", 100) +
		"중요한 마지막 정정: authorization boundary를 그대로 유지하고, 최종 provider 요청과 예산 검사가 동일해야 한다."
	messages := []types.Message{
		{Role: "user", Content: mustJSON("Inspect the legacy token handler.")},
		{Role: "assistant", Content: mustJSON("I will inspect the old handler.")},
		{Role: "user", Content: mustJSON("Keep the API authorization boundary unchanged.")},
		{Role: "assistant", Content: mustJSON("Understood.")},
		{Role: "user", Content: mustJSON(longCorrection)},
		{Role: "assistant", Content: mustJSON("I will focus on the current verifier.")},
		{Role: "user", Content: mustJSON("Where is the current token verifier handled?")},
	}
	events := make(chan Event, 256)
	go RunLoopWithOptions(context.Background(), provider, "fixture-model", messages, workDir, RunOptions{
		HarnessProfile: &profile,
		Recorder:       recorder,
		TokenEstimator: estimator,
		PlanAnchor:     &anchor,
		CompactionContext: CompactionContext{
			WorkstreamID:            "ws-auth",
			WorkstreamObjective:     "Retain safe authorization behavior.",
			Constraints:             []string{"Preserve the API boundary."},
			Decisions:               []string{"Keep the stored Plan canonical."},
			LastVerificationStatus:  "passed",
			LastVerificationSource:  "workstream",
			LastVerificationSummary: "The previous verification passed.",
			PlanID:                  "plan-auth",
			PlanRevision:            7,
			PlanStateRevision:       19,
			StageID:                 "verify",
			PlanEvidence: []CompactionPlanEvidence{{
				StageID: "research", ReceiptDigest: "sha256:" + strings.Repeat("a", 64),
				CriteriaDigest: "sha256:" + strings.Repeat("b", 64), VerificationStatus: "passed", CompletionStatus: "completed",
			}},
		},
		SandboxRunner:       runner,
		SandboxPolicy:       fakeBashPolicy(sandbox.EnforcementRequired),
		DisablePlugins:      true,
		DisableWorkspaceMCP: true,
		EvidencePolicy:      EvidencePolicyConfig{Policy: EvidencePolicyMeasure},
	}, events)
	for event := range events {
		if event.Type == "context_plan" {
			plan, ok := event.Data.(ContextPlan)
			if !ok {
				t.Fatalf("context_plan payload type = %T", event.Data)
			}
			contextPlans = append(contextPlans, plan)
		}
	}

	requests, errs := provider.snapshot()
	if len(errs) != 0 {
		t.Fatalf("fake provider callbacks failed: %v", errs)
	}
	if len(requests) != 7 {
		t.Fatalf("provider requests = %d, want 5 exploration + compaction narrative + answer", len(requests))
	}
	if len(contextPlans) == 0 {
		t.Fatal("RunLoop emitted no completed context plan")
	}
	finalPlan := contextPlans[len(contextPlans)-1]
	if !finalPlan.Fits || finalPlan.NeedsCompaction || finalPlan.RequiredTokens > finalPlan.ContextWindowTokens ||
		finalPlan.CompactionSnapshotDigest == "" {
		t.Fatalf("post-compaction final request did not pass budget recalculation: %+v", finalPlan)
	}
	if len(estimator.requests) == 0 {
		t.Fatal("token estimator received no completed request")
	}
	estimatedRequest := estimator.requests[len(estimator.requests)-1].Request
	if !reflect.DeepEqual(estimatedRequest, *requests[6]) {
		t.Fatalf("token estimator did not budget the exact final provider request (estimate_count=%d model=%q/%q max_tokens=%d/%d system_equal=%v messages_equal=%v tools_equal=%v tool_choice_equal=%v thinking_equal=%v metadata_equal=%v)",
			len(estimator.requests), estimatedRequest.Model, requests[6].Model, estimatedRequest.MaxTokens, requests[6].MaxTokens,
			reflect.DeepEqual(estimatedRequest.System, requests[6].System), reflect.DeepEqual(estimatedRequest.Messages, requests[6].Messages),
			reflect.DeepEqual(estimatedRequest.Tools, requests[6].Tools), reflect.DeepEqual(estimatedRequest.ToolChoice, requests[6].ToolChoice),
			reflect.DeepEqual(estimatedRequest.Thinking, requests[6].Thinking), reflect.DeepEqual(estimatedRequest.Metadata, requests[6].Metadata))
	}
	snapshots := recorder.snapshotsCopy()
	if len(snapshots) != 1 {
		t.Fatalf("compaction snapshots = %d, want 1", len(snapshots))
	}
	snapshot := snapshots[0]
	if snapshot.Objective != "Where is the current token verifier handled?" ||
		snapshot.Plan.Objective != anchor.Objective() || snapshot.Plan.ID != "plan-auth" ||
		snapshot.Plan.StateRevision != 19 || snapshot.Plan.StageID != "verify" {
		t.Fatalf("caller did not pass the current objective and canonical Plan reference: %+v", snapshot)
	}
	if !reflect.DeepEqual(snapshot.UserInstructions, sanitizeRecentSnapshotList(userInstructionTexts(messages), maxCompactionUserInstructions, 600)) {
		t.Fatalf("actual compaction changed bounded instructions: input=%q snapshot=%q", userInstructionTexts(messages), snapshot.UserInstructions)
	}
	if finalPlan.CompactionSnapshotDigest != snapshot.Digest {
		t.Fatalf("fitted provider request plan snapshot digest=%q, actual snapshot digest=%q", finalPlan.CompactionSnapshotDigest, snapshot.Digest)
	}
	if !strings.Contains(strings.Join(snapshot.UserInstructions, "\n"), "Keep the API authorization boundary unchanged") ||
		!strings.Contains(strings.Join(snapshot.UserInstructions, "\n"), "Correction: focus on the current token verifier") ||
		!strings.Contains(strings.Join(snapshot.UserInstructions, "\n"), "authorization boundary를 그대로 유지하고, 최종 provider 요청과 예산 검사가 동일해야 한다.") {
		t.Fatalf("caller lost prior constraint/correction: %q", snapshot.UserInstructions)
	}
	if snapshot.WorkstreamID != "ws-auth" || len(snapshot.Constraints) != 1 || len(snapshot.Decisions) != 1 ||
		snapshot.LastVerification == nil || snapshot.LastVerification.Status != "passed" || len(snapshot.PlanEvidence) != 1 {
		t.Fatalf("caller omitted typed durable Workstream state: %+v", snapshot)
	}
	if len(snapshot.Evidence) != 1 || snapshot.Evidence[0].Source != "tool" || snapshot.Evidence[0].Status != "passed" {
		t.Fatalf("caller omitted run-local verification evidence: %+v", snapshot.Evidence)
	}
	finalMessages := string(mustJSON(requests[6].Messages))
	for _, required := range []string{
		"Keep the API authorization boundary unchanged",
		"Correction: focus on the current token verifier",
		"authorization boundary를 그대로 유지하고, 최종 provider 요청과 예산 검사가 동일해야 한다.",
		"Where is the current token verifier handled?",
		"plan-auth",
		"Preserve the API boundary.",
		"Keep the stored Plan canonical.",
	} {
		if !strings.Contains(finalMessages, required) {
			t.Fatalf("actual post-compaction provider payload omitted %q:\n%s", required, finalMessages)
		}
	}
}

type phase2NoCallProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *phase2NoCallProvider) Name() string              { return "phase2-no-call" }
func (p *phase2NoCallProvider) DisplayName() string       { return "phase2-no-call" }
func (p *phase2NoCallProvider) Models() []types.ModelInfo { return nil }
func (p *phase2NoCallProvider) Validate() error           { return nil }
func (p *phase2NoCallProvider) StreamMessage(context.Context, *types.MessagesRequest, *types.StreamOptions) (<-chan types.SSEEvent, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	stream := make(chan types.SSEEvent)
	close(stream)
	return stream, nil
}
func (p *phase2NoCallProvider) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

type phase2ContextRecorder struct {
	mu        sync.Mutex
	failures  []string
	plans     []ContextPlan
	snapshots []CompactionSnapshot
}

func (*phase2ContextRecorder) RunStarted()                         {}
func (*phase2ContextRecorder) ReceiptWritten(string, AgentReceipt) {}
func (*phase2ContextRecorder) RunCompleted(RunSummary)             {}
func (r *phase2ContextRecorder) RunFailed(message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures = append(r.failures, message)
}
func (r *phase2ContextRecorder) ContextPlanned(plan ContextPlan) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.plans = append(r.plans, plan)
}
func (r *phase2ContextRecorder) CompactionRecorded(s CompactionSnapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.snapshots = append(r.snapshots, s)
}
func (r *phase2ContextRecorder) failureCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.failures)
}
func (r *phase2ContextRecorder) snapshotsCopy() []CompactionSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]CompactionSnapshot(nil), r.snapshots...)
}

var _ RunRecorder = (*phase2ContextRecorder)(nil)
var _ RunFailureRecorder = (*phase2ContextRecorder)(nil)
var _ RunContextRecorder = (*phase2ContextRecorder)(nil)
