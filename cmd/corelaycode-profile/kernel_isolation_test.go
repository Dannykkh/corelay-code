package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/capabilityprofile"
	"github.com/Dannykkh/corelay-code/internal/providers"
	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestKernelIsolationProvisionerBindsExactTargetAndTransportEvidence(t *testing.T) {
	const rawEndpoint = "https://private-profile-target.invalid/tenant-secret"
	target, err := capabilityprofile.NewTargetIdentity(capabilityprofile.TargetSpec{
		Provider: "profile-test", Model: "model-a", Endpoint: rawEndpoint,
	})
	if err != nil {
		t.Fatal(err)
	}
	transport := testTargetBindingProof(rawEndpoint)
	provisioner, err := newKernelIsolationProvisioner(target, transport)
	if err != nil {
		t.Fatal(err)
	}
	request := capabilityprofile.WorkspaceRequest{
		TargetDigest: target.Digest(),
		PlanDigest:   digestProfileFields("plan"),
		CaseID:       "native-tool",
		Attempt:      1,
	}
	guard, err := provisioner.Prepare(context.Background(), t.TempDir(), request)
	if err != nil {
		t.Fatal(err)
	}
	first := guard.Proof()
	if !first.Ready || !first.DisposableWorkspace || !first.AgentExecutionIsolated ||
		!first.FilesystemBoundaryEnforced || !first.OutboundAllowlistEnforced ||
		first.OutboundTargetDigest != target.Digest() ||
		first.Mechanism != profileKernelIsolationMechanism ||
		!validProfileDigest(first.EvidenceDigest) {
		t.Fatalf("proof = %#v", first)
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{rawEndpoint, "private-profile-target.invalid", "tenant-secret"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("proof leaked %q: %s", secret, encoded)
		}
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	if err := guard.Close(); err != nil {
		t.Fatalf("second Close() = %v", err)
	}
	if after := guard.Proof(); after.Ready {
		t.Fatalf("closed guard still reports ready: %#v", after)
	}

	mismatch := request
	mismatch.TargetDigest = digestProfileFields("different-target")
	if _, err := provisioner.Prepare(context.Background(), t.TempDir(), mismatch); !errors.Is(err, capabilityprofile.ErrIsolationUnavailable) {
		t.Fatalf("mismatched target error = %v", err)
	}
}

func TestKernelIsolationProvisionerRejectsIncompleteTransportProof(t *testing.T) {
	const rawEndpoint = "https://provider.invalid"
	target, err := capabilityprofile.NewTargetIdentity(capabilityprofile.TargetSpec{
		Provider: "profile-test", Model: "model-a", Endpoint: rawEndpoint,
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*providers.TargetBindingProof){
		"wrong scope":  func(proof *providers.TargetBindingProof) { proof.Scope = "full-process" },
		"missing hash": func(proof *providers.TargetBindingProof) { proof.IPSetHash = "" },
		"zero address": func(proof *providers.TargetBindingProof) { proof.AddressCount = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			proof := testTargetBindingProof(rawEndpoint)
			mutate(&proof)
			if _, err := newKernelIsolationProvisioner(target, proof); !errors.Is(err, capabilityprofile.ErrIsolationUnavailable) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	wrongEndpoint := testTargetBindingProof("https://different-provider.invalid")
	if _, err := newKernelIsolationProvisioner(target, wrongEndpoint); !errors.Is(err, capabilityprofile.ErrIsolationUnavailable) {
		t.Fatalf("wrong endpoint error = %v", err)
	}
}

func TestDefaultTargetProviderFactoryUsesBoundClient(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests++
		if request.URL.Path != "/tenant/v1/chat/completions" {
			t.Errorf("request path = %q", request.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	dependencies := defaultCLIDependencies()
	bound, err := dependencies.createTarget(context.Background(), "openai", &types.ProviderConfig{
		APIKey: "test-key", BaseURL: server.URL + "/tenant",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	if bound.provider == nil || bound.provider.Name() != "openai" || !validTargetBindingProof(bound.proof) {
		t.Fatalf("bound provider = %#v proof=%#v", bound.provider, bound.proof)
	}
	events, err := bound.provider.StreamMessage(context.Background(), &types.MessagesRequest{
		Model: "test-model", MaxTokens: 1,
		Messages: []types.Message{{Role: "user", Content: json.RawMessage(`"hello"`)}},
	}, &types.StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	if requests != 1 {
		t.Fatalf("provider requests = %d, want 1", requests)
	}
}
