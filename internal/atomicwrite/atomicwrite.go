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
package atomicwrite

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/logging"
)

var logger = logging.Component("atomicwrite")

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
// a successful rename. Wrapped in a var purely as a test seam — the
// real syscall can't be made to fail portably, and a dir-sync failure
// is exactly the branch that MUST NOT turn a completed write into a
// returned error. Production code MUST NOT mutate this; only in-package
// tests override it (with a `t.Cleanup` restore), same convention as
// `renameFunc`.
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
// On Windows `fsutil.SyncParentDir` is a compile-time no-op — see its
// docblock for why FlushFileBuffers can't service a directory handle.
var syncParentDir = fsutil.SyncParentDir

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
func commitDirEntry(path string) {
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
	var err error
	for _, d := range renameBackoff {
		if d > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d):
			}
		}
		err = renameFunc(src, dst)
		if err == nil {
			// Rename committed — flush the directory entry so a
			// crash immediately after can't lose it. Best-effort:
			// see commitDirEntry for why a failure here must not
			// fail an already-successful write.
			commitDirEntry(dst)
			return nil
		}
	}
	return err
}

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
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
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
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := RenameWithRetry(tmpName, path); err != nil {
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
		// No parent-dir barrier on this branch: we didn't
		// publish the directory entry, the concurrent winner
		// did — and its own RenameWithRetry already ran the
		// barrier for it.
		if existing, readErr := os.ReadFile(path); readErr == nil && bytes.Equal(existing, data) {
			return nil
		}
		return err
	}
	tmpName = "" // rename succeeded — suppress the deferred os.Remove
	return nil
}
