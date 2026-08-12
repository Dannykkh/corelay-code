package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Dannykkh/corelay-code/internal/harness"
	"github.com/Dannykkh/corelay-code/internal/types"
)

const (
	defaultToolRepairBytes     = 64
	defaultToolParseInputBytes = 256 << 10
	defaultToolParseCalls      = 16
	defaultToolParseDepth      = 32
	defaultToolArgumentBytes   = 64 << 10

	hardToolParseInputBytes = 1 << 20
	hardToolParseCalls      = 64
	hardToolParseDepth      = 64
	hardToolArgumentBytes   = 256 << 10
	hardToolRepairBytes     = 256

	maxRecoveryReasoningBytes          = 64 << 10
	defaultToolResponseCorrectionLimit = 2
)

var (
	// <function=NAME> ... <parameter=KEY>VALUE</parameter> ... </function>
	leakFuncRe  = regexp.MustCompile(`(?s)<function=([A-Za-z_][A-Za-z0-9_]*)\s*>(.*?)</function>`)
	leakParamRe = regexp.MustCompile(`(?s)<parameter=([A-Za-z_][A-Za-z0-9_]*)\s*>(.*?)</parameter>`)
	// <tool_call>{"name":..,"arguments":{..}}</tool_call>
	leakJSONRe = regexp.MustCompile(`(?s)<tool_call>\s*(.*?)\s*</tool_call>`)
	// New recovery formats are deliberately limited to complete Markdown fences
	// and a whole-response JSON object. This avoids mining arbitrary prose for
	// executable-looking snippets.
	leakFenceRe = regexp.MustCompile("(?s)```([A-Za-z0-9_-]*)[ \\t]*\\r?\\n(.*?)\\r?\\n[ \\t]*```")
	toolNameRe  = regexp.MustCompile(`"(?:name|tool|tool_name)"\s*:`)
	hermesRe    = regexp.MustCompile(`(?is)<tool[_-]call>\s*(.*?)\s*</tool[_-]call>`)
	hermesTagRe = regexp.MustCompile(`(?i)</?tool[_-]call>`)
	liquidNumRe = regexp.MustCompile(`^[+-]?(?:\d+\.\d*|\.\d+|\d+)(?:[eE][+-]?\d+)?`)
)

// ToolParseStatus is the format-local result of a pure parser. Malformed
// means that a format marker was present but its payload could not be decoded;
// Rejected means that a payload decoded but failed a safety contract.
type ToolParseStatus string

const (
	ToolParseParsed        ToolParseStatus = "parsed"
	ToolParseNotApplicable ToolParseStatus = "not_applicable"
	ToolParseMalformed     ToolParseStatus = "malformed"
	ToolParseRejected      ToolParseStatus = "rejected"
)

// ToolCallFormat identifies the decoder path without retaining model text.
type ToolCallFormat string

const (
	ToolCallFormatNative       ToolCallFormat = "provider_native"
	ToolCallFormatDeclared     ToolCallFormat = "declared_profile"
	ToolCallFormatHermes       ToolCallFormat = "hermes"
	ToolCallFormatLiquid       ToolCallFormat = "liquid"
	ToolCallFormatFencedJSON   ToolCallFormat = "fenced_json"
	ToolCallFormatBareJSON     ToolCallFormat = "bare_json"
	ToolCallFormatSuffixRepair ToolCallFormat = "suffix_repair"
	ToolCallFormatLegacy       ToolCallFormat = "legacy_text_recovery"
	ToolCallFormatCascade      ToolCallFormat = "cascade"
)

// ToolParseReason is deliberately a closed, non-sensitive code. Parser and
// schema error strings are never copied into results or traces.
type ToolParseReason string

const (
	ToolParseReasonNone              ToolParseReason = ""
	ToolParseReasonInputLimit        ToolParseReason = "input_limit"
	ToolParseReasonCallLimit         ToolParseReason = "call_limit"
	ToolParseReasonDepthLimit        ToolParseReason = "depth_limit"
	ToolParseReasonArgumentLimit     ToolParseReason = "argument_limit"
	ToolParseReasonMalformedEnvelope ToolParseReason = "malformed_envelope"
	ToolParseReasonInvalidJSON       ToolParseReason = "invalid_json"
	ToolParseReasonInvalidArguments  ToolParseReason = "invalid_arguments"
	ToolParseReasonCatalog           ToolParseReason = "catalog_required"
	ToolParseReasonUnknownTool       ToolParseReason = "unknown_tool"
	ToolParseReasonSchema            ToolParseReason = "schema_rejected"
	ToolParseReasonDuplicateID       ToolParseReason = "duplicate_id"
	ToolParseReasonDuplicateName     ToolParseReason = "duplicate_name"
	ToolParseReasonAmbiguous         ToolParseReason = "ambiguous_tool_call"
	ToolParseReasonRepairBudget      ToolParseReason = "repair_budget"
	ToolParseReasonDeclaredContract  ToolParseReason = "declared_contract"
	ToolParseReasonUnsupportedPolicy ToolParseReason = "unsupported_response_policy"
)

// ToolTextRange is a half-open byte range occupied by an exact parsed
// envelope. It is transient decoder state and never contains source text.
type ToolTextRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// ToolParseTrace is safe to persist: it contains only a digest, format, typed
// status, and a bounded reason code. It never contains the source text, tool
// arguments, callback errors, or credentials.
type ToolParseTrace struct {
	Digest string          `json:"digest"`
	Format ToolCallFormat  `json:"format"`
	Status ToolParseStatus `json:"status"`
	Reason ToolParseReason `json:"reason,omitempty"`
}

// ToolParseResult contains transient execution candidates plus persist-safe
// diagnostics. Calls are excluded from JSON serialization so callers cannot
// accidentally persist tool arguments as parser telemetry.
type ToolParseResult struct {
	Status ToolParseStatus  `json:"status"`
	Format ToolCallFormat   `json:"format"`
	Digest string           `json:"digest"`
	Reason ToolParseReason  `json:"reason,omitempty"`
	Calls  []toolUseBlock   `json:"-"`
	Ranges []ToolTextRange  `json:"-"`
	Trace  []ToolParseTrace `json:"trace,omitempty"`
}

// ToolArgumentValidator validates a canonical JSON object against the selected
// tool schema. Its error is intentionally reduced to ToolParseReasonSchema.
type ToolArgumentValidator func(toolName string, input json.RawMessage) error

// DeclaredToolTextParser is the run profile's optional text parser. The
// cascade discards its diagnostic fields and independently revalidates Calls.
type DeclaredToolTextParser func(text string) ToolParseResult

// ToolRecoveryOptions controls the conservative extensions to legacy leaked
// tool-call recovery. Aliases are exact and explicit: no fuzzy or case-folded
// tool-name guessing is performed. When AllowedTools is non-empty, recovered
// names must be members after alias resolution.
type ToolRecoveryOptions struct {
	Aliases          map[string]string
	AllowedTools     map[string]struct{}
	MaxRepairBytes   int
	MaxInputBytes    int
	MaxCalls         int
	MaxJSONDepth     int
	MaxArgumentBytes int
	ValidateObject   ToolArgumentValidator
	DeclaredParser   DeclaredToolTextParser
}

type recoveredToolEnvelope struct {
	Name       string          `json:"name"`
	Tool       string          `json:"tool"`
	ToolName   string          `json:"tool_name"`
	Arguments  json.RawMessage `json:"arguments"`
	Args       json.RawMessage `json:"args"`
	Parameters json.RawMessage `json:"parameters"`
}

type toolParseLimits struct {
	inputBytes    int
	calls         int
	depth         int
	argumentBytes int
	repairBytes   int
}

type strictJSONError struct {
	reason ToolParseReason
}

func (e *strictJSONError) Error() string { return string(e.reason) }

type boundedReasoningBuffer struct {
	text string
}

// Append retains only a UTF-8-safe suffix because leaked calls are emitted at
// the end of reasoning streams. The buffer is never persisted.
func (b *boundedReasoningBuffer) Append(fragment string) {
	if fragment == "" {
		return
	}
	if len(fragment) >= maxRecoveryReasoningBytes {
		b.text = validUTF8Suffix(fragment, maxRecoveryReasoningBytes)
		return
	}
	combined := b.text + fragment
	b.text = validUTF8Suffix(combined, maxRecoveryReasoningBytes)
}

func (b *boundedReasoningBuffer) String() string { return b.text }

func validUTF8Suffix(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	value = value[len(value)-maximum:]
	for value != "" && !utf8.ValidString(value) {
		value = value[1:]
	}
	return value
}

type toolResponseDecision struct {
	Calls           []toolUseBlock
	VisibleText     string
	Parse           ToolParseResult
	NeedsCorrection bool
}

// ToolResponseFailure is safe to surface and persist. It contains typed parser
// metadata only; model text, reasoning, arguments, and callback errors are
// intentionally absent.
type ToolResponseFailure struct {
	Code     string          `json:"code"`
	Digest   string          `json:"digest"`
	Format   ToolCallFormat  `json:"format"`
	Status   ToolParseStatus `json:"status"`
	Reason   ToolParseReason `json:"reason"`
	Attempts int             `json:"attempts"`
}

func (f *ToolResponseFailure) Error() string {
	if f == nil {
		return ""
	}
	return fmt.Sprintf("%s: format=%s status=%s reason=%s digest=%s attempts=%d",
		f.Code, f.Format, f.Status, f.Reason, f.Digest, f.Attempts)
}

type toolResponseCorrectionState struct {
	attempts int
	limit    int
}

func newToolResponseCorrectionState() toolResponseCorrectionState {
	return toolResponseCorrectionState{limit: defaultToolResponseCorrectionLimit}
}

func (s *toolResponseCorrectionState) next(result ToolParseResult) (string, *ToolResponseFailure) {
	if result.Status != ToolParseMalformed && result.Status != ToolParseRejected {
		return "", nil
	}
	if s.limit <= 0 {
		s.limit = defaultToolResponseCorrectionLimit
	}
	if s.attempts >= s.limit {
		return "", newToolResponseFailure(result, s.attempts)
	}
	s.attempts++
	return fmt.Sprintf(
		"[tool-response-correction] The previous response could not be used as a tool call. "+
			"format=%s status=%s reason=%s digest=%s correction=%d/%d. "+
			"Return either a valid provider-native tool call or one valid tool-call envelope using only the advertised catalog and schema. Do not repeat the invalid payload.",
		result.Format,
		result.Status,
		result.Reason,
		result.Digest,
		s.attempts,
		s.limit,
	), nil
}

func newToolResponseFailure(result ToolParseResult, attempts int) *ToolResponseFailure {
	code := "malformed_tool_call"
	if result.Reason == ToolParseReasonAmbiguous {
		code = "ambiguous_tool_call"
	} else if result.Reason == ToolParseReasonUnsupportedPolicy {
		code = "unsupported_response_policy"
	} else if result.Status == ToolParseRejected {
		code = "rejected_tool_call"
	}
	return &ToolResponseFailure{
		Code:     code,
		Digest:   result.Digest,
		Format:   result.Format,
		Status:   result.Status,
		Reason:   result.Reason,
		Attempts: attempts,
	}
}

func applyToolResponsePolicy(
	policy harness.ResponsePolicy,
	visibleText string,
	reasoningText string,
	nativeCalls []toolUseBlock,
	tools []types.ToolDef,
) toolResponseDecision {
	digest := digestToolText(visibleText)
	if len(nativeCalls) > 0 {
		result := toolParseResult(ToolCallFormatNative, ToolParseParsed, ToolParseReasonNone, digest, nativeCalls, nil)
		result.Trace = []ToolParseTrace{traceForToolResult(result)}
		return toolResponseDecision{Calls: nativeCalls, VisibleText: visibleText, Parse: result}
	}

	switch policy {
	case harness.ResponseNative:
		result := toolParseResult(ToolCallFormatNative, ToolParseNotApplicable, ToolParseReasonNone, digest, nil, nil)
		result.Trace = []ToolParseTrace{traceForToolResult(result)}
		return toolResponseDecision{VisibleText: visibleText, Parse: result}

	case harness.ResponseNativeWithTextRecovery:
		limits := normalizedToolParseLimits(ToolRecoveryOptions{})
		if len(visibleText) > limits.inputBytes {
			result := toolParseResult(ToolCallFormatLegacy, ToolParseRejected, ToolParseReasonInputLimit, digest, nil, nil)
			result.Trace = []ToolParseTrace{traceForToolResult(result)}
			return toolResponseDecision{VisibleText: visibleText, Parse: result}
		}
		recovered, cleaned := recoverToolCallsForCatalog(visibleText, tools)
		if len(recovered) == 0 {
			result := toolParseResult(ToolCallFormatLegacy, ToolParseNotApplicable, ToolParseReasonNone, digest, nil, nil)
			result.Trace = []ToolParseTrace{traceForToolResult(result)}
			return toolResponseDecision{VisibleText: visibleText, Parse: result}
		}
		if !legacyRecoveredCallsWithinBounds(recovered, limits) {
			result := toolParseResult(ToolCallFormatLegacy, ToolParseRejected, ToolParseReasonArgumentLimit, digest, nil, nil)
			result.Trace = []ToolParseTrace{traceForToolResult(result)}
			return toolResponseDecision{VisibleText: visibleText, Parse: result}
		}
		result := toolParseResult(ToolCallFormatLegacy, ToolParseParsed, ToolParseReasonNone, digest, recovered, nil)
		result.Trace = []ToolParseTrace{traceForToolResult(result)}
		return toolResponseDecision{Calls: recovered, VisibleText: cleaned, Parse: result}

	case harness.ResponseMultiFormat:
		source := visibleText
		usingReasoning := false
		if strings.TrimSpace(source) == "" && strings.TrimSpace(reasoningText) != "" {
			source = reasoningText
			usingReasoning = true
		}
		if source == "" {
			result := toolParseResult(ToolCallFormatCascade, ToolParseNotApplicable, ToolParseReasonNone, digest, nil, nil)
			result.Trace = []ToolParseTrace{traceForToolResult(result)}
			return toolResponseDecision{VisibleText: visibleText, Parse: result}
		}
		options := multiFormatToolRecoveryOptions(tools)
		result := decodeToolCallCascade(source, nil, options)
		decision := toolResponseDecision{VisibleText: visibleText, Parse: result}
		if result.Status == ToolParseParsed {
			decision.Calls = result.Calls
			if usingReasoning {
				decision.VisibleText = ""
			} else if cleaned, ok := removeExactToolTextRanges(visibleText, result.Ranges); ok {
				decision.VisibleText = cleaned
			} else {
				result.Status = ToolParseRejected
				result.Reason = ToolParseReasonDeclaredContract
				result.Calls = nil
				result.Ranges = nil
				decision.Calls = nil
				decision.Parse = result
				decision.NeedsCorrection = true
			}
		} else if result.Status == ToolParseMalformed || result.Status == ToolParseRejected {
			decision.NeedsCorrection = true
		}
		return decision

	default:
		result := toolParseResult(ToolCallFormatCascade, ToolParseRejected, ToolParseReasonUnsupportedPolicy, digest, nil, nil)
		result.Trace = []ToolParseTrace{traceForToolResult(result)}
		return toolResponseDecision{VisibleText: visibleText, Parse: result, NeedsCorrection: true}
	}
}

func multiFormatToolRecoveryOptions(tools []types.ToolDef) ToolRecoveryOptions {
	options := toolRecoveryOptionsForCatalog(tools)
	allowed := options.AllowedTools
	options.ValidateObject = func(name string, input json.RawMessage) error {
		schema, err := schemaForAllowedTool(allowed, name)
		if err != nil {
			return err
		}
		return validateToolInputSchema(input, schema)
	}
	return options
}

func legacyRecoveredCallsWithinBounds(calls []toolUseBlock, limits toolParseLimits) bool {
	if len(calls) == 0 || len(calls) > limits.calls {
		return false
	}
	total := 0
	for _, call := range calls {
		total += len(call.InputRaw)
		if total > limits.argumentBytes {
			return false
		}
	}
	return true
}

func removeExactToolTextRanges(text string, ranges []ToolTextRange) (string, bool) {
	if !validToolTextRanges(text, ranges) {
		return text, false
	}
	var cleaned strings.Builder
	previousEnd := 0
	for _, span := range ranges {
		cleaned.WriteString(text[previousEnd:span.Start])
		previousEnd = span.End
	}
	cleaned.WriteString(text[previousEnd:])
	return cleaned.String(), true
}

func recordToolParseResult(eventCh chan<- Event, recorder RunRecorder, spanID string, result ToolParseResult) {
	data := map[string]string{
		"digest": string(result.Digest),
		"format": string(result.Format),
		"status": string(result.Status),
		"reason": string(result.Reason),
	}
	if eventCh != nil {
		eventCh <- Event{Type: "tool_parse", Data: data}
	}
	finish := startRunSpan(recorder, spanID, "response.tool_parse", data)
	status := "ok"
	if result.Status == ToolParseNotApplicable {
		status = "skipped"
	} else if result.Status == ToolParseMalformed || result.Status == ToolParseRejected {
		status = "error"
	}
	finish(status, data)
}

// decodeToolCallCascade is a deterministic, side-effect-free decoder. Native
// calls are authoritative when present. Text formats are evaluated in fixed
// priority order; a rejected interpretation or conflicting parsed
// interpretation yields no executable candidates.
func decodeToolCallCascade(text string, native []toolUseBlock, opts ToolRecoveryOptions) ToolParseResult {
	digest := digestToolText(text)
	limits := normalizedToolParseLimits(opts)
	if len(native) > 0 {
		result := toolParseResult(ToolCallFormatNative, ToolParseParsed, ToolParseReasonNone, digest, native, nil)
		result = validateToolParseResult(result, opts, limits)
		result.Trace = []ToolParseTrace{traceForToolResult(result)}
		return result
	}
	if len(text) > limits.inputBytes {
		return toolParseResult(ToolCallFormatCascade, ToolParseRejected, ToolParseReasonInputLimit, digest, nil, nil)
	}
	if !utf8.ValidString(text) {
		return toolParseResult(ToolCallFormatCascade, ToolParseMalformed, ToolParseReasonMalformedEnvelope, digest, nil, nil)
	}

	parsers := []struct {
		format ToolCallFormat
		parse  func(string, ToolRecoveryOptions, toolParseLimits, string) ToolParseResult
	}{
		{ToolCallFormatDeclared, parseDeclaredToolText},
		{ToolCallFormatHermes, parseHermesToolText},
		{ToolCallFormatLiquid, parseLiquidToolText},
		{ToolCallFormatFencedJSON, parseFencedToolText},
		{ToolCallFormatBareJSON, parseBareToolText},
		{ToolCallFormatSuffixRepair, parseSuffixRepairToolText},
	}

	results := make([]ToolParseResult, 0, len(parsers))
	trace := make([]ToolParseTrace, 0, len(parsers))
	for _, parser := range parsers {
		result := parser.parse(text, opts, limits, digest)
		result.Format = parser.format
		result.Digest = digest
		result.Trace = nil
		if result.Status == ToolParseParsed && !validToolTextRanges(text, result.Ranges) {
			result.Status = ToolParseRejected
			result.Reason = ToolParseReasonDeclaredContract
			result.Calls = nil
			result.Ranges = nil
		}
		if result.Status == ToolParseParsed {
			result = validateToolParseResult(result, opts, limits)
		}
		results = append(results, result)
		trace = append(trace, traceForToolResult(result))
	}

	var parsed []ToolParseResult
	for _, result := range results {
		if result.Status == ToolParseParsed {
			parsed = append(parsed, result)
		}
	}
	if len(parsed) > 1 {
		first := canonicalToolCallInterpretation(parsed[0].Calls)
		for _, candidate := range parsed[1:] {
			if !bytes.Equal(first, canonicalToolCallInterpretation(candidate.Calls)) {
				return toolParseResult(ToolCallFormatCascade, ToolParseRejected, ToolParseReasonAmbiguous, digest, nil, trace)
			}
		}
	}

	// A decoded-but-rejected envelope is an explicit unsafe signal. Do not use
	// a lower-priority interpretation to bypass catalog or schema rejection.
	for _, result := range results {
		if result.Status == ToolParseRejected {
			result.Calls = nil
			result.Trace = trace
			return result
		}
	}
	if len(parsed) > 0 {
		parsed[0].Trace = trace
		return parsed[0]
	}
	for _, result := range results {
		if result.Status == ToolParseMalformed {
			result.Trace = trace
			return result
		}
	}
	return toolParseResult(ToolCallFormatCascade, ToolParseNotApplicable, ToolParseReasonNone, digest, nil, trace)
}

func normalizedToolParseLimits(opts ToolRecoveryOptions) toolParseLimits {
	return toolParseLimits{
		inputBytes:    boundedPositive(opts.MaxInputBytes, defaultToolParseInputBytes, hardToolParseInputBytes),
		calls:         boundedPositive(opts.MaxCalls, defaultToolParseCalls, hardToolParseCalls),
		depth:         boundedPositive(opts.MaxJSONDepth, defaultToolParseDepth, hardToolParseDepth),
		argumentBytes: boundedPositive(opts.MaxArgumentBytes, defaultToolArgumentBytes, hardToolArgumentBytes),
		repairBytes:   boundedPositive(opts.MaxRepairBytes, defaultToolRepairBytes, hardToolRepairBytes),
	}
}

func boundedPositive(value, fallback, maximum int) int {
	if value <= 0 {
		return fallback
	}
	if value > maximum {
		return maximum
	}
	return value
}

func digestToolText(text string) string {
	digest := sha256.Sum256([]byte(text))
	return fmt.Sprintf("sha256:%x", digest[:])
}

func toolParseResult(format ToolCallFormat, status ToolParseStatus, reason ToolParseReason, digest string, calls []toolUseBlock, trace []ToolParseTrace) ToolParseResult {
	return ToolParseResult{
		Status: status,
		Format: format,
		Digest: digest,
		Reason: reason,
		Calls:  calls,
		Trace:  trace,
	}
}

func validToolTextRanges(text string, ranges []ToolTextRange) bool {
	if len(ranges) == 0 {
		return false
	}
	previousEnd := -1
	for _, span := range ranges {
		if span.Start < 0 || span.End <= span.Start || span.End > len(text) || span.Start < previousEnd {
			return false
		}
		previousEnd = span.End
	}
	return true
}

func traceForToolResult(result ToolParseResult) ToolParseTrace {
	return ToolParseTrace{
		Digest: result.Digest,
		Format: result.Format,
		Status: result.Status,
		Reason: result.Reason,
	}
}

func validateToolParseResult(result ToolParseResult, opts ToolRecoveryOptions, limits toolParseLimits) ToolParseResult {
	if result.Status != ToolParseParsed || len(result.Calls) == 0 {
		result.Status = ToolParseMalformed
		result.Reason = ToolParseReasonMalformedEnvelope
		result.Calls = nil
		return result
	}
	if len(result.Calls) > limits.calls {
		result.Status = ToolParseRejected
		result.Reason = ToolParseReasonCallLimit
		result.Calls = nil
		return result
	}
	if len(opts.AllowedTools) == 0 {
		result.Status = ToolParseRejected
		result.Reason = ToolParseReasonCatalog
		result.Calls = nil
		return result
	}

	ids := make(map[string]struct{}, len(result.Calls))
	names := make(map[string]struct{}, len(result.Calls))
	normalized := make([]toolUseBlock, 0, len(result.Calls))
	totalArgumentBytes := 0
	for index, call := range result.Calls {
		resolved, ok := resolveRecoveredToolName(call.Name, opts)
		if !ok {
			result.Status = ToolParseRejected
			result.Reason = ToolParseReasonUnknownTool
			result.Calls = nil
			return result
		}
		if _, duplicate := names[resolved]; duplicate {
			result.Status = ToolParseRejected
			result.Reason = ToolParseReasonDuplicateName
			result.Calls = nil
			return result
		}
		names[resolved] = struct{}{}

		id := call.ID
		if id == "" {
			id = fmt.Sprintf("call_recovered_%d", index)
		}
		if _, duplicate := ids[id]; duplicate {
			result.Status = ToolParseRejected
			result.Reason = ToolParseReasonDuplicateID
			result.Calls = nil
			return result
		}
		ids[id] = struct{}{}

		raw := bytes.TrimSpace(call.Input)
		if len(raw) == 0 && strings.TrimSpace(call.InputRaw) != "" {
			raw = []byte(strings.TrimSpace(call.InputRaw))
		}
		if len(raw) == 0 {
			raw = []byte("{}")
		}
		value, err := decodeStrictJSON(raw, limits.depth)
		if err != nil {
			result.Status = ToolParseRejected
			result.Reason = strictJSONReason(err, ToolParseReasonInvalidArguments)
			result.Calls = nil
			return result
		}
		object, ok := value.(map[string]any)
		if !ok {
			result.Status = ToolParseRejected
			result.Reason = ToolParseReasonInvalidArguments
			result.Calls = nil
			return result
		}
		canonical, err := json.Marshal(object)
		if err != nil {
			result.Status = ToolParseRejected
			result.Reason = ToolParseReasonInvalidArguments
			result.Calls = nil
			return result
		}
		totalArgumentBytes += len(canonical)
		if totalArgumentBytes > limits.argumentBytes {
			result.Status = ToolParseRejected
			result.Reason = ToolParseReasonArgumentLimit
			result.Calls = nil
			return result
		}
		if opts.ValidateObject != nil && !toolArgumentValid(opts.ValidateObject, resolved, json.RawMessage(canonical)) {
			result.Status = ToolParseRejected
			result.Reason = ToolParseReasonSchema
			result.Calls = nil
			return result
		}
		normalized = append(normalized, toolUseBlock{
			ID:       id,
			Name:     resolved,
			Input:    json.RawMessage(canonical),
			InputRaw: string(canonical),
		})
	}
	result.Calls = normalized
	result.Reason = ToolParseReasonNone
	return result
}

func toolArgumentValid(validate ToolArgumentValidator, name string, input json.RawMessage) (valid bool) {
	defer func() {
		if recover() != nil {
			valid = false
		}
	}()
	return validate(name, input) == nil
}

func canonicalToolCallInterpretation(calls []toolUseBlock) []byte {
	type canonicalCall struct {
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	values := make([]canonicalCall, 0, len(calls))
	for _, call := range calls {
		values = append(values, canonicalCall{Name: call.Name, Input: call.Input})
	}
	encoded, _ := json.Marshal(values)
	return encoded
}

func strictJSONReason(err error, fallback ToolParseReason) ToolParseReason {
	var issue *strictJSONError
	if errors.As(err, &issue) {
		return issue.reason
	}
	return fallback
}

func decodeStrictJSON(input []byte, maxDepth int) (any, error) {
	if !utf8.Valid(input) {
		return nil, &strictJSONError{reason: ToolParseReasonInvalidJSON}
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.UseNumber()
	value, err := readStrictJSONValue(decoder, 0, maxDepth)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, &strictJSONError{reason: ToolParseReasonInvalidJSON}
	}
	return value, nil
}

func readStrictJSONValue(decoder *json.Decoder, depth, maxDepth int) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, &strictJSONError{reason: ToolParseReasonInvalidJSON}
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return token, nil
	}
	if depth >= maxDepth {
		return nil, &strictJSONError{reason: ToolParseReasonDepthLimit}
	}
	switch delimiter {
	case '{':
		object := make(map[string]any)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, &strictJSONError{reason: ToolParseReasonInvalidJSON}
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, &strictJSONError{reason: ToolParseReasonInvalidJSON}
			}
			if _, duplicate := object[key]; duplicate {
				if key == "id" || key == "call_id" || key == "tool_call_id" {
					return nil, &strictJSONError{reason: ToolParseReasonDuplicateID}
				}
				if key == "name" || key == "tool" || key == "tool_name" {
					return nil, &strictJSONError{reason: ToolParseReasonDuplicateName}
				}
				return nil, &strictJSONError{reason: ToolParseReasonInvalidJSON}
			}
			value, err := readStrictJSONValue(decoder, depth+1, maxDepth)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return nil, &strictJSONError{reason: ToolParseReasonInvalidJSON}
		}
		return object, nil
	case '[':
		array := make([]any, 0)
		for decoder.More() {
			value, err := readStrictJSONValue(decoder, depth+1, maxDepth)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return nil, &strictJSONError{reason: ToolParseReasonInvalidJSON}
		}
		return array, nil
	default:
		return nil, &strictJSONError{reason: ToolParseReasonInvalidJSON}
	}
}

// The format parsers are kept as small pure functions so their contracts can
// be corpus-tested independently of the agent loop.
func parseDeclaredToolText(text string, opts ToolRecoveryOptions, _ toolParseLimits, digest string) (result ToolParseResult) {
	if opts.DeclaredParser == nil {
		return toolParseResult(ToolCallFormatDeclared, ToolParseNotApplicable, ToolParseReasonNone, digest, nil, nil)
	}
	defer func() {
		if recover() != nil {
			result = toolParseResult(ToolCallFormatDeclared, ToolParseMalformed, ToolParseReasonDeclaredContract, digest, nil, nil)
		}
	}()
	result = opts.DeclaredParser(text)
	result.Format = ToolCallFormatDeclared
	result.Digest = digest
	result.Trace = nil
	if result.Status != ToolParseParsed && result.Status != ToolParseNotApplicable && result.Status != ToolParseMalformed && result.Status != ToolParseRejected {
		return toolParseResult(ToolCallFormatDeclared, ToolParseMalformed, ToolParseReasonDeclaredContract, digest, nil, nil)
	}
	if result.Status != ToolParseParsed {
		result.Calls = nil
		if result.Status == ToolParseNotApplicable {
			result.Reason = ToolParseReasonNone
		} else {
			result.Reason = ToolParseReasonDeclaredContract
		}
	} else {
		result.Reason = ToolParseReasonNone
	}
	return result
}

func parseHermesToolText(text string, _ ToolRecoveryOptions, limits toolParseLimits, digest string) ToolParseResult {
	matches := hermesRe.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		if hermesTagRe.MatchString(text) {
			return toolParseResult(ToolCallFormatHermes, ToolParseMalformed, ToolParseReasonMalformedEnvelope, digest, nil, nil)
		}
		return toolParseResult(ToolCallFormatHermes, ToolParseNotApplicable, ToolParseReasonNone, digest, nil, nil)
	}
	if hermesTagRe.MatchString(hermesRe.ReplaceAllString(text, "")) {
		return toolParseResult(ToolCallFormatHermes, ToolParseMalformed, ToolParseReasonMalformedEnvelope, digest, nil, nil)
	}

	calls := make([]toolUseBlock, 0, len(matches))
	ranges := make([]ToolTextRange, 0, len(matches))
	for _, match := range matches {
		payload := text[match[2]:match[3]]
		parsed := parseJSONToolPayload(payload, limits, digest, ToolCallFormatHermes)
		if parsed.Status != ToolParseParsed {
			return parsed
		}
		calls = append(calls, parsed.Calls...)
		ranges = append(ranges, ToolTextRange{Start: match[0], End: match[1]})
		if len(calls) > limits.calls {
			return toolParseResult(ToolCallFormatHermes, ToolParseRejected, ToolParseReasonCallLimit, digest, nil, nil)
		}
	}
	result := toolParseResult(ToolCallFormatHermes, ToolParseParsed, ToolParseReasonNone, digest, calls, nil)
	result.Ranges = ranges
	return result
}

func parseLiquidToolText(text string, _ ToolRecoveryOptions, limits toolParseLimits, digest string) ToolParseResult {
	const (
		startMarker = "<|tool_call_start|>"
		endMarker   = "<|tool_call_end|>"
	)
	if !strings.Contains(text, startMarker) && !strings.Contains(text, endMarker) {
		return toolParseResult(ToolCallFormatLiquid, ToolParseNotApplicable, ToolParseReasonNone, digest, nil, nil)
	}

	calls := make([]toolUseBlock, 0)
	ranges := make([]ToolTextRange, 0)
	cursor := 0
	for cursor < len(text) {
		startRelative := strings.Index(text[cursor:], startMarker)
		endRelative := strings.Index(text[cursor:], endMarker)
		if endRelative >= 0 && (startRelative < 0 || endRelative < startRelative) {
			return toolParseResult(ToolCallFormatLiquid, ToolParseMalformed, ToolParseReasonMalformedEnvelope, digest, nil, nil)
		}
		if startRelative < 0 {
			break
		}
		start := cursor + startRelative
		payloadStart := start + len(startMarker)
		endPayloadRelative := strings.Index(text[payloadStart:], endMarker)
		if endPayloadRelative < 0 {
			return toolParseResult(ToolCallFormatLiquid, ToolParseMalformed, ToolParseReasonMalformedEnvelope, digest, nil, nil)
		}
		end := payloadStart + endPayloadRelative
		payload := text[payloadStart:end]
		if strings.Contains(payload, startMarker) {
			return toolParseResult(ToolCallFormatLiquid, ToolParseMalformed, ToolParseReasonMalformedEnvelope, digest, nil, nil)
		}
		parser := liquidLiteralParser{source: payload, maxDepth: limits.depth, maxCalls: limits.calls}
		parsedCalls, err := parser.parseCalls()
		if err != nil {
			reason := strictJSONReason(err, ToolParseReasonMalformedEnvelope)
			status := ToolParseMalformed
			if reason == ToolParseReasonDepthLimit || reason == ToolParseReasonCallLimit || reason == ToolParseReasonInvalidArguments {
				status = ToolParseRejected
			}
			return toolParseResult(ToolCallFormatLiquid, status, reason, digest, nil, nil)
		}
		calls = append(calls, parsedCalls...)
		ranges = append(ranges, ToolTextRange{Start: start, End: end + len(endMarker)})
		if len(calls) > limits.calls {
			return toolParseResult(ToolCallFormatLiquid, ToolParseRejected, ToolParseReasonCallLimit, digest, nil, nil)
		}
		cursor = end + len(endMarker)
	}
	if cursor == 0 || strings.Contains(text[cursor:], endMarker) || len(calls) == 0 {
		return toolParseResult(ToolCallFormatLiquid, ToolParseMalformed, ToolParseReasonMalformedEnvelope, digest, nil, nil)
	}
	result := toolParseResult(ToolCallFormatLiquid, ToolParseParsed, ToolParseReasonNone, digest, calls, nil)
	result.Ranges = ranges
	return result
}

type liquidLiteralParser struct {
	source   string
	pos      int
	maxDepth int
	maxCalls int
}

func (p *liquidLiteralParser) parseCalls() ([]toolUseBlock, error) {
	p.skipSpace()
	bracketed := p.consume('[')
	p.skipSpace()
	if bracketed && p.consume(']') {
		p.skipSpace()
		if p.pos != len(p.source) {
			return nil, &strictJSONError{reason: ToolParseReasonMalformedEnvelope}
		}
		return nil, &strictJSONError{reason: ToolParseReasonMalformedEnvelope}
	}

	calls := make([]toolUseBlock, 0, 1)
	for {
		call, err := p.parseCall()
		if err != nil {
			return nil, err
		}
		calls = append(calls, call)
		if len(calls) > p.maxCalls {
			return nil, &strictJSONError{reason: ToolParseReasonCallLimit}
		}
		p.skipSpace()
		if bracketed && p.peek(']') {
			p.pos++
			break
		}
		if !p.consume(',') {
			if bracketed {
				return nil, &strictJSONError{reason: ToolParseReasonMalformedEnvelope}
			}
			break
		}
		p.skipSpace()
		if bracketed && p.consume(']') {
			break
		}
		if !bracketed && p.pos >= len(p.source) {
			return nil, &strictJSONError{reason: ToolParseReasonMalformedEnvelope}
		}
	}
	p.skipSpace()
	if p.pos != len(p.source) {
		return nil, &strictJSONError{reason: ToolParseReasonMalformedEnvelope}
	}
	return calls, nil
}

func (p *liquidLiteralParser) parseCall() (toolUseBlock, error) {
	p.skipSpace()
	name, ok := p.parseIdentifier()
	if !ok {
		return toolUseBlock{}, &strictJSONError{reason: ToolParseReasonMalformedEnvelope}
	}
	p.skipSpace()
	if !p.consume('(') {
		return toolUseBlock{}, &strictJSONError{reason: ToolParseReasonMalformedEnvelope}
	}
	arguments := make(map[string]any)
	p.skipSpace()
	if !p.consume(')') {
		for {
			p.skipSpace()
			key, ok := p.parseIdentifier()
			if !ok {
				return toolUseBlock{}, &strictJSONError{reason: ToolParseReasonInvalidArguments}
			}
			if _, duplicate := arguments[key]; duplicate {
				return toolUseBlock{}, &strictJSONError{reason: ToolParseReasonInvalidArguments}
			}
			p.skipSpace()
			if !p.consume('=') {
				return toolUseBlock{}, &strictJSONError{reason: ToolParseReasonInvalidArguments}
			}
			value, err := p.parseValue(1)
			if err != nil {
				return toolUseBlock{}, err
			}
			arguments[key] = value
			p.skipSpace()
			if p.consume(')') {
				break
			}
			if !p.consume(',') {
				return toolUseBlock{}, &strictJSONError{reason: ToolParseReasonInvalidArguments}
			}
			p.skipSpace()
			if p.consume(')') {
				break
			}
		}
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return toolUseBlock{}, &strictJSONError{reason: ToolParseReasonInvalidArguments}
	}
	return toolUseBlock{Name: name, Input: json.RawMessage(encoded), InputRaw: string(encoded)}, nil
}

func (p *liquidLiteralParser) parseValue(depth int) (any, error) {
	if depth > p.maxDepth {
		return nil, &strictJSONError{reason: ToolParseReasonDepthLimit}
	}
	p.skipSpace()
	if p.pos >= len(p.source) {
		return nil, &strictJSONError{reason: ToolParseReasonInvalidArguments}
	}
	switch p.source[p.pos] {
	case '\'', '"':
		return p.parseString()
	case '[':
		return p.parseList(depth)
	case '{':
		return p.parseDict(depth)
	}
	for literal, value := range map[string]any{"True": true, "False": false, "None": nil} {
		if p.consumeWord(literal) {
			return value, nil
		}
	}
	match := liquidNumRe.FindString(p.source[p.pos:])
	if match == "" {
		return nil, &strictJSONError{reason: ToolParseReasonInvalidArguments}
	}
	if _, err := strconv.ParseFloat(match, 64); err != nil {
		return nil, &strictJSONError{reason: ToolParseReasonInvalidArguments}
	}
	p.pos += len(match)
	return json.Number(match), nil
}

func (p *liquidLiteralParser) parseString() (string, error) {
	quote := p.source[p.pos]
	p.pos++
	var output strings.Builder
	for p.pos < len(p.source) {
		current := p.source[p.pos]
		p.pos++
		if current == quote {
			return output.String(), nil
		}
		if current < 0x20 {
			return "", &strictJSONError{reason: ToolParseReasonInvalidArguments}
		}
		if current != '\\' {
			output.WriteByte(current)
			continue
		}
		if p.pos >= len(p.source) {
			return "", &strictJSONError{reason: ToolParseReasonInvalidArguments}
		}
		escape := p.source[p.pos]
		p.pos++
		switch escape {
		case 'n':
			output.WriteByte('\n')
		case 't':
			output.WriteByte('\t')
		case 'r':
			output.WriteByte('\r')
		case 'b':
			output.WriteByte('\b')
		case 'f':
			output.WriteByte('\f')
		case '0':
			output.WriteByte(0)
		case '\\', '\'', '"':
			output.WriteByte(escape)
		case 'x':
			value, ok := p.parseHexEscape(2)
			if !ok {
				return "", &strictJSONError{reason: ToolParseReasonInvalidArguments}
			}
			output.WriteRune(rune(value))
		case 'u':
			value, ok := p.parseHexEscape(4)
			if !ok || value >= 0xd800 && value <= 0xdfff {
				return "", &strictJSONError{reason: ToolParseReasonInvalidArguments}
			}
			output.WriteRune(rune(value))
		default:
			return "", &strictJSONError{reason: ToolParseReasonInvalidArguments}
		}
	}
	return "", &strictJSONError{reason: ToolParseReasonInvalidArguments}
}

func (p *liquidLiteralParser) parseHexEscape(length int) (uint64, bool) {
	if p.pos+length > len(p.source) {
		return 0, false
	}
	value, err := strconv.ParseUint(p.source[p.pos:p.pos+length], 16, 32)
	if err != nil {
		return 0, false
	}
	p.pos += length
	return value, true
}

func (p *liquidLiteralParser) parseList(depth int) ([]any, error) {
	p.pos++
	values := make([]any, 0)
	p.skipSpace()
	if p.consume(']') {
		return values, nil
	}
	for {
		value, err := p.parseValue(depth + 1)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
		p.skipSpace()
		if p.consume(']') {
			return values, nil
		}
		if !p.consume(',') {
			return nil, &strictJSONError{reason: ToolParseReasonInvalidArguments}
		}
		p.skipSpace()
		if p.consume(']') {
			return values, nil
		}
	}
}

func (p *liquidLiteralParser) parseDict(depth int) (map[string]any, error) {
	p.pos++
	object := make(map[string]any)
	p.skipSpace()
	if p.consume('}') {
		return object, nil
	}
	for {
		p.skipSpace()
		if p.pos >= len(p.source) || (p.source[p.pos] != '\'' && p.source[p.pos] != '"') {
			return nil, &strictJSONError{reason: ToolParseReasonInvalidArguments}
		}
		key, err := p.parseString()
		if err != nil || key == "" {
			return nil, &strictJSONError{reason: ToolParseReasonInvalidArguments}
		}
		if _, duplicate := object[key]; duplicate {
			return nil, &strictJSONError{reason: ToolParseReasonInvalidArguments}
		}
		p.skipSpace()
		if !p.consume(':') {
			return nil, &strictJSONError{reason: ToolParseReasonInvalidArguments}
		}
		value, err := p.parseValue(depth + 1)
		if err != nil {
			return nil, err
		}
		object[key] = value
		p.skipSpace()
		if p.consume('}') {
			return object, nil
		}
		if !p.consume(',') {
			return nil, &strictJSONError{reason: ToolParseReasonInvalidArguments}
		}
		p.skipSpace()
		if p.consume('}') {
			return object, nil
		}
	}
}

func (p *liquidLiteralParser) parseIdentifier() (string, bool) {
	if p.pos >= len(p.source) || !isLiquidIdentStart(p.source[p.pos]) {
		return "", false
	}
	start := p.pos
	p.pos++
	for p.pos < len(p.source) && isLiquidIdentContinue(p.source[p.pos]) {
		p.pos++
	}
	return p.source[start:p.pos], true
}

func (p *liquidLiteralParser) consumeWord(word string) bool {
	if !strings.HasPrefix(p.source[p.pos:], word) {
		return false
	}
	end := p.pos + len(word)
	if end < len(p.source) && isLiquidIdentContinue(p.source[end]) {
		return false
	}
	p.pos = end
	return true
}

func (p *liquidLiteralParser) skipSpace() {
	for p.pos < len(p.source) {
		switch p.source[p.pos] {
		case ' ', '\t', '\r', '\n':
			p.pos++
		default:
			return
		}
	}
}

func (p *liquidLiteralParser) consume(want byte) bool {
	if p.pos >= len(p.source) || p.source[p.pos] != want {
		return false
	}
	p.pos++
	return true
}

func (p *liquidLiteralParser) peek(want byte) bool {
	return p.pos < len(p.source) && p.source[p.pos] == want
}

func isLiquidIdentStart(value byte) bool {
	return value == '_' || value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}

func isLiquidIdentContinue(value byte) bool {
	return isLiquidIdentStart(value) || value >= '0' && value <= '9'
}

func parseFencedToolText(text string, _ ToolRecoveryOptions, limits toolParseLimits, digest string) ToolParseResult {
	matches := leakFenceRe.FindAllStringSubmatchIndex(text, -1)
	calls := make([]toolUseBlock, 0)
	ranges := make([]ToolTextRange, 0)
	completeSupported := 0
	remaining := text
	for _, match := range matches {
		language := text[match[2]:match[3]]
		if !supportedToolFence(language) {
			continue
		}
		completeSupported++
		payload := text[match[4]:match[5]]
		parsed := parseJSONToolPayload(payload, limits, digest, ToolCallFormatFencedJSON)
		if parsed.Status != ToolParseParsed {
			return parsed
		}
		calls = append(calls, parsed.Calls...)
		ranges = append(ranges, ToolTextRange{Start: match[0], End: match[1]})
		if len(calls) > limits.calls {
			return toolParseResult(ToolCallFormatFencedJSON, ToolParseRejected, ToolParseReasonCallLimit, digest, nil, nil)
		}
	}
	if len(matches) > 0 {
		remaining = leakFenceRe.ReplaceAllString(text, "")
	}
	if hasSupportedFenceOpening(remaining) {
		return toolParseResult(ToolCallFormatFencedJSON, ToolParseMalformed, ToolParseReasonMalformedEnvelope, digest, nil, nil)
	}
	if completeSupported == 0 {
		return toolParseResult(ToolCallFormatFencedJSON, ToolParseNotApplicable, ToolParseReasonNone, digest, nil, nil)
	}
	result := toolParseResult(ToolCallFormatFencedJSON, ToolParseParsed, ToolParseReasonNone, digest, calls, nil)
	result.Ranges = ranges
	return result
}

func parseBareToolText(text string, _ ToolRecoveryOptions, limits toolParseLimits, digest string) ToolParseResult {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[') {
		return toolParseResult(ToolCallFormatBareJSON, ToolParseNotApplicable, ToolParseReasonNone, digest, nil, nil)
	}
	result := parseJSONToolPayload(trimmed, limits, digest, ToolCallFormatBareJSON)
	if result.Status == ToolParseParsed {
		result.Ranges = []ToolTextRange{trimmedToolTextRange(text)}
	}
	return result
}

func parseSuffixRepairToolText(text string, _ ToolRecoveryOptions, limits toolParseLimits, digest string) ToolParseResult {
	trimmed := []byte(strings.TrimSpace(text))
	if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return toolParseResult(ToolCallFormatSuffixRepair, ToolParseNotApplicable, ToolParseReasonNone, digest, nil, nil)
	}
	if _, err := decodeStrictJSON(trimmed, limits.depth); err == nil {
		return toolParseResult(ToolCallFormatSuffixRepair, ToolParseNotApplicable, ToolParseReasonNone, digest, nil, nil)
	}
	repaired, ok := repairTruncatedJSONValue(trimmed, limits.repairBytes, limits.depth)
	if !ok {
		return toolParseResult(ToolCallFormatSuffixRepair, ToolParseMalformed, ToolParseReasonRepairBudget, digest, nil, nil)
	}
	parsed := parseJSONToolPayload(string(repaired), limits, digest, ToolCallFormatSuffixRepair)
	if parsed.Status == ToolParseMalformed {
		parsed.Reason = ToolParseReasonRepairBudget
	}
	if parsed.Status == ToolParseParsed {
		parsed.Ranges = []ToolTextRange{trimmedToolTextRange(text)}
	}
	return parsed
}

func trimmedToolTextRange(text string) ToolTextRange {
	start := 0
	for start < len(text) {
		switch text[start] {
		case ' ', '\t', '\r', '\n':
			start++
		default:
			goto findEnd
		}
	}

findEnd:
	end := len(text)
	for end > start {
		switch text[end-1] {
		case ' ', '\t', '\r', '\n':
			end--
		default:
			return ToolTextRange{Start: start, End: end}
		}
	}
	return ToolTextRange{Start: start, End: end}
}

func hasSupportedFenceOpening(text string) bool {
	for {
		index := strings.Index(text, "```")
		if index < 0 {
			return false
		}
		tail := text[index+3:]
		lineEnd := strings.IndexAny(tail, "\r\n")
		if lineEnd < 0 {
			lineEnd = len(tail)
		}
		label := strings.TrimSpace(tail[:lineEnd])
		if supportedToolFence(label) {
			return true
		}
		text = tail[lineEnd:]
	}
}

func parseJSONToolPayload(payload string, limits toolParseLimits, digest string, format ToolCallFormat) ToolParseResult {
	value, err := decodeStrictJSON([]byte(strings.TrimSpace(payload)), limits.depth)
	if err != nil {
		reason := strictJSONReason(err, ToolParseReasonInvalidJSON)
		status := ToolParseMalformed
		if reason == ToolParseReasonDepthLimit || reason == ToolParseReasonDuplicateID || reason == ToolParseReasonDuplicateName {
			status = ToolParseRejected
		}
		return toolParseResult(format, status, reason, digest, nil, nil)
	}
	calls, reason := toolCallsFromJSONValue(value, limits)
	if reason != ToolParseReasonNone {
		return toolParseResult(format, ToolParseRejected, reason, digest, nil, nil)
	}
	if len(calls) == 0 {
		return toolParseResult(format, ToolParseMalformed, ToolParseReasonMalformedEnvelope, digest, nil, nil)
	}
	return toolParseResult(format, ToolParseParsed, ToolParseReasonNone, digest, calls, nil)
}

func toolCallsFromJSONValue(value any, limits toolParseLimits) ([]toolUseBlock, ToolParseReason) {
	items := []any{value}
	if array, ok := value.([]any); ok {
		items = array
	}
	if len(items) == 0 {
		return nil, ToolParseReasonMalformedEnvelope
	}
	if len(items) > limits.calls {
		return nil, ToolParseReasonCallLimit
	}
	calls := make([]toolUseBlock, 0, len(items))
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			return nil, ToolParseReasonMalformedEnvelope
		}
		call, reason := toolCallFromJSONObject(object, limits)
		if reason != ToolParseReasonNone {
			return nil, reason
		}
		calls = append(calls, call)
	}
	return calls, ToolParseReasonNone
}

func toolCallFromJSONObject(object map[string]any, limits toolParseLimits) (toolUseBlock, ToolParseReason) {
	var function map[string]any
	if rawFunction, exists := object["function"]; exists {
		var ok bool
		function, ok = rawFunction.(map[string]any)
		if !ok {
			return toolUseBlock{}, ToolParseReasonMalformedEnvelope
		}
	}

	nameValues := valuesForKeys(object, "name", "tool", "tool_name")
	if function != nil {
		nameValues = append(nameValues, valuesForKeys(function, "name")...)
	}
	name, reason := oneStrictString(nameValues, ToolParseReasonDuplicateName)
	if reason != ToolParseReasonNone || name == "" {
		if reason == ToolParseReasonNone {
			reason = ToolParseReasonMalformedEnvelope
		}
		return toolUseBlock{}, reason
	}

	idValues := valuesForKeys(object, "id", "call_id", "tool_call_id")
	id, reason := oneOptionalStrictString(idValues, ToolParseReasonDuplicateID)
	if reason != ToolParseReasonNone {
		return toolUseBlock{}, reason
	}

	argumentValues := valuesForKeys(object, "arguments", "args", "parameters")
	if function != nil {
		argumentValues = append(argumentValues, valuesForKeys(function, "arguments")...)
	}
	arguments, reason := oneJSONObject(argumentValues, limits.depth)
	if reason != ToolParseReasonNone {
		return toolUseBlock{}, reason
	}
	return toolUseBlock{ID: id, Name: name, Input: arguments, InputRaw: string(arguments)}, ToolParseReasonNone
}

func valuesForKeys(object map[string]any, keys ...string) []any {
	values := make([]any, 0, len(keys))
	for _, key := range keys {
		if value, exists := object[key]; exists {
			values = append(values, value)
		}
	}
	return values
}

func oneStrictString(values []any, conflictReason ToolParseReason) (string, ToolParseReason) {
	selected := ""
	for _, value := range values {
		text, ok := value.(string)
		if !ok || text == "" {
			return "", ToolParseReasonMalformedEnvelope
		}
		if selected != "" && selected != text {
			return "", conflictReason
		}
		selected = text
	}
	return selected, ToolParseReasonNone
}

func oneOptionalStrictString(values []any, conflictReason ToolParseReason) (string, ToolParseReason) {
	if len(values) == 0 {
		return "", ToolParseReasonNone
	}
	return oneStrictString(values, conflictReason)
}

func oneJSONObject(values []any, maxDepth int) (json.RawMessage, ToolParseReason) {
	if len(values) == 0 {
		return json.RawMessage("{}"), ToolParseReasonNone
	}
	var selected []byte
	for _, value := range values {
		if encoded, ok := value.(string); ok {
			decoded, err := decodeStrictJSON([]byte(encoded), maxDepth)
			if err != nil {
				return nil, strictJSONReason(err, ToolParseReasonInvalidArguments)
			}
			value = decoded
		}
		object, ok := value.(map[string]any)
		if !ok {
			return nil, ToolParseReasonInvalidArguments
		}
		canonical, err := json.Marshal(object)
		if err != nil {
			return nil, ToolParseReasonInvalidArguments
		}
		if selected != nil && !bytes.Equal(selected, canonical) {
			return nil, ToolParseReasonInvalidArguments
		}
		selected = canonical
	}
	return json.RawMessage(selected), ToolParseReasonNone
}

func repairTruncatedJSONValue(input []byte, maxAdded, maxDepth int) ([]byte, bool) {
	trimmed := bytes.TrimSpace(input)
	if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') || maxAdded <= 0 || maxDepth <= 0 {
		return nil, false
	}
	stack := make([]byte, 0, 8)
	inString := false
	escaped := false
	rootClosed := false
	for _, current := range trimmed {
		if rootClosed {
			if current != ' ' && current != '\t' && current != '\r' && current != '\n' {
				return nil, false
			}
			continue
		}
		if inString {
			if escaped {
				escaped = false
				continue
			}
			switch current {
			case '\\':
				escaped = true
			case '"':
				inString = false
			default:
				if current < 0x20 {
					return nil, false
				}
			}
			continue
		}
		switch current {
		case '"':
			inString = true
		case '{':
			stack = append(stack, '}')
		case '[':
			stack = append(stack, ']')
		case '}', ']':
			if len(stack) == 0 || stack[len(stack)-1] != current {
				return nil, false
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				rootClosed = true
			}
		}
		if len(stack) > maxDepth {
			return nil, false
		}
	}
	if escaped || rootClosed || len(stack) == 0 {
		return nil, false
	}
	suffix := make([]byte, 0, len(stack)+1)
	if inString {
		suffix = append(suffix, '"')
	}
	for index := len(stack) - 1; index >= 0; index-- {
		suffix = append(suffix, stack[index])
	}
	if len(suffix) > maxAdded {
		return nil, false
	}
	repaired := append(append([]byte(nil), trimmed...), suffix...)
	if !json.Valid(repaired) {
		return nil, false
	}
	return repaired, true
}

// recoverLeakedToolCalls salvages tool calls a model emitted as plain text
// instead of in the parseable format the backend recognizes. The zero-option
// behavior preserves the legacy XML and tool_call formats and their stable
// call_recovered_N IDs, while also accepting conservative fenced and bare JSON.
// The caller still validates names and schemas against the real tool catalog.
func recoverLeakedToolCalls(text string) (calls []toolUseBlock, cleaned string) {
	return recoverLeakedToolCallsWithOptions(text, ToolRecoveryOptions{})
}

// recoverLeakedToolCallsWithOptions applies explicit alias and catalog policy
// while extracting candidates. Extraction order is stable for compatibility:
// legacy function XML, legacy tool_call JSON, fenced JSON, then bare JSON.
func recoverLeakedToolCallsWithOptions(text string, opts ToolRecoveryOptions) (calls []toolUseBlock, cleaned string) {
	cleaned = text
	n := 0
	add := func(name string, raw json.RawMessage) bool {
		resolved, ok := resolveRecoveredToolName(name, opts)
		if !ok {
			return false
		}
		calls = append(calls, toolUseBlock{
			ID:       fmt.Sprintf("call_recovered_%d", n),
			Name:     resolved,
			Input:    raw,
			InputRaw: string(raw),
		})
		n++
		return true
	}

	// Format A: <function=NAME>...<parameter=k>v</parameter>...</function>.
	cleaned = leakFuncRe.ReplaceAllStringFunc(cleaned, func(match string) string {
		parts := leakFuncRe.FindStringSubmatch(match)
		if len(parts) != 3 {
			return match
		}
		input := map[string]string{}
		for _, parameter := range leakParamRe.FindAllStringSubmatch(parts[2], -1) {
			input[parameter[1]] = strings.TrimSpace(parameter[2])
		}
		raw, _ := json.Marshal(input)
		if !add(parts[1], raw) {
			return match
		}
		return ""
	})

	// Format B: <tool_call>{json}</tool_call>. Legacy envelopes stay permissive
	// about additional fields and argument shape; downstream schema validation
	// remains authoritative.
	cleaned = leakJSONRe.ReplaceAllStringFunc(cleaned, func(match string) string {
		parts := leakJSONRe.FindStringSubmatch(match)
		if len(parts) != 2 {
			return match
		}
		name, raw, ok := decodeRecoveredToolJSON(parts[1], opts, false)
		if !ok || !add(name, raw) {
			body := strings.TrimSpace(parts[1])
			if len(opts.Aliases) == 0 && len(opts.AllowedTools) == 0 && strings.HasPrefix(body, "{") && strings.HasSuffix(body, "}") {
				return ""
			}
			return match
		}
		return ""
	})

	// Format C: a complete Markdown fence whose body is one JSON tool envelope.
	cleaned = leakFenceRe.ReplaceAllStringFunc(cleaned, func(match string) string {
		parts := leakFenceRe.FindStringSubmatch(match)
		if len(parts) != 3 || !supportedToolFence(parts[1]) {
			return match
		}
		name, raw, ok := decodeRecoveredToolJSON(parts[2], opts, true)
		if !ok || !add(name, raw) {
			return match
		}
		return ""
	})

	// Format D: bare JSON is accepted only when it is the entire remaining
	// response. We never scan prose for an embedded object.
	bare := strings.TrimSpace(cleaned)
	if strings.HasPrefix(bare, "{") {
		if name, raw, ok := decodeRecoveredToolJSON(bare, opts, true); ok && add(name, raw) {
			cleaned = ""
		}
	}

	// Preserve the legacy handling for malformed orphan wrappers: remove only
	// the tags and leave their body visible to the caller.
	cleaned = strings.NewReplacer("<tool_call>", "", "</tool_call>", "").Replace(cleaned)
	return calls, strings.TrimSpace(cleaned)
}

func resolveRecoveredToolName(name string, opts ToolRecoveryOptions) (string, bool) {
	if name == "" {
		return "", false
	}
	if alias, ok := opts.Aliases[name]; ok {
		name = alias
		if name == "" {
			return "", false
		}
	}
	if len(opts.AllowedTools) > 0 {
		if _, ok := opts.AllowedTools[name]; !ok {
			return "", false
		}
	}
	return name, true
}

func supportedToolFence(language string) bool {
	switch strings.ToLower(strings.TrimSpace(language)) {
	case "", "json", "tool", "tool-call", "tool_call":
		return true
	default:
		return false
	}
}

func decodeRecoveredToolJSON(input string, opts ToolRecoveryOptions, strict bool) (string, json.RawMessage, bool) {
	rawEnvelope := []byte(strings.TrimSpace(input))
	if len(rawEnvelope) == 0 || rawEnvelope[0] != '{' {
		return "", nil, false
	}

	var envelope recoveredToolEnvelope
	var legacyEnvelope struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	decode := func(value []byte) error {
		if strict {
			envelope = recoveredToolEnvelope{}
			return json.Unmarshal(value, &envelope)
		}
		legacyEnvelope = struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}{}
		return json.Unmarshal(value, &legacyEnvelope)
	}
	if err := decode(rawEnvelope); err != nil {
		if !toolNameRe.Match(rawEnvelope) {
			return "", nil, false
		}
		maxAdded := opts.MaxRepairBytes
		if maxAdded <= 0 {
			maxAdded = defaultToolRepairBytes
		}
		repaired, ok := repairTruncatedJSONObject(rawEnvelope, maxAdded)
		if !ok || decode(repaired) != nil {
			return "", nil, false
		}
	}

	var name string
	var arguments json.RawMessage
	if strict {
		var ok bool
		name, ok = oneRecoveredString(envelope.Name, envelope.Tool, envelope.ToolName)
		if !ok {
			return "", nil, false
		}
		arguments, ok = oneRecoveredJSON(envelope.Arguments, envelope.Args, envelope.Parameters)
		if !ok {
			return "", nil, false
		}
	} else {
		name = legacyEnvelope.Name
		arguments = legacyEnvelope.Arguments
	}
	if len(arguments) == 0 {
		arguments = json.RawMessage("{}")
	}
	if strict && !isJSONObject(arguments) {
		return "", nil, false
	}

	// Resolve here as an early rejection so an unknown or empty alias cannot be
	// removed from cleaned text. add performs the same check for legacy XML.
	if _, ok := resolveRecoveredToolName(name, opts); !ok {
		return "", nil, false
	}
	return name, arguments, true
}

func oneRecoveredString(values ...string) (string, bool) {
	var selected string
	for _, value := range values {
		if value == "" {
			continue
		}
		if selected != "" && selected != value {
			return "", false
		}
		selected = value
	}
	return selected, selected != ""
}

func oneRecoveredJSON(values ...json.RawMessage) (json.RawMessage, bool) {
	var selected json.RawMessage
	for _, value := range values {
		value = bytes.TrimSpace(value)
		if len(value) == 0 {
			continue
		}
		if len(selected) > 0 && !bytes.Equal(canonicalRecoveredJSON(selected), canonicalRecoveredJSON(value)) {
			return nil, false
		}
		selected = append(json.RawMessage(nil), value...)
	}
	return selected, true
}

func canonicalRecoveredJSON(value json.RawMessage) []byte {
	var decoded any
	if json.Unmarshal(value, &decoded) != nil {
		return bytes.TrimSpace(value)
	}
	canonical, err := json.Marshal(decoded)
	if err != nil {
		return bytes.TrimSpace(value)
	}
	return canonical
}

func isJSONObject(value json.RawMessage) bool {
	value = bytes.TrimSpace(value)
	if len(value) == 0 || value[0] != '{' || value[len(value)-1] != '}' {
		return false
	}
	var object map[string]json.RawMessage
	return json.Unmarshal(value, &object) == nil
}

// repairTruncatedJSONObject performs a bounded, deterministic suffix-only
// repair. It may close an unterminated string and open object/array delimiters;
// it never inserts missing tokens in the middle or accepts trailing prose.
func repairTruncatedJSONObject(input []byte, maxAdded int) ([]byte, bool) {
	trimmed := bytes.TrimSpace(input)
	if len(trimmed) == 0 || trimmed[0] != '{' || maxAdded <= 0 {
		return nil, false
	}

	stack := make([]byte, 0, 8)
	inString := false
	escaped := false
	rootClosed := false
	for _, current := range trimmed {
		if rootClosed {
			if current != ' ' && current != '\t' && current != '\r' && current != '\n' {
				return nil, false
			}
			continue
		}
		if inString {
			if escaped {
				escaped = false
				continue
			}
			switch current {
			case '\\':
				escaped = true
			case '"':
				inString = false
			default:
				if current < 0x20 {
					return nil, false
				}
			}
			continue
		}

		switch current {
		case '"':
			inString = true
		case '{':
			stack = append(stack, '}')
		case '[':
			stack = append(stack, ']')
		case '}', ']':
			if len(stack) == 0 || stack[len(stack)-1] != current {
				return nil, false
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				rootClosed = true
			}
		}
	}

	// A dangling escape has multiple plausible meanings; do not guess it.
	if escaped || rootClosed || len(stack) == 0 {
		return nil, false
	}

	suffix := make([]byte, 0, len(stack)+1)
	if inString {
		suffix = append(suffix, '"')
	}
	for i := len(stack) - 1; i >= 0; i-- {
		suffix = append(suffix, stack[i])
	}
	if len(suffix) > maxAdded {
		return nil, false
	}

	repaired := append(append([]byte(nil), trimmed...), suffix...)
	if !json.Valid(repaired) {
		return nil, false
	}
	return repaired, true
}
