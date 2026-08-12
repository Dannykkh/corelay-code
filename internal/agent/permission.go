package agent

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

type DangerLevel string

const (
	DangerSafe      DangerLevel = "safe"
	DangerModerate  DangerLevel = "moderate"
	DangerDangerous DangerLevel = "dangerous"
)

type PermissionConfig struct {
	AutoApprove     string   `json:"autoApprove"` // "safe", "moderate", "all", "none"
	BlockedPaths    []string `json:"blockedPaths"`
	BlockedCommands []string `json:"blockedCommands"`
}

// PermissionDecision is the transport-neutral result consumed by the common
// tool dispatcher. Approval is deliberately distinct from allow: callers must
// complete an explicit approval round trip or fail closed.
type PermissionDecision string

const (
	PermissionAllow    PermissionDecision = "allow"
	PermissionDeny     PermissionDecision = "deny"
	PermissionApproval PermissionDecision = "approval"
)

// PermissionResult records the resolved decision and its user-safe rationale.
type PermissionResult struct {
	Decision PermissionDecision
	Reason   string
	Danger   DangerLevel
}

func DefaultPermissionConfig() PermissionConfig {
	return PermissionConfig{
		AutoApprove: "safe",
		BlockedPaths: []string{
			"/etc/passwd", "/etc/shadow", ".ssh/", ".aws/credentials",
			".env", ".env.local", ".env.production",
		},
		BlockedCommands: []string{
			"rm -rf /", "mkfs", "dd if=", ":(){ :|:& };:", "> /dev/sda",
			"shutdown", "reboot",
		},
	}
}

var dangerousBashPatterns = []string{
	"rm -rf", "rm -r /", "sudo rm", "chmod 777", "mkfs", "dd if=",
	":(){ :|:& };:", "> /dev/sda", "shutdown", "reboot", "kill -9 1",
	"pkill -9", "> /dev/null 2>&1 &", "curl | sh", "wget | sh",
}

var moderateBashPatterns = []string{
	"rm ", "mv ", "cp -r", "git push", "git reset --hard",
	"npm publish", "docker rm", "pip install", "apt install",
	"brew install", "chmod", "chown",
}

// ClassifyDanger returns the danger level for a tool call.
func ClassifyDanger(toolName string, input json.RawMessage) (DangerLevel, string) {
	toolName = canonicalPermissionToolName(toolName)
	switch toolName {
	case "Bash":
		var args struct {
			Command string `json:"command"`
		}
		json.Unmarshal(input, &args)
		cmd := strings.ToLower(args.Command)

		for _, p := range dangerousBashPatterns {
			if strings.Contains(cmd, p) {
				return DangerDangerous, "Dangerous command: " + p
			}
		}
		for _, p := range moderateBashPatterns {
			if strings.Contains(cmd, p) {
				return DangerModerate, "Potentially risky: " + p
			}
		}
		return DangerSafe, ""

	case "Write":
		return DangerModerate, "Creating/overwriting file"

	case "Git":
		var args struct {
			Command string `json:"command"`
			Args    string `json:"args"`
		}
		json.Unmarshal(input, &args)
		command := strings.ToLower(strings.TrimSpace(args.Command))
		argv := strings.Fields(args.Args)

		if dangerousGitInvocation(command, argv) {
			return DangerDangerous, "Destructive git operation"
		}
		if mutatingGitInvocation(command, argv) {
			return DangerModerate, "Git mutating command"
		}
		return DangerSafe, ""

	case "Edit":
		return DangerSafe, ""

	case "Read", "Glob", "Grep", "LS", loadToolResultToolName, reportCompletionToolName, "WebSearch", "WebFetch", "WebResearch",
		"TaskCreate", "TaskUpdate", "TaskList",
		"NotebookRead":
		return DangerSafe, ""

	case "NotebookEdit":
		return DangerModerate, "Modifying notebook"

	default:
		return DangerModerate, "Unknown tool"
	}
}

func dangerousGitInvocation(command string, argv []string) bool {
	for _, argument := range argv {
		flag := strings.ToLower(argument)
		if strings.HasPrefix(flag, "--force") || (command == "push" && flag == "-f") {
			return true
		}
	}
	return command == "reset" && containsGitArgument(argv, "--hard")
}

// mutatingGitInvocation is intentionally conservative: only structurally
// read-only invocations are safe. A model-provided confirm field is not part of
// this decision and can never substitute for the approval broker.
func mutatingGitInvocation(command string, argv []string) bool {
	switch command {
	case "status", "diff", "log", "show", "blame", "grep", "shortlog",
		"describe", "rev-parse", "ls-files", "ls-tree", "cat-file",
		"name-rev", "for-each-ref":
		return false
	case "branch":
		return mutatingGitBranch(argv)
	case "stash":
		return len(argv) == 0 || (strings.ToLower(argv[0]) != "list" && strings.ToLower(argv[0]) != "show")
	case "remote":
		if len(argv) == 0 || (len(argv) == 1 && (argv[0] == "-v" || argv[0] == "--verbose")) {
			return false
		}
		first := strings.ToLower(argv[0])
		return first != "show" && first != "get-url"
	case "tag":
		return mutatingGitTag(argv)
	default:
		// checkout/switch/reset/rebase/merge/cherry-pick and every unknown
		// subcommand require explicit approval rather than optimistic execution.
		return true
	}
}

func mutatingGitBranch(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	for _, argument := range argv {
		flag := strings.ToLower(argument)
		switch flag {
		case "-d", "-D", "--delete", "-m", "-M", "--move", "-c", "-C", "--copy", "--edit-description", "--set-upstream-to", "--unset-upstream":
			return true
		}
	}
	for _, argument := range argv {
		flag := strings.ToLower(argument)
		if flag == "--list" || flag == "--show-current" || flag == "-a" || flag == "--all" ||
			flag == "-r" || flag == "--remotes" || flag == "--contains" || strings.HasPrefix(flag, "--contains=") ||
			flag == "--no-contains" || strings.HasPrefix(flag, "--no-contains=") || flag == "--merged" ||
			strings.HasPrefix(flag, "--merged=") || flag == "--no-merged" || strings.HasPrefix(flag, "--no-merged=") ||
			flag == "--points-at" || strings.HasPrefix(flag, "--points-at=") {
			return false
		}
	}
	// A bare branch name creates a branch. Ambiguous flag forms are approval-only.
	return true
}

func mutatingGitTag(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	for _, argument := range argv {
		flag := strings.ToLower(argument)
		switch flag {
		case "-d", "--delete", "-a", "--annotate", "-s", "--sign", "-u", "--local-user", "-m", "--message", "-F", "--file", "-f", "--force":
			return true
		}
	}
	first := strings.ToLower(argv[0])
	if first == "-l" || first == "--list" || first == "-n" || first == "-v" || first == "--verify" ||
		first == "--contains" || strings.HasPrefix(first, "--contains=") || first == "--no-contains" ||
		strings.HasPrefix(first, "--no-contains=") || first == "--points-at" || strings.HasPrefix(first, "--points-at=") ||
		first == "--merged" || strings.HasPrefix(first, "--merged=") || first == "--no-merged" || strings.HasPrefix(first, "--no-merged=") {
		return false
	}
	// A bare tag name creates a lightweight tag.
	return true
}

func containsGitArgument(argv []string, expected string) bool {
	for _, argument := range argv {
		if strings.EqualFold(argument, expected) {
			return true
		}
	}
	return false
}

// CheckPath validates that a file path is safe to access.
func CheckPath(path string, workDir string, cfg PermissionConfig) (bool, string) {
	workspace, err := canonicalWorkspace(workDir)
	if err != nil {
		return false, "Unsafe workspace path: " + err.Error()
	}
	if _, err := canonicalPathWithinWorkspace(path, workDir, workspace, cfg); err != nil {
		return false, err.Error()
	}
	return true, ""
}

// pathWithin reports whether target is base, or lives under it, using path
// semantics — so "/work-evil" is NOT considered within "/work" (a plain string
// prefix check would wrongly accept it).
func pathWithin(target, base string) bool {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// CheckPermission determines if a tool call should be allowed.
func CheckPermission(toolName string, input json.RawMessage, workDir string, cfg PermissionConfig) (bool, string, DangerLevel) {
	toolName = canonicalPermissionToolName(toolName)
	level, reason := ClassifyDanger(toolName, input)

	// Check blocked commands for Bash
	if toolName == "Bash" {
		var args struct {
			Command string `json:"command"`
		}
		json.Unmarshal(input, &args)
		for _, blocked := range cfg.BlockedCommands {
			if strings.Contains(strings.ToLower(args.Command), strings.ToLower(blocked)) {
				return false, "Blocked command: " + blocked, DangerDangerous
			}
		}
	}

	// All path-bearing tools and their explicit recovery aliases share the same
	// canonical workspace boundary. Invalid or ambiguous input fails closed.
	if ok, msg := checkToolWorkspacePaths(toolName, input, workDir, cfg); !ok {
		return false, msg, DangerDangerous
	}

	// Auto-approve check
	switch cfg.AutoApprove {
	case "all":
		return true, "", level
	case "none":
		return false, "Manual approval required", level
	case "moderate":
		if level == DangerDangerous {
			return false, reason, level
		}
		return true, "", level
	default: // "safe"
		if level != DangerSafe {
			return false, reason, level
		}
		return true, "", level
	}
}

// ResolvePermission applies hard path/command constraints before the immutable
// permission snapshot. Explicit deny always wins. An unmatched snapshot then
// falls through to the existing automatic policy; calls beyond that policy
// require explicit approval instead of being silently promoted to allow.
func ResolvePermission(
	toolName string,
	input json.RawMessage,
	workDir string,
	cfg PermissionConfig,
	snapshotDecision string,
) PermissionResult {
	// AutoApprove=all isolates the non-overridable command and path checks from
	// the configurable automatic-approval threshold.
	hardCfg := cfg
	hardCfg.AutoApprove = "all"
	hardAllowed, hardReason, danger := CheckPermission(toolName, input, workDir, hardCfg)
	if !hardAllowed {
		return PermissionResult{
			Decision: PermissionDeny,
			Reason:   hardReason,
			Danger:   danger,
		}
	}

	switch snapshotDecision {
	case "deny":
		return PermissionResult{
			Decision: PermissionDeny,
			Reason:   "Denied by permission rule",
			Danger:   danger,
		}
	case "allow":
		return PermissionResult{Decision: PermissionAllow, Danger: danger}
	}

	autoAllowed, autoReason, _ := CheckPermission(toolName, input, workDir, cfg)
	if autoAllowed {
		return PermissionResult{Decision: PermissionAllow, Danger: danger}
	}
	if strings.TrimSpace(autoReason) == "" {
		autoReason = "Explicit approval required"
	}
	return PermissionResult{
		Decision: PermissionApproval,
		Reason:   autoReason,
		Danger:   danger,
	}
}
