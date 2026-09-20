package workstream

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
)

func TestWorkstreamPlanStorePersistsRevisionAndStageOwnership(t *testing.T) {
	workspace := t.TempDir()
	store := NewStore(workspace)
	for _, id := range []string{"ws_a", "ws_b"} {
		if _, err := store.Create(CreateRequest{ID: id, Title: id}); err != nil {
			t.Fatal(err)
		}
	}
	definition := json.RawMessage(`{"version":1,"name":"Feature","objective":"Ship a feature","stages":[{"id":"research","name":"Research"},{"id":"implement","name":"Implement"}],"tasks":[]}`)
	created, err := store.CreatePlan("ws_a", CreatePlanRequest{ID: "plan_feature", Definition: definition})
	if err != nil {
		t.Fatal(err)
	}
	if created.WorkstreamID != "ws_a" || created.Revision != 1 || created.StateRevision != 1 ||
		created.Status != PlanStatusDraft || created.ApprovedRevision != 0 || len(created.Stages) != 2 ||
		!reflect.DeepEqual(created.Stages[0], PlanStage{ID: "research", Status: PlanStageStatusPending}) ||
		!reflect.DeepEqual(created.Stages[1], PlanStage{ID: "implement", Status: PlanStageStatusPending}) {
		t.Fatalf("created plan = %+v", created)
	}

	secondStore := NewStore(workspace)
	reloaded, err := secondStore.GetPlan("ws_a", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	var wantDefinition any
	var gotDefinition any
	if err := json.Unmarshal(definition, &wantDefinition); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(reloaded.Definition, &gotDefinition); err != nil {
		t.Fatal(err)
	}
	if reloaded.Revision != created.Revision || !reflect.DeepEqual(gotDefinition, wantDefinition) {
		t.Fatalf("reloaded plan = %+v", reloaded)
	}
	if _, err := secondStore.GetPlan("ws_b", created.ID); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("cross-workstream lookup error = %v, want ErrPlanNotFound", err)
	}
	plans, err := secondStore.ListPlans("ws_a")
	if err != nil || len(plans) != 1 || plans[0].ID != created.ID {
		t.Fatalf("ListPlans = %+v, %v", plans, err)
	}
	timeline, err := secondStore.Timeline("ws_a")
	if err != nil {
		t.Fatalf("Timeline = %v", err)
	}
	if len(timeline) == 0 || timeline[len(timeline)-1].Type != "plan_created" || timeline[len(timeline)-1].Data["planId"] != created.ID {
		t.Fatalf("timeline = %+v, %v", timeline, err)
	}
}

func TestWorkstreamPlanApprovalRevisionAndEvidenceTransitions(t *testing.T) {
	workspace := t.TempDir()
	store := NewStore(workspace)
	if _, err := store.Create(CreateRequest{ID: "ws_plan_lifecycle", Title: "Plan lifecycle"}); err != nil {
		t.Fatal(err)
	}
	definition := json.RawMessage(`{"version":1,"name":"Feature","objective":"Ship a feature","stages":[{"id":"research","name":"Research"},{"id":"implement","name":"Implement"}],"tasks":[]}`)
	created, err := store.CreatePlan("ws_plan_lifecycle", CreatePlanRequest{ID: "plan_lifecycle", Definition: definition})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := store.ApprovePlan("ws_plan_lifecycle", created.ID, ApprovePlanRequest{
		ExpectedRevision: 1, ExpectedStateRevision: 1,
	})
	if err != nil || approved.Status != PlanStatusApproved || approved.ApprovedRevision != 1 || approved.StateRevision != 2 {
		t.Fatalf("ApprovePlan = %+v, %v", approved, err)
	}

	// A definition edit is a new revision and discards the previous approval.
	updated, err := store.UpdatePlan("ws_plan_lifecycle", created.ID, UpdatePlanRequest{
		ExpectedRevision: 1, ExpectedStateRevision: 2,
		Definition: json.RawMessage(`{"version":1,"name":"Feature v2","objective":"Ship a changed feature","stages":[{"id":"implement","name":"Implement"}],"tasks":[]}`),
	})
	if err != nil || updated.Revision != 2 || updated.StateRevision != 3 || updated.Status != PlanStatusDraft || updated.ApprovedRevision != 0 ||
		len(updated.Stages) != 1 || updated.Stages[0].ID != "implement" || updated.Stages[0].Status != PlanStageStatusPending {
		t.Fatalf("UpdatePlan = %+v, %v", updated, err)
	}
	if _, err := store.BeginPlanStage("ws_plan_lifecycle", created.ID, BeginPlanStageRequest{
		ExpectedRevision: 1, ExpectedStateRevision: 2, StageID: "research", RunID: "stale-run",
	}); !errors.Is(err, ErrPlanRevisionConflict) {
		t.Fatalf("stale revision execution error = %v, want ErrPlanRevisionConflict", err)
	}
	if _, err := store.BeginPlanStage("ws_plan_lifecycle", created.ID, BeginPlanStageRequest{
		ExpectedRevision: 2, ExpectedStateRevision: 3, StageID: "implement", RunID: "draft-run",
	}); !errors.Is(err, ErrPlanTransition) {
		t.Fatalf("unapproved execution error = %v, want ErrPlanTransition", err)
	}
}

func TestWorkstreamPlanCASAndCompletedEvidenceRequirements(t *testing.T) {
	workspace := t.TempDir()
	first := NewStore(workspace)
	if _, err := first.Create(CreateRequest{ID: "ws_plan_cas", Title: "Plan CAS"}); err != nil {
		t.Fatal(err)
	}
	created, err := first.CreatePlan("ws_plan_cas", CreatePlanRequest{
		ID:         "plan_cas",
		Definition: json.RawMessage(`{"version":1,"name":"Feature","objective":"Ship feature","stages":[{"id":"research","name":"Research"},{"id":"implement","name":"Implement"}],"tasks":[]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := first.ApprovePlan("ws_plan_cas", created.ID, ApprovePlanRequest{ExpectedRevision: 1, ExpectedStateRevision: 1})
	if err != nil {
		t.Fatal(err)
	}

	stores := []*Store{NewStore(workspace), NewStore(workspace)}
	var wait sync.WaitGroup
	results := make(chan error, len(stores))
	for i, store := range stores {
		wait.Add(1)
		go func(index int, store *Store) {
			defer wait.Done()
			_, err := store.BeginPlanStage("ws_plan_cas", created.ID, BeginPlanStageRequest{
				ExpectedRevision: approved.Revision, ExpectedStateRevision: approved.StateRevision,
				StageID: "research", RunID: fmt.Sprintf("run-%d", index),
			})
			results <- err
		}(i, store)
	}
	wait.Wait()
	close(results)
	successes, conflicts := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrPlanRevisionConflict) {
			conflicts++
		} else {
			t.Fatalf("concurrent BeginPlanStage error = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d, want one each", successes, conflicts)
	}

	plan, err := first.GetPlan("ws_plan_cas", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	stage := plan.Stages[0]
	activeRunID := stage.Attempts[len(stage.Attempts)-1].RunID
	if _, err := first.FinishPlanStage("ws_plan_cas", created.ID, FinishPlanStageRequest{
		ExpectedRevision: plan.Revision, ExpectedStateRevision: plan.StateRevision,
		StageID: "research", RunID: activeRunID, Status: PlanStageStatusCompleted,
		VerificationStatus: "passed", CompletionStatus: "complete",
	}); !errors.Is(err, ErrPlanEvidence) {
		t.Fatalf("completion without receipt error = %v, want ErrPlanEvidence", err)
	}

	failed, err := first.FinishPlanStage("ws_plan_cas", created.ID, FinishPlanStageRequest{
		ExpectedRevision: plan.Revision, ExpectedStateRevision: plan.StateRevision,
		StageID: "research", RunID: activeRunID, Status: PlanStageStatusFailed,
		VerificationStatus: "failed", ReceiptDigest: testPlanDigest("failed receipt"),
	})
	if err != nil || failed.Status != PlanStatusFailed || failed.Stages[0].Status != PlanStageStatusFailed {
		t.Fatalf("failed FinishPlanStage = %+v, %v", failed, err)
	}
	if failed.Stages[0].Attempts[0].Evidence != nil || failed.Stages[0].Attempts[0].VerificationStatus != "failed" {
		t.Fatalf("failed verification must not create acceptance evidence: %+v", failed.Stages[0].Attempts[0])
	}
}

func TestWorkstreamPlanReconcileClosesInterruptedAttemptAsIncomplete(t *testing.T) {
	store := NewStore(t.TempDir())
	if _, err := store.Create(CreateRequest{ID: "ws_plan_reconcile", Title: "Plan reconcile"}); err != nil {
		t.Fatal(err)
	}
	created, err := store.CreatePlan("ws_plan_reconcile", CreatePlanRequest{
		ID: "plan_reconcile", Definition: json.RawMessage(`{"stages":[{"id":"build","name":"Build"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := store.ApprovePlan("ws_plan_reconcile", created.ID, ApprovePlanRequest{
		ExpectedRevision: created.Revision, ExpectedStateRevision: created.StateRevision,
	})
	if err != nil {
		t.Fatal(err)
	}
	running, err := store.BeginPlanStage("ws_plan_reconcile", created.ID, BeginPlanStageRequest{
		ExpectedRevision: approved.Revision, ExpectedStateRevision: approved.StateRevision,
		StageID: "build", RunID: "run_interrupted",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := ReconcilePlanStageRequest{
		ExpectedRevision: running.Revision, ExpectedStateRevision: running.StateRevision,
		StageID: "build", RunID: "run_interrupted",
	}
	if _, err := store.ReconcilePlanStage("ws_plan_reconcile", created.ID, request); !errors.Is(err, ErrPlanTransition) {
		t.Fatalf("reconcile without manual acknowledgement error=%v, want ErrPlanTransition", err)
	}
	wrongRun := request
	wrongRun.ManualAcknowledged = true
	wrongRun.RunID = "run_other"
	if _, err := store.ReconcilePlanStage("ws_plan_reconcile", created.ID, wrongRun); !errors.Is(err, ErrPlanTransition) {
		t.Fatalf("reconcile for wrong run error=%v, want ErrPlanTransition", err)
	}

	request.ManualAcknowledged = true
	reconciled, err := store.ReconcilePlanStage("ws_plan_reconcile", created.ID, request)
	if err != nil {
		t.Fatal(err)
	}
	attempt := reconciled.Stages[0].Attempts[0]
	if reconciled.Status != PlanStatusFailed || reconciled.Stages[0].Status != PlanStageStatusFailed ||
		attempt.Status != PlanStageAttemptFailed || attempt.VerificationStatus != "not-run" || attempt.Evidence != nil ||
		attempt.RunID != "run_interrupted" || attempt.FinishedAt.IsZero() {
		t.Fatalf("reconciled interrupted run was not closed as incomplete: %+v", reconciled)
	}
	if _, err := store.FinishPlanStage("ws_plan_reconcile", created.ID, FinishPlanStageRequest{
		ExpectedRevision: running.Revision, ExpectedStateRevision: running.StateRevision,
		StageID: "build", RunID: "run_interrupted", Status: PlanStageStatusCompleted,
		VerificationStatus: "passed", CompletionStatus: "complete",
	}); !errors.Is(err, ErrPlanRevisionConflict) {
		t.Fatalf("late finish after reconciliation error=%v, want ErrPlanRevisionConflict", err)
	}
}

func testPlanDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func TestWorkstreamPlanStoreRejectsUnsafeOrDuplicateStageIDs(t *testing.T) {
	store := NewStore(t.TempDir())
	if _, err := store.Create(CreateRequest{ID: "ws_plan_validation", Title: "Plan validation"}); err != nil {
		t.Fatal(err)
	}
	for _, definition := range []string{
		`{"stages":[{"id":"../escape"}]}`,
		`{"stages":[{"id":"repeat"},{"id":"repeat"}]}`,
		`{"stages":[{"id":" padded "}]}`,
	} {
		t.Run(fmt.Sprintf("definition_%d", len(definition)), func(t *testing.T) {
			if _, err := store.CreatePlan("ws_plan_validation", CreatePlanRequest{Definition: json.RawMessage(definition)}); err == nil {
				t.Fatalf("CreatePlan accepted unsafe definition %s", definition)
			}
		})
	}
	if _, err := store.CreatePlan("ws_plan_validation", CreatePlanRequest{ID: "../escape", Definition: json.RawMessage(`{"stages":[]}`)}); err == nil {
		t.Fatal("CreatePlan accepted a path-like plan ID")
	}
}

func TestWorkstreamPlanStoreSerializesDuplicateCreationAcrossStoreInstances(t *testing.T) {
	workspace := t.TempDir()
	first := NewStore(workspace)
	if _, err := first.Create(CreateRequest{ID: "ws_plan_race", Title: "Plan race"}); err != nil {
		t.Fatal(err)
	}
	definition := json.RawMessage(`{"stages":[]}`)
	stores := []*Store{NewStore(workspace), NewStore(workspace)}
	var wait sync.WaitGroup
	errs := make(chan error, len(stores))
	for _, store := range stores {
		wait.Add(1)
		go func(store *Store) {
			defer wait.Done()
			_, err := store.CreatePlan("ws_plan_race", CreatePlanRequest{ID: "plan_same", Definition: definition})
			errs <- err
		}(store)
	}
	wait.Wait()
	close(errs)
	successes, duplicates := 0, 0
	for err := range errs {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrPlanConflict) {
			duplicates++
		} else {
			t.Fatalf("CreatePlan error = %v", err)
		}
	}
	if successes != 1 || duplicates != 1 {
		t.Fatalf("successes=%d duplicates=%d, want one each", successes, duplicates)
	}
}
