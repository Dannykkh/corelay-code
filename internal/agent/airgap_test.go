package agent

import (
	"encoding/json"
	"testing"

	"github.com/aniclew/aniclew/internal/types"
)

func TestOfflineMode(t *testing.T) {
	cases := map[string]bool{
		"":      false,
		"0":     false,
		"false": false,
		"no":    false,
		"1":     true,
		"true":  true,
		"TRUE":  true,
		"yes":   true,
		"on":    true,
		" 1 ":   true,
	}
	for val, want := range cases {
		t.Setenv("ANICLEW_OFFLINE", val)
		if got := OfflineMode(); got != want {
			t.Errorf("OfflineMode() with %q = %v, want %v", val, got, want)
		}
	}
}

func TestFilterEgressTools(t *testing.T) {
	defs := []types.ToolDef{
		{Name: "Read"}, {Name: "Write"}, {Name: "WebSearch"},
		{Name: "WebFetch"}, {Name: "WebResearch"}, {Name: "HTTPRequest"},
		{Name: "Bash"},
	}

	// Online: untouched.
	t.Setenv("ANICLEW_OFFLINE", "0")
	if got := filterEgressTools(defs); len(got) != len(defs) {
		t.Fatalf("online: expected %d tools, got %d", len(defs), len(got))
	}

	// Offline: egress tools removed, local tools kept.
	t.Setenv("ANICLEW_OFFLINE", "1")
	got := filterEgressTools(defs)
	names := map[string]bool{}
	for _, d := range got {
		names[d.Name] = true
	}
	for _, banned := range []string{"WebSearch", "WebFetch", "WebResearch", "HTTPRequest"} {
		if names[banned] {
			t.Errorf("offline: %s should be filtered out", banned)
		}
	}
	for _, kept := range []string{"Read", "Write", "Bash"} {
		if !names[kept] {
			t.Errorf("offline: %s should be kept", kept)
		}
	}
	// Caller's slice must not be mutated.
	if len(defs) != 7 {
		t.Errorf("input slice was mutated: len=%d", len(defs))
	}
}

func TestExecuteToolBlocksEgressOffline(t *testing.T) {
	t.Setenv("ANICLEW_OFFLINE", "1")
	for _, name := range []string{"WebSearch", "WebFetch", "WebResearch", "HTTPRequest"} {
		input := json.RawMessage(`{"url":"https://example.com","query":"x"}`)
		out, isErr := ExecuteTool(name, input, t.TempDir())
		if !isErr {
			t.Errorf("%s: expected error result in offline mode", name)
		}
		if want := "[OFFLINE]"; len(out) < len(want) || out[:len(want)] != want {
			t.Errorf("%s: expected [OFFLINE] prefix, got %q", name, out)
		}
	}
}
