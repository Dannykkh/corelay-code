package capabilityprofile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ImprovementResult records a measured profile refresh, not a claim that the
// model or source code has trained itself. Rejected candidates stay off the
// automatic-selection path. Only bounded, content-free evidence is persisted.
type ImprovementResult struct {
	SourceProfileID       string           `json:"sourceProfileId,omitempty"`
	CandidateLessons      LessonPolicy     `json:"candidateLessons,omitempty"`
	CandidateLessonDigest string           `json:"candidateLessonDigest,omitempty"`
	Control               *ProfileRef      `json:"control,omitempty"`
	SchemaVersion         int              `json:"schemaVersion"`
	Decision              string           `json:"decision"`
	Reasons               []string         `json:"reasons"`
	Comparison            ComparisonReport `json:"comparison"`
	Trial                 ProfileRef       `json:"trial"`
	Published             *ProfileRef      `json:"published,omitempty"`
}

func EvaluateImprovement(baseline, candidate CapabilityProfile, now time.Time) (ImprovementResult, error) {
	return evaluateImprovement(baseline, candidate, now, false)
}

func evaluateImprovement(baseline, candidate CapabilityProfile, now time.Time, lessonExperiment bool) (ImprovementResult, error) {
	comparison, err := compareRevisions(baseline, candidate, lessonExperiment)
	if err != nil {
		return ImprovementResult{}, err
	}
	result := ImprovementResult{SchemaVersion: 1, Decision: "rejected", Reasons: []string{}, Comparison: comparison}
	left, right := baseline.Snapshot(), candidate.Snapshot()
	if !right.Verified || len(right.QuarantineReasons) != 0 {
		result.Reasons = append(result.Reasons, "candidate-unverified")
	}
	if !right.CreatedAt.After(left.CreatedAt) || right.CreatedAt.After(now) || !right.ExpiresAt.After(now) {
		result.Reasons = append(result.Reasons, "candidate-time-invalid")
	}
	if right.Metrics.FalseDone != 0 || comparison.Candidate.SafetyFailures != 0 {
		result.Reasons = append(result.Reasons, "candidate-safety-or-false-done")
	}
	previous, _ := comparisonObservationMap(left.Observations)
	current, _ := comparisonObservationMap(right.Observations)
	for key, before := range previous {
		after := current[key]
		if (cleanSuccess(before) && !cleanSuccess(after)) || after.Retries > before.Retries ||
			(!before.Malformed && after.Malformed) || (!before.TransportFailure && after.TransportFailure) {
			result.Reasons = append(result.Reasons, "per-attempt-regression")
			break
		}
	}
	if comparison.Verdict != VerdictCandidate {
		result.Reasons = append(result.Reasons, "no-measured-improvement")
	}
	if len(result.Reasons) == 0 {
		result.Decision = "accepted"
	}
	return result, nil
}

// Improve reuses the existing isolated profiler. It never calls Runner.Run,
// because Run publishes before a comparison can reject a candidate.
func (r *Runner) Improve(ctx context.Context, target TargetIdentity, baselineID string) (ImprovementResult, error) {
	return r.improve(ctx, target, baselineID, false)
}

// Learn proposes code-owned reminders from calibration failures, freezes them,
// then runs control and candidate independently through the isolated profiler.
func (r *Runner) Learn(ctx context.Context, target TargetIdentity, baselineID string) (ImprovementResult, error) {
	return r.improve(ctx, target, baselineID, true)
}

func (r *Runner) improve(ctx context.Context, target TargetIdentity, baselineID string, learn bool) (ImprovementResult, error) {
	if r == nil || r.profiler == nil || r.store == nil || !r.plan.Valid() || !target.Valid() {
		return ImprovementResult{}, ErrInvalidRuntime
	}
	release, err := r.store.lockProfiling(target)
	if err != nil {
		return ImprovementResult{}, err
	}
	defer release()
	baseline, err := r.store.Load(target, baselineID)
	if err != nil {
		return ImprovementResult{}, err
	}
	snapshot := baseline.Snapshot()
	if snapshot.Provenance.PlanDigest != r.plan.Digest() ||
		snapshot.Provenance.ProfilerVersion != ProfilerImplementationVersion ||
		snapshot.Provenance.Scoring.ConfidenceThresholdBasisPoints != r.profiler.config.ConfidenceThresholdBasisPoints ||
		snapshot.Provenance.Scoring.MinimumObservations != r.profiler.config.MinimumObservations ||
		len(snapshot.ManualOverrides) != 0 {
		return ImprovementResult{}, ErrIncompatibleProfiles
	}
	// Never improve an obsolete baseline while another profile is already active.
	if selected, selectErr := r.store.AutoSelect(target, r.profiler.config.Clock()); selectErr == nil {
		if selected.ID() != baselineID {
			return ImprovementResult{}, fmt.Errorf("baseline is not the currently selected profile")
		}
	} else if !errors.Is(selectErr, ErrNoSelectableProfile) {
		return ImprovementResult{}, selectErr
	}
	policy := snapshot.Provenance.Lessons
	if learn {
		policy, err = ProposeLessons(baseline)
		if err != nil {
			return ImprovementResult{}, err
		}
		if policy == snapshot.Provenance.Lessons {
			return ImprovementResult{SchemaVersion: 1, SourceProfileID: baselineID, Decision: "rejected", Reasons: []string{"no-new-calibration-lesson"}}, nil
		}
		baseline, err = r.profiler.RunWithLessons(ctx, target, r.plan, snapshot.Provenance.Lessons)
		if err != nil {
			return ImprovementResult{}, err
		}
	}
	var candidate CapabilityProfile
	if learn || snapshot.Provenance.LessonDigest != "" {
		candidate, err = r.profiler.RunWithLessons(ctx, target, r.plan, policy)
	} else {
		candidate, err = r.profiler.Run(ctx, target, r.plan)
	}
	if err != nil {
		return ImprovementResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return ImprovementResult{}, err
	}
	result, err := evaluateImprovement(baseline, candidate, r.profiler.config.Clock(), learn)
	if err != nil {
		return ImprovementResult{}, err
	}
	result.SourceProfileID = baselineID
	result.CandidateLessons = policy
	result.CandidateLessonDigest = candidate.Snapshot().Provenance.LessonDigest
	trials, err := NewStore(filepath.Join(r.store.Root(), "rsi-trials"))
	if err != nil {
		return ImprovementResult{}, err
	}
	if learn {
		ref, err := trials.Save(baseline)
		if err != nil {
			return ImprovementResult{}, err
		}
		result.Control = &ref
	}
	result.Trial, err = trials.Save(candidate)
	if err != nil {
		return ImprovementResult{}, err
	}
	// Persist the decision before publication. A crash can leave an accepted but
	// unpublished trial; only the main store establishes effective publication.
	if err := persistImprovementDecision(trials, result); err != nil {
		return ImprovementResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return ImprovementResult{}, err
	}
	if result.Decision == "accepted" {
		ref, err := r.store.Save(candidate)
		if err != nil {
			return ImprovementResult{}, err
		}
		result.Published = &ref
	}
	return result, nil
}

func persistImprovementDecision(trials *Store, result ImprovementResult) error {
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(trials.Root(), ".decision-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	name := result.Comparison.Baseline.ProfileID + "-" + result.Comparison.Candidate.ProfileID + ".json"
	return os.Link(file.Name(), filepath.Join(trials.Root(), name))
}

// The lock is intentionally not stolen on timeout. After a killed profiler,
// an operator can remove it only after confirming that no run is still active.
func (s *Store) lockProfiling(target TargetIdentity) (func(), error) {
	if s == nil || !target.Valid() {
		return nil, ErrInvalidRuntime
	}
	path := filepath.Join(s.Root(), ".profiling-"+target.Digest()+".lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("profile target is locked or unavailable")
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	return func() { _ = os.Remove(path) }, nil
}
