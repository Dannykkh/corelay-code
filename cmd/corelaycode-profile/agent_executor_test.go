package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/capabilityprofile"
	"github.com/Dannykkh/corelay-code/internal/harness"
	"github.com/Dannykkh/corelay-code/internal/types"
)

type profileTestProvider struct {
	mu            sync.Mutex
	name          string
	validateCalls int
	streamCalls   int
	marker        string
	validateErr   error
	toolName      string
	toolInput     string
}

func TestProbeDoneAllowsSuccessRejectsBlockedCompletion(t *testing.T) {
	for _, test := range []struct {
		name  string
		data  any
		allow bool
	}{
		{name: "legacy", data: nil, allow: true},
		{name: "verified", data: map[string]any{
			"terminalState":    agent.EvidenceTerminalVerified,
			"completionStatus": agent.CompletionStatusComplete,
		}, allow: true},
		{name: "incomplete", data: map[string]any{
			"terminalState":    agent.EvidenceTerminalVerified,
			"completionStatus": agent.CompletionStatusIncomplete,
		}, allow: false},
		{name: "blocked count", data: map[string]any{
			"terminalState":     agent.EvidenceTerminalVerified,
			"completionStatus":  agent.CompletionStatusComplete,
			"completionBlocked": 1,
		}, allow: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := probeDoneAllowsSuccess(test.data); got != test.allow {
				t.Fatalf("probeDoneAllowsSuccess() = %v, want %v", got, test.allow)
			}
		})
	}
}

func (p *profileTestProvider) Name() string        { return p.name }
func (p *profileTestProvider) DisplayName() string { return "Profile Test" }
func (p *profileTestProvider) Models() []types.ModelInfo {
	return []types.ModelInfo{{ID: "profile-model", ContextWindow: 64_000, MaxOutput: 4_096}}
}
func (p *profileTestProvider) Validate() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.validateCalls++
	return p.validateErr
}

func (p *profileTestProvider) StreamMessage(_ context.Context, req *types.MessagesRequest, _ *types.StreamOptions) (<-chan types.SSEEvent, error) {
	p.mu.Lock()
	p.streamCalls++
	call := p.streamCalls
	if call == 1 {
		p.marker = markerFromRequest(req)
	}
	marker := p.marker
	toolName := p.toolName
	toolInput := p.toolInput
	p.mu.Unlock()
	if call == 1 {
		if toolName == "" {
			toolName = "Read"
		}
		if toolInput == "" {
			toolInput = `{"file_path":"probe.txt"}`
		}
		return nativeToolEvents(toolName, toolInput), nil
	}
	return textEvents(marker), nil
}

func TestAgentProbeExecutorObservesActualSafetyDenialAndCanary(t *testing.T) {
	provider := &profileTestProvider{
		name:      "profile-test",
		toolName:  "Write",
		toolInput: `{"file_path":"../.corelay-profile-boundary-canary","content":"COMPROMISED"}`,
	}
	target, err := capabilityprofile.NewTargetIdentity(capabilityprofile.TargetSpec{
		Provider: provider.name, Model: "profile-model", Endpoint: "https://endpoint.invalid",
	})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := newAgentProbeExecutor(provider, "profile-model", target, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	plan := capabilityprofile.DefaultProbePlan()
	var probeCase capabilityprofile.ProbeCase
	for _, candidate := range plan.Cases() {
		if candidate.Category == capabilityprofile.CategorySafetyToolDenial {
			probeCase = candidate
			break
		}
	}
	container := t.TempDir()
	workspace := filepath.Join(container, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	request := capabilityprofile.WorkspaceRequest{
		TargetDigest: target.Digest(), PlanDigest: plan.Digest(),
		CaseID: probeCase.ID, Attempt: 1,
	}
	canaryPath := filepath.Join(container, capabilityprofile.RuntimeBoundaryCanaryName)
	if err := os.WriteFile(canaryPath, capabilityprofile.RuntimeBoundaryCanary(request), 0o600); err != nil {
		t.Fatal(err)
	}
	observation, err := executor.Execute(context.Background(), capabilityprofile.ProbeExecution{
		Target: target, PlanVersion: plan.Version(), PlanDigest: plan.Digest(),
		Variant: plan.Variant(), Case: probeCase, Attempt: 1, WorkspaceRoot: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !observation.Success || !observation.SafetyPassed || observation.ArtifactDigest == "" {
		t.Fatalf("observation = %+v", observation)
	}
	content, err := os.ReadFile(canaryPath)
	if err != nil || string(content) != string(capabilityprofile.RuntimeBoundaryCanary(request)) {
		t.Fatalf("canary=%q error=%v", content, err)
	}
}

func TestAgentProbeExecutorUsesRealAgentLoopAndReturnsBoundedEvidence(t *testing.T) {
	provider := &profileTestProvider{name: "profile-test"}
	target, err := capabilityprofile.NewTargetIdentity(capabilityprofile.TargetSpec{
		Provider: provider.name, Model: "profile-model", Endpoint: "https://endpoint.invalid",
		APIKey: "sk-not-retained",
	})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := newAgentProbeExecutor(provider, "profile-model", target, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	plan := capabilityprofile.DefaultProbePlan()
	probeCase := plan.Cases()[0]
	observation, err := executor.Execute(context.Background(), capabilityprofile.ProbeExecution{
		Target: target, PlanVersion: plan.Version(), PlanDigest: plan.Digest(),
		Variant: plan.Variant(), Case: probeCase, Attempt: 1, WorkspaceRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !observation.Success || observation.Malformed || observation.FalseDone ||
		observation.TraceDigest == "" || observation.SchemaVersion != capabilityprofile.CurrentObservationSchemaVersion {
		t.Fatalf("observation = %+v", observation)
	}
	if provider.validateCalls != 1 || provider.streamCalls != 2 {
		t.Fatalf("provider calls validate=%d stream=%d", provider.validateCalls, provider.streamCalls)
	}
	encoded, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"endpoint.invalid", "sk-not-retained", provider.marker} {
		if string(encoded) != "" && forbidden != "" && containsText(string(encoded), forbidden) {
			t.Fatalf("observation leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestAgentProbeExecutorMeasuresRepositoryMapThroughRealAgentLoop(t *testing.T) {
	provider := &profileTestProvider{
		name:      "profile-test",
		toolName:  "RepoMap",
		toolInput: `{"path":".","include_signatures":true,"max_files":10}`,
	}
	target, err := capabilityprofile.NewTargetIdentity(capabilityprofile.TargetSpec{
		Provider: provider.name, Model: "profile-model", Endpoint: "https://endpoint.invalid",
	})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := newAgentProbeExecutor(provider, "profile-model", target, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	plan := capabilityprofile.DefaultProbePlan()
	var probeCase capabilityprofile.ProbeCase
	for _, candidate := range plan.Cases() {
		if candidate.Category == capabilityprofile.CategoryRepositoryMap {
			probeCase = candidate
			break
		}
	}
	observation, err := executor.Execute(context.Background(), capabilityprofile.ProbeExecution{
		Target: target, PlanVersion: plan.Version(), PlanDigest: plan.Digest(),
		Variant: plan.Variant(), Case: probeCase, Attempt: 1, WorkspaceRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !observation.Success || observation.Malformed || observation.FalseDone {
		t.Fatalf("repository-map observation = %+v", observation)
	}
}

func TestAgentProbeExecutorBuildsMinimalAblationWithoutWeakeningKernelSafety(t *testing.T) {
	provider := &profileTestProvider{name: "profile-test"}
	target, err := capabilityprofile.NewTargetIdentity(capabilityprofile.TargetSpec{
		Provider: provider.name, Model: "profile-model", Endpoint: "https://endpoint.invalid",
	})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := newAgentProbeExecutor(provider, "profile-model", target, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := capabilityprofile.ProbePlanForVariant(capabilityprofile.HarnessVariantMinimal)
	if err != nil {
		t.Fatal(err)
	}
	var probeCase capabilityprofile.ProbeCase
	for _, candidate := range plan.Cases() {
		if candidate.Category == capabilityprofile.CategoryPlanAnchor {
			probeCase = candidate
			break
		}
	}
	execution := capabilityprofile.ProbeExecution{
		Target: target, PlanVersion: plan.Version(), PlanDigest: plan.Digest(), Variant: plan.Variant(),
		Case: probeCase, Attempt: 1, WorkspaceRoot: t.TempDir(),
	}
	fixture := agentProbeFixture{marker: "CORELAY_PROBE_TEST"}
	profile, anchor, err := executor.harnessFor(execution, fixture)
	if err != nil {
		t.Fatal(err)
	}
	if anchor != nil || profile.PlanAnchorMode() != harness.PlanAnchorOff ||
		profile.ResponsePolicy() != harness.ResponseNative || profile.EditPolicy() != harness.EditExact ||
		profile.ToolRouting() != harness.ToolRoutingDirect || !profile.ReadBeforeWrite() {
		t.Fatalf("minimal variant escaped its bounded ablation contract: profile=%+v anchor=%+v", profile, anchor)
	}
}

func TestReferenceFormatCasesBindFixturesToExpectedParsers(t *testing.T) {
	plan := capabilityprofile.DefaultProbePlan()
	want := map[capabilityprofile.ProbeCategory]string{
		capabilityprofile.CategoryFormatCodeblock: string(agent.ToolCallFormatCodeblock),
		capabilityprofile.CategoryFormatTokenized: string(agent.ToolCallFormatTokenized),
	}
	seen := make(map[capabilityprofile.ProbeCategory]bool)
	for _, probeCase := range plan.Cases() {
		expected, ok := want[probeCase.Category]
		if !ok {
			continue
		}
		seen[probeCase.Category] = true
		if got := expectedToolFormat(probeCase.Category); got != expected {
			t.Fatalf("category %q format=%q want=%q", probeCase.Category, got, expected)
		}
		fixture, err := prepareAgentProbeFixture(capabilityprofile.ProbeExecution{
			PlanDigest: plan.Digest(), Variant: plan.Variant(), Case: probeCase,
			Attempt: 1, WorkspaceRoot: t.TempDir(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if !containsText(fixture.prompt, "probe.txt") || fixture.marker == "" {
			t.Fatalf("category %q fixture is incomplete", probeCase.Category)
		}
	}
	for category := range want {
		if !seen[category] {
			t.Fatalf("missing reference format case %q", category)
		}
	}
}

func markerFromRequest(req *types.MessagesRequest) string {
	if req == nil || len(req.Messages) == 0 {
		return ""
	}
	var prompt string
	_ = json.Unmarshal(req.Messages[0].Content, &prompt)
	const prefix = "CORELAY_PROBE_"
	index := containsIndex(prompt, prefix)
	if index < 0 {
		return ""
	}
	end := index
	for end < len(prompt) {
		value := prompt[end]
		if !((value >= 'A' && value <= 'Z') || (value >= '0' && value <= '9') || value == '_') {
			break
		}
		end++
	}
	return prompt[index:end]
}

func nativeToolEvents(name, input string) <-chan types.SSEEvent {
	events := make(chan types.SSEEvent, 5)
	block, _ := json.Marshal(map[string]string{"type": "tool_use", "id": "probe-tool", "name": name})
	delta, _ := json.Marshal(map[string]string{"type": "input_json_delta", "partial_json": input})
	events <- types.SSEEvent{Type: "content_block_start", ContentBlock: block}
	events <- types.SSEEvent{Type: "content_block_delta", Delta: delta}
	events <- types.SSEEvent{Type: "content_block_stop"}
	events <- types.SSEEvent{Type: "message_delta", Delta: json.RawMessage(`{"stop_reason":"tool_use"}`)}
	events <- types.SSEEvent{Type: "message_stop"}
	close(events)
	return events
}

func textEvents(marker string) <-chan types.SSEEvent {
	events := make(chan types.SSEEvent, 5)
	events <- types.SSEEvent{Type: "content_block_start", ContentBlock: json.RawMessage(`{"type":"text"}`)}
	events <- types.SSEEvent{Type: "content_block_delta", Delta: json.RawMessage(fmt.Sprintf(`{"type":"text_delta","text":%q}`, marker))}
	events <- types.SSEEvent{Type: "content_block_stop"}
	events <- types.SSEEvent{Type: "message_delta", Delta: json.RawMessage(`{"stop_reason":"end_turn"}`)}
	events <- types.SSEEvent{Type: "message_stop"}
	close(events)
	return events
}

func containsText(value, needle string) bool { return containsIndex(value, needle) >= 0 }

func containsIndex(value, needle string) int {
	if needle == "" {
		return 0
	}
	for index := 0; index+len(needle) <= len(value); index++ {
		if value[index:index+len(needle)] == needle {
			return index
		}
	}
	return -1
}
