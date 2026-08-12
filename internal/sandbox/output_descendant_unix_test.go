//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestUnconfinedRunnerKillsDescendantWriterOnOutputLimit(t *testing.T) {
	temporaryDir := t.TempDir()
	command := helperCommand("output-parent")
	command.Environment.Set[helperReadyPath] = filepath.Join(temporaryDir, "descendant-ready")
	command.Environment.Set[helperStartPath] = filepath.Join(temporaryDir, "descendant-start")
	command.OutputLimitBytes = 1024
	command.Timeout = 5 * time.Second
	result, report := NewUnconfinedRunner().Run(
		context.Background(),
		Policy{Enforcement: EnforcementDisabled},
		command,
	)
	assertOutputLimitFailure(t, result, report, command.OutputLimitBytes)
	childPID, err := parseOutputDescendantPID(string(result.Stdout))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		err = syscall.Kill(childPID, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if err != nil {
			t.Fatalf("check descendant writer %d: %v", childPID, err)
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(childPID, syscall.SIGKILL)
			t.Fatalf("descendant writer %d survived output-limit termination", childPID)
		}
		time.Sleep(time.Millisecond)
	}
}

func parseOutputDescendantPID(output string) (int, error) {
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "child-pid=") {
			return strconv.Atoi(strings.TrimPrefix(line, "child-pid="))
		}
	}
	return 0, fmt.Errorf("descendant pid missing from output %q", output)
}
