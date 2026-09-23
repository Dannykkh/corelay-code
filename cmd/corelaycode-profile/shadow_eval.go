package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"

	"github.com/Dannykkh/corelay-code/internal/agent"
)

type shadowLabels struct {
	SameFailure          string `json:"sameFailure"`
	NewEvidence          string `json:"newEvidence"`
	AlternativeSupported string `json:"alternativeSupported"`
}
type shadowObservedOutcome struct {
	Task                     string `json:"task"`
	Intervention             string `json:"intervention"`
	LaterRepeatedErrorRounds *int   `json:"laterRepeatedErrorRounds,omitempty"`
}
type shadowCase struct {
	ID               string                `json:"id"`
	Group            string                `json:"group"`
	Split            string                `json:"split"`
	Origin           string                `json:"origin"`
	TraceID          string                `json:"traceId,omitempty"`
	Point            agent.ShadowPoint     `json:"point"`
	Evidence         agent.ShadowEvidence  `json:"evidence"`
	Labels           shadowLabels          `json:"labels"`
	ExpectedAdvisory string                `json:"expectedAdvisory"`
	Outcome          shadowObservedOutcome `json:"outcome"`
}
type shadowCorpus struct {
	SchemaVersion int          `json:"schemaVersion"`
	Cases         []shadowCase `json:"cases"`
}
type shadowRecordedResponse struct {
	CaseID        string           `json:"caseId"`
	RequestDigest string           `json:"requestDigest"`
	Response      json.RawMessage  `json:"response"`
	Usage         *shadowCallUsage `json:"usage,omitempty"`
}
type shadowCallUsage struct {
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	DurationMS   int64  `json:"durationMs"`
	InputTokens  int    `json:"inputTokens"`
	OutputTokens int    `json:"outputTokens"`
}
type shadowResponses struct {
	SchemaVersion int                      `json:"schemaVersion"`
	Responses     []shadowRecordedResponse `json:"responses"`
}
type shadowCaseResult struct {
	ID            string                `json:"id"`
	RequestDigest string                `json:"requestDigest"`
	Status        string                `json:"status"`
	Baseline      string                `json:"baseline"`
	FixedRule     string                `json:"fixedRule"`
	Judge         string                `json:"judge"`
	Outcome       shadowObservedOutcome `json:"observedOutcome"`
}
type shadowEvalReport struct {
	SchemaVersion     int                       `json:"schemaVersion"`
	Mode              string                    `json:"mode"`
	Split             string                    `json:"split"`
	Cases             int                       `json:"cases"`
	Synthetic         int                       `json:"synthetic"`
	Missing           int                       `json:"missing"`
	Invalid           int                       `json:"invalid"`
	Evaluated         int                       `json:"evaluated"`
	Abstained         int                       `json:"abstained"`
	BaselineMatches   int                       `json:"baselineMatches"`
	FixedRuleMatches  int                       `json:"fixedRuleMatches"`
	JudgeMatches      int                       `json:"judgeMatches"`
	QuestionConfusion map[string]map[string]int `json:"questionConfusion"`
	QuestionMatches   map[string]int            `json:"questionMatches"`
	UsageStatus       string                    `json:"usageStatus"`
	Usage             *shadowUsageTotal         `json:"usage,omitempty"`
	LatencyP50MS      *int64                    `json:"latencyP50Ms,omitempty"`
	LatencyP95MS      *int64                    `json:"latencyP95Ms,omitempty"`
	CostStatus        string                    `json:"costStatus"`
	Results           []shadowCaseResult        `json:"results"`
}
type shadowUsageTotal struct {
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	Calls        int    `json:"calls"`
	DurationMS   int64  `json:"durationMs"`
	InputTokens  int    `json:"inputTokens"`
	OutputTokens int    `json:"outputTokens"`
}

var shadowCaseID = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,80}$`)

func readShadowJSON(path string, out any) error {
	f, err := os.Open(path)
	if err != nil {
		return errors.New("cannot read shadow evaluation file")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 2*1024*1024+1))
	if err != nil || len(b) > 2*1024*1024 {
		return errors.New("shadow evaluation file exceeds limit")
	}
	return decodeShadowJSON(b, out)
}

func decodeShadowJSON(b []byte, out any) error {
	// The same duplicate-key rejection used for model responses prevents
	// silently changing a split, label, or request binding in a manifest.
	return agent.DecodeShadowDocument(b, out)
}

func validShadowLabel(s string) bool { return s == "yes" || s == "no" || s == "unknown" }
func validShadowAdvisory(s string) bool {
	return s == "continue" || s == "gather_evidence" || s == "consider_alternative" || s == "abstain"
}
func validShadowSplit(s string) bool {
	return s == "development" || s == "selection" || s == "evaluation"
}

func validateShadowCorpus(c shadowCorpus) error {
	bad := errors.New("invalid shadow corpus")
	if c.SchemaVersion != 1 || len(c.Cases) == 0 || len(c.Cases) > 1000 {
		return bad
	}
	ids, groups, points := map[string]bool{}, map[string]string{}, map[string]string{}
	runs, evidenceSplits := map[string]string{}, map[string]string{}
	for _, v := range c.Cases {
		if !shadowCaseID.MatchString(v.ID) || !shadowCaseID.MatchString(v.Group) || ids[v.ID] || !validShadowSplit(v.Split) {
			return bad
		}
		ids[v.ID] = true
		if s, ok := runs[v.Point.RunID]; ok && s != v.Split {
			return errors.New("shadow run crosses splits")
		}
		runs[v.Point.RunID] = v.Split
		evidenceJSON, _ := json.Marshal(v.Evidence)
		if s, ok := evidenceSplits[string(evidenceJSON)]; ok && s != v.Split {
			return errors.New("shadow evidence crosses splits")
		}
		evidenceSplits[string(evidenceJSON)] = v.Split
		if s, ok := groups[v.Group]; ok && s != v.Split {
			return errors.New("shadow group crosses splits")
		}
		groups[v.Group] = v.Split
		if v.Origin != "synthetic" && v.Origin != "reviewed" {
			return bad
		}
		if v.TraceID != "" && !shadowCaseID.MatchString(v.TraceID) {
			return bad
		}
		if v.Origin == "reviewed" && v.TraceID == "" {
			return bad
		}
		if !validShadowLabel(v.Labels.SameFailure) || !validShadowLabel(v.Labels.NewEvidence) || !validShadowLabel(v.Labels.AlternativeSupported) || !validShadowAdvisory(v.ExpectedAdvisory) {
			return bad
		}
		switch v.Outcome.Task {
		case "passed", "failed", "unknown":
		default:
			return bad
		}
		switch v.Outcome.Intervention {
		case "none", "direction", "requirement", "authority", "unknown":
		default:
			return bad
		}
		if v.Outcome.LaterRepeatedErrorRounds != nil && *v.Outcome.LaterRepeatedErrorRounds < 0 {
			return bad
		}
		_, digest, err := agent.BuildShadowEvaluationRequest(v.Point, v.Evidence)
		if err != nil {
			return bad
		}
		if s, ok := points[digest]; ok && s != v.Split {
			return errors.New("shadow evidence crosses splits")
		}
		points[digest] = v.Split
	}
	return nil
}

func runShadowEval(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("shadow-eval", flag.ContinueOnError)
	flags.SetOutput(stderr)
	corpusPath := flags.String("corpus", "", "reviewed or synthetic JSON corpus")
	responsePath := flags.String("responses", "", "optional recorded judge responses; no provider is called")
	split := flags.String("split", "", "development, selection, or evaluation (required)")
	if flags.Parse(args) != nil || flags.NArg() != 0 || *corpusPath == "" || !validShadowSplit(*split) {
		fmt.Fprintln(stderr, "shadow-eval requires --corpus and --split")
		return 2
	}
	var corpus shadowCorpus
	if readShadowJSON(*corpusPath, &corpus) != nil || validateShadowCorpus(corpus) != nil {
		fmt.Fprintln(stderr, "invalid shadow corpus or split isolation")
		return 2
	}
	byID := map[string]shadowRecordedResponse{}
	caseIDs := map[string]bool{}
	for _, v := range corpus.Cases {
		caseIDs[v.ID] = true
	}
	if *responsePath != "" {
		var responses shadowResponses
		if readShadowJSON(*responsePath, &responses) != nil || responses.SchemaVersion != 1 || len(responses.Responses) > 1000 {
			fmt.Fprintln(stderr, "invalid shadow responses")
			return 2
		}
		for _, v := range responses.Responses {
			if _, ok := byID[v.CaseID]; ok || !caseIDs[v.CaseID] {
				fmt.Fprintln(stderr, "duplicate or unknown response case")
				return 2
			}
			if v.Usage != nil && (v.Usage.Provider == "" || v.Usage.Model == "" || len(v.Usage.Model) > 128 || v.Usage.DurationMS < 0 || v.Usage.InputTokens < 0 || v.Usage.OutputTokens < 0) {
				fmt.Fprintln(stderr, "invalid response usage")
				return 2
			}
			byID[v.CaseID] = v
		}
	}
	report := shadowEvalReport{SchemaVersion: 1, Mode: "offline-recorded-responses", Split: *split, UsageStatus: "unknown-no-live-calls", CostStatus: "unknown-no-price", QuestionConfusion: map[string]map[string]int{"sameFailure": {}, "newEvidence": {}, "alternativeSupported": {}}, QuestionMatches: map[string]int{"sameFailure": 0, "newEvidence": 0, "alternativeSupported": 0}, Results: []shadowCaseResult{}}
	latencies := []int64{}
	for _, v := range corpus.Cases {
		if v.Split != *split {
			continue
		}
		if ctx.Err() != nil {
			fmt.Fprintln(stderr, "shadow evaluation canceled")
			return 1
		}
		request, digest, _ := agent.BuildShadowEvaluationRequest(v.Point, v.Evidence)
		r := shadowCaseResult{ID: v.ID, RequestDigest: digest, Status: "missing", Baseline: "continue", FixedRule: "continue", Judge: "abstain", Outcome: v.Outcome}
		if v.Point.ConsecutiveErrors >= 2 || v.Point.GuardDenied {
			r.FixedRule = "gather_evidence"
		}
		report.Cases++
		if v.Origin == "synthetic" {
			report.Synthetic++
		}
		if r.Baseline == v.ExpectedAdvisory {
			report.BaselineMatches++
		}
		if r.FixedRule == v.ExpectedAdvisory {
			report.FixedRuleMatches++
		}
		if response, ok := byID[v.ID]; !ok {
			report.Missing++
		} else if response.RequestDigest != digest {
			r.Status = "binding_mismatch"
			report.Invalid++
		} else {
			if response.Usage != nil {
				if report.Usage == nil {
					report.Usage = &shadowUsageTotal{Provider: response.Usage.Provider, Model: response.Usage.Model}
				}
				if report.Usage.Provider != response.Usage.Provider || report.Usage.Model != response.Usage.Model {
					fmt.Fprintln(stderr, "mixed response models")
					return 2
				}
				report.Usage.Calls++
				report.Usage.DurationMS += response.Usage.DurationMS
				report.Usage.InputTokens += response.Usage.InputTokens
				report.Usage.OutputTokens += response.Usage.OutputTokens
				latencies = append(latencies, response.Usage.DurationMS)
				report.UsageStatus = "reported-by-provider"
			}
			answers, advisory, status := agent.EvaluateShadowResponse(request, response.Response)
			r.Status, r.Judge = status, advisory
			if status != "evaluated" {
				report.Invalid++
			} else {
				report.Evaluated++
				if advisory == "abstain" {
					report.Abstained++
				}
				if advisory == v.ExpectedAdvisory {
					report.JudgeMatches++
				}
				for name, pair := range map[string][2]string{"sameFailure": {v.Labels.SameFailure, answers.SameFailure.Value}, "newEvidence": {v.Labels.NewEvidence, answers.NewEvidence.Value}, "alternativeSupported": {v.Labels.AlternativeSupported, answers.AlternativeSupported.Value}} {
					report.QuestionConfusion[name][pair[0]+"->"+pair[1]]++
					if pair[0] == pair[1] {
						report.QuestionMatches[name]++
					}
				}
			}
		}
		report.Results = append(report.Results, r)
	}
	if report.Cases == 0 {
		fmt.Fprintln(stderr, "selected split has no cases")
		return 2
	}
	if report.Usage != nil && report.Usage.Calls != report.Cases {
		report.UsageStatus = "partial-provider-usage"
	}
	if len(latencies) > 0 {
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		p50, p95 := latencies[(len(latencies)-1)/2], latencies[(95*len(latencies)+99)/100-1]
		report.LatencyP50MS, report.LatencyP95MS = &p50, &p95
	}
	if json.NewEncoder(stdout).Encode(report) != nil {
		return 1
	}
	if report.Invalid > 0 || report.Missing > 0 {
		return 3
	}
	return 0
}
