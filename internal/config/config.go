package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type ProviderSettings struct {
	APIKey  string `json:"apiKey,omitempty"`
	BaseURL string `json:"baseUrl,omitempty"`
}

type RuntimeQuotaSource struct {
	Name            string            `json:"name,omitempty"`
	Type            string            `json:"type"`
	Path            string            `json:"path,omitempty"`
	URL             string            `json:"url,omitempty"`
	Headers         map[string]string `json:"headers,omitempty"`
	IntervalSeconds int               `json:"intervalSeconds,omitempty"`
	TimeoutSeconds  int               `json:"timeoutSeconds,omitempty"`
	Disabled        bool              `json:"disabled,omitempty"`
}

type Project struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

type Config struct {
	Projects              []Project                   `json:"projects"` // registered projects
	Port                  int                         `json:"port"`
	DefaultProvider       string                      `json:"defaultProvider"`
	DefaultModel          string                      `json:"defaultModel"`
	RouterEnabled         bool                        `json:"routerEnabled"`
	ResponseLang          string                      `json:"responseLang"`   // "ko", "en", "ja", "zh", "auto"
	UILang                string                      `json:"uiLang"`         // "ko", "en"
	SkillSource           string                      `json:"skillSource"`    // "claude", "codex", "gemini", "all", "none"
	SkillDirs             []string                    `json:"skillDirs"`      // extra custom skill directories
	MCPConfigPaths        []string                    `json:"mcpConfigPaths"` // extra MCP config file paths
	WorkDir               string                      `json:"workDir"`        // default workspace
	AccessToken           string                      `json:"accessToken"`    // web UI access token (empty = no auth)
	Providers             map[string]ProviderSettings `json:"providers"`
	RuntimeQuotaSources   []RuntimeQuotaSource        `json:"runtimeQuotaSources,omitempty"`
	EvidencePolicy        string                      `json:"evidencePolicy,omitempty"`        // off, measure, advisory, block
	EvidenceMaxStopBlocks int                         `json:"evidenceMaxStopBlocks,omitempty"` // max blocked completion nudges before allowing

	// Agent-loop tuning for local models (Ollama/SGLang). Zero/absent values use
	// built-in defaults; these only apply to local providers (cloud models keep
	// their full toolset and provider-default sampling).
	LocalToolBudget  int      `json:"localToolBudget,omitempty"`  // max tools sent to local models (0 → default 16; env CORELAY_MAX_TOOLS overrides; ANICLEW_MAX_TOOLS is legacy)
	AgentTemperature *float64 `json:"agentTemperature,omitempty"` // sampling temperature for the local agent loop (absent → 0 for reliable tool calls)

	// ReadOnlyExploreRounds bounds how many tool-using rounds a pure read-only
	// question (e.g. "what is this project?") may explore before the loop forces
	// an answer, so the agent doesn't crawl the whole tree (slow + can hit the
	// iteration cap with no answer). 0 → built-in default (5). Set very high to
	// effectively disable the guard. Action tasks (edit/fix/create) are exempt.
	ReadOnlyExploreRounds int `json:"readOnlyExploreRounds,omitempty"`
}

func DefaultConfig() Config {
	return Config{
		Port:      4000,
		Providers: map[string]ProviderSettings{},
	}
}

// BaseDirName is the canonical Corelay Code state directory. The legacy names
// remain readable so a rename never strands an existing installation.
const (
	BaseDirName            = ".corelay"
	LegacyBaseDirName      = ".aniclew"
	LegacyProxyBaseDirName = ".claude-proxy"
)

// BaseDir returns the directory holding config, receipts, undo snapshots and
// per-project state.
//
// An existing legacy directory wins over a non-existent new one: renaming the
// default must not strand a user's history. New installs get BaseDirName. Set
// CORELAY_CONFIG_DIR to override discovery; ANICLEW_CONFIG_DIR remains a
// compatibility fallback.
func BaseDir() string {
	if dir := renamedEnv("CORELAY_CONFIG_DIR", "ANICLEW_CONFIG_DIR"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()

	current := filepath.Join(home, BaseDirName)
	if _, err := os.Stat(current); err == nil {
		return current
	}
	for _, name := range []string{LegacyBaseDirName, LegacyProxyBaseDirName} {
		legacy := filepath.Join(home, name)
		if _, err := os.Stat(legacy); err == nil {
			return legacy
		}
	}
	return current
}

// IsBaseDirPath reports whether a path sits inside either the current or the
// legacy state directory. Used by the permission layer, which must recognise
// both names for as long as the legacy one can still be in service.
func IsBaseDirPath(path string) bool {
	return strings.Contains(path, BaseDirName) ||
		strings.Contains(path, LegacyBaseDirName) ||
		strings.Contains(path, LegacyProxyBaseDirName)
}

func renamedEnv(primary, legacy string) string {
	if value := strings.TrimSpace(os.Getenv(primary)); value != "" {
		return value
	}
	return strings.TrimSpace(os.Getenv(legacy))
}

func configDir() string {
	return BaseDir()
}

func configPath() string {
	return filepath.Join(configDir(), "config.json")
}

func Load() Config {
	cfg := DefaultConfig()
	data, err := os.ReadFile(configPath())
	if err != nil {
		return cfg
	}
	json.Unmarshal(data, &cfg)
	if cfg.Providers == nil {
		cfg.Providers = map[string]ProviderSettings{}
	}
	return cfg
}

func Save(cfg Config) error {
	dir := configDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(), data, 0644)
}

func ConfigPath() string {
	return configPath()
}

// CapabilityProfileDir is the process-wide root for immutable empirical model
// profiles. CORELAY_CONFIG_DIR (or its ANICLEW_CONFIG_DIR compatibility
// fallback) relocates all application state without a second path override.
func CapabilityProfileDir() string {
	return filepath.Join(BaseDir(), "capability-profiles")
}

func _() string { return runtime.GOOS } // keep import
