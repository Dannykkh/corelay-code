package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestWorkstreamAPI_CreateListGetHandoff(t *testing.T) {
	workDir := t.TempDir()
	s := New(nil, "", 0)
	s.SetWorkDir(workDir)

	body := []byte(`{
		"id":"ws_api",
		"title":"API Workstream",
		"summary":"server api test",
		"nextAction":"generate handoff",
		"goal":{"objective":"prove api works","acceptanceCriteria":["created","handoff"]}
	}`)
	createReq := httptest.NewRequest(http.MethodPost, "/api/workstreams", bytes.NewReader(body))
	createRec := httptest.NewRecorder()
	s.handleWorkstreamCreate(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", createRec.Code, createRec.Body.String())
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/workstreams", nil)
	listRec := httptest.NewRecorder()
	s.handleWorkstreamList(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", listRec.Code, listRec.Body.String())
	}
	var listed struct {
		Workstreams []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"workstreams"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("list json: %v", err)
	}
	if len(listed.Workstreams) != 1 || listed.Workstreams[0].ID != "ws_api" {
		t.Fatalf("listed workstreams = %+v", listed.Workstreams)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/workstreams/ws_api", nil)
	getReq.SetPathValue("id", "ws_api")
	getRec := httptest.NewRecorder()
	s.handleWorkstreamGet(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", getRec.Code, getRec.Body.String())
	}

	handoffReq := httptest.NewRequest(http.MethodPost, "/api/workstreams/ws_api/handoff", bytes.NewReader([]byte(`{}`)))
	handoffReq.SetPathValue("id", "ws_api")
	handoffRec := httptest.NewRecorder()
	s.handleWorkstreamHandoff(handoffRec, handoffReq)
	if handoffRec.Code != http.StatusOK {
		t.Fatalf("handoff status=%d body=%s", handoffRec.Code, handoffRec.Body.String())
	}
	var handoff struct {
		Path     string `json:"path"`
		Markdown string `json:"markdown"`
	}
	if err := json.Unmarshal(handoffRec.Body.Bytes(), &handoff); err != nil {
		t.Fatalf("handoff json: %v", err)
	}
	if handoff.Path == "" || handoff.Markdown == "" {
		t.Fatalf("empty handoff response: %+v", handoff)
	}
	if _, err := os.Stat(handoff.Path); err != nil {
		t.Fatalf("handoff file missing: %v", err)
	}
}
