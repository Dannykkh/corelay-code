package agent

import (
	"encoding/json"
	"testing"
)

func TestClassifyDangerGitInvocationTable(t *testing.T) {
	tests := []struct {
		name    string
		command string
		args    string
		want    DangerLevel
	}{
		{name: "status", command: "status", args: "--short", want: DangerSafe},
		{name: "branch list", command: "branch", args: "--list feature/*", want: DangerSafe},
		{name: "branch show current", command: "branch", args: "--show-current", want: DangerSafe},
		{name: "branch create", command: "branch", args: "feature/new", want: DangerModerate},
		{name: "branch delete", command: "branch", args: "-D old", want: DangerModerate},
		{name: "stash list", command: "stash", args: "list", want: DangerSafe},
		{name: "stash show", command: "stash", args: "show stash@{0}", want: DangerSafe},
		{name: "stash default push", command: "stash", want: DangerModerate},
		{name: "stash pop", command: "stash", args: "pop", want: DangerModerate},
		{name: "stash drop", command: "stash", args: "drop", want: DangerModerate},
		{name: "stash clear", command: "stash", args: "clear", want: DangerModerate},
		{name: "remote list", command: "remote", args: "-v", want: DangerSafe},
		{name: "remote show", command: "remote", args: "show origin", want: DangerSafe},
		{name: "remote add", command: "remote", args: "add origin url", want: DangerModerate},
		{name: "remote remove", command: "remote", args: "remove origin", want: DangerModerate},
		{name: "remote set url", command: "remote", args: "set-url origin url", want: DangerModerate},
		{name: "tag list", command: "tag", args: "--list v*", want: DangerSafe},
		{name: "tag verify", command: "tag", args: "--verify v1", want: DangerSafe},
		{name: "tag create", command: "tag", args: "v1", want: DangerModerate},
		{name: "tag delete", command: "tag", args: "--delete v1", want: DangerModerate},
		{name: "checkout", command: "checkout", args: "feature", want: DangerModerate},
		{name: "switch", command: "switch", args: "feature", want: DangerModerate},
		{name: "reset soft", command: "reset", args: "--soft HEAD~1", want: DangerModerate},
		{name: "reset hard", command: "reset", args: "--hard HEAD~1", want: DangerDangerous},
		{name: "rebase", command: "rebase", args: "main", want: DangerModerate},
		{name: "merge", command: "merge", args: "feature", want: DangerModerate},
		{name: "cherry pick", command: "cherry-pick", args: "abc123", want: DangerModerate},
		{name: "push", command: "push", args: "origin main", want: DangerModerate},
		{name: "force push long", command: "push", args: "--force-with-lease", want: DangerDangerous},
		{name: "force push short", command: "push", args: "-f", want: DangerDangerous},
		{name: "unknown conservative", command: "worktree", args: "add ../copy", want: DangerModerate},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input, err := json.Marshal(map[string]interface{}{
				"command": test.command,
				"args":    test.args,
				"confirm": true,
			})
			if err != nil {
				t.Fatal(err)
			}
			got, _ := ClassifyDanger("Git", input)
			if got != test.want {
				t.Fatalf("ClassifyDanger(%q, %q)=%q, want %q", test.command, test.args, got, test.want)
			}
		})
	}
}
