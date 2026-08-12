// Package memory implements long-term memory storage and retrieval for the
// Corelay Code agent loop, inspired by Claude Code's memdir + extractMemories +
// autoDream pattern.
//
// This is distinct from agent.SessionMemory: that one offloads large tool
// outputs to disk to keep the active context small. This package stores
// SEMANTIC memory — durable facts, user preferences, project state, and
// pointers to external systems — extracted at the end of each query loop
// (via a forked agent) and periodically consolidated in the background.
//
// Storage layout (per workspace, hashed):
//
//	~/.corelay/projects/<key>/memory/
//	├── MEMORY.md       (index, capped at 200 lines / 25KB)
//	├── user_*.md
//	├── feedback_*.md
//	├── project_*.md
//	└── reference_*.md
package memory

// Type classifies a durable memory entry. The four types mirror the
// taxonomy used by Claude Code's memoryTypes.ts so that consolidation
// prompts can reason about them uniformly.
type Type string

const (
	// TypeUser captures the user's role, preferences, responsibilities, or
	// knowledge. Used to tailor future answers to the user's perspective.
	TypeUser Type = "user"

	// TypeFeedback captures guidance from the user about how to approach
	// work — both corrections ("don't do X") and confirmations ("yes that
	// approach was right"). Recorded with the reason so edge cases can be
	// judged later instead of blindly applied.
	TypeFeedback Type = "feedback"

	// TypeProject captures ongoing work, goals, decisions, and deadlines
	// that aren't otherwise derivable from code or git history. Decays
	// fast — relative dates should be converted to absolute on save.
	TypeProject Type = "project"

	// TypeReference captures pointers to external systems (Linear projects,
	// Grafana dashboards, Slack channels) and what they're for.
	TypeReference Type = "reference"
)

// Valid reports whether t is one of the four canonical types.
func (t Type) Valid() bool {
	switch t {
	case TypeUser, TypeFeedback, TypeProject, TypeReference:
		return true
	}
	return false
}

// Entry is a single durable memory item materialized as a Markdown file
// with YAML frontmatter on disk.
type Entry struct {
	// Name is a short identifier shown in the MEMORY.md index. Should be
	// human-readable and stable enough to dedupe against future entries.
	Name string `json:"name"`

	// Description is a one-line hook (under ~150 chars) used both in the
	// frontmatter and in the MEMORY.md index entry. It is what future
	// sessions read to decide whether to load the full body.
	Description string `json:"description"`

	// Type is one of the four canonical types.
	Type Type `json:"type"`

	// Body is the markdown body of the memory file (after frontmatter).
	// For feedback/project entries this should follow the convention of
	// leading with the rule/fact, then **Why:** and **How to apply:** lines.
	Body string `json:"body"`

	// File is the absolute path to the .md file on disk. Empty for entries
	// that have not been persisted yet.
	File string `json:"file,omitempty"`

	// ModifiedUnix is the file mtime in unix seconds. Zero before persist.
	ModifiedUnix int64 `json:"modified,omitempty"`
}
