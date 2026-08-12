package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/processsupervisor"
	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestRunLoopMCPHelper(t *testing.T) {
	if os.Getenv("CORELAY_MCP_HELPER") != "1" {
		return
	}
	encoder := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			return
		}
		if len(request.ID) == 0 {
			continue
		}
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]string{"name": "fixture", "version": "1"},
			}
		case "tools/list":
			toolName := os.Getenv("CORELAY_MCP_TOOL_NAME")
			if toolName == "" {
				toolName = "mcp_fixture_echo"
			}
			result = map[string]any{"tools": []map[string]any{{
				"name":        toolName,
				"description": "Echo a fixture value",
				"inputSchema": map[string]any{
					"type":                 "object",
					"properties":           map[string]any{"value": map[string]any{"type": "string"}},
					"required":             []string{"value"},
					"additionalProperties": false,
				},
			}}}
		case "tools/call":
			if os.Getenv("CORELAY_MCP_BLOCK") == "1" {
				select {}
			}
			value := os.Getenv("CORELAY_MCP_RESULT")
			if value == "" {
				value = "fixture"
			}
			result = map[string]any{"content": []map[string]string{{"type": "text", "text": value}}}
		default:
			result = map[string]any{}
		}
		if encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result}) != nil {
			return
		}
	}
}

type mcpLoopRecorder struct {
	mu      sync.Mutex
	reports []processsupervisor.Report
	summary RunSummary
}

func (r *mcpLoopRecorder) RunStarted()                         {}
func (r *mcpLoopRecorder) ReceiptWritten(string, AgentReceipt) {}
func (r *mcpLoopRecorder) RunCompleted(summary RunSummary) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.summary = summary
}
func (r *mcpLoopRecorder) MCPReported(report processsupervisor.Report) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reports = append(r.reports, report)
}

func (r *mcpLoopRecorder) snapshot() ([]processsupervisor.Report, RunSummary) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]processsupervisor.Report(nil), r.reports...), r.summary
}

func TestRunLoopConnectsMCPBeforeFirstCatalogAndRecordsStart(t *testing.T) {
	DisconnectAllMCP()
	t.Cleanup(DisconnectAllMCP)
	workDir := t.TempDir()
	config := MCPConfig{MCPServers: map[string]MCPServerConfig{
		"fixture": {
			Command: os.Args[0],
			Args:    []string{"-test.run=^TestRunLoopMCPHelper$"},
			Env:     map[string]string{"CORELAY_MCP_HELPER": "1"},
		},
	}}
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, ".mcp.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}

	provider := &responsePolicyTestProvider{steps: []responsePolicyStep{{visible: "done"}}}
	recorder := &mcpLoopRecorder{}
	mcpExecution := secureTestMCPExecution(context.Background())
	runContext, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	events := make(chan Event, 64)
	go RunLoopWithOptions(
		runContext,
		provider,
		"mcp-loop-model",
		[]types.Message{{Role: "user", Content: mustJSON("use the fixture if needed")}},
		workDir,
		RunOptions{
			ResponseLang: "en",
			Recorder:     recorder,
			MCPExecution: &mcpExecution,
		},
		events,
	)
	seenStartEvent := false
	for event := range events {
		if event.Type == "error" {
			t.Fatalf("RunLoop error: %v", event.Data)
		}
		if event.Type == "mcp_process" {
			seenStartEvent = true
		}
	}
	cancelRun()
	DisconnectAllMCP()

	requests := provider.requestsSnapshot()
	if len(requests) == 0 || !containsTool(requests[0].Tools, "mcp_fixture_echo") {
		if len(requests) == 0 {
			t.Fatal("provider received no request")
		}
		t.Fatalf("first request omitted live MCP tool: %v", toolNames(requests[0].Tools))
	}
	reports, summary := recorder.snapshot()
	if !seenStartEvent || len(reports) != 1 || !reports[0].Started {
		t.Fatalf("MCP start evidence: event=%v reports=%#v", seenStartEvent, reports)
	}
	if len(summary.MCP) != 1 || !summary.MCP[0].Started {
		t.Fatalf("RunSummary MCP evidence = %#v", summary.MCP)
	}
}
