package agent

import (
	"fmt"
	"strings"

	"github.com/Dannykkh/corelay-code/internal/harness"
)

// PlanAnchorSpec is the mutable input used to construct a durable plan
// reminder. NewPlanAnchor copies and normalizes all caller-owned slices.
type PlanAnchorSpec struct {
	Objective        string
	CurrentStep      string
	RemainingSteps   []string
	DefinitionOfDone []string
	Revision         int
}

// PlanAnchor is immutable structured state for compact per-iteration plan
// reminders. Progress produces a new revision instead of mutating this value.
type PlanAnchor struct {
	objective        string
	currentStep      string
	remainingSteps   []string
	definitionOfDone []string
	revision         int
}

// NewPlanAnchor validates, normalizes, and copies a plan anchor specification.
func NewPlanAnchor(spec PlanAnchorSpec) (PlanAnchor, error) {
	objective := normalizeAnchorText(spec.Objective)
	if objective == "" {
		return PlanAnchor{}, fmt.Errorf("plan anchor objective is required")
	}
	if spec.Revision < 0 {
		return PlanAnchor{}, fmt.Errorf("plan anchor revision must be non-negative")
	}

	definitionOfDone := normalizeAnchorItems(spec.DefinitionOfDone)
	if len(definitionOfDone) == 0 {
		return PlanAnchor{}, fmt.Errorf("plan anchor Definition of Done is required")
	}

	return PlanAnchor{
		objective:        objective,
		currentStep:      normalizeAnchorText(spec.CurrentStep),
		remainingSteps:   normalizeAnchorItems(spec.RemainingSteps),
		definitionOfDone: definitionOfDone,
		revision:         spec.Revision,
	}, nil
}

func (a PlanAnchor) Objective() string   { return a.objective }
func (a PlanAnchor) CurrentStep() string { return a.currentStep }
func (a PlanAnchor) Revision() int       { return a.revision }

// RemainingSteps returns a copy so progress state cannot be mutated by callers.
func (a PlanAnchor) RemainingSteps() []string {
	return append([]string(nil), a.remainingSteps...)
}

// DefinitionOfDone returns a copy of the completion contract.
func (a PlanAnchor) DefinitionOfDone() []string {
	return append([]string(nil), a.definitionOfDone...)
}

// Valid reports whether the anchor contains the required durable state.
func (a PlanAnchor) Valid() bool {
	return a.objective != "" && len(a.definitionOfDone) > 0 && a.revision >= 0
}

// WithProgress returns the next immutable progress revision.
func (a PlanAnchor) WithProgress(currentStep string, remainingSteps []string) (PlanAnchor, error) {
	if !a.Valid() {
		return PlanAnchor{}, fmt.Errorf("cannot update an unresolved plan anchor")
	}
	return PlanAnchor{
		objective:        a.objective,
		currentStep:      normalizeAnchorText(currentStep),
		remainingSteps:   normalizeAnchorItems(remainingSteps),
		definitionOfDone: append([]string(nil), a.definitionOfDone...),
		revision:         a.revision + 1,
	}, nil
}

// Render emits a bounded prompt fragment. Strict mode adds the evidence-gate
// completion rule; off mode emits no prompt content.
func (a PlanAnchor) Render(mode harness.PlanAnchorMode) (string, error) {
	if mode == harness.PlanAnchorOff {
		return "", nil
	}
	if !mode.Valid() {
		return "", fmt.Errorf("invalid plan-anchor mode %q", mode)
	}
	if !a.Valid() {
		return "", fmt.Errorf("cannot render an unresolved plan anchor")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "<plan-anchor revision=%q>", fmt.Sprintf("%d", a.revision))
	b.WriteByte(10)
	writeAnchorLine(&b, "Objective", a.objective)
	if a.currentStep != "" {
		writeAnchorLine(&b, "Current step", a.currentStep)
	}
	writeAnchorList(&b, "Remaining steps", a.remainingSteps)
	writeAnchorList(&b, "Definition of Done", a.definitionOfDone)
	if mode == harness.PlanAnchorStrict {
		writeAnchorLine(
			&b,
			"Completion rule",
			"Report done only after the configured verification and evidence gates pass.",
		)
	}
	b.WriteString("</plan-anchor>")
	return b.String(), nil
}

func normalizeAnchorText(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func normalizeAnchorItems(items []string) []string {
	if len(items) == 0 {
		return nil
	}
	normalized := make([]string, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		item = normalizeAnchorText(item)
		if item == "" {
			continue
		}
		if _, exists := seen[item]; exists {
			continue
		}
		seen[item] = struct{}{}
		normalized = append(normalized, item)
	}
	return normalized
}

func writeAnchorLine(b *strings.Builder, label, value string) {
	b.WriteString(label)
	b.WriteString(": ")
	b.WriteString(escapeAnchorText(value))
	b.WriteByte(10)
}

func writeAnchorList(b *strings.Builder, label string, items []string) {
	b.WriteString(label)
	b.WriteString(":")
	b.WriteByte(10)
	for _, item := range items {
		b.WriteString("- ")
		b.WriteString(escapeAnchorText(item))
		b.WriteByte(10)
	}
}

func escapeAnchorText(value string) string {
	return strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	).Replace(value)
}
