package acp

// Stable v1 methods from schema/v1/meta.json at the package provenance pin.
const (
	MethodInitialize             = "initialize"
	MethodAuthenticate           = "authenticate"
	MethodSessionNew             = "session/new"
	MethodSessionLoad            = "session/load"
	MethodSessionSetMode         = "session/set_mode"
	MethodSessionSetConfigOption = "session/set_config_option"
	MethodSessionPrompt          = "session/prompt"
	MethodSessionCancel          = "session/cancel"
	MethodSessionList            = "session/list"
	MethodSessionDelete          = "session/delete"
	MethodSessionResume          = "session/resume"
	MethodSessionClose           = "session/close"
	MethodLogout                 = "logout"

	MethodSessionRequestPermission = "session/request_permission"
	MethodSessionUpdate            = "session/update"
	MethodFSWriteTextFile          = "fs/write_text_file"
	MethodFSReadTextFile           = "fs/read_text_file"
	MethodTerminalCreate           = "terminal/create"
	MethodTerminalOutput           = "terminal/output"
	MethodTerminalRelease          = "terminal/release"
	MethodTerminalWaitForExit      = "terminal/wait_for_exit"
	MethodTerminalKill             = "terminal/kill"
	MethodElicitationCreate        = "elicitation/create"
	MethodElicitationComplete      = "elicitation/complete"

	MethodCancelRequest = "$/cancel_request"
)

var stableAgentMethods = [...]string{
	MethodInitialize,
	MethodAuthenticate,
	MethodSessionNew,
	MethodSessionLoad,
	MethodSessionSetMode,
	MethodSessionSetConfigOption,
	MethodSessionPrompt,
	MethodSessionCancel,
	MethodSessionList,
	MethodSessionDelete,
	MethodSessionResume,
	MethodSessionClose,
	MethodLogout,
}

var stableClientMethods = [...]string{
	MethodSessionRequestPermission,
	MethodSessionUpdate,
	MethodFSWriteTextFile,
	MethodFSReadTextFile,
	MethodTerminalCreate,
	MethodTerminalOutput,
	MethodTerminalRelease,
	MethodTerminalWaitForExit,
	MethodTerminalKill,
	MethodElicitationCreate,
	MethodElicitationComplete,
}

func StableAgentMethodNames() []string {
	return append([]string(nil), stableAgentMethods[:]...)
}

func StableClientMethodNames() []string {
	return append([]string(nil), stableClientMethods[:]...)
}
