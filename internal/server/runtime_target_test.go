package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aniclew/aniclew/internal/config"
	"github.com/aniclew/aniclew/internal/providers"
	"github.com/aniclew/aniclew/internal/router"
	"github.com/aniclew/aniclew/internal/runtimeplane"
	"github.com/aniclew/aniclew/internal/types"
)

func TestResolveDirectRuntimeProviderSwitchesByRequestedModel(t *testing.T) {
	isolateConfigHome(t)
	cfg := config.DefaultConfig()
	cfg.Providers = map[string]config.ProviderSettings{
		"openai": {APIKey: "configured-openai-key"},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}

	provider, model, target, selection, err := resolveDirectRuntimeProvider(
		runtimeStatusTestProvider{name: "anthropic", displayName: "Anthropic"},
		"claude-sonnet-4-20250514",
		"gpt-5.5",
		nil,
		nil,
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	if provider == nil || provider.Name() != "openai" {
		t.Fatalf("expected openai provider, got %#v", provider)
	}
	if model != "gpt-5.5" || target.Group != runtimeplane.GroupCodex {
		t.Fatalf("unexpected runtime target: model=%q target=%+v", model, target)
	}
	if selection.AccountID != "provider:openai" {
		t.Fatalf("expected scheduler to select configured openai account, got %+v", selection)
	}

	openAI, ok := provider.(*providers.OpenAICompat)
	if !ok {
		t.Fatalf("expected OpenAI-compatible provider, got %T", provider)
	}
	header, value := openAI.AuthHeader()
	if header != "Authorization" || value != "Bearer configured-openai-key" {
		t.Fatalf("configured OpenAI key was not used, got %q %q", header, value)
	}
}

func TestResolveDirectRuntimeProviderPreservesUnknownModelOnActiveProvider(t *testing.T) {
	provider, model, target, _, err := resolveDirectRuntimeProvider(
		runtimeStatusTestProvider{name: "anthropic", displayName: "Anthropic"},
		"claude-sonnet-4-20250514",
		"custom-model",
		nil,
		nil,
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	if provider == nil || provider.Name() != "anthropic" {
		t.Fatalf("expected active provider, got %#v", provider)
	}
	if model != "custom-model" || target.Group != runtimeplane.GroupClaude {
		t.Fatalf("unexpected runtime target: model=%q target=%+v", model, target)
	}
}

func TestSelectDirectRuntimeProviderNameCanUseConfiguredGroupAccount(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Providers = map[string]config.ProviderSettings{
		"openai-work": {APIKey: "configured-openai-work-key", BaseURL: "http://127.0.0.1:65534"},
	}

	providerName, selection := selectDirectRuntimeProviderName(
		cfg,
		runtimeplane.RuntimeTarget{Provider: "openai", Model: "gpt-5.5", Group: runtimeplane.GroupCodex},
		"anthropic",
		"Anthropic",
		testRuntimeNow(),
		nil,
		nil,
		"",
	)

	if providerName != "openai-work" {
		t.Fatalf("expected configured codex account, got provider=%q selection=%+v", providerName, selection)
	}
	if selection.Action != runtimeplane.DecisionSwitch || selection.AccountID != "provider:openai-work" {
		t.Fatalf("unexpected scheduler selection: %+v", selection)
	}
}

func TestSelectDirectRuntimeProviderNameUsesSessionLeaseAsCurrentAccount(t *testing.T) {
	now := testRuntimeNow()
	cfg := config.DefaultConfig()
	cfg.Providers = map[string]config.ProviderSettings{
		"openai":      {APIKey: "configured-openai-key"},
		"openai-work": {APIKey: "configured-openai-work-key", BaseURL: "http://127.0.0.1:65534"},
	}
	leases := runtimeplane.NewLeaseStore()
	leases.Upsert("session-1", "", runtimeplane.AccountState{
		ID:       "provider:openai-work",
		Provider: "openai-work",
		Group:    runtimeplane.GroupCodex,
		Status:   runtimeplane.AccountHealthy,
	}, "gpt-5.5", now, time.Hour)

	providerName, selection := selectDirectRuntimeProviderName(
		cfg,
		runtimeplane.RuntimeTarget{Provider: "openai", Model: "gpt-5.5", Group: runtimeplane.GroupCodex},
		"anthropic",
		"Anthropic",
		now,
		nil,
		leases,
		"session-1",
	)

	if providerName != "openai-work" || selection.AccountID != "provider:openai-work" || selection.Action != runtimeplane.DecisionStay {
		t.Fatalf("expected lease to keep openai-work, provider=%q selection=%+v", providerName, selection)
	}
}

func TestSelectDirectRuntimeProviderNameLeaseFallsThroughWhenIneligible(t *testing.T) {
	now := testRuntimeNow()
	cfg := config.DefaultConfig()
	cfg.Providers = map[string]config.ProviderSettings{
		"openai":      {APIKey: "configured-openai-key"},
		"openai-work": {APIKey: "configured-openai-work-key", BaseURL: "http://127.0.0.1:65534"},
	}
	leases := runtimeplane.NewLeaseStore()
	leases.Upsert("session-1", "", runtimeplane.AccountState{
		ID:       "provider:openai-work",
		Provider: "openai-work",
		Group:    runtimeplane.GroupCodex,
		Status:   runtimeplane.AccountHealthy,
	}, "gpt-5.5", now, time.Hour)
	telemetry := runtimeplane.NewTelemetryStore()
	telemetry.RecordFailure("openai-work", runtimeplane.GroupCodex, 429, now)

	providerName, selection := selectDirectRuntimeProviderName(
		cfg,
		runtimeplane.RuntimeTarget{Provider: "openai", Model: "gpt-5.5", Group: runtimeplane.GroupCodex},
		"anthropic",
		"Anthropic",
		now,
		telemetry,
		leases,
		"session-1",
	)

	if providerName != "openai" || selection.Action != runtimeplane.DecisionSwitch || selection.AccountID != "provider:openai" {
		t.Fatalf("cooled-down leased account should fail over, provider=%q selection=%+v", providerName, selection)
	}
}

func TestSelectDirectRuntimeProviderNameAgedLeaseRunsScheduler(t *testing.T) {
	now := testRuntimeNow()
	cfg := config.DefaultConfig()
	cfg.Providers = map[string]config.ProviderSettings{
		"openai":      {APIKey: "configured-openai-key"},
		"openai-work": {APIKey: "configured-openai-work-key", BaseURL: "http://127.0.0.1:65534"},
	}
	leases := runtimeplane.NewLeaseStore()
	leaseTime := now.Add(-2 * runtimeplane.DefaultLeaseReevalInterval)
	leases.Upsert("session-1", "", runtimeplane.AccountState{
		ID:       "provider:openai-work",
		Provider: "openai-work",
		Group:    runtimeplane.GroupCodex,
		Status:   runtimeplane.AccountHealthy,
	}, "gpt-5.5", leaseTime, time.Hour)

	providerName, selection := selectDirectRuntimeProviderName(
		cfg,
		runtimeplane.RuntimeTarget{Provider: "openai", Model: "gpt-5.5", Group: runtimeplane.GroupCodex},
		"anthropic",
		"Anthropic",
		now,
		nil,
		leases,
		"session-1",
	)

	if providerName != "openai-work" || selection.AccountID != "provider:openai-work" {
		t.Fatalf("scheduler should keep sticky leased account, provider=%q selection=%+v", providerName, selection)
	}
	lease, ok := leases.Current("session-1", runtimeplane.GroupCodex, now)
	if !ok || !lease.LastReevalAt.Equal(now) {
		t.Fatalf("aged lease should be refreshed by a full scheduler pass, ok=%v lease=%+v", ok, lease)
	}
}

func TestExplicitModelOverridesRouter(t *testing.T) {
	if !explicitModelOverridesRouter("gpt-5.5") {
		t.Fatal("expected recognized Codex model to override router")
	}
	if !explicitModelOverridesRouter("claude-sonnet-4-20250514") {
		t.Fatal("expected recognized Claude model to override router")
	}
	if explicitModelOverridesRouter("custom-model") {
		t.Fatal("unknown custom model should leave router policy in charge")
	}
	if explicitModelOverridesRouter("") {
		t.Fatal("omitted model should leave router policy in charge")
	}
}

func TestHandleMessagesExplicitModelOverridesEnabledRouter(t *testing.T) {
	isolateConfigHome(t)

	var upstreamModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body types.OAIChatRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		upstreamModel = body.Model
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"total_tokens\":18}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	cfg := config.DefaultConfig()
	cfg.Providers = map[string]config.ProviderSettings{
		"openai": {APIKey: "configured-openai-key", BaseURL: upstream.URL},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}

	s := New(
		runtimeStatusTestProvider{name: "anthropic", displayName: "Anthropic"},
		"claude-sonnet-4-20250514",
		0,
	)
	s.SetRouter(router.New(nil, nil))

	requestBody := []byte(`{"model":"gpt-5.5","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(requestBody))
	req.Header.Set("X-Claude-Code-Session-Id", "session-router-1")
	rec := httptest.NewRecorder()

	s.handleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if upstreamModel != "gpt-5.5" {
		t.Fatalf("router overrode explicit model, upstream model=%q", upstreamModel)
	}
	if count := s.runtimeLeases.Count(time.Now().UTC()); count != 1 {
		t.Fatalf("expected one runtime lease, got %d", count)
	}
}

func TestHandleMessagesDirectPathRoutesGPTModelToOpenAICompatibleProvider(t *testing.T) {
	isolateConfigHome(t)

	var upstreamPath string
	var upstreamAuth string
	var upstreamModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		upstreamAuth = r.Header.Get("Authorization")

		var body types.OAIChatRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		upstreamModel = body.Model

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Ratelimit-Limit-Tokens", "1000")
		w.Header().Set("X-Ratelimit-Remaining-Tokens", "900")
		w.Header().Set("X-Ratelimit-Reset-Tokens", "6m0s")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"total_tokens\":18}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	cfg := config.DefaultConfig()
	cfg.Providers = map[string]config.ProviderSettings{
		"openai": {APIKey: "configured-openai-key", BaseURL: upstream.URL},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}

	s := New(
		runtimeStatusTestProvider{name: "anthropic", displayName: "Anthropic"},
		"claude-sonnet-4-20250514",
		0,
	)
	requestBody := []byte(`{"model":"gpt-5.5","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(requestBody))
	req.Header.Set("X-Claude-Code-Session-Id", "session-direct-1")
	rec := httptest.NewRecorder()

	s.handleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if upstreamPath != "/v1/chat/completions" {
		t.Fatalf("unexpected upstream path %q", upstreamPath)
	}
	if upstreamAuth != "Bearer configured-openai-key" {
		t.Fatalf("configured OpenAI auth was not forwarded, got %q", upstreamAuth)
	}
	if upstreamModel != "gpt-5.5" {
		t.Fatalf("unexpected upstream model %q", upstreamModel)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"model":"gpt-5.5"`)) {
		t.Fatalf("response did not preserve translated model: %s", rec.Body.String())
	}
	accounts := s.runtimeTelemetry.Merge(nil, time.Now().UTC())
	if len(accounts) != 1 || accounts[0].ID != "provider:openai" || accounts[0].SevenDay.Used != 7 {
		t.Fatalf("runtime telemetry did not record OpenAI usage: %+v", accounts)
	}
	if accounts[0].RateLimit.Limit != 1000 || accounts[0].RateLimit.Used != 100 {
		t.Fatalf("runtime telemetry did not record OpenAI rate headers: %+v", accounts)
	}
	lease, ok := s.runtimeLeases.Current("session-direct-1", runtimeplane.GroupCodex, time.Now().UTC())
	if !ok || lease.AccountID != "provider:openai" {
		t.Fatalf("runtime lease not recorded: ok=%v lease=%+v", ok, lease)
	}
}

func testRuntimeNow() time.Time {
	return time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)
}
