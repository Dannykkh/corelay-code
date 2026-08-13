package agent

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// Clean-room provenance (observed 2026-08-12, commit pinned for reproducibility):
//   - Hermes/fenced/bare normalization behavior:
//     https://github.com/Doorman11991/smallcode/blob/624970cce81657fd2751ee7f11ba85f35f2bb5c3/src/tools/tool_call_extractor.js
//   - Liquid marker and Python-literal behavior:
//     https://github.com/Doorman11991/smallcode/blob/624970cce81657fd2751ee7f11ba85f35f2bb5c3/src/tools/liquid_tool_parser.js
//   - Release-level format descriptions (1.5.1 and 1.1.0):
//     https://github.com/Doorman11991/smallcode/blob/624970cce81657fd2751ee7f11ba85f35f2bb5c3/CHANGELOG.md
//   - Continue tool codeblock grammar and streaming parser:
//     https://github.com/continuedev/continue/blob/5522c6f44ca0ac3528b37244818fbfa39b5af470/core/tools/systemMessageTools/toolCodeblocks/index.ts
//     https://github.com/continuedev/continue/blob/5522c6f44ca0ac3528b37244818fbfa39b5af470/core/tools/systemMessageTools/toolCodeblocks/parseSystemToolCall.ts
//   - goose direct tokenized Tool Shim parser:
//     https://github.com/aaif-goose/goose/blob/849b6f2ae84c2f8c0a8d90df3b29fafb1728d759/crates/goose/src/providers/toolshim.rs
//
// These cases independently specify observable input/output contracts. They do
// not copy upstream implementation or fixtures.

func TestDecodeToolCallCascadePositiveCorpus(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		native     []toolUseBlock
		configure  func(*ToolRecoveryOptions)
		wantFormat ToolCallFormat
		wantNames  []string
	}{
		{
			name: "provider native is authoritative",
			text: `<tool_call>{"name":"Write","arguments":{"file_path":"ignored"}}</tool_call>`,
			native: []toolUseBlock{{
				ID: "native-1", Name: "Read", Input: json.RawMessage(`{"file_path":"native.txt"}`),
			}},
			wantFormat: ToolCallFormatNative,
			wantNames:  []string{"Read"},
		},
		{
			name: "declared profile precedes builtins",
			text: `profile-call`,
			configure: func(options *ToolRecoveryOptions) {
				options.DeclaredParser = func(text string) ToolParseResult {
					if text != "profile-call" {
						return ToolParseResult{Status: ToolParseNotApplicable}
					}
					return ToolParseResult{
						Status: ToolParseParsed,
						Calls: []toolUseBlock{{
							Name: "Read", Input: json.RawMessage(`{"file_path":"declared.txt"}`),
						}},
						Ranges: []ToolTextRange{{Start: 0, End: len(text)}},
					}
				}
			},
			wantFormat: ToolCallFormatDeclared,
			wantNames:  []string{"Read"},
		},
		{
			name:       "hermes envelope",
			text:       `before <tool_call>{"name":"Read","arguments":{"file_path":"notes.txt"}}</tool_call> after`,
			wantFormat: ToolCallFormatHermes,
			wantNames:  []string{"Read"},
		},
		{
			name:       "hermes OpenAI function envelope",
			text:       `<tool_call>{"function":{"name":"Read","arguments":"{\"file_path\":\"openai.txt\"}"}}</tool_call>`,
			wantFormat: ToolCallFormatHermes,
			wantNames:  []string{"Read"},
		},
		{
			name: "liquid literals and multiple calls",
			text: `<|tool_call_start|>[read_file(path='a\n.txt', options={'lines':[1, 2], 'ok':True, 'none':None}), ` +
				`bash(command="go test", timeout=1.5e1)]<|tool_call_end|>`,
			wantFormat: ToolCallFormatLiquid,
			wantNames:  []string{"Read", "Bash"},
		},
		{
			name:       "Continue tool codeblock with typed and multiline arguments",
			text:       "I will inspect it now.\n```tool\nTOOL_NAME: Read\nBEGIN_ARG: file_path\n\"notes.txt\"\nEND_ARG\nBEGIN_ARG: options\n{\"line_start\":1,\"note\":\"first\\nsecond\"}\nEND_ARG\n```",
			wantFormat: ToolCallFormatCodeblock,
			wantNames:  []string{"Read"},
		},
		{
			name:       "goose tokenized tool call",
			text:       `thinking first <|tool_calls_section_begin|><|tool_call_begin|>functions.Read:0<|tool_call_argument_begin|>{"file_path":"notes.txt"}<|tool_call_argument_end|><|tool_call_end|><|tool_calls_section_end|>`,
			wantFormat: ToolCallFormatTokenized,
			wantNames:  []string{"Read"},
		},
		{
			name:       "fenced JSON array",
			text:       "```tool_call\n[{\"tool\":\"read_file\",\"args\":{\"file_path\":\"a.txt\"}},{\"name\":\"Bash\",\"arguments\":{\"command\":\"pwd\"}}]\n```",
			wantFormat: ToolCallFormatFencedJSON,
			wantNames:  []string{"Read", "Bash"},
		},
		{
			name:       "tool_call JSON fence is not a codeblock prefix collision",
			text:       "```tool_call\n{\"name\":\"Read\",\"arguments\":{\"file_path\":\"a.txt\"}}\n```",
			wantFormat: ToolCallFormatFencedJSON,
			wantNames:  []string{"Read"},
		},
		{
			name:       "bare JSON object",
			text:       ` {"tool_name":"Write","parameters":{"file_path":"out.txt","content":"ok"}} `,
			wantFormat: ToolCallFormatBareJSON,
			wantNames:  []string{"Write"},
		},
		{
			name:       "bounded suffix repair",
			text:       `{"name":"Read","arguments":{"file_path":"truncated.txt"`,
			wantFormat: ToolCallFormatSuffixRepair,
			wantNames:  []string{"Read"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := corpusToolRecoveryOptions()
			if test.configure != nil {
				test.configure(&options)
			}
			result := decodeToolCallCascade(test.text, test.native, options)
			if result.Status != ToolParseParsed || result.Format != test.wantFormat {
				t.Fatalf("result status/format = %s/%s, reason=%s trace=%#v", result.Status, result.Format, result.Reason, result.Trace)
			}
			if got := toolCallNames(result.Calls); !reflect.DeepEqual(got, test.wantNames) {
				t.Fatalf("names = %#v, want %#v", got, test.wantNames)
			}
			for index, call := range result.Calls {
				if call.ID == "" || !json.Valid(call.Input) || call.InputRaw != string(call.Input) {
					t.Fatalf("call %d not normalized: %#v", index, call)
				}
			}
		})
	}
}

func TestDecodeToolCallCascadeMalformedCorpus(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		wantStatus ToolParseStatus
	}{
		{name: "plain prose", text: `Use {"name":"Read"} as an example.`, wantStatus: ToolParseNotApplicable},
		{name: "orphan Hermes opening", text: `<tool_call>{"name":"Read"}`, wantStatus: ToolParseMalformed},
		{name: "orphan Hermes closing", text: `{"name":"Read"}</tool_call>`, wantStatus: ToolParseMalformed},
		{name: "Liquid missing value", text: `<|tool_call_start|>[read_file(path=)]<|tool_call_end|>`, wantStatus: ToolParseRejected},
		{name: "Liquid missing end", text: `<|tool_call_start|>[read_file(path='a')]`, wantStatus: ToolParseMalformed},
		{name: "tool codeblock missing END_ARG", text: "```tool\nTOOL_NAME: Read\nBEGIN_ARG: file_path\na.txt\n```", wantStatus: ToolParseMalformed},
		{name: "tool codeblock must be final", text: "```tool\nTOOL_NAME: Read\n```\ntrailing prose", wantStatus: ToolParseMalformed},
		{name: "tokenized orphan call", text: `<|tool_call_begin|>functions.Read:0<|tool_call_argument_begin|>{}<|tool_call_end|>`, wantStatus: ToolParseMalformed},
		{name: "tokenized missing section end", text: `<|tool_calls_section_begin|><|tool_call_begin|>functions.Read:0<|tool_call_argument_begin|>{}<|tool_call_end|>`, wantStatus: ToolParseMalformed},
		{name: "fence invalid JSON", text: "```json\n{not-json}\n```", wantStatus: ToolParseMalformed},
		{name: "bare invalid middle", text: `{"name":,"arguments":{}}`, wantStatus: ToolParseMalformed},
		{name: "dangling escape is not guessed", text: `{"name":"Read","arguments":{"file_path":"a\`, wantStatus: ToolParseMalformed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := decodeToolCallCascade(test.text, nil, corpusToolRecoveryOptions())
			if result.Status != test.wantStatus || len(result.Calls) != 0 {
				t.Fatalf("status=%s format=%s reason=%s calls=%#v", result.Status, result.Format, result.Reason, result.Calls)
			}
		})
	}
}

func TestDecodeToolCallCascadeRejectsAdversarialCorpus(t *testing.T) {
	deep := `{"name":"Read","arguments":{"a":{"b":{"c":1}}}}`
	tests := []struct {
		name      string
		text      string
		configure func(*ToolRecoveryOptions)
		want      ToolParseReason
	}{
		{name: "unknown tool", text: `{"name":"DeleteEverything","arguments":{}}`, want: ToolParseReasonUnknownTool},
		{name: "case mismatch", text: `{"name":"read","arguments":{}}`, want: ToolParseReasonUnknownTool},
		{name: "duplicate JSON name key", text: `{"name":"Read","name":"Write","arguments":{}}`, want: ToolParseReasonDuplicateName},
		{name: "duplicate call IDs", text: `[{"id":"same","name":"Read","arguments":{}},{"id":"same","name":"Write","arguments":{}}]`, want: ToolParseReasonDuplicateID},
		{name: "duplicate resolved names", text: `[{"name":"Read","arguments":{"file_path":"a"}},{"name":"read_file","arguments":{"file_path":"b"}}]`, want: ToolParseReasonDuplicateName},
		{name: "non-object arguments", text: `{"name":"Read","arguments":["a"]}`, want: ToolParseReasonInvalidArguments},
		{
			name: "duplicate codeblock argument",
			text: "```tool\nTOOL_NAME: Read\nBEGIN_ARG: file_path\na.txt\nEND_ARG\nBEGIN_ARG: file_path\nb.txt\nEND_ARG\n```",
			want: ToolParseReasonInvalidArguments,
		},
		{
			name: "tokenized non-object arguments",
			text: `<|tool_calls_section_begin|><|tool_call_begin|>functions.Read:0<|tool_call_argument_begin|>["a"]<|tool_call_end|><|tool_calls_section_end|>`,
			want: ToolParseReasonInvalidArguments,
		},
		{
			name: "schema callback",
			text: `{"name":"Read","arguments":{"wrong":true}}`,
			configure: func(options *ToolRecoveryOptions) {
				options.ValidateObject = func(_ string, _ json.RawMessage) error { return errors.New("sensitive callback detail") }
			},
			want: ToolParseReasonSchema,
		},
		{
			name: "schema callback panic",
			text: `{"name":"Read","arguments":{}}`,
			configure: func(options *ToolRecoveryOptions) {
				options.ValidateObject = func(_ string, _ json.RawMessage) error { panic("sensitive panic detail") }
			},
			want: ToolParseReasonSchema,
		},
		{
			name:      "depth limit",
			text:      deep,
			configure: func(options *ToolRecoveryOptions) { options.MaxJSONDepth = 3 },
			want:      ToolParseReasonDepthLimit,
		},
		{
			name:      "call limit",
			text:      `[{"name":"Read","arguments":{}},{"name":"Write","arguments":{}}]`,
			configure: func(options *ToolRecoveryOptions) { options.MaxCalls = 1 },
			want:      ToolParseReasonCallLimit,
		},
		{
			name:      "argument byte limit",
			text:      `{"name":"Write","arguments":{"content":"123456789"}}`,
			configure: func(options *ToolRecoveryOptions) { options.MaxArgumentBytes = 8 },
			want:      ToolParseReasonArgumentLimit,
		},
		{
			name:      "input byte limit",
			text:      `{"name":"Read","arguments":{}}`,
			configure: func(options *ToolRecoveryOptions) { options.MaxInputBytes = 8 },
			want:      ToolParseReasonInputLimit,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := corpusToolRecoveryOptions()
			if test.configure != nil {
				test.configure(&options)
			}
			result := decodeToolCallCascade(test.text, nil, options)
			if result.Status != ToolParseRejected || result.Reason != test.want || len(result.Calls) != 0 {
				t.Fatalf("status=%s format=%s reason=%s calls=%#v trace=%#v", result.Status, result.Format, result.Reason, result.Calls, result.Trace)
			}
		})
	}
}

func TestDecodeToolCallCascadeRejectsConflictingInterpretations(t *testing.T) {
	text := `<tool_call>{"name":"Read","arguments":{"file_path":"a"}}</tool_call>` + "\n" +
		"```json\n{\"name\":\"Write\",\"arguments\":{\"file_path\":\"b\"}}\n```"
	result := decodeToolCallCascade(text, nil, corpusToolRecoveryOptions())
	if result.Status != ToolParseRejected || result.Format != ToolCallFormatCascade || result.Reason != ToolParseReasonAmbiguous || len(result.Calls) != 0 {
		t.Fatalf("ambiguous result = %#v", result)
	}
}

func TestDecodeToolCallCascadeAllowsEquivalentInterpretationsOnce(t *testing.T) {
	text := `<tool_call>{"name":"Read","arguments":{"file_path":"same"}}</tool_call>` + "\n" +
		"```json\n{\"name\":\"Read\",\"arguments\":{\"file_path\":\"same\"}}\n```"
	result := decodeToolCallCascade(text, nil, corpusToolRecoveryOptions())
	if result.Status != ToolParseParsed || result.Format != ToolCallFormatHermes || len(result.Calls) != 1 {
		t.Fatalf("equivalent result = %#v", result)
	}
}

func TestDecodeToolCallCascadeTelemetryDoesNotRetainRawText(t *testing.T) {
	const secret = "top-secret-parser-token"
	text := `{"name":"Write","arguments":{"content":"` + secret + `"}}`
	result := decodeToolCallCascade(text, nil, corpusToolRecoveryOptions())
	if result.Status != ToolParseParsed || result.Digest == "" {
		t.Fatalf("result = %#v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), text) {
		t.Fatalf("telemetry retained raw input: %s", encoded)
	}
	for _, trace := range result.Trace {
		if trace.Digest != result.Digest || trace.Format == "" {
			t.Fatalf("unsafe or incomplete trace: %#v", trace)
		}
	}
}

func TestDecodeToolCallCascadeIsDeterministic(t *testing.T) {
	text := `<|tool_call_start|>[read_file(path='x', flags=[True, None, 2.5])]<|tool_call_end|>`
	first := decodeToolCallCascade(text, nil, corpusToolRecoveryOptions())
	second := decodeToolCallCascade(text, nil, corpusToolRecoveryOptions())
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("results differ:\nfirst=%#v\nsecond=%#v", first, second)
	}
}

func FuzzDecodeToolCallCascade(f *testing.F) {
	seeds := []string{
		`plain response`,
		`<tool_call>{"name":"Read","arguments":{"file_path":"a"}}</tool_call>`,
		`<|tool_call_start|>[read_file(path='a')]<|tool_call_end|>`,
		"```tool\nTOOL_NAME: Read\nBEGIN_ARG: file_path\na.txt\nEND_ARG\n```",
		`<|tool_calls_section_begin|><|tool_call_begin|>functions.Read:0<|tool_call_argument_begin|>{"file_path":"a"}<|tool_call_end|><|tool_calls_section_end|>`,
		"```json\n{\"name\":\"Read\",\"arguments\":{}}\n```",
		`{"name":"Read","arguments":{"file_path":"truncated"`,
		`{"name":"Read","name":"Write","arguments":{}}`,
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		options := corpusToolRecoveryOptions()
		options.MaxInputBytes = 4096
		options.MaxArgumentBytes = 2048
		result := decodeToolCallCascade(input, nil, options)
		switch result.Status {
		case ToolParseParsed, ToolParseNotApplicable, ToolParseMalformed, ToolParseRejected:
		default:
			t.Fatalf("unknown status %q", result.Status)
		}
		if result.Status != ToolParseParsed && len(result.Calls) != 0 {
			t.Fatalf("non-parsed result retained calls: %#v", result)
		}
		if result.Status == ToolParseParsed {
			for _, call := range result.Calls {
				if call.Name == "" || call.ID == "" || !isJSONObject(call.Input) {
					t.Fatalf("invalid parsed call: %#v", call)
				}
			}
		}
		if _, err := json.Marshal(result); err != nil {
			t.Fatalf("diagnostics are not serializable: %v", err)
		}
	})
}

func corpusToolRecoveryOptions() ToolRecoveryOptions {
	return ToolRecoveryOptions{
		AllowedTools: map[string]struct{}{
			"Read":  {},
			"Write": {},
			"Bash":  {},
		},
		Aliases: map[string]string{
			"read_file":  "Read",
			"write_file": "Write",
			"bash":       "Bash",
		},
	}
}

func toolCallNames(calls []toolUseBlock) []string {
	names := make([]string, 0, len(calls))
	for _, call := range calls {
		names = append(names, call.Name)
	}
	return names
}
