package manifest

// nullable returns a value suitable for SQL binding: nil for "" (stored
// as NULL, so a local item's empty origin columns and a foreign item's
// empty path stay distinguishable), the value otherwise.
//
// Lives here rather than in any single feature file because multiple
// store paths bind it (playlists.go, history.go) — keeping it in a
// dedicated helpers file makes the shared dependency explicit so a
// future file split can't silently break a distant caller.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
