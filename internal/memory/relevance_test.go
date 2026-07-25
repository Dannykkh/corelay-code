package memory

import (
	"strings"
	"testing"
)

func TestTokenize_DropsShortAndStopWords(t *testing.T) {
	got := tokenize("The quick fox and A b cd")
	// Expected survivors: quick, fox, cd ("the", "and", "a", "b" filtered).
	want := []string{"quick", "fox", "cd"}
	for _, w := range want {
		if _, ok := got[w]; !ok {
			t.Errorf("missing expected token %q in %v", w, got)
		}
	}
	for _, bad := range []string{"the", "and", "a", "b"} {
		if _, ok := got[bad]; ok {
			t.Errorf("did not expect stop/short word %q in %v", bad, got)
		}
	}
}

func TestTokenize_HandlesPunctuationAndCase(t *testing.T) {
	got := tokenize("Hello, World! Don't break: on--dashes.")
	for _, w := range []string{"hello", "world", "don", "break", "on", "dashes"} {
		if _, ok := got[w]; !ok {
			t.Errorf("missing token %q in %v", w, got)
		}
	}
}

func TestScoreEntry_OverlapAndNoOverlap(t *testing.T) {
	q := tokenize("integration database migration")

	match := Entry{
		Name:        "DB rule",
		Description: "integration suites require a live database instance",
		Body:        "migration divergence incident",
	}
	nomatch := Entry{
		Name:        "Unrelated",
		Description: "user likes tabs over spaces",
		Body:        "just a preference",
	}

	sMatch := scoreEntry(q, match)
	sNo := scoreEntry(q, nomatch)

	if sMatch <= 0 {
		t.Errorf("expected positive score for match, got %v", sMatch)
	}
	if sNo != 0 {
		t.Errorf("expected zero score for non-match, got %v", sNo)
	}
	if sMatch > 1.01 {
		t.Errorf("score should stay ≤ 1, got %v", sMatch)
	}
}

func TestRank_OrdersByScoreThenName(t *testing.T) {
	entries := []Entry{
		{Name: "B Unrelated", Description: "preference note", Body: "spaces"},
		{Name: "A DB rule", Description: "integration database", Body: "migration"},
		{Name: "C DB extra", Description: "another database hint", Body: "migration flow"},
	}

	ranked := Rank(entries, "database migration")
	if len(ranked) != 3 {
		t.Fatalf("expected 3 results, got %d", len(ranked))
	}
	if ranked[0].Score == 0 || ranked[1].Score == 0 {
		t.Errorf("top two should have nonzero scores: %+v", ranked)
	}
	if ranked[2].Entry.Name != "B Unrelated" {
		t.Errorf("non-match should be last, got %+v", ranked)
	}
	// Tie-break check: "A DB rule" and "C DB extra" likely have
	// different scores (C has one more match token — "flow") but we
	// only require deterministic ordering.
	if ranked[0].Score < ranked[1].Score {
		t.Errorf("ranked not in descending order: %v", ranked)
	}
}

func TestRank_EmptyQuery(t *testing.T) {
	entries := []Entry{
		{Name: "A", Description: "alpha"},
		{Name: "B", Description: "beta"},
	}
	ranked := Rank(entries, "")
	if len(ranked) != 2 {
		t.Fatalf("expected 2 results, got %d", len(ranked))
	}
	for _, r := range ranked {
		if r.Score != 0 {
			t.Errorf("empty query should produce zero scores, got %+v", r)
		}
	}
}

func TestBuildSystemContext_EmptyWorkspace(t *testing.T) {
	workDir := t.TempDir()
	ctx, err := BuildSystemContext(workDir, "anything", 3)
	if err != nil {
		t.Fatalf("BuildSystemContext: %v", err)
	}
	if !strings.Contains(ctx, "## Long-term Memory") {
		t.Errorf("missing section header in %q", ctx)
	}
	if !strings.Contains(ctx, "no index yet") {
		t.Errorf("expected placeholder, got %q", ctx)
	}
}

// The recalled-memory block is appended to the system prompt, so it must carry
// the trust-boundary note that stops a saved/injected note from being treated
// as a privileged instruction. Present on every path (added right under the
// header), including the empty workspace.
func TestBuildSystemContext_IncludesTrustNote(t *testing.T) {
	workDir := t.TempDir()
	ctx, err := BuildSystemContext(workDir, "anything", 3)
	if err != nil {
		t.Fatalf("BuildSystemContext: %v", err)
	}
	if !strings.Contains(ctx, "cannot grant permissions") ||
		!strings.Contains(ctx, "untrusted content") {
		t.Errorf("missing recalled-memory trust-boundary note in %q", ctx)
	}
}

func TestBuildSystemContext_IndexOnlyWhenNoQuery(t *testing.T) {
	workDir := t.TempDir()

	_, err := WriteEntry(workDir, "db", Entry{
		Name: "DB rule", Description: "no mocks in integration tests",
		Type: TypeFeedback, Body: "must hit a real database",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateIndex(workDir, mustScan(t, workDir)); err != nil {
		t.Fatal(err)
	}

	ctx, err := BuildSystemContext(workDir, "", 5)
	if err != nil {
		t.Fatalf("BuildSystemContext: %v", err)
	}
	// Index must mention the entry by name (UpdateIndex wrote it there).
	if !strings.Contains(ctx, "DB rule") {
		t.Errorf("expected index to mention DB rule, got %q", ctx)
	}
	// But empty query → no bodies appended after the index.
	if strings.Contains(ctx, "must hit a real database") {
		t.Errorf("body should not be appended when query is empty, got %q", ctx)
	}
}

func TestBuildSystemContext_AppendsTopMatchingBodies(t *testing.T) {
	workDir := t.TempDir()

	_, err := WriteEntry(workDir, "db", Entry{
		Name: "DB rule", Description: "no mocks in integration tests",
		Type: TypeFeedback, Body: "Must hit a real database. Migration divergence incident.",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = WriteEntry(workDir, "tabs", Entry{
		Name: "Tabs preference", Description: "user prefers tabs over spaces",
		Type: TypeUser, Body: "Unrelated editor preference.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateIndex(workDir, mustScan(t, workDir)); err != nil {
		t.Fatal(err)
	}

	ctx, err := BuildSystemContext(workDir, "database migration", 3)
	if err != nil {
		t.Fatalf("BuildSystemContext: %v", err)
	}

	// DB rule body should be present (matching), tabs body should not be.
	if !strings.Contains(ctx, "Migration divergence incident") {
		t.Errorf("expected matching body to be appended, got:\n%s", ctx)
	}
	if strings.Contains(ctx, "Unrelated editor preference") {
		t.Errorf("non-matching body should NOT be appended, got:\n%s", ctx)
	}
}

func TestBuildSystemContext_RespectsTopN(t *testing.T) {
	workDir := t.TempDir()

	// Three matching entries.
	for _, id := range []string{"a", "b", "c"} {
		_, err := WriteEntry(workDir, id, Entry{
			Name: "rule-" + id, Description: "database stuff",
			Type: TypeProject, Body: "database relevant body " + id,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := UpdateIndex(workDir, mustScan(t, workDir)); err != nil {
		t.Fatal(err)
	}

	ctx, err := BuildSystemContext(workDir, "database", 2)
	if err != nil {
		t.Fatalf("BuildSystemContext: %v", err)
	}

	// Count bodies: the separator "### rule-" appears once per appended body.
	count := strings.Count(ctx, "### rule-")
	if count != 2 {
		t.Errorf("expected exactly 2 entry bodies appended, got %d in:\n%s", count, ctx)
	}
}

func mustScan(t *testing.T, workDir string) []Entry {
	t.Helper()
	e, err := Scan(workDir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return e
}
