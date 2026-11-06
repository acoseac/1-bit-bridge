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

	// ConfigPath is the settings file the running bridge loaded, which
	// the candidate is checked against before the swap. Every production
	// caller names it: the console's install, the automatic one and
	// `bridge update`. Empty skips the check.
	ConfigPath string

	// Automatic marks the auto-installer's install, which maybeAutoInstall
	// sets on its copy. A candidate that predates the settings check is
	// held back from it, and installed with a warning by hand.
	Automatic bool

	// ConfigCheck overrides the candidate's settings check (CheckConfig).
	// Production leaves it nil; a test stands in for a candidate.
	ConfigCheck func(ctx context.Context, candidate, configPath string) ConfigCheck

	// RestoreBackup overrides the rename that puts the backup back over the
	// binary in an operator's Rollback (RollbackBinary). Production leaves
	// it nil; a test holds a rollback in it.
	RestoreBackup func(dst, backupExt string) error

	// BeforeRollbackRecord, nil in production, runs in an operator's
	// Rollback once it has read the marker and decided, just before it
	// writes its record, the install marker's lock held: a test's hold on a
	// rollback there.
	BeforeRollbackRecord func()

	// holdBeforeSwap, nil in production, runs in Install just before the
	// swap, its marker armed: a test's hold on an install there.
	holdBeforeSwap func()
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
//   - ErrMarkerLockUnavailable when the install marker's lock cannot be
//     held (another process past its wait, no lock to give, a failure, a
//     filesystem that keeps none), and ctx's error for a stop in its
//     wait: nothing armed or swapped (backlog B692).
//   - ErrInstallPendingRestart, as above, and for the auto-installer
//     ErrInstallHeldBack and ErrInstallAwaitsConfirmation (each a
//     holdError), when the decision made again under that lock refuses
//     (decideUnderLock).
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
//
// The marker's read, the decision, the marker's writes and the swap
// all run holding the install marker's lock (backlog B692;
// lockMarkerForUpdate), taken only once the download, its
// verification and the settings check are done: never through them,
// since a migrating store open and every start wait at most
// stateLockWait for it, and the download alone may take
// downloadTimeout. Under it Install decides again on the marker as it
// is then (decideUnderLock), since another process may have installed
// or recorded a hold meanwhile. Where the lock cannot be held it
// refuses with ErrMarkerLockUnavailable, nothing armed or swapped; a
// stop in its wait returns ctx's error.
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
	// A swap for this same target has already landed and the process has
	// not restarted into it. Installing again re-runs the whole ~30 MiB
	// download, and — worse — swapBinary's EEXIST retry does
	// os.Remove(bak) before re-linking, so the SECOND install destroys
	// the operator's rollback target and replaces it with a copy of what
	// is already live. The console's canRollback keeps reporting true
	// throughout: the backup is there, and nothing the marker records
	// refuses its rollback, since the version installed has not started.
	//
	// Reachable through the ordinary console flow, not an exotic one:
	// apiUpdatesInstall does NOT restart (restart is a separate operator
	// action), and u.status.CurrentVersion is written ONCE at
	// construction — Install only decorates the local copy it returns —
	// so UpdateAvailable stays true for the process lifetime and the
	// console keeps inviting the click that does the damage.
	//
	// AFTER the in-flight try-lock, not before it: a caller arriving
	// while another install is mid-flight should hear "an install is
	// already in progress", which is the more immediate and more
	// actionable truth. This one is a fact about the HOST and only
	// matters once the caller would otherwise have proceeded. It also
	// means reaching this refusal PROVES the lock was free, which is
	// what TestInstallConcurrentCallsSerialized now leans on.
	//
	// The marker ALONE, deliberately — NOT `u.pendingRestart.Load() ||
	// …`. The in-memory flag records THAT a swap landed and not WHICH
	// version, so an install of a genuinely newer release would have
	// been refused by it, stranding the host on a version it had not
	// even booted (TestANewerReleaseIsStillInstallable caught exactly
	// that during development). The marker is version-aware, is written
	// before every swap, and is visible to the CLI's separate process —
	// so it is strictly stronger here, and the flag keeps its one real
	// job: the auto-installer's pending-restart fast path.
	//
	// Read here without the install marker's lock, to spare a download
	// it would refuse; the decision that binds is made again under the
	// lock, before the marker's write (decideUnderLock, backlog B692),
	// since `bridge update` and serve's own install share no
	// installInFlight and either may install while the other downloads.
	if u.swapAwaitingRestart(opts.DataDir, status.LatestVersion) {
		return status, fmt.Errorf("%w: %s is already staged on disk", ErrInstallPendingRestart, status.LatestVersion)
	}
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

	// The candidate's settings check (backlog B613), once it is verified
	// and before anything is armed. A version that refuses this bridge's
	// settings exits at their load at its first start, before the boot
	// rollback runs, and its service manager starts it again and again;
	// the check asks it first, through its own loader, so a tightening of
	// any setting (B528's fraction, B533's leading zero, or the next one)
	// keeps the binary where it is.
	candidate := extracted
	if opts.ConfigPath != "" {
		staged, err := stageForCheck(extracted, opts.BinaryPath)
		if err != nil {
			return status, fmt.Errorf("stage the candidate for its settings check: %w", err)
		}
		// Gone once the swap has moved it into place; removed here on
		// every other way out.
		defer func() { _ = os.Remove(staged) }()
		warning, err := u.checkCandidate(ctx, opts, staged, status.LatestVersion)
		if err != nil {
			return status, err
		}
		status.InstallWarning = warning
		candidate = staged
	}

	// The install marker's lock (backlog B692), from here through the swap:
	// taken in the documented order, since this process holds server.lock
	// (serve) or nothing (`bridge update`), never the ladder, and takes
	// neither under it. Windows' swap stops the service first, for up to
	// scmStopWait, with the lock held: serve's stop takes no part of it.
	lock, err := lockMarkerForUpdate(ctx, opts.DataDir, "nothing was installed")
	if err != nil {
		return status, err
	}
	defer lock.Release()
	prior, priorErr := LoadState(opts.DataDir)
	if err := u.decideUnderLock(opts, status.LatestVersion); err != nil {
		return status, err
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
	if opts.holdBeforeSwap != nil {
		opts.holdBeforeSwap()
	}
	if err := swapBinary(opts.BinaryPath, candidate, ".bak", markSwapStarted); err != nil {
		// Swap failed — the marker back as this install read it under the
		// lock, so the next boot doesn't roll back something that didn't
		// actually change, and a hold or a veto it carried stays (backlog
		// B692; none where there was no marker to read, as before).
		if perr := putMarkerBack(opts.DataDir, prior, priorErr); perr != nil {
			logger.Warn("could not put the install marker back after a swap that failed", "err", perr)
		}
		return status, fmt.Errorf("swap: %w", err)
	}

	// Record the STATE, not the intent: a swap landed and nothing has
	// restarted into it. The one place that knows a swap happened is the
	// place that records it.
	//
	// NOT pendingRestart — that is the auto-installer's restart intent,
	// and its own docblock forbids a manual install from setting it ("a
	// manual admin/CLI install must NOT trigger the auto-installer's
	// restart"). An earlier draft did exactly that, so with autoInstall
	// on an operator's manual install would have been followed by an
	// unrequested restart hours later (CodeRabbit on #977).
	u.swapPending.Store(true)

	// Refresh status's CurrentVersion field for the response —
	// we'll be running TargetVersion after restart. The cached
	// Status itself will refresh on the new binary's first poll.
	status.CurrentVersion = status.LatestVersion + " (pending restart)"
	return status, nil
}

// Sentinel errors so callers can distinguish "client-fixable"
// (no-update / active-sessions / install-in-flight / pending-restart /
// unsupported-platform) from "something went wrong".
var (
	ErrNoUpdate            = errors.New("no update available")
	ErrActiveSessions      = errors.New("active downloads — refuse to restart")
	ErrInstallInFlight     = errors.New("an install is already in progress")
	ErrPathNotWritable     = errors.New("binary path not writable by this user (try sudo bridge update)")
	ErrInstallNotSupported = errors.New("self-install not yet supported on this platform; download manually and replace the binary (see PROTOCOL.md → Updates)")
	ErrCompatGateRefused   = errors.New("install would orphan a paired iOS client below the candidate's MinClientVersion floor")
	// ErrInstallPendingRestart means the candidate is already on disk
	// and the process has not restarted into it. The remedy is a
	// RESTART, not another install — which is the whole point of
	// naming it: a second install re-downloads the same bytes and
	// destroys the .bak the first one created.
	ErrInstallPendingRestart = errors.New("this release is already installed and waiting for a restart; restart the bridge instead of installing again")
	// ErrConfigRefused means the candidate's settings check refused this
	// bridge's settings: it would not start with them, so it was not
	// installed, and the refusal is recorded (State.RejectedReason). The
	// remedy is the settings, which the error's reasons name by line.
	ErrConfigRefused = errors.New("the new version refuses this bridge's settings")
	// ErrConfigCheckFailed means the candidate's settings check did not
	// answer (it could not run, ran past its time, or exited another
	// way). Nothing was installed and nothing recorded: a later attempt
	// checks again.
	ErrConfigCheckFailed = errors.New("the new version could not check this bridge's settings")
	// ErrConfigCheckUnsupported means the candidate predates the settings
	// check. The auto-installer holds it back; a manual install takes it
	// with a warning (Status.InstallWarning).
	ErrConfigCheckUnsupported = errors.New("the new version predates the settings check")
	// ErrInstallHeldBack means the auto-installer's install found, under the
	// install marker's lock, a hold of its candidate that another process
	// recorded after the auto-installer's gate read the marker (HeldBack):
	// nothing was installed.
	ErrInstallHeldBack = errors.New("the version is held back from the auto-installer")
	// ErrInstallAwaitsConfirmation means the auto-installer's install found,
	// under the install marker's lock, that the running version's own install
	// awaits its confirmation (ProbationHolds; backlog B660): nothing was
	// installed, so the backup a crash rollback puts back stays.
	ErrInstallAwaitsConfirmation = errors.New("the running install awaits its confirmation")
	// ErrMarkerLockUnavailable means an install, a rollback, a hold's record
	// or a restamp was refused because the install marker's lock
	// (update-state.lock) could not be held: nothing was written or swapped.
	ErrMarkerLockUnavailable = errors.New("the install marker's lock cannot be held")
	// ErrRollbackRefused means the operator's rollback would put the backup
	// back over a store a newer version may have changed: nothing was
	// written or swapped.
	ErrRollbackRefused = errors.New("the rollback was refused")
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
// Since backlog B692 it holds the install marker's lock
// (lockMarkerForUpdate) from its read of the marker through its record
// and the backup's rename, so no migrating store open records, and no
// start's housekeeping decides, in between. Under it the rollback
// refuses where the marker records that a version other than the
// backup's opened the data dir, or cannot be read (RollbackRefusalOn,
// which the console's Updates panel asks of its own read, backlog B707:
// that version's store may hold what the backup's version does not
// read; no forced downgrade is offered), and otherwise records, durably
// and before the rename, the marker it read with the version rolled
// back from held back, its store migrations vetoed and the rollback
// under way (OperatorRollbackRecord), so a command of that version
// still running from its image cannot migrate the store past the
// version put back, and clears the under-way mark only once the rename
// has landed (round 2): a rollback the process's end stops between the
// two leaves a record the version that runs next finishes. A rename
// that fails puts the marker back as it was read. A lock it cannot hold
// refuses with ErrMarkerLockUnavailable, and a stop in its wait returns
// ctx's error, nothing read, written or renamed either way.
//
// Errors: ErrInstallInFlight when an Install (or another Rollback)
// is already running on this Updater, ErrActiveSessions when Force
// is false and Sessions.Inflight() is nonzero, ErrMarkerLockUnavailable,
// and ErrRollbackRefused.
func (u *Updater) Rollback(ctx context.Context, opts InstallOptions) error {
	// Phase B-Windows (PR #48) implements the rollback rename
	// alongside darwin/linux — no platform guard needed here. The
	// Windows-side `RollbackBinary` handles the SCM-stop dance
	// transparently.
	//
	// Serialise against Install with the same try-lock: rollback
	// renames .bak over the live binary and rewrites update-state.json
	// — the exact swap targets an in-flight install is mutating.
	if !u.installInFlight.CompareAndSwap(false, true) {
		return ErrInstallInFlight
	}
	defer u.installInFlight.Store(false)
	if !opts.Force && opts.Sessions != nil && opts.Sessions.Inflight() > 0 {
		return fmt.Errorf("%w: %d inflight download(s)",
			ErrActiveSessions, opts.Sessions.Inflight())
	}
	lock, err := lockMarkerForUpdate(ctx, opts.DataDir, "nothing was rolled back")
	if err != nil {
		return err
	}
	defer lock.Release()
	// Read the marker BEFORE the swap undoes it — TargetVersion is the
	// release being rejected in both reachable rollback windows (marker
	// "installing", still running the old binary; and marker
	// "installed", running the new one).
	st, err := LoadState(opts.DataDir)
	if why := RollbackRefusalOn(st, err); why != "" {
		if err != nil {
			// The read's own error, kept for code and said without its path.
			return fmt.Errorf("%w: %s (%w); nothing was rolled back", ErrRollbackRefused, why, markerIOError{err})
		}
		return fmt.Errorf("%w: %s; nothing was rolled back", ErrRollbackRefused, why)
	}
	if fi, err := os.Stat(opts.BinaryPath + ".bak"); err != nil || fi.IsDir() {
		return errors.New("there is no backup of the previous binary beside this one to put back; nothing was rolled back")
	}
	rejected := rejectedVersionOf(st)
	if opts.BeforeRollbackRecord != nil {
		opts.BeforeRollbackRecord()
	}
	// The hold and the veto first, durably, then the rename (as the early
	// rollback and the boot's do): a rename that landed before them would
	// leave the version rolled back free to install again, and its
	// commands to migrate the store past the binary put back.
	if err := SaveState(opts.DataDir, OperatorRollbackRecord(st, rejected)); err != nil {
		return fmt.Errorf("the rollback of %s could not be recorded (%w), and a backup put back without that record could be "+
			"installed over again, or migrated past by that version's commands; nothing was rolled back", rejected, markerIOError{err})
	}
	restore := opts.RestoreBackup
	if restore == nil {
		restore = RollbackBinary
	}
	if err := restore(opts.BinaryPath, ".bak"); err != nil {
		// The binary in place is still the one rolled back from: the
		// marker back as it was read.
		if perr := putMarkerBack(opts.DataDir, st, nil); perr != nil {
			return fmt.Errorf("%w; nor could the install marker be put back as it was (%w)", err, markerIOError{perr})
		}
		return err
	}
	// The backup is back: the rollback is no longer under way (round 2). A
	// record this write cannot clear is finished by the boot of the version
	// put back (BootRollbackFinished), and the rollback stands meanwhile, its
	// hold and its veto with it, so its failure is said and not returned.
	done := OperatorRollbackRecord(st, rejected)
	if done.RollbackStarted {
		done.RollbackStarted = false
		if err := SaveState(opts.DataDir, done); err != nil {
			logger.Warn("the rollback is done, and its record could not be cleared; the next start finishes it",
				"version", rejected, "err", markerIOError{err})
		}
	}
	// A restart deferred by the auto-installer would now bounce the
	// process for a binary that is no longer on disk. Nothing is
	// pending any more.
	u.pendingRestart.Store(false)
	u.swapPending.Store(false)
	return nil
}

// checkCandidate runs the staged candidate's settings check against the
// settings the bridge loaded, and decides what the install does with its
// answer (backlog B613):
//
//   - valid: the install goes on.
//   - refused: nothing is installed, for a manual install as for an
//     automatic one, since the candidate would not start with these
//     settings; the refusal is recorded against a digest of them, which
//     holds the version back from the auto-installer until they change.
//   - unsupported, a candidate that predates the check: the auto-installer
//     holds it back, recorded whatever the settings are; a manual install
//     takes it, with a warning (the operator can always choose a version).
//   - failed: nothing is installed and nothing recorded, so the next
//     attempt checks again; a check the context stopped carries the
//     cancellation, which the auto-installer does not count as a failure.
//
// A stop while a hold's record waits for the install marker's lock is the
// answer too, ctx's error, never the refusal's (backlog B692's round 2):
// nothing was recorded, and a caller stopped there is told it was stopped.
func (u *Updater) checkCandidate(ctx context.Context, opts InstallOptions, staged, target string) (warning string, err error) {
	check := opts.ConfigCheck
	if check == nil {
		check = CheckConfig
	}
	res := check(ctx, staged, opts.ConfigPath)
	switch res.Verdict {
	case ConfigValid:
		return "", nil
	case ConfigRefused:
		logger.Warn("the new version refuses this bridge's settings; it was not installed",
			"version", target, "reasons", strings.Join(res.Reasons, "; "))
		if err := recordHold(ctx, opts, target, refusedReason(target, res.Reasons), true); err != nil {
			return "", err
		}
		return "", &holdError{sentinel: ErrConfigRefused,
			reason: target + " refuses this bridge's settings, so it was not installed: " + reasonText(res.Reasons)}
	case ConfigCheckUnsupported:
		if !opts.Automatic {
			logger.Warn("installing a version that predates the settings check, without it", "version", target)
			return fmt.Sprintf("%s predates the settings check, so it was installed without one: "+
				"if it refuses this bridge's settings it will not start until they are fixed", target), nil
		}
		reason := fmt.Sprintf("%s predates the settings check, so it cannot say whether it loads this bridge's settings "+
			"and is not installed automatically: install it by hand to take it", target)
		if err := recordHold(ctx, opts, target, reason, false); err != nil {
			return "", err
		}
		return "", &holdError{sentinel: ErrConfigCheckUnsupported, reason: reason}
	}
	if res.Err != nil {
		return "", fmt.Errorf("%w: %w", ErrConfigCheckFailed, res.Err)
	}
	logger.Warn("the new version's settings check did not answer; nothing was installed",
		"version", target, "detail", res.Detail, "output", res.Output)
	return "", fmt.Errorf("%w: %s: %s; nothing was installed, and the bridge log has the check's output",
		ErrConfigCheckFailed, target, res.Detail)
}

// holdError is the check's refusal, or a candidate the auto-installer holds
// back because it predates the check: a sentence that says so and why, the
// reason the console and `bridge update` show, matching its sentinel through
// errors.Is.
type holdError struct {
	sentinel error
	reason   string
}

func (e *holdError) Error() string { return e.reason }

func (e *holdError) Unwrap() error { return e.sentinel }

// RefusalReason is the reason err carries when it is the settings check's
// refusal, or a hold of a candidate that predates the check.
func RefusalReason(err error) (string, bool) {
	var he *holdError
	if errors.As(err, &he) {
		return he.reason, true
	}
	return "", false
}

// refusedReason is the recorded reason a version refused the settings.
func refusedReason(target string, reasons []string) string {
	return target + " refuses this bridge's settings: " + reasonText(reasons)
}

// reasonText is a refusal's reasons on one line.
func reasonText(reasons []string) string {
	if len(reasons) == 0 {
		return "it gave no reason"
	}
	return strings.Join(reasons, "; ")
}

// recordHold records target as held back for reason: against the digest
// of the settings for a refusal of them, whatever the settings are
// otherwise. A record that cannot be made is said and skipped: the install
// is refused all the same, and the next attempt checks again. The record is
// a read, a change and a write of the marker (recordRejection) holding the
// install marker's lock (backlog B692), taken here and nowhere else on the
// way, so a lock that cannot be held (another holder past the wait, no lock
// to give, a failure, a filesystem that keeps none) records nothing either.
// A stop in its wait, or one before an uncontended take, records nothing and
// is returned, ctx's error, for the install to answer (round 2); it returns
// nothing else.
func recordHold(ctx context.Context, opts InstallOptions, target, reason string, onSettings bool) error {
	digest := ""
	if onSettings {
		d, err := ConfigDigest(opts.ConfigPath)
		if err != nil {
			logger.Warn("could not read the settings to hold the refused version back against; the next check asks again",
				"version", target, "err", err)
			return nil
		}
		digest = d
	}
	lock, err := lockMarkerForUpdate(ctx, opts.DataDir, "the version held back was not recorded")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) {
			return err
		}
		logger.Warn("could not record the version held back; the next check asks again", "version", target, "err", err)
		return nil
	}
	defer lock.Release()
	if err := recordRejection(opts.DataDir, target, reason, digest); err != nil {
		logger.Warn("could not record the version held back; the next check asks again", "version", target, "err", err)
	}
	return nil
}

// swapAwaitingRestart reports whether a swap for `target` has ALREADY
// landed on this host and the process has not yet restarted into it.
//
// Keyed on the PERSISTED marker rather than the in-memory pendingRestart
// flag, because the two callers that need protecting are different
// PROCESSES. `bridge update` is its own process and sees no atomic.Bool
// the serving bridge set; and a marker written before a crash must still
// refuse the next attempt. The in-memory flag stays as the cheap first
// arm in Install — this is the one that is actually load-bearing.
//
// Three terms, all required:
//
//   - Status "installing" AND SwapStarted. Together they mean the
//     destructive step ran, so .bak now holds the binary we were running
//     when the marker was armed. A marker armed but not swapped (the
//     Windows SCM-stop window) must NOT refuse: nothing was mutated, and
//     DecideBootAction reads that state as BootClearNotSwapped.
//
//   - Same target. A NEWER release is a legitimate install: refusing it
//     would strand the host on a version it has not even booted. Only a
//     repeat of the target already on disk is the defect.
//
//   - Within recencyWindow. This is what keeps the refusal from becoming
//     a wedge. Past the window the next boot resolves the marker anyway
//     (DecideBootAction: the target's boot confirms it, at any age since
//     backlog B705, and any other version's clears it as abandoned);
//     refusing on it would block installs on a host that never
//     restarts, with no way out. The two bounds are the same constant ON
//     PURPOSE — the refusal expires exactly when a boot stops putting
//     .bak back, so the engine and the boot path cannot disagree about
//     whether a marker still guards a restore.
//
// A marker that cannot be read is NOT a refusal: LoadState already
// normalises missing and malformed to the zero State, and failing closed
// here would make an unreadable file block every install on the host.
// The guard exists to stop a SECOND install eating .bak, and the cost of
// missing one is the bug we already had — not a new one.
func (u *Updater) swapAwaitingRestart(dataDir, target string) bool {
	if dataDir == "" || target == "" {
		return false
	}
	st, err := LoadState(dataDir)
	if err != nil {
		return false
	}
	if st.Status != "installing" || !st.SwapStarted {
		return false
	}
	if normalizeTag(st.TargetVersion) != normalizeTag(target) {
		return false
	}
	// OR the in-process flag: recencyWindow is six hours and a manual
	// restart can take longer. Past it the marker reads abandoned — which
	// is the right answer for a BOOT deciding whether to roll back, and
	// the wrong one here, because the binary on disk really is still the
	// target and .bak really is still the rollback copy. Within this
	// process we know that directly (CodeRabbit on #977).
	//
	// Safe only BELOW the target comparison above: swapPending records
	// that a swap happened, not which version, so consulted earlier it
	// would refuse a genuinely newer release.
	return u.swapPending.Load() || u.now().Sub(st.AttemptedAt) <= recencyWindow
}

// decideUnderLock makes Install's decisions again on the marker in
// opts.DataDir as it is once the install marker's lock is held (backlog
// B692): Install read it before its download, and another process may have
// changed it since, since `bridge update` and serve's own install share no
// installInFlight. A swap of target waiting for its restart refuses
// (swapAwaitingRestart), whichever process made it, so the second install
// stops before it eats the first one's backup; and for the auto-installer, a
// hold of target recorded since its gate read the marker refuses too
// (HeldBack), as the gate would have, and so does the running install's
// probation (ProbationHolds; backlog B660): the swap would replace the backup
// a crash rollback puts back with the unconfirmed running version. A manual
// install is not held by it. The lock is the caller's.
func (u *Updater) decideUnderLock(opts InstallOptions, target string) error {
	if u.swapAwaitingRestart(opts.DataDir, target) {
		return fmt.Errorf("%w: %s is already staged on disk", ErrInstallPendingRestart, target)
	}
	if opts.Automatic {
		if hold := HeldBack(opts.DataDir, opts.ConfigPath, target); hold.Version != "" {
			return &holdError{sentinel: ErrInstallHeldBack, reason: hold.Reason}
		}
		if why := u.probationHolds(opts.DataDir, u.Status().CurrentVersion); why != "" {
			return &holdError{sentinel: ErrInstallAwaitsConfirmation, reason: why}
		}
	}
	return nil
}

// rejectedVersionOf names the release a rollback of the marker st is
// rejecting. The install marker's TargetVersion is authoritative —
// version.ServerVersion is only correct once the operator has already
// restarted onto the new binary, and the admin "Roll back" button is
// reachable before that too. Falls back to the running version when no
// install is recorded (a hand-staged .bak, or a rollback after
// BootCleanupBak retired the marker).
func rejectedVersionOf(st State) string {
	if st.TargetVersion != "" {
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
		// Never sweep the process working directory. os.ReadDir("") is not
		// how that would happen: it fails with ENOENT, so this would return
		// 0 below anyway. A root resolved before the listing would
		// (filepath.Clean("") is ".", filepath.Abs("") the working
		// directory), and this function deletes what it matches, so an
		// unset DataDir is refused before anything can resolve it
		// (TestReapScratchDirsRefusesEmptyRoot).
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
