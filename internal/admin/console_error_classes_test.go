package admin

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// TestTheConsoleSaysWhatFailedByItsClass drives the classes no fixture
// reaches on every platform: a read held busy past its retries, whose last
// error on Windows is an access denial and so a permission's too, says busy
// (asked first); a snapshot stopped by its context says so; a stat that
// failed otherwise says what the stat answered without its path; and each
// permission the console names, which the fixtures take away with a mode
// that Windows does not refuse by and root is not refused by
// (skipWhereModesDoNotRefuse). Every answer names no path.
func TestTheConsoleSaysWhatFailedByItsClass(t *testing.T) {
	const private = "/Private Data"
	denied := &fs.PathError{Op: "open", Path: private + "/tokens.json", Err: fs.ErrPermission}
	loggingtest.Record(t)
	const notRevoked = "the device token was not revoked and still works"
	tokenStoreFault := func(err error) string {
		rec := httptest.NewRecorder()
		writeTokenStoreFault(rec, "revoke", notRevoked, err)
		var body saveRefusalBody
		decodeBody(t, rec, &body)
		return body.Message
	}
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
		{"a certificate refused a permission", certInspectRefusal(&fs.PathError{Op: "open", Path: private + "/tls/server.crt", Err: fs.ErrPermission}),
			errMsgCertificateNotPermitted},
		{"a certificate that failed otherwise", certInspectRefusal(errors.New("tls: no CERTIFICATE block")), errMsgCertificateNotRead},
		{"a token write refused a permission", tokenStoreFault(fmt.Errorf("auth: persist: %w", denied)), notRevoked + ": " + errMsgTokensNotPermitted},
		{"a token write that failed otherwise", tokenStoreFault(errors.New("auth: persist: no space left on device")), notRevoked + ": " + errMsgTokensNotSaved},
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
