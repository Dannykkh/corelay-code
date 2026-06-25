package hooks

import (
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// HookType defines when a hook fires.
type HookType string

const (
	HookPreToolUse   HookType = "pre_tool_use"
	HookPostToolUse  HookType = "post_tool_use"
	HookSessionStart HookType = "session_start"
	HookSessionEnd   HookType = "session_end"
	HookPreCompact   HookType = "pre_compact"
	HookPostCompact  HookType = "post_compact"
)

// Hook represents a single hook definition.
type Hook struct {
	Type    HookType `json:"type"`
	Command string   `json:"command"`
	Timeout int      `json:"timeout,omitempty"` // seconds, default 30
	Source  string   `json:"source"`            // "claude", "codex", "gemini", "project"
}

// HookResult holds the result of a hook execution.
type HookResult struct {
	Hook     Hook          `json:"hook"`
	Output   string        `json:"output"`
	Error    string        `json:"error,omitempty"`
	Duration time.Duration `json:"duration"`
	Blocked  bool          `json:"blocked"` // true if hook returned non-zero (block action)
}

// Registry manages hooks for the current project.
type Registry struct {
	hooks   []Hook
	workDir string
}

// NewRegistry creates a hook registry for a project.
func NewRegistry() *Registry {
	return &Registry{}
}

// Load reads hooks from the appropriate CLI config files based on skill source.
func (r *Registry) Load(workDir string, skillSource string) {
	r.workDir = workDir
	r.hooks = nil

	if workDir == "" {
		return
	}

	sources := resolveSourceList(skillSource)

	for _, src := range sources {
		hooks := loadHooksFromSource(workDir, src)
		r.hooks = append(r.hooks, hooks...)
	}

	if len(r.hooks) > 0 {
		log.Printf("[Hooks] Loaded %d hooks from %s (source: %s)", len(r.hooks), filepath.Base(workDir), skillSource)
	}
}

// ExecuteAsync runs hooks in background (non-blocking). Results are logged but not returned.
func (r *Registry) ExecuteAsync(hookType HookType, env map[string]string) {
	go func() {
		results := r.Execute(hookType, env)
		for _, result := range results {
			if result.Error != "" {
				log.Printf("[Hooks async] %s error: %s", hookType, result.Error)
			}
		}
	}()
}

// Execute runs all hooks of the given type and returns results.
func (r *Registry) Execute(hookType HookType, env map[string]string) []HookResult {
	var results []HookResult

	for _, h := range r.hooks {
		if h.Type != hookType {
			continue
		}

		result := r.run(h, env)
		results = append(results, result)

		// If hook blocked (non-zero exit), log it
		if result.Blocked {
			log.Printf("[Hooks] %s blocked by %s: %s", hookType, h.Source, result.Output)
		}
	}

	return results
}

// IsBlocked checks if any hook of the given type would block.
func (r *Registry) IsBlocked(hookType HookType, env map[string]string) (bool, string) {
	for _, result := range r.Execute(hookType, env) {
		if result.Blocked {
			return true, result.Output
		}
	}
	return false, ""
}

// GetHooks returns all loaded hooks.
func (r *Registry) GetHooks() []Hook {
	return r.hooks
}

// ── Internal ──

func (r *Registry) run(h Hook, env map[string]string) HookResult {
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = 30
	}

	start := time.Now()

	shell, args := hookShell(h.Command)
	cmd := exec.Command(shell, args...)
	cmd.Dir = r.workDir

	// Set environment
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	// Run with timeout
	done := make(chan error, 1)
	var output []byte

	go func() {
		var err error
		output, err = cmd.CombinedOutput()
		done <- err
	}()

	select {
	case err := <-done:
		result := HookResult{
			Hook:     h,
			Output:   strings.TrimSpace(string(output)),
			Duration: time.Since(start),
		}
		if err != nil {
			result.Error = err.Error()
			result.Blocked = true // non-zero exit = block
		}
		return result

	case <-time.After(time.Duration(timeout) * time.Second):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		return HookResult{
			Hook:     h,
			Error:    "timeout",
			Duration: time.Since(start),
			Blocked:  false, // timeout doesn't block, just warns
		}
	}
}

func hookShell(command string) (string, []string) {
	if runtime.GOOS == "windows" {
		if gitBash := findGitBash(); gitBash != "" {
			return gitBash, []string{"-c", command}
		}
	}
	return "bash", []string{"-c", command}
}

func findGitBash() string {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		candidate := filepath.Join(dir, "bash.exe")
		if _, err := os.Stat(candidate); err != nil {
			continue
		}
		if strings.Contains(strings.ToLower(filepath.Clean(candidate)), `\git\`) {
			return candidate
		}
	}
	for _, candidate := range []string{
		filepath.Join(os.Getenv("ProgramFiles"), "Git", "bin", "bash.exe"),
		filepath.Join(os.Getenv("ProgramFiles(x86)"), "Git", "bin", "bash.exe"),
		filepath.Join(os.Getenv("LocalAppData"), "Programs", "Git", "bin", "bash.exe"),
	} {
		if candidate == "" {
			continue
		}
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

func resolveSourceList(skillSource string) []string {
	switch skillSource {
	case "claude":
		return []string{"claude"}
	case "codex":
		return []string{"codex"}
	case "gemini":
		return []string{"gemini"}
	case "none":
		return nil
	default: // "all" or empty
		return []string{"claude", "codex", "gemini"}
	}
}

func loadHooksFromSource(workDir, source string) []Hook {
	switch source {
	case "claude":
		return loadClaudeHooks(workDir)
	case "codex":
		return loadCodexHooks(workDir)
	case "gemini":
		return loadGeminiHooks(workDir)
	}
	return nil
}

// loadClaudeHooks reads .claude/settings.json → hooks
func loadClaudeHooks(workDir string) []Hook {
	paths := []string{
		filepath.Join(workDir, ".claude", "settings.json"),
		filepath.Join(workDir, ".claude", "settings-local.json"),
	}

	var hooks []Hook
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var settings struct {
			Hooks map[string][]struct {
				Matcher string `json:"matcher"`
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		}
		if json.Unmarshal(data, &settings) != nil || settings.Hooks == nil {
			continue
		}

		for hookType, defs := range settings.Hooks {
			for _, d := range defs {
				hooks = append(hooks, Hook{
					Type:    HookType(hookType),
					Command: d.Command,
					Timeout: d.Timeout,
					Source:  "claude",
				})
			}
		}
	}
	return hooks
}

// loadCodexHooks reads codex.json or AGENTS.md for Codex patterns
func loadCodexHooks(workDir string) []Hook {
	// Codex uses AGENTS.md for instructions, not hooks per se
	// Check for codex-specific config
	path := filepath.Join(workDir, "codex.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var cfg struct {
		Hooks map[string]string `json:"hooks"`
	}
	if json.Unmarshal(data, &cfg) != nil || cfg.Hooks == nil {
		return nil
	}

	var hooks []Hook
	for hookType, cmd := range cfg.Hooks {
		hooks = append(hooks, Hook{
			Type:    HookType(hookType),
			Command: cmd,
			Source:  "codex",
		})
	}
	return hooks
}

// loadGeminiHooks reads .gemini/settings.json
func loadGeminiHooks(workDir string) []Hook {
	path := filepath.Join(workDir, ".gemini", "settings.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var cfg struct {
		Hooks map[string]string `json:"hooks"`
	}
	if json.Unmarshal(data, &cfg) != nil || cfg.Hooks == nil {
		return nil
	}

	var hooks []Hook
	for hookType, cmd := range cfg.Hooks {
		hooks = append(hooks, Hook{
			Type:    HookType(hookType),
			Command: cmd,
			Source:  "gemini",
		})
	}
	return hooks
}
