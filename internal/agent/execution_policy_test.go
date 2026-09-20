package agent

import (
	"testing"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

func TestResolveExecutionPolicyDefaultsAndMigratesLegacyThresholdsToWorkspace(t *testing.T) {
	legacyValues := []string{"", "safe", "moderate", "all", "none"}
	for _, legacy := range legacyValues {
		t.Run("legacy_"+legacy, func(t *testing.T) {
			capabilities := sandbox.Capabilities{ProcessIsolation: true, ProcessTreeKill: true}
			got, err := ResolveExecutionPolicy(ExecutionPolicyRequest{}, legacy, capabilities)
			if err != nil {
				t.Fatalf("ResolveExecutionPolicy() error = %v", err)
			}
			if got.Mode != ExecutionModeWorkspace || got.Revision != 1 {
				t.Fatalf("resolved mode/revision = %q/%d, want workspace/1", got.Mode, got.Revision)
			}
			if got.RuntimeCapabilities != capabilities {
				t.Fatalf("runtime capabilities = %#v, want captured %#v", got.RuntimeCapabilities, capabilities)
			}
			if got.RuntimeCapabilities.FilesystemIsolation || got.RuntimeCapabilities.NetworkIsolation {
				t.Fatal("process containment was incorrectly promoted to file/network isolation")
			}
		})
	}
}

func TestResolveExecutionPolicyAcceptsExplicitModeAndRejectsUnknownInputs(t *testing.T) {
	got, err := ResolveExecutionPolicy(
		ExecutionPolicyRequest{Mode: ExecutionModeFull, Revision: 9},
		"invalid-but-ignored-for-explicit-mode",
		sandbox.Capabilities{},
	)
	if err != nil {
		t.Fatalf("explicit full mode should not be coupled to the legacy threshold: %v", err)
	}
	if got.Mode != ExecutionModeFull || got.Revision != 9 {
		t.Fatalf("resolved policy = %#v, want explicit full revision 9", got)
	}

	for _, test := range []struct {
		name    string
		request ExecutionPolicyRequest
		legacy  string
	}{
		{name: "unknown mode", request: ExecutionPolicyRequest{Mode: "unrestricted"}},
		{name: "unknown legacy threshold", legacy: "trust-everything"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ResolveExecutionPolicy(test.request, test.legacy, sandbox.Capabilities{}); err == nil {
				t.Fatal("expected unsupported policy input to fail closed")
			}
		})
	}
}

func TestResolveChildExecutionPolicyInheritsOrNarrowsWithoutEscalation(t *testing.T) {
	parent, err := ResolveExecutionPolicy(
		ExecutionPolicyRequest{Mode: ExecutionModeFull, Revision: 17},
		"",
		sandbox.Capabilities{FilesystemIsolation: true, NetworkIsolation: true},
	)
	if err != nil {
		t.Fatal(err)
	}

	for _, mode := range []ExecutionMode{"", ExecutionModeWorkspace, ExecutionModeReadOnly} {
		child, err := ResolveChildExecutionPolicy(parent, mode, sandbox.Capabilities{ProcessTreeKill: true})
		if err != nil {
			t.Fatalf("ResolveChildExecutionPolicy(%q): %v", mode, err)
		}
		want := mode
		if want == "" {
			want = ExecutionModeFull
		}
		if child.Mode != want || child.Revision != parent.Revision || child.ParentRevision != parent.Revision {
			t.Fatalf("child policy = %#v, want mode=%q and parent revision=%d", child, want, parent.Revision)
		}
		if child.RuntimeCapabilities.FilesystemIsolation || child.RuntimeCapabilities.NetworkIsolation {
			t.Fatal("child inherited isolation capability unavailable in its own runtime")
		}
	}

	workspaceParent, err := ResolveExecutionPolicy(
		ExecutionPolicyRequest{Mode: ExecutionModeWorkspace},
		"",
		sandbox.Capabilities{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveChildExecutionPolicy(workspaceParent, ExecutionModeFull, sandbox.Capabilities{}); err == nil {
		t.Fatal("child escalated workspace parent to full")
	}
	readOnlyParent, err := ResolveChildExecutionPolicy(workspaceParent, ExecutionModeReadOnly, sandbox.Capabilities{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveChildExecutionPolicy(readOnlyParent, ExecutionModeWorkspace, sandbox.Capabilities{}); err == nil {
		t.Fatal("grandchild escalated read-only parent to workspace")
	}
}

func TestValidateExecutionPolicySnapshotRequiresExplicitFullProvenance(t *testing.T) {
	for _, snapshot := range []ExecutionPolicySnapshot{
		{Mode: ExecutionModeFull, Revision: 2},
		{Mode: ExecutionModeFull, Revision: 2, FullSelectionRevision: 1, Source: "user-selected"},
		{Mode: ExecutionModeWorkspace, Revision: 2, FullSelectionRevision: 2, Source: "user-selected"},
		{Mode: ExecutionModeReadOnly, Revision: 2, Source: "inherited"},
	} {
		if err := ValidateExecutionPolicySnapshot(snapshot); err == nil {
			t.Fatalf("invalid execution policy snapshot accepted: %#v", snapshot)
		}
	}
	valid, err := ResolveExecutionPolicy(
		ExecutionPolicyRequest{Mode: ExecutionModeFull, Revision: 2},
		"",
		sandbox.Capabilities{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateExecutionPolicySnapshot(valid); err != nil {
		t.Fatalf("resolved explicit full snapshot rejected: %v", err)
	}
}
