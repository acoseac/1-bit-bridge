package main

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/packaging"
)

// TestUninstallPromptDoesNotClaimDeletionIsImpossible guards a reassurance the
// operator reads at the most consequential prompt in the CLI.
//
// The wipe prompt used to promise that the bridge could not delete --library
// files at all, being read-only by design. That was true when written and
// stopped being true when the web upload / delete-as-trash surface landed:
// trash.Manager unlinks inside a library root, gated live on
// library.allowDelete, and runServe describes it as "the only thing in the
// bridge that removes library content".
//
// The prompt's docblock records that this reassurance exists because a real
// user asked. A promise in that position has to stay true, so it is now scoped
// to this command rather than restated about the bridge as a whole.
//
// This drives actUninstall itself rather than scanning menu.go's source. The
// first version did scan, and it was wrong twice over: it fired on the docblock
// explaining the change (this package's commentary names what it discusses),
// and a source scan cannot survive a reflow or a switch to another print
// helper. Driving the real function is also what this repo's own test
// discipline asks for — a test that never touches the wiring proves nothing.
// (gemini-code-assist, PR #855.)
func TestUninstallPromptDoesNotClaimDeletionIsImpossible(t *testing.T) {
	// A real config dir, so a regression that actually wipes is visible as a
	// missing file rather than as an assertion about text.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(cfgPath, []byte("libraryRoots: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	// "no" at the WIPE prompt: the exact-phrase check refuses it, so nothing
	// is deleted and we still get the whole prompt on stdout.
	//
	// kind: KindNone skips the service-uninstall question above it, which
	// would otherwise consume this line.
	in := bufio.NewReader(strings.NewReader("no\n"))
	state := menuState{initialized: true, cfgPath: cfgPath, kind: packaging.KindNone}

	actUninstall(context.Background(), in, &stdout, &stderr, state)
	out := stdout.String()

	if !strings.Contains(out, "cancelled") {
		t.Fatalf("the wipe was not cancelled by a non-WIPE answer; this test must not be able "+
			"to delete anything:\nstdout:\n%s\nstderr:\n%s", out, stderr.String())
	}
	if _, err := os.Stat(cfgPath); err != nil {
		t.Fatalf("the config file was removed despite a non-WIPE answer: %v", err)
	}

	for _, banned := range []string{"no code path that", "read-only by design"} {
		if strings.Contains(out, banned) {
			t.Errorf("the wipe prompt still tells the operator %q. The bridge CAN delete library "+
				"files since the delete-as-trash surface landed (trash.Manager, gated on "+
				"library.allowDelete). Scope the promise to this command instead of restating "+
				"it about the bridge.\nprompt:\n%s", banned, out)
		}
	}

	// The reassurance must not merely stop being false — it must still be
	// there. A prompt that drops the line would pass a "no false claim" check
	// while answering the operator with silence at the moment they are asked
	// to type WIPE.
	if !strings.Contains(out, "your music library") {
		t.Errorf("the wipe prompt no longer mentions the music library at all. It exists because "+
			"a real operator asked whether this destroys their music.\nprompt:\n%s", out)
	}
	if !strings.Contains(out, "not touched") {
		t.Errorf("the wipe prompt mentions the library but no longer says it is left alone:\n%s", out)
	}
}
