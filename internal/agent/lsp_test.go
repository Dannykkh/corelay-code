package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/processsupervisor"
	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

type lspDiscardWriteCloser struct{ bytes.Buffer }

func (*lspDiscardWriteCloser) Close() error { return nil }

func TestLSPPushDiagnosticsBelongToRequestedDocument(t *testing.T) {
	for _, tc := range []struct {
		name          string
		targetItems   []lspDiagnostic
		publishTarget bool
		want          string
		wantError     bool
	}{
		{name: "target diagnostics survive another file", publishTarget: true, targetItems: []lspDiagnostic{{Message: "target error"}}, want: "target error"},
		{name: "empty target diagnostics are authoritative", publishTarget: true, want: "No diagnostics found"},
		{name: "another file cannot satisfy target request", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uri := "file:///workspace/target.go"
			var frames bytes.Buffer
			writer := bufio.NewWriter(&frames)
			publish := func(document string, items []lspDiagnostic) {
				writeLSPHelperFrame(t, writer, lspMessage{JSONRPC: "2.0", Method: "textDocument/publishDiagnostics", Params: mustJSON(map[string]any{"uri": document, "diagnostics": items})})
			}
			if tc.publishTarget {
				publish(uri, tc.targetItems)
			}
			publish("file:///workspace/other.go", []lspDiagnostic{{Message: "unrelated error"}})
			writeLSPHelperFrame(t, writer, lspMessage{JSONRPC: "2.0", ID: json.RawMessage("1"), Error: &lspRPCError{Code: -32601, Message: "pull diagnostics unsupported"}})
			client := &lspClient{stdin: &lspDiscardWriteCloser{}, stdout: bufio.NewReader(&frames), timeout: time.Second, diagnosticsMu: newSyncDiagnostics()}
			result, err := client.execute(context.Background(), lspInput{Operation: "diagnostics", FilePath: "/workspace/target.go", MaxResults: 10}, uri)
			if (err != nil) != tc.wantError {
				t.Fatalf("result=%q err=%v", result, err)
			}
			if strings.Contains(result, "unrelated error") || !strings.Contains(result, tc.want) {
				t.Fatalf("wrong document diagnostics: %q", result)
			}
		})
	}
}

func TestLSPServerHelper(t *testing.T) {
	if !strings.Contains(strings.Join(os.Args, " "), "-test.run=^TestLSPServerHelper$") {
		return
	}
	reader := bufio.NewReader(os.Stdin)
	writer := bufio.NewWriter(os.Stdout)
	for {
		frame, err := readLSPFrame(reader)
		if err != nil {
			return
		}
		var message lspMessage
		if json.Unmarshal(frame, &message) != nil {
			return
		}
		switch message.Method {
		case "initialize":
			writeLSPHelperFrame(t, writer, lspMessage{JSONRPC: "2.0", ID: message.ID, Result: mustJSON(map[string]any{"capabilities": map[string]any{}})})
		case "shutdown":
			writeLSPHelperFrame(t, writer, lspMessage{JSONRPC: "2.0", ID: message.ID, Result: json.RawMessage("null")})
		case "textDocument/definition":
			var params struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
			}
			_ = json.Unmarshal(message.Params, &params)
			writeLSPHelperFrame(t, writer, lspMessage{JSONRPC: "2.0", ID: message.ID, Result: mustJSON([]lspLocation{{URI: params.TextDocument.URI, Range: lspRange{Start: lspPosition{Line: 1, Character: 2}}}})})
		case "textDocument/references":
			writeLSPHelperFrame(t, writer, lspMessage{JSONRPC: "2.0", ID: message.ID, Result: mustJSON([]lspLocation{})})
		case "textDocument/diagnostic":
			writeLSPHelperFrame(t, writer, lspMessage{JSONRPC: "2.0", ID: message.ID, Result: mustJSON(map[string]any{"items": []lspDiagnostic{{Range: lspRange{Start: lspPosition{Line: 2, Character: 1}}, Severity: 1, Message: "undefined: Missing"}}})})
		case "exit":
			return
		}
	}
}

func writeLSPHelperFrame(t *testing.T, writer *bufio.Writer, message lspMessage) {
	t.Helper()
	frame, err := json.Marshal(message)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(writer, "Content-Length: %d\r\n\r\n", len(frame))
	_, _ = writer.Write(frame)
	_ = writer.Flush()
}

func TestLSPClientUsesSemanticProtocolAndWindowsSafeURIs(t *testing.T) {
	workspace := t.TempDir()
	file := filepath.Join(workspace, "space dir", "main.go")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("package sample\nfunc Answer() int { return Missing }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	client, err := newLSPClient(context.Background(), executable, workspace, LSPExecutionOptions{
		Runner:          processsupervisor.NewHostRunner(),
		Policy:          sandbox.Policy{Enforcement: sandbox.EnforcementDisabled},
		ExecutionPolicy: ExecutionPolicySnapshot{Mode: ExecutionModeFull},
		Args:            []string{"-test.run=^TestLSPServerHelper$"},
		CallTimeout:     5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	if err := client.initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(file)
	uri := lspURI(file)
	if !strings.Contains(uri, "%20") {
		t.Fatalf("URI did not escape a space: %q", uri)
	}
	if err := client.didOpen(context.Background(), uri, content); err != nil {
		t.Fatal(err)
	}
	definition, err := client.execute(context.Background(), lspInput{Operation: "definition", FilePath: file, Line: 2, Column: 1, MaxResults: 10}, uri)
	if err != nil || !strings.Contains(definition, "LSP definition (semantic)") || !strings.Contains(definition, "main.go:2:3") {
		t.Fatalf("definition=%q err=%v", definition, err)
	}
	diagnostics, err := client.execute(context.Background(), lspInput{Operation: "diagnostics", FilePath: file, MaxResults: 10}, uri)
	if err != nil || !strings.Contains(diagnostics, "undefined: Missing") || !strings.Contains(diagnostics, "[error]") {
		t.Fatalf("diagnostics=%q err=%v", diagnostics, err)
	}
	references, err := client.execute(context.Background(), lspInput{Operation: "references", FilePath: file, Line: 2, Column: 1, MaxResults: 10}, uri)
	if err != nil || !strings.Contains(references, "No semantic locations found") {
		t.Fatalf("references=%q err=%v", references, err)
	}
}

func TestLSPRequestCancellationAndMissingServerFallback(t *testing.T) {
	workspace := t.TempDir()
	file := filepath.Join(workspace, "main.go")
	if err := os.WriteFile(file, []byte("package sample\nfunc Answer() int { return 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	client, err := newLSPClient(context.Background(), executable, workspace, LSPExecutionOptions{
		Runner:          processsupervisor.NewHostRunner(),
		Policy:          sandbox.Policy{Enforcement: sandbox.EnforcementDisabled},
		ExecutionPolicy: ExecutionPolicySnapshot{Mode: ExecutionModeFull},
		Args:            []string{"-test.run=^TestLSPServerHelper$"},
		CallTimeout:     5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.request(ctx, "initialize", nil); err == nil {
		t.Fatal("canceled request unexpectedly succeeded")
	}
	t.Setenv("CORELAY_LSP_EXECUTABLE", filepath.Join(workspace, "missing-gopls"))
	output, isError := executeLSPWithOptions(json.RawMessage(fmt.Sprintf(`{"operation":"definition","file_path":%q,"line":2,"column":1}`, file)), workspace, ToolExecutionOptions{Context: context.Background()})
	if isError || !strings.Contains(output, "RepoMap fallback (structural only; not semantic)") {
		t.Fatalf("fallback output=%q isError=%v", output, isError)
	}
	if !strings.Contains(output, "LSP unavailable") {
		t.Fatal("fallback did not expose unavailable capability")
	}
}

func TestLSPToolIsReadOnlyAndPathScoped(t *testing.T) {
	definitionFound := false
	for _, definition := range ExtendedToolDefs() {
		if definition.Name == "LSP" {
			definitionFound = true
			break
		}
	}
	if !definitionFound {
		t.Fatal("LSP is missing from the extended tool catalog")
	}
	if danger, _ := ClassifyDanger("LSP", json.RawMessage(`{"operation":"diagnostics","file_path":"main.go"}`)); danger != DangerSafe {
		t.Fatalf("LSP danger = %v, want safe", danger)
	}
	if !IsConcurrencySafe("LSP", map[string]any{"operation": "diagnostics"}) {
		t.Fatal("LSP should be concurrency safe")
	}
	if !planModeTools["LSP"] {
		t.Fatal("LSP should be available in plan mode")
	}
}

func TestInstalledGoplsSemanticFixture(t *testing.T) {
	executable, err := exec.LookPath("gopls")
	if err != nil {
		t.Skip("gopls is not installed; Q15 remains a configured-environment check")
	}
	workspace := filepath.Join(t.TempDir(), "space project")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"go.mod":    "module fixture.example\n\ngo 1.23\n",
		"answer.go": "package fixture\n\nfunc Answer() int { return 42 }\n",
		"use.go":    "package fixture\n\nfunc Use() int { return Answer() }\n",
		"broken.go": "package fixture\n\nfunc Broken() int { var value int = \"bad\"; return value }\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	client, err := newLSPClient(context.Background(), executable, workspace, LSPExecutionOptions{
		Runner:          processsupervisor.NewHostRunner(),
		Policy:          sandbox.Policy{Enforcement: sandbox.EnforcementDisabled},
		ExecutionPolicy: ExecutionPolicySnapshot{Mode: ExecutionModeFull},
		Args:            []string{"serve"},
		CallTimeout:     20 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	if err := client.initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	usePath := filepath.Join(workspace, "use.go")
	useContent, _ := os.ReadFile(usePath)
	useURI := lspURI(usePath)
	if err := client.didOpen(context.Background(), useURI, useContent); err != nil {
		t.Fatal(err)
	}
	useLine := "func Use() int { return Answer() }"
	callColumn := strings.Index(useLine, "Answer") + 1
	definition, err := client.execute(context.Background(), lspInput{Operation: "definition", FilePath: usePath, Line: 3, Column: callColumn, MaxResults: 20}, useURI)
	if err != nil || !strings.Contains(definition, "answer.go:3:") {
		t.Fatalf("gopls definition=%q err=%v", definition, err)
	}
	answerPath := filepath.Join(workspace, "answer.go")
	answerContent, _ := os.ReadFile(answerPath)
	answerURI := lspURI(answerPath)
	if err := client.didOpen(context.Background(), answerURI, answerContent); err != nil {
		t.Fatal(err)
	}
	answerLine := "func Answer() int { return 42 }"
	references, err := client.execute(context.Background(), lspInput{Operation: "references", FilePath: answerPath, Line: 3, Column: strings.Index(answerLine, "Answer") + 1, MaxResults: 20}, answerURI)
	if err != nil || !strings.Contains(references, "use.go:3:") {
		t.Fatalf("gopls references=%q err=%v", references, err)
	}
	brokenPath := filepath.Join(workspace, "broken.go")
	brokenContent, _ := os.ReadFile(brokenPath)
	brokenURI := lspURI(brokenPath)
	if err := client.didOpen(context.Background(), brokenURI, brokenContent); err != nil {
		t.Fatal(err)
	}
	diagnostics, err := client.execute(context.Background(), lspInput{Operation: "diagnostics", FilePath: brokenPath, MaxResults: 20}, brokenURI)
	if err != nil || !strings.Contains(diagnostics, "cannot use") {
		t.Fatalf("gopls diagnostics=%q err=%v", diagnostics, err)
	}

	external := filepath.Join(t.TempDir(), "external project")
	if err := os.MkdirAll(external, 0o755); err != nil {
		t.Fatal(err)
	}
	externalFiles := map[string]string{
		"go.mod":    "module external.example\n\ngo 1.23\n",
		"answer.go": "package external\n\nfunc Answer() int { return 7 }\n",
		"use.go":    "package external\n\nfunc Use() int { return Answer() }\n",
	}
	for name, content := range externalFiles {
		if err := os.WriteFile(filepath.Join(external, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	externalUse := filepath.Join(external, "use.go")
	t.Setenv("CORELAY_LSP_EXECUTABLE", executable)
	fullPolicy := &ExecutionPolicySnapshot{Mode: ExecutionModeFull}
	externalLine := "func Use() int { return Answer() }"
	externalInput := json.RawMessage(fmt.Sprintf(`{"operation":"definition","file_path":%q,"line":3,"column":%d,"max_results":20}`, externalUse, strings.Index(externalLine, "Answer")+1))
	externalResult, externalError := executeLSPWithOptions(externalInput, workspace, ToolExecutionOptions{Context: context.Background(), ExecutionPolicy: fullPolicy})
	if externalError || !strings.Contains(externalResult, "answer.go:3:") {
		t.Fatalf("external gopls definition=%q error=%v", externalResult, externalError)
	}
}
