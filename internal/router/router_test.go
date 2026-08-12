package router

import (
	"encoding/json"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/types"
)

func makeReq(text string, tools ...string) *types.MessagesRequest {
	content, _ := json.Marshal(text)
	req := &types.MessagesRequest{
		Messages: []types.Message{
			{Role: "user", Content: content},
		},
	}
	for _, t := range tools {
		req.Tools = append(req.Tools, types.ToolDef{Name: t})
	}
	return req
}

func TestClassify_ExplainIntent(t *testing.T) {
	cfg := DefaultConfig()
	role, _ := Classify(makeReq("what is this function?"), &cfg)
	if role != RoleExplain {
		t.Errorf("Expected explain, got %s", role)
	}
}

func TestClassify_BreadthSignalAvoidsCheapRoute(t *testing.T) {
	cfg := DefaultConfig()

	// Short comparison/research queries carry no intent keyword and are tiny,
	// so they would fall to the cheap short-context role. The breadth signal
	// must lift them to the default (capable) model instead.
	for _, q := range []string{"compare python and go", "recommend a database", "비교해줘"} {
		role, _ := Classify(makeReq(q), &cfg)
		if role != RoleDefault {
			t.Errorf("breadth query %q: expected default, got %s", q, role)
		}
	}

	// A plain short query without any breadth signal still uses the cheap route.
	role, _ := Classify(makeReq("hello there"), &cfg)
	if role != RoleShortCtx {
		t.Errorf("plain short query: expected short-context, got %s", role)
	}
}

func TestClassify_DebugIntent(t *testing.T) {
	cfg := DefaultConfig()
	role, _ := Classify(makeReq("fix this bug"), &cfg)
	if role != RoleDebug {
		t.Errorf("Expected debug, got %s", role)
	}
}

func TestClassify_CommitIntent(t *testing.T) {
	cfg := DefaultConfig()
	role, _ := Classify(makeReq("commit these changes"), &cfg)
	if role != RoleCommit {
		t.Errorf("Expected commit, got %s", role)
	}
}

func TestClassify_Korean(t *testing.T) {
	cfg := DefaultConfig()

	role, _ := Classify(makeReq("이 함수 설명해줘"), &cfg)
	if role != RoleExplain {
		t.Errorf("Korean explain: expected explain, got %s", role)
	}

	role, _ = Classify(makeReq("디버그 해줘"), &cfg)
	if role != RoleDebug {
		t.Errorf("Korean debug: expected debug, got %s", role)
	}

	role, _ = Classify(makeReq("리팩토링 해주세요"), &cfg)
	if role != RoleRefactor {
		t.Errorf("Korean refactor: expected refactor, got %s", role)
	}
}

func TestClassify_ToolBased(t *testing.T) {
	cfg := DefaultConfig()

	role, _ := Classify(makeReq("do something", "Agent"), &cfg)
	if role != RoleAgentSpawn {
		t.Errorf("Agent tool: expected agent-spawn, got %s", role)
	}

	role, _ = Classify(makeReq("edit file", "Edit", "Write", "Glob"), &cfg)
	if role != RoleFileEdit {
		t.Errorf("Edit tool: expected file-edit, got %s", role)
	}

	role, _ = Classify(makeReq("read code", "Read", "Glob", "Grep"), &cfg)
	if role != RoleFileRead {
		t.Errorf("Read-only: expected file-read, got %s", role)
	}
}

func TestRoute_Disabled(t *testing.T) {
	r := New(nil, nil)
	r.config.Enabled = false
	decision := r.Route(makeReq("hello"))
	if decision.Provider != "default" {
		t.Errorf("Disabled router should return default, got %s", decision.Provider)
	}
}

func TestRoute_Enabled(t *testing.T) {
	r := New(nil, nil)
	decision := r.Route(makeReq("explain this"))
	if decision.Role != RoleExplain {
		t.Errorf("Expected explain role, got %s", decision.Role)
	}
	// Default config routes explain to ollama/qwen3:8b
	if decision.Provider != "ollama" {
		t.Errorf("Expected ollama for explain, got %s", decision.Provider)
	}
}

func TestRoute_Fallback(t *testing.T) {
	r := New(nil, nil)
	fb := r.GetFallback(RoleRefactor)
	if fb == nil {
		t.Fatal("Refactor should have fallback")
	}
	if fb.Provider != "anthropic" {
		t.Errorf("Refactor fallback should be anthropic, got %s", fb.Provider)
	}
}

func TestSetRule(t *testing.T) {
	r := New(nil, nil)
	r.SetRule(RoleExplain, "gemini", "gemini-flash", nil)

	decision := r.Route(makeReq("what is this?"))
	if decision.Provider != "gemini" || decision.Model != "gemini-flash" {
		t.Errorf("Updated rule should route to gemini/flash, got %s/%s", decision.Provider, decision.Model)
	}
}

func TestTrackUsage(t *testing.T) {
	r := New(nil, nil)
	r.TrackUsage("ollama", "qwen3:14b", 1000)
	r.TrackUsage("ollama", "qwen3:14b", 500)

	usage := r.GetUsageSummary()
	if len(usage) != 1 {
		t.Fatalf("Expected 1 usage entry, got %d", len(usage))
	}
	if usage[0].Requests != 2 {
		t.Errorf("Expected 2 requests, got %d", usage[0].Requests)
	}
	if usage[0].Tokens != 1500 {
		t.Errorf("Expected 1500 tokens, got %d", usage[0].Tokens)
	}
}
