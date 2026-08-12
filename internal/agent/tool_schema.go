package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/Dannykkh/corelay-code/internal/types"
)

const (
	toolCatalogSchemaMarkerPrefix       = "\x00corelay-tool-catalog-schema:"
	legacyToolCatalogSchemaMarkerPrefix = "\x00aniclew-tool-catalog-schema:"
	maxToolSchemaDepth                  = 64
	boundToolExecutionProtocol          = "corelay.tool-execution.v1"
	legacyBoundToolExecutionProtocol    = "aniclew.tool-execution.v1"
)

type toolCatalogSchemaSnapshot struct {
	schemas          map[string]json.RawMessage
	identities       map[string]toolExecutorIdentity
	pluginExecutors  map[string]pluginExecutorBinding
	mcpExecutors     map[string]mcpRuntimeExecutorBinding
	processGlobalMCP bool
	err              error
}

type toolExecutorProvenance uint8

const (
	toolExecutorBuiltIn toolExecutorProvenance = iota + 1
	toolExecutorMCP
	toolExecutorPlugin
	toolExecutorOther
)

type toolExecutorIdentity struct {
	CatalogMarker string                 `json:"catalog_marker"`
	ToolName      string                 `json:"tool_name"`
	Kind          toolExecutorProvenance `json:"kind"`
	Owner         string                 `json:"owner"`
	ExecutorID    string                 `json:"executor_id"`
	SchemaDigest  string                 `json:"schema_digest"`
	Token         string                 `json:"token"`
}

type boundToolExecutionEnvelope struct {
	Protocol string               `json:"protocol"`
	Identity toolExecutorIdentity `json:"identity"`
	Input    json.RawMessage      `json:"input"`
}

var toolCatalogSchemaSnapshots sync.Map

var (
	builtInToolDefinitionsOnce sync.Once
	builtInToolDefinitions     map[string]types.ToolDef
)

// registerToolCatalogSchemas binds an immutable schema snapshot to a marker
// carried by the existing allowed-name map. This preserves the dispatcher API
// used by all loops while retaining the exact post-pruning catalog, including
// dynamically supplied MCP and plugin ToolDefs.
func registerToolCatalogSchemas(tools []types.ToolDef) string {
	return registerToolCatalogSchemasWithLegacyMCP(tools, true)
}

// registerRunToolCatalogSchemas deliberately excludes process-global MCP
// clients. Run-owned MCP bindings are carried by ToolDef.RuntimeBinding and
// therefore remain attached to the exact catalog generation.
func registerRunToolCatalogSchemas(tools []types.ToolDef) string {
	return registerToolCatalogSchemasWithLegacyMCP(tools, false)
}

func registerToolCatalogSchemasWithLegacyMCP(tools []types.ToolDef, includeLegacyMCP bool) string {
	type descriptor struct {
		Name       string          `json:"name"`
		Schema     json.RawMessage `json:"schema"`
		Owner      string          `json:"owner,omitempty"`
		ExecutorID string          `json:"executor_id,omitempty"`
	}
	type catalogDescriptor struct {
		Tools []descriptor `json:"tools"`
		MCP   []descriptor `json:"mcp"`
	}
	descriptors := make([]descriptor, 0, len(tools))
	legacyDescriptors := make([]descriptor, 0, len(tools))
	for _, tool := range tools {
		item := descriptor{
			Name:   tool.Name,
			Schema: append(json.RawMessage(nil), tool.InputSchema...),
		}
		legacyItem := item
		if binding, bound, err := mcpRuntimeBindingFromTool(tool); err == nil && bound {
			item.Owner = binding.serverName
			item.ExecutorID = binding.runtimeID + ":" + binding.executorID
			legacyItem = item
		} else if binding, bound, err := pluginRuntimeBindingFromTool(tool); err == nil && bound {
			item.Owner = binding.pluginName
			item.ExecutorID = binding.executorID
			legacyItem.Owner = binding.pluginName
			legacyItem.ExecutorID = legacyPluginExecutorID(binding)
		} else if tool.RuntimeBinding != nil {
			item.Owner = "invalid-runtime-binding"
			item.ExecutorID = fmt.Sprintf("%T", tool.RuntimeBinding)
			legacyItem = item
		}
		descriptors = append(descriptors, item)
		legacyDescriptors = append(legacyDescriptors, legacyItem)
	}
	sort.SliceStable(descriptors, func(i, j int) bool {
		if descriptors[i].Name == descriptors[j].Name {
			return bytes.Compare(descriptors[i].Schema, descriptors[j].Schema) < 0
		}
		return descriptors[i].Name < descriptors[j].Name
	})
	sort.SliceStable(legacyDescriptors, func(i, j int) bool {
		if legacyDescriptors[i].Name == legacyDescriptors[j].Name {
			return bytes.Compare(legacyDescriptors[i].Schema, legacyDescriptors[j].Schema) < 0
		}
		return legacyDescriptors[i].Name < legacyDescriptors[j].Name
	})
	var mcpBindings []mcpToolBinding
	if includeLegacyMCP {
		mcpBindings = getMCPToolBindings()
	}
	mcpDescriptors := make([]descriptor, 0, len(mcpBindings))
	for _, binding := range mcpBindings {
		mcpDescriptors = append(mcpDescriptors, descriptor{
			Name:       binding.Tool.Name,
			Schema:     append(json.RawMessage(nil), binding.Tool.InputSchema...),
			Owner:      binding.ServerName,
			ExecutorID: binding.ExecutorID,
		})
	}
	sort.SliceStable(mcpDescriptors, func(i, j int) bool {
		if mcpDescriptors[i].Name == mcpDescriptors[j].Name {
			return bytes.Compare(mcpDescriptors[i].Schema, mcpDescriptors[j].Schema) < 0
		}
		return mcpDescriptors[i].Name < mcpDescriptors[j].Name
	})
	encoded, _ := json.Marshal(catalogDescriptor{Tools: descriptors, MCP: mcpDescriptors})
	digest := sha256.Sum256(encoded)
	marker := toolCatalogSchemaMarkerPrefix + hex.EncodeToString(digest[:])
	legacyEncoded, _ := json.Marshal(catalogDescriptor{Tools: legacyDescriptors, MCP: mcpDescriptors})
	legacyDigest := sha256.Sum256(legacyEncoded)
	legacyMarker := legacyToolCatalogSchemaMarkerPrefix + hex.EncodeToString(legacyDigest[:])

	snapshot := buildToolCatalogSchemaSnapshotForCatalog(marker, tools, mcpBindings, includeLegacyMCP)
	toolCatalogSchemaSnapshots.LoadOrStore(marker, snapshot)
	// Paused runs may still carry the pre-rename marker. It resolves to the
	// corresponding legacy identities while new identities use Corelay.
	legacySnapshot := buildToolCatalogSchemaSnapshotForCatalog(legacyMarker, tools, mcpBindings, includeLegacyMCP)
	canonicalizeLegacyToolCatalogSnapshot(&legacySnapshot)
	toolCatalogSchemaSnapshots.LoadOrStore(legacyMarker, legacySnapshot)
	return marker
}

func canonicalizeLegacyToolCatalogSnapshot(snapshot *toolCatalogSchemaSnapshot) {
	if snapshot == nil || snapshot.err != nil {
		return
	}
	for name, identity := range snapshot.identities {
		switch identity.Kind {
		case toolExecutorBuiltIn:
			identity.Owner = "aniclew"
		case toolExecutorPlugin:
			binding, ok := snapshot.pluginExecutors[name]
			if !ok {
				continue
			}
			binding.executorID = legacyPluginExecutorID(binding)
			snapshot.pluginExecutors[name] = binding
			identity.ExecutorID = binding.executorID
		}
		identity.Token = toolExecutorIdentityToken(identity)
		snapshot.identities[name] = identity
	}
}

func buildToolCatalogSchemaSnapshot(
	marker string,
	tools []types.ToolDef,
	mcpBindings []mcpToolBinding,
) toolCatalogSchemaSnapshot {
	return buildToolCatalogSchemaSnapshotForCatalog(marker, tools, mcpBindings, true)
}

func buildToolCatalogSchemaSnapshotForCatalog(
	marker string,
	tools []types.ToolDef,
	mcpBindings []mcpToolBinding,
	processGlobalMCP bool,
) toolCatalogSchemaSnapshot {
	snapshot := toolCatalogSchemaSnapshot{
		schemas:          make(map[string]json.RawMessage, len(tools)),
		identities:       make(map[string]toolExecutorIdentity, len(tools)),
		pluginExecutors:  make(map[string]pluginExecutorBinding),
		mcpExecutors:     make(map[string]mcpRuntimeExecutorBinding),
		processGlobalMCP: processGlobalMCP,
	}
	builtIns := currentBuiltInToolDefinitions()
	mcpByName := make(map[string][]mcpToolBinding, len(mcpBindings))
	for _, binding := range mcpBindings {
		mcpByName[binding.Tool.Name] = append(mcpByName[binding.Tool.Name], binding)
	}
	runtimeMCPByName := make(map[string][]mcpRuntimeExecutorBinding)
	for _, tool := range tools {
		binding, bound, err := mcpRuntimeBindingFromTool(tool)
		if err != nil {
			snapshot.err = err
			return snapshot
		}
		if bound {
			runtimeMCPByName[tool.Name] = append(runtimeMCPByName[tool.Name], binding)
		}
	}
	for name, matches := range mcpByName {
		if _, reserved := builtIns[name]; reserved {
			snapshot.err = fmt.Errorf("MCP tool %q collides with a reserved built-in name", name)
			return snapshot
		}
		if len(matches)+len(runtimeMCPByName[name]) > 1 {
			snapshot.err = fmt.Errorf("multiple MCP executors claim tool name %q", name)
			return snapshot
		}
	}
	for name, matches := range runtimeMCPByName {
		if _, reserved := builtIns[name]; reserved {
			snapshot.err = fmt.Errorf("MCP tool %q collides with a reserved built-in name", name)
			return snapshot
		}
		if len(matches)+len(mcpByName[name]) > 1 {
			snapshot.err = fmt.Errorf("multiple MCP executors claim tool name %q", name)
			return snapshot
		}
	}

	for _, tool := range tools {
		if _, duplicate := snapshot.schemas[tool.Name]; duplicate {
			snapshot.err = fmt.Errorf("tool catalog contains duplicate definition %q", tool.Name)
			return snapshot
		}
		identity := toolExecutorIdentity{
			CatalogMarker: marker,
			ToolName:      tool.Name,
			SchemaDigest:  toolSchemaDigest(tool.InputSchema),
		}
		mcpRuntimeBinding, mcpRuntimeBound, mcpRuntimeErr := mcpRuntimeBindingFromTool(tool)
		if mcpRuntimeErr != nil {
			snapshot.err = mcpRuntimeErr
			return snapshot
		}
		var pluginBinding pluginExecutorBinding
		var pluginBound bool
		if !mcpRuntimeBound {
			var pluginErr error
			pluginBinding, pluginBound, pluginErr = pluginRuntimeBindingFromTool(tool)
			if pluginErr != nil {
				snapshot.err = pluginErr
				return snapshot
			}
		}
		if builtIn, reserved := builtIns[tool.Name]; reserved {
			if pluginBound || mcpRuntimeBound {
				snapshot.err = fmt.Errorf("dynamic tool %q collides with a reserved built-in name", tool.Name)
				return snapshot
			}
			allowedVariant := tool.Name == "Edit" && isAllowedEditToolSchema(tool.InputSchema)
			if !allowedVariant && !bytes.Equal(bytes.TrimSpace(tool.InputSchema), bytes.TrimSpace(builtIn.InputSchema)) {
				snapshot.err = fmt.Errorf("tool %q overrides a reserved built-in schema", tool.Name)
				return snapshot
			}
			identity.Kind = toolExecutorBuiltIn
			identity.Owner = "corelaycode"
			identity.ExecutorID = "builtin:" + tool.Name
		} else if pluginBound {
			if matches := len(mcpByName[tool.Name]) + len(runtimeMCPByName[tool.Name]); matches != 0 {
				snapshot.err = fmt.Errorf("plugin tool %q collides with an MCP executor", tool.Name)
				return snapshot
			}
			identity.Kind = toolExecutorPlugin
			identity.Owner = pluginBinding.pluginName
			identity.ExecutorID = pluginBinding.executorID
			snapshot.pluginExecutors[tool.Name] = clonePluginExecutorBinding(pluginBinding)
		} else if mcpRuntimeBound {
			identity.Kind = toolExecutorMCP
			identity.Owner = mcpRuntimeBinding.serverName
			identity.ExecutorID = mcpRuntimeBinding.executorID
			snapshot.mcpExecutors[tool.Name] = mcpRuntimeBinding
		} else if matches := mcpByName[tool.Name]; len(matches) == 1 {
			if !bytes.Equal(bytes.TrimSpace(tool.InputSchema), bytes.TrimSpace(matches[0].Tool.InputSchema)) {
				snapshot.err = fmt.Errorf("tool %q does not match its MCP executor schema", tool.Name)
				return snapshot
			}
			identity.Kind = toolExecutorMCP
			identity.Owner = matches[0].ServerName
			identity.ExecutorID = matches[0].ExecutorID
		} else {
			identity.Kind = toolExecutorOther
			identity.Owner = "unbound"
			identity.ExecutorID = "unbound:" + tool.Name
		}
		identity.Token = toolExecutorIdentityToken(identity)
		snapshot.schemas[tool.Name] = append(json.RawMessage(nil), tool.InputSchema...)
		snapshot.identities[tool.Name] = identity
	}
	return snapshot
}

func currentBuiltInToolDefinitions() map[string]types.ToolDef {
	builtInToolDefinitionsOnce.Do(func() {
		definitions := append([]types.ToolDef(nil), ToolDefs("")...)
		definitions = append(definitions, ExtendedToolDefs()...)
		definitions = append(definitions, ComputerUseToolDefs()...)
		definitions = append(definitions, AdvancedToolDefs()...)
		// LoadToolResult is advertised only for runs with a bound reader, but it
		// remains a first-class built-in identity when present in that immutable
		// run catalog.
		definitions = append(definitions, loadToolResultDefinition())
		// ReportCompletion is appended only by strict-run composition, but its
		// fixed identity is always reserved against MCP/plugin replacement.
		definitions = append(definitions, ReportCompletionToolDef())
		builtInToolDefinitions = make(map[string]types.ToolDef, len(definitions))
		for _, definition := range definitions {
			builtInToolDefinitions[definition.Name] = definition
		}
	})
	return builtInToolDefinitions
}

func snapshotForAllowedTools(allowed map[string]struct{}) (toolCatalogSchemaSnapshot, error) {
	marker := ""
	for candidate := range allowed {
		if !isToolCatalogSchemaMarker(candidate) {
			continue
		}
		if marker != "" && marker != candidate {
			return toolCatalogSchemaSnapshot{}, fmt.Errorf("tool catalog has conflicting schema snapshots")
		}
		marker = candidate
	}
	if marker == "" {
		return toolCatalogSchemaSnapshot{}, fmt.Errorf("tool catalog schema snapshot is unavailable")
	}
	loaded, ok := toolCatalogSchemaSnapshots.Load(marker)
	if !ok {
		return toolCatalogSchemaSnapshot{}, fmt.Errorf("tool catalog schema snapshot is unavailable")
	}
	snapshot, ok := loaded.(toolCatalogSchemaSnapshot)
	if !ok {
		return toolCatalogSchemaSnapshot{}, fmt.Errorf("tool catalog schema snapshot is invalid")
	}
	if snapshot.err != nil {
		return toolCatalogSchemaSnapshot{}, snapshot.err
	}
	return snapshot, nil
}

func schemaForAllowedTool(allowed map[string]struct{}, toolName string) (json.RawMessage, error) {
	snapshot, err := snapshotForAllowedTools(allowed)
	if err != nil {
		return nil, err
	}
	schema, ok := snapshot.schemas[toolName]
	if !ok {
		return nil, fmt.Errorf("tool %q has no schema in the immutable catalog", toolName)
	}
	if len(bytes.TrimSpace(schema)) == 0 {
		return nil, fmt.Errorf("tool %q has an empty input schema", toolName)
	}
	return append(json.RawMessage(nil), schema...), nil
}

func executorIdentityForAllowedTool(
	allowed map[string]struct{},
	toolName string,
) (toolExecutorIdentity, error) {
	snapshot, err := snapshotForAllowedTools(allowed)
	if err != nil {
		return toolExecutorIdentity{}, err
	}
	identity, ok := snapshot.identities[toolName]
	if !ok {
		return toolExecutorIdentity{}, fmt.Errorf("tool %q has no bound executor identity", toolName)
	}
	return identity, nil
}

func validateBoundToolExecutorIdentity(identity toolExecutorIdentity) error {
	if identity.CatalogMarker == "" || identity.ToolName == "" || identity.Token == "" {
		return fmt.Errorf("bound executor identity is incomplete")
	}
	loaded, ok := toolCatalogSchemaSnapshots.Load(identity.CatalogMarker)
	if !ok {
		return fmt.Errorf("bound executor catalog is unavailable")
	}
	snapshot, ok := loaded.(toolCatalogSchemaSnapshot)
	if !ok || snapshot.err != nil {
		return fmt.Errorf("bound executor catalog is invalid")
	}
	expected, ok := snapshot.identities[identity.ToolName]
	if !ok || expected != identity || identity.Token != toolExecutorIdentityToken(identity) {
		return fmt.Errorf("bound executor token does not match the immutable catalog")
	}
	return nil
}

func toolSchemaDigest(schema json.RawMessage) string {
	digest := sha256.Sum256(bytes.TrimSpace(schema))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func toolExecutorIdentityToken(identity toolExecutorIdentity) string {
	identity.Token = ""
	encoded, _ := json.Marshal(identity)
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func bindToolExecutionInput(
	input json.RawMessage,
	identity toolExecutorIdentity,
) (json.RawMessage, error) {
	if err := validateBoundToolExecutorIdentity(identity); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(boundToolExecutionEnvelope{
		Protocol: boundToolExecutionProtocol,
		Identity: identity,
		Input:    append(json.RawMessage(nil), input...),
	})
	if err != nil {
		return nil, fmt.Errorf("encode bound tool execution: %w", err)
	}
	return encoded, nil
}

func unwrapBoundToolExecutionInput(
	input json.RawMessage,
) (json.RawMessage, toolExecutorIdentity, bool, error) {
	var probe struct {
		Protocol string `json:"protocol"`
	}
	if json.Unmarshal(input, &probe) != nil ||
		!renamedProtocolMatches(probe.Protocol, boundToolExecutionProtocol, legacyBoundToolExecutionProtocol) {
		return input, toolExecutorIdentity{}, false, nil
	}
	var envelope boundToolExecutionEnvelope
	if err := json.Unmarshal(input, &envelope); err != nil {
		return nil, toolExecutorIdentity{}, true, fmt.Errorf("decode bound tool execution: %w", err)
	}
	if len(bytes.TrimSpace(envelope.Input)) == 0 {
		return nil, toolExecutorIdentity{}, true, fmt.Errorf("bound tool input is empty")
	}
	if err := validateBoundToolExecutorIdentity(envelope.Identity); err != nil {
		return nil, toolExecutorIdentity{}, true, err
	}
	return append(json.RawMessage(nil), envelope.Input...), envelope.Identity, true, nil
}

func isToolCatalogSchemaMarker(value string) bool {
	return strings.HasPrefix(value, toolCatalogSchemaMarkerPrefix) ||
		strings.HasPrefix(value, legacyToolCatalogSchemaMarkerPrefix)
}

func renamedProtocolMatches(value, current, legacy string) bool {
	return value == current || value == legacy
}

// validateToolExecutorIdentity prevents a dynamically connected MCP server
// from hijacking a built-in/plugin name after the immutable catalog was
// compiled. It also ensures an MCP catalog entry still resolves to exactly one
// executor with the schema captured for this run.
func validateToolExecutorIdentity(allowed map[string]struct{}, toolName string) error {
	identity, err := executorIdentityForAllowedTool(allowed, toolName)
	if err != nil {
		return err
	}
	snapshot, err := snapshotForAllowedTools(allowed)
	if err != nil {
		return err
	}
	current := make([]mcpToolBinding, 0, 1)
	if snapshot.processGlobalMCP {
		for _, binding := range getMCPToolBindings() {
			if binding.Tool.Name == toolName {
				current = append(current, binding)
			}
		}
	}
	switch identity.Kind {
	case toolExecutorBuiltIn, toolExecutorOther:
		if len(current) != 0 {
			return fmt.Errorf("MCP executor now collides with bound tool %q", toolName)
		}
	case toolExecutorMCP:
		if binding, runOwned := snapshot.mcpExecutors[toolName]; runOwned {
			definition := types.ToolDef{
				Name: toolName, InputSchema: snapshot.schemas[toolName],
				RuntimeBinding: mcpToolRuntimeMetadata{binding: binding},
			}
			if binding.serverName != identity.Owner || binding.executorID != identity.ExecutorID {
				return fmt.Errorf("MCP tool %q runtime identity changed after catalog resolution", toolName)
			}
			return validateMCPRuntimeBindingDefinition(definition, binding)
		}
		if len(current) != 1 {
			return fmt.Errorf("MCP tool %q no longer has exactly one bound executor", toolName)
		}
		if current[0].ServerName != identity.Owner ||
			current[0].ExecutorID != identity.ExecutorID ||
			toolSchemaDigest(current[0].Tool.InputSchema) != identity.SchemaDigest {
			return fmt.Errorf("MCP tool %q executor schema changed after catalog resolution", toolName)
		}
	case toolExecutorPlugin:
		if len(current) != 0 {
			return fmt.Errorf("MCP executor now collides with bound plugin tool %q", toolName)
		}
		loaded, ok := toolCatalogSchemaSnapshots.Load(identity.CatalogMarker)
		if !ok {
			return fmt.Errorf("plugin tool %q catalog is unavailable", toolName)
		}
		snapshot, ok := loaded.(toolCatalogSchemaSnapshot)
		if !ok || snapshot.err != nil {
			return fmt.Errorf("plugin tool %q catalog is invalid", toolName)
		}
		binding, ok := snapshot.pluginExecutors[toolName]
		if !ok || binding.executorID != identity.ExecutorID || binding.pluginName != identity.Owner {
			return fmt.Errorf("plugin tool %q executor identity changed after catalog resolution", toolName)
		}
		definition := types.ToolDef{
			Name:           toolName,
			InputSchema:    snapshot.schemas[toolName],
			RuntimeBinding: pluginToolRuntimeMetadata{binding: binding},
		}
		if err := validatePluginBindingDefinition(definition, binding); err != nil {
			return err
		}
		if err := validatePluginBindingArtifacts(binding); err != nil {
			return err
		}
	default:
		return fmt.Errorf("tool %q has invalid executor provenance", toolName)
	}
	return nil
}

func catalogUsesProcessGlobalMCP(marker string) bool {
	loaded, ok := toolCatalogSchemaSnapshots.Load(marker)
	if !ok {
		return false
	}
	snapshot, ok := loaded.(toolCatalogSchemaSnapshot)
	return ok && snapshot.processGlobalMCP
}

type compiledToolSchema struct {
	types            []string
	typeSet          map[string]struct{}
	properties       map[string]*compiledToolSchema
	required         []string
	enum             []any
	hasEnum          bool
	items            *compiledToolSchema
	additionalMode   toolAdditionalPropertiesMode
	additionalSchema *compiledToolSchema
	minLength        *int
	maxLength        *int
	minItems         *int
	maxItems         *int
	uniqueItems      bool
	minimum          *big.Rat
	maximum          *big.Rat
}

type toolAdditionalPropertiesMode uint8

const (
	toolAdditionalAllow toolAdditionalPropertiesMode = iota
	toolAdditionalDeny
	toolAdditionalSchema
)

// validateToolInputSchema validates one normalized tool input against the
// exact ToolDef.InputSchema advertised for this run. Unsupported schema
// keywords remain annotations; malformed supported keywords fail closed.
func validateToolInputSchema(input, schema json.RawMessage) error {
	inputValue, err := decodeToolJSON(input)
	if err != nil {
		return fmt.Errorf("input is malformed JSON: %w", err)
	}
	if _, ok := inputValue.(map[string]any); !ok {
		return fmt.Errorf("$: expected object")
	}

	schemaValue, err := decodeToolJSON(schema)
	if err != nil {
		return fmt.Errorf("schema is malformed JSON: %w", err)
	}
	schemaObject, ok := schemaValue.(map[string]any)
	if !ok {
		return fmt.Errorf("schema root must be an object")
	}
	compiled, err := compileToolSchema(schemaObject, "$", 0)
	if err != nil {
		return fmt.Errorf("malformed schema: %w", err)
	}
	return compiled.validate(inputValue, "$", 0)
}

func decodeToolJSON(raw json.RawMessage) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("multiple JSON values")
		}
		return nil, err
	}
	return value, nil
}

func compileToolSchema(object map[string]any, path string, depth int) (*compiledToolSchema, error) {
	if depth > maxToolSchemaDepth {
		return nil, fmt.Errorf("%s: schema nesting exceeds %d levels", path, maxToolSchemaDepth)
	}
	compiled := &compiledToolSchema{
		typeSet:        make(map[string]struct{}),
		properties:     make(map[string]*compiledToolSchema),
		additionalMode: toolAdditionalAllow,
	}

	if declared, exists := object["type"]; exists {
		var declaredTypes []string
		switch value := declared.(type) {
		case string:
			declaredTypes = []string{value}
		case []any:
			if len(value) == 0 {
				return nil, fmt.Errorf("%s.type: type array must not be empty", path)
			}
			for index, item := range value {
				name, ok := item.(string)
				if !ok {
					return nil, fmt.Errorf("%s.type[%d]: expected string", path, index)
				}
				declaredTypes = append(declaredTypes, name)
			}
		default:
			return nil, fmt.Errorf("%s.type: expected string or string array", path)
		}
		for _, name := range declaredTypes {
			if !supportedToolSchemaType(name) {
				return nil, fmt.Errorf("%s.type: unsupported type %q", path, name)
			}
			if _, duplicate := compiled.typeSet[name]; duplicate {
				continue
			}
			compiled.typeSet[name] = struct{}{}
			compiled.types = append(compiled.types, name)
		}
	}

	if declared, exists := object["properties"]; exists {
		properties, ok := declared.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s.properties: expected object", path)
		}
		propertyNames := make([]string, 0, len(properties))
		for name := range properties {
			propertyNames = append(propertyNames, name)
		}
		sort.Strings(propertyNames)
		for _, name := range propertyNames {
			rawProperty := properties[name]
			propertyObject, ok := rawProperty.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s: property schema must be an object", toolSchemaPropertyPath(path, name))
			}
			property, err := compileToolSchema(propertyObject, toolSchemaPropertyPath(path, name), depth+1)
			if err != nil {
				return nil, err
			}
			compiled.properties[name] = property
		}
	}

	if declared, exists := object["required"]; exists {
		required, ok := declared.([]any)
		if !ok {
			return nil, fmt.Errorf("%s.required: expected string array", path)
		}
		for index, rawName := range required {
			name, ok := rawName.(string)
			if !ok {
				return nil, fmt.Errorf("%s.required[%d]: expected string", path, index)
			}
			compiled.required = append(compiled.required, name)
		}
		sort.Strings(compiled.required)
	}

	if declared, exists := object["enum"]; exists {
		values, ok := declared.([]any)
		if !ok || len(values) == 0 {
			return nil, fmt.Errorf("%s.enum: expected non-empty array", path)
		}
		compiled.enum = values
		compiled.hasEnum = true
	}

	if declared, exists := object["items"]; exists {
		itemObject, ok := declared.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s.items: expected schema object", path)
		}
		items, err := compileToolSchema(itemObject, path+"[]", depth+1)
		if err != nil {
			return nil, err
		}
		compiled.items = items
	}

	if declared, exists := object["additionalProperties"]; exists {
		switch value := declared.(type) {
		case bool:
			if !value {
				compiled.additionalMode = toolAdditionalDeny
			}
		case map[string]any:
			additional, err := compileToolSchema(value, path+".*", depth+1)
			if err != nil {
				return nil, err
			}
			compiled.additionalMode = toolAdditionalSchema
			compiled.additionalSchema = additional
		default:
			return nil, fmt.Errorf("%s.additionalProperties: expected boolean or schema object", path)
		}
	}

	var constraintErr error
	if declared, exists := object["minLength"]; exists {
		compiled.minLength, constraintErr = compileToolSchemaSize(declared, path+".minLength")
		if constraintErr != nil {
			return nil, constraintErr
		}
	}
	if declared, exists := object["maxLength"]; exists {
		compiled.maxLength, constraintErr = compileToolSchemaSize(declared, path+".maxLength")
		if constraintErr != nil {
			return nil, constraintErr
		}
	}
	if compiled.minLength != nil && compiled.maxLength != nil && *compiled.minLength > *compiled.maxLength {
		return nil, fmt.Errorf("%s: minLength exceeds maxLength", path)
	}
	if declared, exists := object["minItems"]; exists {
		compiled.minItems, constraintErr = compileToolSchemaSize(declared, path+".minItems")
		if constraintErr != nil {
			return nil, constraintErr
		}
	}
	if declared, exists := object["maxItems"]; exists {
		compiled.maxItems, constraintErr = compileToolSchemaSize(declared, path+".maxItems")
		if constraintErr != nil {
			return nil, constraintErr
		}
	}
	if compiled.minItems != nil && compiled.maxItems != nil && *compiled.minItems > *compiled.maxItems {
		return nil, fmt.Errorf("%s: minItems exceeds maxItems", path)
	}
	if declared, exists := object["uniqueItems"]; exists {
		unique, ok := declared.(bool)
		if !ok {
			return nil, fmt.Errorf("%s.uniqueItems: expected boolean", path)
		}
		compiled.uniqueItems = unique
	}
	if declared, exists := object["minimum"]; exists {
		compiled.minimum, constraintErr = compileToolSchemaNumber(declared, path+".minimum")
		if constraintErr != nil {
			return nil, constraintErr
		}
	}
	if declared, exists := object["maximum"]; exists {
		compiled.maximum, constraintErr = compileToolSchemaNumber(declared, path+".maximum")
		if constraintErr != nil {
			return nil, constraintErr
		}
	}
	if compiled.minimum != nil && compiled.maximum != nil && compiled.minimum.Cmp(compiled.maximum) > 0 {
		return nil, fmt.Errorf("%s: minimum exceeds maximum", path)
	}

	return compiled, nil
}

func compileToolSchemaSize(value any, path string) (*int, error) {
	number, ok := value.(json.Number)
	if !ok {
		return nil, fmt.Errorf("%s: expected non-negative integer", path)
	}
	rational, ok := new(big.Rat).SetString(number.String())
	if !ok || !rational.IsInt() || rational.Sign() < 0 || !rational.Num().IsInt64() {
		return nil, fmt.Errorf("%s: expected non-negative integer", path)
	}
	integer := rational.Num().Int64()
	converted := int(integer)
	if converted < 0 || int64(converted) != integer {
		return nil, fmt.Errorf("%s: integer is too large", path)
	}
	return &converted, nil
}

func compileToolSchemaNumber(value any, path string) (*big.Rat, error) {
	number, ok := value.(json.Number)
	if !ok {
		return nil, fmt.Errorf("%s: expected number", path)
	}
	rational, ok := new(big.Rat).SetString(number.String())
	if !ok {
		return nil, fmt.Errorf("%s: expected number", path)
	}
	return rational, nil
}

func supportedToolSchemaType(name string) bool {
	switch name {
	case "string", "number", "integer", "boolean", "object", "array", "null":
		return true
	default:
		return false
	}
}

func (schema *compiledToolSchema) validate(value any, path string, depth int) error {
	if depth > maxToolSchemaDepth {
		return fmt.Errorf("%s: input nesting exceeds %d levels", path, maxToolSchemaDepth)
	}
	if schema.hasEnum {
		matched := false
		for _, candidate := range schema.enum {
			if toolJSONValuesEqual(value, candidate) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%s: value is not in the allowed enum", path)
		}
	}
	if len(schema.types) > 0 && !schema.acceptsType(value) {
		return fmt.Errorf("%s: expected %s", path, strings.Join(schema.types, " or "))
	}
	if text, ok := value.(string); ok {
		length := utf8.RuneCountInString(text)
		if schema.minLength != nil && length < *schema.minLength {
			return fmt.Errorf("%s: string is shorter than minLength", path)
		}
		if schema.maxLength != nil && length > *schema.maxLength {
			return fmt.Errorf("%s: string exceeds maxLength", path)
		}
	}
	if number, ok := value.(json.Number); ok && (schema.minimum != nil || schema.maximum != nil) {
		rational, valid := new(big.Rat).SetString(number.String())
		if !valid {
			return fmt.Errorf("%s: invalid number", path)
		}
		if schema.minimum != nil && rational.Cmp(schema.minimum) < 0 {
			return fmt.Errorf("%s: number is below minimum", path)
		}
		if schema.maximum != nil && rational.Cmp(schema.maximum) > 0 {
			return fmt.Errorf("%s: number exceeds maximum", path)
		}
	}

	switch typed := value.(type) {
	case map[string]any:
		for _, name := range schema.required {
			if _, exists := typed[name]; !exists {
				return fmt.Errorf("%s: required property is missing", toolSchemaPropertyPath(path, name))
			}
		}
		propertyNames := make([]string, 0, len(typed))
		for name := range typed {
			propertyNames = append(propertyNames, name)
		}
		sort.Strings(propertyNames)
		for _, name := range propertyNames {
			propertyValue := typed[name]
			propertyPath := toolSchemaPropertyPath(path, name)
			if property, exists := schema.properties[name]; exists {
				if err := property.validate(propertyValue, propertyPath, depth+1); err != nil {
					return err
				}
				continue
			}
			switch schema.additionalMode {
			case toolAdditionalDeny:
				return fmt.Errorf("%s: property is not allowed", propertyPath)
			case toolAdditionalSchema:
				if err := schema.additionalSchema.validate(propertyValue, propertyPath, depth+1); err != nil {
					return err
				}
			}
		}
	case []any:
		if schema.minItems != nil && len(typed) < *schema.minItems {
			return fmt.Errorf("%s: array has fewer than minItems", path)
		}
		if schema.maxItems != nil && len(typed) > *schema.maxItems {
			return fmt.Errorf("%s: array exceeds maxItems", path)
		}
		if schema.uniqueItems {
			for left := 0; left < len(typed); left++ {
				for right := left + 1; right < len(typed); right++ {
					if toolJSONValuesEqual(typed[left], typed[right]) {
						return fmt.Errorf("%s[%d]: duplicate array item", path, right)
					}
				}
			}
		}
		if schema.items != nil {
			for index, item := range typed {
				if err := schema.items.validate(item, fmt.Sprintf("%s[%d]", path, index), depth+1); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (schema *compiledToolSchema) acceptsType(value any) bool {
	for _, expected := range schema.types {
		switch expected {
		case "string":
			_, ok := value.(string)
			if ok {
				return true
			}
		case "number":
			_, ok := value.(json.Number)
			if ok {
				return true
			}
		case "integer":
			if number, ok := value.(json.Number); ok && toolJSONNumberIsInteger(number) {
				return true
			}
		case "boolean":
			_, ok := value.(bool)
			if ok {
				return true
			}
		case "object":
			_, ok := value.(map[string]any)
			if ok {
				return true
			}
		case "array":
			_, ok := value.([]any)
			if ok {
				return true
			}
		case "null":
			if value == nil {
				return true
			}
		}
	}
	return false
}

func toolJSONNumberIsInteger(number json.Number) bool {
	rational, ok := new(big.Rat).SetString(number.String())
	return ok && rational.IsInt()
}

func toolJSONValuesEqual(left, right any) bool {
	leftNumber, leftIsNumber := left.(json.Number)
	rightNumber, rightIsNumber := right.(json.Number)
	if leftIsNumber || rightIsNumber {
		if !leftIsNumber || !rightIsNumber {
			return false
		}
		leftRational, leftOK := new(big.Rat).SetString(leftNumber.String())
		rightRational, rightOK := new(big.Rat).SetString(rightNumber.String())
		return leftOK && rightOK && leftRational.Cmp(rightRational) == 0
	}

	switch typedLeft := left.(type) {
	case map[string]any:
		typedRight, ok := right.(map[string]any)
		if !ok || len(typedLeft) != len(typedRight) {
			return false
		}
		for key, leftValue := range typedLeft {
			rightValue, exists := typedRight[key]
			if !exists || !toolJSONValuesEqual(leftValue, rightValue) {
				return false
			}
		}
		return true
	case []any:
		typedRight, ok := right.([]any)
		if !ok || len(typedLeft) != len(typedRight) {
			return false
		}
		for index := range typedLeft {
			if !toolJSONValuesEqual(typedLeft[index], typedRight[index]) {
				return false
			}
		}
		return true
	case string:
		typedRight, ok := right.(string)
		return ok && typedLeft == typedRight
	case bool:
		typedRight, ok := right.(bool)
		return ok && typedLeft == typedRight
	case nil:
		return right == nil
	default:
		return false
	}
}

func toolSchemaPropertyPath(base, name string) string {
	if name != "" {
		identifier := true
		for index, current := range name {
			if !(current == '_' || current == '-' ||
				current >= 'a' && current <= 'z' ||
				current >= 'A' && current <= 'Z' ||
				index > 0 && current >= '0' && current <= '9') {
				identifier = false
				break
			}
		}
		if identifier {
			return base + "." + name
		}
	}
	encoded, _ := json.Marshal(name)
	return base + "[" + string(encoded) + "]"
}
