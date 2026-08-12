package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPluginLoad(t *testing.T) {
	dir := t.TempDir()

	// Create a test plugin
	pluginDir := filepath.Join(dir, "test-plugin")
	os.MkdirAll(pluginDir, 0755)

	manifest := Plugin{
		Name:        "test-plugin",
		Version:     "1.0.0",
		Description: "A test plugin",
		Author:      "tester",
		Commands: []PluginCommand{
			{Name: "greet", Description: "Say hello", Command: "echo hello"},
		},
		Hooks: []PluginHook{
			{Event: "pre_tool_use", Command: "echo pre"},
		},
	}
	data, _ := json.MarshalIndent(manifest, "", "  ")
	os.WriteFile(filepath.Join(pluginDir, "plugin.json"), data, 0644)

	// Load
	pm := NewPluginManager(dir)
	pm.LoadAll()

	plugins := pm.GetPlugins()
	if len(plugins) != 1 {
		t.Fatalf("Expected 1 plugin, got %d", len(plugins))
	}
	if plugins[0].Name != "test-plugin" {
		t.Errorf("Expected name 'test-plugin', got %q", plugins[0].Name)
	}

	cmds := pm.GetAllCommands()
	if len(cmds) != 1 || cmds[0].Name != "greet" {
		t.Errorf("Expected 1 command 'greet', got %v", cmds)
	}

	hooks := pm.GetAllHooks()
	if len(hooks) != 1 || hooks[0].Event != "pre_tool_use" {
		t.Errorf("Expected 1 hook, got %v", hooks)
	}
}

func TestPluginEmptyDir(t *testing.T) {
	pm := NewPluginManager(t.TempDir())
	pm.LoadAll()
	if len(pm.GetPlugins()) != 0 {
		t.Error("Empty dir should load 0 plugins")
	}
}

func TestPluginStrictManifestRejectsMalformedDuplicateAndUnknownFields(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		want     string
	}{
		{name: "malformed", manifest: `{`, want: "EOF"},
		{
			name:     "duplicate field",
			manifest: `{"name":"first","name":"second","version":"1.0.0"}`,
			want:     `duplicate field "name"`,
		},
		{
			name:     "nested duplicate field",
			manifest: `{"name":"plugin","version":"1.0.0","tools":[{"name":"one","name":"two","command":"legacy"}]}`,
			want:     `duplicate field "name"`,
		},
		{
			name:     "unknown top-level field",
			manifest: `{"name":"plugin","version":"1.0.0","surprise":true}`,
			want:     `unknown field "surprise"`,
		},
		{
			name:     "unknown tool field",
			manifest: `{"name":"plugin","version":"1.0.0","tools":[{"name":"one","command":"legacy","environment":{"TOKEN":"x"}}]}`,
			want:     `unknown field "environment"`,
		},
		{
			name:     "duplicate tool name",
			manifest: `{"name":"plugin","version":"1.0.0","tools":[{"name":"same","command":"first"},{"name":"same","command":"second"}]}`,
			want:     `duplicate tool name "same"`,
		},
		{
			name:     "reserved tool name",
			manifest: `{"name":"plugin","version":"1.0.0","tools":[{"name":"Bash","command":"legacy"}]}`,
			want:     `collides with a reserved built-in name`,
		},
		{
			name:     "mixed shell and exec form",
			manifest: `{"name":"plugin","version":"1.0.0","tools":[{"name":"mixed","command":"echo unsafe","executable":"bin/tool","input_schema":{"type":"object"}}]}`,
			want:     `cannot combine legacy command and executable`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			pluginDir := filepath.Join(dir, "fixture")
			if err := os.MkdirAll(pluginDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(pluginDir, "plugin.json"), []byte(test.manifest), 0o600); err != nil {
				t.Fatal(err)
			}
			manager := NewPluginManager(dir)
			err := manager.LoadAllStrict()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadAllStrict error = %v, want %q", err, test.want)
			}
			if got := manager.GetPlugins(); len(got) != 0 {
				t.Fatalf("invalid manifest left a partial catalog: %#v", got)
			}
		})
	}
}

func TestPluginStrictManifestReadIsBounded(t *testing.T) {
	dir := t.TempDir()
	pluginDir := filepath.Join(dir, "oversized")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := strings.Repeat(" ", maxPluginManifestBytes+1)
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := NewPluginManager(dir)
	if err := manager.LoadAllStrict(); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized manifest error = %v", err)
	}
}

func TestDefaultPluginDirsUsesProjectThenGlobalClaudeLocations(t *testing.T) {
	workDir := t.TempDir()
	directories := DefaultPluginDirs(workDir)
	if len(directories) == 0 || directories[0] != filepath.Join(workDir, ".claude", "plugins") {
		t.Fatalf("default plugin directories = %#v", directories)
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		if len(directories) != 2 || directories[1] != filepath.Join(home, ".claude", "plugins") {
			t.Fatalf("global plugin directory = %#v", directories)
		}
	}
}
