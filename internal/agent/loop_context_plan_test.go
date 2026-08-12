package agent

import (
	"context"
	"sync"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestRunLoopBlocksUnresolvedOverflowWithoutProviderCall(t *testing.T) {
	t.Setenv("CORELAY_CONFIG_DIR", t.TempDir())
	t.Setenv("CORELAY_MEMORY", "off")
	t.Setenv("CORELAY_AUTOSKILL", "off")
	t.Setenv("CORELAY_AUTOVERIFY", "off")
	t.Setenv("CORELAY_MAX_TOOLS", "30")

	profile := phase2Profile("blocked-overflow", 16_384, 2_048)
	estimator := &phase2TokenEstimator{estimate: func(ContextEstimateRequest) TokenEstimate {
		return TokenEstimate{
			InputTokens: 1_000_000,
			Source:      "forced-overflow",
			Confidence:  "exact",
		}
	}}
	provider := &phase2NoCallProvider{}
	recorder := &phase2ContextRecorder{}
	workDir := t.TempDir()
	events := make(chan Event, 512)
	done := make(chan struct{})
	go func() {
		RunLoopWithOptions(
			context.Background(),
			provider,
			"model",
			[]types.Message{{Role: "user", Content: mustJSON("implement a small change")}},
			workDir,
			RunOptions{
				HarnessProfile: &profile,
				TokenEstimator: estimator,
				Recorder:       recorder,
			},
			events,
		)
		close(done)
	}()

	foundBlocked := false
	for event := range events {
		if event.Type != "context_blocked" {
			continue
		}
		blocked, ok := event.Data.(ContextBlockedEvent)
		if !ok {
			t.Fatalf("context_blocked payload type = %T", event.Data)
		}
		if blocked.Code != "context_overflow_after_compaction" || !blocked.Plan.Blocked {
			t.Fatalf("context_blocked payload = %#v", blocked)
		}
		foundBlocked = true
	}
	<-done
	if !foundBlocked {
		t.Fatal("context_blocked event was not emitted")
	}
	if got := provider.Calls(); got != 0 {
		t.Fatalf("provider StreamMessage calls = %d, want 0", got)
	}
	if recorder.failureCount() != 1 {
		t.Fatalf("RunFailed calls = %d, want 1", recorder.failureCount())
	}
	if len(recorder.snapshotsCopy()) != 1 {
		t.Fatalf("compaction snapshots = %d, want 1", len(recorder.snapshotsCopy()))
	}
}

type phase2NoCallProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *phase2NoCallProvider) Name() string              { return "phase2-no-call" }
func (p *phase2NoCallProvider) DisplayName() string       { return "phase2-no-call" }
func (p *phase2NoCallProvider) Models() []types.ModelInfo { return nil }
func (p *phase2NoCallProvider) Validate() error           { return nil }
func (p *phase2NoCallProvider) StreamMessage(context.Context, *types.MessagesRequest, *types.StreamOptions) (<-chan types.SSEEvent, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	stream := make(chan types.SSEEvent)
	close(stream)
	return stream, nil
}
func (p *phase2NoCallProvider) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

type phase2ContextRecorder struct {
	mu        sync.Mutex
	failures  []string
	plans     []ContextPlan
	snapshots []CompactionSnapshot
}

func (*phase2ContextRecorder) RunStarted()                         {}
func (*phase2ContextRecorder) ReceiptWritten(string, AgentReceipt) {}
func (*phase2ContextRecorder) RunCompleted(RunSummary)             {}
func (r *phase2ContextRecorder) RunFailed(message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures = append(r.failures, message)
}
func (r *phase2ContextRecorder) ContextPlanned(plan ContextPlan) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.plans = append(r.plans, plan)
}
func (r *phase2ContextRecorder) CompactionRecorded(s CompactionSnapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.snapshots = append(r.snapshots, s)
}
func (r *phase2ContextRecorder) failureCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.failures)
}
func (r *phase2ContextRecorder) snapshotsCopy() []CompactionSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]CompactionSnapshot(nil), r.snapshots...)
}

var _ RunRecorder = (*phase2ContextRecorder)(nil)
var _ RunFailureRecorder = (*phase2ContextRecorder)(nil)
var _ RunContextRecorder = (*phase2ContextRecorder)(nil)
