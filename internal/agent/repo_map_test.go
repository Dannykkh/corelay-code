package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepoMapIsDeterministicBoundedAndDoesNotExposeBodiesOrValues(t *testing.T) {
	workspace := t.TempDir()
	writeRepoMapFixture(t, filepath.Join(workspace, "main.go"), `package sample

const credential = "VALUE-MUST-NOT-LEAK"

type Runner struct{}

func (Runner) Execute(input string) string {
	return "VALUE-MUST-NOT-LEAK"
}
`)
	writeRepoMapFixture(t, filepath.Join(workspace, "src", "app.ts"), `export const handler = (token = "VALUE-MUST-NOT-LEAK") => token;
export function run(input: string) { return "VALUE-MUST-NOT-LEAK"; }
`)
	writeRepoMapFixture(t, filepath.Join(workspace, ".hidden", "leak.go"), `package hidden
func Leak() string { return "VALUE-MUST-NOT-LEAK" }
`)
	writeRepoMapFixture(t, filepath.Join(workspace, "node_modules", "leak.ts"), `export function leak() { return "VALUE-MUST-NOT-LEAK"; }
`)
	writeRepoMapFixture(t, filepath.Join(workspace, "DIST", "leak.ts"), `export function leak() { return "VALUE-MUST-NOT-LEAK"; }
`)
	writeRepoMapFixture(t, filepath.Join(workspace, "README.md"), "VALUE-MUST-NOT-LEAK")
	writeRepoMapFixture(t, filepath.Join(workspace, "huge.go"), "package huge\n"+strings.Repeat("x", maximumRepoMapFileBytes))

	input := json.RawMessage(`{"path":".","max_files":10}`)
	first, isError := executeRepoMap(input, workspace, ToolExecutionOptions{Context: context.Background()})
	second, secondError := executeRepoMap(input, workspace, ToolExecutionOptions{Context: context.Background()})
	if isError || secondError || first != second {
		t.Fatalf("repo map not deterministic: firstError=%v secondError=%v\nfirst=%s\nsecond=%s", isError, secondError, first, second)
	}
	for _, expected := range []string{
		"main.go:", "type Runner struct", "func (Runner) Execute(input string) string",
		"src/app.ts:", "export const handler = (...) =>", "export function run",
	} {
		if !strings.Contains(first, expected) {
			t.Fatalf("repo map omitted %q:\n%s", expected, first)
		}
	}
	for _, forbidden := range []string{"VALUE-MUST-NOT-LEAK", ".hidden", "node_modules", "DIST", "README.md", "huge.go", "return"} {
		if strings.Contains(first, forbidden) {
			t.Fatalf("repo map exposed forbidden content %q:\n%s", forbidden, first)
		}
	}
	if len(first) > maximumRepoMapBytes {
		t.Fatalf("repo map length=%d", len(first))
	}
}

func TestRepoMapHonorsFileAndSignatureControls(t *testing.T) {
	workspace := t.TempDir()
	for _, name := range []string{"c.go", "a.go", "b.go"} {
		writeRepoMapFixture(t, filepath.Join(workspace, name), "package sample\nfunc Visible() {}\n")
	}
	include := false
	entries, total, truncated, err := buildRepoMap(context.Background(), workspace, workspace, include, 2)
	if err != nil || truncated || total != 3 || len(entries) != 2 {
		t.Fatalf("entries=%#v total=%d truncated=%v err=%v", entries, total, truncated, err)
	}
	if entries[0].path != "a.go" || entries[1].path != "b.go" || len(entries[0].signatures) != 0 {
		t.Fatalf("unexpected bounded entries: %#v", entries)
	}
	rendered := renderRepoMap(entries, total, truncated)
	if !strings.Contains(rendered, "1 additional source files omitted") {
		t.Fatalf("missing truncation notice: %s", rendered)
	}
}

func TestRepoMapUsesCommonReadOnlySafetyContracts(t *testing.T) {
	definitionFound := false
	for _, definition := range ExtendedToolDefs() {
		if definition.Name == "RepoMap" {
			definitionFound = true
			break
		}
	}
	danger, _ := ClassifyDanger("RepoMap", json.RawMessage(`{}`))
	if !definitionFound || danger != DangerSafe || !IsConcurrencySafe("RepoMap", map[string]any{}) || !planModeTools["RepoMap"] {
		t.Fatal("RepoMap did not inherit the read-only catalog contract")
	}
	workspace := t.TempDir()
	if _, isError := executeRepoMap(json.RawMessage(`{"path":"../outside-repo-map"}`), workspace, ToolExecutionOptions{}); !isError {
		t.Fatal("RepoMap accepted a workspace escape")
	}
}

func TestRepoMapCancellationFailsClosed(t *testing.T) {
	workspace := t.TempDir()
	writeRepoMapFixture(t, filepath.Join(workspace, "a.go"), "package sample\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, isError := executeRepoMap(json.RawMessage(`{}`), workspace, ToolExecutionOptions{Context: ctx}); !isError {
		t.Fatal("RepoMap treated cancellation as success")
	}
}

func writeRepoMapFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
