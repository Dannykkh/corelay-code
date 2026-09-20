package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/protocol"
	"github.com/Dannykkh/corelay-code/internal/types"
	"github.com/Dannykkh/corelay-code/internal/workstream"
)

type failingDurableImageCommitStore struct {
	*agent.SessionStore
	err error
}

func (s *failingDurableImageCommitStore) SaveExpected(*agent.Session, uint64) error {
	return s.err
}

func durableImageRequestForTest(t *testing.T, imageBytes []byte) []types.Message {
	t.Helper()
	content, err := json.Marshal([]any{
		map[string]string{"type": "text", "text": "inspect image"},
		map[string]any{
			"type": "image",
			"source": map[string]string{
				"type": "base64", "media_type": "image/png",
				"data": base64.StdEncoding.EncodeToString(imageBytes),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return []types.Message{{Role: "user", Content: content}}
}

func createApprovedImagePlanForTest(t *testing.T, workDir, workstreamID, planID, stageID string) (*workstream.Store, *workstream.Plan) {
	t.Helper()
	workstreams := workstream.NewStore(workDir)
	if _, err := workstreams.Create(workstream.CreateRequest{ID: workstreamID, Title: planID}); err != nil {
		t.Fatal(err)
	}
	setup := New(nil, "", 0)
	setup.SetWorkDir(workDir)
	plan := createWorkstreamPlanAPIForTest(t, setup, workDir, workstreamID, planID, stageID)
	approved, err := workstreams.ApprovePlan(workstreamID, plan.ID, workstream.ApprovePlanRequest{
		ExpectedRevision: plan.Revision, ExpectedStateRevision: plan.StateRevision,
	})
	if err != nil {
		t.Fatal(err)
	}
	return workstreams, approved
}

func TestAgentLoopTransportsImageAndKeepsDurableTranscriptDisplaySafe(t *testing.T) {
	t.Setenv("CORELAY_MEMORY", "off")
	t.Setenv("CORELAY_AUTOSKILL", "off")
	t.Setenv("CORELAY_AUTOVERIFY", "off")
	workDir := t.TempDir()
	imageBytes := agentImagePNGFixture(t)
	encodedImage := base64.StdEncoding.EncodeToString(imageBytes)
	storeBase := t.TempDir()
	store := agent.NewSessionStore(storeBase)
	session := agent.Session{
		Workspace: workDir,
		Messages: []agent.SessionMessage{{
			Role: "user", Content: "Inspect this [📎 image attached]",
		}},
	}
	if err := store.Save(&session); err != nil {
		t.Fatal(err)
	}
	provider := &imageTransportCaptureProvider{}
	server := New(provider, "fake-model", 0)
	server.SetWorkDir(workDir)
	server.SetSessionStore(store)
	body, err := json.Marshal(map[string]any{
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]string{"type": "text", "text": "Inspect this image."},
				map[string]any{"type": "image", "source": map[string]string{
					"type": "base64", "media_type": "image/png", "data": encodedImage,
				}},
			},
		}},
		"durableSessionId": session.ID,
		"expectedRevision": session.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.handleAgentLoop(recorder, httptest.NewRequest(http.MethodPost, "/api/agent", bytes.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	requests := provider.requestSnapshot()
	if len(requests) != 1 {
		t.Fatalf("provider calls=%d, want one", len(requests))
	}
	var foundImage bool
	for _, message := range requests[0].Messages {
		if message.Role != "user" {
			continue
		}
		var blocks []types.ContentBlockParam
		if err := json.Unmarshal(message.Content, &blocks); err != nil {
			continue
		}
		for _, block := range blocks {
			if block.Type != "image" {
				continue
			}
			if block.Source == nil || block.Source.MediaType != "image/png" {
				t.Fatalf("provider image source=%+v, want image/png", block.Source)
			}
			decoded, err := base64.StdEncoding.Strict().DecodeString(block.Source.Data)
			if err != nil || !bytes.Equal(decoded, imageBytes) {
				t.Fatalf("provider image differs from upload: decoded=%d err=%v", len(decoded), err)
			}
			foundImage = true
		}
	}
	if !foundImage {
		t.Fatalf("provider request contains no image block: %s", serverMessagesDump(requests[0].Messages))
	}
	committed, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	transcript, _ := json.Marshal(committed.Messages)
	if strings.Contains(string(transcript), encodedImage) || strings.Contains(string(transcript), "base64,") {
		t.Fatalf("image bytes leaked into durable transcript: %s", transcript)
	}
	if len(committed.Messages[0].Attachments) != 1 {
		t.Fatalf("durable user message image refs=%+v, want one owned reference", committed.Messages[0].Attachments)
	}

	// Reload through a fresh store instance, then continue with text only. The
	// server must reconstruct the earlier image from the durable session blob.
	reopened := agent.NewSessionStore(storeBase)
	resumed, err := reopened.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Chat currently sends display-only message fields on every save. The
	// session store must retain owned refs for an unchanged existing message.
	resumed.Messages[0].Attachments = nil
	resumed.Messages = append(resumed.Messages, agent.SessionMessage{
		Role: "user", Content: "Describe that image again.", Timestamp: resumed.UpdatedAt.Add(time.Second),
	})
	if err := reopened.SaveExpected(resumed, resumed.Revision); err != nil {
		t.Fatal(err)
	}
	server.SetSessionStore(reopened)
	resumeBody, err := json.Marshal(map[string]any{
		"messages": []map[string]string{
			{"role": "user", "content": "Inspect this [📎 image attached]"},
			{"role": "assistant", "content": "Image received."},
			{"role": "user", "content": "Describe that image again."},
		},
		"durableSessionId": session.ID,
		"expectedRevision": resumed.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	resumeRecorder := httptest.NewRecorder()
	server.handleAgentLoop(resumeRecorder, httptest.NewRequest(http.MethodPost, "/api/agent", bytes.NewReader(resumeBody)))
	if resumeRecorder.Code != http.StatusOK {
		t.Fatalf("resume status=%d body=%s", resumeRecorder.Code, resumeRecorder.Body.String())
	}
	requests = provider.requestSnapshot()
	if len(requests) != 2 {
		t.Fatalf("provider calls after resume=%d, want two", len(requests))
	}
	if got := capturedImageBytes(t, requests[1].Messages); !bytes.Equal(got, imageBytes) {
		t.Fatalf("resumed provider image differs from original: got=%d want=%d", len(got), len(imageBytes))
	}
	if strings.Contains(resumeRecorder.Body.String(), encodedImage) {
		t.Fatal("image bytes leaked into resumed event stream")
	}
}

func TestAgentLoopRejectsImagesWhenSelectedModelCapabilityIsNotSupported(t *testing.T) {
	imageBytes := agentImagePNGFixture(t)
	cases := []struct {
		name       string
		model      string
		models     []types.ModelInfo
		persisted  bool
		wantCode   string
		wantReason string
	}{
		{
			name:       "explicitly unsupported",
			model:      "fake-model",
			models:     []types.ModelInfo{{ID: "fake-model", ImageInput: types.ImageInputUnsupported}},
			wantCode:   "image_input_unsupported",
			wantReason: "does not support image input",
		},
		{
			name:       "registered but unknown",
			model:      "fake-model",
			models:     []types.ModelInfo{{ID: "fake-model"}},
			wantCode:   "image_input_capability_unknown",
			wantReason: "is unverified",
		},
		{
			name:       "unregistered model",
			model:      "custom-model",
			models:     []types.ModelInfo{{ID: "fake-model", ImageInput: types.ImageInputSupported}},
			wantCode:   "image_input_capability_unknown",
			wantReason: "is unverified",
		},
		{
			name:       "persisted image history with marker-only request",
			model:      "fake-model",
			models:     []types.ModelInfo{{ID: "fake-model"}},
			persisted:  true,
			wantCode:   "image_input_capability_unknown",
			wantReason: "is unverified",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			workDir := t.TempDir()
			store := agent.NewSessionStore(t.TempDir())
			session := agent.Session{
				Workspace: workDir,
				Messages:  []agent.SessionMessage{{Role: "user", Content: "Inspect this [📎 image attached]"}},
			}
			if err := store.Save(&session); err != nil {
				t.Fatal(err)
			}
			wantStoredImageCount := 0
			var wantStoredImageBytes int64
			if testCase.persisted {
				memory, _, err := store.OpenToolResultMemory(session.ID, workDir)
				if err != nil {
					t.Fatal(err)
				}
				reference, err := memory.StoreImage("image/png", imageBytes)
				if err != nil {
					t.Fatal(err)
				}
				session.Messages[0].Attachments = []agent.SessionImageReference{reference}
				if err := store.SaveExpected(&session, session.Revision); err != nil {
					t.Fatal(err)
				}
				wantStoredImageCount = 1
				wantStoredImageBytes = int64(len(imageBytes))
			}
			beforeRevision := session.Revision
			capture := &imageTransportCaptureProvider{}
			provider := &configuredImageTransportProvider{imageTransportCaptureProvider: capture, models: testCase.models}
			server := New(provider, testCase.model, 0)
			server.SetWorkDir(workDir)
			server.SetSessionStore(store)
			messages := any(durableImageRequestForTest(t, imageBytes))
			if testCase.persisted {
				messages = []map[string]string{{"role": "user", "content": "Inspect this [📎 image attached]"}}
			}
			body, err := json.Marshal(map[string]any{
				"messages":         messages,
				"durableSessionId": session.ID,
				"expectedRevision": session.Revision,
			})
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			server.handleAgentLoop(recorder, httptest.NewRequest(http.MethodPost, "/api/agent", bytes.NewReader(body)))
			if recorder.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status=%d body=%s, want 422", recorder.Code, recorder.Body.String())
			}
			var response struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode error response: %v (%s)", err, recorder.Body.String())
			}
			if response.Error.Code != testCase.wantCode || !strings.Contains(response.Error.Message, testCase.wantReason) {
				t.Fatalf("image capability error = (%q, %q), want code %q and message containing %q", response.Error.Code, response.Error.Message, testCase.wantCode, testCase.wantReason)
			}
			if capture.callCount() != 0 {
				t.Fatalf("provider calls=%d, want zero", capture.callCount())
			}
			unchanged, err := store.Get(session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if unchanged.Revision != beforeRevision || len(unchanged.Messages[0].Attachments) != wantStoredImageCount {
				t.Fatalf("rejected request changed durable session: revision=%d refs=%+v", unchanged.Revision, unchanged.Messages[0].Attachments)
			}
			memory, _, err := store.OpenToolResultMemory(session.ID, workDir)
			if err != nil {
				t.Fatal(err)
			}
			if count, storedBytes := memory.ImageStats(); count != wantStoredImageCount || storedBytes != wantStoredImageBytes {
				t.Fatalf("rejected request changed stored images: count=%d bytes=%d", count, storedBytes)
			}
		})
	}
}

func TestAgentLoopAllowsTextOnlyRequestForUnknownImageCapability(t *testing.T) {
	workDir := t.TempDir()
	capture := &imageTransportCaptureProvider{}
	provider := &configuredImageTransportProvider{
		imageTransportCaptureProvider: capture,
		models:                        []types.ModelInfo{{ID: "fake-model"}},
	}
	server := New(provider, "fake-model", 0)
	server.SetWorkDir(workDir)
	body, err := json.Marshal(map[string]any{
		"messages": []map[string]string{{"role": "user", "content": "plain text request"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.handleAgentLoop(recorder, httptest.NewRequest(http.MethodPost, "/api/agent", bytes.NewReader(body)))
	if recorder.Code != http.StatusOK || capture.callCount() != 1 {
		t.Fatalf("text-only unknown-capability run status=%d provider calls=%d body=%s", recorder.Code, capture.callCount(), recorder.Body.String())
	}
}

func TestCanonicalProtocolRejectsImagesBeforeUnsupportedProviderCall(t *testing.T) {
	imageBytes := agentImagePNGFixture(t)
	for _, testCase := range []struct {
		name     string
		cap      types.ImageInputCapability
		wantCode string
	}{
		{name: "unsupported", cap: types.ImageInputUnsupported, wantCode: "image_input_unsupported"},
		{name: "unknown", cap: types.ImageInputUnknown, wantCode: "image_input_capability_unknown"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			capture := &imageTransportCaptureProvider{}
			provider := &configuredImageTransportProvider{
				imageTransportCaptureProvider: capture,
				models:                        []types.ModelInfo{{ID: "fake-model", ImageInput: testCase.cap}},
			}
			server := New(provider, "fake-model", 0)
			message := durableImageRequestForTest(t, imageBytes)
			body, err := json.Marshal(map[string]any{
				"model": "fake-model", "max_tokens": 64, "stream": false, "messages": message,
			})
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			server.handleCanonicalProtocol(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body)), protocol.AnthropicMessages)
			if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), testCase.wantCode) {
				t.Fatalf("protocol image response status=%d body=%s, want 422 with %q", recorder.Code, recorder.Body.String(), testCase.wantCode)
			}
			if !strings.Contains(strings.ToLower(recorder.Body.String()), "image input") {
				t.Fatalf("protocol error is not actionable: %s", recorder.Body.String())
			}
			if capture.callCount() != 0 {
				t.Fatalf("provider calls=%d, want zero", capture.callCount())
			}
		})
	}
}

func TestCanonicalProtocolRechecksImageCapabilityAfterAutomaticModelFallback(t *testing.T) {
	imageBytes := agentImagePNGFixture(t)
	capture := &imageTransportCaptureProvider{
		retryError:    errors.New("529 overloaded"),
		retryFailures: 3,
	}
	provider := &configuredImageTransportProvider{
		imageTransportCaptureProvider: capture,
		models:                        []types.ModelInfo{{ID: "gpt-4o", ImageInput: types.ImageInputSupported}},
		name:                          "openai",
	}
	server := New(provider, "gpt-4o", 0)
	message := durableImageRequestForTest(t, imageBytes)
	body, err := json.Marshal(map[string]any{
		"model": "gpt-4o", "max_tokens": 64, "stream": false, "messages": message,
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.handleCanonicalProtocol(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body)), protocol.AnthropicMessages)
	if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), "image_input_capability_unknown") {
		t.Fatalf("fallback image response status=%d body=%s, want explicit unknown capability 422", recorder.Code, recorder.Body.String())
	}
	if calls := capture.requestSnapshot(); len(calls) != 3 {
		t.Fatalf("provider calls=%d, want three supported-primary attempts and no fallback call", len(calls))
	} else {
		for index, request := range calls {
			if request.Model != "gpt-4o" {
				t.Fatalf("request %d model=%q, want only supported primary gpt-4o", index+1, request.Model)
			}
		}
	}
}

func TestPlanImagePreflightFailureCleansPreparedBlob(t *testing.T) {
	workDir := t.TempDir()
	store := agent.NewSessionStore(t.TempDir())
	session := agent.Session{
		Workspace: workDir,
		Messages:  []agent.SessionMessage{{Role: "user", Content: "inspect [📎 image attached]"}},
	}
	if err := store.Save(&session); err != nil {
		t.Fatal(err)
	}
	run, err := prepareDurableAgentRun(store, session.ID, session.Revision, workDir, durableImageRequestForTest(t, agentImagePNGFixture(t)))
	if err != nil {
		t.Fatal(err)
	}
	if err := run.PrepareImages(durableImageRequestForTest(t, agentImagePNGFixture(t))); err != nil {
		t.Fatal(err)
	}
	if count, _ := run.toolResultMemory.ImageStats(); count != 1 {
		t.Fatalf("prepared image count=%d, want one before plan preflight", count)
	}

	workstreams, plan := createApprovedImagePlanForTest(t, workDir, "ws_plan_image_begin_failure", "plan_image_begin_failure", "research")
	_, err = beginWorkstreamPlanStageForAgentRun(workstreams, run, "ws_plan_image_begin_failure", plan.ID,
		workstream.BeginPlanStageRequest{
			ExpectedRevision: plan.Revision + 1, ExpectedStateRevision: plan.StateRevision,
			StageID: "research", RunID: "run_image_begin_failure",
		})
	if err == nil {
		t.Fatal("stale plan preflight unexpectedly started a stage")
	}
	if run.pendingImages != nil {
		t.Fatal("failed plan preflight retained pending image revision")
	}
	if count, _ := run.toolResultMemory.ImageStats(); count != 0 {
		t.Fatalf("failed plan preflight left %d unreferenced image blobs", count)
	}
	persisted, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Revision != session.Revision || len(persisted.Messages[0].Attachments) != 0 {
		t.Fatalf("failed plan preflight changed durable session: %+v", persisted)
	}
}

func TestPlanImageCommitFailureClosesStartedStageAsNotRun(t *testing.T) {
	workDir := t.TempDir()
	baseStore := agent.NewSessionStore(t.TempDir())
	session := agent.Session{
		Workspace: workDir,
		Messages:  []agent.SessionMessage{{Role: "user", Content: "inspect [📎 image attached]"}},
	}
	if err := baseStore.Save(&session); err != nil {
		t.Fatal(err)
	}
	commitErr := errors.New("injected session persistence failure")
	store := &failingDurableImageCommitStore{SessionStore: baseStore, err: commitErr}
	request := durableImageRequestForTest(t, agentImagePNGFixture(t))
	run, err := prepareDurableAgentRun(store, session.ID, session.Revision, workDir, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.PrepareImages(request); err != nil {
		t.Fatal(err)
	}
	workstreams, plan := createApprovedImagePlanForTest(t, workDir, "ws_plan_image_commit_failure", "plan_image_commit_failure", "research")
	startedPlan, err := workstreams.BeginPlanStage("ws_plan_image_commit_failure", plan.ID, workstream.BeginPlanStageRequest{
		ExpectedRevision: plan.Revision, ExpectedStateRevision: plan.StateRevision,
		StageID: "research", RunID: "run_image_commit_failure",
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := &workstreamPlanStageRecorder{
		store: workstreams, workstreamID: "ws_plan_image_commit_failure",
		planID: plan.ID, stageID: "research", revision: plan.Revision,
		stateRevision: startedPlan.StateRevision, runID: "run_image_commit_failure",
	}
	err = commitDurableImagesForPlanRun(run, recorder)
	if !errors.Is(err, commitErr) {
		t.Fatalf("image commit error=%v, want injected persistence failure", err)
	}
	if run.pendingImages != nil {
		t.Fatal("failed image commit retained pending image revision")
	}
	if count, _ := run.toolResultMemory.ImageStats(); count != 0 {
		t.Fatalf("failed image commit left %d unreferenced blobs", count)
	}
	persisted, err := baseStore.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Revision != session.Revision || len(persisted.Messages[0].Attachments) != 0 {
		t.Fatalf("failed image commit changed durable session: %+v", persisted)
	}
	failedPlan, err := workstreams.GetPlan("ws_plan_image_commit_failure", plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempt := failedPlan.Stages[0].Attempts[0]
	if failedPlan.Status != workstream.PlanStatusFailed ||
		failedPlan.Stages[0].Status != workstream.PlanStageStatusFailed ||
		attempt.Status != workstream.PlanStageAttemptFailed ||
		attempt.VerificationStatus != "not-run" || attempt.Evidence != nil || attempt.ReceiptDigest != "" {
		t.Fatalf("failed pre-execution attempt was not closed as not-run: %+v", failedPlan)
	}
}

func TestAgentLoopRejectsCorruptDurableImageBeforeProviderCall(t *testing.T) {
	workDir := t.TempDir()
	storeBase := t.TempDir()
	store := agent.NewSessionStore(storeBase)
	session := agent.Session{Workspace: workDir, Messages: []agent.SessionMessage{{Role: "user", Content: "inspect"}}}
	if err := store.Save(&session); err != nil {
		t.Fatal(err)
	}
	imageBytes := agentImagePNGFixture(t)
	memory, _, err := store.OpenToolResultMemory(session.ID, workDir)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := memory.StoreImage("image/png", imageBytes)
	if err != nil {
		t.Fatal(err)
	}
	session.Messages[0].Attachments = []agent.SessionImageReference{reference}
	if err := store.SaveExpected(&session, session.Revision); err != nil {
		t.Fatal(err)
	}
	imagePath := durableImageBlobPath(t, storeBase, workDir, session.ID, reference.Digest)
	if err := os.WriteFile(imagePath, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := &imageTransportCaptureProvider{}
	server := New(provider, "fake-model", 0)
	server.SetWorkDir(workDir)
	server.SetSessionStore(store)
	body, err := json.Marshal(map[string]any{
		"messages":         []map[string]string{{"role": "user", "content": "inspect"}},
		"durableSessionId": session.ID,
		"expectedRevision": session.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.handleAgentLoop(recorder, httptest.NewRequest(http.MethodPost, "/api/agent", bytes.NewReader(body)))
	if recorder.Code != http.StatusUnprocessableEntity || provider.callCount() != 0 {
		t.Fatalf("corrupt image response status=%d provider calls=%d body=%s", recorder.Code, provider.callCount(), recorder.Body.String())
	}
}

func TestAgentLoopDuplicateImagesKeepBothProviderBlocksAndDurableRefs(t *testing.T) {
	workDir := t.TempDir()
	store := agent.NewSessionStore(t.TempDir())
	session := agent.Session{Workspace: workDir, Messages: []agent.SessionMessage{{Role: "user", Content: "inspect"}}}
	if err := store.Save(&session); err != nil {
		t.Fatal(err)
	}
	imageBytes := agentImagePNGFixture(t)
	encoded := base64.StdEncoding.EncodeToString(imageBytes)
	provider := &imageTransportCaptureProvider{}
	server := New(provider, "fake-model", 0)
	server.SetWorkDir(workDir)
	server.SetSessionStore(store)
	body, err := json.Marshal(map[string]any{
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]string{"type": "text", "text": "inspect"},
				map[string]any{"type": "image", "source": map[string]string{"type": "base64", "media_type": "image/png", "data": encoded}},
				map[string]any{"type": "image", "source": map[string]string{"type": "base64", "media_type": "image/png", "data": encoded}},
			},
		}},
		"durableSessionId": session.ID,
		"expectedRevision": session.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.handleAgentLoop(recorder, httptest.NewRequest(http.MethodPost, "/api/agent", bytes.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	committed, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(committed.Messages[0].Attachments) != 2 {
		t.Fatalf("durable duplicate refs=%+v, want two", committed.Messages[0].Attachments)
	}
	requests := provider.requestSnapshot()
	if len(requests) != 1 || capturedImageCount(requests[0].Messages) != 2 {
		t.Fatalf("provider calls/images = %d/%d, want 1/2", len(requests), func() int {
			if len(requests) == 0 {
				return 0
			}
			return capturedImageCount(requests[0].Messages)
		}())
	}
}

func TestAgentLoopImageBudgetRejectionDoesNotAdvanceSessionRevision(t *testing.T) {
	workDir := t.TempDir()
	store := agent.NewSessionStore(t.TempDir())
	imageBytes := agentImagePNGFixture(t)
	newImageBytes := agentImagePNGFixtureWithColor(t, color.RGBA{R: 230, G: 40, B: 190, A: 255})
	session := &agent.Session{Workspace: workDir}
	for index := 0; index < protocol.MaxImageBlocks; index++ {
		session.Messages = append(session.Messages, agent.SessionMessage{Role: "user", Content: fmt.Sprintf("image turn %d", index)})
	}
	session.Messages = append(session.Messages, agent.SessionMessage{Role: "user", Content: "current image [📎 image attached]"})
	if err := store.Save(session); err != nil {
		t.Fatal(err)
	}
	memory, _, err := store.OpenToolResultMemory(session.ID, workDir)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := memory.StoreImage("image/png", imageBytes)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < protocol.MaxImageBlocks; index++ {
		session.Messages[index].Attachments = []agent.SessionImageReference{reference}
	}
	if err := store.SaveExpected(session, session.Revision); err != nil {
		t.Fatal(err)
	}
	beforeRevision := session.Revision
	encoded := base64.StdEncoding.EncodeToString(newImageBytes)
	messages := make([]any, 0, len(session.Messages))
	for index, message := range session.Messages {
		if index == len(session.Messages)-1 {
			messages = append(messages, map[string]any{
				"role": "user",
				"content": []any{
					map[string]string{"type": "text", "text": "a ninth image"},
					map[string]any{"type": "image", "source": map[string]string{"type": "base64", "media_type": "image/png", "data": encoded}},
				},
			})
			continue
		}
		messages = append(messages, map[string]string{"role": "user", "content": message.Content})
	}
	provider := &imageTransportCaptureProvider{}
	server := New(provider, "fake-model", 0)
	server.SetWorkDir(workDir)
	server.SetSessionStore(store)
	makeRequest := func(messageValues []any, revision uint64) *httptest.ResponseRecorder {
		t.Helper()
		body, marshalErr := json.Marshal(map[string]any{
			"messages": messageValues, "durableSessionId": session.ID, "expectedRevision": revision,
		})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		recorder := httptest.NewRecorder()
		server.handleAgentLoop(recorder, httptest.NewRequest(http.MethodPost, "/api/agent", bytes.NewReader(body)))
		return recorder
	}
	tooMany := makeRequest(messages, beforeRevision)
	if tooMany.Code != http.StatusUnprocessableEntity || !strings.Contains(tooMany.Body.String(), "session_image_budget_exceeded") {
		t.Fatalf("ninth image response status=%d body=%s", tooMany.Code, tooMany.Body.String())
	}
	unchanged, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Revision != beforeRevision || len(unchanged.Messages[len(unchanged.Messages)-1].Attachments) != 0 || provider.callCount() != 0 {
		t.Fatalf("budget rejection mutated session/provider: revision=%d refs=%+v calls=%d", unchanged.Revision, unchanged.Messages[len(unchanged.Messages)-1].Attachments, provider.callCount())
	}
	if imageCount, imageBytesStored := memory.ImageStats(); imageCount != 1 || imageBytesStored != int64(len(imageBytes)) {
		t.Fatalf("rejected image left an orphan: image count=%d bytes=%d", imageCount, imageBytesStored)
	}

	// A text-only retry still resumes all eight retained images successfully.
	messages[len(messages)-1] = map[string]string{"role": "user", "content": "continue using prior images"}
	retried := makeRequest(messages, beforeRevision)
	if retried.Code != http.StatusOK {
		t.Fatalf("text retry status=%d body=%s", retried.Code, retried.Body.String())
	}
	requests := provider.requestSnapshot()
	maxImages := 0
	for _, request := range requests {
		if count := capturedImageCount(request.Messages); count > maxImages {
			maxImages = count
		}
	}
	if maxImages != protocol.MaxImageBlocks {
		t.Fatalf("resumed provider max images=%d calls=%d, want %d images in one request", maxImages, len(requests), protocol.MaxImageBlocks)
	}
}

func durableImageBlobPath(t *testing.T, storeBase, workspace, sessionID, digest string) string {
	t.Helper()
	canonical, err := filepath.Abs(filepath.Clean(workspace))
	if err != nil {
		t.Fatal(err)
	}
	if resolved, err := filepath.EvalSymlinks(canonical); err == nil {
		canonical = filepath.Clean(resolved)
	}
	if runtime.GOOS == "windows" {
		canonical = strings.ToLower(canonical)
	}
	workspaceDigest := sha256.Sum256([]byte(canonical))
	label := "ws_" + hex.EncodeToString(workspaceDigest[:]) + ":" + sessionID
	storeDigest := sha256.Sum256([]byte(label))
	storeName := "sm_" + hex.EncodeToString(storeDigest[:])
	imageName := "image_" + strings.TrimPrefix(digest, "sha256:") + ".blob"
	return filepath.Join(storeBase, "session-output", storeName, imageName)
}

func capturedImageBytes(t *testing.T, messages []types.Message) []byte {
	t.Helper()
	for _, message := range messages {
		if message.Role != "user" {
			continue
		}
		var blocks []types.ContentBlockParam
		if err := json.Unmarshal(message.Content, &blocks); err != nil {
			continue
		}
		for _, block := range blocks {
			if block.Type != "image" || block.Source == nil {
				continue
			}
			decoded, err := base64.StdEncoding.Strict().DecodeString(block.Source.Data)
			if err != nil {
				t.Fatalf("decode captured image: %v", err)
			}
			return decoded
		}
	}
	return nil
}

func capturedImageCount(messages []types.Message) int {
	count := 0
	for _, message := range messages {
		if message.Role != "user" {
			continue
		}
		var blocks []types.ContentBlockParam
		if json.Unmarshal(message.Content, &blocks) != nil {
			continue
		}
		for _, block := range blocks {
			if block.Type == "image" {
				count++
			}
		}
	}
	return count
}

func TestAgentLoopRejectsInvalidImageAndOversizedBodyBeforeProviderCall(t *testing.T) {
	workDir := t.TempDir()
	provider := &imageTransportCaptureProvider{}
	server := New(provider, "fake-model", 0)
	server.SetWorkDir(workDir)
	validPNG := agentImagePNGFixture(t)
	invalidImageBody, err := json.Marshal(map[string]any{
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type": "image", "source": map[string]string{
					"type": "base64", "media_type": "image/jpeg", "data": base64.StdEncoding.EncodeToString(validPNG),
				},
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	invalidImageRecorder := httptest.NewRecorder()
	server.handleAgentLoop(invalidImageRecorder, httptest.NewRequest(http.MethodPost, "/api/agent", bytes.NewReader(invalidImageBody)))
	if invalidImageRecorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid image status=%d body=%s, want 400", invalidImageRecorder.Code, invalidImageRecorder.Body.String())
	}
	if provider.callCount() != 0 {
		t.Fatalf("provider calls=%d after invalid image, want 0", provider.callCount())
	}

	oversizedRecorder := httptest.NewRecorder()
	oversizedBody := `{"messages":[{"role":"user","content":"hello"}]}` + strings.Repeat(" ", protocol.MaxRequestBytes)
	server.handleAgentLoop(oversizedRecorder, httptest.NewRequest(
		http.MethodPost,
		"/api/agent",
		strings.NewReader(oversizedBody),
	))
	if oversizedRecorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body status=%d body=%s, want 413", oversizedRecorder.Code, oversizedRecorder.Body.String())
	}
	if provider.callCount() != 0 {
		t.Fatalf("provider calls=%d after oversized body, want 0", provider.callCount())
	}
}

func agentImagePNGFixture(t *testing.T) []byte {
	return agentImagePNGFixtureWithColor(t, color.RGBA{R: 20, G: 110, B: 240, A: 255})
}

func agentImagePNGFixtureWithColor(t *testing.T, value color.RGBA) []byte {
	t.Helper()
	fixture := image.NewRGBA(image.Rect(0, 0, 2, 2))
	fixture.Set(0, 0, value)
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, fixture); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func serverMessagesDump(messages []types.Message) string {
	encoded, _ := json.Marshal(messages)
	return string(encoded)
}

type imageTransportCaptureProvider struct {
	mu            sync.Mutex
	requests      []*types.MessagesRequest
	retryError    error
	retryFailures int
}

func (p *imageTransportCaptureProvider) Name() string        { return "fake" }
func (p *imageTransportCaptureProvider) DisplayName() string { return "Fake" }
func (p *imageTransportCaptureProvider) Models() []types.ModelInfo {
	return []types.ModelInfo{{ID: "fake-model", ImageInput: types.ImageInputSupported}}
}
func (p *imageTransportCaptureProvider) Validate() error { return nil }

type configuredImageTransportProvider struct {
	*imageTransportCaptureProvider
	models []types.ModelInfo
	name   string
}

func (p *configuredImageTransportProvider) Name() string {
	if p.name != "" {
		return p.name
	}
	return p.imageTransportCaptureProvider.Name()
}

func (p *configuredImageTransportProvider) Models() []types.ModelInfo {
	return append([]types.ModelInfo(nil), p.models...)
}

func (p *imageTransportCaptureProvider) requestSnapshot() []*types.MessagesRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*types.MessagesRequest(nil), p.requests...)
}

func (p *imageTransportCaptureProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

func (p *imageTransportCaptureProvider) StreamMessage(_ context.Context, request *types.MessagesRequest, _ *types.StreamOptions) (<-chan types.SSEEvent, error) {
	p.mu.Lock()
	index := len(p.requests)
	copyRequest := *request
	copyRequest.Messages = append([]types.Message(nil), request.Messages...)
	for i := range copyRequest.Messages {
		copyRequest.Messages[i].Content = append(json.RawMessage(nil), request.Messages[i].Content...)
	}
	p.requests = append(p.requests, &copyRequest)
	retry := p.retryError != nil && index < p.retryFailures
	retryError := p.retryError
	p.mu.Unlock()
	if retry {
		return nil, retryError
	}

	events := make(chan types.SSEEvent, 5)
	go func() {
		defer close(events)
		events <- types.SSEEvent{Type: "content_block_start", ContentBlock: json.RawMessage(`{"type":"text"}`)}
		events <- types.SSEEvent{Type: "content_block_delta", Delta: json.RawMessage(`{"type":"text_delta","text":"Image received."}`)}
		events <- types.SSEEvent{Type: "content_block_stop"}
		events <- types.SSEEvent{Type: "message_delta", Delta: json.RawMessage(`{"stop_reason":"end_turn"}`)}
		events <- types.SSEEvent{Type: "message_stop"}
	}()
	return events, nil
}
