package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// saysPath reports whether s names the path p, written as it is or as %q
// writes it (a Windows path's backslashes doubled).
func saysPath(s, p string) bool {
	q := strconv.Quote(p)
	return strings.Contains(s, p) || strings.Contains(s, q[1:len(q)-1])
}

// TestBridgeRestoreNamesTheSnapshotAsItWasTypedBesideARestoreThatHoldsIt
// (backlog B826's third round): bridge restore of a snapshot typed through a
// link to the folder that holds it, with a separator after it, beside another
// process's restore given up that holds that folder (its marker beside the
// lock it holds), is refused before its prompt, exit 1, the install as it
// was, naming the snapshot as typed and cleaned (backup.CleanSnapshotPath),
// never where it resolves, and the restore that holds the folder by its own
// snapshot's name; and it still says that the end of the bridge restore that
// runs that restore ends it, never a restart of the bridge, the advice it
// takes from the refusal's chain.
func TestBridgeRestoreNamesTheSnapshotAsItWasTypedBesideARestoreThatHoldsIt(t *testing.T) {
	_, _, snapA := seedRestorableInstall(t, closedAdminAddr(t))
	cfgB, dataB, _ := seedRestorableInstall(t, closedAdminAddr(t))
	resolved, err := fsutil.ResolveLinks(snapA)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(resolved)
	link := filepath.Join(t.TempDir(), "linked-backups")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("no symbolic link can be made here: %v", err)
	}
	typed := filepath.Join(link, filepath.Base(resolved))
	holdBackupsLockElsewhere(t, root)
	const blocker = "2026-01-02T03-04-05Z"
	writeHoldMarker(t, root, backup.StuckCheck{Snapshot: blocker, Step: "its read of the snapshot it restores", Since: time.Now().UTC(), Restore: true})
	before := readInstallState(t, cfgB, dataB)

	code, stdout, stderr := runRestoreYes(t, cfgB, typed+string(filepath.Separator))
	if code != 1 || strings.Contains(stdout, "Restore complete") {
		t.Errorf("bridge restore beside a restore that holds the folder = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !saysPath(stderr, typed) {
		t.Errorf("bridge restore's refusal does not name %s, as typed and cleaned:\n%s", typed, stderr)
	}
	if typed != resolved && saysPath(stderr, resolved) {
		t.Errorf("bridge restore's refusal names %s, where the snapshot resolves:\n%s", resolved, stderr)
	}
	for _, want := range []string{"(a restore of snapshot " + blocker, "The end of the bridge restore that runs it ends it", "stop that command"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("bridge restore's refusal does not say %q:\n%s", want, stderr)
		}
	}
	if strings.Contains(stderr, "restart the bridge") {
		t.Errorf("bridge restore's refusal advises a restart of the bridge, which runs no restore:\n%s", stderr)
	}
	requireInstallUntouched(t, cfgB, dataB, before)
}
