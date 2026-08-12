package acp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

const (
	CodeParseError             = -32700
	CodeInvalidRequest         = -32600
	CodeMethodNotFound         = -32601
	CodeInvalidParams          = -32602
	CodeInternalError          = -32603
	CodeRequestCancelled       = -32800
	CodeAuthenticationRequired = -32000
	CodeResourceNotFound       = -32002
)

type RequestID struct {
	kind byte
	text string
	num  int64
}

func StringID(value string) RequestID { return RequestID{kind: 's', text: value} }
func IntegerID(value int64) RequestID { return RequestID{kind: 'i', num: value} }
func NullID() RequestID               { return RequestID{kind: 'n'} }

func (id RequestID) IsNull() bool { return id.kind == 'n' || id.kind == 0 }

func (id RequestID) key() string {
	switch id.kind {
	case 's':
		return "s:" + id.text
	case 'i':
		return "i:" + strconv.FormatInt(id.num, 10)
	default:
		return "n:"
	}
}

func (id RequestID) MarshalJSON() ([]byte, error) {
	switch id.kind {
	case 's':
		return json.Marshal(id.text)
	case 'i':
		return []byte(strconv.FormatInt(id.num, 10)), nil
	case 'n', 0:
		return []byte("null"), nil
	default:
		return nil, errors.New("acp: invalid request id")
	}
}

func (id *RequestID) UnmarshalJSON(data []byte) error {
	parsed, err := parseRequestID(data)
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

func parseRequestID(raw json.RawMessage) (RequestID, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return NullID(), nil
	}
	var value string
	if len(raw) > 0 && raw[0] == '"' {
		if err := json.Unmarshal(raw, &value); err != nil || !validWireString(value, 512) {
			return RequestID{}, errors.New("invalid string request id")
		}
		return StringID(value), nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var number json.Number
	if err := decoder.Decode(&number); err != nil {
		return RequestID{}, errors.New("request id must be a string, integer, or null")
	}
	integer, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil {
		return RequestID{}, errors.New("request id number must be an int64")
	}
	return IntegerID(integer), nil
}

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return "acp: rpc error"
	}
	return fmt.Sprintf("acp: rpc error %d: %s", e.Code, safeDisplay(e.Message, 256))
}

func PublicError(code int, message string) error {
	return &RPCError{Code: code, Message: safeDisplay(message, 256)}
}

var (
	ErrClosed      = errors.New("acp: connection closed")
	ErrFrameTooBig = errors.New("acp: frame exceeds limit")
)

type rpcEnvelope struct {
	ID        RequestID
	HasID     bool
	Method    string
	Params    json.RawMessage
	Result    json.RawMessage
	Error     *RPCError
	IsRequest bool
}

type wireRequest struct {
	JSONRPC string     `json:"jsonrpc"`
	ID      *RequestID `json:"id,omitempty"`
	Method  string     `json:"method"`
	Params  any        `json:"params,omitempty"`
}

type wireResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      RequestID `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *RPCError `json:"error,omitempty"`
}

func decodeEnvelope(frame []byte) (rpcEnvelope, *RPCError) {
	if err := validateJSON(frame, maxJSONDepth, '{'); err != nil {
		if json.Valid(frame) {
			return rpcEnvelope{}, &RPCError{Code: CodeInvalidRequest, Message: "invalid JSON-RPC request"}
		}
		return rpcEnvelope{}, &RPCError{Code: CodeParseError, Message: "parse error"}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(frame, &fields); err != nil {
		return rpcEnvelope{}, &RPCError{Code: CodeParseError, Message: "parse error"}
	}
	allowed := map[string]struct{}{"jsonrpc": {}, "id": {}, "method": {}, "params": {}, "result": {}, "error": {}}
	seenFolded := make(map[string]string, len(fields))
	for name := range fields {
		if _, ok := allowed[name]; !ok {
			return rpcEnvelope{}, &RPCError{Code: CodeInvalidRequest, Message: "invalid JSON-RPC envelope"}
		}
		folded := strings.ToLower(name)
		if prior, ok := seenFolded[folded]; ok && prior != name {
			return rpcEnvelope{}, &RPCError{Code: CodeInvalidRequest, Message: "invalid JSON-RPC envelope"}
		}
		seenFolded[folded] = name
	}
	var version string
	if err := json.Unmarshal(fields["jsonrpc"], &version); err != nil || version != "2.0" {
		return rpcEnvelope{}, &RPCError{Code: CodeInvalidRequest, Message: "invalid JSON-RPC version"}
	}
	envelope := rpcEnvelope{}
	if rawID, ok := fields["id"]; ok {
		id, err := parseRequestID(rawID)
		if err != nil {
			return rpcEnvelope{}, &RPCError{Code: CodeInvalidRequest, Message: "invalid JSON-RPC id"}
		}
		envelope.ID, envelope.HasID = id, true
	}
	methodRaw, hasMethod := fields["method"]
	_, hasResult := fields["result"]
	_, hasError := fields["error"]
	if hasMethod {
		if hasResult || hasError {
			return rpcEnvelope{}, &RPCError{Code: CodeInvalidRequest, Message: "request cannot contain response fields"}
		}
		if err := json.Unmarshal(methodRaw, &envelope.Method); err != nil || !validWireString(envelope.Method, 256) {
			return rpcEnvelope{}, &RPCError{Code: CodeInvalidRequest, Message: "invalid JSON-RPC method"}
		}
		envelope.Params = cloneRaw(fields["params"])
		envelope.IsRequest = true
		return envelope, nil
	}
	if !envelope.HasID || hasResult == hasError || fields["params"] != nil {
		return rpcEnvelope{}, &RPCError{Code: CodeInvalidRequest, Message: "invalid JSON-RPC response"}
	}
	if hasResult {
		envelope.Result = cloneRaw(fields["result"])
		return envelope, nil
	}
	var rpcErr RPCError
	if err := decodeObject(fields["error"], &rpcErr, []string{"code", "message", "data"}, []string{"code", "message"}); err != nil {
		return rpcEnvelope{}, &RPCError{Code: CodeInvalidRequest, Message: "invalid JSON-RPC error response"}
	}
	if rpcErr.Code < math.MinInt32 || rpcErr.Code > math.MaxInt32 || !validWireString(rpcErr.Message, 1024) {
		return rpcEnvelope{}, &RPCError{Code: CodeInvalidRequest, Message: "invalid JSON-RPC error response"}
	}
	rpcErr.Message = safeDisplay(rpcErr.Message, 256)
	rpcErr.Data = nil
	envelope.Error = &rpcErr
	return envelope, nil
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func decodeObject(raw json.RawMessage, target any, allowed, required []string) error {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		raw = []byte("{}")
	}
	if err := validateJSON(raw, maxJSONDepth, '{'); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		allowedSet[name] = struct{}{}
	}
	for name := range fields {
		if _, ok := allowedSet[name]; !ok {
			return fmt.Errorf("unsupported field %q", name)
		}
	}
	for _, name := range required {
		if _, ok := fields[name]; !ok {
			return fmt.Errorf("missing field %q", name)
		}
	}
	if meta, ok := fields["_meta"]; ok {
		if err := validateMeta(meta); err != nil {
			return err
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}
