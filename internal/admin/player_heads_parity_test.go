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

// TestPlayerRoutesTableCoversEveryPlayerHead pins the OTHER half of the
// same contract, which the test above never checked despite its failure
// message promising it ("and, if a route was added, its route() case").
//
// route() dispatches through an object literal and falls back with
// `routes[section] || routes.albums`. That fallback is silent by design
// — a mistyped path should render something rather than nothing — but it
// also means a head PLAYER_HEADS claims and the table forgets renders
// the ALBUM GRID under the wrong title, with no error anywhere. That is
// not hypothetical: /genre, /composer, /playlist and /mix were all
// registered, all claimed, and all silently fell through to Albums from
// the day the router was written until the case that added them.
func TestPlayerRoutesTableCoversEveryPlayerHead(t *testing.T) {
	src, err := staticFS.ReadFile("static/player/boot.js")
	if err != nil {
		t.Fatal(err)
	}
	heads := parsePlayerHeads(t, string(src))
	cases := parseRouteTableKeys(t, string(src))

	// Every head must have its own case. The reverse is allowed: `search`
	// and the section-only entries can legitimately exist as cases
	// without being reachable heads.
	var missing []string
	for h := range heads {
		if knownRouteGaps[h] {
			continue
		}
		if !cases[h] {
			missing = append(missing, h)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("route() has no case for %s.\n"+
			"  heads:        %s\n"+
			"  route() cases: %s\n"+
			"Without a case, `routes[section] || routes.albums` renders the album "+
			"grid under that section's title instead — silently, with no error.",
			strings.Join(missing, " "), sortedKeys(heads), sortedKeys(cases))
	}
}

// knownRouteGaps are heads deliberately still falling through. It is
// EMPTY, and the emptiness is the assertion: every registered player
// route now has a route() case, so nothing silently renders the album
// grid under someone else's title.
//
// Kept rather than deleted so a future gap has to be added here
// explicitly — a visible, reviewable line — instead of being introduced
// by simply forgetting a case, which is how genre, composer, playlist
// and mix all went unnoticed for the router's whole life.
var knownRouteGaps = map[string]bool{}

// routeTableBlockRe captures the body of route()'s dispatch literal. It
// is anchored on the `const routes = {` declaration and stops at the
// lookup that consumes it, so an added case is picked up automatically.
var routeTableBlockRe = regexp.MustCompile(`(?s)const routes = \{(.*?)
  \};`)
var routeKeyRe = regexp.MustCompile(`(?m)^\s{4}([a-z]+):`)

func parseRouteTableKeys(t *testing.T, src string) map[string]bool {
	t.Helper()
	m := routeTableBlockRe.FindStringSubmatch(src)
	if m == nil {
		t.Fatal("could not find `const routes = {...}` in boot.js — " +
			"the route-table guard's anchor moved")
	}
	out := map[string]bool{}
	for _, k := range routeKeyRe.FindAllStringSubmatch(m[1], -1) {
		out[k[1]] = true
	}
	if len(out) == 0 {
		t.Fatal("parsed an empty route table — the anchor matched but the body didn't")
	}
	return out
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
