package hooks

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

const (
	maxHookEnvironmentNames = 32
	maxHookEnvironmentName  = 64
	maxHookEnvironmentValue = 4096
	maxHookEnvironmentBytes = 32 * 1024
	maxHookOutputBytes      = 8 * 1024
	maxHookOutputScanBytes  = 64 * 1024
	maxSandboxDetailBytes   = 512
)

var allowedHookEnvironmentNames = map[string]struct{}{
	"AFTER_MESSAGES":  {},
	"BLOCK_CODE":      {},
	"COMPACT_TRIGGER": {},
	"ESTIMATED_INPUT": {},
	"MESSAGE_COUNT":   {},
	"MODEL":           {},
	"RESULT":          {},
	"SNAPSHOT_DIGEST": {},
	"STRATEGY":        {},
	"TOOL_ERROR":      {},
	"TOOL_NAME":       {},
	"TOOL_RESULT":     {},
	"WORK_DIR":        {},
}

var reservedHookEnvironmentNames = map[string]struct{}{
	"TOOL_RESULT_BYTES":  {},
	"TOOL_RESULT_SHA256": {},
}

var (
	credentialAssignmentPattern = regexp.MustCompile(`(?i)\b(api[_-]?key|access[_-]?token|auth(?:orization)?|credential|password|private[_-]?key|secret|token)\b\s*[:=]\s*(?:"[^"]*"|'[^']*'|[^\s,;]+)`)
	bearerPattern               = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]+`)
	secretTokenPattern          = regexp.MustCompile(`\b(?:sk|pk)-[A-Za-z0-9_-]{8,}\b`)
	httpURLPattern              = regexp.MustCompile(`(?i)\bhttps?://[^\s<>"']+`)
)

func buildHookEnvironment(input map[string]string, workDir string) (sandbox.EnvironmentSpec, HookFailureCode) {
	if len(input) > maxHookEnvironmentNames {
		return sandbox.EnvironmentSpec{}, HookFailureEnvironmentInvalid
	}
	keys := make([]string, 0, len(input))
	for key := range input {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	set := make(map[string]string, len(input)+3)
	for _, name := range keys {
		value := input[name]
		if !validHookEnvironmentName(name) || isCredentialLikeName(name) {
			return sandbox.EnvironmentSpec{}, HookFailureEnvironmentInvalid
		}
		if _, reserved := reservedHookEnvironmentNames[name]; reserved {
			return sandbox.EnvironmentSpec{}, HookFailureEnvironmentInvalid
		}
		if _, allowed := allowedHookEnvironmentNames[name]; !allowed {
			return sandbox.EnvironmentSpec{}, HookFailureEnvironmentInvalid
		}
		if name == "TOOL_RESULT" {
			digest := sha256.Sum256([]byte(value))
			set["TOOL_RESULT_SHA256"] = "sha256:" + hex.EncodeToString(digest[:])
			set["TOOL_RESULT_BYTES"] = strconv.Itoa(len(value))
			continue
		}
		if !validHookEnvironmentValue(value) {
			return sandbox.EnvironmentSpec{}, HookFailureEnvironmentInvalid
		}
		if name == "WORK_DIR" {
			continue
		}
		set[name] = value
	}
	if workDir == "" || !validHookEnvironmentValue(workDir) {
		return sandbox.EnvironmentSpec{}, HookFailureEnvironmentInvalid
	}
	set["WORK_DIR"] = workDir
	set["CLAUDE_PROJECT_DIR"] = workDir
	if len(set) > maxHookEnvironmentNames {
		return sandbox.EnvironmentSpec{}, HookFailureEnvironmentInvalid
	}
	totalBytes := 0
	for name, value := range set {
		totalBytes += len(name) + len(value)
	}
	if totalBytes > maxHookEnvironmentBytes {
		return sandbox.EnvironmentSpec{}, HookFailureEnvironmentInvalid
	}
	inherit := []string{"PATH"}
	if runtime.GOOS == "windows" {
		inherit = append(inherit, "SYSTEMROOT", "TEMP", "TMP", "WINDIR")
	}
	return sandbox.EnvironmentSpec{Inherit: inherit, Set: set}, HookFailureNone
}

func validHookEnvironmentName(name string) bool {
	if len(name) == 0 || len(name) > maxHookEnvironmentName {
		return false
	}
	for index, character := range name {
		if character == '_' || character >= 'A' && character <= 'Z' || index > 0 && character >= '0' && character <= '9' {
			continue
		}
		return false
	}
	return name[0] < '0' || name[0] > '9'
}

func validHookEnvironmentValue(value string) bool {
	return len(value) <= maxHookEnvironmentValue && utf8.ValidString(value) && !strings.ContainsRune(value, '\x00')
}

func isCredentialLikeName(name string) bool {
	normalized := strings.ToUpper(name)
	compact := strings.ReplaceAll(strings.ReplaceAll(normalized, "_", ""), "-", "")
	switch compact {
	case "APIKEY", "ACCESSTOKEN", "AUTHTOKEN", "AUTHORIZATION", "CREDENTIAL", "CREDENTIALS", "PASSWORD", "PRIVATEKEY", "SECRET", "SECRETS":
		return true
	}
	parts := strings.FieldsFunc(normalized, func(character rune) bool {
		return character < 'A' || character > 'Z'
	})
	for _, part := range parts {
		switch part {
		case "AUTH", "AUTHORIZATION", "CREDENTIAL", "CREDENTIALS", "KEY", "PASSWORD", "SECRET", "TOKEN":
			return true
		}
	}
	return false
}

func safeHookOutput(stdout, stderr []byte) (string, bool) {
	stdoutPreview, stdoutTruncated := boundedBytes(stdout, maxHookOutputScanBytes)
	stderrPreview, stderrTruncated := boundedBytes(stderr, maxHookOutputScanBytes)
	parts := make([]string, 0, 2)
	if value := strings.TrimSpace(string(stdoutPreview)); value != "" {
		parts = append(parts, value)
	}
	if value := strings.TrimSpace(string(stderrPreview)); value != "" {
		parts = append(parts, value)
	}
	redacted := redactHookText(strings.Join(parts, "\n"))
	bounded, displayTruncated := boundedUTF8(redacted, maxHookOutputBytes)
	return strings.TrimSpace(bounded), stdoutTruncated || stderrTruncated || displayTruncated
}

func redactHookText(value string) string {
	value = credentialAssignmentPattern.ReplaceAllStringFunc(value, func(match string) string {
		separator := strings.IndexAny(match, ":=")
		if separator < 0 {
			return "[redacted-credential]"
		}
		return strings.TrimSpace(match[:separator]) + "=[redacted]"
	})
	value = bearerPattern.ReplaceAllString(value, "Bearer [redacted]")
	value = secretTokenPattern.ReplaceAllString(value, "[redacted-token]")
	value = httpURLPattern.ReplaceAllString(value, "[redacted-url]")
	return value
}

func safeSandboxReport(report sandbox.Report) sandbox.Report {
	report.Runner = sanitizeMetadata(report.Runner, 128)
	report.RequestedEnforcement = safeEnforcement(report.RequestedEnforcement)
	report.EffectiveEnforcement = safeEnforcement(report.EffectiveEnforcement)
	report.Failure = safeSandboxFailureCode(report.Failure)
	if report.Failure == sandbox.FailureNone {
		report.Detail = ""
	} else {
		report.Detail = safeSandboxFailureMessage(report.Failure)
	}
	report.Detail, _ = boundedUTF8(redactHookText(report.Detail), maxSandboxDetailBytes)
	return report
}

func safeEnforcement(value sandbox.Enforcement) sandbox.Enforcement {
	switch value {
	case sandbox.EnforcementRequired, sandbox.EnforcementPreferred, sandbox.EnforcementDisabled:
		return value
	default:
		return ""
	}
}

func safeSandboxFailureCode(value sandbox.FailureCode) sandbox.FailureCode {
	switch value {
	case sandbox.FailureNone,
		sandbox.FailurePolicyInvalid,
		sandbox.FailureCapabilityUnavailable,
		sandbox.FailureRunnerUnavailable,
		sandbox.FailureCommandInvalid,
		sandbox.FailureStartFailed,
		sandbox.FailureTimedOut,
		sandbox.FailureCanceled,
		sandbox.FailureOutputLimit,
		sandbox.FailureExecutionFailed:
		return value
	default:
		return sandbox.FailureExecutionFailed
	}
}

func safeSandboxFailureMessage(failure sandbox.FailureCode) string {
	switch failure {
	case sandbox.FailurePolicyInvalid:
		return "sandbox policy rejected"
	case sandbox.FailureCapabilityUnavailable:
		return "required sandbox capability unavailable"
	case sandbox.FailureRunnerUnavailable:
		return "sandbox runner unavailable"
	case sandbox.FailureCommandInvalid:
		return "sandbox command rejected"
	case sandbox.FailureStartFailed:
		return "sandbox process did not start"
	case sandbox.FailureTimedOut:
		return "sandbox execution timed out"
	case sandbox.FailureCanceled:
		return "sandbox execution canceled"
	case sandbox.FailureOutputLimit:
		return "sandbox output exceeded its capture limit"
	case sandbox.FailureExecutionFailed:
		return "sandbox execution failed"
	default:
		return ""
	}
}

func sanitizeMetadata(value string, limit int) string {
	value = redactHookText(strings.TrimSpace(value))
	value, _ = boundedUTF8(value, limit)
	return value
}

func boundedBytes(value []byte, limit int) ([]byte, bool) {
	if len(value) <= limit {
		return value, false
	}
	return value[:limit], true
}

func boundedUTF8(value string, limit int) (string, bool) {
	changed := false
	if !utf8.ValidString(value) {
		value = strings.ToValidUTF8(value, "[invalid-utf8]")
		changed = true
	}
	if len(value) <= limit {
		return value, changed
	}
	value = value[:limit]
	for !utf8.ValidString(value) && len(value) > 0 {
		value = value[:len(value)-1]
	}
	return value, true
}

func ambientValue(name string) string {
	return os.Getenv(name)
}
