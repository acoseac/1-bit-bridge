//go:build !linux

package updater

// FileCapabilities is nil everywhere but Linux, which alone gives a file
// capabilities (setcap's security.capability).
func FileCapabilities(string) ([]byte, error) { return nil, nil }

// carryCapabilities carries nothing where files carry no capabilities.
func carryCapabilities(_, _ string) {
	// No file capabilities off Linux.
}
