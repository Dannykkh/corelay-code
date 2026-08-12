package agent

import (
	"errors"
	"strings"

	"github.com/Dannykkh/corelay-code/internal/hooks"
)

// RunTerminalKind is the bounded, content-free disposition carried by the
// final done event. A done frame is a stream boundary, not by itself proof of
// task success; consumers must also inspect DurableRunTerminalMetadata.
type RunTerminalKind string

const (
	RunTerminalCompleted      RunTerminalKind = "completed"
	RunTerminalCommand        RunTerminalKind = "command"
	RunTerminalCancelled      RunTerminalKind = "cancelled"
	RunTerminalContextBlocked RunTerminalKind = "context_blocked"
	RunTerminalMaxIterations  RunTerminalKind = "max_iterations"
	RunTerminalMaxCycles      RunTerminalKind = "max_cycles"
	RunTerminalFailed         RunTerminalKind = "failed"
	RunTerminalNoTerminal     RunTerminalKind = "no_terminal"
)

// RunTerminalDurablePolicy states what a durable adapter may do with the
// terminal. It is descriptive metadata; blocked DurableRunTerminalMetadata is
// still the authoritative fail-closed signal for existing consumers.
type RunTerminalDurablePolicy string

const (
	RunTerminalDurableCommit    RunTerminalDurablePolicy = "commit"
	RunTerminalDurableNone      RunTerminalDurablePolicy = "none"
	RunTerminalDurableReconcile RunTerminalDurablePolicy = "reconcile"
)

var errRunReceiptAlreadyAttempted = errors.New("run receipt already attempted")

type runTerminalOutcome struct {
	kind          RunTerminalKind
	stopReason    string
	durablePolicy RunTerminalDurablePolicy
	message       string
	data          map[string]interface{}
	summary       RunSummary
	success       bool
}

// runTerminalFinalizer is the single linearization point for RunLoop exits.
// It deliberately owns no provider, tool, or mode behavior. Its only job is
// to make lifecycle effects and the terminal frame exactly once and ordered.
type runTerminalFinalizer struct {
	eventCh  chan<- Event
	recorder RunRecorder

	finalized        bool
	recorderStarted  bool
	hookStarted      bool
	hookEnd          func()
	hookSnapshot     func() []hooks.HookResult
	receiptAttempted bool
	outcome          *runTerminalOutcome
}

func newRunTerminalFinalizer(eventCh chan<- Event, recorder RunRecorder) *runTerminalFinalizer {
	finalizer := &runTerminalFinalizer{eventCh: eventCh, recorder: recorder}
	if recorder != nil {
		recorder.RunStarted()
		finalizer.recorderStarted = true
	}
	return finalizer
}

// BeginHookSession executes SessionStart and registers the matching SessionEnd
// before control can return to the loop. Preflight failures never call this,
// so SessionEnd count always equals SessionStart count and is either zero or
// one. The finalizer runs SessionEnd before recorder terminalization.
func (f *runTerminalFinalizer) BeginHookSession(
	start func(),
	end func(),
	snapshot func() []hooks.HookResult,
) {
	if f == nil || f.finalized || f.hookStarted {
		return
	}
	f.hookStarted = true
	f.hookEnd = end
	f.hookSnapshot = snapshot
	if start != nil {
		start()
	}
}

// WriteReceipt preserves the historical receipt creation conditions while
// making both the filesystem write and recorder callback at-most-once.
func (f *runTerminalFinalizer) WriteReceipt(workDir string, receipt AgentReceipt) (string, error) {
	if f == nil || f.finalized || f.receiptAttempted {
		return "", errRunReceiptAlreadyAttempted
	}
	f.receiptAttempted = true
	path, err := writeAgentReceipt(workDir, receipt)
	if err != nil {
		return "", err
	}
	if f.recorder != nil {
		f.recorder.ReceiptWritten(path, receipt)
	}
	return path, nil
}

func (f *runTerminalFinalizer) Complete(
	kind RunTerminalKind,
	stopReason string,
	durablePolicy RunTerminalDurablePolicy,
	data map[string]interface{},
	summary RunSummary,
) {
	if f == nil || f.finalized || f.outcome != nil {
		return
	}
	if kind == "" {
		kind = RunTerminalCompleted
	}
	if durablePolicy == "" {
		durablePolicy = RunTerminalDurableCommit
	}
	f.outcome = &runTerminalOutcome{
		kind: kind, stopReason: boundedRunTerminalReason(stopReason),
		durablePolicy: durablePolicy, data: cloneRunTerminalData(data),
		summary: summary, success: true,
	}
}

func (f *runTerminalFinalizer) Fail(
	kind RunTerminalKind,
	stopReason string,
	message string,
	data map[string]interface{},
) {
	if f == nil || f.finalized || f.outcome != nil {
		return
	}
	if kind == "" {
		kind = RunTerminalFailed
	}
	f.outcome = &runTerminalOutcome{
		kind: kind, stopReason: boundedRunTerminalReason(stopReason),
		durablePolicy: RunTerminalDurableReconcile,
		message:       sanitizeSnapshotText(redactSensitiveString(message), 1000),
		data:          cloneRunTerminalData(data),
	}
}

func (f *runTerminalFinalizer) Finalize() {
	if f == nil || f.finalized {
		return
	}
	f.finalized = true
	if f.outcome == nil {
		f.outcome = &runTerminalOutcome{
			kind: RunTerminalNoTerminal, stopReason: "no_terminal",
			durablePolicy: RunTerminalDurableReconcile,
			message:       "run exited without a terminal outcome",
		}
	}
	if f.hookStarted && f.hookEnd != nil {
		f.hookEnd()
	}

	outcome := f.outcome
	if outcome.success {
		if f.hookSnapshot != nil {
			outcome.summary.Hooks = f.hookSnapshot()
		}
		if f.recorderStarted {
			f.recorder.RunCompleted(outcome.summary)
		}
	} else if f.recorderStarted {
		failRun(f.recorder, outcome.message)
	}

	payload := cloneRunTerminalData(outcome.data)
	payload["kind"] = string(outcome.kind)
	payload["stopReason"] = outcome.stopReason
	payload["durablePolicy"] = string(outcome.durablePolicy)
	if !outcome.success {
		// Every non-success done frame must fail closed for all existing durable
		// consumers. Preserve any completion status/count metadata already set.
		payload["terminalState"] = EvidenceTerminalBlocked
	}
	f.eventCh <- Event{Type: "done", Data: payload}
	close(f.eventCh)
}

func cloneRunTerminalData(data map[string]interface{}) map[string]interface{} {
	if len(data) == 0 {
		return make(map[string]interface{}, 4)
	}
	clone := make(map[string]interface{}, len(data)+4)
	for key, value := range data {
		clone[key] = value
	}
	return clone
}

func boundedRunTerminalReason(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	return sanitizeSnapshotText(redactSensitiveString(value), 128)
}
