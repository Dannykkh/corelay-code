package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
	"unicode/utf8"
)

const shadowContract = `v1;schema=yes|no|unknown+evidence_ids;policy=unknown:abstain,new:continue,same+alternative:consider_alternative,same:gather_evidence,else:continue;trigger=errors>=2|guard-denial;max=3;input=8192;output=2048
same_failure: Do the previous and current failures have the same observable failure type?
new_evidence: Has this attempt added evidence relevant to resolving the previous failure?
alternative_supported: Does the available evidence support considering a specific different approach?`

// ShadowJudgmentConfig is an explicit, trusted harness injection, not a model
// tool. Prepare must resolve and sanitize evidence in this run's namespace.
// No raw transcript, arguments, or tool output is automatically sent to Judge.
// Both callbacks must honor cancellation; adapters must bound response reads.
// The caller authorizes the JudgeTarget and its evidence transmission scope.
type ShadowJudgmentConfig struct {
	JudgeTarget string
	Prepare     func(context.Context, ShadowPoint) (ShadowEvidence, error)
	Judge       func(context.Context, ShadowRequest) ([]byte, error)
}

type ShadowToolObservation struct {
	ID           string `json:"id"`
	ToolDigest   string `json:"toolDigest"`
	ResultDigest string `json:"resultDigest"`
	Error        bool   `json:"error"`
	Executed     bool   `json:"executed"`
}

type ShadowRound struct {
	Sequence  int                     `json:"sequence"`
	Tools     []ShadowToolObservation `json:"tools"`
	Truncated bool                    `json:"truncated"`
}

type ShadowPoint struct {
	RunID             string      `json:"runId"`
	SessionDigest     string      `json:"sessionDigest"`
	SessionRevision   uint64      `json:"sessionRevision"`
	WorkspaceDigest   string      `json:"workspaceDigest"`
	CoderDigest       string      `json:"coderDigest"`
	JudgeDigest       string      `json:"judgeDigest"`
	ContractDigest    string      `json:"contractDigest"`
	Sequence          int         `json:"sequence"`
	ConsecutiveErrors int         `json:"consecutiveErrors"`
	GuardDenied       bool        `json:"guardDenied"`
	Previous          ShadowRound `json:"previous"`
	Current           ShadowRound `json:"current"`
}

type ShadowEvidence struct {
	Objective  string            `json:"objective"`
	Acceptance string            `json:"acceptance"`
	Excerpts   map[string]string `json:"excerpts"`
	Missing    bool              `json:"missing"`
	Truncated  bool              `json:"truncated"`
}

type ShadowRequest struct {
	Point    ShadowPoint    `json:"point"`
	Contract string         `json:"contract"`
	Evidence ShadowEvidence `json:"evidence"`
}

type ShadowAnswer struct {
	Value       string   `json:"value"`
	EvidenceIDs []string `json:"evidenceIds"`
}

type ShadowResponse struct {
	SameFailure          ShadowAnswer `json:"sameFailure"`
	NewEvidence          ShadowAnswer `json:"newEvidence"`
	AlternativeSupported ShadowAnswer `json:"alternativeSupported"`
}

// ShadowJudgmentRecord contains no evidence text or provider error message.
// Usage is deliberately absent until a metered provider adapter is connected.
type ShadowJudgmentRecord struct {
	RunID                    string          `json:"runId"`
	Sequence                 int             `json:"sequence"`
	SessionDigest            string          `json:"sessionDigest"`
	SessionRevision          uint64          `json:"sessionRevision"`
	WorkspaceDigest          string          `json:"workspaceDigest"`
	CoderDigest              string          `json:"coderDigest"`
	JudgeDigest              string          `json:"judgeDigest"`
	ContractDigest           string          `json:"contractDigest"`
	InputDigest              string          `json:"inputDigest,omitempty"`
	Status                   string          `json:"status"`
	Advisory                 string          `json:"advisory"`
	DurationMS               int64           `json:"durationMs"`
	Answers                  *ShadowResponse `json:"answers,omitempty"`
	LaterRounds              int             `json:"laterRounds"`
	LaterErrorRounds         int             `json:"laterErrorRounds"`
	LaterRepeatedErrorRounds int             `json:"laterRepeatedErrorRounds"`
	LaterTruncatedRounds     int             `json:"laterTruncatedRounds"`
}

// RunShadowJudgmentRecorder is called on the run goroutine, before terminal
// finalization, with at most three records. Worker callbacks never call it.
type RunShadowJudgmentRecorder interface {
	ShadowJudgmentRecorded(ShadowJudgmentRecord)
}

type shadowObserver struct {
	mu           sync.Mutex
	ctx          context.Context
	cancel       context.CancelFunc
	cfg          ShadowJudgmentConfig
	base         ShadowPoint
	queue        chan ShadowPoint
	closed       bool
	lastSequence int
	lastDenied   int
	previous     ShadowRound
	records      []ShadowJudgmentRecord
	timeout      time.Duration
	failureKeys  map[int]map[string]bool
}

func shadowDigest(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func newShadowObserver(ctx context.Context, cfg *ShadowJudgmentConfig, base ShadowPoint) *shadowObserver {
	if cfg == nil || cfg.Prepare == nil || cfg.Judge == nil || cfg.JudgeTarget == "" {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	base.JudgeDigest = shadowDigest(cfg.JudgeTarget)
	base.ContractDigest = shadowDigest(shadowContract)
	o := &shadowObserver{ctx: ctx, cancel: cancel, cfg: *cfg, base: base, queue: make(chan ShadowPoint, 1), timeout: 2 * time.Second, failureKeys: map[int]map[string]bool{}}
	go o.work()
	return o
}

// observe is called only by the owning loop. The bounded metadata snapshot
// references results; resolving their contents requires the explicit Prepare.
func (o *shadowObserver) observe(sequence, errors int, guard RunGuardSnapshot, results []toolDispatchResult) {
	if o == nil {
		return
	}
	current := ShadowRound{Sequence: sequence, Truncated: len(results) > 16}
	for i, r := range results {
		if i == 16 {
			break
		}
		current.Tools = append(current.Tools, ShadowToolObservation{ID: fmt.Sprintf("r%d-t%d", sequence, i), ToolDigest: shadowDigest(r.Tool.Name), ResultDigest: shadowDigest(r.Content), Error: r.IsError, Executed: r.Executed})
	}
	p := o.base
	o.mu.Lock()
	for i := range o.records {
		r := &o.records[i]
		if sequence <= r.Sequence {
			continue
		}
		r.LaterRounds++
		if current.Truncated {
			r.LaterTruncatedRounds++
		}
		anyError, repeated := false, false
		for _, tool := range current.Tools {
			if tool.Error {
				anyError = true
				repeated = repeated || o.failureKeys[r.Sequence][tool.ToolDigest+tool.ResultDigest]
			}
		}
		if anyError {
			r.LaterErrorRounds++
		}
		if repeated {
			r.LaterRepeatedErrorRounds++
		}
	}
	o.mu.Unlock()
	p.Sequence, p.ConsecutiveErrors, p.GuardDenied = sequence, errors, guard.Denied > o.lastDenied
	p.Previous, p.Current = o.previous, current
	o.previous, o.lastDenied = current, guard.Denied
	if errors >= 2 || p.GuardDenied {
		o.submit(p)
	}
}

func (o *shadowObserver) submit(p ShadowPoint) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed || o.ctx.Err() != nil || p.Sequence <= o.lastSequence || len(o.records) >= 3 {
		return
	}
	o.lastSequence = p.Sequence
	o.failureKeys[p.Sequence] = map[string]bool{}
	for _, tool := range p.Current.Tools {
		if tool.Error {
			o.failureKeys[p.Sequence][tool.ToolDigest+tool.ResultDigest] = true
		}
	}
	// Copy all slice storage so the worker cannot observe later loop mutations.
	p.Previous.Tools = append([]ShadowToolObservation(nil), p.Previous.Tools...)
	p.Current.Tools = append([]ShadowToolObservation(nil), p.Current.Tools...)
	r := ShadowJudgmentRecord{RunID: p.RunID, Sequence: p.Sequence, SessionDigest: p.SessionDigest, SessionRevision: p.SessionRevision, WorkspaceDigest: p.WorkspaceDigest, CoderDigest: p.CoderDigest, JudgeDigest: p.JudgeDigest, ContractDigest: p.ContractDigest, Status: "pending", Advisory: "abstain"}
	select {
	case o.queue <- p:
	default:
		r.Status = "dropped"
	}
	o.records = append(o.records, r)
}

func (o *shadowObserver) work() {
	for {
		select {
		case <-o.ctx.Done():
			return
		case p := <-o.queue:
			if o.ctx.Err() != nil {
				return
			}
			start := time.Now()
			ctx, cancel := context.WithTimeout(o.ctx, o.timeout)
			response, digest, status := o.evaluate(ctx, p)
			cancel()
			o.mu.Lock()
			if !o.closed {
				for i := range o.records {
					if o.records[i].Sequence == p.Sequence {
						r := &o.records[i]
						r.Status = status
						r.InputDigest = digest
						r.DurationMS = time.Since(start).Milliseconds()
						if status == "evaluated" {
							r.Answers = &response
							r.Advisory = shadowAdvisory(response)
						}
					}
				}
			}
			o.mu.Unlock()
		}
	}
}

func (o *shadowObserver) evaluate(ctx context.Context, p ShadowPoint) (ShadowResponse, string, string) {
	e, err := o.cfg.Prepare(ctx, p)
	if ctx.Err() != nil {
		return ShadowResponse{}, "", shadowContextStatus(ctx)
	}
	if err != nil {
		return ShadowResponse{}, "", "prepare_error"
	}
	if e.Missing || e.Truncated || p.Previous.Truncated || p.Current.Truncated || !validShadowEvidence(e, p) {
		return ShadowResponse{}, "", "insufficient_evidence"
	}
	request := ShadowRequest{Point: p, Contract: shadowContract, Evidence: e}
	encoded, err := json.Marshal(request)
	if err != nil || len(encoded) > 8192 {
		return ShadowResponse{}, "", "input_budget"
	}
	digest := shadowDigest(string(encoded))
	// Detach the callback's map before handing evidence to the judge.
	request = ShadowRequest{}
	if json.Unmarshal(encoded, &request) != nil {
		return ShadowResponse{}, digest, "invalid"
	}
	raw, err := o.cfg.Judge(ctx, request)
	if ctx.Err() != nil {
		return ShadowResponse{}, digest, shadowContextStatus(ctx)
	}
	if err != nil {
		return ShadowResponse{}, digest, "judge_error"
	}
	response, err := decodeShadowResponse(raw, e)
	if err != nil {
		return ShadowResponse{}, digest, "invalid"
	}
	return response, digest, "evaluated"
}

func shadowContextStatus(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "timeout"
	}
	return "canceled"
}

func validShadowEvidence(e ShadowEvidence, p ShadowPoint) bool {
	if e.Objective == "" || e.Acceptance == "" || !utf8.ValidString(e.Objective+e.Acceptance) || len(e.Excerpts) == 0 {
		return false
	}
	allowed := map[string]bool{}
	for _, round := range []ShadowRound{p.Previous, p.Current} {
		covered := false
		for _, v := range round.Tools {
			allowed[v.ID] = true
			if e.Excerpts[v.ID] != "" {
				covered = true
			}
		}
		if !covered {
			return false
		}
	}
	for id, text := range e.Excerpts {
		if !allowed[id] || text == "" || !utf8.ValidString(text) {
			return false
		}
	}
	return true
}

func decodeShadowResponse(raw []byte, e ShadowEvidence) (ShadowResponse, error) {
	var r ShadowResponse
	invalid := errors.New("invalid shadow response")
	if len(raw) > 2048 || !utf8.Valid(raw) {
		return r, invalid
	}
	if rejectDuplicateJSONKeys(raw) != nil {
		return r, invalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&r) != nil {
		return r, invalid
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return r, invalid
	}
	for _, a := range []ShadowAnswer{r.SameFailure, r.NewEvidence, r.AlternativeSupported} {
		if a.Value != "yes" && a.Value != "no" && a.Value != "unknown" {
			return r, invalid
		}
		if a.Value != "unknown" && len(a.EvidenceIDs) == 0 {
			return r, invalid
		}
		for _, id := range a.EvidenceIDs {
			if _, ok := e.Excerpts[id]; !ok {
				return r, invalid
			}
		}
	}
	return r, nil
}

func shadowAdvisory(r ShadowResponse) string {
	if r.SameFailure.Value == "unknown" || r.NewEvidence.Value == "unknown" || r.AlternativeSupported.Value == "unknown" {
		return "abstain"
	}
	if r.NewEvidence.Value == "yes" {
		return "continue"
	}
	if r.SameFailure.Value == "yes" {
		if r.AlternativeSupported.Value == "yes" {
			return "consider_alternative"
		}
		return "gather_evidence"
	}
	return "continue"
}

// close does not wait for provider IO. Pending work is canceled and late
// results cannot mutate the returned audit. Adapters own cancellation of IO.
func (o *shadowObserver) close() []ShadowJudgmentRecord {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil
	}
	o.closed = true
	o.cancel()
	for i := range o.records {
		if o.records[i].Status == "pending" {
			o.records[i].Status = "canceled"
		}
	}
	return append([]ShadowJudgmentRecord(nil), o.records...)
}
