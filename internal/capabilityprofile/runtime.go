package capabilityprofile

import (
	"context"
	"fmt"
)

// RunManifest is a bounded, content-free description of the exact probe plan
// that a Runner will execute. It is safe to display before an explicit run.
type RunManifest struct {
	TargetDigest        string          `json:"targetDigest"`
	PlanVersion         string          `json:"planVersion"`
	PlanDigest          string          `json:"planDigest"`
	FixtureDigest       string          `json:"fixtureDigest"`
	Variant             HarnessVariant  `json:"variant"`
	Attempts            int             `json:"attempts"`
	CalibrationAttempts int             `json:"calibrationAttempts"`
	HoldoutAttempts     int             `json:"holdoutAttempts"`
	SafetyAttempts      int             `json:"safetyAttempts"`
	Categories          []ProbeCategory `json:"categories"`
}

// RunResult binds the immutable profile produced by Profiler to the exact
// content-addressed location published by Store.
type RunResult struct {
	Profile CapabilityProfile
	Ref     ProfileRef
}

// Runner is the application-level plan -> holdout -> immutable-store
// composition. Probe execution and isolation remain injected contracts; this
// type deliberately does not invent a second profiler or scoring path.
type Runner struct {
	profiler *Profiler
	store    *Store
	plan     ProbePlan
}

func NewRunner(profiler *Profiler, store *Store, plan ProbePlan) (*Runner, error) {
	if profiler == nil || profiler.workspaces == nil || profiler.executor == nil ||
		store == nil || store.Root() == "" || !plan.Valid() {
		return nil, ErrInvalidRuntime
	}
	if !calibrationPrecedesHoldout(plan) {
		return nil, fmt.Errorf("%w: holdout cases must follow calibration cases", ErrInvalidRuntime)
	}
	return &Runner{profiler: profiler, store: store, plan: plan}, nil
}

// Manifest returns only target and plan digests plus bounded attempt counts.
// It never contains fixture prompts, endpoints, credentials, or observations.
func (r *Runner) Manifest(target TargetIdentity) (RunManifest, error) {
	if r == nil || r.profiler == nil || r.store == nil || !r.plan.Valid() {
		return RunManifest{}, ErrInvalidRuntime
	}
	return DescribeRun(target, r.plan)
}

// DescribeRun builds a safe preview without requiring provider, executor,
// workspace, or store composition. It cannot execute or persist anything.
func DescribeRun(target TargetIdentity, plan ProbePlan) (RunManifest, error) {
	if !target.Valid() {
		return RunManifest{}, ErrInvalidTarget
	}
	if !plan.Valid() || !calibrationPrecedesHoldout(plan) {
		return RunManifest{}, ErrInvalidPlan
	}
	manifest := RunManifest{
		TargetDigest:  target.Digest(),
		PlanVersion:   plan.Version(),
		PlanDigest:    plan.Digest(),
		FixtureDigest: plan.FixtureDigest(),
		Variant:       plan.Variant(),
		Attempts:      plan.Attempts(),
		Categories:    plan.SortedCategories(),
	}
	for _, probeCase := range plan.Cases() {
		attempts := probeCase.Repeats
		if probeCase.Stage == StageHoldout {
			manifest.HoldoutAttempts += attempts
		} else {
			manifest.CalibrationAttempts += attempts
		}
		if probeCase.SafetyCritical {
			manifest.SafetyAttempts += attempts
		}
	}
	return manifest, nil
}

// Run profiles the exact target with the runner's immutable plan and publishes
// the resulting verified or quarantined profile exactly once. Cancellation or
// profiler validation errors are returned before Store.Save is called.
func (r *Runner) Run(ctx context.Context, target TargetIdentity) (RunResult, error) {
	if r == nil || r.profiler == nil || r.store == nil || !r.plan.Valid() {
		return RunResult{}, ErrInvalidRuntime
	}
	if !target.Valid() {
		return RunResult{}, ErrInvalidTarget
	}
	profile, err := r.profiler.Run(ctx, target, r.plan)
	if err != nil {
		return RunResult{}, err
	}
	ref, err := r.store.Save(profile)
	if err != nil {
		return RunResult{}, err
	}
	return RunResult{Profile: profile, Ref: ref}, nil
}

func calibrationPrecedesHoldout(plan ProbePlan) bool {
	seenHoldout := false
	for _, probeCase := range plan.Cases() {
		if probeCase.Stage == StageHoldout {
			seenHoldout = true
			continue
		}
		if seenHoldout {
			return false
		}
	}
	return seenHoldout
}
