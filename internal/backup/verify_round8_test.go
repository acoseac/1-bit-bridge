package backup_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// TestAStopBeforeVerificationReportsItsIncompleteWrite covers both handoffs
// to abandonPending: a slot held by another snapshot and a folder lock held
// by another opener. The recovery publication must not disappear at return.
func TestAStopBeforeVerificationReportsItsIncompleteWrite(t *testing.T) {
	for _, where := range []string{"queued for the slot", "waiting for the folder lock"} {
		t.Run(where, func(t *testing.T) {
			c := captureT(t, storeWithTracks(t, 5))
			root := filepath.Dir(c.Dir)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			release := func() {}
			if where == "queued for the slot" {
				blocker := captureT(t, storeWithTracks(t, 5))
				p := newParking()
				release = p.release
				t.Cleanup(backup.SetAfterCheckForTest(func(dir string) {
					if dir == blocker.Dir {
						p.park()
					}
				}))
				t.Cleanup(backup.SetOnSlotWaitForTest(cancel))
				callers := joinBackupCallers(t, func() { release(); cancel() })
				callers.start(func() {
					backup.FinishCapture(context.Background(), blocker, nil, backup.VerifyOptions{})
				})
				select {
				case <-p.entered:
				case <-time.After(30 * time.Second):
					t.Fatal("blocker never acquired the slot")
				}
			} else {
				f, err := fsutil.OpenLockFile(root, backup.LockFileName)
				if err != nil {
					t.Fatal(err)
				}
				if err := fsutil.TryLock(f); err != nil {
					f.Close()
					t.Fatal(err)
				}
				release = sync.OnceFunc(func() { _ = fsutil.Unlock(f); _ = f.Close() })
				t.Cleanup(backup.SetOnLockWaitForTest(func() { cancel(); release() }))
				joinBackupCallers(t, func() { release(); cancel() })
			}
			res, err := backup.VerifySnapshot(ctx, c.Dir, backup.VerifyOptions{BackupsRoot: root})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("want cancellation, got %v", err)
			}
			if res.Write != backup.WriteRecorded || !res.Recorded {
				t.Fatalf("INCOMPLETE write lost at %s: %+v", where, res)
			}
			requireVerdict(t, "recovery publication", listed(t, c.Dir), backup.StateIncomplete, backup.ReasonCancelled, backup.StateIncomplete, backup.ReasonCancelled)
		})
	}
}

// TestAnAnsweredVerificationOrClaimCanStillHoldTheFolder pins the release
// window behind the two prune regressions: a result is not an unlock.
func TestAnAnsweredVerificationOrClaimCanStillHoldTheFolder(t *testing.T) {
	for _, action := range []string{"verify", "claim"} {
		t.Run(action, func(t *testing.T) {
			c := captureT(t, storeWithTracks(t, 5))
			c.Release()
			root := filepath.Dir(c.Dir)
			p := newParking()
			t.Cleanup(backup.SetOnLockReleaseForTest(func(dir string) {
				if dir == root {
					p.park()
				}
			}))
			t.Cleanup(func() { p.release(); awaitClean(t) })
			if action == "verify" {
				res, err := backup.VerifySnapshot(t.Context(), c.Dir, backup.VerifyOptions{BackupsRoot: root})
				if err != nil || !res.Recorded {
					t.Fatalf("verification: %+v, %v", res, err)
				}
			} else {
				claimed, err := backup.Claim(t.Context(), c.Dir, root)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(claimed.Release)
			}
			select {
			case <-p.entered:
			case <-time.After(30 * time.Second):
				t.Fatal("lock release never began")
			}
			if pr, err := backup.Prune(root, 1); err != nil || !pr.Skipped {
				t.Fatalf("prune before release: %+v, %v; want skipped", pr, err)
			}
			p.release()
			awaitClean(t)
			if pr, err := backup.Prune(root, 1); err != nil || pr.Skipped {
				t.Fatalf("prune after release: %+v, %v; want run", pr, err)
			}
			awaitClean(t)
		})
	}
}
