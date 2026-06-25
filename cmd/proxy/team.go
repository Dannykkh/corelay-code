package main

import (
	"context"
	"encoding/json"
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

func runTeam(args []string) {
	if len(args) == 0 {
		runTeamRun(args)
		return
	}
	switch args[0] {
	case "run":
		runTeamRun(args[1:])
	case "plan":
		runTeamPlan(args[1:])
	case "validate":
		runTeamValidate(args[1:])
	default:
		runTeamRun(args)
	}
}

func runTeamRun(args []string) {
	if len(args) > 0 && args[0] == "run" {
		args = args[1:]
	}

	fs := flag.NewFlagSet("team run", flag.ExitOnError)
	planPath := fs.String("plan", "", "Path to a TeamPlan JSON file")
	objective := fs.String("objective", "", "Inline objective used to generate a basic Daedalus TeamPlan")
	name := fs.String("name", "daedalus-plan", "Generated plan name when --objective is used")
	providerName := fs.String("provider", "", "Provider name (default: plan, saved config, or ollama)")
	model := fs.String("model", "", "Model ID (default: plan, saved config, or qwen3:8b)")
	workDir := fs.String("workdir", "", "Workspace directory (default: saved config or cwd)")
	baseDir := fs.String("base-dir", "", "State directory (default: ~/.claude-proxy)")
	verifyCommand := fs.String("verify", "", "Override or set plan verification command")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "team: %v\n", err)
		os.Exit(2)
	}

	plan, err := teamPlan(*planPath, *name, *objective)
	if err != nil {
		fmt.Fprintf(os.Stderr, "team: %v\n", err)
		os.Exit(2)
	}
	if *verifyCommand != "" {
		plan.VerifyCommand = *verifyCommand
	}
	if err := agent.ValidateTeamPlan(plan); err != nil {
		fmt.Fprintf(os.Stderr, "team: %v\n", err)
		os.Exit(2)
	}

	cfg := config.Load()
	registerWorkerCustomProviders(cfg)

	if *providerName == "" {
		*providerName = planPreferredProvider(plan)
	}
	if *providerName == "" {
		*providerName = cfg.DefaultProvider
	}
	if *providerName == "" {
		*providerName = "ollama"
	}
	if *model == "" {
		*model = planPreferredModel(plan)
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

	provider, err := providers.Create(*providerName, &types.ProviderConfig{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "team: %v\n", err)
		os.Exit(2)
	}
	if err := provider.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "team warning: %v\n", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	runner := agent.NewTeam(provider, *model, *workDir, *baseDir, agent.TeamConfig{
		Name:            plan.Name,
		VerifyCommand:   plan.VerifyCommand,
		Capacity:        plan.Capacity,
		ProviderFactory: createProviderByName,
	})
	for _, task := range plan.ToTeamTasks() {
		runner.AddTask(task)
	}

	eventCh := make(chan agent.Event, 64)
	go func() {
		defer close(eventCh)
		verification := agent.ReceiptVerification{Status: "not-run", Source: "none"}
		eventCh <- agent.Event{Type: "status", Data: fmt.Sprintf("TeamPlan %q: %d task(s)", plan.Name, len(plan.Tasks))}
		if err := runner.ExecuteWaves(ctx, eventCh); err != nil {
			eventCh <- agent.Event{Type: "error", Data: err.Error()}
			emitTeamReceipt(eventCh, runner, plan, *baseDir, *workDir, "failed", verification)
			return
		}
		if plan.VerifyCommand != "" {
			passed, detail := runner.Verify(ctx)
			if passed {
				verification = agent.ReceiptVerification{Status: "passed", Source: "team-verify"}
				eventCh <- agent.Event{Type: "status", Data: "Verification PASSED"}
			} else {
				verification = agent.ReceiptVerification{Status: "failed", Source: "team-verify"}
				eventCh <- agent.Event{Type: "error", Data: "Verification FAILED: " + detail}
				emitTeamReceipt(eventCh, runner, plan, *baseDir, *workDir, "failed", verification)
				return
			}
		}
		eventCh <- agent.Event{Type: "text", Data: "\n\n" + runner.Summary()}
		emitTeamReceipt(eventCh, runner, plan, *baseDir, *workDir, "completed", verification)
		eventCh <- agent.Event{Type: "done", Data: nil}
	}()

	failed := printAgentEvents(eventCh)
	runner.Shutdown()
	if failed {
		os.Exit(1)
	}
}

func runTeamPlan(args []string) {
	fs := flag.NewFlagSet("team plan", flag.ExitOnError)
	objective := fs.String("objective", "", "Objective used to generate a basic Daedalus TeamPlan")
	name := fs.String("name", "daedalus-plan", "Generated plan name")
	outPath := fs.String("out", "", "Optional path to write TeamPlan JSON")
	verifyCommand := fs.String("verify", "", "Optional verification command")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "team: %v\n", err)
		os.Exit(2)
	}

	if strings.TrimSpace(*objective) == "" {
		fmt.Fprintln(os.Stderr, "team: --objective is required")
		os.Exit(2)
	}
	plan := agent.NewDaedalusTeamPlan(*name, *objective)
	if *verifyCommand != "" {
		plan.VerifyCommand = *verifyCommand
	}
	if err := agent.ValidateTeamPlan(plan); err != nil {
		fmt.Fprintf(os.Stderr, "team: %v\n", err)
		os.Exit(2)
	}

	if strings.TrimSpace(*outPath) != "" {
		if err := agent.SaveTeamPlan(*outPath, plan); err != nil {
			fmt.Fprintf(os.Stderr, "team: write plan: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "TeamPlan written: %s\n", *outPath)
		return
	}
	data, err := json.MarshalIndent(plan.Normalized(), "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "team: marshal plan: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(data))
}

func runTeamValidate(args []string) {
	fs := flag.NewFlagSet("team validate", flag.ExitOnError)
	planPath := fs.String("plan", "", "Path to a TeamPlan JSON file")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "team: %v\n", err)
		os.Exit(2)
	}
	if strings.TrimSpace(*planPath) == "" {
		fmt.Fprintln(os.Stderr, "team: --plan is required")
		os.Exit(2)
	}
	plan, err := agent.LoadTeamPlan(*planPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "team: %v\n", err)
		os.Exit(2)
	}
	if err := agent.ValidateTeamPlan(plan); err != nil {
		fmt.Fprintf(os.Stderr, "team: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "TeamPlan valid: %s (%d task(s))\n", plan.Name, len(plan.Tasks))
}

func teamPlan(planPath, name, objective string) (agent.TeamPlan, error) {
	if strings.TrimSpace(planPath) != "" {
		return agent.LoadTeamPlan(planPath)
	}
	objective = strings.TrimSpace(objective)
	if objective == "" {
		return agent.TeamPlan{}, fmt.Errorf("--plan or --objective is required")
	}
	return agent.NewDaedalusTeamPlan(name, objective), nil
}

func planPreferredProvider(plan agent.TeamPlan) string {
	for _, agentSpec := range plan.Agents {
		if strings.TrimSpace(agentSpec.Provider) != "" {
			return strings.TrimSpace(agentSpec.Provider)
		}
	}
	for _, task := range plan.Tasks {
		if strings.TrimSpace(task.Provider) != "" {
			return strings.TrimSpace(task.Provider)
		}
	}
	return ""
}

func planPreferredModel(plan agent.TeamPlan) string {
	for _, agentSpec := range plan.Agents {
		if strings.TrimSpace(agentSpec.Model) != "" {
			return strings.TrimSpace(agentSpec.Model)
		}
	}
	for _, task := range plan.Tasks {
		if strings.TrimSpace(task.Model) != "" {
			return strings.TrimSpace(task.Model)
		}
	}
	return ""
}

func printAgentEvents(eventCh <-chan agent.Event) bool {
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
	return failed
}

func emitTeamReceipt(eventCh chan<- agent.Event, team *agent.Team, plan agent.TeamPlan, baseDir, workDir, status string, verification agent.ReceiptVerification) {
	receipt := team.BuildRunReceipt(plan, status, verification)
	path, err := agent.WriteTeamRunReceipt(baseDir, workDir, receipt)
	if err != nil {
		eventCh <- agent.Event{Type: "status", Data: "Team receipt write failed: " + err.Error()}
		return
	}
	eventCh <- agent.Event{Type: "status", Data: "Team receipt saved: " + path}
}
