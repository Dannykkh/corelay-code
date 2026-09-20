package acpbridge

import (
	"testing"

	"github.com/Dannykkh/corelay-code/internal/acp"
)

func TestACPAdvertisesAndConvertsStreamableHTTPMCP(t *testing.T) {
	backend := newBackendFixture(t, scriptedRunner())
	if !backend.backend.Descriptor().MCPCapabilities.HTTP {
		t.Fatal("ACP descriptor did not advertise streamable HTTP MCP")
	}
	specs, err := sessionMCPServerSpecs([]acp.MCPServer{{
		Type: "http", Name: "remote", URL: "https://example.invalid/mcp",
		Headers: []acp.HTTPHeader{{Name: "Authorization", Value: "Bearer fixture"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 || specs[0].Type != "http" || specs[0].URL != "https://example.invalid/mcp" || specs[0].Headers["Authorization"] != "Bearer fixture" {
		t.Fatalf("HTTP spec = %#v", specs)
	}
}
