package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/Dannykkh/corelay-code/internal/harness"
	"github.com/Dannykkh/corelay-code/internal/types"
)

const (
	maxUnifiedPatchBytes = 256 << 10
	maxUnifiedPatchHunks = 128
)

var unifiedHunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@(?: .*)?$`)

type editPolicyInput struct {
	FilePath   string `json:"file_path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
	Regex      bool   `json:"regex"`
	Patch      string `json:"patch"`
	Content    string `json:"content"`
}

func normalizeEditPolicy(policy harness.EditPolicy) harness.EditPolicy {
	if policy == "" {
		return harness.EditCorelayWaterfall
	}
	return policy
}

func applyEditPolicyToToolDefs(
	tools []types.ToolDef,
	policy harness.EditPolicy,
) []types.ToolDef {
	policy = normalizeEditPolicy(policy)
	result := append([]types.ToolDef(nil), tools...)
	for index := range result {
		if result[index].Name != "Edit" {
			continue
		}
		result[index] = editToolDefinitionForPolicy(policy)
		break
	}
	return result
}

func editToolDefinitionForPolicy(policy harness.EditPolicy) types.ToolDef {
	policy = normalizeEditPolicy(policy)
	definition := types.ToolDef{Name: "Edit"}
	switch policy {
	case harness.EditPatchFirst:
		definition.Description = "Apply a unified patch to one file. If patch application fails, optional old_string/new_string fields provide the exact/fuzzy fallback."
		definition.InputSchema = json.RawMessage(`{
			"type":"object",
			"properties":{
				"file_path":{"type":"string","description":"Path to the file to edit"},
				"patch":{"type":"string","description":"Unified diff hunks for this file only"},
				"old_string":{"type":"string","description":"Optional fallback text to replace"},
				"new_string":{"type":"string","description":"Optional fallback replacement"},
				"replace_all":{"type":"boolean","description":"Replace every fallback occurrence"}
			},
			"required":["file_path","patch"],
			"additionalProperties":false
		}`)
	case harness.EditExact:
		definition.Description = "Replace an exact string in a file. No regex or fuzzy fallback is permitted."
		definition.InputSchema = json.RawMessage(`{
			"type":"object",
			"properties":{
				"file_path":{"type":"string","description":"Path to the file to edit"},
				"old_string":{"type":"string","description":"Exact text to replace"},
				"new_string":{"type":"string","description":"Replacement text"},
				"replace_all":{"type":"boolean","description":"Replace every exact occurrence"}
			},
			"required":["file_path","old_string","new_string"],
			"additionalProperties":false
		}`)
	case harness.EditWholeFile:
		definition.Description = "Replace the complete contents of an existing file."
		definition.InputSchema = json.RawMessage(`{
			"type":"object",
			"properties":{
				"file_path":{"type":"string","description":"Path to the existing file"},
				"content":{"type":"string","description":"Complete replacement content"}
			},
			"required":["file_path","content"],
			"additionalProperties":false
		}`)
	default:
		definition.Description = "Replace a string in a file. Supports exact match, regex, and whitespace-insensitive fallback."
		definition.InputSchema = json.RawMessage(`{
			"type":"object",
			"properties":{
				"file_path":{"type":"string","description":"Path to the file to edit"},
				"old_string":{"type":"string","description":"The string to find and replace"},
				"new_string":{"type":"string","description":"The replacement string"},
				"replace_all":{"type":"boolean","description":"Replace all occurrences"},
				"regex":{"type":"boolean","description":"Treat old_string as a regex pattern"}
			},
			"required":["file_path","old_string","new_string"],
			"additionalProperties":false
		}`)
	}
	return definition
}

func isAllowedEditToolSchema(schema json.RawMessage) bool {
	for _, policy := range []harness.EditPolicy{
		harness.EditCorelayWaterfall,
		harness.EditPatchFirst,
		harness.EditExact,
		harness.EditWholeFile,
	} {
		candidate := editToolDefinitionForPolicy(policy).InputSchema
		if string(compactJSON(candidate)) == string(compactJSON(schema)) {
			return true
		}
	}
	return false
}

func compactJSON(input json.RawMessage) []byte {
	var value any
	if json.Unmarshal(input, &value) != nil {
		return nil
	}
	encoded, _ := json.Marshal(value)
	return encoded
}

func applyEditPolicy(
	content string,
	args editPolicyInput,
	policy harness.EditPolicy,
) (string, int, error) {
	policy = normalizeEditPolicy(policy)
	switch policy {
	case harness.EditWholeFile:
		return args.Content, 1, nil
	case harness.EditPatchFirst:
		patched, count, err := applyUnifiedPatch(content, args.Patch)
		if err == nil {
			return patched, count, nil
		}
		if args.OldString == "" {
			return "", 0, fmt.Errorf("unified patch did not apply: %w", err)
		}
		fallback, fallbackCount, fallbackErr := applyStringEdit(content, args, true, false)
		if fallbackErr != nil {
			return "", 0, fmt.Errorf("unified patch did not apply (%v); fallback failed: %w", err, fallbackErr)
		}
		return fallback, fallbackCount, nil
	case harness.EditExact:
		if args.Regex {
			return "", 0, errors.New("regex editing is disabled by the exact edit policy")
		}
		return applyStringEdit(content, args, false, false)
	case harness.EditCorelayWaterfall:
		return applyStringEdit(content, args, true, true)
	default:
		return "", 0, fmt.Errorf("unsupported edit policy %q", policy)
	}
}

func applyStringEdit(
	content string,
	args editPolicyInput,
	allowFuzzy bool,
	allowRegex bool,
) (string, int, error) {
	if args.Regex {
		if !allowRegex {
			return "", 0, errors.New("regex editing is disabled by this edit policy")
		}
		re, err := regexp.Compile(args.OldString)
		if err != nil {
			return "", 0, fmt.Errorf("invalid regex: %w", err)
		}
		matches := re.FindAllStringIndex(content, -1)
		if len(matches) == 0 {
			return "", 0, errors.New("regex pattern not found in file")
		}
		if args.ReplaceAll {
			return re.ReplaceAllString(content, args.NewString), len(matches), nil
		}
		location := matches[0]
		return content[:location[0]] + re.ReplaceAllString(content[location[0]:location[1]], args.NewString) + content[location[1]:], 1, nil
	}
	if args.OldString == "" {
		return "", 0, errors.New("old_string must not be empty")
	}
	if strings.Contains(content, args.OldString) {
		if args.ReplaceAll {
			count := strings.Count(content, args.OldString)
			return strings.ReplaceAll(content, args.OldString, args.NewString), count, nil
		}
		return strings.Replace(content, args.OldString, args.NewString, 1), 1, nil
	}
	if allowFuzzy {
		if result, ok := fuzzyReplace(content, args.OldString, args.NewString); ok {
			return result, 1, nil
		}
	}
	return "", 0, errors.New("old_string was not found")
}

type unifiedPatchHunk struct {
	oldStart int
	oldCount int
	newStart int
	newCount int
	lines    []string
}

func applyUnifiedPatch(content, patch string) (string, int, error) {
	if strings.TrimSpace(patch) == "" {
		return "", 0, errors.New("patch is empty")
	}
	if len(patch) > maxUnifiedPatchBytes {
		return "", 0, errors.New("patch exceeds the safe byte limit")
	}
	if strings.IndexByte(patch, 0) >= 0 {
		return "", 0, errors.New("patch contains a NUL byte")
	}
	patch = strings.ReplaceAll(patch, "\r\n", "\n")
	patch = strings.ReplaceAll(patch, "\r", "\n")
	patchLines := strings.Split(patch, "\n")
	hunks := make([]unifiedPatchHunk, 0, 4)
	for index := 0; index < len(patchLines); {
		line := patchLines[index]
		if !strings.HasPrefix(line, "@@ ") {
			if strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ") ||
				strings.HasPrefix(line, "diff ") || strings.HasPrefix(line, "index ") || line == "" {
				index++
				continue
			}
			return "", 0, fmt.Errorf("unexpected patch line %d", index+1)
		}
		match := unifiedHunkHeader.FindStringSubmatch(line)
		if match == nil {
			return "", 0, fmt.Errorf("invalid hunk header at line %d", index+1)
		}
		oldStart, _ := strconv.Atoi(match[1])
		oldCount := 1
		if match[2] != "" {
			oldCount, _ = strconv.Atoi(match[2])
		}
		newCount := 1
		newStart, _ := strconv.Atoi(match[3])
		if match[4] != "" {
			newCount, _ = strconv.Atoi(match[4])
		}
		if oldStart < 0 || oldCount < 0 || newStart < 0 || newCount < 0 ||
			(oldStart == 0 && oldCount != 0) || (newStart == 0 && newCount != 0) {
			return "", 0, fmt.Errorf("invalid hunk range at line %d", index+1)
		}
		index++
		hunk := unifiedPatchHunk{oldStart: oldStart, oldCount: oldCount, newStart: newStart, newCount: newCount}
		for index < len(patchLines) && !strings.HasPrefix(patchLines[index], "@@ ") {
			body := patchLines[index]
			if body == "" && index == len(patchLines)-1 {
				index++
				break
			}
			if strings.HasPrefix(body, `\ No newline at end of file`) {
				index++
				continue
			}
			if body == "" || (body[0] != ' ' && body[0] != '-' && body[0] != '+') {
				return "", 0, fmt.Errorf("invalid hunk body at line %d", index+1)
			}
			hunk.lines = append(hunk.lines, body)
			index++
		}
		hunks = append(hunks, hunk)
		if len(hunks) > maxUnifiedPatchHunks {
			return "", 0, errors.New("patch exceeds the safe hunk limit")
		}
	}
	if len(hunks) == 0 {
		return "", 0, errors.New("patch has no hunks")
	}

	lineEnding := "\n"
	if strings.Contains(content, "\r\n") {
		lineEnding = "\r\n"
	}
	finalNewline := strings.HasSuffix(content, "\n")
	normalizedContent := strings.ReplaceAll(content, "\r\n", "\n")
	source := strings.Split(strings.TrimSuffix(normalizedContent, "\n"), "\n")
	if content == "" {
		source = nil
	}
	output := make([]string, 0, len(source))
	sourceIndex := 0
	changes := 0
	for hunkIndex, hunk := range hunks {
		target := hunk.oldStart - 1
		if hunk.oldStart == 0 {
			target = 0
		}
		if target < sourceIndex || target > len(source) {
			return "", 0, fmt.Errorf("hunk %d starts outside the source", hunkIndex+1)
		}
		output = append(output, source[sourceIndex:target]...)
		expectedOutputStart := hunk.newStart - 1
		if hunk.newStart == 0 {
			expectedOutputStart = 0
		}
		if expectedOutputStart != len(output) {
			return "", 0, fmt.Errorf("hunk %d new range is inconsistent with prior hunks", hunkIndex+1)
		}
		sourceIndex = target
		consumed, produced := 0, 0
		for _, line := range hunk.lines {
			value := line[1:]
			switch line[0] {
			case ' ':
				if sourceIndex >= len(source) || source[sourceIndex] != value {
					return "", 0, fmt.Errorf("hunk %d context does not match source line %d", hunkIndex+1, sourceIndex+1)
				}
				output = append(output, value)
				sourceIndex++
				consumed++
				produced++
			case '-':
				if sourceIndex >= len(source) || source[sourceIndex] != value {
					return "", 0, fmt.Errorf("hunk %d removal does not match source line %d", hunkIndex+1, sourceIndex+1)
				}
				sourceIndex++
				consumed++
				changes++
			case '+':
				output = append(output, value)
				produced++
				changes++
			}
		}
		if consumed != hunk.oldCount || produced != hunk.newCount {
			return "", 0, fmt.Errorf(
				"hunk %d count mismatch: header old/new=%d/%d body=%d/%d",
				hunkIndex+1,
				hunk.oldCount,
				hunk.newCount,
				consumed,
				produced,
			)
		}
	}
	output = append(output, source[sourceIndex:]...)
	result := strings.Join(output, lineEnding)
	if finalNewline {
		result += lineEnding
	}
	return result, changes, nil
}
