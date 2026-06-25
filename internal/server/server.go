package server

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aniclew/aniclew/internal/agent"
	apiPkg "github.com/aniclew/aniclew/internal/api"
	"github.com/aniclew/aniclew/internal/config"
	"github.com/aniclew/aniclew/internal/gateway"
	"github.com/aniclew/aniclew/internal/hooks"
	"github.com/aniclew/aniclew/internal/kairos"
	"github.com/aniclew/aniclew/internal/observability"
	"github.com/aniclew/aniclew/internal/providers"
	"github.com/aniclew/aniclew/internal/router"
	"github.com/aniclew/aniclew/internal/stream"
	"github.com/aniclew/aniclew/internal/types"
	"github.com/aniclew/aniclew/internal/workstream"
)

//go:embed dashboard.html
var dashboardHTML []byte

//go:embed all:webdist
var webFS embed.FS

type Server struct {
	mu             sync.RWMutex
	activeProvider types.Provider
	activeModel    string
	responseLang   string // "ko", "en", "ja", "zh", "auto"
	router         *router.Router
	daemon         *kairos.Daemon
	memory         *kairos.Memory
	abTester       *kairos.ABTester
	gw             *gateway.Gateway
	sessions       *agent.SessionStore
	tracker        *observability.Tracker
	feedback       *observability.FeedbackStore
	workDir        string // current workspace
	port           int
	loops          *agent.LoopRegistry
}

func (s *Server) SetTracker(t *observability.Tracker) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tracker = t
	if s.daemon != nil {
		s.daemon.SetTracker(t)
	}
}

func (s *Server) SetFeedback(f *observability.FeedbackStore) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.feedback = f
}

func (s *Server) SetResponseLang(lang string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responseLang = lang
}

func New(provider types.Provider, model string, port int) *Server {
	return &Server{
		activeProvider: provider,
		activeModel:    model,
		port:           port,
		// Concurrency cap for simultaneous agent loops across all
		// projects. 3 is the Wave-1 default — raise it if users report
		// contention, but a low cap keeps provider spend predictable.
		loops: agent.NewLoopRegistry(3),
	}
}

// Loops exposes the server's agent-loop registry so other subsystems (e.g.,
// graceful shutdown) can observe or cancel running loops. Returns nil if
// called before New, which never happens in practice.
func (s *Server) Loops() *agent.LoopRegistry {
	return s.loops
}

func (s *Server) SetProvider(p types.Provider, model string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activeProvider = p
	s.activeModel = model
}

func (s *Server) SetRouter(r *router.Router) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.router = r
}

func (s *Server) SetDaemon(d *kairos.Daemon) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d != nil && s.tracker != nil {
		d.SetTracker(s.tracker)
	}
	s.daemon = d
}

func (s *Server) GetDaemon() *kairos.Daemon {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.daemon
}

func (s *Server) SetMemory(m *kairos.Memory) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.memory = m
}

func (s *Server) SetABTester(t *kairos.ABTester) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.abTester = t
}

func (s *Server) SetGateway(g *gateway.Gateway) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gw = g
}

func (s *Server) SetSessionStore(ss *agent.SessionStore) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = ss
}

func (s *Server) SetWorkDir(dir string) {
	s.mu.Lock()
	s.workDir = dir
	mem := s.memory
	daemon := s.daemon
	s.mu.Unlock()

	// Switch KAIROS subsystems to the new project
	if mem != nil {
		mem.SwitchProject(dir)
	}
	if daemon != nil {
		daemon.SwitchProject(dir)
	}
}

func (s *Server) Start() error {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/messages", s.handleMessages)
	mux.HandleFunc("POST /messages", s.handleMessages)
	mux.HandleFunc("GET /dashboard", s.handleDashboard)

	// React SPA — serve static files from embedded webdist
	webSub, _ := fs.Sub(webFS, "webdist")
	webHandler := http.FileServer(http.FS(webSub))
	mux.HandleFunc("GET /app", func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = "/"
		webHandler.ServeHTTP(w, r)
	})
	mux.Handle("GET /assets/", webHandler)
	mux.Handle("GET /favicon.svg", webHandler)
	mux.Handle("GET /icons.svg", webHandler)
	mux.HandleFunc("GET /api/providers", s.handleListProviders)
	mux.HandleFunc("POST /api/providers/register", s.handleRegisterProvider)
	mux.HandleFunc("GET /api/ollama/models", s.handleOllamaModels)
	mux.HandleFunc("GET /api/config", s.handleGetConfig)

	// Projects
	mux.HandleFunc("GET /api/projects", s.handleListProjects)
	mux.HandleFunc("POST /api/projects", s.handleAddProject)
	mux.HandleFunc("DELETE /api/projects", s.handleDeleteProject)

	// Workspace / folder browsing
	mux.HandleFunc("GET /api/browse", s.handleBrowseFolder)
	mux.HandleFunc("PUT /api/workspace", s.handleSetWorkspace)
	mux.HandleFunc("GET /api/workspace", s.handleGetWorkspace)
	mux.HandleFunc("GET /api/file", s.handleReadFile)
	mux.HandleFunc("POST /api/file/write", s.handleWriteFile)
	mux.HandleFunc("GET /api/tree", s.handleFileTree)
	mux.HandleFunc("PUT /api/config", s.handleSetConfig)
	mux.HandleFunc("GET /api/routes", s.handleGetRoutes)
	mux.HandleFunc("PUT /api/routes", s.handleSetRoute)
	mux.HandleFunc("GET /api/costs", s.handleGetCosts)
	// KAIROS daemon
	// Hooks & Permissions
	mux.HandleFunc("GET /api/hooks", s.handleListHooks)
	mux.HandleFunc("GET /api/permissions", s.handlePermissions)

	// Observability
	mux.HandleFunc("GET /api/traces", s.handleTraces)
	mux.HandleFunc("GET /api/run-traces", s.handleRunTraces)
	mux.HandleFunc("GET /api/metrics", s.handleMetrics)
	mux.HandleFunc("POST /api/feedback", s.handleAddFeedback)
	mux.HandleFunc("GET /api/feedback", s.handleFeedbackStats)

	mux.HandleFunc("GET /api/kairos", s.handleKairosStatus)
	mux.HandleFunc("POST /api/kairos/start", s.handleKairosStart)
	mux.HandleFunc("POST /api/kairos/stop", s.handleKairosStop)
	mux.HandleFunc("GET /api/kairos/tasks", s.handleKairosTasks)
	mux.HandleFunc("POST /api/kairos/tasks", s.handleKairosAddTask)
	mux.HandleFunc("DELETE /api/kairos/tasks", s.handleKairosRemoveTask)
	mux.HandleFunc("GET /api/kairos/logs", s.handleKairosLogs)
	mux.HandleFunc("PUT /api/kairos/autonomy", s.handleKairosAutonomy)
	mux.HandleFunc("GET /api/kairos/git", s.handleKairosGitStatus)
	mux.HandleFunc("GET /api/kairos/notifications", s.handleKairosNotifications)
	mux.HandleFunc("GET /api/kairos/notifications/stream", s.handleKairosSSE)
	mux.HandleFunc("PUT /api/kairos/webhook", s.handleKairosWebhook)

	// Memory (AutoDream)
	mux.HandleFunc("GET /api/memory", s.handleMemoryState)
	mux.HandleFunc("POST /api/memory", s.handleMemoryAdd)
	mux.HandleFunc("GET /api/memory/search", s.handleMemorySearch)
	mux.HandleFunc("POST /api/memory/dream", s.handleMemoryDream)

	// A/B Testing
	mux.HandleFunc("POST /api/ab-test", s.handleABTestRun)
	mux.HandleFunc("GET /api/ab-test", s.handleABTestResults)

	// PR Auto-Reviewer (GitHub Webhook)
	mux.HandleFunc("POST /api/webhook/github", s.handleGitHubWebhook)

	// Team Gateway
	mux.HandleFunc("GET /api/gateway/users", s.handleGatewayUsers)
	mux.HandleFunc("POST /api/gateway/users", s.handleGatewayAddUser)
	mux.HandleFunc("GET /api/gateway/audit", s.handleGatewayAudit)

	// Bridge (remote control)
	mux.HandleFunc("POST /api/bridge/session", s.handleBridgeCreate)
	mux.HandleFunc("POST /api/bridge/send", s.handleBridgeSend)
	mux.HandleFunc("GET /api/bridge/sessions", s.handleBridgeList)

	// Agent loop (coding agent)
	mux.HandleFunc("POST /api/ask", s.handleAskModel)
	mux.HandleFunc("POST /api/agent", s.handleAgentLoop)
	mux.HandleFunc("GET /api/agent/loops", s.handleAgentLoops)
	mux.HandleFunc("POST /api/agent/{sessionId}/cancel", s.handleAgentCancel)
	mux.HandleFunc("POST /api/chronos", s.handleChronos)
	mux.HandleFunc("POST /api/team", s.handleTeamExecute)
	mux.HandleFunc("GET /api/agent-types", s.handleAgentTypes)
	mux.HandleFunc("GET /api/worktrees", s.handleWorktrees)
	mux.HandleFunc("GET /api/plugins", s.handlePlugins)
	mux.HandleFunc("GET /api/rag", s.handleRAGSearch)
	mux.HandleFunc("POST /api/multi-model", s.handleMultiModel)

	// Image upload
	mux.HandleFunc("POST /api/upload", s.handleImageUpload)

	// Project context
	mux.HandleFunc("GET /api/context", s.handleProjectContext)
	mux.HandleFunc("GET /api/skills", s.handleSkillsList)
	mux.HandleFunc("GET /api/project", s.handleProjectDetect)
	mux.HandleFunc("PUT /api/skill-source", s.handleSetSkillSource)

	// Slash commands
	mux.HandleFunc("GET /api/commands", s.handleCommandsList)

	// Plan mode
	mux.HandleFunc("GET /api/plan", s.handlePlanGet)
	mux.HandleFunc("POST /api/plan/approve", s.handlePlanApprove)

	// Sub-agents
	mux.HandleFunc("POST /api/subagent/spawn", s.handleSubAgentSpawn)
	mux.HandleFunc("GET /api/subagent/tasks", s.handleSubAgentTasks)

	// MCP servers
	mux.HandleFunc("GET /api/mcp", s.handleMCPList)
	mux.HandleFunc("POST /api/mcp/connect", s.handleMCPConnect)
	mux.HandleFunc("POST /api/mcp/disconnect", s.handleMCPDisconnect)

	// Workspaces & Sessions
	mux.HandleFunc("GET /api/workspaces", s.handleWorkspaceList)
	mux.HandleFunc("GET /api/workstreams", s.handleWorkstreamList)
	mux.HandleFunc("POST /api/workstreams", s.handleWorkstreamCreate)
	mux.HandleFunc("GET /api/workstreams/{id}", s.handleWorkstreamGet)
	mux.HandleFunc("PATCH /api/workstreams/{id}", s.handleWorkstreamPatch)
	mux.HandleFunc("POST /api/workstreams/{id}/handoff", s.handleWorkstreamHandoff)
	mux.HandleFunc("GET /api/sessions", s.handleSessionList)
	mux.HandleFunc("POST /api/sessions", s.handleSessionSave)
	mux.HandleFunc("GET /api/sessions/{id}", s.handleSessionGet)
	mux.HandleFunc("DELETE /api/sessions/{id}", s.handleSessionDelete)
	mux.HandleFunc("PUT /api/sessions/{id}", s.handleSessionRename)

	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /", s.handleRoot)

	handler := corsMiddleware(authMiddleware(mux))
	// Default to loopback only — a dev box running AniClew should not be
	// reachable from the LAN by default (the agent has Bash/Write/Edit and
	// auth is optional). Set ANICLEW_BIND=0.0.0.0 to open to other hosts
	// (e.g. a server in a closed network used by other machines).
	host := strings.TrimSpace(os.Getenv("ANICLEW_BIND"))
	if host == "" {
		host = "127.0.0.1"
	}
	addr := fmt.Sprintf("%s:%d", host, s.port)
	log.Printf("Server listening on http://%s:%d (bind: %s)", host, s.port, host)
	return http.ListenAndServe(addr, handler)
}

func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := config.Load()
		token := cfg.AccessToken
		if token == "" {
			// No token configured — allow all
			next.ServeHTTP(w, r)
			return
		}

		// Skip auth for static assets and health check
		path := r.URL.Path
		if path == "/app" || strings.HasPrefix(path, "/assets/") || path == "/favicon.svg" || path == "/icons.svg" || path == "/health" {
			next.ServeHTTP(w, r)
			return
		}

		// Check token: query param, header, or cookie
		provided := r.URL.Query().Get("token")
		if provided == "" {
			provided = r.Header.Get("X-Access-Token")
		}
		if provided == "" {
			if c, err := r.Cookie("aniclew-token"); err == nil {
				provided = c.Value
			}
		}
		// Also accept via Authorization: Bearer
		if provided == "" {
			auth := r.Header.Get("Authorization")
			if strings.HasPrefix(auth, "Bearer ") {
				// Don't intercept LLM API keys — only check if it matches our token
				candidate := strings.TrimPrefix(auth, "Bearer ")
				if candidate == token {
					provided = candidate
				}
			}
		}

		if provided != token {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]string{"error": "Unauthorized. Set token via ?token= or X-Access-Token header."})
			return
		}

		next.ServeHTTP(w, r)
	})
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ── Messages Handler (core proxy logic) ──

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	activeProvider := s.activeProvider
	activeModel := s.activeModel
	rt := s.router
	gw := s.gw
	s.mu.RUnlock()

	// ── Team Gateway auth check ──
	var gwUser *gateway.User
	if gw != nil {
		user, err := gw.Authenticate(r)
		if err != nil {
			writeError(w, 401, err.Error())
			return
		}
		if user != nil {
			gwUser = user
			if !gw.CheckBudget(user) {
				writeError(w, 429, "Monthly budget exceeded. Contact admin.")
				return
			}
		}
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, 400, "Failed to read body")
		return
	}

	var req types.MessagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, 400, "Invalid JSON")
		return
	}

	startTime := time.Now()
	opts := &types.StreamOptions{IncomingHeaders: extractHeaders(r)}

	// Detect source from headers
	source := "api"
	if ua := r.Header.Get("User-Agent"); strings.Contains(ua, "claude") {
		source = "claude"
	} else if strings.Contains(ua, "codex") || strings.Contains(ua, "openai") {
		source = "codex"
	} else if r.Header.Get("Referer") != "" {
		source = "web"
	}

	// ── Smart Router or Direct ──
	var provider types.Provider
	var model string

	if rt != nil {
		decision := rt.Route(&req)
		provider, err = rt.GetProvider(decision)
		if err != nil {
			log.Printf("Router provider error, falling back: %v", err)
			provider = activeProvider
			model = activeModel
		} else {
			model = decision.Model
			log.Printf("→ [%s] %s/%s (%s) msgs=%d tools=%d",
				decision.Role, decision.Provider, model, decision.Reason,
				len(req.Messages), len(req.Tools))
		}
	} else {
		provider = activeProvider
		model = activeModel
		log.Printf("→ %s/%s msgs=%d tools=%d", provider.Name(), model, len(req.Messages), len(req.Tools))
	}

	if provider == nil {
		writeError(w, 500, "No provider configured")
		return
	}

	req.Model = model

	// ── Stream with retry + 529 fallback ──
	var ch <-chan types.SSEEvent
	retryCfg := apiPkg.DefaultRetryConfig()
	retryCfg.MaxRetries = 5
	consecutive529 := 0

	var lastErr error
	for attempt := 1; attempt <= retryCfg.MaxRetries; attempt++ {
		ch, lastErr = provider.StreamMessage(r.Context(), &req, opts)
		if lastErr == nil {
			break
		}

		log.Printf("[Proxy] Attempt %d/%d failed: %v", attempt, retryCfg.MaxRetries, lastErr)

		// Track 529s for fallback
		if strings.Contains(lastErr.Error(), "529") || strings.Contains(lastErr.Error(), "overloaded") {
			consecutive529++
			if consecutive529 >= retryCfg.Max529BeforeFallback {
				if fb := apiPkg.GetFallbackModel(model); fb != "" {
					log.Printf("[Proxy] %d consecutive 529s — falling back %s → %s", consecutive529, model, fb)
					model = fb
					req.Model = model
					consecutive529 = 0
				}
			}
		}

		if attempt < retryCfg.MaxRetries {
			delay := apiPkg.CalculateBackoff(attempt, retryCfg, "")
			log.Printf("[Proxy] Retrying in %v", delay)
			select {
			case <-r.Context().Done():
				writeError(w, 499, "Client disconnected")
				return
			case <-time.After(delay):
			}
		}
	}

	if lastErr != nil {
		// ── Fallback on failure ──
		if rt != nil {
			decision := rt.Route(&req)
			fallback := rt.GetFallback(decision.Role)
			if fallback != nil {
				log.Printf("Escalating to fallback: %s/%s", fallback.Provider, fallback.Model)
				fbProvider, fbErr := rt.GetProvider(router.RouteDecision{
					Provider: fallback.Provider, Model: fallback.Model,
				})
				if fbErr == nil {
					req.Model = fallback.Model
					ch, lastErr = fbProvider.StreamMessage(r.Context(), &req, opts)
				}
			}
		}
		if lastErr != nil {
			writeError(w, 502, lastErr.Error())
			return
		}
	}

	// ── Stream SSE ──
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)

	// Stream with watchdog
	outputTokens := 0
	inputTokens := 0
	flusher, hasFlusher := w.(http.Flusher)

	_, streamCancel := context.WithCancel(r.Context())
	defer streamCancel()

	watchdog := apiPkg.NewStreamWatchdog(func() {
		log.Printf("[Stream] Idle timeout for %s/%s — aborting", provider.Name(), model)
		streamCancel() // Actually abort the stream
	})
	defer watchdog.Stop()

	for event := range ch {
		watchdog.Ping()
		if err := stream.WriteSSEEvent(w, event); err != nil {
			break
		}
		if hasFlusher {
			flusher.Flush()
		}
		// Track tokens
		if event.Usage != nil {
			if event.Usage.OutputTokens > 0 {
				outputTokens = event.Usage.OutputTokens
			}
			if event.Usage.InputTokens > 0 {
				inputTokens = event.Usage.InputTokens
			}
		}
		if event.Type == "message_stop" {
			break
		}
	}

	// Calculate accurate cost
	cost := apiPkg.CalculateCost(model, apiPkg.TokenUsage{
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
	})

	// Record cost
	if rt != nil {
		rt.TrackUsage(provider.Name(), model, outputTokens)
	}

	// Gateway audit
	if gw != nil && gwUser != nil {
		cost := float64(outputTokens) / 1_000_000 * 5 // rough estimate
		gw.RecordUsage(gwUser.ID, provider.Name(), model, "", outputTokens, cost)
	}

	// Observability trace
	s.mu.RLock()
	tracker := s.tracker
	wd := s.workDir
	s.mu.RUnlock()
	if tracker != nil {
		latency := time.Since(startTime).Milliseconds()
		tracker.Record(observability.RequestTrace{
			ID:           fmt.Sprintf("req_%d", startTime.UnixNano()),
			Timestamp:    startTime,
			Provider:     provider.Name(),
			Model:        model,
			LatencyMs:    latency,
			InputTokens:  len(req.Messages) * 100, // estimate
			OutputTokens: outputTokens,
			Cost:         cost,
			Status:       "ok",
			Source:       source,
			WorkDir:      wd,
		})
	}
}

// ── Dashboard ──

func (s *Server) handleDashboard(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(dashboardHTML)
}

// ── Provider List ──

func (s *Server) handleListProviders(w http.ResponseWriter, _ *http.Request) {
	type info struct {
		Name        string            `json:"name"`
		DisplayName string            `json:"displayName"`
		Models      []types.ModelInfo `json:"models"`
	}
	var result []info
	for _, name := range providers.ProviderOrder {
		p, err := providers.Create(name, nil)
		if err != nil {
			continue
		}
		result = append(result, info{Name: p.Name(), DisplayName: p.DisplayName(), Models: p.Models()})
	}
	writeJSON(w, result)
}

// ── Workspace / Folder Browsing ──

func (s *Server) handleBrowseFolder(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("path")
	if dir == "" {
		dir, _ = os.Getwd()
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		writeError(w, 400, "Cannot read directory: "+err.Error())
		return
	}

	type entry struct {
		Name      string `json:"name"`
		IsDir     bool   `json:"isDir"`
		Size      int64  `json:"size"`
		IsProject bool   `json:"isProject"` // has go.mod, package.json, etc.
	}

	var result []entry
	for _, e := range entries {
		// Skip hidden files
		if strings.HasPrefix(e.Name(), ".") && e.Name() != ".." {
			continue
		}
		info, _ := e.Info()
		size := int64(0)
		if info != nil {
			size = info.Size()
		}

		isProject := false
		if e.IsDir() {
			// Check if this directory is a project
			for _, marker := range []string{"go.mod", "package.json", "Cargo.toml", "pyproject.toml", "pom.xml", "*.csproj", "*.sln"} {
				if strings.Contains(marker, "*") {
					matches, _ := filepath.Glob(filepath.Join(dir, e.Name(), marker))
					if len(matches) > 0 {
						isProject = true
						break
					}
				} else if _, err := os.Stat(filepath.Join(dir, e.Name(), marker)); err == nil {
					isProject = true
					break
				}
			}
		}

		result = append(result, entry{
			Name:      e.Name(),
			IsDir:     e.IsDir(),
			Size:      size,
			IsProject: isProject,
		})
	}

	// Sort: directories first, then files
	sort.Slice(result, func(i, j int) bool {
		if result[i].IsDir != result[j].IsDir {
			return result[i].IsDir
		}
		return result[i].Name < result[j].Name
	})

	// Add parent directory
	parent := filepath.Dir(dir)
	writeJSON(w, map[string]any{
		"current": dir,
		"parent":  parent,
		"entries": result,
	})
}

func (s *Server) handleSetWorkspace(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	json.NewDecoder(r.Body).Decode(&body)

	// Verify directory exists
	info, err := os.Stat(body.Path)
	if err != nil || !info.IsDir() {
		writeError(w, 400, "Invalid directory: "+body.Path)
		return
	}

	s.mu.Lock()
	s.workDir = body.Path
	s.mu.Unlock()

	// Save to config
	cfg := config.Load()
	cfg.WorkDir = body.Path
	config.Save(cfg)

	// Detect project
	project := agent.DetectProject(body.Path)

	log.Printf("Workspace set: %s (%s)", body.Path, project.Type)
	writeJSON(w, map[string]any{
		"ok":      true,
		"path":    body.Path,
		"project": project,
	})
}

func (s *Server) handleGetWorkspace(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	wd := s.workDir
	s.mu.RUnlock()

	if wd == "" {
		wd, _ = os.Getwd()
	}

	project := agent.DetectProject(wd)
	writeJSON(w, map[string]any{
		"path":    wd,
		"project": project,
	})
}

// ── File Read (direct, no agent) ──

func (s *Server) handleReadFile(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	wd := s.workDir
	s.mu.RUnlock()
	if wd == "" {
		wd, _ = os.Getwd()
	}

	relPath := r.URL.Query().Get("path")
	if relPath == "" {
		writeError(w, 400, "path required")
		return
	}

	// Security: prevent path traversal outside workspace
	fullPath := filepath.Join(wd, relPath)
	absWd, _ := filepath.Abs(wd)
	absFile, _ := filepath.Abs(fullPath)
	if !strings.HasPrefix(absFile, absWd) {
		writeError(w, 403, "Access denied: path outside workspace")
		return
	}

	info, err := os.Stat(fullPath)
	if err != nil {
		writeError(w, 404, "File not found")
		return
	}

	ext := strings.ToLower(filepath.Ext(fullPath))

	// Binary check
	isBinary := false
	if f, err := os.Open(fullPath); err == nil {
		buf := make([]byte, 512)
		n, _ := f.Read(buf)
		f.Close()
		for _, b := range buf[:n] {
			if b == 0 {
				isBinary = true
				break
			}
		}
	}

	if isBinary {
		writeJSON(w, map[string]any{
			"path": relPath, "type": "binary", "size": info.Size(),
			"ext": ext, "lines": 0, "content": "[Binary file]",
		})
		return
	}

	// Image
	if ext == ".png" || ext == ".jpg" || ext == ".jpeg" || ext == ".gif" || ext == ".svg" || ext == ".webp" || ext == ".ico" {
		writeJSON(w, map[string]any{
			"path": relPath, "type": "image", "size": info.Size(),
			"ext": ext, "lines": 0, "content": "[Image file: " + ext + "]",
		})
		return
	}

	data, err := os.ReadFile(fullPath)
	if err != nil {
		writeError(w, 500, "Read error: "+err.Error())
		return
	}

	content := string(data)
	lines := strings.Count(content, "\n") + 1

	// Truncate large files
	if len(content) > 100000 {
		content = content[:100000] + "\n... (truncated)"
	}

	fileType := "text"
	if ext == ".md" {
		fileType = "markdown"
	}
	if ext == ".json" {
		fileType = "json"
	}
	if ext == ".go" || ext == ".ts" || ext == ".tsx" || ext == ".js" || ext == ".py" || ext == ".rs" || ext == ".java" || ext == ".cs" {
		fileType = "code"
	}

	writeJSON(w, map[string]any{
		"path": relPath, "type": fileType, "size": info.Size(),
		"ext": ext, "lines": lines, "content": content,
	})
}

// ── Recursive File Tree (for accordion) ──

type treeNode struct {
	Name     string      `json:"name"`
	Path     string      `json:"path"`
	IsDir    bool        `json:"isDir"`
	Size     int64       `json:"size,omitempty"`
	Lines    int         `json:"lines,omitempty"`
	Children []*treeNode `json:"children,omitempty"`
}

func (s *Server) handleWriteFile(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	wd := s.workDir
	s.mu.RUnlock()
	if wd == "" {
		wd, _ = os.Getwd()
	}

	var body struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, 400, "Invalid JSON")
		return
	}

	// Security: prevent path traversal
	fullPath := filepath.Join(wd, body.Path)
	absWd, _ := filepath.Abs(wd)
	absFile, _ := filepath.Abs(fullPath)
	if !strings.HasPrefix(absFile, absWd) {
		writeError(w, 403, "Access denied: path outside workspace")
		return
	}

	// Create parent directories if needed
	os.MkdirAll(filepath.Dir(fullPath), 0755)

	if err := os.WriteFile(fullPath, []byte(body.Content), 0644); err != nil {
		writeError(w, 500, err.Error())
		return
	}

	writeJSON(w, map[string]any{"ok": true, "path": body.Path, "size": len(body.Content)})
}

func (s *Server) handleFileTree(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	wd := s.workDir
	s.mu.RUnlock()
	if wd == "" {
		wd, _ = os.Getwd()
	}

	root := buildTree(wd, wd, 4, 0) // max depth 4
	writeJSON(w, root)
}

func buildTree(basePath, currentPath string, maxDepth, depth int) []*treeNode {
	if depth >= maxDepth {
		return nil
	}

	entries, err := os.ReadDir(currentPath)
	if err != nil {
		return nil
	}

	skipDirs := map[string]bool{
		"node_modules": true, ".git": true, "__pycache__": true,
		"dist": true, ".next": true, "vendor": true, ".venv": true,
		"target": true, ".idea": true, "build": true,
	}

	var nodes []*treeNode

	// Directories first
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") && e.Name() != ".github" {
			continue
		}
		if !e.IsDir() {
			continue
		}
		if skipDirs[e.Name()] {
			continue
		}

		fullPath := filepath.Join(currentPath, e.Name())
		rel, _ := filepath.Rel(basePath, fullPath)

		node := &treeNode{
			Name:  e.Name(),
			Path:  strings.ReplaceAll(rel, "\\", "/"),
			IsDir: true,
		}
		node.Children = buildTree(basePath, fullPath, maxDepth, depth+1)
		nodes = append(nodes, node)
	}

	// Then files
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if e.IsDir() {
			continue
		}

		fullPath := filepath.Join(currentPath, e.Name())
		rel, _ := filepath.Rel(basePath, fullPath)
		info, _ := e.Info()

		size := int64(0)
		if info != nil {
			size = info.Size()
		}

		nodes = append(nodes, &treeNode{
			Name: e.Name(),
			Path: strings.ReplaceAll(rel, "\\", "/"),
			Size: size,
		})
	}

	return nodes
}

func (s *Server) handleRegisterProvider(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name    string `json:"name"`    // e.g., "ollama-home", "ollama-office"
		BaseURL string `json:"baseUrl"` // e.g., "http://192.168.1.100:11434"
		APIKey  string `json:"apiKey"`  // optional
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, 400, "Invalid JSON")
		return
	}
	if body.Name == "" {
		writeError(w, 400, "name required")
		return
	}

	// API key update only (no baseUrl)
	if body.BaseURL == "" && body.APIKey != "" {
		cfg := config.Load()
		if cfg.Providers == nil {
			cfg.Providers = map[string]config.ProviderSettings{}
		}
		existing := cfg.Providers[body.Name]
		existing.APIKey = body.APIKey
		cfg.Providers[body.Name] = existing
		config.Save(cfg)

		writeJSON(w, map[string]any{"ok": true, "name": body.Name, "keySet": true})
		return
	}

	if body.BaseURL == "" {
		writeError(w, 400, "baseUrl required for custom provider")
		return
	}

	providers.RegisterCustomProvider(body.Name, &types.ProviderConfig{
		APIKey:  body.APIKey,
		BaseURL: body.BaseURL,
	})

	// Save to config
	cfg := config.Load()
	if cfg.Providers == nil {
		cfg.Providers = map[string]config.ProviderSettings{}
	}
	cfg.Providers[body.Name] = config.ProviderSettings{
		APIKey:  body.APIKey,
		BaseURL: body.BaseURL,
	}
	config.Save(cfg)

	log.Printf("Custom provider registered: %s → %s", body.Name, body.BaseURL)
	writeJSON(w, map[string]any{
		"ok":   true,
		"name": body.Name,
		"url":  body.BaseURL,
	})
}

// ── Config API ──

func (s *Server) handleGetConfig(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	writeJSON(w, map[string]any{
		"provider":      s.activeProvider.Name(),
		"model":         s.activeModel,
		"routerEnabled": s.router != nil,
		"responseLang":  s.responseLang,
	})
}

func (s *Server) handleSetConfig(w http.ResponseWriter, r *http.Request) {
	var update struct {
		Provider      string `json:"provider"`
		Model         string `json:"model"`
		RouterEnabled *bool  `json:"routerEnabled"`
		ResponseLang  string `json:"responseLang"`
	}
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		writeError(w, 400, "Invalid JSON")
		return
	}

	if update.Provider != "" && update.Model != "" {
		p, err := providers.Create(update.Provider, nil)
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		s.SetProvider(p, update.Model)
		log.Printf("Provider switched → %s/%s", update.Provider, update.Model)
	}

	if update.ResponseLang != "" {
		s.SetResponseLang(update.ResponseLang)
		log.Printf("Response language set to: %s", update.ResponseLang)
	}

	if update.RouterEnabled != nil {
		s.mu.Lock()
		if *update.RouterEnabled && s.router == nil {
			s.router = router.New(nil, nil)
			log.Println("Smart Router enabled")
		} else if !*update.RouterEnabled {
			s.router = nil
			log.Println("Smart Router disabled")
		}
		s.mu.Unlock()
	}

	// Persist to config.json
	cfg := config.Load()
	if update.Provider != "" {
		cfg.DefaultProvider = update.Provider
	}
	if update.Model != "" {
		cfg.DefaultModel = update.Model
	}
	if update.ResponseLang != "" {
		cfg.ResponseLang = update.ResponseLang
	}
	config.Save(cfg)

	s.mu.RLock()
	writeJSON(w, map[string]any{
		"ok":            true,
		"provider":      s.activeProvider.Name(),
		"model":         s.activeModel,
		"routerEnabled": s.router != nil,
	})
	s.mu.RUnlock()
}

// ── Routes API ──

func (s *Server) handleGetRoutes(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	rt := s.router
	s.mu.RUnlock()
	if rt == nil {
		writeJSON(w, map[string]string{"error": "Router not enabled"})
		return
	}
	writeJSON(w, rt.GetConfig())
}

func (s *Server) handleSetRoute(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	rt := s.router
	s.mu.RUnlock()
	if rt == nil {
		writeError(w, 400, "Router not enabled")
		return
	}

	var update struct {
		Role     string         `json:"role"`
		Provider string         `json:"provider"`
		Model    string         `json:"model"`
		Fallback *router.Target `json:"fallback"`
	}
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		writeError(w, 400, "Invalid JSON")
		return
	}

	rt.SetRule(router.RoleID(update.Role), update.Provider, update.Model, update.Fallback)
	writeJSON(w, map[string]bool{"ok": true})
}

// ── Costs API ──

func (s *Server) handleGetCosts(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	rt := s.router
	s.mu.RUnlock()
	if rt == nil {
		writeJSON(w, map[string]any{"total": 0, "breakdown": []any{}})
		return
	}
	writeJSON(w, map[string]any{
		"total":     rt.GetTotalCost(),
		"breakdown": rt.GetCostSummary(),
	})
}

// ── Health / Root ──

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	writeJSON(w, map[string]string{
		"status": "ok", "provider": s.activeProvider.Name(), "model": s.activeModel,
	})
}

func (s *Server) handleRoot(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := map[string]any{
		"name":     "aniclew",
		"version":  "1.0.0",
		"provider": s.activeProvider.Name(),
		"model":    s.activeModel,
		"router":   s.router != nil,
		"hint":     fmt.Sprintf("Set ANTHROPIC_BASE_URL=http://localhost:%d to use with your CLI tool", s.port),
	}
	if s.router != nil {
		result["totalCost"] = fmt.Sprintf("$%.4f", s.router.GetTotalCost())
	}
	writeJSON(w, result)
}

// ── KAIROS Daemon API ──

func (d *Server) handleKairosStatus(w http.ResponseWriter, _ *http.Request) {
	daemon := d.GetDaemon()
	if daemon == nil {
		writeJSON(w, map[string]any{"enabled": false, "state": "not-initialized"})
		return
	}
	cfg := daemon.GetConfig()
	writeJSON(w, map[string]any{
		"enabled":      cfg.Enabled,
		"state":        daemon.GetState(),
		"autonomy":     cfg.Autonomy,
		"tasks":        len(daemon.GetTasks()),
		"tickInterval": cfg.TickInterval.String(),
	})
}

func (d *Server) handleKairosStart(w http.ResponseWriter, _ *http.Request) {
	daemon := d.GetDaemon()
	if daemon == nil {
		cfg := kairos.DefaultDaemonConfig()
		daemon = kairos.NewDaemon(cfg)
		home, _ := os.UserHomeDir()
		daemon.SetBaseDir(filepath.Join(home, ".claude-proxy"))
		d.SetDaemon(daemon)
	}
	// Sync daemon with current workspace
	d.mu.RLock()
	workDir := d.workDir
	d.mu.RUnlock()
	daemon.SwitchProject(workDir)
	d.mu.RLock()
	daemon.SetProvider(d.activeProvider, d.activeModel)
	d.mu.RUnlock()

	// Auto-add git-watch if not present
	hasGitWatch := false
	for _, t := range daemon.GetTasks() {
		if t.Type == "git-watch" {
			hasGitWatch = true
			break
		}
	}
	if !hasGitWatch {
		daemon.AddTask(kairos.AutoGitWatchTask())
	}

	daemon.Start()
	writeJSON(w, map[string]any{"ok": true, "state": "running"})
}

func (d *Server) handleKairosStop(w http.ResponseWriter, _ *http.Request) {
	daemon := d.GetDaemon()
	if daemon == nil {
		writeError(w, 400, "Daemon not initialized")
		return
	}
	daemon.Stop()
	writeJSON(w, map[string]any{"ok": true, "state": "stopped"})
}

func (d *Server) handleKairosTasks(w http.ResponseWriter, _ *http.Request) {
	daemon := d.GetDaemon()
	if daemon == nil {
		writeJSON(w, []any{})
		return
	}
	writeJSON(w, daemon.GetTasks())
}

func (d *Server) handleKairosAddTask(w http.ResponseWriter, r *http.Request) {
	daemon := d.GetDaemon()
	if daemon == nil {
		writeError(w, 400, "Daemon not initialized. Start KAIROS first.")
		return
	}
	var task kairos.Task
	if err := json.NewDecoder(r.Body).Decode(&task); err != nil {
		writeError(w, 400, "Invalid JSON")
		return
	}
	daemon.AddTask(task)
	writeJSON(w, map[string]any{"ok": true, "tasks": len(daemon.GetTasks())})
}

func (d *Server) handleKairosRemoveTask(w http.ResponseWriter, r *http.Request) {
	daemon := d.GetDaemon()
	if daemon == nil {
		writeError(w, 400, "Daemon not initialized")
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	daemon.RemoveTask(body.ID)
	writeJSON(w, map[string]any{"ok": true})
}

func (d *Server) handleKairosLogs(w http.ResponseWriter, _ *http.Request) {
	daemon := d.GetDaemon()
	if daemon == nil {
		writeJSON(w, []any{})
		return
	}
	writeJSON(w, daemon.GetLogs(50))
}

func (d *Server) handleKairosAutonomy(w http.ResponseWriter, r *http.Request) {
	daemon := d.GetDaemon()
	if daemon == nil {
		writeError(w, 400, "Daemon not initialized")
		return
	}
	var body struct {
		Mode string `json:"mode"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	daemon.SetAutonomy(body.Mode)
	writeJSON(w, map[string]any{"ok": true, "autonomy": body.Mode})
}

func (d *Server) handleKairosNotifications(w http.ResponseWriter, _ *http.Request) {
	daemon := d.GetDaemon()
	if daemon == nil || daemon.Notifier() == nil {
		writeJSON(w, []any{})
		return
	}
	writeJSON(w, daemon.Notifier().Recent(20))
}

func (d *Server) handleKairosSSE(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "Streaming not supported")
		return
	}

	// Send keepalive immediately so client knows connection works
	fmt.Fprintf(w, ": keepalive\n\n")
	flusher.Flush()

	// Wait for daemon to exist (poll every 2s)
	var ch chan kairos.Notification
	for {
		daemon := d.GetDaemon()
		if daemon != nil && daemon.Notifier() != nil {
			ch = daemon.Notifier().Subscribe()
			defer daemon.Notifier().Unsubscribe(ch)
			break
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(2 * time.Second):
			fmt.Fprintf(w, ": waiting for daemon\n\n")
			flusher.Flush()
		}
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case notif, ok := <-ch:
			if !ok {
				return
			}
			data, _ := json.Marshal(notif)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

func (d *Server) handleKairosWebhook(w http.ResponseWriter, r *http.Request) {
	daemon := d.GetDaemon()
	if daemon == nil || daemon.Notifier() == nil {
		writeError(w, 400, "Daemon not initialized")
		return
	}
	var body struct {
		URL string `json:"url"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	daemon.Notifier().SetWebhook(body.URL)
	writeJSON(w, map[string]any{"ok": true, "webhook": body.URL})
}

func (s *Server) handleListHooks(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	workDir := s.workDir
	s.mu.RUnlock()

	registry := hooks.NewRegistry()
	registry.Load(workDir, "")
	writeJSON(w, registry.GetHooks())
}

func (s *Server) handlePermissions(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	workDir := s.workDir
	s.mu.RUnlock()

	snap := hooks.CapturePermissions(workDir)
	writeJSON(w, snap)
}

func (s *Server) handleTraces(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	tracker := s.tracker
	s.mu.RUnlock()
	if tracker == nil {
		writeJSON(w, []any{})
		return
	}
	limit := 50
	if q := r.URL.Query().Get("limit"); q != "" {
		fmt.Sscanf(q, "%d", &limit)
	}
	writeJSON(w, tracker.Recent(limit))
}

func (s *Server) handleRunTraces(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	tracker := s.tracker
	s.mu.RUnlock()
	if tracker == nil {
		writeJSON(w, []any{})
		return
	}
	limit := 50
	if q := r.URL.Query().Get("limit"); q != "" {
		fmt.Sscanf(q, "%d", &limit)
	}
	writeJSON(w, tracker.RecentRuns(limit))
}

func (s *Server) handleAddFeedback(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	fb := s.feedback
	currentModel := s.activeModel
	currentProvider := ""
	if s.activeProvider != nil {
		currentProvider = s.activeProvider.Name()
	}
	s.mu.RUnlock()
	if fb == nil {
		writeError(w, 500, "Feedback not initialized")
		return
	}
	var body observability.Feedback
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, 400, "Invalid JSON")
		return
	}
	// Auto-fill model if not specified
	if body.Model == "" || body.Model == "auto" {
		body.Model = currentModel
	}
	if body.Provider == "" {
		body.Provider = currentProvider
	}
	fb.Add(body)
	writeJSON(w, map[string]any{"ok": true, "model": body.Model})
}

func (s *Server) handleFeedbackStats(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	fb := s.feedback
	s.mu.RUnlock()
	if fb == nil {
		writeJSON(w, map[string]any{})
		return
	}
	writeJSON(w, fb.Stats())
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	tracker := s.tracker
	s.mu.RUnlock()
	if tracker == nil {
		writeJSON(w, map[string]any{})
		return
	}
	window := 60 // default 1 hour
	if q := r.URL.Query().Get("window"); q != "" {
		fmt.Sscanf(q, "%d", &window)
	}
	writeJSON(w, tracker.Compute(window))
}

func (d *Server) handleKairosGitStatus(w http.ResponseWriter, _ *http.Request) {
	d.mu.RLock()
	workDir := d.workDir
	d.mu.RUnlock()

	status, err := kairos.CheckGitStatus(workDir)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, status)
}

// ── Image Upload ──

func (s *Server) handleImageUpload(w http.ResponseWriter, r *http.Request) {
	r.ParseMultipartForm(10 << 20) // 10MB max
	file, header, err := r.FormFile("image")
	if err != nil {
		writeError(w, 400, "No image file: "+err.Error())
		return
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		writeError(w, 500, "Read failed: "+err.Error())
		return
	}

	b64 := base64.StdEncoding.EncodeToString(data)
	mediaType := header.Header.Get("Content-Type")
	if mediaType == "" {
		mediaType = "image/png"
	}

	writeJSON(w, map[string]any{
		"ok":        true,
		"filename":  header.Filename,
		"size":      len(data),
		"mediaType": mediaType,
		"base64":    b64,
	})
}

// ── Sub-agents ──

var subAgentMgr *agent.SubAgentManager

func (s *Server) handleSubAgentSpawn(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	provider := s.activeProvider
	model := s.activeModel
	s.mu.RUnlock()

	var body struct {
		Tasks []struct {
			Name        string   `json:"name"`
			Instruction string   `json:"instruction"`
			Files       []string `json:"files"`
		} `json:"tasks"`
		WorkDir string `json:"workDir"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, 400, "Invalid JSON")
		return
	}

	workDir := body.WorkDir
	if workDir == "" {
		workDir, _ = os.Getwd()
	}

	if subAgentMgr == nil {
		subAgentMgr = agent.NewSubAgentManager(provider, model, workDir)
	}

	var spawned []map[string]string
	for _, t := range body.Tasks {
		task := subAgentMgr.Spawn(t.Name, t.Instruction, t.Files)
		spawned = append(spawned, map[string]string{"id": task.ID, "name": task.Name, "status": task.Status})
	}
	writeJSON(w, map[string]any{"spawned": len(spawned), "tasks": spawned})
}

func (s *Server) handleSubAgentTasks(w http.ResponseWriter, _ *http.Request) {
	if subAgentMgr == nil {
		writeJSON(w, []any{})
		return
	}
	writeJSON(w, subAgentMgr.GetTasks())
}

// ── Slash Commands ──

func (s *Server) handleCommandsList(w http.ResponseWriter, r *http.Request) {
	workDir := r.URL.Query().Get("workDir")
	if workDir == "" {
		workDir, _ = os.Getwd()
	}
	skills := agent.LoadSkills(workDir)
	commands := agent.ParseSlashCommands(skills)
	writeJSON(w, commands)
}

// ── Plan Mode ──

func (s *Server) handlePlanGet(w http.ResponseWriter, _ *http.Request) {
	plan := agent.GetActivePlan()
	if plan == nil {
		writeJSON(w, map[string]string{"status": "no_plan"})
		return
	}
	writeJSON(w, plan)
}

func (s *Server) handlePlanApprove(w http.ResponseWriter, _ *http.Request) {
	result := agent.ApprovePlan()
	writeJSON(w, map[string]string{"result": result})
}

// ── MCP Servers ──

func (s *Server) handleMCPList(w http.ResponseWriter, r *http.Request) {
	workDir := r.URL.Query().Get("workDir")
	if workDir == "" {
		workDir, _ = os.Getwd()
	}
	servers := agent.ListMCPServers(workDir)
	mcpTools := agent.GetMCPTools()
	writeJSON(w, map[string]any{
		"servers": servers,
		"tools":   len(mcpTools),
	})
}

func (s *Server) handleMCPConnect(w http.ResponseWriter, r *http.Request) {
	var body struct {
		WorkDir string `json:"workDir"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	if body.WorkDir == "" {
		body.WorkDir, _ = os.Getwd()
	}
	count, err := agent.ConnectMCPServers(body.WorkDir)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"connected": count, "tools": len(agent.GetMCPTools())})
}

func (s *Server) handleMCPDisconnect(w http.ResponseWriter, _ *http.Request) {
	agent.DisconnectAllMCP()
	writeJSON(w, map[string]bool{"ok": true})
}

// ── Project Context & Skills ──

func (s *Server) handleProjectContext(w http.ResponseWriter, r *http.Request) {
	workDir := r.URL.Query().Get("workDir")
	if workDir == "" {
		workDir, _ = os.Getwd()
	}
	ctx := agent.LoadProjectContext(workDir)
	mcpCfg := agent.LoadMCPConfig(workDir)
	skills := agent.LoadSkills(workDir)

	writeJSON(w, map[string]any{
		"workDir":   workDir,
		"context":   ctx,
		"mcpConfig": mcpCfg,
		"skills":    len(skills),
		"skillNames": func() []string {
			names := make([]string, len(skills))
			for i, s := range skills {
				names[i] = s.Name
			}
			return names
		}(),
	})
}

func (s *Server) handleSkillsList(w http.ResponseWriter, r *http.Request) {
	workDir := r.URL.Query().Get("workDir")
	if workDir == "" {
		workDir, _ = os.Getwd()
	}
	skills := agent.LoadSkillsWithConfig(workDir, nil)

	type skillInfo struct {
		Name   string `json:"name"`
		Path   string `json:"path"`
		Source string `json:"source"`
	}
	var result []skillInfo
	for _, sk := range skills {
		source := "custom"
		if strings.Contains(sk.Path, ".claude") {
			source = "claude"
		} else if strings.Contains(sk.Path, ".codex") {
			source = "codex"
		} else if strings.Contains(sk.Path, ".gemini") {
			source = "gemini"
		}
		result = append(result, skillInfo{Name: sk.Name, Path: sk.Path, Source: source})
	}
	writeJSON(w, result)
}

func (s *Server) handleProjectDetect(w http.ResponseWriter, r *http.Request) {
	workDir := r.URL.Query().Get("workDir")
	if workDir == "" {
		workDir, _ = os.Getwd()
	}
	info := agent.DetectProject(workDir)
	writeJSON(w, info)
}

func (s *Server) handleSetSkillSource(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Source string `json:"source"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	// Save to config
	cfg := config.Load()
	cfg.SkillSource = body.Source
	config.Save(cfg)
	writeJSON(w, map[string]any{"ok": true, "skillSource": body.Source})
}

// ── Session Management ──

func (s *Server) handleWorkspaceList(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	store := s.sessions
	s.mu.RUnlock()
	if store == nil {
		writeJSON(w, []any{})
		return
	}
	writeJSON(w, store.ListWorkspaces())
}

func (s *Server) requestWorkDir(r *http.Request, explicit string) string {
	workDir := strings.TrimSpace(explicit)
	if workDir == "" {
		workDir = strings.TrimSpace(r.URL.Query().Get("workDir"))
	}
	if workDir == "" {
		s.mu.RLock()
		workDir = s.workDir
		s.mu.RUnlock()
	}
	if workDir == "" {
		workDir, _ = os.Getwd()
	}
	return workDir
}

func (s *Server) handleWorkstreamList(w http.ResponseWriter, r *http.Request) {
	workDir := s.requestWorkDir(r, "")
	status := workstream.Status(strings.TrimSpace(r.URL.Query().Get("status")))
	if status != "" && !status.Valid() {
		writeError(w, 400, "invalid status")
		return
	}
	store := workstream.NewStore(workDir)
	workstreams, err := store.List(status)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"workDir": workDir, "workstreams": workstreams})
}

func (s *Server) handleWorkstreamCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		WorkDir    string          `json:"workDir"`
		ID         string          `json:"id"`
		Title      string          `json:"title"`
		Summary    string          `json:"summary"`
		NextAction string          `json:"nextAction"`
		Tags       []string        `json:"tags"`
		Goal       workstream.Goal `json:"goal"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, 400, "Invalid JSON")
		return
	}
	workDir := s.requestWorkDir(r, body.WorkDir)
	store := workstream.NewStore(workDir)
	ws, err := store.Create(workstream.CreateRequest{
		ID:         body.ID,
		Title:      body.Title,
		Summary:    body.Summary,
		NextAction: body.NextAction,
		Tags:       body.Tags,
		Goal:       body.Goal,
	})
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"ok":         true,
		"workstream": ws,
		"path":       workstream.StatePath(workDir, ws.ID),
	})
}

func (s *Server) handleWorkstreamGet(w http.ResponseWriter, r *http.Request) {
	workDir := s.requestWorkDir(r, "")
	id := r.PathValue("id")
	store := workstream.NewStore(workDir)
	ws, err := store.Get(id)
	if err != nil {
		writeError(w, 404, err.Error())
		return
	}
	timeline, err := store.Timeline(id)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"workstream": ws, "timeline": timeline})
}

func (s *Server) handleWorkstreamPatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		WorkDir          string                         `json:"workDir"`
		Status           *workstream.Status             `json:"status"`
		Summary          *string                        `json:"summary"`
		NextAction       *string                        `json:"nextAction"`
		OpenQuestions    []string                       `json:"openQuestions"`
		Tags             []string                       `json:"tags"`
		Goal             *workstream.Goal               `json:"goal"`
		LastVerification *workstream.VerificationResult `json:"lastVerification"`
		HasOpenQuestions bool                           `json:"hasOpenQuestions"`
		HasTags          bool                           `json:"hasTags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, 400, "Invalid JSON")
		return
	}
	workDir := s.requestWorkDir(r, body.WorkDir)
	store := workstream.NewStore(workDir)
	patch := workstream.Patch{
		Status:           body.Status,
		Summary:          body.Summary,
		NextAction:       body.NextAction,
		Goal:             body.Goal,
		LastVerification: body.LastVerification,
	}
	// JSON cannot distinguish omitted slices from null with this simple
	// decoder shape, so accept explicit booleans for clients that need to
	// clear lists. Non-empty lists are always applied.
	if len(body.OpenQuestions) > 0 || body.HasOpenQuestions {
		patch.OpenQuestions = body.OpenQuestions
	}
	if len(body.Tags) > 0 || body.HasTags {
		patch.Tags = body.Tags
	}
	ws, err := store.Patch(r.PathValue("id"), patch)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "workstream": ws})
}

func (s *Server) handleWorkstreamHandoff(w http.ResponseWriter, r *http.Request) {
	var body struct {
		WorkDir            string `json:"workDir"`
		IncludeReceipts    bool   `json:"includeReceipts"`
		IncludeMemoryIndex bool   `json:"includeMemoryIndex"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	workDir := s.requestWorkDir(r, body.WorkDir)
	store := workstream.NewStore(workDir)
	snap, err := store.GenerateHandoff(r.PathValue("id"), workstream.HandoffOptions{
		IncludeReceipts:    body.IncludeReceipts,
		IncludeMemoryIndex: body.IncludeMemoryIndex,
	})
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "path": snap.Path, "markdown": snap.Markdown})
}

func (s *Server) handleSessionList(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	store := s.sessions
	s.mu.RUnlock()
	if store == nil {
		writeJSON(w, []any{})
		return
	}
	workspace := r.URL.Query().Get("workspace")
	if workspace != "" {
		writeJSON(w, store.List(workspace))
	} else {
		writeJSON(w, store.ListAll())
	}
}

func (s *Server) handleSessionGet(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	store := s.sessions
	s.mu.RUnlock()
	if store == nil {
		writeError(w, 400, "Sessions not initialized")
		return
	}
	id := r.PathValue("id")
	sess, err := store.Get(id)
	if err != nil {
		writeError(w, 404, err.Error())
		return
	}
	writeJSON(w, sess)
}

func (s *Server) handleSessionSave(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	store := s.sessions
	prov := s.activeProvider
	model := s.activeModel
	s.mu.RUnlock()
	if store == nil {
		writeError(w, 400, "Sessions not initialized")
		return
	}
	var sess agent.Session
	if err := json.NewDecoder(r.Body).Decode(&sess); err != nil {
		writeError(w, 400, "Invalid JSON")
		return
	}
	if sess.Provider == "" {
		sess.Provider = prov.Name()
	}
	if sess.Model == "" {
		sess.Model = model
	}
	if err := store.Save(&sess); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "id": sess.ID})
}

func (s *Server) handleSessionDelete(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	store := s.sessions
	s.mu.RUnlock()
	if store == nil {
		writeError(w, 400, "Sessions not initialized")
		return
	}
	id := r.PathValue("id")
	store.Delete(id)
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleSessionRename(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	store := s.sessions
	s.mu.RUnlock()
	if store == nil {
		writeError(w, 400, "Sessions not initialized")
		return
	}
	id := r.PathValue("id")
	var body struct {
		Title string `json:"title"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	if err := store.Rename(id, body.Title); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// ── Agent Loop ──

func (s *Server) handleAgentLoop(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	provider := s.activeProvider
	model := s.activeModel
	s.mu.RUnlock()

	if provider == nil {
		writeError(w, 500, "No provider configured")
		return
	}

	var body struct {
		Messages     []types.Message `json:"messages"`
		WorkDir      string          `json:"workDir"`
		ResponseLang string          `json:"responseLang"`
		WorkstreamID string          `json:"workstreamId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, 400, "Invalid JSON")
		return
	}

	workDir := body.WorkDir
	if workDir == "" {
		s.mu.RLock()
		workDir = s.workDir
		s.mu.RUnlock()
	}
	if workDir == "" {
		workDir, _ = os.Getwd()
	}

	s.mu.RLock()
	respLang := body.ResponseLang
	if respLang == "" {
		respLang = s.responseLang
	}
	if respLang == "" {
		respLang = "auto"
	}
	tracker := s.tracker
	s.mu.RUnlock()
	traceID := observability.NewTraceID("run")

	var ws *workstream.Workstream
	var wsStore *workstream.Store
	workstreamContext := ""
	if body.WorkstreamID != "" {
		wsStore = workstream.NewStore(workDir)
		loaded, err := wsStore.Get(body.WorkstreamID)
		if err != nil {
			writeError(w, 404, err.Error())
			return
		}
		ws = loaded
		workstreamContext = workstream.RenderContext(*ws, 2000)
	}

	// Register with the loop registry before we touch the response writer
	// — if the cap is already hit we reject with 429 instead of opening
	// an SSE stream that will never stream anything useful.
	sessionID, loopCtx, release, err := s.loops.Register(r.Context(), workDir)
	if err != nil {
		switch {
		case errors.Is(err, agent.ErrTooManyLoops):
			w.Header().Set("Retry-After", "5")
			writeError(w, 429, fmt.Sprintf("too many concurrent agent loops (max %d)", s.loops.MaxConcurrent()))
		case errors.Is(err, agent.ErrShuttingDown):
			writeError(w, 503, "server is shutting down")
		default:
			writeError(w, 500, err.Error())
		}
		return
	}
	defer release()

	// SSE response
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)

	// First frame: hand the client its session id so it can later POST to
	// /api/agent/{sessionId}/cancel or query /api/agent/loops.
	sessionEvent, _ := json.Marshal(agent.Event{
		Type: "session",
		Data: map[string]string{"sessionId": sessionID, "workDir": workDir, "traceId": traceID},
	})
	fmt.Fprintf(w, "data: %s\n\n", sessionEvent)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	if ws != nil {
		workstreamEvent, _ := json.Marshal(agent.Event{
			Type: "workstream",
			Data: map[string]any{
				"id":         ws.ID,
				"title":      ws.Title,
				"status":     ws.Status,
				"nextAction": ws.NextAction,
				"traceId":    traceID,
			},
		})
		fmt.Fprintf(w, "data: %s\n\n", workstreamEvent)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	eventCh := make(chan agent.Event, 64)

	var recorders []agent.RunRecorder
	if ws != nil && wsStore != nil {
		recorders = append(recorders, &workstreamRunRecorder{
			store:        wsStore,
			id:           ws.ID,
			sessionID:    sessionID,
			traceID:      traceID,
			providerName: provider.Name(),
			model:        model,
		})
	}
	if tracker != nil {
		recorders = append(recorders, newObservabilityRunRecorder(tracker, observability.RunTrace{
			ID:           traceID,
			Kind:         "agent",
			Provider:     provider.Name(),
			Model:        model,
			WorkDir:      workDir,
			WorkstreamID: body.WorkstreamID,
			Metadata: map[string]string{
				"sessionId": sessionID,
				"source":    "api.agent",
			},
		}))
	}
	var recorder agent.RunRecorder
	if len(recorders) == 1 {
		recorder = recorders[0]
	} else if len(recorders) > 1 {
		recorder = compositeRunRecorder{recorders: recorders}
	}

	go agent.RunLoopWithOptions(loopCtx, provider, model, body.Messages, workDir, agent.RunOptions{
		ResponseLang:      respLang,
		WorkstreamContext: workstreamContext,
		Recorder:          recorder,
	}, eventCh)

	for event := range eventCh {
		data, _ := json.Marshal(event)
		fmt.Fprintf(w, "data: %s\n\n", data)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	fmt.Fprintf(w, "data: {\"type\":\"stream_end\"}\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

type workstreamRunRecorder struct {
	store        *workstream.Store
	id           string
	sessionID    string
	traceID      string
	providerName string
	model        string
}

func (r *workstreamRunRecorder) RunStarted() {
	if r == nil || r.store == nil {
		return
	}
	if err := r.store.AppendEvent(r.id, workstream.TimelineEvent{
		Type:    "agent_run_started",
		Message: "Agent run started",
		Data: map[string]string{
			"sessionId": r.sessionID,
			"traceId":   r.traceID,
			"provider":  r.providerName,
			"model":     r.model,
		},
	}); err != nil {
		log.Printf("[workstream] record start failed: %v", err)
	}
}

func (r *workstreamRunRecorder) ReceiptWritten(path string, receipt agent.AgentReceipt) {
	if r == nil || r.store == nil {
		return
	}
	if err := r.store.AppendEvent(r.id, workstream.TimelineEvent{
		Type:    "receipt_written",
		Message: "Agent receipt written",
		Data: map[string]string{
			"path":         path,
			"traceId":      r.traceID,
			"provider":     receipt.Provider,
			"model":        receipt.Model,
			"projectType":  receipt.ProjectType,
			"verification": receipt.Verification.Status,
		},
	}); err != nil {
		log.Printf("[workstream] record receipt failed: %v", err)
	}
}

func (r *workstreamRunRecorder) RunCompleted(summary agent.RunSummary) {
	if r == nil || r.store == nil {
		return
	}
	verification := workstream.VerificationResult{
		Status:  summary.Verification.Status,
		Source:  summary.Verification.Source,
		Summary: "agent run completed",
	}
	if _, err := r.store.Patch(r.id, workstream.Patch{LastVerification: &verification}); err != nil {
		log.Printf("[workstream] record verification failed: %v", err)
	}
	if err := r.store.AppendEvent(r.id, workstream.TimelineEvent{
		Type:    "agent_run_completed",
		Message: "Agent run completed",
		Data: map[string]string{
			"sessionId":    r.sessionID,
			"traceId":      r.traceID,
			"iterations":   fmt.Sprintf("%d", summary.Iterations),
			"provider":     summary.Provider,
			"model":        summary.Model,
			"projectType":  summary.ProjectType,
			"planMode":     fmt.Sprintf("%v", summary.PlanMode),
			"editedFiles":  strings.Join(summary.EditedFiles, ","),
			"verification": summary.Verification.Status,
			"receipt":      summary.ReceiptPath,
		},
	}); err != nil {
		log.Printf("[workstream] record completion failed: %v", err)
	}
}

func (r *workstreamRunRecorder) RunFailed(message string) {
	if r == nil || r.store == nil {
		return
	}
	verification := workstream.VerificationResult{
		Status:  "failed",
		Source:  "agent",
		Summary: message,
	}
	if _, err := r.store.Patch(r.id, workstream.Patch{LastVerification: &verification}); err != nil {
		log.Printf("[workstream] record failure verification failed: %v", err)
	}
	if err := r.store.AppendEvent(r.id, workstream.TimelineEvent{
		Type:    "agent_run_failed",
		Message: "Agent run failed",
		Data: map[string]string{
			"sessionId": r.sessionID,
			"traceId":   r.traceID,
			"provider":  r.providerName,
			"model":     r.model,
			"error":     message,
		},
	}); err != nil {
		log.Printf("[workstream] record failure failed: %v", err)
	}
}

// ── Bridge (remote control) ──

func (s *Server) handleBridgeCreate(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	provider := s.activeProvider
	model := s.activeModel
	workDir := s.workDir
	s.mu.RUnlock()

	if provider == nil {
		writeError(w, 500, "No provider configured")
		return
	}

	bridge := agent.NewBridge(provider, model, workDir)
	sess := bridge.CreateSession()
	writeJSON(w, sess)
}

func (s *Server) handleBridgeSend(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	provider := s.activeProvider
	model := s.activeModel
	workDir := s.workDir
	s.mu.RUnlock()

	var body struct {
		SessionID string `json:"sessionId"`
		Message   string `json:"message"`
	}
	json.NewDecoder(r.Body).Decode(&body)

	bridge := agent.NewBridge(provider, model, workDir)
	sess := bridge.CreateSession()
	result, err := bridge.Send(sess.ID, body.Message)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]string{"response": result, "sessionId": sess.ID})
}

func (s *Server) handleBridgeList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, []any{})
}

// ── Ask Model (no tools) ──

func (s *Server) handleAskModel(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	provider := s.activeProvider
	model := s.activeModel
	s.mu.RUnlock()

	if provider == nil {
		writeError(w, 500, "No provider configured")
		return
	}

	var body struct {
		Question     string `json:"question"`
		SystemPrompt string `json:"systemPrompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, 400, "Invalid JSON")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)

	eventCh := make(chan agent.Event, 64)
	go agent.AskModel(r.Context(), provider, model, body.Question, body.SystemPrompt, eventCh)

	for event := range eventCh {
		data, _ := json.Marshal(event)
		fmt.Fprintf(w, "data: %s\n\n", data)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

// ── Agent loop registry ──

// handleAgentLoops returns snapshots of every agent loop currently running
// on this server. Optional ?workDir=<path> filters to loops in a specific
// project, which the multi-project UI uses to show per-tab busy state.
func (s *Server) handleAgentLoops(w http.ResponseWriter, r *http.Request) {
	workDir := r.URL.Query().Get("workDir")
	var snaps []agent.ActiveLoopSnapshot
	if workDir != "" {
		snaps = s.loops.ByWorkDir(workDir)
	} else {
		snaps = s.loops.List()
	}
	writeJSON(w, map[string]any{
		"loops":         snaps,
		"maxConcurrent": s.loops.MaxConcurrent(),
		"count":         s.loops.Count(),
	})
}

// handleAgentCancel cancels a running loop by session id. Returns 404 if
// the session is unknown (already finished or never registered). The
// response body is a small JSON object for clients that want confirmation.
func (s *Server) handleAgentCancel(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("sessionId")
	if sessionID == "" {
		writeError(w, 400, "missing sessionId")
		return
	}
	if ok := s.loops.Cancel(sessionID); !ok {
		writeError(w, 404, "session not found")
		return
	}
	writeJSON(w, map[string]any{"sessionId": sessionID, "cancelled": true})
}

// ── Agent Types & Worktrees ──

func (s *Server) handleAgentTypes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, agent.BuiltinAgentTypes())
}

func (s *Server) handleMultiModel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Prompt string `json:"prompt"`
		Models []struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
		} `json:"models"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, 400, "Invalid JSON")
		return
	}
	if body.Prompt == "" || len(body.Models) == 0 {
		writeError(w, 400, "prompt and models required")
		return
	}

	targets := make([]struct{ Provider, Model string }, len(body.Models))
	for i, m := range body.Models {
		targets[i] = struct{ Provider, Model string }{m.Provider, m.Model}
	}

	results := agent.MultiModelQuery(r.Context(), body.Prompt, targets)
	writeJSON(w, results)
}

func (s *Server) handleRAGSearch(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	workDir := s.workDir
	s.mu.RUnlock()
	if workDir == "" {
		workDir, _ = os.Getwd()
	}

	query := r.URL.Query().Get("q")
	if query == "" {
		writeError(w, 400, "q parameter required")
		return
	}
	results := agent.RAGSearch(workDir, query, 5)
	writeJSON(w, results)
}

func (s *Server) handlePlugins(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	workDir := s.workDir
	s.mu.RUnlock()

	home, _ := os.UserHomeDir()
	dirs := []string{
		filepath.Join(workDir, ".claude", "plugins"),
		filepath.Join(home, ".claude", "plugins"),
	}
	pm := agent.NewPluginManager(dirs...)
	pm.LoadAll()
	writeJSON(w, pm.GetPlugins())
}

func (s *Server) handleWorktrees(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	workDir := s.workDir
	s.mu.RUnlock()
	if workDir == "" {
		writeJSON(w, []any{})
		return
	}
	writeJSON(w, agent.ListWorktrees(workDir))
}

// ── Team Execution ──

func (s *Server) handleTeamExecute(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	provider := s.activeProvider
	model := s.activeModel
	workDir := s.workDir
	s.mu.RUnlock()

	if provider == nil {
		writeError(w, 500, "No provider configured")
		return
	}

	var body struct {
		Name          string `json:"name"`
		VerifyCommand string `json:"verifyCommand"`
		Tasks         []struct {
			ID          string   `json:"id"`
			Name        string   `json:"name"`
			Description string   `json:"description"`
			Files       []string `json:"files"`
			DependsOn   []string `json:"dependsOn"`
		} `json:"tasks"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, 400, "Invalid JSON")
		return
	}

	if len(body.Tasks) == 0 {
		writeError(w, 400, "At least one task required")
		return
	}
	if workDir == "" {
		workDir, _ = os.Getwd()
	}

	home, _ := os.UserHomeDir()
	baseDir := filepath.Join(home, ".claude-proxy")

	team := agent.NewTeam(provider, model, workDir, baseDir, agent.TeamConfig{
		Name:          body.Name,
		VerifyCommand: body.VerifyCommand,
	})

	for _, t := range body.Tasks {
		team.AddTask(agent.TeamTask{
			ID:          t.ID,
			Name:        t.Name,
			Description: t.Description,
			Files:       t.Files,
			DependsOn:   t.DependsOn,
		})
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)

	eventCh := make(chan agent.Event, 64)

	go func() {
		defer close(eventCh)

		if err := team.ExecuteWaves(r.Context(), eventCh); err != nil {
			eventCh <- agent.Event{Type: "error", Data: err.Error()}
		}

		// Verify if command configured
		if body.VerifyCommand != "" {
			passed, detail := team.Verify(r.Context())
			if passed {
				eventCh <- agent.Event{Type: "status", Data: "Verification PASSED"}
			} else {
				eventCh <- agent.Event{Type: "status", Data: "Verification FAILED: " + detail}
			}
		}

		eventCh <- agent.Event{Type: "text", Data: "\n\n" + team.Summary()}
		team.Shutdown()
		eventCh <- agent.Event{Type: "done", Data: nil}
	}()

	for event := range eventCh {
		data, _ := json.Marshal(event)
		fmt.Fprintf(w, "data: %s\n\n", data)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

// ── Chronos (Autonomous Loop) ──

func (s *Server) handleChronos(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	provider := s.activeProvider
	model := s.activeModel
	workDir := s.workDir
	tracker := s.tracker
	s.mu.RUnlock()

	if provider == nil {
		writeError(w, 500, "No provider configured")
		return
	}

	var body struct {
		Task          string `json:"task"`
		VerifyCommand string `json:"verifyCommand"`
		MaxCycles     int    `json:"maxCycles"`
		WorkstreamID  string `json:"workstreamId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, 400, "Invalid JSON")
		return
	}

	if body.Task == "" {
		writeError(w, 400, "task is required")
		return
	}
	if workDir == "" {
		workDir, _ = os.Getwd()
	}
	traceID := observability.NewTraceID("run")

	cfg := agent.DefaultChronosConfig()
	if body.VerifyCommand != "" {
		cfg.VerifyCommand = body.VerifyCommand
	}
	if body.MaxCycles > 0 {
		cfg.MaxCycles = body.MaxCycles
	}

	var ws *workstream.Workstream
	var wsStore *workstream.Store
	if body.WorkstreamID != "" {
		wsStore = workstream.NewStore(workDir)
		loaded, err := wsStore.Get(body.WorkstreamID)
		if err != nil {
			writeError(w, 404, err.Error())
			return
		}
		ws = loaded
		cfg.WorkstreamContext = workstream.RenderContext(*ws, 2000)
		if err := wsStore.AppendEvent(ws.ID, workstream.TimelineEvent{
			Type:    "chronos_run_started",
			Message: "Chronos run started",
			Data: map[string]string{
				"provider":  provider.Name(),
				"model":     model,
				"traceId":   traceID,
				"task":      body.Task,
				"maxCycles": fmt.Sprintf("%d", cfg.MaxCycles),
			},
		}); err != nil {
			writeError(w, 500, err.Error())
			return
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)

	eventCh := make(chan agent.Event, 64)
	traceStartedAt := time.Now().UTC()
	chronosTrace := observability.RunTrace{
		ID:           traceID,
		Kind:         "chronos",
		StartedAt:    traceStartedAt,
		Provider:     provider.Name(),
		Model:        model,
		WorkDir:      workDir,
		WorkstreamID: body.WorkstreamID,
		Status:       "running",
		Metadata: map[string]string{
			"task":          body.Task,
			"maxCycles":     fmt.Sprintf("%d", cfg.MaxCycles),
			"verifyCommand": cfg.VerifyCommand,
		},
		Spans: []observability.RunSpan{{
			ID:        "chronos",
			Name:      "chronos.run",
			StartedAt: traceStartedAt,
			Status:    "running",
			Data:      map[string]string{"task": body.Task},
		}},
	}

	traceEvent, _ := json.Marshal(agent.Event{
		Type: "trace",
		Data: map[string]string{"traceId": traceID},
	})
	fmt.Fprintf(w, "data: %s\n\n", traceEvent)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	if ws != nil {
		workstreamEvent, _ := json.Marshal(agent.Event{
			Type: "workstream",
			Data: map[string]any{
				"id":         ws.ID,
				"title":      ws.Title,
				"status":     ws.Status,
				"nextAction": ws.NextAction,
				"traceId":    traceID,
			},
		})
		fmt.Fprintf(w, "data: %s\n\n", workstreamEvent)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	go agent.RunChronos(r.Context(), provider, model, body.Task, workDir, cfg, eventCh)

	recordedChronosEnd := false
	for event := range eventCh {
		if !recordedChronosEnd {
			switch event.Type {
			case "done":
				recordedChronosEnd = true
				finishRunTrace(&chronosTrace, "ok", "")
				if tracker != nil {
					tracker.RecordRun(chronosTrace)
				}
				if wsStore != nil && ws != nil {
					verification := workstream.VerificationResult{
						Status:  "not-run",
						Source:  "chronos",
						Summary: "chronos run completed",
					}
					if _, err := wsStore.Patch(ws.ID, workstream.Patch{LastVerification: &verification}); err != nil {
						log.Printf("[workstream] record chronos verification failed: %v", err)
					}
					if err := wsStore.AppendEvent(ws.ID, workstream.TimelineEvent{
						Type:    "chronos_run_completed",
						Message: "Chronos run completed",
						Data: map[string]string{
							"provider": provider.Name(),
							"model":    model,
							"traceId":  traceID,
							"task":     body.Task,
						},
					}); err != nil {
						log.Printf("[workstream] record chronos completion failed: %v", err)
					}
				}
			case "error":
				recordedChronosEnd = true
				finishRunTrace(&chronosTrace, "failed", fmt.Sprint(event.Data))
				if tracker != nil {
					tracker.RecordRun(chronosTrace)
				}
				if wsStore != nil && ws != nil {
					verification := workstream.VerificationResult{
						Status:  "failed",
						Source:  "chronos",
						Summary: fmt.Sprint(event.Data),
					}
					if _, err := wsStore.Patch(ws.ID, workstream.Patch{LastVerification: &verification}); err != nil {
						log.Printf("[workstream] record chronos verification failed: %v", err)
					}
					if err := wsStore.AppendEvent(ws.ID, workstream.TimelineEvent{
						Type:    "chronos_run_failed",
						Message: "Chronos run failed",
						Data: map[string]string{
							"provider": provider.Name(),
							"model":    model,
							"traceId":  traceID,
							"task":     body.Task,
							"error":    fmt.Sprint(event.Data),
						},
					}); err != nil {
						log.Printf("[workstream] record chronos failure failed: %v", err)
					}
				}
			}
		}
		data, _ := json.Marshal(event)
		fmt.Fprintf(w, "data: %s\n\n", data)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	fmt.Fprintf(w, "data: {\"type\":\"stream_end\"}\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// ── Memory (AutoDream) API ──

func (s *Server) handleMemoryState(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	mem := s.memory
	s.mu.RUnlock()
	if mem == nil {
		writeJSON(w, map[string]string{"error": "Memory not initialized"})
		return
	}
	writeJSON(w, mem.GetState())
}

func (s *Server) handleMemoryAdd(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	mem := s.memory
	s.mu.RUnlock()
	if mem == nil {
		writeError(w, 400, "Memory not initialized")
		return
	}
	var entry kairos.MemoryEntry
	if err := json.NewDecoder(r.Body).Decode(&entry); err != nil {
		writeError(w, 400, "Invalid JSON")
		return
	}
	mem.AddEntry(entry)
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleMemorySearch(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	mem := s.memory
	s.mu.RUnlock()
	if mem == nil {
		writeJSON(w, []any{})
		return
	}
	q := r.URL.Query().Get("q")
	writeJSON(w, mem.Search(q))
}

func (s *Server) handleMemoryDream(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	mem := s.memory
	provider := s.activeProvider
	s.mu.RUnlock()
	if mem == nil {
		writeError(w, 400, "Memory not initialized")
		return
	}
	mem.Dream(provider)
	writeJSON(w, map[string]any{"ok": true, "state": mem.GetState()})
}

// ── A/B Testing API ──

func (s *Server) handleABTestRun(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	ab := s.abTester
	s.mu.RUnlock()
	if ab == nil {
		writeError(w, 400, "A/B tester not initialized")
		return
	}

	var body struct {
		Prompt    string `json:"prompt"`
		ProviderA string `json:"providerA"`
		ModelA    string `json:"modelA"`
		ProviderB string `json:"providerB"`
		ModelB    string `json:"modelB"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, 400, "Invalid JSON")
		return
	}

	pA, err := providers.Create(body.ProviderA, nil)
	if err != nil {
		writeError(w, 400, "Invalid providerA: "+err.Error())
		return
	}
	pB, err := providers.Create(body.ProviderB, nil)
	if err != nil {
		writeError(w, 400, "Invalid providerB: "+err.Error())
		return
	}

	result := ab.RunTest(r.Context(), body.Prompt, pA, body.ModelA, pB, body.ModelB)
	writeJSON(w, result)
}

func (s *Server) handleABTestResults(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	ab := s.abTester
	s.mu.RUnlock()
	if ab == nil {
		writeJSON(w, []any{})
		return
	}
	writeJSON(w, ab.GetResults(20))
}

// ── PR Auto-Reviewer (GitHub Webhook) ──

func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	provider := s.activeProvider
	model := s.activeModel
	s.mu.RUnlock()

	if provider == nil {
		writeError(w, 500, "No provider configured")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, 400, "Failed to read body")
		return
	}

	event := r.Header.Get("X-GitHub-Event")
	if event != "pull_request" {
		writeJSON(w, map[string]string{"status": "ignored", "event": event})
		return
	}

	result, err := kairos.HandlePRWebhook(r.Context(), body, provider, model)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if result == nil {
		writeJSON(w, map[string]string{"status": "skipped"})
		return
	}
	writeJSON(w, result)
}

// ── Team Gateway API ──

func (s *Server) handleGatewayUsers(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	gw := s.gw
	s.mu.RUnlock()
	if gw == nil {
		writeJSON(w, []any{})
		return
	}
	writeJSON(w, gw.GetUsers())
}

func (s *Server) handleGatewayAddUser(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	gw := s.gw
	s.mu.RUnlock()
	if gw == nil {
		writeError(w, 400, "Gateway not initialized")
		return
	}
	var body struct {
		Name    string   `json:"name"`
		Role    string   `json:"role"`
		Budget  float64  `json:"budget"`
		Allowed []string `json:"allowedProviders"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, 400, "Invalid JSON")
		return
	}
	user := gw.AddUser(body.Name, body.Role, body.Budget, body.Allowed)
	writeJSON(w, user)
}

func (s *Server) handleGatewayAudit(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	gw := s.gw
	s.mu.RUnlock()
	if gw == nil {
		writeJSON(w, []any{})
		return
	}
	writeJSON(w, gw.GetAudit(50))
}

// ── Helpers ──

func extractHeaders(r *http.Request) map[string]string {
	h := map[string]string{}
	for _, key := range []string{"authorization", "x-api-key", "anthropic-beta", "x-app", "user-agent", "x-claude-code-session-id"} {
		if v := r.Header.Get(key); v != "" {
			h[strings.ToLower(key)] = v
		}
	}
	return h
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": "api_error", "message": msg},
	})
}

// ── Projects CRUD ──

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	cfg := config.Load()
	type projectInfo struct {
		Path      string `json:"path"`
		Name      string `json:"name"`
		Type      string `json:"type,omitempty"`
		Framework string `json:"framework,omitempty"`
		FileCount int    `json:"fileCount,omitempty"`
		Active    bool   `json:"active"`
	}

	s.mu.RLock()
	currentWork := s.workDir
	s.mu.RUnlock()

	projects := make([]projectInfo, 0, len(cfg.Projects))
	for _, p := range cfg.Projects {
		info := projectInfo{Path: p.Path, Name: p.Name, Active: p.Path == currentWork}
		// Auto-detect project type
		proj := agent.DetectProject(p.Path)
		info.Type = proj.Type
		info.Framework = proj.Framework
		info.FileCount = proj.FileCount
		if info.Name == "" {
			info.Name = proj.Name
		}
		projects = append(projects, info)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(projects)
}

func (s *Server) handleAddProject(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, 400, "Invalid JSON")
		return
	}

	// Validate directory exists
	stat, err := os.Stat(body.Path)
	if err != nil || !stat.IsDir() {
		writeError(w, 400, "Directory not found: "+body.Path)
		return
	}

	// Auto-detect name if empty
	if body.Name == "" {
		body.Name = filepath.Base(body.Path)
	}

	cfg := config.Load()

	// Check duplicate
	for _, p := range cfg.Projects {
		if filepath.Clean(p.Path) == filepath.Clean(body.Path) {
			writeError(w, 409, "Project already exists")
			return
		}
	}

	cfg.Projects = append(cfg.Projects, config.Project{Path: body.Path, Name: body.Name})
	if err := config.Save(cfg); err != nil {
		writeError(w, 500, "Failed to save config")
		return
	}

	// Switch to the new project
	s.SetWorkDir(body.Path)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "name": body.Name, "path": body.Path})
}

func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, 400, "path query parameter required")
		return
	}

	cfg := config.Load()
	found := false
	filtered := make([]config.Project, 0, len(cfg.Projects))
	for _, p := range cfg.Projects {
		if filepath.Clean(p.Path) == filepath.Clean(path) {
			found = true
			continue
		}
		filtered = append(filtered, p)
	}

	if !found {
		writeError(w, 404, "Project not found")
		return
	}

	cfg.Projects = filtered
	config.Save(cfg)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}
