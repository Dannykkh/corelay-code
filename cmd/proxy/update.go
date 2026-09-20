package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/Dannykkh/corelay-code/internal/updater"
)

func runUpdate(args []string) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	artifact := fs.String("artifact", "", "verified local release artifact")
	sha256Value := fs.String("sha256", "", "expected artifact SHA-256 digest")
	target := fs.String("target", "", "executable to replace (default: current executable)")
	backup := fs.String("backup", "", "rollback copy path (default: target.previous)")
	dryRun := fs.Bool("dry-run", false, "verify the artifact without replacing the executable")
	rollback := fs.Bool("rollback", false, "restore the target's explicit rollback copy")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "update: invalid arguments; see 'corelaycode help'")
		return 2
	}
	if *rollback && *dryRun {
		fmt.Fprintln(os.Stderr, "update: -rollback cannot be combined with -dry-run")
		return 2
	}
	var err error
	resolvedTarget := strings.TrimSpace(*target)
	if resolvedTarget == "" {
		resolvedTarget, err = os.Executable()
		if err != nil {
			fmt.Fprintf(os.Stderr, "update: current executable is unavailable: %v\n", err)
			return 1
		}
	}
	if *rollback {
		resolvedBackup := strings.TrimSpace(*backup)
		if resolvedBackup == "" {
			resolvedBackup = resolvedTarget + ".previous"
		}
		if shouldDeferSelfUpdate(resolvedTarget) {
			helperPath, err := scheduleSelfUpdate(updateHelperRequest{
				Target:    resolvedTarget,
				Backup:    resolvedBackup,
				ParentPID: uint32(os.Getpid()),
				Rollback:  true,
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "update: could not schedule rollback helper: %v\n", err)
				return 1
			}
			fmt.Fprintf(os.Stdout, "Rollback scheduled for %s; helper: %s\n", resolvedTarget, helperPath)
			return 0
		}
		if err := updater.Rollback(resolvedTarget, resolvedBackup); err != nil {
			if errors.Is(err, updater.ErrTargetInUse) {
				fmt.Fprintln(os.Stderr, "update: target is in use; stop the running server and retry rollback")
			} else {
				fmt.Fprintf(os.Stderr, "update: rollback failed: %v\n", err)
			}
			return 1
		}
		fmt.Fprintf(os.Stdout, "Rolled back %s from %s\n", resolvedTarget, resolvedBackup)
		return 0
	}
	if strings.TrimSpace(*artifact) == "" || strings.TrimSpace(*sha256Value) == "" {
		fmt.Fprintln(os.Stderr, "update: -artifact and -sha256 are required; release discovery and download are not automatic")
		return 2
	}
	digest, err := updater.VerifyArtifact(*artifact, *sha256Value)
	if err != nil {
		fmt.Fprintf(os.Stderr, "update: artifact verification failed: %v\n", err)
		return 1
	}
	if *dryRun {
		fmt.Fprintf(os.Stdout, "Verified artifact SHA-256: %s\n", digest)
		return 0
	}
	if shouldDeferSelfUpdate(resolvedTarget) {
		helperPath, err := scheduleSelfUpdate(updateHelperRequest{
			Artifact:  *artifact,
			Digest:    digest,
			Target:    resolvedTarget,
			Backup:    strings.TrimSpace(*backup),
			ParentPID: uint32(os.Getpid()),
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "update: could not schedule update helper: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stdout, "Update scheduled for %s; helper: %s; SHA-256: %s\n", resolvedTarget, helperPath, digest)
		return 0
	}
	result, err := updater.Install(*artifact, resolvedTarget, digest, *backup)
	if err != nil {
		if errors.Is(err, updater.ErrTargetInUse) {
			fmt.Fprintln(os.Stderr, "update: target is in use; stop the running server and retry, leaving the current executable unchanged")
			return 1
		}
		if errors.Is(err, updater.ErrRollback) {
			fmt.Fprintf(os.Stderr, "update: failed and rollback is uncertain: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "update: install failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stdout, "Updated %s; rollback copy: %s; SHA-256: %s\n", result.Target, result.Backup, result.ArtifactDigest)
	return 0
}

type updateHelperRequest struct {
	Artifact  string
	Digest    string
	Target    string
	Backup    string
	ParentPID uint32
	Rollback  bool
}

func runUpdateHelper(args []string) int {
	fs := flag.NewFlagSet("update-helper", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	artifact := fs.String("artifact", "", "verified local release artifact")
	digest := fs.String("sha256", "", "expected artifact SHA-256 digest")
	target := fs.String("target", "", "executable to replace")
	backup := fs.String("backup", "", "rollback copy path")
	parentPID := fs.Uint("parent-pid", 0, "process ID of the explicit update command")
	helperPath := fs.String("helper-path", "", "temporary helper executable to remove after completion")
	rollback := fs.Bool("rollback", false, "restore the rollback copy")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || strings.TrimSpace(*target) == "" || *parentPID == 0 || strings.TrimSpace(*helperPath) == "" {
		fmt.Fprintln(os.Stderr, "update-helper: invalid arguments")
		return 2
	}
	if err := validateUpdateHelperPath(*helperPath); err != nil {
		fmt.Fprintf(os.Stderr, "update-helper: %v\n", err)
		return 2
	}
	if err := waitForUpdateParent(uint32(*parentPID)); err != nil {
		fmt.Fprintf(os.Stderr, "update-helper: waiting for parent: %v\n", err)
		cleanupUpdateHelper(*helperPath)
		return 1
	}
	var err error
	if *rollback {
		resolvedBackup := strings.TrimSpace(*backup)
		if resolvedBackup == "" {
			resolvedBackup = strings.TrimSpace(*target) + ".previous"
		}
		err = updater.Rollback(*target, resolvedBackup)
	} else {
		if strings.TrimSpace(*artifact) == "" || strings.TrimSpace(*digest) == "" {
			fmt.Fprintln(os.Stderr, "update-helper: artifact and sha256 are required")
			cleanupUpdateHelper(*helperPath)
			return 2
		}
		_, err = updater.Install(*artifact, *target, *digest, strings.TrimSpace(*backup))
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "update-helper: operation failed: %v\n", err)
		cleanupUpdateHelper(*helperPath)
		return 1
	}
	if *rollback {
		resolvedBackup := strings.TrimSpace(*backup)
		if resolvedBackup == "" {
			resolvedBackup = strings.TrimSpace(*target) + ".previous"
		}
		fmt.Fprintf(os.Stdout, "Rolled back %s from %s\n", *target, resolvedBackup)
	} else {
		fmt.Fprintf(os.Stdout, "Updated %s\n", *target)
	}
	cleanupUpdateHelper(*helperPath)
	return 0
}

func updateHelperParentArgument(pid uint32) string {
	return strconv.FormatUint(uint64(pid), 10)
}
