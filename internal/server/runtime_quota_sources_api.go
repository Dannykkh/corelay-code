package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aniclew/aniclew/internal/config"
	"github.com/aniclew/aniclew/internal/runtimeplane"
)

type runtimeQuotaSourcesResponse struct {
	Sources []runtimeQuotaCollectorInfo `json:"sources"`
}

type runtimeQuotaSourceTestResponse struct {
	OK           bool                      `json:"ok"`
	Source       runtimeQuotaCollectorInfo `json:"source"`
	AccountCount int                       `json:"accountCount"`
	Accounts     []runtimeAccountInfo      `json:"accounts"`
}

type runtimeQuotaSourceSampleResponse struct {
	OK     bool                      `json:"ok"`
	Path   string                    `json:"path"`
	Source runtimeQuotaCollectorInfo `json:"source"`
}

type runtimeQuotaSourcePatch struct {
	Name            *string            `json:"name,omitempty"`
	Type            *string            `json:"type,omitempty"`
	Path            *string            `json:"path,omitempty"`
	URL             *string            `json:"url,omitempty"`
	Headers         *map[string]string `json:"headers,omitempty"`
	IntervalSeconds *int               `json:"intervalSeconds,omitempty"`
	TimeoutSeconds  *int               `json:"timeoutSeconds,omitempty"`
	Disabled        *bool              `json:"disabled,omitempty"`
}

func (s *Server) handleRuntimeQuotaSources(w http.ResponseWriter, _ *http.Request) {
	cfg := config.Load()
	writeJSON(w, runtimeQuotaSourcesResponse{
		Sources: runtimeQuotaSourceInfosForAPI(cfg.RuntimeQuotaSources),
	})
}

func (s *Server) handleRuntimeQuotaSourceCreate(w http.ResponseWriter, r *http.Request) {
	var source config.RuntimeQuotaSource
	if err := json.NewDecoder(r.Body).Decode(&source); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	normalized, err := normalizeRuntimeQuotaSourceConfig(source)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	cfg := config.Load()
	cfg.RuntimeQuotaSources = append(cfg.RuntimeQuotaSources, normalized)
	if err := config.Save(cfg); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	count := s.SetRuntimeQuotaCollectors(cfg.RuntimeQuotaSources)
	writeJSON(w, map[string]any{
		"ok":             true,
		"collectorCount": count,
		"sources":        runtimeQuotaSourceInfosForAPI(cfg.RuntimeQuotaSources),
	})
}

func (s *Server) handleRuntimeQuotaSourceSample(w http.ResponseWriter, _ *http.Request) {
	source, err := s.writeRuntimeQuotaSourceSample()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, runtimeQuotaSourceSampleResponse{
		OK:     true,
		Path:   source.Path,
		Source: runtimeQuotaCollectorInfos([]config.RuntimeQuotaSource{source})[0],
	})
}

func (s *Server) handleRuntimeQuotaSourceTest(w http.ResponseWriter, r *http.Request) {
	var source config.RuntimeQuotaSource
	if err := json.NewDecoder(r.Body).Decode(&source); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}
	normalized, err := normalizeRuntimeQuotaSourceConfig(source)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeRuntimeQuotaSourceTestResponse(w, normalized)
}

func (s *Server) handleRuntimeQuotaSourceTestSaved(w http.ResponseWriter, r *http.Request) {
	idx, ok := runtimeQuotaSourceIndex(w, r)
	if !ok {
		return
	}
	cfg := config.Load()
	if idx < 0 || idx >= len(cfg.RuntimeQuotaSources) {
		writeError(w, http.StatusNotFound, "quota source not found")
		return
	}
	source := cfg.RuntimeQuotaSources[idx]
	source.Disabled = false
	normalized, err := normalizeRuntimeQuotaSourceConfig(source)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeRuntimeQuotaSourceTestResponse(w, normalized)
}

func (s *Server) handleRuntimeQuotaSourcePatch(w http.ResponseWriter, r *http.Request) {
	idx, ok := runtimeQuotaSourceIndex(w, r)
	if !ok {
		return
	}
	var patch runtimeQuotaSourcePatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	cfg := config.Load()
	if idx < 0 || idx >= len(cfg.RuntimeQuotaSources) {
		writeError(w, http.StatusNotFound, "quota source not found")
		return
	}

	source := cfg.RuntimeQuotaSources[idx]
	applyRuntimeQuotaSourcePatch(&source, patch)
	normalized, err := normalizeRuntimeQuotaSourceConfig(source)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg.RuntimeQuotaSources[idx] = normalized
	if err := config.Save(cfg); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	count := s.SetRuntimeQuotaCollectors(cfg.RuntimeQuotaSources)
	writeJSON(w, map[string]any{
		"ok":             true,
		"collectorCount": count,
		"sources":        runtimeQuotaSourceInfosForAPI(cfg.RuntimeQuotaSources),
	})
}

func (s *Server) handleRuntimeQuotaSourceDelete(w http.ResponseWriter, r *http.Request) {
	idx, ok := runtimeQuotaSourceIndex(w, r)
	if !ok {
		return
	}
	cfg := config.Load()
	if idx < 0 || idx >= len(cfg.RuntimeQuotaSources) {
		writeError(w, http.StatusNotFound, "quota source not found")
		return
	}

	cfg.RuntimeQuotaSources = append(cfg.RuntimeQuotaSources[:idx], cfg.RuntimeQuotaSources[idx+1:]...)
	if err := config.Save(cfg); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	count := s.SetRuntimeQuotaCollectors(cfg.RuntimeQuotaSources)
	writeJSON(w, map[string]any{
		"ok":             true,
		"collectorCount": count,
		"sources":        runtimeQuotaSourceInfosForAPI(cfg.RuntimeQuotaSources),
	})
}

func runtimeQuotaSourceInfosForAPI(sources []config.RuntimeQuotaSource) []runtimeQuotaCollectorInfo {
	infos := runtimeQuotaCollectorInfos(sources)
	if infos == nil {
		return []runtimeQuotaCollectorInfo{}
	}
	return infos
}

func writeRuntimeQuotaSourceTestResponse(w http.ResponseWriter, source config.RuntimeQuotaSource) {
	accounts, err := testRuntimeQuotaSource(source)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, runtimeQuotaSourceTestResponse{
		OK:           true,
		Source:       runtimeQuotaCollectorInfos([]config.RuntimeQuotaSource{source})[0],
		AccountCount: len(accounts),
		Accounts:     runtimeAccountInfos(accounts),
	})
}

func testRuntimeQuotaSource(source config.RuntimeQuotaSource) ([]runtimeplane.AccountState, error) {
	source.Disabled = false
	store := runtimeplane.NewTelemetryStore()
	collector := runtimeplane.NewQuotaCollector(store, runtimeQuotaSourcesFromConfig([]config.RuntimeQuotaSource{source}))
	now := time.Now().UTC()
	reports := collector.CollectOnce(context.Background(), now)
	if len(reports) == 0 {
		return nil, fmt.Errorf("quota source is disabled or unsupported")
	}
	if reports[0].Error != "" {
		return nil, fmt.Errorf("%s", reports[0].Error)
	}
	return store.Merge(nil, now), nil
}

func (s *Server) writeRuntimeQuotaSourceSample() (config.RuntimeQuotaSource, error) {
	s.mu.RLock()
	providerName := ""
	if s.activeProvider != nil {
		providerName = strings.TrimSpace(s.activeProvider.Name())
	}
	s.mu.RUnlock()
	if providerName == "" {
		providerName = "anthropic"
	}
	group := runtimeplane.ClassifyProviderGroup(providerName)
	now := time.Now().UTC()

	path := filepath.Join(filepath.Dir(config.ConfigPath()), "runtime-quota-sample.json")
	type sampleWindow struct {
		Used    int    `json:"used"`
		Limit   int    `json:"limit"`
		ResetAt string `json:"resetAt"`
	}
	type sampleAccount struct {
		Provider   string       `json:"provider"`
		Group      string       `json:"group"`
		FiveHour   sampleWindow `json:"fiveHour"`
		SevenDay   sampleWindow `json:"sevenDay"`
		AllowStale bool         `json:"allowStale,omitempty"`
	}
	payload := struct {
		GeneratedAt string          `json:"generatedAt"`
		Accounts    []sampleAccount `json:"accounts"`
	}{
		GeneratedAt: now.Format(time.RFC3339),
		Accounts: []sampleAccount{{
			Provider: providerName,
			Group:    string(group),
			FiveHour: sampleWindow{
				Limit:   100,
				ResetAt: now.Add(5 * time.Hour).Format(time.RFC3339),
			},
			SevenDay: sampleWindow{
				Limit:   1000,
				ResetAt: now.Add(7 * 24 * time.Hour).Format(time.RFC3339),
			},
		}},
	}

	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return config.RuntimeQuotaSource{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return config.RuntimeQuotaSource{}, err
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return config.RuntimeQuotaSource{}, err
	}
	return config.RuntimeQuotaSource{
		Name:            "local quota sample",
		Type:            runtimeplane.QuotaSourceFile,
		Path:            path,
		IntervalSeconds: 60,
		TimeoutSeconds:  5,
	}, nil
}

func runtimeQuotaSourceIndex(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := strings.TrimSpace(r.PathValue("index"))
	idx, err := strconv.Atoi(raw)
	if err != nil || idx < 0 {
		writeError(w, http.StatusBadRequest, "invalid quota source index")
		return 0, false
	}
	return idx, true
}

func applyRuntimeQuotaSourcePatch(source *config.RuntimeQuotaSource, patch runtimeQuotaSourcePatch) {
	if patch.Name != nil {
		source.Name = *patch.Name
	}
	if patch.Type != nil {
		source.Type = *patch.Type
	}
	if patch.Path != nil {
		source.Path = *patch.Path
	}
	if patch.URL != nil {
		source.URL = *patch.URL
	}
	if patch.Headers != nil {
		source.Headers = *patch.Headers
	}
	if patch.IntervalSeconds != nil {
		source.IntervalSeconds = *patch.IntervalSeconds
	}
	if patch.TimeoutSeconds != nil {
		source.TimeoutSeconds = *patch.TimeoutSeconds
	}
	if patch.Disabled != nil {
		source.Disabled = *patch.Disabled
	}
}

func normalizeRuntimeQuotaSourceConfig(source config.RuntimeQuotaSource) (config.RuntimeQuotaSource, error) {
	source.Name = strings.TrimSpace(source.Name)
	source.Type = strings.ToLower(strings.TrimSpace(source.Type))
	source.Path = strings.TrimSpace(source.Path)
	source.URL = strings.TrimSpace(source.URL)
	source.Headers = cleanRuntimeQuotaHeaders(source.Headers)

	switch source.Type {
	case runtimeplane.QuotaSourceFile:
		if source.Path == "" {
			return source, fmt.Errorf("path required for file quota source")
		}
		source.URL = ""
		source.Headers = nil
	case runtimeplane.QuotaSourceHTTP:
		if source.URL == "" {
			return source, fmt.Errorf("url required for http quota source")
		}
		source.Path = ""
	default:
		return source, fmt.Errorf("unsupported quota source type %q", source.Type)
	}

	if source.IntervalSeconds < 0 {
		return source, fmt.Errorf("intervalSeconds must be non-negative")
	}
	if source.TimeoutSeconds < 0 {
		return source, fmt.Errorf("timeoutSeconds must be non-negative")
	}
	return source, nil
}

func cleanRuntimeQuotaHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	out := make(map[string]string, len(headers))
	for key, value := range headers {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" || value == "" {
			continue
		}
		out[key] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func runtimeQuotaHeaderNames(headers map[string]string) []string {
	if len(headers) == 0 {
		return nil
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
