package capabilityprofile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 8, 12, 1, 2, 3, 0, time.UTC)

type fakeWorkspaceFactory struct {
	fail        bool
	badProof    bool
	wrongTarget bool
	closeErr    bool
	acquires    int
}

func (f *fakeWorkspaceFactory) Acquire(_ context.Context, request WorkspaceRequest) (WorkspaceLease, error) {
	f.acquires++
	if f.fail {
		return nil, errors.New("workspace failure containing raw-secret-error")
	}
	outbound := request.TargetDigest
	if f.wrongTarget {
		outbound = digestText("another-target")
	}
	proof := IsolationProof{
		Ready:                !f.badProof,
		WorkspaceDigest:      digestText(fmt.Sprintf("workspace:%s:%d", request.CaseID, request.Attempt)),
		OutboundTargetDigest: outbound,
	}
	return &fakeLease{root: fmt.Sprintf("isolated/%s/%d", request.CaseID, request.Attempt), proof: proof, closeErr: f.closeErr}, nil
}

type fakeLease struct {
	root     string
	proof    IsolationProof
	closeErr bool
}

func (l *fakeLease) Root() string          { return l.root }
func (l *fakeLease) Proof() IsolationProof { return l.proof }
func (l *fakeLease) Close() error {
	if l.closeErr {
		return errors.New("cleanup failure containing raw-secret-error")
	}
	return nil
}

type fakeExecutor struct {
	calls  int
	mutate func(ProbeExecution, *ProbeObservation) error
}

func (e *fakeExecutor) Execute(_ context.Context, execution ProbeExecution) (ProbeObservation, error) {
	e.calls++
	observation := ProbeObservation{
		SchemaVersion:  CurrentObservationSchemaVersion,
		Success:        true,
		Latency:        time.Duration(execution.Attempt) * time.Millisecond,
		ContextTokens:  execution.Case.ContextTokens,
		ToolCount:      execution.Case.ToolCount,
		SafetyPassed:   true,
		TraceDigest:    digestText(fmt.Sprintf("trace:%s:%d", execution.Case.ID, execution.Attempt)),
		ArtifactDigest: digestText(fmt.Sprintf("artifact:%s:%d", execution.Case.ID, execution.Attempt)),
	}
	if e.mutate != nil {
		return observation, e.mutate(execution, &observation)
	}
	return observation, nil
}

func testTarget(t *testing.T) TargetIdentity {
	t.Helper()
	target, err := NewTargetIdentity(TargetSpec{
		Provider: "test-provider",
		Model:    "small-model/v1",
		Endpoint: "https://private-endpoint.invalid/v1?tenant=hidden",
		APIKey:   "sk-never-persist-this-key",
		ServingParameters: map[string]any{
			"temperature": 0.2,
			"max_tokens":  1024,
			"metadata":    map[string]any{"tenant": "never-persist-this-parameter"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func testProbePlan(t *testing.T) ProbePlan {
	t.Helper()
	plan, err := NewProbePlan(ProbePlanSpec{
		Version: "test-plan-v1",
		Cases: []ProbeCase{
			probe("cal-native", StageCalibration, CategoryProtocolNative, 1, 2, 0, 8, false, false),
			probe("cal-context", StageCalibration, CategoryContextCeiling, 2, 2, 16_000, 8, false, false),
			probe("cal-tools", StageCalibration, CategoryToolCatalog, 3, 2, 0, 32, false, false),
			probe("cal-safety", StageCalibration, CategorySafetyBoundary, 4, 2, 0, 4, true, true),
			probe("holdout-native", StageHoldout, CategoryProtocolNative, 5, 2, 0, 8, false, false),
			probe("holdout-safety", StageHoldout, CategorySafetyBoundary, 6, 2, 0, 4, true, true),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func testProfiler(t *testing.T, factory *fakeWorkspaceFactory, executor *fakeExecutor, ttl time.Duration) *Profiler {
	t.Helper()
	profiler, err := NewProfiler(factory, executor, ProfilerConfig{
		ConfidenceThresholdBasisPoints: 8_000,
		MinimumObservations:            8,
		ProfileTTL:                     ttl,
		Clock:                          func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	return profiler
}

func TestWaterfallRecommendationRenameKeepsPersistedLegacyValid(t *testing.T) {
	recommendation := Recommendations{
		WirePolicy:       "auto",
		ResponsePolicy:   "native-with-text-recovery",
		EditPolicy:       corelayWaterfallEditPolicy,
		PlanAnchorMode:   "strict",
		MaxReliableTools: 1,
		RepeatLimit:      2,
		ReadBeforeWrite:  true,
	}
	if err := validateRecommendations(recommendation); err != nil {
		t.Fatalf("new Corelay recommendation rejected: %v", err)
	}
	recommendation.EditPolicy = legacyAniClewWaterfallEditPolicy
	if err := validateRecommendations(recommendation); err != nil {
		t.Fatalf("persisted AniClew recommendation rejected: %v", err)
	}
	if got := recommend(nil).EditPolicy; got != corelayWaterfallEditPolicy {
		t.Fatalf("new recommendation edit policy=%q want=%q", got, corelayWaterfallEditPolicy)
	}
}

func TestProfilerHappyPathIsVerifiedAndDeterministic(t *testing.T) {
	target := testTarget(t)
	plan := testProbePlan(t)

	firstExecutor := &fakeExecutor{}
	first, err := testProfiler(t, &fakeWorkspaceFactory{}, firstExecutor, 48*time.Hour).Run(context.Background(), target, plan)
	if err != nil {
		t.Fatal(err)
	}
	second, err := testProfiler(t, &fakeWorkspaceFactory{}, &fakeExecutor{}, 48*time.Hour).Run(context.Background(), target, plan)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Valid() || !first.Verified() {
		t.Fatalf("profile valid=%v verified=%v reasons=%v", first.Valid(), first.Verified(), first.QuarantineReasons())
	}
	if first.ID() != second.ID() {
		t.Fatalf("deterministic profile IDs differ: %s != %s", first.ID(), second.ID())
	}
	if firstExecutor.calls != plan.Attempts() {
		t.Fatalf("executor calls=%d want=%d", firstExecutor.calls, plan.Attempts())
	}
	snapshot := first.Snapshot()
	if snapshot.ConfidenceBasisPoints != 10_000 || snapshot.Metrics.Attempts != plan.Attempts() {
		t.Fatalf("unexpected score/metrics: confidence=%d metrics=%+v", snapshot.ConfidenceBasisPoints, snapshot.Metrics)
	}
	if snapshot.Recommendations.WirePolicy != "auto" || snapshot.Recommendations.ResponsePolicy != "native" {
		t.Fatalf("unexpected protocol recommendations: %+v", snapshot.Recommendations)
	}
	if snapshot.Recommendations.ReliableInputTokens != 14_400 {
		t.Fatalf("reliable input tokens=%d want=14400", snapshot.Recommendations.ReliableInputTokens)
	}
	if snapshot.Recommendations.MaxReliableTools != 32 {
		t.Fatalf("max reliable tools=%d want=32", snapshot.Recommendations.MaxReliableTools)
	}

	// Snapshot mutation cannot mutate the profile-owned observation slice.
	snapshot.Observations[0].Success = false
	if !first.Snapshot().Observations[0].Success {
		t.Fatal("profile observation changed through snapshot alias")
	}
}

func TestHoldoutDoesNotIncreaseConfidenceAndFailureQuarantines(t *testing.T) {
	executor := &fakeExecutor{mutate: func(execution ProbeExecution, observation *ProbeObservation) error {
		if execution.Case.Stage == StageHoldout {
			observation.Success = false
		}
		return nil
	}}
	profile, err := testProfiler(t, &fakeWorkspaceFactory{}, executor, 48*time.Hour).Run(context.Background(), testTarget(t), testProbePlan(t))
	if err != nil {
		t.Fatal(err)
	}
	if profile.Verified() || !hasReason(profile, QuarantineHoldoutFailure) {
		t.Fatalf("holdout failure did not quarantine profile: %+v", profile.Snapshot())
	}
	if profile.ConfidenceBasisPoints() != 10_000 {
		t.Fatalf("holdout leaked into confidence: got %d want calibration-only 10000", profile.ConfidenceBasisPoints())
	}
}

func TestSafetyFailureQuarantines(t *testing.T) {
	executor := &fakeExecutor{mutate: func(execution ProbeExecution, observation *ProbeObservation) error {
		if execution.Case.SafetyCritical {
			observation.SafetyPassed = false
		}
		return nil
	}}
	profile, err := testProfiler(t, &fakeWorkspaceFactory{}, executor, 48*time.Hour).Run(context.Background(), testTarget(t), testProbePlan(t))
	if err != nil {
		t.Fatal(err)
	}
	if profile.Verified() || !hasReason(profile, QuarantineSafetyFailure) {
		t.Fatalf("safety failure did not quarantine: %v", profile.QuarantineReasons())
	}
}

func TestMissingTraceAndSchemaMismatchQuarantine(t *testing.T) {
	executor := &fakeExecutor{mutate: func(execution ProbeExecution, observation *ProbeObservation) error {
		if execution.Case.ID == "cal-native" && execution.Attempt == 1 {
			observation.TraceDigest = ""
			observation.SchemaVersion = CurrentObservationSchemaVersion + 1
		}
		return nil
	}}
	profile, err := testProfiler(t, &fakeWorkspaceFactory{}, executor, 48*time.Hour).Run(context.Background(), testTarget(t), testProbePlan(t))
	if err != nil {
		t.Fatal(err)
	}
	if !profile.Valid() || profile.Verified() || !hasReason(profile, QuarantineTraceMissing) || !hasReason(profile, QuarantineSchemaMismatch) {
		t.Fatalf("missing trace/schema mismatch reasons=%v valid=%v", profile.QuarantineReasons(), profile.Valid())
	}
}

func TestLowConfidenceAndMetricsAreDeterministic(t *testing.T) {
	executor := &fakeExecutor{mutate: func(execution ProbeExecution, observation *ProbeObservation) error {
		if execution.Case.Stage == StageCalibration &&
			(execution.Case.Category == CategoryProtocolNative || execution.Case.Category == CategoryContextCeiling) {
			observation.Success = false
			observation.Malformed = true
			observation.FalseDone = true
			observation.Recovered = true
			observation.Retries = 2
		}
		return nil
	}}
	profile, err := testProfiler(t, &fakeWorkspaceFactory{}, executor, 48*time.Hour).Run(context.Background(), testTarget(t), testProbePlan(t))
	if err != nil {
		t.Fatal(err)
	}
	metrics := profile.Snapshot().Metrics
	if !hasReason(profile, QuarantineLowConfidence) || metrics.Malformed != 4 || metrics.FalseDone != 4 || metrics.Recoveries != 4 || metrics.Retries != 8 {
		t.Fatalf("low-confidence metrics/reasons mismatch: metrics=%+v reasons=%v", metrics, profile.QuarantineReasons())
	}
}

func TestProfileValidationRejectsUnboundedLifetimeAndRecommendations(t *testing.T) {
	profile, err := testProfiler(t, &fakeWorkspaceFactory{}, &fakeExecutor{}, 48*time.Hour).Run(context.Background(), testTarget(t), testProbePlan(t))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*ProfileSnapshot)
	}{
		{"ttl", func(snapshot *ProfileSnapshot) {
			snapshot.ExpiresAt = snapshot.CreatedAt.Add(maxProfileTTL + time.Second)
		}},
		{"tools", func(snapshot *ProfileSnapshot) { snapshot.Recommendations.MaxReliableTools = maxRecommendedTools + 1 }},
		{"tokens", func(snapshot *ProfileSnapshot) {
			snapshot.Recommendations.ReliableInputTokens = maxRecommendedInputTokens + 1
		}},
		{"repeat", func(snapshot *ProfileSnapshot) { snapshot.Recommendations.RepeatLimit = maxRecommendedRepeatLimit + 1 }},
		{"wire-enum", func(snapshot *ProfileSnapshot) { snapshot.Recommendations.WirePolicy = "native" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := profile.Snapshot()
			test.mutate(&snapshot)
			if _, err := profileFromSnapshot(snapshot, true); !errors.Is(err, ErrInvalidProfile) {
				t.Fatalf("profileFromSnapshot error=%v want invalid profile", err)
			}
		})
	}
}

func TestIsolationUnavailableCallsExecutorZero(t *testing.T) {
	factory := &fakeWorkspaceFactory{fail: true}
	executor := &fakeExecutor{}
	profile, err := testProfiler(t, factory, executor, 48*time.Hour).Run(context.Background(), testTarget(t), testProbePlan(t))
	if err != nil {
		t.Fatal(err)
	}
	if executor.calls != 0 {
		t.Fatalf("executor calls=%d want=0", executor.calls)
	}
	if !profile.Valid() || profile.Verified() || !hasReason(profile, QuarantineIsolation) {
		t.Fatalf("unexpected isolated-failure profile: %+v", profile.Snapshot())
	}
}

func TestInvalidProofFailsClosedAndRecordsCleanupFailure(t *testing.T) {
	factory := &fakeWorkspaceFactory{badProof: true, closeErr: true}
	executor := &fakeExecutor{}
	profile, err := testProfiler(t, factory, executor, 48*time.Hour).Run(context.Background(), testTarget(t), testProbePlan(t))
	if err != nil {
		t.Fatal(err)
	}
	if executor.calls != 0 || !hasReason(profile, QuarantineIsolation) || !hasReason(profile, QuarantineCleanup) {
		t.Fatalf("invalid proof was not fail-closed: calls=%d reasons=%v", executor.calls, profile.QuarantineReasons())
	}
}

func TestNoRawEndpointKeyParameterOrExecutorErrorInJSON(t *testing.T) {
	rawError := "provider-error-never-persist-this"
	executor := &fakeExecutor{mutate: func(_ ProbeExecution, _ *ProbeObservation) error { return errors.New(rawError) }}
	profile, err := testProfiler(t, &fakeWorkspaceFactory{}, executor, 48*time.Hour).Run(context.Background(), testTarget(t), testProbePlan(t))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(profile.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	serialized := string(encoded)
	for _, forbidden := range []string{
		"https://private-endpoint.invalid", "tenant=hidden", "sk-never-persist-this-key",
		"never-persist-this-parameter", rawError, "isolated/",
	} {
		if strings.Contains(serialized, forbidden) {
			t.Fatalf("serialized profile leaked %q: %s", forbidden, serialized)
		}
	}
}

func hasReason(profile CapabilityProfile, wanted QuarantineReason) bool {
	for _, reason := range profile.QuarantineReasons() {
		if reason == wanted {
			return true
		}
	}
	return false
}

func digestText(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
