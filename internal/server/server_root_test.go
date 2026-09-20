package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/buildinfo"
	"github.com/Dannykkh/corelay-code/internal/types"
)

type rootMetadataProvider struct{}

func (rootMetadataProvider) Name() string              { return "private-provider" }
func (rootMetadataProvider) DisplayName() string       { return "Private Provider" }
func (rootMetadataProvider) Models() []types.ModelInfo { return nil }
func (rootMetadataProvider) Validate() error           { return nil }
func (rootMetadataProvider) StreamMessage(context.Context, *types.MessagesRequest, *types.StreamOptions) (<-chan types.SSEEvent, error) {
	return nil, nil
}

func TestRootReportsBuildMetadata(t *testing.T) {
	srv := &Server{activeProvider: rootMetadataProvider{}, activeModel: "private-model", port: 4000}
	recorder := httptest.NewRecorder()
	srv.handleRoot(recorder, httptest.NewRequest("GET", "/", nil))
	if recorder.Code != 200 {
		t.Fatalf("root status = %d, want 200", recorder.Code)
	}
	var response struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Commit  string `json:"commit"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode root response: %v", err)
	}
	if response.Name != "corelaycode" || response.Version != buildinfo.Version || response.Commit != buildinfo.Commit {
		t.Fatalf("root identity/version/commit = %q/%q/%q, want corelaycode/%q/%q", response.Name, response.Version, response.Commit, buildinfo.Version, buildinfo.Commit)
	}
}
