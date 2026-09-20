package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/capabilityprofile"
	"github.com/Dannykkh/corelay-code/internal/types"
)

// A deterministic provider makes the intervention observable without a paid
// model: only the injected tool-contract lesson makes it actually read the file.
type lessonAwareProvider struct {
	profileTestProvider
	sawLesson bool
}

type cliLessonExecutor struct{ cliComparisonExecutor }

func (e cliLessonExecutor) Execute(ctx context.Context, input capabilityprofile.ProbeExecution) (capabilityprofile.ProbeObservation, error) {
	result, err := e.cliComparisonExecutor.Execute(ctx, input)
	result.LessonDigest = input.Lessons.Digest()
	return result, err
}

func TestSelectedLessonsReachNextProductionRun(t *testing.T) {
	provider := &lessonAwareProvider{profileTestProvider: profileTestProvider{name: "profile-test"}}
	target, err := capabilityprofile.NewTargetIdentity(capabilityprofile.TargetSpec{Provider: provider.Name(), Model: "profile-model", Endpoint: "https://endpoint.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	plan := capabilityprofile.DefaultProbePlan()
	profiler, err := capabilityprofile.NewProfiler(cliComparisonWorkspaceFactory{}, cliLessonExecutor{}, capabilityprofile.ProfilerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := profiler.RunWithLessons(context.Background(), target, plan, capabilityprofile.LessonToolContract)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := capabilityprofile.NewAutomaticSelection(target, profile, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	execution := capabilityprofile.ProbeExecution{Target: target, PlanVersion: plan.Version(), PlanDigest: plan.Digest(), Variant: plan.Variant(), Case: plan.Cases()[0], Attempt: 1, WorkspaceRoot: root}
	fixture, err := prepareAgentProbeFixture(execution)
	if err != nil {
		t.Fatal(err)
	}
	probeEnvironmentMu.Lock()
	defer probeEnvironmentMu.Unlock()
	restore, err := scopeProbeEnvironment(root)
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	message, _ := json.Marshal(fixture.prompt)
	anchor, err := agent.NewPlanAnchor(agent.PlanAnchorSpec{Objective: fixture.prompt, DefinitionOfDone: []string{"Read probe.txt and report its marker"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	events := make(chan agent.Event, 64)
	go agent.RunLoopWithOptions(ctx, provider, "profile-model", []types.Message{{Role: "user", Content: message}}, root, agent.RunOptions{
		CapabilityProfile: &selection, PlanAnchor: &anchor, DisableWorkspaceMCP: true, DisablePlugins: true,
	}, events)
	observation := observeAgentProbe(events, fixture, execution, time.Now())
	if !provider.sawLesson || !observation.value.Success || provider.streamCalls != 2 {
		t.Fatalf("selected lesson did not reach actual execution: %+v", observation.value)
	}
}

func (p *lessonAwareProvider) StreamMessage(ctx context.Context, req *types.MessagesRequest, options *types.StreamOptions) (<-chan types.SSEEvent, error) {
	p.sawLesson = strings.Contains(string(req.System), "check its live name, required arguments")
	if !p.sawLesson {
		return textEvents(markerFromRequest(req)), nil
	}
	if len(req.Tools) == 1 && req.Tools[0].Name == "SelectToolCategory" {
		return nativeToolEvents("SelectToolCategory", `{"category":"read"}`), nil
	}
	return p.profileTestProvider.StreamMessage(ctx, req, options)
}

func TestLessonCandidateChangesActualAgentExecution(t *testing.T) {
	for _, policy := range []capabilityprofile.LessonPolicy{0, capabilityprofile.LessonToolContract} {
		provider := &lessonAwareProvider{profileTestProvider: profileTestProvider{name: "profile-test"}}
		target, err := capabilityprofile.NewTargetIdentity(capabilityprofile.TargetSpec{Provider: provider.Name(), Model: "profile-model", Endpoint: "https://endpoint.invalid"})
		if err != nil {
			t.Fatal(err)
		}
		executor, err := newAgentProbeExecutor(provider, "profile-model", target, 10*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		plan := capabilityprofile.DefaultProbePlan()
		observation, err := executor.Execute(context.Background(), capabilityprofile.ProbeExecution{
			Target: target, PlanVersion: plan.Version(), PlanDigest: plan.Digest(), Variant: plan.Variant(),
			Case: plan.Cases()[0], Attempt: 1, WorkspaceRoot: t.TempDir(), Lessons: policy, LessonEvaluation: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if observation.Success != (policy != 0) || provider.sawLesson != (policy != 0) || observation.LessonDigest != policy.Digest() {
			t.Fatalf("policy=%v observation=%+v saw=%v", policy, observation, provider.sawLesson)
		}
		if policy != 0 && provider.streamCalls != 2 {
			t.Fatal("candidate did not execute the actual read/result loop")
		}
	}
}
