package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
	"github.com/Dannykkh/corelay-code/internal/types"
)

type sandboxLoopRecorder struct {
	reports []SandboxExecutionRecord
	summary RunSummary
}

func (r *sandboxLoopRecorder) RunStarted() {}

func (r *sandboxLoopRecorder) ReceiptWritten(string, AgentReceipt) {}

func (r *sandboxLoopRecorder) RunCompleted(summary RunSummary) { r.summary = summary }

func (r *sandboxLoopRecorder) SandboxReported(record SandboxExecutionRecord) {
	r.reports = append(r.reports, record)
}

func TestRunLoopPropagatesSandboxOptionsAndTypedReport(t *testing.T) {
	isolateEvidenceLoopTest(t)
	workDir := t.TempDir()
	runner := &fakeBashRunner{name: "loop-fake", capabilities: fakeBashCapabilities()}
	runner.run = func(_ context.Context, policy sandbox.Policy, _ sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
		report := fakeBashReport(runner, policy)
		report.Started = true
		report.EffectiveEnforcement = policy.Enforcement
		report.AppliedIsolation.ProcessIsolation = true
		return sandbox.Result{Started: true, Stdout: []byte("sandbox-ok\n"), ExitCode: 0}, report
	}
	provider := &scriptedLoopProvider{steps: []scriptedLoopStep{
		toolUseStep("toolu_sandbox", "Bash", map[string]string{"command": "echo sandbox-ok"}),
		textStep("done"),
	}}
	recorder := &sandboxLoopRecorder{}
	userContent, _ := json.Marshal("Run a sandboxed command and report the result.")
	eventCh := make(chan Event, 64)
	go RunLoopWithOptions(context.Background(), provider, "fake-model", []types.Message{
		{Role: "user", Content: userContent},
	}, workDir, RunOptions{
		ResponseLang:   "auto",
		Recorder:       recorder,
		SandboxRunner:  runner,
		SandboxPolicy:  fakeBashPolicy(sandbox.EnforcementRequired),
		EvidencePolicy: EvidencePolicyConfig{Policy: EvidencePolicyOff},
	}, eventCh)

	var eventReport sandbox.Report
	for event := range eventCh {
		if event.Type != "tool_result" {
			continue
		}
		data, ok := event.Data.(map[string]interface{})
		if !ok || data["id"] != "toolu_sandbox" {
			continue
		}
		eventReport, _ = data["sandbox"].(sandbox.Report)
	}

	if runner.calls != 1 || runner.policy.Enforcement != sandbox.EnforcementRequired {
		t.Fatalf("runner calls=%d policy=%+v", runner.calls, runner.policy)
	}
	if eventReport.Runner != "loop-fake" || !eventReport.Started {
		t.Fatalf("event report = %+v", eventReport)
	}
	if len(recorder.reports) != 1 || recorder.reports[0].ToolID != "toolu_sandbox" {
		t.Fatalf("recorder reports = %+v", recorder.reports)
	}
	if len(recorder.summary.Sandbox) != 1 || recorder.summary.Sandbox[0].Report.Runner != "loop-fake" {
		t.Fatalf("run summary sandbox = %+v", recorder.summary.Sandbox)
	}
}

func TestRunLoopRejectsHalfConfiguredSandboxBeforeModelCall(t *testing.T) {
	isolateEvidenceLoopTest(t)
	provider := &scriptedLoopProvider{steps: []scriptedLoopStep{textStep("must not run")}}
	userContent, _ := json.Marshal("hello")
	eventCh := make(chan Event, 8)
	go RunLoopWithOptions(context.Background(), provider, "fake-model", []types.Message{
		{Role: "user", Content: userContent},
	}, t.TempDir(), RunOptions{
		SandboxRunner: &fakeBashRunner{name: "configured", capabilities: fakeBashCapabilities()},
	}, eventCh)

	var sawError bool
	for event := range eventCh {
		if event.Type == "error" {
			sawError = true
		}
	}
	if !sawError || provider.callCount() != 0 {
		t.Fatalf("sawError=%v provider calls=%d", sawError, provider.callCount())
	}
}
