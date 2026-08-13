package capabilityprofile

import "errors"

var (
	ErrInvalidTarget        = errors.New("invalid capability-profile target")
	ErrInvalidPlan          = errors.New("invalid capability probe plan")
	ErrInvalidProfile       = errors.New("invalid capability profile")
	ErrSchemaMismatch       = errors.New("capability-profile schema mismatch")
	ErrTargetMismatch       = errors.New("capability-profile target mismatch")
	ErrProfileConflict      = errors.New("capability profile already exists")
	ErrProfileNotFound      = errors.New("capability profile not found")
	ErrNoSelectableProfile  = errors.New("no automatically selectable capability profile")
	ErrManualOverride       = errors.New("invalid capability-profile manual override")
	ErrUnsafeStorePath      = errors.New("unsafe capability-profile store path")
	ErrIsolationUnavailable = errors.New("capability-profile isolation is unavailable")
	ErrInvalidRuntime       = errors.New("invalid capability-profile runtime")
	ErrIncompatibleProfiles = errors.New("incompatible capability profiles")
)

// QuarantineReason is a stable, machine-readable reason that prevents a
// candidate profile from being applied automatically.
type QuarantineReason string

const (
	QuarantineSchemaMismatch  QuarantineReason = "schema_mismatch"
	QuarantineTargetChanged   QuarantineReason = "target_changed"
	QuarantineTraceMissing    QuarantineReason = "trace_missing"
	QuarantineArtifactMissing QuarantineReason = "artifact_missing"
	QuarantineLowConfidence   QuarantineReason = "low_confidence"
	QuarantineSafetyFailure   QuarantineReason = "safety_failure"
	QuarantineHoldoutFailure  QuarantineReason = "holdout_regression"
	QuarantineIsolation       QuarantineReason = "isolation_unavailable"
	QuarantineCleanup         QuarantineReason = "isolation_cleanup_failed"
	QuarantineExpired         QuarantineReason = "expired"
	QuarantineManualOnly      QuarantineReason = "manual_override_only"
	QuarantineCorrupt         QuarantineReason = "corrupt_profile"
)

func (r QuarantineReason) valid() bool {
	switch r {
	case QuarantineSchemaMismatch,
		QuarantineTargetChanged,
		QuarantineTraceMissing,
		QuarantineArtifactMissing,
		QuarantineLowConfidence,
		QuarantineSafetyFailure,
		QuarantineHoldoutFailure,
		QuarantineIsolation,
		QuarantineCleanup,
		QuarantineExpired,
		QuarantineManualOnly,
		QuarantineCorrupt:
		return true
	default:
		return false
	}
}
