package agent

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"
)

// DecodeShadowDocument strictly decodes bounded evaluation documents supplied
// by the CLI. Callers own the file-size limit; duplicate keys are forbidden.
func DecodeShadowDocument(b []byte, out any) error {
	if rejectDuplicateJSONKeys(b) != nil {
		return errors.New("invalid shadow document")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return errors.New("invalid shadow document")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("invalid shadow document")
	}
	return nil
}

var shadowEvidenceID = regexp.MustCompile(`^r([0-9]{1,10})-t([0-9]{1,2})$`)
var shadowRunID = regexp.MustCompile(`^run_[0-9a-f_]{1,64}$`)

func validShadowDigest(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32
}

// ValidateShadowRecord is the storage boundary for the optional recorder.
// Model text and arbitrary errors cannot enter the redacted trace through it.
func ValidateShadowRecord(r ShadowJudgmentRecord) error {
	bad := errors.New("invalid shadow record")
	if r.LaterTruncatedRounds < 0 || r.LaterTruncatedRounds > r.LaterRounds {
		return bad
	}
	if !shadowRunID.MatchString(r.RunID) || r.Sequence < 1 || r.DurationMS < 0 || r.LaterRounds < 0 || r.LaterErrorRounds < 0 || r.LaterRepeatedErrorRounds < 0 || r.LaterRepeatedErrorRounds > r.LaterErrorRounds || r.LaterErrorRounds > r.LaterRounds {
		return bad
	}
	for _, d := range []string{r.SessionDigest, r.WorkspaceDigest, r.CoderDigest, r.JudgeDigest, r.ContractDigest} {
		if !validShadowDigest(d) {
			return bad
		}
	}
	if r.InputDigest != "" && !validShadowDigest(r.InputDigest) {
		return bad
	}
	switch r.Status {
	case "evaluated":
		if r.Answers == nil || r.InputDigest == "" || shadowAdvisory(*r.Answers) != r.Advisory {
			return bad
		}
		e := ShadowEvidence{Excerpts: map[string]string{}}
		for _, a := range []ShadowAnswer{r.Answers.SameFailure, r.Answers.NewEvidence, r.Answers.AlternativeSupported} {
			for _, id := range a.EvidenceIDs {
				parts := shadowEvidenceID.FindStringSubmatch(id)
				if parts == nil {
					return bad
				}
				seq, err := strconv.ParseInt(parts[1], 10, 64)
				index, _ := strconv.Atoi(parts[2])
				if err != nil || seq < 1 || seq > int64(r.Sequence) || index >= 16 {
					return bad
				}
				e.Excerpts[id] = "reference"
			}
		}
		raw, _ := json.Marshal(r.Answers)
		if _, err := decodeShadowResponse(raw, e); err != nil {
			return bad
		}
	case "dropped", "canceled", "timeout", "prepare_error", "judge_error", "insufficient_evidence", "input_budget", "invalid":
		if r.Answers != nil || r.Advisory != "abstain" {
			return bad
		}
	default:
		return bad
	}
	return nil
}

// BuildShadowEvaluationRequest freezes the same question and input contract
// used online. Labels and later outcomes must never be passed here as evidence.
func BuildShadowEvaluationRequest(p ShadowPoint, e ShadowEvidence) (ShadowRequest, string, error) {
	p.ContractDigest = shadowDigest(shadowContract)
	request := ShadowRequest{Point: p, Contract: shadowContract, Evidence: e}
	identity := ShadowJudgmentRecord{RunID: p.RunID, Sequence: p.Sequence, SessionDigest: p.SessionDigest, SessionRevision: p.SessionRevision, WorkspaceDigest: p.WorkspaceDigest, CoderDigest: p.CoderDigest, JudgeDigest: p.JudgeDigest, ContractDigest: p.ContractDigest, Status: "canceled", Advisory: "abstain"}
	if ValidateShadowRecord(identity) != nil || p.ConsecutiveErrors < 0 {
		return ShadowRequest{}, "", errors.New("invalid shadow point")
	}
	b, err := json.Marshal(request)
	if err != nil || len(b) > 8192 {
		return ShadowRequest{}, "", errors.New("shadow input budget exceeded")
	}
	return request, shadowDigest(string(b)), nil
}

// EvaluateShadowResponse uses online validation and advisory mapping offline.
// Missing evidence is an abstention, not a negative answer or a correct label.
func EvaluateShadowResponse(request ShadowRequest, raw []byte) (*ShadowResponse, string, string) {
	if request.Contract != shadowContract || request.Point.ContractDigest != shadowDigest(shadowContract) {
		return nil, "abstain", "invalid_contract"
	}
	if _, _, err := BuildShadowEvaluationRequest(request.Point, request.Evidence); err != nil {
		return nil, "abstain", "invalid_input"
	}
	if request.Evidence.Missing || request.Evidence.Truncated || request.Point.Previous.Truncated || request.Point.Current.Truncated || !validShadowEvidence(request.Evidence, request.Point) {
		return nil, "abstain", "insufficient_evidence"
	}
	r, err := decodeShadowResponse(raw, request.Evidence)
	if err != nil {
		return nil, "abstain", "invalid"
	}
	return &r, shadowAdvisory(r), "evaluated"
}
