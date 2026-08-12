package acp

import "context"

// BackendDescriptor declares only capabilities that cannot be inferred from
// optional backend interfaces. Session lifecycle capabilities are derived by
// Connection and therefore cannot be advertised without an implementation.
type BackendDescriptor struct {
	AgentInfo             Implementation
	PromptCapabilities    PromptCapabilities
	MCPCapabilities       MCPCapabilities
	AdditionalDirectories bool
	AuthMethods           []AuthMethod
}

// Backend contains the stable v1 baseline required of every ACP agent.
// Optional methods are separate interfaces below so capability advertisement
// and routing cannot drift apart.
type Backend interface {
	Descriptor() BackendDescriptor
	Initialize(context.Context, InitializeRequest, Client) error
	NewSession(context.Context, NewSessionRequest, Client) (NewSessionResponse, error)
	Prompt(context.Context, PromptRequest, Client) (PromptResponse, error)
	CancelSession(context.Context, CancelNotification) error
}

type AuthenticateBackend interface {
	Authenticate(context.Context, AuthenticateRequest, Client) (AuthenticateResponse, error)
}

type LogoutBackend interface {
	Logout(context.Context, LogoutRequest, Client) (LogoutResponse, error)
}

type LoadSessionBackend interface {
	LoadSession(context.Context, LoadSessionRequest, Client) (LoadSessionResponse, error)
}

type ListSessionsBackend interface {
	ListSessions(context.Context, ListSessionsRequest, Client) (ListSessionsResponse, error)
}

type DeleteSessionBackend interface {
	DeleteSession(context.Context, DeleteSessionRequest, Client) (DeleteSessionResponse, error)
}

type ResumeSessionBackend interface {
	ResumeSession(context.Context, ResumeSessionRequest, Client) (ResumeSessionResponse, error)
}

type CloseSessionBackend interface {
	CloseSession(context.Context, CloseSessionRequest, Client) (CloseSessionResponse, error)
}

type SetSessionModeBackend interface {
	SetSessionMode(context.Context, SetSessionModeRequest, Client) (SetSessionModeResponse, error)
}

type SetSessionConfigOptionBackend interface {
	SetSessionConfigOption(context.Context, SetSessionConfigOptionRequest, Client) (SetSessionConfigOptionResponse, error)
}

// Client is the transport-neutral stable v1 agent-to-client seam. Every method
// verifies negotiated client capabilities before it writes a frame.
type Client interface {
	SessionUpdate(context.Context, SessionNotification) error
	RequestPermission(context.Context, RequestPermissionRequest) (RequestPermissionResponse, error)
	WriteTextFile(context.Context, WriteTextFileRequest) (WriteTextFileResponse, error)
	ReadTextFile(context.Context, ReadTextFileRequest) (ReadTextFileResponse, error)
	CreateTerminal(context.Context, CreateTerminalRequest) (CreateTerminalResponse, error)
	TerminalOutput(context.Context, TerminalRequest) (TerminalOutputResponse, error)
	WaitForTerminalExit(context.Context, TerminalRequest) (WaitForTerminalExitResponse, error)
	KillTerminal(context.Context, TerminalRequest) (EmptyResponse, error)
	ReleaseTerminal(context.Context, TerminalRequest) (EmptyResponse, error)
	CreateElicitation(context.Context, CreateElicitationRequest) (CreateElicitationResponse, error)
	CompleteElicitation(context.Context, CompleteElicitationNotification) error
}
