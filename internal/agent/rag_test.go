package agent

import "testing"

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
