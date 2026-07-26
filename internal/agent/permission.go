package agent

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/aniclew/aniclew/internal/config"
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
		full := args.Command + " " + args.Args

		if strings.Contains(full, "--force") || strings.Contains(full, "reset --hard") {
			return DangerDangerous, "Destructive git operation"
		}
		if args.Command == "push" || args.Command == "commit" || args.Command == "add" {
			return DangerModerate, "Git mutating command"
		}
		return DangerSafe, ""

	case "Edit":
		return DangerSafe, ""

	case "Read", "Glob", "Grep", "LS", "WebSearch", "WebFetch", "WebResearch",
		"TaskCreate", "TaskUpdate", "TaskList",
		"NotebookRead":
		return DangerSafe, ""

	case "NotebookEdit":
		return DangerModerate, "Modifying notebook"

	default:
		return DangerModerate, "Unknown tool"
	}
}

// CheckPath validates that a file path is safe to access.
func CheckPath(path string, workDir string, cfg PermissionConfig) (bool, string) {
	// Block known sensitive paths
	for _, blocked := range cfg.BlockedPaths {
		if strings.Contains(path, blocked) {
			return false, "Blocked path: " + blocked
		}
	}

	// Resolve the path the way the tools do: a relative path is joined with the
	// agent's workDir, NOT the server process's working directory. Previously
	// this used filepath.Abs(path) directly, which resolved a bare filename like
	// "notes.txt" against the server's cwd and falsely reported it as outside
	// the workspace — so the agent could never Write/Edit with a relative path.
	resolved := path
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(workDir, resolved)
	}
	absPath, err := filepath.Abs(resolved)
	if err != nil {
		return true, "" // unresolvable — defer to the tool's own handling
	}

	// Inside the workspace → always allowed.
	if absWork, werr := filepath.Abs(workDir); werr == nil && pathWithin(absPath, absWork) {
		return true, ""
	}

	// Outside the workspace: still allow the same known-safe areas the original
	// check did — unix /tmp and AniClew's own state directory. We do NOT broaden
	// this to the OS temp dir: that would loosen the boundary whenever the
	// workspace itself lives under temp (common in tests/CI).
	if absTmp, terr := filepath.Abs("/tmp"); terr == nil && pathWithin(absPath, absTmp) {
		return true, ""
	}
	if config.IsBaseDirPath(absPath) {
		return true, ""
	}

	return false, "Path outside workspace: " + path
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

	// Check file paths
	if toolName == "Read" || toolName == "Write" || toolName == "Edit" {
		var args struct {
			FilePath string `json:"file_path"`
		}
		json.Unmarshal(input, &args)
		if args.FilePath != "" {
			if ok, msg := CheckPath(args.FilePath, workDir, cfg); !ok {
				return false, msg, DangerDangerous
			}
		}
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
