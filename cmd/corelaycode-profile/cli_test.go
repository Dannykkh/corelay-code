package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/capabilityprofile"
	"github.com/Dannykkh/corelay-code/internal/config"
	"github.com/Dannykkh/corelay-code/internal/providers"
	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestRunFailsClosedBeforeProviderValidationOrStreamWithoutIsolation(t *testing.T) {
	const rawEndpoint = "https://private-profile-endpoint.invalid/tenant-secret"
	const rawKey = "sk-profile-secret-never-print"
	provider := &profileTestProvider{name: "profile-test"}
	cfg := config.Config{
		DefaultProvider: provider.name,
		DefaultModel:    "profile-model",
		Providers: map[string]config.ProviderSettings{
			provider.name: {APIKey: rawKey, BaseURL: rawEndpoint},
		},
	}
	dependencies := cliDependencies{
		loadConfig:     func() config.Config { return cfg },
		createProvider: func(string, *types.ProviderConfig) (types.Provider, error) { return provider, nil },
		createTarget: func(_ context.Context, _ string, cfg *types.ProviderConfig) (targetBoundProvider, error) {
			return targetBoundProvider{provider: provider, proof: testTargetBindingProof(cfg.BaseURL)}, nil
		},
		createIsolation: func(context.Context, capabilityprofile.TargetIdentity, providers.TargetBindingProof) (capabilityprofile.IsolationProvisioner, error) {
			return nil, capabilityprofile.ErrIsolationUnavailable
		},
		clock: time.Now,
	}
	var stdout, stderr bytes.Buffer
	code := runCLI(context.Background(), []string{
		"run", "--confirm", "--store", t.TempDir(),
	}, &stdout, &stderr, dependencies)
	if code != 1 {
		t.Fatalf("code = %d, stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if provider.validateCalls != 0 || provider.streamCalls != 0 {
		t.Fatalf("provider calls before proof: validate=%d stream=%d", provider.validateCalls, provider.streamCalls)
	}
	combined := stdout.String() + stderr.String()
	if strings.Contains(combined, rawEndpoint) || strings.Contains(combined, rawKey) {
		t.Fatalf("CLI leaked raw target data: %q", combined)
	}
	if !strings.Contains(stderr.String(), "blocked") {
		t.Fatalf("missing fail-closed diagnostic: %q", stderr.String())
	}
}

func TestDryRunListAndStatusAreSafeAndDoNotCallProvider(t *testing.T) {
	const rawEndpoint = "https://private-profile-endpoint.invalid/hidden"
	const rawKey = "sk-profile-secret-never-print"
	provider := &profileTestProvider{name: "profile-test"}
	store := filepath.Join(t.TempDir(), "absent-profile-store")
	cfg := config.Config{
		DefaultProvider: provider.name,
		DefaultModel:    "profile-model",
		Providers: map[string]config.ProviderSettings{
			provider.name: {APIKey: rawKey, BaseURL: rawEndpoint},
		},
	}
	dependencies := cliDependencies{
		loadConfig:     func() config.Config { return cfg },
		createProvider: func(string, *types.ProviderConfig) (types.Provider, error) { return provider, nil },
		createTarget: func(_ context.Context, _ string, cfg *types.ProviderConfig) (targetBoundProvider, error) {
			return targetBoundProvider{provider: provider, proof: testTargetBindingProof(cfg.BaseURL)}, nil
		},
		createIsolation: func(context.Context, capabilityprofile.TargetIdentity, providers.TargetBindingProof) (capabilityprofile.IsolationProvisioner, error) {
			return nil, errors.New("not used")
		},
		clock: time.Now,
	}
	for _, command := range []string{"dry-run", "list", "status"} {
		t.Run(command, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runCLI(context.Background(), []string{command, "--store", store}, &stdout, &stderr, dependencies)
			if code != 0 {
				t.Fatalf("code = %d, stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			combined := stdout.String() + stderr.String()
			if strings.Contains(combined, rawEndpoint) || strings.Contains(combined, rawKey) {
				t.Fatalf("%s leaked raw target data: %q", command, combined)
			}
			if command == "dry-run" && (!strings.Contains(stdout.String(), `"runnable": true`) ||
				!strings.Contains(stdout.String(), capabilityprofile.DefaultProbePlanVersion)) {
				t.Fatalf("dry-run output = %q", stdout.String())
			}
		})
	}
	if provider.validateCalls != 0 || provider.streamCalls != 0 {
		t.Fatalf("safe commands called provider: validate=%d stream=%d", provider.validateCalls, provider.streamCalls)
	}
	if _, err := os.Stat(store); !os.IsNotExist(err) {
		t.Fatalf("dry-run/list/status created the absent store: %v", err)
	}
}

func TestDryRunUsesExplicitTargetFlagsInsteadOfSavedDefaults(t *testing.T) {
	provider := &profileTestProvider{name: "explicit-provider"}
	dependencies := cliDependencies{
		loadConfig: func() config.Config {
			return config.Config{DefaultProvider: "saved-provider", DefaultModel: "saved-model", Providers: map[string]config.ProviderSettings{}}
		},
		createProvider: func(name string, _ *types.ProviderConfig) (types.Provider, error) {
			if name != "explicit-provider" {
				t.Fatalf("provider factory name = %q", name)
			}
			return provider, nil
		},
		createTarget: func(_ context.Context, _ string, cfg *types.ProviderConfig) (targetBoundProvider, error) {
			return targetBoundProvider{provider: provider, proof: testTargetBindingProof(cfg.BaseURL)}, nil
		},
		createIsolation: func(context.Context, capabilityprofile.TargetIdentity, providers.TargetBindingProof) (capabilityprofile.IsolationProvisioner, error) {
			return nil, capabilityprofile.ErrIsolationUnavailable
		},
		clock: time.Now,
	}
	var stdout, stderr bytes.Buffer
	code := runCLI(context.Background(), []string{
		"dry-run", "--provider", "explicit-provider", "--model", "explicit-model",
		"--endpoint", "https://explicit-endpoint.invalid/private", "--store", filepath.Join(t.TempDir(), "profiles"),
	}, &stdout, &stderr, dependencies)
	if code != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), `"provider": "explicit-provider"`) ||
		!strings.Contains(stdout.String(), `"model": "explicit-model"`) ||
		strings.Contains(stdout.String(), "explicit-endpoint.invalid") || strings.Contains(stdout.String(), "saved-provider") {
		t.Fatalf("dry-run target output = %q", stdout.String())
	}
}

func testTargetBindingProof(endpoint string) providers.TargetBindingProof {
	endpointSum := sha256.Sum256([]byte(endpoint))
	return providers.TargetBindingProof{
		Version:        providers.TargetBindingProofVersion,
		Scope:          providers.TargetBindingScope,
		EndpointDigest: hex.EncodeToString(endpointSum[:]),
		TargetDigest:   digestProfileFields("transport-target"),
		HostHash:       digestProfileFields("host"),
		IPSetHash:      digestProfileFields("ip-set"),
		PathHash:       digestProfileFields("path"),
		AddressCount:   1,
	}
}

func TestSafeCLIStorePathRejectsBroadTargets(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.VolumeName(cwd) + string(filepath.Separator)
	if _, err := safeCLIStorePath(root); !errors.Is(err, capabilityprofile.ErrUnsafeStorePath) {
		t.Fatalf("root error = %v, want ErrUnsafeStorePath", err)
	}
	if home, err := os.UserHomeDir(); err == nil {
		if _, err := safeCLIStorePath(home); !errors.Is(err, capabilityprofile.ErrUnsafeStorePath) {
			t.Fatalf("home error = %v, want ErrUnsafeStorePath", err)
		}
	}
	want := filepath.Join(t.TempDir(), "profiles")
	got, err := safeCLIStorePath(want)
	if err != nil {
		t.Fatalf("safeCLIStorePath() error = %v", err)
	}
	if !samePathText(got, want) {
		t.Fatalf("safeCLIStorePath() = %q, want %q", got, want)
	}
}
