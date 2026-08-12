package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

func (c *Connection) SessionUpdate(ctx context.Context, notification SessionNotification) error {
	if err := c.requireInitializedSession(notification.SessionID); err != nil {
		return err
	}
	if err := validateSessionUpdate(notification.Update); err != nil {
		return &RPCError{Code: CodeInvalidParams, Message: "invalid session update"}
	}
	if notification.Update.Title != nil {
		title := safeDisplay(*notification.Update.Title, 1024)
		notification.Update.Title = &title
	}
	return c.notify(ctx, MethodSessionUpdate, notification)
}

func (c *Connection) RequestPermission(ctx context.Context, request RequestPermissionRequest) (RequestPermissionResponse, error) {
	if err := c.requireInitializedSession(request.SessionID); err != nil {
		return RequestPermissionResponse{}, err
	}
	c.mu.Lock()
	state := c.sessions[request.SessionID]
	activePrompt := state != nil && state.promptCancel != nil
	c.mu.Unlock()
	if !activePrompt {
		return RequestPermissionResponse{}, &RPCError{Code: CodeInvalidRequest, Message: "permission request requires an active prompt"}
	}
	request.Options = append([]PermissionOption(nil), request.Options...)
	request.Meta = nil
	request.ToolCall.Title = safeDisplay(request.ToolCall.Title, 512)
	if !validWireString(request.ToolCall.ToolCallID, 512) || !validWireString(request.ToolCall.Title, 512) || validateToolKind(request.ToolCall.Kind, false) != nil || validateToolStatus(request.ToolCall.Status, false) != nil || len(request.Options) == 0 || len(request.Options) > 16 {
		return RequestPermissionResponse{}, &RPCError{Code: CodeInvalidParams, Message: "invalid permission request"}
	}
	optionIDs := make(map[string]struct{}, len(request.Options))
	for index := range request.Options {
		option := &request.Options[index]
		option.Meta = nil
		option.Name = safeDisplay(option.Name, 256)
		if !validWireString(option.OptionID, 256) || !validWireString(option.Name, 256) || validatePermissionKind(option.Kind) != nil {
			return RequestPermissionResponse{}, &RPCError{Code: CodeInvalidParams, Message: "invalid permission option"}
		}
		if _, duplicate := optionIDs[option.OptionID]; duplicate {
			return RequestPermissionResponse{}, &RPCError{Code: CodeInvalidParams, Message: "duplicate permission option"}
		}
		optionIDs[option.OptionID] = struct{}{}
	}
	var response RequestPermissionResponse
	err := c.call(ctx, request.SessionID, MethodSessionRequestPermission, request, &response,
		[]string{"outcome", "_meta"}, []string{"outcome"})
	if err != nil {
		return response, err
	}
	switch response.Outcome.Outcome {
	case "cancelled":
		if response.Outcome.OptionID != "" {
			return response, &RPCError{Code: CodeInvalidRequest, Message: "invalid permission outcome"}
		}
	case "selected":
		if _, ok := optionIDs[response.Outcome.OptionID]; !ok {
			return response, &RPCError{Code: CodeInvalidRequest, Message: "invalid permission outcome"}
		}
	default:
		return response, &RPCError{Code: CodeInvalidRequest, Message: "invalid permission outcome"}
	}
	return response, nil
}

func (c *Connection) WriteTextFile(ctx context.Context, request WriteTextFileRequest) (WriteTextFileResponse, error) {
	if err := c.requireClientCapability(request.SessionID, func(caps ClientCapabilities) bool { return caps.FS.WriteTextFile }); err != nil {
		return WriteTextFileResponse{}, err
	}
	if !filepath.IsAbs(request.Path) || len(request.Path) > 4096 || len(request.Content) > MaxStringBytes || !utf8.ValidString(request.Content) {
		return WriteTextFileResponse{}, invalidParams()
	}
	var response WriteTextFileResponse
	err := c.call(ctx, request.SessionID, MethodFSWriteTextFile, request, &response, []string{"_meta"}, nil)
	return response, err
}

func (c *Connection) ReadTextFile(ctx context.Context, request ReadTextFileRequest) (ReadTextFileResponse, error) {
	if err := c.requireClientCapability(request.SessionID, func(caps ClientCapabilities) bool { return caps.FS.ReadTextFile }); err != nil {
		return ReadTextFileResponse{}, err
	}
	if !filepath.IsAbs(request.Path) || len(request.Path) > 4096 {
		return ReadTextFileResponse{}, invalidParams()
	}
	var response ReadTextFileResponse
	err := c.call(ctx, request.SessionID, MethodFSReadTextFile, request, &response, []string{"content", "_meta"}, []string{"content"})
	if err == nil && (len(response.Content) > MaxStringBytes || !utf8.ValidString(response.Content)) {
		err = &RPCError{Code: CodeInvalidRequest, Message: "invalid file response"}
	}
	return response, err
}

func (c *Connection) CreateTerminal(ctx context.Context, request CreateTerminalRequest) (CreateTerminalResponse, error) {
	if err := c.requireClientCapability(request.SessionID, func(caps ClientCapabilities) bool { return caps.Terminal }); err != nil {
		return CreateTerminalResponse{}, err
	}
	if !validWireString(request.Command, 4096) || len(request.Args) > 256 || len(request.Env) > 256 || (request.CWD != nil && (!filepath.IsAbs(*request.CWD) || len(*request.CWD) > 4096)) {
		return CreateTerminalResponse{}, invalidParams()
	}
	for _, arg := range request.Args {
		if len(arg) > 4096 || !utf8.ValidString(arg) {
			return CreateTerminalResponse{}, invalidParams()
		}
	}
	for _, env := range request.Env {
		if !validWireString(env.Name, 256) || len(env.Value) > MaxStringBytes || !utf8.ValidString(env.Value) {
			return CreateTerminalResponse{}, invalidParams()
		}
	}
	var response CreateTerminalResponse
	err := c.call(ctx, request.SessionID, MethodTerminalCreate, request, &response, []string{"terminalId", "_meta"}, []string{"terminalId"})
	if err == nil && !validWireString(response.TerminalID, 512) {
		err = &RPCError{Code: CodeInvalidRequest, Message: "invalid terminal response"}
	}
	return response, err
}

func (c *Connection) TerminalOutput(ctx context.Context, request TerminalRequest) (TerminalOutputResponse, error) {
	if err := c.validateTerminalRequest(request); err != nil {
		return TerminalOutputResponse{}, err
	}
	var response TerminalOutputResponse
	err := c.call(ctx, request.SessionID, MethodTerminalOutput, request, &response,
		[]string{"output", "truncated", "exitStatus", "_meta"}, []string{"output", "truncated"})
	if err == nil && (len(response.Output) > MaxStringBytes || !utf8.ValidString(response.Output)) {
		err = &RPCError{Code: CodeInvalidRequest, Message: "invalid terminal response"}
	}
	return response, err
}

func (c *Connection) WaitForTerminalExit(ctx context.Context, request TerminalRequest) (WaitForTerminalExitResponse, error) {
	if err := c.validateTerminalRequest(request); err != nil {
		return WaitForTerminalExitResponse{}, err
	}
	var response WaitForTerminalExitResponse
	err := c.call(ctx, request.SessionID, MethodTerminalWaitForExit, request, &response,
		[]string{"exitCode", "signal", "_meta"}, nil)
	return response, err
}

func (c *Connection) KillTerminal(ctx context.Context, request TerminalRequest) (EmptyResponse, error) {
	if err := c.validateTerminalRequest(request); err != nil {
		return EmptyResponse{}, err
	}
	var response EmptyResponse
	err := c.call(ctx, request.SessionID, MethodTerminalKill, request, &response, []string{"_meta"}, nil)
	return response, err
}

func (c *Connection) ReleaseTerminal(ctx context.Context, request TerminalRequest) (EmptyResponse, error) {
	if err := c.validateTerminalRequest(request); err != nil {
		return EmptyResponse{}, err
	}
	var response EmptyResponse
	err := c.call(ctx, request.SessionID, MethodTerminalRelease, request, &response, []string{"_meta"}, nil)
	return response, err
}

func (c *Connection) CreateElicitation(ctx context.Context, request CreateElicitationRequest) (CreateElicitationResponse, error) {
	c.mu.Lock()
	initialized := c.initialized
	caps := c.clientCaps
	c.mu.Unlock()
	if !initialized {
		return CreateElicitationResponse{}, &RPCError{Code: CodeInvalidRequest, Message: "client capability negotiation is incomplete"}
	}
	if !validWireString(request.Message, 4096) {
		return CreateElicitationResponse{}, invalidParams()
	}
	sessionID := request.SessionID
	sessionScoped := sessionID != ""
	requestScoped := request.RequestID != nil
	if sessionScoped == requestScoped || (!sessionScoped && request.ToolCallID != "") || (request.ToolCallID != "" && !validWireString(request.ToolCallID, 512)) {
		return CreateElicitationResponse{}, invalidParams()
	}
	if sessionScoped {
		if err := c.requireInitializedSession(sessionID); err != nil {
			return CreateElicitationResponse{}, err
		}
	}
	switch request.Mode {
	case "form":
		if caps.Elicitation == nil || caps.Elicitation.Form == nil || requireBoundedRawObject(request.RequestedSchema) != nil || request.ElicitationID != "" || request.URL != "" {
			return CreateElicitationResponse{}, methodNotFound()
		}
	case "url":
		if caps.Elicitation == nil || caps.Elicitation.URL == nil || !validWireString(request.ElicitationID, 512) || !validURL(request.URL) || len(request.RequestedSchema) != 0 {
			return CreateElicitationResponse{}, methodNotFound()
		}
	default:
		return CreateElicitationResponse{}, methodNotFound()
	}
	var response CreateElicitationResponse
	err := c.call(ctx, sessionID, MethodElicitationCreate, request, &response,
		[]string{"action", "content", "_meta"}, []string{"action"})
	if err != nil {
		return response, err
	}
	switch response.Action {
	case "accept":
		if len(response.Content) > 0 && requireBoundedRawObject(response.Content) != nil {
			return response, &RPCError{Code: CodeInvalidRequest, Message: "invalid elicitation response"}
		}
	case "decline", "cancel":
		if len(response.Content) > 0 && string(response.Content) != "null" {
			return response, &RPCError{Code: CodeInvalidRequest, Message: "invalid elicitation response"}
		}
	default:
		return response, &RPCError{Code: CodeInvalidRequest, Message: "unsupported elicitation response"}
	}
	return response, nil
}

func (c *Connection) CompleteElicitation(ctx context.Context, notification CompleteElicitationNotification) error {
	c.mu.Lock()
	initialized := c.initialized
	caps := c.clientCaps
	c.mu.Unlock()
	if !initialized || caps.Elicitation == nil || caps.Elicitation.URL == nil {
		return methodNotFound()
	}
	if !validWireString(notification.ElicitationID, 512) {
		return invalidParams()
	}
	return c.notify(ctx, MethodElicitationComplete, notification)
}

func (c *Connection) validateTerminalRequest(request TerminalRequest) error {
	if err := c.requireClientCapability(request.SessionID, func(caps ClientCapabilities) bool { return caps.Terminal }); err != nil {
		return err
	}
	if !validWireString(request.TerminalID, 512) {
		return invalidParams()
	}
	return nil
}

func (c *Connection) requireInitializedSession(sessionID string) error {
	if validateSessionID(sessionID) != nil {
		return invalidParams()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.initialized {
		return &RPCError{Code: CodeInvalidRequest, Message: "client capability negotiation is incomplete"}
	}
	if _, ok := c.sessions[sessionID]; !ok {
		return &RPCError{Code: CodeResourceNotFound, Message: "session not found"}
	}
	return nil
}

func (c *Connection) requireClientCapability(sessionID string, supported func(ClientCapabilities) bool) error {
	if err := c.requireInitializedSession(sessionID); err != nil {
		return err
	}
	c.mu.Lock()
	caps := c.clientCaps
	c.mu.Unlock()
	if !supported(caps) {
		return methodNotFound()
	}
	return nil
}

func (c *Connection) notify(ctx context.Context, method string, params any) error {
	return c.send(ctx, wireRequest{JSONRPC: "2.0", Method: method, Params: params})
}

func (c *Connection) call(ctx context.Context, sessionID, method string, params, result any, allowed, required []string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	callCtx, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	if c.closed || c.ctx == nil {
		c.mu.Unlock()
		cancel()
		return ErrClosed
	}
	if len(c.pending) >= c.opts.MaxInflight {
		c.mu.Unlock()
		cancel()
		return &RPCError{Code: CodeInternalError, Message: "too many inflight client requests"}
	}
	var id RequestID
	for attempts := 0; attempts < c.opts.MaxInflight+1; attempts++ {
		if c.nextRequest <= 0 || c.nextRequest == math.MaxInt64 {
			c.nextRequest = 1
		}
		id = IntegerID(c.nextRequest)
		c.nextRequest++
		if _, exists := c.pending[id.key()]; !exists {
			break
		}
	}
	pending := &pendingCall{sessionID: sessionID, cancel: cancel, response: make(chan pendingResponse, 1)}
	c.pending[id.key()] = pending
	c.mu.Unlock()
	defer func() {
		cancel()
		c.mu.Lock()
		delete(c.pending, id.key())
		c.mu.Unlock()
	}()

	if err := c.send(callCtx, wireRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params}); err != nil {
		return err
	}
	select {
	case <-callCtx.Done():
		c.trySend(wireRequest{JSONRPC: "2.0", Method: MethodCancelRequest, Params: struct {
			RequestID RequestID `json:"requestId"`
		}{RequestID: id}})
		return cancelledError()
	case response := <-pending.response:
		if callCtx.Err() != nil {
			return cancelledError()
		}
		if response.err != nil {
			return response.err
		}
		if err := decodeObject(response.result, result, allowed, required); err != nil {
			return &RPCError{Code: CodeInvalidRequest, Message: "invalid client response"}
		}
		return nil
	}
}

func validateSessionUpdate(update SessionUpdate) error {
	switch update.SessionUpdate {
	case "user_message_chunk", "agent_message_chunk", "agent_thought_chunk":
		if update.Content == nil || len(update.ToolContent) != 0 || update.ToolCallID != "" {
			return errors.New("invalid content chunk")
		}
		if update.MessageID != "" && !validWireString(update.MessageID, 512) {
			return errors.New("invalid message id")
		}
		return validateContentBlock(*update.Content, PromptCapabilities{Image: true, Audio: true, EmbeddedContext: true}, false)
	case "tool_call":
		if !validWireString(update.ToolCallID, 512) || update.Title == nil || !validWireString(*update.Title, 512) || validateToolKind(update.Kind, true) != nil || validateToolStatus(update.Status, true) != nil || update.Content != nil {
			return errors.New("invalid tool call")
		}
	case "tool_call_update":
		if !validWireString(update.ToolCallID, 512) || (update.Title != nil && !validWireString(*update.Title, 512)) || validateToolKind(update.Kind, true) != nil || validateToolStatus(update.Status, true) != nil || update.Content != nil {
			return errors.New("invalid tool call update")
		}
	case "plan":
		if len(update.Entries) > 256 {
			return errors.New("invalid plan")
		}
		for _, entry := range update.Entries {
			if !validWireString(entry.Content, 4096) || (entry.Priority != PriorityHigh && entry.Priority != PriorityMedium && entry.Priority != PriorityLow) || (entry.Status != PlanPending && entry.Status != PlanInProgress && entry.Status != PlanCompleted) || validateMeta(json.RawMessage(entry.Meta)) != nil {
				return errors.New("invalid plan entry")
			}
		}
	case "available_commands_update":
		if len(update.AvailableCommands) > 256 {
			return errors.New("invalid available commands")
		}
		for _, command := range update.AvailableCommands {
			if !validWireString(command.Name, 256) || !validWireString(command.Description, 1024) || (command.Input != nil && !validWireString(command.Input.Hint, 1024)) || validateMeta(json.RawMessage(command.Meta)) != nil {
				return errors.New("invalid available command")
			}
		}
	case "current_mode_update":
		if !validWireString(update.CurrentModeID, 256) {
			return errors.New("invalid current mode")
		}
	case "config_option_update":
		if validateSessionSetup(nil, update.ConfigOptions) != nil {
			return errors.New("invalid config option update")
		}
	case "session_info_update":
		if update.Title != nil && len(*update.Title) > 1024 {
			return errors.New("invalid session title")
		}
	case "usage_update":
		if update.Used == nil || update.Size == nil || *update.Used > *update.Size {
			return errors.New("invalid usage update")
		}
		if update.Cost != nil && (math.IsNaN(update.Cost.Amount) || math.IsInf(update.Cost.Amount, 0) || !validWireString(update.Cost.Currency, 16)) {
			return errors.New("invalid usage cost")
		}
	default:
		return errors.New("unsupported session update")
	}
	for _, content := range update.ToolContent {
		if err := validateToolContent(content); err != nil {
			return err
		}
	}
	if len(update.Locations) > 256 {
		return errors.New("too many tool locations")
	}
	for _, location := range update.Locations {
		if !filepath.IsAbs(location.Path) || len(location.Path) > 4096 || validateMeta(json.RawMessage(location.Meta)) != nil {
			return errors.New("tool location is invalid")
		}
	}
	return validateMeta(json.RawMessage(update.Meta))
}

func validateToolContent(content ToolCallContent) error {
	if err := validateMeta(json.RawMessage(content.Meta)); err != nil {
		return err
	}
	switch content.Type {
	case "content":
		if content.Content == nil {
			return errors.New("tool content is missing")
		}
		return validateContentBlock(*content.Content, PromptCapabilities{Image: true, Audio: true, EmbeddedContext: true}, false)
	case "diff":
		if !filepath.IsAbs(content.Path) || len(content.Path) > 4096 || len(content.NewText) > MaxStringBytes || !utf8.ValidString(content.NewText) || (content.OldText != nil && (len(*content.OldText) > MaxStringBytes || !utf8.ValidString(*content.OldText))) {
			return errors.New("tool diff is invalid")
		}
	case "terminal":
		if !validWireString(content.TerminalID, 512) {
			return errors.New("tool terminal is invalid")
		}
	default:
		return errors.New("tool content type is invalid")
	}
	return nil
}

func (c *Connection) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return fmt.Sprintf("ACP connection(initialized=%t,sessions=%d,inflight=%d)", c.initialized, len(c.sessions), len(c.inbound)+len(c.pending))
}

func sanitizeNoSecrets(value string) string {
	return strings.TrimSpace(safeDisplay(value, 512))
}
