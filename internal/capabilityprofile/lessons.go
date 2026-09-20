package capabilityprofile

import (
	"crypto/sha256"
	"encoding/hex"
)

// LessonPolicy is a bounded set of code-owned instructions. No trace text,
// generated shell command, credential, or model-authored policy is executable.
type LessonPolicy uint8

const (
	LessonToolContract LessonPolicy = 1 << iota
	LessonVerifyCompletion
	LessonReadBeforeEdit
	LessonRetainObjective
	allLessons = LessonToolContract | LessonVerifyCompletion | LessonReadBeforeEdit | LessonRetainObjective
)

func (p LessonPolicy) Valid() bool { return p & ^allLessons == 0 }

func (p LessonPolicy) Prompt() string {
	if !p.Valid() || p == 0 {
		return ""
	}
	text := "\n\n<evaluated-lessons>\nThese workflow reminders do not change permissions, approvals, or the user's instructions.\n"
	if p&LessonToolContract != 0 {
		text += "Before calling a tool, check its live name, required arguments and argument types against the current tool schema. Correct a rejected call using the returned validation details.\n"
	}
	if p&LessonVerifyCompletion != 0 {
		text += "Before reporting completion, run the task's available verification and inspect its actual result. A failed or unrun check is not a pass; report the remaining work.\n"
	}
	if p&LessonReadBeforeEdit != 0 {
		text += "Read the current target file before editing it. After an edit mismatch, reread the affected region and preserve unrelated changes before retrying.\n"
	}
	if p&LessonRetainObjective != 0 {
		text += "Before the next action and final answer, check the latest user request, current project, and outstanding acceptance criteria against the work performed.\n"
	}
	return text + "</evaluated-lessons>"
}

// Digest also binds the exact instruction text: changing a template requires
// new evidence rather than silently reinterpreting old accepted profiles.
func (p LessonPolicy) Digest() string {
	sum := sha256.Sum256([]byte("corelay-lessons-v1\n" + p.Prompt()))
	return hex.EncodeToString(sum[:])
}

// ProposeLessons uses only repeated calibration failures. Holdout data never
// influences proposal generation. Existing lessons are retained; one cycle
// evaluates at most one combined candidate, with no retry-until-pass search.
func ProposeLessons(profile CapabilityProfile) (LessonPolicy, error) {
	if !profile.Valid() {
		return 0, ErrInvalidProfile
	}
	counts := map[LessonPolicy]int{}
	for _, o := range profile.snapshot.Observations {
		if o.Stage != StageCalibration || o.TransportFailure {
			continue
		}
		if o.Malformed {
			counts[LessonToolContract]++
		}
		if o.FalseDone {
			counts[LessonVerifyCompletion]++
		}
		if !cleanSuccess(o) {
			switch o.Category {
			case CategoryEditPatch, CategoryEditExact, CategoryEditFuzzy:
				counts[LessonReadBeforeEdit]++
			case CategoryPlanAnchor, CategoryContextCeiling:
				counts[LessonRetainObjective]++
			}
		}
	}
	policy := profile.snapshot.Provenance.Lessons
	for lesson, count := range counts {
		if count >= 2 {
			policy |= lesson
		}
	}
	return policy, nil
}
