//go:build !windows

package updater

// inUse is Windows' alone (held_windows.go): here a rename never waits on
// another process's open file, so no refusal is a hold that ends.
func inUse(error) bool { return false }

// movable is Windows' alone: here a rename's source is never held from it.
func movable(string) error { return nil }
