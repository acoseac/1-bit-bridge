//go:build !windows

package main

// redirectServiceIO is a no-op off Windows. `main` only calls it
// guarded by isWindowsService(), which is always false off Windows —
// so in practice this never runs, but keeps main.go platform-
// agnostic.
func redirectServiceIO() {
	// Intentional no-op off Windows — see file docstring above.
}
