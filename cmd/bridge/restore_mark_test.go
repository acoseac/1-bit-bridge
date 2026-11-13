package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// writeHoldMarker leaves beside the lock of the backups folder at root the
// marker of a hold given up, as the process that holds the lock writes it,
// and answers its path.
func writeHoldMarker(t *testing.T, root string, s backup.StuckCheck) string {
	t.Helper()
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, backup.StuckMarkerName)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// restoreHoldWords are what bridge restore and bridge backup --verify say of a
// restore given up that holds the snapshots (stillRunningEnd, backlog B871's
// item 6): what holds them, a step left in a process still running or waiting
// on storage that stopped answering, and what the operator can do; never
// "stop that command", which can name a command that already printed its
// refusal and ended.
var restoreHoldWords = []string{"left in a process that is still running", "a hung mount", "end that bridge restore process if one still runs"}

// holdWordsCase is a case of
// TestTheCommandsSayWhatEndsARestoreThatHoldsTheSnapshots: whether the hold
// its marker names is a restore's, what every command says of it, and what
// none says.
type holdWordsCase struct {
	name    string
	restore bool
	says    []string
	never   []string
}

// holdWordsCases are a restore's marker and a verification's.
var holdWordsCases = []holdWordsCase{
	{"a restore's", true, append([]string{"(a restore of snapshot "}, restoreHoldWords...), []string{"restart the bridge", "stop that command"}},
	{"a verification's", false, []string{"(snapshot ", "restart the bridge", "stop the bridge backup --verify that runs it"}, []string{"bridge restore that runs it", "bridge restore process"}},
}

// TestTheCommandsSayWhatEndsARestoreThatHoldsTheSnapshots (backlog B826's
// second round, Codex on its brief): a restore given up that holds a backups
// folder, as the marker its process writes beside the lock it holds says,
// is named a restore by every command that meets it, never as a check that a
// restart of the bridge ends, which runs none: bridge backup --verify, bridge
// restore, the backup ticker's reason for taking no snapshot and a prune's
// words. Since backlog B871 (item 6) the commands say what holds the folder
// and what can be done, never "stop that command". A verification's marker,
// as written before restores were marked, keeps its words.
func TestTheCommandsSayWhatEndsARestoreThatHoldsTheSnapshots(t *testing.T) {
	for _, tc := range holdWordsCases {
		t.Run(tc.name, func(t *testing.T) { commandsSayWhatEndsAHold(t, tc, nil) })
	}
}

// commandsSayWhatEndsAHold is a case of
// TestTheCommandsSayWhatEndsARestoreThatHoldsTheSnapshots, whole, which
// TestEachHoldWordsCaseWaitsForItsVerificationsEnd runs too, its snapshot
// handed to onSnapshot, where that is set, as soon as it is made and before
// anything verifies it.
func commandsSayWhatEndsAHold(t *testing.T, tc holdWordsCase, onSnapshot func(t *testing.T, snap string)) {
	cfgA, _, snapA := seedRestorableInstall(t, closedAdminAddr(t))
	if onSnapshot != nil {
		onSnapshot(t, snapA)
	}
	cfgB, dataB, _ := seedRestorableInstall(t, closedAdminAddr(t))
	root, err := fsutil.ResolveLinks(filepath.Dir(snapA))
	if err != nil {
		t.Fatal(err)
	}
	holdBackupsLockElsewhere(t, root)
	step := "its check"
	if tc.restore {
		step = "its read of the snapshot it restores"
	}
	writeHoldMarker(t, root, backup.StuckCheck{Snapshot: filepath.Base(snapA), Step: step, Since: time.Now().UTC(), Restore: tc.restore})
	prev := verifyCmdLockWait
	verifyCmdLockWait = 5 * time.Second
	t.Cleanup(func() { verifyCmdLockWait = prev })
	// bridge backup --verify answers before its verification's worker lets go
	// of what it held: the case waits for that worker's end before its seam
	// goes back and its fixtures go (backlog B871's round 3).
	awaitVerifyEndOnCleanup(t, backup.CleanSnapshotPath(snapA))
	before := readInstallState(t, cfgB, dataB)

	var out, verr strings.Builder
	if code := backupCmd([]string{"--config", cfgA, "--verify", snapA}, &out, &verr); code != 1 {
		t.Errorf("bridge backup --verify beside %s hold = %d\n%s", tc.name, code, verr.String())
	}
	code, stdout, rerr := runRestoreYes(t, cfgB, snapA)
	if code != 1 || strings.Contains(stdout, "Restore complete") {
		t.Errorf("bridge restore beside %s hold = %d\n%s", tc.name, code, rerr)
	}
	for what, stderr := range map[string]string{"bridge backup --verify": verr.String(), "bridge restore": rerr} {
		for _, want := range tc.says {
			if !strings.Contains(stderr, want) {
				t.Errorf("%s beside %s hold does not say %q:\n%s", what, tc.name, want, stderr)
			}
		}
		for _, never := range tc.never {
			if strings.Contains(stderr, never) {
				t.Errorf("%s beside %s hold says %q:\n%s", what, tc.name, never, stderr)
			}
		}
	}
	requireInstallUntouched(t, cfgB, dataB, before)

	why, held := captureHeldBack(context.Background(), root)
	want := "a check of " + filepath.Base(snapA) + " that did not end still holds the backups folder"
	if tc.restore {
		want = "a restore of " + filepath.Base(snapA) + " that did not end still holds the backups folder"
	}
	if !held || !strings.Contains(why, want) {
		t.Errorf("the ticker's reason = %q, %v; want %q", why, held, want)
	}
	skip := pruneSkipWords(&backup.StillRunningError{Check: backup.StuckCheck{Snapshot: "S", Restore: tc.restore}})
	wantSkip := "a check that did not end holds the snapshots"
	if tc.restore {
		wantSkip = "a restore that did not end holds the snapshots"
	}
	if !strings.HasPrefix(skip, wantSkip) {
		t.Errorf("the prune's words = %q, want them to begin %q", skip, wantSkip)
	}
}

// TestEachHoldWordsCaseWaitsForItsVerificationsEnd is backlog B871's round 3.
// CI's race on #1457 (run 43370189144, bridge 4/4) was
// TestTheTickerTakesNoSnapshotWhileItsOwnProbesReleaseDoesNotEnd's
// backup.SetReleaseBoundForTest beside the release of the verification that
// bridge backup --verify ran in a case of
// TestTheCommandsSayWhatEndsARestoreThatHoldsTheSnapshots: the verification
// answers before its worker lets go of what it held (backup.verifyAndThen),
// and the case put verifyCmdLockWait back, removed its fixtures and returned
// while the worker went on. Here each case runs whole with that worker parked
// between its answer and its end (backup.SetAroundVerifyEndForTest), let go on
// only once the case's cleanup waits for its end (onVerifyEndAwait): as the
// worker ends, the case's seam and its snapshot are still as the case made
// them. Red on the round before, whose cases never waited: each returned with
// the worker still parked.
//
// Since round 4 the wrapper is installed once the case knows its snapshot,
// and parks the worker of that snapshot's verification alone: a worker of an
// earlier verification, parked between its drop and its read of the wrapper
// across the wrapper's installation (parkAnEarlierWorker), where a worker a
// test before let go at its drop can stand, passes through it, and the case
// still parks and joins its own. Red on round 3's wrapper, installed before
// the case and parking the first worker that came to it.
func TestEachHoldWordsCaseWaitsForItsVerificationsEnd(t *testing.T) {
	for _, tc := range holdWordsCases {
		t.Run(tc.name, func(t *testing.T) {
			earlier := parkAnEarlierWorker(t)
			letGo := make(chan struct{})
			release := sync.OnceFunc(func() { close(letGo) })
			parked := make(chan struct{})
			passed := make(chan struct{})
			var parkedOne atomic.Bool
			// What the worker found as it ended, written before passed closes.
			var (
				waitAtEnd time.Duration
				snapAtEnd error
			)
			// The wrapper, installed as soon as the case's snapshot is made:
			// it parks the worker of that snapshot's verification, keyed as
			// the case's wait keys it (awaitVerifyEndOnCleanup), a folder in
			// the case's own t.TempDir that no other test's verification
			// names, and which only the case's bridge backup --verify
			// verifies (its bridge restore takes the folder's lock, and runs
			// no verification). Every other worker passes through.
			wrap := func(t *testing.T, snap string) {
				want := backup.CleanSnapshotPath(snap)
				t.Cleanup(backup.SetAroundVerifyEndForTest(func(dir string, end func()) {
					if dir != want || !parkedOne.CompareAndSwap(false, true) {
						end()
						return
					}
					close(parked)
					<-letGo
					waitAtEnd = verifyCmdLockWait
					_, snapAtEnd = os.Lstat(dir)
					end()
					close(passed)
				}))
				// The earlier worker, let go once the wrapper stands, runs it
				// and passes through, before the case verifies anything.
				earlier.letGo()
				select {
				case <-earlier.ended:
				case <-parked:
					t.Fatalf("the case's wrapper parked the worker of an earlier verification, of %s, as its own", earlier.dir)
				case <-time.After(30 * time.Second):
					t.Fatal("the earlier verification's worker never passed the case's wrapper")
				}
			}
			prevAwait := onVerifyEndAwait
			onVerifyEndAwait = func(string) { release() }
			t.Cleanup(func() { onVerifyEndAwait = prevAwait })
			// A case that does not wait returns with the worker parked: it is
			// let go here, and waited for.
			t.Cleanup(func() {
				release()
				select {
				case <-parked:
				default:
					return
				}
				select {
				case <-passed:
				case <-time.After(30 * time.Second):
					t.Error("the parked worker never passed its end")
				}
			})
			t.Run("case", func(t *testing.T) { commandsSayWhatEndsAHold(t, tc, wrap) })
			select {
			case <-parked:
			case <-time.After(30 * time.Second):
				t.Fatal("no verification's worker came to its end in the case")
			}
			select {
			case <-letGo:
			default:
				t.Fatal("the case returned with its verification's worker parked between its answer and its end: its cleanup put its seams back and removed its fixtures without waiting for that end")
			}
			select {
			case <-passed:
			case <-time.After(30 * time.Second):
				t.Fatal("the worker let go never passed its end")
			}
			if waitAtEnd != 5*time.Second {
				t.Errorf("as the worker ended, verifyCmdLockWait was %v, not the case's 5s: the case put its seam back first", waitAtEnd)
			}
			if snapAtEnd != nil {
				t.Errorf("as the worker ended, the case's snapshot was gone (%v): the case removed its fixtures first", snapAtEnd)
			}
		})
	}
}

// earlierWorker is the worker of a verification of a snapshot of its own
// (dir), parked once it has dropped its hold, until letGo; ended closes as
// it runs its end hook.
type earlierWorker struct {
	dir   string
	letGo func()
	ended <-chan struct{}
}

// parkAnEarlierWorker verifies a snapshot of its own and parks its worker
// between its drop and its read of the wrapper of its last step
// (backup.SetOnVerifyDroppedForTest): where a worker stands that a test
// before let go at its drop, all a drain waits for, as the next test installs
// its wrapper (backlog B871's round 4). Its cleanup lets the worker go and
// waits for its end, before the seams go back.
func parkAnEarlierWorker(t *testing.T) earlierWorker {
	t.Helper()
	_, _, snap := seedRestorableInstall(t, closedAdminAddr(t))
	dir := backup.CleanSnapshotPath(snap)
	entered, out, ended := make(chan struct{}), make(chan struct{}), make(chan struct{})
	enter, letGo, end := sync.OnceFunc(func() { close(entered) }), sync.OnceFunc(func() { close(out) }), sync.OnceFunc(func() { close(ended) })
	t.Cleanup(backup.SetOnVerifyEndForTest(func(d string) {
		if d == dir {
			end()
		}
	}))
	t.Cleanup(backup.SetOnVerifyDroppedForTest(func(d string) {
		if d == dir {
			enter()
			<-out
		}
	}))
	t.Cleanup(func() {
		letGo()
		select {
		case <-ended:
		case <-time.After(30 * time.Second):
			t.Errorf("the earlier verification's worker, of %s, never ended", dir)
		}
	})
	if _, err := backup.VerifySnapshot(context.Background(), dir, backup.VerifyOptions{}); err != nil {
		t.Fatalf("the earlier verification: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the earlier verification's worker never dropped its hold")
	}
	if n := backup.ActiveHoldsForTest(); n != 0 {
		t.Fatalf("%d holds active with the earlier worker past its drop; want none, as a drain finds it", n)
	}
	return earlierWorker{dir: dir, letGo: letGo, ended: ended}
}

// TestBridgeRestoreAsksTheMarkerAgainstTheLockBeforeItsPrompt (backlog B826's
// second round, item 2 and Codex's first finding on its brief): bridge
// restore reads the marker beside the lock of the folder the snapshot
// resolves into before its prompt, and counts it only while another open
// holds that lock: a marker beside a held lock refuses the restore before it
// asks, named however the snapshot is (a link to it elsewhere among them); one
// beside a lock nobody holds, left by a process that ended, is no refusal, and
// the restore, confirmed, clears it under the lock and restores.
func TestBridgeRestoreAsksTheMarkerAgainstTheLockBeforeItsPrompt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		held    bool
		through string // "link": the snapshot named through a link elsewhere
	}{
		{"a marker beside a held lock", true, ""},
		{"a marker beside a lock nobody holds", false, ""},
		{"a marker beside a held lock, the snapshot named through a link", true, "link"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, snapA := seedRestorableInstall(t, closedAdminAddr(t))
			cfgB, dataB, _ := seedRestorableInstall(t, closedAdminAddr(t))
			root, err := fsutil.ResolveLinks(filepath.Dir(snapA))
			if err != nil {
				t.Fatal(err)
			}
			named := snapA
			if tc.through == "link" {
				named = filepath.Join(t.TempDir(), "x")
				if err := os.Symlink(snapA, named); err != nil {
					t.Skipf("no symbolic link can be made here: %v", err)
				}
			}
			if tc.held {
				holdBackupsLockElsewhere(t, root)
			}
			marker := writeHoldMarker(t, root, backup.StuckCheck{Snapshot: filepath.Base(snapA), Step: "its check", Since: time.Now().UTC()})
			before := readInstallState(t, cfgB, dataB)

			var stdout, stderr strings.Builder
			args := []string{"--config", cfgB, named}
			if !tc.held {
				args = []string{"--config", cfgB, "--yes", named}
			}
			code := restoreCmd(context.Background(), args, strings.NewReader(""), &stdout, &stderr)
			if tc.held {
				if code != 1 || !strings.Contains(stderr.String(), "did not end still holds them") {
					t.Errorf("bridge restore beside a held lock's marker = %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
				}
				if strings.Contains(stderr.String(), "Type 'yes'") || strings.Contains(stdout.String(), "Restore from") {
					t.Errorf("bridge restore asked for its confirmation beside a held lock's marker:\n%s%s", stdout.String(), stderr.String())
				}
				requireInstallUntouched(t, cfgB, dataB, before)
				return
			}
			if code != 0 || !strings.Contains(stdout.String(), "Restore complete") {
				t.Fatalf("bridge restore beside a stale marker = %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
			}
			if _, err := os.Lstat(marker); !os.IsNotExist(err) {
				t.Errorf("the stale marker outlived the restore that held the lock: %v", err)
			}
		})
	}
}
