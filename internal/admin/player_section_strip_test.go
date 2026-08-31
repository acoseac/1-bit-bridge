package admin

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Below 769px the player's section strip is a horizontally-scrolling row
// that opens at scrollLeft 0, so the entries furthest right sat off-screen
// and the strip showed no active state at all. Measured before the fix on
// /folders AND /sources: activeVisible false, scrollLeft 0.
//
// The geometry is pinned by running the SHIPPED function under node rather
// than by a Go replica, for the reason the upload-digest test states: a
// replica asserts its author's beliefs about the code, not the code.

// TestSectionStripRevealsTheActiveEntry drives boot.js's own
// sectionScrollLeft through the cases that decide whether the strip moves.
func TestSectionStripRevealsTheActiveEntry(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped client source")
	}
	fn := extractJSFunction(t,
		readFile(t, filepath.Join("static", "player", "boot.js")), "sectionScrollLeft")

	// A 300px-wide strip holding 800px of entries.
	const base = `{navLeft: 0, navWidth: 300, scrollLeft: 0, maxScroll: 500}`
	cases := []struct {
		name  string
		args  string
		want  int
		moves bool
	}{
		{
			// The desktop rail is a vertical column with nothing to scroll.
			// Returning an offset there would be a write to a scroll
			// position no reader can see.
			name: "vertical rail never moves", args: `{...B, maxScroll: 0, itemLeft: 0, itemWidth: 100}`,
		},
		{
			// route() runs on every filter change, so re-centring
			// unconditionally would yank the strip back from wherever the
			// reader had scrolled it each time they touched a dropdown.
			name: "already visible is left alone", args: `{...B, itemLeft: 10, itemWidth: 80}`,
		},
		{
			name: "entry off the right edge is centred",
			args: `{...B, itemLeft: 520, itemWidth: 80}`,
			// 520 - (300-80)/2
			want: 410, moves: true,
		},
		{
			name: "centring past the end clamps to maxScroll",
			args: `{...B, itemLeft: 760, itemWidth: 80}`,
			want: 500, moves: true,
		},
		{
			name: "entry off the left edge clamps to zero",
			args: `{...B, scrollLeft: 200, itemLeft: -150, itemWidth: 80}`,
			want: 0, moves: true,
		},
		{
			// An entry WIDER than the strip cannot be centred without a
			// negative offset; clamping must show its start rather than
			// scrolling backwards past the first entry.
			name: "entry wider than the strip shows its start",
			args: `{...B, itemLeft: 400, itemWidth: 400}`,
			want: 400, moves: true,
		},
	}

	var script strings.Builder
	script.WriteString(fn)
	script.WriteString("\nconst B = " + base + ";\nconst out = [\n")
	for _, c := range cases {
		script.WriteString("  sectionScrollLeft(" + c.args + "),\n")
	}
	script.WriteString("];\nconsole.log(JSON.stringify(out));\n")

	dir := t.TempDir()
	path := filepath.Join(dir, "strip.mjs")
	if err := os.WriteFile(path, []byte(script.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, path).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got []*int
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("client returned %q, not a list of offsets: %v", out, err)
	}
	if len(got) != len(cases) {
		t.Fatalf("got %d results for %d cases", len(got), len(cases))
	}
	for i, c := range cases {
		switch {
		case !c.moves && got[i] != nil:
			t.Errorf("%s: returned %d, want null — the strip must not move here", c.name, *got[i])
		case c.moves && got[i] == nil:
			t.Errorf("%s: returned null, want %d", c.name, c.want)
		case c.moves && *got[i] != c.want:
			t.Errorf("%s: returned %d, want %d", c.name, *got[i], c.want)
		}
	}
}

// TestSectionStripRevealDoesNotUseScrollIntoView pins the mechanism, not
// just the result.
//
// scrollIntoView walks ANCESTORS, so on a page whose vertical position the
// boost router is separately restoring it becomes a second writer to the
// same scroll state — and that router's own offset is generation-guarded
// precisely because a stale write landing last is a real defect there.
// Assigning the container's scrollLeft cannot reach anything else, and the
// difference is invisible in any assertion about the offset alone.
func TestSectionStripRevealDoesNotUseScrollIntoView(t *testing.T) {
	fn := extractJSFunction(t,
		readFile(t, filepath.Join("static", "player", "boot.js")), "revealActiveSection")
	if strings.Contains(fn, "scrollIntoView") {
		t.Error("revealActiveSection calls scrollIntoView; it must assign the " +
			"strip's own scrollLeft so it cannot scroll the page as a side effect")
	}
	if !strings.Contains(fn, "scrollLeft =") {
		t.Error("revealActiveSection no longer assigns scrollLeft; the strip " +
			"will not move at all")
	}
}
