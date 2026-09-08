package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestManagedSkipsTheHostOperatorChecks — measured against a live hosted
// tenant, `service-manager` and `browser-opener` were the ONLY two
// warnings in an otherwise healthy report, and both told the reader to
// run something on a host they have no shell on ("use `bridge init
// --no-service`", "install missing"). A preflight whose only findings are
// unactionable is worse than one with none: it says something is wrong.
func TestManagedSkipsTheHostOperatorChecks(t *testing.T) {
	ctx := context.Background()

	managed := Deps{Managed: true}
	for name, c := range map[string]Check{
		"service-manager": checkServiceManager(ctx, managed),
		"browser-opener":  checkBrowserOpener(ctx, managed),
		// The log path is the host's too — and unlike the other two this
		// one prints an absolute path from the host into a report the
		// tenant is looking at.
		"log-file-size": checkLogSize(ctx, Deps{Managed: true, LogPath: mustBigLog(t)}),
	} {
		if c.Status != OK {
			t.Errorf("%s on a managed bridge: status = %q, want ok", name, c.Status)
		}
		if !strings.Contains(c.Summary, "skipped") {
			t.Errorf("%s summary = %q, want it to say the check was skipped", name, c.Summary)
		}
		if c.Hint != "" {
			t.Errorf("%s still carries advice for the host operator: %q", name, c.Hint)
		}
	}

	// NEGATIVE CONTROL: unmanaged, both checks actually run. Their
	// verdict depends on the machine the suite runs on, so the assertion
	// is that they answered about THIS host rather than skipping.
	unmanaged := Deps{}
	for name, c := range map[string]Check{
		"service-manager": checkServiceManager(ctx, unmanaged),
		"browser-opener":  checkBrowserOpener(ctx, unmanaged),
		"log-file-size":   checkLogSize(ctx, Deps{LogPath: mustBigLog(t)}),
	} {
		if strings.Contains(c.Summary, "skipped") {
			t.Errorf("%s skipped on an unmanaged bridge: %q", name, c.Summary)
		}
	}
}

// TestManagedDoesNotSilenceTheRestOfThePreflight — the skip is scoped to
// the two checks that are advice for whoever started the process. A
// managed bridge still has roots that can vanish and a cert that expires,
// and those are the reader's problem to report even if not to fix.
func TestManagedDoesNotSilenceTheRestOfThePreflight(t *testing.T) {
	rep := Run(context.Background(), Deps{Managed: true, LibraryRoots: []string{"/nonexistent-root-for-this-test"}})

	var found bool
	for _, c := range rep.Checks {
		if c.Name == checkNameLibraryRoots {
			found = true
			if c.Status == OK {
				t.Errorf("library-roots = ok for a missing root on a managed bridge")
			}
		}
	}
	if !found {
		t.Fatal("library-roots check absent from a managed report")
	}
}

// mustBigLog writes a file over the warn threshold, so the unmanaged arm
// of the test above has something to warn ABOUT. A control against an
// absent file would pass for the wrong reason: checkLogSize skips a
// missing path, which reads the same as the managed skip.
func mustBigLog(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bridge.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(logSizeWarnBytes + 1); err != nil {
		t.Fatal(err)
	}
	return path
}
