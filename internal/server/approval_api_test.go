package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/approval"
)

func openServerApproval(t *testing.T, s *Server, sessionID string) approval.Pending {
	t.Helper()
	pending, err := s.approvals.Open(approval.Draft{
		SessionID:       sessionID,
		SessionRevision: 3,
		RunID:           "run-test",
		ToolName:        "bash_exec",
		RedactedInput:   "git status [REDACTED]",
		InputDigest:     "sha256:test",
		DangerLevel:     "high",
		Scope:           "workspace",
	})
	if err != nil {
		t.Fatalf("Open approval = %v", err)
	}
	return pending
}

func approvalAPIRequest(t *testing.T, method, target, id, body string) (*http.Request, *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if id != "" {
		req.SetPathValue("id", id)
	}
	return req, httptest.NewRecorder()
}

func TestApprovalAPIListsAndGetsOnlyMatchingSession(t *testing.T) {
	s := New(nil, "", 0)
	t.Cleanup(s.ShutdownApprovals)
	pending := openServerApproval(t, s, "session-owner")

	req, rec := approvalAPIRequest(t, http.MethodGet, "/api/approvals?sessionId=session-owner", "", "")
	s.handleApprovalList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, pending.ID) || !strings.Contains(body, "[REDACTED]") {
		t.Fatalf("list body missing safe approval metadata: %s", body)
	}
	for _, forbidden := range []string{"rawInput", "rawCommand", "prompt", "session-owner"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("list body disclosed %q: %s", forbidden, body)
		}
	}

	req, rec = approvalAPIRequest(t, http.MethodGet, "/api/approvals/"+pending.ID+"?sessionId=session-owner", pending.ID, "")
	s.handleApprovalGet(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "session-owner") {
		t.Fatalf("get body disclosed session id: %s", rec.Body.String())
	}

	req, rec = approvalAPIRequest(t, http.MethodGet, "/api/approvals/"+pending.ID+"?sessionId=session-other", pending.ID, "")
	s.handleApprovalGet(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("wrong-session get status=%d body=%s, want 404", rec.Code, rec.Body.String())
	}
	wrongBody := rec.Body.String()

	req, rec = approvalAPIRequest(t, http.MethodGet, "/api/approvals/apr_unknown?sessionId=session-other", "apr_unknown", "")
	s.handleApprovalGet(rec, req)
	if rec.Code != http.StatusNotFound || rec.Body.String() != wrongBody {
		t.Fatalf("unknown response differs from wrong-session response: status=%d body=%s want=%s", rec.Code, rec.Body.String(), wrongBody)
	}

	req, rec = approvalAPIRequest(t, http.MethodGet, "/api/approvals", "", "")
	s.handleApprovalList(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing-session list status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
}

func TestApprovalAPIResolveAllowDenyDuplicateAndWrongSession(t *testing.T) {
	s := New(nil, "", 0)
	t.Cleanup(s.ShutdownApprovals)
	allowed := openServerApproval(t, s, "session-a")

	resolve := func(id, body string) *httptest.ResponseRecorder {
		req, rec := approvalAPIRequest(t, http.MethodPost, "/api/approvals/"+id+"/resolve", id, body)
		s.handleApprovalResolve(rec, req)
		return rec
	}

	rec := resolve(allowed.ID, `{"sessionId":"session-other","decision":"allow_once"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("wrong-session resolve status=%d body=%s, want 404", rec.Code, rec.Body.String())
	}

	rec = resolve(allowed.ID, `{"sessionId":"session-a","decision":"allow_once"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"decision":"allow_once"`) {
		t.Fatalf("allow resolve status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = resolve(allowed.ID, `{"sessionId":"session-a","decision":"allow_once"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate resolve status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}

	denied := openServerApproval(t, s, "session-a")
	rec = resolve(denied.ID, `{"sessionId":"session-a","outcome":"deny"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"decision":"deny"`) {
		t.Fatalf("deny resolve status=%d body=%s", rec.Code, rec.Body.String())
	}

	invalid := openServerApproval(t, s, "session-a")
	rec = resolve(invalid.ID, `{"sessionId":"session-a","decision":"allow_always"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid decision status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}

	rec = resolve(invalid.ID, `{"decision":"deny"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing session status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
}

func TestApprovalAPIExpiryIsHiddenAndCannotAllow(t *testing.T) {
	s := New(nil, "", 0)
	s.ShutdownApprovals()
	s.approvals = newApprovalHub(15 * time.Millisecond)
	t.Cleanup(s.ShutdownApprovals)
	pending := openServerApproval(t, s, "session-expiry")
	time.Sleep(20 * time.Millisecond)

	req, rec := approvalAPIRequest(t, http.MethodGet, "/api/approvals/"+pending.ID+"?sessionId=session-expiry", pending.ID, "")
	s.handleApprovalGet(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expired get status=%d body=%s, want 404", rec.Code, rec.Body.String())
	}

	req, rec = approvalAPIRequest(t, http.MethodPost, "/api/approvals/"+pending.ID+"/resolve", pending.ID, `{"sessionId":"session-expiry","decision":"allow_once"}`)
	s.handleApprovalResolve(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expired resolve status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}
}

func TestApprovalLifecycleCancelsOnAgentCancelAndShutdown(t *testing.T) {
	t.Run("agent cancel", func(t *testing.T) {
		s := New(nil, "", 0)
		t.Cleanup(s.ShutdownApprovals)
		sessionID, _, release, err := s.loops.Register(context.Background(), t.TempDir())
		if err != nil {
			t.Fatalf("Register loop = %v", err)
		}
		defer release()
		pending := openServerApproval(t, s, sessionID)

		req := httptest.NewRequest(http.MethodPost, "/api/agent/"+sessionID+"/cancel", nil)
		req.SetPathValue("sessionId", sessionID)
		rec := httptest.NewRecorder()
		s.handleAgentCancel(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("cancel status=%d body=%s", rec.Code, rec.Body.String())
		}
		resolution, err := s.approvals.Await(context.Background(), sessionID, pending.ID)
		if !errors.Is(err, approval.ErrSessionCanceled) {
			t.Fatalf("Await(agent cancel) = %v, want ErrSessionCanceled", err)
		}
		if resolution.Outcome != approval.OutcomeDeny || resolution.Reason != approval.ReasonSessionCanceled {
			t.Fatalf("cancel resolution = %+v", resolution)
		}
	})

	t.Run("server shutdown", func(t *testing.T) {
		s := New(nil, "", 0)
		pending := openServerApproval(t, s, "session-shutdown")
		result := make(chan struct {
			resolution approval.Resolution
			err        error
		}, 1)
		go func() {
			resolution, err := s.approvals.Await(context.Background(), pending.SessionID, pending.ID)
			result <- struct {
				resolution approval.Resolution
				err        error
			}{resolution, err}
		}()

		s.ShutdownApprovals()
		got := <-result
		if !errors.Is(got.err, approval.ErrBrokerClosed) {
			t.Fatalf("Await(shutdown) = %v, want ErrBrokerClosed", got.err)
		}
		if got.resolution.Outcome != approval.OutcomeDeny || got.resolution.Reason != approval.ReasonBrokerShutdown {
			t.Fatalf("shutdown resolution = %+v", got.resolution)
		}
	})
}

func TestApprovalHubCancelTombstonesSessionAndKeepsOwnershipPrivate(t *testing.T) {
	s := New(nil, "", 0)
	t.Cleanup(s.ShutdownApprovals)
	pending := openServerApproval(t, s, "session-tombstone")

	s.approvals.CancelSession(pending.SessionID)
	if got := s.approvals.list(pending.SessionID); len(got) != 0 {
		t.Fatalf("pending after CancelSession = %#v, want empty", got)
	}
	if _, ok := s.approvals.get(pending.SessionID, pending.ID); ok {
		t.Fatal("get(canceled approval) succeeded")
	}
	if _, err := s.approvals.Open(approval.Draft{
		SessionID: "session-tombstone",
		RunID:     "run-late",
		ToolName:  "bash_exec",
	}); !errors.Is(err, approval.ErrSessionCanceled) {
		t.Fatalf("Open(canceled session) = %v, want ErrSessionCanceled", err)
	}

	_, wrongErr := s.approvals.Resolve(approval.Decision{
		ApprovalID: pending.ID,
		SessionID:  "session-other",
		Outcome:    approval.OutcomeAllowOnce,
	})
	_, unknownErr := s.approvals.Resolve(approval.Decision{
		ApprovalID: "apr_unknown",
		SessionID:  "session-other",
		Outcome:    approval.OutcomeAllowOnce,
	})
	if !errors.Is(wrongErr, approval.ErrApprovalNotFound) || wrongErr.Error() != unknownErr.Error() {
		t.Fatalf("wrong-session error=%v unknown error=%v, want identical not-found", wrongErr, unknownErr)
	}
}
