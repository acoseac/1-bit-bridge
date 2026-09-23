package updater

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// renameFunc is the rename seam, shared by both platforms' swaps and by
// the staging step below. Overridden in tests to force the cross-device
// branch; production is os.Rename.
//
// Declared here rather than in swap_unix.go so Windows has it too —
// stageIntoDir is shared, and Windows reaches the cross-volume copy on
// the very host its own docblock says every update used to fail on.
var renameFunc = os.Rename

// stageIntoDir places src as a temp file in dst's OWN directory and
// returns that path. It does NOT touch dst.
//
// **This is the step the swap's no-file window used to enclose.** Both
// fallback paths — the POSIX two-rename swap and Windows, where the
// hardlink trick cannot apply at all and the two-rename swap is the ONLY
// path — vacated dst to bak FIRST and then called placeNewBinary, which
// falls back to a cross-volume copy plus an fsync on EXDEV /
// ERROR_NOT_SAME_DEVICE. Both files described that gap as "the tiny
// no-file window between the two renames"; with the copy inside it, it
// was the duration of a ~30 MiB transfer, not two syscalls. A power loss
// there leaves no executable and no process to run the in-process
// restore, and boot-time rollback cannot help — the missing file IS the
// bridge.
//
// placeNewBinaryWindows' own docblock names the host where the copy
// branch fires: "bridge.exe on D: and the data dir under %LOCALAPPDATA%
// on C: — a small-SSD media PC — EVERY update failed". On that host every
// update spent multiple seconds with no bridge.exe on disk.
//
// Staging first makes the commit two ADJACENT renames on one volume,
// which is what those comments always claimed. It reorders the staging,
// never the commit: the vacate→install ordering and the rollback-on-
// failure behaviour are unchanged.
//
// The cheap path is tried first, so a same-volume install still pays a
// rename rather than a copy — Windows takes this branch on the ordinary
// single-volume host, and making it copy 30 MiB unconditionally would be
// a real regression bought for nothing.
//
// `renameFunc` for the move (it may genuinely be cross-device, and the
// test seam forces exactly that) but the caller's commit uses a plain
// os.Rename, because a rename WITHIN one directory cannot be. Same
// discipline the previous copy helper documented.
//
// mode 0 skips the chmod — NTFS has no executable bit to set.
func stageIntoDir(src, dst string, mode os.FileMode) (staged string, err error) {
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".bridge-swap-*")
	if err != nil {
		return "", fmt.Errorf("create staging file beside %s: %w", dst, err)
	}
	tmpName := tmp.Name()
	// Close before the rename below replaces this file: Windows refuses
	// to touch a path that still has an open handle.
	//
	// Deliberately NOT the two-defer idiom (defer Remove, then defer
	// Close) that the rest of the tree uses and that was proposed here
	// (Gemini on #978). That idiom exists to order a deferred Close
	// BEFORE a deferred Remove; this closes the handle explicitly, with
	// its error checked, before the cleanup defer is even registered —
	// strictly stronger than the ordering the rule is about. The only
	// window it leaves is the two statements between CreateTemp and
	// Close, neither of which can panic.
	if cerr := tmp.Close(); cerr != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("close staging file: %w", cerr)
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()

	if rerr := renameFunc(src, tmpName); rerr != nil {
		if !isCrossDeviceErr(rerr) {
			// Narrow on purpose. A rename refused for any other reason
			// — permissions, a vanished source — is a fact worth
			// surfacing, and masking it behind a copy that then fails
			// for its own reason would report the wrong cause.
			return "", fmt.Errorf("stage %s beside %s: %w", src, dst, rerr)
		}
		if cerr := copyFileTo(src, tmpName); cerr != nil {
			return "", cerr
		}
		// The rename path consumes src by moving it; the copy path has
		// to do it explicitly, so both leave the scratch dir clean.
		_ = os.Remove(src)
	}

	if mode != 0 {
		// os.CreateTemp makes the file 0o600, and a renamed-in source
		// carries whatever mode the extractor's umask-masked O_CREATE
		// left. Setting it explicitly here is what stops the two swap
		// paths installing different permissions — the rename path used
		// to inherit a umask-masked mode while the copy path chmod'd an
		// unmasked 0o755, under a comment claiming they matched.
		if cerr := os.Chmod(tmpName, mode); cerr != nil {
			return "", fmt.Errorf("chmod staged binary: %w", cerr)
		}
	}
	committed = true
	return tmpName, nil
}

// copyFileTo copies src over the already-created file at tmpName and
// fsyncs it, so the bytes are durable before anything is renamed.
func copyFileTo(src, tmpName string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source binary: %w", err)
	}
	defer in.Close()

	out, err := os.OpenFile(tmpName, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open staging file: %w", err)
	}
	// Deferred as well as closed explicitly below: a panic during the
	// copy or the sync would otherwise leak the descriptor, and on
	// Windows an open handle blocks the caller's cleanup from unlinking
	// the staging file (Gemini on #978). The explicit Close on the
	// success path stays, because its error is the one that means the
	// bytes may not be durable; the deferred one is the panic net and
	// its second call is a harmless no-op.
	defer func() { _ = out.Close() }()
	if _, cerr := io.Copy(out, in); cerr != nil {
		return fmt.Errorf("copy binary across devices: %w", cerr)
	}
	if serr := out.Sync(); serr != nil {
		return fmt.Errorf("sync staged binary: %w", serr)
	}
	if cerr := out.Close(); cerr != nil {
		return fmt.Errorf("close staged binary: %w", cerr)
	}
	// Explicitly, not deferred: os.Remove(src) runs next and Windows
	// refuses to unlink a file that still has an open handle. The defer
	// above stays as the error-path fallback.
	_ = in.Close()
	return nil
}
