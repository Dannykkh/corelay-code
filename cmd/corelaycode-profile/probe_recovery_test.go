package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/capabilityprofile"
	"github.com/Dannykkh/corelay-code/internal/types"
)

type interruptedRecoveryProvider struct {
	profileTestProvider
	mode         string
	answerMarker string
	sawResume    bool
}

func (p *interruptedRecoveryProvider) StreamMessage(_ context.Context, req *types.MessagesRequest, _ *types.StreamOptions) (<-chan types.SSEEvent, error) {
	p.streamCalls++
	if p.mode == "marker-only" {
		return textEvents(p.answerMarker), nil
	}
	switch p.streamCalls {
	case 1:
		return nativeToolEvents("Read", `{"file_path":"resume/checkpoint.txt"}`), nil
	case 2:
		return nativeToolEvents("Write", `{"file_path":"resume/checkpoint.txt","content":"state=resumed\n"}`), nil
	case 3:
		encoded, _ := json.Marshal(req.Messages)
		p.sawResume = strings.Contains(string(encoded), "durable checkpoint was reconciled")
		if p.mode == "replay" {
			return nativeToolEvents("Write", `{"file_path":"resume/checkpoint.txt","content":"state=resumed\n"}`), nil
		}
		return nativeToolEvents("Read", `{"file_path":"resume/checkpoint.txt"}`), nil
	default:
		answer := "state=resumed"
		if p.mode == "wrong-answer" {
			answer = "state=pending"
		}
		return textEvents(p.answerMarker + "\n" + answer), nil
	}
}

func TestInterruptedRecoveryProbeRequiresDurableReconcileAndNoReplay(t *testing.T) {
	for _, mode := range []string{"success", "marker-only", "wrong-answer", "replay"} {
		for attempt := 1; attempt <= 3; attempt++ {
			t.Run(fmt.Sprintf("%s/attempt=%d", mode, attempt), func(t *testing.T) {
				plan := capabilityprofile.DefaultProbePlan()
				var probeCase capabilityprofile.ProbeCase
				for _, candidate := range plan.Cases() {
					if candidate.Category == capabilityprofile.CategoryInterruptRecovery {
						probeCase = candidate
						break
					}
				}
				if probeCase.ID == "" {
					t.Fatal("interruption category missing")
				}
				provider := &interruptedRecoveryProvider{profileTestProvider: profileTestProvider{name: "profile-test"}, mode: mode,
					answerMarker: fmt.Sprintf("CORELAY_PROBE_%d_%d", probeCase.Seed, attempt)}
				target, err := capabilityprofile.NewTargetIdentity(capabilityprofile.TargetSpec{Provider: provider.Name(), Model: "profile-model", Endpoint: "https://endpoint.invalid", APIKey: "fixture-only"})
				if err != nil {
					t.Fatal(err)
				}
				executor, err := newAgentProbeExecutor(provider, "profile-model", target, 10*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				workspace := t.TempDir()
				observed, err := executor.Execute(context.Background(), capabilityprofile.ProbeExecution{
					Target: target, PlanVersion: plan.Version(), PlanDigest: plan.Digest(), Variant: plan.Variant(), Case: probeCase, Attempt: attempt, WorkspaceRoot: workspace,
				})
				if mode == "marker-only" {
					if err == nil || observed.Success || observed.Recovered || !observed.FalseDone {
						t.Fatalf("marker-only accepted: observation=%+v err=%v", observed, err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				wantSuccess := mode == "success"
				if observed.Success != wantSuccess || observed.Recovered != wantSuccess || observed.FalseDone == wantSuccess {
					t.Fatalf("observation=%+v", observed)
				}
				if provider.streamCalls != 4 || !provider.sawResume {
					t.Fatalf("calls=%d resume=%v", provider.streamCalls, provider.sawResume)
				}
				content, err := os.ReadFile(filepath.Join(workspace, "resume", "checkpoint.txt"))
				if err != nil || string(content) != "state=resumed\n" {
					t.Fatalf("postimage=%q err=%v", content, err)
				}
				if observed.TraceDigest == "" || observed.ArtifactDigest == "" {
					t.Fatal("missing reconciliation evidence")
				}
			})
		}
	}
}
