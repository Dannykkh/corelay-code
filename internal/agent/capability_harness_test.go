package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/capabilityprofile"
	"github.com/Dannykkh/corelay-code/internal/config"
	"github.com/Dannykkh/corelay-code/internal/harness"
	"github.com/Dannykkh/corelay-code/internal/types"
)

var capabilityHarnessNow = time.Date(2026, 8, 12, 6, 0, 0, 0, time.UTC)

func TestResolveRunHarnessEmpiricalCompositionAndExplicitPrecedence(t *testing.T) {
	const model = "qwen-coder:30b"
	selection := capabilityHarnessSelection(t, "ollama", model, 48*time.Hour)
	temperature := 0.35
	provider := &policyTestProvider{
		name: "ollama",
		models: []types.ModelInfo{{
			ID:            model,
			ContextWindow: 64_000,
			MaxOutput:     8_000,
		}},
	}
	resolved, err := resolveRunHarnessWithCapability(
		provider,
		model,
		nil,
		config.Config{LocalToolBudget: 9, AgentTemperature: &temperature},
		7,
		&selection,
		capabilityHarnessNow.Add(time.Hour),
	)
	if err != nil {
		t.Fatal(err)
	}

	profile := resolved.Profile
	if profile.ToolBudget() != 7 {
		t.Fatalf("tool budget = %d, want explicit env value 7", profile.ToolBudget())
	}
	if got, ok := profile.Temperature(); !ok || got != temperature {
		t.Fatalf("temperature = (%v, %v), want explicit config value", got, ok)
	}
	if profile.ContextWindow() != 64_000 || profile.OutputReserve() != 8_000 {
		t.Fatalf("provider metadata = context %d output %d", profile.ContextWindow(), profile.OutputReserve())
	}
	if profile.ReliableInputTokens() != 28_800 {
		t.Fatalf("reliable input ceiling = %d, want empirical 28800", profile.ReliableInputTokens())
	}
	if profile.ResponsePolicy() != harness.ResponseNative ||
		profile.EditPolicy() != harness.EditPatchFirst ||
		profile.PlanAnchorMode() != harness.PlanAnchorCompact ||
		profile.ToolRouting() != harness.ToolRoutingTwoStage {
		t.Fatalf("empirical policies were not composed: response=%q edit=%q anchor=%q routing=%q",
			profile.ResponsePolicy(), profile.EditPolicy(), profile.PlanAnchorMode(), profile.ToolRouting())
	}
	if !resolved.Matched || !strings.HasPrefix(resolved.Label, "empirical:") {
		t.Fatalf("empirical resolution metadata = %+v", resolved)
	}
	configured, err := resolveRunHarnessWithCapability(
		provider,
		model,
		nil,
		config.Config{LocalToolBudget: 9},
		0,
		&selection,
		capabilityHarnessNow.Add(time.Hour),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := configured.Profile.ToolBudget(); got != 9 {
		t.Fatalf("tool budget = %d, want explicit config value 9", got)
	}
}

func TestResolveRunHarnessIgnoresIneligibleRuntimeSelectionAndExplicitProfileWins(t *testing.T) {
	const model = "qwen-coder:30b"
	selection := capabilityHarnessSelection(t, "ollama", model, 2*time.Hour)
	for _, test := range []struct {
		name         string
		providerName string
		now          time.Time
	}{
		{name: "provider mismatch", providerName: "remote", now: capabilityHarnessNow.Add(time.Hour)},
		{name: "future created", providerName: "ollama", now: capabilityHarnessNow.Add(-time.Second)},
		{name: "expired", providerName: "ollama", now: capabilityHarnessNow.Add(3 * time.Hour)},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := &policyTestProvider{name: test.providerName}
			resolved, err := resolveRunHarnessWithCapability(
				provider,
				model,
				nil,
				config.Config{},
				0,
				&selection,
				test.now,
			)
			if err != nil {
				t.Fatal(err)
			}
			if resolved.Profile.ReliableInputTokens() != 0 ||
				resolved.Profile.ToolRouting() != harness.ToolRoutingDirect ||
				strings.HasPrefix(resolved.Label, "empirical:") {
				t.Fatalf("ineligible selection changed fallback profile: %+v", resolved)
			}
		})
	}

	provider := &policyTestProvider{name: "remote"}
	explicit := harness.MustResolveProfile(harness.ProfileSpec{
		ID:                  "explicit-capability-test",
		ToolBudget:          3,
		ContextWindow:       40_000,
		OutputReserve:       4_000,
		ReliableInputTokens: 12_345,
		ToolRouting:         harness.ToolRoutingDirect,
	})
	beforeCalls := provider.modelCalls
	resolved, err := resolveRunHarnessWithCapability(
		provider,
		model,
		&explicit,
		config.Config{LocalToolBudget: 99},
		88,
		&selection,
		capabilityHarnessNow.Add(time.Hour),
	)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Profile.ID() != explicit.ID() || resolved.Profile.ReliableInputTokens() != 12_345 || resolved.Profile.ToolBudget() != 3 {
		t.Fatalf("explicit HarnessProfile did not win: %+v", resolved)
	}
	if provider.modelCalls != beforeCalls {
		t.Fatal("explicit HarnessProfile read lower-precedence provider metadata")
	}
}

func TestResolveRunHarnessEmpiricalRouterUsesDeterministicWhenTwoStageIsNotReliable(t *testing.T) {
	const model = "qwen-coder:30b"
	selection := capabilityHarnessSelectionWithExecutor(
		t,
		"ollama",
		model,
		48*time.Hour,
		capabilityHarnessExecutor{rejectTwoStage: true},
	)
	provider := &policyTestProvider{
		name: "ollama",
		models: []types.ModelInfo{{
			ID:            model,
			ContextWindow: 20_000,
			MaxOutput:     4_000,
		}},
	}
	resolved, err := resolveRunHarnessWithCapability(
		provider,
		model,
		nil,
		config.Config{},
		0,
		&selection,
		capabilityHarnessNow.Add(time.Hour),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := resolved.Profile.ToolRouting(); got != harness.ToolRoutingDeterministic {
		t.Fatalf("ToolRouting() = %q, want deterministic", got)
	}
}

type capabilityHarnessWorkspaceFactory struct{}

func (capabilityHarnessWorkspaceFactory) Acquire(_ context.Context, request capabilityprofile.WorkspaceRequest) (capabilityprofile.WorkspaceLease, error) {
	return capabilityHarnessLease{proof: capabilityprofile.IsolationProof{
		Ready:                true,
		WorkspaceDigest:      capabilityHarnessDigest(request.CaseID),
		OutboundTargetDigest: request.TargetDigest,
	}}, nil
}

type capabilityHarnessLease struct {
	proof capabilityprofile.IsolationProof
}

func (l capabilityHarnessLease) Root() string                            { return "isolated-capability-harness" }
func (l capabilityHarnessLease) Proof() capabilityprofile.IsolationProof { return l.proof }
func (capabilityHarnessLease) Close() error                              { return nil }

type capabilityHarnessExecutor struct {
	rejectTwoStage bool
}

func (e capabilityHarnessExecutor) Execute(_ context.Context, execution capabilityprofile.ProbeExecution) (capabilityprofile.ProbeObservation, error) {
	return capabilityprofile.ProbeObservation{
		SchemaVersion:  capabilityprofile.CurrentObservationSchemaVersion,
		Success:        !(e.rejectTwoStage && execution.Case.Category == capabilityprofile.CategoryTwoStageRouting),
		Latency:        time.Millisecond,
		ContextTokens:  execution.Case.ContextTokens,
		ToolCount:      execution.Case.ToolCount,
		SafetyPassed:   true,
		TraceDigest:    capabilityHarnessDigest("trace:" + execution.Case.ID),
		ArtifactDigest: capabilityHarnessDigest("artifact:" + execution.Case.ID),
	}, nil
}

func capabilityHarnessSelection(t *testing.T, provider, model string, ttl time.Duration) capabilityprofile.AutomaticSelection {
	return capabilityHarnessSelectionWithExecutor(t, provider, model, ttl, capabilityHarnessExecutor{})
}

func capabilityHarnessSelectionWithExecutor(
	t *testing.T,
	provider string,
	model string,
	ttl time.Duration,
	executor capabilityHarnessExecutor,
) capabilityprofile.AutomaticSelection {
	t.Helper()
	target, err := capabilityprofile.NewTargetIdentity(capabilityprofile.TargetSpec{
		Provider: provider,
		Model:    model,
		Endpoint: "https://private-capability-endpoint.invalid/v1",
		APIKey:   "sk-never-visible-in-harness",
		ServingParameters: map[string]any{
			"temperature": 0,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	profiler, err := capabilityprofile.NewProfiler(
		capabilityHarnessWorkspaceFactory{},
		executor,
		capabilityprofile.ProfilerConfig{
			ProfileTTL: ttl,
			Clock:      func() time.Time { return capabilityHarnessNow },
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := profiler.Run(context.Background(), target, capabilityprofile.DefaultProbePlan())
	if err != nil {
		t.Fatal(err)
	}
	if !profile.Verified() {
		t.Fatalf("profile quarantined: %v", profile.QuarantineReasons())
	}
	selection, err := capabilityprofile.NewAutomaticSelection(target, profile, capabilityHarnessNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return selection
}

func capabilityHarnessDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
