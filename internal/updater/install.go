package updater

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/version"
)

// downloadTimeout bounds the archive + checksums download phase of an
// Install. Generous on purpose: the archive is tens of MiB and must
// survive a ~30 KB/s worst-case residential link, so the only job of
// this deadline is to keep a wedged CDN connection from parking the
// install forever — NOT to enforce a throughput floor. The download
// http.Client itself carries no overall Timeout (see Client doc in
// github.go).
const downloadTimeout = 15 * time.Minute

// verifyTimeout bounds the signature/notarization check. On darwin
// verifyBinary shells to `codesign … --check-notarization`, which can
// reach out to Apple and hang (offline / captive portal) — this deadline
// caps that so a wedged verify can't park the install forever. The parent
// ctx is still honoured; this only adds an upper bound.
const verifyTimeout = 2 * time.Minute

// InstallOptions configures one Install attempt.
type InstallOptions struct {
	// DataDir is the bridge's working data directory; the install
	// path creates a per-attempt scratch dir directly under it
	// (os.MkdirTemp "install-*") and writes update-state.json here
	// for boot-time rollback bookkeeping.
	DataDir string

	// BinaryPath is the absolute path of the running bridge binary
	// (typically os.Executable()). Install swaps the file at this
	// path with the freshly-extracted new binary.
	BinaryPath string

	// Force, when true, lets Install proceed even when the sessions
	// tracker reports >0 inflight downloads. Used by `bridge update
	// --force` and the admin UI's "Install anyway" affordance.
	// Operators see explicit confirmation copy first.
	Force bool

	// Sessions is the per-bridge inflight-download tracker. Nil
	// disables the gate (test harness only) — production callers
	// always wire one.
	Sessions *Tracker

	// Verifier overrides the platform-default signature/notarization
	// check (verifyBinary). Production callers leave this nil so
	// macOS gets codesign --verify and other platforms get the
	// SHA-256-only path. Tests inject a no-op verifier because the
	// fake archive's binary isn't Apple-signed.
	Verifier func(ctx context.Context, newBinary string) error

	// OverrideCompatGate, when true, lets Install proceed past the
	// MinClientVersion compat gate (i.e. install a release whose
	// floor would orphan one or more paired clients). Wired only
	// to the manual `bridge update --override-client-floor` CLI
	// path; the auto-installer never sets this flag. Operators
	// who flip it accept the orphan-on-purpose UX (older devices
	// must update before they reconnect).
	OverrideCompatGate bool
}

// Install downloads, verifies, swaps, and arms the rollback marker
// for the latest release the updater has cached. Returns the
// post-install Status; the caller is responsible for triggering the
// process restart (typically through the admin Restart endpoint or
// a CLI exit) — Install does NOT call os.Exit itself so a CLI
// caller can print a final status line before the restart cycle.
//
// Errors:
//
//   - ErrNoUpdate when no update is available (caller should refuse
//     to start an install in this case before reaching here, but
//     the check is symmetric defence).
//   - ErrActiveSessions when Force is false and Sessions.Inflight()
//     is nonzero.
//   - ErrInstallInFlight when another Install on this Updater is
//     already running (e.g. the admin POST racing the auto-installer
//     poll). Callers retry later; the admin API maps this to 409.
//   - ErrCompatGateRefused when the candidate's MinClientVersion floor
//     would orphan a still-paired older iOS client and OverrideCompatGate
//     is false.
//   - ErrNoMatchingAsset when the release's published assets don't
//     include one for the running GOOS/GOARCH (returned via the download
//     path's archiveAndChecksumFor).
//   - ErrPathNotWritable when the running binary's parent directory
//     isn't writable by the bridge process (returned by preflightWritable
//     before any irreversible work begins).
//   - Any download / verify / swap / state-save error returned verbatim
//     with a wrapped context prefix.
//
// Swap implementations are wired for every supported GOOS — Unix (incl.
// darwin / linux / BSDs) via swap_unix.go's atomic rename, Windows via
// swap_windows.go's live rename of the running executable. Windows
// requires a process restart (handled by the caller / SCM) for the new
// bytes to take effect; ErrInstallNotSupported is reserved as a public
// sentinel for hypothetical future platforms but not produced today.
//
// The marker is written BEFORE the swap so a process crash between
// swap and marker would leave the operator running the new binary
// without a rollback record (recoverable; just no automatic
// rollback). The reverse ordering would leave a marker without a
// matching swap (would trigger a spurious rollback on next boot).
// We pick the lesser of two evils.
func (u *Updater) Install(ctx context.Context, opts InstallOptions) (Status, error) {
	status := u.Status()
	if !status.UpdateAvailable || status.LatestVersion == "" {
		return status, ErrNoUpdate
	}
	// Serialize the WHOLE attempt: concurrent in-process callers (the
	// auto-installer poll and the admin POST share this Updater) used
	// to race on the scratch dir, the .bak rename target, and
	// update-state.json — one installer's deferred cleanScratch could
	// delete the other's verified binary mid-swap. Fail fast rather
	// than queue: every caller already has a retry path (next poll
	// cycle, or the operator re-clicking).
	if !u.installInFlight.CompareAndSwap(false, true) {
		return status, ErrInstallInFlight
	}
	defer u.installInFlight.Store(false)
	if !opts.Force && opts.Sessions != nil && opts.Sessions.Inflight() > 0 {
		return status, fmt.Errorf("%w: %d inflight download(s)",
			ErrActiveSessions, opts.Sessions.Inflight())
	}

	// Permission preflight FIRST, before any network I/O: if we can't
	// write to the binary's directory, the swap will fail late (after a
	// ~30 MiB download + extract + verify). Catching it up front gives the
	// operator the "try sudo" message immediately instead of after paying
	// the download cost. The swap still surfaces a writability error later
	// if the ACL changes mid-flight, but the common non-writable-dir case
	// no longer wastes a download.
	if err := preflightWritable(opts.BinaryPath); err != nil {
		return status, err
	}

	// Re-fetch the release so the asset list is fresh — the cached
	// Status has only the tag and the URL. (We could cache the full
	// release on poll, but the asset list is large-ish and changes
	// rarely — fetching here keeps memory low and the latest-poll
	// path lean.)
	rel, err := u.client.LatestRelease(ctx)
	if err != nil {
		return status, fmt.Errorf("re-fetch release: %w", err)
	}
	if normalizeTag(rel.TagName) != status.LatestVersion {
		// Race: a newer release published between poll and install.
		// Refuse rather than install something the operator didn't
		// see in the UI.
		return status, fmt.Errorf("release tag drift: cached %s, fetched %s — re-poll and try again",
			status.LatestVersion, normalizeTag(rel.TagName))
	}

	archive, checksums, err := archiveAndChecksumFor(rel)
	if err != nil {
		return status, err
	}

	// MinClientVersion compat gate. Refuses installs whose floor
	// would orphan a still-paired older iOS client. The auto-
	// installer always honours the gate; the manual CLI / admin
	// path can override via OverrideCompatGate. Pre-Phase-C
	// releases that don't ship release-meta.json are treated as
	// "no floor" — the gate is permissive when the asset is
	// absent so we don't block legitimate older releases.
	if !opts.OverrideCompatGate && u.tokenSnapshot != nil {
		// release-meta.json is a tiny JSON sidecar — the API client's
		// short poll Timeout is the right bound here, unlike the
		// archive download below.
		meta, err := releaseMetaFor(ctx, u.client.http, rel)
		if err != nil && !errors.Is(err, ErrReleaseMetaMissing) {
			return status, fmt.Errorf("fetch release-meta.json: %w", err)
		}
		if err == nil {
			// Sanity-check the meta belongs to the candidate we're about to
			// install. A stale CDN cache or a partial publication could deliver
			// an OLDER meta against a NEWER archive, driving the compat gate off
			// the wrong client floor. On mismatch, treat as no-floor (warn +
			// permissive) — matching the "absent -> no floor" posture rather
			// than gating on possibly-wrong data. Both operands go through
			// normalizeTag so a `v`-prefix / whitespace skew isn't a false
			// mismatch (status.LatestVersion is already normalized; the
			// double-normalize is idempotent and defensive).
			if meta.Version != "" && normalizeTag(meta.Version) != normalizeTag(status.LatestVersion) {
				logger.Warn("release-meta.json version mismatch; treating as no client-version floor",
					"metaVersion", meta.Version, "candidate", status.LatestVersion)
			} else if reason := compatGateReason(meta.MinClientVersion, u.tokenSnapshot()); reason != "" {
				u.recordDeferredReason(reason)
				return status, fmt.Errorf("%w: %s", ErrCompatGateRefused, reason)
			}
		}
	}

	// Scratch dir directly under dataDir keeps temp files inside the
	// bridge's writable area (avoids /tmp permissions surprises on
	// macOS) and makes cleanup obvious. Each attempt gets its own
	// MkdirTemp directory — deliberately NO persistent shared parent
	// (the earlier <DataDir>/updates/ root): unique names mean a
	// root-run CLI's leftovers (root-owned, 0o700) never block later
	// attempts by the unprivileged service user, and no cleanup —
	// including one from a concurrent caller in ANOTHER process (the
	// CLI can't share the in-process try-lock) — can delete this
	// attempt's files mid-swap. DataDir itself is MkdirAll'd first to
	// preserve the old implicit create-if-missing behaviour; its
	// ownership is already correct for the running user.
	if err := os.MkdirAll(opts.DataDir, 0o700); err != nil {
		return status, fmt.Errorf("create scratch: %w", err)
	}
	// Opportunistic sweep of dirs abandoned by a killed attempt — see
	// ReapScratchDirs. Runs here rather than at startup so it also
	// covers the long-lived-process case, and costs one ReadDir per
	// install.
	ReapScratchDirs(opts.DataDir, time.Now())
	scratch, err := os.MkdirTemp(opts.DataDir, scratchDirPrefix)
	if err != nil {
		return status, fmt.Errorf("create scratch: %w", err)
	}
	defer cleanScratch(scratch)

	// Sanitise the release asset name before using it as a path component.
	// `archive.Name` comes from the GitHub Releases API; a `../../../`-style
	// name (a compromised release) would otherwise let the download write
	// outside `scratch` (CWE-22 path traversal). DeepSeek review; the "."/".."
	// residue reject added per Gemini security-MEDIUM on PR #368.
	safeName, err := sanitizeAssetName(archive.Name)
	if err != nil {
		return status, err
	}
	archivePath := filepath.Join(scratch, safeName)
	// Asset fetches ride the timeout-free download client — the API
	// client's overall Timeout caps the body read and would kill a
	// multi-MiB archive mid-stream on a slow link. A hung CDN
	// connection is bounded by this per-phase deadline instead.
	dlCtx, cancelDL := context.WithTimeout(ctx, downloadTimeout)
	defer cancelDL()
	if _, err := downloadVerified(dlCtx, u.client.downloadClient(),
		archive.BrowserDownloadURL, safeName,
		checksums.BrowserDownloadURL, archivePath); err != nil {
		return status, fmt.Errorf("download: %w", err)
	}

	binaryName := "bridge"
	if isWindows() {
		binaryName = "bridge.exe"
	}
	extracted := filepath.Join(scratch, binaryName)
	if err := extractBridgeBinary(archivePath, extracted); err != nil {
		return status, fmt.Errorf("extract: %w", err)
	}

	verifier := opts.Verifier
	if verifier == nil {
		verifier = verifyBinary
	}
	// Bound the verify: on darwin it can contact Apple and hang offline
	// (see verifyTimeout). Derives from the parent ctx so a caller
	// cancellation still propagates.
	verifyCtx, cancelVerify := context.WithTimeout(ctx, verifyTimeout)
	err = verifier(verifyCtx, extracted)
	cancelVerify()
	if err != nil {
		return status, fmt.Errorf("verify: %w", err)
	}

	// Marker first (see method-doc rationale). This is the boot
	// contract: Status="installing" + TargetVersion → next boot
	// expects to be running TargetVersion or rolls back.
	//
	// A fresh State value here is also what CLEARS a prior
	// RejectedVersion: SaveState persists the whole struct, so
	// installing anything (including, deliberately, a manual re-install
	// of the rejected build) retires the operator's rollback. Don't
	// switch this to a load-modify-save without re-deciding that.
	state := State{
		Status:          "installing",
		TargetVersion:   status.LatestVersion,
		PreviousVersion: version.ServerVersion,
		AttemptedAt:     time.Now().UTC(),
	}
	if err := SaveState(opts.DataDir, state); err != nil {
		return status, fmt.Errorf("save state: %w", err)
	}

	// swapBinary calls this immediately before its first destructive
	// filesystem operation — AFTER the Windows SCM stop, which can hold
	// the marker armed over an untouched filesystem for up to 15 s. See
	// State.SwapStarted for why a boot-time restore must not fire until
	// this has landed.
	markSwapStarted := func() error {
		state.SwapStarted = true
		return SaveState(opts.DataDir, state)
	}
	if err := swapBinary(opts.BinaryPath, extracted, ".bak", markSwapStarted); err != nil {
		// Swap failed — clear the marker so the next boot doesn't
		// roll back something that didn't actually change.
		_ = ClearState(opts.DataDir)
		return status, fmt.Errorf("swap: %w", err)
	}

	// Refresh status's CurrentVersion field for the response —
	// we'll be running TargetVersion after restart. The cached
	// Status itself will refresh on the new binary's first poll.
	status.CurrentVersion = status.LatestVersion + " (pending restart)"
	return status, nil
}

// Sentinel errors so callers can distinguish "client-fixable"
// (no-update / active-sessions / install-in-flight /
// unsupported-platform) from "something went wrong".
var (
	ErrNoUpdate            = errors.New("no update available")
	ErrActiveSessions      = errors.New("active downloads — refuse to restart")
	ErrInstallInFlight     = errors.New("an install is already in progress")
	ErrPathNotWritable     = errors.New("binary path not writable by this user (try sudo bridge update)")
	ErrInstallNotSupported = errors.New("self-install not yet supported on this platform; download manually and replace the binary (see PROTOCOL.md → Updates)")
	ErrCompatGateRefused   = errors.New("install would orphan a paired iOS client below the candidate's MinClientVersion floor")
)

// Rollback restores bridge.bak over the live binary. Used by the
// admin "Roll back" button when the operator notices the new
// version is broken AFTER a successful install (the boot-time
// rollback only fires on truly unbootable builds).
//
// The rejected version is RECORDED (State.RejectedVersion) rather than
// the marker simply being cleared: a rollback only takes effect on
// restart, and the poller on that restart sees the still-latest
// candidate against the now-older running version and would re-install
// it within seconds — the operator's only escape being to hand-edit
// bridge.yaml. A strictly newer release still installs normally.
//
// Errors: ErrInstallInFlight when an Install (or another Rollback)
// is already running on this Updater, ErrActiveSessions when Force
// is false and Sessions.Inflight() is nonzero.
func (u *Updater) Rollback(opts InstallOptions) error {
	// Phase B-Windows (PR #48) implements the rollback rename
	// alongside darwin/linux — no platform guard needed here. The
	// Windows-side `RollbackBinary` handles the SCM-stop dance
	// transparently.
	//
	// Serialise against Install with the same try-lock: rollback
	// renames .bak over the live binary and clears update-state.json
	// — the exact swap targets an in-flight install is mutating.
	if !u.installInFlight.CompareAndSwap(false, true) {
		return ErrInstallInFlight
	}
	defer u.installInFlight.Store(false)
	if !opts.Force && opts.Sessions != nil && opts.Sessions.Inflight() > 0 {
		return fmt.Errorf("%w: %d inflight download(s)",
			ErrActiveSessions, opts.Sessions.Inflight())
	}
	// Read the marker BEFORE the swap undoes it — TargetVersion is the
	// release being rejected in both reachable rollback windows (marker
	// "installing", still running the old binary; and marker
	// "installed", running the new one).
	rejected := rejectedVersionFor(opts.DataDir)
	if err := RollbackBinary(opts.BinaryPath, ".bak"); err != nil {
		return err
	}
	// A restart deferred by the auto-installer would now bounce the
	// process for a binary that is no longer on disk. Nothing is
	// pending any more.
	u.pendingRestart.Store(false)
	// Status "" so DecideBootAction reads this as BootNoop — the marker
	// survives purely to carry the rejection.
	if err := SaveState(opts.DataDir, State{RejectedVersion: rejected}); err != nil {
		// Falling back to the pre-rejection behaviour is strictly no
		// worse than before: the auto-installer may re-install, but a
		// stale "installing" marker must not be left behind to drive a
		// spurious boot-time rollback.
		logger.Warn("could not record the rolled-back version; auto-install may re-install it",
			"version", rejected, "err", err)
		_ = ClearState(opts.DataDir)
	}
	return nil
}

// rejectedVersionFor names the release a rollback is rejecting. The
// install marker's TargetVersion is authoritative — version.ServerVersion
// is only correct once the operator has already restarted onto the new
// binary, and the admin "Roll back" button is reachable before that too.
// Falls back to the running version when no marker survives (a
// hand-staged .bak, or a rollback after BootCleanupBak retired the
// marker).
func rejectedVersionFor(dataDir string) string {
	if st, err := LoadState(dataDir); err == nil && st.TargetVersion != "" {
		return normalizeTag(st.TargetVersion)
	}
	return normalizeTag(version.ServerVersion)
}

// armSwap invokes swapBinary's swap-started hook, if the caller wired
// one. Shared by both platform implementations so the nil-tolerance and
// the error wrapping can't drift between them. A non-nil return MUST
// abort the swap before anything is mutated — that is the whole point
// of the hook.
func armSwap(markSwapStarted func() error) error {
	if markSwapStarted == nil {
		return nil
	}
	if err := markSwapStarted(); err != nil {
		return fmt.Errorf("record swap-started marker: %w", err)
	}
	return nil
}

// removeFunc indirects os.Remove for preflightWritable's
// delete-permission probe. Production = os.Remove; tests override it to
// exercise the retry/fail path without a real restrictive ACL.
// Test-only seam — production code MUST NOT mutate it (same convention
// as renameFunc in internal/manifest/extractors.go).
var removeFunc = os.Remove

// writeProbeName is the FIXED basename preflightWritable creates in the
// binary's directory. Fixed, not os.CreateTemp's random suffix, because
// the case this probe exists to detect — "can create but not delete"
// (a Windows FILE_ADD_FILE-without-FILE_DELETE_CHILD ACL) — is exactly
// the case where the probe file is left behind. A random name leaked a
// fresh undeletable file per attempt, and Install runs the preflight on
// EVERY attempt, so auto-install at the 6 h cadence accumulated ~4/day
// in e.g. /usr/local/bin, forever. With a fixed name the leak is capped
// at one.
//
// A fixed name is only safe BECAUSE of the stale-probe recovery in
// preflightWritable — see there. Deliberately NOT O_EXCL: a leftover
// probe would then fail every subsequent preflight. Two processes
// probing concurrently is fine too — the loser's Remove gets ENOENT,
// which the retry loop treats as success (the directory is demonstrably
// deletable).
const writeProbeName = ".bridge-write-test"

// preflightWritable checks the directory the binary lives in is both
// writable AND deletable by this process. Better-message-than-
// permission-denied: we ask the operator to run sudo / re-install in
// user mode, with a concrete remediation path.
func preflightWritable(binaryPath string) error {
	dir := filepath.Dir(binaryPath)
	name := filepath.Join(dir, writeProbeName)
	tmp, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		// A stale probe left by ANOTHER user is root-owned 0600 (the
		// documented `sudo bridge update` vs unprivileged-service split
		// on bridge.ars.md), so opening it for writing is EACCES even
		// when the DIRECTORY is perfectly writable — which would turn a
		// bounded one-file leak into a PERMANENT false "not writable"
		// that blocks every future update. Clear it and retry once.
		//
		// This is a permission proof, not a bypass: unlinking needs
		// write+execute on the DIRECTORY and not ownership of the file,
		// and directory write permission is exactly what this function
		// is testing. If the unlink fails too, the directory genuinely
		// can't be managed and the original create error — the
		// informative one — is what surfaces.
		//
		// A sticky-bit (+t) directory holding another user's probe
		// resolves correctly rather than as an edge case: the unlink
		// fails, and so would the swap's own rename of the root-owned
		// live binary, so "not writable" is the true answer there.
		if rmErr := removeFunc(name); rmErr != nil {
			return fmt.Errorf("%w (path=%s, err=%v)", ErrPathNotWritable, dir, err)
		}
		tmp, err = os.OpenFile(name, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("%w (path=%s, err=%v)", ErrPathNotWritable, dir, err)
		}
	}
	tmp.Close()
	// Verify we can DELETE here too, not just create — a "create file
	// but not delete child" ACL (Windows) or a sticky-bit dir (POSIX)
	// lets CreateTemp succeed while the real binary swap later fails.
	// Retry briefly to absorb the Windows Defender oplock window that
	// transiently locks a freshly-closed file (the AV-flakiness class
	// behind renameWithRetry, PR #100); a genuine permission gap still
	// surfaces after the budget.
	// ~1s total budget: a Defender oplock on a freshly-closed temp can
	// persist for several hundred ms under load / in a VM, so a 100ms
	// budget would false-fail the preflight there (Gemini on PR #385).
	// The instant success path (first Remove succeeds) pays nothing.
	var rmErr error
	for _, backoff := range []time.Duration{0, 10 * time.Millisecond, 50 * time.Millisecond, 150 * time.Millisecond, 300 * time.Millisecond, 500 * time.Millisecond} {
		if backoff > 0 {
			time.Sleep(backoff)
		}
		rmErr = removeFunc(name)
		if rmErr == nil || errors.Is(rmErr, os.ErrNotExist) {
			return nil
		}
	}
	return fmt.Errorf("%w (path=%s, cannot delete scratch file: %v)", ErrPathNotWritable, dir, rmErr)
}

// cleanScratch wipes one attempt's scratch dir so disk doesn't
// accumulate failed-download leftovers. Each attempt removes only its
// own MkdirTemp directory so a concurrent attempt's files survive a
// stray cleanup.
func cleanScratch(scratch string) {
	_ = os.RemoveAll(scratch)
}

// scratchDirPrefix is the MkdirTemp prefix for a per-attempt scratch
// dir. Shared by the creator and the reaper so the two can't drift.
const scratchDirPrefix = "install-"

// scratchReapAge is how long an `install-*` dir must have gone
// untouched before ReapScratchDirs treats it as abandoned. Generous:
// it must exceed the longest plausible live install (downloadTimeout is
// 15 min for the archive alone, plus verify and swap), because the
// window is the ONLY thing separating an abandoned dir from one another
// process is actively filling — the in-process try-lock can't see the
// `bridge update` CLI.
const scratchReapAge = 2 * time.Hour

// ReapScratchDirs removes abandoned per-attempt scratch directories
// from dataDir and returns how many it deleted.
//
// The per-attempt `install-<random>` layout gave each caller isolation
// from every other caller's cleanup, but it also gave up the
// self-healing the old shared `<DataDir>/updates/` dir had: that one was
// RemoveAll'd wholesale at the end of every attempt, so a previous run's
// leftovers went with it. Now nothing removes a dir whose deferred
// cleanScratch never ran — a kill, an OOM, or a power loss mid-install
// strands the downloaded archive plus the extracted binary (tens of MiB)
// directly in DataDir, permanently, next to the DB and the certs.
//
// Only mtime-quiet dirs are touched, so an install running right now in
// another process is never disturbed. Fail-open throughout: an
// unreadable dataDir or an undeletable entry is skipped, because
// reclaiming disk must never be able to fail an update.
func ReapScratchDirs(dataDir string, now time.Time) int {
	if dataDir == "" {
		// Never enumerate the process working directory — os.ReadDir("")
		// would, and this function deletes what it finds.
		return 0
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return 0
	}
	reaped := 0
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), scratchDirPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) < scratchReapAge {
			continue // possibly a live attempt in another process
		}
		if err := os.RemoveAll(filepath.Join(dataDir, e.Name())); err != nil {
			logger.Warn("could not reap abandoned install scratch dir",
				"dir", e.Name(), "err", err)
			continue
		}
		reaped++
	}
	if reaped > 0 {
		logger.Info("reaped abandoned install scratch dirs", "count", reaped)
	}
	return reaped
}

// sanitizeAssetName reduces a GitHub release asset name to a single safe path
// component for joining into the scratch dir, and rejects anything that isn't
// a plain filename. filepath.Base strips directory + traversal segments (so
// "../../etc/passwd" → "passwd") using the SAME separator semantics
// filepath.Join later uses to place the file — that pairing is what keeps the
// result inside scratch on every OS.
//
// (A path.Base here would be WRONG, despite GitHub names being forward-slash
// strings: path.Base only treats "/" as a separator, so a Windows-style
// "..\\..\\x" payload would survive Base intact and then get split by
// filepath.Join on Windows → traversal. Gemini's literal path.Base suggestion
// from the r2 review is declined for this reason.)
//
// Three checks, in order: (1) reject any backslash on the RAW name up front
// — a "\" never appears in a legit forward-slash GitHub name, and rejecting
// it before filepath.Base makes behaviour uniform across OSes (non-Windows
// filepath.Base leaves "\" intact; Windows strips it to a basename — the
// same input must not take two different paths); (2) reject the "."/".."
// residue (which would escape scratch on Join); (3) reject a residual "/"
// in the basename (filepath.Base returns "/" for a bare root path — neither
// "." nor ".." but not a filename). Pure helper so the rejects are
// unit-testable without driving the whole Install network flow. DeepSeek
// review + Gemini security-MEDIUM on PR #368; cross-platform reject per the
// r2 review + CodeRabbit on PR #385.
func sanitizeAssetName(name string) (string, error) {
	if strings.ContainsRune(name, '\\') {
		return "", fmt.Errorf("invalid release asset name: %q", name)
	}
	base := filepath.Base(name)
	if base == "." || base == ".." {
		return "", fmt.Errorf("invalid release asset name: %q", name)
	}
	if strings.ContainsRune(base, '/') {
		return "", fmt.Errorf("invalid release asset name: %q", name)
	}
	return base, nil
}

// isWindows is the runtime check for the swap path's
// platform-conditional behaviour. Wrapped in a helper so a future
// build-tag-gated install path can substitute it.
func isWindows() bool {
	return goosWindows
}

// goosWindows is set in install_windows.go to true and in
// install_other.go to false. Keeps "what platform am I" out of
// runtime.GOOS string compares scattered through the package.
//
// Yes this is the same answer runtime.GOOS gives you; the build-
// tagged variable form makes the test-only override trivial.
