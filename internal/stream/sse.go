package stream

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/aniclew/aniclew/internal/types"
)

// WriteSSEEvent writes a single Anthropic SSE event to an http.ResponseWriter.
func WriteSSEEvent(w http.ResponseWriter, event types.SSEEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, data)
	if err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// ReadOpenAISSE reads OpenAI-format SSE from a reader and sends parsed chunks to a channel.
func ReadOpenAISSE(ctx context.Context, body io.ReadCloser, ch chan<- types.OAIStreamChunk) {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			body.Close()
		case <-done:
		}
	}()

	defer close(done)
	defer body.Close()
	defer close(ch)

	scanner := bufio.NewScanner(body)
	// Increase buffer for large responses
	scanner.Buffer(make([]byte, 0, 256*1024), 256*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if line == "data: [DONE]" {
			return
		}
		if strings.HasPrefix(line, "data: ") {
			jsonStr := line[6:]
			var chunk types.OAIStreamChunk
			if err := json.Unmarshal([]byte(jsonStr), &chunk); err != nil {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case ch <- chunk:
			}
		}
	}
}
