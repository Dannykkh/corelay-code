package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/harness"
	"github.com/Dannykkh/corelay-code/internal/observability"
	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestShadowKernelToPersistentTrace(t *testing.T) {
	isolateResumeContinuationEnvironment(t)
	dir := t.TempDir()
	tracker := observability.NewTracker(dir)
	recorder := newObservabilityRunRecorder(tracker, observability.RunTrace{ID: "kernel-shadow-fixture", Kind: "agent"})
	calls := 0
	provider := protocolRouteProvider{stream: func(context.Context, *types.MessagesRequest) (<-chan types.SSEEvent, error) {
		calls++
		ch := make(chan types.SSEEvent, 5)
		block, _ := json.Marshal(map[string]string{"type": "tool_use", "id": fmt.Sprintf("read-%d", calls), "name": "Read"})
		delta, _ := json.Marshal(map[string]string{"type": "input_json_delta", "partial_json": `{"file_path":"missing.txt"}`})
		ch <- types.SSEEvent{Type: "content_block_start", ContentBlock: block}
		ch <- types.SSEEvent{Type: "content_block_delta", Delta: delta}
		ch <- types.SSEEvent{Type: "content_block_stop"}
		ch <- types.SSEEvent{Type: "message_delta", Delta: json.RawMessage(`{"stop_reason":"tool_use"}`)}
		ch <- types.SSEEvent{Type: "message_stop"}
		close(ch)
		return ch, nil
	}}
	profile := harness.MustResolveProfile(harness.ProfileSpec{ID: "shadow-persist-test", MaxIterations: 4, MaxErrorRounds: 3, RepeatLimit: 1, ReadBeforeWrite: harness.SomeBool(false), ResponsePolicy: harness.ResponseNative, ToolRouting: harness.ToolRoutingDirect})
	cfg := &agent.ShadowJudgmentConfig{JudgeTarget: "fixture", Prepare: func(ctx context.Context, p agent.ShadowPoint) (agent.ShadowEvidence, error) {
		<-ctx.Done()
		return agent.ShadowEvidence{}, ctx.Err()
	}, Judge: func(context.Context, agent.ShadowRequest) ([]byte, error) { return nil, fmt.Errorf("must not call") }}
	events := make(chan agent.Event, 128)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go agent.RunLoopWithOptions(ctx, provider, "fixture", []types.Message{{Role: "user", Content: json.RawMessage(`"inspect missing file"`)}}, t.TempDir(), agent.RunOptions{HarnessProfile: &profile, EvidencePolicy: agent.EvidencePolicyConfig{Policy: agent.EvidencePolicyOff}, DisablePlugins: true, DisableWorkspaceMCP: true, Recorder: compositeRunRecorder{recorders: []agent.RunRecorder{recorder}}, ShadowJudgment: cfg}, events)
	for range events {
	}
	if ctx.Err() != nil {
		t.Fatal("kernel blocked")
	}
	runs := observability.NewTracker(dir).RecentRuns(1)
	if len(runs) != 1 || runs[0].Status != "failed" {
		t.Fatalf("kernel trace=%+v", runs)
	}
	count := 0
	for _, span := range runs[0].Spans {
		if span.Name == "agent.shadow_judgment" {
			count++
			var r agent.ShadowJudgmentRecord
			if json.Unmarshal([]byte(span.Data["record"]), &r) != nil || agent.ValidateShadowRecord(r) != nil {
				t.Fatal("invalid persistent kernel observation")
			}
		}
	}
	if count != 2 || calls != 3 {
		t.Fatalf("judgments=%d calls=%d", count, calls)
	}
}

func shadowRecorderFixture() agent.ShadowJudgmentRecord {
	d := strings.Repeat("a", 64)
	answer := func(v string) agent.ShadowAnswer { return agent.ShadowAnswer{Value: v, EvidenceIDs: []string{"r2-t0"}} }
	return agent.ShadowJudgmentRecord{RunID: "run_" + strings.Repeat("b", 32), Sequence: 2, SessionDigest: d, WorkspaceDigest: d, CoderDigest: d, JudgeDigest: d, ContractDigest: d, InputDigest: d,
		Status: "evaluated", Advisory: "gather_evidence", Answers: &agent.ShadowResponse{SameFailure: answer("yes"), NewEvidence: answer("no"), AlternativeSupported: answer("no")}, LaterRounds: 2, LaterErrorRounds: 1, LaterRepeatedErrorRounds: 1}
}

func TestShadowRecorderPersistsThroughCompositeAndReload(t *testing.T) {
	for _, failed := range []bool{false, true} {
		dir := t.TempDir()
		tracker := observability.NewTracker(dir)
		r := newObservabilityRunRecorder(tracker, observability.RunTrace{ID: "trace-fixture", Kind: "agent"})
		composite := compositeRunRecorder{recorders: []agent.RunRecorder{r}}
		composite.RunStarted()
		record := shadowRecorderFixture()
		composite.ShadowJudgmentRecorded(record)
		composite.ShadowJudgmentRecorded(record) // dedup
		record.Answers.SameFailure.Value = "secret-mutated-after-submit"
		if failed {
			composite.RunFailed("fixture failed")
		} else {
			composite.RunCompleted(agent.RunSummary{Verification: agent.ReceiptVerification{Status: "passed", TerminalState: agent.EvidenceTerminalVerified}})
		}
		record = shadowRecorderFixture()
		record.Sequence = 3
		composite.ShadowJudgmentRecorded(record) // late
		runs := observability.NewTracker(dir).RecentRuns(1)
		if len(runs) != 1 {
			t.Fatalf("persisted runs=%d", len(runs))
		}
		count := 0
		for _, span := range runs[0].Spans {
			if span.Name == "agent.shadow_judgment" {
				count++
				var decoded agent.ShadowJudgmentRecord
				if json.Unmarshal([]byte(span.Data["record"]), &decoded) != nil || agent.ValidateShadowRecord(decoded) != nil || decoded.LaterRepeatedErrorRounds != 1 {
					t.Fatalf("bad persisted record: %s", span.Data["record"])
				}
				if strings.Contains(span.Data["record"], "secret") {
					t.Fatal("caller mutation escaped")
				}
			}
		}
		if count != 1 {
			t.Fatalf("judgments=%d", count)
		}
		if failed && runs[0].Status != "failed" {
			t.Fatal("failure outcome lost")
		}
		if !failed && runs[0].Metadata["terminalState"] != agent.EvidenceTerminalVerified {
			t.Fatal("verification outcome lost")
		}
	}
}

func TestShadowRecorderRejectsForgedAndCrossRunRecords(t *testing.T) {
	r := newObservabilityRunRecorder(nil, observability.RunTrace{})
	record := shadowRecorderFixture()
	r.ShadowJudgmentRecorded(record)
	record.Sequence = 3
	record.RunID = "run_" + strings.Repeat("c", 32)
	r.ShadowJudgmentRecorded(record)
	record = shadowRecorderFixture()
	record.Sequence = 4
	record.Advisory = "run secret command"
	r.ShadowJudgmentRecorded(record)
	if len(r.trace.Spans) != 1 || r.trace.Metadata["shadowRejected"] != "true" {
		t.Fatal("invalid record admitted")
	}
}
