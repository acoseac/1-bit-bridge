package backup_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
)

// A refused recheck can have a cleanup removal in flight when its step is
// given up. Only removal of the last effective veto restores the old record.
func TestSetAsideRemovalFinishesAfterGiveUp(t *testing.T) {
	for _, two := range []bool{false, true} {
		name := "one veto"
		if two {
			name = "two vetoes"
		}
		t.Run(name, func(t *testing.T) {
			requireDirectorySync(t)
			dir := verifiedSnapshot(t, storeWithTracks(t, 5))
			root := filepath.Dir(dir)
			bundle, beside := filepath.Join(dir, backup.VetoFileName), backup.RootVetoPath(dir)
			before := readBytes(t, filepath.Join(dir, backup.ManifestFile))
			t.Cleanup(backup.SetVerifyDeadlineForTest(300 * time.Millisecond))
			t.Cleanup(backup.SetSyncVetoForTest(func(string) error { return errors.New("injected: no veto sync succeeds") }))
			t.Cleanup(backup.SetVerdictWriteFailuresForTest(nil, nil, func(path string) error {
				if path == beside && !two {
					return errors.New("injected: the root veto cannot be created")
				}
				return nil
			}))
			p := newParking()
			t.Cleanup(backup.SetRemoveVetoForTest(func(path string) error {
				if path == bundle {
					p.park()
				}
				return os.Remove(path)
			}))
			callerDone := make(chan struct{})
			t.Cleanup(func() {
				p.release()
				waitCaller(t, callerDone)
				awaitClean(t)
			})
			type answer struct {
				res backup.VerifyResult
				err error
			}
			done := make(chan answer, 1)
			go func() {
				defer close(callerDone)
				res, err := backup.VerifySnapshot(context.Background(), dir, backup.VerifyOptions{BackupsRoot: root})
				done <- answer{res, err}
			}()
			select {
			case <-p.entered:
			case <-time.After(30 * time.Second):
				t.Fatal("setAside never entered its bundle-veto cleanup")
			}
			if !pathExists(t, bundle) || pathExists(t, beside) != two {
				t.Fatal("the cleanup did not start with the requested vetoes")
			}
			select {
			case got := <-done:
				if !errors.Is(got.err, backup.ErrCheckGivenUp) || !got.res.RecordRead || got.res.Record.DatabaseState != backup.StateIncomplete {
					t.Fatalf("while removal is parked: %+v, %v; want a give-up and INCOMPLETE read back", got.res, got.err)
				}
			case <-time.After(backup.StopWriteBound + 5*time.Second):
				t.Fatal("the recheck waited for the cleanup past its bound")
			}
			p.release()
			awaitClean(t)
			if pathExists(t, bundle) || pathExists(t, beside) != two {
				t.Error("the in-flight bundle removal did not finish, or the next removal ran after give-up")
			}
			if after := readBytes(t, filepath.Join(dir, backup.ManifestFile)); !bytes.Equal(before, after) {
				t.Error("the refused recheck changed the earlier check's record")
			}
			got := listed(t, dir)
			want := backup.StateVerified
			if two {
				want = backup.StateIncomplete
			}
			if got.DatabaseState != want || got.FilesState != want {
				t.Errorf("after cleanup: %s/%s, want %s", got.DatabaseState, got.FilesState, want)
			}
		})
	}
}
