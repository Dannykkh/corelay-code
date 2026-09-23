package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/agent"
)

func shadowEvalFixture() shadowCorpus {
	d := strings.Repeat("a", 64)
	return shadowCorpus{SchemaVersion: 1, Cases: []shadowCase{{ID: "repeat", Group: "fixture-repair", Split: "development", Origin: "synthetic",
		Point:    agent.ShadowPoint{RunID: "run_" + strings.Repeat("a", 32), SessionDigest: d, WorkspaceDigest: d, CoderDigest: d, JudgeDigest: d, Sequence: 2, ConsecutiveErrors: 2, Previous: agent.ShadowRound{Tools: []agent.ShadowToolObservation{{ID: "r1-t0", Error: true}}}, Current: agent.ShadowRound{Tools: []agent.ShadowToolObservation{{ID: "r2-t0", Error: true}}}},
		Evidence: agent.ShadowEvidence{Objective: "repair", Acceptance: "test passes", Excerpts: map[string]string{"r1-t0": "private-fixture-text", "r2-t0": "same failure"}},
		Labels:   shadowLabels{"yes", "no", "yes"}, ExpectedAdvisory: "consider_alternative", Outcome: shadowObservedOutcome{Task: "failed", Intervention: "direction"},
	}}}
}

func writeShadowTestJSON(t *testing.T, name string, v any) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if os.WriteFile(p, b, 0600) != nil {
		t.Fatal("write fixture")
	}
	return p
}

func shadowResponseFixture(c shadowCase) shadowRecordedResponse {
	_, digest, _ := agent.BuildShadowEvaluationRequest(c.Point, c.Evidence)
	a := func(v string) agent.ShadowAnswer { return agent.ShadowAnswer{Value: v, EvidenceIDs: []string{"r2-t0"}} }
	raw, _ := json.Marshal(agent.ShadowResponse{SameFailure: a("yes"), NewEvidence: a("no"), AlternativeSupported: a("yes")})
	return shadowRecordedResponse{CaseID: c.ID, RequestDigest: digest, Response: raw}
}

func TestShadowEvalOfflineBindingAndNoProvider(t *testing.T) {
	corpus := shadowEvalFixture()
	response := shadowResponseFixture(corpus.Cases[0])
	for _, mode := range []string{"valid", "missing", "stale", "invalid-answer"} {
		t.Run(mode, func(t *testing.T) {
			args := []string{"shadow-eval", "--corpus", writeShadowTestJSON(t, "cases.json", corpus), "--split", "development"}
			r := response
			if mode == "stale" {
				r.RequestDigest = "different"
			}
			if mode == "invalid-answer" {
				r.Response = json.RawMessage(`{"command":"private-value"}`)
			}
			if mode != "missing" {
				args = append(args, "--responses", writeShadowTestJSON(t, "answers.json", shadowResponses{SchemaVersion: 1, Responses: []shadowRecordedResponse{r}}))
			}
			var out, errOut bytes.Buffer
			code := runCLI(context.Background(), args, &out, &errOut, cliDependencies{})
			want := 3
			if mode == "valid" {
				want = 0
			}
			if code != want {
				t.Fatalf("code=%d stderr=%s", code, errOut.String())
			}
			var report shadowEvalReport
			if json.Unmarshal(out.Bytes(), &report) != nil {
				t.Fatalf("bad report %s", out.String())
			}
			if report.Cases != 1 || report.Synthetic != 1 || report.BaselineMatches != 0 || report.FixedRuleMatches != 0 {
				t.Fatalf("bad comparison %+v", report)
			}
			if mode == "valid" && (report.JudgeMatches != 1 || report.QuestionConfusion["sameFailure"]["yes->yes"] != 1) {
				t.Fatalf("judge comparison %+v", report)
			}
			if mode != "valid" && report.JudgeMatches != 0 {
				t.Fatal("missing/invalid judgment credited")
			}
			if strings.Contains(out.String(), "private-") {
				t.Fatal("evidence or error leaked")
			}
		})
	}
}

func TestShadowEvalRejectsLeakageAndDuplicateResponses(t *testing.T) {
	for _, mode := range []string{"group", "run", "evidence", "duplicate-id", "bad-label", "duplicate-response"} {
		t.Run(mode, func(t *testing.T) {
			c := shadowEvalFixture()
			v := c.Cases[0]
			v.ID = "second"
			v.Split = "evaluation"
			switch mode {
			case "run":
				v.Group = "other"
			case "evidence":
				v.Group = "other"
				v.Point.RunID = "run_" + strings.Repeat("b", 32)
			case "duplicate-id":
				v.Split = "development"
				v.ID = "repeat"
			case "bad-label":
				v.Split = "development"
				v.Labels.SameFailure = "maybe"
			}
			args := []string{"--split", "development"}
			if mode == "duplicate-response" {
				r := shadowResponseFixture(v)
				r.CaseID = "repeat"
				args = append(args, "--responses", writeShadowTestJSON(t, "responses.json", shadowResponses{1, []shadowRecordedResponse{r, r}}))
			} else {
				c.Cases = append(c.Cases, v)
			}
			args = append(args, "--corpus", writeShadowTestJSON(t, "cases.json", c))
			var out, errOut bytes.Buffer
			if code := runShadowEval(context.Background(), args, &out, &errOut); code != 2 {
				t.Fatalf("code=%d out=%s", code, out.String())
			}
		})
	}
}

func TestShadowEvalRejectsDuplicateJSONAndCancellation(t *testing.T) {
	var corpus shadowCorpus
	if decodeShadowJSON([]byte(`{"schemaVersion":1,"schemaVersion":2,"cases":[]}`), &corpus) == nil {
		t.Fatal("duplicate JSON accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, errOut bytes.Buffer
	if runShadowEval(ctx, []string{"--corpus", writeShadowTestJSON(t, "cases.json", shadowEvalFixture()), "--split", "development"}, &out, &errOut) != 1 {
		t.Fatal("cancel ignored")
	}
}

func TestShadowBundledCorpusBindings(t *testing.T) {
	for _, split := range []string{"development", "selection", "evaluation"} {
		var out, errOut bytes.Buffer
		code := runShadowEval(context.Background(), []string{"--corpus", "testdata/shadow/corpus.json", "--responses", "testdata/shadow/responses.json", "--split", split}, &out, &errOut)
		if code != 0 {
			t.Fatalf("%s code=%d stderr=%s out=%s", split, code, errOut.String(), out.String())
		}
		var report shadowEvalReport
		if json.Unmarshal(out.Bytes(), &report) != nil || report.Synthetic != report.Cases || report.Evaluated != report.Cases {
			t.Fatal("synthetic fixture contract changed")
		}
	}
}
