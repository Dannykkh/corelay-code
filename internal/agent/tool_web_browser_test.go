package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func requireInstalledBrowser(t *testing.T) {
	t.Helper()
	bin, err := installedWebBrowser()
	if err != nil {
		t.Fatal(err)
	}
	if bin == "" {
		t.Skip("no installed Chrome/Chromium; browser tests never download during CI")
	}
}

func TestWebBrowserInstalledIsReadOnlyAndDoesNotExposePath(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CORELAY_BROWSER_PATH", executable)
	if !WebBrowserInstalled() {
		t.Fatal("configured executable should be reported as installed without launching it")
	}
	t.Setenv("CORELAY_BROWSER_PATH", filepath.Join(t.TempDir(), "missing-browser"))
	if WebBrowserInstalled() {
		t.Fatal("missing configured browser should be reported as unavailable")
	}
}

func TestBrowserFetchRenderedPageAndAutoFallback(t *testing.T) {
	requireInstalledBrowser(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/frame":
			_, _ = w.Write([]byte(`<html><body><p>IFRAME_RENDERED_MARKER</p><a href="/guide">Frame guide</a></body></html>`))
		default:
			_, _ = w.Write([]byte(`<html><head><title>Browser Fixture</title></head><body><main id="app"></main><script>
setTimeout(() => {
 document.querySelector('#app').innerHTML = '<h1>설치 안내</h1><p>JAVASCRIPT_RENDERED_MARKER</p><table><tr><th>Option</th><th>Value</th></tr><tr><td>retry</td><td>3</td></tr></table><div id="shadow"></div><iframe src="/frame"></iframe>';
 document.querySelector('#shadow').attachShadow({mode:'open'}).innerHTML = '<p>SHADOW_RENDERED_MARKER</p>';
}, 200);
</script></body></html>`))
		}
	}))
	defer server.Close()
	before, _ := filepath.Glob(filepath.Join(os.TempDir(), "corelay-web-browser-*"))
	for _, provider := range []string{"direct", "browser", "auto"} {
		t.Run(provider, func(t *testing.T) {
			input, _ := json.Marshal(webFetchArgs{URL: server.URL, Provider: provider, MaxChars: 5000})
			out, failed := ExecuteTool("WebFetch", input, t.TempDir())
			if failed {
				t.Fatalf("fetch failed: %s", out)
			}
			if provider == "direct" {
				if strings.Contains(out, "JAVASCRIPT_RENDERED_MARKER") {
					t.Fatal("direct unexpectedly ran scripts")
				}
				return
			}
			for _, want := range []string{"provider=browser", "# 설치 안내", "JAVASCRIPT_RENDERED_MARKER", "IFRAME_RENDERED_MARKER", "SHADOW_RENDERED_MARKER", "| retry | 3 |", server.URL + "/guide"} {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q in %s", want, out)
				}
			}
		})
	}
	after, _ := filepath.Glob(filepath.Join(os.TempDir(), "corelay-web-browser-*"))
	if strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Fatalf("browser profiles leaked: before=%v after=%v", before, after)
	}
	if len(webBrowserSlots) != 0 {
		t.Fatal("browser slot leaked")
	}
}

func TestBrowserFetchCancelClosesPage(t *testing.T) {
	requireInstalledBrowser(t)
	started, disconnected := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/slow" {
			return
		}
		close(started)
		<-r.Context().Done()
		close(disconnected)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan bool, 1)
	go func() { _, err := browserWebFetch(ctx, server.URL+"/slow"); done <- err != nil }()
	select {
	case <-started:
	case <-time.After(20 * time.Second):
		t.Fatal("browser never navigated")
	}
	cancel()
	select {
	case failed := <-done:
		if !failed {
			t.Fatal("cancel succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("browser ignored cancel")
	}
	select {
	case <-disconnected:
	case <-time.After(5 * time.Second):
		t.Fatal("browser connection leaked")
	}
	if len(webBrowserSlots) != 0 {
		t.Fatal("canceled browser slot leaked")
	}
}

func TestBrowserFetchErrorAndRedirect(t *testing.T) {
	requireInstalledBrowser(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "/final", http.StatusFound)
		case "/final":
			_, _ = w.Write([]byte("<html><head><title>Final</title></head><body><main>FINAL_PAGE_MARKER</main></body></html>"))
		default:
			http.Error(w, "missing page", http.StatusNotFound)
		}
	}))
	defer server.Close()
	result, err := browserWebFetch(context.Background(), server.URL+"/redirect")
	if err != nil || result.FinalURL != server.URL+"/final" || !strings.Contains(result.Content, "FINAL_PAGE_MARKER") {
		t.Fatalf("redirect: %+v %v", result, err)
	}
	if _, err := browserWebFetch(context.Background(), server.URL+"/missing"); err == nil {
		t.Fatal("HTTP 404 accepted")
	}
}

func TestBrowserFetchOfflineAndValidation(t *testing.T) {
	for _, target := range []string{"file:///etc/passwd", "javascript:alert(1)", "http://user:password@example.com"} {
		if _, err := browserWebFetch(context.Background(), target); err == nil {
			t.Fatalf("accepted %s", target)
		}
	}
	t.Setenv("CORELAY_BROWSER_PATH", filepath.Join(t.TempDir(), "nonexistent-browser"))
	if _, err := browserWebFetch(context.Background(), "https://example.com"); err == nil || !strings.Contains(err.Error(), "CORELAY_BROWSER_PATH") {
		t.Fatalf("invalid executable: %v", err)
	}
	t.Setenv("CORELAY_OFFLINE", "1")
	out, failed := ExecuteTool("WebFetch", json.RawMessage(`{"url":"https://example.com","provider":"browser"}`), t.TempDir())
	if !failed || !strings.Contains(out, "OFFLINE") {
		t.Fatalf("offline gate: %s", out)
	}
}

func TestBrowserFallbackDetectsScriptsNotMetadata(t *testing.T) {
	for _, tc := range []struct {
		html string
		want bool
	}{
		{`<script src="app.js"></script><main></main>`, true},
		{`<script type="module">render()</script>`, true},
		{`<script type="application/ld+json">{}</script><p>Short static page</p>`, false},
		{`<p>Short static page</p>`, false},
	} {
		if got := webPageNeedsBrowser(tc.html, htmlToText(tc.html)); got != tc.want {
			t.Errorf("got %v want %v for %s", got, tc.want, tc.html)
		}
	}
}

func TestBrowserAutoKeepsStaticPagesOnHTTP(t *testing.T) {
	t.Setenv("CORELAY_BROWSER_PATH", filepath.Join(t.TempDir(), "missing-browser"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<main>Short static document</main>"))
	}))
	defer server.Close()
	result, err := autoWebFetch(context.Background(), server.URL)
	if err != nil || result.Source != "direct" {
		t.Fatalf("static page launched browser: %+v %v", result, err)
	}
}

func TestBrowserAutoKeepsLongJavaScriptShellWhenStaticBodyIsSufficient(t *testing.T) {
	t.Setenv("CORELAY_BROWSER_PATH", filepath.Join(t.TempDir(), "missing-browser"))
	staticBody := strings.Repeat("This is stable server-rendered documentation content. ", 12)
	longShell := strings.Repeat("const hydrationChunk = 'client shell';", 1200)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, "<html><body><main><h1>Static documentation</h1><p>%s</p></main><script>%s</script></body></html>", staticBody, longShell)
	}))
	defer server.Close()

	raw := fmt.Sprintf("<html><body><main><p>%s</p></main><script>%s</script></body></html>", staticBody, longShell)
	if webPageNeedsBrowser(raw, htmlToText(raw)) {
		t.Fatal("sufficient static content incorrectly triggered browser rendering")
	}
	result, err := autoWebFetch(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("static page with long shell failed: %v", err)
	}
	if result.Source != "direct" || !strings.Contains(result.Content, "Static documentation") {
		t.Fatalf("long shell changed provider selection: source=%q content=%q", result.Source, result.Content)
	}
}

func TestBrowserFetchChangingDOMReturnsBoundedMeaningfulSnapshot(t *testing.T) {
	requireInstalledBrowser(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<html><body><main><h1>Live status</h1><p id="stable">This readable content remains useful while a client dashboard keeps updating.</p></main><script>setInterval(() => document.body.setAttribute('data-tick', String(Date.now())), 25)</script></body></html>`)
	}))
	defer server.Close()

	result, err := browserWebFetch(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("changing DOM failed instead of returning bounded snapshot: %v", err)
	}
	if result.LimitNotice == "" || !strings.Contains(result.LimitNotice, "bounded snapshot") {
		t.Fatalf("missing bounded snapshot notice: %q", result.LimitNotice)
	}
	if !strings.Contains(result.Content, "This readable content remains useful") {
		t.Fatalf("meaningful content was lost: %s", result.Content)
	}
	formatted := formatFetchResult(result, "", 12000)
	if !strings.Contains(formatted, "Limit: DOM kept changing") {
		t.Fatalf("formatted result omitted limit notice: %s", formatted)
	}
}

func TestBrowserFetchRejectsOversizedDOM(t *testing.T) {
	requireInstalledBrowser(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<main id="app"></main><script>document.querySelector('#app').textContent = 'x'.repeat(2097153)</script>`))
	}))
	defer server.Close()
	_, err := browserWebFetch(context.Background(), server.URL)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized DOM accepted: %v", err)
	}
}
