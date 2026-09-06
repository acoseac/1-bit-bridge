package main

import (
	"os"
	"strings"
	"testing"
)

// TestUninstallPromptDoesNotClaimDeletionIsImpossible guards a reassurance the
// operator reads at the most consequential prompt in the CLI.
//
// The wipe prompt used to promise that "bridge has no code path that can delete
// --library files (read-only by design)". That was true when written and stopped
// being true when the web upload / delete-as-trash surface landed: trash.Manager
// unlinks inside a library root, gated live on library.allowDelete, and runServe
// describes it as "the only thing in the bridge that removes library content".
//
// The prompt's docblock records that this reassurance exists because a real user
// asked. A promise in that position has to stay true, so it is now scoped to
// this command rather than to the bridge as a whole.
//
// The guard is textual because the claim is text: it fails if the absolute form
// comes back, and separately if the scoped reassurance disappears — a prompt
// that simply drops the line would pass a "no false claim" check while losing
// the thing the user asked for.
func TestUninstallPromptDoesNotClaimDeletionIsImpossible(t *testing.T) {
	src, err := os.ReadFile("menu.go")
	if err != nil {
		t.Fatal(err)
	}
	// Only the lines that actually reach the operator. Scanning the whole
	// file would fire on the docblock above actUninstall, which has to be
	// able to describe the old claim in order to explain why it changed —
	// the same trap the CSS-class guard in internal/admin hit, where this
	// repo's commentary names the very things it discusses.
	var printed []string
	for _, line := range strings.Split(string(src), "\n") {
		if strings.Contains(line, "fmt.Fprint") {
			printed = append(printed, line)
		}
	}
	if len(printed) == 0 {
		t.Fatal("found no Fprint call sites in menu.go — the scan is not seeing the file")
	}
	body := strings.Join(printed, "\n")

	for _, banned := range []string{
		"no code path that",
		"read-only by design",
	} {
		if strings.Contains(body, banned) {
			t.Errorf("menu.go still claims %q. The bridge CAN delete library files since the "+
				"delete-as-trash surface landed (trash.Manager, gated on library.allowDelete). "+
				"Scope the promise to this command instead of restating it about the bridge.", banned)
		}
	}

	if !strings.Contains(body, "this wipe removes only the config dir") {
		t.Error("the scoped library reassurance is gone from the wipe prompt. It exists because a " +
			"real operator asked whether this destroys their music; dropping it answers them " +
			"with silence at the moment they are typing WIPE.")
	}
}
