package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Dannykkh/corelay-code/internal/acp"
	"github.com/Dannykkh/corelay-code/internal/acpbridge"
	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/config"
	"github.com/Dannykkh/corelay-code/internal/providers"
	"github.com/Dannykkh/corelay-code/internal/types"
)

var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(
	ctx context.Context,
	args []string,
	stdin io.ReadCloser,
	stdout io.WriteCloser,
	stderr io.Writer,
) int {
	flags := flag.NewFlagSet("corelaycode-acp", flag.ContinueOnError)
	flags.SetOutput(stderr)
	providerName := flags.String("provider", "", "provider name (defaults to saved configuration)")
	model := flags.String("model", "", "model ID (defaults to saved configuration)")
	responseLang := flags.String("response-lang", "", "response language: auto, ko, en, ja, or zh")
	maxFrame := flags.Int("max-frame-bytes", acp.DefaultMaxFrameBytes, "maximum newline JSON-RPC frame size")
	maxInflight := flags.Int("max-inflight", acp.DefaultMaxInflight, "maximum concurrent JSON-RPC requests")
	writeQueue := flags.Int("write-queue", acp.DefaultWriteQueue, "maximum queued JSON-RPC writes")
	shutdownTimeout := flags.Duration("shutdown-timeout", 10*time.Second, "maximum agent shutdown wait")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "corelaycode-acp: unexpected positional arguments")
		return 2
	}

	cfg := config.Load()
	registerCustomProviders(cfg)
	if strings.TrimSpace(*providerName) == "" {
		*providerName = cfg.DefaultProvider
	}
	if strings.TrimSpace(*model) == "" {
		*model = cfg.DefaultModel
	}
	if strings.TrimSpace(*responseLang) == "" {
		*responseLang = cfg.ResponseLang
	}
	if strings.TrimSpace(*responseLang) == "" {
		*responseLang = "auto"
	}
	if strings.TrimSpace(*providerName) == "" || strings.TrimSpace(*model) == "" {
		fmt.Fprintln(stderr, "corelaycode-acp: --provider and --model are required when no saved defaults exist")
		return 2
	}
	if *shutdownTimeout <= 0 || *shutdownTimeout > 5*time.Minute {
		fmt.Fprintln(stderr, "corelaycode-acp: --shutdown-timeout must be between 1ns and 5m")
		return 2
	}

	selectedProvider := strings.TrimSpace(*providerName)
	settings := cfg.Providers[selectedProvider]
	provider, err := providers.Create(selectedProvider, &types.ProviderConfig{
		APIKey:  settings.APIKey,
		BaseURL: settings.BaseURL,
	})
	if err != nil {
		fmt.Fprintln(stderr, "corelaycode-acp: provider configuration is invalid")
		return 2
	}
	if err := provider.Validate(); err != nil {
		fmt.Fprintln(stderr, "corelaycode-acp: provider is unavailable")
		return 2
	}
	// A standalone ACP process owns a dedicated session namespace. The server's
	// SessionStore lock is process-local, so sharing its directory would make
	// optimistic revisions unsafe across two executables.
	stateDir := filepath.Join(config.BaseDir(), "acp")
	stateLock, err := acpbridge.AcquireStateLock(stateDir)
	if err != nil {
		fmt.Fprintln(stderr, "corelaycode-acp: durable state is already in use")
		return 1
	}
	defer stateLock.Close()
	backend, err := acpbridge.New(acpbridge.Options{
		Provider:     provider,
		DefaultModel: strings.TrimSpace(*model),
		Store:        agent.NewSessionStore(stateDir),
		ResponseLang: strings.TrimSpace(*responseLang),
		Version:      version,
		// The bridge owns each session's MCP runtime and immutable catalog, so
		// the backend's ordinary bounded concurrency applies across sessions.
	})
	if err != nil {
		fmt.Fprintln(stderr, "corelaycode-acp: composition failed")
		return 1
	}

	connection := acp.NewConnection(stdin, stdout, backend, acp.Options{
		MaxFrameBytes: *maxFrame,
		MaxInflight:   *maxInflight,
		WriteQueue:    *writeQueue,
	})
	serveErr := connection.Serve(ctx)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), *shutdownTimeout)
	shutdownErr := backend.Shutdown(shutdownCtx)
	cancel()
	if !agent.WaitForMemoryTasks(*shutdownTimeout) {
		fmt.Fprintln(stderr, "corelaycode-acp: background memory shutdown timed out")
		return 1
	}
	if serveErr != nil {
		fmt.Fprintln(stderr, "corelaycode-acp: transport stopped unexpectedly")
		return 1
	}
	if shutdownErr != nil {
		fmt.Fprintln(stderr, "corelaycode-acp: agent shutdown timed out")
		return 1
	}
	return 0
}

func registerCustomProviders(cfg config.Config) {
	builtins := map[string]struct{}{
		"anthropic": {}, "openai": {}, "gemini": {}, "groq": {},
		"ollama": {}, "sglang": {}, "github-copilot": {}, "zai": {},
	}
	for name, settings := range cfg.Providers {
		if _, builtin := builtins[name]; builtin || strings.TrimSpace(name) == "" || strings.TrimSpace(settings.BaseURL) == "" {
			continue
		}
		providers.RegisterCustomProvider(name, &types.ProviderConfig{
			APIKey:  settings.APIKey,
			BaseURL: settings.BaseURL,
		})
	}
}
