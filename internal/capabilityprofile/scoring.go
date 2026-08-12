package capabilityprofile

import "sort"

func metricsFor(observations []ObservationRecord) AggregateMetrics {
	var metrics AggregateMetrics
	for _, observation := range observations {
		metrics.Attempts++
		if observation.Success {
			metrics.Successes++
		}
		if observation.Malformed {
			metrics.Malformed++
		}
		metrics.Retries += observation.Retries
		metrics.LatencyMillisTotal += observation.LatencyMillis
		if observation.LatencyMillis > metrics.LatencyMillisMax {
			metrics.LatencyMillisMax = observation.LatencyMillis
		}
		if observation.ContextTokens > metrics.ContextTokensMax {
			metrics.ContextTokensMax = observation.ContextTokens
		}
		if observation.ToolCount > metrics.ToolCountMax {
			metrics.ToolCountMax = observation.ToolCount
		}
		if observation.FalseDone {
			metrics.FalseDone++
		}
		if observation.Recovered {
			metrics.Recoveries++
		}
		if observation.TransportFailure {
			metrics.TransportFailures++
		}
	}
	return metrics
}

func scoreConfidence(observations []ObservationRecord, minimumObservations int) int {
	if len(observations) == 0 || minimumObservations <= 0 {
		return 0
	}
	metrics := metricsFor(observations)
	attempts := metrics.Attempts
	rate := func(good int) int { return good * 10_000 / attempts }
	successRate := rate(metrics.Successes)
	wellFormedRate := rate(attempts - metrics.Malformed)
	truthfulRate := rate(attempts - metrics.FalseDone)
	traceCount := 0
	for _, observation := range observations {
		if validDigest(observation.TraceDigest) {
			traceCount++
		}
	}
	traceRate := rate(traceCount)
	retryPenalty := metrics.Retries * 1_000 / attempts
	if retryPenalty > 10_000 {
		retryPenalty = 10_000
	}
	recoveryPenalty := rate(metrics.Recoveries)

	base := (successRate*45 + wellFormedRate*20 + truthfulRate*15 + traceRate*10 + (10_000-retryPenalty)*5 + (10_000-recoveryPenalty)*5) / 100
	sampleFactor := 10_000
	if attempts < minimumObservations {
		sampleFactor = attempts * 10_000 / minimumObservations
	}
	base = base * sampleFactor / 10_000

	// Penalize case-to-case instability without using floating point. A model
	// that succeeds on only a subset of fixtures must not look as trustworthy
	// as one with the same mean spread uniformly across the plan.
	type caseRate struct{ successes, attempts int }
	byCase := make(map[string]caseRate)
	for _, observation := range observations {
		value := byCase[observation.CaseID]
		value.attempts++
		if observation.Success && !observation.Malformed && !observation.FalseDone {
			value.successes++
		}
		byCase[observation.CaseID] = value
	}
	rates := make([]int, 0, len(byCase))
	for _, value := range byCase {
		rates = append(rates, value.successes*10_000/value.attempts)
	}
	sort.Ints(rates)
	mean := 0
	for _, value := range rates {
		mean += value
	}
	mean /= len(rates)
	deviation := 0
	for _, value := range rates {
		if value > mean {
			deviation += value - mean
		} else {
			deviation += mean - value
		}
	}
	deviation /= len(rates)
	base -= deviation / 4
	if base < 0 {
		return 0
	}
	if base > 10_000 {
		return 10_000
	}
	return base
}

func calibrationObservations(observations []ObservationRecord) []ObservationRecord {
	calibration := make([]ObservationRecord, 0, len(observations))
	for _, observation := range observations {
		if observation.Stage == StageCalibration {
			calibration = append(calibration, observation)
		}
	}
	return calibration
}

func holdoutPassed(observations []ObservationRecord) bool {
	seen := false
	for _, observation := range observations {
		if observation.Stage != StageHoldout {
			continue
		}
		seen = true
		if !cleanSuccess(observation) {
			return false
		}
	}
	return seen
}

func safetyPassed(observations []ObservationRecord) bool {
	seen := false
	for _, observation := range observations {
		if !observation.SafetyCritical {
			continue
		}
		seen = true
		if !cleanSuccess(observation) || !observation.SafetyPassed {
			return false
		}
	}
	return seen
}

func cleanSuccess(observation ObservationRecord) bool {
	return observation.SchemaVersion == CurrentObservationSchemaVersion &&
		observation.ObservedSchemaVersion == CurrentObservationSchemaVersion &&
		observation.Success && !observation.Malformed && !observation.FalseDone &&
		!observation.TransportFailure && validDigest(observation.TraceDigest) &&
		(!observation.ArtifactRequired || validDigest(observation.ArtifactDigest))
}

func recommend(observations []ObservationRecord) Recommendations {
	recommendation := Recommendations{
		WirePolicy:          "auto",
		ResponsePolicy:      "native-with-text-recovery",
		EditPolicy:          corelayWaterfallEditPolicy,
		PlanAnchorMode:      "strict",
		MaxReliableTools:    1,
		ReliableInputTokens: 4_096,
		RepeatLimit:         2,
		ReadBeforeWrite:     true,
	}

	if categoryStable(observations, CategoryProtocolNative) {
		recommendation.ResponsePolicy = "native"
	} else if categoryStable(observations, CategoryFormatHermes) ||
		categoryStable(observations, CategoryFormatLiquid) ||
		categoryStable(observations, CategoryFormatFencedJSON) ||
		categoryStable(observations, CategoryFormatBareJSON) {
		recommendation.ResponsePolicy = "multi-format"
	}

	for _, observation := range observations {
		if observation.Stage != StageCalibration || !cleanSuccess(observation) {
			continue
		}
		if observation.Category == CategoryToolCatalog && caseStable(observations, observation.CaseID) && observation.ToolCount > recommendation.MaxReliableTools {
			recommendation.MaxReliableTools = observation.ToolCount
		}
		if observation.Category == CategoryContextCeiling && caseStable(observations, observation.CaseID) {
			// Reserve ten percent rather than advertising the empirical cliff.
			window := observation.ContextTokens * 9 / 10
			if window > recommendation.ReliableInputTokens {
				recommendation.ReliableInputTokens = window
			}
		}
	}
	if categoryStable(observations, CategoryEditPatch) {
		recommendation.EditPolicy = "patch-first"
	} else if categoryStable(observations, CategoryEditExact) {
		recommendation.EditPolicy = "exact"
	} else if categoryStable(observations, CategoryEditFuzzy) {
		recommendation.EditPolicy = corelayWaterfallEditPolicy
	}
	recommendation.UseTwoStageRouting = categoryStable(observations, CategoryTwoStageRouting)
	if categoryStable(observations, CategoryRepetition) {
		recommendation.RepeatLimit = 3
	}
	if categoryStable(observations, CategoryPlanAnchor) {
		recommendation.PlanAnchorMode = "compact"
	}
	return recommendation
}

func categoryStable(observations []ObservationRecord, category ProbeCategory) bool {
	seen := false
	for _, observation := range observations {
		if observation.Stage != StageCalibration || observation.Category != category {
			continue
		}
		seen = true
		if !cleanSuccess(observation) {
			return false
		}
	}
	return seen
}

func caseStable(observations []ObservationRecord, caseID string) bool {
	seen := false
	for _, observation := range observations {
		if observation.CaseID != caseID {
			continue
		}
		seen = true
		if !cleanSuccess(observation) {
			return false
		}
	}
	return seen
}
