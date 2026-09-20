package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func hasKeyword(keywords []string, want string) bool {
	for _, k := range keywords {
		if k == want {
			return true
		}
	}
	return false
}

func TestExtractKeywordsStripsInlineCodeBackticks(t *testing.T) {
	// Observed with a real prompt: "run `python -m pytest test_calc.py`" left
	// "`python" as a keyword, which never matches a path or symbol on disk.
	got := extractKeywords("calc.py has bugs, run `pytest` and fix them")

	if hasKeyword(got, "`pytest") {
		t.Fatalf("backtick survived trimming: %v", got)
	}
	if !hasKeyword(got, "pytest") {
		t.Fatalf("expected bare %q in %v", "pytest", got)
	}
	if !hasKeyword(got, "calc.py") {
		t.Fatalf("expected %q to survive as a filename in %v", "calc.py", got)
	}
}

func TestExtractKeywordsDropsStopWordsAndDeduplicates(t *testing.T) {
	got := extractKeywords("the report is in the report module")

	if hasKeyword(got, "the") || hasKeyword(got, "is") || hasKeyword(got, "in") {
		t.Fatalf("stop word survived: %v", got)
	}

	count := 0
	for _, k := range got {
		if k == "report" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected %q exactly once, got %d in %v", "report", count, got)
	}
}

func TestRAGSearchExcludesIndexedSkillBodiesButKeepsProjectContext(t *testing.T) {
	workDir := t.TempDir()
	skillRoot := filepath.Join(workDir, ".claude", "skills")
	skillPath := filepath.Join(skillRoot, "browser-crawler", "SKILL.md")
	docsPath := filepath.Join(workDir, "docs", "crawler-architecture.md")
	if err := os.MkdirAll(filepath.Dir(skillPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(docsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skillPath, []byte("browser crawler release parsing SELECTED_SKILL_BODY"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(docsPath, []byte("browser crawler release parsing PROJECT_DOC_CONTEXT"), 0o600); err != nil {
		t.Fatal(err)
	}

	results := RAGSearch(workDir, "browser crawler release parsing", 10, skillRoot)
	if len(results) == 0 {
		t.Fatal("excluded skill file also suppressed unrelated project documentation")
	}
	for _, result := range results {
		if strings.Contains(result.File, ".claude/skills/") || strings.Contains(result.Content, "SELECTED_SKILL_BODY") {
			t.Fatalf("RAG returned an indexed skill body: %+v", result)
		}
	}
	foundDocs := false
	for _, result := range results {
		if strings.Contains(result.Content, "PROJECT_DOC_CONTEXT") {
			foundDocs = true
		}
	}
	if !foundDocs {
		t.Fatalf("RAG stopped returning ordinary relevant project context: %+v", results)
	}
}

func TestRAGSearchExcludesNestedSkillReferences(t *testing.T) {
	workDir := t.TempDir()
	skillRoot := filepath.Join(workDir, ".agents", "skills")
	referencePath := filepath.Join(skillRoot, "workflow", "references", "policy.md")
	if err := os.MkdirAll(filepath.Dir(referencePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(referencePath, []byte("hidden policy crawler SECRET_REFERENCE_BODY"), 0o600); err != nil {
		t.Fatal(err)
	}
	results := RAGSearch(workDir, "hidden policy crawler", 10, skillRoot)
	for _, result := range results {
		if strings.Contains(result.Content, "SECRET_REFERENCE_BODY") {
			t.Fatalf("RAG returned a nested skill reference: %+v", result)
		}
	}
}
