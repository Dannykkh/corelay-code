package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aniclew/aniclew/internal/config"
	"github.com/aniclew/aniclew/internal/runtimeplane"
	"github.com/aniclew/aniclew/internal/types"
)

func TestHandleRuntimeStatusReportsActiveTargetAndScheduler(t *testing.T) {
	isolateConfigHome(t)
	cfg := config.DefaultConfig()
	cfg.Providers = map[string]config.ProviderSettings{
		"anthropic": {APIKey: "secret-runtime-key"},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}

	s := New(runtimeStatusTestProvider{
		name:        "anthropic",
		displayName: "Anthropic",
		models: []types.ModelInfo{{
			ID:            "claude-sonnet-4-20250514",
			DisplayName:   "Claude Sonnet 4",
			ContextWindow: 200000,
		}},
	}, "claude-sonnet-4-20250514", 0)

	req := httptest.NewRequest(http.MethodGet, "/api/runtime", nil)
	rec := httptest.NewRecorder()
	s.handleRuntimeStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret-runtime-key") || strings.Contains(rec.Body.String(), "apiKey") {
		t.Fatalf("runtime status leaked provider secret: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "0001-01-01") {
		t.Fatalf("runtime status leaked zero time values: %s", rec.Body.String())
	}

	var got runtimeStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("runtime status json: %v", err)
	}
	if got.Active.Provider != "anthropic" || got.Active.Model != "claude-sonnet-4-20250514" {
		t.Fatalf("unexpected active target: %+v", got.Active)
	}
	if got.Active.ModelGroup != runtimeplane.GroupClaude || got.Active.SelectionGroup != runtimeplane.GroupClaude {
		t.Fatalf("unexpected active groups: %+v", got.Active)
	}
	if got.Scheduler.FiveHourMax <= 0 || got.Scheduler.SevenDayMax <= 0 || got.Scheduler.StaleAfterSeconds <= 0 {
		t.Fatalf("scheduler defaults missing: %+v", got.Scheduler)
	}
	if len(got.Accounts) != 1 || got.Accounts[0].ID != "provider:anthropic" || got.Accounts[0].Group != runtimeplane.GroupClaude {
		t.Fatalf("unexpected runtime accounts: %+v", got.Accounts)
	}
	if got.Selection.AccountID != "provider:anthropic" || got.Selection.Action != runtimeplane.DecisionStay {
		t.Fatalf("unexpected account selection: %+v", got.Selection)
	}
	if len(got.Providers) == 0 {
		t.Fatalf("provider inventory missing")
	}
}

func TestHandleRuntimeTelemetryPushesQuotaSnapshot(t *testing.T) {
	isolateConfigHome(t)
	cfg := config.DefaultConfig()
	cfg.Providers = map[string]config.ProviderSettings{
		"anthropic": {},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}

	s := New(runtimeStatusTestProvider{name: "anthropic", displayName: "Anthropic"}, "claude-sonnet-4-20250514", 0)
	body := []byte(`{"provider":"anthropic","group":"claude","fiveHour":{"used":80,"limit":100},"sevenDay":{"used":200,"limit":1000}}`)
	pushReq := httptest.NewRequest(http.MethodPost, "/api/runtime/telemetry", bytes.NewReader(body))
	pushRec := httptest.NewRecorder()
	s.handleRuntimeTelemetry(pushRec, pushReq)
	if pushRec.Code != http.StatusOK {
		t.Fatalf("push status=%d body=%s", pushRec.Code, pushRec.Body.String())
	}

	statusReq := httptest.NewRequest(http.MethodGet, "/api/runtime", nil)
	statusRec := httptest.NewRecorder()
	s.handleRuntimeStatus(statusRec, statusReq)

	var got runtimeStatusResponse
	if err := json.Unmarshal(statusRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("runtime status json: %v", err)
	}
	if len(got.Accounts) != 1 {
		t.Fatalf("unexpected accounts: %+v", got.Accounts)
	}
	if got.Accounts[0].FiveHour.Used != 80 || got.Accounts[0].FiveHour.Limit != 100 {
		t.Fatalf("five hour snapshot not exposed: %+v", got.Accounts[0])
	}
	if got.Accounts[0].SevenDay.Used != 200 || got.Accounts[0].SevenDay.Limit != 1000 {
		t.Fatalf("seven day snapshot not exposed: %+v", got.Accounts[0])
	}
}

func TestHandleRuntimeStatusMergesLiveTelemetry(t *testing.T) {
	isolateConfigHome(t)
	cfg := config.DefaultConfig()
	cfg.Providers = map[string]config.ProviderSettings{
		"anthropic": {},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}

	s := New(runtimeStatusTestProvider{name: "anthropic", displayName: "Anthropic"}, "claude-sonnet-4-20250514", 0)
	now := time.Now().UTC()
	s.runtimeTelemetry.RecordUsage("anthropic", runtimeplane.GroupClaude, 12, 8, now)
	s.runtimeTelemetry.RecordFailure("anthropic", runtimeplane.GroupClaude, 429, now)

	req := httptest.NewRequest(http.MethodGet, "/api/runtime", nil)
	rec := httptest.NewRecorder()
	s.handleRuntimeStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got runtimeStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("runtime status json: %v", err)
	}
	if !strings.Contains(got.QuotaSource, "live proxy telemetry") {
		t.Fatalf("quota source did not report live telemetry: %q", got.QuotaSource)
	}
	if len(got.Accounts) != 1 {
		t.Fatalf("unexpected accounts: %+v", got.Accounts)
	}
	if got.Accounts[0].FiveHour.Used != 20 || got.Accounts[0].SevenDay.Used != 20 {
		t.Fatalf("telemetry usage not merged: %+v", got.Accounts[0])
	}
	if got.Accounts[0].CooldownUntil == nil || !got.Accounts[0].CooldownUntil.After(now) {
		t.Fatalf("rate limit cooldown not merged: %+v", got.Accounts[0])
	}
}

func TestRuntimeQuotaCollectorFromFileUpdatesTelemetry(t *testing.T) {
	isolateConfigHome(t)
	path := filepath.Join(t.TempDir(), "quota.json")
	if err := os.WriteFile(path, []byte(`{"provider":"anthropic","group":"claude","fiveHour":{"used":44,"limit":100},"sevenDay":{"used":90,"limit":1000}}`), 0644); err != nil {
		t.Fatal(err)
	}

	s := New(runtimeStatusTestProvider{name: "anthropic", displayName: "Anthropic"}, "claude-sonnet-4-20250514", 0)
	count := s.StartRuntimeQuotaCollectors([]config.RuntimeQuotaSource{{
		Name:            "test-file",
		Type:            runtimeplane.QuotaSourceFile,
		Path:            path,
		IntervalSeconds: 3600,
	}})
	defer s.StopRuntimeQuotaCollectors()
	if count != 1 {
		t.Fatalf("collector count=%d want=1", count)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		accounts := s.runtimeTelemetry.Merge(nil, time.Now().UTC())
		if len(accounts) == 1 && accounts[0].FiveHour.Used == 44 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("collector did not update telemetry: %+v", s.runtimeTelemetry.Merge(nil, time.Now().UTC()))
}

func TestHandleRuntimeStatusReportsQuotaCollectorSource(t *testing.T) {
	isolateConfigHome(t)
	cfg := config.DefaultConfig()
	cfg.Providers = map[string]config.ProviderSettings{"anthropic": {}}
	cfg.RuntimeQuotaSources = []config.RuntimeQuotaSource{{
		Name:            "test-file",
		Type:            runtimeplane.QuotaSourceFile,
		Path:            "quota.json",
		Headers:         map[string]string{"Authorization": "secret"},
		IntervalSeconds: 3600,
	}}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}

	s := New(runtimeStatusTestProvider{name: "anthropic", displayName: "Anthropic"}, "claude-sonnet-4-20250514", 0)
	path := filepath.Join(t.TempDir(), "quota.json")
	if err := os.WriteFile(path, []byte(`{"provider":"anthropic","group":"claude"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if count := s.StartRuntimeQuotaCollectors([]config.RuntimeQuotaSource{{
		Name:            "test-file",
		Type:            runtimeplane.QuotaSourceFile,
		Path:            path,
		IntervalSeconds: 3600,
	}}); count != 1 {
		t.Fatalf("collector count=%d want=1", count)
	}
	defer s.StopRuntimeQuotaCollectors()

	req := httptest.NewRequest(http.MethodGet, "/api/runtime", nil)
	rec := httptest.NewRecorder()
	s.handleRuntimeStatus(rec, req)

	var got runtimeStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("runtime status json: %v", err)
	}
	if !strings.Contains(got.QuotaSource, "subscription quota collector polling 1 source") {
		t.Fatalf("quota source did not report collector: %q", got.QuotaSource)
	}
	if len(got.QuotaCollectors) != 1 {
		t.Fatalf("quota collectors not exposed: %+v", got.QuotaCollectors)
	}
	if got.QuotaCollectors[0].Name != "test-file" || !got.QuotaCollectors[0].Enabled || got.QuotaCollectors[0].Path != "quota.json" {
		t.Fatalf("unexpected quota collector info: %+v", got.QuotaCollectors[0])
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("quota collector secret headers leaked: %s", rec.Body.String())
	}
}

func TestRuntimeQuotaSourceAPIAddsTogglesAndDeletesFileSource(t *testing.T) {
	isolateConfigHome(t)
	path := filepath.Join(t.TempDir(), "quota.json")
	if err := os.WriteFile(path, []byte(`{"provider":"anthropic","group":"claude","fiveHour":{"used":7,"limit":10}}`), 0644); err != nil {
		t.Fatal(err)
	}

	s := New(runtimeStatusTestProvider{name: "anthropic", displayName: "Anthropic"}, "claude-sonnet-4-20250514", 0)
	createBody := []byte(fmt.Sprintf(`{"name":"local quota","type":"file","path":%q,"intervalSeconds":3600}`, path))
	createReq := httptest.NewRequest(http.MethodPost, "/api/runtime/quota-sources", bytes.NewReader(createBody))
	createRec := httptest.NewRecorder()
	s.handleRuntimeQuotaSourceCreate(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", createRec.Code, createRec.Body.String())
	}
	if s.RuntimeQuotaCollectorCount() != 1 {
		t.Fatalf("collector was not started")
	}
	cfg := config.Load()
	if len(cfg.RuntimeQuotaSources) != 1 || cfg.RuntimeQuotaSources[0].Path != path {
		t.Fatalf("source not persisted: %+v", cfg.RuntimeQuotaSources)
	}

	patchReq := httptest.NewRequest(http.MethodPatch, "/api/runtime/quota-sources/0", strings.NewReader(`{"disabled":true}`))
	patchReq.SetPathValue("index", "0")
	patchRec := httptest.NewRecorder()
	s.handleRuntimeQuotaSourcePatch(patchRec, patchReq)
	if patchRec.Code != http.StatusOK {
		t.Fatalf("patch status=%d body=%s", patchRec.Code, patchRec.Body.String())
	}
	if s.RuntimeQuotaCollectorCount() != 0 {
		t.Fatalf("collector should be stopped after disable")
	}
	if cfg = config.Load(); len(cfg.RuntimeQuotaSources) != 1 || !cfg.RuntimeQuotaSources[0].Disabled {
		t.Fatalf("source not disabled: %+v", cfg.RuntimeQuotaSources)
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/api/runtime/quota-sources/0", nil)
	deleteReq.SetPathValue("index", "0")
	deleteRec := httptest.NewRecorder()
	s.handleRuntimeQuotaSourceDelete(deleteRec, deleteReq)
	if deleteRec.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", deleteRec.Code, deleteRec.Body.String())
	}
	if cfg = config.Load(); len(cfg.RuntimeQuotaSources) != 0 {
		t.Fatalf("source not deleted: %+v", cfg.RuntimeQuotaSources)
	}
	var deleteBody runtimeQuotaSourcesResponse
	if err := json.Unmarshal(deleteRec.Body.Bytes(), &deleteBody); err != nil {
		t.Fatalf("delete response json: %v", err)
	}
	if deleteBody.Sources == nil || len(deleteBody.Sources) != 0 {
		t.Fatalf("delete response should expose empty sources array: %+v", deleteBody.Sources)
	}
}

func TestRuntimeQuotaSourceAPIHidesHeaderValues(t *testing.T) {
	isolateConfigHome(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"provider":"anthropic","group":"claude"}`))
	}))
	defer upstream.Close()

	s := New(runtimeStatusTestProvider{name: "anthropic", displayName: "Anthropic"}, "claude-sonnet-4-20250514", 0)
	createBody := []byte(fmt.Sprintf(`{"name":"quota api","type":"http","url":%q,"headers":{"Authorization":"Bearer secret-token"}}`, upstream.URL))
	createReq := httptest.NewRequest(http.MethodPost, "/api/runtime/quota-sources", bytes.NewReader(createBody))
	createRec := httptest.NewRecorder()
	s.handleRuntimeQuotaSourceCreate(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", createRec.Code, createRec.Body.String())
	}
	if strings.Contains(createRec.Body.String(), "secret-token") {
		t.Fatalf("create response leaked header value: %s", createRec.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/runtime/quota-sources", nil)
	getRec := httptest.NewRecorder()
	s.handleRuntimeQuotaSources(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", getRec.Code, getRec.Body.String())
	}
	if strings.Contains(getRec.Body.String(), "secret-token") {
		t.Fatalf("get response leaked header value: %s", getRec.Body.String())
	}
	var got runtimeQuotaSourcesResponse
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("quota sources json: %v", err)
	}
	if len(got.Sources) != 1 || len(got.Sources[0].HeaderNames) != 1 || got.Sources[0].HeaderNames[0] != "Authorization" {
		t.Fatalf("header names not exposed as expected: %+v", got.Sources)
	}
}

func TestRuntimeQuotaSourceAPITestsUnsavedFileSource(t *testing.T) {
	isolateConfigHome(t)
	path := filepath.Join(t.TempDir(), "quota.json")
	if err := os.WriteFile(path, []byte(`{"provider":"anthropic","group":"claude","fiveHour":{"used":12,"limit":100}}`), 0644); err != nil {
		t.Fatal(err)
	}

	s := New(runtimeStatusTestProvider{name: "anthropic", displayName: "Anthropic"}, "claude-sonnet-4-20250514", 0)
	body := []byte(fmt.Sprintf(`{"name":"preview","type":"file","path":%q}`, path))
	req := httptest.NewRequest(http.MethodPost, "/api/runtime/quota-sources/test", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleRuntimeQuotaSourceTest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("test status=%d body=%s", rec.Code, rec.Body.String())
	}

	var got runtimeQuotaSourceTestResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("test response json: %v", err)
	}
	if !got.OK || got.AccountCount != 1 || len(got.Accounts) != 1 {
		t.Fatalf("unexpected test response: %+v", got)
	}
	if got.Accounts[0].Provider != "anthropic" || got.Accounts[0].FiveHour.Used != 12 {
		t.Fatalf("snapshot preview not returned: %+v", got.Accounts[0])
	}
	if len(config.Load().RuntimeQuotaSources) != 0 {
		t.Fatalf("unsaved test should not persist source")
	}
}

func TestRuntimeQuotaSourceAPITestsSavedHTTPSourcesWithStoredHeaders(t *testing.T) {
	isolateConfigHome(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"provider":"openai","group":"codex","sevenDay":{"used":22,"limit":100}}`))
	}))
	defer upstream.Close()

	cfg := config.DefaultConfig()
	cfg.RuntimeQuotaSources = []config.RuntimeQuotaSource{{
		Name:    "saved-http",
		Type:    runtimeplane.QuotaSourceHTTP,
		URL:     upstream.URL,
		Headers: map[string]string{"Authorization": "Bearer secret-token"},
	}}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}

	s := New(runtimeStatusTestProvider{name: "anthropic", displayName: "Anthropic"}, "claude-sonnet-4-20250514", 0)
	req := httptest.NewRequest(http.MethodPost, "/api/runtime/quota-sources/0/test", nil)
	req.SetPathValue("index", "0")
	rec := httptest.NewRecorder()
	s.handleRuntimeQuotaSourceTestSaved(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("saved test status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret-token") {
		t.Fatalf("saved test leaked header value: %s", rec.Body.String())
	}
	var got runtimeQuotaSourceTestResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("saved test response json: %v", err)
	}
	if got.AccountCount != 1 || got.Accounts[0].Provider != "openai" || got.Accounts[0].SevenDay.Used != 22 {
		t.Fatalf("unexpected saved test response: %+v", got)
	}
	if len(got.Source.HeaderNames) != 1 || got.Source.HeaderNames[0] != "Authorization" {
		t.Fatalf("header name missing: %+v", got.Source)
	}
}

func TestRuntimeQuotaSourceAPICreatesSampleFileWithoutPersistingSource(t *testing.T) {
	isolateConfigHome(t)
	s := New(runtimeStatusTestProvider{name: "anthropic", displayName: "Anthropic"}, "claude-sonnet-4-20250514", 0)

	req := httptest.NewRequest(http.MethodPost, "/api/runtime/quota-sources/sample", nil)
	rec := httptest.NewRecorder()
	s.handleRuntimeQuotaSourceSample(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("sample status=%d body=%s", rec.Code, rec.Body.String())
	}
	var sample runtimeQuotaSourceSampleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &sample); err != nil {
		t.Fatalf("sample response json: %v", err)
	}
	if !sample.OK || sample.Path == "" || sample.Source.Type != runtimeplane.QuotaSourceFile {
		t.Fatalf("unexpected sample response: %+v", sample)
	}
	if filepath.Base(sample.Path) != "runtime-quota-sample.json" {
		t.Fatalf("unexpected sample path: %s", sample.Path)
	}
	if _, err := os.Stat(sample.Path); err != nil {
		t.Fatalf("sample file missing: %v", err)
	}
	if len(config.Load().RuntimeQuotaSources) != 0 {
		t.Fatalf("sample creation should not persist source")
	}

	testReq := httptest.NewRequest(http.MethodPost, "/api/runtime/quota-sources/test", strings.NewReader(fmt.Sprintf(`{"type":"file","path":%q}`, sample.Path)))
	testRec := httptest.NewRecorder()
	s.handleRuntimeQuotaSourceTest(testRec, testReq)
	if testRec.Code != http.StatusOK {
		t.Fatalf("sample test status=%d body=%s", testRec.Code, testRec.Body.String())
	}
	var preview runtimeQuotaSourceTestResponse
	if err := json.Unmarshal(testRec.Body.Bytes(), &preview); err != nil {
		t.Fatalf("sample test response json: %v", err)
	}
	if preview.AccountCount != 1 || preview.Accounts[0].Provider != "anthropic" || preview.Accounts[0].FiveHour.Limit != 100 {
		t.Fatalf("sample did not parse as quota snapshot: %+v", preview)
	}
}

func isolateConfigHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOMEDRIVE", "")
	t.Setenv("HOMEPATH", "")
}

type runtimeStatusTestProvider struct {
	name        string
	displayName string
	models      []types.ModelInfo
}

func (p runtimeStatusTestProvider) Name() string              { return p.name }
func (p runtimeStatusTestProvider) DisplayName() string       { return p.displayName }
func (p runtimeStatusTestProvider) Models() []types.ModelInfo { return p.models }
func (p runtimeStatusTestProvider) Validate() error           { return nil }
func (p runtimeStatusTestProvider) StreamMessage(context.Context, *types.MessagesRequest, *types.StreamOptions) (<-chan types.SSEEvent, error) {
	ch := make(chan types.SSEEvent)
	close(ch)
	return ch, nil
}
