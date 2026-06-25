package workstream

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

func (s *Store) GenerateHandoff(id string, opts HandoffOptions) (HandoffSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ws, err := s.getLocked(id)
	if err != nil {
		return HandoffSnapshot{}, err
	}
	events, err := readJSONLines[TimelineEvent](TimelinePath(s.workspace, id))
	if err != nil {
		return HandoffSnapshot{}, err
	}
	now := time.Now().UTC()
	md := renderHandoff(*ws, events, now, opts)
	name := now.Format("20060102-150405") + "-" + slug(ws.Title) + ".md"
	path := filepath.Join(HandoffsDir(s.workspace, id), name)
	if err := writeFileAtomic(path, []byte(md)); err != nil {
		return HandoffSnapshot{}, err
	}
	if err := s.appendEventLocked(id, TimelineEvent{
		Type:    "handoff_generated",
		Message: "Handoff generated",
		Data:    map[string]string{"path": path},
	}); err != nil {
		return HandoffSnapshot{}, err
	}
	return HandoffSnapshot{Path: path, Markdown: md}, nil
}

func renderHandoff(ws Workstream, events []TimelineEvent, now time.Time, opts HandoffOptions) string {
	var b strings.Builder
	b.WriteString("# Handoff: " + ws.Title + "\n\n")
	b.WriteString("## Session Metadata\n")
	b.WriteString("- Created: " + now.Format(time.RFC3339) + "\n")
	b.WriteString("- Workspace: " + ws.Workspace + "\n")
	b.WriteString("- Workstream ID: " + ws.ID + "\n")
	b.WriteString("- Status: " + string(ws.Status) + "\n\n")

	b.WriteString("## Current State Summary\n")
	if ws.Summary != "" {
		b.WriteString(ws.Summary + "\n\n")
	} else {
		b.WriteString("(no summary recorded)\n\n")
	}

	b.WriteString("## Goal\n")
	if ws.Goal.Objective != "" {
		b.WriteString("- Objective: " + ws.Goal.Objective + "\n")
	}
	writeList(&b, "Acceptance Criteria", ws.Goal.AcceptanceCriteria)
	writeList(&b, "Definition Of Done", ws.Goal.DefinitionOfDone)
	if len(ws.Goal.VerificationPolicy.Commands) > 0 {
		writeList(&b, "Verification Commands", ws.Goal.VerificationPolicy.Commands)
	}
	b.WriteByte('\n')

	b.WriteString("## Verification\n")
	if ws.LastVerification.Status == "" {
		b.WriteString("- Status: not-run\n\n")
	} else {
		b.WriteString("- Status: " + ws.LastVerification.Status + "\n")
		if ws.LastVerification.Source != "" {
			b.WriteString("- Source: " + ws.LastVerification.Source + "\n")
		}
		if ws.LastVerification.Summary != "" {
			b.WriteString("- Summary: " + ws.LastVerification.Summary + "\n")
		}
		b.WriteByte('\n')
	}

	b.WriteString("## Recent Timeline\n")
	if len(events) == 0 {
		b.WriteString("- No events recorded.\n\n")
	} else {
		start := 0
		if len(events) > 10 {
			start = len(events) - 10
		}
		for _, e := range events[start:] {
			msg := e.Message
			if msg == "" {
				msg = e.Type
			}
			b.WriteString(fmt.Sprintf("- %s [%s] %s\n", e.At.Format(time.RFC3339), e.Type, msg))
		}
		b.WriteByte('\n')
	}

	b.WriteString("## Pending Work\n")
	if ws.NextAction != "" {
		b.WriteString("1. " + ws.NextAction + "\n\n")
	} else {
		b.WriteString("1. Decide the next action.\n\n")
	}

	b.WriteString("## Blockers And Open Questions\n")
	if len(ws.OpenQuestions) == 0 {
		b.WriteString("- None recorded.\n\n")
	} else {
		for _, q := range ws.OpenQuestions {
			b.WriteString("- " + strings.TrimSpace(q) + "\n")
		}
		b.WriteByte('\n')
	}

	b.WriteString("## Context For Resuming\n")
	b.WriteString("- Load the workstream by ID and continue from the pending work section.\n")
	if opts.IncludeReceipts {
		b.WriteString("- Receipt details are available from timeline event metadata when recorded.\n")
	}
	if opts.IncludeMemoryIndex {
		b.WriteString("- Long-term semantic memory remains in MEMORY.md and memory/ files; treat it as background data.\n")
	}
	return b.String()
}

func writeList(b *strings.Builder, title string, items []string) {
	if len(items) == 0 {
		return
	}
	b.WriteString("### " + title + "\n")
	for _, item := range items {
		b.WriteString("- " + strings.TrimSpace(item) + "\n")
	}
}
