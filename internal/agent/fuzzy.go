package agent

import (
	"fmt"
	"sort"
	"strings"
)

// fuzzyReplace attempts a whitespace-insensitive replacement when an exact
// match of `old` fails. It compares `old` and `content` line-by-line after
// trimming each line, so differences in indentation or trailing whitespace
// (a common error from weaker models quoting code) don't block the edit.
// This mirrors Aider's tiered fallback (exact → whitespace-normalized).
//
// Only the first matching window is replaced. Returns (result, matched).
func fuzzyReplace(content, old, newStr string) (string, bool) {
	contentLines := strings.Split(content, "\n")
	oldLines := strings.Split(strings.Trim(old, "\n"), "\n")
	n := len(oldLines)
	if n == 0 || old == "" {
		return "", false
	}

	oldTrim := make([]string, n)
	for i, l := range oldLines {
		oldTrim[i] = strings.TrimSpace(l)
	}

	for start := 0; start+n <= len(contentLines); start++ {
		matched := true
		for i := 0; i < n; i++ {
			if strings.TrimSpace(contentLines[start+i]) != oldTrim[i] {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		newLines := strings.Split(newStr, "\n")
		result := make([]string, 0, len(contentLines)-n+len(newLines))
		result = append(result, contentLines[:start]...)
		result = append(result, newLines...)
		result = append(result, contentLines[start+n:]...)
		return strings.Join(result, "\n"), true
	}
	return "", false
}

// identifierTokens splits a source line into identifier-ish tokens, dropping
// anything too short to be a useful signal.
func identifierTokens(line string) []string {
	fields := strings.FieldsFunc(strings.ToLower(line), func(r rune) bool {
		return !(r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	})
	tokens := make([]string, 0, len(fields))
	for _, f := range fields {
		if len(f) >= 3 {
			tokens = append(tokens, f)
		}
	}
	return tokens
}

// closestLinesHint points the model at where the text it was looking for
// probably lives, so a failed edit can be corrected on the next attempt rather
// than retried blindly.
//
// Two passes, because containment alone is not enough: it requires the model's
// first line to appear verbatim in the file, and a model that quoted the text
// correctly would not have reached this error in the first place. When the
// quote is wrong — the case that actually needs a hint — containment finds
// nothing and the hint comes back empty. The token pass covers that.
//
// Returns "" when nothing resembles the requested text.
func closestLinesHint(content, old string) string {
	first := strings.TrimSpace(strings.Split(strings.Trim(old, "\n"), "\n")[0])
	if first == "" {
		return ""
	}

	lines := strings.Split(content, "\n")

	// Pass 1: verbatim containment. Precise when the first line was quoted right.
	var hits []string
	for i, line := range lines {
		if strings.Contains(strings.TrimSpace(line), first) {
			hits = append(hits, fmt.Sprintf("  line %d: %s", i+1, strings.TrimSpace(line)))
			if len(hits) >= 3 {
				break
			}
		}
	}
	if len(hits) > 0 {
		return "\nSimilar lines in the file:\n" + strings.Join(hits, "\n")
	}

	// Pass 2: token overlap, ranked. Catches a misremembered quote as long as it
	// shares identifiers with the real line.
	wanted := identifierTokens(first)
	if len(wanted) == 0 {
		return ""
	}
	wantedSet := make(map[string]bool, len(wanted))
	for _, t := range wanted {
		wantedSet[t] = true
	}

	type scored struct {
		index   int
		text    string
		overlap int
	}
	var candidates []scored
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		seen := make(map[string]bool)
		overlap := 0
		for _, t := range identifierTokens(trimmed) {
			if wantedSet[t] && !seen[t] {
				seen[t] = true
				overlap++
			}
		}
		// Require more than a single shared token: one common word (an "if", a
		// variable used everywhere) would otherwise nominate half the file.
		if overlap >= 2 || (overlap == 1 && len(wantedSet) == 1) {
			candidates = append(candidates, scored{i, trimmed, overlap})
		}
	}
	if len(candidates) == 0 {
		return ""
	}

	sort.SliceStable(candidates, func(a, b int) bool {
		return candidates[a].overlap > candidates[b].overlap
	})
	if len(candidates) > 3 {
		candidates = candidates[:3]
	}

	parts := make([]string, 0, len(candidates))
	for _, c := range candidates {
		parts = append(parts, fmt.Sprintf("  line %d: %s", c.index+1, c.text))
	}
	return "\nClosest lines in the file (the text you asked for was not found verbatim):\n" +
		strings.Join(parts, "\n")
}
