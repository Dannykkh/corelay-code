package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/config"
	"github.com/Dannykkh/corelay-code/internal/gateway"
	"github.com/Dannykkh/corelay-code/internal/kairos"
	"github.com/Dannykkh/corelay-code/internal/observability"
	"github.com/Dannykkh/corelay-code/internal/providers"
	"github.com/Dannykkh/corelay-code/internal/router"
	"github.com/Dannykkh/corelay-code/internal/server"
	"github.com/Dannykkh/corelay-code/internal/translate"
	"github.com/Dannykkh/corelay-code/internal/tray"
	"github.com/Dannykkh/corelay-code/internal/types"
)

func main() {
	providerName := flag.String("provider", "", "Provider name")
	model := flag.String("model", "", "Model ID")
	port := flag.Int("port", 0, "Listen port (default 4000)")
	enableRouter := flag.Bool("router", false, "Enable smart router")
	flag.Usage = func() { printRootUsage(flag.CommandLine.Output()) }

	// Subcommand: `corelaycode chat` runs the built-in terminal client (connects to
	// a running server's /api/agent). `corelaycode tui` is an explicit alias for
	// the full-screen client. Everything else starts the server.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "chat":
			runChat(os.Args[2:])
			return
		case "tui":
			runChat(append([]string{"-tui"}, os.Args[2:]...))
			return
		case "help":
			printRootUsage(os.Stdout)
			return
		case "worker":
			runWorker(os.Args[2:])
			return
		case "team":
			runTeam(os.Args[2:])
			return
		}
	}

	flag.Parse()

	// Load saved config
	cfg := config.Load()

	// Apply defaults from config
	if *port == 0 {
		if cfg.Port > 0 {
			*port = cfg.Port
		} else {
			*port = 4000
		}
	}
	if *providerName == "" && cfg.DefaultProvider != "" {
		*providerName = cfg.DefaultProvider
	}
	if *model == "" && cfg.DefaultModel != "" {
		*model = cfg.DefaultModel
	}
	if cfg.RouterEnabled {
		*enableRouter = true
	}

	// Interactive selection if still empty
	if *providerName == "" || *model == "" {
		*providerName, *model = interactiveSelect()

		// Save choice
		cfg.DefaultProvider = *providerName
		cfg.DefaultModel = *model
		cfg.Port = *port
		cfg.RouterEnabled = *enableRouter
		if err := config.Save(cfg); err != nil {
			log.Printf("Warning: could not save config: %v", err)
		} else {
			fmt.Fprintf(os.Stderr, "  Config saved to %s\n", config.ConfigPath())
		}
	}

	provider, err := providers.Create(*providerName, &types.ProviderConfig{})
	if err != nil {
		log.Fatalf("Error: %v", err)
	}

	if err := provider.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "  Warning: %v\n", err)
	}

	srv := server.New(provider, *model, *port)
	if cfg.EvidencePolicy != "" || cfg.EvidenceMaxStopBlocks > 0 {
		srv.SetEvidencePolicy(agent.EvidencePolicyConfig{
			Policy:        cfg.EvidencePolicy,
			MaxStopBlocks: cfg.EvidenceMaxStopBlocks,
		})
	}

	// ── Initialize all subsystems ──
	baseDir := config.BaseDir()

	// Register custom providers from config
	for name, settings := range cfg.Providers {
		// Skip built-in provider names
		isBuiltin := false
		for _, b := range []string{"anthropic", "openai", "gemini", "groq", "ollama", "github-copilot", "zai"} {
			if name == b {
				isBuiltin = true
				break
			}
		}
		if !isBuiltin && settings.BaseURL != "" {
			providers.RegisterCustomProvider(name, &types.ProviderConfig{
				APIKey:  settings.APIKey,
				BaseURL: settings.BaseURL,
			})
			fmt.Fprintf(os.Stderr, "  Custom provider: %s → %s\n", name, settings.BaseURL)
		}
	}

	// Set workspace from config
	if cfg.WorkDir != "" {
		srv.SetWorkDir(cfg.WorkDir)
	}

	// Smart Router
	routerStatus := "DISABLED"
	if *enableRouter {
		rt := router.New(nil, nil)
		srv.SetRouter(rt)
		routerStatus = "ENABLED (auto-route by role)"
	}

	// Observability
	tracker := observability.NewTracker(baseDir)
	srv.SetTracker(tracker)
	srv.SetFeedback(observability.NewFeedbackStore(baseDir))

	// AutoDream Memory
	mem := kairos.NewMemory(baseDir)
	srv.SetMemory(mem)

	// A/B Tester
	ab := kairos.NewABTester(kairos.ABTestConfig{Enabled: true})
	srv.SetABTester(ab)

	// Team Gateway
	gw := gateway.New(baseDir)
	srv.SetGateway(gw)

	// Session Store
	sessions := agent.NewSessionStore(baseDir)
	srv.SetSessionStore(sessions)

	runtimeQuotaCollectors := srv.StartRuntimeQuotaCollectors(cfg.RuntimeQuotaSources)

	fmt.Fprintf(os.Stderr, "\n")
	fmt.Fprintf(os.Stderr, "  ╔══════════════════════════════════════════╗\n")
	fmt.Fprintf(os.Stderr, "  ║ Corelay Code v1.0 — Any Model, One Agent ║\n")
	fmt.Fprintf(os.Stderr, "  ╚══════════════════════════════════════════╝\n")
	fmt.Fprintf(os.Stderr, "\n")
	fmt.Fprintf(os.Stderr, "  Proxy:     http://localhost:%d\n", *port)
	fmt.Fprintf(os.Stderr, "  Dashboard: http://localhost:%d/dashboard\n", *port)
	fmt.Fprintf(os.Stderr, "  Provider:  %s\n", provider.DisplayName())
	fmt.Fprintf(os.Stderr, "  Model:     %s\n", *model)
	fmt.Fprintf(os.Stderr, "  Router:    %s\n", routerStatus)
	if runtimeQuotaCollectors > 0 {
		fmt.Fprintf(os.Stderr, "  Quota:     collector polling %d source(s)\n", runtimeQuotaCollectors)
	}
	if agent.OfflineMode() {
		fmt.Fprintf(os.Stderr, "  Network:   AIR-GAP (CORELAY_OFFLINE) — WebSearch/WebFetch/HTTPRequest disabled\n")
	}
	if budget := translate.ToolBudget(); budget > 0 {
		fmt.Fprintf(os.Stderr, "  Tools:     budget %d (CORELAY_MAX_TOOLS) — large tool lists pruned for weak models\n", budget)
	}
	fmt.Fprintf(os.Stderr, "\n")
	fmt.Fprintf(os.Stderr, "  Usage:\n")
	// CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST makes Claude Code drop provider vars that
	// come from ~/.claude/settings.json, so a base URL left behind by another proxy
	// tool cannot silently take routing back. It also drops model slots set there.
	fmt.Fprintf(os.Stderr, "    ANTHROPIC_BASE_URL=http://localhost:%d CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST=1 claude\n", *port)
	fmt.Fprintf(os.Stderr, "\n")

	// Graceful shutdown channel — wired to OS signals AND the tray Quit menu so
	// the user can stop the server from either place and we run the same drain.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	// Auto-open the dashboard once on startup.
	go tray.OpenBrowser(*port)

	// System tray icon (Windows only; no-op elsewhere). Right-click → Open
	// Dashboard / Quit Corelay Code. Quit pushes os.Interrupt into sigCh so the
	// existing graceful drain path handles tear-down.
	if err := tray.Run(*port,
		func() { tray.OpenBrowser(*port) },
		func() {
			select {
			case sigCh <- os.Interrupt:
			default:
			}
		},
	); err != nil {
		fmt.Fprintf(os.Stderr, "  Tray:      unavailable (%v)\n", err)
	} else if tray.Active() {
		fmt.Fprintf(os.Stderr, "  Tray:      enabled (right-click for menu)\n")
	}

	go func() {
		if err := srv.Start(); err != nil {
			log.Fatalf("Server error: %v", err)
		}
	}()

	<-sigCh
	fmt.Fprintf(os.Stderr, "\n  Shutting down...\n")
	tray.Stop()

	// Drain order matters:
	//   1. Stop accepting new agent loops and cancel live ones, bounded
	//      to 30s so a hung provider stream cannot block shutdown.
	//   2. Wait for background memory extract/consolidate goroutines,
	//      which were spawned with their own context.Background() timers
	//      and are NOT cancelled by the HTTP context. They write to disk
	//      so letting them finish avoids corrupted MEMORY.md.
	//   3. Disconnect MCP servers last — they're cheapest to tear down
	//      and some agent loops may still be talking to them during (1).
	if loops := srv.Loops(); loops != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if ok := loops.Shutdown(ctx); !ok {
			fmt.Fprintf(os.Stderr, "  Warning: %d agent loop(s) still active at shutdown timeout\n", loops.Count())
		}
		cancel()
	}
	if !agent.WaitForMemoryTasks(30 * time.Second) {
		fmt.Fprintf(os.Stderr, "  Warning: background memory tasks did not finish within 30s\n")
	}
	srv.StopRuntimeQuotaCollectors()
	agent.DisconnectAllMCP()
	fmt.Fprintf(os.Stderr, "  Goodbye.\n")
}

func printRootUsage(out io.Writer) {
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  corelaycode [server flags]        Start the Corelay Code server")
	fmt.Fprintln(out, "  corelaycode chat [flags]          Interactive TUI with automatic line fallback")
	fmt.Fprintln(out, "  corelaycode tui [flags]           Require the full-screen TUI")
	fmt.Fprintln(out, "  corelaycode chat -plain           Line-oriented terminal client")
	fmt.Fprintln(out, "  corelaycode chat -p <prompt>      One-shot terminal client")
	fmt.Fprintln(out, "  corelaycode worker [flags]        Run a worker client")
	fmt.Fprintln(out, "  corelaycode team [flags]          Run a team client")
	fmt.Fprintln(out, "\nServer flags:")
	flag.CommandLine.SetOutput(out)
	flag.CommandLine.PrintDefaults()
}

func interactiveSelect() (string, string) {
	fmt.Fprintf(os.Stderr, "\n  Corelay Code\n")
	fmt.Fprintf(os.Stderr, "  ═══════════════════════\n\n")
	fmt.Fprintf(os.Stderr, "  Select provider:\n")

	for i, name := range providers.ProviderOrder {
		p, _ := providers.Create(name, nil)
		fmt.Fprintf(os.Stderr, "    %d. %s\n", i+1, p.DisplayName())
	}
	fmt.Fprintf(os.Stderr, "\n  > ")

	var choice int
	fmt.Scan(&choice)
	if choice < 1 || choice > len(providers.ProviderOrder) {
		log.Fatal("Invalid selection")
	}

	provName := providers.ProviderOrder[choice-1]
	provider, _ := providers.Create(provName, nil)
	models := provider.Models()

	fmt.Fprintf(os.Stderr, "\n  Select model:\n")
	for i, m := range models {
		fmt.Fprintf(os.Stderr, "    %d. %s (%s)\n", i+1, m.DisplayName, m.ID)
	}
	fmt.Fprintf(os.Stderr, "    %d. [Custom model ID]\n", len(models)+1)
	fmt.Fprintf(os.Stderr, "\n  > ")

	fmt.Scan(&choice)
	var modelID string
	if choice == len(models)+1 {
		fmt.Fprintf(os.Stderr, "  Model ID: ")
		fmt.Scan(&modelID)
	} else if choice >= 1 && choice <= len(models) {
		modelID = models[choice-1].ID
	} else {
		log.Fatal("Invalid selection")
	}

	return provName, modelID
}
