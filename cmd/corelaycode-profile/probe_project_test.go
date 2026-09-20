package main

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/capabilityprofile"
	"github.com/Dannykkh/corelay-code/internal/types"
)

type projectTestProvider struct {
	profileTestProvider
	calls  int
	wrong  bool
	first  string
	second string
	leak   bool
}

func (p *projectTestProvider) StreamMessage(_ context.Context, req *types.MessagesRequest, _ *types.StreamOptions) (<-chan types.SSEEvent, error) {
	p.calls++
	// The second selected session must not inherit A's previous answer.
	if p.calls == 3 && p.first != "" {
		for _, m := range req.Messages {
			if strings.Contains(string(m.Content), p.first) {
				p.leak = true
			}
		}
	}
	if p.calls == 5 && p.second != "" {
		for _, m := range req.Messages {
			if strings.Contains(string(m.Content), p.second) {
				p.leak = true
			}
		}
	}
	if p.calls%2 == 1 {
		return nativeToolEvents("Read", `{"file_path":"README.md"}`), nil
	}
	re := regexp.MustCompile(`PROJECT_[0-9a-f]{48}`)
	answer := re.FindString(string(req.Messages[len(req.Messages)-1].Content))
	if p.first == "" {
		p.first = answer
	}
	if p.calls == 4 {
		p.second = answer
	}
	if p.wrong && p.calls == 4 {
		answer = p.first
	}
	return textEvents(answer), nil
}

func TestProjectProbeActuallyResumesSeparateSessions(t *testing.T) {
	for _, wrong := range []bool{false, true} {
		name := "correct-project"
		if wrong {
			name = "wrong-project"
		}
		t.Run(name, func(t *testing.T) {
			for attempt := 1; attempt <= 3; attempt++ {
				provider := &projectTestProvider{profileTestProvider: profileTestProvider{name: "project-test"}, wrong: wrong}
				target, err := capabilityprofile.NewTargetIdentity(capabilityprofile.TargetSpec{Provider: provider.Name(), Model: "profile-model", Endpoint: "https://endpoint.invalid"})
				if err != nil {
					t.Fatal(err)
				}
				executor, err := newAgentProbeExecutor(provider, "profile-model", target, 30*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				plan := capabilityprofile.DefaultProbePlan()
				var probeCase capabilityprofile.ProbeCase
				for _, candidate := range plan.Cases() {
					if candidate.Category == capabilityprofile.CategoryProjectSwitch {
						probeCase = candidate
						break
					}
				}
				result, err := executor.Execute(context.Background(), capabilityprofile.ProbeExecution{Target: target, PlanVersion: plan.Version(), PlanDigest: plan.Digest(), Variant: plan.Variant(), Case: probeCase, Attempt: attempt, WorkspaceRoot: t.TempDir()})
				if wrong {
					if result.Success || !result.FalseDone || err == nil {
						t.Fatalf("wrong project accepted: %+v %v", result, err)
					}
				} else {
					if err != nil || !result.Success || provider.calls != 6 {
						t.Fatalf("A/B/A run failed calls=%d result=%+v err=%v", provider.calls, result, err)
					}
				}
				if provider.leak {
					t.Fatal("project A transcript leaked into B provider request")
				}
			}
		})
	}
}
