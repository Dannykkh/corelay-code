package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/Dannykkh/corelay-code/internal/protocol"
)

func classifyProtocolWriteError(writeErr, requestErr error) (int, string, string) {
	if requestErr != nil {
		return 499, "request_cancelled", "client disconnected"
	}
	if errors.Is(writeErr, context.Canceled) || errors.Is(writeErr, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout, "upstream_timeout", "upstream provider timed out"
	}
	return protocol.ErrorDetails(writeErr)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	s.handleCanonicalProtocol(w, r, protocol.AnthropicMessages)
}

func (s *Server) handleOpenAIChatCompletions(w http.ResponseWriter, r *http.Request) {
	s.handleCanonicalProtocol(w, r, protocol.OpenAIChat)
}

func (s *Server) handleOpenAIResponses(w http.ResponseWriter, r *http.Request) {
	s.handleCanonicalProtocol(w, r, protocol.OpenAIResponses)
}
