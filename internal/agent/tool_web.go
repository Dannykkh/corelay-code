package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	defaultSearchResults = 5
	maxSearchResults     = 10
	defaultFetchTop      = 3
	defaultFetchChars    = 12000
	maxFetchChars        = 30000
	maxFrameDepth        = 2
	maxFrameFetches      = 3
	minReadableFrameText = 800
)

type webSearchArgs struct {
	Query      string   `json:"query"`
	MaxResults int      `json:"max_results,omitempty"`
	Provider   string   `json:"provider,omitempty"`  // auto, multi, ollama, duckduckgo, google, naver, bing, yahoo
	Providers  []string `json:"providers,omitempty"` // explicit provider list
	Sort       string   `json:"sort,omitempty"`      // relevance, latest
	Recency    string   `json:"recency,omitempty"`   // day, week, month, year
	DateFrom   string   `json:"date_from,omitempty"` // YYYY-MM-DD
	DateTo     string   `json:"date_to,omitempty"`   // YYYY-MM-DD
}

type webFetchArgs struct {
	URL      string `json:"url"`
	Prompt   string `json:"prompt,omitempty"`
	MaxChars int    `json:"max_chars,omitempty"`
	Provider string `json:"provider,omitempty"` // auto, ollama, direct
}

type webResearchArgs struct {
	Query      string   `json:"query"`
	MaxResults int      `json:"max_results,omitempty"`
	FetchTop   int      `json:"fetch_top,omitempty"`
	MaxChars   int      `json:"max_chars,omitempty"`
	Provider   string   `json:"provider,omitempty"`  // auto, multi, ollama, duckduckgo, google, naver, bing, yahoo
	Providers  []string `json:"providers,omitempty"` // explicit provider list
	Sort       string   `json:"sort,omitempty"`      // relevance, latest
	Recency    string   `json:"recency,omitempty"`   // day, week, month, year
	DateFrom   string   `json:"date_from,omitempty"` // YYYY-MM-DD
	DateTo     string   `json:"date_to,omitempty"`   // YYYY-MM-DD
}

type webSearchResult struct {
	Rank           int      `json:"rank"`
	Title          string   `json:"title"`
	URL            string   `json:"url"`
	Snippet        string   `json:"snippet"`
	Source         string   `json:"source"`
	Providers      []string `json:"providers,omitempty"`
	SearchedAt     string   `json:"searched_at,omitempty"`
	PublishedAt    string   `json:"published_at,omitempty"`
	UpdatedAt      string   `json:"updated_at,omitempty"`
	RelevanceScore float64  `json:"relevance_score,omitempty"`
	FreshnessScore float64  `json:"freshness_score,omitempty"`
	AuthorityScore float64  `json:"authority_score,omitempty"`
	Score          float64  `json:"score,omitempty"`
}

type webFetchResult struct {
	URL         string   `json:"url"`
	FinalURL    string   `json:"final_url,omitempty"`
	Title       string   `json:"title,omitempty"`
	Status      int      `json:"status,omitempty"`
	ContentType string   `json:"content_type,omitempty"`
	Content     string   `json:"content"`
	Links       []string `json:"links,omitempty"`
	Source      string   `json:"source"`
}

// WebFetch fetches a URL and returns cleaned text content.
func executeWebFetch(input json.RawMessage, _ string) (string, bool) {
	var args webFetchArgs
	json.Unmarshal(input, &args)
	args.URL = strings.TrimSpace(args.URL)
	if args.URL == "" {
		return "URL is required", true
	}
	args.MaxChars = normalizePositiveLimit(args.MaxChars, defaultFetchChars, maxFetchChars)

	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()

	result, err := fetchWeb(ctx, args)
	if err != nil {
		return err.Error(), true
	}
	return formatFetchResult(result, args.Prompt, args.MaxChars), false
}

// WebSearch searches the web with the built-in DuckDuckGo provider by default.
// Ollama's hosted web_search is only used when provider:"ollama" is explicit.
func executeWebSearch(input json.RawMessage, _ string) (string, bool) {
	var args webSearchArgs
	json.Unmarshal(input, &args)
	args.Query = strings.TrimSpace(args.Query)
	if args.Query == "" {
		return "Query is required", true
	}
	args.MaxResults = normalizePositiveLimit(args.MaxResults, defaultSearchResults, maxSearchResults)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	results, provider, fallbackNote, err := searchWeb(ctx, args)
	if err != nil {
		return err.Error(), true
	}
	return formatSearchResults(args.Query, provider, fallbackNote, results), false
}

// WebResearch searches, fetches the top pages, and returns compact cited context.
func executeWebResearch(input json.RawMessage, _ string) (string, bool) {
	var args webResearchArgs
	json.Unmarshal(input, &args)
	args.Query = strings.TrimSpace(args.Query)
	if args.Query == "" {
		return "Query is required", true
	}
	args.MaxResults = normalizePositiveLimit(args.MaxResults, defaultSearchResults, maxSearchResults)
	args.FetchTop = normalizePositiveLimit(args.FetchTop, defaultFetchTop, args.MaxResults)
	args.MaxChars = normalizePositiveLimit(args.MaxChars, defaultFetchChars, maxFetchChars)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	results, provider, fallbackNote, err := searchWeb(ctx, webSearchArgs{
		Query:      args.Query,
		MaxResults: args.MaxResults,
		Provider:   args.Provider,
		Providers:  args.Providers,
		Sort:       args.Sort,
		Recency:    args.Recency,
		DateFrom:   args.DateFrom,
		DateTo:     args.DateTo,
	})
	if err != nil {
		return err.Error(), true
	}

	var b strings.Builder
	fmt.Fprintf(&b, "WebResearch query=%q search_provider=%s\n", args.Query, provider)
	if fallbackNote != "" {
		fmt.Fprintf(&b, "Note: %s\n", fallbackNote)
	}
	b.WriteString("\nSources:\n")
	for _, r := range results {
		fmt.Fprintf(&b, "[%d] %s\n    %s\n    %s\n", r.Rank, r.Title, r.URL, r.Snippet)
		if r.PublishedAt != "" || r.UpdatedAt != "" || len(r.Providers) > 0 {
			fmt.Fprintf(&b, "    published=%s updated=%s providers=%s score=%.2f\n",
				coalesceString(r.PublishedAt, "unknown"),
				coalesceString(r.UpdatedAt, "unknown"),
				strings.Join(r.Providers, ","),
				r.Score)
		}
	}

	if len(results) == 0 {
		return b.String(), false
	}

	b.WriteString("\nFetched context:\n")
	fetched := 0
	for _, r := range results {
		if fetched >= args.FetchTop {
			break
		}
		if strings.TrimSpace(r.URL) == "" {
			fmt.Fprintf(&b, "\n[%d] fetch skipped: result has no URL\n", r.Rank)
			continue
		}
		fetchResult, ferr := fetchWeb(ctx, webFetchArgs{
			URL:      r.URL,
			Prompt:   args.Query,
			MaxChars: args.MaxChars / maxInt(1, args.FetchTop),
			Provider: researchFetchProvider(args),
		})
		if ferr != nil {
			fmt.Fprintf(&b, "\n[%d] fetch failed: %v\n", r.Rank, ferr)
			continue
		}
		fetched++
		content := extractRelevantText(fetchResult.Content, args.Query, args.MaxChars/maxInt(1, args.FetchTop))
		fmt.Fprintf(&b, "\n[%d] %s\nURL: %s\nProvider: %s\n%s\n",
			r.Rank, coalesceString(fetchResult.Title, r.Title), fetchResult.URL, fetchResult.Source, content)
	}

	return b.String(), false
}

func researchFetchProvider(args webResearchArgs) string {
	if len(args.Providers) == 0 && strings.EqualFold(strings.TrimSpace(args.Provider), "ollama") {
		return "ollama"
	}
	return "direct"
}

func searchWeb(ctx context.Context, args webSearchArgs) ([]webSearchResult, string, string, error) {
	return searchWebWithProviders(ctx, args)
}

func fetchWeb(ctx context.Context, args webFetchArgs) (webFetchResult, error) {
	provider := strings.ToLower(strings.TrimSpace(args.Provider))
	if provider == "" || provider == "auto" {
		provider = webFetchDefaultProvider()
	}

	switch provider {
	case "ollama":
		result, err := ollamaWebFetch(ctx, args.URL)
		if err == nil {
			result.Content = truncateString(result.Content, args.MaxChars)
			return result, nil
		}
		if strings.EqualFold(strings.TrimSpace(args.Provider), "ollama") {
			return webFetchResult{}, err
		}
		result, derr := directWebFetch(ctx, args.URL, args.MaxChars)
		if derr != nil {
			return webFetchResult{}, fmt.Errorf("ollama web_fetch failed: %v; direct fetch failed: %w", err, derr)
		}
		return result, nil
	case "direct", "duckduckgo", "ddg":
		return directWebFetch(ctx, args.URL, args.MaxChars)
	default:
		return webFetchResult{}, fmt.Errorf("unsupported web fetch provider %q", args.Provider)
	}
}

func webSearchDefaultProvider() string {
	return "duckduckgo"
}

func webFetchDefaultProvider() string {
	return "direct"
}

func ollamaWebSearch(ctx context.Context, query string, maxResults int) ([]webSearchResult, error) {
	key := strings.TrimSpace(os.Getenv("OLLAMA_API_KEY"))
	if key == "" {
		return nil, fmt.Errorf("OLLAMA_API_KEY is not set")
	}
	base := strings.TrimRight(coalesceString(os.Getenv("ANICLEW_OLLAMA_WEB_BASE_URL"), "https://ollama.com"), "/")
	body, _ := json.Marshal(map[string]any{
		"query":       query,
		"max_results": maxResults,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/web_search", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "AniClew/1.0 (Ollama WebSearch)")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama web_search request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
		return nil, fmt.Errorf("ollama web_search HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var decoded struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2*1024*1024)).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("parse ollama web_search: %w", err)
	}
	results := make([]webSearchResult, 0, len(decoded.Results))
	for i, r := range decoded.Results {
		if strings.TrimSpace(r.URL) == "" {
			continue
		}
		results = append(results, webSearchResult{
			Rank:    i + 1,
			Title:   strings.TrimSpace(r.Title),
			URL:     strings.TrimSpace(r.URL),
			Snippet: strings.TrimSpace(r.Content),
			Source:  "ollama",
		})
	}
	return results, nil
}

func ollamaWebFetch(ctx context.Context, targetURL string) (webFetchResult, error) {
	key := strings.TrimSpace(os.Getenv("OLLAMA_API_KEY"))
	if key == "" {
		return webFetchResult{}, fmt.Errorf("OLLAMA_API_KEY is not set")
	}
	base := strings.TrimRight(coalesceString(os.Getenv("ANICLEW_OLLAMA_WEB_BASE_URL"), "https://ollama.com"), "/")
	body, _ := json.Marshal(map[string]any{"url": targetURL})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/web_fetch", bytes.NewReader(body))
	if err != nil {
		return webFetchResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "AniClew/1.0 (Ollama WebFetch)")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return webFetchResult{}, fmt.Errorf("ollama web_fetch request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
		return webFetchResult{}, fmt.Errorf("ollama web_fetch HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var decoded struct {
		Title   string   `json:"title"`
		Content string   `json:"content"`
		Links   []string `json:"links"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2*1024*1024)).Decode(&decoded); err != nil {
		return webFetchResult{}, fmt.Errorf("parse ollama web_fetch: %w", err)
	}
	return webFetchResult{
		URL:     targetURL,
		Title:   strings.TrimSpace(decoded.Title),
		Content: strings.TrimSpace(decoded.Content),
		Links:   decoded.Links,
		Source:  "ollama",
	}, nil
}

func duckDuckGoSearch(ctx context.Context, opts webSearchOptions) ([]webSearchResult, error) {
	searchURL := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(opts.Query)
	if df := duckDuckGoDateFilter(opts.Recency); df != "" {
		searchURL += "&df=" + url.QueryEscape(df)
	}
	log.Printf("[WebSearch] DuckDuckGo: %s", opts.Query)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", browserUserAgent())
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("DuckDuckGo search error: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("DuckDuckGo HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return nil, fmt.Errorf("read DuckDuckGo results: %w", err)
	}
	results := extractDuckDuckGoResults(string(body), opts.MaxResults)
	if len(results) == 0 {
		text := htmlToText(string(body))
		if text != "" {
			results = append(results, webSearchResult{
				Rank:    1,
				Title:   "DuckDuckGo raw result text",
				Snippet: truncateString(text, 2000),
				Source:  "duckduckgo",
			})
		}
	}
	return results, nil
}

func directWebFetch(ctx context.Context, targetURL string, maxChars int) (webFetchResult, error) {
	client := &http.Client{Timeout: 35 * time.Second}
	visited := map[string]bool{}
	return directWebFetchRecursive(ctx, client, targetURL, maxChars, 0, visited)
}

func directWebFetchRecursive(ctx context.Context, client *http.Client, targetURL string, maxChars, depth int, visited map[string]bool) (webFetchResult, error) {
	originalURL := strings.TrimSpace(targetURL)
	fetchURL := canonicalWebFetchURL(originalURL)
	parsed, err := url.Parse(fetchURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return webFetchResult{}, fmt.Errorf("invalid URL: %s", targetURL)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return webFetchResult{}, fmt.Errorf("unsupported URL scheme: %s", parsed.Scheme)
	}
	normalized := parsed.String()
	if visited[normalized] {
		return webFetchResult{}, fmt.Errorf("skipping already fetched URL: %s", normalized)
	}
	visited[normalized] = true

	log.Printf("[WebFetch] Direct: %s", fetchURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return webFetchResult{}, fmt.Errorf("invalid URL: %w", err)
	}
	req.Header.Set("User-Agent", browserUserAgent())
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/json,text/plain")

	resp, err := client.Do(req)
	if err != nil {
		return webFetchResult{}, fmt.Errorf("fetch error: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return webFetchResult{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return webFetchResult{}, fmt.Errorf("read error: %w", err)
	}
	content := string(body)
	contentType := resp.Header.Get("Content-Type")
	title := ""
	links := []string{}
	if strings.Contains(strings.ToLower(contentType), "html") || looksLikeHTML(content) {
		rawHTML := content
		title = extractHTMLTitle(rawHTML)
		links = extractLinks(rawHTML, resp.Request.URL)
		content = htmlToText(rawHTML)

		if shouldFetchFrames(rawHTML, content, depth) {
			frameURLs := extractFrameSources(rawHTML, resp.Request.URL, maxFrameFetches)
			for _, frameURL := range frameURLs {
				frameResult, ferr := directWebFetchRecursive(ctx, client, frameURL, maxChars, depth+1, visited)
				if ferr != nil || strings.TrimSpace(frameResult.Content) == "" {
					continue
				}
				if title == "" {
					title = frameResult.Title
				}
				links = appendUniqueStrings(links, frameResult.Links...)
				content = strings.TrimSpace(content + "\n\n--- Frame: " + frameResult.URL + " ---\n" + frameResult.Content)
			}
		}
	}
	if maxChars <= 0 {
		maxChars = defaultFetchChars
	}
	content = truncateString(content, maxChars)
	return webFetchResult{
		URL:         originalURL,
		FinalURL:    resp.Request.URL.String(),
		Title:       title,
		Status:      resp.StatusCode,
		ContentType: contentType,
		Content:     content,
		Links:       links,
		Source:      "direct",
	}, nil
}

func canonicalWebFetchURL(raw string) string {
	if mobileURL, ok := naverMobilePostURL(raw); ok {
		return mobileURL
	}
	return raw
}

func naverMobilePostURL(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if host != "blog.naver.com" && host != "m.blog.naver.com" {
		return "", false
	}

	parts := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	if len(parts) >= 2 && parts[0] != "" && isDigits(parts[1]) {
		return "https://m.blog.naver.com/" + parts[0] + "/" + parts[1], true
	}

	if strings.EqualFold(strings.Trim(u.Path, "/"), "PostView.naver") {
		blogID := strings.TrimSpace(u.Query().Get("blogId"))
		logNo := strings.TrimSpace(u.Query().Get("logNo"))
		if blogID != "" && isDigits(logNo) {
			return "https://m.blog.naver.com/" + url.PathEscape(blogID) + "/" + logNo, true
		}
	}
	return "", false
}

func shouldFetchFrames(pageHTML, text string, depth int) bool {
	if depth >= maxFrameDepth || !strings.Contains(strings.ToLower(pageHTML), "<iframe") {
		return false
	}
	if len([]rune(strings.TrimSpace(text))) < minReadableFrameText {
		return true
	}
	lower := strings.ToLower(pageHTML)
	return strings.Contains(lower, `id="mainframe"`) ||
		strings.Contains(lower, `name="mainframe"`) ||
		strings.Contains(lower, "postview.naver")
}

func extractFrameSources(page string, base *url.URL, limit int) []string {
	frameMatches := regexp.MustCompile(`(?is)<iframe\b([^>]*)>`).FindAllStringSubmatch(page, -1)
	type candidate struct {
		url      string
		priority int
		idx      int
	}
	candidates := make([]candidate, 0, len(frameMatches))
	seen := map[string]bool{}
	for i, m := range frameMatches {
		if len(m) < 2 {
			continue
		}
		attrs := m[1]
		src := htmlAttrValue(attrs, "src")
		if src == "" {
			continue
		}
		resolved, ok := resolveFrameURL(src, base)
		if !ok || seen[resolved] {
			continue
		}
		seen[resolved] = true
		lowerAttrs := strings.ToLower(attrs)
		priority := 0
		if strings.Contains(lowerAttrs, "mainframe") || strings.Contains(strings.ToLower(resolved), "postview.naver") {
			priority = 2
		} else if base != nil {
			if parsed, err := url.Parse(resolved); err == nil && canFetchFrame(base, parsed) {
				priority = 1
			}
		}
		candidates = append(candidates, candidate{url: resolved, priority: priority, idx: i})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].priority == candidates[j].priority {
			return candidates[i].idx < candidates[j].idx
		}
		return candidates[i].priority > candidates[j].priority
	})
	if limit <= 0 || len(candidates) < limit {
		limit = len(candidates)
	}
	out := make([]string, 0, limit)
	for _, c := range candidates[:limit] {
		parsed, err := url.Parse(c.url)
		if err == nil && base != nil && !canFetchFrame(base, parsed) {
			continue
		}
		out = append(out, c.url)
	}
	return out
}

func htmlAttrValue(attrs, name string) string {
	pattern := fmt.Sprintf(`(?is)\b%s\s*=\s*("([^"]*)"|'([^']*)'|([^\s>]+))`, regexp.QuoteMeta(name))
	match := regexp.MustCompile(pattern).FindStringSubmatch(attrs)
	if len(match) == 0 {
		return ""
	}
	for _, value := range match[2:] {
		if value != "" {
			return html.UnescapeString(strings.TrimSpace(value))
		}
	}
	return ""
}

func resolveFrameURL(raw string, base *url.URL) (string, bool) {
	raw = strings.TrimSpace(html.UnescapeString(raw))
	if raw == "" || strings.HasPrefix(strings.ToLower(raw), "javascript:") || strings.HasPrefix(strings.ToLower(raw), "about:") {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	if base != nil {
		u = base.ResolveReference(u)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}
	return u.String(), true
}

func canFetchFrame(parent, child *url.URL) bool {
	if parent == nil || child == nil {
		return false
	}
	if !strings.EqualFold(parent.Hostname(), child.Hostname()) {
		return isNaverHost(parent.Hostname()) && isNaverHost(child.Hostname())
	}
	return true
}

func isNaverHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	return host == "naver.com" || strings.HasSuffix(host, ".naver.com")
}

func extractDuckDuckGoResults(page string, maxResults int) []webSearchResult {
	anchors := regexp.MustCompile(`(?is)<a[^>]+class="[^"]*result__a[^"]*"[^>]+href="([^"]+)"[^>]*>(.*?)</a>`).FindAllStringSubmatch(page, -1)
	snippets := regexp.MustCompile(`(?is)<a[^>]+class="[^"]*result__snippet[^"]*"[^>]*>(.*?)</a>|<div[^>]+class="[^"]*result__snippet[^"]*"[^>]*>(.*?)</div>`).FindAllStringSubmatch(page, -1)
	results := make([]webSearchResult, 0, minInt(maxResults, len(anchors)))
	seen := map[string]bool{}
	for i, match := range anchors {
		if len(results) >= maxResults {
			break
		}
		target := normalizeDuckDuckGoURL(html.UnescapeString(match[1]))
		if target == "" || seen[target] {
			continue
		}
		seen[target] = true
		snippet := ""
		if i < len(snippets) {
			for _, part := range snippets[i][1:] {
				if part != "" {
					snippet = cleanHTMLFragment(part)
					break
				}
			}
		}
		results = append(results, webSearchResult{
			Rank:    len(results) + 1,
			Title:   cleanHTMLFragment(match[2]),
			URL:     target,
			Snippet: snippet,
			Source:  "duckduckgo",
		})
	}
	return results
}

func normalizeDuckDuckGoURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	}
	if strings.HasPrefix(raw, "/") {
		raw = "https://duckduckgo.com" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if uddg := parsed.Query().Get("uddg"); uddg != "" {
		if decoded, err := url.QueryUnescape(uddg); err == nil {
			return decoded
		}
		return uddg
	}
	return raw
}

func formatSearchResults(query, provider, note string, results []webSearchResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "WebSearch query=%q provider=%s results=%d\n", query, provider, len(results))
	if note != "" {
		fmt.Fprintf(&b, "Note: %s\n", note)
	}
	for _, r := range results {
		fmt.Fprintf(&b, "\n[%d] %s\nURL: %s\nSnippet: %s\nSource: %s\n",
			r.Rank, coalesceString(r.Title, "(untitled)"), r.URL, r.Snippet, r.Source)
		if r.SearchedAt != "" {
			fmt.Fprintf(&b, "Searched-At: %s\n", r.SearchedAt)
		}
		if r.PublishedAt != "" {
			fmt.Fprintf(&b, "Published-At: %s\n", r.PublishedAt)
		}
		if r.UpdatedAt != "" {
			fmt.Fprintf(&b, "Updated-At: %s\n", r.UpdatedAt)
		}
		if len(r.Providers) > 0 {
			fmt.Fprintf(&b, "Providers: %s\n", strings.Join(r.Providers, ", "))
		}
		if r.Score > 0 {
			fmt.Fprintf(&b, "Scores: total=%.2f relevance=%.2f freshness=%.2f authority=%.2f\n",
				r.Score, r.RelevanceScore, r.FreshnessScore, r.AuthorityScore)
		}
	}
	if len(results) == 0 {
		b.WriteString("\nNo results returned.")
	}
	return b.String()
}

func formatFetchResult(result webFetchResult, prompt string, maxChars int) string {
	content := result.Content
	if strings.TrimSpace(prompt) != "" {
		content = extractRelevantText(content, prompt, maxChars)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "WebFetch provider=%s\nURL: %s\n", result.Source, result.URL)
	if result.FinalURL != "" && result.FinalURL != result.URL {
		fmt.Fprintf(&b, "Final-URL: %s\n", result.FinalURL)
	}
	if result.Status != 0 {
		fmt.Fprintf(&b, "Status: %d\n", result.Status)
	}
	if result.ContentType != "" {
		fmt.Fprintf(&b, "Content-Type: %s\n", result.ContentType)
	}
	if result.Title != "" {
		fmt.Fprintf(&b, "Title: %s\n", result.Title)
	}
	if len(result.Links) > 0 {
		fmt.Fprintf(&b, "Links: %s\n", strings.Join(limitStrings(result.Links, 10), ", "))
	}
	fmt.Fprintf(&b, "\n%s", content)
	return b.String()
}

func extractRelevantText(content, prompt string, maxChars int) string {
	content = strings.TrimSpace(content)
	if content == "" || strings.TrimSpace(prompt) == "" {
		return truncateString(content, maxChars)
	}
	keywords := keywordSet(prompt)
	if len(keywords) == 0 {
		return truncateString(content, maxChars)
	}
	paragraphs := splitParagraphs(content)
	type scored struct {
		text  string
		score int
		idx   int
	}
	var ranked []scored
	for i, p := range paragraphs {
		lower := strings.ToLower(p)
		score := 0
		for kw := range keywords {
			if strings.Contains(lower, kw) {
				score++
			}
		}
		if score > 0 {
			ranked = append(ranked, scored{text: p, score: score, idx: i})
		}
	}
	if len(ranked) == 0 {
		return truncateString(content, maxChars)
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score == ranked[j].score {
			return ranked[i].idx < ranked[j].idx
		}
		return ranked[i].score > ranked[j].score
	})
	var selected []scored
	total := 0
	for _, p := range ranked {
		if total+len(p.text) > maxChars && len(selected) > 0 {
			break
		}
		selected = append(selected, p)
		total += len(p.text) + 2
	}
	sort.SliceStable(selected, func(i, j int) bool { return selected[i].idx < selected[j].idx })
	var parts []string
	for _, p := range selected {
		parts = append(parts, p.text)
	}
	return truncateString(strings.Join(parts, "\n\n"), maxChars)
}

// htmlToText strips HTML tags and returns readable text.
func htmlToText(input string) string {
	input = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`).ReplaceAllString(input, "")
	input = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`).ReplaceAllString(input, "")
	input = regexp.MustCompile(`(?is)<noscript[^>]*>.*?</noscript>`).ReplaceAllString(input, "")
	input = regexp.MustCompile(`(?is)<nav[^>]*>.*?</nav>`).ReplaceAllString(input, "")
	input = regexp.MustCompile(`(?is)<footer[^>]*>.*?</footer>`).ReplaceAllString(input, "")
	input = regexp.MustCompile(`(?i)<br\s*/?\s*>`).ReplaceAllString(input, "\n")
	input = regexp.MustCompile(`(?i)</?(p|div|h[1-6]|li|tr|section|article|main|blockquote|pre|table)[^>]*>`).ReplaceAllString(input, "\n")
	input = regexp.MustCompile(`<[^>]+>`).ReplaceAllString(input, "")
	input = html.UnescapeString(input)
	input = regexp.MustCompile(`[ \t]+`).ReplaceAllString(input, " ")
	input = regexp.MustCompile(`\n[ \t]+`).ReplaceAllString(input, "\n")
	input = regexp.MustCompile(`\n{3,}`).ReplaceAllString(input, "\n\n")
	return strings.TrimSpace(input)
}

func cleanHTMLFragment(s string) string {
	return htmlToText(s)
}

func extractHTMLTitle(page string) string {
	m := regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`).FindStringSubmatch(page)
	if len(m) < 2 {
		return ""
	}
	return truncateString(cleanHTMLFragment(m[1]), 200)
}

func extractLinks(page string, base *url.URL) []string {
	matches := regexp.MustCompile(`(?is)<a[^>]+href=["']([^"']+)["']`).FindAllStringSubmatch(page, -1)
	out := make([]string, 0, minInt(20, len(matches)))
	seen := map[string]bool{}
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		href := strings.TrimSpace(html.UnescapeString(m[1]))
		if href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(strings.ToLower(href), "javascript:") {
			continue
		}
		u, err := url.Parse(href)
		if err != nil {
			continue
		}
		if base != nil {
			u = base.ResolveReference(u)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			continue
		}
		value := u.String()
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
		if len(out) >= 20 {
			break
		}
	}
	return out
}

func looksLikeHTML(s string) bool {
	lower := strings.ToLower(s[:minInt(len(s), 512)])
	return strings.Contains(lower, "<html") || strings.Contains(lower, "<body") || strings.Contains(lower, "<!doctype html")
}

func splitParagraphs(s string) []string {
	raw := regexp.MustCompile(`\n{2,}`).Split(s, -1)
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func keywordSet(s string) map[string]bool {
	words := regexp.MustCompile(`[A-Za-z0-9가-힣_./-]+`).FindAllString(strings.ToLower(s), -1)
	out := map[string]bool{}
	for _, w := range words {
		w = strings.Trim(w, ".,/\\-_")
		if len([]rune(w)) < 3 {
			continue
		}
		out[w] = true
	}
	return out
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func normalizePositiveLimit(value, fallback, maxValue int) int {
	if value <= 0 {
		value = fallback
	}
	if maxValue > 0 && value > maxValue {
		value = maxValue
	}
	return value
}

func truncateString(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "\n... (truncated)"
}

func limitStrings(in []string, limit int) []string {
	if limit <= 0 || len(in) <= limit {
		return in
	}
	return in[:limit]
}

func appendUniqueStrings(base []string, values ...string) []string {
	seen := map[string]bool{}
	for _, value := range base {
		seen[value] = true
	}
	for _, value := range values {
		if strings.TrimSpace(value) == "" || seen[value] {
			continue
		}
		seen[value] = true
		base = append(base, value)
	}
	return base
}

func browserUserAgent() string {
	return "Mozilla/5.0 (compatible; AniClew/1.0; +https://github.com/aniclew/aniclew)"
}

func coalesceString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
