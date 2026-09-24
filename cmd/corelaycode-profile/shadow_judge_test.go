package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/agent"
)

func TestShadowJudgeOllamaUsesOnlyFrozenEvidenceAndMeters(t *testing.T) {
	corpus := shadowEvalFixture()
	c := corpus.Cases[0]
	c.Labels.SameFailure = "no" // Must not appear in the model request.
	corpus.Cases[0] = c
	answer := shadowResponseFixture(c).Response
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" || r.Header.Get("Authorization") != "" {
			t.Error("unexpected endpoint or authorization")
		}
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.Messages) != 2 || strings.Contains(body.Messages[1].Content, "expectedAdvisory") || strings.Contains(body.Messages[1].Content, "sameFailure") {
			t.Error("label or malformed request sent to model")
		}
		json.NewEncoder(w).Encode(map[string]any{"done": true, "message": map[string]any{"content": string(answer)}, "prompt_eval_count": 40, "eval_count": 20})
	}))
	defer server.Close()
	corpusPath := writeShadowTestJSON(t, "corpus.json", corpus)
	outPath := filepath.Join(t.TempDir(), "responses.json")
	var stdout, stderr bytes.Buffer
	code := runShadowJudge(context.Background(), []string{"--corpus", corpusPath, "--split", "development", "--provider", "ollama", "--model", "test", "--endpoint", server.URL + "/api/chat", "--out", outPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	var responses shadowResponses
	if readShadowJSON(outPath, &responses) != nil || len(responses.Responses) != 1 || responses.Responses[0].Usage.InputTokens != 40 {
		t.Fatal("metered response not saved")
	}
	stdout.Reset()
	stderr.Reset()
	if runShadowEval(context.Background(), []string{"--corpus", corpusPath, "--split", "development", "--responses", outPath}, &stdout, &stderr) != 0 {
		t.Fatalf("evaluation failed: %s", stderr.String())
	}
	var report shadowEvalReport
	if json.Unmarshal(stdout.Bytes(), &report) != nil || report.Usage == nil || report.Usage.Calls != 1 || report.Usage.OutputTokens != 20 {
		t.Fatal("usage not included in report")
	}
	if info, err := os.Stat(outPath); err != nil || info.Size() == 0 {
		t.Fatal("response file missing")
	}
	stdout.Reset()
	stderr.Reset()
	if runShadowJudge(context.Background(), []string{"--corpus", corpusPath, "--split", "development", "--provider", "ollama", "--model", "test", "--endpoint", server.URL + "/api/chat", "--out", outPath}, &stdout, &stderr) != 2 {
		t.Fatal("existing output file should prevent calls")
	}
}

func TestShadowJudgeJevConvertsNoulAndAbstains(t *testing.T) {
	c := shadowEvalFixture().Cases[0]
	request, _, err := agent.BuildShadowEvaluationRequest(c.Point, c.Evidence)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("missing test authorization")
		}
		var body map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["questions"] == nil || body["state"] == nil || body["labels"] != nil {
			t.Error("invalid TypeSafe request")
		}
		json.NewEncoder(w).Encode(map[string]any{"model": "jev-2026-09-11", "answers": map[string]any{
			"sameFailure":          map[string]any{"type": "noul", "noul": 0.92},
			"newEvidence":          map[string]any{"type": "noul", "noul": 0.49},
			"alternativeSupported": map[string]any{"type": "noul", "noul": 0.04},
		}, "usage": map[string]any{"input_tokens": 73, "output_tokens": 12}})
	}))
	defer server.Close()
	client := shadowJudgeClient{provider: "jev", model: "jev-latest", endpoint: server.URL, apiKey: "test-key", http: server.Client()}
	result, err := client.judge(context.Background(), request)
	if err != nil || result.inputTokens != 73 || result.outputTokens != 12 || result.resolvedModel != "jev-2026-09-11" || result.jevProbabilities["newEvidence"] != 0.49 {
		t.Fatalf("jev call failed: %v", err)
	}
	answers, advisory, status := agent.EvaluateShadowResponse(request, result.response)
	if status != "evaluated" || advisory != "abstain" || answers.NewEvidence.Value != "unknown" {
		t.Fatalf("unexpected Jev mapping: %s %s %+v", status, advisory, answers)
	}
}

func TestShadowJudgeEndpointAndMissingKey(t *testing.T) {
	for _, endpoint := range []string{"https://127.0.0.1:11434/api/chat", "http://example.com/api/chat", "http://127.0.0.1:11434/other", "http://127.0.0.1:11434/api/chat?x=1"} {
		if validLoopbackEndpoint(endpoint) {
			t.Fatalf("unsafe endpoint accepted: %s", endpoint)
		}
	}
	t.Setenv("TYPESAFE_API_KEY", "")
	var stdout, stderr bytes.Buffer
	if runShadowJudge(context.Background(), []string{"--corpus", "missing", "--split", "development", "--provider", "jev", "--model", "jev-latest", "--out", filepath.Join(t.TempDir(), "out.json")}, &stdout, &stderr) != 2 || !strings.Contains(stderr.String(), "TYPESAFE_API_KEY") {
		t.Fatal("missing key not reported")
	}
}

func TestShadowJudgeDoesNotPersistMalformedModelText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"done": true, "message": map[string]any{"content": `{"secret":"private-fixture-text"}`}, "prompt_eval_count": 4, "eval_count": 5})
	}))
	defer server.Close()
	outPath := filepath.Join(t.TempDir(), "answers.json")
	var stdout, stderr bytes.Buffer
	code := runShadowJudge(context.Background(), []string{"--corpus", writeShadowTestJSON(t, "corpus.json", shadowEvalFixture()), "--split", "development", "--provider", "ollama", "--model", "test", "--endpoint", server.URL + "/api/chat", "--out", outPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	b, err := os.ReadFile(outPath)
	if err != nil || strings.Contains(string(b), "private-fixture-text") || !strings.Contains(string(b), `"response": null`) {
		t.Fatal("unvalidated model text persisted")
	}
}

func TestShadowJudgeRejectsMissingUsageInsteadOfCountingZero(t *testing.T) {
	c := shadowEvalFixture().Cases[0]
	request, _, _ := agent.BuildShadowEvaluationRequest(c.Point, c.Evidence)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"done": true, "message": map[string]any{"content": string(shadowResponseFixture(c).Response)}})
	}))
	defer server.Close()
	client := shadowJudgeClient{provider: "ollama", model: "test", endpoint: server.URL, http: server.Client()}
	if _, err := client.judge(context.Background(), request); err == nil {
		t.Fatal("missing usage accepted as zero")
	}
}
