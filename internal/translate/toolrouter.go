package translate

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	"github.com/Dannykkh/corelay-code/internal/types"
)

// ToolCategory is a bounded intent class used to reduce the tool schema shown
// to models that are measurably harmed by a large catalog. It is deliberately
// independent from provider wire formats so the same decision can be reused by
// the agent, proxy, Chronos, and sub-agent paths.
type ToolCategory string

const (
	ToolCategoryRead    ToolCategory = "read"
	ToolCategoryWrite   ToolCategory = "write"
	ToolCategorySearch  ToolCategory = "search"
	ToolCategoryRun     ToolCategory = "run"
	ToolCategoryPlan    ToolCategory = "plan"
	ToolCategoryWeb     ToolCategory = "web"
	ToolCategoryHost    ToolCategory = "host"
	ToolCategoryRespond ToolCategory = "respond"
)

var toolCategoryPriority = []ToolCategory{
	ToolCategoryWrite,
	ToolCategoryRun,
	ToolCategorySearch,
	ToolCategoryPlan,
	ToolCategoryRead,
	ToolCategoryWeb,
	ToolCategoryHost,
	ToolCategoryRespond,
}

// ToolCategoryScore is safe routing telemetry. It contains no prompt text.
type ToolCategoryScore struct {
	Category ToolCategory
	Score    float64
}

// ToolRouteDecision is a deterministic routing result. Fallback means no
// positive intent signal was found; callers should normally retain the full
// catalog in that case instead of guessing.
type ToolRouteDecision struct {
	Category   ToolCategory
	Confidence float64
	Fallback   bool
	Scores     []ToolCategoryScore
}

type weightedToolSignal struct {
	pattern *regexp.Regexp
	weight  float64
}

func signal(pattern string, weight float64) weightedToolSignal {
	return weightedToolSignal{pattern: regexp.MustCompile(`(?i)` + pattern), weight: weight}
}

// Signals intentionally cover both English and Korean requests. They are not
// a semantic model: low-margin and no-signal decisions are explicitly marked
// so the caller can fall back to the full catalog.
var toolCategorySignals = map[ToolCategory][]weightedToolSignal{
	ToolCategoryRead: {
		signal(`\b(read|show|cat|display|view|inspect|review|audit|analy[sz]e|examine)\b|읽어|보여|확인|검토|분석|감사`, 3),
		signal(`\b(file|contents?|source|code)\b|파일|내용|소스|코드`, 1),
		signal(`\b(fix|change|update|modify|create|write|delete)\b|수정|변경|추가|삭제|구현`, -2),
	},
	ToolCategoryWrite: {
		signal(`\b(fix|change|update|modify|edit|refactor|rename|replace|patch|implement)\b|고쳐|수정|변경|편집|리팩터|이름\s*변경|교체|패치|구현`, 3),
		signal(`\b(add|insert|append|create|write|make|remove|delete)\b|추가|삽입|생성|작성|만들|제거|삭제`, 2.5),
		signal(`\b(bug|error|issue|broken|failing|crash)\b|버그|오류|에러|문제|실패|깨졌`, 2),
		signal(`\b(explain|what|why|tell me)\b|설명|무엇|왜`, -1.5),
	},
	ToolCategorySearch: {
		signal(`\b(find|search|grep|locate|where\s+(is|are)|references?|occurrences?)\b|찾아|검색|어디|참조|사용처|호출처`, 3),
		signal(`\b(regex|pattern|all files|codebase|project-wide)\b|정규식|패턴|모든\s*파일|전체\s*코드`, 2),
		signal(`\b(fix|change|create|write)\b|수정|변경|생성|작성`, -1.5),
	},
	ToolCategoryRun: {
		signal(`\b(run|execute|start|launch|invoke)\b|실행|돌려|시작`, 3),
		signal(`\b(test|tests|spec|pytest|jest|vitest|build|compile|lint|format)\b|테스트|빌드|컴파일|린트|포맷`, 3),
		signal(`\b(npm|pnpm|yarn|pip|cargo|go test|docker|kubernetes|terraform|git)\b`, 2.5),
		signal(`\b(explain|what|why)\b|설명|무엇|왜`, -1.5),
	},
	ToolCategoryPlan: {
		signal(`\b(plan|design|architect|decompose|roadmap|step by step)\b|계획|설계|아키텍처|분해|로드맵|단계별`, 3),
		signal(`\b(full|complete|entire|whole|end.to.end|multiple files|system|feature|service|application)\b|전체|완전|전부|여러\s*파일|시스템|기능|서비스|프로그램`, 1.5),
		signal(`\b(show|read|display|cat)\b|보여|읽어`, -2),
	},
	ToolCategoryWeb: {
		signal(`\b(search the web|google|online|internet|look up)\b|웹\s*검색|인터넷|온라인|검색해`, 3),
		signal(`\b(latest|current version|newest|recent|documentation|api reference|website|https?://)\b|최신|현재\s*버전|문서|공식\s*문서|웹사이트|링크`, 2.5),
	},
	ToolCategoryHost: {
		signal(`\b(screenshot|screen|mouse|click|clipboard|window|open app|desktop)\b|스크린샷|화면|마우스|클릭|클립보드|창|앱\s*열`, 3),
	},
	ToolCategoryRespond: {
		signal(`\b(explain|what is|what are|what does|how does|tell me|describe|compare|opinion)\b|설명해|무엇|뭐야|어떻게|비교|의견`, 3),
		signal(`\b(thanks|thank you|okay|ok|yes|no|got it|hello|hi)\b|고마워|감사|알겠|응|아니|안녕`, 3),
		signal(`\b(failing|broken|error|bug|review|check|read|file|code)\b|실패|오류|버그|검토|확인|파일|코드`, -2),
	},
}

var shortAcknowledgementPattern = regexp.MustCompile(`(?i)^(thanks|thank you|ok|okay|yes|no|got it|hello|hi|고마워|감사해|감사합니다|알겠어|알겠습니다|응|아니|안녕)[.!?\s]*$`)

// ClassifyToolIntent deterministically scores a request. It performs no model,
// network, filesystem, or environment access and therefore has stable output
// for the same input.
func ClassifyToolIntent(message string) ToolRouteDecision {
	trimmed := strings.TrimSpace(message)
	if trimmed == "" {
		return fallbackToolRoute()
	}

	if len([]rune(trimmed)) <= 12 && shortAcknowledgementPattern.MatchString(trimmed) {
		return ToolRouteDecision{
			Category:   ToolCategoryRespond,
			Confidence: 1,
			Scores:     []ToolCategoryScore{{Category: ToolCategoryRespond, Score: 1}},
		}
	}

	scores := make([]ToolCategoryScore, 0, len(toolCategoryPriority))
	for _, category := range toolCategoryPriority {
		score := 0.0
		for _, candidate := range toolCategorySignals[category] {
			if candidate.pattern.MatchString(trimmed) {
				score += candidate.weight
			}
		}
		if category == ToolCategoryPlan && len([]rune(trimmed)) > 200 {
			score += 2
		}
		scores = append(scores, ToolCategoryScore{Category: category, Score: score})
	}

	sort.SliceStable(scores, func(i, j int) bool {
		return scores[i].Score > scores[j].Score
	})
	if len(scores) == 0 || scores[0].Score <= 0 {
		decision := fallbackToolRoute()
		decision.Scores = scores
		return decision
	}

	top := scores[0].Score
	second := 0.0
	if len(scores) > 1 && scores[1].Score > 0 {
		second = scores[1].Score
	}
	confidence := (top - second) / maxFloat(top, 3)
	if confidence < 0 {
		confidence = 0
	}
	if confidence > 1 {
		confidence = 1
	}
	return ToolRouteDecision{
		Category:   scores[0].Category,
		Confidence: confidence,
		Scores:     scores,
	}
}

func fallbackToolRoute() ToolRouteDecision {
	return ToolRouteDecision{Category: ToolCategoryRead, Fallback: true}
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

var toolsByCategory = map[ToolCategory]map[string]struct{}{
	ToolCategoryRead: toolNameSet(
		"Read", "Glob", "LS", "Grep", "RepoMap", "NotebookRead", "PDFRead", "ImageRead", "GitDiff",
	),
	ToolCategoryWrite: toolNameSet(
		"Read", "Glob", "Grep", "RepoMap", "Write", "Edit", "Lint", "Test", "Diff", "GitDiff", "Bash",
	),
	ToolCategorySearch: toolNameSet(
		"Grep", "Glob", "LS", "Read", "RepoMap", "GitDiff", "TaskList",
	),
	ToolCategoryRun: toolNameSet(
		"Bash", "Test", "Lint", "Git", "GitDiff", "GitCommit", "Read",
	),
	ToolCategoryPlan: toolNameSet(
		"Read", "Glob", "Grep", "LS", "RepoMap", "Write", "Edit", "Bash", "TaskCreate", "TaskUpdate", "TaskList", "GitDiff", "Diff",
	),
	ToolCategoryWeb: toolNameSet(
		"WebSearch", "WebFetch", "WebResearch", "HTTPRequest", "Read",
	),
	ToolCategoryHost: toolNameSet(
		"Screenshot", "MouseClick", "TypeText", "OpenApp", "ListWindows", "Clipboard",
	),
	ToolCategoryRespond: {},
}

func toolNameSet(names ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		set[name] = struct{}{}
	}
	return set
}

// FilterToolsForCategory returns whole, unchanged definitions in their
// original order. Unknown categories retain the full catalog. Unknown dynamic
// tools are included only when their own name/description confidently matches
// the selected category; plan deliberately retains them as the broad fallback.
func FilterToolsForCategory(tools []types.ToolDef, category ToolCategory) []types.ToolDef {
	allowed, known := toolsByCategory[category]
	if !known {
		return append([]types.ToolDef(nil), tools...)
	}
	if category == ToolCategoryRespond {
		return []types.ToolDef{}
	}

	kept := make([]types.ToolDef, 0, len(tools))
	for _, tool := range tools {
		if _, ok := allowed[tool.Name]; ok {
			kept = append(kept, tool)
			continue
		}
		if isKnownRoutedTool(tool.Name) {
			continue
		}
		if category == ToolCategoryPlan {
			kept = append(kept, tool)
			continue
		}
		decision := ClassifyToolIntent(tool.Name + " " + tool.Description)
		if !decision.Fallback && decision.Category == category && decision.Confidence >= 0.25 {
			kept = append(kept, tool)
		}
	}
	return kept
}

func isKnownRoutedTool(name string) bool {
	for _, names := range toolsByCategory {
		if _, ok := names[name]; ok {
			return true
		}
	}
	return false
}

// RouteTools applies deterministic filtering only for a confident decision.
// Low-confidence and no-signal requests retain the full catalog. This makes
// misclassification a context-cost issue instead of a capability loss.
func RouteTools(tools []types.ToolDef, message string, minimumConfidence float64) ([]types.ToolDef, ToolRouteDecision, int) {
	decision := ClassifyToolIntent(message)
	if minimumConfidence < 0 {
		minimumConfidence = 0
	}
	if minimumConfidence > 1 {
		minimumConfidence = 1
	}
	if decision.Fallback || decision.Confidence < minimumConfidence {
		return append([]types.ToolDef(nil), tools...), decision, 0
	}
	kept := FilterToolsForCategory(tools, decision.Category)
	return kept, decision, len(tools) - len(kept)
}

const toolCategorySelectorName = "SelectToolCategory"

// ToolCategorySelectorDef is the compact first-stage schema for an explicit
// two-stage model route. The agent loop must intercept this synthetic tool and
// replace it with FilterToolsForCategory output; it must never reach a normal
// tool executor.
func ToolCategorySelectorDef() types.ToolDef {
	categories := make([]string, 0, len(toolCategoryPriority))
	for _, category := range toolCategoryPriority {
		categories = append(categories, string(category))
	}
	schema, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"category": map[string]any{
				"type":        "string",
				"enum":        categories,
				"description": "The tool category needed for the next action",
			},
		},
		"required":             []string{"category"},
		"additionalProperties": false,
	})
	return types.ToolDef{
		Name:        toolCategorySelectorName,
		Description: "Select one category for the next action: read, write, search, run, plan, web, host, or respond.",
		InputSchema: schema,
	}
}

// ToolCategorySelectorName exposes the reserved synthetic name without
// allowing callers to redefine its schema.
func ToolCategorySelectorName() string { return toolCategorySelectorName }
