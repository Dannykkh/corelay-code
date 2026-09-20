// Package buildinfo exposes build-time metadata shared by the CLI and server.
package buildinfo

// Version and Commit are set by release and local build commands with -ldflags.
var (
	Version = "dev"
	Commit  = "unknown"
)
