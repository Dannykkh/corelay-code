package agent

import (
	"os"
	"strings"

	"github.com/Dannykkh/corelay-code/internal/types"
)

// egressTools are the built-in agent tools that reach the public internet.
// In air-gap mode they are stripped from the advertised toolset AND refused at
// execution time, so a model running in a closed network cannot phone home.
//
// Note: Bash can also reach the network (e.g. `curl`), but gating it wholesale
// would break legitimate local use — an air-gapped deployment relies on the
// host having no route off the network for that case. These tools are the ones
// whose entire purpose is outbound internet access.
var egressTools = map[string]bool{
	"WebSearch":   true,
	"WebFetch":    true,
	"WebResearch": true,
	"HTTPRequest": true,
}

// OfflineMode reports whether Corelay Code is running air-gapped / offline.
// CORELAY_OFFLINE is canonical; ANICLEW_OFFLINE remains a compatibility
// fallback. In this mode the agent must make no outbound internet calls.
func OfflineMode() bool {
	switch strings.ToLower(renamedAgentEnv("CORELAY_OFFLINE", "ANICLEW_OFFLINE")) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// renamedAgentEnv returns the canonical environment value when it is
// non-empty, otherwise the legacy value. Keeping this in the agent package
// gives every runtime toggle identical rename precedence.
func renamedAgentEnv(primary, legacy string) string {
	if value := strings.TrimSpace(os.Getenv(primary)); value != "" {
		return value
	}
	return strings.TrimSpace(os.Getenv(legacy))
}

// IsEgressTool reports whether a tool reaches the public internet.
func IsEgressTool(name string) bool {
	return egressTools[name]
}

// filterEgressTools returns defs unchanged when online, or a copy with the
// internet-egress tools removed when offline. The input slice is never mutated.
func filterEgressTools(defs []types.ToolDef) []types.ToolDef {
	if !OfflineMode() {
		return defs
	}
	out := make([]types.ToolDef, 0, len(defs))
	for _, d := range defs {
		if egressTools[d.Name] {
			continue
		}
		out = append(out, d)
	}
	return out
}
