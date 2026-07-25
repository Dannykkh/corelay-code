package memory

import (
	"fmt"
	"strings"
)

// Hard caps on MEMORY.md size, mirroring Claude Code's memdir/memdir.ts.
// The intent is to keep the entrypoint small enough that it can be loaded
// into every conversation context cheaply. Anything that doesn't fit gets
// pushed into topic files and referenced from the index.
const (
	// MaxEntrypointLines caps the number of lines we will load from
	// MEMORY.md. p97 of real indexes today; long single-line indexes can
	// slip past this cap, which is why MaxEntrypointBytes also exists.
	MaxEntrypointLines = 200

	// MaxEntrypointBytes caps the byte size of MEMORY.md after the line
	// cap. ~125 chars/line at 200 lines is ~25KB, which keeps the loaded
	// content well under one prompt-cache block.
	MaxEntrypointBytes = 25_000
)

// EntrypointTruncation is the result of applying the line and byte caps to
// raw MEMORY.md content. The Content field includes a trailing warning when
// truncation actually fired so the model can see why detail is missing.
type EntrypointTruncation struct {
	// Content is the (possibly truncated) MEMORY.md text plus warning.
	Content string

	// LineCount is the original line count BEFORE truncation.
	LineCount int

	// ByteCount is the original byte count BEFORE truncation.
	ByteCount int

	// LineCapped is true when the line cap fired.
	LineCapped bool

	// ByteCapped is true when the byte cap fired (based on the original
	// byte count, not the post-line-truncation size).
	ByteCapped bool
}

// TruncateEntrypoint enforces both caps and appends a warning that names
// which cap fired. Lines are cut first (a natural boundary), then bytes
// are cut at the last newline before the byte cap so we never slice a
// line in half. Inputs that fit both caps are returned unmodified.
func TruncateEntrypoint(raw string) EntrypointTruncation {
	trimmed := strings.TrimSpace(raw)
	lines := strings.Split(trimmed, "\n")
	lineCount := len(lines)
	byteCount := len(trimmed)

	lineCapped := lineCount > MaxEntrypointLines
	// Check the ORIGINAL byte count — long single lines are exactly the
	// failure mode the byte cap targets, and a post-line-truncation size
	// would understate the warning.
	byteCapped := byteCount > MaxEntrypointBytes

	if !lineCapped && !byteCapped {
		return EntrypointTruncation{
			Content:   trimmed,
			LineCount: lineCount,
			ByteCount: byteCount,
		}
	}

	var truncated string
	if lineCapped {
		truncated = strings.Join(lines[:MaxEntrypointLines], "\n")
	} else {
		truncated = trimmed
	}

	if len(truncated) > MaxEntrypointBytes {
		// Cut at the last newline before the byte cap. If there is no
		// newline (single huge line), fall back to a hard byte cut.
		head := truncated[:MaxEntrypointBytes]
		cut := strings.LastIndex(head, "\n")
		if cut <= 0 {
			cut = MaxEntrypointBytes
		}
		truncated = truncated[:cut]
	}

	reason := truncationReason(lineCount, byteCount, lineCapped, byteCapped)
	warning := "\n\n> WARNING: " + EntrypointName + " is " + reason +
		". Only a prefix reached the model. Index rows should stay on a " +
		"single line; put anything longer in the per-topic files."

	return EntrypointTruncation{
		Content:    truncated + warning,
		LineCount:  lineCount,
		ByteCount:  byteCount,
		LineCapped: lineCapped,
		ByteCapped: byteCapped,
	}
}

// truncationReason produces the human-readable cause shown in the warning.
// Order matters: byte-only and line-only have specific phrasings; the
// "both" case falls through to the generic form.
func truncationReason(lines, bytes int, lineCapped, byteCapped bool) string {
	switch {
	case byteCapped && !lineCapped:
		return fmt.Sprintf("%s (limit: %s) — index entries are too long",
			formatBytes(bytes), formatBytes(MaxEntrypointBytes))
	case lineCapped && !byteCapped:
		return fmt.Sprintf("%d lines (limit: %d)", lines, MaxEntrypointLines)
	default:
		return fmt.Sprintf("%d lines and %s", lines, formatBytes(bytes))
	}
}

// formatBytes is a tiny human-friendly byte size formatter, intentionally
// kept here so the memory package has no internal dependencies beyond the
// standard library. We do not need precision below 0.1KB granularity.
func formatBytes(n int) string {
	switch {
	case n >= 1024*1024:
		return fmt.Sprintf("%.1fMB", float64(n)/(1024*1024))
	case n >= 1024:
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	default:
		return fmt.Sprintf("%dB", n)
	}
}
