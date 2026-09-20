//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package config

import "os"

func replaceConfigFile(source, target string) error { return os.Rename(source, target) }
