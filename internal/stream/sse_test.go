package stream

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestReadOpenAISSEReturnsWhenContextCanceledAndReceiverBlocked(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	body := io.NopCloser(strings.NewReader(`data: {"choices":[{"delta":{"content":"blocked"}}]}` + "\n"))
	ch := make(chan types.OAIStreamChunk)
	done := make(chan struct{})

	go func() {
		defer close(done)
		ReadOpenAISSE(ctx, body, ch)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ReadOpenAISSE blocked after context cancellation")
	}
}
