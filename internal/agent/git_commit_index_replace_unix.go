//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package agent

import "os"

func replaceGitCommitIndexFile(source, target string) error {
	return os.Rename(source, target)
}
