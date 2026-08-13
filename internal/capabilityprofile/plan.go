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
	CurrentProbePlanSchemaVersion = 2
	LegacyProbePlanVersion        = "corelay-capability-probes-v1"
	DefaultProbePlanVersion       = "corelay-capability-probes-v2"
	maxProbeRepeats               = 20
)

// HarnessVariant identifies the immutable assistance policy used for every
// attempt in a ProbePlan. Variants share the Agent Kernel and safety pipeline;
// they differ only in the adaptive harness capabilities under measurement.
type HarnessVariant string

const (
	HarnessVariantCorelay HarnessVariant = "corelay"
	HarnessVariantMinimal HarnessVariant = "minimal"
)

func (v HarnessVariant) valid() bool {
	return v == HarnessVariantCorelay || v == HarnessVariantMinimal
}

func (v HarnessVariant) Valid() bool { return v.valid() }

func ParseHarnessVariant(value string) (HarnessVariant, error) {
	variant := HarnessVariant(strings.ToLower(strings.TrimSpace(value)))
	if !variant.valid() {
		return "", fmt.Errorf("%w: invalid harness variant", ErrInvalidPlan)
	}
	return variant, nil
}

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
	CategoryFormatCodeblock  ProbeCategory = "format-tool-codeblock"
	CategoryFormatTokenized  ProbeCategory = "format-tokenized"
	CategoryFormatFencedJSON ProbeCategory = "format-fenced-json"
	CategoryFormatBareJSON   ProbeCategory = "format-bare-json"
	CategoryToolCatalog      ProbeCategory = "tool-catalog"
	CategoryTwoStageRouting  ProbeCategory = "two-stage-routing"
	CategoryRepositoryMap    ProbeCategory = "repository-map"
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
		CategoryFormatCodeblock, CategoryFormatTokenized, CategoryFormatFencedJSON,
		CategoryFormatBareJSON, CategoryToolCatalog,
		CategoryTwoStageRouting, CategoryRepositoryMap, CategoryContextCeiling, CategoryEditPatch,
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
	Variant HarnessVariant
	Cases   []ProbeCase
}

type ProbePlan struct {
	version       string
	variant       HarnessVariant
	cases         []ProbeCase
	digest        string
	fixtureDigest string
}

var safeIdentifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

func NewProbePlan(spec ProbePlanSpec) (ProbePlan, error) {
	version := strings.TrimSpace(spec.Version)
	variant := spec.Variant
	if variant == "" {
		variant = HarnessVariantCorelay
	}
	if !safeIdentifier.MatchString(version) || credentialLikeIdentityLabel(version) {
		return ProbePlan{}, fmt.Errorf("%w: invalid version", ErrInvalidPlan)
	}
	if !variant.valid() {
		return ProbePlan{}, fmt.Errorf("%w: invalid harness variant", ErrInvalidPlan)
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

	payload := probePlanPayload{SchemaVersion: CurrentProbePlanSchemaVersion, Version: version, Variant: variant, Cases: cases}
	encoded, _ := json.Marshal(payload)
	sum := sha256.Sum256(encoded)
	fixtureEncoded, _ := json.Marshal(probeFixturePayload{
		SchemaVersion: CurrentProbePlanSchemaVersion, Version: version, Cases: cases,
	})
	fixtureSum := sha256.Sum256(fixtureEncoded)
	return ProbePlan{
		version: version, variant: variant, cases: cases,
		digest: hex.EncodeToString(sum[:]), fixtureDigest: hex.EncodeToString(fixtureSum[:]),
	}, nil
}

func DefaultProbePlan() ProbePlan {
	plan, err := ProbePlanForVariant(HarnessVariantCorelay)
	if err != nil {
		panic(err)
	}
	return plan
}

// ProbePlanForVariant returns the fixed production probe matrix for one
// immutable harness variant. All variants intentionally share the exact same
// fixture shapes, seeds, repeats, and holdouts.
func ProbePlanForVariant(variant HarnessVariant) (ProbePlan, error) {
	plan, err := NewProbePlan(ProbePlanSpec{
		Version: DefaultProbePlanVersion,
		Variant: variant,
		Cases: []ProbeCase{
			probe("native-call", StageCalibration, CategoryProtocolNative, 101, 3, 0, 8, false, false),
			probe("hermes-call", StageCalibration, CategoryFormatHermes, 102, 3, 0, 8, false, false),
			probe("liquid-call", StageCalibration, CategoryFormatLiquid, 103, 3, 0, 8, false, false),
			probe("tool-codeblock-call", StageCalibration, CategoryFormatCodeblock, 104, 3, 0, 8, false, false),
			probe("tokenized-call", StageCalibration, CategoryFormatTokenized, 105, 3, 0, 8, false, false),
			probe("fenced-json-call", StageCalibration, CategoryFormatFencedJSON, 106, 3, 0, 8, false, false),
			probe("bare-json-call", StageCalibration, CategoryFormatBareJSON, 107, 3, 0, 8, false, false),
			probe("tool-catalog-16", StageCalibration, CategoryToolCatalog, 201, 3, 0, 16, false, false),
			probe("tool-catalog-48", StageCalibration, CategoryToolCatalog, 202, 3, 0, 48, false, false),
			probe("routing-small-context", StageCalibration, CategoryTwoStageRouting, 203, 3, 16_000, 48, false, false),
			probe("repository-map", StageCalibration, CategoryRepositoryMap, 204, 3, 0, 32, false, false),
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
		return ProbePlan{}, err
	}
	return plan, nil
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
	if p.version == "" || !p.variant.valid() || !validDigest(p.digest) || !validDigest(p.fixtureDigest) || len(p.cases) == 0 {
		return false
	}
	resolved, err := NewProbePlan(ProbePlanSpec{Version: p.version, Variant: p.variant, Cases: p.cases})
	return err == nil && resolved.digest == p.digest && resolved.fixtureDigest == p.fixtureDigest
}

func (p ProbePlan) Version() string         { return p.version }
func (p ProbePlan) Variant() HarnessVariant { return p.variant }
func (p ProbePlan) Digest() string          { return p.digest }
func (p ProbePlan) FixtureDigest() string   { return p.fixtureDigest }
func (p ProbePlan) Cases() []ProbeCase      { return append([]ProbeCase(nil), p.cases...) }
func (p ProbePlan) Attempts() int {
	total := 0
	for _, probeCase := range p.cases {
		total += probeCase.Repeats
	}
	return total
}

type probePlanPayload struct {
	SchemaVersion int            `json:"schemaVersion"`
	Version       string         `json:"version"`
	Variant       HarnessVariant `json:"variant"`
	Cases         []ProbeCase    `json:"cases"`
}

type probeFixturePayload struct {
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
