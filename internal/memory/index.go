package memory

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// IndexManager reads, merges, and rewrites MEMORY.md for a single
// workspace. It treats the file as a human-editable document: sections
// outside the managed "auto memory" block are preserved verbatim, so
// hand-authored project goals and keyword indexes survive refreshes.
//
// The managed block is delimited by two marker lines:
//
//	<!-- auto-memory:start -->
//	...managed content...
//	<!-- auto-memory:end -->
//
// These markers follow the convention used elsewhere in Corelay Code for
// sections written by tools (see e.g. CLAUDE.md's code map section).

const (
	autoStartMarker = "<!-- auto-memory:start -->"
	autoEndMarker   = "<!-- auto-memory:end -->"
)

// UpdateIndex rewrites the managed block of MEMORY.md for the workspace,
// reflecting the entries currently on disk. Hand-authored content
// outside the markers is preserved. If MEMORY.md does not exist yet, a
// minimal scaffold is created with the managed block already in place.
//
// Returns the truncation metadata for the FINAL content so callers can
// warn the user when their index is pushing against the caps.
func UpdateIndex(workDir string, entries []Entry) (EntrypointTruncation, error) {
	path := Entrypoint(workDir)

	// Read current content, or start from a scaffold.
	current, err := readEntrypoint(path)
	if errors.Is(err, os.ErrNotExist) {
		current = defaultScaffold()
	} else if err != nil {
		return EntrypointTruncation{}, fmt.Errorf("memory: read entrypoint: %w", err)
	}

	// Build the managed block from the live entries, then substitute it
	// into the current content between the markers (creating them if the
	// file pre-dates the convention).
	block := renderManagedBlock(entries)
	merged := substituteManagedBlock(current, block)

	// Enforce caps on the FINAL content. We store the capped version on
	// disk so that the next read stays bounded even if the user never
	// triggers a manual cleanup.
	trunc := TruncateEntrypoint(merged)
	if err := writeEntrypoint(path, trunc.Content); err != nil {
		return trunc, fmt.Errorf("memory: write entrypoint: %w", err)
	}
	return trunc, nil
}

// ReadIndex returns the current MEMORY.md content as EntrypointTruncation,
// applying caps so the caller sees exactly what a session load would see.
// Missing file is reported as an empty, uncapped truncation with a "no
// index yet" sentinel body so callers do not need a separate nil check.
func ReadIndex(workDir string) (EntrypointTruncation, error) {
	path := Entrypoint(workDir)
	content, err := readEntrypoint(path)
	if errors.Is(err, os.ErrNotExist) {
		return EntrypointTruncation{Content: "(no index yet)"}, nil
	}
	if err != nil {
		return EntrypointTruncation{}, err
	}
	return TruncateEntrypoint(content), nil
}

// defaultScaffold is used when MEMORY.md does not exist. Mirrors the
// shape the user's handwritten MEMORY.md already uses so the first
// automated write does not surprise the reader.
func defaultScaffold() string {
	var b strings.Builder
	b.WriteString("# MEMORY.md\n\n")
	b.WriteString("Long-term memory index for this workspace. Hand-edit the\n")
	b.WriteString("sections you own; the block between the markers below is\n")
	b.WriteString("managed by the Corelay Code memory service and will be rewritten.\n\n")
	b.WriteString(autoStartMarker)
	b.WriteByte('\n')
	b.WriteString(autoEndMarker)
	b.WriteByte('\n')
	return b.String()
}

// renderManagedBlock groups entries by type and produces a compact
// markdown section: one H3 per type, one bullet per entry. Bullets are
// one line (title — description) so they stay within the ~150-char
// guideline for MEMORY.md index rows.
func renderManagedBlock(entries []Entry) string {
	if len(entries) == 0 {
		return "\n_No extracted memories yet._\n"
	}

	byType := map[Type][]Entry{}
	for _, e := range entries {
		byType[e.Type] = append(byType[e.Type], e)
	}

	// Stable type ordering: user → feedback → project → reference.
	order := []Type{TypeUser, TypeFeedback, TypeProject, TypeReference}

	var b strings.Builder
	b.WriteByte('\n')
	for _, t := range order {
		list := byType[t]
		if len(list) == 0 {
			continue
		}
		sort.SliceStable(list, func(i, j int) bool { return list[i].Name < list[j].Name })
		b.WriteString("### ")
		b.WriteString(strings.ToUpper(string(t[:1])))
		b.WriteString(string(t[1:]))
		b.WriteByte('\n')
		for _, e := range list {
			b.WriteString("- **")
			b.WriteString(e.Name)
			b.WriteString("** — ")
			b.WriteString(oneLine(e.Description))
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// oneLine collapses any newlines and consecutive whitespace in s to
// single spaces, trimming the ends. Used for bullet descriptions so no
// multi-line description can accidentally break the index layout.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\n", " ")
	// Collapse runs of whitespace.
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return strings.TrimSpace(s)
}

// substituteManagedBlock finds the markers in doc and replaces the
// content between them with block. If the markers are missing, they
// (and block) are appended to the end of doc so the next call finds
// them. The returned content always ends with a single trailing
// newline so concatenation is well-behaved.
func substituteManagedBlock(doc, block string) string {
	start := strings.Index(doc, autoStartMarker)
	end := strings.Index(doc, autoEndMarker)
	if start < 0 || end < 0 || end < start {
		// Missing or malformed markers — append a fresh block at the end.
		var b strings.Builder
		b.WriteString(strings.TrimRight(doc, "\n"))
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(autoStartMarker)
		b.WriteString(block)
		b.WriteString(autoEndMarker)
		b.WriteByte('\n')
		return b.String()
	}

	head := doc[:start+len(autoStartMarker)]
	tail := doc[end:]
	return strings.TrimRight(head, "\n") + block + tail
}

// readEntrypoint reads MEMORY.md. Split out so tests can substitute if
// they ever need to; currently just a thin wrapper on os.ReadFile.
func readEntrypoint(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// writeEntrypoint writes MEMORY.md atomically via tmp + rename. Same
// rationale as WriteEntry in persist.go — a crash in the middle of a
// write must never leave a half-flushed index.
func writeEntrypoint(path, content string) error {
	if err := os.MkdirAll(dirOf(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(ensureTrailingNewline(content)), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// dirOf is a local shim to avoid importing path/filepath solely for
// filepath.Dir when we already have a string path on the stack.
func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			return path[:i]
		}
	}
	return "."
}

// ensureTrailingNewline guarantees the stored file ends with exactly
// one newline — helpful for diffing against user-edited revisions.
func ensureTrailingNewline(s string) string {
	s = strings.TrimRight(s, "\n")
	return s + "\n"
}
