package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/types"
)

var richToolInputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "action": {"type": "string", "enum": ["build", "test"]},
    "threshold": {"type": "number"},
    "attempts": {"type": "integer"},
    "dry_run": {"type": "boolean"},
    "config": {
      "type": "object",
      "properties": {"label": {"type": "string"}},
      "required": ["label"],
      "additionalProperties": false
    },
    "jobs": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {"name": {"type": "string"}},
        "required": ["name"],
        "additionalProperties": false
      }
    }
  },
  "required": ["action", "config", "jobs"],
  "additionalProperties": false
}`)

func TestValidateToolInputSchemaAcceptsSupportedShapes(t *testing.T) {
	valid := []json.RawMessage{
		json.RawMessage(`{
      "action":"build",
      "threshold":0.75,
      "attempts":2,
      "dry_run":true,
      "config":{"label":"release"},
      "jobs":[{"name":"unit"},{"name":"integration"}]
    }`),
		// JSON Schema integer is mathematical, so 2.0 is an integer.
		json.RawMessage(`{
      "action":"test",
      "attempts":2.0,
      "config":{"label":"ci"},
      "jobs":[]
    }`),
	}
	for index, input := range valid {
		if err := validateToolInputSchema(input, richToolInputSchema); err != nil {
			t.Fatalf("valid input %d rejected: %v", index, err)
		}
	}

	permissive := json.RawMessage(`{
    "type":"object",
    "properties":{"known":{"type":"string"}}
  }`)
	if err := validateToolInputSchema(
		json.RawMessage(`{"known":"yes","undeclared":{"nested":true}}`),
		permissive,
	); err != nil {
		t.Fatalf("undeclared property rejected without additionalProperties=false: %v", err)
	}

	typedAdditional := json.RawMessage(`{
    "type":"object",
    "additionalProperties":{"type":"integer"}
  }`)
	if err := validateToolInputSchema(json.RawMessage(`{"workers":3}`), typedAdditional); err != nil {
		t.Fatalf("typed additional property rejected: %v", err)
	}
}

func TestValidateToolInputSchemaRejectsContractViolations(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "root shape", input: `[]`, want: "$: expected object"},
		{name: "required", input: `{"action":"build","jobs":[]}`, want: "$.config: required property is missing"},
		{name: "string", input: `{"action":"build","config":{"label":4},"jobs":[]}`, want: "$.config.label: expected string"},
		{name: "number", input: `{"action":"build","threshold":"high","config":{"label":"x"},"jobs":[]}`, want: "$.threshold: expected number"},
		{name: "integer", input: `{"action":"build","attempts":1.5,"config":{"label":"x"},"jobs":[]}`, want: "$.attempts: expected integer"},
		{name: "boolean", input: `{"action":"build","dry_run":"false","config":{"label":"x"},"jobs":[]}`, want: "$.dry_run: expected boolean"},
		{name: "object", input: `{"action":"build","config":[],"jobs":[]}`, want: "$.config: expected object"},
		{name: "array", input: `{"action":"build","config":{"label":"x"},"jobs":{}}`, want: "$.jobs: expected array"},
		{name: "enum", input: `{"action":"deploy","config":{"label":"x"},"jobs":[]}`, want: "$.action: value is not in the allowed enum"},
		{name: "nested required", input: `{"action":"build","config":{"label":"x"},"jobs":[{}]}`, want: "$.jobs[0].name: required property is missing"},
		{name: "nested item type", input: `{"action":"build","config":{"label":"x"},"jobs":[{"name":7}]}`, want: "$.jobs[0].name: expected string"},
		{name: "root additional", input: `{"action":"build","config":{"label":"x"},"jobs":[],"extra":1}`, want: "$.extra: property is not allowed"},
		{name: "nested additional", input: `{"action":"build","config":{"label":"x","extra":1},"jobs":[]}`, want: "$.config.extra: property is not allowed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateToolInputSchema(json.RawMessage(test.input), richToolInputSchema)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}

	typedAdditional := json.RawMessage(`{
    "type":"object",
    "additionalProperties":{"type":"integer"}
  }`)
	err := validateToolInputSchema(json.RawMessage(`{"workers":1.5}`), typedAdditional)
	if err == nil || !strings.Contains(err.Error(), "$.workers: expected integer") {
		t.Fatalf("typed additional property error = %v", err)
	}
}

func TestValidateToolInputSchemaRejectsMalformedSupportedKeywords(t *testing.T) {
	tests := []struct {
		name   string
		schema string
		want   string
	}{
		{name: "invalid JSON", schema: `{`, want: "schema is malformed JSON"},
		{name: "root array", schema: `[]`, want: "schema root must be an object"},
		{name: "numeric type", schema: `{"type":1}`, want: "type: expected string or string array"},
		{name: "unsupported type", schema: `{"type":"date"}`, want: "unsupported type"},
		{name: "properties array", schema: `{"properties":[]}`, want: "properties: expected object"},
		{name: "property boolean", schema: `{"properties":{"x":true}}`, want: "property schema must be an object"},
		{name: "required string", schema: `{"required":"x"}`, want: "required: expected string array"},
		{name: "required non-string", schema: `{"required":[1]}`, want: "required[0]: expected string"},
		{name: "enum scalar", schema: `{"enum":"x"}`, want: "enum: expected non-empty array"},
		{name: "empty enum", schema: `{"enum":[]}`, want: "enum: expected non-empty array"},
		{name: "items array", schema: `{"items":[]}`, want: "items: expected schema object"},
		{name: "additional string", schema: `{"additionalProperties":"no"}`, want: "additionalProperties: expected boolean or schema object"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateToolInputSchema(json.RawMessage(`{}`), json.RawMessage(test.schema))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestToolCatalogSchemaSnapshotsRemainCatalogSpecific(t *testing.T) {
	stringCatalog := toolCatalogNames([]types.ToolDef{{
		Name:        "DynamicTool",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"]}`),
	}})
	integerCatalog := toolCatalogNames([]types.ToolDef{{
		Name:        "DynamicTool",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"integer"}},"required":["value"]}`),
	}})

	stringSchema, err := schemaForAllowedTool(stringCatalog, "DynamicTool")
	if err != nil {
		t.Fatalf("string catalog schema: %v", err)
	}
	integerSchema, err := schemaForAllowedTool(integerCatalog, "DynamicTool")
	if err != nil {
		t.Fatalf("integer catalog schema: %v", err)
	}
	if err := validateToolInputSchema(json.RawMessage(`{"value":"x"}`), stringSchema); err != nil {
		t.Fatalf("string catalog rejected string: %v", err)
	}
	if err := validateToolInputSchema(json.RawMessage(`{"value":"x"}`), integerSchema); err == nil {
		t.Fatal("integer catalog reused a same-name schema from another snapshot")
	}
	if _, err := schemaForAllowedTool(map[string]struct{}{"DynamicTool": {}}, "DynamicTool"); err == nil {
		t.Fatal("catalog without an immutable schema snapshot was accepted")
	}
}

func TestToolCatalogRejectsDuplicateAndReservedExecutorNames(t *testing.T) {
	bash := currentBuiltInToolDefinitions()["Bash"]
	tests := []struct {
		name  string
		tools []types.ToolDef
		mcp   []mcpToolBinding
		want  string
	}{
		{
			name: "duplicate catalog definition",
			tools: []types.ToolDef{
				{Name: "Dynamic", InputSchema: json.RawMessage(`{"type":"object"}`)},
				{Name: "Dynamic", InputSchema: json.RawMessage(`{"type":"object"}`)},
			},
			want: `duplicate definition "Dynamic"`,
		},
		{
			name:  "reserved MCP name",
			tools: []types.ToolDef{bash},
			mcp: []mcpToolBinding{{
				ServerName: "server-a",
				ExecutorID: "executor-a",
				Tool: MCPTool{
					Name:        "Bash",
					InputSchema: bash.InputSchema,
				},
			}},
			want: `collides with a reserved built-in name`,
		},
		{
			name: "reserved schema override",
			tools: []types.ToolDef{{
				Name:        "Bash",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			}},
			want: `overrides a reserved built-in schema`,
		},
		{
			name: "duplicate MCP executor",
			tools: []types.ToolDef{{
				Name:        "mcp_dynamic",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			}},
			mcp: []mcpToolBinding{
				{ServerName: "server-a", ExecutorID: "executor-a", Tool: MCPTool{Name: "mcp_dynamic", InputSchema: json.RawMessage(`{"type":"object"}`)}},
				{ServerName: "server-b", ExecutorID: "executor-b", Tool: MCPTool{Name: "mcp_dynamic", InputSchema: json.RawMessage(`{"type":"object"}`)}},
			},
			want: `multiple MCP executors claim tool name`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := buildToolCatalogSchemaSnapshot("test:"+test.name, test.tools, test.mcp)
			if snapshot.err == nil || !strings.Contains(snapshot.err.Error(), test.want) {
				t.Fatalf("snapshot error = %v, want substring %q", snapshot.err, test.want)
			}
		})
	}
}
