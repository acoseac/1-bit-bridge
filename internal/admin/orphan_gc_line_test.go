package admin

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/integrity"
)

// TestEveryOrphanRefusalKindIsWorded runs the shipped describeOrphanGCRefusal
// under node for every kind the background orphan sweep can report
// (integrity.OrphanRefusalKinds), and requires each to come back as a
// sentence of its own: not empty, not the fallback that shows the bare key,
// and not another kind's words. Empty includes what console.log prints for
// a case that returns nothing or null ("undefined", "null"): the card would
// then read "Orphan sidecar GC is refusing." with no reason, or with that
// word as one (CodeRabbit on #1071).
//
// The server sends the kind as a key and the console words it, the
// discipline the Jobs page keeps for every bounded reason. That split has
// two sides, and TestEveryJobsFieldIsRenderedSomewhere sees only one: it
// proves app.js READS orphanSidecarGCRefusal, and a kind added to the Go
// side without words would still be read, then shown as its key.
func TestEveryOrphanRefusalKindIsWorded(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped console source")
	}
	// Three kinds: the lost index, the partial walk and the empty catalog
	// (2026-09-28). integrity's TestEveryOrphanRefusalKindIsListed holds
	// the list to every kind the sweep declares.
	kinds := integrity.OrphanRefusalKinds()
	if len(kinds) < 3 {
		t.Fatalf("only %d refusal kind(s) listed; the list is broken, so this test proves nothing", len(kinds))
	}
	names := make([]string, 0, len(kinds)+1)
	for _, k := range kinds {
		names = append(names, string(k))
	}
	// The last one is not a kind: the fallback's own words, to compare
	// against.
	names = append(names, "notAKind")
	lines := describeOrphanGCRefusals(t, node, names)
	fallback := lines[len(lines)-1]
	if !strings.Contains(fallback, "notAKind") {
		t.Fatalf("the fallback does not show the key it was given: %q", fallback)
	}
	seen := map[string]string{}
	for i, k := range kinds {
		line := lines[i]
		switch {
		case strings.TrimSpace(line) == "" || line == "undefined" || line == "null":
			t.Errorf("kind %q has no refusal reason (%q): the Jobs card would say the sweep refuses and not why", k, line)
		case line == strings.ReplaceAll(fallback, "notAKind", string(k)):
			t.Errorf("kind %q falls through to the fallback (%q): the Jobs card would show it as its key", k, line)
		case strings.Contains(line, string(k)):
			t.Errorf("kind %q is shown as its key: %q", k, line)
		case seen[line] != "":
			t.Errorf("kinds %q and %q read the same: %q", seen[line], k, line)
		}
		seen[line] = string(k)
	}
}

// describeOrphanGCRefusals runs describeOrphanGCRefusal on each name and
// returns what it printed, one line each.
func describeOrphanGCRefusals(t *testing.T, node string, names []string) []string {
	t.Helper()
	payload, err := json.Marshal(names)
	if err != nil {
		t.Fatal(err)
	}
	out := runConsoleFunction(t, node, "describeOrphanGCRefusal",
		"for (const k of "+string(payload)+") console.log(describeOrphanGCRefusal(k));")
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) != len(names) {
		t.Fatalf("node printed %d line(s) for %d name(s):\n%s", len(lines), len(names), out)
	}
	return lines
}
