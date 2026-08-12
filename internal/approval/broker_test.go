package approval

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func testDraft(sessionID string) Draft {
	return Draft{
		SessionID:       sessionID,
		SessionRevision: 7,
		RunID:           "run-1",
		ToolCallID:      "call-1",
		ToolName:        "bash_exec",
		RedactedInput:   "git status [REDACTED]",
		InputDigest:     "sha256:test",
		DangerLevel:     "high",
		Scope:           "workspace",
		RememberAllowed: false,
	}
}

func TestBrokerAllowOnceAndDuplicateResolution(t *testing.T) {
	broker := NewBroker(time.Second)
	t.Cleanup(broker.Shutdown)
	pending, err := broker.Open(testDraft("session-a"))
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	if !strings.HasPrefix(pending.ID, "apr_") || len(pending.ID) != len("apr_")+26 {
		t.Fatalf("approval ID = %q, want crypto base32 ID", pending.ID)
	}
	if pending.ToolCallID != "call-1" {
		t.Fatalf("pending ToolCallID = %q, want exact model call ID", pending.ToolCallID)
	}

	decision := Decision{ApprovalID: pending.ID, SessionID: pending.SessionID, Outcome: OutcomeAllowOnce}
	resolution, err := broker.Resolve(decision)
	if err != nil {
		t.Fatalf("Resolve(allow once) = %v", err)
	}
	if !resolution.Allowed() {
		t.Fatalf("resolution = %+v, want allowed", resolution)
	}

	duplicate, err := broker.Resolve(decision)
	if !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("Resolve(duplicate) = %v, want ErrAlreadyResolved", err)
	}
	if duplicate != resolution {
		t.Fatalf("duplicate resolution = %+v, want original %+v", duplicate, resolution)
	}

	awaited, err := broker.Await(context.Background(), pending.SessionID, pending.ID)
	if err != nil {
		t.Fatalf("Await(resolved) = %v", err)
	}
	if awaited != resolution {
		t.Fatalf("Await resolution = %+v, want %+v", awaited, resolution)
	}
}

func TestBrokerExplicitDeny(t *testing.T) {
	broker := NewBroker(time.Second)
	t.Cleanup(broker.Shutdown)
	pending, err := broker.Open(testDraft("session-a"))
	if err != nil {
		t.Fatalf("Open = %v", err)
	}

	resolution, err := broker.Resolve(Decision{
		ApprovalID: pending.ID,
		SessionID:  pending.SessionID,
		Outcome:    OutcomeDeny,
	})
	if err != nil {
		t.Fatalf("Resolve(deny) = %v", err)
	}
	if resolution.Outcome != OutcomeDeny || resolution.Reason != ReasonUser || resolution.Allowed() {
		t.Fatalf("deny resolution = %+v", resolution)
	}
}

func TestBrokerWrongSessionDoesNotDiscloseOwnership(t *testing.T) {
	broker := NewBroker(time.Second)
	t.Cleanup(broker.Shutdown)
	pending, err := broker.Open(testDraft("session-owner"))
	if err != nil {
		t.Fatalf("Open = %v", err)
	}

	_, wrongErr := broker.Resolve(Decision{
		ApprovalID: pending.ID,
		SessionID:  "session-attacker",
		Outcome:    OutcomeAllowOnce,
	})
	_, unknownErr := broker.Resolve(Decision{
		ApprovalID: "apr_" + strings.Repeat("z", 26),
		SessionID:  "session-attacker",
		Outcome:    OutcomeAllowOnce,
	})
	if !errors.Is(wrongErr, ErrApprovalNotFound) || !errors.Is(unknownErr, ErrApprovalNotFound) {
		t.Fatalf("wrong=%v unknown=%v, want same non-disclosing not-found class", wrongErr, unknownErr)
	}
	if wrongErr.Error() != unknownErr.Error() {
		t.Fatalf("wrong-session error %q discloses more than unknown error %q", wrongErr, unknownErr)
	}
	if _, err := broker.Await(context.Background(), "session-attacker", pending.ID); !errors.Is(err, ErrApprovalNotFound) {
		t.Fatalf("Await(wrong session) = %v, want ErrApprovalNotFound", err)
	}

	resolution, err := broker.Resolve(Decision{
		ApprovalID: pending.ID,
		SessionID:  pending.SessionID,
		Outcome:    OutcomeDeny,
	})
	if err != nil || resolution.Reason != ReasonUser {
		t.Fatalf("owner Resolve after wrong-session attempt = %+v, %v", resolution, err)
	}
}

func TestBrokerExpiryFailsClosed(t *testing.T) {
	broker := NewBroker(20 * time.Millisecond)
	t.Cleanup(broker.Shutdown)
	pending, err := broker.Open(testDraft("session-expiry"))
	if err != nil {
		t.Fatalf("Open = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resolution, err := broker.Await(ctx, pending.SessionID, pending.ID)
	if !errors.Is(err, ErrApprovalExpired) {
		t.Fatalf("Await(expired) = %v, want ErrApprovalExpired", err)
	}
	if resolution.Outcome != OutcomeDeny || resolution.Reason != ReasonExpired || resolution.Allowed() {
		t.Fatalf("expired resolution = %+v, want deny", resolution)
	}
}

func TestBrokerLateResolveCannotAllow(t *testing.T) {
	broker := NewBroker(10 * time.Millisecond)
	t.Cleanup(broker.Shutdown)
	pending, err := broker.Open(testDraft("session-late"))
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	broker.mu.Lock()
	broker.requests[pending.ID].timer.Stop()
	broker.mu.Unlock()
	time.Sleep(20 * time.Millisecond)

	resolution, err := broker.Resolve(Decision{
		ApprovalID: pending.ID,
		SessionID:  pending.SessionID,
		Outcome:    OutcomeAllowOnce,
	})
	if !errors.Is(err, ErrApprovalExpired) {
		t.Fatalf("Resolve(after expiry) = %v, want ErrApprovalExpired", err)
	}
	if resolution.Outcome != OutcomeDeny || resolution.Reason != ReasonExpired || resolution.Allowed() {
		t.Fatalf("late resolution = %+v, want expired deny", resolution)
	}
}

func TestBrokerContextCancellationFailsClosed(t *testing.T) {
	broker := NewBroker(time.Second)
	t.Cleanup(broker.Shutdown)
	pending, err := broker.Open(testDraft("session-context"))
	if err != nil {
		t.Fatalf("Open = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	resolution, err := broker.Await(ctx, pending.SessionID, pending.ID)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Await(canceled) = %v, want context.Canceled", err)
	}
	if resolution.Outcome != OutcomeDeny || resolution.Reason != ReasonContextCanceled {
		t.Fatalf("canceled resolution = %+v, want context-canceled deny", resolution)
	}
	if _, err := broker.Resolve(Decision{
		ApprovalID: pending.ID,
		SessionID:  pending.SessionID,
		Outcome:    OutcomeAllowOnce,
	}); !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("Resolve(after context cancellation) = %v, want ErrAlreadyResolved", err)
	}
}

func TestBrokerCancelSessionOnlyCancelsMatchingPending(t *testing.T) {
	broker := NewBroker(time.Second)
	t.Cleanup(broker.Shutdown)
	first, err := broker.Open(testDraft("session-a"))
	if err != nil {
		t.Fatalf("Open(first) = %v", err)
	}
	secondDraft := testDraft("session-a")
	secondDraft.RunID = "run-2"
	second, err := broker.Open(secondDraft)
	if err != nil {
		t.Fatalf("Open(second) = %v", err)
	}
	other, err := broker.Open(testDraft("session-b"))
	if err != nil {
		t.Fatalf("Open(other) = %v", err)
	}

	broker.CancelSession("session-a")
	broker.CancelSession("session-a")
	for _, pending := range []Pending{first, second} {
		resolution, err := broker.Await(context.Background(), pending.SessionID, pending.ID)
		if !errors.Is(err, ErrSessionCanceled) {
			t.Fatalf("Await(canceled %s) = %v, want ErrSessionCanceled", pending.ID, err)
		}
		if resolution.Outcome != OutcomeDeny || resolution.Reason != ReasonSessionCanceled {
			t.Fatalf("session-canceled resolution = %+v", resolution)
		}
	}
	if _, err := broker.Resolve(Decision{
		ApprovalID: other.ID,
		SessionID:  other.SessionID,
		Outcome:    OutcomeAllowOnce,
	}); err != nil {
		t.Fatalf("Resolve(other session) = %v", err)
	}
	if _, err := broker.Open(testDraft("session-a")); !errors.Is(err, ErrSessionCanceled) {
		t.Fatalf("Open(canceled session) = %v, want ErrSessionCanceled", err)
	}
}

func TestBrokerConcurrentOpenCancelIsAtomicAndTombstoned(t *testing.T) {
	broker := NewBroker(time.Second)
	t.Cleanup(broker.Shutdown)

	generatorEntered := make(chan struct{})
	releaseGenerator := make(chan struct{})
	broker.idGenerator = func() (string, error) {
		close(generatorEntered)
		<-releaseGenerator
		return "apr_" + strings.Repeat("a", 26), nil
	}

	type openResult struct {
		pending Pending
		err     error
	}
	opened := make(chan openResult, 1)
	go func() {
		pending, err := broker.Open(testDraft("session-open-cancel"))
		opened <- openResult{pending: pending, err: err}
	}()
	<-generatorEntered // Open owns broker.mu while the ID generator is blocked.

	canceled := make(chan struct{})
	go func() {
		broker.CancelSession("session-open-cancel")
		close(canceled)
	}()
	close(releaseGenerator)

	result := <-opened
	if result.err != nil {
		t.Fatalf("Open(before concurrent cancel) = %v", result.err)
	}
	<-canceled

	resolution, err := broker.Await(context.Background(), result.pending.SessionID, result.pending.ID)
	if !errors.Is(err, ErrSessionCanceled) {
		t.Fatalf("Await(concurrently canceled) = %v, want ErrSessionCanceled", err)
	}
	if resolution.Outcome != OutcomeDeny || resolution.Reason != ReasonSessionCanceled {
		t.Fatalf("concurrently canceled resolution = %+v", resolution)
	}
	if _, err := broker.Open(testDraft("session-open-cancel")); !errors.Is(err, ErrSessionCanceled) {
		t.Fatalf("Open(after concurrent cancel) = %v, want ErrSessionCanceled", err)
	}
	if _, err := broker.Resolve(Decision{
		ApprovalID: result.pending.ID,
		SessionID:  "wrong-session",
		Outcome:    OutcomeAllowOnce,
	}); !errors.Is(err, ErrApprovalNotFound) {
		t.Fatalf("Resolve(wrong session after cancel) = %v, want ErrApprovalNotFound", err)
	}
	if late, err := broker.Resolve(Decision{
		ApprovalID: result.pending.ID,
		SessionID:  result.pending.SessionID,
		Outcome:    OutcomeAllowOnce,
	}); !errors.Is(err, ErrAlreadyResolved) || late.Reason != ReasonSessionCanceled {
		t.Fatalf("Resolve(after cancel) = %+v, %v; want canceled ErrAlreadyResolved", late, err)
	}
}

func TestBrokerConcurrentCancelAndResolveHasOneTerminalResolution(t *testing.T) {
	broker := NewBroker(time.Second)
	t.Cleanup(broker.Shutdown)
	pending, err := broker.Open(testDraft("session-cancel-resolve"))
	if err != nil {
		t.Fatalf("Open = %v", err)
	}

	start := make(chan struct{})
	cancelDone := make(chan struct{})
	resolveDone := make(chan struct{})
	var resolved Resolution
	var resolveErr error
	go func() {
		<-start
		broker.CancelSession(pending.SessionID)
		close(cancelDone)
	}()
	go func() {
		<-start
		resolved, resolveErr = broker.Resolve(Decision{
			ApprovalID: pending.ID,
			SessionID:  pending.SessionID,
			Outcome:    OutcomeAllowOnce,
		})
		close(resolveDone)
	}()
	close(start)
	<-cancelDone
	<-resolveDone

	terminal, awaitErr := broker.Await(context.Background(), pending.SessionID, pending.ID)
	switch {
	case resolveErr == nil:
		if !resolved.Allowed() || terminal != resolved || awaitErr != nil {
			t.Fatalf("resolve-first terminal=%+v err=%v resolve=%+v", terminal, awaitErr, resolved)
		}
	case errors.Is(resolveErr, ErrAlreadyResolved):
		if terminal.Outcome != OutcomeDeny || terminal.Reason != ReasonSessionCanceled ||
			!errors.Is(awaitErr, ErrSessionCanceled) {
			t.Fatalf("cancel-first terminal=%+v err=%v resolve=%+v/%v", terminal, awaitErr, resolved, resolveErr)
		}
	default:
		t.Fatalf("Resolve(concurrent cancel) = %+v, %v", resolved, resolveErr)
	}
	if _, err := broker.Open(testDraft(pending.SessionID)); !errors.Is(err, ErrSessionCanceled) {
		t.Fatalf("Open(after cancel/resolve race) = %v, want ErrSessionCanceled", err)
	}
}

func TestBrokerCancelBeforeOpenRejectsWithoutDisclosingOtherSessions(t *testing.T) {
	broker := NewBroker(time.Second)
	t.Cleanup(broker.Shutdown)
	broker.CancelSession("session-canceled-first")

	if _, err := broker.Open(testDraft("session-canceled-first")); !errors.Is(err, ErrSessionCanceled) {
		t.Fatalf("Open(after cancel) = %v, want ErrSessionCanceled", err)
	}
	if _, err := broker.Open(testDraft("session-other")); err != nil {
		t.Fatalf("Open(other session) = %v", err)
	}
}

func TestBrokerShutdownFailsClosedAndIsIdempotent(t *testing.T) {
	broker := NewBroker(time.Second)
	pending, err := broker.Open(testDraft("session-shutdown"))
	if err != nil {
		t.Fatalf("Open = %v", err)
	}

	result := make(chan struct {
		resolution Resolution
		err        error
	}, 1)
	go func() {
		resolution, awaitErr := broker.Await(context.Background(), pending.SessionID, pending.ID)
		result <- struct {
			resolution Resolution
			err        error
		}{resolution, awaitErr}
	}()
	broker.Shutdown()
	broker.Shutdown()

	got := <-result
	if !errors.Is(got.err, ErrBrokerClosed) {
		t.Fatalf("Await(shutdown) = %v, want ErrBrokerClosed", got.err)
	}
	if got.resolution.Outcome != OutcomeDeny || got.resolution.Reason != ReasonBrokerShutdown {
		t.Fatalf("shutdown resolution = %+v", got.resolution)
	}
	if _, err := broker.Open(testDraft("session-new")); !errors.Is(err, ErrBrokerClosed) {
		t.Fatalf("Open(after shutdown) = %v, want ErrBrokerClosed", err)
	}
	if _, err := broker.Resolve(Decision{
		ApprovalID: pending.ID,
		SessionID:  pending.SessionID,
		Outcome:    OutcomeAllowOnce,
	}); !errors.Is(err, ErrBrokerClosed) {
		t.Fatalf("Resolve(after shutdown) = %v, want ErrBrokerClosed", err)
	}
}

func TestBrokerConcurrentResolutionHasSingleWinner(t *testing.T) {
	broker := NewBroker(time.Second)
	t.Cleanup(broker.Shutdown)
	pending, err := broker.Open(testDraft("session-race"))
	if err != nil {
		t.Fatalf("Open = %v", err)
	}

	const contenders = 64
	type result struct {
		resolution Resolution
		err        error
	}
	results := make(chan result, contenders)
	var start sync.WaitGroup
	start.Add(1)
	var workers sync.WaitGroup
	workers.Add(contenders)
	for i := 0; i < contenders; i++ {
		outcome := OutcomeDeny
		if i%2 == 0 {
			outcome = OutcomeAllowOnce
		}
		go func(outcome Outcome) {
			defer workers.Done()
			start.Wait()
			resolution, resolveErr := broker.Resolve(Decision{
				ApprovalID: pending.ID,
				SessionID:  pending.SessionID,
				Outcome:    outcome,
			})
			results <- result{resolution, resolveErr}
		}(outcome)
	}
	start.Done()
	workers.Wait()
	close(results)

	winners := 0
	duplicates := 0
	var winner Resolution
	for got := range results {
		switch {
		case got.err == nil:
			winners++
			winner = got.resolution
		case errors.Is(got.err, ErrAlreadyResolved):
			duplicates++
			if winner.ApprovalID != "" && got.resolution != winner {
				t.Fatalf("duplicate observed different resolution: %+v vs %+v", got.resolution, winner)
			}
		default:
			t.Fatalf("unexpected concurrent Resolve error: %v", got.err)
		}
	}
	if winners != 1 || duplicates != contenders-1 {
		t.Fatalf("winners=%d duplicates=%d, want 1/%d", winners, duplicates, contenders-1)
	}
	awaited, err := broker.Await(context.Background(), pending.SessionID, pending.ID)
	if err != nil {
		t.Fatalf("Await(winner) = %v", err)
	}
	if awaited != winner {
		t.Fatalf("Await = %+v, want winner %+v", awaited, winner)
	}
}

func TestBrokerDefaultAndConfiguredTTL(t *testing.T) {
	tests := []struct {
		name string
		ttl  time.Duration
		want time.Duration
	}{
		{name: "default", ttl: 0, want: DefaultTTL},
		{name: "configured", ttl: 750 * time.Millisecond, want: 750 * time.Millisecond},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			broker := NewBroker(test.ttl)
			t.Cleanup(broker.Shutdown)
			pending, err := broker.Open(testDraft("session-ttl"))
			if err != nil {
				t.Fatalf("Open = %v", err)
			}
			if got := pending.ExpiresAt.Sub(pending.CreatedAt); got != test.want {
				t.Fatalf("TTL = %s, want %s", got, test.want)
			}
		})
	}
}

func TestBrokerRejectsUnsafeOrIncompleteDrafts(t *testing.T) {
	tests := []struct {
		name  string
		draft Draft
	}{
		{name: "missing session", draft: testDraft("")},
		{name: "missing run", draft: func() Draft { d := testDraft("session"); d.RunID = ""; return d }()},
		{name: "missing tool", draft: func() Draft { d := testDraft("session"); d.ToolName = ""; return d }()},
		{name: "unbounded summary", draft: func() Draft {
			d := testDraft("session")
			d.RedactedInput = strings.Repeat("x", maxRedactedInputBytes+1)
			return d
		}()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			broker := NewBroker(time.Second)
			t.Cleanup(broker.Shutdown)
			if _, err := broker.Open(test.draft); !errors.Is(err, ErrInvalidDraft) {
				t.Fatalf("Open = %v, want ErrInvalidDraft", err)
			}
		})
	}
}
