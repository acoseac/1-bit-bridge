//go:build !windows

package doctor

// knownStartupDir is a no-op off Windows — the Startup folder is a
// Windows concept, and windowsStartupDir (its only caller) is only
// meaningful there. The build-tagged windows sibling resolves the real
// path via SHGetKnownFolderPath.
func knownStartupDir() (string, bool) { return "", false }
