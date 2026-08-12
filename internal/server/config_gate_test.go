package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/config"
	"github.com/Dannykkh/corelay-code/internal/providers"
)

type configAPIErrorResponse struct {
	Type  string `json:"type"`
	Error struct {
		Type       string `json:"type"`
		Code       string `json:"code"`
		Message    string `json:"message"`
		ActiveRuns int    `json:"activeRuns"`
	} `json:"error"`
}

func TestSetConfigBlocksRuntimeMutationAcrossWorkspaces(t *testing.T) {
	t.Setenv("CORELAY_CONFIG_DIR", t.TempDir())
	initial, err := providers.Create("openai", nil)
	if err != nil {
		t.Fatalf("Create initial provider = %v", err)
	}
	s := New(initial, "existing-model", 0)

	_, _, release, err := s.loops.Register(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Register active loop = %v", err)
	}
	defer release()

	secretModel := "model-with-private-token-material"
	rec := putConfig(t, s, `{"provider":"anthropic","model":"`+secretModel+`","routerEnabled":true}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("PUT /api/config status = %d, want %d; body=%s", rec.Code, http.StatusConflict, rec.Body.String())
	}
	var response configAPIErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode config error = %v", err)
	}
	if response.Type != "error" || response.Error.Type != "config_error" ||
		response.Error.Code != "config_change_blocked_active_runs" || response.Error.ActiveRuns != 1 {
		t.Fatalf("config error = %+v", response)
	}
	if strings.Contains(rec.Body.String(), secretModel) {
		t.Fatal("active-run conflict echoed requested model")
	}

	s.mu.RLock()
	providerName := s.activeProvider.Name()
	model := s.activeModel
	routerEnabled := s.router != nil
	s.mu.RUnlock()
	if providerName != "openai" || model != "existing-model" || routerEnabled {
		t.Fatalf("runtime changed while loop active: provider=%q model=%q router=%t", providerName, model, routerEnabled)
	}
	if persisted := config.Load(); persisted.DefaultProvider != "" || persisted.DefaultModel != "" || persisted.RouterEnabled {
		t.Fatalf("blocked mutation was persisted: %+v", persisted)
	}
}

func TestSetConfigAllowsNoOpAndLanguageUpdateDuringActiveRun(t *testing.T) {
	t.Setenv("CORELAY_CONFIG_DIR", t.TempDir())
	initial, err := providers.Create("openai", nil)
	if err != nil {
		t.Fatalf("Create initial provider = %v", err)
	}
	s := New(initial, "same-model", 0)

	_, _, release, err := s.loops.Register(context.Background(), "workspace-a")
	if err != nil {
		t.Fatalf("Register active loop = %v", err)
	}
	defer release()

	rec := putConfig(t, s, `{"provider":"openai","model":"same-model","routerEnabled":false,"responseLang":"ko"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("no-op PUT /api/config status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	s.mu.RLock()
	responseLang := s.responseLang
	s.mu.RUnlock()
	if responseLang != "ko" {
		t.Fatalf("response language = %q, want ko", responseLang)
	}
	persisted := config.Load()
	if persisted.DefaultProvider != "openai" || persisted.DefaultModel != "same-model" || persisted.ResponseLang != "ko" {
		t.Fatalf("no-op/language update not persisted: %+v", persisted)
	}
}

func TestSetConfigAppliesRuntimeMutationAfterLoopsDrain(t *testing.T) {
	t.Setenv("CORELAY_CONFIG_DIR", t.TempDir())
	initial, err := providers.Create("openai", nil)
	if err != nil {
		t.Fatalf("Create initial provider = %v", err)
	}
	s := New(initial, "old-model", 0)

	_, _, release, err := s.loops.Register(context.Background(), "workspace-a")
	if err != nil {
		t.Fatalf("Register active loop = %v", err)
	}
	release()

	rec := putConfig(t, s, `{"provider":"anthropic","model":"new-model","routerEnabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/config status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	s.mu.RLock()
	providerName := s.activeProvider.Name()
	model := s.activeModel
	routerEnabled := s.router != nil
	s.mu.RUnlock()
	if providerName != "anthropic" || model != "new-model" || !routerEnabled {
		t.Fatalf("runtime = %q/%q router=%t", providerName, model, routerEnabled)
	}
	persisted := config.Load()
	if persisted.DefaultProvider != "anthropic" || persisted.DefaultModel != "new-model" || !persisted.RouterEnabled {
		t.Fatalf("runtime update not persisted: %+v", persisted)
	}
}

func TestSetConfigFailsClosedWhenRegistryIsUnavailableOrShuttingDown(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*Server)
		code string
	}{
		{
			name: "unavailable",
			set:  func(s *Server) { s.loops = nil },
			code: "loop_registry_unavailable",
		},
		{
			name: "shutting down",
			set: func(s *Server) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if ok := s.loops.Shutdown(ctx); !ok {
					t.Fatal("empty registry did not drain")
				}
			},
			code: "server_shutting_down",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CORELAY_CONFIG_DIR", t.TempDir())
			initial, err := providers.Create("openai", nil)
			if err != nil {
				t.Fatalf("Create initial provider = %v", err)
			}
			s := New(initial, "old-model", 0)
			tc.set(s)

			rec := putConfig(t, s, `{"provider":"anthropic","model":"new-model"}`)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("PUT /api/config status = %d, want 503; body=%s", rec.Code, rec.Body.String())
			}
			var response configAPIErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode config error = %v", err)
			}
			if response.Error.Code != tc.code {
				t.Fatalf("error code = %q, want %q", response.Error.Code, tc.code)
			}
			s.mu.RLock()
			providerName := s.activeProvider.Name()
			model := s.activeModel
			s.mu.RUnlock()
			if providerName != "openai" || model != "old-model" {
				t.Fatalf("runtime changed after fail-closed response: %q/%q", providerName, model)
			}
		})
	}
}

func putConfig(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/config", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleSetConfig(rec, req)
	return rec
}
