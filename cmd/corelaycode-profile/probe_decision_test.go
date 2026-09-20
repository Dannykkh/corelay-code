package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/capabilityprofile"
	"github.com/Dannykkh/corelay-code/internal/types"
)

type decisionRetentionProvider struct {
	profileTestProvider
	wrong                bool
	wantLimit            int
	sawCompactedDecision bool
	leakedFinalAnswer    bool
}

func (p *decisionRetentionProvider) StreamMessage(_ context.Context, req *types.MessagesRequest, _ *types.StreamOptions) (<-chan types.SSEEvent, error) {
	p.streamCalls++
	serialized, _ := json.Marshal(req.Messages)
	p.sawCompactedDecision = strings.Contains(string(serialized), "deterministic structured state") && strings.Contains(string(serialized), fmt.Sprintf("retry_limit=%d", p.wantLimit))
	last := string(req.Messages[len(req.Messages)-1].Content)
	p.leakedFinalAnswer = strings.Contains(last, "retry_limit=") || strings.Contains(last, "authorization=required")
	limit, authorization := p.wantLimit, "required"
	if p.wrong {
		limit, authorization = 19, "optional"
	}
	markerRequest := &types.MessagesRequest{Messages: req.Messages[len(req.Messages)-1:]}
	return textEvents(fmt.Sprintf("%s\n{\"retry_limit\":%d,\"authorization\":%q}", markerFromRequest(markerRequest), limit, authorization)), nil
}

func TestDecisionRetentionProbeCompactsChangedDecisionAndRejectsOldAnswer(t *testing.T) {
	for _, wrong := range []bool{false, true} {
		for attempt := 1; attempt <= 3; attempt++ {
			t.Run(fmt.Sprintf("wrong=%v/attempt=%d", wrong, attempt), func(t *testing.T) {
				plan := capabilityprofile.DefaultProbePlan()
				var probeCase capabilityprofile.ProbeCase
				for _, candidate := range plan.Cases() {
					if candidate.Category == capabilityprofile.CategoryDecisionRetention {
						probeCase = candidate
						break
					}
				}
				if probeCase.ID == "" {
					t.Fatal("decision retention category absent")
				}
				provider := &decisionRetentionProvider{
					profileTestProvider: profileTestProvider{name: "profile-test"}, wrong: wrong,
					wantLimit: 2 + int((probeCase.Seed+int64(attempt))%5),
				}
				target, err := capabilityprofile.NewTargetIdentity(capabilityprofile.TargetSpec{Provider: provider.Name(), Model: "profile-model", Endpoint: "https://endpoint.invalid", APIKey: "fixture-only"})
				if err != nil {
					t.Fatal(err)
				}
				executor, err := newAgentProbeExecutor(provider, "profile-model", target, 10*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				observation, err := executor.Execute(context.Background(), capabilityprofile.ProbeExecution{
					Target: target, PlanVersion: plan.Version(), PlanDigest: plan.Digest(), Variant: plan.Variant(),
					Case: probeCase, Attempt: attempt, WorkspaceRoot: t.TempDir(),
				})
				if err != nil {
					t.Fatal(err)
				}
				if observation.Success == wrong || observation.FalseDone != wrong {
					t.Fatalf("observation=%+v", observation)
				}
				if provider.streamCalls != 1 || !provider.sawCompactedDecision || provider.leakedFinalAnswer {
					t.Fatalf("calls=%d compacted=%v leaked=%v", provider.streamCalls, provider.sawCompactedDecision, provider.leakedFinalAnswer)
				}
				if observation.TraceDigest == "" || observation.ArtifactDigest == "" {
					t.Fatal("missing compaction/answer evidence")
				}
			})
		}
	}
}

func TestDecisionProbeAnswerRequiresDecisionBeyondMarker(t *testing.T) {
	for _, answer := range []string{"MARKER", "MARKER\n{}", `MARKER
{"retry_limit":3,"authorization":"optional"}`, `MARKER
{"retry_limit":3,"authorization":"required","explanation":"extra"}`} {
		if decisionProbeAnswerMatches(answer, "MARKER", 3) {
			t.Fatalf("accepted wrong answer %q", answer)
		}
	}
}
