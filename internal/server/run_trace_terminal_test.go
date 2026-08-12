package server

import (
	"testing"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/observability"
)

func TestRunTraceRecordsEvidenceTerminalState(t *testing.T) {
	tracker := observability.NewTracker(t.TempDir())
	recorder := newObservabilityRunRecorder(tracker, observability.RunTrace{
		ID: "run_terminal_state", Kind: "agent", Provider: "fake", Model: "fake-model",
	})
	recorder.RunStarted()
	recorder.ReceiptWritten("receipt.json", agent.AgentReceipt{
		Provider: "fake", Model: "fake-model",
		Verification: agent.ReceiptVerification{
			Status: "passed", TerminalState: agent.EvidenceTerminalVerified,
		},
	})
	recorder.RunCompleted(agent.RunSummary{
		Provider: "fake", Model: "fake-model",
		Verification: agent.ReceiptVerification{
			Status: "passed", TerminalState: agent.EvidenceTerminalVerified,
		},
	})

	runs := tracker.RecentRuns(1)
	if len(runs) != 1 || runs[0].Metadata["terminalState"] != agent.EvidenceTerminalVerified {
		t.Fatalf("run trace = %#v", runs)
	}
	found := false
	for _, span := range runs[0].Spans {
		if span.Name == "agent.receipt" && span.Data["terminalState"] == agent.EvidenceTerminalVerified {
			found = true
		}
	}
	if !found {
		t.Fatalf("receipt span missing terminal state: %#v", runs[0].Spans)
	}
}
