package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
)

type chatOutputFormat string

const (
	chatOutputHuman         chatOutputFormat = "human"
	chatOutputJSON          chatOutputFormat = "json"
	chatOutputJSONL         chatOutputFormat = "jsonl"
	chatOutputSchemaVersion                  = 1
	maxChatOutputEventBytes                  = 64 << 10
)

type chatOutputError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type chatTerminalSummary struct {
	Kind                string `json:"kind,omitempty"`
	StopReason          string `json:"stopReason,omitempty"`
	DurablePolicy       string `json:"durablePolicy,omitempty"`
	TerminalState       string `json:"terminalState,omitempty"`
	CompletionStatus    string `json:"completionStatus,omitempty"`
	CompletionRevision  uint64 `json:"completionRevision,omitempty"`
	CompletionCriteria  int    `json:"completionCriteria,omitempty"`
	CompletionSatisfied int    `json:"completionSatisfied,omitempty"`
	CompletionBlocked   int    `json:"completionBlocked,omitempty"`
}

type chatOutputResult struct {
	SchemaVersion   int                  `json:"schemaVersion"`
	RunID           string               `json:"runId"`
	Status          string               `json:"status"`
	ExitCode        int                  `json:"exitCode"`
	Text            string               `json:"text"`
	SessionID       string               `json:"sessionId,omitempty"`
	SessionRevision uint64               `json:"sessionRevision,omitempty"`
	Terminal        *chatTerminalSummary `json:"terminal,omitempty"`
	Error           *chatOutputError     `json:"error,omitempty"`
}

type chatOutputEvent struct {
	SchemaVersion int    `json:"schemaVersion"`
	RunID         string `json:"runId"`
	Seq           uint64 `json:"seq"`
	Event         string `json:"event"`
	Data          any    `json:"data"`
}

type chatResultEventData struct {
	Status          string               `json:"status"`
	ExitCode        int                  `json:"exitCode"`
	Text            string               `json:"text"`
	SessionID       string               `json:"sessionId,omitempty"`
	SessionRevision uint64               `json:"sessionRevision,omitempty"`
	Terminal        *chatTerminalSummary `json:"terminal,omitempty"`
	Error           *chatOutputError     `json:"error,omitempty"`
}

type chatOutputEmitter struct {
	format chatOutputFormat
	out    io.Writer
	runID  string
	seq    uint64
	err    error
}

func newChatRunID() string {
	var random [16]byte
	if _, err := rand.Read(random[:]); err == nil {
		return hex.EncodeToString(random[:])
	}
	return fmt.Sprintf("local-%d", time.Now().UTC().UnixNano())
}

func (e *chatOutputEmitter) setRunID(runID string) {
	if e == nil || e.seq != 0 || e.err != nil {
		return
	}
	runID = strings.TrimSpace(runID)
	if safePublicChatRunID(runID) {
		e.runID = runID
	}
}

func safePublicChatRunID(runID string) bool {
	if runID == "" || len(runID) > 128 {
		return false
	}
	for _, char := range runID {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.' || char == ':' {
			continue
		}
		return false
	}
	return true
}

func normalizedRunTerminalKind(kind string) string {
	switch strings.TrimSpace(kind) {
	case "completed", "command", "cancelled", "context_blocked", "max_iterations", "max_cycles", "failed", "no_terminal":
		return strings.TrimSpace(kind)
	case "":
		return ""
	default:
		return "unknown"
	}
}

func (e *chatOutputEmitter) event(name string, data any) {
	if e == nil || e.format != chatOutputJSONL || e.err != nil {
		return
	}
	e.seq++
	e.encode(chatOutputEvent{
		SchemaVersion: chatOutputSchemaVersion,
		RunID:         e.runID,
		Seq:           e.seq,
		Event:         name,
		Data:          data,
	})
}

func (e *chatOutputEmitter) result(result chatOutputResult) {
	if e == nil || e.err != nil {
		return
	}
	result.SchemaVersion = chatOutputSchemaVersion
	result.RunID = e.runID
	switch e.format {
	case chatOutputJSON:
		e.encode(result)
	case chatOutputJSONL:
		e.seq++
		e.encode(chatOutputEvent{
			SchemaVersion: chatOutputSchemaVersion,
			RunID:         e.runID,
			Seq:           e.seq,
			Event:         "result",
			Data: chatResultEventData{
				Status:          result.Status,
				ExitCode:        result.ExitCode,
				Text:            result.Text,
				SessionID:       result.SessionID,
				SessionRevision: result.SessionRevision,
				Terminal:        result.Terminal,
				Error:           result.Error,
			},
		})
	}
}

func (e *chatOutputEmitter) encode(value any) {
	encoder := json.NewEncoder(e.out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		e.err = err
	}
}

func newChatOutputResult(runID string, status string, exitCode int, text string, sessionID string, revision uint64, terminal *chatTerminalSummary, err error, accessToken, serverURL string) chatOutputResult {
	result := chatOutputResult{
		SchemaVersion:   chatOutputSchemaVersion,
		RunID:           runID,
		Status:          status,
		ExitCode:        exitCode,
		Text:            sanitizeChatOutputText(text, accessToken, serverURL, 0),
		SessionID:       sanitizeChatOutputText(sessionID, accessToken, serverURL, 256),
		SessionRevision: revision,
		Terminal:        terminal,
	}
	if err != nil {
		code := chatOutputErrorCode(err)
		result.Error = &chatOutputError{
			Code:    code,
			Message: sanitizeChatOutputText(err.Error(), accessToken, serverURL, maxChatOutputEventBytes),
		}
	}
	return result
}

func chatOutputErrorCode(err error) string {
	if err == nil {
		return ""
	}
	if isChatCancellation(err) {
		return "cancelled"
	}
	if errors.Is(err, errChatUnknownTerminal) {
		return "unknown_terminal"
	}
	var blocked *chatCompletionBlockedError
	if errors.As(err, &blocked) {
		return "completion_blocked"
	}
	var terminalErr *chatTerminalFailureError
	if errors.As(err, &terminalErr) {
		switch terminalErr.kind {
		case "cancelled", "context_blocked", "max_iterations", "max_cycles", "failed", "no_terminal":
			return "run_" + terminalErr.kind
		default:
			return "run_failed"
		}
	}
	return "run_failed"
}

func chatTerminalSummaryFromData(data any, accessToken, serverURL string) *chatTerminalSummary {
	encoded, err := json.Marshal(data)
	if err != nil {
		return nil
	}
	var payload struct {
		Kind                string `json:"kind"`
		StopReason          string `json:"stopReason"`
		DurablePolicy       string `json:"durablePolicy"`
		TerminalState       string `json:"terminalState"`
		CompletionStatus    string `json:"completionStatus"`
		CompletionRevision  uint64 `json:"completionRevision"`
		CompletionCriteria  int    `json:"completionCriteria"`
		CompletionSatisfied int    `json:"completionSatisfied"`
		CompletionBlocked   int    `json:"completionBlocked"`
	}
	if err := json.Unmarshal(encoded, &payload); err != nil {
		return nil
	}
	if payload.Kind == "" && payload.TerminalState == "" && payload.CompletionStatus == "" {
		return nil
	}
	summary := &chatTerminalSummary{
		StopReason:          boundedTerminalField(payload.StopReason, accessToken, serverURL, 128),
		CompletionRevision:  payload.CompletionRevision,
		CompletionCriteria:  payload.CompletionCriteria,
		CompletionSatisfied: payload.CompletionSatisfied,
		CompletionBlocked:   payload.CompletionBlocked,
	}
	switch payload.Kind {
	case "completed", "command", "cancelled", "context_blocked", "max_iterations", "max_cycles", "failed", "no_terminal":
		summary.Kind = payload.Kind
	}
	switch payload.DurablePolicy {
	case "commit", "none", "reconcile":
		summary.DurablePolicy = payload.DurablePolicy
	}
	switch payload.TerminalState {
	case "verified", "unverified", "blocked":
		summary.TerminalState = payload.TerminalState
	}
	switch payload.CompletionStatus {
	case "complete", "incomplete", "blocked":
		summary.CompletionStatus = payload.CompletionStatus
	}
	return summary
}

func boundedTerminalField(value, accessToken, serverURL string, maxBytes int) string {
	value = sanitizeChatOutputText(value, accessToken, serverURL, maxBytes)
	return strings.TrimSpace(value)
}

func sanitizeChatOutputText(value, accessToken, serverURL string, maxBytes int) string {
	value = terminalSafe(value)
	if accessToken != "" {
		value = strings.ReplaceAll(value, accessToken, "[redacted]")
	}
	if parsed, err := url.Parse(strings.TrimSpace(serverURL)); err == nil && parsed.User != nil {
		value = strings.ReplaceAll(value, parsed.User.String()+"@", "")
		value = strings.ReplaceAll(value, parsed.User.String(), "[redacted]")
	}
	if maxBytes > 0 {
		value = truncateUTF8Bytes(value, maxBytes)
	}
	return value
}

func normalizeChatOutputEvent(ev agentWireEvent, emitter *chatOutputEmitter, showThinking bool, accessToken, serverURL string) (string, any, bool) {
	readString := func() string {
		var value string
		if json.Unmarshal(ev.Data, &value) == nil {
			return sanitizeChatOutputText(value, accessToken, serverURL, maxChatOutputEventBytes)
		}
		return sanitizeChatOutputText(string(ev.Data), accessToken, serverURL, maxChatOutputEventBytes)
	}
	sanitize := func(value string, maxBytes int) string {
		return sanitizeChatOutputText(value, accessToken, serverURL, maxBytes)
	}
	switch ev.Type {
	case "session":
		var payload struct {
			SessionID       string `json:"sessionId"`
			TraceID         string `json:"traceId"`
			ExecutionPolicy struct {
				Mode     string `json:"mode"`
				Revision uint64 `json:"revision"`
			} `json:"executionPolicy"`
		}
		_ = json.Unmarshal(ev.Data, &payload)
		traceID := strings.TrimSpace(payload.TraceID)
		if accessToken == "" || !strings.Contains(traceID, accessToken) {
			emitter.setRunID(traceID)
		}
		return "session", map[string]any{
			"sessionId": sanitize(payload.SessionID, 256),
			"executionPolicy": map[string]any{
				"mode":     sanitize(payload.ExecutionPolicy.Mode, 32),
				"revision": payload.ExecutionPolicy.Revision,
			},
		}, true
	case "workstream":
		var payload struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		_ = json.Unmarshal(ev.Data, &payload)
		return "workstream", map[string]string{"id": sanitize(payload.ID, 256), "status": sanitize(payload.Status, 64)}, true
	case "text":
		return "text", readString(), true
	case "thinking":
		if !showThinking {
			return "", nil, false
		}
		return "thinking", readString(), true
	case "status":
		return "status", readString(), true
	case "tool_start":
		var payload struct {
			Name string `json:"name"`
			ID   string `json:"id"`
		}
		_ = json.Unmarshal(ev.Data, &payload)
		return "tool_start", map[string]string{"name": sanitize(payload.Name, 128), "id": sanitize(payload.ID, 128)}, true
	case "tool_result":
		var payload struct {
			Name     string `json:"name"`
			ID       string `json:"id"`
			Result   string `json:"result"`
			IsError  bool   `json:"isError"`
			Executed bool   `json:"executed"`
		}
		_ = json.Unmarshal(ev.Data, &payload)
		return "tool_result", map[string]any{
			"name":     sanitize(payload.Name, 128),
			"id":       sanitize(payload.ID, 128),
			"result":   sanitize(payload.Result, 4096),
			"isError":  payload.IsError,
			"executed": payload.Executed,
		}, true
	case "diff":
		var payload struct {
			File string `json:"file"`
			Diff string `json:"diff"`
		}
		_ = json.Unmarshal(ev.Data, &payload)
		return "diff", map[string]string{"file": sanitize(payload.File, 512), "diff": sanitize(payload.Diff, maxChatOutputEventBytes)}, true
	case "undo_preview", "undo_result":
		var payload struct {
			Entries []struct {
				ID     string `json:"id"`
				Path   string `json:"path"`
				Status string `json:"status"`
			} `json:"entries"`
			Reverted []string `json:"reverted"`
			OK       bool     `json:"ok"`
			Error    string   `json:"error"`
		}
		_ = json.Unmarshal(ev.Data, &payload)
		entries := make([]map[string]string, 0, len(payload.Entries))
		for _, entry := range payload.Entries {
			entries = append(entries, map[string]string{
				"id": sanitize(entry.ID, 32), "path": sanitize(entry.Path, 512), "status": sanitize(entry.Status, 64),
			})
		}
		reverted := make([]string, 0, len(payload.Reverted))
		for _, path := range payload.Reverted {
			reverted = append(reverted, sanitize(path, 512))
		}
		return ev.Type, map[string]any{
			"entries": entries, "reverted": reverted, "ok": payload.OK, "error": sanitize(payload.Error, 2048),
		}, true
	case "error":
		return "error", map[string]string{"message": readString()}, true
	case "approval_required":
		var payload approvalRequiredEvent
		_ = json.Unmarshal(ev.Data, &payload)
		return "approval_required", map[string]string{
			"id":            sanitize(payload.ID, 256),
			"sessionId":     sanitize(payload.SessionID, 256),
			"toolName":      sanitize(payload.ToolName, 128),
			"redactedInput": sanitize(payload.RedactedInput, 4096),
			"dangerLevel":   sanitize(payload.DangerLevel, 64),
			"scope":         sanitize(payload.Scope, 256),
			"expiresAt":     sanitize(payload.ExpiresAt, 64),
		}, true
	case "heartbeat", "tool_input", "done", "stream_end", "command":
		return "", nil, false
	default:
		// Keep the event contract extensible without passing upstream payloads
		// through to machine consumers. In particular, raw tool inputs are never emitted.
		return "other", map[string]string{"name": sanitize(ev.Type, 128)}, true
	}
}

func isChatCancellation(err error) bool {
	return errors.Is(err, context.Canceled)
}
