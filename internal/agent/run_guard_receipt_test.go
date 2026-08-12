package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/harness"
	"github.com/Dannykkh/corelay-code/internal/types"
)

type recoveryRunRecorder struct {
	summary  RunSummary
	receipt  AgentReceipt
	complete int
	failed   int
}

func (*recoveryRunRecorder) RunStarted() {}
func (r *recoveryRunRecorder) ReceiptWritten(_ string, receipt AgentReceipt) {
	r.receipt = receipt
}
func (r *recoveryRunRecorder) RunCompleted(summary RunSummary) {
	r.complete++
	r.summary = summary
}
func (r *recoveryRunRecorder) RunFailed(string) { r.failed++ }

func TestRunGuardSnapshotPreservesContentFreeDenialAuditAcrossReset(t *testing.T) {
	guard := NewRunGuard(1)
	input := ActionFingerprintInput{
		ToolName:  "Read",
		Arguments: json.RawMessage(`{"file_path":"private-value.txt"}`),
		PlanStep:  "inspect",
	}
	if decision := guard.Observe(input); !decision.Allowed {
		t.Fatalf("first decision = %#v", decision)
	}
	denied := guard.Observe(input)
	if denied.Allowed || denied.Reason != RunGuardRepeatedAction {
		t.Fatalf("second decision = %#v", denied)
	}
	guard.Reset()
	if decision := guard.Observe(input); !decision.Allowed {
		t.Fatalf("decision after reset = %#v", decision)
	}

	snapshot := guard.Snapshot()
	if snapshot.RepeatLimit != 1 || snapshot.Observations != 3 || snapshot.Denied != 1 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if snapshot.LastDeniedReason != RunGuardRepeatedAction ||
		snapshot.LastDeniedFingerprint != denied.Fingerprint ||
		snapshot.LastDeniedOccurrences != 2 {
		t.Fatalf("denial audit = %#v, decision=%#v", snapshot, denied)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if strings.Contains(string(encoded), "private-value") || strings.Contains(string(encoded), "Read") {
		t.Fatalf("snapshot leaked normalized action: %s", encoded)
	}
}

func TestRunLoopRepeatedActionTerminalWritesContentFreeRecoveryReceipt(t *testing.T) {
	configDir := isolateEvidenceLoopTest(t)
	workDir := t.TempDir()
	profile := harness.MustResolveProfile(harness.ProfileSpec{
		ID:              "run-guard-receipt",
		MaxIterations:   4,
		MaxErrorRounds:  3,
		RepeatLimit:     1,
		ReadBeforeWrite: harness.SomeBool(false),
		ResponsePolicy:  harness.ResponseNative,
		ToolRouting:     harness.ToolRoutingDirect,
	})
	provider := &scriptedLoopProvider{steps: []scriptedLoopStep{
		toolUseStep("read-1", "Read", map[string]string{"file_path": "secret-missing.txt"}),
		toolUseStep("read-2", "Read", map[string]string{"file_path": "secret-missing.txt"}),
		toolUseStep("read-3", "Read", map[string]string{"file_path": "secret-missing.txt"}),
	}}
	userContent, _ := json.Marshal("inspect the missing file")
	eventCh := make(chan Event, 128)
	go RunLoopWithOptions(context.Background(), provider, "fixture-model", []types.Message{
		{Role: "user", Content: userContent},
	}, workDir, RunOptions{
		HarnessProfile:      &profile,
		EvidencePolicy:      EvidencePolicyConfig{Policy: EvidencePolicyOff},
		DisablePlugins:      true,
		DisableWorkspaceMCP: true,
	}, eventCh)

	var events []Event
	for event := range eventCh {
		events = append(events, event)
	}
	if len(events) == 0 || events[len(events)-1].Type != "done" {
		t.Fatalf("terminal events = %s", eventDump(events))
	}
	done, ok := events[len(events)-1].Data.(map[string]interface{})
	if !ok || done["stopReason"] != "consecutive_tool_failures" {
		t.Fatalf("done = %#v", events[len(events)-1].Data)
	}
	recovery, ok := done["recovery"].(RunGuardSnapshot)
	if !ok || recovery.Denied != 2 || recovery.Observations != 3 || recovery.LastDeniedOccurrences != 3 {
		t.Fatalf("terminal recovery = %#v", done["recovery"])
	}
	receipt := readOnlyAgentReceipt(t, configDir)
	if receipt.Recovery == nil || receipt.Recovery.Denied != 2 || receipt.Recovery.Observations != 3 {
		t.Fatalf("receipt recovery = %#v", receipt.Recovery)
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("marshal receipt: %v", err)
	}
	if strings.Contains(string(encoded), "secret-missing") {
		t.Fatalf("receipt leaked raw action: %s", encoded)
	}
	if receipt.Verification.TerminalState != EvidenceTerminalBlocked {
		t.Fatalf("terminal state = %q", receipt.Verification.TerminalState)
	}
}

func TestRunLoopRecoveredRepeatCarriesSameAuditInReceiptSummaryAndDone(t *testing.T) {
	isolateEvidenceLoopTest(t)
	workDir := t.TempDir()
	profile := harness.MustResolveProfile(harness.ProfileSpec{
		ID:              "run-guard-recovered",
		MaxIterations:   4,
		MaxErrorRounds:  3,
		RepeatLimit:     1,
		ReadBeforeWrite: harness.SomeBool(false),
		ResponsePolicy:  harness.ResponseNative,
		ToolRouting:     harness.ToolRoutingDirect,
	})
	provider := &scriptedLoopProvider{steps: []scriptedLoopStep{
		toolUseStep("read-1", "Read", map[string]string{"file_path": "missing-audit.txt"}),
		toolUseStep("read-2", "Read", map[string]string{"file_path": "missing-audit.txt"}),
		textStep("recovered answer"),
	}}
	recorder := &recoveryRunRecorder{}
	userContent, _ := json.Marshal("inspect the missing file")
	eventCh := make(chan Event, 128)
	go RunLoopWithOptions(context.Background(), provider, "fixture-model", []types.Message{
		{Role: "user", Content: userContent},
	}, workDir, RunOptions{
		HarnessProfile:      &profile,
		EvidencePolicy:      EvidencePolicyConfig{Policy: EvidencePolicyOff},
		DisablePlugins:      true,
		DisableWorkspaceMCP: true,
		Recorder:            recorder,
	}, eventCh)

	var done map[string]interface{}
	for event := range eventCh {
		if event.Type == "done" {
			done, _ = event.Data.(map[string]interface{})
		}
	}
	if recorder.complete != 1 || recorder.failed != 0 {
		t.Fatalf("recorder terminal counts = completed:%d failed:%d", recorder.complete, recorder.failed)
	}
	if recorder.summary.Recovery == nil || recorder.receipt.Recovery == nil {
		t.Fatalf("missing recovery audit: summary=%#v receipt=%#v", recorder.summary.Recovery, recorder.receipt.Recovery)
	}
	terminalRecovery, ok := done["recovery"].(RunGuardSnapshot)
	if !ok {
		t.Fatalf("done recovery = %#v", done["recovery"])
	}
	if terminalRecovery != *recorder.summary.Recovery || terminalRecovery != *recorder.receipt.Recovery {
		t.Fatalf("recovery mismatch: done=%#v summary=%#v receipt=%#v", terminalRecovery, recorder.summary.Recovery, recorder.receipt.Recovery)
	}
	if terminalRecovery.Denied != 1 || terminalRecovery.Observations != 2 {
		t.Fatalf("recovery audit = %#v", terminalRecovery)
	}
}

func TestRunGuardReceiptSanitizerRejectsForgedAuditFields(t *testing.T) {
	receipt := AgentReceipt{
		Recovery: &RunGuardSnapshot{
			RepeatLimit:           -1,
			Observations:          -2,
			Denied:                99,
			LastDeniedReason:      RunGuardReason("forged-secret-reason"),
			LastDeniedFingerprint: "raw-secret-fingerprint",
			LastDeniedOccurrences: -3,
		},
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("marshal receipt: %v", err)
	}
	if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "forged") {
		t.Fatalf("sanitized receipt leaked forged audit fields: %s", encoded)
	}
	var payload struct {
		Recovery *RunGuardSnapshot `json:"recovery"`
	}
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatalf("decode receipt: %v", err)
	}
	if payload.Recovery == nil || *payload.Recovery != (RunGuardSnapshot{}) {
		t.Fatalf("sanitized recovery = %#v", payload.Recovery)
	}
}
