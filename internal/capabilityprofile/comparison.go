package capabilityprofile

import (
	"fmt"
	"sort"
)

const CurrentComparisonSchemaVersion = 1

type ComparisonVerdict string

const (
	VerdictCandidate        ComparisonVerdict = "candidate"
	VerdictBaseline         ComparisonVerdict = "baseline"
	VerdictTie              ComparisonVerdict = "tie"
	VerdictUnsafeRegression ComparisonVerdict = "unsafe-regression"
)

type ComparisonProfileSummary struct {
	ProfileID             string           `json:"profileId"`
	Variant               HarnessVariant   `json:"variant"`
	PlanDigest            string           `json:"planDigest"`
	Verified              bool             `json:"verified"`
	ConfidenceBasisPoints int              `json:"confidenceBasisPoints"`
	Metrics               AggregateMetrics `json:"metrics"`
	SafetyFailures        int              `json:"safetyFailures"`
}

// ComparisonDelta always means candidate minus baseline. Negative values are
// improvements for malformed, retries, latency, false-done, transport, and
// safety-failure fields.
type ComparisonDelta struct {
	ConfidenceBasisPoints int   `json:"confidenceBasisPoints"`
	Successes             int   `json:"successes"`
	Malformed             int   `json:"malformed"`
	Retries               int   `json:"retries"`
	LatencyMillisTotal    int64 `json:"latencyMillisTotal"`
	FalseDone             int   `json:"falseDone"`
	Recoveries            int   `json:"recoveries"`
	TransportFailures     int   `json:"transportFailures"`
	SafetyFailures        int   `json:"safetyFailures"`
}

type CategoryComparison struct {
	Category           ProbeCategory   `json:"category"`
	Attempts           int             `json:"attempts"`
	BaselineSuccesses  int             `json:"baselineSuccesses"`
	CandidateSuccesses int             `json:"candidateSuccesses"`
	Delta              ComparisonDelta `json:"delta"`
}

// ComparisonReport is content-free: it contains only immutable identifiers,
// bounded counters, and typed verdicts derived from validated profiles.
type ComparisonReport struct {
	SchemaVersion    int                      `json:"schemaVersion"`
	TargetDigest     string                   `json:"targetDigest"`
	PlanVersion      string                   `json:"planVersion"`
	FixtureDigest    string                   `json:"fixtureDigest"`
	Baseline         ComparisonProfileSummary `json:"baseline"`
	Candidate        ComparisonProfileSummary `json:"candidate"`
	Delta            ComparisonDelta          `json:"delta"`
	Categories       []CategoryComparison     `json:"categories"`
	SafetyRegression bool                     `json:"safetyRegression"`
	Verdict          ComparisonVerdict        `json:"verdict"`
}

type observationShape struct {
	Stage            ProbeStage
	Category         ProbeCategory
	Attempt          int
	SafetyCritical   bool
	ArtifactRequired bool
}

func CompareProfiles(baseline, candidate CapabilityProfile) (ComparisonReport, error) {
	if !baseline.Valid() || !candidate.Valid() {
		return ComparisonReport{}, ErrInvalidProfile
	}
	left, right := baseline.Snapshot(), candidate.Snapshot()
	leftVariant, rightVariant := profileVariant(left), profileVariant(right)
	if left.Target.TargetDigest != right.Target.TargetDigest ||
		left.Provenance.PlanVersion != right.Provenance.PlanVersion ||
		!validDigest(left.Provenance.FixtureDigest) || left.Provenance.FixtureDigest != right.Provenance.FixtureDigest ||
		left.Provenance.ExpectedAttempts != right.Provenance.ExpectedAttempts ||
		leftVariant == rightVariant {
		return ComparisonReport{}, ErrIncompatibleProfiles
	}

	leftByKey, err := comparisonObservationMap(left.Observations)
	if err != nil {
		return ComparisonReport{}, err
	}
	rightByKey, err := comparisonObservationMap(right.Observations)
	if err != nil || len(leftByKey) != len(rightByKey) {
		return ComparisonReport{}, ErrIncompatibleProfiles
	}
	for key, leftObservation := range leftByKey {
		rightObservation, ok := rightByKey[key]
		if !ok || observationShapeFor(leftObservation) != observationShapeFor(rightObservation) {
			return ComparisonReport{}, ErrIncompatibleProfiles
		}
	}

	leftSafetyFailures := safetyFailureCount(left.Observations)
	rightSafetyFailures := safetyFailureCount(right.Observations)
	report := ComparisonReport{
		SchemaVersion:    CurrentComparisonSchemaVersion,
		TargetDigest:     left.Target.TargetDigest,
		PlanVersion:      left.Provenance.PlanVersion,
		FixtureDigest:    left.Provenance.FixtureDigest,
		Baseline:         comparisonSummary(left, leftVariant, leftSafetyFailures),
		Candidate:        comparisonSummary(right, rightVariant, rightSafetyFailures),
		Delta:            comparisonDelta(left, right, leftSafetyFailures, rightSafetyFailures),
		Categories:       compareCategories(left.Observations, right.Observations),
		SafetyRegression: safetyRegressed(leftByKey, rightByKey),
	}
	switch {
	case report.SafetyRegression:
		report.Verdict = VerdictUnsafeRegression
	case right.Verified != left.Verified:
		if right.Verified {
			report.Verdict = VerdictCandidate
		} else {
			report.Verdict = VerdictBaseline
		}
	case right.ConfidenceBasisPoints > left.ConfidenceBasisPoints:
		report.Verdict = VerdictCandidate
	case right.ConfidenceBasisPoints < left.ConfidenceBasisPoints:
		report.Verdict = VerdictBaseline
	default:
		report.Verdict = metricVerdict(left.Metrics, right.Metrics)
	}
	return report, nil
}

func safetyRegressed(left, right map[string]ObservationRecord) bool {
	for key, baseline := range left {
		if !baseline.SafetyCritical || !cleanSuccess(baseline) || !baseline.SafetyPassed {
			continue
		}
		candidate := right[key]
		if !cleanSuccess(candidate) || !candidate.SafetyPassed {
			return true
		}
	}
	return false
}

func metricVerdict(left, right AggregateMetrics) ComparisonVerdict {
	if right.Successes != left.Successes {
		if right.Successes > left.Successes {
			return VerdictCandidate
		}
		return VerdictBaseline
	}
	leftFailures := left.Malformed + left.FalseDone + left.TransportFailures
	rightFailures := right.Malformed + right.FalseDone + right.TransportFailures
	if rightFailures != leftFailures {
		if rightFailures < leftFailures {
			return VerdictCandidate
		}
		return VerdictBaseline
	}
	if right.Retries != left.Retries {
		if right.Retries < left.Retries {
			return VerdictCandidate
		}
		return VerdictBaseline
	}
	return VerdictTie
}

func profileVariant(snapshot ProfileSnapshot) HarnessVariant {
	if snapshot.Provenance.Variant == "" {
		return HarnessVariantCorelay
	}
	return snapshot.Provenance.Variant
}

func comparisonSummary(snapshot ProfileSnapshot, variant HarnessVariant, safetyFailures int) ComparisonProfileSummary {
	return ComparisonProfileSummary{
		ProfileID:             snapshot.ProfileID,
		Variant:               variant,
		PlanDigest:            snapshot.Provenance.PlanDigest,
		Verified:              snapshot.Verified,
		ConfidenceBasisPoints: snapshot.ConfidenceBasisPoints,
		Metrics:               snapshot.Metrics,
		SafetyFailures:        safetyFailures,
	}
}

func comparisonDelta(left, right ProfileSnapshot, leftSafety, rightSafety int) ComparisonDelta {
	return metricDelta(left.Metrics, right.Metrics, leftSafety, rightSafety, right.ConfidenceBasisPoints-left.ConfidenceBasisPoints)
}

func metricDelta(left, right AggregateMetrics, leftSafety, rightSafety, confidenceDelta int) ComparisonDelta {
	return ComparisonDelta{
		ConfidenceBasisPoints: confidenceDelta,
		Successes:             right.Successes - left.Successes,
		Malformed:             right.Malformed - left.Malformed,
		Retries:               right.Retries - left.Retries,
		LatencyMillisTotal:    right.LatencyMillisTotal - left.LatencyMillisTotal,
		FalseDone:             right.FalseDone - left.FalseDone,
		Recoveries:            right.Recoveries - left.Recoveries,
		TransportFailures:     right.TransportFailures - left.TransportFailures,
		SafetyFailures:        rightSafety - leftSafety,
	}
}

func comparisonObservationMap(observations []ObservationRecord) (map[string]ObservationRecord, error) {
	result := make(map[string]ObservationRecord, len(observations))
	for _, observation := range observations {
		key := fmt.Sprintf("%s\x00%d", observation.CaseID, observation.Attempt)
		if _, exists := result[key]; exists {
			return nil, ErrIncompatibleProfiles
		}
		result[key] = observation
	}
	return result, nil
}

func observationShapeFor(observation ObservationRecord) observationShape {
	return observationShape{
		Stage:            observation.Stage,
		Category:         observation.Category,
		Attempt:          observation.Attempt,
		SafetyCritical:   observation.SafetyCritical,
		ArtifactRequired: observation.ArtifactRequired,
	}
}

func safetyFailureCount(observations []ObservationRecord) int {
	failures := 0
	for _, observation := range observations {
		if observation.SafetyCritical && (!cleanSuccess(observation) || !observation.SafetyPassed) {
			failures++
		}
	}
	return failures
}

func compareCategories(left, right []ObservationRecord) []CategoryComparison {
	leftByCategory := observationsByCategory(left)
	rightByCategory := observationsByCategory(right)
	categories := make([]ProbeCategory, 0, len(leftByCategory))
	for category := range leftByCategory {
		categories = append(categories, category)
	}
	sort.Slice(categories, func(i, j int) bool { return categories[i] < categories[j] })
	result := make([]CategoryComparison, 0, len(categories))
	for _, category := range categories {
		leftObservations, rightObservations := leftByCategory[category], rightByCategory[category]
		leftMetrics, rightMetrics := metricsFor(leftObservations), metricsFor(rightObservations)
		leftSafety, rightSafety := safetyFailureCount(leftObservations), safetyFailureCount(rightObservations)
		result = append(result, CategoryComparison{
			Category:           category,
			Attempts:           leftMetrics.Attempts,
			BaselineSuccesses:  leftMetrics.Successes,
			CandidateSuccesses: rightMetrics.Successes,
			Delta:              metricDelta(leftMetrics, rightMetrics, leftSafety, rightSafety, 0),
		})
	}
	return result
}

func observationsByCategory(observations []ObservationRecord) map[ProbeCategory][]ObservationRecord {
	result := make(map[ProbeCategory][]ObservationRecord)
	for _, observation := range observations {
		result[observation.Category] = append(result[observation.Category], observation)
	}
	return result
}
