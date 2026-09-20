package protocol

import (
	"bytes"
	"encoding/base64"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"strings"

	_ "golang.org/x/image/webp"
)

func decodeImageBase64(data string) ([]byte, bool) {
	if data == "" || len(data) > MaxImageEncodedBytes || strings.ContainsAny(data, "\r\n") {
		return nil, false
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(data)
	if err != nil || len(decoded) == 0 || len(decoded) > MaxImageBytes {
		return nil, false
	}
	return decoded, true
}

func validateImagePayload(mediaType, data string) (int, error) {
	decoded, ok := decodeImageBase64(data)
	if !ok {
		return 0, NewError(400, "invalid_content_block", "image data is invalid or exceeds the image byte limit")
	}
	if err := ValidateImageBytes(mediaType, decoded); err != nil {
		return 0, err
	}
	return len(decoded), nil
}

// ValidateImageBytes verifies raw image bytes against the same format,
// dimension, and pixel limits as canonical request image blocks.
func ValidateImageBytes(mediaType string, data []byte) error {
	if len(data) == 0 || len(data) > MaxImageBytes {
		return NewError(400, "invalid_content_block", "image data is invalid or exceeds the image byte limit")
	}
	expectedFormat := ""
	switch mediaType {
	case "image/png":
		expectedFormat = "png"
	case "image/jpeg":
		expectedFormat = "jpeg"
	case "image/gif":
		expectedFormat = "gif"
	case "image/webp":
		expectedFormat = "webp"
	default:
		return NewError(400, "invalid_content_block", "image media type is not supported")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || format != expectedFormat {
		return NewError(400, "invalid_content_block", "image bytes do not match the declared media type")
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > MaxImageDimension || config.Height > MaxImageDimension {
		return NewError(400, "invalid_content_block", "image dimensions exceed the image dimension limit")
	}
	if int64(config.Width) > MaxImagePixels/int64(config.Height) {
		return NewError(400, "invalid_content_block", "image pixel count exceeds the image pixel limit")
	}
	return nil
}
