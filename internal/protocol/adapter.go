package protocol

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Dannykkh/corelay-code/internal/types"
)

// Wire limits are deliberately smaller than the model context window. The
// adapter boundary must reject oversized HTTP frames before they enter the
// provider/router kernel.
const (
	MaxRequestBytes = 8 << 20
	MaxOutputBytes  = 16 << 20
	MaxEvents       = 16_384
	MaxMessages     = 2_048
	MaxTools        = 256
	MaxStringBytes  = 1 << 20
)

type Name string

const (
	AnthropicMessages Name = "anthropic_messages"
	OpenAIChat        Name = "openai_chat_completions"
	OpenAIResponses   Name = "openai_responses"
)

type Request struct {
	Messages     *types.MessagesRequest
	Stream       bool
	IncludeUsage bool
}

type ResponseMeta struct {
	ID      string
	Model   string
	Created time.Time
}

type Usage struct {
	InputTokens  int
	OutputTokens int
}

type Adapter interface {
	Name() Name
	Decode(io.Reader) (*Request, error)
	WriteResponse(context.Context, http.ResponseWriter, ResponseMeta, <-chan types.SSEEvent, *Request) (Usage, error)
	WriteError(http.ResponseWriter, int, string, string)
}

type Registry struct {
	adapters map[Name]Adapter
}

func NewRegistry(adapters ...Adapter) (*Registry, error) {
	registry := &Registry{adapters: make(map[Name]Adapter, len(adapters))}
	for _, adapter := range adapters {
		if adapter == nil || adapter.Name() == "" {
			return nil, errors.New("protocol: adapter name is required")
		}
		if _, exists := registry.adapters[adapter.Name()]; exists {
			return nil, fmt.Errorf("protocol: duplicate adapter %q", adapter.Name())
		}
		registry.adapters[adapter.Name()] = adapter
	}
	return registry, nil
}

func (r *Registry) Get(name Name) (Adapter, bool) {
	if r == nil {
		return nil, false
	}
	adapter, ok := r.adapters[name]
	return adapter, ok
}

func (r *Registry) Names() []Name {
	names := make([]Name, 0, len(r.adapters))
	for name := range r.adapters {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	return names
}

var (
	defaultRegistryOnce sync.Once
	defaultRegistry     *Registry
)

func DefaultRegistry() *Registry {
	defaultRegistryOnce.Do(func() {
		defaultRegistry, _ = NewRegistry(NewAnthropicAdapter(), NewChatAdapter(), NewResponsesAdapter())
	})
	return defaultRegistry
}

type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func NewError(status int, code, message string) error {
	return &Error{Status: status, Code: code, Message: message}
}

func ErrorDetails(err error) (status int, code, message string) {
	var protocolErr *Error
	if errors.As(err, &protocolErr) {
		return protocolErr.Status, protocolErr.Code, protocolErr.Message
	}
	return http.StatusBadRequest, "invalid_request", "invalid protocol request"
}

func DecodeStrictBounded(r io.Reader, target any) error {
	return decodeStrictBoundedFor("", r, target)
}

func decodeStrictBoundedFor(name Name, r io.Reader, target any) error {
	limited := &io.LimitedReader{R: r, N: MaxRequestBytes + 1}
	data, err := io.ReadAll(limited)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return NewError(http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the protocol limit")
		}
		return NewError(http.StatusBadRequest, "invalid_request", "request body could not be read")
	}
	if len(data) > MaxRequestBytes {
		return NewError(http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the protocol limit")
	}
	if err := validateJSONValue(data, 0); err != nil {
		return NewError(http.StatusBadRequest, "invalid_json", "request body contains duplicate keys, excessive nesting, or malformed JSON")
	}
	if err := validateEnvelopeCaseFold(data, name); err != nil {
		return NewError(http.StatusBadRequest, "invalid_json", "request body contains semantically duplicate envelope keys")
	}
	decoder := json.NewDecoder(newBytesReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return NewError(http.StatusBadRequest, "invalid_json", "request body is not a supported JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return NewError(http.StatusBadRequest, "invalid_json", "request body must contain exactly one JSON value")
	}
	return nil
}

func validateEnvelopeCaseFold(data []byte, name Name) error {
	var root any
	if err := json.Unmarshal(data, &root); err != nil {
		return err
	}
	object, ok := root.(map[string]any)
	if !ok {
		return errors.New("request root is not an object")
	}
	if err := rejectFoldedDuplicateKeys(object); err != nil {
		return err
	}
	switch name {
	case OpenAIChat:
		if err := validateObjectArrayField(object, "messages", func(message map[string]any) error {
			if err := rejectFoldedDuplicateKeys(message); err != nil {
				return err
			}
			if err := validateObjectArrayField(message, "tool_calls", func(call map[string]any) error {
				if err := rejectFoldedDuplicateKeys(call); err != nil {
					return err
				}
				return validateObjectField(call, "function")
			}); err != nil {
				return err
			}
			return validateContentPartEnvelopes(fieldFold(message, "content"))
		}); err != nil {
			return err
		}
		if err := validateObjectArrayField(object, "tools", func(tool map[string]any) error {
			if err := rejectFoldedDuplicateKeys(tool); err != nil {
				return err
			}
			return validateObjectField(tool, "function")
		}); err != nil {
			return err
		}
		if err := validateObjectField(object, "stream_options"); err != nil {
			return err
		}
		return validateNestedToolChoice(object, true)
	case OpenAIResponses:
		if items, ok := fieldFold(object, "input").([]any); ok {
			for _, raw := range items {
				item, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				if err := rejectFoldedDuplicateKeys(item); err != nil {
					return err
				}
				if err := validateContentPartEnvelopes(fieldFold(item, "content")); err != nil {
					return err
				}
			}
		}
		if err := validateObjectArrayField(object, "tools", rejectFoldedDuplicateKeys); err != nil {
			return err
		}
		return validateNestedToolChoice(object, false)
	case AnthropicMessages:
		if err := validateObjectArrayField(object, "messages", func(message map[string]any) error {
			if err := rejectFoldedDuplicateKeys(message); err != nil {
				return err
			}
			return validateAnthropicContentEnvelopes(fieldFold(message, "content"))
		}); err != nil {
			return err
		}
		if err := validateObjectArrayField(object, "tools", rejectFoldedDuplicateKeys); err != nil {
			return err
		}
		if err := validateAnthropicContentEnvelopes(fieldFold(object, "system")); err != nil {
			return err
		}
		if err := validateObjectField(object, "tool_choice"); err != nil {
			return err
		}
		return validateObjectField(object, "thinking")
	default:
		return nil
	}
}

func rejectFoldedDuplicateKeys(object map[string]any) error {
	seen := make(map[string]struct{}, len(object))
	for key := range object {
		folded := strings.ToLower(key)
		if _, duplicate := seen[folded]; duplicate {
			return errors.New("case-folded duplicate object key")
		}
		seen[folded] = struct{}{}
	}
	return nil
}

func fieldFold(object map[string]any, name string) any {
	for key, value := range object {
		if strings.EqualFold(key, name) {
			return value
		}
	}
	return nil
}

func validateObjectField(object map[string]any, name string) error {
	value, ok := fieldFold(object, name).(map[string]any)
	if !ok {
		return nil
	}
	return rejectFoldedDuplicateKeys(value)
}

func validateObjectArrayField(object map[string]any, name string, validate func(map[string]any) error) error {
	values, ok := fieldFold(object, name).([]any)
	if !ok {
		return nil
	}
	for _, raw := range values {
		value, ok := raw.(map[string]any)
		if ok {
			if err := validate(value); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateContentPartEnvelopes(value any) error {
	parts, ok := value.([]any)
	if !ok {
		return nil
	}
	for _, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if err := rejectFoldedDuplicateKeys(part); err != nil {
			return err
		}
		if err := validateObjectField(part, "image_url"); err != nil {
			return err
		}
	}
	return nil
}

func validateAnthropicContentEnvelopes(value any) error {
	parts, ok := value.([]any)
	if !ok {
		return nil
	}
	for _, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if err := rejectFoldedDuplicateKeys(part); err != nil {
			return err
		}
		if err := validateObjectField(part, "source"); err != nil {
			return err
		}
		if err := validateObjectField(part, "cache_control"); err != nil {
			return err
		}
	}
	return nil
}

func validateNestedToolChoice(object map[string]any, chat bool) error {
	choice, ok := fieldFold(object, "tool_choice").(map[string]any)
	if !ok {
		return nil
	}
	if err := rejectFoldedDuplicateKeys(choice); err != nil {
		return err
	}
	if chat {
		return validateObjectField(choice, "function")
	}
	return nil
}

const maxJSONDepth = 64

func validateJSONValue(data []byte, requiredRoot byte) error {
	decoder := json.NewDecoder(newBytesReader(data))
	decoder.UseNumber()
	if err := consumeJSONValue(decoder, 0, requiredRoot); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON data")
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder, depth int, requiredRoot byte) error {
	if depth > maxJSONDepth {
		return errors.New("JSON nesting exceeds limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if depth == 0 && requiredRoot != 0 && (!isDelimiter || byte(delimiter) != requiredRoot) {
		return errors.New("unexpected JSON root type")
	}
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		keys := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, duplicate := keys[key]; duplicate {
				return errors.New("duplicate object key")
			}
			keys[key] = struct{}{}
			if err := consumeJSONValue(decoder, depth+1, 0); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return fmt.Errorf("unterminated object: closing=%v err=%w", closing, err)
		}
	case '[':
		for decoder.More() {
			if err := consumeJSONValue(decoder, depth+1, 0); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return fmt.Errorf("unterminated array: closing=%v err=%w", closing, err)
		}
	default:
		return errors.New("unexpected delimiter")
	}
	return nil
}

func validBoundedWireString(value string, max int) bool {
	if value == "" || len(value) > max || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// byteReader avoids exposing a large body through error formatting.
type byteReader struct {
	b []byte
	i int
}

func newBytesReader(b []byte) *byteReader { return &byteReader{b: b} }

func (r *byteReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}

func GenerateID(prefix string) (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate protocol id: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

func ensureResponseMeta(meta ResponseMeta, prefix string) (ResponseMeta, error) {
	if meta.ID == "" {
		id, err := GenerateID(prefix)
		if err != nil {
			return meta, err
		}
		meta.ID = id
	}
	if meta.Created.IsZero() {
		meta.Created = time.Now().UTC()
	}
	return meta, nil
}
