//go:build !windows

package processsupervisor

func newWindowsJobAdapter(AdapterDependencies) Runner {
	return NewUnavailableRunner("Windows Job Objects are unavailable on this platform")
}
