package config

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
)

// SkillSettings is the effective skill source and the distinct additional
// roots that apply to a workspace. Project roots take precedence over global
// custom roots during discovery.
type SkillSettings struct {
	Source      string
	ProjectDirs []string
	Dirs        []string
}

var errInvalidSkillSource = errors.New("invalid skill source")

// ResolveSkillSettings applies exact registered-project overrides over global
// defaults. Durable sessions use their stored workspace as the lookup key, so
// resuming a session keeps it within the same project's skill policy.
func ResolveSkillSettings(cfg Config, workspace string) (SkillSettings, error) {
	settings := SkillSettings{
		Source: normalizeSkillSource(cfg.SkillSource),
		Dirs:   cleanSkillDirs(cfg.SkillDirs),
	}
	workspaceKey := canonicalProjectWorkspace(workspace)
	if workspaceKey != "" {
		for _, project := range cfg.Projects {
			if !sameProjectWorkspaceKey(canonicalProjectWorkspace(project.Path), workspaceKey) {
				continue
			}
			if source := normalizeSkillSource(project.SkillSource); source != "" {
				settings.Source = source
			}
			settings.ProjectDirs = cleanSkillDirs(project.SkillDirs)
			break
		}
	}
	if settings.Source == "" {
		settings.Source = "all"
	}
	if !validSkillSource(settings.Source) {
		return SkillSettings{}, errInvalidSkillSource
	}
	return settings, nil
}

// NormalizeSkillSource canonicalizes an explicit source value for API and run
// option overrides.
func NormalizeSkillSource(source string) string {
	return normalizeSkillSource(source)
}

// ValidSkillSource reports whether source is one of the supported skill
// catalog policies. Empty is intentionally invalid here; callers that want a
// default should resolve it first.
func ValidSkillSource(source string) bool {
	return validSkillSource(normalizeSkillSource(source))
}

// SameProjectWorkspace compares workspace paths after absolute, symlink, and
// platform-specific case normalization. It requires exact path equality.
func SameProjectWorkspace(left, right string) bool {
	leftKey := canonicalProjectWorkspace(left)
	rightKey := canonicalProjectWorkspace(right)
	return sameProjectWorkspaceKey(leftKey, rightKey)
}

func sameProjectWorkspaceKey(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func normalizeSkillSource(source string) string {
	return strings.ToLower(strings.TrimSpace(source))
}

func validSkillSource(source string) bool {
	switch source {
	case "claude", "codex", "gemini", "all", "none":
		return true
	default:
		return false
	}
}

func cleanSkillDirs(dirs []string) []string {
	cleaned := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		if dir = strings.TrimSpace(dir); dir != "" {
			cleaned = append(cleaned, dir)
		}
	}
	return cleaned
}

func canonicalProjectWorkspace(workspace string) string {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return ""
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		abs = resolved
	}
	return filepath.Clean(abs)
}
