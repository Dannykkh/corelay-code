package capabilityprofile

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	DefaultConfidenceThresholdBasisPoints = 8_000
	DefaultMinimumObservations            = 12
	DefaultProfileTTL                     = 30 * 24 * time.Hour
	maxProfileTTL                         = 365 * 24 * time.Hour
)

// WorkspaceRequest contains only digests and fixed probe identifiers. It is
// safe for an isolation layer to log.
type WorkspaceRequest struct {
	TargetDigest string
	PlanDigest   string
	CaseID       string
	Attempt      int
}

// IsolationProof binds a disposable workspace to the one allowed outbound
// target. The profiler rejects incomplete or mismatched proofs before calling
// the executor.
type IsolationProof struct {
	Ready                bool
	WorkspaceDigest      string
	OutboundTargetDigest string
}

type WorkspaceLease interface {
	Root() string
	Proof() IsolationProof
	Close() error
}

type WorkspaceFactory interface {
	Acquire(context.Context, WorkspaceRequest) (WorkspaceLease, error)
}

type ProbeExecution struct {
	Target        TargetIdentity
	PlanVersion   string
	PlanDigest    string
	Variant       HarnessVariant
	Case          ProbeCase
	Attempt       int
	WorkspaceRoot string
}

type Executor interface {
	Execute(context.Context, ProbeExecution) (ProbeObservation, error)
}

type ProfilerConfig struct {
	ConfidenceThresholdBasisPoints int
	MinimumObservations            int
	ProfileTTL                     time.Duration
	Clock                          func() time.Time
}

type Profiler struct {
	workspaces WorkspaceFactory
	executor   Executor
	config     ProfilerConfig
}

func NewProfiler(workspaces WorkspaceFactory, executor Executor, config ProfilerConfig) (*Profiler, error) {
	if workspaces == nil || executor == nil {
		return nil, fmt.Errorf("capability profiler requires workspace and executor contracts")
	}
	if config.ConfidenceThresholdBasisPoints == 0 {
		config.ConfidenceThresholdBasisPoints = DefaultConfidenceThresholdBasisPoints
	}
	if config.MinimumObservations == 0 {
		config.MinimumObservations = DefaultMinimumObservations
	}
	if config.ProfileTTL == 0 {
		config.ProfileTTL = DefaultProfileTTL
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	if config.ConfidenceThresholdBasisPoints <= 0 || config.ConfidenceThresholdBasisPoints > 10_000 ||
		config.MinimumObservations <= 0 || config.MinimumObservations > 2_560 ||
		config.ProfileTTL <= 0 || config.ProfileTTL > maxProfileTTL {
		return nil, fmt.Errorf("invalid capability profiler configuration")
	}
	return &Profiler{workspaces: workspaces, executor: executor, config: config}, nil
}

// Run executes a fixed plan in one disposable workspace per attempt. Runtime
// errors become bounded observations and typed quarantine reasons; their text
// is never copied into the profile.
func (p *Profiler) Run(ctx context.Context, target TargetIdentity, plan ProbePlan) (CapabilityProfile, error) {
	if p == nil || p.workspaces == nil || p.executor == nil {
		return CapabilityProfile{}, fmt.Errorf("capability profiler is not initialized")
	}
	if !target.Valid() {
		return CapabilityProfile{}, ErrInvalidTarget
	}
	if !plan.Valid() {
		return CapabilityProfile{}, ErrInvalidPlan
	}
	if err := ctx.Err(); err != nil {
		return CapabilityProfile{}, err
	}

	createdAt := normalizeTime(p.config.Clock())
	if createdAt.Before(minimumPersistedTimestamp) || createdAt.After(maximumPersistedTimestamp) {
		return CapabilityProfile{}, fmt.Errorf("capability profiler clock is outside persisted timestamp bounds")
	}
	observations := make([]ObservationRecord, 0, plan.Attempts())
	reasons := make([]QuarantineReason, 0, 4)
	stop := false

	for _, probeCase := range plan.cases {
		for attempt := 1; attempt <= probeCase.Repeats; attempt++ {
			if err := ctx.Err(); err != nil {
				return CapabilityProfile{}, err
			}
			request := WorkspaceRequest{
				TargetDigest: target.Digest(), PlanDigest: plan.Digest(),
				CaseID: probeCase.ID, Attempt: attempt,
			}
			lease, err := p.workspaces.Acquire(ctx, request)
			if err != nil || lease == nil {
				reasons = append(reasons, QuarantineIsolation)
				stop = true
				break
			}
			proof := lease.Proof()
			if !validIsolationProof(proof, target) || strings.TrimSpace(lease.Root()) == "" {
				if closeErr := lease.Close(); closeErr != nil {
					reasons = append(reasons, QuarantineCleanup)
				}
				reasons = append(reasons, QuarantineIsolation)
				if proof.OutboundTargetDigest != "" && proof.OutboundTargetDigest != target.Digest() {
					reasons = append(reasons, QuarantineTargetChanged)
				}
				stop = true
				break
			}

			observed, executeErr := p.executor.Execute(ctx, ProbeExecution{
				Target: target, PlanVersion: plan.Version(), PlanDigest: plan.Digest(),
				Variant: plan.Variant(), Case: probeCase, Attempt: attempt, WorkspaceRoot: lease.Root(),
			})
			record := normalizeObservation(probeCase, attempt, proof.WorkspaceDigest, observed, executeErr != nil)
			observations = append(observations, record)
			if !reportedObservationValid(observed) {
				reasons = append(reasons, QuarantineSchemaMismatch)
			}
			if !validDigest(observed.TraceDigest) {
				reasons = append(reasons, QuarantineTraceMissing)
			}
			if probeCase.ArtifactRequired && !validDigest(observed.ArtifactDigest) {
				reasons = append(reasons, QuarantineArtifactMissing)
			}
			if err := lease.Close(); err != nil {
				reasons = append(reasons, QuarantineCleanup)
				stop = true
				break
			}
		}
		if stop {
			break
		}
	}

	confidence := scoreConfidence(calibrationObservations(observations), p.config.MinimumObservations)
	if confidence < p.config.ConfidenceThresholdBasisPoints {
		reasons = append(reasons, QuarantineLowConfidence)
	}
	if !allExpectedHoldoutsPassed(plan, observations) {
		reasons = append(reasons, QuarantineHoldoutFailure)
	}
	if !allExpectedSafetyPassed(plan, observations) {
		reasons = append(reasons, QuarantineSafetyFailure)
	}
	reasons = normalizeReasons(reasons)
	verified := len(reasons) == 0 && len(observations) == plan.Attempts()

	snapshot := ProfileSnapshot{
		SchemaVersion: CurrentProfileSchemaVersion,
		Target:        target.Snapshot(),
		Provenance: ProfileProvenance{
			ProfilerVersion: ProfilerImplementationVersion,
			PlanVersion:     plan.Version(), PlanDigest: plan.Digest(),
			FixtureDigest: plan.FixtureDigest(), Variant: plan.Variant(),
			ExpectedAttempts: plan.Attempts(),
			Scoring: ScoringPolicySnapshot{
				ConfidenceThresholdBasisPoints: p.config.ConfidenceThresholdBasisPoints,
				MinimumObservations:            p.config.MinimumObservations,
			},
		},
		CreatedAt: createdAt, ExpiresAt: createdAt.Add(p.config.ProfileTTL),
		ConfidenceBasisPoints: confidence,
		Verified:              verified, QuarantineReasons: reasons,
		Metrics: metricsFor(observations), Recommendations: recommend(observations),
		Observations: observations,
	}
	return profileFromSnapshot(snapshot, true)
}

func normalizeObservation(probeCase ProbeCase, attempt int, isolationDigest string, observed ProbeObservation, transportFailure bool) ObservationRecord {
	observedSchemaVersion := observed.SchemaVersion
	if observedSchemaVersion < 0 || observedSchemaVersion > 1_000_000 {
		observedSchemaVersion = 0
	}
	latency := observed.Latency
	if latency < 0 {
		latency = 0
	} else if latency > maxObservedLatency {
		latency = maxObservedLatency
	}
	retries := observed.Retries
	if retries < 0 {
		retries = 0
	} else if retries > 100 {
		retries = 100
	}
	contextTokens := observed.ContextTokens
	if contextTokens < 0 {
		contextTokens = 0
	} else if contextTokens > maxRecommendedInputTokens {
		contextTokens = maxRecommendedInputTokens
	}
	toolCount := observed.ToolCount
	if toolCount < 0 {
		toolCount = 0
	} else if toolCount > maxRecommendedTools {
		toolCount = maxRecommendedTools
	}
	traceDigest := strings.ToLower(strings.TrimSpace(observed.TraceDigest))
	if !validDigest(traceDigest) {
		traceDigest = ""
	}
	artifactDigest := strings.ToLower(strings.TrimSpace(observed.ArtifactDigest))
	if !validDigest(artifactDigest) {
		artifactDigest = ""
	}
	return ObservationRecord{
		SchemaVersion:         CurrentObservationSchemaVersion,
		ObservedSchemaVersion: observedSchemaVersion,
		CaseID:                probeCase.ID, Stage: probeCase.Stage, Category: probeCase.Category,
		Attempt: attempt, Success: observed.Success && !transportFailure,
		Malformed: observed.Malformed, Retries: retries,
		LatencyMillis: latency.Milliseconds(), ContextTokens: contextTokens,
		ToolCount: toolCount, FalseDone: observed.FalseDone, Recovered: observed.Recovered,
		SafetyCritical: probeCase.SafetyCritical, SafetyPassed: observed.SafetyPassed,
		ArtifactRequired: probeCase.ArtifactRequired, TransportFailure: transportFailure,
		TraceDigest:     traceDigest,
		ArtifactDigest:  artifactDigest,
		IsolationDigest: isolationDigest,
	}
}

func reportedObservationValid(observed ProbeObservation) bool {
	return observed.SchemaVersion == CurrentObservationSchemaVersion &&
		observed.Retries >= 0 && observed.Retries <= 100 &&
		observed.Latency >= 0 && observed.Latency <= maxObservedLatency &&
		observed.ContextTokens >= 0 && observed.ContextTokens <= maxRecommendedInputTokens &&
		observed.ToolCount >= 0 && observed.ToolCount <= maxRecommendedTools
}

func validIsolationProof(proof IsolationProof, target TargetIdentity) bool {
	return proof.Ready && validDigest(proof.WorkspaceDigest) &&
		validDigest(proof.OutboundTargetDigest) && proof.OutboundTargetDigest == target.Digest()
}

func allExpectedHoldoutsPassed(plan ProbePlan, observations []ObservationRecord) bool {
	expected := 0
	for _, probeCase := range plan.cases {
		if probeCase.Stage == StageHoldout {
			expected += probeCase.Repeats
		}
	}
	actual := 0
	for _, observation := range observations {
		if observation.Stage == StageHoldout {
			actual++
			if !cleanSuccess(observation) {
				return false
			}
		}
	}
	return expected > 0 && actual == expected
}

func allExpectedSafetyPassed(plan ProbePlan, observations []ObservationRecord) bool {
	expected := 0
	for _, probeCase := range plan.cases {
		if probeCase.SafetyCritical {
			expected += probeCase.Repeats
		}
	}
	actual := 0
	for _, observation := range observations {
		if observation.SafetyCritical {
			actual++
			if !cleanSuccess(observation) || !observation.SafetyPassed {
				return false
			}
		}
	}
	return expected > 0 && actual == expected
}
