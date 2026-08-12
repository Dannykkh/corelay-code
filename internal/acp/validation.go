package acp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	DefaultMaxFrameBytes = 8 << 20
	DefaultMaxInflight   = 128
	DefaultWriteQueue    = 128
	MaxMetaBytes         = 64 << 10
	MaxStringBytes       = 1 << 20
	maxJSONDepth         = 64
	maxCollectionItems   = 2048
)

func validateJSON(data []byte, depthLimit int, requiredRoot byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := consumeJSON(decoder, 0, depthLimit, requiredRoot); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}

func consumeJSON(decoder *json.Decoder, depth, depthLimit int, requiredRoot byte) error {
	if depth > depthLimit {
		return errors.New("JSON nesting exceeds limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, isDelim := token.(json.Delim)
	if depth == 0 && requiredRoot != 0 && (!isDelim || byte(delim) != requiredRoot) {
		return errors.New("unexpected JSON root")
	}
	if !isDelim {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return errors.New("duplicate object key")
			}
			seen[key] = struct{}{}
			if err := consumeJSON(decoder, depth+1, depthLimit, 0); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("unterminated object")
		}
	case '[':
		for decoder.More() {
			if err := consumeJSON(decoder, depth+1, depthLimit, 0); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("unterminated array")
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}

func validateMeta(raw json.RawMessage) error {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	if len(raw) > MaxMetaBytes {
		return errors.New("acp: _meta exceeds limit")
	}
	if err := validateJSON(raw, 16, '{'); err != nil {
		return errors.New("acp: _meta must be a bounded JSON object")
	}
	return nil
}

func validWireString(value string, max int) bool {
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

func validOptionalWireString(value string, max int) bool {
	return value == "" || validWireString(value, max)
}

var secretDisplayPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(authorization\s*[:=]\s*(?:bearer|basic)\s+)[^\s,;]+`),
	regexp.MustCompile(`(?i)((?:api[_-]?key|token|password|secret)\s*[:=]\s*)[^\s,;]+`),
	regexp.MustCompile(`(?i)(https?://)[^/@\s]+:[^/@\s]+@`),
}

func safeDisplay(value string, max int) string {
	if !utf8.ValidString(value) {
		return "invalid text"
	}
	for _, pattern := range secretDisplayPatterns {
		value = pattern.ReplaceAllString(value, `${1}[REDACTED]`)
	}
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, value)
	if len(value) > max {
		value = value[:max]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
	}
	return value
}

func validateSessionID(id string) error {
	if !validWireString(id, 512) {
		return errors.New("sessionId is invalid")
	}
	return nil
}

func validateImplementation(info *Implementation) error {
	if info == nil {
		return nil
	}
	if !validWireString(info.Name, 256) || !validWireString(info.Version, 128) || !validOptionalWireString(info.Title, 256) {
		return errors.New("implementation metadata is invalid")
	}
	return validateMeta(json.RawMessage(info.Meta))
}

func validateMCPServers(servers []MCPServer, capabilities MCPCapabilities) error {
	if len(servers) > 64 {
		return errors.New("too many MCP servers")
	}
	for _, server := range servers {
		if !validWireString(server.Name, 256) {
			return errors.New("MCP server name is invalid")
		}
		switch server.Type {
		case "":
			if !validWireString(server.Command, 4096) || server.Args == nil || server.Env == nil || len(server.Args) > 256 || len(server.Env) > 256 || server.URL != "" || len(server.Headers) != 0 {
				return errors.New("stdio MCP server is invalid")
			}
		case "http":
			if !capabilities.HTTP || server.Command != "" || len(server.Args) != 0 || len(server.Env) != 0 || !validURL(server.URL) || server.Headers == nil || len(server.Headers) > 256 {
				return errors.New("HTTP MCP server is not supported or invalid")
			}
		case "sse":
			if !capabilities.SSE || server.Command != "" || len(server.Args) != 0 || len(server.Env) != 0 || !validURL(server.URL) || server.Headers == nil || len(server.Headers) > 256 {
				return errors.New("SSE MCP server is not supported or invalid")
			}
		default:
			return errors.New("MCP server type is not supported")
		}
		for _, arg := range server.Args {
			if len(arg) > 4096 || !utf8.ValidString(arg) {
				return errors.New("MCP server argument is invalid")
			}
		}
		for _, env := range server.Env {
			if !validWireString(env.Name, 256) || len(env.Value) > MaxStringBytes || !utf8.ValidString(env.Value) {
				return errors.New("MCP server environment is invalid")
			}
		}
		for _, header := range server.Headers {
			if !validWireString(header.Name, 256) || len(header.Value) > MaxStringBytes || !utf8.ValidString(header.Value) {
				return errors.New("MCP server header is invalid")
			}
		}
		if err := validateMeta(json.RawMessage(server.Meta)); err != nil {
			return err
		}
	}
	return nil
}

func validURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.IsAbs() && parsed.Host != "" && parsed.User == nil && (parsed.Scheme == "http" || parsed.Scheme == "https")
}

func validateDirectories(cwd string, additional []string, supported bool) error {
	if !filepath.IsAbs(cwd) || len(cwd) > 4096 {
		return errors.New("cwd must be an absolute bounded path")
	}
	if len(additional) > 64 || (len(additional) > 0 && !supported) {
		return errors.New("additional directories are not supported or exceed the limit")
	}
	for _, path := range additional {
		if !filepath.IsAbs(path) || len(path) > 4096 {
			return errors.New("additional directory must be an absolute bounded path")
		}
	}
	return nil
}

func validateContentBlock(block ContentBlock, caps PromptCapabilities, prompt bool) error {
	if err := validateMeta(json.RawMessage(block.Meta)); err != nil {
		return err
	}
	if err := validateAnnotations(block.Annotations); err != nil {
		return err
	}
	switch block.Type {
	case "text":
		if len(block.Text) > MaxStringBytes || !utf8.ValidString(block.Text) || block.Data != "" || block.Resource != nil || block.MimeType != "" || block.URI != "" || block.Name != "" || block.Title != "" || block.Description != "" || block.Size != nil {
			return errors.New("text content is invalid")
		}
	case "resource_link":
		if !validWireString(block.Name, 512) || !validWireString(block.URI, 4096) || block.Data != "" || block.Resource != nil || block.Text != "" || !validOptionalWireString(block.MimeType, 256) || !validOptionalWireString(block.Title, 512) || !validOptionalWireString(block.Description, 1024) {
			return errors.New("resource link is invalid")
		}
	case "image":
		if prompt && !caps.Image {
			return errors.New("image prompt content is not supported")
		}
		if !validWireString(block.MimeType, 256) || !validOptionalWireString(block.URI, 4096) || len(block.Data) == 0 || len(block.Data) > MaxStringBytes*4 || !validBase64(block.Data) || block.Text != "" || block.Name != "" || block.Title != "" || block.Description != "" || block.Size != nil || block.Resource != nil {
			return errors.New("image content is invalid")
		}
	case "audio":
		if prompt && !caps.Audio {
			return errors.New("audio prompt content is not supported")
		}
		if !validWireString(block.MimeType, 256) || len(block.Data) == 0 || len(block.Data) > MaxStringBytes*4 || !validBase64(block.Data) || block.Text != "" || block.URI != "" || block.Name != "" || block.Title != "" || block.Description != "" || block.Size != nil || block.Resource != nil {
			return errors.New("audio content is invalid")
		}
	case "resource":
		if prompt && !caps.EmbeddedContext {
			return errors.New("embedded context is not supported")
		}
		if block.Resource == nil || !validWireString(block.Resource.URI, 4096) || (block.Resource.Text == nil) == (block.Resource.Blob == nil) || (block.Resource.Text != nil && (len(*block.Resource.Text) > MaxStringBytes || !utf8.ValidString(*block.Resource.Text))) || (block.Resource.Blob != nil && (len(*block.Resource.Blob) > MaxStringBytes*4 || !validBase64(*block.Resource.Blob))) || !validOptionalWireString(block.Resource.MimeType, 256) || validateMeta(json.RawMessage(block.Resource.Meta)) != nil || block.Text != "" || block.Data != "" || block.MimeType != "" || block.URI != "" || block.Name != "" || block.Title != "" || block.Description != "" || block.Size != nil {
			return errors.New("embedded resource is invalid")
		}
	default:
		return errors.New("content type is not supported")
	}
	return nil
}

func validateAnnotations(annotations *Annotations) error {
	if annotations == nil {
		return nil
	}
	if len(annotations.Audience) > 2 || (annotations.LastModified != nil && !validWireString(*annotations.LastModified, 256)) || (annotations.Priority != nil && (math.IsNaN(*annotations.Priority) || math.IsInf(*annotations.Priority, 0))) || validateMeta(json.RawMessage(annotations.Meta)) != nil {
		return errors.New("content annotations are invalid")
	}
	seen := make(map[string]struct{}, len(annotations.Audience))
	for _, role := range annotations.Audience {
		if role != "user" && role != "assistant" {
			return errors.New("content annotation role is invalid")
		}
		if _, duplicate := seen[role]; duplicate {
			return errors.New("duplicate content annotation role")
		}
		seen[role] = struct{}{}
	}
	return nil
}

func validBase64(value string) bool {
	_, err := base64.StdEncoding.DecodeString(value)
	return err == nil
}

func validatePrompt(request PromptRequest, caps PromptCapabilities) error {
	if err := validateSessionID(request.SessionID); err != nil {
		return err
	}
	if len(request.Prompt) == 0 || len(request.Prompt) > maxCollectionItems {
		return errors.New("prompt content count is invalid")
	}
	for _, block := range request.Prompt {
		if err := validateContentBlock(block, caps, true); err != nil {
			return err
		}
	}
	return validateMeta(json.RawMessage(request.Meta))
}

func validateStopReason(reason StopReason) error {
	if !reason.valid() {
		return invalidEnum("stop reason", reason)
	}
	return nil
}

func validateToolKind(kind ToolKind, optional bool) error {
	if optional && kind == "" {
		return nil
	}
	switch kind {
	case ToolRead, ToolEdit, ToolDelete, ToolMove, ToolSearch, ToolExecute, ToolThink, ToolFetch, ToolSwitchMode, ToolOther:
		return nil
	default:
		return invalidEnum("tool kind", kind)
	}
}

func validateToolStatus(status ToolCallStatus, optional bool) error {
	if optional && status == "" {
		return nil
	}
	switch status {
	case ToolPending, ToolInProgress, ToolCompleted, ToolFailed:
		return nil
	default:
		return invalidEnum("tool call status", status)
	}
}

func validatePermissionKind(kind PermissionOptionKind) error {
	switch kind {
	case PermissionAllowOnce, PermissionAllowAlways, PermissionRejectOnce, PermissionRejectAlways:
		return nil
	default:
		return invalidEnum("permission option kind", kind)
	}
}

func requireBoundedRawObject(raw json.RawMessage) error {
	if len(raw) == 0 || len(raw) > MaxMetaBytes || validateJSON(raw, 16, '{') != nil {
		return fmt.Errorf("bounded JSON object required")
	}
	return nil
}
