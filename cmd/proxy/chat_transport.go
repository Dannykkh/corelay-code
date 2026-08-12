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
)

const (
	maxAgentSSELineBytes       = 1 << 20
	maxAgentSSEEventBytes      = 1 << 20
	maxAgentHTTPErrorBodyBytes = 4 << 10
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
	Messages         []chatMsg `json:"messages"`
	WorkDir          string    `json:"workDir"`
	ResponseLang     string    `json:"responseLang"`
	DurableSessionID string    `json:"durableSessionId,omitempty"`
	ExpectedRevision *uint64   `json:"expectedRevision,omitempty"`
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

func (t *agentStreamTransport) ReconcileSession(ctx context.Context, id string, revision uint64) (*agent.Session, error) {
	return t.mutateSessionLifecycle(ctx, id, "reconcile", revision)
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

// StartTurn starts a single ordered SSE stream. On a clean HTTP EOF it emits
// one EOF item; protocol, transport, and context failures emit one Err item.
func (t *agentStreamTransport) StartTurn(ctx context.Context, turn agentTurnRequest) <-chan agentStreamItem {
	items := make(chan agentStreamItem, 16)
	go func() {
		defer close(items)

		body, err := json.Marshal(turn)
		if err != nil {
			emitAgentTerminal(items, agentStreamItem{Err: err})
			return
		}
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
