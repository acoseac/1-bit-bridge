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
// link, with a separator after it, beside another process's restore given up
// that holds the folder (its marker beside the lock it holds), is refused
// before its prompt, exit 1, the install as it was, naming the snapshot as
// typed and cleaned (backup.CleanSnapshotPath), never anything in the folder
// where it resolves, and the restore that holds the folder by its own
// snapshot's name; and it still says what ends that restore, never a restart
// of the bridge, the advice it takes from the refusal's chain. Since backlog
// B871 (item 14) it is typed both ways a link can lead to the snapshot,
// through a link to the folder that holds it and through a link to the
// snapshot itself, each of which would show where the snapshot resolves in
// words that leaked it: before round 3 the refusal here named no path at all,
// so only a refusal handed the resolved snapshot turns it red.
func TestBridgeRestoreNamesTheSnapshotAsItWasTypedBesideARestoreThatHoldsIt(t *testing.T) {
	for _, way := range []string{"through a linked folder", "through a link to the snapshot"} {
		t.Run(way, func(t *testing.T) {
			_, _, snapA := seedRestorableInstall(t, closedAdminAddr(t))
			cfgB, dataB, _ := seedRestorableInstall(t, closedAdminAddr(t))
			resolved, err := fsutil.ResolveLinks(snapA)
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Dir(resolved)
			var typed string
			if way == "through a linked folder" {
				link := filepath.Join(t.TempDir(), "linked-backups")
				if err := os.Symlink(root, link); err != nil {
					t.Skipf("no symbolic link can be made here: %v", err)
				}
				typed = filepath.Join(link, filepath.Base(resolved))
			} else {
				typed = filepath.Join(t.TempDir(), "latest")
				if err := os.Symlink(resolved, typed); err != nil {
					t.Skipf("no symbolic link can be made here: %v", err)
				}
			}
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
			// The lock's file, which the lock's own words would name where it
			// resolves, aside (backlog B871's item 12).
			lock := filepath.Join(root, backup.LockFileName)
			q := strconv.Quote(lock)
			rest := strings.ReplaceAll(strings.ReplaceAll(stderr, lock, ""), q[1:len(q)-1], "")
			if saysPath(rest, root+string(filepath.Separator)) {
				t.Errorf("bridge restore's refusal names a path in %s, where the snapshot resolves:\n%s", root, stderr)
			}
			for _, want := range append([]string{"(a restore of snapshot " + blocker}, restoreHoldWords...) {
				if !strings.Contains(stderr, want) {
					t.Errorf("bridge restore's refusal does not say %q:\n%s", want, stderr)
				}
			}
			if strings.Contains(stderr, "restart the bridge") || strings.Contains(stderr, "stop that command") {
				t.Errorf("bridge restore's refusal advises a restart of the bridge, which runs no restore, or a stop of a command that may have ended:\n%s", stderr)
			}
			requireInstallUntouched(t, cfgB, dataB, before)
		})
	}
}
