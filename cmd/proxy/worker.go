package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/aniclew/aniclew/internal/agent"
	"github.com/aniclew/aniclew/internal/config"
	"github.com/aniclew/aniclew/internal/providers"
	"github.com/aniclew/aniclew/internal/types"
)

func runWorker(args []string) {
	if len(args) > 0 && args[0] == "run" {
		args = args[1:]
	}

	fs := flag.NewFlagSet("worker run", flag.ExitOnError)
	taskPath := fs.String("task", "", "Path to an AgentTask JSON file")
	prompt := fs.String("prompt", "", "Inline task prompt when --task is not used")
	providerName := fs.String("provider", "", "Provider name (default: saved config or ollama)")
	model := fs.String("model", "", "Model ID (default: saved config or qwen3:8b)")
	workDir := fs.String("workdir", "", "Workspace directory (default: saved config or cwd)")
	baseDir := fs.String("base-dir", "", "State directory (default: ~/.claude-proxy)")
	verifyCommand := fs.String("verify", "", "Optional verification command to run after the worker")
	responseLang := fs.String("lang", "auto", "Response language hint")
	_ = responseLang // reserved for future per-worker output shaping
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "worker: %v\n", err)
		os.Exit(2)
	}

	cfg := config.Load()
	registerWorkerCustomProviders(cfg)

	if *providerName == "" {
		*providerName = cfg.DefaultProvider
	}
	if *providerName == "" {
		*providerName = "ollama"
	}
	if *model == "" {
		*model = cfg.DefaultModel
	}
	if *model == "" {
		*model = "qwen3:8b"
	}
	if *workDir == "" {
		*workDir = cfg.WorkDir
	}
	if *workDir == "" {
		wd, _ := os.Getwd()
		*workDir = wd
	}
	if *baseDir == "" {
		home, _ := os.UserHomeDir()
		*baseDir = filepath.Join(home, ".claude-proxy")
	}

	task, err := workerTask(*taskPath, *prompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker: %v\n", err)
		os.Exit(2)
	}
	workerObjective := strings.TrimSpace(task.Goal)
	if workerObjective == "" {
		workerObjective = strings.TrimSpace(task.Description)
	}
	workerPlan := agent.TeamPlan{
		Version:       1,
		Name:          "cli-worker",
		Objective:     workerObjective,
		VerifyCommand: *verifyCommand,
		Capacity:      agent.DefaultLocalCapacity(),
		Tasks:         []agent.AgentTask{task},
	}

	provider, err := providers.Create(*providerName, &types.ProviderConfig{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker: %v\n", err)
		os.Exit(2)
	}
	if err := provider.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "worker warning: %v\n", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	team := agent.NewTeam(provider, *model, *workDir, *baseDir, agent.TeamConfig{
		Name:            "cli-worker",
		MaxWaveSize:     1,
		VerifyCommand:   *verifyCommand,
		Capacity:        agent.DefaultLocalCapacity(),
		ProviderFactory: createProviderByName,
	})
	team.AddTask(task.ToTeamTask())

	eventCh := make(chan agent.Event, 64)
	go func() {
		defer close(eventCh)
		verification := agent.ReceiptVerification{Status: "not-run", Source: "none"}
		if err := team.ExecuteWaves(ctx, eventCh); err != nil {
			eventCh <- agent.Event{Type: "error", Data: err.Error()}
			emitTeamReceipt(eventCh, team, workerPlan, *baseDir, *workDir, "failed", verification)
			return
		}
		if *verifyCommand != "" {
			passed, detail := team.Verify(ctx)
			if passed {
				verification = agent.ReceiptVerification{Status: "passed", Source: "team-verify"}
				eventCh <- agent.Event{Type: "status", Data: "Verification PASSED"}
			} else {
				verification = agent.ReceiptVerification{Status: "failed", Source: "team-verify"}
				eventCh <- agent.Event{Type: "error", Data: "Verification FAILED: " + detail}
				emitTeamReceipt(eventCh, team, workerPlan, *baseDir, *workDir, "failed", verification)
				return
			}
		}
		eventCh <- agent.Event{Type: "text", Data: "\n\n" + team.Summary()}
		emitTeamReceipt(eventCh, team, workerPlan, *baseDir, *workDir, "completed", verification)
		eventCh <- agent.Event{Type: "done", Data: nil}
	}()

	failed := false
	for event := range eventCh {
		switch event.Type {
		case "text":
			fmt.Print(event.Data)
		case "error":
			failed = true
			fmt.Fprintf(os.Stderr, "[error] %v\n", event.Data)
		case "status":
			fmt.Fprintf(os.Stderr, "[status] %v\n", event.Data)
		case "tool_start":
			if data, ok := event.Data.(map[string]string); ok {
				fmt.Fprintf(os.Stderr, "[tool] %s\n", data["name"])
			}
		case "tool_result":
			fmt.Fprintln(os.Stderr, "[tool-result]")
		case "done":
			// no-op
		}
	}
	team.Shutdown()

	if failed {
		os.Exit(1)
	}
}

func workerTask(taskPath, prompt string) (agent.AgentTask, error) {
	if strings.TrimSpace(taskPath) != "" {
		return agent.LoadAgentTask(taskPath)
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return agent.AgentTask{}, fmt.Errorf("--task or --prompt is required")
	}
	return agent.AgentTask{
		ID:          "inline",
		Name:        "Inline task",
		Kind:        agent.TaskKindImplement,
		Role:        "coder",
		Goal:        prompt,
		Description: prompt,
	}, nil
}

func registerWorkerCustomProviders(cfg config.Config) {
	for name, settings := range cfg.Providers {
		if isBuiltinProviderName(name) || settings.BaseURL == "" {
			continue
		}
		providers.RegisterCustomProvider(name, &types.ProviderConfig{
			APIKey:  settings.APIKey,
			BaseURL: settings.BaseURL,
		})
	}
}

func isBuiltinProviderName(name string) bool {
	for _, builtin := range []string{"anthropic", "openai", "gemini", "groq", "ollama", "sglang", "github-copilot", "zai"} {
		if name == builtin {
			return true
		}
	}
	return false
}

func createProviderByName(name string) (types.Provider, error) {
	return providers.Create(name, &types.ProviderConfig{})
}
