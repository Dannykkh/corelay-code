// Package acpbridge composes the transport-neutral ACP core with Corelay Code's
// existing agent loop and durable session store. It is an adapter, not a
// second agent loop: production execution delegates directly to
// agent.RunLoopWithOptions.
//
// Client-supplied stdio MCP definitions remain in runtime session state and
// are passed to the existing agent loop as an exact run-owned catalog. HTTP
// and SSE MCP transports remain capability-gated and fail closed.
package acpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Dannykkh/corelay-code/internal/acp"
	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/types"
)

const (
	defaultReasoningMode  = "hidden"
	progressReasoningMode = "progress"
	maxPromptBytes        = 1 << 20
	maxOutboundTextBytes  = 1 << 20
	maxSessionPageSize    = 100
)

// SessionStore is the durable subset used by the ACP composition. The
// concrete agent.SessionStore satisfies it; keeping this seam narrow makes the
// bridge independently testable without duplicating persistence behavior.
type SessionStore interface {
	SaveExpected(*agent.Session, uint64) error
	CommitInterruptedRun(*agent.Session, uint64, agent.SessionInterruption) error
	DeleteExpected(string, uint64) (agent.SessionDeleteResult, error)
	Get(string) (*agent.Session, error)
	List(string) []agent.SessionSummary
	ListAll() []agent.SessionSummary
	MarkInterrupted(string, uint64, agent.SessionInterruption) (*agent.Session, error)
	UpdateInterruptedRun(string, uint64, agent.SessionInterruption, agent.SessionInterruption) (*agent.Session, error)
	OpenToolResultMemory(string, string) (*agent.SessionMemory, []agent.ToolResultReference, error)
}

// Runner starts exactly one invocation of the existing Corelay Code loop and
// returns its event stream. Implementations must close the stream when the run
// finishes. AgentRunner is the production implementation.
type Runner interface {
	Start(
		context.Context,
		types.Provider,
		string,
		[]types.Message,
		string,
		agent.RunOptions,
	) (<-chan agent.Event, error)
}

type RunnerFunc func(
	context.Context,
	types.Provider,
	string,
	[]types.Message,
	string,
	agent.RunOptions,
) (<-chan agent.Event, error)

func (f RunnerFunc) Start(
	ctx context.Context,
	provider types.Provider,
	model string,
	messages []types.Message,
	workDir string,
	options agent.RunOptions,
) (<-chan agent.Event, error) {
	return f(ctx, provider, model, messages, workDir, options)
}

// AgentRunner delegates to the sole Corelay Code agent kernel.
type AgentRunner struct{}

func (AgentRunner) Start(
	ctx context.Context,
	provider types.Provider,
	model string,
	messages []types.Message,
	workDir string,
	options agent.RunOptions,
) (<-chan agent.Event, error) {
	events := make(chan agent.Event, 64)
	go agent.RunLoopWithOptions(ctx, provider, model, messages, workDir, options, events)
	return events, nil
}

type Options struct {
	Provider          types.Provider
	DefaultModel      string
	Store             SessionStore
	Runner            Runner
	ResponseLang      string
	Version           string
	ApprovalTTL       time.Duration
	LoopRegistry      *agent.LoopRegistry
	MCPRuntimeFactory agent.MCPRuntimeFactory
	MCPExecution      *agent.MCPExecutionOptions
}

type Backend struct {
	provider          types.Provider
	defaultModel      string
	store             SessionStore
	runner            Runner
	responseLang      string
	version           string
	approvalTTL       time.Duration
	loops             *agent.LoopRegistry
	mcpRuntimeFactory agent.MCPRuntimeFactory
	mcpExecution      *agent.MCPExecutionOptions

	mu       sync.Mutex
	sessions map[string]*runtimeSession
}

type runtimeSession struct {
	persisted   agent.Session
	toolResults *agent.SessionMemory
	resultRefs  []agent.ToolResultReference
	mcpServers  []agent.MCPServerSpec
	mcpRuntime  agent.MCPRuntime
	mcpCancel   context.CancelFunc
	reasoning   string
	cancel      context.CancelFunc
	done        chan struct{}
	activeID    string
	loading     bool
	quarantined bool
}

// promptExecutionJournal is the ACP composition for the shared kernel's
// synchronous pre-execution durability seam. It is scoped to one exact
// runtimeSession pointer and LoopRegistry invocation; it never becomes a
// second execution loop or a transport-owned tool lifecycle.
type promptExecutionJournal struct {
	backend          *Backend
	sessionID        string
	state            *runtimeSession
	activeID         string
	mu               sync.Mutex
	persisted        agent.Session
	expectedRevision uint64
	marker           *agent.SessionInterruption
	entries          []agent.ToolExecutionJournalEntry
	failed           bool
}

const executionJournalReason = "Tool execution began; a successful terminal must atomically commit or explicit reconciliation is required."

func New(options Options) (*Backend, error) {
	if options.Provider == nil || options.Store == nil {
		return nil, errors.New("acp bridge requires a provider and session store")
	}
	model := strings.TrimSpace(options.DefaultModel)
	if model == "" {
		return nil, errors.New("acp bridge requires a default model")
	}
	if options.Runner == nil {
		options.Runner = AgentRunner{}
	}
	if strings.TrimSpace(options.ResponseLang) == "" {
		options.ResponseLang = "auto"
	}
	if strings.TrimSpace(options.Version) == "" {
		options.Version = "dev"
	}
	if options.ApprovalTTL <= 0 {
		options.ApprovalTTL = 5 * time.Minute
	}
	if options.LoopRegistry == nil {
		options.LoopRegistry = agent.NewLoopRegistry(3)
	}
	return &Backend{
		provider:          options.Provider,
		defaultModel:      model,
		store:             options.Store,
		runner:            options.Runner,
		responseLang:      options.ResponseLang,
		version:           strings.TrimSpace(options.Version),
		approvalTTL:       options.ApprovalTTL,
		loops:             options.LoopRegistry,
		mcpRuntimeFactory: options.MCPRuntimeFactory,
		mcpExecution:      cloneMCPExecutionOptions(options.MCPExecution),
		sessions:          make(map[string]*runtimeSession),
	}, nil
}

func newPromptExecutionJournal(
	backend *Backend,
	sessionID string,
	state *runtimeSession,
	activeID string,
	persisted agent.Session,
) *promptExecutionJournal {
	return &promptExecutionJournal{
		backend:          backend,
		sessionID:        sessionID,
		state:            state,
		activeID:         activeID,
		persisted:        cloneSession(persisted),
		expectedRevision: persisted.Revision,
	}
}

// Record persists an exact or aggregate content-free marker before the shared
// kernel can emit tool_execution_start or invoke an executor. Store failures
// are intentionally collapsed to a fixed error so paths and persistence
// details cannot cross back into model or ACP-visible text.
func (journal *promptExecutionJournal) Record(entry agent.ToolExecutionJournalEntry) error {
	if journal == nil || journal.backend == nil || journal.state == nil {
		return errors.New("ACP execution journal unavailable")
	}
	entry = agent.ToolExecutionJournalEntry{
		ID: strings.TrimSpace(entry.ID), Name: strings.TrimSpace(entry.Name),
		InputDigest: strings.TrimSpace(entry.InputDigest), RunID: strings.TrimSpace(entry.RunID),
	}
	firstMarker, err := agent.AggregateToolExecutionJournal(
		[]agent.ToolExecutionJournalEntry{entry}, time.Time{}, executionJournalReason,
	)
	if err != nil {
		journal.fail()
		return errors.New("ACP execution journal unavailable")
	}

	journal.mu.Lock()
	defer journal.mu.Unlock()
	if journal.failed || !journal.active() {
		journal.failLocked()
		return errors.New("ACP execution journal unavailable")
	}

	if journal.marker == nil {
		if journal.persisted.ReconcileRequired || journal.persisted.Revision != journal.expectedRevision {
			journal.failLocked()
			return errors.New("ACP execution journal unavailable")
		}
		updated, markErr := journal.backend.store.MarkInterrupted(
			journal.sessionID, journal.expectedRevision, firstMarker,
		)
		if markErr != nil || updated == nil || updated.Interruption == nil ||
			updated.Interruption.RunID != firstMarker.RunID {
			journal.failLocked()
			return errors.New("ACP execution journal unavailable")
		}
		journal.persisted = cloneSession(*updated)
		journal.expectedRevision = updated.Revision
		journal.marker = cloneSessionInterruption(updated.Interruption)
		journal.entries = []agent.ToolExecutionJournalEntry{entry}
		if !journal.publish(*updated) {
			journal.failLocked()
			return errors.New("ACP execution journal unavailable")
		}
		return nil
	}

	persisted, loadErr := journal.backend.store.Get(journal.sessionID)
	if loadErr != nil || persisted == nil || persisted.Revision != journal.expectedRevision ||
		!persisted.ReconcileRequired || persisted.Interruption == nil ||
		!sameSessionInterruption(persisted.Interruption, journal.marker) ||
		persisted.Interruption.RunID != firstMarker.RunID {
		journal.failLocked()
		return errors.New("ACP execution journal unavailable")
	}
	nextEntries := append(append([]agent.ToolExecutionJournalEntry(nil), journal.entries...), entry)
	nextMarker, aggregateErr := agent.AggregateToolExecutionJournal(
		nextEntries, journal.marker.At, executionJournalReason,
	)
	if aggregateErr != nil {
		journal.failLocked()
		return errors.New("ACP execution journal unavailable")
	}
	updated, updateErr := journal.backend.store.UpdateInterruptedRun(
		journal.sessionID, journal.expectedRevision, *journal.marker, nextMarker,
	)
	if updateErr != nil || updated == nil || updated.Interruption == nil ||
		!sameSessionInterruption(updated.Interruption, &nextMarker) {
		journal.failLocked()
		return errors.New("ACP execution journal unavailable")
	}
	journal.persisted = cloneSession(*updated)
	journal.expectedRevision = updated.Revision
	journal.marker = cloneSessionInterruption(updated.Interruption)
	journal.entries = nextEntries
	if !journal.publish(*updated) {
		journal.failLocked()
		return errors.New("ACP execution journal unavailable")
	}
	return nil
}

func (journal *promptExecutionJournal) active() bool {
	journal.backend.mu.Lock()
	defer journal.backend.mu.Unlock()
	current := journal.backend.sessions[journal.sessionID]
	return current == journal.state && current != nil && current.cancel != nil &&
		current.activeID == journal.activeID && !current.loading && !current.quarantined
}

func (journal *promptExecutionJournal) publish(persisted agent.Session) bool {
	journal.backend.mu.Lock()
	defer journal.backend.mu.Unlock()
	current := journal.backend.sessions[journal.sessionID]
	if current != journal.state || current == nil || current.cancel == nil || current.activeID != journal.activeID {
		return false
	}
	current.persisted = cloneSession(persisted)
	return true
}

func (journal *promptExecutionJournal) fail() {
	if journal == nil {
		return
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	journal.failLocked()
}

func (journal *promptExecutionJournal) failLocked() {
	journal.failed = true
	if journal.backend == nil {
		return
	}
	journal.backend.mu.Lock()
	if current := journal.backend.sessions[journal.sessionID]; current == journal.state && current != nil {
		current.quarantined = true
	}
	journal.backend.mu.Unlock()
}

func (journal *promptExecutionJournal) guarded() bool {
	if journal == nil {
		return false
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	return journal.marker != nil
}

func (journal *promptExecutionJournal) failedWithoutGuard() bool {
	if journal == nil {
		return false
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	return journal.failed && journal.marker == nil
}

// EnrichReconciliation replaces the exact pre-execution aggregate with the
// observer's more complete applied/may-have-applied state. It is still a
// same-run exact-marker CAS; failure never adopts another revision.
func (journal *promptExecutionJournal) EnrichReconciliation(next *agent.SessionInterruption) error {
	if journal == nil || next == nil {
		return nil
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	if journal.failed || journal.marker == nil || !journal.active() ||
		strings.TrimSpace(next.RunID) != journal.marker.RunID {
		journal.failLocked()
		return errors.New("ACP execution journal unavailable")
	}
	if err := agent.ValidateSessionInterruption(*next); err != nil {
		journal.failLocked()
		return errors.New("ACP execution journal unavailable")
	}
	persisted, err := journal.backend.store.Get(journal.sessionID)
	if err != nil || persisted == nil || persisted.Revision != journal.expectedRevision ||
		!persisted.ReconcileRequired || persisted.Interruption == nil ||
		!sameSessionInterruption(persisted.Interruption, journal.marker) {
		journal.failLocked()
		return errors.New("ACP execution journal unavailable")
	}
	if sameSessionInterruption(journal.marker, next) {
		return nil
	}
	updated, err := journal.backend.store.UpdateInterruptedRun(
		journal.sessionID, journal.expectedRevision, *journal.marker, *next,
	)
	if err != nil || updated == nil || updated.Interruption == nil ||
		!sameSessionInterruption(updated.Interruption, next) {
		journal.failLocked()
		return errors.New("ACP execution journal unavailable")
	}
	journal.persisted = cloneSession(*updated)
	journal.expectedRevision = updated.Revision
	journal.marker = cloneSessionInterruption(updated.Interruption)
	if !journal.publish(*updated) {
		journal.failLocked()
		return errors.New("ACP execution journal unavailable")
	}
	return nil
}

func successfulACPJournalTerminal(outcome runOutcome, policy string) bool {
	if outcome.Err != nil || outcome.TransportFailure || outcome.TerminalFailure != nil ||
		outcome.Observer == nil || !outcome.Observer.Completed() || policy != durablePolicyCommit {
		return false
	}
	switch outcome.TerminalKind {
	case "", string(agent.RunTerminalCompleted), string(agent.RunTerminalCommand):
		terminal, ok := outcome.Observer.TerminalMetadata()
		return !ok || !terminal.BlocksSuccess()
	default:
		return false
	}
}

func cloneSessionInterruption(value *agent.SessionInterruption) *agent.SessionInterruption {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func sameSessionInterruption(left, right *agent.SessionInterruption) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.At.Equal(right.At) && left.Reason == right.Reason && left.RunID == right.RunID &&
		left.ToolName == right.ToolName && left.ToolCallID == right.ToolCallID &&
		left.InputDigest == right.InputDigest && left.SideEffectState == right.SideEffectState &&
		left.Summary == right.Summary
}

func (b *Backend) Descriptor() acp.BackendDescriptor {
	return acp.BackendDescriptor{
		AgentInfo: acp.Implementation{
			Name:    "corelaycode",
			Title:   "Corelay Code",
			Version: b.version,
		},
		// The current RunLoop accepts text prompts and stable stdio MCP. HTTP
		// and SSE remain false until their transports have run-owned support.
		PromptCapabilities: acp.PromptCapabilities{},
		MCPCapabilities:    acp.MCPCapabilities{},
	}
}

func (b *Backend) Initialize(context.Context, acp.InitializeRequest, acp.Client) error {
	return nil
}

func (b *Backend) NewSession(
	ctx context.Context,
	request acp.NewSessionRequest,
	_ acp.Client,
) (acp.NewSessionResponse, error) {
	if err := ctx.Err(); err != nil {
		return acp.NewSessionResponse{}, err
	}
	if len(request.AdditionalDirectories) != 0 {
		return acp.NewSessionResponse{}, invalidParams("additional directories are unsupported")
	}
	mcpServers, err := sessionMCPServerSpecs(request.MCPServers)
	if err != nil {
		return acp.NewSessionResponse{}, err
	}
	workspace, err := canonicalWorkspace(request.CWD)
	if err != nil {
		return acp.NewSessionResponse{}, invalidParams("invalid workspace")
	}
	session := agent.Session{
		Workspace: workspace,
		Messages:  []agent.SessionMessage{},
		Provider:  b.provider.Name(),
		Model:     b.defaultModel,
	}
	if err := b.store.SaveExpected(&session, 0); err != nil {
		return acp.NewSessionResponse{}, mapStoreError(err)
	}
	toolResults, resultRefs, err := b.store.OpenToolResultMemory(session.ID, workspace)
	if err != nil {
		return acp.NewSessionResponse{}, mapStoreError(err)
	}
	b.mu.Lock()
	b.sessions[session.ID] = &runtimeSession{
		persisted: cloneSession(session), reasoning: defaultReasoningMode,
		toolResults: toolResults, resultRefs: append([]agent.ToolResultReference(nil), resultRefs...),
		mcpServers: cloneSessionMCPServers(mcpServers),
	}
	options := b.configOptionsLocked(b.sessions[session.ID])
	b.mu.Unlock()
	return acp.NewSessionResponse{SessionID: session.ID, ConfigOptions: options}, nil
}

func (b *Backend) LoadSession(
	ctx context.Context,
	request acp.LoadSessionRequest,
	client acp.Client,
) (acp.LoadSessionResponse, error) {
	if err := ctx.Err(); err != nil {
		return acp.LoadSessionResponse{}, err
	}
	if len(request.AdditionalDirectories) != 0 {
		return acp.LoadSessionResponse{}, invalidParams("additional directories are unsupported")
	}
	mcpServers, err := sessionMCPServerSpecs(request.MCPServers)
	if err != nil {
		return acp.LoadSessionResponse{}, err
	}
	persisted, err := b.store.Get(request.SessionID)
	if err != nil {
		return acp.LoadSessionResponse{}, mapStoreError(err)
	}
	if !sameWorkspace(persisted.Workspace, request.CWD) {
		return acp.LoadSessionResponse{}, invalidParams("session workspace does not match cwd")
	}
	if err := b.validateLoadable(*persisted); err != nil {
		return acp.LoadSessionResponse{}, err
	}
	if persisted.Model == "" {
		persisted.Model = b.defaultModel
	}
	if persisted.Provider == "" {
		persisted.Provider = b.provider.Name()
	}
	toolResults, resultRefs, err := b.store.OpenToolResultMemory(persisted.ID, persisted.Workspace)
	if err != nil {
		return acp.LoadSessionResponse{}, mapStoreError(err)
	}

	b.mu.Lock()
	state := b.sessions[request.SessionID]
	if state != nil && (state.cancel != nil || state.loading || state.quarantined) {
		b.mu.Unlock()
		return acp.LoadSessionResponse{}, invalidRequest("session is active or requires recovery")
	}
	created := state == nil
	var previous runtimeSession
	if state == nil {
		state = &runtimeSession{reasoning: defaultReasoningMode, loading: true}
		b.sessions[request.SessionID] = state
	} else {
		previous = runtimeSession{
			persisted:   cloneSession(state.persisted),
			toolResults: state.toolResults,
			resultRefs:  append([]agent.ToolResultReference(nil), state.resultRefs...),
			mcpServers:  cloneSessionMCPServers(state.mcpServers),
			mcpRuntime:  state.mcpRuntime,
			mcpCancel:   state.mcpCancel,
			reasoning:   state.reasoning,
			cancel:      state.cancel,
			done:        state.done,
			activeID:    state.activeID,
			loading:     state.loading,
			quarantined: state.quarantined,
		}
		state.loading = true
	}
	state.persisted = cloneSession(*persisted)
	state.toolResults = toolResults
	state.resultRefs = append([]agent.ToolResultReference(nil), resultRefs...)
	state.mcpServers = cloneSessionMCPServers(mcpServers)
	state.mcpRuntime = nil
	state.mcpCancel = nil
	if state.reasoning == "" {
		state.reasoning = defaultReasoningMode
	}
	options := b.configOptionsLocked(state)
	b.mu.Unlock()

	if err := replayHistory(ctx, client, *persisted); err != nil {
		b.mu.Lock()
		if current := b.sessions[request.SessionID]; current == state && current.cancel == nil && current.loading {
			if created {
				delete(b.sessions, request.SessionID)
			} else {
				*current = previous
			}
		}
		b.mu.Unlock()
		return acp.LoadSessionResponse{}, err
	}
	b.mu.Lock()
	if current := b.sessions[request.SessionID]; current != state || !state.loading {
		b.mu.Unlock()
		return acp.LoadSessionResponse{}, internalFailure()
	}
	state.loading = false
	b.mu.Unlock()
	if previous.mcpCancel != nil {
		previous.mcpCancel()
	}
	if previous.mcpRuntime != nil {
		previous.mcpRuntime.Close()
	}
	return acp.LoadSessionResponse{ConfigOptions: options}, nil
}

func (b *Backend) ListSessions(
	ctx context.Context,
	request acp.ListSessionsRequest,
	_ acp.Client,
) (acp.ListSessionsResponse, error) {
	if err := ctx.Err(); err != nil {
		return acp.ListSessionsResponse{}, err
	}
	offset, err := decodeCursor(request.Cursor)
	if err != nil {
		return acp.ListSessionsResponse{}, invalidParams("invalid session cursor")
	}
	var summaries []agent.SessionSummary
	if request.CWD == nil {
		summaries = b.store.ListAll()
	} else {
		summaries = b.store.List(*request.CWD)
	}
	visible := make([]agent.SessionSummary, 0, len(summaries))
	for _, summary := range summaries {
		if summary.Provider != "" && summary.Provider != b.provider.Name() {
			continue
		}
		if summary.LifecycleStatus == agent.SessionLifecycleClosed ||
			summary.LifecycleStatus == agent.SessionLifecycleInterrupted ||
			summary.LifecycleStatus == agent.SessionLifecycleRecoveryNeeded ||
			summary.ReconcileRequired {
			continue
		}
		visible = append(visible, summary)
	}
	if offset > len(visible) {
		return acp.ListSessionsResponse{}, invalidParams("invalid session cursor")
	}
	end := offset + maxSessionPageSize
	if end > len(visible) {
		end = len(visible)
	}
	items := make([]acp.SessionInfo, 0, end-offset)
	for _, summary := range visible[offset:end] {
		title := sanitizeText(summary.Title, 1024)
		updated := summary.UpdatedAt.UTC().Format(time.RFC3339Nano)
		items = append(items, acp.SessionInfo{
			SessionID: summary.ID,
			CWD:       summary.Workspace,
			Title:     &title,
			UpdatedAt: &updated,
		})
	}
	var next *string
	if end < len(visible) {
		value := strconv.Itoa(end)
		next = &value
	}
	return acp.ListSessionsResponse{Sessions: items, NextCursor: next}, nil
}

// ResumeSession is the ACP durable-session entry point. Replay, workspace,
// lifecycle, provider, and model validation stay owned by LoadSession so the
// two advertised methods cannot drift.
func (b *Backend) ResumeSession(
	ctx context.Context,
	request acp.ResumeSessionRequest,
	client acp.Client,
) (acp.ResumeSessionResponse, error) {
	return b.LoadSession(ctx, acp.LoadSessionRequest{
		MCPServers:            request.MCPServers,
		CWD:                   request.CWD,
		AdditionalDirectories: request.AdditionalDirectories,
		SessionID:             request.SessionID,
		Meta:                  request.Meta,
	}, client)
}

// DeleteSession removes exactly the revision observed by this ACP runtime.
// When the session is not loaded, Get supplies a fresh revision and
// DeleteExpected still closes the race with another adapter. A stale loaded
// runtime therefore cannot delete a newer HTTP/Web commit.
func (b *Backend) DeleteSession(
	ctx context.Context,
	request acp.DeleteSessionRequest,
	_ acp.Client,
) (acp.DeleteSessionResponse, error) {
	if err := ctx.Err(); err != nil {
		return acp.DeleteSessionResponse{}, err
	}
	persisted, err := b.store.Get(request.SessionID)
	if err != nil {
		return acp.DeleteSessionResponse{}, mapStoreError(err)
	}

	b.mu.Lock()
	if err := ctx.Err(); err != nil {
		b.mu.Unlock()
		return acp.DeleteSessionResponse{}, err
	}
	state := b.sessions[request.SessionID]
	if state != nil {
		if state.loading || state.quarantined || state.cancel != nil {
			b.mu.Unlock()
			return acp.DeleteSessionResponse{}, invalidRequest("session is active or requires recovery")
		}
		persisted = &state.persisted
	}
	if _, err := b.store.DeleteExpected(request.SessionID, persisted.Revision); err != nil {
		b.mu.Unlock()
		return acp.DeleteSessionResponse{}, mapStoreError(err)
	}
	delete(b.sessions, request.SessionID)
	b.mu.Unlock()

	if state != nil {
		if state.mcpCancel != nil {
			state.mcpCancel()
		}
		if state.mcpRuntime != nil {
			state.mcpRuntime.Close()
		}
	}
	return acp.DeleteSessionResponse{}, nil
}

func (b *Backend) CloseSession(
	ctx context.Context,
	request acp.CloseSessionRequest,
	_ acp.Client,
) (acp.CloseSessionResponse, error) {
	b.mu.Lock()
	state := b.sessions[request.SessionID]
	var done <-chan struct{}
	if state != nil {
		if state.loading || state.quarantined {
			b.mu.Unlock()
			return acp.CloseSessionResponse{}, invalidRequest("session is loading or requires recovery")
		}
		done = state.done
		if state.cancel != nil {
			state.cancel()
		}
	}
	b.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return acp.CloseSessionResponse{}, ctx.Err()
		}
	}
	b.mu.Lock()
	if current := b.sessions[request.SessionID]; current != nil && current.quarantined {
		b.mu.Unlock()
		return acp.CloseSessionResponse{}, invalidRequest("session recovery state could not be persisted")
	}
	current := b.sessions[request.SessionID]
	delete(b.sessions, request.SessionID)
	b.mu.Unlock()
	if current != nil {
		if current.mcpCancel != nil {
			current.mcpCancel()
		}
		if current.mcpRuntime != nil {
			current.mcpRuntime.Close()
		}
	}
	return acp.CloseSessionResponse{}, nil
}

func (b *Backend) SetSessionConfigOption(
	ctx context.Context,
	request acp.SetSessionConfigOptionRequest,
	client acp.Client,
) (acp.SetSessionConfigOptionResponse, error) {
	if err := ctx.Err(); err != nil {
		return acp.SetSessionConfigOptionResponse{}, err
	}
	var value string
	if err := json.Unmarshal(request.Value, &value); err != nil {
		return acp.SetSessionConfigOptionResponse{}, invalidParams("config value must be a string")
	}
	b.mu.Lock()
	state := b.sessions[request.SessionID]
	if state == nil {
		b.mu.Unlock()
		return acp.SetSessionConfigOptionResponse{}, notFound("session not found")
	}
	if state.loading || state.quarantined || state.persisted.ReconcileRequired {
		b.mu.Unlock()
		return acp.SetSessionConfigOptionResponse{}, invalidRequest("session requires explicit recovery")
	}
	if state.cancel != nil {
		b.mu.Unlock()
		return acp.SetSessionConfigOptionResponse{}, invalidRequest("session has an active prompt")
	}
	switch request.ConfigID {
	case "model":
		if !b.supportsModel(value) {
			b.mu.Unlock()
			return acp.SetSessionConfigOptionResponse{}, invalidParams("model is not available")
		}
		candidate := cloneSession(state.persisted)
		candidate.Model = value
		if err := b.store.SaveExpected(&candidate, state.persisted.Revision); err != nil {
			b.mu.Unlock()
			return acp.SetSessionConfigOptionResponse{}, mapStoreError(err)
		}
		state.persisted = candidate
	case "reasoning":
		if value != defaultReasoningMode && value != progressReasoningMode {
			b.mu.Unlock()
			return acp.SetSessionConfigOptionResponse{}, invalidParams("reasoning mode is not available")
		}
		// This controls only sanitized progress visibility. Raw chain-of-thought
		// is never forwarded and provider-native effort is not claimed.
		state.reasoning = value
	default:
		b.mu.Unlock()
		return acp.SetSessionConfigOptionResponse{}, invalidParams("config option is not available")
	}
	options := b.configOptionsLocked(state)
	b.mu.Unlock()
	if err := client.SessionUpdate(ctx, acp.SessionNotification{
		SessionID: request.SessionID,
		Update: acp.SessionUpdate{
			SessionUpdate: "config_option_update",
			ConfigOptions: options,
		},
	}); err != nil {
		return acp.SetSessionConfigOptionResponse{}, err
	}
	return acp.SetSessionConfigOptionResponse{ConfigOptions: options}, nil
}

func (b *Backend) Prompt(
	ctx context.Context,
	request acp.PromptRequest,
	client acp.Client,
) (acp.PromptResponse, error) {
	prompt, err := promptText(request.Prompt)
	if err != nil {
		return acp.PromptResponse{}, err
	}
	if isUIOnlyCommand(prompt) {
		return acp.PromptResponse{}, invalidParams("UI-only command is unsupported over ACP; use session configuration instead")
	}
	b.mu.Lock()
	state := b.sessions[request.SessionID]
	if state == nil {
		b.mu.Unlock()
		return acp.PromptResponse{}, notFound("session not found")
	}
	if state.loading || state.quarantined || state.persisted.ReconcileRequired ||
		state.persisted.LifecycleStatus == agent.SessionLifecycleInterrupted ||
		state.persisted.LifecycleStatus == agent.SessionLifecycleRecoveryNeeded {
		b.mu.Unlock()
		return acp.PromptResponse{}, invalidRequest("session requires explicit recovery before another prompt")
	}
	if state.cancel != nil {
		b.mu.Unlock()
		return acp.PromptResponse{}, invalidRequest("session already has an active prompt")
	}
	persisted := cloneSession(state.persisted)
	reasoning := state.reasoning
	toolResults := state.toolResults
	resultRefs := append([]agent.ToolResultReference(nil), state.resultRefs...)
	b.mu.Unlock()

	activeID, promptCtx, release, err := b.loops.Register(ctx, persisted.Workspace)
	if err != nil {
		switch {
		case errors.Is(err, agent.ErrTooManyLoops):
			return acp.PromptResponse{}, invalidRequest("agent concurrency limit reached")
		case errors.Is(err, agent.ErrShuttingDown):
			return acp.PromptResponse{}, invalidRequest("agent is shutting down")
		default:
			return acp.PromptResponse{}, internalFailure()
		}
	}
	done := make(chan struct{})
	cancel := func() { b.loops.Cancel(activeID) }
	b.mu.Lock()
	if current := b.sessions[request.SessionID]; current != state || current.cancel != nil {
		b.mu.Unlock()
		release()
		return acp.PromptResponse{}, invalidRequest("session already has an active prompt")
	}
	state.cancel = cancel
	state.done = done
	state.activeID = activeID
	b.mu.Unlock()
	defer func() {
		cancel()
		release()
		b.mu.Lock()
		if current := b.sessions[request.SessionID]; current == state && current.done == done {
			current.cancel = nil
			current.done = nil
			current.activeID = ""
		}
		close(done)
		b.mu.Unlock()
	}()
	if err := promptCtx.Err(); err != nil {
		return acp.PromptResponse{}, err
	}

	runMCP, err := b.sessionMCPRuntime(promptCtx, request.SessionID, state, persisted.Workspace)
	if err != nil {
		return acp.PromptResponse{}, err
	}

	messages := sessionMessagesForRun(persisted.Messages)
	rawPrompt, _ := json.Marshal(prompt)
	messages = append(messages, types.Message{Role: "user", Content: rawPrompt})

	persisted.Messages = append(persisted.Messages, agent.SessionMessage{
		Role:      "user",
		Content:   sanitizeText(prompt, maxOutboundTextBytes),
		Timestamp: time.Now().UTC(),
	})
	previousTitle := persisted.Title
	if err := b.store.SaveExpected(&persisted, state.persisted.Revision); err != nil {
		return acp.PromptResponse{}, mapStoreError(err)
	}
	b.mu.Lock()
	if current := b.sessions[request.SessionID]; current == state {
		current.persisted = cloneSession(persisted)
	}
	b.mu.Unlock()
	if persisted.Title != previousTitle {
		title := sanitizeText(persisted.Title, 1024)
		if err := client.SessionUpdate(promptCtx, acp.SessionNotification{
			SessionID: request.SessionID,
			Update:    acp.SessionUpdate{SessionUpdate: "session_info_update", Title: &title},
		}); err != nil {
			return acp.PromptResponse{}, err
		}
	}

	requester := newApprovalRequester(
		client,
		activeID,
		request.SessionID,
		persisted.Revision,
		b.approvalTTL,
	)
	sandboxRunner, sandboxPolicy := agent.DefaultSandboxExecution(persisted.Workspace)
	executionJournal := newPromptExecutionJournal(
		b, request.SessionID, state, activeID, persisted,
	)
	events, err := b.runner.Start(promptCtx, b.provider, persisted.Model, messages, persisted.Workspace, agent.RunOptions{
		SessionID:            activeID,
		ApprovalRequester:    requester,
		ResponseLang:         b.responseLang,
		DisableWorkspaceMCP:  true,
		MCPRuntime:           runMCP,
		SandboxRunner:        sandboxRunner,
		SandboxPolicy:        sandboxPolicy,
		ToolResultStore:      toolResults,
		ToolResultReader:     toolResults,
		ToolResultReferences: resultRefs,
		PreExecutionJournal:  executionJournal.Record,
	})
	if err != nil || events == nil {
		return acp.PromptResponse{}, internalFailure()
	}
	messageID, err := newOpaqueID("msg_")
	if err != nil {
		return acp.PromptResponse{}, internalFailure()
	}
	outcome := b.consumeEvents(promptCtx, cancel, request.SessionID, activeID, messageID, reasoning, client, events)
	// Transport/client failures are not made committable by a trailing kernel
	// terminal. The client may not have observed the corresponding output, so
	// preserve the existing interruption policy and fail the request.
	if outcome.Err != nil && (outcome.TransportFailure || outcome.DurablePolicy != durablePolicyReconcile) {
		if marker := outcome.Observer.ReconciliationMarker(
			"ACP prompt ended before tool activity was durably completed; explicit reconciliation is required",
		); marker != nil {
			var err error
			if executionJournal.guarded() {
				err = executionJournal.EnrichReconciliation(marker)
			} else {
				err = b.markInterrupted(request.SessionID, *marker)
			}
			if err != nil {
				return acp.PromptResponse{}, internalFailure()
			}
		}
		return acp.PromptResponse{}, outcome.Err
	}

	policy := outcome.DurablePolicy
	if policy == "" {
		// Legacy done and the two historical bounded terminal events predate the
		// typed terminal envelope. Preserve their transcript behavior.
		policy = durablePolicyCommit
	}
	forcedReconcile := policy == durablePolicyReconcile
	if !forcedReconcile && outcome.Observer.ReconciliationRequired() {
		marker := outcome.Observer.ReconciliationMarker(
			"ACP prompt completed with ambiguous tool execution lifecycle; explicit reconciliation is required",
		)
		var markerErr error
		if marker == nil {
			markerErr = agent.ErrSessionReconcileRequired
		} else if executionJournal.guarded() {
			markerErr = executionJournal.EnrichReconciliation(marker)
		} else {
			markerErr = b.markInterrupted(request.SessionID, *marker)
		}
		if markerErr != nil {
			return acp.PromptResponse{}, internalFailure()
		}
		return acp.PromptResponse{}, mapStoreError(agent.ErrSessionReconcileRequired)
	}
	if policy == durablePolicyNone && executionJournal.guarded() {
		if marker := outcome.Observer.ReconciliationMarker(
			"ACP prompt completed without a committable durable terminal; explicit reconciliation is required",
		); marker != nil {
			if err := executionJournal.EnrichReconciliation(marker); err != nil {
				return acp.PromptResponse{}, internalFailure()
			}
		}
		return acp.PromptResponse{}, mapStoreError(agent.ErrSessionReconcileRequired)
	}
	if policy != durablePolicyNone && (!executionJournal.guarded() || successfulACPJournalTerminal(outcome, policy)) {
		if err := b.commitObservedRun(request.SessionID, outcome.Observer, executionJournal); err != nil {
			if !executionJournal.failedWithoutGuard() {
				if marker := outcomeInterruptionMarker(outcome,
					"ACP prompt terminal could not be durably committed; explicit reconciliation is required"); marker != nil {
					_ = b.markInterrupted(request.SessionID, *marker)
				}
			}
			return acp.PromptResponse{}, err
		}
	}
	if forcedReconcile {
		marker := outcomeInterruptionMarker(outcome,
			"ACP prompt reached a non-success terminal; explicit reconciliation is required")
		var markerErr error
		if executionJournal.guarded() {
			observerMarker := outcome.Observer.ReconciliationMarker(
				"ACP prompt reached a non-success terminal; explicit reconciliation is required",
			)
			if observerMarker != nil {
				markerErr = executionJournal.EnrichReconciliation(observerMarker)
			}
		} else if marker == nil {
			markerErr = agent.ErrSessionReconcileRequired
		} else {
			markerErr = b.markInterrupted(request.SessionID, *marker)
		}
		if markerErr != nil {
			return acp.PromptResponse{}, internalFailure()
		}
	}
	if outcome.Err != nil {
		return acp.PromptResponse{}, outcome.Err
	}
	if outcome.TerminalFailure != nil {
		return acp.PromptResponse{}, outcome.TerminalFailure
	}
	return acp.PromptResponse{StopReason: outcome.StopReason}, nil
}

func (b *Backend) commitObservedRun(
	sessionID string,
	observer *agent.DurableRunObserver,
	executionJournal *promptExecutionJournal,
) error {
	if observer == nil {
		return internalFailure()
	}
	delta := observer.Messages()
	terminal, terminalSet := observer.TerminalMetadata()
	if len(delta) == 0 && !terminalSet {
		return nil
	}

	if executionJournal == nil {
		return internalFailure()
	}
	executionJournal.mu.Lock()
	defer executionJournal.mu.Unlock()
	if executionJournal.failed || !executionJournal.active() || executionJournal.sessionID != sessionID {
		executionJournal.failLocked()
		return internalFailure()
	}
	persisted, err := b.store.Get(sessionID)
	if err != nil || persisted == nil || persisted.Revision != executionJournal.expectedRevision {
		executionJournal.failLocked()
		return internalFailure()
	}
	if executionJournal.marker != nil {
		if !persisted.ReconcileRequired || persisted.Interruption == nil ||
			!sameSessionInterruption(persisted.Interruption, executionJournal.marker) {
			executionJournal.failLocked()
			return internalFailure()
		}
	} else if persisted.ReconcileRequired || persisted.Interruption != nil {
		executionJournal.failLocked()
		return internalFailure()
	}
	candidate := cloneSession(*persisted)
	candidate.Messages = append(candidate.Messages, delta...)
	if terminalSet {
		terminalCopy := terminal
		candidate.LastRunTerminal = &terminalCopy
	}
	if executionJournal.marker != nil {
		err = b.store.CommitInterruptedRun(
			&candidate, executionJournal.expectedRevision, *executionJournal.marker,
		)
	} else {
		err = b.store.SaveExpected(&candidate, executionJournal.expectedRevision)
	}
	if err != nil {
		executionJournal.failLocked()
		return mapStoreError(err)
	}
	executionJournal.persisted = cloneSession(candidate)
	executionJournal.expectedRevision = candidate.Revision
	executionJournal.marker = nil
	executionJournal.entries = nil
	if !executionJournal.publish(candidate) {
		executionJournal.failLocked()
		return internalFailure()
	}
	b.mu.Lock()
	current := b.sessions[sessionID]
	if current != nil && current == executionJournal.state && current.toolResults != nil {
		current.resultRefs = current.toolResults.ReferencesForMessages(candidate.Messages)
	}
	b.mu.Unlock()
	return nil
}

func (b *Backend) sessionMCPRuntime(
	ctx context.Context,
	sessionID string,
	expected *runtimeSession,
	workspace string,
) (agent.MCPRuntime, error) {
	b.mu.Lock()
	state := b.sessions[sessionID]
	if state != expected || state == nil || state.cancel == nil {
		b.mu.Unlock()
		return nil, invalidRequest("session runtime state changed")
	}
	if state.mcpRuntime != nil {
		if state.mcpRuntime.Healthy() {
			runtime := state.mcpRuntime
			b.mu.Unlock()
			return runtime, nil
		}
		staleRuntime := state.mcpRuntime
		staleCancel := state.mcpCancel
		state.mcpRuntime = nil
		state.mcpCancel = nil
		servers := cloneSessionMCPServers(state.mcpServers)
		b.mu.Unlock()
		if staleCancel != nil {
			staleCancel()
		}
		staleRuntime.Close()
		return b.createSessionMCPRuntime(ctx, sessionID, expected, workspace, servers)
	}
	servers := cloneSessionMCPServers(state.mcpServers)
	b.mu.Unlock()
	if len(servers) == 0 {
		return nil, nil
	}
	return b.createSessionMCPRuntime(ctx, sessionID, expected, workspace, servers)
}

func (b *Backend) createSessionMCPRuntime(
	ctx context.Context,
	sessionID string,
	expected *runtimeSession,
	workspace string,
	servers []agent.MCPServerSpec,
) (agent.MCPRuntime, error) {
	lifetimeCtx, lifetimeCancel := context.WithCancel(context.Background())
	execution := agent.DefaultMCPExecutionOptions(lifetimeCtx, workspace)
	if configured := cloneMCPExecutionOptions(b.mcpExecution); configured != nil {
		execution = *configured
		execution.Context = lifetimeCtx
	}
	factory := b.mcpRuntimeFactory
	if factory == nil {
		factory = agent.NewMCPRuntime
	}
	runtime, err := factory(ctx, workspace, servers, execution)
	if err != nil || runtime == nil {
		lifetimeCancel()
		if runtime != nil {
			runtime.Close()
		}
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		return nil, invalidRequest("secure stdio MCP runtime is unavailable")
	}
	if err := ctx.Err(); err != nil {
		lifetimeCancel()
		runtime.Close()
		return nil, err
	}

	b.mu.Lock()
	state := b.sessions[sessionID]
	if state != expected || state == nil || state.cancel == nil {
		b.mu.Unlock()
		lifetimeCancel()
		runtime.Close()
		return nil, invalidRequest("session runtime state changed")
	}
	if state.mcpRuntime != nil {
		existing := state.mcpRuntime
		b.mu.Unlock()
		lifetimeCancel()
		runtime.Close()
		return existing, nil
	}
	state.mcpRuntime = runtime
	state.mcpCancel = lifetimeCancel
	b.mu.Unlock()
	return runtime, nil
}

func (b *Backend) CancelSession(_ context.Context, notification acp.CancelNotification) error {
	b.mu.Lock()
	state := b.sessions[notification.SessionID]
	if state != nil && state.cancel != nil {
		state.cancel()
	}
	b.mu.Unlock()
	return nil
}

// Shutdown cancels every active prompt and waits until each bridge invocation
// has returned or ctx expires. The ACP Connection remains responsible for
// closing stdio and its own request goroutines.
func (b *Backend) Shutdown(ctx context.Context) error {
	loopsStopped := b.loops.Shutdown(ctx)
	b.mu.Lock()
	runtimes := make([]agent.MCPRuntime, 0, len(b.sessions))
	cancels := make([]context.CancelFunc, 0, len(b.sessions))
	for _, state := range b.sessions {
		if state.mcpCancel != nil {
			cancels = append(cancels, state.mcpCancel)
			state.mcpCancel = nil
		}
		if state.mcpRuntime != nil {
			runtimes = append(runtimes, state.mcpRuntime)
			state.mcpRuntime = nil
		}
	}
	b.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	for _, runtime := range runtimes {
		runtime.Close()
	}
	if !loopsStopped {
		if err := ctx.Err(); err != nil {
			return err
		}
		return errors.New("agent loops did not shut down")
	}
	return nil
}

type runOutcome struct {
	Text             string
	StopReason       acp.StopReason
	Err              error
	TransportFailure bool
	TerminalFailure  error
	TerminalKind     string
	DurablePolicy    string
	TerminalSeen     bool
	Observer         *agent.DurableRunObserver
}

const (
	durablePolicyCommit    = string(agent.RunTerminalDurableCommit)
	durablePolicyNone      = string(agent.RunTerminalDurableNone)
	durablePolicyReconcile = string(agent.RunTerminalDurableReconcile)
)

type runTerminalEnvelope struct {
	Kind          string `json:"kind"`
	StopReason    string `json:"stopReason"`
	DurablePolicy string `json:"durablePolicy"`
}

func (b *Backend) consumeEvents(
	ctx context.Context,
	cancel context.CancelFunc,
	sessionID string,
	activeID string,
	messageID string,
	reasoning string,
	client acp.Client,
	events <-chan agent.Event,
) runOutcome {
	var output strings.Builder
	stopReason := acp.StopEndTurn
	doneSeen := false
	contextBlocked := false
	maxIterations := false
	toolNames := make(map[string]string)
	observer := agent.NewDurableRunObserver(activeID)
	lastProgress := ""
	var transportErr error
	var kernelFailure error
	var terminalFailure error
	var terminalKind string
	var durablePolicy string
	cancelBeforeTerminal := false
	ctxDone := ctx.Done()
	failTransport := func(err error) {
		if contextErr := ctx.Err(); contextErr != nil && errors.Is(err, contextErr) {
			// A client update using the prompt context can observe the requested
			// cancellation before the kernel emits its terminal-last frame. This is
			// cancellation ordering, not a transport write failure.
			cancelBeforeTerminal = !doneSeen
			ctxDone = nil
			return
		}
		if transportErr == nil {
			transportErr = internalFailure()
		}
		cancel()
	}
	finish := func(err error) runOutcome {
		return runOutcome{
			Text:             output.String(),
			StopReason:       stopReason,
			Err:              err,
			TransportFailure: transportErr != nil,
			TerminalFailure:  terminalFailure,
			TerminalKind:     terminalKind,
			DurablePolicy:    durablePolicy,
			TerminalSeen:     doneSeen || contextBlocked || maxIterations,
			Observer:         observer,
		}
	}
	observeDone := func(data any) {
		doneSeen = true
		ctxDone = nil
		stopReason = eventStopReason(data, stopReason)
		envelope, envelopeSet := decodeRunTerminalEnvelope(data)
		if envelopeSet {
			terminalKind = envelope.Kind
			durablePolicy = envelope.DurablePolicy
			stopReason = terminalEnvelopeStopReason(envelope, stopReason)
		}

		metadata, metadataSet := agent.DecodeDurableRunTerminalMetadata(data)
		contractBlocked := metadataSet && (metadata.CompletionStatus == agent.CompletionStatusIncomplete ||
			metadata.CompletionStatus == agent.CompletionStatusBlocked || metadata.CompletionBlocked > 0)
		if contractBlocked {
			terminalFailure = invalidRequest("agent completion is blocked or incomplete")
		}
		if !validDurablePolicy(durablePolicy) || !validTerminalKind(terminalKind) {
			terminalFailure = internalFailure()
			durablePolicy = durablePolicyReconcile
			return
		}
		switch terminalKind {
		case string(agent.RunTerminalContextBlocked):
			stopReason = acp.StopMaxTokens
			if !contractBlocked {
				// Stable ACP can represent context exhaustion directly. It remains a
				// non-success durable terminal even though the RPC has a stop result.
				terminalFailure = nil
			}
		case string(agent.RunTerminalMaxIterations), string(agent.RunTerminalMaxCycles):
			stopReason = acp.StopMaxTurnRequests
			if !contractBlocked {
				terminalFailure = nil
			}
		case string(agent.RunTerminalCancelled):
			stopReason = acp.StopCancelled
			if !contractBlocked {
				terminalFailure = nil
			}
		case string(agent.RunTerminalFailed), string(agent.RunTerminalNoTerminal):
			if terminalFailure == nil {
				terminalFailure = internalFailure()
			}
		case string(agent.RunTerminalCompleted), string(agent.RunTerminalCommand):
			if metadataSet && metadata.BlocksSuccess() && terminalFailure == nil {
				terminalFailure = invalidRequest("agent completion is blocked or incomplete")
			}
		case "":
			// Legacy done has no envelope. Typed completion metadata is still
			// authoritative when present.
			if metadataSet && metadata.BlocksSuccess() && terminalFailure == nil {
				terminalFailure = invalidRequest("agent completion is blocked or incomplete")
			}
		}
	}

	for {
		select {
		case <-ctxDone:
			// ACP requires the original prompt request to remain pending until
			// every ongoing operation has actually stopped. Disable this select
			// arm and keep draining lifecycle events until the runner closes.
			cancelBeforeTerminal = !doneSeen
			ctxDone = nil
		case event, ok := <-events:
			if !ok {
				if transportErr != nil {
					return finish(transportErr)
				}
				if doneSeen {
					// A typed completed/command terminal is the ordering authority. A
					// cancellation observed while that frame was queued must not rewrite
					// it; explicit ACP session cancellation is still mapped by the core.
					if cancelBeforeTerminal && terminalKind != string(agent.RunTerminalCompleted) && terminalKind != string(agent.RunTerminalCommand) {
						return finish(ctx.Err())
					}
					if kernelFailure != nil && (terminalKind == "" || terminalKind == string(agent.RunTerminalCompleted) || terminalKind == string(agent.RunTerminalCommand)) {
						terminalFailure = kernelFailure
					}
					return finish(nil)
				}
				if cancelBeforeTerminal || ctx.Err() != nil {
					return finish(ctx.Err())
				}
				if contextBlocked || maxIterations {
					// Current/legacy kernels close after these exact bounded events. Seal
					// only the durable observer after classification; this synthetic
					// bookkeeping event never decides ACP success and is skipped whenever
					// the terminal-finalizer supplied a truthful done frame.
					observer.Observe(agent.Event{Type: "done"})
					return finish(nil)
				}
				if kernelFailure != nil {
					return finish(kernelFailure)
				}
				return finish(internalFailure())
			}
			// The shared observer consumes only bounded durable lifecycle data.
			// It continues observing while cancellation drains, whereas all
			// client-visible output below is suppressed after terminal state.
			observer.Observe(event)
			if doneSeen {
				continue
			}
			if event.Type == "done" {
				observeDone(event.Data)
				continue
			}
			if transportErr != nil || kernelFailure != nil || contextBlocked || maxIterations || cancelBeforeTerminal {
				continue
			}
			if ctx.Err() != nil {
				cancelBeforeTerminal = true
				ctxDone = nil
				continue
			}
			switch event.Type {
			case "text":
				chunk, _ := event.Data.(string)
				chunk = sanitizeText(chunk, maxOutboundTextBytes-output.Len())
				if chunk == "" {
					continue
				}
				output.WriteString(chunk)
				if err := client.SessionUpdate(ctx, textUpdate(sessionID, messageID, "agent_message_chunk", chunk)); err != nil {
					failTransport(err)
				}
			case "status":
				if reasoning != progressReasoningMode {
					continue
				}
				progress := safeProgress(event.Data)
				if progress == "" || progress == lastProgress {
					continue
				}
				lastProgress = progress
				if err := client.SessionUpdate(ctx, textUpdate(sessionID, messageID, "agent_thought_chunk", progress)); err != nil {
					failTransport(err)
				}
			case "tool_start":
				id, name := toolIdentity(event.Data)
				if id == "" || name == "" {
					continue
				}
				title := sanitizeText(name, 256)
				updateType := "tool_call"
				if _, exists := toolNames[id]; exists {
					updateType = "tool_call_update"
				}
				toolNames[id] = title
				if err := client.SessionUpdate(ctx, acp.SessionNotification{
					SessionID: sessionID,
					Update: acp.SessionUpdate{
						SessionUpdate: updateType,
						ToolCallID:    id,
						Title:         &title,
						Kind:          toolKind(name),
						Status:        acp.ToolPending,
					},
				}); err != nil {
					failTransport(err)
				}
			case "tool_execution_start":
				id, name, _, _ := toolExecutionIdentity(event.Data)
				if id == "" {
					continue
				}
				if name == "" {
					name = toolNames[id]
				}
				if name == "" {
					name = "Tool"
				}
				title := sanitizeText(name, 256)
				toolNames[id] = title
				if err := client.SessionUpdate(ctx, acp.SessionNotification{
					SessionID: sessionID,
					Update: acp.SessionUpdate{
						SessionUpdate: "tool_call_update",
						ToolCallID:    id,
						Title:         &title,
						Kind:          toolKind(name),
						Status:        acp.ToolInProgress,
					},
				}); err != nil {
					failTransport(err)
				}
			case "tool_result":
				id, name, failed, _ := toolResultIdentity(event.Data)
				if id == "" {
					continue
				}
				if name == "" {
					name = toolNames[id]
				}
				if name == "" {
					name = "Tool"
				}
				title := sanitizeText(name, 256)
				status := acp.ToolCompleted
				if failed {
					status = acp.ToolFailed
				}
				if err := client.SessionUpdate(ctx, acp.SessionNotification{
					SessionID: sessionID,
					Update: acp.SessionUpdate{
						SessionUpdate: "tool_call_update",
						ToolCallID:    id,
						Title:         &title,
						Kind:          toolKind(name),
						Status:        status,
					},
				}); err != nil {
					failTransport(err)
				}
			case "context_blocked":
				contextBlocked = true
				stopReason = acp.StopMaxTokens
			case "error":
				if message, _ := event.Data.(string); message == "Max iterations reached" {
					maxIterations = true
					stopReason = acp.StopMaxTurnRequests
				} else {
					kernelFailure = internalFailure()
				}
			}
		}
	}
}

func eventStopReason(data any, fallback acp.StopReason) acp.StopReason {
	encoded, err := json.Marshal(data)
	if err != nil {
		return fallback
	}
	var values struct {
		StopReason string `json:"stopReason"`
	}
	if json.Unmarshal(encoded, &values) != nil {
		return fallback
	}
	value := values.StopReason
	switch value {
	case "max_tokens":
		return acp.StopMaxTokens
	case "max_iterations", "max_turn_requests", "max_cycles":
		return acp.StopMaxTurnRequests
	case "refusal":
		return acp.StopRefusal
	case "cancelled", "canceled":
		return acp.StopCancelled
	case "end_turn", "stop_sequence", "":
		return fallback
	case "pause_turn":
		// Stable ACP v1 has no pause_turn stop reason. The kernel has already
		// completed this prompt turn, so the conservative wire representation
		// is end_turn rather than inventing an unsupported enum.
		return acp.StopEndTurn
	default:
		return fallback
	}
}

func decodeRunTerminalEnvelope(data any) (runTerminalEnvelope, bool) {
	if data == nil {
		return runTerminalEnvelope{}, false
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return runTerminalEnvelope{}, false
	}
	var envelope runTerminalEnvelope
	if json.Unmarshal(encoded, &envelope) != nil {
		return runTerminalEnvelope{}, false
	}
	envelope.Kind = strings.TrimSpace(envelope.Kind)
	envelope.StopReason = strings.TrimSpace(envelope.StopReason)
	envelope.DurablePolicy = strings.TrimSpace(envelope.DurablePolicy)
	return envelope, envelope.Kind != "" || envelope.DurablePolicy != ""
}

func terminalEnvelopeStopReason(envelope runTerminalEnvelope, fallback acp.StopReason) acp.StopReason {
	switch envelope.Kind {
	case string(agent.RunTerminalContextBlocked):
		return acp.StopMaxTokens
	case string(agent.RunTerminalMaxIterations), string(agent.RunTerminalMaxCycles):
		return acp.StopMaxTurnRequests
	case string(agent.RunTerminalCancelled):
		return acp.StopCancelled
	default:
		return eventStopReason(map[string]string{"stopReason": envelope.StopReason}, fallback)
	}
}

func validTerminalKind(kind string) bool {
	switch kind {
	case "",
		string(agent.RunTerminalCompleted), string(agent.RunTerminalCommand),
		string(agent.RunTerminalCancelled), string(agent.RunTerminalContextBlocked),
		string(agent.RunTerminalMaxIterations), string(agent.RunTerminalMaxCycles),
		string(agent.RunTerminalFailed), string(agent.RunTerminalNoTerminal):
		return true
	default:
		return false
	}
}

func validDurablePolicy(policy string) bool {
	switch policy {
	case "", durablePolicyCommit, durablePolicyNone, durablePolicyReconcile:
		return true
	default:
		return false
	}
}

func outcomeInterruptionMarker(outcome runOutcome, reason string) *agent.SessionInterruption {
	if outcome.Observer == nil {
		return nil
	}
	if marker := outcome.Observer.ReconciliationMarker(reason); marker != nil {
		return marker
	}
	if !outcome.TerminalSeen {
		return nil
	}
	return &agent.SessionInterruption{Reason: sanitizeText(reason, 1024)}
}

func (b *Backend) markInterrupted(sessionID string, marker agent.SessionInterruption) (resultErr error) {
	defer func() {
		if resultErr == nil {
			return
		}
		b.mu.Lock()
		if state := b.sessions[sessionID]; state != nil {
			state.quarantined = true
		}
		b.mu.Unlock()
	}()
	for attempt := 0; attempt < 3; attempt++ {
		persisted, err := b.store.Get(sessionID)
		if err != nil {
			return err
		}
		if persisted.ReconcileRequired || persisted.LifecycleStatus == agent.SessionLifecycleInterrupted ||
			persisted.LifecycleStatus == agent.SessionLifecycleRecoveryNeeded {
			return nil
		}
		interrupted, err := b.store.MarkInterrupted(sessionID, persisted.Revision, marker)
		if errors.Is(err, agent.ErrSessionRevisionConflict) {
			continue
		}
		if err != nil {
			return err
		}
		b.mu.Lock()
		if state := b.sessions[sessionID]; state != nil {
			state.persisted = cloneSession(*interrupted)
		}
		b.mu.Unlock()
		return nil
	}
	return agent.ErrSessionRevisionConflict
}

func (b *Backend) validateLoadable(session agent.Session) error {
	if session.Provider != "" && session.Provider != b.provider.Name() {
		return invalidRequest("session belongs to another provider")
	}
	if session.Model == "" {
		session.Model = b.defaultModel
	}
	if !b.supportsModel(session.Model) {
		return invalidRequest("session model is unavailable")
	}
	if session.ReconcileRequired || session.LifecycleStatus == agent.SessionLifecycleInterrupted ||
		session.LifecycleStatus == agent.SessionLifecycleRecoveryNeeded {
		return invalidRequest("session requires explicit reconciliation")
	}
	if session.LifecycleStatus == agent.SessionLifecycleClosed {
		return invalidRequest("session is closed")
	}
	return nil
}

func (b *Backend) configOptionsLocked(state *runtimeSession) []acp.SessionConfigOption {
	models := b.modelOptions(state.persisted.Model)
	return []acp.SessionConfigOption{
		{
			Type:         "select",
			ID:           "model",
			Name:         "Model",
			CurrentValue: state.persisted.Model,
			Options:      models,
		},
		{
			Type:         "select",
			ID:           "reasoning",
			Name:         "Reasoning progress",
			Description:  "Controls sanitized progress updates; raw chain-of-thought is never sent.",
			CurrentValue: state.reasoning,
			Options: []acp.SessionConfigSelectOption{
				{Value: defaultReasoningMode, Name: "Hidden"},
				{Value: progressReasoningMode, Name: "Progress only"},
			},
		},
	}
}

func (b *Backend) modelOptions(current string) []acp.SessionConfigSelectOption {
	seen := make(map[string]struct{})
	items := make([]acp.SessionConfigSelectOption, 0, len(b.provider.Models())+1)
	add := func(id, name string) {
		id = strings.TrimSpace(id)
		if id == "" || len(items) >= 126 {
			return
		}
		if _, duplicate := seen[id]; duplicate {
			return
		}
		seen[id] = struct{}{}
		if strings.TrimSpace(name) == "" {
			name = id
		}
		items = append(items, acp.SessionConfigSelectOption{
			Value: id,
			Name:  sanitizeText(name, 256),
		})
	}
	add(current, current)
	add(b.defaultModel, b.defaultModel)
	for _, model := range b.provider.Models() {
		add(model.ID, model.DisplayName)
	}
	return items
}

func (b *Backend) supportsModel(model string) bool {
	if model == b.defaultModel {
		return true
	}
	for _, candidate := range b.provider.Models() {
		if candidate.ID == model {
			return true
		}
	}
	return false
}

func replayHistory(ctx context.Context, client acp.Client, session agent.Session) error {
	for index, message := range session.Messages {
		var updateType string
		switch message.Role {
		case "user":
			updateType = "user_message_chunk"
		case "assistant":
			updateType = "agent_message_chunk"
		case "tool":
			toolCallID, _, ok := durableACPToolReference(message.ToolInput)
			if !ok {
				// Legacy tool records can contain raw, unbound input. Only replay
				// transcript entries created by the shared digest-only observer.
				continue
			}
			title := sanitizeText(message.ToolName, 256)
			if title == "" {
				title = "Tool"
			}
			status := acp.ToolCompleted
			if message.IsError {
				status = acp.ToolFailed
			}
			if err := client.SessionUpdate(ctx, acp.SessionNotification{
				SessionID: session.ID,
				Update: acp.SessionUpdate{
					SessionUpdate: "tool_call",
					ToolCallID:    toolCallID,
					Title:         &title,
					Kind:          toolKind(title),
					Status:        status,
				},
			}); err != nil {
				return err
			}
			continue
		default:
			continue
		}
		text := sanitizeText(message.Content, maxOutboundTextBytes)
		if text == "" {
			continue
		}
		if err := client.SessionUpdate(ctx, textUpdate(
			session.ID,
			fmt.Sprintf("history-%d", index+1),
			updateType,
			text,
		)); err != nil {
			return err
		}
	}
	return nil
}

func durableACPToolReference(value any) (string, string, bool) {
	var toolCallID, inputDigest string
	switch reference := value.(type) {
	case map[string]string:
		toolCallID = reference["toolCallId"]
		inputDigest = reference["inputDigest"]
	case map[string]interface{}:
		toolCallID, _ = reference["toolCallId"].(string)
		inputDigest, _ = reference["inputDigest"].(string)
	default:
		return "", "", false
	}
	toolCallID = boundedWireString(toolCallID, 512)
	if toolCallID == "" || !validACPInputDigest(inputDigest) {
		return "", "", false
	}
	return toolCallID, inputDigest, true
}

func validACPInputDigest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, character := range value[len("sha256:"):] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func textUpdate(sessionID, messageID, updateType, text string) acp.SessionNotification {
	return acp.SessionNotification{
		SessionID: sessionID,
		Update: acp.SessionUpdate{
			SessionUpdate: updateType,
			MessageID:     messageID,
			Content:       &acp.ContentBlock{Type: "text", Text: text},
		},
	}
}

func promptText(blocks []acp.ContentBlock) (string, error) {
	var builder strings.Builder
	for _, block := range blocks {
		if block.Type != "text" {
			return "", invalidParams("only text prompts are supported")
		}
		if builder.Len() > 0 {
			builder.WriteByte('\n')
		}
		if builder.Len()+len(block.Text) > maxPromptBytes {
			return "", invalidParams("prompt is too large")
		}
		builder.WriteString(block.Text)
	}
	if strings.TrimSpace(builder.String()) == "" {
		return "", invalidParams("prompt is empty")
	}
	return builder.String(), nil
}

func sessionMessagesForRun(messages []agent.SessionMessage) []types.Message {
	result := make([]types.Message, 0, len(messages))
	for _, message := range messages {
		if message.Role != "user" && message.Role != "assistant" {
			continue
		}
		content, _ := json.Marshal(sanitizeText(message.Content, maxOutboundTextBytes))
		result = append(result, types.Message{Role: message.Role, Content: content})
	}
	return result
}

func cloneSession(session agent.Session) agent.Session {
	copySession := session
	copySession.Messages = append([]agent.SessionMessage(nil), session.Messages...)
	return copySession
}

func sessionMCPServerSpecs(servers []acp.MCPServer) ([]agent.MCPServerSpec, error) {
	specs := make([]agent.MCPServerSpec, 0, len(servers))
	seenServers := make(map[string]struct{}, len(servers))
	for _, server := range servers {
		if server.Type != "" {
			return nil, invalidParams("only stdio MCP servers are supported")
		}
		if !validMCPWireString(server.Name, 256) || !validMCPWireString(server.Command, 4096) ||
			server.Args == nil || server.Env == nil || len(server.Args) > 256 || len(server.Env) > 256 ||
			server.URL != "" || len(server.Headers) != 0 {
			return nil, invalidParams("stdio MCP server configuration is invalid")
		}
		if _, duplicate := seenServers[server.Name]; duplicate {
			return nil, invalidParams("duplicate MCP server name")
		}
		seenServers[server.Name] = struct{}{}
		args := append([]string(nil), server.Args...)
		for _, argument := range args {
			if len(argument) > 4096 || !utf8.ValidString(argument) || strings.IndexByte(argument, 0) >= 0 {
				return nil, invalidParams("stdio MCP server argument is invalid")
			}
		}
		environment := make(map[string]string, len(server.Env))
		seenEnvironment := make(map[string]struct{}, len(server.Env))
		for _, variable := range server.Env {
			if !validMCPWireString(variable.Name, 256) || len(variable.Value) > acp.MaxStringBytes ||
				!utf8.ValidString(variable.Value) || strings.IndexByte(variable.Value, 0) >= 0 ||
				strings.Contains(variable.Name, "=") {
				return nil, invalidParams("stdio MCP server environment is invalid")
			}
			identity := variable.Name
			if runtime.GOOS == "windows" {
				identity = strings.ToUpper(identity)
			}
			if _, duplicate := seenEnvironment[identity]; duplicate {
				return nil, invalidParams("duplicate MCP environment variable")
			}
			seenEnvironment[identity] = struct{}{}
			environment[variable.Name] = variable.Value
		}
		specs = append(specs, agent.MCPServerSpec{
			Name: server.Name, Command: server.Command, Args: args, Env: environment,
		})
	}
	if err := agent.ValidateMCPServerSpecs(specs); err != nil {
		return nil, invalidParams("stdio MCP server configuration violates the secure runtime policy")
	}
	return specs, nil
}

func validMCPWireString(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && utf8.ValidString(value) &&
		strings.IndexByte(value, 0) < 0 && strings.IndexByte(value, '\n') < 0 &&
		strings.IndexByte(value, '\r') < 0
}

func cloneSessionMCPServers(servers []agent.MCPServerSpec) []agent.MCPServerSpec {
	if servers == nil {
		return nil
	}
	cloned := make([]agent.MCPServerSpec, len(servers))
	for index, server := range servers {
		environment := make(map[string]string, len(server.Env))
		for name, value := range server.Env {
			environment[name] = value
		}
		cloned[index] = agent.MCPServerSpec{
			Name: server.Name, Command: server.Command,
			Args: append([]string(nil), server.Args...), Env: environment,
		}
	}
	return cloned
}

func cloneMCPExecutionOptions(options *agent.MCPExecutionOptions) *agent.MCPExecutionOptions {
	if options == nil {
		return nil
	}
	cloned := *options
	return &cloned
}

func canonicalWorkspace(path string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil || !filepath.IsAbs(abs) {
		return "", errors.New("workspace is not absolute")
	}
	if resolved, resolveErr := filepath.EvalSymlinks(abs); resolveErr == nil {
		abs = resolved
	}
	return filepath.Clean(abs), nil
}

func sameWorkspace(a, b string) bool {
	left, leftErr := canonicalWorkspace(a)
	right, rightErr := canonicalWorkspace(b)
	if leftErr != nil || rightErr != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func decodeCursor(cursor *string) (int, error) {
	if cursor == nil || *cursor == "" {
		return 0, nil
	}
	offset, err := strconv.Atoi(*cursor)
	if err != nil || offset < 0 {
		return 0, errors.New("invalid cursor")
	}
	return offset, nil
}

func safeProgress(value any) string {
	text, _ := value.(string)
	switch {
	case strings.HasPrefix(text, "Thinking"):
		return "Reasoning"
	case strings.HasPrefix(text, "Auto-verify"):
		return "Verifying changes"
	case strings.HasPrefix(text, "Compressing context"):
		return "Compressing context"
	case strings.HasPrefix(text, "Tool response correction"), strings.HasPrefix(text, "Tool routing selector correction"):
		return "Recovering a tool request"
	default:
		return ""
	}
}

func isUIOnlyCommand(prompt string) bool {
	command := strings.ToLower(strings.TrimSpace(prompt))
	return command == "/clear" || command == "/model" || strings.HasPrefix(command, "/model ")
}

func toolIdentity(data any) (string, string) {
	values, ok := data.(map[string]string)
	if !ok {
		return "", ""
	}
	return boundedWireString(values["id"], 512), sanitizeText(values["name"], 256)
}

func toolExecutionIdentity(data any) (string, string, string, string) {
	values, ok := data.(map[string]string)
	if !ok {
		return "", "", "", ""
	}
	return boundedWireString(values["id"], 256), sanitizeText(values["name"], 128),
		boundedWireString(values["inputDigest"], 80), boundedWireString(values["runId"], 256)
}

func toolResultIdentity(data any) (string, string, bool, bool) {
	values, ok := data.(map[string]interface{})
	if !ok {
		return "", "", false, false
	}
	id, _ := values["id"].(string)
	name, _ := values["name"].(string)
	failed, _ := values["isError"].(bool)
	executed, _ := values["executed"].(bool)
	return boundedWireString(id, 512), sanitizeText(name, 256), failed, executed
}

func toolKind(name string) acp.ToolKind {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "read":
		return acp.ToolRead
	case "write", "edit":
		return acp.ToolEdit
	case "grep", "glob", "ls":
		return acp.ToolSearch
	case "bash", "lint", "test":
		return acp.ToolExecute
	case "webfetch", "websearch", "httprequest":
		return acp.ToolFetch
	default:
		return acp.ToolOther
	}
}

func mapStoreError(err error) error {
	switch {
	case errors.Is(err, agent.ErrSessionNotFound):
		return notFound("session not found")
	case errors.Is(err, agent.ErrInvalidSessionID):
		return invalidParams("invalid session id")
	case errors.Is(err, agent.ErrSessionRevisionConflict),
		errors.Is(err, agent.ErrSessionConflict),
		errors.Is(err, agent.ErrSessionLifecycleInvalid),
		errors.Is(err, agent.ErrSessionReconcileRequired),
		errors.Is(err, agent.ErrSessionVersionUnsupported),
		errors.Is(err, agent.ErrSessionCorrupt):
		return invalidRequest("session state changed or is unavailable")
	default:
		return internalFailure()
	}
}

func invalidParams(message string) error {
	return &acp.RPCError{Code: acp.CodeInvalidParams, Message: sanitizeText(message, 256)}
}

func invalidRequest(message string) error {
	return &acp.RPCError{Code: acp.CodeInvalidRequest, Message: sanitizeText(message, 256)}
}

func notFound(message string) error {
	return &acp.RPCError{Code: acp.CodeResourceNotFound, Message: sanitizeText(message, 256)}
}

func internalFailure() error {
	return &acp.RPCError{Code: acp.CodeInternalError, Message: "agent run failed"}
}
