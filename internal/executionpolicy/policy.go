// Package executionpolicy defines the shared, run-scoped authority contract.
// It is independent of agent, server, hooks, and executor implementations so
// every boundary can consume the same immutable snapshot.
package executionpolicy

import (
	"fmt"
	"strings"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

type Mode string

const (
	ModeReadOnly  Mode = "read-only"
	ModeWorkspace Mode = "workspace"
	ModeFull      Mode = "full"
)

type Request struct {
	Mode     Mode   `json:"mode"`
	Revision uint64 `json:"revision,omitempty"`
}

type Source string

const (
	SourceDefault          Source = "default-workspace"
	SourceLegacyMigration  Source = "legacy-workspace-migration"
	SourceUserSelected     Source = "user-selected"
	SourceInherited        Source = "inherited"
	SourceChildRestriction Source = "child-restriction"
)

// Snapshot captures user authority and the exact runtime facts observed when
// the run starts. RuntimeCapabilities is descriptive and does not grant access.
type Snapshot struct {
	Mode                  Mode                 `json:"mode"`
	Revision              uint64               `json:"revision"`
	RuntimeCapabilities   sandbox.Capabilities `json:"runtimeCapabilities"`
	ParentRevision        uint64               `json:"parentRevision,omitempty"`
	FullSelectionRevision uint64               `json:"fullSelectionRevision,omitempty"`
	Source                Source               `json:"source"`
}

// Resolve validates an explicit mode or applies the compatibility migration.
// Historical AutoApprove settings remain thresholds inside workspace and
// never imply full access.
func Resolve(request Request, legacyAutoApprove string, capabilities sandbox.Capabilities) (Snapshot, error) {
	mode := request.Mode
	source := SourceUserSelected
	if mode == "" {
		if err := validateLegacyAutoApprove(legacyAutoApprove); err != nil {
			return Snapshot{}, err
		}
		mode = ModeWorkspace
		source = SourceDefault
		if strings.TrimSpace(legacyAutoApprove) != "" {
			source = SourceLegacyMigration
		}
	}
	if !validMode(mode) {
		return Snapshot{}, fmt.Errorf("invalid execution mode %q", mode)
	}
	revision := request.Revision
	if revision == 0 {
		revision = 1
	}
	snapshot := Snapshot{Mode: mode, Revision: revision, RuntimeCapabilities: capabilities, Source: source}
	if mode == ModeFull && source == SourceUserSelected {
		snapshot.FullSelectionRevision = revision
	}
	return snapshot, nil
}

// ResolveChild derives a child snapshot from its parent's authority. It
// rejects escalation and binds the child to the parent's exact revision.
func ResolveChild(parent Snapshot, requested Mode, capabilities sandbox.Capabilities) (Snapshot, error) {
	if err := ValidateSnapshot(parent); err != nil {
		return Snapshot{}, fmt.Errorf("invalid parent execution policy: %w", err)
	}
	mode := requested
	source := SourceChildRestriction
	if mode == "" {
		mode = parent.Mode
		source = SourceInherited
	}
	if !validMode(mode) {
		return Snapshot{}, fmt.Errorf("invalid child execution mode %q", mode)
	}
	if modeRank(mode) > modeRank(parent.Mode) {
		return Snapshot{}, fmt.Errorf("child execution mode %q exceeds parent mode %q", mode, parent.Mode)
	}
	return Snapshot{
		Mode:                  mode,
		Revision:              parent.Revision,
		RuntimeCapabilities:   capabilities,
		ParentRevision:        parent.Revision,
		FullSelectionRevision: parent.FullSelectionRevision,
		Source:                source,
	}, nil
}

// ValidateSnapshot checks the authority provenance carried across internal
// run boundaries. RuntimeCapabilities remain descriptive and are deliberately
// not considered when deciding whether a mode or child restriction is valid.
func ValidateSnapshot(snapshot Snapshot) error {
	if !validMode(snapshot.Mode) || snapshot.Revision == 0 {
		return fmt.Errorf("mode and revision are required")
	}
	switch snapshot.Source {
	case SourceDefault, SourceLegacyMigration, SourceUserSelected:
		if snapshot.ParentRevision != 0 {
			return fmt.Errorf("root policy cannot claim a parent revision")
		}
	case SourceInherited, SourceChildRestriction:
		if snapshot.ParentRevision == 0 || snapshot.ParentRevision != snapshot.Revision {
			return fmt.Errorf("child policy must bind its parent revision")
		}
	default:
		return fmt.Errorf("invalid policy source %q", snapshot.Source)
	}
	if snapshot.Mode == ModeFull {
		if snapshot.FullSelectionRevision == 0 {
			return fmt.Errorf("full mode requires explicit selection provenance")
		}
		if snapshot.Source == SourceUserSelected && snapshot.FullSelectionRevision != snapshot.Revision {
			return fmt.Errorf("full selection revision does not match the selected policy revision")
		}
	}
	if snapshot.Source == SourceUserSelected && snapshot.Mode != ModeFull && snapshot.FullSelectionRevision != 0 {
		return fmt.Errorf("non-full user selection cannot carry full-mode authority")
	}
	return nil
}

func validMode(mode Mode) bool {
	switch mode {
	case ModeReadOnly, ModeWorkspace, ModeFull:
		return true
	default:
		return false
	}
}

func modeRank(mode Mode) int {
	switch mode {
	case ModeReadOnly:
		return 0
	case ModeWorkspace:
		return 1
	case ModeFull:
		return 2
	default:
		return -1
	}
}

func validateLegacyAutoApprove(value string) error {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "safe", "moderate", "all", "none":
		return nil
	default:
		return fmt.Errorf("unsupported legacy auto-approval setting %q", value)
	}
}
