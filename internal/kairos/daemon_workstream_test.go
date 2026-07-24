package kairos

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aniclew/aniclew/internal/observability"
	"github.com/aniclew/aniclew/internal/types"
	"github.com/aniclew/aniclew/internal/workstream"
)

func TestDaemonExecuteTaskRecordsWorkstream(t *testing.T) {
	workDir := t.TempDir()
	store := workstream.NewStore(workDir)
	if _, err := store.Create(workstream.CreateRequest{
		ID:         "ws_kairos",
		Title:      "KAIROS Workstream",
		Summary:    "kairos background summary",
		NextAction: "run daemon task",
		Goal: workstream.Goal{
			Objective:          "prove kairos workstream recording",
			AcceptanceCriteria: []string{"prompt context", "timeline event"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	provider := &daemonFakeProvider{text: "daemon task complete"}
	tracker := observability.NewTracker(t.TempDir())
	daemon := NewDaemon(DefaultDaemonConfig())
	daemon.SwitchProject(workDir)
	daemon.SetProvider(provider, "fake-model")
	daemon.SetTracker(tracker)

	daemon.executeTask(context.Background(), Task{
		ID:           "task-1",
		Type:         "custom",
		Description:  "run a workstream task",
		WorkstreamID: "ws_kairos",
	}, "autonomous")

	if provider.calls != 1 {
		t.Fatalf("provider calls=%d, want 1", provider.calls)
	}
	if !strings.Contains(provider.prompt, "## Workstream Context") ||
		!strings.Contains(provider.prompt, "KAIROS Workstream") ||
		!strings.Contains(provider.prompt, "prove kairos workstream recording") {
		t.Fatalf("provider did not receive workstream context:\n%s", provider.prompt)
	}

	updated, err := store.Get("ws_kairos")
	if err != nil {
		t.Fatal(err)
	}
	if updated.LastVerification.Status != "not-run" || updated.LastVerification.Source != "kairos" {
		t.Fatalf("unexpected verification: %+v", updated.LastVerification)
	}

	timeline, err := store.Timeline("ws_kairos")
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, event := range timeline {
		have[event.Type] = true
	}
	for _, want := range []string{"kairos_task_started", "verification_updated", "kairos_task_completed"} {
		if !have[want] {
			t.Fatalf("missing timeline event %q in %+v", want, timeline)
		}
	}

	runTraces := tracker.RecentRuns(10)
	if len(runTraces) != 1 {
		t.Fatalf("run traces=%d, want 1", len(runTraces))
	}
	if runTraces[0].Kind != "kairos" || runTraces[0].WorkstreamID != "ws_kairos" || runTraces[0].Status != "ok" {
		t.Fatalf("unexpected run trace: %+v", runTraces[0])
	}
	if !hasRunSpan(runTraces[0], "kairos.task") {
		t.Fatalf("kairos run trace missing task span: %+v", runTraces[0].Spans)
	}
}

func TestNewDaemonNormalizesZeroDurations(t *testing.T) {
	daemon := NewDaemon(DaemonConfig{})
	cfg := daemon.GetConfig()
	if cfg.TickInterval <= 0 {
		t.Fatalf("TickInterval was not normalized: %v", cfg.TickInterval)
	}
	if cfg.BlockingBudget <= 0 {
		t.Fatalf("BlockingBudget was not normalized: %v", cfg.BlockingBudget)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		daemon.Start()
		daemon.Stop()
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("daemon Start/Stop with zero config did not return")
	}
}

type daemonFakeProvider struct {
	text   string
	calls  int
	prompt string
}

func (p *daemonFakeProvider) Name() string              { return "fake" }
func (p *daemonFakeProvider) DisplayName() string       { return "Fake" }
func (p *daemonFakeProvider) Models() []types.ModelInfo { return nil }
func (p *daemonFakeProvider) Validate() error           { return nil }

func (p *daemonFakeProvider) StreamMessage(ctx context.Context, req *types.MessagesRequest, opts *types.StreamOptions) (<-chan types.SSEEvent, error) {
	p.calls++
	if len(req.Messages) > 0 {
		_ = json.Unmarshal(req.Messages[0].Content, &p.prompt)
	}

	ch := make(chan types.SSEEvent, 3)
	go func() {
		defer close(ch)
		delta, _ := json.Marshal(map[string]string{"type": "text_delta", "text": p.text})
		ch <- types.SSEEvent{Type: "content_block_delta", Delta: delta}
		ch <- types.SSEEvent{Type: "message_stop"}
	}()
	return ch, nil
}

func hasRunSpan(trace observability.RunTrace, name string) bool {
	for _, span := range trace.Spans {
		if span.Name == name {
			return true
		}
	}
	return false
}
