package agent

import (
	"errors"
	"sync"

	"github.com/Dannykkh/corelay-code/internal/protocol"
	"github.com/Dannykkh/corelay-code/internal/types"
)

var errImageReadPayloadUnavailable = errors.New("image payload transport is unavailable")

// imageReadPayloadSink keeps file bytes out of tool text, events, and durable
// session records while carrying the image to the next provider request.
type imageReadPayloadSink struct {
	mu     sync.Mutex
	budget *protocol.ImageBudget
	images map[string]types.ContentBlockParam
}

func newImageReadPayloadSink(messages []types.Message) (*imageReadPayloadSink, error) {
	budget, err := protocol.NewImageBudget(messages)
	if err != nil {
		return nil, err
	}
	return &imageReadPayloadSink{budget: budget, images: make(map[string]types.ContentBlockParam)}, nil
}

func (sink *imageReadPayloadSink) store(toolCallID string, data []byte) (types.ContentBlockParam, error) {
	if sink == nil || sink.budget == nil || toolCallID == "" {
		return types.ContentBlockParam{}, errImageReadPayloadUnavailable
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if _, exists := sink.images[toolCallID]; exists {
		return types.ContentBlockParam{}, errors.New("image payload already exists for this tool call")
	}
	block, err := sink.budget.AddImage(data)
	if err != nil {
		return types.ContentBlockParam{}, err
	}
	sink.images[toolCallID] = block
	return block, nil
}

func (sink *imageReadPayloadSink) take(toolCallID string) (types.ContentBlockParam, bool) {
	if sink == nil || toolCallID == "" {
		return types.ContentBlockParam{}, false
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	block, ok := sink.images[toolCallID]
	delete(sink.images, toolCallID)
	return block, ok
}
