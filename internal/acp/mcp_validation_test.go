package acp

import "testing"

func TestStableV1StdioMCPAcceptsPATHCommandWithoutAdvertisingRemoteTransports(t *testing.T) {
	server := MCPServer{
		Name: "package-runner", Command: "npx",
		Args: []string{"-y", "example-mcp"}, Env: []EnvVariable{},
	}
	if err := validateMCPServers([]MCPServer{server}, MCPCapabilities{}); err != nil {
		t.Fatalf("stdio PATH command rejected: %v", err)
	}
	remote := MCPServer{
		Type: "http", Name: "remote", URL: "https://example.invalid/mcp", Headers: []HTTPHeader{},
	}
	if err := validateMCPServers([]MCPServer{remote}, MCPCapabilities{}); err == nil {
		t.Fatal("HTTP MCP was accepted without advertised capability")
	}
}
