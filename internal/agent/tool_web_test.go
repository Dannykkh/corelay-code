package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	t.Setenv("ANICLEW_OLLAMA_WEB_BASE_URL", server.URL)

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
	t.Setenv("ANICLEW_OLLAMA_WEB_BASE_URL", server.URL)

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
