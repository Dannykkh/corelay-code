package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"time"

	"github.com/Dannykkh/corelay-code/internal/agent"
)

const shadowJevEndpoint = "https://api.typesafe.ai/v1/systemone"

type shadowJudgeClient struct {
	provider string
	model    string
	endpoint string
	apiKey   string
	http     *http.Client
}

func runShadowJudge(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("shadow-judge", flag.ContinueOnError)
	flags.SetOutput(stderr)
	corpusPath := flags.String("corpus", "", "reviewed or synthetic JSON corpus")
	split := flags.String("split", "", "development, selection, or evaluation")
	provider := flags.String("provider", "", "ollama or jev")
	model := flags.String("model", "", "exact model ID")
	outputPath := flags.String("out", "", "new response JSON path; existing files are preserved")
	endpoint := flags.String("endpoint", "http://127.0.0.1:11434/api/chat", "loopback Ollama chat endpoint")
	if flags.Parse(args) != nil || flags.NArg() != 0 || *corpusPath == "" || !validShadowSplit(*split) || *outputPath == "" || *model == "" || len(*model) > 128 {
		fmt.Fprintln(stderr, "shadow-judge requires --corpus, --split, --provider, --model and --out")
		return 2
	}
	if _, err := os.Lstat(*outputPath); err == nil {
		fmt.Fprintln(stderr, "response file already exists")
		return 2
	} else if !os.IsNotExist(err) {
		fmt.Fprintln(stderr, "cannot inspect response path")
		return 2
	}
	client := shadowJudgeClient{provider: *provider, model: *model, http: &http.Client{Timeout: 90 * time.Second}}
	switch *provider {
	case "ollama":
		if !validLoopbackEndpoint(*endpoint) {
			fmt.Fprintln(stderr, "Ollama endpoint must be loopback HTTP")
			return 2
		}
		client.endpoint = *endpoint
	case "jev":
		client.endpoint = shadowJevEndpoint
		client.apiKey = os.Getenv("TYPESAFE_API_KEY")
		if client.apiKey == "" {
			fmt.Fprintln(stderr, "TYPESAFE_API_KEY is required for Jev")
			return 2
		}
	default:
		fmt.Fprintln(stderr, "shadow-judge provider must be ollama or jev")
		return 2
	}
	var corpus shadowCorpus
	if readShadowJSON(*corpusPath, &corpus) != nil || validateShadowCorpus(corpus) != nil {
		fmt.Fprintln(stderr, "invalid shadow corpus or split isolation")
		return 2
	}
	responses := shadowResponses{SchemaVersion: 1, Responses: []shadowRecordedResponse{}}
	for _, c := range corpus.Cases {
		if c.Split != *split {
			continue
		}
		request, digest, err := agent.BuildShadowEvaluationRequest(c.Point, c.Evidence)
		if err != nil {
			fmt.Fprintln(stderr, "invalid shadow request")
			return 2
		}
		if _, _, status := agent.EvaluateShadowResponse(request, []byte(`null`)); status == "insufficient_evidence" {
			fmt.Fprintln(stderr, "shadow corpus contains insufficient evidence")
			return 2
		}
		callCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		start := time.Now()
		raw, inputTokens, outputTokens, err := client.judge(callCtx, request)
		cancel()
		if err != nil {
			fmt.Fprintf(stderr, "model call failed for case %s: %s\n", c.ID, safeShadowCallError(err))
			return 1
		}
		if !json.Valid(raw) || len(raw) > 2048 {
			// Never persist unbounded or non-JSON model text.
			raw = []byte(`null`)
		} else if _, _, status := agent.EvaluateShadowResponse(request, raw); status != "evaluated" {
			// The response file is a bounded decision artifact, not a raw model log.
			raw = []byte(`null`)
		}
		responses.Responses = append(responses.Responses, shadowRecordedResponse{CaseID: c.ID, RequestDigest: digest, Response: raw,
			Usage: &shadowCallUsage{Provider: *provider, Model: *model, DurationMS: time.Since(start).Milliseconds(), InputTokens: inputTokens, OutputTokens: outputTokens}})
	}
	if len(responses.Responses) == 0 {
		fmt.Fprintln(stderr, "selected split has no cases")
		return 2
	}
	b, err := json.MarshalIndent(responses, "", "  ")
	if err != nil || len(b) > 2*1024*1024 {
		fmt.Fprintln(stderr, "response file exceeds limit")
		return 1
	}
	f, err := os.OpenFile(*outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		fmt.Fprintln(stderr, "cannot create response file; existing files are preserved")
		return 1
	}
	_, writeErr := f.Write(b)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(*outputPath)
		fmt.Fprintln(stderr, "cannot complete response file")
		return 1
	}
	fmt.Fprintf(stdout, "saved %d model responses to %s\n", len(responses.Responses), *outputPath)
	return 0
}

func validLoopbackEndpoint(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "/api/chat" {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || (net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback())
}

func safeShadowCallError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	return "request or response error"
}

func (c shadowJudgeClient) post(ctx context.Context, body any) ([]byte, error) {
	b, err := json.Marshal(body)
	if err != nil || len(b) > 32*1024 {
		return nil, errors.New("request exceeds budget")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("provider returned non-success status")
	}
	b, err = io.ReadAll(io.LimitReader(resp.Body, 64*1024+1))
	if err != nil || len(b) > 64*1024 {
		return nil, errors.New("provider response exceeds budget")
	}
	return b, nil
}

func (c shadowJudgeClient) judge(ctx context.Context, request agent.ShadowRequest) ([]byte, int, int, error) {
	if c.provider == "jev" {
		return c.judgeJev(ctx, request)
	}
	return c.judgeOllama(ctx, request)
}

func (c shadowJudgeClient) judgeOllama(ctx context.Context, request agent.ShadowRequest) ([]byte, int, int, error) {
	state, _ := json.Marshal(struct {
		Objective  string            `json:"objective"`
		Acceptance string            `json:"acceptance"`
		Previous   agent.ShadowRound `json:"previous"`
		Current    agent.ShadowRound `json:"current"`
		Excerpts   map[string]string `json:"excerpts"`
	}{request.Evidence.Objective, request.Evidence.Acceptance, request.Point.Previous, request.Point.Current, request.Evidence.Excerpts})
	answerSchema := map[string]any{"type": "object", "properties": map[string]any{
		"value":       map[string]any{"type": "string", "enum": []string{"yes", "no", "unknown"}},
		"evidenceIds": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	}, "required": []string{"value", "evidenceIds"}, "additionalProperties": false}
	body := map[string]any{"model": c.model, "stream": false, "think": false, "format": map[string]any{
		"type": "object", "properties": map[string]any{
			"sameFailure": answerSchema, "newEvidence": answerSchema, "alternativeSupported": answerSchema,
		}, "required": []string{"sameFailure", "newEvidence", "alternativeSupported"}, "additionalProperties": false,
	}, "options": map[string]any{"temperature": 0, "num_ctx": 4096, "num_predict": 200}, "messages": []map[string]string{
		{"role": "system", "content": "Answer the three questions using only the provided evidence. Return only JSON with sameFailure, newEvidence, alternativeSupported objects. Each object has value (yes/no/unknown) and evidenceIds (array of excerpt keys considered; required for yes/no). If uncertain, use unknown. Do not include other keys. Questions: sameFailure = same observable failure type? newEvidence = did current attempt add relevant evidence? alternativeSupported = is a specific different approach supported?"},
		{"role": "user", "content": string(state)},
	}}
	b, err := c.post(ctx, body)
	if err != nil {
		return nil, 0, 0, err
	}
	var result struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		Done            bool `json:"done"`
		PromptEvalCount *int `json:"prompt_eval_count"`
		EvalCount       *int `json:"eval_count"`
	}
	if json.Unmarshal(b, &result) != nil || !result.Done || result.PromptEvalCount == nil || result.EvalCount == nil || *result.PromptEvalCount < 0 || *result.EvalCount < 0 {
		return nil, 0, 0, errors.New("invalid provider response")
	}
	return []byte(result.Message.Content), *result.PromptEvalCount, *result.EvalCount, nil
}

func (c shadowJudgeClient) judgeJev(ctx context.Context, request agent.ShadowRequest) ([]byte, int, int, error) {
	ids := make([]string, 0, len(request.Evidence.Excerpts))
	for id := range request.Evidence.Excerpts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	state := map[string]any{"objective": request.Evidence.Objective, "acceptance": request.Evidence.Acceptance,
		"previous": request.Point.Previous, "current": request.Point.Current, "excerpts": request.Evidence.Excerpts}
	questions := map[string]any{
		"sameFailure":          map[string]any{"type": "noul", "instructions": "Do the previous and current attempts show the same observable failure type? Judge only from excerpts and tool observations."},
		"newEvidence":          map[string]any{"type": "noul", "instructions": "Did the current attempt add new evidence relevant to resolving the previous failure?"},
		"alternativeSupported": map[string]any{"type": "noul", "instructions": "Does the available evidence support a specific different approach to this task?"},
	}
	b, err := c.post(ctx, map[string]any{"model": c.model, "state": state, "questions": questions})
	if err != nil {
		return nil, 0, 0, err
	}
	var result struct {
		Answers map[string]struct {
			Type string   `json:"type"`
			Noul *float64 `json:"noul"`
		} `json:"answers"`
		Usage struct {
			InputTokens  *int `json:"input_tokens"`
			OutputTokens *int `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(b, &result) != nil || len(result.Answers) != 3 || result.Usage.InputTokens == nil || result.Usage.OutputTokens == nil || *result.Usage.InputTokens < 0 || *result.Usage.OutputTokens < 0 {
		return nil, 0, 0, errors.New("invalid provider response")
	}
	values := map[string]agent.ShadowAnswer{}
	for _, name := range []string{"sameFailure", "newEvidence", "alternativeSupported"} {
		a, ok := result.Answers[name]
		if !ok || a.Type != "noul" || a.Noul == nil || *a.Noul < 0 || *a.Noul > 1 {
			return nil, 0, 0, errors.New("invalid provider answer")
		}
		value := "unknown"
		if *a.Noul >= 0.8 {
			value = "yes"
		} else if *a.Noul <= 0.2 {
			value = "no"
		}
		answer := agent.ShadowAnswer{Value: value}
		if value != "unknown" {
			// Jev does not return citations. These IDs identify evidence supplied to the
			// judgment, not model-selected supporting spans.
			answer.EvidenceIDs = ids
		}
		values[name] = answer
	}
	raw, _ := json.Marshal(agent.ShadowResponse{SameFailure: values["sameFailure"], NewEvidence: values["newEvidence"], AlternativeSupported: values["alternativeSupported"]})
	return raw, *result.Usage.InputTokens, *result.Usage.OutputTokens, nil
}
