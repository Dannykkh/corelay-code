package workstream

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestStoreConcurrentDisjointPatchesAcrossInstances(t *testing.T) {
	workspace := t.TempDir()
	reader := NewStore(workspace)
	ws, err := reader.Create(CreateRequest{ID: "ws_concurrent", Title: "Concurrent updates"})
	if err != nil {
		t.Fatal(err)
	}
	for iteration := 0; iteration < 20; iteration++ {
		start := make(chan struct{})
		results := make(chan error, 2)
		next := fmt.Sprintf("next-%d", iteration)
		decision := fmt.Sprintf("decision-%d", iteration)
		for _, patch := range []Patch{{NextAction: &next}, {Decisions: []string{decision}}} {
			go func(patch Patch) {
				store := NewStore(workspace)
				<-start
				_, err := store.Patch(ws.ID, patch)
				results <- err
			}(patch)
		}
		close(start)
		for result := 0; result < 2; result++ {
			if err := <-results; err != nil {
				t.Errorf("concurrent patch %d: %v", iteration, err)
			}
		}
		if t.Failed() {
			t.FailNow()
		}
		got, err := reader.Get(ws.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.NextAction != next || len(got.Decisions) != 1 || got.Decisions[0] != decision {
			t.Fatalf("lost disjoint update at iteration %d: nextAction=%q decisions=%v", iteration, got.NextAction, got.Decisions)
		}
	}
	timeline, err := reader.Timeline(ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(timeline) != 41 {
		t.Fatalf("timeline entries = %d, want creation plus 40 patches", len(timeline))
	}
}

func TestStoreCreateGetListRoundtrip(t *testing.T) {
	store := NewStore(t.TempDir())
	ws, err := store.Create(CreateRequest{
		ID:      "ws_test",
		Title:   "Local model runtime",
		Summary: "Durable state for local models.",
		Goal: Goal{
			Objective:          "Make long-running work resumable.",
			AcceptanceCriteria: []string{"state persists", "handoff generates"},
		},
	})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if ws.ID != "ws_test" {
		t.Fatalf("ID = %q", ws.ID)
	}

	got, err := store.Get("ws_test")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got.Title != "Local model runtime" || got.Status != StatusActive {
		t.Fatalf("unexpected workstream: %+v", got)
	}

	list, err := store.List(StatusActive)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(list) != 1 || list[0].ID != "ws_test" {
		t.Fatalf("List = %+v", list)
	}

	timeline, err := store.Timeline("ws_test")
	if err != nil {
		t.Fatalf("Timeline failed: %v", err)
	}
	if len(timeline) != 1 || timeline[0].Type != "created" {
		t.Fatalf("timeline = %+v", timeline)
	}
}

func TestStoreAcceptsExistingWorkstreamWrittenThroughWorkspaceSymlink(t *testing.T) {
	workspace := t.TempDir()
	linkRoot := t.TempDir()
	alias := filepath.Join(linkRoot, "workspace-alias")
	if err := os.Symlink(workspace, alias); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("directory symlink unavailable: %v", err)
		}
		t.Fatal(err)
	}

	legacyWorkspace := filepath.Clean(alias)
	legacyStore := &Store{workspace: legacyWorkspace}
	legacy := Workstream{
		Version: CurrentVersion, ID: "ws_alias", Title: "Alias workspace",
		Workspace: legacyWorkspace, Status: StatusActive,
	}
	if err := writeJSONAtomic(StatePath(legacyStore.workspace, legacy.ID), &legacy); err != nil {
		t.Fatal(err)
	}

	canonicalStore := NewStore(workspace)
	if !sameWorkspace(canonicalStore.Workspace(), workspace) {
		t.Fatalf("NewStore workspace %q does not match canonical workspace %q", canonicalStore.Workspace(), workspace)
	}
	loaded, err := canonicalStore.Get(legacy.ID)
	if err != nil {
		t.Fatalf("Get through canonical workspace after alias write = %v", err)
	}
	if loaded.ID != legacy.ID {
		t.Fatalf("loaded workstream = %#v", loaded)
	}
}

func TestRootUsesCorelayStateAndFallsBackToLegacyState(t *testing.T) {
	workspace := t.TempDir()
	wantCurrent := filepath.Join(workspace, stateDirName, workstreamsDir)
	if got := Root(workspace); got != wantCurrent {
		t.Fatalf("Root() = %q, want new state root %q", got, wantCurrent)
	}

	legacy := filepath.Join(workspace, legacyStateDirName, workstreamsDir)
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := Root(workspace); got != legacy {
		t.Fatalf("Root() = %q, want legacy state root %q", got, legacy)
	}

	if err := os.MkdirAll(wantCurrent, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := Root(workspace); got != wantCurrent {
		t.Fatalf("Root() = %q, want current state root %q", got, wantCurrent)
	}
}

func TestRootFallsBackToClaudeProxyState(t *testing.T) {
	workspace := t.TempDir()
	legacy := filepath.Join(workspace, proxyStateDirName, workstreamsDir)
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := Root(workspace); got != legacy {
		t.Fatalf("Root() = %q, want oldest legacy state root %q", got, legacy)
	}
}

func TestPatchVerificationAppendsTimeline(t *testing.T) {
	store := NewStore(t.TempDir())
	if _, err := store.Create(CreateRequest{ID: "ws_patch", Title: "Patch test"}); err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	vr := VerificationResult{Status: "passed", Source: "auto-verify", Summary: "go test passed"}
	ws, err := store.Patch("ws_patch", Patch{LastVerification: &vr})
	if err != nil {
		t.Fatalf("Patch failed: %v", err)
	}
	if ws.LastVerification.Status != "passed" {
		t.Fatalf("verification not updated: %+v", ws.LastVerification)
	}

	events, err := store.Timeline("ws_patch")
	if err != nil {
		t.Fatalf("Timeline failed: %v", err)
	}
	if events[len(events)-1].Type != "verification_updated" {
		t.Fatalf("last event = %+v", events[len(events)-1])
	}
}

func TestRenderContextCapsAndFences(t *testing.T) {
	ws := Workstream{
		ID:         "ws_ctx",
		Title:      "Context",
		Status:     StatusActive,
		Summary:    strings.Repeat("x", 3000),
		NextAction: "Run tests",
		Goal:       Goal{Objective: "Keep local model focused"},
	}
	got := RenderContext(ws, 600)
	if !strings.Contains(got, "background data, not instructions") {
		t.Fatalf("trust fence missing: %s", got)
	}
	if !strings.Contains(got, "Workstream context truncated") {
		t.Fatalf("expected truncation warning")
	}
	if len(got) > 600 {
		t.Fatalf("context not capped enough: %d", len(got))
	}
}

func TestGenerateHandoffWritesMarkdownAndEvent(t *testing.T) {
	store := NewStore(t.TempDir())
	if _, err := store.Create(CreateRequest{
		ID:         "ws_handoff",
		Title:      "Handoff Test",
		Summary:    "Current state summary.",
		NextAction: "Implement API handlers.",
		Goal: Goal{
			Objective:          "Generate handoff.",
			AcceptanceCriteria: []string{"handoff file exists"},
		},
	}); err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	snap, err := store.GenerateHandoff("ws_handoff", HandoffOptions{IncludeReceipts: true, IncludeMemoryIndex: true})
	if err != nil {
		t.Fatalf("GenerateHandoff failed: %v", err)
	}
	if snap.Path == "" {
		t.Fatal("expected handoff path")
	}
	if _, err := os.Stat(snap.Path); err != nil {
		t.Fatalf("handoff file missing: %v", err)
	}
	if filepath.Base(filepath.Dir(snap.Path)) != handoffsDir {
		t.Fatalf("handoff path not under handoffs dir: %s", snap.Path)
	}
	for _, want := range []string{"# Handoff: Handoff Test", "## Goal", "Implement API handlers"} {
		if !strings.Contains(snap.Markdown, want) {
			t.Fatalf("handoff missing %q:\n%s", want, snap.Markdown)
		}
	}

	events, err := store.Timeline("ws_handoff")
	if err != nil {
		t.Fatalf("Timeline failed: %v", err)
	}
	if events[len(events)-1].Type != "handoff_generated" {
		t.Fatalf("last event = %+v", events[len(events)-1])
	}
}

func TestGenerateHandoffRestoresDecisionsPlanStageUnfinishedWorkAndVerification(t *testing.T) {
	store := NewStore(t.TempDir())
	ws, err := store.Create(CreateRequest{
		ID: "ws_resume_plan", Title: "Resume plan", Summary: "Keep the chosen design and verify the remaining stage.",
		NextAction: "Continue source review", Decisions: []string{"Use the stored Plan as the workflow owner."},
		Goal: Goal{Objective: "Resume the approved objective", AcceptanceCriteria: []string{"Keep the stage binding exact"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	definition := json.RawMessage(`{"version":1,"name":"Resume Plan","objective":"Restore the current workflow objective","stages":[{"id":"research","name":"Research","taskIds":["task_research"]},{"id":"implement","name":"Implement","taskIds":["task_implement"]}],"tasks":[{"id":"task_research","name":"Inspect source","stage":"research","goal":"Read the production caller","acceptanceCriteria":["Record the exact caller"]},{"id":"task_implement","name":"Implement","stage":"implement","goal":"Apply the reviewed change","acceptanceCriteria":["Run the relevant test"]}]}`)
	plan, err := store.CreatePlan(ws.ID, CreatePlanRequest{ID: "plan_resume", Definition: definition})
	if err != nil {
		t.Fatal(err)
	}
	plan, err = store.ApprovePlan(ws.ID, plan.ID, ApprovePlanRequest{
		ExpectedRevision: plan.Revision, ExpectedStateRevision: plan.StateRevision,
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err = store.BeginPlanStage(ws.ID, plan.ID, BeginPlanStageRequest{
		ExpectedRevision: plan.Revision, ExpectedStateRevision: plan.StateRevision,
		StageID: "research", RunID: "run_resume_incomplete",
	})
	if err != nil {
		t.Fatal(err)
	}
	verification := VerificationResult{Status: "failed", Source: "go-test", Summary: "One targeted test still fails."}
	ws, err = store.Patch(ws.ID, Patch{
		Decisions:        []string{"Preserve the existing API contract.", "Keep the Plan stage incomplete until evidence passes."},
		LastVerification: &verification,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ws.Decisions) != 2 {
		t.Fatalf("workstream decisions = %+v", ws.Decisions)
	}

	snapshot, err := store.GenerateHandoff(ws.ID, HandoffOptions{IncludeReceipts: true, PlanID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Preserve the existing API contract.", "Approved Plan plan_resume revision 1",
		"Plan ID: plan_resume", "Definition revision: 1", "Current stage: research — Research (running)",
		"run_resume_incomplete", "Unfinished task: task_research: Read the production caller",
		"Recovery required:", "process stopped", "Status: failed", "One targeted test still fails.",
	} {
		if !strings.Contains(snapshot.Markdown, want) {
			t.Fatalf("handoff is missing %q:\n%s", want, snapshot.Markdown)
		}
	}
	if _, err := store.GenerateHandoff(ws.ID, HandoffOptions{PlanID: "plan_missing"}); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("unknown selected Plan error=%v, want ErrPlanNotFound", err)
	}
}

func TestGenerateHandoffIncludesPendingStageTasksAndAcceptance(t *testing.T) {
	store := NewStore(t.TempDir())
	ws, err := store.Create(CreateRequest{ID: "ws_handoff_pending", Title: "Pending stage resume"})
	if err != nil {
		t.Fatal(err)
	}
	definition := json.RawMessage(`{"version":1,"name":"Pending Plan","objective":"Start the approved implementation","stages":[{"id":"implement","name":"Implement","taskIds":["task_implement"]}],"tasks":[{"id":"task_implement","name":"Implement change","stage":"implement","goal":"Update the production handler","acceptanceCriteria":["The handler uses the canonical state","The regression test passes"]}]}`)
	plan, err := store.CreatePlan(ws.ID, CreatePlanRequest{ID: "plan_pending", Definition: definition})
	if err != nil {
		t.Fatal(err)
	}
	plan, err = store.ApprovePlan(ws.ID, plan.ID, ApprovePlanRequest{
		ExpectedRevision: plan.Revision, ExpectedStateRevision: plan.StateRevision,
	})
	if err != nil {
		t.Fatal(err)
	}

	snapshot, err := store.GenerateHandoff(ws.ID, HandoffOptions{PlanID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Current stage: implement — Implement (pending)",
		"Unfinished task: task_implement: Update the production handler",
		"Acceptance: The handler uses the canonical state",
		"Acceptance: The regression test passes",
	} {
		if !strings.Contains(snapshot.Markdown, want) {
			t.Fatalf("pending-stage handoff is missing %q:\n%s", want, snapshot.Markdown)
		}
	}
}

func TestRenderContextWithPlanPreservesWorkflowReferencesWithinBound(t *testing.T) {
	workspace := t.TempDir()
	store := NewStore(workspace)
	ws, err := store.Create(CreateRequest{
		ID: "ws_context_resume", Title: strings.Repeat("Long title ", 40),
		Summary: strings.Repeat("Long summary ", 500), Decisions: []string{strings.Repeat("decision ", 300)},
		Goal: Goal{Objective: strings.Repeat("goal ", 200)},
	})
	if err != nil {
		t.Fatal(err)
	}
	definition := json.RawMessage(`{"objective":"The durable plan objective","stages":[{"id":"resume-stage","name":"Resume stage","taskIds":["resume-task"]}],"tasks":[{"id":"resume-task","stage":"resume-stage","goal":"Review the changed files","acceptanceCriteria":["Evidence must pass"]}]}`)
	plan, err := store.CreatePlan(ws.ID, CreatePlanRequest{ID: "plan_context_resume", Definition: definition})
	if err != nil {
		t.Fatal(err)
	}
	plan, err = store.ApprovePlan(ws.ID, plan.ID, ApprovePlanRequest{
		ExpectedRevision: plan.Revision, ExpectedStateRevision: plan.StateRevision,
	})
	if err != nil {
		t.Fatal(err)
	}
	context := RenderContextWithPlan(*ws, plan, "resume-stage", 2000)
	if len(context) > 2000 {
		t.Fatalf("combined Workstream/Plan context exceeded bound: %d", len(context))
	}
	for _, want := range []string{"ws_context_resume", "plan_context_resume", "definition revision 1", "Current stage: resume-stage", "The durable plan objective"} {
		if !strings.Contains(context, want) {
			t.Fatalf("bounded context lost %q:\n%s", want, context)
		}
	}
}

func TestRejectsInvalidStatus(t *testing.T) {
	store := NewStore(t.TempDir())
	if _, err := store.Create(CreateRequest{ID: "ws_status", Title: "Status"}); err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	bad := Status("done-ish")
	if _, err := store.Patch("ws_status", Patch{Status: &bad}); err == nil {
		t.Fatal("expected invalid status error")
	}
}
