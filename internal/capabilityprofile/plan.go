package capabilityprofile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const (
	CurrentProbePlanSchemaVersion = 1
	DefaultProbePlanVersion       = "corelay-capability-probes-v1"
	maxProbeRepeats               = 20
)

type ProbeStage string

const (
	StageCalibration ProbeStage = "calibration"
	StageHoldout     ProbeStage = "holdout"
)

func (s ProbeStage) valid() bool { return s == StageCalibration || s == StageHoldout }

type ProbeCategory string

const (
	CategoryProtocolNative   ProbeCategory = "protocol-native"
	CategoryFormatHermes     ProbeCategory = "format-hermes"
	CategoryFormatLiquid     ProbeCategory = "format-liquid"
	CategoryFormatFencedJSON ProbeCategory = "format-fenced-json"
	CategoryFormatBareJSON   ProbeCategory = "format-bare-json"
	CategoryToolCatalog      ProbeCategory = "tool-catalog"
	CategoryTwoStageRouting  ProbeCategory = "two-stage-routing"
	CategoryContextCeiling   ProbeCategory = "context-ceiling"
	CategoryEditPatch        ProbeCategory = "edit-patch"
	CategoryEditExact        ProbeCategory = "edit-exact"
	CategoryEditFuzzy        ProbeCategory = "edit-fuzzy"
	CategoryRepetition       ProbeCategory = "repetition"
	CategoryTruncation       ProbeCategory = "truncation"
	CategoryPlanAnchor       ProbeCategory = "plan-anchor"
	CategorySafetyBoundary   ProbeCategory = "safety-boundary"
	CategorySafetyToolDenial ProbeCategory = "safety-tool-denial"
)

func (c ProbeCategory) valid() bool {
	switch c {
	case CategoryProtocolNative, CategoryFormatHermes, CategoryFormatLiquid,
		CategoryFormatFencedJSON, CategoryFormatBareJSON, CategoryToolCatalog,
		CategoryTwoStageRouting, CategoryContextCeiling, CategoryEditPatch,
		CategoryEditExact, CategoryEditFuzzy, CategoryRepetition,
		CategoryTruncation, CategoryPlanAnchor, CategorySafetyBoundary,
		CategorySafetyToolDenial:
		return true
	default:
		return false
	}
}

// ProbeCase contains only deterministic fixture metadata, never raw prompts.
type ProbeCase struct {
	ID               string        `json:"id"`
	FixtureVersion   string        `json:"fixtureVersion"`
	Stage            ProbeStage    `json:"stage"`
	Category         ProbeCategory `json:"category"`
	Seed             int64         `json:"seed"`
	Repeats          int           `json:"repeats"`
	ContextTokens    int           `json:"contextTokens,omitempty"`
	ToolCount        int           `json:"toolCount,omitempty"`
	SafetyCritical   bool          `json:"safetyCritical,omitempty"`
	ArtifactRequired bool          `json:"artifactRequired"`
}

type ProbePlanSpec struct {
	Version string
	Cases   []ProbeCase
}

type ProbePlan struct {
	version string
	cases   []ProbeCase
	digest  string
}

var safeIdentifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

func NewProbePlan(spec ProbePlanSpec) (ProbePlan, error) {
	version := strings.TrimSpace(spec.Version)
	if !safeIdentifier.MatchString(version) || credentialLikeIdentityLabel(version) {
		return ProbePlan{}, fmt.Errorf("%w: invalid version", ErrInvalidPlan)
	}
	if len(spec.Cases) == 0 || len(spec.Cases) > 128 {
		return ProbePlan{}, fmt.Errorf("%w: case count must be between 1 and 128", ErrInvalidPlan)
	}

	cases := append([]ProbeCase(nil), spec.Cases...)
	seen := make(map[string]struct{}, len(cases))
	hasCalibration := false
	hasHoldout := false
	hasSafety := false
	hasHoldoutSafety := false
	for i := range cases {
		probeCase := &cases[i]
		probeCase.ID = strings.TrimSpace(probeCase.ID)
		probeCase.FixtureVersion = strings.TrimSpace(probeCase.FixtureVersion)
		if !safeIdentifier.MatchString(probeCase.ID) || !safeIdentifier.MatchString(probeCase.FixtureVersion) ||
			credentialLikeIdentityLabel(probeCase.ID) || credentialLikeIdentityLabel(probeCase.FixtureVersion) {
			return ProbePlan{}, fmt.Errorf("%w: case %d has invalid identifiers", ErrInvalidPlan, i)
		}
		if _, exists := seen[probeCase.ID]; exists {
			return ProbePlan{}, fmt.Errorf("%w: duplicate case ID %q", ErrInvalidPlan, probeCase.ID)
		}
		seen[probeCase.ID] = struct{}{}
		if !probeCase.Stage.valid() || !probeCase.Category.valid() {
			return ProbePlan{}, fmt.Errorf("%w: case %q has unsupported stage or category", ErrInvalidPlan, probeCase.ID)
		}
		if probeCase.Repeats <= 0 || probeCase.Repeats > maxProbeRepeats ||
			probeCase.ContextTokens < 0 || probeCase.ContextTokens > maxRecommendedInputTokens ||
			probeCase.ToolCount < 0 || probeCase.ToolCount > maxRecommendedTools {
			return ProbePlan{}, fmt.Errorf("%w: case %q has invalid bounds", ErrInvalidPlan, probeCase.ID)
		}
		hasCalibration = hasCalibration || probeCase.Stage == StageCalibration
		hasHoldout = hasHoldout || probeCase.Stage == StageHoldout
		hasSafety = hasSafety || probeCase.SafetyCritical
		hasHoldoutSafety = hasHoldoutSafety || (probeCase.Stage == StageHoldout && probeCase.SafetyCritical)
	}
	if !hasCalibration || !hasHoldout || !hasSafety || !hasHoldoutSafety {
		return ProbePlan{}, fmt.Errorf("%w: calibration, holdout, and holdout-safety cases are required", ErrInvalidPlan)
	}

	payload := probePlanPayload{SchemaVersion: CurrentProbePlanSchemaVersion, Version: version, Cases: cases}
	encoded, _ := json.Marshal(payload)
	sum := sha256.Sum256(encoded)
	return ProbePlan{version: version, cases: cases, digest: hex.EncodeToString(sum[:])}, nil
}

func DefaultProbePlan() ProbePlan {
	plan, err := NewProbePlan(ProbePlanSpec{
		Version: DefaultProbePlanVersion,
		Cases: []ProbeCase{
			probe("native-call", StageCalibration, CategoryProtocolNative, 101, 3, 0, 8, false, false),
			probe("hermes-call", StageCalibration, CategoryFormatHermes, 102, 3, 0, 8, false, false),
			probe("liquid-call", StageCalibration, CategoryFormatLiquid, 103, 3, 0, 8, false, false),
			probe("fenced-json-call", StageCalibration, CategoryFormatFencedJSON, 104, 3, 0, 8, false, false),
			probe("bare-json-call", StageCalibration, CategoryFormatBareJSON, 105, 3, 0, 8, false, false),
			probe("tool-catalog-16", StageCalibration, CategoryToolCatalog, 201, 3, 0, 16, false, false),
			probe("tool-catalog-48", StageCalibration, CategoryToolCatalog, 202, 3, 0, 48, false, false),
			probe("routing-small-context", StageCalibration, CategoryTwoStageRouting, 203, 3, 16_000, 48, false, false),
			probe("context-16k", StageCalibration, CategoryContextCeiling, 301, 3, 16_000, 8, false, false),
			probe("context-32k", StageCalibration, CategoryContextCeiling, 302, 3, 32_000, 8, false, false),
			probe("edit-patch", StageCalibration, CategoryEditPatch, 401, 3, 0, 8, false, true),
			probe("edit-exact", StageCalibration, CategoryEditExact, 402, 3, 0, 8, false, true),
			probe("edit-fuzzy", StageCalibration, CategoryEditFuzzy, 403, 3, 0, 8, false, true),
			probe("repetition-stop", StageCalibration, CategoryRepetition, 501, 3, 0, 8, false, false),
			probe("truncation-recovery", StageCalibration, CategoryTruncation, 502, 3, 0, 8, false, false),
			probe("plan-anchor", StageCalibration, CategoryPlanAnchor, 503, 3, 0, 8, false, false),
			probe("workspace-boundary", StageCalibration, CategorySafetyBoundary, 601, 3, 0, 4, true, true),
			probe("unsafe-tool-denial", StageCalibration, CategorySafetyToolDenial, 602, 3, 0, 4, true, false),
			probe("holdout-native", StageHoldout, CategoryProtocolNative, 701, 2, 0, 12, false, false),
			probe("holdout-context", StageHoldout, CategoryContextCeiling, 702, 2, 24_000, 12, false, false),
			probe("holdout-edit", StageHoldout, CategoryEditPatch, 703, 2, 0, 12, false, true),
			probe("holdout-safety", StageHoldout, CategorySafetyBoundary, 704, 2, 0, 4, true, true),
		},
	})
	if err != nil {
		panic(err)
	}
	return plan
}

func probe(id string, stage ProbeStage, category ProbeCategory, seed int64, repeats, contextTokens, toolCount int, safety, artifact bool) ProbeCase {
	return ProbeCase{
		ID:               id,
		FixtureVersion:   "fixture-v1",
		Stage:            stage,
		Category:         category,
		Seed:             seed,
		Repeats:          repeats,
		ContextTokens:    contextTokens,
		ToolCount:        toolCount,
		SafetyCritical:   safety,
		ArtifactRequired: artifact,
	}
}

func (p ProbePlan) Valid() bool {
	if p.version == "" || !validDigest(p.digest) || len(p.cases) == 0 {
		return false
	}
	resolved, err := NewProbePlan(ProbePlanSpec{Version: p.version, Cases: p.cases})
	return err == nil && resolved.digest == p.digest
}

func (p ProbePlan) Version() string    { return p.version }
func (p ProbePlan) Digest() string     { return p.digest }
func (p ProbePlan) Cases() []ProbeCase { return append([]ProbeCase(nil), p.cases...) }
func (p ProbePlan) Attempts() int {
	total := 0
	for _, probeCase := range p.cases {
		total += probeCase.Repeats
	}
	return total
}

type probePlanPayload struct {
	SchemaVersion int         `json:"schemaVersion"`
	Version       string      `json:"version"`
	Cases         []ProbeCase `json:"cases"`
}

// SortedCategories is useful for deterministic diagnostics without exposing
// the plan's owned case slice.
func (p ProbePlan) SortedCategories() []ProbeCategory {
	set := make(map[ProbeCategory]struct{})
	for _, probeCase := range p.cases {
		set[probeCase.Category] = struct{}{}
	}
	categories := make([]ProbeCategory, 0, len(set))
	for category := range set {
		categories = append(categories, category)
	}
	sort.Slice(categories, func(i, j int) bool { return categories[i] < categories[j] })
	return categories
}
