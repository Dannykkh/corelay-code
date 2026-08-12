package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/capabilityprofile"
	"github.com/Dannykkh/corelay-code/internal/config"
	"github.com/Dannykkh/corelay-code/internal/harness"
	"github.com/Dannykkh/corelay-code/internal/providers"
	"github.com/Dannykkh/corelay-code/internal/types"
	"github.com/Dannykkh/corelay-code/internal/workstream"
)

var serverCapabilityNow = time.Date(2026, 8, 12, 8, 0, 0, 0, time.UTC)

func TestSelectAutomaticCapabilityProfileUsesExactLiveTargetWithoutPersistingSecrets(t *testing.T) {
	base := t.TempDir()
	t.Setenv("CORELAY_CONFIG_DIR", base)
	const (
		providerName = "openai-private"
		model        = "small-code-model"
		endpoint     = "https://private-runtime.invalid/tenant/alpha"
		apiKey       = "sk-never-persist-capability-key"
	)
	profileID := persistServerCapabilityProfile(t, providerName, model, endpoint, apiKey, 48*time.Hour, false)
	provider := &providers.OpenAICompat{
		ProviderName: providerName,
		ProviderDisp: "private runtime",
		BaseURL:      endpoint,
	}

	selection := selectAutomaticCapabilityProfile(provider, model, serverCapabilityNow.Add(time.Hour))
	if selection == nil {
		t.Fatal("verified exact-target profile was not selected")
	}
	recommendation, selectedID, ok := selection.RecommendationsFor(providerName, model, serverCapabilityNow.Add(time.Hour))
	if !ok || selectedID != profileID || recommendation.ReliableInputTokens == 0 {
		t.Fatalf("selection = (%+v, %q, %v), want profile %q", recommendation, selectedID, ok, profileID)
	}

	err := filepath.WalkDir(config.CapabilityProfileDir(), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, secret := range []string{endpoint, apiKey} {
			if strings.Contains(string(data), secret) {
				t.Fatalf("capability store leaked raw target material %q", secret)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSelectAutomaticCapabilityProfileFallsBackForIneligibleOrInexactEvidence(t *testing.T) {
	for _, test := range []struct {
		name        string
		persist     bool
		quarantine  bool
		profileTTL  time.Duration
		providerURL string
		model       string
		now         time.Time
	}{
		{
			name: "no profile", providerURL: "https://runtime.invalid/v1",
			model: "model", now: serverCapabilityNow.Add(time.Hour),
		},
		{
			name: "expired", persist: true, profileTTL: 2 * time.Hour,
			providerURL: "https://runtime.invalid/v1", model: "model", now: serverCapabilityNow.Add(3 * time.Hour),
		},
		{
			name: "quarantined", persist: true, quarantine: true, profileTTL: 48 * time.Hour,
			providerURL: "https://runtime.invalid/v1", model: "model", now: serverCapabilityNow.Add(time.Hour),
		},
		{
			name: "endpoint mismatch", persist: true, profileTTL: 48 * time.Hour,
			providerURL: "https://different.invalid/v1", model: "model", now: serverCapabilityNow.Add(time.Hour),
		},
		{
			name: "model mismatch", persist: true, profileTTL: 48 * time.Hour,
			providerURL: "https://runtime.invalid/v1", model: "other-model", now: serverCapabilityNow.Add(time.Hour),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("CORELAY_CONFIG_DIR", t.TempDir())
			if test.persist {
				persistServerCapabilityProfile(
					t,
					"openai-test",
					"model",
					"https://runtime.invalid/v1",
					"secret-key-not-stored",
					test.profileTTL,
					test.quarantine,
				)
			}
			provider := &providers.OpenAICompat{ProviderName: "openai-test", BaseURL: test.providerURL}
			if selected := selectAutomaticCapabilityProfile(provider, test.model, test.now); selected != nil {
				t.Fatal("ineligible or inexact profile changed the legacy fallback")
			}
		})
	}
}

func TestSelectAutomaticCapabilityProfileFailsClosedForOpaqueEndpoint(t *testing.T) {
	t.Setenv("CORELAY_CONFIG_DIR", t.TempDir())
	provider := serverOpaqueCapabilityProvider{name: "opaque"}
	if selected := selectAutomaticCapabilityProfile(provider, "model", serverCapabilityNow); selected != nil {
		t.Fatal("provider with an unverifiable endpoint received empirical policy")
	}
	if entries, err := os.ReadDir(config.CapabilityProfileDir()); err == nil || len(entries) != 0 {
		t.Fatal("opaque provider should not initialize or read the capability store")
	}
}

func TestCapabilityPlanAnchorUsesWorkstreamThenPlainUserObjective(t *testing.T) {
	messages := []types.Message{{Role: "user", Content: jsonRawString(t, "implement exact capability selection")}}
	anchor := capabilityPlanAnchor(messages, nil)
	if anchor == nil || anchor.Objective() != "implement exact capability selection" {
		t.Fatalf("plain-message anchor = %#v", anchor)
	}
	if rendered, err := anchor.Render(harness.PlanAnchorCompact); err != nil || !strings.Contains(rendered, "available verification evidence") {
		t.Fatalf("fallback anchor render = %q, %v", rendered, err)
	}

	ws := &workstream.Workstream{
		NextAction: "run focused tests",
		Goal: workstream.Goal{
			Objective:        "ship the verified composition root",
			DefinitionOfDone: []string{"full tests pass"},
		},
	}
	anchor = capabilityPlanAnchor(messages, ws)
	if anchor == nil || anchor.Objective() != ws.Goal.Objective || anchor.CurrentStep() != ws.NextAction {
		t.Fatalf("workstream anchor = %#v", anchor)
	}
	if got := anchor.DefinitionOfDone(); len(got) != 1 || got[0] != "full tests pass" {
		t.Fatalf("DefinitionOfDone() = %v", got)
	}
	if capabilityPlanAnchor([]types.Message{{Role: "user", Content: []byte(`[]`)}}, nil) != nil {
		t.Fatal("tool-result-only messages produced an invented objective")
	}
}

func TestAgentCompositionInjectsSelectedCapabilityProfileAndRequiredPlanAnchor(t *testing.T) {
	t.Setenv("CORELAY_CONFIG_DIR", t.TempDir())
	t.Setenv("CORELAY_MEMORY", "off")
	t.Setenv("CORELAY_AUTOSKILL", "off")
	t.Setenv("CORELAY_AUTOVERIFY", "off")
	const (
		providerName = "capability-run-provider"
		model        = "capability-run-model"
		endpoint     = "https://capability-run.invalid/v1"
	)
	persistServerCapabilityProfileAt(
		t,
		providerName,
		model,
		endpoint,
		"secret-not-retained",
		time.Now().UTC().Add(-time.Hour),
		48*time.Hour,
		false,
	)
	provider := &serverCapabilityRunProvider{name: providerName, endpoint: endpoint}
	server := New(provider, model, 0)
	server.SetWorkDir(t.TempDir())
	body := []byte(`{"messages":[{"role":"user","content":"verify empirical composition"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/agent", bytes.NewReader(body))
	recorder := httptest.NewRecorder()

	server.handleAgentLoop(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", provider.calls)
	}
	for _, fragment := range []string{
		"<plan-anchor",
		"Objective: verify empirical composition",
		"Definition of Done:",
	} {
		if !strings.Contains(provider.systemPrompt, fragment) {
			t.Fatalf("empirical run prompt missing %q:\n%s", fragment, provider.systemPrompt)
		}
	}
	if provider.toolCount != 1 {
		t.Fatalf("empirical two-stage selector exposed %d tools, want 1", provider.toolCount)
	}
	if !strings.Contains(recorder.Body.String(), `"type":"done"`) {
		t.Fatalf("agent SSE did not complete: %s", recorder.Body.String())
	}
}

func TestChronosAndSubAgentCompositionInjectSelectedCapabilityProfile(t *testing.T) {
	t.Setenv("CORELAY_CONFIG_DIR", t.TempDir())
	const (
		providerName = "capability-worker-provider"
		model        = "capability-worker-model"
		endpoint     = "https://capability-worker.invalid/v1"
	)
	persistServerCapabilityProfileAt(
		t,
		providerName,
		model,
		endpoint,
		"secret-not-retained",
		time.Now().UTC().Add(-time.Hour),
		48*time.Hour,
		false,
	)

	t.Run("chronos", func(t *testing.T) {
		provider := &serverCapabilityRunProvider{
			name:         providerName,
			endpoint:     endpoint,
			responseText: "[COMPLETE]",
		}
		server := New(provider, model, 0)
		server.SetWorkDir(t.TempDir())
		req := httptest.NewRequest(
			http.MethodPost,
			"/api/chronos",
			bytes.NewReader([]byte(`{"task":"verify chronos composition","maxCycles":1}`)),
		)
		recorder := httptest.NewRecorder()

		server.handleChronos(recorder, req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
		}
		if provider.calls != 1 || provider.toolCount != 1 {
			t.Fatalf("chronos provider calls/tools = %d/%d, want 1/1", provider.calls, provider.toolCount)
		}
	})

	t.Run("sub-agent", func(t *testing.T) {
		previousManager := subAgentMgr
		subAgentMgr = nil
		t.Cleanup(func() { subAgentMgr = previousManager })

		provider := &serverCapabilityRunProvider{name: providerName, endpoint: endpoint}
		server := New(provider, model, 0)
		workDir := t.TempDir()
		body, err := json.Marshal(map[string]any{
			"workDir": workDir,
			"tasks": []map[string]any{{
				"name":        "profile-test",
				"instruction": "report completion without editing",
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/subagents/spawn", bytes.NewReader(body))
		recorder := httptest.NewRecorder()

		server.handleSubAgentSpawn(recorder, req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
		}
		if subAgentMgr == nil {
			t.Fatal("sub-agent manager was not created")
		}
		subAgentMgr.Wait(2 * time.Second)
		if provider.calls != 1 || provider.toolCount != 1 {
			t.Fatalf("sub-agent provider calls/tools = %d/%d, want 1/1", provider.calls, provider.toolCount)
		}
		tasks := subAgentMgr.GetTasks()
		if len(tasks) != 1 || tasks[0].Status != "completed" {
			t.Fatalf("sub-agent tasks = %#v", tasks)
		}
	})
}

type serverCapabilityWorkspaceFactory struct{}

func (serverCapabilityWorkspaceFactory) Acquire(_ context.Context, request capabilityprofile.WorkspaceRequest) (capabilityprofile.WorkspaceLease, error) {
	return serverCapabilityLease{proof: capabilityprofile.IsolationProof{
		Ready:                true,
		WorkspaceDigest:      serverCapabilityDigest(request.CaseID),
		OutboundTargetDigest: request.TargetDigest,
	}}, nil
}

type serverCapabilityLease struct {
	proof capabilityprofile.IsolationProof
}

func (l serverCapabilityLease) Root() string                            { return "isolated-server-capability" }
func (l serverCapabilityLease) Proof() capabilityprofile.IsolationProof { return l.proof }
func (serverCapabilityLease) Close() error                              { return nil }

type serverCapabilityExecutor struct {
	quarantine bool
}

func (e serverCapabilityExecutor) Execute(_ context.Context, execution capabilityprofile.ProbeExecution) (capabilityprofile.ProbeObservation, error) {
	success := !(e.quarantine && execution.Case.Stage == capabilityprofile.StageHoldout)
	return capabilityprofile.ProbeObservation{
		SchemaVersion:  capabilityprofile.CurrentObservationSchemaVersion,
		Success:        success,
		Latency:        time.Millisecond,
		ContextTokens:  execution.Case.ContextTokens,
		ToolCount:      execution.Case.ToolCount,
		SafetyPassed:   success || !execution.Case.SafetyCritical,
		TraceDigest:    serverCapabilityDigest("trace:" + execution.Case.ID),
		ArtifactDigest: serverCapabilityDigest("artifact:" + execution.Case.ID),
	}, nil
}

func persistServerCapabilityProfile(
	t *testing.T,
	provider string,
	model string,
	endpoint string,
	apiKey string,
	ttl time.Duration,
	quarantine bool,
) string {
	return persistServerCapabilityProfileAt(t, provider, model, endpoint, apiKey, serverCapabilityNow, ttl, quarantine)
}

func persistServerCapabilityProfileAt(
	t *testing.T,
	provider string,
	model string,
	endpoint string,
	apiKey string,
	createdAt time.Time,
	ttl time.Duration,
	quarantine bool,
) string {
	t.Helper()
	target, err := capabilityprofile.NewTargetIdentity(capabilityprofile.TargetSpec{
		Provider: provider,
		Model:    model,
		Endpoint: endpoint,
		APIKey:   apiKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	profiler, err := capabilityprofile.NewProfiler(
		serverCapabilityWorkspaceFactory{},
		serverCapabilityExecutor{quarantine: quarantine},
		capabilityprofile.ProfilerConfig{
			ProfileTTL: ttl,
			Clock:      func() time.Time { return createdAt },
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := profiler.Run(context.Background(), target, capabilityprofile.DefaultProbePlan())
	if err != nil {
		t.Fatal(err)
	}
	store, err := capabilityprofile.NewStore(config.CapabilityProfileDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(profile); err != nil {
		t.Fatal(err)
	}
	return profile.ID()
}

func serverCapabilityDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func jsonRawString(t *testing.T, value string) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

type serverOpaqueCapabilityProvider struct {
	name string
}

func (p serverOpaqueCapabilityProvider) Name() string            { return p.name }
func (p serverOpaqueCapabilityProvider) DisplayName() string     { return p.name }
func (serverOpaqueCapabilityProvider) Models() []types.ModelInfo { return nil }
func (serverOpaqueCapabilityProvider) Validate() error           { return nil }
func (serverOpaqueCapabilityProvider) StreamMessage(context.Context, *types.MessagesRequest, *types.StreamOptions) (<-chan types.SSEEvent, error) {
	return nil, nil
}

type serverCapabilityRunProvider struct {
	name         string
	endpoint     string
	responseText string
	calls        int
	systemPrompt string
	toolCount    int
}

func (p *serverCapabilityRunProvider) Name() string                      { return p.name }
func (p *serverCapabilityRunProvider) DisplayName() string               { return p.name }
func (*serverCapabilityRunProvider) Models() []types.ModelInfo           { return nil }
func (*serverCapabilityRunProvider) Validate() error                     { return nil }
func (p *serverCapabilityRunProvider) CapabilityProfileEndpoint() string { return p.endpoint }
func (p *serverCapabilityRunProvider) StreamMessage(_ context.Context, request *types.MessagesRequest, _ *types.StreamOptions) (<-chan types.SSEEvent, error) {
	p.calls++
	var systemBlocks []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(request.System, &systemBlocks); err == nil && len(systemBlocks) > 0 {
		p.systemPrompt = systemBlocks[0].Text
	} else {
		p.systemPrompt = string(request.System)
	}
	p.toolCount = len(request.Tools)
	responseText := p.responseText
	if responseText == "" {
		responseText = "done"
	}
	events := make(chan types.SSEEvent, 4)
	go func() {
		defer close(events)
		events <- types.SSEEvent{Type: "content_block_start", ContentBlock: jsonRawObject(map[string]string{"type": "text"})}
		events <- types.SSEEvent{Type: "content_block_delta", Delta: jsonRawObject(map[string]string{"type": "text_delta", "text": responseText})}
		events <- types.SSEEvent{Type: "content_block_stop"}
		events <- types.SSEEvent{Type: "message_stop"}
	}()
	return events, nil
}

func jsonRawObject(value any) []byte {
	encoded, _ := json.Marshal(value)
	return encoded
}
