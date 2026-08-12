package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxPluginManifestBytes = 1 << 20
	maxPlugins             = 128
	maxPluginTools         = 64
	maxPluginHooks         = 64
	maxPluginCommands      = 64
	maxPluginAgents        = 32
	maxPluginArgs          = 64
	maxPluginStringBytes   = 16 << 10
	maxPluginArgumentBytes = 4 << 10
	maxPluginJSONDepth     = 64
)

// Plugin represents a loaded plugin. Legacy command descriptors remain
// visible for compatibility, but only tools with an exec-form Executable are
// eligible for the secure execution catalog.
type Plugin struct {
	Name        string            `json:"name"`
	Version     string            `json:"version"`
	Description string            `json:"description"`
	Author      string            `json:"author"`
	Tools       []PluginTool      `json:"tools,omitempty"`
	Hooks       []PluginHook      `json:"hooks,omitempty"`
	Commands    []PluginCommand   `json:"commands,omitempty"`
	AgentTypes  []PluginAgentType `json:"agents,omitempty"`

	rootDir        string
	manifestDigest string
}

// PluginTool is a tool provided by a plugin. Command is a legacy listing-only
// descriptor and is never passed to a shell or any other process API.
type PluginTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Command     string          `json:"command,omitempty"`
	Executable  string          `json:"executable,omitempty"`
	Args        []string        `json:"args,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// PluginHook is a legacy listing-only hook descriptor. Hook execution has a
// separate secure composition boundary and must never execute Command here.
type PluginHook struct {
	Event   string `json:"event"`
	Command string `json:"command"`
}

// PluginCommand is a legacy listing-only slash command descriptor.
type PluginCommand struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Command     string `json:"command"`
}

// PluginAgentType is a custom agent type from a plugin.
type PluginAgentType struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	SystemPrompt string   `json:"systemPrompt"`
	Tools        []string `json:"tools"`
}

// PluginManager loads and manages plugins.
type PluginManager struct {
	plugins []Plugin
	dirs    []string
	loadErr error
}

// NewPluginManager creates a plugin manager.
func NewPluginManager(dirs ...string) *PluginManager {
	return &PluginManager{dirs: append([]string(nil), dirs...)}
}

// DefaultPluginDirs is the shared project/global discovery contract used by
// both the agent composition root and the listing API.
func DefaultPluginDirs(workDir string) []string {
	directories := make([]string, 0, 2)
	if strings.TrimSpace(workDir) != "" {
		directories = append(directories, filepath.Join(workDir, ".claude", "plugins"))
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		directories = append(directories, filepath.Join(home, ".claude", "plugins"))
	}
	return directories
}

// LoadAll preserves the original best-effort API for the listing endpoint.
// Parsing is nevertheless strict and transactional: one invalid manifest
// results in an empty set, never a partially trusted catalog.
func (pm *PluginManager) LoadAll() {
	if err := pm.LoadAllStrict(); err != nil {
		log.Printf("[Plugins] Load rejected: %v", err)
	}
}

// LoadAllStrict scans plugin directories using bounded reads, duplicate-key
// detection and DisallowUnknownFields. The receiver changes only after every
// discovered manifest has passed validation.
func (pm *PluginManager) LoadAllStrict() error {
	loaded, err := loadPluginManifests(pm.dirs)
	pm.loadErr = err
	if err != nil {
		pm.plugins = nil
		return err
	}
	pm.plugins = loaded
	for _, plugin := range loaded {
		log.Printf("[Plugins] Loaded: %s v%s (%d tools, %d hooks, %d commands)",
			plugin.Name, plugin.Version,
			len(plugin.Tools), len(plugin.Hooks), len(plugin.Commands))
	}
	return nil
}

// LoadError returns the latest strict loading error, if any.
func (pm *PluginManager) LoadError() error {
	return pm.loadErr
}

// GetPlugins returns a defensive copy of all loaded plugins.
func (pm *PluginManager) GetPlugins() []Plugin {
	result := make([]Plugin, len(pm.plugins))
	for index, plugin := range pm.plugins {
		result[index] = clonePlugin(plugin)
	}
	return result
}

// GetAllTools returns tools from all plugins. Legacy command tools can be
// displayed by callers but are deliberately not executable.
func (pm *PluginManager) GetAllTools() []PluginTool {
	var tools []PluginTool
	for _, plugin := range pm.plugins {
		for _, tool := range plugin.Tools {
			tools = append(tools, clonePluginTool(tool))
		}
	}
	return tools
}

// GetAllHooks returns hooks from all plugins.
func (pm *PluginManager) GetAllHooks() []PluginHook {
	var hooks []PluginHook
	for _, plugin := range pm.plugins {
		hooks = append(hooks, plugin.Hooks...)
	}
	return append([]PluginHook(nil), hooks...)
}

// GetAllCommands returns commands from all plugins.
func (pm *PluginManager) GetAllCommands() []PluginCommand {
	var commands []PluginCommand
	for _, plugin := range pm.plugins {
		commands = append(commands, plugin.Commands...)
	}
	return append([]PluginCommand(nil), commands...)
}

func loadPluginManifests(dirs []string) ([]Plugin, error) {
	plugins := make([]Plugin, 0)
	pluginNames := make(map[string]string)
	toolNames := make(map[string]string)
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("scan plugin directory: %w", err)
		}
		for _, entry := range entries {
			if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				continue
			}
			if len(plugins) >= maxPlugins {
				return nil, fmt.Errorf("plugin count exceeds limit of %d", maxPlugins)
			}
			pluginDir := filepath.Join(dir, entry.Name())
			manifestPath := filepath.Join(pluginDir, "plugin.json")
			data, err := readBoundedRegularPluginFile(manifestPath, maxPluginManifestBytes)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, fmt.Errorf("read plugin manifest %q: %w", entry.Name(), err)
			}
			if err := rejectDuplicatePluginJSONFields(data); err != nil {
				return nil, fmt.Errorf("parse plugin manifest %q: %w", entry.Name(), err)
			}
			var plugin Plugin
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&plugin); err != nil {
				return nil, fmt.Errorf("parse plugin manifest %q: %w", entry.Name(), err)
			}
			if err := requirePluginJSONEOF(decoder); err != nil {
				return nil, fmt.Errorf("parse plugin manifest %q: %w", entry.Name(), err)
			}
			canonicalRoot, err := canonicalPluginRoot(pluginDir)
			if err != nil {
				return nil, fmt.Errorf("validate plugin %q: %w", entry.Name(), err)
			}
			plugin.rootDir = canonicalRoot
			plugin.manifestDigest = sha256Bytes(data)
			if err := validatePluginManifest(&plugin); err != nil {
				return nil, fmt.Errorf("validate plugin %q: %w", entry.Name(), err)
			}
			if previous, duplicate := pluginNames[plugin.Name]; duplicate {
				return nil, fmt.Errorf("duplicate plugin name %q in %s and %s", plugin.Name, previous, entry.Name())
			}
			pluginNames[plugin.Name] = entry.Name()
			for _, tool := range plugin.Tools {
				if previous, duplicate := toolNames[tool.Name]; duplicate {
					return nil, fmt.Errorf("duplicate plugin tool name %q in %s and %s", tool.Name, previous, plugin.Name)
				}
				toolNames[tool.Name] = plugin.Name
			}
			plugins = append(plugins, plugin)
		}
	}
	return plugins, nil
}

func validatePluginManifest(plugin *Plugin) error {
	if err := validatePluginIdentifier("plugin name", plugin.Name); err != nil {
		return err
	}
	if strings.TrimSpace(plugin.Version) == "" {
		return errors.New("plugin version is required")
	}
	for label, value := range map[string]string{
		"version": plugin.Version, "description": plugin.Description, "author": plugin.Author,
	} {
		if err := validatePluginString(label, value, maxPluginStringBytes); err != nil {
			return err
		}
	}
	if len(plugin.Tools) > maxPluginTools || len(plugin.Hooks) > maxPluginHooks ||
		len(plugin.Commands) > maxPluginCommands || len(plugin.AgentTypes) > maxPluginAgents {
		return errors.New("plugin descriptor count exceeds a manifest limit")
	}

	toolNames := make(map[string]struct{}, len(plugin.Tools))
	for index := range plugin.Tools {
		tool := &plugin.Tools[index]
		if err := validatePluginIdentifier("tool name", tool.Name); err != nil {
			return err
		}
		if _, duplicate := toolNames[tool.Name]; duplicate {
			return fmt.Errorf("duplicate tool name %q", tool.Name)
		}
		toolNames[tool.Name] = struct{}{}
		if _, reserved := currentBuiltInToolDefinitions()[tool.Name]; reserved {
			return fmt.Errorf("plugin tool %q collides with a reserved built-in name", tool.Name)
		}
		if err := validatePluginString("tool description", tool.Description, maxPluginStringBytes); err != nil {
			return err
		}
		if err := validatePluginString("legacy tool command", tool.Command, maxPluginStringBytes); err != nil {
			return err
		}
		if err := validatePluginString("tool executable", tool.Executable, maxPluginStringBytes); err != nil {
			return err
		}
		if tool.Command != "" && tool.Executable != "" {
			return fmt.Errorf("tool %q cannot combine legacy command and executable", tool.Name)
		}
		if len(tool.Args) > maxPluginArgs {
			return fmt.Errorf("tool %q has too many constant arguments", tool.Name)
		}
		for _, argument := range tool.Args {
			if err := validatePluginString("tool argument", argument, maxPluginArgumentBytes); err != nil {
				return err
			}
		}
		if tool.Executable == "" && len(tool.Args) != 0 {
			return fmt.Errorf("tool %q has arguments without an executable", tool.Name)
		}
		if tool.Executable != "" && len(bytes.TrimSpace(tool.InputSchema)) == 0 {
			return fmt.Errorf("executable tool %q requires input_schema", tool.Name)
		}
		if len(bytes.TrimSpace(tool.InputSchema)) != 0 {
			if err := validatePluginToolSchema(tool.InputSchema); err != nil {
				return fmt.Errorf("tool %q input_schema: %w", tool.Name, err)
			}
		}
	}

	if err := validateNamedPluginDescriptors(plugin.Commands, plugin.AgentTypes); err != nil {
		return err
	}
	for _, hook := range plugin.Hooks {
		if err := validatePluginString("hook event", hook.Event, maxPluginStringBytes); err != nil {
			return err
		}
		if strings.TrimSpace(hook.Event) == "" {
			return errors.New("hook event is required")
		}
		if err := validatePluginString("legacy hook command", hook.Command, maxPluginStringBytes); err != nil {
			return err
		}
	}
	return nil
}

func validateNamedPluginDescriptors(commands []PluginCommand, agents []PluginAgentType) error {
	commandNames := make(map[string]struct{}, len(commands))
	for _, command := range commands {
		if err := validatePluginIdentifier("command name", command.Name); err != nil {
			return err
		}
		if _, duplicate := commandNames[command.Name]; duplicate {
			return fmt.Errorf("duplicate command name %q", command.Name)
		}
		commandNames[command.Name] = struct{}{}
		for label, value := range map[string]string{"command description": command.Description, "legacy command": command.Command} {
			if err := validatePluginString(label, value, maxPluginStringBytes); err != nil {
				return err
			}
		}
	}
	agentNames := make(map[string]struct{}, len(agents))
	for _, agentType := range agents {
		if err := validatePluginIdentifier("agent name", agentType.Name); err != nil {
			return err
		}
		if _, duplicate := agentNames[agentType.Name]; duplicate {
			return fmt.Errorf("duplicate agent name %q", agentType.Name)
		}
		agentNames[agentType.Name] = struct{}{}
		for label, value := range map[string]string{"agent description": agentType.Description, "agent system prompt": agentType.SystemPrompt} {
			if err := validatePluginString(label, value, maxPluginStringBytes); err != nil {
				return err
			}
		}
		if len(agentType.Tools) > maxPluginTools {
			return fmt.Errorf("agent %q has too many tools", agentType.Name)
		}
		for _, name := range agentType.Tools {
			if err := validatePluginIdentifier("agent tool name", name); err != nil {
				return err
			}
		}
	}
	return nil
}

func validatePluginToolSchema(raw json.RawMessage) error {
	value, err := decodeToolJSON(raw)
	if err != nil {
		return err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return errors.New("schema root must be an object")
	}
	compiled, err := compileToolSchema(object, "$", 0)
	if err != nil {
		return err
	}
	if len(compiled.typeSet) > 0 {
		if _, objectType := compiled.typeSet["object"]; !objectType {
			return errors.New("schema root must accept object input")
		}
	}
	return nil
}

func validatePluginIdentifier(label, value string) error {
	if value == "" || len(value) > 128 {
		return fmt.Errorf("%s must contain 1 to 128 characters", label)
	}
	for index, character := range []byte(value) {
		valid := character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '_' || character == '-' || character == '.' || character == ':'
		if !valid || index == 0 && character >= '0' && character <= '9' {
			return fmt.Errorf("%s %q contains unsupported characters", label, value)
		}
	}
	return nil
}

func validatePluginString(label, value string, maximum int) error {
	if len(value) > maximum {
		return fmt.Errorf("%s exceeds %d bytes", label, maximum)
	}
	if strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("%s contains NUL", label)
	}
	return nil
}

func readBoundedRegularPluginFile(path string, maximum int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("file must be a non-symlink regular file")
	}
	if info.Size() > maximum {
		return nil, fmt.Errorf("file exceeds %d-byte limit", maximum)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("file exceeds %d-byte limit", maximum)
	}
	return data, nil
}

func canonicalPluginRoot(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("plugin root is not a directory")
	}
	return filepath.Clean(resolved), nil
}

func rejectDuplicatePluginJSONFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := scanUniquePluginJSONValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("manifest contains trailing JSON")
		}
		return err
	}
	return nil
}

func scanUniquePluginJSONValue(decoder *json.Decoder, depth int) error {
	if depth > maxPluginJSONDepth {
		return fmt.Errorf("manifest nesting exceeds %d levels", maxPluginJSONDepth)
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key must be a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate field %q", key)
			}
			seen[key] = struct{}{}
			if err := scanUniquePluginJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			if err != nil {
				return err
			}
			return errors.New("malformed object")
		}
	case '[':
		for decoder.More() {
			if err := scanUniquePluginJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			if err != nil {
				return err
			}
			return errors.New("malformed array")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	return nil
}

func requirePluginJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("manifest contains trailing JSON")
		}
		return err
	}
	return nil
}

func clonePlugin(plugin Plugin) Plugin {
	result := plugin
	result.Tools = make([]PluginTool, len(plugin.Tools))
	for index, tool := range plugin.Tools {
		result.Tools[index] = clonePluginTool(tool)
	}
	result.Hooks = append([]PluginHook(nil), plugin.Hooks...)
	result.Commands = append([]PluginCommand(nil), plugin.Commands...)
	result.AgentTypes = make([]PluginAgentType, len(plugin.AgentTypes))
	for index, agentType := range plugin.AgentTypes {
		result.AgentTypes[index] = agentType
		result.AgentTypes[index].Tools = append([]string(nil), agentType.Tools...)
	}
	return result
}

func clonePluginTool(tool PluginTool) PluginTool {
	result := tool
	result.Args = append([]string(nil), tool.Args...)
	result.InputSchema = append(json.RawMessage(nil), tool.InputSchema...)
	return result
}

func sha256Bytes(value []byte) string {
	digest := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(digest[:])
}
