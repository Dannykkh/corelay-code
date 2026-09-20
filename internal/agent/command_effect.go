package agent

import (
	"encoding/json"
	"strings"
)

// CommandEffectKind is shared by permission classification and concurrency
// scheduling. Only ReadOnly is eligible for speculative parallel execution;
// unknown or syntactically complex shell commands remain approval-only and
// serial.
type CommandEffectKind string

const (
	CommandEffectReadOnly    CommandEffectKind = "read-only"
	CommandEffectMutating    CommandEffectKind = "mutating"
	CommandEffectDestructive CommandEffectKind = "destructive"
	CommandEffectUnknown     CommandEffectKind = "unknown"
)

// CommandEffect is a conservative summary of one Bash or Git invocation.
type CommandEffect struct {
	Kind   CommandEffectKind
	Reason string
}

func classifyCommandEffect(toolName string, input map[string]interface{}) CommandEffect {
	switch canonicalPermissionToolName(toolName) {
	case "Bash":
		command, ok := input["command"].(string)
		if !ok {
			return unknownCommandEffect("missing Bash command")
		}
		return classifyBashCommandEffect(command)
	case "Git":
		command, ok := input["command"].(string)
		if !ok {
			return unknownCommandEffect("missing Git command")
		}
		args, ok := input["args"].(string)
		if !ok && input["args"] != nil {
			return unknownCommandEffect("invalid Git arguments")
		}
		return classifyGitCommandEffect(command, args)
	default:
		return unknownCommandEffect("tool has no command-effect contract")
	}
}

func classifyBashCommandEffect(command string) CommandEffect {
	words, ok := splitSimpleShellCommand(command)
	if !ok || len(words) == 0 {
		return unknownCommandEffect("shell syntax is compound, quoted, expanded, or ambiguous")
	}
	if strings.Contains(words[0], "=") {
		return unknownCommandEffect("environment assignment is not classified")
	}
	for i, word := range words {
		words[i] = strings.ToLower(word)
	}
	// Only bare command names have a useful static effect contract. A path such
	// as ./cat or C:\\tools\\git can name an arbitrary executable that merely
	// borrows a read-only utility's basename.
	if strings.ContainsAny(words[0], `/\\:`) {
		return unknownCommandEffect("path-qualified executable is not classified")
	}
	executable := words[0]
	args := words[1:]

	switch executable {
	case "git":
		if len(args) < 1 {
			return unknownCommandEffect("Git subcommand is missing")
		}
		return classifyGitInvocation(args[0], args[1:])
	case "find":
		for _, arg := range args {
			switch arg {
			case "-delete", "-exec", "-execdir", "-ok", "-okdir", "-fprint", "-fprint0", "-fprintf", "-fls":
				return CommandEffect{Kind: CommandEffectMutating, Reason: "find can delete, execute, or write a file"}
			}
		}
		return CommandEffect{Kind: CommandEffectReadOnly, Reason: "find query only"}
	case "rm":
		if hasRecursiveForceFlags(args) {
			return CommandEffect{Kind: CommandEffectDestructive, Reason: "recursive forced deletion"}
		}
		return CommandEffect{Kind: CommandEffectMutating, Reason: "file deletion"}
	case "mkfs", "dd", "shutdown", "reboot", "halt", "poweroff":
		return CommandEffect{Kind: CommandEffectDestructive, Reason: "system-level command"}
	case "chmod":
		for _, arg := range args {
			if arg == "777" || strings.Contains(arg, "777") {
				return CommandEffect{Kind: CommandEffectDestructive, Reason: "world-writable permission change"}
			}
		}
		return CommandEffect{Kind: CommandEffectMutating, Reason: "file permission change"}
	case "kill", "pkill":
		for _, arg := range args {
			if arg == "-9" || arg == "-KILL" {
				if executable == "pkill" || containsCommandEffectArg(args, "1") {
					return CommandEffect{Kind: CommandEffectDestructive, Reason: "forced process termination"}
				}
			}
		}
		return CommandEffect{Kind: CommandEffectMutating, Reason: "process termination"}
	case "mv", "cp", "mkdir", "rmdir", "touch", "chown", "ln", "install", "tee", "sponge":
		return CommandEffect{Kind: CommandEffectMutating, Reason: "file or system state change"}
	case "sed", "perl":
		for _, arg := range args {
			if arg == "-i" || strings.HasPrefix(arg, "-i") {
				return CommandEffect{Kind: CommandEffectMutating, Reason: "in-place file edit"}
			}
		}
		return unknownCommandEffect("script command effect is not statically known")
	case "ls", "cat", "head", "tail", "wc", "grep", "egrep", "fgrep", "rg", "ag", "stat", "file", "du", "df", "pwd", "basename", "dirname", "realpath", "readlink", "uname", "whoami", "date", "which", "where", "type", "echo", "printf", "true", "false":
		return CommandEffect{Kind: CommandEffectReadOnly, Reason: "known read-only command"}
	default:
		return unknownCommandEffect("command is not in the read-only effect catalog")
	}
}

func classifyGitCommandEffect(command, rawArgs string) CommandEffect {
	command = strings.ToLower(strings.TrimSpace(command))
	if command == "" || strings.ContainsAny(command, " \t\r\n;&|<>`$\\'\"") {
		return unknownCommandEffect("Git command is malformed")
	}
	args, ok := splitSimpleShellCommand(rawArgs)
	if !ok {
		return unknownCommandEffect("Git arguments are quoted or ambiguous")
	}
	return classifyGitInvocation(command, args)
}

func classifyGitInvocation(command string, args []string) CommandEffect {
	command = strings.ToLower(strings.TrimSpace(command))
	for _, arg := range args {
		if arg == "--output" || strings.HasPrefix(arg, "--output=") {
			return CommandEffect{Kind: CommandEffectMutating, Reason: "Git writes command output to a file"}
		}
	}
	if dangerousGitInvocation(command, args) {
		return CommandEffect{Kind: CommandEffectDestructive, Reason: "destructive Git operation"}
	}
	if !mutatingGitInvocation(command, args) {
		return CommandEffect{Kind: CommandEffectReadOnly, Reason: "known read-only Git invocation"}
	}
	if knownMutatingGitCommand(command) {
		return CommandEffect{Kind: CommandEffectMutating, Reason: "Git changes repository state"}
	}
	return unknownCommandEffect("Git subcommand is not classified")
}

func knownMutatingGitCommand(command string) bool {
	switch command {
	case "add", "am", "apply", "bisect", "branch", "checkout", "cherry-pick", "clean", "clone", "commit", "config", "fetch", "filter-branch", "gc", "init", "merge", "mv", "notes", "pull", "push", "rebase", "remote", "replace", "reset", "restore", "revert", "rm", "stash", "submodule", "switch", "tag", "update-index", "worktree":
		return true
	default:
		return false
	}
}

func splitSimpleShellCommand(command string) ([]string, bool) {
	var words []string
	var current strings.Builder
	flush := func() {
		if current.Len() > 0 {
			words = append(words, current.String())
			current.Reset()
		}
	}
	for _, r := range command {
		switch r {
		case '\'', '"', '$', '`', '\\', ';', '|', '&', '>', '<', '(', ')', '\n', '\r', '#':
			return nil, false
		case ' ', '\t':
			flush()
		default:
			current.WriteRune(r)
		}
	}
	flush()
	return words, true
}

func hasRecursiveForceFlags(args []string) bool {
	for _, arg := range args {
		flag := strings.TrimLeft(arg, "-")
		if strings.Contains(flag, "r") && strings.Contains(flag, "f") {
			return true
		}
	}
	return false
}

func containsCommandEffectArg(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func unknownCommandEffect(reason string) CommandEffect {
	return CommandEffect{Kind: CommandEffectUnknown, Reason: reason}
}

func readOnlyPolicyAllowsBuiltIn(toolName string, input json.RawMessage) bool {
	toolName = canonicalPermissionToolName(toolName)
	switch toolName {
	case "Bash", "Git":
		var object map[string]interface{}
		if err := json.Unmarshal(input, &object); err != nil || object == nil {
			return false
		}
		return classifyCommandEffect(toolName, object).Kind == CommandEffectReadOnly
	case "Read", "Glob", "Grep", "LS", "RepoMap", "LSP", "NotebookRead", "ImageRead", "PDFRead",
		loadToolResultToolName, loadSkillToolName, reportCompletionToolName, "WebSearch", "WebFetch", "WebResearch", "TaskList":
		return true
	default:
		return false
	}
}

func dangerForCommandEffect(effect CommandEffect) (DangerLevel, string) {
	switch effect.Kind {
	case CommandEffectReadOnly:
		return DangerSafe, ""
	case CommandEffectMutating:
		return DangerModerate, effect.Reason
	case CommandEffectDestructive:
		return DangerDangerous, effect.Reason
	default:
		return DangerUnknown, effect.Reason
	}
}
