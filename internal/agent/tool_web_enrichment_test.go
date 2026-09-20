package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestWebFetchMarkdownStructure(t *testing.T) {
	base, _ := url.Parse("https://example.com/docs/start")
	got := webHTMLToMarkdown(`<html><head><title>Title</title></head><body><nav>NO NAV</nav><aside>NO ASIDE</aside><main>
<h1>Install</h1><p>Read <a href="../guide">the guide</a>.</p>
<pre><code>func main() {
    run()

    done()
}</code></pre><table><tr><th>Option</th><th>Value</th></tr><tr><td>retry</td><td>3</td></tr></table>
<p hidden>NO HIDDEN</p><a href="javascript:alert(1)">Safe label</a></main></body></html>`, base)
	for _, want := range []string{"# Install", "[the guide](https://example.com/guide)", "    run()\n\n    done()", "| Option | Value |\n| --- | --- |", "| retry | 3 |"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	for _, unwanted := range []string{"NO NAV", "NO ASIDE", "NO HIDDEN", "javascript:"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("unexpected %q in %s", unwanted, got)
		}
	}
	selected := extractRelevantText(got, "done", 1000)
	if !strings.Contains(selected, "func main()") || !strings.Contains(selected, "    done()") || strings.Count(selected, "```") != 2 {
		t.Fatalf("code block split: %s", selected)
	}
}

func TestWebFetchSelectsKoreanTailBeforeTruncation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<main>" + strings.Repeat("<p>Unrelated introductory material.</p>", 2000) + "<p>설치 방법: corelaycode 실행</p></main>"))
	}))
	defer server.Close()
	input, _ := json.Marshal(webFetchArgs{URL: server.URL, Prompt: "설치", MaxChars: 100})
	out, failed := ExecuteTool("WebFetch", input, t.TempDir())
	if failed || !strings.Contains(out, "설치 방법: corelaycode 실행") || strings.Contains(out, "introductory") {
		t.Fatalf("tail lost: %s (error=%v)", out, failed)
	}
	input, _ = json.Marshal(webFetchArgs{URL: server.URL, MaxChars: 100})
	out, failed = ExecuteTool("WebFetch", input, t.TempDir())
	if failed || len([]rune(out)) > 1000 || !strings.Contains(out, "truncated") {
		t.Fatalf("unbounded output: length=%d error=%v", len(out), failed)
	}
}
