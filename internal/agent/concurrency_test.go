package agent

import (
	"testing"
)

func TestIsConcurrencySafe(t *testing.T) {
	tests := []struct {
		tool  string
		input map[string]interface{}
		safe  bool
	}{
		// Always safe
		{"Read", nil, true},
		{"Glob", nil, true},
		{"Grep", nil, true},

		// Never safe
		{"Write", nil, false},
		{"Edit", nil, false},

		// Bash: depends on command
		{"Bash", map[string]interface{}{"command": "ls -la"}, true},
		{"Bash", map[string]interface{}{"command": "cat README.md"}, true},
		{"Bash", map[string]interface{}{"command": "git status"}, true},
		{"Bash", map[string]interface{}{"command": "git log --oneline"}, true},
		{"Bash", map[string]interface{}{"command": "grep -r foo src/"}, true},
		{"Bash", map[string]interface{}{"command": "find . -name *.go"}, true},
		{"Git", map[string]interface{}{"command": "status", "args": "--short"}, true},
		{"Git", map[string]interface{}{"command": "branch", "args": "--list feature/*"}, true},

		// Bash: unsafe
		{"Bash", map[string]interface{}{"command": "cd src && ls"}, false},
		{"Bash", map[string]interface{}{"command": "rm -rf node_modules"}, false},
		{"Bash", map[string]interface{}{"command": "npm install"}, false},
		{"Bash", map[string]interface{}{"command": "git push"}, false},
		{"Bash", map[string]interface{}{"command": "git commit -m 'x'"}, false},
		{"Bash", map[string]interface{}{"command": "echo foo > file.txt"}, false},
		{"Bash", map[string]interface{}{"command": "docker run nginx"}, false},
		{"Bash", map[string]interface{}{"command": "sudo apt install"}, false},
		{"Bash", map[string]interface{}{"command": "mv a.txt b.txt"}, false},
		{"Bash", map[string]interface{}{"command": "kill -9 1234"}, false},
		{"Bash", map[string]interface{}{"command": "find . -delete"}, false},
		{"Bash", map[string]interface{}{"command": "python -c \"open('x','w').write('x')\""}, false},
		{"Bash", map[string]interface{}{"command": "git branch feature/new"}, false},
		{"Bash", map[string]interface{}{"command": "echo x > output.txt"}, false},
		{"Bash", map[string]interface{}{"command": "ls && python -c open"}, false},
		{"Bash", map[string]interface{}{"command": "echo 'a;b'"}, false},
		{"Bash", map[string]interface{}{"command": "unknown-tool --version"}, false},
		{"Git", map[string]interface{}{"command": "branch", "args": "feature/new"}, false},
		{"Git", map[string]interface{}{"command": "unknown-subcommand", "args": ""}, false},

		// Unknown tool
		{"CustomTool", nil, false},
	}

	for _, tt := range tests {
		name := tt.tool
		if cmd, ok := tt.input["command"]; ok {
			name += " " + cmd.(string)
		}
		t.Run(name, func(t *testing.T) {
			result := IsConcurrencySafe(tt.tool, tt.input)
			if result != tt.safe {
				t.Errorf("IsConcurrencySafe(%s, %v) = %v, want %v", tt.tool, tt.input, result, tt.safe)
			}
		})
	}
}

func TestPartitionToolCalls(t *testing.T) {
	calls := []ToolCall{
		{ID: "1", Name: "Read", Input: map[string]interface{}{}},
		{ID: "2", Name: "Grep", Input: map[string]interface{}{}},
		{ID: "3", Name: "Write", Input: map[string]interface{}{}},
		{ID: "4", Name: "Glob", Input: map[string]interface{}{}},
	}

	concurrent, serial := PartitionToolCalls(calls)

	if len(concurrent) != 3 { // Read, Grep, Glob
		t.Errorf("Expected 3 concurrent, got %d", len(concurrent))
	}
	if len(serial) != 1 { // Write
		t.Errorf("Expected 1 serial, got %d", len(serial))
	}
	if serial[0].Name != "Write" {
		t.Errorf("Serial should be Write, got %s", serial[0].Name)
	}
}
