package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readFileAPIResponse(t *testing.T, s *Server, workspace, path string) (fileReadResponse, int) {
	t.Helper()
	query := url.Values{"workDir": []string{workspace}, "path": []string{path}}
	rec := httptest.NewRecorder()
	s.handleReadFile(rec, httptest.NewRequest(http.MethodGet, "/api/file?"+query.Encode(), nil))
	var response fileReadResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode file response for %q: %v; body=%s", path, err, rec.Body.String())
		}
	}
	return response, rec.Code
}

func TestReadFileUsesBoundedDirectResponseAndCanonicalPaths(t *testing.T) {
	workspace := t.TempDir()
	configureProjectWorkspaces(t, workspace)
	if err := os.MkdirAll(filepath.Join(workspace, "one", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, "two"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "one", "shared.txt"), []byte("one content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "two", "shared.txt"), []byte("two content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "one", "deep", "nested.txt"), []byte("nested"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "binary.dat"), []byte{'h', 0, 'i'}, 0o600); err != nil {
		t.Fatal(err)
	}
	large := strings.Repeat("0123456789\n", int(maxFileReadBytes/11)+2)
	if err := os.WriteFile(filepath.Join(workspace, "large.txt"), []byte(large), 0o600); err != nil {
		t.Fatal(err)
	}

	provider := &agentLoopFakeProvider{}
	s := New(provider, "test-model", 0)
	s.SetWorkDir(workspace)

	one, status := readFileAPIResponse(t, s, workspace, "one/../one/shared.txt")
	if status != http.StatusOK || one.Path != "one/shared.txt" || one.Type != "text" || one.Content != "one content\n" {
		t.Fatalf("canonical first file status=%d response=%+v", status, one)
	}
	two, status := readFileAPIResponse(t, s, workspace, "two/shared.txt")
	if status != http.StatusOK || two.Path != "two/shared.txt" || two.Content != "two content\n" {
		t.Fatalf("same-basename file status=%d response=%+v", status, two)
	}

	directory, status := readFileAPIResponse(t, s, workspace, "one")
	var foundDeep bool
	for _, entry := range directory.Entries {
		if entry.Path == "one/deep" && entry.IsDir {
			foundDeep = true
		}
	}
	if status != http.StatusOK || directory.Path != "one" || directory.Type != "directory" || !foundDeep {
		t.Fatalf("directory response status=%d response=%+v", status, directory)
	}

	binary, status := readFileAPIResponse(t, s, workspace, "binary.dat")
	if status != http.StatusOK || binary.Type != "binary" || binary.Content != "[Binary file]" {
		t.Fatalf("binary response status=%d response=%+v", status, binary)
	}

	tooLarge, status := readFileAPIResponse(t, s, workspace, "large.txt")
	if status != http.StatusOK || tooLarge.Type != "too_large" || !tooLarge.Truncated || tooLarge.Size <= maxFileReadBytes || !strings.Contains(tooLarge.Content, "truncated at 100000 bytes") {
		t.Fatalf("large response status=%d response type=%s size=%d truncated=%v content bytes=%d", status, tooLarge.Type, tooLarge.Size, tooLarge.Truncated, len(tooLarge.Content))
	}
	if len(tooLarge.Content) > int(maxFileReadBytes)+64 {
		t.Fatalf("large response was not bounded: content bytes=%d", len(tooLarge.Content))
	}

	if _, status := readFileAPIResponse(t, s, workspace, "missing.txt"); status != http.StatusNotFound {
		t.Fatalf("missing file status=%d, want %d", status, http.StatusNotFound)
	}
	if provider.calls != 0 {
		t.Fatalf("direct file reads invoked the model %d times", provider.calls)
	}
}
