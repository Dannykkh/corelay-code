package agent

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestRecoverLeakedToolCallsPreservesLegacyFormatsAndIDs(t *testing.T) {
	text := `prefix <tool_call>{"name":"Write","arguments":{"file_path":"b.txt","content":"b"}}</tool_call>
<function=Read><parameter=file_path> a.txt </parameter></function> suffix`

	calls, cleaned := recoverLeakedToolCalls(text)
	if cleaned != "prefix \n suffix" {
		t.Fatalf("cleaned = %q, want %q", cleaned, "prefix \n suffix")
	}
	if len(calls) != 2 {
		t.Fatalf("len(calls) = %d, want 2", len(calls))
	}

	// Legacy recovery has always grouped function XML before tool_call JSON,
	// independent of their source positions. Keep that ID behavior stable.
	if calls[0].ID != "call_recovered_0" || calls[0].Name != "Read" {
		t.Fatalf("first call = %#v", calls[0])
	}
	if calls[1].ID != "call_recovered_1" || calls[1].Name != "Write" {
		t.Fatalf("second call = %#v", calls[1])
	}
	assertJSONEqual(t, calls[0].Input, json.RawMessage(`{"file_path":"a.txt"}`))
	assertJSONEqual(t, calls[1].Input, json.RawMessage(`{"content":"b","file_path":"b.txt"}`))
	if calls[0].InputRaw != string(calls[0].Input) || calls[1].InputRaw != string(calls[1].Input) {
		t.Fatal("InputRaw no longer mirrors Input")
	}
}

func TestRecoverLeakedToolCallsKeepsLegacyArgumentShape(t *testing.T) {
	calls, cleaned := recoverLeakedToolCalls(`<tool_call>{"name":"Read","arguments":"legacy-value"}</tool_call>`)
	if cleaned != "" || len(calls) != 1 {
		t.Fatalf("calls=%#v cleaned=%q", calls, cleaned)
	}
	if string(calls[0].Input) != `"legacy-value"` {
		t.Fatalf("legacy arguments = %s", calls[0].Input)
	}
}

func TestRecoverLeakedToolCallsPreservesLegacyClosedEnvelopeCleaning(t *testing.T) {
	calls, cleaned := recoverLeakedToolCalls(`<tool_call>{"name":123}</tool_call>`)
	if len(calls) != 0 || cleaned != "" {
		t.Fatalf("legacy malformed envelope behavior changed: calls=%#v cleaned=%q", calls, cleaned)
	}
}

func TestRecoverLeakedToolCallsFencedJSONWithExplicitAlias(t *testing.T) {
	text := "working\n```json\n{\"tool\":\"read_file\",\"args\":{\"file_path\":\"a.txt\"}}\n```\ndone"
	opts := ToolRecoveryOptions{
		Aliases:      map[string]string{"read_file": "Read"},
		AllowedTools: map[string]struct{}{"Read": {}},
	}

	calls, cleaned := recoverLeakedToolCallsWithOptions(text, opts)
	if cleaned != "working\n\ndone" {
		t.Fatalf("cleaned = %q", cleaned)
	}
	if len(calls) != 1 || calls[0].Name != "Read" || calls[0].ID != "call_recovered_0" {
		t.Fatalf("calls = %#v", calls)
	}
	assertJSONEqual(t, calls[0].Input, json.RawMessage(`{"file_path":"a.txt"}`))
}

func TestRecoverLeakedToolCallsBareJSON(t *testing.T) {
	calls, cleaned := recoverLeakedToolCalls(` {"tool_name":"Write","parameters":{"file_path":"a.txt","content":"ok"}} `)
	if cleaned != "" || len(calls) != 1 {
		t.Fatalf("calls=%#v cleaned=%q", calls, cleaned)
	}
	if calls[0].Name != "Write" {
		t.Fatalf("name = %q", calls[0].Name)
	}
	assertJSONEqual(t, calls[0].Input, json.RawMessage(`{"content":"ok","file_path":"a.txt"}`))
}

func TestRecoverLeakedToolCallsRepairsTruncatedJSONDeterministically(t *testing.T) {
	input := `{"name":"Read","arguments":{"file_path":"notes.txt"`
	first, firstCleaned := recoverLeakedToolCalls(input)
	second, secondCleaned := recoverLeakedToolCalls(input)
	if firstCleaned != "" || secondCleaned != "" || len(first) != 1 || len(second) != 1 {
		t.Fatalf("first=%#v/%q second=%#v/%q", first, firstCleaned, second, secondCleaned)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("repair is not deterministic: %#v != %#v", first, second)
	}
	assertJSONEqual(t, first[0].Input, json.RawMessage(`{"file_path":"notes.txt"}`))
}

func TestRecoverLeakedToolCallsRejectsUnsafeJSONCandidates(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{name: "embedded object in prose", text: `example {"name":"Read","arguments":{"file_path":"a"}} only`},
		{name: "unsupported fence", text: "```javascript\n{\"name\":\"Read\",\"arguments\":{}}\n```"},
		{name: "trailing prose", text: `{"name":"Read","arguments":{"file_path":"a"} trailing`},
		{name: "mismatched delimiter", text: `{"name":"Read","arguments":[}`},
		{name: "dangling escape", text: `{"name":"Read","arguments":{"file_path":"a\`},
		{name: "conflicting names", text: `{"name":"Read","tool":"Write","arguments":{}}`},
		{name: "non-object new arguments", text: `{"name":"Read","arguments":"a"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls, cleaned := recoverLeakedToolCalls(test.text)
			if len(calls) != 0 {
				t.Fatalf("calls = %#v", calls)
			}
			if cleaned != test.text {
				t.Fatalf("cleaned = %q, want original %q", cleaned, test.text)
			}
		})
	}
}

func TestRecoverLeakedToolCallsRequiresExplicitAliasAndCatalogMembership(t *testing.T) {
	input := `{"name":"read_file","arguments":{"file_path":"a.txt"}}`
	allowed := map[string]struct{}{"Read": {}}

	calls, cleaned := recoverLeakedToolCallsWithOptions(input, ToolRecoveryOptions{AllowedTools: allowed})
	if len(calls) != 0 || cleaned != input {
		t.Fatalf("implicit alias accepted: calls=%#v cleaned=%q", calls, cleaned)
	}

	calls, cleaned = recoverLeakedToolCallsWithOptions(input, ToolRecoveryOptions{
		Aliases:      map[string]string{"read_file": "Read"},
		AllowedTools: allowed,
	})
	if len(calls) != 1 || calls[0].Name != "Read" || cleaned != "" {
		t.Fatalf("explicit alias rejected: calls=%#v cleaned=%q", calls, cleaned)
	}

	whitespaceName := `{"name":" Read ","arguments":{}}`
	calls, cleaned = recoverLeakedToolCallsWithOptions(whitespaceName, ToolRecoveryOptions{AllowedTools: allowed})
	if len(calls) != 0 || cleaned != whitespaceName {
		t.Fatalf("tool name was implicitly normalized: calls=%#v cleaned=%q", calls, cleaned)
	}
}

func TestRepairTruncatedJSONObjectHonorsBudget(t *testing.T) {
	input := []byte(`{"name":"Read","arguments":{"nested":{"file_path":"a"`)
	if repaired, ok := repairTruncatedJSONObject(input, 2); ok || repaired != nil {
		t.Fatalf("repair exceeded budget: %s", repaired)
	}
	if repaired, ok := repairTruncatedJSONObject(input, 4); !ok || !json.Valid(repaired) {
		t.Fatalf("bounded repair failed: %s", repaired)
	}
}

func assertJSONEqual(t *testing.T, got, want json.RawMessage) {
	t.Helper()
	var gotValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("invalid got JSON %q: %v", got, err)
	}
	var wantValue any
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("invalid want JSON %q: %v", want, err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("JSON mismatch: got %s want %s", got, want)
	}
}
