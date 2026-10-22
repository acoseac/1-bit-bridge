// Package atomicwrite provides the tmp-file-then-rename helper the
// scanner and enricher both use to commit cached JPEGs to disk
// without a concurrent reader ever observing a torn file.
//
// Before this package the same pattern lived twice — once in
// `internal/enrich/enricher.go::writeArtworkAtomic`, once in
// `internal/manifest/extractors.go::writeArtworkAtomicScan` — with
// the only meaningful difference being the tmp-file prefix
// (`.caa-*.jpg.tmp` vs `.scan-*.jpg.tmp`), which lets operators tell
// from a stale `.tmp` left on disk WHICH side was the writer.
// Consolidating preserves that diagnostic shape via the `TmpPrefix`
// option.
//
// The streaming-write variant
// (`internal/enrich/enricher.go::writeArtworkAtomicStream`) is NOT
// covered here — that path carries additional invariants (size cap
// via `io.LimitReader`, zero-byte-refusal, MIME-detection hand-off
// to the JPEG sniff) that the buffered shape doesn't need. A
// future refactor could absorb it; for now the buffered helper is
// what's load-bearing-duplicated.
//
// # Atomic AND durable
//
// Every rename path in this package flushes the destination's parent
// directory afterwards (`commitDirEntry`, a no-op on Windows). Without
// it the writes were atomic with respect to concurrent readers — the
// package's original goal, and it held — but not crash-durable: the
// file's bytes were fsynced while the directory ENTRY that publishes
// them could still be sitting unflushed in the filesystem journal, so
// a power loss right after the rename could resurrect the previous
// contents.
//
// The barrier lives here, at the single rename chokepoint, precisely
// so that every persist site in the tree — `config.Save`,
// `auth.persist`, the updater state file, the adminauth store, backup
// snapshots, transcode / analyze sidecars, the TLS cert pair, the
// artwork caches — inherits it with no call-site change and no way to
// forget it. It is unconditional: there is no opt-out knob, because
// every current caller is a "commit this file" operation where
// durability is the whole point, and the added cost is one
// metadata-only fsync against writes that already fsync their full
// contents.
//
// What a failed barrier means is the caller's choice. `WriteBytes` and
// the renames tolerate it (`commitDirEntry`): the write has landed and
// is visible. `WriteBytesDurable` fails on it, for a caller that hands
// out nothing a file covers until the file is durable (the player's ID
// and token reservation, B470 PR 4b1): a rename whose directory entry
// is not durable can roll back at a crash, and what was handed out from
// it would be handed out again. It syncs every entry between the file
// and a root the caller vouches for, on every write, and keeps nothing
// between writes. Where there is no barrier at all (Windows),
// `WriteBytesDurable` refuses with `ErrNoDirectoryBarrier` and the others
// write as before.
package atomicwrite

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/logging"
)

var logger = logging.Component("atomicwrite")

// ErrNoDirectoryBarrier is WriteBytesDurable's refusal, and ProbeDurable's
// answer, on a platform with no directory barrier (Windows, where
// fsutil.SyncParentDir is a no-op): a rename there is atomic, and nothing
// makes the directory entry naming it durable, so no write can be counted
// durable. It wraps errors.ErrUnsupported.
var ErrNoDirectoryBarrier = fmt.Errorf("atomicwrite: this platform cannot fsync a directory, so no write here is made durable: %w", errors.ErrUnsupported)

// ErrBarrierFailed marks, in what WriteBytesDurable and ProbeDurable
// return, a directory fsync that failed; nothing else they return wraps it.
// Each opens a directory once and fsyncs that descriptor (syncDirAt), so the
// mark means the filesystem would not sync the directory, as some network
// and FUSE mounts answer every directory fsync (EINVAL, ENOTSUP), and never
// that the directory could not be opened (one this process cannot read, or
// no descriptor left): a caller that advises moving to another filesystem
// asks errors.Is first.
var ErrBarrierFailed = errors.New("atomicwrite: the directory barrier failed")

// ErrNotUnderRoot is WriteBytesDurable's and ProbeDurable's refusal of a path
// that does not lie below the durable root they are given, as written or as
// its symbolic links resolve: the root is what the caller vouches for, and
// it vouches for nothing outside it. Nothing is created, written or synced.
var ErrNotUnderRoot = errors.New("atomicwrite: the path is not below the durable root")

// renameFunc is the rename implementation called by RenameWithRetry.
// Wrapped in a var so tests can inject a deterministic failure
// without waiting out the full retry backoff budget. Production
// code MUST NOT mutate this directly — use `SetRenameFuncForTest`
// from a `_test.go` so the documented contract that this is a
// test-only seam stays grep-able.
var renameFunc = os.Rename

// SetRenameFuncForTest swaps the rename implementation. Tests
// `defer atomicwrite.SetRenameFuncForTest(os.Rename)` to restore
// the production behaviour. Returns the previous value so a test
// can chain a restore via the standard pattern:
//
//	prev := atomicwrite.SetRenameFuncForTest(failOnce)
//	defer atomicwrite.SetRenameFuncForTest(prev)
//
// Public only because tests in `internal/enrich` and
// `internal/manifest` both need access; the variable itself stays
// unexported so accidental package-level mutation is impossible.
func SetRenameFuncForTest(fn func(src, dst string) error) func(src, dst string) error {
	prev := renameFunc
	renameFunc = fn
	return prev
}

// syncParentDir is the parent-directory durability barrier run after
// a successful rename by WriteBytes and the renames (commitDirEntry).
// Wrapped in a var purely as a test seam — the real syscall can't be
// made to fail portably, and a dir-sync failure is exactly the branch
// that MUST NOT turn a completed write into a returned error. The strict
// writer and its probe do not use it: they open each directory once and
// fsync that descriptor (syncDir, syncDirAt). Production code MUST NOT
// mutate this; in-package tests override it (with a `t.Cleanup`
// restore), same convention as `renameFunc`.
//
// **A test that overrides this MUST NOT call `t.Parallel()`.** The seam
// is process-global, so two parallel tests installing different stubs
// clobber each other's expectations regardless of any synchronisation —
// a mutex around the assignment would silence the race DETECTOR while
// leaving the logical interference in place, which is worse than no
// guard at all because it hides the bug. Serial tests with a
// `t.Cleanup` restore are the contract, matching every other seam in
// the tree (`renameFunc` here, `commandContext` in internal/tailscale,
// `soxLookPath` / `soxProbeCommand` in internal/transcode,
// `afterExtractHookForTests` in internal/manifest, `Pool.runner`).
//
// It is nil on a platform that has no barrier, which is also the platform
// seam a test sets: on Windows `fsutil.SyncParentDir` is a compile-time
// no-op (`fsutil.SyncsParentDir` is false; see its docblock for why
// FlushFileBuffers can't service a directory handle), and a no-op that
// answers nil is not one a caller can tell from a barrier that ran.
// commitDirEntry then has nothing to run (and syncDir is nil there too, so
// WriteBytesDurable refuses).
var syncParentDir = platformBarrier()

// syncFile is the file fsync each write runs on what it stages, and the
// strict writer on a destination another writer filled with the same bytes.
// A test seam, process-global like syncParentDir, with the same contract:
// in-package tests override it with a `t.Cleanup` restore and never call
// `t.Parallel()`.
var syncFile = (*os.File).Sync

// syncDir is the strict writer's and the probe's directory barrier: the
// fsync of a directory they opened themselves, so that its failure, and
// only its failure, is ErrBarrierFailed. It is (*os.File).Sync where the
// platform has a directory barrier, and nil where it has none (Windows,
// where fsutil.SyncsParentDir is false), and then WriteBytesDurable and
// ProbeDurable refuse with ErrNoDirectoryBarrier. A test seam, process-global
// like syncParentDir, with the same contract: in-package tests override it
// with a `t.Cleanup` restore and never call `t.Parallel()`, and tests in
// other packages go through SetSyncDirForTest.
var syncDir = platformSyncDir()

// platformSyncDir is (*os.File).Sync where the platform has a directory
// barrier, and nil where it has none.
func platformSyncDir() func(d *os.File) error {
	if !fsutil.SyncsParentDir {
		return nil
	}
	return (*os.File).Sync
}

// SetSyncDirForTest swaps the strict writer's and the probe's directory
// barrier for a test in another package: fn is handed the name of the
// directory the barrier is to fsync, which it has opened, and fn's answer is
// the fsync's; nil is a platform with no barrier. It returns what puts the
// barrier before it back. The seam is process-global: a test that uses it
// MUST NOT call `t.Parallel()`, and restores it in a `t.Cleanup`. Public only
// because the player engine's tests drive its reservation and its probe
// through the real WriteBytesDurable and ProbeDurable, with a barrier that
// fails and, on Windows too, with one that answers.
func SetSyncDirForTest(fn func(dir string) error) (restore func()) {
	prev := syncDir
	syncDir = nil
	if fn != nil {
		syncDir = func(d *os.File) error { return fn(d.Name()) }
	}
	return func() { syncDir = prev }
}

// beforeMkdirAll is a test seam, nil in production: a strict write calls it
// once it has checked the path against its root and before it creates the
// directories it needs, so a test can act there as another writer would.
// In-package tests set it with a `t.Cleanup` restore and never call
// `t.Parallel()`.
var beforeMkdirAll func(dir string)

// platformBarrier is fsutil.SyncParentDir where it is a barrier, and nil
// where it is a no-op.
func platformBarrier() func(path string) error {
	if !fsutil.SyncsParentDir {
		return nil
	}
	return fsutil.SyncParentDir
}

// dirSyncWarnOnce keeps the "this filesystem can't fsync a directory"
// signal to exactly one log line per process. The condition is a
// permanent property of the mount (some network / FUSE filesystems
// answer EINVAL / ENOTSUP on a directory fsync), so a per-write line
// would be pure spam on the scanner's artwork-commit path — but total
// silence would hide a real durability downgrade from the operator.
var dirSyncWarnOnce sync.Once

// commitDirEntry flushes the parent directory entry created by a
// just-completed rename, upgrading the write from *atomic* to
// *crash-durable*.
//
// Atomic and durable are different guarantees, and the package
// previously only delivered the first. `tmp.Sync()` before the rename
// makes the file's BYTES durable, and rename(2) makes the swap atomic
// with respect to concurrent readers — but the directory ENTRY that
// publishes those bytes at the destination name lives in the parent
// directory's own metadata. On ext4 / XFS / APFS that entry can sit in
// the journal, unflushed, after rename returns. A power loss in that
// window leaves the bytes on disk but reachable only under the old
// (tmp) name — i.e. the config / token store / cert reverts to its
// previous contents, or the destination is missing entirely.
//
// **A failure here never fails the write.** By the time this runs the
// rename has already succeeded: the new content is live and visible to
// every reader, and the only thing lost is the extra durability
// hardening. Reporting it as an error would make callers take
// remediation paths (retry, abort a scan, surface a failure to the
// operator) for an operation that actually completed — strictly worse
// than the pre-existing behaviour, which had no barrier at all. So the
// error is swallowed, and surfaced once per process via `logger.Warn`.
// WriteBytesDurable alone does not come here: its caller asked for the
// failure (durableDirEntry).
func commitDirEntry(path string) {
	if syncParentDir == nil {
		return
	}
	if err := syncParentDir(path); err != nil {
		dirSyncWarnOnce.Do(func() {
			logger.Warn("parent directory fsync failed; atomic writes on this filesystem are not crash-durable (logged once per process)",
				"path", path, "err", err)
		})
	}
}

// renameBackoff is the per-attempt sleep schedule for
// `RenameWithRetry`. Five attempts; total wall-clock budget 750 ms.
//
// On POSIX the first attempt always succeeds; the loop is a no-op
// on Unix. On Windows, Defender and Search Indexer hold transient
// handles on freshly-written files via their scan-on-close hook —
// 750 ms covers their typical scan latency comfortably. A
// non-transient permission error on the parent directory burns the
// full budget before failing, which is acceptable on a per-album-
// once code path.
var renameBackoff = []time.Duration{
	0,
	50 * time.Millisecond,
	100 * time.Millisecond,
	200 * time.Millisecond,
	400 * time.Millisecond,
}

// RenameRetryBudget is the longest RenameWithRetry sleeps between its
// attempts: the time a writer can hold a staged file after deciding to
// rename it, before the rename lands. adminauth waits this long (and a
// margin) before confirming a write another process might have renamed
// over.
func RenameRetryBudget() time.Duration {
	var total time.Duration
	for _, d := range renameBackoff {
		total += d
	}
	return total
}

// RenameWithRetry retries `os.Rename` to absorb the transient
// "Access is denied" / sharing-violation Windows produces under
// the tmp-file-then-rename pattern. Concurrent scanner workers
// writing the same content-hash also race here.
//
// On success it also fsyncs the destination's parent directory (a
// no-op on Windows) so the write is crash-durable, not merely atomic
// — see `commitDirEntry`. Every caller in the tree inherits that for
// free; a barrier failure is tolerated and never fails the rename.
//
// Caller is responsible for post-failure semantics — typically the
// "stat the destination and accept if its bytes match what we
// tried to write" fallback that `WriteBytes` implements.
//
// This is the context-free form for one-shot writers (config /
// token-store / update-state persists) that aren't driven by a
// cancellable worker. A caller running inside a cancellable job (the
// transcode / analyze pools) should use RenameWithRetryCtx so a
// shutting-down worker frees its slot without waiting out the backoff.
func RenameWithRetry(src, dst string) error {
	return RenameWithRetryCtx(context.Background(), src, dst)
}

// RenameWithRetryCtx is RenameWithRetry with a cancellation-aware
// backoff: on ctx cancellation between attempts it returns ctx.Err()
// immediately instead of sleeping out the remaining schedule. The
// rename syscall itself isn't interruptible, but the inter-attempt
// sleeps (up to ~750ms total on Windows) are — enough to let a
// shutting-down transcode/analyze worker release its slot promptly
// rather than block the pool's shutdown grace window. The first
// attempt (no sleep) always runs; only the backoff sleeps observe ctx.
//
// The post-rename parent-directory barrier is NOT ctx-gated: fsync
// isn't interruptible, it runs only after the rename already
// committed, and skipping it on a cancelled ctx would silently
// downgrade durability exactly during shutdown — when a crash window
// is most likely.
func RenameWithRetryCtx(ctx context.Context, src, dst string) error {
	if err := renameRetry(ctx, src, dst); err != nil {
		return err
	}
	// Rename committed — flush the directory entry so a crash
	// immediately after can't lose it. Best-effort: see commitDirEntry
	// for why a failure here must not fail an already-successful write.
	commitDirEntry(dst)
	return nil
}

// renameRetry is the rename and its retries, without the barrier: each
// caller runs that in its own mode (commitDirEntry, or durableDirEntry
// for WriteBytesDurable).
func renameRetry(ctx context.Context, src, dst string) error {
	var err error
	for _, d := range renameBackoff {
		if d > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d):
			}
		}
		if err = renameFunc(src, dst); err == nil {
			return nil
		}
	}
	return err
}

// durableDirEntry is the barrier WriteBytesDurable requires: unlike
// commitDirEntry's, its failure is the write's, because the caller hands
// out nothing the file covers until the directory entry naming it is
// durable. A platform with no barrier is ErrNoDirectoryBarrier, never a
// barrier that ran.
func durableDirEntry(path string) error {
	if err := syncDirAt(filepath.Dir(path)); err != nil {
		return fmt.Errorf("the write landed, but its directory entry is not durable: %w", err)
	}
	return nil
}

// underRoot makes root and path absolute and clean, and checks what a
// strict write and its probe both require before they touch anything:
// path's directory at or below root as written (ErrNotUnderRoot), and root
// a directory that exists, which neither creates. It returns root and
// path's directory. An empty root is refused rather than taken for the
// working directory, which is what filepath.Abs makes of it.
func underRoot(root, path string) (string, string, error) {
	if root == "" {
		return "", "", fmt.Errorf("%w: no root was given", ErrNotUnderRoot)
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", "", err
	}
	if path, err = filepath.Abs(path); err != nil {
		return "", "", err
	}
	dir := filepath.Dir(path)
	if !within(root, dir) {
		return "", "", fmt.Errorf("%w: %s is not below %s", ErrNotUnderRoot, path, root)
	}
	switch info, err := os.Stat(root); {
	case err != nil:
		return "", "", err
	case !info.IsDir():
		return "", "", &fs.PathError{Op: "root", Path: root, Err: syscall.ENOTDIR}
	}
	return root, dir, nil
}

// within reports whether p is root or lies below it; both are absolute and
// clean.
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// nearest is the directory at or below root that holds dir, or holds the
// first of the directories a write of dir creates: dir itself when it
// exists, and otherwise the nearest above it that does, root at the
// highest. It must be a directory (syscall.ENOTDIR), and lie at or below
// root as its symbolic links resolve too (ErrNotUnderRoot), so nothing is
// created through a link that leads out of root. Any error Stat gives but
// fs.ErrNotExist is returned as it is.
func nearest(root, dir string) (string, error) {
	for d := dir; ; d = filepath.Dir(d) {
		info, err := os.Stat(d)
		switch {
		case err == nil && !info.IsDir():
			return "", &fs.PathError{Op: "probe", Path: d, Err: syscall.ENOTDIR}
		case err == nil:
			real, err := filepath.EvalSymlinks(d)
			if err != nil {
				return "", err
			}
			realRoot, err := filepath.EvalSymlinks(root)
			if err != nil {
				return "", err
			}
			if !within(realRoot, real) {
				return "", fmt.Errorf("%w: %s resolves to %s, outside %s", ErrNotUnderRoot, d, real, realRoot)
			}
			return d, nil
		case !errors.Is(err, fs.ErrNotExist) || d == root:
			return "", err
		}
	}
}

// durableChain is the directories whose fsync makes every entry between dir
// and root durable: dir, which holds the file, then each directory above it
// up to root, each of which holds the entry of the one below; root is among
// them, its own entry never. It walks the path as written, which makes the
// entry of every symbolic link it passes through durable, and then as it
// resolves, which reaches a directory a link points to in its own parent.
// Each directory is taken once, by where it resolves to, the deepest first.
// dir exists and lies at or below root as written (underRoot); it must lie
// below root as it resolves too (ErrNotUnderRoot).
func durableChain(root, dir string) ([]string, error) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	if !within(realRoot, realDir) {
		return nil, fmt.Errorf("%w: %s resolves to %s, outside %s", ErrNotUnderRoot, dir, realDir, realRoot)
	}
	var chain []string
	seen := map[string]bool{}
	for _, walk := range [][2]string{{dir, root}, {realDir, realRoot}} {
		for d := walk[0]; ; d = filepath.Dir(d) {
			real, err := filepath.EvalSymlinks(d)
			if err != nil {
				return nil, err
			}
			if !seen[real] {
				seen[real] = true
				chain = append(chain, d)
			}
			if d == walk[1] {
				break
			}
		}
	}
	return chain, nil
}

// ProbeDurable reports, without writing anything, whether WriteBytesDurable
// can make a write of path below root durable: ErrNoDirectoryBarrier on a
// platform with no directory barrier, the refusals WriteBytesDurable makes
// before it writes (ErrNotUnderRoot, a root that does not exist, a regular
// file where a directory should be), and otherwise the strict barrier's
// answer for every directory a write would fsync that exists now: path's
// directory, or while that does not exist yet the nearest one above it that
// does, on whose filesystem it will be created, and each above that up to
// root. Some network and FUSE mounts fail every directory fsync (EINVAL,
// ENOTSUP), and there every WriteBytesDurable fails; a caller that would then
// refuse its work for ever, with nothing naming the cause, asks once as it
// starts and refuses to start instead.
//
// What it probes must be a directory: a barrier opens and fsyncs whatever is
// there, and answers nil for a regular file, where no write can ever be
// made. A regular file is an error wrapping syscall.ENOTDIR, and any other
// error Stat gives is returned as it is. Each directory is opened once, and
// that descriptor fsynced (syncDirAt): one this process cannot open
// (searchable and not readable, or no descriptor left) is the open's error,
// unmarked, and only an fsync's own failure, a directory the filesystem
// would not sync, is marked ErrBarrierFailed. Nothing opens a directory a
// second time, so no failure of a second open can pass for the fsync's.
func ProbeDurable(root, path string) error {
	if syncDir == nil {
		return ErrNoDirectoryBarrier
	}
	root, dir, err := underRoot(root, path)
	if err != nil {
		return err
	}
	near, err := nearest(root, dir)
	if err != nil {
		return err
	}
	chain, err := durableChain(root, near)
	if err != nil {
		return err
	}
	for _, d := range chain {
		if err := syncDirAt(d); err != nil {
			if errors.Is(err, ErrBarrierFailed) {
				return fmt.Errorf("the directory %s cannot be synced: %w", d, err)
			}
			return err
		}
	}
	return nil
}

// sameAndSynced reports whether path holds exactly data and, when it does,
// fsyncs it through the open it read the bytes from: bytes another writer
// renamed into place are visible once its rename lands, and durable only
// once they are fsynced. A destination it cannot open or read is not the
// same, and the caller's own error stands.
func sameAndSynced(path string, data []byte) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, nil
	}
	defer func() { _ = f.Close() }()
	existing, err := io.ReadAll(io.LimitReader(f, int64(len(data))+1))
	if err != nil || !bytes.Equal(existing, data) {
		return false, nil
	}
	if err := syncFile(f); err != nil {
		return true, fmt.Errorf("the destination holds the same bytes, but they are not durable: %w", err)
	}
	return true, nil
}

// syncDirAt is the strict barrier on dir: it opens dir and fsyncs that
// descriptor, which makes the entries dir holds durable. Only the fsync's
// own failure is marked ErrBarrierFailed. An open that fails (a directory
// this process cannot read, no descriptor left) is returned as it is, since
// no fsync ran, and so is a platform with no barrier
// (ErrNoDirectoryBarrier). The close of a descriptor opened to read is not
// checked: the fsync has answered by then.
func syncDirAt(dir string) error {
	if syncDir == nil {
		return ErrNoDirectoryBarrier
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	if err := syncDir(d); err != nil {
		return &barrierError{err: fmt.Errorf("sync dir %q: %w", dir, err)}
	}
	return nil
}

// barrierError is a directory fsync's failure: it reads as the fsync's
// error, which it wraps, and errors.Is matches it to ErrBarrierFailed.
type barrierError struct{ err error }

func (e *barrierError) Error() string        { return e.err.Error() }
func (e *barrierError) Unwrap() error        { return e.err }
func (e *barrierError) Is(target error) bool { return target == ErrBarrierFailed }

// WriteBytes writes `data` to `path` via tmp-file + rename so a
// concurrent reader never sees a torn file. The tmp file is
// created in the SAME directory as `path` so the rename is a
// same-filesystem atomic operation (cross-FS renames degrade to
// copy-and-delete, which is racy AND breaks Windows' atomic
// rename guarantee).
//
// `tmpPrefix` is the `os.CreateTemp` prefix used to name the
// scratch file. The caller picks a prefix that identifies the
// caller-side writer (`.caa-*.jpg.tmp` for enricher,
// `.scan-*.jpg.tmp` for scanner) so a stale tmp on disk after a
// crash tells operators which subsystem failed.
//
// Cache directory perms are 0o700 (owner-only) — application-
// owned caches shouldn't be world-readable on POSIX. The
// MkdirAll call is idempotent; existing dirs with looser perms
// from prior deployments stay at their previous mode (operators
// upgrade by `rmdir`-ing the cache).
//
// Run as root (a `sudo bridge scan` over a service install writes the
// scanner's covers here), the directories it creates and the file it
// writes keep the install's owner: fsutil.MkdirAll gives a new directory
// its parent's owner, and fsutil.KeepOwner the staged file the owner of
// what it replaces, or of its directory. Without them the artwork cache
// was root's 0700 and the service could neither read a cover nor add one.
//
// On rename failure the function reads the existing destination
// and accepts the write as already-committed if the bytes match
// `data`. Same rationale the original `writeArtworkAtomic` /
// `writeArtworkAtomicScan` carried: a concurrent writer of the
// SAME bytes (mbid-keyed cache, content-hash-keyed scanner path)
// is functionally indistinguishable from our completed write, and
// failing the operation just to surface the race would force the
// caller into a retry loop that performs the same comparison.
//
// Per-error specifics are surfaced unchanged to the caller (we
// return the original `os.Rename` error if byte-equality also
// fails) so the caller can route on `errors.Is` / `os.IsNotExist`
// as appropriate.
func WriteBytes(path string, data []byte, tmpPrefix string) error {
	if err := fsutil.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeBytes(path, data, tmpPrefix, false)
}

// WriteBytesDurable is WriteBytes for a caller that hands out nothing the
// file covers until the write is durable: it fails unless the file's bytes,
// and every directory entry between the file and root, were fsynced.
// WriteBytes tolerates a failed directory fsync once its rename has landed
// (commitDirEntry); here that failure is the write's, because a rename
// whose entry is not durable can roll back at a crash, and so can a file
// whose directory's own entry is not. A failure may leave the new bytes in
// place, so the caller counts the write as not made, never as undone. A
// rename that fails while the destination already holds the same bytes is
// taken as WriteBytes takes it, once the destination itself and then its
// directory are synced.
//
// Every write syncs every entry from the file up to root, the directories
// it creates for path included, the deepest first and before the file is
// written, and the file's own entry once its rename lands (durableChain):
// root is fsynced, for the entry of the directory below it, and its own
// entry never. The writer keeps nothing between writes. A directory whose
// entry an earlier write, or an earlier run, could not sync is synced again
// by the next write that passes through it, whoever made it and whatever
// happened since: process-local state would not survive the restart that
// leaves such a directory behind. Nothing is ever removed to make a later
// write create a directory again: a removal races every other writer there.
// The path is walked as written and as its symbolic links resolve, so a
// directory reached through a link is synced in its own parent, and the
// link's own entry in the link's parent.
//
// root is the caller's guarantee: it must exist (the writer never creates
// it), and its own entry, and those above it, must be durable already, or
// need not be, which only the caller can say (the player's engine passes
// its data directory: engine.Config.DataDir). path must lie below root as
// written and as its links resolve (ErrNotUnderRoot); a root that does not
// exist is Stat's error. Each is refused before anything is created,
// written or synced, and so is a platform with no directory barrier
// (Windows: ErrNoDirectoryBarrier), where no write could be counted
// durable; WriteBytes and the renames keep their best effort there.
func WriteBytesDurable(root, path string, data []byte, tmpPrefix string) error {
	if syncDir == nil {
		return ErrNoDirectoryBarrier
	}
	root, dir, err := underRoot(root, path)
	if err != nil {
		return err
	}
	if _, err := nearest(root, dir); err != nil {
		return err
	}
	if beforeMkdirAll != nil {
		beforeMkdirAll(dir)
	}
	if err := fsutil.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	chain, err := durableChain(root, dir)
	if err != nil {
		return err
	}
	for _, d := range chain[1:] {
		if err := syncDirAt(d); err != nil {
			return fmt.Errorf("the directory entries between %s and %s are not durable: %w", dir, root, err)
		}
	}
	return writeBytes(filepath.Join(dir, filepath.Base(path)), data, tmpPrefix, true)
}

// writeBytes stages data beside path and renames it into place: the file
// half of WriteBytes, or of WriteBytesDurable when durable is set, whose
// barrier then makes the file's own entry durable. path's directory exists.
func writeBytes(path string, data []byte, tmpPrefix string, durable bool) error {
	dir := filepath.Dir(path)
	// Same directory as `path` so the rename is a same-filesystem atomic op.
	tmp, err := os.CreateTemp(dir, tmpPrefix)
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	// Panic-safety FD close (LIFO order — runs before Remove). See
	// internal/auth/auth.go for the rationale. Windows requires
	// the FD released BEFORE rename, so the success path also
	// closes explicitly below; the defer is the unwind path for
	// the error branches.
	defer func() { _ = tmp.Close() }()
	if err := fsutil.KeepOwner(tmp, path); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := syncFile(tmp); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := renameRetry(context.Background(), tmpName, path); err != nil {
		// Race / AV scan window may have produced a valid
		// destination already. Verify byte-equivalence —
		// size alone isn't proof (mbid-keyed names don't
		// embed a content hash; a future re-fetch with
		// different bytes could match on size). Cost: one
		// read of the destination on the rare-fallback
		// path.
		//
		// Don't clear tmpName here — the rename failed, the
		// tmp file is still on disk; the deferred Remove
		// above must run (otherwise we leak a `.tmp` per
		// race / AV-window hit, accumulating over a long
		// uptime).
		//
		// No parent-dir barrier on this branch for WriteBytes:
		// we didn't publish the directory entry, the concurrent
		// winner did — and its own rename already ran the
		// barrier for it. WriteBytesDurable makes the winner's
		// write durable itself (sameAndSynced), since matching
		// bytes prove it visible, not durable, and the winner's
		// barrier may have been tolerated.
		if durable {
			switch same, syncErr := sameAndSynced(path, data); {
			case syncErr != nil:
				return syncErr
			case same:
				return durableDirEntry(path)
			}
			return err
		}
		if existing, readErr := os.ReadFile(path); readErr == nil && bytes.Equal(existing, data) {
			return nil
		}
		return err
	}
	tmpName = "" // rename succeeded — suppress the deferred os.Remove
	if durable {
		return durableDirEntry(path)
	}
	commitDirEntry(path)
	return nil
}
