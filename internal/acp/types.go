// Package acp implements the stable v1 Agent Client Protocol transport seam.
//
// Wire names and shapes in this package were independently derived from the
// official stable schema pinned at:
// https://github.com/agentclientprotocol/agent-client-protocol/blob/af41b25f57a79c5629b3164e23fb4e8650badeeb/schema/v1/schema.json
// Unstable/RFD v2 fields are intentionally not part of this package.
package acp

import (
	"encoding/json"
	"fmt"
)

const ProtocolVersion = 1

type Meta json.RawMessage

func (m Meta) MarshalJSON() ([]byte, error) {
	if len(m) == 0 {
		return []byte("null"), nil
	}
	if err := validateMeta(json.RawMessage(m)); err != nil {
		return nil, err
	}
	return append([]byte(nil), m...), nil
}

func (m *Meta) UnmarshalJSON(data []byte) error {
	if err := validateMeta(data); err != nil {
		return err
	}
	*m = append((*m)[:0], data...)
	return nil
}

type Implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
	Meta    Meta   `json:"_meta,omitempty"`
}

type EmptyCapability struct {
	Meta Meta `json:"_meta,omitempty"`
}

type FileSystemCapabilities struct {
	ReadTextFile  bool `json:"readTextFile"`
	WriteTextFile bool `json:"writeTextFile"`
	Meta          Meta `json:"_meta,omitempty"`
}

type ElicitationCapabilities struct {
	Form *EmptyCapability `json:"form,omitempty"`
	URL  *EmptyCapability `json:"url,omitempty"`
	Meta Meta             `json:"_meta,omitempty"`
}

type ClientSessionCapabilities struct {
	ConfigOptions *EmptyCapability `json:"configOptions,omitempty"`
	Meta          Meta             `json:"_meta,omitempty"`
}

type ClientCapabilities struct {
	FS          FileSystemCapabilities     `json:"fs"`
	Terminal    bool                       `json:"terminal"`
	Session     *ClientSessionCapabilities `json:"session,omitempty"`
	Elicitation *ElicitationCapabilities   `json:"elicitation,omitempty"`
	Meta        Meta                       `json:"_meta,omitempty"`
}

type PromptCapabilities struct {
	Image           bool `json:"image"`
	Audio           bool `json:"audio"`
	EmbeddedContext bool `json:"embeddedContext"`
	Meta            Meta `json:"_meta,omitempty"`
}

type MCPCapabilities struct {
	HTTP bool `json:"http"`
	SSE  bool `json:"sse"`
	Meta Meta `json:"_meta,omitempty"`
}

type SessionCapabilities struct {
	List                  *EmptyCapability `json:"list,omitempty"`
	Delete                *EmptyCapability `json:"delete,omitempty"`
	AdditionalDirectories *EmptyCapability `json:"additionalDirectories,omitempty"`
	Resume                *EmptyCapability `json:"resume,omitempty"`
	Close                 *EmptyCapability `json:"close,omitempty"`
	Meta                  Meta             `json:"_meta,omitempty"`
}

type AgentAuthCapabilities struct {
	Logout *EmptyCapability `json:"logout,omitempty"`
	Meta   Meta             `json:"_meta,omitempty"`
}

type AgentCapabilities struct {
	LoadSession         bool                  `json:"loadSession"`
	PromptCapabilities  PromptCapabilities    `json:"promptCapabilities"`
	MCPCapabilities     MCPCapabilities       `json:"mcpCapabilities"`
	SessionCapabilities SessionCapabilities   `json:"sessionCapabilities"`
	Auth                AgentAuthCapabilities `json:"auth"`
	Meta                Meta                  `json:"_meta,omitempty"`
}

type AuthMethod struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Meta        Meta   `json:"_meta,omitempty"`
}

type InitializeRequest struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities ClientCapabilities `json:"clientCapabilities,omitempty"`
	ClientInfo         *Implementation    `json:"clientInfo,omitempty"`
	Meta               Meta               `json:"_meta,omitempty"`
}

type InitializeResponse struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities AgentCapabilities `json:"agentCapabilities"`
	AuthMethods       []AuthMethod      `json:"authMethods"`
	AgentInfo         *Implementation   `json:"agentInfo,omitempty"`
	Meta              Meta              `json:"_meta,omitempty"`
}

type AuthenticateRequest struct {
	MethodID string `json:"methodId"`
	Meta     Meta   `json:"_meta,omitempty"`
}

type AuthenticateResponse struct {
	Meta Meta `json:"_meta,omitempty"`
}
type LogoutRequest struct {
	Meta Meta `json:"_meta,omitempty"`
}
type LogoutResponse struct {
	Meta Meta `json:"_meta,omitempty"`
}

type EnvVariable struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Meta  Meta   `json:"_meta,omitempty"`
}

type HTTPHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Meta  Meta   `json:"_meta,omitempty"`
}

// MCPServer represents the stable v1 stdio/http/sse union. Type is omitted
// for the default stdio variant and required for http/sse.
type MCPServer struct {
	Type    string        `json:"type,omitempty"`
	Name    string        `json:"name"`
	Command string        `json:"command,omitempty"`
	Args    []string      `json:"args,omitempty"`
	Env     []EnvVariable `json:"env,omitempty"`
	URL     string        `json:"url,omitempty"`
	Headers []HTTPHeader  `json:"headers,omitempty"`
	Meta    Meta          `json:"_meta,omitempty"`
}

type NewSessionRequest struct {
	CWD                   string      `json:"cwd"`
	AdditionalDirectories []string    `json:"additionalDirectories,omitempty"`
	MCPServers            []MCPServer `json:"mcpServers"`
	Meta                  Meta        `json:"_meta,omitempty"`
}

type SessionMode struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Meta        Meta   `json:"_meta,omitempty"`
}

type SessionModeState struct {
	CurrentModeID  string        `json:"currentModeId"`
	AvailableModes []SessionMode `json:"availableModes"`
	Meta           Meta          `json:"_meta,omitempty"`
}

type SessionConfigSelectOption struct {
	Value       string `json:"value"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Meta        Meta   `json:"_meta,omitempty"`
}

// SessionConfigOption is the stable select/boolean union.
type SessionConfigOption struct {
	Type         string                      `json:"type"`
	ID           string                      `json:"id"`
	Name         string                      `json:"name"`
	Description  string                      `json:"description,omitempty"`
	Category     string                      `json:"category,omitempty"`
	CurrentValue any                         `json:"currentValue"`
	Options      []SessionConfigSelectOption `json:"options,omitempty"`
	Meta         Meta                        `json:"_meta,omitempty"`
}

type NewSessionResponse struct {
	SessionID     string                `json:"sessionId"`
	Modes         *SessionModeState     `json:"modes,omitempty"`
	ConfigOptions []SessionConfigOption `json:"configOptions,omitempty"`
	Meta          Meta                  `json:"_meta,omitempty"`
}

type LoadSessionRequest struct {
	MCPServers            []MCPServer `json:"mcpServers"`
	CWD                   string      `json:"cwd"`
	AdditionalDirectories []string    `json:"additionalDirectories,omitempty"`
	SessionID             string      `json:"sessionId"`
	Meta                  Meta        `json:"_meta,omitempty"`
}

type LoadSessionResponse struct {
	Modes         *SessionModeState     `json:"modes,omitempty"`
	ConfigOptions []SessionConfigOption `json:"configOptions,omitempty"`
	Meta          Meta                  `json:"_meta,omitempty"`
}

type ListSessionsRequest struct {
	CWD    *string `json:"cwd,omitempty"`
	Cursor *string `json:"cursor,omitempty"`
	Meta   Meta    `json:"_meta,omitempty"`
}

type SessionInfo struct {
	SessionID             string   `json:"sessionId"`
	CWD                   string   `json:"cwd"`
	AdditionalDirectories []string `json:"additionalDirectories,omitempty"`
	Title                 *string  `json:"title,omitempty"`
	UpdatedAt             *string  `json:"updatedAt,omitempty"`
	Meta                  Meta     `json:"_meta,omitempty"`
}

type ListSessionsResponse struct {
	Sessions   []SessionInfo `json:"sessions"`
	NextCursor *string       `json:"nextCursor,omitempty"`
	Meta       Meta          `json:"_meta,omitempty"`
}

type SessionRequest struct {
	SessionID string `json:"sessionId"`
	Meta      Meta   `json:"_meta,omitempty"`
}

type DeleteSessionRequest = SessionRequest
type DeleteSessionResponse struct {
	Meta Meta `json:"_meta,omitempty"`
}

type ResumeSessionRequest struct {
	SessionID             string      `json:"sessionId"`
	CWD                   string      `json:"cwd"`
	AdditionalDirectories []string    `json:"additionalDirectories,omitempty"`
	MCPServers            []MCPServer `json:"mcpServers,omitempty"`
	Meta                  Meta        `json:"_meta,omitempty"`
}

type ResumeSessionResponse = LoadSessionResponse
type CloseSessionRequest = SessionRequest
type CloseSessionResponse struct {
	Meta Meta `json:"_meta,omitempty"`
}

type SetSessionModeRequest struct {
	SessionID string `json:"sessionId"`
	ModeID    string `json:"modeId"`
	Meta      Meta   `json:"_meta,omitempty"`
}

type SetSessionModeResponse struct {
	Meta Meta `json:"_meta,omitempty"`
}

type SetSessionConfigOptionRequest struct {
	SessionID string          `json:"sessionId"`
	ConfigID  string          `json:"configId"`
	Type      string          `json:"type,omitempty"`
	Value     json.RawMessage `json:"value"`
	Meta      Meta            `json:"_meta,omitempty"`
}

type SetSessionConfigOptionResponse struct {
	ConfigOptions []SessionConfigOption `json:"configOptions"`
	Meta          Meta                  `json:"_meta,omitempty"`
}

type Annotations struct {
	Audience     []string `json:"audience,omitempty"`
	LastModified *string  `json:"lastModified,omitempty"`
	Priority     *float64 `json:"priority,omitempty"`
	Meta         Meta     `json:"_meta,omitempty"`
}

type EmbeddedResource struct {
	URI      string  `json:"uri"`
	MimeType string  `json:"mimeType,omitempty"`
	Text     *string `json:"text,omitempty"`
	Blob     *string `json:"blob,omitempty"`
	Meta     Meta    `json:"_meta,omitempty"`
}

// ContentBlock is the stable v1 content union. Validation enforces fields for
// text, image, audio, resource_link, and resource discriminators.
type ContentBlock struct {
	Type        string            `json:"type"`
	Text        string            `json:"text,omitempty"`
	Data        string            `json:"data,omitempty"`
	MimeType    string            `json:"mimeType,omitempty"`
	URI         string            `json:"uri,omitempty"`
	Name        string            `json:"name,omitempty"`
	Title       string            `json:"title,omitempty"`
	Description string            `json:"description,omitempty"`
	Size        *int64            `json:"size,omitempty"`
	Resource    *EmbeddedResource `json:"resource,omitempty"`
	Annotations *Annotations      `json:"annotations,omitempty"`
	Meta        Meta              `json:"_meta,omitempty"`
}

type PromptRequest struct {
	SessionID string         `json:"sessionId"`
	Prompt    []ContentBlock `json:"prompt"`
	Meta      Meta           `json:"_meta,omitempty"`
}

type StopReason string

const (
	StopEndTurn         StopReason = "end_turn"
	StopMaxTokens       StopReason = "max_tokens"
	StopMaxTurnRequests StopReason = "max_turn_requests"
	StopRefusal         StopReason = "refusal"
	StopCancelled       StopReason = "cancelled"
)

type PromptResponse struct {
	StopReason StopReason `json:"stopReason"`
	Meta       Meta       `json:"_meta,omitempty"`
}

type CancelNotification = SessionRequest

type ToolKind string

const (
	ToolRead       ToolKind = "read"
	ToolEdit       ToolKind = "edit"
	ToolDelete     ToolKind = "delete"
	ToolMove       ToolKind = "move"
	ToolSearch     ToolKind = "search"
	ToolExecute    ToolKind = "execute"
	ToolThink      ToolKind = "think"
	ToolFetch      ToolKind = "fetch"
	ToolSwitchMode ToolKind = "switch_mode"
	ToolOther      ToolKind = "other"
)

type ToolCallStatus string

const (
	ToolPending    ToolCallStatus = "pending"
	ToolInProgress ToolCallStatus = "in_progress"
	ToolCompleted  ToolCallStatus = "completed"
	ToolFailed     ToolCallStatus = "failed"
)

type ToolCallLocation struct {
	Path string  `json:"path"`
	Line *uint32 `json:"line,omitempty"`
	Meta Meta    `json:"_meta,omitempty"`
}

// ToolCall deliberately has no rawInput/rawOutput fields. ACP permits those
// fields, but this transport seam never serializes raw tool payloads.
type ToolCall struct {
	ToolCallID string             `json:"toolCallId"`
	Title      string             `json:"title"`
	Kind       ToolKind           `json:"kind,omitempty"`
	Status     ToolCallStatus     `json:"status,omitempty"`
	Content    []ToolCallContent  `json:"content,omitempty"`
	Locations  []ToolCallLocation `json:"locations,omitempty"`
	Meta       Meta               `json:"_meta,omitempty"`
}

type ToolCallUpdate struct {
	ToolCallID string             `json:"toolCallId"`
	Kind       ToolKind           `json:"kind,omitempty"`
	Status     ToolCallStatus     `json:"status,omitempty"`
	Title      string             `json:"title,omitempty"`
	Content    []ToolCallContent  `json:"content,omitempty"`
	Locations  []ToolCallLocation `json:"locations,omitempty"`
	Meta       Meta               `json:"_meta,omitempty"`
}

type ToolCallContent struct {
	Type       string        `json:"type"`
	Content    *ContentBlock `json:"content,omitempty"`
	Path       string        `json:"path,omitempty"`
	OldText    *string       `json:"oldText,omitempty"`
	NewText    string        `json:"newText,omitempty"`
	TerminalID string        `json:"terminalId,omitempty"`
	Meta       Meta          `json:"_meta,omitempty"`
}

type PlanEntryStatus string
type PlanEntryPriority string

const (
	PlanPending    PlanEntryStatus   = "pending"
	PlanInProgress PlanEntryStatus   = "in_progress"
	PlanCompleted  PlanEntryStatus   = "completed"
	PriorityHigh   PlanEntryPriority = "high"
	PriorityMedium PlanEntryPriority = "medium"
	PriorityLow    PlanEntryPriority = "low"
)

type PlanEntry struct {
	Content  string            `json:"content"`
	Priority PlanEntryPriority `json:"priority"`
	Status   PlanEntryStatus   `json:"status"`
	Meta     Meta              `json:"_meta,omitempty"`
}

type AvailableCommandInput struct {
	Hint string `json:"hint"`
}

type AvailableCommand struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Input       *AvailableCommandInput `json:"input,omitempty"`
	Meta        Meta                   `json:"_meta,omitempty"`
}

type SessionUpdate struct {
	SessionUpdate     string                `json:"sessionUpdate"`
	Content           *ContentBlock         `json:"content,omitempty"`
	MessageID         string                `json:"messageId,omitempty"`
	ToolCallID        string                `json:"toolCallId,omitempty"`
	Title             *string               `json:"title,omitempty"`
	Kind              ToolKind              `json:"kind,omitempty"`
	Status            ToolCallStatus        `json:"status,omitempty"`
	ToolContent       []ToolCallContent     `json:"toolContent,omitempty"`
	Locations         []ToolCallLocation    `json:"locations,omitempty"`
	Entries           []PlanEntry           `json:"entries,omitempty"`
	AvailableCommands []AvailableCommand    `json:"availableCommands,omitempty"`
	CurrentModeID     string                `json:"currentModeId,omitempty"`
	ConfigOptions     []SessionConfigOption `json:"configOptions,omitempty"`
	UpdatedAt         *string               `json:"updatedAt,omitempty"`
	Used              *uint64               `json:"used,omitempty"`
	Size              *uint64               `json:"size,omitempty"`
	Cost              *Cost                 `json:"cost,omitempty"`
	Meta              Meta                  `json:"_meta,omitempty"`
}

type Cost struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
	Meta     Meta    `json:"_meta,omitempty"`
}

// MarshalJSON maps the Go-only ToolContent field to the stable v1 "content"
// key for tool_call and tool_call_update variants without colliding with the
// ContentChunk content field.
func (u SessionUpdate) MarshalJSON() ([]byte, error) {
	var content any
	if len(u.ToolContent) > 0 {
		content = u.ToolContent
	} else if u.Content != nil {
		content = u.Content
	}
	var entries *[]PlanEntry
	if u.Entries != nil {
		entries = &u.Entries
	}
	var availableCommands *[]AvailableCommand
	if u.AvailableCommands != nil {
		availableCommands = &u.AvailableCommands
	}
	var configOptions *[]SessionConfigOption
	if u.ConfigOptions != nil {
		configOptions = &u.ConfigOptions
	}
	switch u.SessionUpdate {
	case "plan":
		if entries == nil {
			empty := []PlanEntry{}
			entries = &empty
		}
	case "available_commands_update":
		if availableCommands == nil {
			empty := []AvailableCommand{}
			availableCommands = &empty
		}
	case "config_option_update":
		if configOptions == nil {
			empty := []SessionConfigOption{}
			configOptions = &empty
		}
	}
	return json.Marshal(struct {
		SessionUpdate     string                 `json:"sessionUpdate"`
		Content           any                    `json:"content,omitempty"`
		MessageID         string                 `json:"messageId,omitempty"`
		ToolCallID        string                 `json:"toolCallId,omitempty"`
		Title             *string                `json:"title,omitempty"`
		Kind              ToolKind               `json:"kind,omitempty"`
		Status            ToolCallStatus         `json:"status,omitempty"`
		Locations         []ToolCallLocation     `json:"locations,omitempty"`
		Entries           *[]PlanEntry           `json:"entries,omitempty"`
		AvailableCommands *[]AvailableCommand    `json:"availableCommands,omitempty"`
		CurrentModeID     string                 `json:"currentModeId,omitempty"`
		ConfigOptions     *[]SessionConfigOption `json:"configOptions,omitempty"`
		UpdatedAt         *string                `json:"updatedAt,omitempty"`
		Used              *uint64                `json:"used,omitempty"`
		Size              *uint64                `json:"size,omitempty"`
		Cost              *Cost                  `json:"cost,omitempty"`
		Meta              Meta                   `json:"_meta,omitempty"`
	}{
		SessionUpdate: u.SessionUpdate, Content: content, MessageID: u.MessageID,
		ToolCallID: u.ToolCallID, Title: u.Title, Kind: u.Kind, Status: u.Status,
		Locations: u.Locations, Entries: entries, AvailableCommands: availableCommands,
		CurrentModeID: u.CurrentModeID, ConfigOptions: configOptions, UpdatedAt: u.UpdatedAt,
		Used: u.Used, Size: u.Size, Cost: u.Cost, Meta: u.Meta,
	})
}

type SessionNotification struct {
	SessionID string        `json:"sessionId"`
	Update    SessionUpdate `json:"update"`
	Meta      Meta          `json:"_meta,omitempty"`
}

type PermissionOptionKind string

const (
	PermissionAllowOnce    PermissionOptionKind = "allow_once"
	PermissionAllowAlways  PermissionOptionKind = "allow_always"
	PermissionRejectOnce   PermissionOptionKind = "reject_once"
	PermissionRejectAlways PermissionOptionKind = "reject_always"
)

type PermissionOption struct {
	OptionID string               `json:"optionId"`
	Name     string               `json:"name"`
	Kind     PermissionOptionKind `json:"kind"`
	Meta     Meta                 `json:"_meta,omitempty"`
}

type PermissionToolCall struct {
	ToolCallID string         `json:"toolCallId"`
	Kind       ToolKind       `json:"kind,omitempty"`
	Status     ToolCallStatus `json:"status,omitempty"`
	Title      string         `json:"title,omitempty"`
}

type RequestPermissionRequest struct {
	SessionID string             `json:"sessionId"`
	ToolCall  PermissionToolCall `json:"toolCall"`
	Options   []PermissionOption `json:"options"`
	Meta      Meta               `json:"_meta,omitempty"`
}

type PermissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
	Meta     Meta   `json:"_meta,omitempty"`
}

type RequestPermissionResponse struct {
	Outcome PermissionOutcome `json:"outcome"`
	Meta    Meta              `json:"_meta,omitempty"`
}

type WriteTextFileRequest struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
	Content   string `json:"content"`
	Meta      Meta   `json:"_meta,omitempty"`
}
type WriteTextFileResponse struct {
	Meta Meta `json:"_meta,omitempty"`
}

type ReadTextFileRequest struct {
	SessionID string  `json:"sessionId"`
	Path      string  `json:"path"`
	Line      *uint32 `json:"line,omitempty"`
	Limit     *uint32 `json:"limit,omitempty"`
	Meta      Meta    `json:"_meta,omitempty"`
}
type ReadTextFileResponse struct {
	Content string `json:"content"`
	Meta    Meta   `json:"_meta,omitempty"`
}

type CreateTerminalRequest struct {
	SessionID       string        `json:"sessionId"`
	Command         string        `json:"command"`
	Args            []string      `json:"args,omitempty"`
	Env             []EnvVariable `json:"env,omitempty"`
	CWD             *string       `json:"cwd,omitempty"`
	OutputByteLimit *uint64       `json:"outputByteLimit,omitempty"`
	Meta            Meta          `json:"_meta,omitempty"`
}
type CreateTerminalResponse struct {
	TerminalID string `json:"terminalId"`
	Meta       Meta   `json:"_meta,omitempty"`
}
type TerminalRequest struct {
	SessionID  string `json:"sessionId"`
	TerminalID string `json:"terminalId"`
	Meta       Meta   `json:"_meta,omitempty"`
}
type TerminalExitStatus struct {
	ExitCode *uint32 `json:"exitCode,omitempty"`
	Signal   *string `json:"signal,omitempty"`
	Meta     Meta    `json:"_meta,omitempty"`
}
type TerminalOutputResponse struct {
	Output     string              `json:"output"`
	Truncated  bool                `json:"truncated"`
	ExitStatus *TerminalExitStatus `json:"exitStatus,omitempty"`
	Meta       Meta                `json:"_meta,omitempty"`
}
type WaitForTerminalExitResponse = TerminalExitStatus
type EmptyResponse struct {
	Meta Meta `json:"_meta,omitempty"`
}

// CreateElicitationRequest represents stable form/url modes. RequestedSchema
// remains bounded opaque JSON because the stable schema is itself a JSON
// Schema subset and is not interpreted by the transport.
type CreateElicitationRequest struct {
	Mode            string          `json:"mode"`
	Message         string          `json:"message"`
	SessionID       string          `json:"sessionId,omitempty"`
	ToolCallID      string          `json:"toolCallId,omitempty"`
	RequestID       *RequestID      `json:"requestId,omitempty"`
	RequestedSchema json.RawMessage `json:"requestedSchema,omitempty"`
	ElicitationID   string          `json:"elicitationId,omitempty"`
	URL             string          `json:"url,omitempty"`
	Meta            Meta            `json:"_meta,omitempty"`
}

type CreateElicitationResponse struct {
	Action  string          `json:"action"`
	Content json.RawMessage `json:"content,omitempty"`
	Meta    Meta            `json:"_meta,omitempty"`
}

type CompleteElicitationNotification struct {
	ElicitationID string `json:"elicitationId"`
	Meta          Meta   `json:"_meta,omitempty"`
}

func (s StopReason) valid() bool {
	switch s {
	case StopEndTurn, StopMaxTokens, StopMaxTurnRequests, StopRefusal, StopCancelled:
		return true
	default:
		return false
	}
}

func invalidEnum(name string, value any) error {
	return fmt.Errorf("acp: invalid %s %q", name, value)
}
