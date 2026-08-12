package acp

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
)

func (c *Connection) dispatch(ctx context.Context, requestKey, method string, raw json.RawMessage) (any, *RPCError) {
	if method == MethodInitialize {
		return c.dispatchInitialize(ctx, raw)
	}
	if !c.isInitialized() {
		return nil, &RPCError{Code: CodeInvalidRequest, Message: "initialize must complete before other methods"}
	}
	switch method {
	case MethodAuthenticate:
		return c.dispatchAuthenticate(ctx, raw)
	case MethodLogout:
		return c.dispatchLogout(ctx, raw)
	case MethodSessionNew:
		return c.dispatchNewSession(ctx, raw)
	case MethodSessionLoad:
		return c.dispatchLoadSession(ctx, raw)
	case MethodSessionList:
		return c.dispatchListSessions(ctx, raw)
	case MethodSessionDelete:
		return c.dispatchDeleteSession(ctx, raw)
	case MethodSessionResume:
		return c.dispatchResumeSession(ctx, raw)
	case MethodSessionClose:
		return c.dispatchCloseSession(ctx, raw)
	case MethodSessionSetMode:
		return c.dispatchSetMode(ctx, raw)
	case MethodSessionSetConfigOption:
		return c.dispatchSetConfig(ctx, raw)
	case MethodSessionPrompt:
		return c.dispatchPrompt(ctx, requestKey, raw)
	default:
		return nil, &RPCError{Code: CodeMethodNotFound, Message: "method not found"}
	}
}

func (c *Connection) dispatchInitialize(ctx context.Context, raw json.RawMessage) (any, *RPCError) {
	var request InitializeRequest
	if err := decodeObject(raw, &request,
		[]string{"protocolVersion", "clientCapabilities", "clientInfo", "_meta"},
		[]string{"protocolVersion"}); err != nil || request.ProtocolVersion < 0 || request.ProtocolVersion > 65535 || validateImplementation(request.ClientInfo) != nil {
		return nil, invalidParams()
	}
	c.mu.Lock()
	if c.initialized || c.initializing {
		c.mu.Unlock()
		return nil, &RPCError{Code: CodeInvalidRequest, Message: "initialize may only be called once"}
	}
	c.initializing = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.initializing = false
		c.mu.Unlock()
	}()

	descriptor := c.backend.Descriptor()
	if err := validateDescriptor(c.backend, descriptor); err != nil {
		return nil, internalError()
	}
	if err := c.backend.Initialize(ctx, request, c); err != nil {
		return nil, mapBackendError(ctx, err)
	}
	if ctx.Err() != nil {
		return nil, cancelledError()
	}
	capabilities := deriveCapabilities(c.backend, descriptor)
	response := InitializeResponse{
		ProtocolVersion:   ProtocolVersion,
		AgentCapabilities: capabilities,
		AuthMethods:       append([]AuthMethod{}, descriptor.AuthMethods...),
		AgentInfo:         &descriptor.AgentInfo,
	}
	c.mu.Lock()
	c.clientCaps = request.ClientCapabilities
	c.descriptor = descriptor
	c.initialized = true
	c.mu.Unlock()
	return response, nil
}

func validateDescriptor(backend Backend, descriptor BackendDescriptor) error {
	if err := validateImplementation(&descriptor.AgentInfo); err != nil {
		return err
	}
	if len(descriptor.AuthMethods) > 32 {
		return errors.New("too many auth methods")
	}
	_, supportsAuth := backend.(AuthenticateBackend)
	seen := make(map[string]struct{}, len(descriptor.AuthMethods))
	for _, method := range descriptor.AuthMethods {
		if !supportsAuth || !validWireString(method.ID, 256) || !validWireString(method.Name, 256) || !validOptionalWireString(method.Description, 1024) {
			return errors.New("auth method is invalid or unsupported")
		}
		if _, duplicate := seen[method.ID]; duplicate {
			return errors.New("duplicate auth method")
		}
		seen[method.ID] = struct{}{}
	}
	return nil
}

func deriveCapabilities(backend Backend, descriptor BackendDescriptor) AgentCapabilities {
	capabilities := AgentCapabilities{
		PromptCapabilities: descriptor.PromptCapabilities,
		MCPCapabilities:    descriptor.MCPCapabilities,
	}
	if _, ok := backend.(LoadSessionBackend); ok {
		capabilities.LoadSession = true
	}
	if _, ok := backend.(ListSessionsBackend); ok {
		capabilities.SessionCapabilities.List = &EmptyCapability{}
	}
	if _, ok := backend.(DeleteSessionBackend); ok {
		capabilities.SessionCapabilities.Delete = &EmptyCapability{}
	}
	if descriptor.AdditionalDirectories {
		capabilities.SessionCapabilities.AdditionalDirectories = &EmptyCapability{}
	}
	if _, ok := backend.(ResumeSessionBackend); ok {
		capabilities.SessionCapabilities.Resume = &EmptyCapability{}
	}
	if _, ok := backend.(CloseSessionBackend); ok {
		capabilities.SessionCapabilities.Close = &EmptyCapability{}
	}
	if _, ok := backend.(LogoutBackend); ok {
		capabilities.Auth.Logout = &EmptyCapability{}
	}
	return capabilities
}

func (c *Connection) dispatchAuthenticate(ctx context.Context, raw json.RawMessage) (any, *RPCError) {
	backend, ok := c.backend.(AuthenticateBackend)
	if !ok || len(c.descriptor.AuthMethods) == 0 {
		return nil, methodNotFound()
	}
	var request AuthenticateRequest
	if decodeObject(raw, &request, []string{"methodId", "_meta"}, []string{"methodId"}) != nil || !validWireString(request.MethodID, 256) {
		return nil, invalidParams()
	}
	known := false
	for _, method := range c.descriptor.AuthMethods {
		known = known || method.ID == request.MethodID
	}
	if !known {
		return nil, invalidParams()
	}
	response, err := backend.Authenticate(ctx, request, c)
	return backendResult(ctx, response, err)
}

func (c *Connection) dispatchLogout(ctx context.Context, raw json.RawMessage) (any, *RPCError) {
	backend, ok := c.backend.(LogoutBackend)
	if !ok {
		return nil, methodNotFound()
	}
	var request LogoutRequest
	if decodeObject(raw, &request, []string{"_meta"}, nil) != nil {
		return nil, invalidParams()
	}
	response, err := backend.Logout(ctx, request, c)
	return backendResult(ctx, response, err)
}

func (c *Connection) dispatchNewSession(ctx context.Context, raw json.RawMessage) (any, *RPCError) {
	var request NewSessionRequest
	if decodeObject(raw, &request, []string{"cwd", "additionalDirectories", "mcpServers", "_meta"}, []string{"cwd", "mcpServers"}) != nil ||
		request.MCPServers == nil ||
		validateDirectories(request.CWD, request.AdditionalDirectories, c.descriptor.AdditionalDirectories) != nil ||
		validateMCPServers(request.MCPServers, c.descriptor.MCPCapabilities) != nil {
		return nil, invalidParams()
	}
	response, err := c.backend.NewSession(ctx, request, c)
	if err != nil {
		return nil, mapBackendError(ctx, err)
	}
	if ctx.Err() != nil {
		return nil, cancelledError()
	}
	if validateSessionID(response.SessionID) != nil || validateSessionSetup(response.Modes, response.ConfigOptions) != nil {
		return nil, internalError()
	}
	c.mu.Lock()
	if _, exists := c.sessions[response.SessionID]; exists {
		c.mu.Unlock()
		return nil, internalError()
	}
	c.sessions[response.SessionID] = &sessionState{}
	c.mu.Unlock()
	return response, nil
}

func (c *Connection) dispatchLoadSession(ctx context.Context, raw json.RawMessage) (any, *RPCError) {
	backend, ok := c.backend.(LoadSessionBackend)
	if !ok {
		return nil, methodNotFound()
	}
	var request LoadSessionRequest
	if decodeObject(raw, &request, []string{"mcpServers", "cwd", "additionalDirectories", "sessionId", "_meta"}, []string{"mcpServers", "cwd", "sessionId"}) != nil ||
		request.MCPServers == nil ||
		validateSessionID(request.SessionID) != nil || validateDirectories(request.CWD, request.AdditionalDirectories, c.descriptor.AdditionalDirectories) != nil ||
		validateMCPServers(request.MCPServers, c.descriptor.MCPCapabilities) != nil {
		return nil, invalidParams()
	}
	created, rpcErr := c.reserveSession(request.SessionID)
	if rpcErr != nil {
		return nil, rpcErr
	}
	response, err := backend.LoadSession(ctx, request, c)
	if err != nil || ctx.Err() != nil {
		c.rollbackSession(request.SessionID, created)
		if err != nil {
			return nil, mapBackendError(ctx, err)
		}
		return nil, cancelledError()
	}
	if validateSessionSetup(response.Modes, response.ConfigOptions) != nil {
		c.rollbackSession(request.SessionID, created)
		return nil, internalError()
	}
	return response, nil
}

func (c *Connection) dispatchListSessions(ctx context.Context, raw json.RawMessage) (any, *RPCError) {
	backend, ok := c.backend.(ListSessionsBackend)
	if !ok {
		return nil, methodNotFound()
	}
	var request ListSessionsRequest
	if decodeObject(raw, &request, []string{"cwd", "cursor", "_meta"}, nil) != nil ||
		(request.CWD != nil && (!filepath.IsAbs(*request.CWD) || len(*request.CWD) > 4096)) ||
		(request.Cursor != nil && !validWireString(*request.Cursor, 4096)) {
		return nil, invalidParams()
	}
	response, err := backend.ListSessions(ctx, request, c)
	if err != nil {
		return nil, mapBackendError(ctx, err)
	}
	if len(response.Sessions) > maxCollectionItems {
		return nil, internalError()
	}
	if response.Sessions == nil {
		response.Sessions = []SessionInfo{}
	}
	seen := make(map[string]struct{}, len(response.Sessions))
	for _, session := range response.Sessions {
		if validateSessionID(session.SessionID) != nil || validateDirectories(session.CWD, session.AdditionalDirectories, c.descriptor.AdditionalDirectories) != nil ||
			(session.Title != nil && len(*session.Title) > 1024) || (session.UpdatedAt != nil && !validWireString(*session.UpdatedAt, 256)) ||
			validateMeta(json.RawMessage(session.Meta)) != nil {
			return nil, internalError()
		}
		if _, duplicate := seen[session.SessionID]; duplicate {
			return nil, internalError()
		}
		seen[session.SessionID] = struct{}{}
	}
	return response, nil
}

func (c *Connection) dispatchDeleteSession(ctx context.Context, raw json.RawMessage) (any, *RPCError) {
	backend, ok := c.backend.(DeleteSessionBackend)
	if !ok {
		return nil, methodNotFound()
	}
	request, rpcErr := decodeSessionRequest(raw)
	if rpcErr != nil {
		return nil, rpcErr
	}
	c.mu.Lock()
	if state := c.sessions[request.SessionID]; state != nil && state.promptCancel != nil {
		c.mu.Unlock()
		return nil, &RPCError{Code: CodeInvalidRequest, Message: "session has an active prompt"}
	}
	c.mu.Unlock()
	response, err := backend.DeleteSession(ctx, request, c)
	if err != nil {
		return nil, mapBackendError(ctx, err)
	}
	c.mu.Lock()
	delete(c.sessions, request.SessionID)
	c.mu.Unlock()
	return response, nil
}

func (c *Connection) dispatchResumeSession(ctx context.Context, raw json.RawMessage) (any, *RPCError) {
	backend, ok := c.backend.(ResumeSessionBackend)
	if !ok {
		return nil, methodNotFound()
	}
	var request ResumeSessionRequest
	if decodeObject(raw, &request, []string{"sessionId", "cwd", "additionalDirectories", "mcpServers", "_meta"}, []string{"sessionId", "cwd"}) != nil ||
		validateSessionID(request.SessionID) != nil || validateDirectories(request.CWD, request.AdditionalDirectories, c.descriptor.AdditionalDirectories) != nil ||
		validateMCPServers(request.MCPServers, c.descriptor.MCPCapabilities) != nil {
		return nil, invalidParams()
	}
	created, rpcErr := c.reserveSession(request.SessionID)
	if rpcErr != nil {
		return nil, rpcErr
	}
	response, err := backend.ResumeSession(ctx, request, c)
	if err != nil || ctx.Err() != nil {
		c.rollbackSession(request.SessionID, created)
		if err != nil {
			return nil, mapBackendError(ctx, err)
		}
		return nil, cancelledError()
	}
	if validateSessionSetup(response.Modes, response.ConfigOptions) != nil {
		return nil, internalError()
	}
	return response, nil
}

func (c *Connection) dispatchCloseSession(ctx context.Context, raw json.RawMessage) (any, *RPCError) {
	backend, ok := c.backend.(CloseSessionBackend)
	if !ok {
		return nil, methodNotFound()
	}
	request, rpcErr := decodeSessionRequest(raw)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if !c.hasSession(request.SessionID) {
		return nil, &RPCError{Code: CodeResourceNotFound, Message: "session not found"}
	}
	c.markSessionCancelled(request.SessionID)
	if err := c.backend.CancelSession(ctx, request); err != nil {
		return nil, mapBackendError(ctx, err)
	}
	response, err := backend.CloseSession(ctx, request, c)
	if err != nil {
		return nil, mapBackendError(ctx, err)
	}
	c.mu.Lock()
	delete(c.sessions, request.SessionID)
	c.mu.Unlock()
	return response, nil
}

func (c *Connection) dispatchSetMode(ctx context.Context, raw json.RawMessage) (any, *RPCError) {
	backend, ok := c.backend.(SetSessionModeBackend)
	if !ok {
		return nil, methodNotFound()
	}
	var request SetSessionModeRequest
	if decodeObject(raw, &request, []string{"sessionId", "modeId", "_meta"}, []string{"sessionId", "modeId"}) != nil ||
		validateSessionID(request.SessionID) != nil || !validWireString(request.ModeID, 256) || !c.hasSession(request.SessionID) {
		return nil, invalidParams()
	}
	response, err := backend.SetSessionMode(ctx, request, c)
	return backendResult(ctx, response, err)
}

func (c *Connection) dispatchSetConfig(ctx context.Context, raw json.RawMessage) (any, *RPCError) {
	backend, ok := c.backend.(SetSessionConfigOptionBackend)
	if !ok {
		return nil, methodNotFound()
	}
	var request SetSessionConfigOptionRequest
	if decodeObject(raw, &request, []string{"sessionId", "configId", "type", "value", "_meta"}, []string{"sessionId", "configId", "value"}) != nil ||
		validateSessionID(request.SessionID) != nil || !validWireString(request.ConfigID, 256) || !c.hasSession(request.SessionID) || validateConfigValue(request) != nil {
		return nil, invalidParams()
	}
	response, err := backend.SetSessionConfigOption(ctx, request, c)
	if err != nil {
		return nil, mapBackendError(ctx, err)
	}
	if validateSessionSetup(nil, response.ConfigOptions) != nil {
		return nil, internalError()
	}
	if response.ConfigOptions == nil {
		response.ConfigOptions = []SessionConfigOption{}
	}
	return response, nil
}

func (c *Connection) dispatchPrompt(ctx context.Context, requestKey string, raw json.RawMessage) (any, *RPCError) {
	var request PromptRequest
	if decodeObject(raw, &request, []string{"sessionId", "prompt", "_meta"}, []string{"sessionId", "prompt"}) != nil ||
		validatePrompt(request, c.descriptor.PromptCapabilities) != nil {
		return nil, invalidParams()
	}
	c.mu.Lock()
	state := c.sessions[request.SessionID]
	if state == nil {
		c.mu.Unlock()
		return nil, &RPCError{Code: CodeResourceNotFound, Message: "session not found"}
	}
	if state.promptCancel != nil {
		c.mu.Unlock()
		return nil, &RPCError{Code: CodeInvalidRequest, Message: "session already has an active prompt"}
	}
	promptCtx, cancel := context.WithCancel(ctx)
	state.promptCancel = cancel
	state.promptRequestKey = requestKey
	state.cancelledBySession = false
	c.mu.Unlock()
	defer func() {
		cancel()
		c.mu.Lock()
		if current := c.sessions[request.SessionID]; current == state && current.promptRequestKey == requestKey {
			current.promptCancel = nil
			current.promptRequestKey = ""
		}
		c.mu.Unlock()
	}()

	response, err := c.backend.Prompt(promptCtx, request, c)
	c.mu.Lock()
	cancelledBySession := state.cancelledBySession
	c.mu.Unlock()
	if cancelledBySession {
		return PromptResponse{StopReason: StopCancelled}, nil
	}
	if promptCtx.Err() != nil {
		return nil, cancelledError()
	}
	if err != nil {
		return nil, mapBackendError(promptCtx, err)
	}
	if validateStopReason(response.StopReason) != nil {
		return nil, internalError()
	}
	return response, nil
}

func (c *Connection) reserveSession(id string) (bool, *RPCError) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.sessions[id]
	if state != nil {
		if state.promptCancel != nil {
			return false, &RPCError{Code: CodeInvalidRequest, Message: "session has an active prompt"}
		}
		return false, nil
	}
	c.sessions[id] = &sessionState{}
	return true, nil
}

func (c *Connection) rollbackSession(id string, created bool) {
	if !created {
		return
	}
	c.mu.Lock()
	delete(c.sessions, id)
	c.mu.Unlock()
}

func (c *Connection) hasSession(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.sessions[id]
	return ok
}

func (c *Connection) isInitialized() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.initialized
}

func decodeSessionRequest(raw json.RawMessage) (SessionRequest, *RPCError) {
	var request SessionRequest
	if decodeObject(raw, &request, []string{"sessionId", "_meta"}, []string{"sessionId"}) != nil || validateSessionID(request.SessionID) != nil {
		return request, invalidParams()
	}
	return request, nil
}

func validateConfigValue(request SetSessionConfigOptionRequest) error {
	switch request.Type {
	case "":
		var value string
		if json.Unmarshal(request.Value, &value) != nil || !validWireString(value, 256) {
			return errors.New("config value is invalid")
		}
	case "boolean":
		var value bool
		if json.Unmarshal(request.Value, &value) != nil {
			return errors.New("config value is invalid")
		}
	default:
		return errors.New("config type is invalid")
	}
	return nil
}

func validateSessionSetup(modes *SessionModeState, options []SessionConfigOption) error {
	if modes != nil {
		if !validWireString(modes.CurrentModeID, 256) || len(modes.AvailableModes) > 128 {
			return errors.New("session modes are invalid")
		}
		available := make([]string, 0, len(modes.AvailableModes))
		for _, mode := range modes.AvailableModes {
			if !validWireString(mode.ID, 256) || !validWireString(mode.Name, 256) || slices.Contains(available, mode.ID) {
				return errors.New("session mode is invalid")
			}
			available = append(available, mode.ID)
		}
		if !slices.Contains(available, modes.CurrentModeID) {
			return errors.New("current session mode is unavailable")
		}
	}
	if len(options) > 128 {
		return errors.New("too many session options")
	}
	seen := make(map[string]struct{}, len(options))
	for _, option := range options {
		if !validWireString(option.ID, 256) || !validWireString(option.Name, 256) || (option.Type != "select" && option.Type != "boolean") {
			return errors.New("session option is invalid")
		}
		if !validOptionalWireString(option.Description, 1024) || !validOptionalWireString(option.Category, 256) || validateMeta(json.RawMessage(option.Meta)) != nil {
			return errors.New("session option metadata is invalid")
		}
		if _, duplicate := seen[option.ID]; duplicate {
			return errors.New("duplicate session option")
		}
		seen[option.ID] = struct{}{}
		switch option.Type {
		case "boolean":
			if _, ok := option.CurrentValue.(bool); !ok || len(option.Options) != 0 {
				return errors.New("boolean session option is invalid")
			}
		case "select":
			current, ok := option.CurrentValue.(string)
			if !ok || !validWireString(current, 256) || len(option.Options) > 256 {
				return errors.New("select session option is invalid")
			}
			values := make(map[string]struct{}, len(option.Options))
			for _, value := range option.Options {
				if !validWireString(value.Value, 256) || !validWireString(value.Name, 256) || !validOptionalWireString(value.Description, 1024) {
					return errors.New("select session value is invalid")
				}
				if _, duplicate := values[value.Value]; duplicate {
					return errors.New("duplicate session value")
				}
				values[value.Value] = struct{}{}
			}
			if _, exists := values[current]; !exists {
				return errors.New("current session value is unavailable")
			}
		}
	}
	return nil
}

func backendResult[T any](ctx context.Context, response T, err error) (any, *RPCError) {
	if err != nil {
		return nil, mapBackendError(ctx, err)
	}
	if ctx.Err() != nil {
		return nil, cancelledError()
	}
	return response, nil
}

func mapBackendError(ctx context.Context, err error) *RPCError {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return cancelledError()
	}
	var rpcErr *RPCError
	if errors.As(err, &rpcErr) {
		if rpcErr.Code < -2147483648 || rpcErr.Code > 2147483647 {
			return internalError()
		}
		return &RPCError{Code: rpcErr.Code, Message: safeDisplay(rpcErr.Message, 256)}
	}
	return internalError()
}

func invalidParams() *RPCError {
	return &RPCError{Code: CodeInvalidParams, Message: "invalid method parameters"}
}
func methodNotFound() *RPCError {
	return &RPCError{Code: CodeMethodNotFound, Message: "method not found"}
}
func internalError() *RPCError { return &RPCError{Code: CodeInternalError, Message: "internal error"} }
func cancelledError() *RPCError {
	return &RPCError{Code: CodeRequestCancelled, Message: "request cancelled"}
}
