//go:build !windows

package transcode

import (
	"os"
	"syscall"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// outputFaultErrnos are the causes, besides a permission, that describe the
// output side's volume rather than the job: hostOutputFault's table.
var outputFaultErrnos = map[syscall.Errno]outputFaultKind{
	syscall.EROFS:    outputReadOnly,
	syscall.ENOSPC:   outputFull,
	syscall.EDQUOT:   outputQuota,
	syscall.EIO:      outputGone,
	syscall.ENOTCONN: outputGone,
	syscall.ESTALE:   outputGone,
}

// createOutput creates the empty file a tool will write at path, before the
// tool runs, so a directory that cannot take a new file fails here, in an
// error whose cause the pool can classify (output_fault.go), and not in the
// tool's own open, whose message is all the pool would see.
//
// ownerOf is whose owner the file takes when this process is root
// (fsutil.KeepOwner's dst): the rendition the sidecar will replace, or for
// the render scratch, the directory it is created in. sox opens its output
// with O_TRUNC, so the file keeps that owner, as it kept the one
// fsutil.Precreate gave it (CLAUDE.md, the B17 bullet). O_EXCL, so nothing
// is written through an entry already at path: the caller has just removed
// any debris there, and the name carries a per-job token.
func createOutput(path, ownerOf string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return err
	}
	if err := fsutil.KeepOwner(f, ownerOf); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	return f.Close()
}
