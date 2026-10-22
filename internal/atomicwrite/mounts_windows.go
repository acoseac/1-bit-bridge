//go:build windows

package atomicwrite

// mountIDsExpected is false here: the strict writer refuses on Windows before
// it looks at a path, having no directory barrier (ErrNoDirectoryBarrier), so
// no mount identity is ever asked for there; a test that sets a barrier
// compares none unless it gives mount IDs itself (mountIDOf).
const mountIDsExpected = false

// platformMountID is no ID: none is asked for on Windows.
func platformMountID(string) (uint64, bool, error) { return 0, false, nil }
