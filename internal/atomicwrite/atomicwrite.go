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
// between writes; it refuses a path that does not lie plainly below that
// root (a ".." element, or a symbolic link or a mount point below it), so
// that one walk up from the file reaches every entry. Where there is no
// barrier at all (Windows), `WriteBytesDurable` refuses with
// `ErrNoDirectoryBarrier` and the others write as before.
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
	"slices"
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
// asks errors.Is first. The failure it marks is an *fs.PathError naming the
// directory that would not sync, which errors.As finds.
var ErrBarrierFailed = errors.New("atomicwrite: the directory barrier failed")

// ErrNotUnderRoot is WriteBytesDurable's and ProbeDurable's refusal of a path
// that does not lie plainly below the durable root they are given: outside
// it as written, the path or the root spelled with a ".." element, the path
// reached through a symbolic link or a mount point strictly below the root,
// or a file mounted at the path itself. The root is what the caller vouches
// for, and the walk from the file up to it is all the writer syncs: a link's
// target and a mount's source have entries outside that walk. Nothing is
// created, written or synced.
var ErrNotUnderRoot = errors.New("atomicwrite: the path is not plainly below the durable root")

// ErrMountsUnknown is WriteBytesDurable's and ProbeDurable's refusal where
// the writer cannot tell one mount from another: the platform gives no mount
// identity for the root, or gives the root's and not that of a directory or
// the file below it (on Linux the kernel's mount ID comes from statx, Linux
// 5.8, or else /proc/self/fdinfo, Linux 3.15), and so does the fallback after
// a failed rename. The writer says so rather than take two mounts for one: a
// bind mount shows no device of its own.
var ErrMountsUnknown = errors.New("atomicwrite: the mounts between the file and the root cannot be told apart")

// ErrVolatile is WriteBytesDurable's and ProbeDurable's refusal of a root on
// storage that does not keep what is written to it through a reboot: on
// Linux tmpfs, ramfs, an overlay mounted volatile, and an overlay whose upper
// layer is any of those, as Raspberry Pi OS's "Overlay File System" option
// sets one up over its root, told by statfs and, for an overlay, by the upper
// layer the mount table names on the line of the root's own mount ID
// (strictPath.storage). Every barrier answers there, and a reboot takes what
// was written. Nothing is created, written or synced.
var ErrVolatile = errors.New("atomicwrite: the root is on storage that does not keep what is written to it through a reboot")

// ErrMediumUnknown is their refusal of a root on storage of which that cannot
// be told: an overlay whose upper layer the mount table, unreadable, does not
// name, or names by a path that reaches here nothing, or another filesystem
// than the one the overlay writes to. They say so rather than take it for
// storage that outlives a reboot.
var ErrMediumUnknown = errors.New("atomicwrite: whether the root's storage keeps what is written to it through a reboot cannot be told")

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

// SetSyncFileForTest replaces the staged-file barrier for a failure test.
// Like SetSyncDirForTest it is process-global: do not use t.Parallel, and
// restore it only after every writer using it has stopped.
func SetSyncFileForTest(fn func(*os.File) error) (restore func()) {
	prev := syncFile
	syncFile = fn
	return func() { syncFile = prev }
}

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

// mountIDOf is the mount a path lies on, by the identity the platform gives
// it (platformMountID), whatever the path's spelling: on Linux the kernel's
// mount ID, from statx or else /proc/self/fdinfo, a link at the end not
// followed; on macOS the mounted volume's filesystem ID (statfs); and no ID
// (ok false) where the platform gives none (Windows, which never asks:
// mountIDsExpected). A test seam, process-global like syncParentDir, with the
// same contract: in-package tests override it with a `t.Cleanup` restore and
// never call `t.Parallel()`.
var mountIDOf = platformMountID

// mediumOf is the medium check (strictPath.storage), asked of the root and the
// ID of the mount it lies on: platformMedium. A test seam, process-global like
// syncParentDir, with the same contract: in-package tests override it with a
// `t.Cleanup` restore and never call `t.Parallel()`, and tests in other
// packages go through SetMediumForTest.
var mediumOf = platformMedium

// copyUpAncestorsOf is the directories above the root, up to the root of its
// mount, that a copy-up can create entries in (strictPath.storage), asked of
// the root and the ID of the mount it lies on: platformCopyUps, every one on
// an overlay and none elsewhere. A test seam, process-global like
// syncParentDir, with the same contract.
var copyUpAncestorsOf = platformCopyUps

// lstat is the Lstat the strict writer sees each entry through: a test seam,
// so that a test can report a device (st_dev) as some filesystems report it,
// differently for directories and other entries on one mount, on which no
// decision rests (plain). Process-global like syncParentDir, with the same
// contract: in-package tests override it with a `t.Cleanup` restore and never
// call `t.Parallel()`.
var lstat = os.Lstat

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
// failure (durableChain).
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
// caller runs that in its own mode (commitDirEntry, or durableChain for
// WriteBytesDurable).
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

// durableChain is the barrier WriteBytesDurable runs once its write has
// landed, by its rename or by the fallback a failed rename takes for the same
// bytes: every directory in chain, from the file's up to the root, the
// deepest first (strictPath.chain). Unlike commitDirEntry's, its failure is
// the write's, because the caller hands out nothing the file covers until
// every entry naming it is durable. It syncs the whole chain, not the file's
// directory alone, because staging the file is itself a write to the
// directories above it on an overlay: a directory found only in the lower
// layer is copied up, its entry created in its parent's upper directory, and
// so on up to the root, after the syncs that ran before the file was staged,
// and overlayfs fsyncs none of the parents a copy-up creates (Codex on round
// 9). Then it syncs the directories above the root a copy-up can create
// entries in, on an overlay (syncAbove). A platform with no barrier is
// ErrNoDirectoryBarrier, never a barrier that ran.
func durableChain(chain, above []string) error {
	for _, d := range chain {
		if err := syncDirAt(d); err != nil {
			return fmt.Errorf("the write landed, but the directory entries between %s and %s are not durable: %w", chain[0], chain[len(chain)-1], err)
		}
	}
	if err := syncAbove(above); err != nil {
		return fmt.Errorf("the write landed, but %w", err)
	}
	return nil
}

// strictPath is a strict write's path, checked against its root
// (checkPath): root with its links resolved, the directories strictly below
// it down to the file's directory, the shallowest first, and the file, each
// spelled from that resolved root.
type strictPath struct {
	root string
	dirs []string
	file string
}

// dir is the file's directory.
func (p strictPath) dir() string {
	if len(p.dirs) == 0 {
		return p.root
	}
	return p.dirs[len(p.dirs)-1]
}

// chain is the directories whose fsync makes every entry between the file
// and the root durable, while the first n of p.dirs exist: the deepest of
// them, which holds the file, or the first of the directories a write
// creates, then each above it, each holding the entry of the one below, and
// the root last, its own entry never. With no link and no mount below the
// root (plain), each entry is in the directory above it on this one walk.
func (p strictPath) chain(n int) []string {
	chain := make([]string, 0, n+1)
	for i := n - 1; i >= 0; i-- {
		chain = append(chain, p.dirs[i])
	}
	return append(chain, p.root)
}

// checkPath checks what a strict write and its probe both require of root
// and path before they touch anything: no ".." element in either, since
// filepath.Abs removes one lexically while a reader resolves the link before
// it (round 6 wrote root/ids for root/link/../ids, which a reader opens below
// where link points); a path whose last element names a directory
// (NamesDirectory: syscall.EISDIR), which the cleaning would take away;
// path's directory at or below root as written, so a path spelled through a
// link to root, or root through a link to it, is refused rather than walked;
// and root a directory that exists, which neither the writer nor the probe
// creates. The ".." and the "as written" refusals are ErrNotUnderRoot, and an
// empty root is refused rather than taken for the working directory, which
// is what filepath.Abs makes of it. root's links
// are resolved here, once, and every path of the write is spelled from where
// it resolved: a link at or above root is the caller's, as root's own entry
// is.
func checkPath(root, path string) (strictPath, error) {
	switch {
	case root == "":
		return strictPath{}, fmt.Errorf("%w: no root was given", ErrNotUnderRoot)
	case hasDotDot(root) || hasDotDot(path):
		return strictPath{}, fmt.Errorf("%w: %s or %s has a \"..\" element", ErrNotUnderRoot, path, root)
	case NamesDirectory(path):
		return strictPath{}, &fs.PathError{Op: "probe", Path: path, Err: syscall.EISDIR}
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return strictPath{}, err
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return strictPath{}, err
	}
	rel, err := filepath.Rel(absRoot, filepath.Dir(absPath))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return strictPath{}, fmt.Errorf("%w: %s is not below %s as written", ErrNotUnderRoot, path, root)
	}
	realRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return strictPath{}, err
	}
	if info, err := os.Stat(realRoot); err != nil {
		return strictPath{}, err
	} else if !info.IsDir() {
		return strictPath{}, &fs.PathError{Op: "probe", Path: realRoot, Err: syscall.ENOTDIR}
	}
	p := strictPath{root: realRoot}
	if rel != "." {
		d := realRoot
		for _, el := range strings.Split(rel, string(filepath.Separator)) {
			d = filepath.Join(d, el)
			p.dirs = append(p.dirs, d)
		}
	}
	p.file = filepath.Join(p.dir(), filepath.Base(absPath))
	return p, nil
}

// hasDotDot reports whether p has a ".." element.
func hasDotDot(p string) bool {
	return slices.Contains(strings.Split(filepath.ToSlash(p), "/"), "..")
}

// NamesDirectory reports whether path's last element names a directory
// rather than a file: none at all (path empty, or ending in a separator),
// "." or "..". filepath.Abs and filepath.Clean take such an element away
// (root/ids/ cleans to root/ids), so a writer that cleans the path makes a
// file a reader of the path as given cannot open: a regular file answers
// root/ids/ with ENOTDIR (Codex on round 7, where the player engine's
// restart then found its reservation stuck). WriteBytesDurable refuses such
// a path, an *fs.PathError wrapping syscall.EISDIR, and a caller that keeps
// the path to read it again refuses it too (the player engine's New, for its
// queue and reservation files). A root may end in a separator: a root is a
// directory.
func NamesDirectory(path string) bool {
	slash := filepath.ToSlash(path)
	last := slash[strings.LastIndex(slash, "/")+1:]
	return last == "" || last == "." || last == ".."
}

// plain checks each of p's directories that exists, from the root down, and
// then the destination itself when all of them do and it is there: each
// directory a directory (an *fs.PathError wrapping syscall.ENOTDIR) and not a
// symbolic link, and each, the destination included unless it is a link, on
// the root's own mount (ErrNotUnderRoot). A link's target has its entry in the
// target's own parent, and a mount's source in the source's, both outside the
// walk up to the root, so neither is walked, whether it leads out of the root
// or not; and a rename over a file mounted at the destination fails (EBUSY),
// its source's entry never synced. A link at the destination is an entry of
// its directory, which the rename replaces.
//
// A mount is told by the identity the platform gives it (mountIDOf), whatever
// the path's spelling, never by the device (st_dev) a stat reports, which
// some filesystems report differently on one mount: overlayfs with its layers
// on different filesystems and xino off, the default and the Raspberry Pi OS
// "Overlay File System" option, reports the overlay's device for directories
// and a pseudo device per layer for every other entry, and btrfs a device per
// subvolume (the Claude API on round 8: round 8 refused a reservation file
// there once it existed, and the restart's probe with it). Where the platform
// gives no identity for the root, or gives the root's and not that of an entry
// below it, the writer cannot tell one mount from another, and says so
// (ErrMountsUnknown) rather than take them for one (Codex on round 8). The
// root's is asked for every time, a file in the root with nothing below it
// included, so that a probe refuses as its caller starts what every write
// after the first would refuse. Where no identity is expected (Windows, which
// refuses before it gets here: mountIDsExpected), none is compared.
//
// It returns how many directories exist: the first that does not ends the
// walk, since nothing below it exists either.
func (p strictPath) plain() (int, error) {
	rootMount, ids, err := mountIDOf(p.root)
	switch {
	case err != nil:
		return 0, err
	case !ids && mountIDsExpected:
		return 0, fmt.Errorf("%w: the platform gives no mount ID for %s", ErrMountsUnknown, p.root)
	}
	onRootsMount := func(d string) error {
		if !ids {
			return nil
		}
		switch m, ok, err := mountIDOf(d); {
		case err != nil:
			return err
		case !ok:
			return fmt.Errorf("%w: the platform gives %s's mount ID and not %s's", ErrMountsUnknown, p.root, d)
		case m != rootMount:
			return fmt.Errorf("%w: %s is a mount point below %s", ErrNotUnderRoot, d, p.root)
		}
		return nil
	}
	for i, d := range p.dirs {
		info, err := lstat(d)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return i, nil
		case err != nil:
			return 0, err
		case info.Mode()&fs.ModeSymlink != 0:
			return 0, fmt.Errorf("%w: %s is a symbolic link below %s", ErrNotUnderRoot, d, p.root)
		case !info.IsDir():
			return 0, &fs.PathError{Op: "probe", Path: d, Err: syscall.ENOTDIR}
		}
		if err := onRootsMount(d); err != nil {
			return 0, err
		}
	}
	switch info, err := lstat(p.file); {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return 0, err
	case info.Mode()&fs.ModeSymlink == 0:
		if err := onRootsMount(p.file); err != nil {
			return 0, err
		}
	}
	return len(p.dirs), nil
}

// onItsDirectorysMount reports whether path is a regular file on the mount of
// the directory that holds it, which a rename into that directory would have
// replaced: a file mounted there, or a symbolic link, has its entry
// elsewhere, which the strict write's barrier never syncs. The mount is told
// by identity, never by device (plain), and where the platform gives none
// for either, that is ErrMountsUnknown, never a match: Codex on round 8,
// where two unknown identities passed for one and a file mounted once the
// checks had passed gave the fallback its bytes. Where no identity is
// expected (Windows, which never gets here), none is compared.
func onItsDirectorysMount(path string) (bool, error) {
	info, err := lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false, nil
	}
	dir := filepath.Dir(path)
	a, okA, errA := mountIDOf(path)
	b, okB, errB := mountIDOf(dir)
	switch {
	case errA != nil || errB != nil:
		return false, nil
	case !okA || !okB:
		if !mountIDsExpected {
			return true, nil
		}
		return false, fmt.Errorf("%w: the platform gives no mount ID for %s or %s", ErrMountsUnknown, path, dir)
	}
	return a == b, nil
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
// It refuses what every write would: a mount between the file and root, or at
// the file, told by the identity the platform gives each entry, never by its
// device (ErrNotUnderRoot), a root, or an entry below it, whose mount the
// platform gives no identity for (ErrMountsUnknown), and a root on storage
// that does not keep what is written to it through a reboot (ErrVolatile), or
// of which that cannot be told (ErrMediumUnknown), so that its caller learns
// as it starts what the write after the first would refuse.
//
// What it probes must be a directory: a barrier opens and fsyncs whatever is
// there, and answers nil for a regular file, where no write can ever be
// made. A regular file is an error wrapping syscall.ENOTDIR, and any other
// error Lstat gives is returned as it is. Each directory is opened once, and
// that descriptor fsynced (syncDirAt): one this process cannot open
// (searchable and not readable, or no descriptor left) is the open's error,
// unmarked, and only an fsync's own failure, a directory the filesystem
// would not sync, is marked ErrBarrierFailed. Nothing opens a directory a
// second time, so no failure of a second open can pass for the fsync's.
func ProbeDurable(root, path string) error {
	if syncDir == nil {
		return ErrNoDirectoryBarrier
	}
	p, err := checkPath(root, path)
	if err != nil {
		return err
	}
	n, err := p.plain()
	if err != nil {
		return err
	}
	above, err := p.storage()
	if err != nil {
		return err
	}
	for _, d := range p.chain(n) {
		if err := syncDirAt(d); err != nil {
			if errors.Is(err, ErrBarrierFailed) {
				return fmt.Errorf("the directory %s cannot be synced: %w", d, err)
			}
			return err
		}
	}
	return syncAbove(above)
}

// sameAndSynced reports whether path holds exactly data and, when it does,
// fsyncs it through the open it read the bytes from: bytes another writer
// renamed into place are visible once its rename lands, and durable only
// once they are fsynced. A destination it cannot open or read is not the
// same, and the caller's own error stands. Its caller asks first whether
// path is a regular file on its directory's own mount (onItsDirectorysMount):
// a file mounted there, as one mounted after the write's checks would be, or
// a symbolic link, has its entry elsewhere, and the barrier on path's
// directory would vouch for bytes it never named (Codex on round 7: round 7
// took a mounted file's bytes, synced it and the root, and answered nil).
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
// own failure is marked ErrBarrierFailed, and it is an *fs.PathError naming
// dir, as (*os.File).Sync's is: a barrier a test sets that answers another
// error is wrapped in one, so a caller that names the directory that failed
// (the player engine's New) finds it in what is returned whatever the
// barrier. An open that fails (a directory this process cannot read, no
// descriptor left) is returned as it is, since no fsync ran, and so is a
// platform with no barrier (ErrNoDirectoryBarrier). The close of a
// descriptor opened to read is not checked: the fsync has answered by then.
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
		var pe *fs.PathError
		if !errors.As(err, &pe) {
			err = &fs.PathError{Op: "sync", Path: dir, Err: err}
		}
		return &barrierError{err: err}
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
	return writeBytes(path, data, tmpPrefix, nil, nil)
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
// taken as WriteBytes takes it, once the destination itself and then every
// directory from its own up to root are synced.
//
// Every write syncs every entry from the file up to root, the directories
// it creates for path included, the deepest first and before the file is
// written, and then once the write has landed, by its rename or by that
// fallback, every directory from the file's up to root again, the file's own
// entry first (strictPath.chain, durableChain): staging the file on an
// overlay copies a lower-only directory up, creating its entry and those
// above it after the first syncs (Codex on round 9). root is fsynced, for
// the entry of the directory below it, and off an overlay its own entry
// never. On an overlay every directory above root, up to the root of its
// mount, is synced too once the write lands (strictPath.storage): a write
// below a root found in the lower layer alone copies root and those
// directories up, and their entries are the copy-up's, which the caller
// could not have made durable (Codex on round 10). The writer keeps nothing
// between writes. A directory whose
// entry an earlier write, or an earlier run, could not sync is synced again
// by the next write that passes through it, whoever made it and whatever
// happened since: process-local state would not survive the restart that
// leaves such a directory behind. Nothing is ever removed to make a later
// write create a directory again: a removal races every other writer there.
//
// path must lie plainly below root (ErrNotUnderRoot): no ".." element in
// either, its last element a file's (NamesDirectory: EISDIR), path's
// directory at or below root as written, no symbolic link strictly below
// root on the way to it, and no mount there or at the file itself, a bind
// mount of root's own filesystem included, told by the identity the platform
// gives each entry's mount (on Linux the kernel's mount ID, from statx or
// else /proc/self/fdinfo), whatever the path's spelling, and never by the
// device a stat reports, which overlayfs and btrfs report differently on one
// mount. Where no identity can be had the writer says so (ErrMountsUnknown)
// rather than take two mounts for one. Then every entry the file depends on
// is in the directory above it on one walk from the file's directory up to
// root, and that walk is all the writer syncs: a link's target, or a mount's
// source, keeps an entry outside it, which the writer does not chase. Round 6
// walked the path as written and as it resolved instead, and missed a link
// reached through another (Codex, the Claude API and Argon on round 6). The
// check runs before anything is created, and again once the missing
// directories are, before any is synced, for one another writer put in place
// meanwhile.
//
// root must be on storage that keeps what is written to it through a reboot
// (ErrVolatile): a write on tmpfs, ramfs, or an overlay whose upper layer is
// one, has every barrier answer and is gone after the reboot, and where what
// the check must read cannot be read the writer says so (ErrMediumUnknown),
// failing closed (strictPath.storage, mediumFrom). It refuses what it can
// tell is volatile, and does not prove every stack persistent: storage that
// keeps nothing under a filesystem that reads as a disk's is the caller's to
// avoid. A root on a bind of a directory inside an overlay is refused too
// (ErrMediumUnknown): the directories above it that a copy-up creates are out
// of that mount's reach.
//
// root is the caller's guarantee: it must exist (the writer never creates
// it), and off an overlay its own entry, and those above it, must be durable
// already, or need not be, which only the caller can say (the player's engine passes
// its data directory: engine.Config.DataDir). A link at or above root is
// the caller's too, and so is root as a mount point: root is resolved once,
// as the write begins, and every path the write opens, creates and fsyncs is
// spelled from where it resolved, as are the names its errors give. A root
// that does not exist is EvalSymlinks's error. Each refusal comes before
// anything is created, written or synced, and so does a platform with no
// directory barrier (Windows: ErrNoDirectoryBarrier), where no write could be
// counted durable; WriteBytes and the renames keep their best effort there.
func WriteBytesDurable(root, path string, data []byte, tmpPrefix string) error {
	if syncDir == nil {
		return ErrNoDirectoryBarrier
	}
	p, err := checkPath(root, path)
	if err != nil {
		return err
	}
	if _, err := p.plain(); err != nil {
		return err
	}
	above, err := p.storage()
	if err != nil {
		return err
	}
	if beforeMkdirAll != nil {
		beforeMkdirAll(p.dir())
	}
	if err := fsutil.MkdirAll(p.dir(), 0o700); err != nil {
		return err
	}
	switch n, err := p.plain(); {
	case err != nil:
		return err
	case n < len(p.dirs):
		return &fs.PathError{Op: "mkdir", Path: p.dirs[n], Err: fs.ErrNotExist}
	}
	for _, d := range p.chain(len(p.dirs))[1:] {
		if err := syncDirAt(d); err != nil {
			return fmt.Errorf("the directory entries between %s and %s are not durable: %w", p.dir(), p.root, err)
		}
	}
	return writeBytes(p.file, data, tmpPrefix, p.chain(len(p.dirs)), above)
}

// writeBytes stages data beside path and renames it into place: the file
// half of WriteBytes, with chain nil, or of WriteBytesDurable, whose barrier
// then syncs every directory in chain, from the file's up to the root, and
// those above the root a copy-up can create entries in (durableChain), once
// the write has landed. path's directory exists.
func writeBytes(path string, data []byte, tmpPrefix string, chain, above []string) error {
	durable := chain != nil
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
			switch ours, idErr := onItsDirectorysMount(path); {
			case idErr != nil:
				return fmt.Errorf("%w; the rename had failed: %w", idErr, err)
			case !ours:
				return err
			}
			switch same, syncErr := sameAndSynced(path, data); {
			case syncErr != nil:
				return syncErr
			case same:
				return durableChain(chain, above)
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
		return durableChain(chain, above)
	}
	commitDirEntry(path)
	return nil
}
