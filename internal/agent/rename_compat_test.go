package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestRenamedAgentEnvironmentPrefersCorelayAndFallsBack(t *testing.T) {
	t.Setenv("CORELAY_OFFLINE", "false")
	t.Setenv("ANICLEW_OFFLINE", "true")
	if OfflineMode() {
		t.Fatal("CORELAY_OFFLINE did not take priority over ANICLEW_OFFLINE")
	}

	t.Setenv("CORELAY_MEMORY", "off")
	t.Setenv("ANICLEW_MEMORY", "on")
	if memoryEnabled() {
		t.Fatal("CORELAY_MEMORY did not take priority over ANICLEW_MEMORY")
	}

	t.Setenv("CORELAY_AUTOSKILL", "on")
	t.Setenv("ANICLEW_AUTOSKILL", "off")
	if !autoSkillEnabled() {
		t.Fatal("CORELAY_AUTOSKILL did not take priority over ANICLEW_AUTOSKILL")
	}

	t.Setenv("CORELAY_AUTOVERIFY", "off")
	t.Setenv("ANICLEW_AUTOVERIFY", "on")
	if autoVerifyEnabled() {
		t.Fatal("CORELAY_AUTOVERIFY did not take priority over ANICLEW_AUTOVERIFY")
	}

	t.Setenv("CORELAY_EVIDENCE_POLICY", EvidencePolicyBlock)
	t.Setenv("ANICLEW_EVIDENCE_POLICY", EvidencePolicyOff)
	if got := evidencePolicyFromEnv(); got != EvidencePolicyBlock {
		t.Fatalf("evidencePolicyFromEnv() = %q, want %q", got, EvidencePolicyBlock)
	}

	t.Setenv("CORELAY_OFFLINE", "")
	t.Setenv("ANICLEW_OFFLINE", "yes")
	if !OfflineMode() {
		t.Fatal("ANICLEW_OFFLINE fallback was not honored")
	}
}

func TestCorelayProtocolsEmitCurrentAndRecognizeLegacy(t *testing.T) {
	for name, protocol := range map[string]string{
		"tool execution":   boundToolExecutionProtocol,
		"plugin approval":  pluginApprovalExecutionProtocol,
		"file mutation":    fileMutationExecutionProtocol,
		"host interaction": hostInteractionExecutionProtocol,
		"plugin executor":  pluginExecutorProtocol,
	} {
		if !strings.HasPrefix(protocol, "corelay.") {
			t.Errorf("%s protocol = %q, want corelay.*", name, protocol)
		}
	}

	legacyPluginInput := json.RawMessage(`{
		"protocol":"aniclew.plugin-approval.v1",
		"approval":{"signature":"legacy-proof"},
		"input":{"value":"ok"}
	}`)
	unwrapped, _, handled, err := unwrapPluginApprovalExecutionInput(legacyPluginInput)
	if err != nil || !handled || !strings.Contains(string(unwrapped), `"value":"ok"`) {
		t.Fatalf("legacy plugin envelope: handled=%v input=%s err=%v", handled, unwrapped, err)
	}

	legacyHostInput := json.RawMessage(`{
		"protocol":"aniclew.host-interaction.v1",
		"approval":{"signature":"legacy-proof"},
		"input":{"region":"screen"}
	}`)
	unwrapped, _, handled, err = unwrapHostInteractionExecutionInput(legacyHostInput)
	if err != nil || !handled || !strings.Contains(string(unwrapped), `"region":"screen"`) {
		t.Fatalf("legacy host envelope: handled=%v input=%s err=%v", handled, unwrapped, err)
	}
}

func TestPersistedLegacyToolCatalogIdentityRemainsValid(t *testing.T) {
	read := currentBuiltInToolDefinitions()["Read"]
	currentMarker := registerRunToolCatalogSchemas([]types.ToolDef{read})
	legacyMarker := strings.Replace(
		currentMarker,
		toolCatalogSchemaMarkerPrefix,
		legacyToolCatalogSchemaMarkerPrefix,
		1,
	)
	allowed := map[string]struct{}{"Read": {}, legacyMarker: {}}
	identity, err := executorIdentityForAllowedTool(allowed, "Read")
	if err != nil {
		t.Fatal(err)
	}
	if identity.CatalogMarker != legacyMarker || identity.Owner != "aniclew" {
		t.Fatalf("legacy identity = %#v", identity)
	}
	if err := validateBoundToolExecutorIdentity(identity); err != nil {
		t.Fatalf("legacy identity was not accepted: %v", err)
	}
}

func TestCorelayRuntimeIdentityStrings(t *testing.T) {
	if !strings.Contains(baseSystemPrompt, "You are Corelay Code") {
		t.Fatalf("base system prompt does not use display name: %q", baseSystemPrompt[:64])
	}
	if got := browserUserAgent(); !strings.Contains(got, "CorelayCode/1.0") ||
		!strings.Contains(got, "github.com/Dannykkh/corelay-code") {
		t.Fatalf("default web user agent = %q", got)
	}
}
