package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// TestTheCommandsSayWhatEndsARestoreThatHoldsTheSnapshots (backlog B826's
// second round, Codex on its brief): a restore given up that holds a backups
// folder, as the marker its process writes beside the lock it holds says,
// is named a restore by every command that meets it, and what ends it is the
// bridge restore command that runs it, never a restart of the bridge, which
// runs none: bridge backup --verify, bridge restore, the backup ticker's
// reason for taking no snapshot and a prune's words. A verification's marker,
// as written before restores were marked, keeps its words.
func TestTheCommandsSayWhatEndsARestoreThatHoldsTheSnapshots(t *testing.T) {
	for _, tc := range []struct {
		name    string
		restore bool
		says    []string
		never   string
	}{
		{"a restore's", true, []string{"(a restore of snapshot ", "The end of the bridge restore that runs it ends it", "stop that command"}, "restart the bridge"},
		{"a verification's", false, []string{"(snapshot ", "restart the bridge", "stop the bridge backup --verify that runs it"}, "bridge restore that runs it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgA, _, snapA := seedRestorableInstall(t, closedAdminAddr(t))
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
				if strings.Contains(stderr, tc.never) {
					t.Errorf("%s beside %s hold says %q:\n%s", what, tc.name, tc.never, stderr)
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
		})
	}
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
