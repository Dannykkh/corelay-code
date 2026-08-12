//go:build !windows

package sandbox

func newWindowsJobAdapter(_ AdapterDependencies) Runner {
	return NewUnavailableRunner("Windows Job Objects are unavailable on this platform")
}
