package agent

import (
	"strings"
	"testing"
)

const hintSource = `def build_report(rows, cfg):
    title = get_setting(cfg, "title")
    separator = get_setting(cfg, "separator")

    if get_setting(cfg, "show_footer"):
        avg_columns = sum(len(row) for row in rows) / len(rows)
        lines.append(f"rows: {len(rows)}")

    return "\n".join(lines)
`

func TestClosestLinesHintFindsMisquotedLine(t *testing.T) {
	// What a weaker model actually does: quotes the line with the wrong
	// indentation and a dropped call. Containment finds nothing here, which is
	// how this hint used to come back empty in a live gemma4:12b run.
	got := closestLinesHint(hintSource, "avg_columns = sum(len(row) for row in rows)/len(rows)")

	if got == "" {
		t.Fatal("hint was empty for a misquoted line")
	}
	if !strings.Contains(got, "avg_columns") {
		t.Fatalf("hint did not point at the real line:\n%s", got)
	}
	if !strings.Contains(got, "line 6") {
		t.Fatalf("hint did not carry a line number:\n%s", got)
	}
}

func TestClosestLinesHintPrefersVerbatimMatch(t *testing.T) {
	got := closestLinesHint(hintSource, `    title = get_setting(cfg, "title")`)

	if !strings.Contains(got, "Similar lines") {
		t.Fatalf("expected the verbatim-containment wording, got:\n%s", got)
	}
	if !strings.Contains(got, "line 2") {
		t.Fatalf("expected line 2, got:\n%s", got)
	}
}

func TestClosestLinesHintStaysQuietWhenNothingResembles(t *testing.T) {
	got := closestLinesHint(hintSource, "connect_to_database(timeout_seconds)")
	if got != "" {
		t.Fatalf("expected no hint for unrelated text, got:\n%s", got)
	}
}

func TestClosestLinesHintIgnoresSingleCommonToken(t *testing.T) {
	// One shared token is not evidence: "lines" appears all over the file.
	got := closestLinesHint(hintSource, "lines = compute_everything(a, b, c)")
	if strings.Count(got, "line ") > 3 {
		t.Fatalf("hint nominated too many candidates:\n%s", got)
	}
}

func TestIdentifierTokensDropsShortNoise(t *testing.T) {
	got := identifierTokens(`  if x = get_setting(cfg, "title"):`)
	for _, tok := range got {
		if len(tok) < 3 {
			t.Fatalf("token %q shorter than 3 chars in %v", tok, got)
		}
	}
	var hasGetSetting bool
	for _, tok := range got {
		if tok == "get_setting" {
			hasGetSetting = true
		}
	}
	if !hasGetSetting {
		t.Fatalf("expected get_setting in %v", got)
	}
}
