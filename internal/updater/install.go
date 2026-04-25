package updater

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/version"
)

// InstallOptions configures one Install attempt.
type InstallOptions struct {
	// DataDir is the bridge's working data directory; the install
	// path uses <DataDir>/updates/ as its scratch area and writes
	// update-state.json here for boot-time rollback bookkeeping.
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
//   - ErrInstallNotSupported on Windows (Phase B; Windows install
//     is a follow-up).
//   - Any download / verify / swap error returned verbatim.
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
	if !opts.Force && opts.Sessions != nil && opts.Sessions.Inflight() > 0 {
		return status, fmt.Errorf("%w: %d inflight download(s)",
			ErrActiveSessions, opts.Sessions.Inflight())
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

	// Scratch dir under dataDir keeps temp files inside the
	// bridge's writable area (avoids /tmp permissions surprises on
	// macOS) and makes cleanup obvious.
	scratch := filepath.Join(opts.DataDir, "updates")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return status, fmt.Errorf("create scratch: %w", err)
	}
	defer cleanScratch(scratch)

	archivePath := filepath.Join(scratch, archive.Name)
	if _, err := downloadVerified(ctx, u.client.http,
		archive.BrowserDownloadURL, archive.Name,
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
	if err := verifier(ctx, extracted); err != nil {
		return status, fmt.Errorf("verify: %w", err)
	}

	// Permission preflight: if we can't write to the binary's
	// directory, the swap will fail with a less clear error mid-
	// flight. Catch it here so the operator gets a usable message.
	if err := preflightWritable(opts.BinaryPath); err != nil {
		return status, err
	}

	// Marker first (see method-doc rationale). This is the boot
	// contract: Status="installing" + TargetVersion → next boot
	// expects to be running TargetVersion or rolls back.
	state := State{
		Status:          "installing",
		TargetVersion:   status.LatestVersion,
		PreviousVersion: version.ServerVersion,
		AttemptedAt:     time.Now().UTC(),
	}
	if err := SaveState(opts.DataDir, state); err != nil {
		return status, fmt.Errorf("save state: %w", err)
	}

	if err := swapBinary(opts.BinaryPath, extracted, ".bak"); err != nil {
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
// (no-update / active-sessions / unsupported-platform) from
// "something went wrong".
var (
	ErrNoUpdate            = errors.New("no update available")
	ErrActiveSessions      = errors.New("active downloads — refuse to restart")
	ErrPathNotWritable     = errors.New("binary path not writable by this user (try sudo bridge update)")
	ErrInstallNotSupported = errors.New("self-install not yet supported on this platform; download manually and replace the binary (see PROTOCOL.md → Updates)")
)

// Rollback restores bridge.bak over the live binary. Used by the
// admin "Roll back" button when the operator notices the new
// version is broken AFTER a successful install (the boot-time
// rollback only fires on truly unbootable builds).
func (u *Updater) Rollback(opts InstallOptions) error {
	if isWindows() {
		return ErrInstallNotSupported
	}
	if !opts.Force && opts.Sessions != nil && opts.Sessions.Inflight() > 0 {
		return fmt.Errorf("%w: %d inflight download(s)",
			ErrActiveSessions, opts.Sessions.Inflight())
	}
	if err := RollbackBinary(opts.BinaryPath, ".bak"); err != nil {
		return err
	}
	_ = ClearState(opts.DataDir)
	return nil
}

// preflightWritable checks the directory the binary lives in is
// writable by this process. Better-message-than-permission-denied:
// we ask the operator to run sudo / re-install in user mode, with a
// concrete remediation path.
func preflightWritable(binaryPath string) error {
	dir := filepath.Dir(binaryPath)
	tmp, err := os.CreateTemp(dir, ".bridge-write-test-*")
	if err != nil {
		return fmt.Errorf("%w (path=%s, err=%v)", ErrPathNotWritable, dir, err)
	}
	name := tmp.Name()
	tmp.Close()
	_ = os.Remove(name)
	return nil
}

// cleanScratch wipes the updates/ scratch dir between attempts so
// disk doesn't accumulate failed-download leftovers.
func cleanScratch(scratch string) {
	_ = os.RemoveAll(scratch)
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

// httpClient returns the underlying http.Client used by the github
// releases client — install reuses it so TLS pooling / proxies /
// timeouts are consistent with the poll path.
//
// Defined here (not in github.go) so tests in this package can build
// a Client manually and still drive Install through it.
func (c *Client) httpClient() *http.Client { return c.http }
