package backup_test

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// throughALinkedFolder is the snapshot at snap, a resolved path, named through
// a link made elsewhere to the folder that holds it: the spelling a restore is
// given, which resolves to snap, as Windows' 8.3 temporary folder and macOS's
// /var resolve elsewhere. It skips where no link can be made.
func throughALinkedFolder(t *testing.T, snap string) string {
	t.Helper()
	return throughALinkedFolderNamed(t, snap, "linked-backups")
}

// throughALinkedFolderNamed is throughALinkedFolder with the link named name.
func throughALinkedFolderNamed(t *testing.T, snap, name string) string {
	t.Helper()
	link := filepath.Join(resolvedTestDir(t, t.TempDir()), name)
	if err := os.Symlink(filepath.Dir(snap), link); err != nil {
		t.Skipf("no symbolic link can be made here: %v", err)
	}
	given := filepath.Join(link, filepath.Base(snap))
	if got := resolvedTestDir(t, given); got != snap {
		t.Fatalf("premise: %s resolves to %s, want %s", given, got, snap)
	}
	return given
}

// throughALinkToTheSnapshot is the snapshot at snap, a resolved path, named
// through a link made elsewhere to the snapshot itself: the other spelling a
// restore can be given that resolves to snap, its folder another than the
// folder that holds snap (backlog B871's item 14). It skips where no link can
// be made.
func throughALinkToTheSnapshot(t *testing.T, snap string) string {
	t.Helper()
	return throughALinkToTheSnapshotNamed(t, snap, "latest")
}

// throughALinkToTheSnapshotNamed is throughALinkToTheSnapshot with the link
// named name.
func throughALinkToTheSnapshotNamed(t *testing.T, snap, name string) string {
	t.Helper()
	link := filepath.Join(resolvedTestDir(t, t.TempDir()), name)
	if err := os.Symlink(snap, link); err != nil {
		t.Skipf("no symbolic link can be made here: %v", err)
	}
	if got := resolvedTestDir(t, link); got != snap {
		t.Fatalf("premise: %s resolves to %s, want %s", link, got, snap)
	}
	return link
}

// spelling is a way a restore is given its snapshot otherwise than as it
// resolves, each of which would show the spelling where it resolves in words
// that leaked it (backlog B871's item 14): through a link to the folder that
// holds it, and through a link to the snapshot itself. named makes the link
// named as given.
type spelling struct {
	name    string
	through func(t *testing.T, snap string) string
	named   func(t *testing.T, snap, name string) string
}

// spellings are the two ways a restore is given its snapshot through a link.
var spellings = []spelling{
	{"through a linked folder", throughALinkedFolder, throughALinkedFolderNamed},
	{"through a link to the snapshot", throughALinkToTheSnapshot, throughALinkToTheSnapshotNamed},
}

// says reports whether msg names the path p, written as it is or as %q
// writes it (a Windows path's backslashes doubled).
func says(msg, p string) bool {
	q := strconv.Quote(p)
	return strings.Contains(msg, p) || strings.Contains(msg, q[1:len(q)-1])
}

// requireNamedAsGiven fails the test where what err says does not name given,
// the snapshot (or a file in it) as the restore was given it, or names a path
// in the folder that holds resolved, the snapshot where it resolves, but the
// lock's file there, which the lock's own words name where the snapshot
// resolves (backlog B871's item 12). resolved is "" where the words may name
// where the snapshot resolves after the path given: a lookup's own words.
func requireNamedAsGiven(t *testing.T, what string, err error, given, resolved string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s answered nil", what)
		return
	}
	msg := err.Error()
	if !says(msg, given) {
		t.Errorf("%s does not name %s, as the restore was given it: %v", what, given, err)
	}
	if resolved == "" {
		return
	}
	lock := filepath.Join(filepath.Dir(resolved), backup.LockFileName)
	q := strconv.Quote(lock)
	rest := strings.ReplaceAll(strings.ReplaceAll(msg, lock, ""), q[1:len(q)-1], "")
	if folder := filepath.Dir(resolved) + string(filepath.Separator); says(rest, folder) {
		t.Errorf("%s names a path in %s, where the snapshot resolves, other than its lock's file: %v", what, folder, err)
	}
}

// TestARestoreNamesTheSnapshotAsItWasGiven is backlog B826's third round's
// red: a restore reads its snapshot where it resolves, in the folder whose
// lock it holds, and names it as it was given it. A snapshot named through a
// link to the folder that holds it (as Windows names a temporary folder by
// its 8.3 spelling, which resolves to the long one) whose player-uuid holds
// no UUID is refused naming that copy below the path given, never below where
// it resolves, still config.ErrNotOnePlayerUUID, with the way through, and
// nothing restored. Red on round 2, which named every path in the snapshot
// where it resolves once it held the lock. Since backlog B871 it runs both ways
// a snapshot is given through a link (spellings), its folder linked and the
// snapshot itself.
func TestARestoreNamesTheSnapshotAsItWasGiven(t *testing.T) {
	for _, sp := range spellings {
		t.Run(sp.name, func(t *testing.T) {
			src := storeWithTracks(t, 5)
			withPlayerUUID(t, &src, snapshotPlayerUUID+"\n", 0o600)
			snap := snapshotOf(t, src)
			given := sp.through(t, snap)
			if err := os.WriteFile(filepath.Join(snap, backup.PlayerUUIDFileName), []byte("not-a-uuid\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			b := newDestinationBox(t)
			keepConfigsLock(t, b)

			err := backup.RestoreWithOptions(given, b.targets, backup.RestoreOptions{})
			if !errors.Is(err, config.ErrNotOnePlayerUUID) || err == nil || !strings.Contains(err.Error(), "remove it from the snapshot") {
				t.Errorf("the restore of a snapshot whose player-uuid holds no UUID answered %v, want config.ErrNotOnePlayerUUID and the way through", err)
			}
			requireNamedAsGiven(t, "the refusal of the snapshot's player-uuid", err, filepath.Join(given, backup.PlayerUUIDFileName), snap)
			b.requireUntouched(t, "the restore of a player-uuid that holds no UUID")
		})
	}
}

// keepConfigsLock makes the lock file beside B's bridge.yaml, which a restore
// that restores one takes before it reads the snapshot's files (backlog
// B506), and keeps it among the entries B holds: a refusal after the admission
// writes nothing, and takes that lock.
func keepConfigsLock(t *testing.T, b *destinationBox) {
	t.Helper()
	l, err := config.LockFile(b.targets.BridgeYAML)
	if err != nil {
		t.Fatal(err)
	}
	l.Unlock()
	for _, name := range names(t, b.dir) {
		if !slices.Contains(b.listing, name) {
			b.keep(t, filepath.Join(b.dir, name))
		}
	}
}

// distinctSnapshot is a snapshot, as resolved, of an install of its own whose
// token store, TLS pair and config hold bytes no other install's hold.
func distinctSnapshot(t *testing.T, label string) string {
	t.Helper()
	src := storeWithTracks(t, 5)
	for p, data := range map[string]string{
		src.TokensJSON: `[{"install":"` + label + `"}]`,
		src.ServerCert: label + "'s certificate",
		src.ServerKey:  label + "'s key",
		src.BridgeYAML: "libraryRoots:\n  - /" + label + "\n",
	} {
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return snapshotOf(t, src)
}

// TestARestoreReadsTheSnapshotItLocked (backlog B826's third round): a restore
// given a link to its snapshot reads the snapshot the link led to as the
// restore resolved it and took its folder's lock, never the one it leads to
// by the time the files are read: repointed, once the lock is held, to
// another install's snapshot, the restore still restores the first one's
// files. Its control reads through the path given: the restore then restores
// the other's.
func TestARestoreReadsTheSnapshotItLocked(t *testing.T) {
	first := distinctSnapshot(t, "first")
	second := distinctSnapshot(t, "second")
	link := filepath.Join(resolvedTestDir(t, t.TempDir()), "latest")
	if err := os.Symlink(first, link); err != nil {
		t.Skipf("no symbolic link can be made here: %v", err)
	}
	b := newDestinationBox(t)
	var repointed error
	opts := backup.WithSourceHeldForTest(backup.RestoreOptions{}, func() {
		if repointed = os.Remove(link); repointed == nil {
			repointed = os.Symlink(second, link)
		}
	})
	if err := backup.RestoreWithOptions(link, b.targets, opts); err != nil {
		t.Fatalf("the restore through the link repointed once it held the lock answered %v", err)
	}
	if repointed != nil {
		t.Fatalf("premise: repointing the link: %v", repointed)
	}
	if got := resolvedTestDir(t, link); got != second {
		t.Fatalf("premise: the link leads to %s, want the second snapshot", got)
	}
	b.requireRestored(t, first)
}

// TestTwoSpellingsOfASnapshotsFolderShareItsLock (backlog B826's third round):
// a restore given its snapshot through a link to the folder that holds it
// takes the lock of that folder where it resolves, the lock every holder of
// the folder takes whatever path it is given: while the restore holds it, a
// prune of the folder and another process's verification of a snapshot
// there, each given the folder's own path, are refused, and once the restore
// has returned the folder is free.
func TestTwoSpellingsOfASnapshotsFolderShareItsLock(t *testing.T) {
	src := storeWithTracks(t, 5)
	snap := snapshotOf(t, src)
	other := snapshotOf(t, src)
	root := filepath.Dir(snap)
	given := throughALinkedFolder(t, snap)
	b := newDestinationBox(t)

	var held bool
	var pr backup.PruneResult
	var heldErr, prErr, verifyErr error
	opts := backup.WithSourceHeldForTest(backup.RestoreOptions{}, func() {
		held, heldErr = backupsLockHeld(root)
		pr, prErr = backup.Prune(root, 0)
		_, verifyErr = backup.VerifySnapshotAsAnotherProcessForTest(t.Context(), other, backup.VerifyOptions{LockWait: 300 * time.Millisecond})
	})
	if err := backup.RestoreWithOptions(given, b.targets, opts); err != nil {
		t.Fatalf("the restore through the linked folder answered %v", err)
	}
	if heldErr != nil || !held {
		t.Errorf("the folder's lock, asked by its own path while the restore through the link held it: held %v, %v; want it held", held, heldErr)
	}
	if prErr != nil || !pr.Skipped || !errors.Is(pr.SkippedFor, backup.ErrBackupsBusy) {
		t.Errorf("a prune of the folder by its own path while the restore held it: %+v, %v; want its round skipped for the restore's lock", pr, prErr)
	}
	if !errors.Is(verifyErr, backup.ErrBackupsBusy) {
		t.Errorf("another process's verification of a snapshot there by the folder's own path answered %v, want ErrBackupsBusy", verifyErr)
	}
	b.requireRestored(t, snap)
	awaitClean(t)
	if held, err := backupsLockHeld(root); err != nil || held {
		t.Errorf("the folder's lock once the restore returned: held %v, %v; want it free", held, err)
	}
}

// refusal is one way a restore's admission refuses its snapshot, set up: the
// snapshot as the restore is given it and where it resolves, the restore's
// context and wait, what the refusal carries (its sentinel or typed error),
// whether a failed call's own words may name the snapshot where it resolves
// after the path given, and what else its words say ("nothing was restored"
// unless set: the manifest's refusal keeps the words it had).
type refusal struct {
	given, resolved string
	ctx             context.Context
	wait            time.Duration
	carries         func(error) bool
	resolvedFollows bool
	says            string
}

// heldBy is a refusal's check that a hold given up whose snapshot is named
// blocker refuses it: ErrCheckStillRunning, a StillRunningError naming that
// hold as it is named, never the snapshot the restore was given.
func heldBy(blocker string) func(error) bool {
	return func(err error) bool {
		var held *backup.StillRunningError
		return errors.Is(err, backup.ErrCheckStillRunning) && errors.As(err, &held) && held.Check.Snapshot == blocker
	}
}

// is is a refusal's check that it carries target.
func is(target error) func(error) bool {
	return func(err error) bool { return errors.Is(err, target) }
}

// TestEveryAdmissionRefusalNamesTheSnapshotAsItWasGiven (backlog B826's third
// round): each way a restore's admission refuses a snapshot named through a
// link to the folder that holds it names the snapshot as the restore was
// given it, and never where it resolves (but after it, in the words of a
// lookup that failed where it led), and carries the sentinel or typed error
// its callers choose their advice by: a hold given up in this process, met by
// the spelling given or by the folder the snapshot resolves into, and another
// process's, met by its marker beside the lock it holds, each keeping its own
// name; the lock held past the wait; no lock to be had there; the lock's file
// failing otherwise; the snapshot gone before it resolves, and its manifest
// gone, or not reading, once the lock is held; the snapshot's lookup failing
// otherwise; the restore stopped; and its folder not answering past the stop.
func TestEveryAdmissionRefusalNamesTheSnapshotAsItWasGiven(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stand func(t *testing.T, through func(*testing.T, string) string) refusal
		// shapeless: the case makes the spelling it is given itself.
		shapeless bool
	}{
		{"a hold given up in this process, by the spelling given", func(t *testing.T, through func(*testing.T, string) string) refusal {
			real := resolvedTestDir(t, t.TempDir())
			data := filepath.Join(resolvedTestDir(t, t.TempDir()), "data")
			if err := os.Symlink(real, data); err != nil {
				t.Skipf("no symbolic link can be made here: %v", err)
			}
			src := primeLiveState(t, data)
			c := captureT(t, src)
			other := snapshotOf(t, src)
			given := filepath.Join(data, backup.BackupsDirName, filepath.Base(other))
			if given == other {
				t.Fatalf("premise: the snapshot %s is named as it resolves", given)
			}
			givenUpCheckOf(t, c)
			return refusal{given: given, resolved: other, carries: heldBy(filepath.Base(c.Dir))}
		}, true},
		{"a hold given up in this process, by the folder the snapshot resolves into", func(t *testing.T, through func(*testing.T, string) string) refusal {
			src := storeWithTracks(t, 5)
			c := captureT(t, src)
			other := snapshotOf(t, src)
			givenUpCheckOf(t, c)
			return refusal{given: through(t, other), resolved: other, carries: heldBy(filepath.Base(c.Dir))}
		}, false},
		{"another process's hold given up, by its marker beside the lock it holds", func(t *testing.T, through func(*testing.T, string) string) refusal {
			src := storeWithTracks(t, 5)
			blocker := snapshotOf(t, src)
			snap := snapshotOf(t, src)
			holdLockAsAnotherProcess(t, filepath.Dir(snap))
			writeStuckMarker(t, filepath.Dir(snap), filepath.Base(blocker))
			return refusal{given: through(t, snap), resolved: snap, wait: 20 * time.Second, carries: heldBy(filepath.Base(blocker))}
		}, false},
		{"the lock held past the wait", func(t *testing.T, through func(*testing.T, string) string) refusal {
			snap := snapshotOf(t, storeWithTracks(t, 5))
			holdLockAsAnotherProcess(t, filepath.Dir(snap))
			return refusal{given: through(t, snap), resolved: snap, wait: 300 * time.Millisecond, carries: is(backup.ErrBackupsBusy)}
		}, false},
		{"no lock to be had there", func(t *testing.T, through func(*testing.T, string) string) refusal {
			snap := snapshotOf(t, storeWithTracks(t, 5))
			root := filepath.Dir(snap)
			t.Cleanup(backup.SetOpenBackupsLockForTest(func(dir, name string) (*os.File, error) {
				if dir == root {
					return nil, &fs.PathError{Op: "open", Path: filepath.Join(dir, name), Err: syscall.EROFS}
				}
				return fsutil.OpenLockFile(dir, name)
			}))
			return refusal{given: through(t, snap), resolved: snap, carries: is(backup.ErrBackupsUnlocked)}
		}, false},
		{"the lock's file failing otherwise", func(t *testing.T, through func(*testing.T, string) string) refusal {
			snap := snapshotOf(t, storeWithTracks(t, 5))
			root := filepath.Dir(snap)
			t.Cleanup(backup.SetOpenBackupsLockForTest(func(dir, name string) (*os.File, error) {
				if dir == root {
					return nil, &fs.PathError{Op: "open", Path: filepath.Join(dir, name), Err: syscall.EIO}
				}
				return fsutil.OpenLockFile(dir, name)
			}))
			return refusal{given: through(t, snap), resolved: snap, carries: is(syscall.EIO)}
		}, false},
		{"the snapshot gone before it resolves", func(t *testing.T, through func(*testing.T, string) string) refusal {
			snap := snapshotOf(t, storeWithTracks(t, 5))
			given := through(t, snap)
			if err := os.RemoveAll(snap); err != nil {
				t.Fatal(err)
			}
			return refusal{given: given, resolved: snap, carries: is(backup.ErrSnapshotGone), resolvedFollows: true}
		}, false},
		{"its manifest gone once the lock is held", func(t *testing.T, through func(*testing.T, string) string) refusal {
			snap := snapshotOf(t, storeWithTracks(t, 5))
			if err := os.Remove(filepath.Join(snap, backup.ManifestFile)); err != nil {
				t.Fatal(err)
			}
			return refusal{given: through(t, snap), resolved: snap, carries: is(backup.ErrSnapshotGone)}
		}, false},
		{"its manifest not reading once the lock is held", func(t *testing.T, through func(*testing.T, string) string) refusal {
			snap := snapshotOf(t, storeWithTracks(t, 5))
			if err := os.WriteFile(filepath.Join(snap, backup.ManifestFile), []byte("{not json"), 0o600); err != nil {
				t.Fatal(err)
			}
			return refusal{given: through(t, snap), resolved: snap, says: "read snapshot manifest", carries: func(err error) bool {
				var syntax *json.SyntaxError
				return errors.As(err, &syntax)
			}}
		}, false},
		{"its lookup failing otherwise", func(t *testing.T, through func(*testing.T, string) string) refusal {
			snap := snapshotOf(t, storeWithTracks(t, 5))
			given := through(t, snap)
			// As a lookup that fails past the link says it: where it led.
			t.Cleanup(backup.SetMetadataForTest(func(p string) (string, error) {
				if p == given {
					return "", &fs.PathError{Op: "lstat", Path: snap, Err: fs.ErrPermission}
				}
				return fsutil.ResolveLinks(p)
			}, nil))
			return refusal{given: given, resolved: snap, carries: is(fs.ErrPermission), resolvedFollows: true}
		}, false},
		{"the restore stopped", func(t *testing.T, through func(*testing.T, string) string) refusal {
			snap := snapshotOf(t, storeWithTracks(t, 5))
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return refusal{given: through(t, snap), resolved: snap, ctx: ctx, carries: is(context.Canceled)}
		}, false},
	} {
		for _, sp := range spellings {
			if tc.shapeless && sp.name != spellings[0].name {
				continue
			}
			t.Run(tc.name+", "+sp.name, func(t *testing.T) {
				r := tc.stand(t, sp.through)
				b := newDestinationBox(t)
				says := r.says
				if says == "" {
					says = "nothing was restored"
				}
				err := backup.RestoreWithOptions(r.given, b.targets, backup.RestoreOptions{Context: r.ctx, LockWait: r.wait})
				if !r.carries(err) {
					t.Errorf("the restore answered %v, which does not carry what the refusal of %s carries", err, tc.name)
				}
				if err == nil || !strings.Contains(err.Error(), says) {
					t.Errorf("the restore answered %v, want a refusal that says %q", err, says)
				}
				resolved := r.resolved
				if r.resolvedFollows {
					resolved = ""
				}
				requireNamedAsGiven(t, "the refusal", err, r.given, resolved)
				b.requireUntouched(t, "the refused restore")
			})
		}
	}
}

// TestARestoreRefusedPastTheStopOfItsWaitNamesTheSnapshotAsItWasGiven is the
// last of the admission's refusals (backlog B826's third round): a restore
// whose snapshot's lookup does not answer, stopped, is refused within
// StopWriteBound, naming the snapshot as it was given, its context's error in
// the chain.
func TestARestoreRefusedPastTheStopOfItsWaitNamesTheSnapshotAsItWasGiven(t *testing.T) {
	snap := snapshotOf(t, storeWithTracks(t, 5))
	given := throughALinkedFolder(t, snap)
	b := newDestinationBox(t)
	ends := sourceWorkerEnds(t)
	p := newParking()
	var armed atomic.Bool
	t.Cleanup(backup.SetMetadataForTest(func(path string) (string, error) {
		if path == given && armed.CompareAndSwap(true, false) {
			p.park()
		}
		return fsutil.ResolveLinks(path)
	}, nil))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	callers := joinBackupCallers(t, p.release)
	done := make(chan error, 1)
	armed.Store(true)
	callers.start(func() {
		done <- backup.RestoreWithOptions(given, b.targets, backup.RestoreOptions{Context: ctx, LockWait: 20 * time.Second})
	})
	awaitEntered(t, p.entered, "the snapshot's lookup")
	cancel()
	err := requireReturnedWithin(t, done, time.Now(), backup.StopWriteBound+2*time.Second, "a restore whose snapshot's lookup does not answer")
	if !errors.Is(err, context.Canceled) || err == nil || !strings.Contains(err.Error(), "did not answer within") {
		t.Errorf("the stopped restore answered %v, want its stop's refusal past its bound", err)
	}
	requireNamedAsGiven(t, "the refusal past the stop", err, given, snap)
	p.release()
	awaitSourceWorkerEnd(t, ends, given)
	awaitClean(t)
	b.requireUntouched(t, "the restore refused past its stop")
	callers.wait(t)
}
