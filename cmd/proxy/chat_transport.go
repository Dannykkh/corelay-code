package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

const (
	maxAgentSSELineBytes       = 1 << 20
	maxAgentSSEEventBytes      = 1 << 20
	maxAgentHTTPErrorBodyBytes = 4 << 10
	agentRecoveryWindow        = 20 * time.Second
	agentRecoveryPoll          = 300 * time.Millisecond
)

var (
	errAgentSSELineTooLong  = errors.New("agent SSE line exceeds 1 MiB")
	errAgentSSEEventTooLong = errors.New("agent SSE event exceeds 1 MiB")
)

// agentStreamTransport contains no terminal rendering policy. It is shared by
// full-screen and line-oriented clients that consume the Corelay Code HTTP API.
type agentStreamTransport struct {
	baseURL     string
	accessToken string
	client      *http.Client
}

type serverHealth struct {
	Status   string `json:"status"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type serverConfig struct {
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	ResponseLang  string `json:"responseLang"`
	RouterEnabled bool   `json:"routerEnabled"`
}

type serverRoot struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Router   bool   `json:"router"`
}

type activeLoopInfo struct {
	SessionID string    `json:"sessionId"`
	WorkDir   string    `json:"workDir"`
	StartedAt time.Time `json:"startedAt"`
}

type sessionSaveResult struct {
	ID       string `json:"id"`
	Version  int    `json:"version"`
	Revision uint64 `json:"revision"`
}

type sessionDeleteResult struct {
	OK      bool                      `json:"ok"`
	Cleanup agent.SessionDeleteResult `json:"cleanup"`
}

type approvalResolution struct {
	ID         string    `json:"id"`
	Decision   string    `json:"decision"`
	Reason     string    `json:"reason,omitempty"`
	ResolvedAt time.Time `json:"resolvedAt"`
}

type agentCancelResult struct {
	SessionID string `json:"sessionId"`
	Cancelled bool   `json:"cancelled"`
}

type agentWireEvent struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// Exactly one of Event, Err, or EOF is meaningful in an agentStreamItem.
type agentStreamItem struct {
	Event agentWireEvent
	Err   error
	EOF   bool
}

type agentTurnRequest struct {
	Messages         []chatMsg                     `json:"messages"`
	WorkDir          string                        `json:"workDir"`
	ResponseLang     string                        `json:"responseLang"`
	ExecutionPolicy  *agent.ExecutionPolicyRequest `json:"executionPolicy,omitempty"`
	DurableSessionID string                        `json:"durableSessionId,omitempty"`
	ExpectedRevision *uint64                       `json:"expectedRevision,omitempty"`
	RequestID        string                        `json:"requestId,omitempty"`
}

func parseExecutionModeFlag(value string) (agent.ExecutionMode, error) {
	mode := agent.ExecutionMode(strings.TrimSpace(value))
	if mode == "" || mode == agent.ExecutionModeReadOnly || mode == agent.ExecutionModeWorkspace || mode == agent.ExecutionModeFull {
		return mode, nil
	}
	return "", fmt.Errorf("invalid execution mode %q; expected read-only, workspace, or full", value)
}

func requestedExecutionPolicy(mode agent.ExecutionMode) *agent.ExecutionPolicyRequest {
	if mode == "" {
		return nil
	}
	return &agent.ExecutionPolicyRequest{Mode: mode}
}

func validEffectiveExecutionPolicy(mode agent.ExecutionMode, revision uint64) bool {
	return revision > 0 && (mode == agent.ExecutionModeReadOnly || mode == agent.ExecutionModeWorkspace || mode == agent.ExecutionModeFull)
}

func resolveCLIExecutionPolicy(
	workDir string,
	requestedMode agent.ExecutionMode,
) (sandbox.Runner, sandbox.Policy, agent.ExecutionPolicySnapshot, error) {
	effectiveMode := requestedMode
	if effectiveMode == "" {
		effectiveMode = agent.ExecutionModeWorkspace
	}
	runner, policy := agent.SandboxExecutionForMode(workDir, effectiveMode)
	snapshot, err := agent.ResolveExecutionPolicy(
		agent.ExecutionPolicyRequest{Mode: requestedMode},
		"",
		runner.Capabilities(),
	)
	if err != nil {
		return nil, sandbox.Policy{}, agent.ExecutionPolicySnapshot{}, err
	}
	return runner, policy, snapshot, nil
}

// agentHTTPError is safe to render in a terminal. Body is capped at 4 KiB,
// stripped of terminal control bytes, and redacted for the configured token.
type agentHTTPError struct {
	StatusCode int
	Body       string
}

func (e *agentHTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Body)
}

func newAgentStreamTransport(base, token string, client *http.Client) *agentStreamTransport {
	if client == nil {
		client = http.DefaultClient
	}
	return &agentStreamTransport{
		baseURL:     strings.TrimRight(strings.TrimSpace(base), "/"),
		accessToken: token,
		client:      client,
	}
}

func (t *agentStreamTransport) Health(ctx context.Context) (serverHealth, error) {
	var result serverHealth
	err := t.doJSON(ctx, http.MethodGet, "/health", nil, nil, &result)
	return result, err
}

func (t *agentStreamTransport) Config(ctx context.Context) (serverConfig, error) {
	var result serverConfig
	err := t.doJSON(ctx, http.MethodGet, "/api/config", nil, nil, &result)
	return result, err
}

func (t *agentStreamTransport) SetConfig(ctx context.Context, provider, model string) (serverConfig, error) {
	var result serverConfig
	body := map[string]string{"provider": provider, "model": model}
	err := t.doJSON(ctx, http.MethodPut, "/api/config", nil, body, &result)
	return result, err
}

func (t *agentStreamTransport) Root(ctx context.Context) (serverRoot, error) {
	var result serverRoot
	err := t.doJSON(ctx, http.MethodGet, "/", nil, nil, &result)
	return result, err
}

func (t *agentStreamTransport) Commands(ctx context.Context, workDir string) ([]agent.SlashCommand, error) {
	query := url.Values{}
	if workDir != "" {
		query.Set("workDir", workDir)
	}
	var result []agent.SlashCommand
	if err := t.doJSON(ctx, http.MethodGet, "/api/commands", query, nil, &result); err != nil {
		return nil, err
	}
	if result == nil {
		result = []agent.SlashCommand{}
	}
	return result, nil
}

func (t *agentStreamTransport) ActiveLoops(ctx context.Context, workDir string) ([]activeLoopInfo, error) {
	query := url.Values{}
	if workDir != "" {
		query.Set("workDir", workDir)
	}
	var response struct {
		Loops []activeLoopInfo `json:"loops"`
	}
	if err := t.doJSON(ctx, http.MethodGet, "/api/agent/loops", query, nil, &response); err != nil {
		return nil, err
	}
	if response.Loops == nil {
		response.Loops = []activeLoopInfo{}
	}
	return response.Loops, nil
}

func (t *agentStreamTransport) ListSessions(ctx context.Context, workDir string) ([]agent.SessionSummary, error) {
	query := url.Values{}
	if workDir != "" {
		query.Set("workspace", workDir)
	}
	var result []agent.SessionSummary
	if err := t.doJSON(ctx, http.MethodGet, "/api/sessions", query, nil, &result); err != nil {
		return nil, err
	}
	if result == nil {
		result = []agent.SessionSummary{}
	}
	return result, nil
}

func (t *agentStreamTransport) GetSession(ctx context.Context, id string) (*agent.Session, error) {
	var result agent.Session
	if err := t.doJSON(ctx, http.MethodGet, sessionPath(id), nil, nil, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (t *agentStreamTransport) SaveSession(
	ctx context.Context,
	session *agent.Session,
	expectedRevision *uint64,
) (sessionSaveResult, error) {
	var result sessionSaveResult
	if session == nil {
		return result, errors.New("session is required")
	}
	body := struct {
		agent.Session
		ExpectedRevision *uint64 `json:"expectedRevision,omitempty"`
	}{
		Session:          *session,
		ExpectedRevision: expectedRevision,
	}
	err := t.doJSON(ctx, http.MethodPost, "/api/sessions", nil, body, &result)
	return result, err
}

func (t *agentStreamTransport) ForkSession(ctx context.Context, id string, revision uint64) (*agent.Session, error) {
	var response struct {
		Session *agent.Session `json:"session"`
	}
	if err := t.doJSON(
		ctx,
		http.MethodPost,
		sessionPath(id)+"/fork",
		nil,
		map[string]uint64{"expectedRevision": revision},
		&response,
	); err != nil {
		return nil, err
	}
	if response.Session == nil {
		return nil, errors.New("fork response did not include a session")
	}
	return response.Session, nil
}

func (t *agentStreamTransport) ReconciliationPreview(
	ctx context.Context,
	id string,
	revision uint64,
) (agent.SessionReconciliationAssessment, error) {
	var assessment agent.SessionReconciliationAssessment
	err := t.doJSON(
		ctx,
		http.MethodGet,
		sessionPath(id)+"/reconcile-preview",
		url.Values{"expectedRevision": []string{fmt.Sprintf("%d", revision)}},
		nil,
		&assessment,
	)
	return assessment, err
}

func (t *agentStreamTransport) ReconcileSession(
	ctx context.Context,
	id string,
	revision uint64,
	evidenceDigest string,
	manualConfirmationAcknowledged bool,
) (*agent.Session, error) {
	var response struct {
		Session *agent.Session `json:"session"`
	}
	err := t.doJSON(
		ctx,
		http.MethodPost,
		sessionPath(id)+"/reconcile",
		nil,
		struct {
			ExpectedRevision               uint64 `json:"expectedRevision"`
			EvidenceDigest                 string `json:"evidenceDigest"`
			ManualConfirmationAcknowledged bool   `json:"manualConfirmationAcknowledged"`
		}{
			ExpectedRevision:               revision,
			EvidenceDigest:                 evidenceDigest,
			ManualConfirmationAcknowledged: manualConfirmationAcknowledged,
		},
		&response,
	)
	if err != nil {
		return nil, err
	}
	if response.Session == nil {
		return nil, errors.New("reconcile response did not include a session")
	}
	return response.Session, nil
}

func (t *agentStreamTransport) CloseSession(ctx context.Context, id string, revision uint64) (*agent.Session, error) {
	return t.mutateSessionLifecycle(ctx, id, "close", revision)
}

func (t *agentStreamTransport) DeleteSession(ctx context.Context, id string, revision uint64) error {
	var result sessionDeleteResult
	return t.doJSON(
		ctx,
		http.MethodDelete,
		sessionPath(id),
		nil,
		map[string]uint64{"expectedRevision": revision},
		&result,
	)
}

func (t *agentStreamTransport) ResolveApproval(
	ctx context.Context,
	approvalID,
	sessionID,
	decision string,
) error {
	var result approvalResolution
	return t.doJSON(
		ctx,
		http.MethodPost,
		"/api/approvals/"+url.PathEscape(approvalID)+"/resolve",
		nil,
		map[string]string{"sessionId": sessionID, "decision": decision},
		&result,
	)
}

func (t *agentStreamTransport) CancelRun(ctx context.Context, sessionID string) error {
	var result agentCancelResult
	return t.doJSON(
		ctx,
		http.MethodPost,
		"/api/agent/"+url.PathEscape(sessionID)+"/cancel",
		nil,
		struct{}{},
		&result,
	)
}

func (t *agentStreamTransport) mutateSessionLifecycle(
	ctx context.Context,
	id,
	action string,
	revision uint64,
) (*agent.Session, error) {
	var response struct {
		Session *agent.Session `json:"session"`
	}
	if err := t.doJSON(
		ctx,
		http.MethodPost,
		sessionPath(id)+"/"+action,
		nil,
		map[string]uint64{"expectedRevision": revision},
		&response,
	); err != nil {
		return nil, err
	}
	if response.Session == nil {
		return nil, fmt.Errorf("%s response did not include a session", action)
	}
	return response.Session, nil
}

func sessionPath(id string) string {
	return "/api/sessions/" + url.PathEscape(id)
}

func (t *agentStreamTransport) doJSON(
	ctx context.Context,
	method,
	path string,
	query url.Values,
	body any,
	target any,
) error {
	var encoded io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		encoded = bytes.NewReader(data)
	}
	req, err := t.newRequest(ctx, method, path, query, encoded)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		return t.httpError(resp)
	}
	if target == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		return fmt.Errorf("decode HTTP %d response: %w", resp.StatusCode, err)
	}
	return nil
}

func (t *agentStreamTransport) newRequest(
	ctx context.Context,
	method,
	path string,
	query url.Values,
	body io.Reader,
) (*http.Request, error) {
	endpoint := t.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	if path != "/health" {
		req.Header.Set("X-Access-Token", t.accessToken)
	}
	return req, nil
}

func (t *agentStreamTransport) httpError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxAgentHTTPErrorBodyBytes))
	body := strings.Map(func(char rune) rune {
		if char == '\n' || char == '\t' || !unicode.IsControl(char) {
			return char
		}
		return -1
	}, strings.ToValidUTF8(string(data), ""))
	if t.accessToken != "" {
		for strings.Contains(body, t.accessToken) {
			body = strings.ReplaceAll(body, t.accessToken, "")
		}
	}
	body = truncateUTF8Bytes(strings.TrimSpace(body), maxAgentHTTPErrorBodyBytes)
	return &agentHTTPError{StatusCode: resp.StatusCode, Body: body}
}

func truncateUTF8Bytes(value string, limit int) string {
	if limit < 1 || len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

// StartTurn starts one logical turn and recovers an uncertain transport ending through
// the durable session receipt. A retry always reuses the exact request body.
func (t *agentStreamTransport) StartTurn(ctx context.Context, turn agentTurnRequest) <-chan agentStreamItem {
	body, err := json.Marshal(turn)
	if err != nil {
		items := make(chan agentStreamItem, 1)
		items <- agentStreamItem{Err: err}
		close(items)
		return items
	}
	if turn.RequestID == "" || turn.DurableSessionID == "" || turn.ExpectedRevision == nil {
		return t.startTurnOnce(ctx, body)
	}
	revision := *turn.ExpectedRevision
	turn.ExpectedRevision = &revision
	items := make(chan agentStreamItem, 16)
	go t.startRecoverableTurn(ctx, turn, body, items)
	return items
}

// startTurnOnce owns exactly one POST and preserves the bounded SSE parser.
func (t *agentStreamTransport) startTurnOnce(ctx context.Context, body []byte) <-chan agentStreamItem {
	items := make(chan agentStreamItem, 16)
	go func() {
		defer close(items)
		req, err := t.newRequest(ctx, http.MethodPost, "/api/agent", nil, bytes.NewReader(body))
		if err != nil {
			emitAgentTerminal(items, agentStreamItem{Err: err})
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")

		resp, err := t.client.Do(req)
		if err != nil {
			emitAgentTerminal(items, agentStreamItem{Err: err})
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= http.StatusBadRequest {
			emitAgentTerminal(items, agentStreamItem{Err: t.httpError(resp)})
			return
		}

		reader := bufio.NewReaderSize(resp.Body, 64<<10)
		var dataLines [][]byte
		dataBytes := 0
		for {
			line, readErr := readAgentSSELine(reader)
			if len(line) > 0 || readErr == nil {
				if len(line) == 0 {
					if len(dataLines) > 0 {
						if !t.emitSSEEvent(ctx, items, dataLines) {
							return
						}
						dataLines = nil
						dataBytes = 0
					}
				} else if data, ok := sseData(line); ok {
					separator := 0
					if len(dataLines) > 0 {
						separator = 1
					}
					if dataBytes+separator+len(data) > maxAgentSSEEventBytes {
						emitAgentTerminal(items, agentStreamItem{Err: errAgentSSEEventTooLong})
						return
					}
					dataLines = append(dataLines, data)
					dataBytes += separator + len(data)
				}
			}

			switch {
			case readErr == nil:
				continue
			case errors.Is(readErr, io.EOF):
				if len(dataLines) > 0 && !t.emitSSEEvent(ctx, items, dataLines) {
					return
				}
				emitAgentTerminal(items, agentStreamItem{EOF: true})
				return
			default:
				emitAgentTerminal(items, agentStreamItem{Err: readErr})
				return
			}
		}
	}()
	return items
}

type agentTurnRecoveryState struct {
	text      strings.Builder
	runtimeID string
	material  bool
	tooMuch   bool
	done      bool
}

func (s *agentTurnRecoveryState) observe(event agentWireEvent) {
	switch event.Type {
	case "done":
		s.done = true
	case "session":
		var value struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(event.Data, &value) == nil {
			s.runtimeID = value.SessionID
		}
	case "text":
		s.material = true
		var value string
		if json.Unmarshal(event.Data, &value) != nil || s.text.Len()+len(value) > maxAgentSSEEventBytes {
			s.tooMuch = true
			return
		}
		s.text.WriteString(value)
	case "heartbeat", "status", "workstream", "context_plan", "stream_end":
	default:
		s.material = true
	}
}

func (t *agentStreamTransport) startRecoverableTurn(ctx context.Context, turn agentTurnRequest, body []byte, items chan<- agentStreamItem) {
	defer close(items)
	state := &agentTurnRecoveryState{}
	var deadline time.Time
	recoveryCtx := ctx
	var last agentStreamItem
	for post := 0; post < 3; post++ {
		if post > 0 {
			if !waitAgentRecovery(recoveryCtx, agentRecoveryPoll) {
				err := recoveryCtx.Err()
				if ctx.Err() != nil {
					err = ctx.Err()
				}
				emitAgentTerminal(items, agentStreamItem{Err: err})
				return
			}
		}
		for item := range t.startTurnOnce(recoveryCtx, body) {
			if item.Err != nil || item.EOF {
				last = item
				continue
			}
			state.observe(item.Event)
			items <- item
		}
		if state.done || ctx.Err() != nil || state.tooMuch || !retryableAgentTurnEnd(last, post > 0) {
			if ctx.Err() != nil {
				last = agentStreamItem{Err: ctx.Err()}
			}
			emitAgentTerminal(items, last)
			return
		}
		if deadline.IsZero() {
			deadline = time.Now().Add(agentRecoveryWindow)
			var cancel context.CancelFunc
			recoveryCtx, cancel = context.WithDeadline(ctx, deadline)
			defer cancel()
		}
		session, err := t.GetSession(recoveryCtx, turn.DurableSessionID)
		if err != nil {
			if retryableAgentRecoveryRead(err) {
				break
			}
			if ctx.Err() != nil {
				last = agentStreamItem{Err: ctx.Err()}
			}
			emitAgentTerminal(items, last)
			return
		}
		if emitRecoveredAgentTurn(items, turn, session, state) {
			return
		}
		if state.material || session.Revision != *turn.ExpectedRevision {
			break
		}
	}
	for time.Now().Before(deadline) && recoveryCtx.Err() == nil {
		if state.runtimeID == "" && !state.material && last.EOF {
			break
		}
		if !waitAgentRecovery(recoveryCtx, agentRecoveryPoll) {
			break
		}
		session, err := t.GetSession(recoveryCtx, turn.DurableSessionID)
		if err != nil {
			if retryableAgentRecoveryRead(err) {
				continue
			}
			break
		}
		if emitRecoveredAgentTurn(items, turn, session, state) {
			return
		}
	}
	if ctx.Err() != nil {
		last = agentStreamItem{Err: ctx.Err()}
	}
	emitAgentTerminal(items, last)
}

func retryableAgentRecoveryRead(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var httpErr *agentHTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode >= http.StatusInternalServerError
	}
	var urlErr *url.Error
	return errors.As(err, &urlErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

func retryableAgentTurnEnd(item agentStreamItem, afterDisconnect bool) bool {
	if item.EOF {
		return true
	}
	if item.Err == nil || errors.Is(item.Err, context.Canceled) || errors.Is(item.Err, context.DeadlineExceeded) ||
		errors.Is(item.Err, errAgentSSELineTooLong) || errors.Is(item.Err, errAgentSSEEventTooLong) ||
		strings.HasPrefix(item.Err.Error(), "decode agent SSE event:") ||
		strings.Contains(item.Err.Error(), "agent SSE event is missing type") {
		return false
	}
	var httpErr *agentHTTPError
	if errors.As(item.Err, &httpErr) {
		if !afterDisconnect || httpErr.StatusCode != http.StatusConflict {
			return false
		}
		var envelope struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal([]byte(httpErr.Body), &envelope)
		return envelope.Error.Code == "session_run_active" || envelope.Error.Code == "session_revision_conflict" ||
			envelope.Error.Code == "session_reconcile_required"
	}
	return true
}

func emitRecoveredAgentTurn(items chan<- agentStreamItem, turn agentTurnRequest, session *agent.Session, state *agentTurnRecoveryState) bool {
	if session == nil || session.ID != turn.DurableSessionID || session.LastAgentRequest == nil ||
		session.LastAgentRequest.ID != turn.RequestID ||
		session.LastAgentRequest.ExpectedRevision != *turn.ExpectedRevision ||
		session.LastAgentRequest.CommittedRevision != session.Revision || state.tooMuch {
		return false
	}
	receipt := session.LastAgentRequest
	if receipt.MessageCount < 0 || receipt.MessageCount > len(session.Messages) {
		return false
	}
	var saved strings.Builder
	for _, message := range session.Messages[receipt.MessageCount:] {
		if message.Role == "assistant" {
			if saved.Len()+len(message.Content) > maxAgentSSEEventBytes {
				return false
			}
			saved.WriteString(message.Content)
		}
	}
	if !strings.HasPrefix(saved.String(), state.text.String()) {
		return false
	}
	if suffix := strings.TrimPrefix(saved.String(), state.text.String()); suffix != "" {
		data, _ := json.Marshal(suffix)
		items <- agentStreamItem{Event: agentWireEvent{Type: "text", Data: data}}
	}
	done := map[string]any{"kind": receipt.Kind}
	if receipt.Terminal != nil {
		encoded, _ := json.Marshal(receipt.Terminal)
		_ = json.Unmarshal(encoded, &done)
		done["kind"] = receipt.Kind
	}
	data, _ := json.Marshal(done)
	items <- agentStreamItem{Event: agentWireEvent{Type: "done", Data: data}}
	emitAgentTerminal(items, agentStreamItem{EOF: true})
	return true
}

func waitAgentRecovery(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (t *agentStreamTransport) emitSSEEvent(
	ctx context.Context,
	items chan<- agentStreamItem,
	dataLines [][]byte,
) bool {
	data := bytes.Join(dataLines, []byte("\n"))
	var event agentWireEvent
	if err := json.Unmarshal(data, &event); err != nil {
		emitAgentTerminal(items, agentStreamItem{Err: fmt.Errorf("decode agent SSE event: %w", err)})
		return false
	}
	if strings.TrimSpace(event.Type) == "" {
		emitAgentTerminal(items, agentStreamItem{Err: errors.New("agent SSE event is missing type")})
		return false
	}
	select {
	case items <- agentStreamItem{Event: event}:
		return true
	case <-ctx.Done():
		emitAgentTerminal(items, agentStreamItem{Err: ctx.Err()})
		return false
	}
}

func emitAgentTerminal(items chan<- agentStreamItem, item agentStreamItem) {
	// Keep the terminal item behind all already-delivered events. Consumers own
	// draining the returned channel, just as they own the stream context.
	items <- item
}

func sseData(line []byte) ([]byte, bool) {
	if len(line) == 0 || line[0] == ':' {
		return nil, false
	}
	field, value, found := bytes.Cut(line, []byte{':'})
	if !found || !bytes.Equal(field, []byte("data")) {
		return nil, false
	}
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	return value, true
}

func readAgentSSELine(reader *bufio.Reader) ([]byte, error) {
	line := make([]byte, 0, 256)
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(fragment) > 0 && fragment[len(fragment)-1] == '\n' {
			fragment = fragment[:len(fragment)-1]
			if len(fragment) > 0 && fragment[len(fragment)-1] == '\r' {
				fragment = fragment[:len(fragment)-1]
			}
		}
		if len(line)+len(fragment) > maxAgentSSELineBytes {
			return nil, errAgentSSELineTooLong
		}
		line = append(line, fragment...)

		switch {
		case err == nil:
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return line, err
		}
	}
}
