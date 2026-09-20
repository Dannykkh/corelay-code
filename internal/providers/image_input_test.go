package providers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestOpenAICompatSendsImageAsDataURL(t *testing.T) {
	imageBytes := testProviderPNG(t)
	imageData := base64.StdEncoding.EncodeToString(imageBytes)
	bodyCh := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/chat/completions" {
			t.Errorf("upstream request = %s %s", request.Method, request.URL.Path)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		bodyCh <- body
		w.Header().Set("Content-Type", "text/event-stream")
	}))
	defer upstream.Close()

	content, err := json.Marshal([]types.ContentBlockParam{{
		Type: "image",
		Source: &types.MediaSource{
			Type:      "base64",
			MediaType: "image/png",
			Data:      imageData,
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	provider := &OpenAICompat{ProviderName: "openai", BaseURL: upstream.URL, HTTPDoer: upstream.Client()}
	events, err := provider.StreamMessage(context.Background(), &types.MessagesRequest{
		Model: "gpt-4.1", MaxTokens: 1,
		Messages: []types.Message{{Role: "user", Content: content}},
	}, &types.StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}

	var wire struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(<-bodyCh, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Messages) != 1 || wire.Messages[0].Role != "user" {
		t.Fatalf("wire messages = %+v", wire.Messages)
	}
	var parts []types.OAIContentPart
	if err := json.Unmarshal(wire.Messages[0].Content, &parts); err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 || parts[0].Type != "image_url" || parts[0].ImageURL == nil {
		t.Fatalf("wire content = %+v", parts)
	}
	if got, want := parts[0].ImageURL.URL, "data:image/png;base64,"+imageData; got != want {
		t.Fatalf("image URL = %q, want canonical data URL", got)
	}
}

func TestAnthropicProviderPassesCanonicalImageSource(t *testing.T) {
	imageData := base64.StdEncoding.EncodeToString(testProviderPNG(t))
	bodyCh := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		bodyCh <- body
		w.Header().Set("Content-Type", "text/event-stream")
	}))
	defer upstream.Close()

	content, err := json.Marshal([]types.ContentBlockParam{{
		Type: "image",
		Source: &types.MediaSource{
			Type:      "base64",
			MediaType: "image/png",
			Data:      imageData,
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	provider := &AnthropicProvider{baseURL: upstream.URL, httpDoer: upstream.Client()}
	events, err := provider.StreamMessage(context.Background(), &types.MessagesRequest{
		Model: "claude-sonnet-4-6", MaxTokens: 1,
		Messages: []types.Message{{Role: "user", Content: content}},
	}, &types.StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}

	var wire types.MessagesRequest
	if err := json.Unmarshal(<-bodyCh, &wire); err != nil {
		t.Fatal(err)
	}
	var blocks []types.ContentBlockParam
	if err := json.Unmarshal(wire.Messages[0].Content, &blocks); err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || blocks[0].Type != "image" || blocks[0].Source == nil || blocks[0].Source.Data != imageData {
		t.Fatalf("upstream canonical image = %+v", blocks)
	}
}

func testProviderPNG(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := png.Encode(&out, image.NewNRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
