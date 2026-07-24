package providers

import (
	"context"

	"github.com/aniclew/aniclew/internal/types"
)

func sendSSEEvent(ctx context.Context, ch chan<- types.SSEEvent, event types.SSEEvent) bool {
	select {
	case <-ctx.Done():
		return false
	case ch <- event:
		return true
	}
}
