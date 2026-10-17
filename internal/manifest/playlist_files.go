package manifest

// Playlist files (backlog B431). Stub: the parser and the path rule are not
// built yet.

const playlistFileMaxEntries = 100_000

func isPlaylistFileName(name string) bool { return false }

func parsePlaylistLines(data []byte) (lines []string, tooMany bool) { return nil, false }

func playlistFileDir(manifestPath string) string { return "" }

func playlistEntryPath(dir, line string) (string, bool) { return "", false }

func playlistFileDisplayName(manifestPath string) string { return "" }
