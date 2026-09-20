package agent

import (
	"encoding/json"

	"github.com/Dannykkh/corelay-code/internal/types"
)

// ResolveModelImageInputCapability returns the capability registered for the
// exact provider/model target. Missing models and invalid capability values
// are treated as unknown so custom endpoints fail closed when images are used.
func ResolveModelImageInputCapability(provider types.Provider, model string) types.ImageInputCapability {
	if provider == nil {
		return types.ImageInputUnknown
	}
	for _, info := range provider.Models() {
		if info.ID != model {
			continue
		}
		switch info.ImageInput {
		case types.ImageInputSupported, types.ImageInputUnsupported:
			return info.ImageInput
		default:
			return types.ImageInputUnknown
		}
	}
	return types.ImageInputUnknown
}

// MessagesContainImageInput detects canonical image content blocks without
// decoding or copying their potentially large base64 payloads.
func MessagesContainImageInput(messages []types.Message) bool {
	for _, message := range messages {
		var text string
		if json.Unmarshal(message.Content, &text) == nil {
			continue
		}
		var blocks []struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(message.Content, &blocks) != nil {
			continue
		}
		for _, block := range blocks {
			if block.Type == "image" {
				return true
			}
		}
	}
	return false
}

// ImageInputCapabilityError is a stable, user-actionable refusal returned
// before an unsupported or unverified target receives image input.
type ImageInputCapabilityError struct {
	Capability types.ImageInputCapability
}

func (e *ImageInputCapabilityError) Error() string {
	if e == nil {
		return ""
	}
	if e.Capability == types.ImageInputUnsupported {
		return "The selected provider/model does not support image input. Choose a model marked supported or remove the image."
	}
	return "Image input support for the selected provider/model is unverified. Choose a model marked supported or remove the image."
}

// Code is suitable for structured API errors and durable run terminals.
func (e *ImageInputCapabilityError) Code() string {
	if e != nil && e.Capability == types.ImageInputUnsupported {
		return "image_input_unsupported"
	}
	return "image_input_capability_unknown"
}

// ValidateImageInputCapability returns an explicit error only when the request
// needs image input. Text-only requests remain valid for unknown models.
func ValidateImageInputCapability(
	capability types.ImageInputCapability,
	needsImage bool,
) *ImageInputCapabilityError {
	if !needsImage || capability == types.ImageInputSupported {
		return nil
	}
	return &ImageInputCapabilityError{Capability: capability}
}
