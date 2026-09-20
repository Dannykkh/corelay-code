//go:build !windows

package main

import "fmt"

func shouldDeferSelfUpdate(string) bool { return false }

func scheduleSelfUpdate(updateHelperRequest) (string, error) {
	return "", fmt.Errorf("self-update helper is only available on Windows")
}

func waitForUpdateParent(uint32) error {
	return fmt.Errorf("update helper is only available on Windows")
}

func validateUpdateHelperPath(string) error {
	return fmt.Errorf("update helper is only available on Windows")
}

func cleanupUpdateHelper(string) {}
