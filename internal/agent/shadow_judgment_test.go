package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/harness"
	"github.com/Dannykkh/corelay-code/internal/types"
)

func shadowTestPoint(seq int) ShadowPoint {
	return ShadowPoint{RunID: "test-run", Sequence: seq, Previous: ShadowRound{Tools: []ShadowToolObservation{{ID: "r0-t0", Error: true}}}, Current: ShadowRound{Sequence: seq, Tools: []ShadowToolObservation{{ID: "r1-t0", Error: true}}}}
}

func shadowTestEvidence(context.Context, ShadowPoint) (ShadowEvidence, error) {
	return ShadowEvidence{Objective: "repair fixture", Acceptance: "fixture passes", Excerpts: map[string]string{"r0-t0": "previous fixture missing configuration", "r1-t0": "fixture missing configuration"}}, nil
}

func shadowTestResponse(a, b, c string) []byte {
	answer := func(v string) ShadowAnswer { return ShadowAnswer{Value: v, EvidenceIDs: []string{"r1-t0"}} }
	raw, _ := json.Marshal(ShadowResponse{SameFailure: answer(a), NewEvidence: answer(b), AlternativeSupported: answer(c)})
	return raw
}

func waitShadowStatus(t *testing.T, o *shadowObserver, status string) {
	t.Helper()
	deadline := time.After(time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		o.mu.Lock()
		found := len(o.records) > 0 && o.records[0].Status == status
		o.mu.Unlock()
		if found {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("status never became %s", status)
		case <-tick.C:
		}
	}
}

func TestShadowResponseValidationAndPolicy(t *testing.T) {
	e, _ := shadowTestEvidence(context.Background(), ShadowPoint{})
	for _, tc := range []struct{ a, b, c, want string }{
		{"yes", "no", "yes", "consider_alternative"}, {"yes", "no", "no", "gather_evidence"},
		{"yes", "yes", "no", "continue"}, {"no", "no", "no", "continue"}, {"unknown", "yes", "yes", "abstain"},
	} {
		r, err := decodeShadowResponse(shadowTestResponse(tc.a, tc.b, tc.c), e)
		if err != nil || shadowAdvisory(r) != tc.want {
			t.Fatalf("%+v: %v %+v", tc, err, r)
		}
	}
	valid := string(shadowTestResponse("yes", "no", "yes"))
	duplicate := strings.Replace(valid, `"value":"yes"`, `"value":"no","value":"yes"`, 1)
	if _, err := decodeShadowResponse([]byte(duplicate), e); err == nil {
		t.Fatal("accepted ambiguous duplicate answer")
	}
	for _, bad := range []string{"{}", "null", valid + "{}", strings.ReplaceAll(valid, "r1-t0", "forged"), strings.ReplaceAll(valid, "yes", "execute"), strings.Repeat("x", 2049), strings.Replace(valid, "sameFailure", "command", 1)} {
		if _, err := decodeShadowResponse([]byte(bad), e); err == nil {
			t.Fatalf("accepted invalid response: %s", bad)
		}
	}
}

func TestShadowQueueBudgetCancellationAndLateResult(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	exited := make(chan struct{})
	cfg := ShadowJudgmentConfig{JudgeTarget: "fixture", Prepare: shadowTestEvidence, Judge: func(ctx context.Context, r ShadowRequest) ([]byte, error) {
		close(entered)
		defer close(exited)
		<-release // Deliberately ignores cancellation to test the closed sink.
		return shadowTestResponse("yes", "no", "yes"), nil
	}}
	o := newShadowObserver(context.Background(), &cfg, ShadowPoint{})
	defer o.close()
	o.submit(shadowTestPoint(1))
	<-entered
	o.submit(shadowTestPoint(1)) // duplicate
	o.submit(shadowTestPoint(2)) // queued
	o.submit(shadowTestPoint(3)) // dropped, still consumes the bounded audit slot
	o.submit(shadowTestPoint(4)) // budget exhausted
	start := time.Now()
	records := o.close()
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("close waited for judge")
	}
	if len(records) != 3 || records[0].Status != "canceled" || records[1].Status != "canceled" || records[2].Status != "dropped" {
		t.Fatalf("records=%+v", records)
	}
	close(release)
	<-exited
	if records[0].Answers != nil || records[0].Advisory != "abstain" {
		t.Fatal("late result escaped")
	}
	if again := o.close(); again != nil {
		t.Fatal("close emitted duplicate audit")
	}
}

func TestShadowEvaluationFailureModes(t *testing.T) {
	for _, mode := range []string{"ok", "timeout", "invalid", "missing", "oversize", "forged-evidence", "judge-mutates-input"} {
		t.Run(mode, func(t *testing.T) {
			cfg := ShadowJudgmentConfig{JudgeTarget: "fixture", Prepare: func(ctx context.Context, p ShadowPoint) (ShadowEvidence, error) {
				e, _ := shadowTestEvidence(ctx, p)
				if mode == "missing" {
					e.Missing = true
				}
				if mode == "oversize" {
					e.Objective = strings.Repeat("x", 8192)
				}
				if mode == "forged-evidence" {
					e.Excerpts["cross-session"] = "secret"
				}
				return e, nil
			}, Judge: func(ctx context.Context, r ShadowRequest) ([]byte, error) {
				if mode == "timeout" {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				if mode == "invalid" {
					return []byte(`{"command":"secret"}`), nil
				}
				if mode == "judge-mutates-input" {
					r.Evidence.Excerpts["forged"] = "fabricated"
					return []byte(strings.ReplaceAll(string(shadowTestResponse("yes", "no", "yes")), "r1-t0", "forged")), nil
				}
				return shadowTestResponse("yes", "no", "yes"), nil
			}}
			o := newShadowObserver(context.Background(), &cfg, ShadowPoint{})
			defer o.close()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			r, _, status := o.evaluate(ctx, shadowTestPoint(1))
			want := map[string]string{"ok": "evaluated", "timeout": "timeout", "invalid": "invalid", "missing": "insufficient_evidence", "oversize": "input_budget", "forged-evidence": "insufficient_evidence", "judge-mutates-input": "invalid"}[mode]
			if status != want {
				t.Fatalf("status=%s want=%s response=%+v", status, want, r)
			}
		})
	}
}

func TestShadowTriggerBindingAndRunIsolation(t *testing.T) {
	var calls atomic.Int32
	cfg := ShadowJudgmentConfig{JudgeTarget: "judge-key-never-recorded", Prepare: func(ctx context.Context, p ShadowPoint) (ShadowEvidence, error) {
		return ShadowEvidence{Objective: "fixture", Acceptance: "pass", Excerpts: map[string]string{p.Previous.Tools[0].ID: "previous missing fixture", p.Current.Tools[0].ID: "missing fixture"}}, nil
	}, Judge: func(ctx context.Context, r ShadowRequest) ([]byte, error) {
		calls.Add(1)
		if r.Point.RunID != "a" || r.Point.SessionRevision != 7 || r.Point.ContractDigest != shadowDigest(shadowContract) {
			t.Error("lost run binding")
		}
		id := r.Point.Current.Tools[0].ID
		return []byte(strings.ReplaceAll(string(shadowTestResponse("yes", "no", "yes")), "r1-t0", id)), nil
	}}
	o := newShadowObserver(context.Background(), &cfg, ShadowPoint{RunID: "a", SessionRevision: 7})
	defer o.close()
	results := []toolDispatchResult{{Content: "private-output", Tool: toolUseBlock{Name: "Read"}, IsError: true}}
	o.observe(1, 1, RunGuardSnapshot{}, results)
	o.observe(2, 2, RunGuardSnapshot{Denied: 1}, results)
	waitShadowStatus(t, o, "evaluated")
	o.observe(3, 1, RunGuardSnapshot{Denied: 1}, results)
	o.observe(4, 0, RunGuardSnapshot{Denied: 1}, []toolDispatchResult{{Content: "new successful result", Executed: true}})
	records := o.close()
	if len(records) != 1 || calls.Load() != 1 || records[0].Advisory != "consider_alternative" {
		t.Fatalf("%+v calls=%d", records, calls.Load())
	}
	if records[0].LaterRounds != 2 || records[0].LaterErrorRounds != 1 || records[0].LaterRepeatedErrorRounds != 1 {
		t.Fatalf("later outcomes=%+v", records[0])
	}
	data, _ := json.Marshal(records)
	if strings.Contains(string(data), "private-output") || strings.Contains(string(data), "judge-key") {
		t.Fatal("raw evidence leaked")
	}
	other := newShadowObserver(context.Background(), &cfg, ShadowPoint{RunID: "b"})
	defer other.close()
	if len(other.records) != 0 || other.lastSequence != 0 {
		t.Fatal("cross-run state")
	}
}

type shadowLoopRecorder struct {
	recoveryRunRecorder
	judgments        []ShadowJudgmentRecord
	terminalRecorded bool
}

func (r *shadowLoopRecorder) ShadowJudgmentRecorded(v ShadowJudgmentRecord) {
	if r.terminalRecorded {
		panic("judgment after terminal")
	}
	r.judgments = append(r.judgments, v)
}
func (r *shadowLoopRecorder) RunCompleted(s RunSummary) {
	r.terminalRecorded = true
	r.recoveryRunRecorder.RunCompleted(s)
}
func (r *shadowLoopRecorder) RunFailed(s string) {
	r.terminalRecorded = true
	r.recoveryRunRecorder.RunFailed(s)
}

func TestShadowRunLoopPreservesGuardAndToolResults(t *testing.T) {
	profile := harness.MustResolveProfile(harness.ProfileSpec{ID: "shadow-test", MaxIterations: 4, MaxErrorRounds: 3, RepeatLimit: 1, ReadBeforeWrite: harness.SomeBool(false), ResponsePolicy: harness.ResponseNative, ToolRouting: harness.ToolRoutingDirect})
	var baseline []bool
	for _, enabled := range []bool{false, true} {
		t.Run(shadowModeName(enabled), func(t *testing.T) {
			isolateEvidenceLoopTest(t)
			provider := &scriptedLoopProvider{steps: []scriptedLoopStep{
				toolUseStep("r1", "Read", map[string]string{"file_path": "missing.txt"}),
				toolUseStep("r2", "Read", map[string]string{"file_path": "missing.txt"}),
				toolUseStep("r3", "Read", map[string]string{"file_path": "missing.txt"}),
			}}
			recorder := &shadowLoopRecorder{}
			opts := RunOptions{HarnessProfile: &profile, EvidencePolicy: EvidencePolicyConfig{Policy: EvidencePolicyOff}, DisablePlugins: true, DisableWorkspaceMCP: true, Recorder: recorder}
			if enabled {
				opts.ShadowJudgment = &ShadowJudgmentConfig{JudgeTarget: "fixture", Prepare: func(ctx context.Context, p ShadowPoint) (ShadowEvidence, error) {
					<-ctx.Done()
					return ShadowEvidence{}, ctx.Err()
				}, Judge: func(context.Context, ShadowRequest) ([]byte, error) {
					t.Error("missing evidence reached judge")
					return nil, nil
				}}
			}
			events := make(chan Event, 128)
			go RunLoopWithOptions(context.Background(), provider, "fixture-model", []types.Message{{Role: "user", Content: mustJSON("inspect missing file")}}, t.TempDir(), opts, events)
			var executed []bool
			timeout := time.After(10 * time.Second)
		loop:
			for {
				select {
				case e, ok := <-events:
					if !ok {
						break loop
					}
					if e.Type == "tool_result" {
						data := e.Data.(map[string]interface{})
						executed = append(executed, data["executed"].(bool))
					}
				case <-timeout:
					t.Fatal("shadow blocked kernel")
				}
			}
			if recorder.failed != 1 || provider.callCount() != 3 {
				t.Fatalf("terminal/provider changed: %+v calls=%d", recorder, provider.callCount())
			}
			if !enabled {
				baseline = executed
			} else {
				if !reflect.DeepEqual(baseline, executed) {
					t.Fatalf("tool execution changed: %v vs %v", baseline, executed)
				}
				if len(recorder.judgments) != 2 {
					t.Fatalf("judgments=%+v", recorder.judgments)
				}
			}
		})
	}
}

func shadowModeName(v bool) string {
	if v {
		return "shadow"
	}
	return "off"
}

func TestShadowSuccessfulRunDoesNotConsultJudge(t *testing.T) {
	isolateEvidenceLoopTest(t)
	provider := &scriptedLoopProvider{steps: []scriptedLoopStep{textStep("fixture answer")}}
	recorder := &shadowLoopRecorder{}
	var calls atomic.Int32
	cfg := &ShadowJudgmentConfig{JudgeTarget: "fixture", Prepare: func(ctx context.Context, p ShadowPoint) (ShadowEvidence, error) {
		calls.Add(1)
		return shadowTestEvidence(ctx, p)
	}, Judge: func(context.Context, ShadowRequest) ([]byte, error) { calls.Add(1); return nil, nil }}
	events := make(chan Event, 128)
	go RunLoopWithOptions(context.Background(), provider, "fixture-model", []types.Message{{Role: "user", Content: mustJSON("answer briefly")}}, t.TempDir(), RunOptions{
		ShadowJudgment: cfg, DisablePlugins: true, DisableWorkspaceMCP: true,
		EvidencePolicy: EvidencePolicyConfig{Policy: EvidencePolicyOff}, Recorder: recorder,
	}, events)
	for range events {
	}
	if calls.Load() != 0 || len(recorder.judgments) != 0 || recorder.complete != 1 || provider.callCount() != 1 {
		t.Fatalf("normal run changed: judge calls=%d records=%d completed=%d coder calls=%d", calls.Load(), len(recorder.judgments), recorder.complete, provider.callCount())
	}
}
