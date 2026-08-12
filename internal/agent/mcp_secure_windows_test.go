//go:build windows

package agent

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const secureMCPWindowsHelper = "CORELAY_SECURE_MCP_WINDOWS_HELPER"

func TestNewMCPClientUsesSecureWindowsJobStreamingDefault(t *testing.T) {
	executable, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewMCPClient(
		"secure-windows-fixture",
		executable,
		[]string{"-test.run=^TestSecureMCPWindowsHelper$"},
		t.TempDir(),
		map[string]string{secureMCPWindowsHelper: "1"},
	)
	if err != nil {
		t.Fatalf("connect through secure default: %v", err)
	}
	if client.process == nil || client.process.PID() <= 0 || client.cmd != nil {
		t.Fatalf("client process=%v pid=%d compatibility command=%v", client.process, client.process.PID(), client.cmd)
	}
	client.Close()
	select {
	case <-client.process.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("secure MCP process was not reaped on close")
	}
}

func TestSecureMCPWindowsHelper(t *testing.T) {
	if os.Getenv(secureMCPWindowsHelper) != "1" {
		return
	}
	reader := bufio.NewScanner(os.Stdin)
	for reader.Scan() {
		var request jsonRPCRequest
		if json.Unmarshal(reader.Bytes(), &request) != nil {
			os.Exit(21)
		}
		if request.ID == 0 {
			continue
		}
		result := any(map[string]any{})
		if request.Method == "tools/list" {
			result = map[string]any{"tools": []any{}}
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"jsonrpc": "2.0", "id": request.ID, "result": result,
		})
	}
	os.Exit(0)
}
