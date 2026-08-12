package acpbridge

import (
	"crypto/rand"
	"encoding/base32"
	"regexp"
	"strings"
	"unicode/utf8"
)

var outboundSecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(authorization\s*[:=]\s*(?:bearer|basic)\s+)[^\s,;]+`),
	regexp.MustCompile(`(?i)((?:api[_-]?key|access[_-]?token|refresh[_-]?token|password|passwd|secret)\s*[:=]\s*["']?)[^\s,"';]+`),
	regexp.MustCompile(`(?i)(https?://)[^/@\s]+:[^/@\s]+@`),
	regexp.MustCompile(`(?i)\b(?:sk[-_]|ghp_|github_pat_|xox[baprs]-)[A-Za-z0-9_-]{12,}\b`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`),
}

func boundedWireString(value string, maxBytes int) string {
	if maxBytes <= 0 || !utf8.ValidString(value) {
		return ""
	}
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value)
	if len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func sanitizeText(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if !utf8.ValidString(value) {
		value = strings.ToValidUTF8(value, "�")
	}
	for _, pattern := range outboundSecretPatterns {
		value = pattern.ReplaceAllStringFunc(value, func(match string) string {
			if index := strings.IndexAny(match, ":="); index >= 0 {
				prefix := match[:index+1]
				if strings.HasPrefix(strings.ToLower(match), "http") {
					return prefix + "//[REDACTED]@"
				}
				return prefix + " [REDACTED]"
			}
			return "[REDACTED]"
		})
	}
	value = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return r
		default:
			if r < 0x20 || r == 0x7f {
				return ' '
			}
			return r
		}
	}, value)
	if len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func newOpaqueID(prefix string) (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return prefix + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(bytes)), nil
}
