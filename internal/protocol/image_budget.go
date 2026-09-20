package protocol

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"sync"

	"github.com/Dannykkh/corelay-code/internal/types"
)

// ImageBudget accounts for image blocks already present in a canonical history
// and additional images introduced by tools during the same agent run.
type ImageBudget struct {
	mu         sync.Mutex
	imageCount int
	totalBytes int
}

// NewImageBudget validates and accounts for all top-level image blocks in the
// initial canonical history. Tool-added images then share the same limits.
func NewImageBudget(messages []types.Message) (*ImageBudget, error) {
	budget := &ImageBudget{}
	for _, message := range messages {
		var text string
		if json.Unmarshal(message.Content, &text) == nil {
			continue
		}
		var blocks []json.RawMessage
		if err := decodeRawArray(message.Content, &blocks); err != nil {
			return nil, NewError(400, "invalid_message_content", "message content must be a string or nonempty block array")
		}
		for _, raw := range blocks {
			var discriminator struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(raw, &discriminator) != nil {
				return nil, NewError(400, "invalid_content_block", "content block is malformed")
			}
			if discriminator.Type != "image" {
				continue
			}
			if message.Role != "user" {
				return nil, NewError(400, "invalid_content_block", "image blocks require user role")
			}
			imageBytes, err := validateCanonicalImageBlock(raw)
			if err != nil {
				return nil, err
			}
			if err := budget.reserve(imageBytes); err != nil {
				return nil, err
			}
		}
	}
	return budget, nil
}

// AddImage validates raw file bytes, applies the shared run budget, and returns
// a canonical image block ready to append to a provider message.
func (budget *ImageBudget) AddImage(data []byte) (types.ContentBlockParam, error) {
	if budget == nil || len(data) == 0 || len(data) > MaxImageBytes {
		return types.ContentBlockParam{}, NewError(400, "invalid_content_block", "image data is invalid or exceeds the image byte limit")
	}
	_, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return types.ContentBlockParam{}, NewError(400, "invalid_content_block", "image data is invalid")
	}
	mediaType := ""
	switch format {
	case "png":
		mediaType = "image/png"
	case "jpeg":
		mediaType = "image/jpeg"
	case "gif":
		mediaType = "image/gif"
	case "webp":
		mediaType = "image/webp"
	default:
		return types.ContentBlockParam{}, NewError(400, "invalid_content_block", "image format is not supported")
	}

	encoded := base64.StdEncoding.EncodeToString(data)
	imageBytes, err := validateImagePayload(mediaType, encoded)
	if err != nil {
		return types.ContentBlockParam{}, err
	}
	budget.mu.Lock()
	err = budget.reserve(imageBytes)
	budget.mu.Unlock()
	if err != nil {
		return types.ContentBlockParam{}, err
	}
	return types.ContentBlockParam{
		Type: "image",
		Source: &types.MediaSource{
			Type:      "base64",
			MediaType: mediaType,
			Data:      encoded,
		},
	}, nil
}

func (budget *ImageBudget) reserve(imageBytes int) error {
	if imageBytes <= 0 || budget.imageCount >= MaxImageBlocks {
		return NewError(400, "invalid_content_block", "image count exceeds the request limit")
	}
	if budget.totalBytes > MaxTotalImageBytes-imageBytes {
		return NewError(400, "invalid_content_block", "total image bytes exceed the request limit")
	}
	budget.imageCount++
	budget.totalBytes += imageBytes
	return nil
}
