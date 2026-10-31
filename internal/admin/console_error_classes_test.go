package admin

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
)

// TestTheConsoleSaysWhatFailedByItsClass drives the classes no fixture
// reaches on every platform: a read held busy past its retries, whose last
// error on Windows is an access denial and so a permission's too, says busy
// (asked first); a snapshot stopped by its context says so; a stat that
// failed otherwise says what the stat answered without its path. Every
// answer names no path.
func TestTheConsoleSaysWhatFailedByItsClass(t *testing.T) {
	const private = "/Private Data"
	denied := &fs.PathError{Op: "open", Path: private + "/tokens.json", Err: fs.ErrPermission}
	for _, tc := range []struct {
		name string
		got  string
		want string
	}{
		{"a busy read, an access denial too", snapshotRefusal(fmt.Errorf("copy tokens.json: %w",
			fmt.Errorf("%w: %w", atomicwrite.ErrReadBusy, denied))), errMsgSnapshotBusy},
		{"a snapshot its context cancelled", snapshotRefusal(fmt.Errorf("vacuum manifest db: %w", context.Canceled)), errMsgSnapshotStopped},
		{"a snapshot past its deadline", snapshotRefusal(context.DeadlineExceeded), errMsgSnapshotStopped},
		{"a snapshot refused a permission", snapshotRefusal(fmt.Errorf("copy tokens.json: %w", denied)), errMsgSnapshotNotPermitted},
		{"a snapshot that failed otherwise", snapshotRefusal(errors.New("backup: player floor is missing; run bridge reset-player")), errMsgSnapshotFailed},
		{"a listing refused a permission", backupsListRefusal(denied), errMsgBackupsNotPermitted},
		{"a listing that failed otherwise", backupsListRefusal(&fs.PathError{Op: "readdirent", Path: private, Err: errors.New("input/output error")}), errMsgBackupsNotListed},
		{"a root that is not there", rootStatRefusal(&fs.PathError{Op: "stat", Path: private, Err: fs.ErrNotExist}), errMsgRootNotFound},
		{"a root refused a permission", rootStatRefusal(&fs.PathError{Op: "stat", Path: private, Err: fs.ErrPermission}), errMsgRootNotPermitted},
		{"a root whose stat failed otherwise", rootStatRefusal(&fs.PathError{Op: "stat", Path: private + "/Music", Err: errors.New("too many levels of symbolic links")}),
			"the bridge could not read that path (stat: too many levels of symbolic links): check it, and type the folder's absolute path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("answered %q, want %q", tc.got, tc.want)
			}
			if strings.Contains(tc.got, private) {
				t.Errorf("the answer names the path: %q", tc.got)
			}
		})
	}
}
