package admin

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The player paints two of its measurements by writing CSS custom
// properties from JavaScript — the played position and the fetched one —
// and nothing connects either end to the other. The class-parity test
// next door exists for exactly this shape of break in the markup half;
// this is the same hole one layer down, and the failure is quieter:
// var(--np-loaded, 0%) with nobody supplying it does not error, it
// silently resolves to the fallback, so the scrubber renders as a
// perfectly ordinary two-tone bar with the download invisible again.
//
// The pair that made this worth writing is --np-buffer (the COLOUR of
// the fetched band) and --np-loaded (how far it extends). They live in
// the same gradient declaration, and an earlier draft spelled the second
// one --np-buffered — one character apart from the first, in a file
// where both appear on adjacent lines.
var (
	// style.setProperty("--x", …) anywhere in the player module.
	jsSetPropRe = regexp.MustCompile(`setProperty\(\s*["'](--[\w-]+)["']`)
	// var(--x — a read, with or without a fallback.
	cssVarUseRe = regexp.MustCompile(`var\(\s*(--[\w-]+)`)
	// --x: … — a declaration. Anchored on the start of a line or a `{`/`;`
	// so a var() read is never mistaken for one.
	cssVarDeclRe = regexp.MustCompile(`(?m)(?:^|[{;])\s*(--[\w-]+)\s*:`)
)

func TestPlayerCustomPropertiesAreSuppliedAndRead(t *testing.T) {
	set := jsSetCustomProperties(t)
	if len(set) == 0 {
		t.Fatal("no style.setProperty(\"--…\") calls scraped from the player " +
			"module — the regex has stopped matching, which would make this " +
			"test pass while checking nothing")
	}

	playerCSS := readCSSNoComments(t, filepath.Join("static", "player.css"))
	appCSS := readCSSNoComments(t, filepath.Join("static", "app.css"))

	used := map[string]bool{}
	for _, m := range cssVarUseRe.FindAllStringSubmatch(playerCSS, -1) {
		used[m[1]] = true
	}
	declared := map[string]bool{}
	for _, css := range []string{playerCSS, appCSS} {
		for _, m := range cssVarDeclRe.FindAllStringSubmatch(css, -1) {
			declared[m[1]] = true
		}
	}
	if len(used) < 10 || len(declared) < 10 {
		t.Fatalf("only %d var() reads and %d declarations scraped — the CSS "+
			"regexes have stopped matching", len(used), len(declared))
	}

	// A property the JS writes that no rule reads is a measurement being
	// computed on every tick and thrown away.
	for _, p := range sortedNames(set) {
		if !used[p] {
			t.Errorf("the player JS sets %s but player.css never reads it — "+
				"either a rule was renamed out from under it, or the write is "+
				"dead. Neither shows up as a broken page in review.", p)
		}
	}

	// And the other direction, scoped to the bar's own namespace: a
	// --np-* the stylesheet reads which nothing declares and nothing
	// writes always resolves to its fallback.
	for _, p := range sortedNames(used) {
		if !strings.HasPrefix(p, "--np-") || declared[p] || set[p] {
			continue
		}
		t.Errorf("player.css reads %s but nothing supplies it: no declaration "+
			"in either stylesheet and no setProperty in the player module. It "+
			"renders as its fallback, forever, silently.", p)
	}
}

func jsSetCustomProperties(t *testing.T) map[string]bool {
	t.Helper()
	dir := filepath.Join("static", "player")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	set := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".js") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, m := range jsSetPropRe.FindAllStringSubmatch(string(b), -1) {
			set[m[1]] = true
		}
	}
	return set
}

// readCSSNoComments strips /* … */ first, for the reason cssCommentRe
// already records next door: this repo's commentary names the tokens it
// discusses, so an unstripped scan reads the prose about --np-loaded as
// a declaration of it.
func readCSSNoComments(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return cssCommentRe.ReplaceAllString(string(b), " ")
}

// sortedNames is the slice form of the sortedKeys helper next door,
// which joins into a string for a failure message. Deterministic order so
// a multi-property break reports the same way every run.
func sortedNames(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
