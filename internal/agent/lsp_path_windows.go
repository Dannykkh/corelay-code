//go:build windows

package agent

import (
	"path/filepath"

	"golang.org/x/sys/windows"
)

// lspCanonicalPath expands Windows 8.3 aliases before a path crosses the LSP
// boundary. gopls compares workspace URIs against the long path reported by
// the OS and rejects an otherwise equivalent short component.
func lspCanonicalPath(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	input, err := windows.UTF16PtrFromString(absolute)
	if err != nil {
		return filepath.Clean(absolute)
	}
	buffer := make([]uint16, 32768)
	length, err := windows.GetLongPathName(input, &buffer[0], uint32(len(buffer)))
	if err != nil || length == 0 || length >= uint32(len(buffer)) {
		return filepath.Clean(absolute)
	}
	return filepath.Clean(windows.UTF16ToString(buffer[:length]))
}
