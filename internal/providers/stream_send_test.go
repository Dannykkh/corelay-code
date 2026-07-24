package providers

import (
	"context"
	"testing"
	"time"

	"github.com/aniclew/aniclew/internal/types"
)

func TestSendSSEEventReturnsWhenContextCanceledAndReceiverBlocked(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ch := make(chan types.SSEEvent)
	done := make(chan bool, 1)
	go func() {
		done <- sendSSEEvent(ctx, ch, types.SSEEvent{Type: "message_stop"})
	}()

	select {
	case ok := <-done:
		if ok {
			t.Fatal("sendSSEEvent reported success after context cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("sendSSEEvent blocked after context cancellation")
	}
}
