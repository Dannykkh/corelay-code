package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Dannykkh/corelay-code/internal/capabilityprofile"
	"github.com/Dannykkh/corelay-code/internal/config"
	"github.com/Dannykkh/corelay-code/internal/providers"
	"github.com/Dannykkh/corelay-code/internal/types"
)

const defaultProfileRunTimeout = 4 * time.Hour

type providerFactory func(string, *types.ProviderConfig) (types.Provider, error)

type targetBoundProvider struct {
	provider types.Provider
	proof    providers.TargetBindingProof
	close    func()
}

func (p targetBoundProvider) Close() {
	if p.close != nil {
		p.close()
	}
}

type targetBoundProviderFactory func(
	context.Context,
	string,
	*types.ProviderConfig,
) (targetBoundProvider, error)

type isolationProvisionerFactory func(
	context.Context,
	capabilityprofile.TargetIdentity,
	providers.TargetBindingProof,
) (capabilityprofile.IsolationProvisioner, error)

type cliDependencies struct {
	loadConfig      func() config.Config
	createProvider  providerFactory
	createTarget    targetBoundProviderFactory
	createIsolation isolationProvisionerFactory
	clock           func() time.Time
}

func defaultCLIDependencies() cliDependencies {
	return cliDependencies{
		loadConfig:     config.Load,
		createProvider: providers.Create,
		createTarget: func(ctx context.Context, name string, cfg *types.ProviderConfig) (targetBoundProvider, error) {
			if cfg == nil {
				return targetBoundProvider{}, capabilityprofile.ErrIsolationUnavailable
			}
			client, proof, err := providers.NewTargetBoundHTTPClient(ctx, cfg.BaseURL)
			if err != nil {
				return targetBoundProvider{}, capabilityprofile.ErrIsolationUnavailable
			}
			provider, err := providers.CreateWithOptions(name, cfg, providers.CreateOptions{HTTPDoer: client})
			if err != nil || provider == nil {
				client.CloseIdleConnections()
				return targetBoundProvider{}, capabilityprofile.ErrIsolationUnavailable
			}
			return targetBoundProvider{
				provider: provider,
				proof:    proof,
				close:    client.CloseIdleConnections,
			}, nil
		},
		createIsolation: func(_ context.Context, target capabilityprofile.TargetIdentity, proof providers.TargetBindingProof) (capabilityprofile.IsolationProvisioner, error) {
			return newKernelIsolationProvisioner(target, proof)
		},
		clock: time.Now,
	}
}

type targetOptions struct {
	provider string
	model    string
	endpoint string
	store    string
}

type resolvedTarget struct {
	identity       capabilityprofile.TargetIdentity
	providerName   string
	providerConfig types.ProviderConfig
	model          string
	endpoint       string
	store          string
}

func runCLI(ctx context.Context, args []string, stdout, stderr io.Writer, dependencies cliDependencies) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}
	if dependencies.loadConfig == nil || dependencies.createProvider == nil || dependencies.createTarget == nil ||
		dependencies.createIsolation == nil || dependencies.clock == nil {
		fmt.Fprintln(stderr, "corelaycode-profile: runtime composition is unavailable")
		return 1
	}
	switch args[0] {
	case "dry-run":
		return runDryRun(args[1:], stdout, stderr, dependencies)
	case "list":
		return runList(args[1:], stdout, stderr, dependencies)
	case "status":
		return runStatus(args[1:], stdout, stderr, dependencies)
	case "run":
		return runProfile(ctx, args[1:], stdout, stderr, dependencies)
	case "compare":
		return runCompare(args[1:], stdout, stderr, dependencies)
	case "help", "-h", "--help":
		printUsage(stdout)
		return 0
	default:
		fmt.Fprintln(stderr, "corelaycode-profile: unknown command")
		printUsage(stderr)
		return 2
	}
}

func runDryRun(args []string, stdout, stderr io.Writer, dependencies cliDependencies) int {
	flags := flag.NewFlagSet("dry-run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	options := bindTargetFlags(flags, dependencies.loadConfig())
	variant := flags.String("variant", string(capabilityprofile.HarnessVariantCorelay), "harness variant: corelay or minimal")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return 2
	}
	resolved, err := resolveTarget(*options, dependencies)
	if err != nil {
		fmt.Fprintln(stderr, "corelaycode-profile: exact target identity is unavailable")
		return 1
	}
	plan, err := resolveProbePlan(*variant)
	if err != nil {
		fmt.Fprintln(stderr, "corelaycode-profile: harness variant is invalid")
		return 2
	}
	manifest, err := capabilityprofile.DescribeRun(resolved.identity, plan)
	if err != nil {
		fmt.Fprintln(stderr, "corelaycode-profile: probe plan is invalid")
		return 1
	}
	output := struct {
		Target   capabilityprofile.TargetSnapshot `json:"target"`
		Plan     capabilityprofile.RunManifest    `json:"plan"`
		Runnable bool                             `json:"runnable"`
		Blocker  string                           `json:"blocker,omitempty"`
	}{
		Target: resolved.identity.Snapshot(), Plan: manifest,
		Runnable: true,
	}
	return writeJSON(stdout, output)
}

func runList(args []string, stdout, stderr io.Writer, dependencies cliDependencies) int {
	options, ok := parseTargetOptions("list", args, stderr, dependencies.loadConfig())
	if !ok {
		return 2
	}
	resolved, err := resolveTarget(options, dependencies)
	if err != nil {
		fmt.Fprintln(stderr, "corelaycode-profile: exact target identity is unavailable")
		return 1
	}
	profiles, _, err := loadProfileInventory(resolved.store, resolved.identity)
	if err != nil {
		fmt.Fprintln(stderr, "corelaycode-profile: profile inventory is unavailable")
		return 1
	}
	return writeJSON(stdout, struct {
		TargetDigest string                            `json:"targetDigest"`
		Profiles     []capabilityprofile.ProfileStatus `json:"profiles"`
	}{TargetDigest: resolved.identity.Digest(), Profiles: profiles})
}

func runStatus(args []string, stdout, stderr io.Writer, dependencies cliDependencies) int {
	options, ok := parseTargetOptions("status", args, stderr, dependencies.loadConfig())
	if !ok {
		return 2
	}
	resolved, err := resolveTarget(options, dependencies)
	if err != nil {
		fmt.Fprintln(stderr, "corelaycode-profile: exact target identity is unavailable")
		return 1
	}
	profiles, store, err := loadProfileInventory(resolved.store, resolved.identity)
	if err != nil {
		fmt.Fprintln(stderr, "corelaycode-profile: profile inventory is unavailable")
		return 1
	}
	output := struct {
		TargetDigest      string                               `json:"targetDigest"`
		Selectable        bool                                 `json:"selectable"`
		SelectedProfileID string                               `json:"selectedProfileId,omitempty"`
		Reasons           []capabilityprofile.QuarantineReason `json:"reasons,omitempty"`
		Profiles          []capabilityprofile.ProfileStatus    `json:"profiles"`
	}{TargetDigest: resolved.identity.Digest(), Profiles: profiles}
	if store == nil {
		return writeJSON(stdout, output)
	}
	selected, selectErr := store.AutoSelect(resolved.identity, dependencies.clock())
	if selectErr == nil {
		output.Selectable = true
		output.SelectedProfileID = selected.ID()
	} else {
		var typed *capabilityprofile.SelectionError
		if !errors.As(selectErr, &typed) {
			fmt.Fprintln(stderr, "corelaycode-profile: profile selection status is unavailable")
			return 1
		}
		output.Reasons = append([]capabilityprofile.QuarantineReason(nil), typed.Reasons...)
	}
	return writeJSON(stdout, output)
}

func loadProfileInventory(
	path string,
	target capabilityprofile.TargetIdentity,
) ([]capabilityprofile.ProfileStatus, *capabilityprofile.Store, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return []capabilityprofile.ProfileStatus{}, nil, nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, nil, capabilityprofile.ErrUnsafeStorePath
	}
	store, err := capabilityprofile.NewStore(path)
	if err != nil {
		return nil, nil, err
	}
	profiles, err := store.List(target)
	if err != nil {
		return nil, nil, err
	}
	return profiles, store, nil
}

func runProfile(ctx context.Context, args []string, stdout, stderr io.Writer, dependencies cliDependencies) int {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	cfg := dependencies.loadConfig()
	options := bindTargetFlags(flags, cfg)
	variant := flags.String("variant", string(capabilityprofile.HarnessVariantCorelay), "harness variant: corelay or minimal")
	confirm := flags.Bool("confirm", false, "confirm the bounded but potentially costly empirical probe run")
	measurementOnly := flags.Bool("measurement-only", false, "return success after publishing a quarantined comparison input; never makes it selectable")
	timeout := flags.Duration("timeout", defaultProfileRunTimeout, "total profiling timeout")
	probeTimeout := flags.Duration("probe-timeout", 5*time.Minute, "timeout for each probe attempt")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return 2
	}
	if !*confirm {
		fmt.Fprintln(stderr, "corelaycode-profile: run requires --confirm; inspect dry-run first")
		return 2
	}
	if *timeout <= 0 || *timeout > 24*time.Hour || *probeTimeout <= 0 || *probeTimeout > time.Hour {
		fmt.Fprintln(stderr, "corelaycode-profile: timeout is outside the supported bounds")
		return 2
	}
	resolved, err := resolveTarget(*options, dependencies)
	if err != nil {
		fmt.Fprintln(stderr, "corelaycode-profile: exact target identity is unavailable")
		return 1
	}
	plan, err := resolveProbePlan(*variant)
	if err != nil {
		fmt.Fprintln(stderr, "corelaycode-profile: harness variant is invalid")
		return 2
	}
	bound, err := dependencies.createTarget(ctx, resolved.providerName, &resolved.providerConfig)
	if err != nil || bound.provider == nil || !validTargetBindingProof(bound.proof) ||
		bound.proof.EndpointDigest != resolved.identity.EndpointDigest() ||
		bound.provider.Name() != resolved.identity.Provider() {
		bound.Close()
		fmt.Fprintln(stderr, "corelaycode-profile: target-bound provider transport is unavailable")
		return 1
	}
	defer bound.Close()
	executor, err := newAgentProbeExecutor(bound.provider, resolved.model, resolved.identity, *probeTimeout)
	if err != nil {
		fmt.Fprintln(stderr, "corelaycode-profile: agent probe executor is unavailable")
		return 1
	}
	provisioner, err := dependencies.createIsolation(ctx, resolved.identity, bound.proof)
	if err != nil || provisioner == nil {
		fmt.Fprintln(stderr, "corelaycode-profile: blocked because target-bound filesystem and outbound isolation is unavailable")
		return 1
	}
	workspaceRoot := filepath.Join(filepath.Dir(resolved.store), "capability-profile-workspaces")
	workspaces, err := capabilityprofile.NewDisposableWorkspaceFactory(workspaceRoot, provisioner)
	if err != nil {
		fmt.Fprintln(stderr, "corelaycode-profile: isolated workspace composition failed")
		return 1
	}
	store, err := capabilityprofile.NewStore(resolved.store)
	if err != nil {
		fmt.Fprintln(stderr, "corelaycode-profile: profile store is unavailable")
		return 1
	}
	profiler, err := capabilityprofile.NewProfiler(workspaces, executor, capabilityprofile.ProfilerConfig{})
	if err != nil {
		fmt.Fprintln(stderr, "corelaycode-profile: profiler composition failed")
		return 1
	}
	runner, err := capabilityprofile.NewRunner(profiler, store, plan)
	if err != nil {
		fmt.Fprintln(stderr, "corelaycode-profile: runtime composition failed")
		return 1
	}
	runCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	// Each executor attempt scopes memory, skills, verification, MCP, plugins,
	// hooks, HOME and config to its disposable lease. Do not mutate process-wide
	// environment here: runCLI is also exercised in-process by embedders and
	// tests, and a successful or failed profile run must leave their state intact.
	previousLogWriter := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(previousLogWriter)
	result, err := runner.Run(runCtx, resolved.identity)
	if err != nil {
		fmt.Fprintln(stderr, "corelaycode-profile: profiling did not publish a profile")
		return 1
	}
	output := struct {
		Ref     capabilityprofile.ProfileRef      `json:"ref"`
		Profile capabilityprofile.ProfileSnapshot `json:"profile"`
	}{Ref: result.Ref, Profile: result.Profile.Snapshot()}
	if code := writeJSON(stdout, output); code != 0 {
		return code
	}
	return profilePublicationExitCode(result.Profile.Verified(), *measurementOnly)
}

func profilePublicationExitCode(verified, measurementOnly bool) int {
	if !verified && !measurementOnly {
		return 1
	}
	return 0
}

func parseTargetOptions(command string, args []string, stderr io.Writer, cfg config.Config) (targetOptions, bool) {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	options := bindTargetFlags(flags, cfg)
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return targetOptions{}, false
	}
	return *options, true
}

func bindTargetFlags(flags *flag.FlagSet, cfg config.Config) *targetOptions {
	options := &targetOptions{}
	flags.StringVar(&options.provider, "provider", cfg.DefaultProvider, "provider name")
	flags.StringVar(&options.model, "model", cfg.DefaultModel, "model ID")
	flags.StringVar(&options.endpoint, "endpoint", "", "exact provider endpoint (never persisted raw)")
	flags.StringVar(&options.store, "store", config.CapabilityProfileDir(), "immutable profile store")
	return options
}

func resolveProbePlan(value string) (capabilityprofile.ProbePlan, error) {
	variant, err := capabilityprofile.ParseHarnessVariant(value)
	if err != nil {
		return capabilityprofile.ProbePlan{}, err
	}
	return capabilityprofile.ProbePlanForVariant(variant)
}

func runCompare(args []string, stdout, stderr io.Writer, dependencies cliDependencies) int {
	flags := flag.NewFlagSet("compare", flag.ContinueOnError)
	flags.SetOutput(stderr)
	options := bindTargetFlags(flags, dependencies.loadConfig())
	baselineID := flags.String("baseline", "", "immutable baseline profile ID")
	candidateID := flags.String("candidate", "", "immutable candidate profile ID")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 ||
		strings.TrimSpace(*baselineID) == "" || strings.TrimSpace(*candidateID) == "" {
		return 2
	}
	resolved, err := resolveTarget(*options, dependencies)
	if err != nil {
		fmt.Fprintln(stderr, "corelaycode-profile: exact target identity is unavailable")
		return 1
	}
	_, store, err := loadProfileInventory(resolved.store, resolved.identity)
	if err != nil || store == nil {
		fmt.Fprintln(stderr, "corelaycode-profile: comparison profiles are unavailable")
		return 1
	}
	baseline, err := store.Load(resolved.identity, strings.TrimSpace(*baselineID))
	if err != nil {
		fmt.Fprintln(stderr, "corelaycode-profile: comparison profiles are unavailable")
		return 1
	}
	candidate, err := store.Load(resolved.identity, strings.TrimSpace(*candidateID))
	if err != nil {
		fmt.Fprintln(stderr, "corelaycode-profile: comparison profiles are unavailable")
		return 1
	}
	report, err := capabilityprofile.CompareProfiles(baseline, candidate)
	if err != nil {
		fmt.Fprintln(stderr, "corelaycode-profile: profiles are not comparable")
		return 1
	}
	return writeJSON(stdout, report)
}

func resolveTarget(options targetOptions, dependencies cliDependencies) (resolvedTarget, error) {
	cfg := dependencies.loadConfig()
	registerCLIProviders(cfg)
	providerName := strings.TrimSpace(options.provider)
	model := strings.TrimSpace(options.model)
	if providerName == "" || model == "" {
		return resolvedTarget{}, capabilityprofile.ErrInvalidTarget
	}
	settings := cfg.Providers[providerName]
	endpoint := strings.TrimSpace(options.endpoint)
	if endpoint == "" {
		endpoint = strings.TrimSpace(settings.BaseURL)
	}
	provider, err := dependencies.createProvider(providerName, &types.ProviderConfig{
		APIKey: settings.APIKey, BaseURL: endpoint,
	})
	if err != nil || provider == nil {
		return resolvedTarget{}, capabilityprofile.ErrInvalidTarget
	}
	if endpoint == "" {
		if compatible, ok := provider.(*providers.OpenAICompat); ok {
			endpoint = strings.TrimSpace(compatible.BaseURL)
		}
	}
	if endpoint == "" {
		return resolvedTarget{}, capabilityprofile.ErrInvalidTarget
	}
	target, err := capabilityprofile.NewTargetIdentity(capabilityprofile.TargetSpec{
		Provider: providerName, Model: model, Endpoint: endpoint,
	})
	if err != nil {
		return resolvedTarget{}, err
	}
	store, err := safeCLIStorePath(options.store)
	if err != nil {
		return resolvedTarget{}, err
	}
	return resolvedTarget{
		identity: target, providerName: providerName,
		providerConfig: types.ProviderConfig{APIKey: settings.APIKey, BaseURL: endpoint},
		model:          model, endpoint: endpoint,
		store: store,
	}, nil
}

func safeCLIStorePath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", capabilityprofile.ErrUnsafeStorePath
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", capabilityprofile.ErrUnsafeStorePath
	}
	absolute = filepath.Clean(absolute)
	if samePathText(filepath.Dir(absolute), absolute) {
		return "", capabilityprofile.ErrUnsafeStorePath
	}
	if home, homeErr := os.UserHomeDir(); homeErr == nil && samePathText(filepath.Clean(home), absolute) {
		return "", capabilityprofile.ErrUnsafeStorePath
	}
	return absolute, nil
}

func samePathText(left, right string) bool {
	return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
}

func registerCLIProviders(cfg config.Config) {
	builtins := map[string]struct{}{
		"anthropic": {}, "openai": {}, "gemini": {}, "groq": {},
		"ollama": {}, "sglang": {}, "github-copilot": {}, "zai": {},
	}
	for name, settings := range cfg.Providers {
		if _, builtin := builtins[name]; builtin || strings.TrimSpace(name) == "" || strings.TrimSpace(settings.BaseURL) == "" {
			continue
		}
		providers.RegisterCustomProvider(name, &types.ProviderConfig{APIKey: settings.APIKey, BaseURL: settings.BaseURL})
	}
}

func writeJSON(writer io.Writer, value any) int {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return 1
	}
	return 0
}

func printUsage(writer io.Writer) {
	fmt.Fprintln(writer, "usage: corelaycode-profile <dry-run|list|status|run|compare> [options]")
	fmt.Fprintln(writer, "run requires --confirm and a target-bound provider transport")
}
