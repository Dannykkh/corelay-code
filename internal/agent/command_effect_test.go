package agent

import (
	"encoding/json"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

func TestCommandEffectAuthorizationAndConcurrencyShareClassification(t *testing.T) {
	tests := []struct {
		name         string
		tool         string
		input        map[string]interface{}
		wantEffect   CommandEffectKind
		wantDanger   DangerLevel
		wantParallel bool
	}{
		{
			name: "read-only find query", tool: "Bash",
			input:      map[string]interface{}{"command": "find . -name *.go"},
			wantEffect: CommandEffectReadOnly, wantDanger: DangerSafe, wantParallel: true,
		},
		{
			name: "find delete", tool: "Bash",
			input:      map[string]interface{}{"command": "find . -delete"},
			wantEffect: CommandEffectMutating, wantDanger: DangerModerate,
		},
		{
			name: "python file write stays unknown", tool: "Bash",
			input:      map[string]interface{}{"command": "python -c \"open('x','w').write('x')\""},
			wantEffect: CommandEffectUnknown, wantDanger: DangerUnknown,
		},
		{
			name: "git branch creation", tool: "Bash",
			input:      map[string]interface{}{"command": "git branch feature/new"},
			wantEffect: CommandEffectMutating, wantDanger: DangerModerate,
		},
		{
			name: "structured git branch creation", tool: "Git",
			input:      map[string]interface{}{"command": "branch", "args": "feature/new"},
			wantEffect: CommandEffectMutating, wantDanger: DangerModerate,
		},
		{
			name: "redirect is ambiguous and asks", tool: "Bash",
			input:      map[string]interface{}{"command": "echo x > output.txt"},
			wantEffect: CommandEffectUnknown, wantDanger: DangerUnknown,
		},
		{
			name: "compound command is ambiguous", tool: "Bash",
			input:      map[string]interface{}{"command": "ls && python -c open"},
			wantEffect: CommandEffectUnknown, wantDanger: DangerUnknown,
		},
		{
			name: "quoted command is ambiguous", tool: "Bash",
			input:      map[string]interface{}{"command": "echo 'a;b'"},
			wantEffect: CommandEffectUnknown, wantDanger: DangerUnknown,
		},
		{
			name: "unknown command is approval-only", tool: "Bash",
			input:      map[string]interface{}{"command": "unknown-tool --version"},
			wantEffect: CommandEffectUnknown, wantDanger: DangerUnknown,
		},
		{
			name: "path-qualified cat is not trusted by basename", tool: "Bash",
			input:      map[string]interface{}{"command": "./cat README.md"},
			wantEffect: CommandEffectUnknown, wantDanger: DangerUnknown,
		},
		{
			name: "path-qualified git is not trusted by basename", tool: "Bash",
			input:      map[string]interface{}{"command": "/tmp/git status"},
			wantEffect: CommandEffectUnknown, wantDanger: DangerUnknown,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			effect := classifyCommandEffect(test.tool, test.input)
			if effect.Kind != test.wantEffect {
				t.Fatalf("effect = %q (%s), want %q", effect.Kind, effect.Reason, test.wantEffect)
			}
			input, err := json.Marshal(test.input)
			if err != nil {
				t.Fatal(err)
			}
			danger, _ := ClassifyDanger(test.tool, input)
			if danger != test.wantDanger {
				t.Fatalf("danger = %q, want %q", danger, test.wantDanger)
			}
			if parallel := IsConcurrencySafe(test.tool, test.input); parallel != test.wantParallel {
				t.Fatalf("parallel = %v, want %v", parallel, test.wantParallel)
			}
		})
	}
}

func TestWorkspaceProcessesRequireApprovalWithoutFilesystemIsolation(t *testing.T) {
	capabilities := sandbox.Capabilities{
		ProcessIsolation: true, ProcessTreeKill: true,
		EnvironmentFiltering: true, Timeouts: true,
	}
	policy, err := ResolveExecutionPolicy(ExecutionPolicyRequest{Mode: ExecutionModeWorkspace}, "", capabilities)
	if err != nil {
		t.Fatal(err)
	}
	permissionConfig := DefaultPermissionConfig()
	permissionConfig.AutoApprove = "moderate"
	for _, test := range []struct {
		tool  string
		input map[string]string
	}{
		{tool: "Bash", input: map[string]string{"command": "cp outside.txt inside.txt"}},
		{tool: "Bash", input: map[string]string{"command": "cat C:/private/secret.txt"}},
		{tool: "Git", input: map[string]string{"command": "fetch", "args": "origin"}},
	} {
		raw, marshalErr := json.Marshal(test.input)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		result := ResolvePermissionWithPolicy(test.tool, raw, t.TempDir(), permissionConfig, "ask", &policy)
		if result.Decision != PermissionApproval {
			t.Errorf("%s %s without filesystem isolation = %#v, want approval", test.tool, raw, result)
		}
	}
}

func TestUnknownCommandEffectRequiresApprovalUnderModerateAutoApprove(t *testing.T) {
	config := DefaultPermissionConfig()
	config.AutoApprove = "moderate"
	for _, command := range []string{
		`python -c "open('x','w').write('x')"`,
		"echo x > output.txt",
		"ls && cat README.md",
		"custom-tool --version",
	} {
		t.Run(command, func(t *testing.T) {
			input, _ := json.Marshal(map[string]string{"command": command})
			allowed, _, danger := CheckPermission("Bash", input, t.TempDir(), config)
			if allowed || danger != DangerUnknown {
				t.Fatalf("CheckPermission() = (%v, %q), want not allowed / unknown", allowed, danger)
			}
		})
	}
}

func TestPartitionToolCallsSerializesMutatingAndAmbiguousCommands(t *testing.T) {
	calls := []ToolCall{
		{ID: "read", Name: "Bash", Input: map[string]interface{}{"command": "git status --short"}},
		{ID: "delete", Name: "Bash", Input: map[string]interface{}{"command": "find . -delete"}},
		{ID: "write", Name: "Bash", Input: map[string]interface{}{"command": "python -c open"}},
		{ID: "branch", Name: "Git", Input: map[string]interface{}{"command": "branch", "args": "feature/new"}},
		{ID: "redirect", Name: "Bash", Input: map[string]interface{}{"command": "echo x > output.txt"}},
	}
	concurrent, serial := PartitionToolCalls(calls)
	if len(concurrent) != 1 || concurrent[0].ID != "read" {
		t.Fatalf("concurrent calls = %#v, want only read-only git status", concurrent)
	}
	if len(serial) != 4 {
		t.Fatalf("serial calls = %#v, want all mutating/ambiguous commands", serial)
	}
}
