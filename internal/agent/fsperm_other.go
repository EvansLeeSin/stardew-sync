//go:build !linux

package agent

// fixOwnership is a no-op on non-Linux platforms (used for tests only).
func fixOwnership(ref, refFile, newDir string, names []string) error { return nil }
