package translate

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestClassifyToolIntentDeterministicEnglishAndKorean(t *testing.T) {
	tests := []struct {
		text string
		want ToolCategory
	}{
		{"fix the parser bug in toolrecover.go", ToolCategoryWrite},
		{"run the full test suite", ToolCategoryRun},
		{"find every caller of RunLoop", ToolCategorySearch},
		{"design the complete session lifecycle across multiple files", ToolCategoryPlan},
		{"read and review internal/agent/loop.go", ToolCategoryRead},
		{"search the web for the latest official API documentation", ToolCategoryWeb},
		{"take a screenshot of the current window", ToolCategoryHost},
		{"고마워", ToolCategoryRespond},
		{"이 버그를 수정하고 구현해줘", ToolCategoryWrite},
		{"전체 테스트를 실행해줘", ToolCategoryRun},
		{"RunLoop 사용처를 모두 찾아줘", ToolCategorySearch},
		{"여러 파일에 걸친 전체 기능을 단계별로 설계해줘", ToolCategoryPlan},
		{"이 파일을 읽고 검토해줘", ToolCategoryRead},
		{"최신 공식 문서를 웹 검색해줘", ToolCategoryWeb},
	}
	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			first := ClassifyToolIntent(tt.text)
			second := ClassifyToolIntent(tt.text)
			if !reflect.DeepEqual(first, second) {
				t.Fatalf("classification is not deterministic:\n%#v\n%#v", first, second)
			}
			if first.Category != tt.want || first.Fallback {
				t.Fatalf("category = %q fallback=%v confidence=%.2f, want %q", first.Category, first.Fallback, first.Confidence, tt.want)
			}
		})
	}
}

func TestClassifyToolIntentMarksUnknownAsFallback(t *testing.T) {
	decision := ClassifyToolIntent("xyzzy plugh")
	if decision.Category != ToolCategoryRead || !decision.Fallback || decision.Confidence != 0 {
		t.Fatalf("unexpected fallback: %#v", decision)
	}
}

func TestClassifyToolIntentDoesNotTreatEveryShortMessageAsAcknowledgement(t *testing.T) {
	decision := ClassifyToolIntent("버그")
	if decision.Category != ToolCategoryWrite || decision.Fallback {
		t.Fatalf("short action signal was lost: %#v", decision)
	}
}

func TestRouteToolsKeepsFullCatalogForAmbiguousOrFallback(t *testing.T) {
	tools := []types.ToolDef{{Name: "Read"}, {Name: "Write"}, {Name: "Bash"}}
	kept, decision, dropped := RouteTools(tools, "xyzzy plugh", 0.5)
	if !decision.Fallback || dropped != 0 || !reflect.DeepEqual(kept, tools) {
		t.Fatalf("fallback changed catalog: decision=%#v kept=%#v dropped=%d", decision, kept, dropped)
	}
	kept[0].Name = "changed"
	if tools[0].Name != "Read" {
		t.Fatal("returned slice aliases input")
	}
}

func TestFilterToolsForCategoryPreservesOrderAndDependencies(t *testing.T) {
	tools := []types.ToolDef{
		{Name: "WebSearch"},
		{Name: "Write"},
		{Name: "Read"},
		{Name: "Edit"},
		{Name: "Bash"},
		{Name: "Grep"},
		{Name: "Screenshot"},
	}
	got := FilterToolsForCategory(tools, ToolCategoryWrite)
	want := []string{"Write", "Read", "Edit", "Bash", "Grep"}
	if !reflect.DeepEqual(toolNamesInOrder(got), want) {
		t.Fatalf("names = %v, want %v", toolNamesInOrder(got), want)
	}
	if got[0].Name == "" || tools[0].Name != "WebSearch" {
		t.Fatal("filter mutated input")
	}
}

func TestFilterToolsForCategoryHandlesDynamicToolsConservatively(t *testing.T) {
	tools := []types.ToolDef{
		{Name: "Read"},
		{Name: "mcp__docs__lookup", Description: "search latest online documentation"},
		{Name: "mcp__opaque__thing", Description: "perform a proprietary operation"},
	}
	web := FilterToolsForCategory(tools, ToolCategoryWeb)
	if got := toolNamesInOrder(web); !reflect.DeepEqual(got, []string{"Read", "mcp__docs__lookup"}) {
		t.Fatalf("web tools = %v", got)
	}
	plan := FilterToolsForCategory(tools, ToolCategoryPlan)
	if len(plan) != len(tools) {
		t.Fatalf("plan should retain dynamic tools, got %v", toolNamesInOrder(plan))
	}
}

func TestRespondCategoryUsesNoToolsOnlyWhenConfident(t *testing.T) {
	tools := []types.ToolDef{{Name: "Read"}, {Name: "Write"}}
	kept, decision, dropped := RouteTools(tools, "고마워", 0.8)
	if decision.Category != ToolCategoryRespond || len(kept) != 0 || dropped != len(tools) {
		t.Fatalf("unexpected respond route: decision=%#v kept=%v dropped=%d", decision, kept, dropped)
	}
}

func TestToolCategorySelectorDefIsBoundedAndStrict(t *testing.T) {
	definition := ToolCategorySelectorDef()
	if definition.Name != ToolCategorySelectorName() {
		t.Fatalf("selector name = %q", definition.Name)
	}
	text := string(definition.InputSchema)
	for _, required := range []string{`"additionalProperties":false`, `"category"`, `"respond"`, `"host"`} {
		if !strings.Contains(text, required) {
			t.Fatalf("selector schema %s missing %s", text, required)
		}
	}
}

func toolNamesInOrder(defs []types.ToolDef) []string {
	names := make([]string, 0, len(defs))
	for _, definition := range defs {
		names = append(names, definition.Name)
	}
	return names
}
