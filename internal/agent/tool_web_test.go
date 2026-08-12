package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestExtractDuckDuckGoResults(t *testing.T) {
	page := `
		<html><body>
			<a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fdocs">Example <b>Docs</b></a>
			<a class="result__snippet">Useful &amp; relevant snippet</a>
		</body></html>`

	results := extractDuckDuckGoResults(page, 5)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].URL != "https://example.com/docs" {
		t.Fatalf("unexpected URL: %q", results[0].URL)
	}
	if results[0].Title != "Example Docs" {
		t.Fatalf("unexpected title: %q", results[0].Title)
	}
	if results[0].Snippet != "Useful & relevant snippet" {
		t.Fatalf("unexpected snippet: %q", results[0].Snippet)
	}
}

func TestExecuteWebFetchDirectHTML(t *testing.T) {
	t.Setenv("OLLAMA_API_KEY", "")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`
			<html>
				<head><title>Local Doc</title></head>
				<meta property="article:published_time" content="2026-06-20T10:00:00+09:00">
				<meta property="article:modified_time" content="2026-06-21T11:00:00+09:00">
				<body>
					<nav>ignored navigation</nav>
					<main>
						<p>Alpha release notes explain the searchable local model harness.</p>
						<p>Beta unrelated text.</p>
						<a href="/next">Next</a>
					</main>
				</body>
			</html>`))
	}))
	defer server.Close()

	input, _ := json.Marshal(map[string]any{
		"url":       server.URL,
		"prompt":    "alpha harness",
		"max_chars": 1000,
	})
	out, isErr := executeWebFetch(input, t.TempDir())
	if isErr {
		t.Fatalf("executeWebFetch returned error: %s", out)
	}
	for _, want := range []string{
		"WebFetch provider=direct",
		"Title: Local Doc",
		"Published-At: 2026-06-20",
		"Updated-At: 2026-06-21",
		"Alpha release notes explain the searchable local model harness.",
		server.URL + "/next",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected output to contain %q, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ignored navigation") {
		t.Fatalf("expected nav content to be stripped, got:\n%s", out)
	}
}

func TestExtractHTMLDatesFromJSONLDAndTime(t *testing.T) {
	page := `
		<html>
			<head>
				<script type="application/ld+json">
				{
					"@type": "NewsArticle",
					"datePublished": "2026-06-22T09:30:00+09:00",
					"dateModified": "2026-06-23T12:00:00+09:00"
				}
				</script>
			</head>
			<body><time datetime="2026-06-24">fallback</time></body>
		</html>`

	published, updated := extractHTMLDates(page)
	if published != "2026-06-22" {
		t.Fatalf("published = %q, want 2026-06-22", published)
	}
	if updated != "2026-06-23" {
		t.Fatalf("updated = %q, want 2026-06-23", updated)
	}
}

func TestExtractHTMLDatesFallsBackToTimeElement(t *testing.T) {
	published, updated := extractHTMLDates(`<html><body><time datetime="2026-06-24">June 24, 2026</time></body></html>`)
	if published != "2026-06-24" {
		t.Fatalf("published = %q, want 2026-06-24", published)
	}
	if updated != "" {
		t.Fatalf("updated = %q, want empty", updated)
	}
}

func TestDefaultWebProvidersIgnoreOllamaAPIKey(t *testing.T) {
	t.Setenv("OLLAMA_API_KEY", "test-key")

	if got := webSearchDefaultProvider(); got != "duckduckgo" {
		t.Fatalf("webSearchDefaultProvider() = %q, want duckduckgo", got)
	}
	if got := webFetchDefaultProvider(); got != "direct" {
		t.Fatalf("webFetchDefaultProvider() = %q, want direct", got)
	}
}

func TestExecuteWebFetchFollowsNestedFrames(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/":
			_, _ = w.Write([]byte(`<html><head><title>Outer Shell</title></head><body><iframe id="mainFrame" src="/frame1"></iframe></body></html>`))
		case "/frame1":
			_, _ = w.Write([]byte(`<html><body><iframe name="mainFrame" src="/frame2"></iframe></body></html>`))
		case "/frame2":
			_, _ = w.Write([]byte(`<html><body><main><p>Nested iframe body content for Naver style blogs.</p><a href="/source">Source</a></main></body></html>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	input, _ := json.Marshal(map[string]any{
		"url":       server.URL,
		"max_chars": 2000,
	})
	out, isErr := executeWebFetch(input, t.TempDir())
	if isErr {
		t.Fatalf("executeWebFetch returned error: %s", out)
	}
	for _, want := range []string{
		"Title: Outer Shell",
		"--- Frame: " + server.URL + "/frame1 ---",
		"--- Frame: " + server.URL + "/frame2 ---",
		"Nested iframe body content for Naver style blogs.",
		server.URL + "/source",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected output to contain %q, got:\n%s", want, out)
		}
	}
}

func TestNaverMobilePostURL(t *testing.T) {
	cases := map[string]string{
		"https://blog.naver.com/naverofficial/224313722912":                                           "https://m.blog.naver.com/naverofficial/224313722912",
		"https://m.blog.naver.com/naverofficial/224313722912":                                         "https://m.blog.naver.com/naverofficial/224313722912",
		"https://blog.naver.com/PostView.naver?blogId=naverofficial&logNo=224313722912&redirect=Dlog": "https://m.blog.naver.com/naverofficial/224313722912",
	}
	for raw, want := range cases {
		got, ok := naverMobilePostURL(raw)
		if !ok {
			t.Fatalf("naverMobilePostURL(%q) did not match", raw)
		}
		if got != want {
			t.Fatalf("naverMobilePostURL(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestFuseSearchResultsDeduplicatesProviders(t *testing.T) {
	opts := webSearchOptions{
		Query:      "official docs",
		MaxResults: 5,
		Sort:       "relevance",
		SearchedAt: time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC),
	}
	results := fuseSearchResults(opts, map[string][]webSearchResult{
		"duckduckgo": {
			{Rank: 1, Title: "Official Docs", URL: "https://example.com/docs?utm_source=x", Snippet: "official docs reference"},
		},
		"google": {
			{Rank: 2, Title: "Official Docs", URL: "https://example.com/docs", Snippet: "official docs reference with longer snippet"},
		},
	})

	if len(results) != 1 {
		t.Fatalf("expected deduped single result, got %d", len(results))
	}
	if got := strings.Join(results[0].Providers, ","); got != "duckduckgo,google" {
		t.Fatalf("unexpected providers: %q", got)
	}
	if !strings.Contains(results[0].Snippet, "longer snippet") {
		t.Fatalf("expected longer snippet to win, got %q", results[0].Snippet)
	}
	if results[0].SearchedAt == "" || results[0].Score <= 0 {
		t.Fatalf("expected searched_at and score, got %+v", results[0])
	}
}

func TestFuseSearchResultsSortsLatestFirst(t *testing.T) {
	opts := webSearchOptions{
		Query:      "release notes",
		MaxResults: 5,
		Sort:       "latest",
		SearchedAt: time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC),
	}
	results := fuseSearchResults(opts, map[string][]webSearchResult{
		"duckduckgo": {
			{Rank: 1, Title: "Old Release Notes", URL: "https://example.com/old", Snippet: "release notes", PublishedAt: "2024-01-01"},
			{Rank: 2, Title: "New Release Notes", URL: "https://example.com/new", Snippet: "release notes", PublishedAt: "2026-06-24"},
		},
	})

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].URL != "https://example.com/new" {
		t.Fatalf("expected newest result first, got %+v", results)
	}
}

func TestFuseSearchResultsRecomputesScoresAfterMerge(t *testing.T) {
	opts := webSearchOptions{
		Query:      "release notes",
		MaxResults: 5,
		Sort:       "latest",
		SearchedAt: time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC),
	}
	results := fuseSearchResults(opts, map[string][]webSearchResult{
		"duckduckgo": {
			{Rank: 1, Title: "Release Notes", URL: "https://example.com/release", Snippet: "release notes"},
		},
		"google": {
			{Rank: 1, Title: "Release Notes", URL: "https://example.com/release?utm_source=g", Snippet: "release notes with detailed current changelog", PublishedAt: "2026-06-24"},
		},
	})

	if len(results) != 1 {
		t.Fatalf("expected one merged result, got %d", len(results))
	}
	if results[0].PublishedAt != "2026-06-24" {
		t.Fatalf("expected merged date, got %+v", results[0])
	}
	if results[0].FreshnessScore < 0.9 {
		t.Fatalf("expected freshness to be recomputed after merge, got %+v", results[0])
	}
}

func TestSelectSearchProvidersExplicitList(t *testing.T) {
	providers, label, notes, err := selectSearchProviders(webSearchArgs{
		Providers: []string{"duckduckgo", "yahoo"},
	})
	if err != nil {
		t.Fatalf("selectSearchProviders returned error: %v notes=%v", err, notes)
	}
	if label != "multi" {
		t.Fatalf("expected multi label, got %q", label)
	}
	var names []string
	for _, provider := range providers {
		names = append(names, provider.Name())
	}
	if got := strings.Join(names, ","); got != "duckduckgo,yahoo" {
		t.Fatalf("unexpected provider order: %q", got)
	}
}

func TestSelectSearchProvidersAllSkipsUnconfiguredAPIs(t *testing.T) {
	t.Setenv("GOOGLE_SEARCH_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "")
	t.Setenv("GOOGLE_SEARCH_ENGINE_ID", "")
	t.Setenv("GOOGLE_CSE_ID", "")
	t.Setenv("NAVER_CLIENT_ID", "")
	t.Setenv("NAVER_CLIENT_SECRET", "")
	t.Setenv("BING_SEARCH_API_KEY", "")
	providers, _, notes, err := selectSearchProviders(webSearchArgs{Provider: "all"})
	if err != nil {
		t.Fatalf("selectSearchProviders(all) returned error: %v notes=%v", err, notes)
	}
	var names []string
	for _, provider := range providers {
		names = append(names, provider.Name())
	}
	if got := strings.Join(names, ","); got != "duckduckgo,yahoo" {
		t.Fatalf("unexpected provider order for all: %q", got)
	}
}

func TestRunSearchProvidersKeepsFastResultsWhenAnotherProviderTimesOut(t *testing.T) {
	opts := webSearchOptions{
		Query:      "fast result",
		MaxResults: 5,
		Sort:       "relevance",
		SearchedAt: time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC),
	}
	providers := []webSearchProvider{
		blockingSearchProvider{name: "slow"},
		staticSearchProvider{
			name: "fast",
			results: []webSearchResult{{
				Rank:    1,
				Title:   "Fast Result",
				URL:     "https://example.com/fast",
				Snippet: "fast result",
			}},
		},
	}

	start := time.Now()
	byProvider, notes := runSearchProvidersWithTimeout(context.Background(), providers, opts, 20*time.Millisecond)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("parallel search took too long: %v", elapsed)
	}
	if got := len(byProvider["fast"]); got != 1 {
		t.Fatalf("fast provider result missing: got %d results, byProvider=%+v notes=%v", got, byProvider, notes)
	}
	if _, ok := byProvider["slow"]; ok {
		t.Fatalf("slow provider should have timed out, byProvider=%+v", byProvider)
	}
	if len(notes) == 0 || !strings.Contains(strings.Join(notes, ";"), "slow failed") {
		t.Fatalf("expected slow failure note, got %v", notes)
	}
}

func TestResearchFetchProviderSeparatesSearchAndFetch(t *testing.T) {
	if got := researchFetchProvider(webResearchArgs{Provider: "google"}); got != "direct" {
		t.Fatalf("google search should fetch direct, got %q", got)
	}
	if got := researchFetchProvider(webResearchArgs{Provider: "multi"}); got != "direct" {
		t.Fatalf("multi search should fetch direct, got %q", got)
	}
	if got := researchFetchProvider(webResearchArgs{Provider: "ollama"}); got != "ollama" {
		t.Fatalf("explicit ollama should fetch with ollama, got %q", got)
	}
	if got := researchFetchProvider(webResearchArgs{Providers: []string{"ollama", "duckduckgo"}}); got != "direct" {
		t.Fatalf("mixed search providers should fetch direct, got %q", got)
	}
}

type staticSearchProvider struct {
	name    string
	results []webSearchResult
}

func (p staticSearchProvider) Name() string { return p.name }
func (p staticSearchProvider) Configured() bool {
	return true
}
func (p staticSearchProvider) Search(context.Context, webSearchOptions) ([]webSearchResult, error) {
	return p.results, nil
}

type blockingSearchProvider struct {
	name string
}

func (p blockingSearchProvider) Name() string { return p.name }
func (p blockingSearchProvider) Configured() bool {
	return true
}
func (p blockingSearchProvider) Search(ctx context.Context, _ webSearchOptions) ([]webSearchResult, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestOllamaWebSearchUsesConfiguredBase(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/web_search" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("unexpected auth header: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"results": [
				{"title": "Ollama Docs", "url": "https://example.com/ollama", "content": "web_search result"}
			]
		}`))
	}))
	defer server.Close()

	t.Setenv("OLLAMA_API_KEY", "test-key")
	t.Setenv("CORELAY_OLLAMA_WEB_BASE_URL", server.URL)

	input, _ := json.Marshal(map[string]any{
		"query":       "ollama web search",
		"provider":    "ollama",
		"max_results": 3,
	})
	out, isErr := executeWebSearch(input, t.TempDir())
	if isErr {
		t.Fatalf("executeWebSearch returned error: %s", out)
	}
	for _, want := range []string{
		`WebSearch query="ollama web search" provider=ollama results=1`,
		"Ollama Docs",
		"https://example.com/ollama",
		"web_search result",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected output to contain %q, got:\n%s", want, out)
		}
	}
}

func TestExecuteWebResearchUsesSearchAndFetch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/web_search":
			_, _ = w.Write([]byte(`{
				"results": [
					{"title": "Loop Engineering", "url": "https://example.com/loop", "content": "loop search snippet"}
				]
			}`))
		case "/api/web_fetch":
			_, _ = w.Write([]byte(`{
				"title": "Loop Engineering",
				"content": "Loop engineering wraps a maker, checker, state, and exit gate around local models.",
				"links": ["https://example.com/source"]
			}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	t.Setenv("OLLAMA_API_KEY", "test-key")
	t.Setenv("CORELAY_OLLAMA_WEB_BASE_URL", server.URL)

	input, _ := json.Marshal(map[string]any{
		"query":       "loop engineering local models",
		"provider":    "ollama",
		"max_results": 2,
		"fetch_top":   1,
		"max_chars":   1000,
	})
	out, isErr := executeWebResearch(input, t.TempDir())
	if isErr {
		t.Fatalf("executeWebResearch returned error: %s", out)
	}
	for _, want := range []string{
		`WebResearch query="loop engineering local models" search_provider=ollama`,
		"Sources:",
		"Fetched context:",
		"Loop engineering wraps a maker, checker, state, and exit gate around local models.",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected output to contain %q, got:\n%s", want, out)
		}
	}
}
