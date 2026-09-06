package main

import (
	"bytes"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestEveryDispatchedSubcommandAppearsInUsage pins the two lists against each
// other.
//
// They agree today — 28 for 28 — but nothing held them there, and the failure
// is silent in the direction that matters: a subcommand added to run()'s switch
// and forgotten in usage() works perfectly and is undiscoverable, which is the
// same shape as a settings control that renders but saves nothing.
//
// The package doc and CLAUDE.md's architecture table both listed FIVE
// subcommands until this sweep, which is how far the drift had already gone
// with no guard.
func TestEveryDispatchedSubcommandAppearsInUsage(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)

	// The dispatcher's switch arms. Help aliases are dispatch, not
	// subcommands, so they are excluded by the leading-dash / "help" filter.
	start := strings.Index(body, "func run(ctx context.Context")
	if start < 0 {
		t.Fatal("could not find run() — the scan is not seeing the dispatcher")
	}
	end := strings.Index(body[start:], "\nfunc ")
	if end < 0 {
		t.Fatal("could not bound run()")
	}
	dispatch := body[start : start+end]

	// EVERY quoted name in each case arm, not just the first: an alias arm
	// (`case "duplicates", "dupes":`) dispatches both, and a first-match-only
	// scan would let the alias be undiscoverable — which is exactly what this
	// test's own negative control caught it doing.
	var cmds []string
	seen := map[string]bool{}
	for _, arm := range regexp.MustCompile(`(?m)^\s*case ("[^:]*"):`).FindAllStringSubmatch(dispatch, -1) {
		for _, q := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(arm[1], -1) {
			name := q[1]
			// Help aliases are dispatch, not subcommands.
			if name == "help" || strings.HasPrefix(name, "-") || seen[name] {
				continue
			}
			seen[name] = true
			cmds = append(cmds, name)
		}
	}
	sort.Strings(cmds)
	if len(cmds) < 20 {
		t.Fatalf("found only %d dispatched subcommands (%v); the scan is not seeing the switch", len(cmds), cmds)
	}

	var out bytes.Buffer
	usage(&out)
	text := out.String()

	var missing []string
	for _, c := range cmds {
		// Word-boundary match: "scan" must not be satisfied by "rescan".
		if !regexp.MustCompile(`(^|\s)` + regexp.QuoteMeta(c) + `(\s|$)`).MatchString(text) {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		t.Errorf("run() dispatches %v but usage() never names them. A subcommand missing from "+
			"usage works perfectly and is undiscoverable — add it to the usage block, or if it "+
			"is deliberately unlisted, say so here.\nusage():\n%s", missing, text)
	}
}

// TestIntegrityAdaptersCarryACancellableContext pins the other half of the
// joined stop path.
//
// integrity's stopFn now WAITS for its run goroutine, which is only meaningful
// if the work that goroutine does can actually be cancelled. These adapters sit
// on the interfaces integrity calls (they take no ctx of their own), and both
// used context.Background() — so an AllVariants walk or a DeleteVariant could
// keep going after scanCancel(), straight into Store.Close().
func TestIntegrityAdaptersCarryACancellableContext(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	for _, decl := range []string{
		"func (a *integrityVariantListerAdapter) AllVariants()",
		"func (a *integrityVariantDeleterAdapter) DeleteVariant(",
	} {
		i := strings.Index(body, decl)
		if i < 0 {
			t.Fatalf("could not find %q", decl)
		}
		end := strings.Index(body[i:], "\n}\n")
		if end < 0 {
			t.Fatalf("could not bound %q", decl)
		}
		fn := body[i : i+end]
		if strings.Contains(fn, "context.Background()") {
			t.Errorf("%s uses context.Background(), so integrity's joined stop cannot "+
				"interrupt it — the wait then just delays Store.Close() behind work that "+
				"was never going to stop:\n%s", decl, fn)
		}
	}
}
