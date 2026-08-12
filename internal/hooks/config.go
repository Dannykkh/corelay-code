package hooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxHookConfigBytes   = 256 * 1024
	maxHookJSONDepth     = 32
	maxHookCount         = 64
	maxHookCommandBytes  = 4096
	maxHookArgumentCount = 64
	maxHookArgumentBytes = 4096
	maxHookInvocation    = 16 * 1024
	maxHookMatcherBytes  = 512
	maxHookSourceBytes   = 32
	maxHookTimeoutSecond = 300
	defaultHookTimeout   = 30 * time.Second
)

type hookConfigError struct{}

var exactHookMatcherCharacters = regexp.MustCompile(`^[A-Za-z0-9_\- ,|]+$`)

func (hookConfigError) Error() string { return "hook configuration rejected" }

func loadRegistrySnapshot(workDir, skillSource string) (registrySnapshot, error) {
	snapshot := registrySnapshot{}
	if strings.TrimSpace(workDir) == "" {
		if _, err := resolveSourceList(skillSource); err != nil {
			snapshot.loadFailure = HookFailureConfigInvalid
			return snapshot, hookConfigError{}
		}
		return snapshot, nil
	}
	canonical, err := canonicalWorkspace(workDir)
	if err != nil {
		snapshot.loadFailure = HookFailureConfigInvalid
		return snapshot, hookConfigError{}
	}
	snapshot.workDir = canonical
	sources, err := resolveSourceList(skillSource)
	if err != nil {
		snapshot.loadFailure = HookFailureConfigInvalid
		return snapshot, hookConfigError{}
	}

	hooks := make([]Hook, 0)
	for _, source := range sources {
		loaded, loadErr := loadHooksFromSource(canonical, source)
		if loadErr != nil {
			snapshot.loadFailure = HookFailureConfigInvalid
			return snapshot, hookConfigError{}
		}
		if len(hooks)+len(loaded) > maxHookCount {
			snapshot.loadFailure = HookFailureConfigInvalid
			return snapshot, hookConfigError{}
		}
		hooks = append(hooks, loaded...)
	}
	snapshot.hooks = cloneHooks(hooks)
	return snapshot, nil
}

func resolveSourceList(skillSource string) ([]string, error) {
	switch strings.ToLower(strings.TrimSpace(skillSource)) {
	case "claude":
		return []string{"claude"}, nil
	case "codex":
		return []string{"codex"}, nil
	case "gemini":
		return []string{"gemini"}, nil
	case "none":
		return nil, nil
	case "", "all":
		return []string{"claude", "codex", "gemini"}, nil
	default:
		return nil, hookConfigError{}
	}
}

func loadHooksFromSource(workDir, source string) ([]Hook, error) {
	switch source {
	case "claude":
		return loadClaudeHooks(workDir)
	case "codex":
		return loadSimpleHooks(workDir, "codex.json", "codex")
	case "gemini":
		return loadSimpleHooks(workDir, filepath.Join(".gemini", "settings.json"), "gemini")
	default:
		return nil, hookConfigError{}
	}
}

type claudeFlatHookDefinition struct {
	Matcher string          `json:"matcher"`
	Command string          `json:"command"`
	Args    json.RawMessage `json:"args"`
	Timeout int             `json:"timeout"`
}

type claudeHookGroup struct {
	Matcher string            `json:"matcher"`
	Hooks   []json.RawMessage `json:"hooks"`
}

type claudeCommandHandler struct {
	Type    string          `json:"type"`
	Command string          `json:"command"`
	Args    json.RawMessage `json:"args"`
	Timeout int             `json:"timeout"`
}

func loadClaudeHooks(workDir string) ([]Hook, error) {
	paths := []string{
		filepath.Join(".claude", "settings.json"),
		filepath.Join(".claude", "settings.local.json"),
		filepath.Join(".claude", "settings-local.json"),
	}
	hooks := make([]Hook, 0)
	for _, relative := range paths {
		data, exists, err := readProjectConfig(workDir, relative)
		if err != nil {
			return nil, err
		}
		if !exists {
			continue
		}
		rawHooks, err := extractHooksObject(data)
		if err != nil {
			return nil, err
		}
		keys := sortedRawKeys(rawHooks)
		for _, key := range keys {
			hookType, ok := parseHookType(key)
			if !ok {
				return nil, hookConfigError{}
			}
			var rawDefinitions []json.RawMessage
			if err := decodeStrictValue(rawHooks[key], &rawDefinitions); err != nil {
				return nil, hookConfigError{}
			}
			for _, rawDefinition := range rawDefinitions {
				flattened, err := decodeClaudeHookDefinition(hookType, rawDefinition)
				if err != nil {
					return nil, err
				}
				hooks = append(hooks, flattened...)
				if len(hooks) > maxHookCount {
					return nil, hookConfigError{}
				}
			}
		}
	}
	return hooks, nil
}

func decodeClaudeHookDefinition(hookType HookType, rawDefinition json.RawMessage) ([]Hook, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(rawDefinition, &object); err != nil || object == nil {
		return nil, hookConfigError{}
	}
	if _, nested := object["hooks"]; !nested {
		var definition claudeFlatHookDefinition
		if err := decodeStrictValue(rawDefinition, &definition); err != nil {
			return nil, hookConfigError{}
		}
		arguments, err := decodeHookArguments(object, definition.Args)
		if err != nil {
			return nil, err
		}
		hook := Hook{
			Type:    hookType,
			Matcher: definition.Matcher,
			Command: definition.Command,
			Args:    arguments,
			Timeout: definition.Timeout,
			Source:  "claude",
		}
		if err := validateHook(hook); err != nil {
			return nil, err
		}
		return []Hook{hook}, nil
	}

	var group claudeHookGroup
	if err := decodeStrictValue(rawDefinition, &group); err != nil || len(group.Hooks) == 0 {
		return nil, hookConfigError{}
	}
	if err := validateHookMatcher(hookType, group.Matcher); err != nil {
		return nil, err
	}
	hooks := make([]Hook, 0, len(group.Hooks))
	for _, rawHandler := range group.Hooks {
		var handlerObject map[string]json.RawMessage
		if err := json.Unmarshal(rawHandler, &handlerObject); err != nil || handlerObject == nil {
			return nil, hookConfigError{}
		}
		var handler claudeCommandHandler
		if err := decodeStrictValue(rawHandler, &handler); err != nil || handler.Type != "command" {
			return nil, hookConfigError{}
		}
		arguments, err := decodeHookArguments(handlerObject, handler.Args)
		if err != nil {
			return nil, err
		}
		hook := Hook{
			Type:    hookType,
			Matcher: group.Matcher,
			Command: handler.Command,
			Args:    arguments,
			Timeout: handler.Timeout,
			Source:  "claude",
		}
		if err := validateHook(hook); err != nil {
			return nil, err
		}
		hooks = append(hooks, hook)
	}
	return hooks, nil
}

func decodeHookArguments(object map[string]json.RawMessage, raw json.RawMessage) ([]string, error) {
	if _, exists := object["args"]; !exists {
		return nil, nil
	}
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, hookConfigError{}
	}
	var arguments []string
	if err := decodeStrictValue(raw, &arguments); err != nil || arguments == nil {
		return nil, hookConfigError{}
	}
	return arguments, nil
}

func loadSimpleHooks(workDir, relative, source string) ([]Hook, error) {
	data, exists, err := readProjectConfig(workDir, relative)
	if err != nil || !exists {
		return nil, err
	}
	rawHooks, err := extractHooksObject(data)
	if err != nil {
		return nil, err
	}
	keys := sortedRawKeys(rawHooks)
	hooks := make([]Hook, 0, len(keys))
	for _, key := range keys {
		hookType, ok := parseHookType(key)
		if !ok {
			return nil, hookConfigError{}
		}
		var command string
		if err := decodeStrictValue(rawHooks[key], &command); err != nil {
			return nil, hookConfigError{}
		}
		hook := Hook{Type: hookType, Command: command, Source: source}
		if err := validateHook(hook); err != nil {
			return nil, err
		}
		hooks = append(hooks, hook)
	}
	return hooks, nil
}

func extractHooksObject(data []byte) (map[string]json.RawMessage, error) {
	if err := validateStrictJSON(data, maxHookJSONDepth); err != nil {
		return nil, hookConfigError{}
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, hookConfigError{}
	}
	raw, exists := top["hooks"]
	if !exists || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return map[string]json.RawMessage{}, nil
	}
	var hooks map[string]json.RawMessage
	if err := json.Unmarshal(raw, &hooks); err != nil || hooks == nil {
		return nil, hookConfigError{}
	}
	return hooks, nil
}

func validateHook(hook Hook) error {
	if !validHookType(hook.Type) || strings.TrimSpace(hook.Command) == "" ||
		len(hook.Command) > maxHookCommandBytes || !utf8.ValidString(hook.Command) ||
		strings.ContainsRune(hook.Command, '\x00') || containsDisallowedControl(hook.Command) {
		return hookConfigError{}
	}
	if hook.Timeout < 0 || hook.Timeout > maxHookTimeoutSecond {
		return hookConfigError{}
	}
	if hook.Source == "" || len(hook.Source) > maxHookSourceBytes {
		return hookConfigError{}
	}
	if err := validateHookMatcher(hook.Type, hook.Matcher); err != nil {
		return err
	}
	if len(hook.Args) > maxHookArgumentCount {
		return hookConfigError{}
	}
	invocationBytes := len(hook.Command)
	for _, argument := range hook.Args {
		if len(argument) > maxHookArgumentBytes || !utf8.ValidString(argument) ||
			strings.ContainsRune(argument, '\x00') || containsDisallowedControl(argument) {
			return hookConfigError{}
		}
		invocationBytes += len(argument)
	}
	if invocationBytes > maxHookInvocation {
		return hookConfigError{}
	}
	return nil
}

func validateHookMatcher(hookType HookType, matcher string) error {
	if len(matcher) > maxHookMatcherBytes || !utf8.ValidString(matcher) || strings.ContainsRune(matcher, '\x00') {
		return hookConfigError{}
	}
	matcher = strings.TrimSpace(matcher)
	if matcher == "" || matcher == "*" {
		return nil
	}
	switch hookType {
	case HookPreToolUse, HookPostToolUse, HookPreCompact, HookPostCompact:
	default:
		// This loop does not retain Claude's session-start/session-end reason,
		// so accepting a meaningful matcher there would silently misapply it.
		return hookConfigError{}
	}
	if exactHookMatcherCharacters.MatchString(matcher) {
		for _, candidate := range splitExactHookMatcher(matcher) {
			if candidate == "" {
				return hookConfigError{}
			}
		}
		return nil
	}
	if _, err := regexp.Compile(matcher); err != nil {
		// JavaScript-only regex features cannot be reproduced by Go's RE2
		// engine and are rejected rather than matched with different semantics.
		return hookConfigError{}
	}
	return nil
}

func matchingHooks(candidates []Hook, environment map[string]string) ([]Hook, HookFailureCode) {
	matching := make([]Hook, 0, len(candidates))
	for _, hook := range candidates {
		matcher := strings.TrimSpace(hook.Matcher)
		if matcher == "" || matcher == "*" {
			matching = append(matching, hook)
			continue
		}
		target := ""
		switch hook.Type {
		case HookPreToolUse, HookPostToolUse:
			target = environment["TOOL_NAME"]
		case HookPreCompact, HookPostCompact:
			target = environment["COMPACT_TRIGGER"]
		default:
			return nil, HookFailureConfigInvalid
		}
		if target == "" {
			return nil, HookFailureEnvironmentInvalid
		}
		matched, err := hookMatcherMatches(matcher, target)
		if err != nil {
			return nil, HookFailureConfigInvalid
		}
		if matched {
			matching = append(matching, hook)
		}
	}
	return matching, HookFailureNone
}

func hookMatcherMatches(matcher, target string) (bool, error) {
	matcher = strings.TrimSpace(matcher)
	if matcher == "" || matcher == "*" {
		return true, nil
	}
	if exactHookMatcherCharacters.MatchString(matcher) {
		for _, candidate := range splitExactHookMatcher(matcher) {
			if candidate == target {
				return true, nil
			}
		}
		return false, nil
	}
	compiled, err := regexp.Compile(matcher)
	if err != nil {
		return false, err
	}
	return compiled.MatchString(target), nil
}

func splitExactHookMatcher(matcher string) []string {
	normalized := strings.ReplaceAll(matcher, ",", "|")
	parts := strings.Split(normalized, "|")
	for index := range parts {
		parts[index] = strings.TrimSpace(parts[index])
	}
	return parts
}

func validHookType(hookType HookType) bool {
	switch hookType {
	case HookPreToolUse, HookPostToolUse, HookSessionStart, HookSessionEnd, HookPreCompact, HookPostCompact:
		return true
	default:
		return false
	}
}

func parseHookType(value string) (HookType, bool) {
	switch value {
	case string(HookPreToolUse), "PreToolUse":
		return HookPreToolUse, true
	case string(HookPostToolUse), "PostToolUse":
		return HookPostToolUse, true
	case string(HookSessionStart), "SessionStart":
		return HookSessionStart, true
	case string(HookSessionEnd), "SessionEnd":
		return HookSessionEnd, true
	case string(HookPreCompact), "PreCompact":
		return HookPreCompact, true
	case string(HookPostCompact), "PostCompact":
		return HookPostCompact, true
	default:
		return "", false
	}
}

func readProjectConfig(workDir, relative string) ([]byte, bool, error) {
	path := filepath.Join(workDir, relative)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, hookConfigError{}
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil || !pathWithinWorkspace(workDir, resolved) {
		return nil, false, hookConfigError{}
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxHookConfigBytes {
		return nil, false, hookConfigError{}
	}
	file, err := os.Open(resolved)
	if err != nil {
		return nil, false, hookConfigError{}
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxHookConfigBytes+1))
	if err != nil || len(data) > maxHookConfigBytes {
		return nil, false, hookConfigError{}
	}
	return data, true, nil
}

func canonicalWorkspace(workDir string) (string, error) {
	absolute, err := filepath.Abs(workDir)
	if err != nil {
		return "", hookConfigError{}
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", hookConfigError{}
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", hookConfigError{}
	}
	return filepath.Clean(resolved), nil
}

func pathWithinWorkspace(workDir, candidate string) bool {
	relative, err := filepath.Rel(workDir, candidate)
	if err != nil || filepath.IsAbs(relative) {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func pathIsRegular(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func sortedRawKeys(values map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func decodeStrictValue(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return hookConfigError{}
	}
	return nil
}

func validateStrictJSON(data []byte, maxDepth int) error {
	if len(data) == 0 || !utf8.Valid(data) {
		return hookConfigError{}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := scanJSONValue(decoder, 0, maxDepth); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return hookConfigError{}
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder, depth, maxDepth int) error {
	if depth > maxDepth {
		return hookConfigError{}
	}
	token, err := decoder.Token()
	if err != nil {
		return hookConfigError{}
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return hookConfigError{}
			}
			key, ok := keyToken.(string)
			if !ok {
				return hookConfigError{}
			}
			if _, exists := seen[key]; exists {
				return hookConfigError{}
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(decoder, depth+1, maxDepth); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return hookConfigError{}
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder, depth+1, maxDepth); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return hookConfigError{}
		}
	default:
		return hookConfigError{}
	}
	return nil
}

func containsDisallowedControl(value string) bool {
	for _, character := range value {
		if character < 0x20 && character != '\n' && character != '\r' && character != '\t' {
			return true
		}
	}
	return false
}
