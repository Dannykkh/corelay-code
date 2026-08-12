package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/acp"
)

func TestRunComposesStdioACPWithoutStdoutNoise(t *testing.T) {
	t.Setenv("CORELAY_CONFIG_DIR", t.TempDir())
	serverInput, clientOutput := io.Pipe()
	clientInput, serverOutput := io.Pipe()
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- run(context.Background(), []string{
			"--provider", "ollama",
			"--model", "qwen3:8b",
			"--shutdown-timeout", "1s",
		}, serverInput, serverOutput, &stderr)
	}()
	frames := make(chan []byte, 8)
	go func() {
		defer close(frames)
		scanner := bufio.NewScanner(clientInput)
		scanner.Buffer(make([]byte, 4096), acp.DefaultMaxFrameBytes+1)
		for scanner.Scan() {
			frames <- append([]byte(nil), scanner.Bytes()...)
		}
	}()

	sendCommandFrame(t, clientOutput, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": acp.MethodInitialize,
		"params": map[string]any{"protocolVersion": 1},
	})
	initialize := readCommandFrame(t, frames)
	if initialize.Method != "" || initialize.Error != nil || string(initialize.ID) != "1" {
		t.Fatalf("initialize response = %#v", initialize)
	}
	sendCommandFrame(t, clientOutput, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": acp.MethodSessionNew,
		"params": map[string]any{"cwd": t.TempDir(), "mcpServers": []any{}},
	})
	created := readCommandFrame(t, frames)
	if created.Error != nil || string(created.ID) != "2" {
		t.Fatalf("session/new response = %#v", created)
	}
	var session acp.NewSessionResponse
	if err := json.Unmarshal(created.Result, &session); err != nil || session.SessionID == "" || len(session.ConfigOptions) != 2 {
		t.Fatalf("session/new result = %s, error = %v", created.Result, err)
	}

	_ = clientOutput.Close()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("run() exit = %d, stderr = %q", code, stderr.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run() did not stop after stdin EOF")
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr = %q", stderr.String())
	}
}

type commandFrame struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *acp.RPCError   `json:"error,omitempty"`
}

func sendCommandFrame(t *testing.T, writer io.Writer, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(append(data, '\n')); err != nil {
		t.Fatalf("write frame error = %v", err)
	}
}

func readCommandFrame(t *testing.T, frames <-chan []byte) commandFrame {
	t.Helper()
	select {
	case data, ok := <-frames:
		if !ok {
			t.Fatal("stdout closed before response")
		}
		var frame commandFrame
		if err := json.Unmarshal(data, &frame); err != nil {
			t.Fatalf("stdout contained non-JSON-RPC data %q: %v", data, err)
		}
		return frame
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for command frame")
	}
	return commandFrame{}
}
