package kairos

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/observability"
	"github.com/Dannykkh/corelay-code/internal/types"
	"github.com/Dannykkh/corelay-code/internal/workstream"
)

func TestDaemonExecuteTaskRecordsWorkstream(t *testing.T) {
	workDir := t.TempDir()
	store := workstream.NewStore(workDir)
	if _, err := store.Create(workstream.CreateRequest{
		ID:         "ws_kairos",
		Title:      "KAIROS Workstream",
		Summary:    "kairos background summary",
		NextAction: "run daemon task",
		Goal: workstream.Goal{
			Objective:          "prove kairos workstream recording",
			AcceptanceCriteria: []string{"prompt context", "timeline event"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	provider := &daemonFakeProvider{text: "daemon task complete"}
	tracker := observability.NewTracker(t.TempDir())
	daemon := NewDaemon(DefaultDaemonConfig())
	daemon.SwitchProject(workDir)
	daemon.SetProvider(provider, "fake-model")
	daemon.SetTracker(tracker)

	daemon.executeTask(context.Background(), Task{
		ID:           "task-1",
		Type:         "custom",
		Description:  "run a workstream task",
		WorkstreamID: "ws_kairos",
	}, "autonomous")

	if provider.calls != 1 {
		t.Fatalf("provider calls=%d, want 1", provider.calls)
	}
	if !strings.Contains(provider.prompt, "## Workstream Context") ||
		!strings.Contains(provider.prompt, "KAIROS Workstream") ||
		!strings.Contains(provider.prompt, "prove kairos workstream recording") {
		t.Fatalf("provider did not receive workstream context:\n%s", provider.prompt)
	}

	updated, err := store.Get("ws_kairos")
	if err != nil {
		t.Fatal(err)
	}
	if updated.LastVerification.Status != "not-run" || updated.LastVerification.Source != "kairos" {
		t.Fatalf("unexpected verification: %+v", updated.LastVerification)
	}

	timeline, err := store.Timeline("ws_kairos")
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, event := range timeline {
		have[event.Type] = true
	}
	for _, want := range []string{"kairos_task_started", "verification_updated", "kairos_task_completed"} {
		if !have[want] {
			t.Fatalf("missing timeline event %q in %+v", want, timeline)
		}
	}

	runTraces := tracker.RecentRuns(10)
	if len(runTraces) != 1 {
		t.Fatalf("run traces=%d, want 1", len(runTraces))
	}
	if runTraces[0].Kind != "kairos" || runTraces[0].WorkstreamID != "ws_kairos" || runTraces[0].Status != "ok" {
		t.Fatalf("unexpected run trace: %+v", runTraces[0])
	}
	if !hasRunSpan(runTraces[0], "kairos.task") {
		t.Fatalf("kairos run trace missing task span: %+v", runTraces[0].Spans)
	}
}

func TestDaemonExecutePlanBoundTaskObservesCanonicalStageWithoutCompletingPlan(t *testing.T) {
	workDir := t.TempDir()
	store, approved := createKAIROSPlan(t, workDir, true)
	provider := &daemonFakeProvider{text: "The stage is complete and verification passed."}
	daemon := NewDaemon(DefaultDaemonConfig())
	daemon.SwitchProject(workDir)
	daemon.SetProvider(provider, "fake-model")
	task := Task{
		ID: "task-plan-observer", Type: "custom", Description: "review the approved stage",
		WorkstreamID: "ws_kairos_plan", PlanID: approved.ID,
		PlanRevision: approved.Revision, StageID: "research",
	}

	daemon.executeTask(context.Background(), task, "autonomous")

	if provider.calls != 1 {
		t.Fatalf("provider calls=%d, want 1", provider.calls)
	}
	for _, want := range []string{
		"## Approved Plan Stage (read-only observer context)",
		"Do not execute the stage",
		"canonical research objective",
		"canonical task acceptance criterion",
		"plan_kairos_observer",
	} {
		if !strings.Contains(provider.prompt, want) {
			t.Fatalf("Plan-bound prompt missing %q:\n%s", want, provider.prompt)
		}
	}

	after, err := store.GetPlan("ws_kairos_plan", approved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != approved.Revision || after.StateRevision != approved.StateRevision ||
		after.Status != workstream.PlanStatusApproved || after.Stages[0].Status != workstream.PlanStageStatusPending ||
		len(after.Stages[0].Attempts) != 0 {
		t.Fatalf("observer mutated Plan stage or evidence: before=%+v after=%+v", approved, after)
	}

	updatedWorkstream, err := store.Get("ws_kairos_plan")
	if err != nil {
		t.Fatal(err)
	}
	if updatedWorkstream.LastVerification.Status != "not-run" || updatedWorkstream.LastVerification.Source != "kairos" {
		t.Fatalf("KAIROS must leave verification unverified: %+v", updatedWorkstream.LastVerification)
	}

	timeline, err := store.Timeline("ws_kairos_plan")
	if err != nil {
		t.Fatal(err)
	}
	var started, completed *workstream.TimelineEvent
	for index := range timeline {
		event := &timeline[index]
		if event.Type == "kairos_task_started" {
			started = event
		}
		if event.Type == "kairos_task_completed" {
			completed = event
		}
	}
	for name, event := range map[string]*workstream.TimelineEvent{"started": started, "completed": completed} {
		if event == nil {
			t.Fatalf("missing KAIROS %s event in %+v", name, timeline)
		}
		if event.Data["planId"] != approved.ID || event.Data["planRevision"] != "1" ||
			event.Data["stageId"] != "research" || event.Data["executionMode"] != "observer" ||
			len(event.Data["stageContextDigest"]) != 64 {
			t.Fatalf("KAIROS %s event missing exact Plan binding: %+v", name, event.Data)
		}
	}
}

func TestDaemonPlanBoundObserverCannotInvokeBuiltInExecutionTask(t *testing.T) {
	workDir := t.TempDir()
	store, approved := createKAIROSPlan(t, workDir, true)
	provider := &daemonFakeProvider{text: "must not run"}
	daemon := NewDaemon(DefaultDaemonConfig())
	daemon.SwitchProject(workDir)
	daemon.SetProvider(provider, "fake-model")
	daemon.executeTask(context.Background(), Task{
		ID: "task-plan-git-watch", Type: "git-watch", Description: "observe the plan stage",
		WorkstreamID: "ws_kairos_plan", PlanID: approved.ID,
		PlanRevision: approved.Revision, StageID: "research",
	}, "autonomous")

	if provider.calls != 0 {
		t.Fatalf("provider calls=%d, want no provider for a built-in task", provider.calls)
	}
	logs := daemon.GetLogs(10)
	if len(logs) == 0 || logs[len(logs)-1].Action != "task-error" ||
		!strings.Contains(logs[len(logs)-1].Detail, "cannot invoke built-in execution tasks") {
		t.Fatalf("Plan-bound built-in task was not rejected: %+v", logs)
	}
	after, err := store.GetPlan("ws_kairos_plan", approved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.StateRevision != approved.StateRevision || len(after.Stages[0].Attempts) != 0 {
		t.Fatalf("rejected built-in task mutated Plan state: %+v", after)
	}
}

func TestDaemonExecutePlanBoundTaskRejectsStaleOrUnapprovedPlanBeforeProvider(t *testing.T) {
	tests := []struct {
		name    string
		approve bool
		stale   bool
	}{
		{name: "draft unapproved", approve: false},
		{name: "stale revision", approve: true, stale: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workDir := t.TempDir()
			store, plan := createKAIROSPlan(t, workDir, test.approve)
			boundRevision := plan.Revision
			if test.stale {
				updated, err := store.UpdatePlan("ws_kairos_plan", plan.ID, workstream.UpdatePlanRequest{
					ExpectedRevision: plan.Revision, ExpectedStateRevision: plan.StateRevision,
					Definition: plan.Definition,
				})
				if err != nil {
					t.Fatal(err)
				}
				plan, err = store.ApprovePlan("ws_kairos_plan", plan.ID, workstream.ApprovePlanRequest{
					ExpectedRevision: updated.Revision, ExpectedStateRevision: updated.StateRevision,
				})
				if err != nil {
					t.Fatal(err)
				}
				if boundRevision == plan.Revision {
					t.Fatal("test setup did not create a stale binding")
				}
			}
			provider := &daemonFakeProvider{text: "should not be requested"}
			daemon := NewDaemon(DefaultDaemonConfig())
			daemon.SwitchProject(workDir)
			daemon.SetProvider(provider, "fake-model")
			daemon.executeTask(context.Background(), Task{
				ID: "task-plan-rejected", Type: "custom", Description: "must fail closed",
				WorkstreamID: "ws_kairos_plan", PlanID: plan.ID,
				PlanRevision: boundRevision, StageID: "research",
			}, "autonomous")

			if provider.calls != 0 {
				t.Fatalf("provider calls=%d, want 0 for invalid binding", provider.calls)
			}
			after, err := store.GetPlan("ws_kairos_plan", plan.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Status != plan.Status || after.Revision != plan.Revision ||
				after.Stages[0].Status != workstream.PlanStageStatusPending || len(after.Stages[0].Attempts) != 0 {
				t.Fatalf("rejected observer changed Plan state: before=%+v after=%+v", plan, after)
			}
		})
	}
}

func TestDaemonExecutePlanBoundTaskProviderFailureIsNotVerification(t *testing.T) {
	workDir := t.TempDir()
	store, plan := createKAIROSPlan(t, workDir, true)
	provider := &daemonFakeProvider{err: errors.New("provider unavailable")}
	daemon := NewDaemon(DefaultDaemonConfig())
	daemon.SwitchProject(workDir)
	daemon.SetProvider(provider, "fake-model")
	daemon.executeTask(context.Background(), Task{
		ID: "task-plan-provider-error", Type: "custom", Description: "observe stage",
		WorkstreamID: "ws_kairos_plan", PlanID: plan.ID,
		PlanRevision: plan.Revision, StageID: "research",
	}, "autonomous")

	if provider.calls != 1 {
		t.Fatalf("provider calls=%d, want 1", provider.calls)
	}
	updatedWorkstream, err := store.Get("ws_kairos_plan")
	if err != nil {
		t.Fatal(err)
	}
	if updatedWorkstream.LastVerification.Status != "not-run" {
		t.Fatalf("provider transport failure must not be verification: %+v", updatedWorkstream.LastVerification)
	}
	after, err := store.GetPlan("ws_kairos_plan", plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.StateRevision != plan.StateRevision || after.Stages[0].Status != workstream.PlanStageStatusPending || len(after.Stages[0].Attempts) != 0 {
		t.Fatalf("failed observer mutated Plan: before=%+v after=%+v", plan, after)
	}
}

func createKAIROSPlan(t *testing.T, workDir string, approve bool) (*workstream.Store, *workstream.Plan) {
	t.Helper()
	store := workstream.NewStore(workDir)
	if _, err := store.Create(workstream.CreateRequest{
		ID: "ws_kairos_plan", Title: "KAIROS Plan observer", Summary: "canonical Plan scope",
		Goal: workstream.Goal{Objective: "canonical research objective", AcceptanceCriteria: []string{"canonical task acceptance criterion"}},
	}); err != nil {
		t.Fatal(err)
	}
	definition, err := json.Marshal(map[string]any{
		"version": 1, "name": "KAIROS observer Plan", "objective": "canonical research objective",
		"verifyCommand": "go test ./internal/workstream",
		"stages":        []map[string]any{{"id": "research", "name": "Research", "kind": "research", "taskIds": []string{"inspect"}}},
		"tasks":         []map[string]any{{"id": "inspect", "name": "Inspect source", "stage": "research", "goal": "read source", "acceptanceCriteria": []string{"canonical task acceptance criterion"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := store.CreatePlan("ws_kairos_plan", workstream.CreatePlanRequest{ID: "plan_kairos_observer", Definition: definition})
	if err != nil {
		t.Fatal(err)
	}
	if approve {
		plan, err = store.ApprovePlan("ws_kairos_plan", plan.ID, workstream.ApprovePlanRequest{
			ExpectedRevision: plan.Revision, ExpectedStateRevision: plan.StateRevision,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return store, plan
}

func TestNewDaemonNormalizesZeroDurations(t *testing.T) {
	daemon := NewDaemon(DaemonConfig{})
	cfg := daemon.GetConfig()
	if cfg.TickInterval <= 0 {
		t.Fatalf("TickInterval was not normalized: %v", cfg.TickInterval)
	}
	if cfg.BlockingBudget <= 0 {
		t.Fatalf("BlockingBudget was not normalized: %v", cfg.BlockingBudget)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		daemon.Start()
		daemon.Stop()
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("daemon Start/Stop with zero config did not return")
	}
}

type daemonFakeProvider struct {
	text   string
	calls  int
	prompt string
	err    error
}

func (p *daemonFakeProvider) Name() string              { return "fake" }
func (p *daemonFakeProvider) DisplayName() string       { return "Fake" }
func (p *daemonFakeProvider) Models() []types.ModelInfo { return nil }
func (p *daemonFakeProvider) Validate() error           { return nil }

func (p *daemonFakeProvider) StreamMessage(ctx context.Context, req *types.MessagesRequest, opts *types.StreamOptions) (<-chan types.SSEEvent, error) {
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	if len(req.Messages) > 0 {
		_ = json.Unmarshal(req.Messages[0].Content, &p.prompt)
	}

	ch := make(chan types.SSEEvent, 3)
	go func() {
		defer close(ch)
		delta, _ := json.Marshal(map[string]string{"type": "text_delta", "text": p.text})
		ch <- types.SSEEvent{Type: "content_block_delta", Delta: delta}
		ch <- types.SSEEvent{Type: "message_stop"}
	}()
	return ch, nil
}

func hasRunSpan(trace observability.RunTrace, name string) bool {
	for _, span := range trace.Spans {
		if span.Name == name {
			return true
		}
	}
	return false
}
