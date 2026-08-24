package admin

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestPlayerHeadsMatchServerRoutes pins the client boost router's notion of
// "which paths are the player's" to the server's registered player routes.
//
// isPlayerPath (boot.js) decides whether a navigation stays inside the player
// module or is fetched as an operator partial and run through dispatchPageInit.
// A path head that the server serves as a player route but that PLAYER_HEADS
// omits would be fetched as an operator page — the fragment IS the player
// shell, but boostSwap would call dispatchPageInit("player") (a no-op) instead
// of mountShell(), leaving the shell un-booted. The reverse drift (a head in
// the JS set with no server route) is a dead entry. Either way the two must
// agree, and only a test that reads both catches it — this exact drift was
// present when the router was first written (genre/composer/playlist/mix).
func TestPlayerHeadsMatchServerRoutes(t *testing.T) {
	// Server truth: the first path segment of every playerRoutes entry.
	wantHeads := map[string]bool{}
	for _, r := range playerRoutes {
		seg := strings.SplitN(strings.TrimPrefix(r, "/"), "/", 2)[0]
		if seg != "" {
			wantHeads[seg] = true
		}
	}

	// Client set: parse the PLAYER_HEADS literal out of boot.js.
	src, err := staticFS.ReadFile("static/player/boot.js")
	if err != nil {
		t.Fatal(err)
	}
	gotHeads := parsePlayerHeads(t, string(src))

	if !equalStringSets(wantHeads, gotHeads) {
		t.Errorf("PLAYER_HEADS (boot.js) and server playerRoutes disagree:\n"+
			"  server heads: %s\n"+
			"  boot.js heads: %s\n"+
			"They must match, or the boost router misroutes a player path. Update "+
			"PLAYER_HEADS in boot.js (and, if a route was added, its route() case).",
			sortedKeys(wantHeads), sortedKeys(gotHeads))
	}
}

var playerHeadsBlockRe = regexp.MustCompile(`(?s)const PLAYER_HEADS = new Set\(\[(.*?)\]\)`)
var quotedRe = regexp.MustCompile(`"([a-z]+)"`)

func parsePlayerHeads(t *testing.T, src string) map[string]bool {
	t.Helper()
	m := playerHeadsBlockRe.FindStringSubmatch(src)
	if m == nil {
		t.Fatal("could not find `const PLAYER_HEADS = new Set([...])` in boot.js — " +
			"the parity guard's anchor moved")
	}
	out := map[string]bool{}
	for _, q := range quotedRe.FindAllStringSubmatch(m[1], -1) {
		out[q[1]] = true
	}
	if len(out) == 0 {
		t.Fatal("parsed an empty PLAYER_HEADS set — the anchor matched but the body didn't")
	}
	return out
}

func equalStringSets(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]bool) string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, " ")
}
