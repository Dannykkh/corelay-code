package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/config"
	"github.com/Dannykkh/corelay-code/internal/types"
)

func writeMCPConfigFixture(t *testing.T, path string, servers map[string]MCPServerConfig) {
	t.Helper()
	data, err := json.Marshal(MCPConfig{MCPServers: servers})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeMCPSettingsFixture(t *testing.T, path string, servers map[string]MCPServerConfig) {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"permissions": map[string]any{"allow": []string{"Read(*)"}},
		"mcpServers":  servers,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMCPConfigWithPathsMergesBySpecificityAndKeepsWorkspacesIsolated(t *testing.T) {
	workspaceA := t.TempDir()
	workspaceB := t.TempDir()
	extraOne := filepath.Join(t.TempDir(), "one.json")
	extraTwo := filepath.Join(t.TempDir(), "two.json")

	writeMCPConfigFixture(t, extraOne, map[string]MCPServerConfig{
		"same": {Command: "extra-one"}, "extra-one": {Command: "extra-one-only"},
	})
	writeMCPConfigFixture(t, extraTwo, map[string]MCPServerConfig{
		"same": {Command: "extra-two"}, "extra-two": {Command: "extra-two-only"},
	})
	writeMCPSettingsFixture(t, filepath.Join(workspaceA, ".claude", "settings.json"), map[string]MCPServerConfig{
		"same": {Command: "workspace-settings"}, "settings-only": {Command: "settings-only"},
	})
	writeMCPConfigFixture(t, filepath.Join(workspaceA, "mcp.json"), map[string]MCPServerConfig{
		"same": {Command: "workspace-mcp-json"}, "mcp-json-only": {Command: "mcp-json-only"},
	})
	writeMCPConfigFixture(t, filepath.Join(workspaceA, ".mcp.json"), map[string]MCPServerConfig{
		"same": {Command: "workspace-dot-mcp"}, "workspace-a-only": {Command: "workspace-a-only"},
	})
	writeMCPConfigFixture(t, filepath.Join(workspaceB, ".mcp.json"), map[string]MCPServerConfig{
		"same": {Command: "workspace-b"}, "workspace-b-only": {Command: "workspace-b-only"},
	})

	raw, present, err := LoadMCPConfigWithPaths(workspaceA, []string{extraOne, extraTwo})
	if err != nil || !present {
		t.Fatalf("merged config = present=%v err=%v", present, err)
	}
	merged, err := ParseMCPConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	wantCommands := map[string]string{
		"same": "workspace-dot-mcp", "extra-one": "extra-one-only", "extra-two": "extra-two-only",
		"settings-only": "settings-only", "mcp-json-only": "mcp-json-only", "workspace-a-only": "workspace-a-only",
	}
	for name, want := range wantCommands {
		server, ok := merged.MCPServers[name]
		if !ok || server.Command != want {
			t.Fatalf("merged server %q = %#v, want command %q", name, server, want)
		}
	}

	raw, present, err = LoadMCPConfigWithPaths(workspaceB, []string{extraOne, extraTwo})
	if err != nil || !present {
		t.Fatalf("workspace B config = present=%v err=%v", present, err)
	}
	merged, err = ParseMCPConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := merged.MCPServers["same"].Command; got != "workspace-b" {
		t.Fatalf("workspace B override = %q, want workspace-b", got)
	}
	if _, leaked := merged.MCPServers["workspace-a-only"]; leaked {
		t.Fatal("workspace A server leaked into workspace B registry")
	}
}

func TestWorkspaceMCPServerSpecsWithPathsConsumesSupplementalConfig(t *testing.T) {
	workspace := t.TempDir()
	supplemental := filepath.Join(t.TempDir(), "configured.json")
	writeMCPConfigFixture(t, supplemental, map[string]MCPServerConfig{
		"configured": {Command: "configured-command", Env: map[string]string{"MCP_FIXTURE": "configured"}},
	})

	specs, present, err := WorkspaceMCPServerSpecsWithPaths(workspace, []string{supplemental})
	if err != nil || !present || len(specs) != 1 {
		t.Fatalf("specs=%#v present=%v err=%v", specs, present, err)
	}
	if specs[0].Name != "configured" || specs[0].Command != "configured-command" || specs[0].Env["MCP_FIXTURE"] != "configured" {
		t.Fatalf("supplemental spec = %#v", specs[0])
	}
}

func TestWorkspaceMCPServerSpecsAcceptsStreamableHTTPConfig(t *testing.T) {
	workspace := t.TempDir()
	writeMCPConfigFixture(t, filepath.Join(workspace, ".mcp.json"), map[string]MCPServerConfig{
		"remote": {Type: "http", URL: "https://example.invalid/mcp", Headers: map[string]string{"Authorization": "Bearer fixture"}},
	})
	specs, present, err := WorkspaceMCPServerSpecsWithPaths(workspace, nil)
	if err != nil || !present || len(specs) != 1 {
		t.Fatalf("HTTP specs=%#v present=%v err=%v", specs, present, err)
	}
	if specs[0].Type != "http" || specs[0].URL != "https://example.invalid/mcp" || specs[0].Headers["Authorization"] != "Bearer fixture" {
		t.Fatalf("HTTP spec = %#v", specs[0])
	}
}

func TestRunLoopConsumesConfiguredMCPConfigPaths(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CORELAY_CONFIG_DIR", configDir)
	t.Setenv("ANICLEW_CONFIG_DIR", "")
	workspace := t.TempDir()
	supplemental := filepath.Join(t.TempDir(), "configured.json")
	writeMCPConfigFixture(t, supplemental, map[string]MCPServerConfig{
		"configured": {
			Command: os.Args[0],
			Args:    []string{"-test.run=^TestRunLoopMCPHelper$"},
			Env: map[string]string{
				"CORELAY_MCP_HELPER":    "1",
				"CORELAY_MCP_RESULT":    "configured-path",
				"CORELAY_MCP_TOOL_NAME": "configured_tool",
			},
		},
	})
	cfg := config.DefaultConfig()
	cfg.MCPConfigPaths = []string{supplemental}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}

	provider := &responsePolicyTestProvider{steps: []responsePolicyStep{{visible: "done"}}}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan Event, 64)
	mcpExecution := secureTestMCPExecution(runCtx)
	go RunLoopWithOptions(
		runCtx,
		provider,
		"mcp-config-path-model",
		[]types.Message{{Role: "user", Content: mustJSON("use configured MCP")}},
		workspace,
		RunOptions{ResponseLang: "en", MCPExecution: &mcpExecution},
		events,
	)
	for event := range events {
		if event.Type == "error" {
			t.Fatalf("RunLoop error: %v", event.Data)
		}
	}
	requests := provider.requestsSnapshot()
	if len(requests) == 0 || !containsTool(requests[0].Tools, "configured_tool") {
		t.Fatalf("configured MCP tool was not advertised: %v", requests)
	}
}
