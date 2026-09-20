package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

var processConfigMu sync.Mutex

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
	Path        string   `json:"path"`
	Name        string   `json:"name"`
	SkillSource string   `json:"skillSource,omitempty"` // optional project override; blank inherits global
	SkillDirs   []string `json:"skillDirs,omitempty"`   // optional additional roots
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

	// LSPExecutable selects the configured read-only language server executable
	// (currently gopls for Go). An empty value uses the gopls name resolved from
	// PATH; the application never installs a language server automatically.
	LSPExecutable string `json:"lspExecutable,omitempty"`
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

// Load preserves the legacy default-on-error behavior. Security-sensitive code
// and state mutations must use LoadChecked or Update instead.
func Load() Config {
	cfg, _, err := LoadChecked()
	if err != nil {
		return DefaultConfig()
	}
	return cfg
}

// LoadChecked distinguishes a missing first-run config from an unreadable or
// malformed existing config. Security-sensitive callers must not use Load,
// which keeps the historical default-on-error behavior for compatibility.
func LoadChecked() (Config, bool, error) {
	return loadConfigUnlocked()
}

// Save replaces a complete validated snapshot. Production read-modify-write
// paths should use Update so concurrent changes are not lost.
func Save(cfg Config) error {
	return withConfigLock(func() error {
		if _, _, err := loadConfigUnlocked(); err != nil {
			return err
		}
		return writeConfigUnlocked(cfg)
	})
}

// Update serializes a complete read-modify-write transaction across goroutines
// and processes. The callback runs against the latest valid snapshot. A failed
// read, callback, or replacement leaves the previously published file intact.
func Update(update func(*Config) error) (Config, error) {
	if update == nil {
		return Config{}, errors.New("config update callback is required")
	}
	var updated Config
	err := withConfigLock(func() error {
		cfg, _, err := loadConfigUnlocked()
		if err != nil {
			return err
		}
		if err := update(&cfg); err != nil {
			return err
		}
		if cfg.Providers == nil {
			cfg.Providers = map[string]ProviderSettings{}
		}
		if err := writeConfigUnlocked(cfg); err != nil {
			return err
		}
		updated = cfg
		return nil
	})
	return updated, err
}

func loadConfigUnlocked() (Config, bool, error) {
	cfg := DefaultConfig()
	data, err := os.ReadFile(configPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, false, nil
		}
		return Config{}, false, fmt.Errorf("read config: %w", err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return Config{}, true, fmt.Errorf("parse config: %w", err)
	}
	if object == nil {
		return Config{}, true, errors.New("parse config: expected a JSON object")
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, true, fmt.Errorf("parse config: %w", err)
	}
	if cfg.Providers == nil {
		cfg.Providers = map[string]ProviderSettings{}
	}
	return cfg, true, nil
}

func writeConfigUnlocked(cfg Config) error {
	dir := configDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secure temporary config: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temporary config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary config: %w", err)
	}
	if err := replaceConfigFile(tmpPath, configPath()); err != nil {
		return fmt.Errorf("publish config: %w", err)
	}
	return nil
}

func withConfigLock(run func() error) error {
	processConfigMu.Lock()
	defer processConfigMu.Unlock()
	dir := configDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	lock, err := acquireConfigFileLock(filepath.Join(dir, ".config.lock"))
	if err != nil {
		return err
	}
	defer lock()
	return run()
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
