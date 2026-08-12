package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestBridgeRunTerminalReducer(t *testing.T) {
	for _, test := range []struct {
		name    string
		events  []Event
		ctxErr  error
		wantErr bool
		want    string
	}{
		{name: "legacy done", events: []Event{{Type: "text", Data: "ok"}, {Type: "done"}}, want: "ok"},
		{name: "typed done", events: []Event{{Type: "text", Data: "ok"}, {Type: "done", Data: map[string]any{"terminalState": EvidenceTerminalUnverified}}}, want: "ok"},
		{name: "blocked done", events: []Event{{Type: "text", Data: "partial"}, {Type: "done", Data: map[string]any{"terminalState": EvidenceTerminalBlocked, "completionStatus": CompletionStatusBlocked, "completionBlocked": 1}}}, wantErr: true, want: "partial"},
		{name: "error before done", events: []Event{{Type: "text", Data: "partial"}, {Type: "error", Data: "provider failed"}, {Type: "done"}}, wantErr: true, want: "partial"},
		{name: "close without done", events: []Event{{Type: "text", Data: "partial"}}, wantErr: true, want: "partial"},
		{name: "normal done beats late cancel", events: []Event{{Type: "done"}}, ctxErr: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			var reducer bridgeRunTerminalReducer
			for _, event := range test.events {
				reducer.Observe(event)
			}
			got, err := reducer.Result(test.ctxErr)
			if (err != nil) != test.wantErr || !strings.Contains(got, test.want) {
				t.Fatalf("Result() = %q, %v; want error=%v containing %q", got, err, test.wantErr, test.want)
			}
		})
	}
}

type bridgeErrorProvider struct{}

func (bridgeErrorProvider) Name() string        { return "bridge-error" }
func (bridgeErrorProvider) DisplayName() string { return "bridge-error" }
func (bridgeErrorProvider) Validate() error     { return nil }
func (bridgeErrorProvider) Models() []types.ModelInfo {
	return []types.ModelInfo{{ID: "test-model"}}
}
func (bridgeErrorProvider) StreamMessage(context.Context, *types.MessagesRequest, *types.StreamOptions) (<-chan types.SSEEvent, error) {
	return nil, errors.New("bridge provider sentinel")
}

func TestBridgeSendCommitsOnlySuccessfulDone(t *testing.T) {
	isolateAlternateLoopEnvironment(t)
	provider := &alternateLoopTestProvider{
		name: "remote", models: []types.ModelInfo{{ID: "test-model"}},
		steps: []alternateLoopStep{{text: "bridge completed"}},
	}
	bridge := NewBridge(provider, "test-model", t.TempDir())
	session := bridge.CreateSession()

	result, err := bridge.Send(session.ID, "perform bridge task")
	if err != nil || result != "bridge completed" {
		t.Fatalf("Send() = %q, %v", result, err)
	}
	stored := bridge.GetSession(session.ID)
	if stored == nil || stored.Status != "idle" || stored.LastOutput != result || len(stored.Messages) != 2 {
		t.Fatalf("stored session = %#v", stored)
	}
}

func TestBridgeSendRollsBackFailedHalfTurn(t *testing.T) {
	isolateAlternateLoopEnvironment(t)
	bridge := NewBridge(bridgeErrorProvider{}, "test-model", t.TempDir())
	session := bridge.CreateSession()

	result, err := bridge.Send(session.ID, "must not become durable history")
	if err == nil || result != "" || !strings.Contains(err.Error(), "bridge provider sentinel") {
		t.Fatalf("Send() = %q, %v", result, err)
	}
	stored := bridge.GetSession(session.ID)
	if stored == nil || stored.Status != "idle" || len(stored.Messages) != 0 {
		t.Fatalf("failed session = %#v", stored)
	}
}
