package capabilityprofile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	CurrentProfileSchemaVersion      = 1
	CurrentObservationSchemaVersion  = 1
	ProfilerImplementationVersion    = "capability-profiler-v1"
	corelayWaterfallEditPolicy       = "corelay-waterfall"
	legacyAniClewWaterfallEditPolicy = "aniclew-waterfall"
	maxManualActorBytes              = 128
	maxManualReasonBytes             = 512
	maxManualOverrideLifetime        = 30 * 24 * time.Hour
	maxRecommendedTools              = 4_096
	maxRecommendedInputTokens        = 4_000_000
	maxRecommendedRepeatLimit        = 100
	maxObservedLatency               = 24 * time.Hour
)

var (
	minimumPersistedTimestamp = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	maximumPersistedTimestamp = time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)
)

// ProbeObservation is the bounded, non-content result returned by an
// Executor. There are deliberately no prompt, response, error-message, URL,
// header, or credential fields in this contract.
type ProbeObservation struct {
	SchemaVersion  int
	Success        bool
	Malformed      bool
	Retries        int
	Latency        time.Duration
	ContextTokens  int
	ToolCount      int
	FalseDone      bool
	Recovered      bool
	SafetyPassed   bool
	TraceDigest    string
	ArtifactDigest string
}

// ObservationRecord is the persisted, bounded result of one fixed probe
// attempt.
type ObservationRecord struct {
	SchemaVersion         int           `json:"schemaVersion"`
	ObservedSchemaVersion int           `json:"observedSchemaVersion"`
	CaseID                string        `json:"caseId"`
	Stage                 ProbeStage    `json:"stage"`
	Category              ProbeCategory `json:"category"`
	Attempt               int           `json:"attempt"`
	Success               bool          `json:"success"`
	Malformed             bool          `json:"malformed"`
	Retries               int           `json:"retries"`
	LatencyMillis         int64         `json:"latencyMillis"`
	ContextTokens         int           `json:"contextTokens"`
	ToolCount             int           `json:"toolCount"`
	FalseDone             bool          `json:"falseDone"`
	Recovered             bool          `json:"recovered"`
	SafetyCritical        bool          `json:"safetyCritical"`
	SafetyPassed          bool          `json:"safetyPassed"`
	ArtifactRequired      bool          `json:"artifactRequired"`
	TransportFailure      bool          `json:"transportFailure"`
	TraceDigest           string        `json:"traceDigest,omitempty"`
	ArtifactDigest        string        `json:"artifactDigest,omitempty"`
	IsolationDigest       string        `json:"isolationDigest"`
}

type AggregateMetrics struct {
	Attempts           int   `json:"attempts"`
	Successes          int   `json:"successes"`
	Malformed          int   `json:"malformed"`
	Retries            int   `json:"retries"`
	LatencyMillisTotal int64 `json:"latencyMillisTotal"`
	LatencyMillisMax   int64 `json:"latencyMillisMax"`
	ContextTokensMax   int   `json:"contextTokensMax"`
	ToolCountMax       int   `json:"toolCountMax"`
	FalseDone          int   `json:"falseDone"`
	Recoveries         int   `json:"recoveries"`
	TransportFailures  int   `json:"transportFailures"`
}

// Recommendations are empirical inputs for a future HarnessResolver wave.
// This package does not apply them to the agent loop.
type Recommendations struct {
	WirePolicy          string `json:"wirePolicy"`
	ResponsePolicy      string `json:"responsePolicy"`
	EditPolicy          string `json:"editPolicy"`
	PlanAnchorMode      string `json:"planAnchorMode"`
	MaxReliableTools    int    `json:"maxReliableTools"`
	ReliableInputTokens int    `json:"reliableInputTokens"`
	RepeatLimit         int    `json:"repeatLimit"`
	UseTwoStageRouting  bool   `json:"useTwoStageRouting"`
	ReadBeforeWrite     bool   `json:"readBeforeWrite"`
}

type ScoringPolicySnapshot struct {
	ConfidenceThresholdBasisPoints int `json:"confidenceThresholdBasisPoints"`
	MinimumObservations            int `json:"minimumObservations"`
}

type ProfileProvenance struct {
	ProfilerVersion  string                `json:"profilerVersion"`
	PlanVersion      string                `json:"planVersion"`
	PlanDigest       string                `json:"planDigest"`
	FixtureDigest    string                `json:"fixtureDigest,omitempty"`
	Variant          HarnessVariant        `json:"variant,omitempty"`
	ExpectedAttempts int                   `json:"expectedAttempts"`
	Scoring          ScoringPolicySnapshot `json:"scoring"`
}

type ManualOverride struct {
	ID        string    `json:"id"`
	Actor     string    `json:"actor"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type ManualOverrideSpec struct {
	ID        string
	Actor     string
	Reason    string
	ExpiresAt time.Time
}

// ProfileSnapshot is a defensive copy of an immutable CapabilityProfile.
type ProfileSnapshot struct {
	SchemaVersion         int                 `json:"schemaVersion"`
	ProfileID             string              `json:"profileId"`
	Target                TargetSnapshot      `json:"target"`
	Provenance            ProfileProvenance   `json:"provenance"`
	CreatedAt             time.Time           `json:"createdAt"`
	ExpiresAt             time.Time           `json:"expiresAt"`
	ConfidenceBasisPoints int                 `json:"confidenceBasisPoints"`
	Verified              bool                `json:"verified"`
	QuarantineReasons     []QuarantineReason  `json:"quarantineReasons"`
	Metrics               AggregateMetrics    `json:"metrics"`
	Recommendations       Recommendations     `json:"recommendations"`
	Observations          []ObservationRecord `json:"observations"`
	ManualOverrides       []ManualOverride    `json:"manualOverrides,omitempty"`
}

// CapabilityProfile owns its slices and is immutable to callers.
type CapabilityProfile struct {
	snapshot ProfileSnapshot
}

func (p CapabilityProfile) Valid() bool                { return validateProfileSnapshot(p.snapshot) == nil }
func (p CapabilityProfile) ID() string                 { return p.snapshot.ProfileID }
func (p CapabilityProfile) Target() TargetSnapshot     { return p.snapshot.Target }
func (p CapabilityProfile) Verified() bool             { return p.snapshot.Verified }
func (p CapabilityProfile) ConfidenceBasisPoints() int { return p.snapshot.ConfidenceBasisPoints }
func (p CapabilityProfile) ExpiresAt() time.Time       { return p.snapshot.ExpiresAt }

func (p CapabilityProfile) QuarantineReasons() []QuarantineReason {
	return append([]QuarantineReason(nil), p.snapshot.QuarantineReasons...)
}

func (p CapabilityProfile) Snapshot() ProfileSnapshot {
	return cloneSnapshot(p.snapshot)
}

// WithManualOverride returns a new content-addressed profile. It does not
// mutate or replace the original profile or patch its recommendations. The
// record authorizes only explicit selection of those existing recommendations
// while both the base profile and the override remain unexpired.
func (p CapabilityProfile) WithManualOverride(spec ManualOverrideSpec, now time.Time) (CapabilityProfile, error) {
	if !p.Valid() {
		return CapabilityProfile{}, ErrInvalidProfile
	}
	now = normalizeTime(now)
	if now.Before(p.snapshot.CreatedAt) || !p.snapshot.ExpiresAt.After(now) {
		return CapabilityProfile{}, fmt.Errorf("%w: base profile is not currently valid", ErrManualOverride)
	}
	id := strings.TrimSpace(spec.ID)
	actor := strings.TrimSpace(spec.Actor)
	reason := strings.TrimSpace(spec.Reason)
	expiresAt := normalizeTime(spec.ExpiresAt)
	if !safeIdentifier.MatchString(id) {
		return CapabilityProfile{}, fmt.Errorf("%w: invalid override ID", ErrManualOverride)
	}
	if err := validateProvenanceText(actor, maxManualActorBytes); err != nil {
		return CapabilityProfile{}, fmt.Errorf("%w: actor: %v", ErrManualOverride, err)
	}
	if err := validateProvenanceText(reason, maxManualReasonBytes); err != nil {
		return CapabilityProfile{}, fmt.Errorf("%w: reason: %v", ErrManualOverride, err)
	}
	if expiresAt.After(maximumPersistedTimestamp) || !expiresAt.After(now) || expiresAt.Sub(now) > maxManualOverrideLifetime {
		return CapabilityProfile{}, fmt.Errorf("%w: expiry must be in the future and no more than 30 days away", ErrManualOverride)
	}

	snapshot := p.Snapshot()
	for _, existing := range snapshot.ManualOverrides {
		if existing.ID == id {
			return CapabilityProfile{}, fmt.Errorf("%w: duplicate override ID", ErrManualOverride)
		}
	}
	snapshot.ManualOverrides = append(snapshot.ManualOverrides, ManualOverride{
		ID: id, Actor: actor, Reason: reason, CreatedAt: now, ExpiresAt: expiresAt,
	})
	sort.Slice(snapshot.ManualOverrides, func(i, j int) bool {
		return snapshot.ManualOverrides[i].ID < snapshot.ManualOverrides[j].ID
	})
	return profileFromSnapshot(snapshot, true)
}

func profileFromSnapshot(snapshot ProfileSnapshot, recomputeID bool) (CapabilityProfile, error) {
	snapshot = cloneSnapshot(snapshot)
	snapshot.CreatedAt = normalizeTime(snapshot.CreatedAt)
	snapshot.ExpiresAt = normalizeTime(snapshot.ExpiresAt)
	for i := range snapshot.ManualOverrides {
		snapshot.ManualOverrides[i].CreatedAt = normalizeTime(snapshot.ManualOverrides[i].CreatedAt)
		snapshot.ManualOverrides[i].ExpiresAt = normalizeTime(snapshot.ManualOverrides[i].ExpiresAt)
	}
	snapshot.QuarantineReasons = normalizeReasons(snapshot.QuarantineReasons)
	if recomputeID {
		snapshot.ProfileID = profileDigest(snapshot)
	}
	if err := validateProfileSnapshot(snapshot); err != nil {
		return CapabilityProfile{}, err
	}
	return CapabilityProfile{snapshot: snapshot}, nil
}

func validateProfileSnapshot(snapshot ProfileSnapshot) error {
	if snapshot.SchemaVersion != CurrentProfileSchemaVersion {
		return ErrSchemaMismatch
	}
	target, err := targetFromSnapshot(snapshot.Target)
	if err != nil {
		return err
	}
	if !target.Valid() || snapshot.Target.TargetDigest != target.Digest() {
		return ErrTargetMismatch
	}
	if !validDigest(snapshot.ProfileID) || profileDigest(snapshot) != snapshot.ProfileID {
		return fmt.Errorf("%w: content digest mismatch", ErrInvalidProfile)
	}
	if snapshot.CreatedAt.Before(minimumPersistedTimestamp) || snapshot.CreatedAt.After(maximumPersistedTimestamp) ||
		snapshot.ExpiresAt.Before(minimumPersistedTimestamp) || snapshot.ExpiresAt.After(maximumPersistedTimestamp) ||
		!snapshot.ExpiresAt.After(snapshot.CreatedAt) || snapshot.ExpiresAt.Sub(snapshot.CreatedAt) > maxProfileTTL {
		return fmt.Errorf("%w: invalid lifetime", ErrInvalidProfile)
	}
	if snapshot.Provenance.ProfilerVersion != ProfilerImplementationVersion ||
		!safeIdentifier.MatchString(snapshot.Provenance.PlanVersion) ||
		credentialLikeIdentityLabel(snapshot.Provenance.PlanVersion) ||
		!validDigest(snapshot.Provenance.PlanDigest) ||
		snapshot.Provenance.ExpectedAttempts <= 0 ||
		snapshot.Provenance.ExpectedAttempts > 2_560 ||
		snapshot.Provenance.Scoring.ConfidenceThresholdBasisPoints <= 0 ||
		snapshot.Provenance.Scoring.ConfidenceThresholdBasisPoints > 10_000 ||
		snapshot.Provenance.Scoring.MinimumObservations <= 0 {
		return fmt.Errorf("%w: invalid provenance", ErrInvalidProfile)
	}
	legacyPlan := snapshot.Provenance.Variant == "" && snapshot.Provenance.FixtureDigest == ""
	if legacyPlan {
		if snapshot.Provenance.PlanVersion != LegacyProbePlanVersion {
			return fmt.Errorf("%w: missing harness variant", ErrInvalidProfile)
		}
	} else if !snapshot.Provenance.Variant.valid() || !validDigest(snapshot.Provenance.FixtureDigest) {
		return fmt.Errorf("%w: invalid harness variant or fixture digest", ErrInvalidProfile)
	}
	if snapshot.ConfidenceBasisPoints < 0 || snapshot.ConfidenceBasisPoints > 10_000 {
		return fmt.Errorf("%w: invalid confidence", ErrInvalidProfile)
	}
	for _, reason := range snapshot.QuarantineReasons {
		if !reason.valid() {
			return fmt.Errorf("%w: invalid quarantine reason", ErrInvalidProfile)
		}
	}
	if !sameReasons(normalizeReasons(snapshot.QuarantineReasons), snapshot.QuarantineReasons) {
		return fmt.Errorf("%w: duplicate or unordered quarantine reasons", ErrInvalidProfile)
	}
	for _, observation := range snapshot.Observations {
		if err := validateObservationRecord(observation); err != nil {
			return err
		}
	}
	if len(snapshot.Observations) > snapshot.Provenance.ExpectedAttempts {
		return fmt.Errorf("%w: observation count exceeds plan", ErrInvalidProfile)
	}
	if err := validateObservationSet(snapshot.Observations); err != nil {
		return err
	}
	if metricsFor(snapshot.Observations) != snapshot.Metrics {
		return fmt.Errorf("%w: aggregate metrics mismatch", ErrInvalidProfile)
	}
	wantConfidence := scoreConfidence(calibrationObservations(snapshot.Observations), snapshot.Provenance.Scoring.MinimumObservations)
	if snapshot.ConfidenceBasisPoints != wantConfidence {
		return fmt.Errorf("%w: confidence mismatch", ErrInvalidProfile)
	}
	if snapshot.Verified {
		if len(snapshot.QuarantineReasons) != 0 ||
			snapshot.ConfidenceBasisPoints < snapshot.Provenance.Scoring.ConfidenceThresholdBasisPoints ||
			len(snapshot.Observations) != snapshot.Provenance.ExpectedAttempts ||
			!holdoutPassed(snapshot.Observations) || !safetyPassed(snapshot.Observations) {
			return fmt.Errorf("%w: verified profile does not satisfy gates", ErrInvalidProfile)
		}
	} else if len(snapshot.QuarantineReasons) == 0 {
		return fmt.Errorf("%w: unverified profile is not quarantined", ErrInvalidProfile)
	}
	if err := validateRecommendations(snapshot.Recommendations); err != nil {
		return fmt.Errorf("%w: invalid recommendations", ErrInvalidProfile)
	}
	seenOverrides := make(map[string]struct{}, len(snapshot.ManualOverrides))
	lastOverrideID := ""
	for _, override := range snapshot.ManualOverrides {
		if !safeIdentifier.MatchString(override.ID) {
			return fmt.Errorf("%w: invalid persisted override ID", ErrInvalidProfile)
		}
		if _, exists := seenOverrides[override.ID]; exists {
			return fmt.Errorf("%w: duplicate persisted override", ErrInvalidProfile)
		}
		seenOverrides[override.ID] = struct{}{}
		if lastOverrideID != "" && override.ID < lastOverrideID {
			return fmt.Errorf("%w: persisted overrides are not canonical", ErrInvalidProfile)
		}
		lastOverrideID = override.ID
		if err := validateProvenanceText(override.Actor, maxManualActorBytes); err != nil {
			return fmt.Errorf("%w: invalid persisted actor", ErrInvalidProfile)
		}
		if err := validateProvenanceText(override.Reason, maxManualReasonBytes); err != nil {
			return fmt.Errorf("%w: invalid persisted reason", ErrInvalidProfile)
		}
		if override.CreatedAt.Before(snapshot.CreatedAt) || override.CreatedAt.Before(minimumPersistedTimestamp) || override.CreatedAt.After(maximumPersistedTimestamp) ||
			override.ExpiresAt.After(maximumPersistedTimestamp) || !override.ExpiresAt.After(override.CreatedAt) || override.ExpiresAt.Sub(override.CreatedAt) > maxManualOverrideLifetime {
			return fmt.Errorf("%w: invalid persisted override lifetime", ErrInvalidProfile)
		}
	}
	return nil
}

func validateObservationRecord(observation ObservationRecord) error {
	if observation.SchemaVersion != CurrentObservationSchemaVersion ||
		observation.ObservedSchemaVersion < 0 || observation.ObservedSchemaVersion > 1_000_000 ||
		!safeIdentifier.MatchString(observation.CaseID) ||
		!observation.Stage.valid() || !observation.Category.valid() ||
		observation.Attempt <= 0 || observation.Attempt > maxProbeRepeats ||
		observation.Retries < 0 || observation.Retries > 100 ||
		observation.LatencyMillis < 0 || observation.LatencyMillis > maxObservedLatency.Milliseconds() ||
		observation.ContextTokens < 0 || observation.ContextTokens > maxRecommendedInputTokens ||
		observation.ToolCount < 0 || observation.ToolCount > maxRecommendedTools ||
		!validDigest(observation.IsolationDigest) {
		return fmt.Errorf("%w: malformed observation", ErrInvalidProfile)
	}
	if observation.TraceDigest != "" && !validDigest(observation.TraceDigest) {
		return fmt.Errorf("%w: malformed trace digest", ErrInvalidProfile)
	}
	if observation.ArtifactDigest != "" && !validDigest(observation.ArtifactDigest) {
		return fmt.Errorf("%w: malformed artifact digest", ErrInvalidProfile)
	}
	return nil
}

func validateObservationSet(observations []ObservationRecord) error {
	type caseShape struct {
		stage            ProbeStage
		category         ProbeCategory
		safetyCritical   bool
		artifactRequired bool
	}
	shapes := make(map[string]caseShape)
	attempts := make(map[string]map[int]struct{})
	for _, observation := range observations {
		shape := caseShape{observation.Stage, observation.Category, observation.SafetyCritical, observation.ArtifactRequired}
		if previous, exists := shapes[observation.CaseID]; exists && previous != shape {
			return fmt.Errorf("%w: inconsistent probe case shape", ErrInvalidProfile)
		}
		shapes[observation.CaseID] = shape
		if attempts[observation.CaseID] == nil {
			attempts[observation.CaseID] = make(map[int]struct{})
		}
		if _, duplicate := attempts[observation.CaseID][observation.Attempt]; duplicate {
			return fmt.Errorf("%w: duplicate probe attempt", ErrInvalidProfile)
		}
		attempts[observation.CaseID][observation.Attempt] = struct{}{}
	}
	for _, caseAttempts := range attempts {
		for attempt := 1; attempt <= len(caseAttempts); attempt++ {
			if _, exists := caseAttempts[attempt]; !exists {
				return fmt.Errorf("%w: non-contiguous probe attempts", ErrInvalidProfile)
			}
		}
	}
	return nil
}

func profileDigest(snapshot ProfileSnapshot) string {
	content := snapshot
	content.ProfileID = ""
	encoded, _ := json.Marshal(content)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func cloneSnapshot(snapshot ProfileSnapshot) ProfileSnapshot {
	snapshot.QuarantineReasons = append([]QuarantineReason(nil), snapshot.QuarantineReasons...)
	snapshot.Observations = append([]ObservationRecord(nil), snapshot.Observations...)
	snapshot.ManualOverrides = append([]ManualOverride(nil), snapshot.ManualOverrides...)
	return snapshot
}

func normalizeReasons(reasons []QuarantineReason) []QuarantineReason {
	if len(reasons) == 0 {
		return nil
	}
	order := map[QuarantineReason]int{
		QuarantineSchemaMismatch: 1, QuarantineTargetChanged: 2,
		QuarantineIsolation: 3, QuarantineCleanup: 4,
		QuarantineTraceMissing: 5, QuarantineArtifactMissing: 6,
		QuarantineSafetyFailure: 7, QuarantineHoldoutFailure: 8,
		QuarantineLowConfidence: 9, QuarantineExpired: 10,
		QuarantineManualOnly: 11, QuarantineCorrupt: 12,
	}
	seen := make(map[QuarantineReason]struct{}, len(reasons))
	result := make([]QuarantineReason, 0, len(reasons))
	for _, reason := range reasons {
		if _, exists := seen[reason]; exists {
			continue
		}
		seen[reason] = struct{}{}
		result = append(result, reason)
	}
	sort.Slice(result, func(i, j int) bool {
		left, lok := order[result[i]]
		right, rok := order[result[j]]
		if lok != rok {
			return lok
		}
		if left != right {
			return left < right
		}
		return result[i] < result[j]
	})
	return result
}

func sameReasons(left, right []QuarantineReason) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func validateRecommendations(recommendation Recommendations) error {
	validWire := recommendation.WirePolicy == "auto" || recommendation.WirePolicy == "anthropic-messages" ||
		recommendation.WirePolicy == "openai-chat-completions" || recommendation.WirePolicy == "openai-responses" ||
		recommendation.WirePolicy == "gemini" || recommendation.WirePolicy == "acp"
	validResponse := recommendation.ResponsePolicy == "native" || recommendation.ResponsePolicy == "native-with-text-recovery" || recommendation.ResponsePolicy == "multi-format"
	validEdit := recommendation.EditPolicy == corelayWaterfallEditPolicy ||
		recommendation.EditPolicy == legacyAniClewWaterfallEditPolicy ||
		recommendation.EditPolicy == "patch-first" || recommendation.EditPolicy == "exact" || recommendation.EditPolicy == "whole-file"
	validPlan := recommendation.PlanAnchorMode == "off" || recommendation.PlanAnchorMode == "compact" || recommendation.PlanAnchorMode == "strict"
	if !validWire || !validResponse || !validEdit || !validPlan || !recommendation.ReadBeforeWrite ||
		recommendation.MaxReliableTools < 0 || recommendation.MaxReliableTools > maxRecommendedTools ||
		recommendation.ReliableInputTokens < 0 || recommendation.ReliableInputTokens > maxRecommendedInputTokens ||
		recommendation.RepeatLimit < 0 || recommendation.RepeatLimit > maxRecommendedRepeatLimit {
		return ErrInvalidProfile
	}
	return nil
}

var secretLikeText = regexp.MustCompile(`(?i)(bearer\s+\S+|sk-[a-z0-9_-]{8,}|api[_ -]?key\s*[:=]\s*\S+|password\s*[:=]\s*\S+|token\s*[:=]\s*\S+)`)

func validateProvenanceText(value string, limit int) error {
	if value == "" || len(value) > limit || !utf8.ValidString(value) || containsControl(value) ||
		secretLikeText.MatchString(value) || strings.Contains(value, "://") {
		return fmt.Errorf("must be bounded printable text without credential-like material")
	}
	return nil
}

func normalizeTime(value time.Time) time.Time { return value.UTC().Round(0) }
