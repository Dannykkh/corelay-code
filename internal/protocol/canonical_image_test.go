package protocol

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestAnthropicImageCanonicalValidationRejectsMalformedAndOversizedInputs(t *testing.T) {
	validPNG := testCanonicalPNG(t, 1, 1)
	oversizedData := make([]byte, MaxImageBytes+1)
	largeAggregatePart := append(testPNGConfigHeader(1, 1), make([]byte, (2700<<10)-33)...)

	tests := []struct {
		name      string
		blocks    []json.RawMessage
		wantError string
	}{
		{name: "empty base64", blocks: []json.RawMessage{canonicalImageBlock(t, "image/png", nil)}, wantError: "image data is invalid"},
		{name: "malformed base64", blocks: []json.RawMessage{json.RawMessage(`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"@@@="}}`)}, wantError: "image data is invalid"},
		{name: "declared MIME mismatch", blocks: []json.RawMessage{canonicalImageBlock(t, "image/jpeg", validPNG)}, wantError: "image bytes do not match"},
		{name: "unsupported MIME", blocks: []json.RawMessage{canonicalImageBlock(t, "image/svg+xml", validPNG)}, wantError: "media type is not supported"},
		{name: "per-image byte limit", blocks: []json.RawMessage{canonicalImageBlock(t, "image/png", oversizedData)}, wantError: "image data is invalid or exceeds"},
		{name: "image count limit", blocks: repeatedImageBlocks(t, validPNG, MaxImageBlocks+1), wantError: "image count exceeds"},
		{name: "aggregate byte limit", blocks: []json.RawMessage{
			canonicalImageBlock(t, "image/png", largeAggregatePart),
			canonicalImageBlock(t, "image/png", largeAggregatePart),
		}, wantError: "total image bytes exceed"},
		{name: "maximum side length", blocks: []json.RawMessage{canonicalImageBlock(t, "image/png", testPNGConfigHeader(MaxImageDimension+1, 1))}, wantError: "image dimensions exceed"},
		{name: "maximum pixel count", blocks: []json.RawMessage{canonicalImageBlock(t, "image/png", testPNGConfigHeader(6000, 5000))}, wantError: "image pixel count exceeds"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := decodeAnthropicImageBlocks(t, test.blocks)
			if err == nil {
				t.Fatal("invalid image input was accepted")
			}
			if !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error = %q, want it to contain %q", err, test.wantError)
			}
		})
	}
}

func TestAnthropicImageCanonicalAcceptsValidImageLargerThanLegacyMessageLimit(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, 1900, 1800))
	if _, err := rand.New(rand.NewSource(42)).Read(img.Pix); err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	if len(encoded.Bytes()) <= 3<<20 || len(encoded.Bytes()) > MaxImageBytes {
		t.Fatalf("fixture size=%d; want a valid image between 3 MiB and %d bytes", encoded.Len(), MaxImageBytes)
	}
	blocks := []json.RawMessage{canonicalImageBlock(t, "image/png", encoded.Bytes())}
	content, err := json.Marshal(blocks)
	if err != nil {
		t.Fatal(err)
	}
	if len(content) <= MaxStringBytes*4 {
		t.Fatalf("encoded message content=%d; want it larger than the former 4 MiB cap", len(content))
	}
	if err := decodeAnthropicImageBlocks(t, blocks); err != nil {
		t.Fatalf("valid image beyond the legacy message cap was rejected: %v", err)
	}
}

func TestCanonicalImageAcceptsEachDeclaredFormat(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	var jpegData, gifData bytes.Buffer
	if err := jpeg.Encode(&jpegData, img, nil); err != nil {
		t.Fatal(err)
	}
	if err := gif.Encode(&gifData, img, nil); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		mediaType string
		data      []byte
	}{
		{name: "png", mediaType: "image/png", data: testCanonicalPNG(t, 2, 2)},
		{name: "jpeg", mediaType: "image/jpeg", data: jpegData.Bytes()},
		{name: "gif", mediaType: "image/gif", data: gifData.Bytes()},
		{name: "webp", mediaType: "image/webp", data: testWebPConfigHeader(1, 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := validateImagePayload(test.mediaType, base64.StdEncoding.EncodeToString(test.data)); err != nil {
				t.Fatalf("valid %s payload rejected: %v", test.name, err)
			}
		})
	}
}

func TestImageBudgetIncludesInitialImagesAndImageReadImages(t *testing.T) {
	t.Run("aggregate bytes", func(t *testing.T) {
		largeImage := append(testPNGConfigHeader(1, 1), make([]byte, (2700<<10)-33)...)
		content, err := json.Marshal([]json.RawMessage{canonicalImageBlock(t, "image/png", largeImage)})
		if err != nil {
			t.Fatal(err)
		}
		budget, err := NewImageBudget([]types.Message{{Role: "user", Content: content}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := budget.AddImage(largeImage); err == nil || !strings.Contains(err.Error(), "total image bytes exceed") {
			t.Fatalf("adding an ImageRead image after the initial image = %v, want aggregate budget rejection", err)
		}
	})

	t.Run("image count", func(t *testing.T) {
		imageBytes := testCanonicalPNG(t, 1, 1)
		content, err := json.Marshal([]json.RawMessage{canonicalImageBlock(t, "image/png", imageBytes)})
		if err != nil {
			t.Fatal(err)
		}
		budget, err := NewImageBudget([]types.Message{{Role: "user", Content: content}})
		if err != nil {
			t.Fatal(err)
		}
		for count := 1; count < MaxImageBlocks; count++ {
			if _, err := budget.AddImage(imageBytes); err != nil {
				t.Fatalf("image %d rejected before the limit: %v", count+1, err)
			}
		}
		if _, err := budget.AddImage(imageBytes); err == nil || !strings.Contains(err.Error(), "image count exceeds") {
			t.Fatalf("image beyond combined count limit = %v, want count rejection", err)
		}
	})
}

func TestOpenAIAdaptersValidateImageDataURLsIntoCanonicalBlocks(t *testing.T) {
	data := testCanonicalPNG(t, 1, 1)
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
	chatBody, err := json.Marshal(map[string]any{
		"model":      "gpt-4o",
		"max_tokens": 1,
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type":      "image_url",
				"image_url": map[string]string{"url": dataURL},
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	responsesBody, err := json.Marshal(map[string]any{
		"model":             "gpt-4o",
		"max_output_tokens": 1,
		"input": []any{map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{map[string]string{
				"type":      "input_image",
				"image_url": dataURL,
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name    string
		adapter Adapter
		body    []byte
	}{
		{name: "chat completions", adapter: NewChatAdapter(), body: chatBody},
		{name: "responses", adapter: NewResponsesAdapter(), body: responsesBody},
	} {
		t.Run(test.name, func(t *testing.T) {
			decoded, err := test.adapter.Decode(bytes.NewReader(test.body))
			if err != nil {
				t.Fatal(err)
			}
			var blocks []types.ContentBlockParam
			if err := json.Unmarshal(decoded.Messages.Messages[0].Content, &blocks); err != nil {
				t.Fatal(err)
			}
			if len(blocks) != 1 || blocks[0].Type != "image" || blocks[0].Source == nil {
				t.Fatalf("canonical blocks = %+v", blocks)
			}
			if blocks[0].Source.MediaType != "image/png" || blocks[0].Source.Data != base64.StdEncoding.EncodeToString(data) {
				t.Fatalf("canonical image source = %+v", blocks[0].Source)
			}
		})
	}
}

func decodeAnthropicImageBlocks(t *testing.T, blocks []json.RawMessage) error {
	t.Helper()
	content, err := json.Marshal(blocks)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		Messages  []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}{
		Model:     "claude-test",
		MaxTokens: 1,
		Messages: []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}{{Role: "user", Content: content}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewAnthropicAdapter().Decode(bytes.NewReader(body))
	return err
}

func canonicalImageBlock(t *testing.T, mediaType string, data []byte) json.RawMessage {
	t.Helper()
	block := types.ContentBlockParam{
		Type: "image",
		Source: &types.MediaSource{
			Type:      "base64",
			MediaType: mediaType,
			Data:      base64.StdEncoding.EncodeToString(data),
		},
	}
	raw, err := json.Marshal(block)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func repeatedImageBlocks(t *testing.T, data []byte, count int) []json.RawMessage {
	t.Helper()
	blocks := make([]json.RawMessage, 0, count)
	for range count {
		blocks = append(blocks, canonicalImageBlock(t, "image/png", data))
	}
	return blocks
}

func testCanonicalPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewNRGBA(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func testPNGConfigHeader(width, height uint32) []byte {
	const pngHeaderSize = 33
	data := make([]byte, pngHeaderSize)
	copy(data[:8], []byte{137, 80, 78, 71, 13, 10, 26, 10})
	binary.BigEndian.PutUint32(data[8:12], 13)
	copy(data[12:16], "IHDR")
	binary.BigEndian.PutUint32(data[16:20], width)
	binary.BigEndian.PutUint32(data[20:24], height)
	data[24] = 8
	data[25] = 2
	binary.BigEndian.PutUint32(data[29:33], crc32.ChecksumIEEE(data[12:29]))
	return data
}

func testWebPConfigHeader(width, height uint32) []byte {
	data := make([]byte, 30)
	copy(data[:4], "RIFF")
	binary.LittleEndian.PutUint32(data[4:8], uint32(len(data)-8))
	copy(data[8:12], "WEBP")
	copy(data[12:16], "VP8X")
	binary.LittleEndian.PutUint32(data[16:20], 10)
	widthMinusOne := width - 1
	heightMinusOne := height - 1
	data[24] = byte(widthMinusOne)
	data[25] = byte(widthMinusOne >> 8)
	data[26] = byte(widthMinusOne >> 16)
	data[27] = byte(heightMinusOne)
	data[28] = byte(heightMinusOne >> 8)
	data[29] = byte(heightMinusOne >> 16)
	return data
}
