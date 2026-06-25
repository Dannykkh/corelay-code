package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

type webSearchOptions struct {
	Query      string
	MaxResults int
	Sort       string
	Recency    string
	DateFrom   string
	DateTo     string
	SearchedAt time.Time
}

type webSearchProvider interface {
	Name() string
	Configured() bool
	Search(context.Context, webSearchOptions) ([]webSearchResult, error)
}

type duckDuckGoSearchProvider struct{}
type ollamaSearchProvider struct{}
type googleSearchProvider struct{}
type naverWebSearchProvider struct{}
type naverBlogSearchProvider struct{}
type bingSearchProvider struct{}
type yahooSearchProvider struct{}

func searchWebWithProviders(ctx context.Context, args webSearchArgs) ([]webSearchResult, string, string, error) {
	opts := webSearchOptions{
		Query:      strings.TrimSpace(args.Query),
		MaxResults: normalizePositiveLimit(args.MaxResults, defaultSearchResults, maxSearchResults),
		Sort:       normalizeSearchSort(args.Sort),
		Recency:    normalizeSearchRecency(args.Recency),
		DateFrom:   strings.TrimSpace(args.DateFrom),
		DateTo:     strings.TrimSpace(args.DateTo),
		SearchedAt: time.Now(),
	}

	providers, label, selectionNotes, err := selectSearchProviders(args)
	if err != nil {
		return nil, label, strings.Join(selectionNotes, "; "), err
	}

	byProvider := map[string][]webSearchResult{}
	var notes []string
	notes = append(notes, selectionNotes...)
	for _, provider := range providers {
		results, perr := provider.Search(ctx, opts)
		if perr != nil {
			notes = append(notes, fmt.Sprintf("%s failed: %v", provider.Name(), perr))
			continue
		}
		if len(results) == 0 {
			notes = append(notes, provider.Name()+" returned no results")
			continue
		}
		byProvider[provider.Name()] = results
	}

	results := fuseSearchResults(opts, byProvider)
	if len(results) == 0 {
		if len(notes) == 0 {
			notes = append(notes, "no search results returned")
		}
		return nil, label, strings.Join(notes, "; "), fmt.Errorf("%s", strings.Join(notes, "; "))
	}
	return results, label, strings.Join(notes, "; "), nil
}

func selectSearchProviders(args webSearchArgs) ([]webSearchProvider, string, []string, error) {
	registry := searchProviderRegistry()
	names := explicitSearchProviderNames(args)
	label := "multi"
	if len(names) == 0 {
		names = defaultSearchProviderNames()
		label = "multi"
	} else if len(names) == 1 {
		label = names[0]
	}

	var selected []webSearchProvider
	var notes []string
	for _, raw := range names {
		name := normalizeSearchProviderName(raw)
		if name == "" {
			continue
		}
		if name == "naver" {
			namesToAdd := []string{"naver-web", "naver-blog"}
			for _, n := range namesToAdd {
				p, ok := registry[n]
				if !ok {
					continue
				}
				if !p.Configured() {
					notes = append(notes, n+" is not configured")
					continue
				}
				selected = append(selected, p)
			}
			continue
		}
		p, ok := registry[name]
		if !ok {
			return nil, label, notes, fmt.Errorf("unsupported web search provider %q", raw)
		}
		if !p.Configured() {
			return nil, label, notes, fmt.Errorf("%s search provider is not configured", p.Name())
		}
		selected = append(selected, p)
	}
	selected = dedupeSearchProviders(selected)
	if len(selected) == 0 {
		return nil, label, notes, fmt.Errorf("no configured web search providers selected")
	}
	if len(selected) == 1 {
		label = selected[0].Name()
	}
	return selected, label, notes, nil
}

func explicitSearchProviderNames(args webSearchArgs) []string {
	if len(args.Providers) > 0 {
		return args.Providers
	}
	provider := strings.TrimSpace(args.Provider)
	if provider == "" || strings.EqualFold(provider, "auto") || strings.EqualFold(provider, "multi") {
		return nil
	}
	if strings.EqualFold(provider, "all") {
		return appendUniqueStrings(defaultSearchProviderNames(), "yahoo")
	}
	return splitProviderList(provider)
}

func defaultSearchProviderNames() []string {
	names := []string{"duckduckgo"}
	registry := searchProviderRegistry()
	for _, name := range []string{"google", "naver-web", "naver-blog", "bing"} {
		if registry[name].Configured() {
			names = append(names, name)
		}
	}
	if truthyEnv("ANICLEW_ENABLE_YAHOO_SEARCH") {
		names = append(names, "yahoo")
	}
	return names
}

func searchProviderRegistry() map[string]webSearchProvider {
	return map[string]webSearchProvider{
		"duckduckgo": duckDuckGoSearchProvider{},
		"ddg":        duckDuckGoSearchProvider{},
		"ollama":     ollamaSearchProvider{},
		"google":     googleSearchProvider{},
		"naver-web":  naverWebSearchProvider{},
		"naver-blog": naverBlogSearchProvider{},
		"bing":       bingSearchProvider{},
		"yahoo":      yahooSearchProvider{},
	}
}

func dedupeSearchProviders(in []webSearchProvider) []webSearchProvider {
	seen := map[string]bool{}
	out := make([]webSearchProvider, 0, len(in))
	for _, p := range in {
		if seen[p.Name()] {
			continue
		}
		seen[p.Name()] = true
		out = append(out, p)
	}
	return out
}

func splitProviderList(value string) []string {
	fields := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ';' || r == '|' || r == ' '
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if strings.TrimSpace(f) != "" {
			out = append(out, f)
		}
	}
	return out
}

func normalizeSearchProviderName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "auto", "multi":
		return ""
	case "duck", "ddg":
		return "duckduckgo"
	case "naverweb", "naver-webkr", "webkr":
		return "naver-web"
	case "naverblog", "naver-blogsearch":
		return "naver-blog"
	default:
		return strings.ToLower(strings.TrimSpace(name))
	}
}

func (duckDuckGoSearchProvider) Name() string     { return "duckduckgo" }
func (duckDuckGoSearchProvider) Configured() bool { return true }
func (duckDuckGoSearchProvider) Search(ctx context.Context, opts webSearchOptions) ([]webSearchResult, error) {
	return duckDuckGoSearch(ctx, opts)
}

func (ollamaSearchProvider) Name() string { return "ollama" }
func (ollamaSearchProvider) Configured() bool {
	return strings.TrimSpace(os.Getenv("OLLAMA_API_KEY")) != ""
}
func (ollamaSearchProvider) Search(ctx context.Context, opts webSearchOptions) ([]webSearchResult, error) {
	return ollamaWebSearch(ctx, opts.Query, opts.MaxResults)
}

func (googleSearchProvider) Name() string { return "google" }
func (googleSearchProvider) Configured() bool {
	return googleSearchAPIKey() != "" && googleSearchEngineID() != ""
}
func (googleSearchProvider) Search(ctx context.Context, opts webSearchOptions) ([]webSearchResult, error) {
	base := coalesceString(os.Getenv("ANICLEW_GOOGLE_SEARCH_URL"), "https://www.googleapis.com/customsearch/v1")
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("key", googleSearchAPIKey())
	q.Set("cx", googleSearchEngineID())
	q.Set("q", opts.Query)
	q.Set("num", fmt.Sprintf("%d", minInt(opts.MaxResults, 10)))
	if restrict := googleDateRestrict(opts.Recency); restrict != "" {
		q.Set("dateRestrict", restrict)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", browserUserAgent())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
		return nil, fmt.Errorf("google HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var decoded struct {
		Items []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Snippet string `json:"snippet"`
			PageMap struct {
				MetaTags []map[string]string `json:"metatags"`
			} `json:"pagemap"`
		} `json:"items"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2*1024*1024)).Decode(&decoded); err != nil {
		return nil, err
	}
	results := make([]webSearchResult, 0, len(decoded.Items))
	for i, item := range decoded.Items {
		if strings.TrimSpace(item.Link) == "" {
			continue
		}
		published, updated := datesFromMetaTags(item.PageMap.MetaTags)
		results = append(results, webSearchResult{
			Rank:        i + 1,
			Title:       strings.TrimSpace(item.Title),
			URL:         strings.TrimSpace(item.Link),
			Snippet:     strings.TrimSpace(item.Snippet),
			Source:      "google",
			PublishedAt: published,
			UpdatedAt:   updated,
		})
	}
	return results, nil
}

func (naverWebSearchProvider) Name() string { return "naver-web" }
func (naverWebSearchProvider) Configured() bool {
	return naverClientID() != "" && naverClientSecret() != ""
}
func (naverWebSearchProvider) Search(ctx context.Context, opts webSearchOptions) ([]webSearchResult, error) {
	return naverSearch(ctx, opts, "webkr")
}

func (naverBlogSearchProvider) Name() string { return "naver-blog" }
func (naverBlogSearchProvider) Configured() bool {
	return naverClientID() != "" && naverClientSecret() != ""
}
func (naverBlogSearchProvider) Search(ctx context.Context, opts webSearchOptions) ([]webSearchResult, error) {
	return naverSearch(ctx, opts, "blog")
}

func naverSearch(ctx context.Context, opts webSearchOptions, kind string) ([]webSearchResult, error) {
	base := coalesceString(os.Getenv("ANICLEW_NAVER_SEARCH_URL"), "https://openapi.naver.com/v1/search/"+kind+".json")
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("query", opts.Query)
	q.Set("display", fmt.Sprintf("%d", minInt(opts.MaxResults, 100)))
	q.Set("sort", naverSort(opts.Sort, opts.Recency))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Naver-Client-Id", naverClientID())
	req.Header.Set("X-Naver-Client-Secret", naverClientSecret())
	req.Header.Set("User-Agent", browserUserAgent())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
		return nil, fmt.Errorf("naver %s HTTP %d: %s", kind, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var decoded struct {
		Items []struct {
			Title       string `json:"title"`
			Link        string `json:"link"`
			Description string `json:"description"`
			PostDate    string `json:"postdate"`
		} `json:"items"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2*1024*1024)).Decode(&decoded); err != nil {
		return nil, err
	}
	source := "naver-" + kind
	if kind == "webkr" {
		source = "naver-web"
	}
	results := make([]webSearchResult, 0, len(decoded.Items))
	for i, item := range decoded.Items {
		published := normalizeDateString(item.PostDate)
		results = append(results, webSearchResult{
			Rank:        i + 1,
			Title:       cleanHTMLFragment(item.Title),
			URL:         strings.TrimSpace(item.Link),
			Snippet:     cleanHTMLFragment(item.Description),
			Source:      source,
			PublishedAt: published,
		})
	}
	return results, nil
}

func (bingSearchProvider) Name() string { return "bing" }
func (bingSearchProvider) Configured() bool {
	return strings.TrimSpace(os.Getenv("BING_SEARCH_API_KEY")) != ""
}
func (bingSearchProvider) Search(ctx context.Context, opts webSearchOptions) ([]webSearchResult, error) {
	base := coalesceString(os.Getenv("BING_SEARCH_ENDPOINT"), "https://api.bing.microsoft.com/v7.0/search")
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("q", opts.Query)
	q.Set("count", fmt.Sprintf("%d", minInt(opts.MaxResults, 50)))
	if freshness := bingFreshness(opts.Recency); freshness != "" {
		q.Set("freshness", freshness)
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Ocp-Apim-Subscription-Key", strings.TrimSpace(os.Getenv("BING_SEARCH_API_KEY")))
	req.Header.Set("User-Agent", browserUserAgent())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
		return nil, fmt.Errorf("bing HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var decoded struct {
		WebPages struct {
			Value []struct {
				Name            string `json:"name"`
				URL             string `json:"url"`
				Snippet         string `json:"snippet"`
				DatePublished   string `json:"datePublished"`
				DateLastCrawled string `json:"dateLastCrawled"`
			} `json:"value"`
		} `json:"webPages"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2*1024*1024)).Decode(&decoded); err != nil {
		return nil, err
	}
	results := make([]webSearchResult, 0, len(decoded.WebPages.Value))
	for i, item := range decoded.WebPages.Value {
		results = append(results, webSearchResult{
			Rank:        i + 1,
			Title:       strings.TrimSpace(item.Name),
			URL:         strings.TrimSpace(item.URL),
			Snippet:     strings.TrimSpace(item.Snippet),
			Source:      "bing",
			PublishedAt: normalizeDateString(item.DatePublished),
			UpdatedAt:   normalizeDateString(item.DateLastCrawled),
		})
	}
	return results, nil
}

func (yahooSearchProvider) Name() string     { return "yahoo" }
func (yahooSearchProvider) Configured() bool { return true }
func (yahooSearchProvider) Search(ctx context.Context, opts webSearchOptions) ([]webSearchResult, error) {
	base := coalesceString(os.Getenv("ANICLEW_YAHOO_SEARCH_URL"), "https://search.yahoo.com/search")
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("p", opts.Query)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", browserUserAgent())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("yahoo HTTP %d: %s", resp.StatusCode, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return nil, err
	}
	return extractYahooResults(string(body), opts.MaxResults), nil
}

func fuseSearchResults(opts webSearchOptions, byProvider map[string][]webSearchResult) []webSearchResult {
	type fusedResult struct {
		result    webSearchResult
		bestRank  int
		providers []string
	}
	fused := map[string]*fusedResult{}
	for _, provider := range orderedSearchProviderNames(byProvider) {
		results := byProvider[provider]
		for _, raw := range results {
			if strings.TrimSpace(raw.URL) == "" {
				continue
			}
			r := enrichSearchResult(raw, provider, opts)
			if !resultWithinDateWindow(r, opts) {
				continue
			}
			key := canonicalSearchURL(r.URL)
			if key == "" {
				key = r.URL
			}
			if existing, ok := fused[key]; ok {
				existing.providers = appendUniqueStrings(existing.providers, provider)
				existing.result.Providers = existing.providers
				existing.result.Source = strings.Join(existing.providers, "+")
				if r.Rank > 0 && (existing.bestRank == 0 || r.Rank < existing.bestRank) {
					existing.bestRank = r.Rank
				}
				if len([]rune(r.Snippet)) > len([]rune(existing.result.Snippet)) {
					existing.result.Snippet = r.Snippet
				}
				if existing.result.PublishedAt == "" {
					existing.result.PublishedAt = r.PublishedAt
				}
				if existing.result.UpdatedAt == "" {
					existing.result.UpdatedAt = r.UpdatedAt
				}
				continue
			}
			providers := []string{provider}
			r.Providers = providers
			r.Source = provider
			fused[key] = &fusedResult{result: r, bestRank: r.Rank, providers: providers}
		}
	}

	results := make([]webSearchResult, 0, len(fused))
	for _, f := range fused {
		r := f.result
		r.RelevanceScore = relevanceScore(opts.Query, r.Title+" "+r.Snippet+" "+r.URL)
		r.FreshnessScore = freshnessScore(r.PublishedAt, r.UpdatedAt, opts)
		r.AuthorityScore = authorityScore(r.URL)
		providerScore := math.Min(1, float64(len(f.providers))/3)
		r.Score = combinedSearchScore(r, providerScore, opts)
		results = append(results, r)
	}
	sort.SliceStable(results, func(i, j int) bool {
		if wantsLatest(opts) {
			ti, iok := bestResultTime(results[i])
			tj, jok := bestResultTime(results[j])
			if iok && jok && !ti.Equal(tj) {
				return ti.After(tj)
			}
			if iok != jok {
				return iok
			}
		}
		if results[i].Score == results[j].Score {
			return results[i].URL < results[j].URL
		}
		return results[i].Score > results[j].Score
	})
	if len(results) > opts.MaxResults {
		results = results[:opts.MaxResults]
	}
	for i := range results {
		results[i].Rank = i + 1
	}
	return results
}

func orderedSearchProviderNames(byProvider map[string][]webSearchResult) []string {
	names := make([]string, 0, len(byProvider))
	for name := range byProvider {
		names = append(names, name)
	}
	sort.SliceStable(names, func(i, j int) bool {
		ri := searchProviderOrderRank(names[i])
		rj := searchProviderOrderRank(names[j])
		if ri != rj {
			return ri < rj
		}
		return names[i] < names[j]
	})
	return names
}

func searchProviderOrderRank(name string) int {
	switch name {
	case "duckduckgo":
		return 10
	case "google":
		return 20
	case "naver-web":
		return 30
	case "naver-blog":
		return 40
	case "bing":
		return 50
	case "yahoo":
		return 60
	case "ollama":
		return 70
	default:
		return 100
	}
}

func enrichSearchResult(raw webSearchResult, provider string, opts webSearchOptions) webSearchResult {
	raw.Title = strings.TrimSpace(raw.Title)
	raw.URL = strings.TrimSpace(raw.URL)
	raw.Snippet = strings.TrimSpace(raw.Snippet)
	raw.Source = coalesceString(raw.Source, provider)
	raw.SearchedAt = opts.SearchedAt.Format(time.RFC3339)
	raw.PublishedAt = normalizeDateString(raw.PublishedAt)
	raw.UpdatedAt = normalizeDateString(raw.UpdatedAt)
	if raw.PublishedAt == "" {
		raw.PublishedAt = normalizeDateString(extractFirstDate(raw.Title + " " + raw.Snippet))
	}
	raw.RelevanceScore = relevanceScore(opts.Query, raw.Title+" "+raw.Snippet+" "+raw.URL)
	raw.FreshnessScore = freshnessScore(raw.PublishedAt, raw.UpdatedAt, opts)
	raw.AuthorityScore = authorityScore(raw.URL)
	return raw
}

func combinedSearchScore(r webSearchResult, providerScore float64, opts webSearchOptions) float64 {
	freshnessWeight := 0.20
	if wantsLatest(opts) {
		freshnessWeight = 0.35
	}
	score := 0.45*r.RelevanceScore + freshnessWeight*r.FreshnessScore + 0.25*r.AuthorityScore + 0.10*providerScore
	if wantsLatest(opts) && r.PublishedAt == "" && r.UpdatedAt == "" {
		score -= 0.15
	}
	return math.Max(0, math.Min(1, score))
}

func relevanceScore(query, text string) float64 {
	keywords := keywordSet(query)
	if len(keywords) == 0 {
		return 0.5
	}
	lower := strings.ToLower(text)
	hits := 0
	for kw := range keywords {
		if strings.Contains(lower, kw) {
			hits++
		}
	}
	return math.Min(1, float64(hits)/float64(len(keywords)))
}

func freshnessScore(published, updated string, opts webSearchOptions) float64 {
	dateText := coalesceString(updated, published)
	t, ok := parseSearchDate(dateText)
	if !ok {
		if wantsLatest(opts) {
			return 0.10
		}
		return 0.40
	}
	age := opts.SearchedAt.Sub(t)
	if age < 0 {
		age = 0
	}
	days := age.Hours() / 24
	switch {
	case days <= 1:
		return 1.0
	case days <= 7:
		return 0.90
	case days <= 31:
		return 0.75
	case days <= 180:
		return 0.55
	case days <= 365:
		return 0.40
	default:
		return 0.20
	}
}

func authorityScore(rawURL string) float64 {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0.3
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "":
		return 0.3
	case strings.HasSuffix(host, ".gov") || strings.Contains(host, ".go.kr"):
		return 0.95
	case strings.HasSuffix(host, ".edu") || strings.Contains(host, "ac.kr"):
		return 0.90
	case strings.Contains(host, "docs.") || strings.Contains(u.Path, "/docs") || strings.Contains(u.Path, "/documentation"):
		return 0.85
	case strings.Contains(host, "github.com") || strings.Contains(host, "developer.") || strings.Contains(host, "developers."):
		return 0.80
	case strings.Contains(host, "blog.") || strings.Contains(host, "medium.com") || strings.Contains(host, "tistory.com") || strings.Contains(host, "blog.naver.com"):
		return 0.55
	default:
		return 0.65
	}
}

func canonicalSearchURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return strings.TrimSpace(raw)
	}
	u.Fragment = ""
	q := u.Query()
	for key := range q {
		lower := strings.ToLower(key)
		if strings.HasPrefix(lower, "utm_") || lower == "fbclid" || lower == "gclid" || lower == "igshid" {
			q.Del(key)
		}
	}
	u.RawQuery = q.Encode()
	u.Host = strings.ToLower(u.Host)
	return strings.TrimRight(u.String(), "/")
}

func wantsLatest(opts webSearchOptions) bool {
	return opts.Sort == "latest" || opts.Recency != "" || opts.DateFrom != "" || opts.DateTo != ""
}

func resultWithinDateWindow(r webSearchResult, opts webSearchOptions) bool {
	t, ok := bestResultTime(r)
	if !ok {
		return true
	}
	if from, ok := parseSearchDate(opts.DateFrom); ok && t.Before(from) {
		return false
	}
	if to, ok := parseSearchDate(opts.DateTo); ok && t.After(to.Add(24*time.Hour-time.Nanosecond)) {
		return false
	}
	return true
}

func bestResultTime(r webSearchResult) (time.Time, bool) {
	if t, ok := parseSearchDate(r.UpdatedAt); ok {
		return t, true
	}
	return parseSearchDate(r.PublishedAt)
}

func normalizeSearchSort(sortValue string) string {
	switch strings.ToLower(strings.TrimSpace(sortValue)) {
	case "latest", "date", "recent", "newest":
		return "latest"
	default:
		return "relevance"
	}
}

func normalizeSearchRecency(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "d", "day", "24h", "today":
		return "day"
	case "w", "week", "7d":
		return "week"
	case "m", "month", "30d":
		return "month"
	case "y", "year", "365d":
		return "year"
	default:
		return ""
	}
}

func googleDateRestrict(recency string) string {
	switch normalizeSearchRecency(recency) {
	case "day":
		return "d1"
	case "week":
		return "w1"
	case "month":
		return "m1"
	case "year":
		return "y1"
	default:
		return ""
	}
}

func duckDuckGoDateFilter(recency string) string {
	switch normalizeSearchRecency(recency) {
	case "day":
		return "d"
	case "week":
		return "w"
	case "month":
		return "m"
	case "year":
		return "y"
	default:
		return ""
	}
}

func bingFreshness(recency string) string {
	switch normalizeSearchRecency(recency) {
	case "day":
		return "Day"
	case "week":
		return "Week"
	case "month":
		return "Month"
	default:
		return ""
	}
}

func naverSort(sortValue, recency string) string {
	if normalizeSearchSort(sortValue) == "latest" || normalizeSearchRecency(recency) != "" {
		return "date"
	}
	return "sim"
}

func googleSearchAPIKey() string {
	return coalesceString(os.Getenv("GOOGLE_SEARCH_API_KEY"), os.Getenv("GOOGLE_API_KEY"))
}

func googleSearchEngineID() string {
	return coalesceString(os.Getenv("GOOGLE_SEARCH_ENGINE_ID"), os.Getenv("GOOGLE_CSE_ID"))
}

func naverClientID() string {
	return coalesceString(os.Getenv("NAVER_CLIENT_ID"), os.Getenv("X_NAVER_CLIENT_ID"))
}

func naverClientSecret() string {
	return coalesceString(os.Getenv("NAVER_CLIENT_SECRET"), os.Getenv("X_NAVER_CLIENT_SECRET"))
}

func truthyEnv(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func datesFromMetaTags(metaTags []map[string]string) (string, string) {
	for _, tags := range metaTags {
		published := firstMetaDate(tags, "article:published_time", "datepublished", "date", "pubdate")
		updated := firstMetaDate(tags, "article:modified_time", "datemodified", "lastmod")
		if published != "" || updated != "" {
			return published, updated
		}
	}
	return "", ""
}

func firstMetaDate(tags map[string]string, keys ...string) string {
	for _, wanted := range keys {
		normalizedWanted := strings.ToLower(strings.ReplaceAll(wanted, "_", ""))
		for key, value := range tags {
			normalized := strings.ToLower(strings.ReplaceAll(key, "_", ""))
			if normalized == normalizedWanted {
				if date := normalizeDateString(value); date != "" {
					return date
				}
			}
		}
	}
	return ""
}

func normalizeDateString(value string) string {
	t, ok := parseSearchDate(value)
	if !ok {
		return ""
	}
	return t.Format("2006-01-02")
}

func extractFirstDate(text string) string {
	patterns := []string{
		`\b\d{4}[-./]\d{1,2}[-./]\d{1,2}\b`,
		`\b\d{8}\b`,
		`\b(?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)[a-z]*\s+\d{1,2},\s+\d{4}\b`,
	}
	for _, pattern := range patterns {
		match := regexp.MustCompile(`(?i)` + pattern).FindString(text)
		if match != "" {
			return match
		}
	}
	return ""
}

func parseSearchDate(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	if match := regexp.MustCompile(`\b(\d{4})[-./]\s*(\d{1,2})[-./]\s*(\d{1,2})\b`).FindStringSubmatch(value); len(match) == 4 {
		return parseDateParts(match[1], match[2], match[3])
	}
	if match := regexp.MustCompile(`\b(\d{4})(\d{2})(\d{2})\b`).FindStringSubmatch(value); len(match) == 4 {
		return parseDateParts(match[1], match[2], match[3])
	}
	layouts := []string{
		time.RFC3339,
		time.RFC1123,
		"2006-01-02",
		"2006/01/02",
		"2006.01.02",
		"Jan 2, 2006",
		"January 2, 2006",
		"2 Jan 2006",
		"02 Jan 2006",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, value); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func parseDateParts(year, month, day string) (time.Time, bool) {
	t, err := time.Parse("2006-1-2", year+"-"+month+"-"+day)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func extractYahooResults(page string, maxResults int) []webSearchResult {
	matches := regexp.MustCompile(`(?is)<a[^>]+href=["']([^"']+)["'][^>]*>(.*?)</a>`).FindAllStringSubmatch(page, -1)
	results := make([]webSearchResult, 0, maxResults)
	seen := map[string]bool{}
	for _, match := range matches {
		if len(results) >= maxResults {
			break
		}
		target := normalizeYahooURL(html.UnescapeString(match[1]))
		if target == "" || seen[target] || isSearchEngineInternalURL(target, "yahoo") {
			continue
		}
		title := cleanHTMLFragment(match[2])
		if len([]rune(title)) < 3 {
			continue
		}
		seen[target] = true
		results = append(results, webSearchResult{
			Rank:   len(results) + 1,
			Title:  title,
			URL:    target,
			Source: "yahoo",
		})
	}
	return results
}

func normalizeYahooURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	}
	if strings.HasPrefix(raw, "/") {
		return ""
	}
	if decoded := regexp.MustCompile(`/RU=([^/]+)`).FindStringSubmatch(raw); len(decoded) == 2 {
		if value, err := url.QueryUnescape(decoded[1]); err == nil {
			raw = value
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.String()
}

func isSearchEngineInternalURL(raw, engine string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return true
	}
	host := strings.ToLower(u.Hostname())
	switch engine {
	case "yahoo":
		return strings.Contains(host, "yahoo.")
	default:
		return false
	}
}
