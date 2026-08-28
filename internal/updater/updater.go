// Package updater polls the GitHub Releases API for newer 1-bit-bridge
// builds and drives the full self-update lifecycle: poll → download →
// SHA-256 verify (always) → platform signature verify (macOS Apple
// codesign + Team-ID; Linux/Windows currently TLS-of-checksums.txt only,
// see verify_other.go for the seam where Sigstore / minisign / SignPath
// would land) → swap (atomic rename on Unix; live rename + caller-driven
// restart on Windows, see swap_windows.go — note this is NOT the
// MoveFileEx/MOVEFILE_DELAY_UNTIL_REBOOT "pending rename" mechanism)
// → arm rollback marker. The caller (admin
// REST endpoint or `bridge update` CLI) is responsible for triggering the
// process restart so a CLI run can print a final status line first.
//
// Concurrency: the poll goroutine is the only writer to the cached Status;
// readers (Status()) take the same mutex. Callers should not block in
// Status() — it returns a copy of the cached value.
//
// Rate-limit budget: GitHub's unauthenticated REST API allows 60 requests
// per hour per source IP. With a 6 h default poll cadence one bridge uses
// 4 hits/day; well under the cap even with many bridges sharing one
// egress IP.
//
// Why the GitHub Releases API and not a separate update server: every
// release goes through goreleaser to GitHub anyway, the artifacts are
// already hosted, the authentication story (TLS to api.github.com,
// SHA-256 in checksums.txt) is already solved, and we don't have a CDN
// pinning problem. Standing up a parallel update server would add code,
// infra, and a second source of truth for "what's the latest version"
// without buying anything users would notice.
package updater

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/mod/semver"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/logging"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

var logger = logging.Component("updater")

// DefaultRepo is the GitHub repo the updater polls for new releases.
// Tests inject an alternate via Options.RepoOverride so a fake server
// can stand in.
const DefaultRepo = "acoseac/1-bit-bridge"

// DefaultCheckInterval is how often the background poller hits the
// GitHub Releases API. 6 h is well under the 60/hr unauthed rate limit
// (4 hits/day) and gives same-day "update available" turnaround
// without burning the budget.
const DefaultCheckInterval = 6 * time.Hour

// minCheckInterval is the floor enforced on operator-supplied poll
// cadences — lower values would push the unauthenticated GitHub API
// budget (60/hr per source IP) into rate-limit territory, especially
// when several bridges share one egress IP. PR #43 review (Gemini)
// flagged the 5 min original as a DoS vector + a doc/impl mismatch
// (PROTOCOL.md and the admin UI both advertise a 1 h floor); raising
// to 1 h aligns the contract end-to-end.
const minCheckInterval = 1 * time.Hour

// autoInstallDeferredMessage is the log message emitted whenever the
// auto-install scheduler steps off the install path (outside window,
// active downloads, compat-gate refusal). Three call sites; one string
// so log-aggregator queries match a single event name.
const autoInstallDeferredMessage = "auto-install deferred"

// activeDownloadsReason is the "reason" attribute value logged whenever
// the auto-installer (or its deferred restart) steps off for in-flight
// downloads — one constant so log-aggregator queries match a single
// value across the gate sites.
const activeDownloadsReason = "active downloads"

// Status is a snapshot of the updater's current view of the world.
// All times are UTC. Returned by Status() as a value so callers don't
// hold the mutex across reads.
type Status struct {
	// CurrentVersion is the bridge's own build version (version.ServerVersion).
	CurrentVersion string

	// LatestVersion is the most recent release tag the poller has seen,
	// stripped of the leading "v". Empty until the first poll succeeds.
	LatestVersion string

	// UpdateAvailable is LatestVersion > CurrentVersion (semver compare).
	// False when LatestVersion is empty or when the bridge is at-or-ahead.
	UpdateAvailable bool

	// ReleaseNotesURL points at the GitHub release page for LatestVersion,
	// or "" when no release has been observed yet.
	ReleaseNotesURL string

	// Channel is the channel the bridge is on (currently always "stable").
	Channel string

	// LastCheck is the wall-clock time of the most recent successful poll.
	// Zero until the first success.
	LastCheck time.Time

	// LastError captures the most recent poll error (rate limit, timeout,
	// JSON parse failure, etc.). Reset to "" on the next success. Useful
	// for surfacing "we tried, here's why nothing happened" in the admin
	// UI without spamming the operator's logs.
	LastError string

	// DeferredReason describes why an auto-install attempt was held
	// off when the gate refused. Empty when the most recent gate
	// decision was either "install proceeded" or "no candidate".
	// Currently the only deferred-with-reason code path is the
	// MinClientVersion compat gate ("would orphan device(s): X");
	// future gates can extend the same field.
	//
	// Surfaced to the admin dashboard as a yellow "held update"
	// card so the operator can see why a release didn't auto-
	// install. Reset to "" on each gate cycle so a one-shot
	// failure doesn't linger after the underlying state changes.
	DeferredReason string
}

// Options configures a new Updater. Zero values pick sane defaults so
// callers can pass Options{} for the production path.
type Options struct {
	// RepoOverride replaces DefaultRepo. Production passes "". Tests pass
	// a fake server's path or an alternate repo for fixture builds.
	RepoOverride string

	// CheckInterval overrides DefaultCheckInterval. Values below
	// minCheckInterval are clamped up; zero picks the default.
	CheckInterval time.Duration

	// Channel overrides version.Channel. "" picks the version package
	// default ("stable").
	Channel string

	// Client overrides the github.Client. Tests inject one pointed at a
	// httptest.Server. Nil picks a default with a 10 s timeout.
	Client *Client

	// AutoInstall enables the poll-loop's automatic install attempt
	// after every successful check. Off by default — production
	// callers wire this from cfg.Update.AutoInstall, which is also
	// off by default. Even when on, the auto-installer honours
	// QuietHoursWindow and the Sessions tracker (active downloads
	// block the install).
	AutoInstall bool

	// QuietHoursWindow restricts auto-install to a daily window in
	// minutes-of-day (server-local time). Zero start AND zero end
	// means "any time" (no restriction). Wrap-around windows (start
	// > end) are supported for overnight schedules. Validate the raw
	// "HH:MM-HH:MM" string at config-load time via
	// config.ParseQuietHours.
	QuietHoursStart int
	QuietHoursEnd   int

	// AutoInstallOpts is the pre-built InstallOptions the auto-
	// installer hands to Install on each cycle. Nil disables the
	// auto-install path entirely (regardless of AutoInstall — a
	// guard so the server can't auto-install if it doesn't know
	// where its own binary is).
	AutoInstallOpts *InstallOptions

	// LiveAutoInstall, LiveQuietHours and LiveCheckInterval override
	// their static twins above when non-nil, re-read at DECISION time
	// rather than captured here. They exist so the three update settings
	// hot-apply: cmd/bridge wires them to closures over the live config
	// holder, and every other caller (tests, the CLI) leaves them nil and
	// keeps the static fields.
	//
	// The distinction that matters for AutoInstall: it gates a background
	// binary swap and a supervised restart, so "applies live" has to be
	// read precisely. maybeAutoInstall runs ONLY from Run's poll loop —
	// the admin "Check now" path deliberately does not call it — and
	// DefaultCheckInterval is 6 h with a 1 h floor. So flipping this on
	// cannot begin anything sooner than the next scheduled poll, and that
	// poll still has to clear the quiet-hours window and the in-flight
	// sessions gate (which is re-checked AFTER the download, since a
	// stream may have started during it). There is no path from "operator
	// ticked the box" to "restart within seconds".
	LiveAutoInstall   func() bool
	LiveQuietHours    func() (start, end int)
	LiveCheckInterval func() time.Duration

	// Rearm wakes Run's wait so LiveCheckInterval is re-read immediately.
	//
	// Without it the new cadence is not consulted until the CURRENT wait
	// expires — up to 6 h on the default — which is indistinguishable
	// from the change being ignored, and would make the settings
	// response's `live` a lie in the one way that matters. Receiving on
	// it never polls: a settings save is not a request to check for
	// updates.
	Rearm <-chan struct{}

	// AutoInstallRestart is invoked after a successful auto-install
	// to trigger the process restart that loads the new binary.
	// Nil disables the auto-install path. cmd/bridge/main.go wires
	// this to os.Exit(0) — same restart contract as the admin
	// console's Restart endpoint.
	AutoInstallRestart func()

	// TokenSnapshot returns the live token list. Used by the
	// MinClientVersion compat gate to decide whether a candidate
	// release would orphan a still-paired older client. nil
	// disables the gate (tests, or pre-Phase-C bridges where
	// auto-install ran without compat checks). Production wires
	// this to `(*auth.Store).List`.
	TokenSnapshot func() []auth.Token

	// Now overrides time.Now for the quiet-hours check. Tests
	// inject a fixed clock. Nil = real clock.
	Now func() time.Time
}

// Updater owns the cached status and the polling goroutine.
type Updater struct {
	repo     string
	interval time.Duration
	channel  string
	client   *Client

	autoInstall        bool
	quietHoursStart    int
	quietHoursEnd      int
	liveAutoInstall    func() bool
	liveQuietHours     func() (int, int)
	liveCheckInterval  func() time.Duration
	rearm              <-chan struct{}
	autoInstallOpts    *InstallOptions
	autoInstallRestart func()
	tokenSnapshot      func() []auth.Token
	now                func() time.Time

	mu     sync.RWMutex
	status Status

	// installInFlight is the try-lock serializing Install across every
	// in-process caller (the auto-installer poll and the admin POST
	// share one Updater). Held for the WHOLE attempt: a second caller
	// fails fast with ErrInstallInFlight instead of racing the first
	// on the .bak rename target and update-state.json. The `bridge
	// update` CLI is a separate process and can't share this lock —
	// the per-attempt scratch dir is what keeps its cleanup from
	// deleting an in-flight install's files.
	installInFlight atomic.Bool

	// pendingRestart marks "auto-install swapped the binary, restart
	// deferred for active downloads". While set, maybeAutoInstall
	// skips the whole install (a ~30 MiB re-download + verify per
	// poll cycle) and only waits for sessions to drain before firing
	// the restart. In-memory only: a manual admin/CLI install must
	// NOT trigger the auto-installer's restart, so this can't be
	// derived from update-state.json (which any successful Install
	// arms).
	pendingRestart atomic.Bool
}

// New builds an Updater. The poller is not started — call Run on it
// from the serveCmd goroutine that owns scanCtx.
func New(opts Options) *Updater {
	repo := opts.RepoOverride
	if repo == "" {
		repo = DefaultRepo
	}
	interval := opts.CheckInterval
	if interval <= 0 {
		interval = DefaultCheckInterval
	}
	if interval < minCheckInterval {
		interval = minCheckInterval
	}
	channel := opts.Channel
	if channel == "" {
		channel = version.Channel
	}
	client := opts.Client
	if client == nil {
		client = NewClient(repo, 10*time.Second)
	} else {
		// Honour caller-injected client, but make sure its repo matches
		// the override so RepoOverride alone is enough to redirect.
		client.repo = repo
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Updater{
		repo:               repo,
		interval:           interval,
		channel:            channel,
		client:             client,
		autoInstall:        opts.AutoInstall,
		quietHoursStart:    opts.QuietHoursStart,
		quietHoursEnd:      opts.QuietHoursEnd,
		liveAutoInstall:    opts.LiveAutoInstall,
		liveQuietHours:     opts.LiveQuietHours,
		liveCheckInterval:  opts.LiveCheckInterval,
		rearm:              opts.Rearm,
		autoInstallOpts:    opts.AutoInstallOpts,
		autoInstallRestart: opts.AutoInstallRestart,
		tokenSnapshot:      opts.TokenSnapshot,
		now:                now,
		status: Status{
			CurrentVersion: version.ServerVersion,
			Channel:        channel,
		},
	}
}

// Run blocks until ctx is cancelled, polling once on entry then once
// per interval. Errors are stored in Status.LastError and logged at
// info level (not stderr-fatal — a transient GitHub outage shouldn't
// noise the operator's terminal).
//
// When auto-install is configured (AutoInstall=true + InstallOptions
// + RestartCallback all wired in Options), every successful poll
// that surfaces an update is followed by `maybeAutoInstall`, which
// honours the quiet-hours window and the sessions tracker. A
// successful auto-install ends with the restart callback firing,
// which terminates the process; service-manager respawn loads the
// new binary. The Phase B `maybeRollbackOnBoot` housekeeping then
// confirms (or rolls back) on the next start.
//
// Designed to be invoked as `go updater.Run(scanCtx)` from serveCmd
// alongside the periodic scanner — same lifetime, same cancellation
// semantics.
func (u *Updater) Run(ctx context.Context) {
	// Initial check on startup so the admin UI has something to show
	// before the first interval elapses. Best-effort; errors are
	// captured in Status.LastError.
	//
	// maybeAutoInstall ONLY runs after a successful checkOnce — a
	// failed poll (GitHub down, rate-limited, transient network
	// blip) leaves the previous poll's cached UpdateAvailable=true
	// untouched, so an unconditional call would fire auto-install
	// off stale state. Caught in PR #43 review (CodeRabbit).
	if u.checkOnce(ctx) {
		u.maybeAutoInstall(ctx)
	}

	// A fresh timer per iteration rather than one ticker: a ticker cannot
	// change period, and re-reading the cadence is what makes
	// update.checkIntervalHours hot.
	for {
		t := time.NewTimer(u.checkInterval())
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
			if u.checkOnce(ctx) {
				u.maybeAutoInstall(ctx)
			}
		case <-u.rearm:
			// Cadence changed; re-read it on the next iteration.
			// Deliberately no poll — see Options.Rearm.
			t.Stop()
		}
	}
}

// autoInstallEnabled resolves the auto-install gate, preferring the live
// provider. Read at the top of every poll cycle, so an operator's flip
// binds on the next one without a restart.
func (u *Updater) autoInstallEnabled() bool {
	if u.liveAutoInstall != nil {
		return u.liveAutoInstall()
	}
	return u.autoInstall
}

// quietHours resolves the auto-install window, preferring the live
// provider. Read at the gate rather than at construction.
func (u *Updater) quietHours() (int, int) {
	if u.liveQuietHours != nil {
		return u.liveQuietHours()
	}
	return u.quietHoursStart, u.quietHoursEnd
}

// checkInterval resolves the poll cadence, preferring the live provider
// and applying the same floor/default clamps New() applies to the static
// value — the clamps are the contract, not an artifact of construction,
// so a live value that skipped them could poll GitHub every second.
func (u *Updater) checkInterval() time.Duration {
	d := u.interval
	if u.liveCheckInterval != nil {
		d = u.liveCheckInterval()
	}
	if d <= 0 {
		d = DefaultCheckInterval
	}
	if d < minCheckInterval {
		d = minCheckInterval
	}
	return d
}

// maybeAutoInstall is the auto-install entry point. Returns
// silently when auto-install is disabled, no update is available,
// quiet-hours forbids it, sessions are inflight, or the
// install/restart wiring is missing. On success the restart
// callback is invoked — the process exits and service-manager
// respawns into the new binary. When a previous cycle installed
// but deferred the restart (pendingRestart), the install is NOT
// re-run — only the sessions gate is re-checked.
//
// All conditional gates log at info level (so the operator can
// audit "why didn't auto-install fire") but never escalate to
// stderr; a deferred install is a normal state.
func (u *Updater) maybeAutoInstall(ctx context.Context) {
	if !u.autoInstallEnabled() {
		return
	}
	// Reset DeferredReason at the start of each cycle so a stale
	// "would orphan device X" from the previous attempt doesn't
	// linger after X updates or disconnects. The Install path
	// re-sets it on this cycle's gate decision if applicable.
	u.clearDeferredReason()
	if u.autoInstallOpts == nil || u.autoInstallRestart == nil {
		// Defensive: the configuration says auto-install is on but
		// the wiring is incomplete. Likely a programming error in
		// cmd/bridge — log loudly so it's visible.
		logger.Warn("autoInstall=true but install opts/restart callback are missing; skipping")
		return
	}
	st := u.Status()
	if !st.UpdateAvailable || st.LatestVersion == "" {
		return
	}
	// Operator-rollback gate, ahead of every other gate INCLUDING the
	// pending-restart fast path: a rejected release must not be
	// re-installed, and a restart still pending for one must not fire.
	if rejected := u.rejectedCandidate(st.LatestVersion); rejected != "" {
		reason := fmt.Sprintf("%s was rolled back on this host; auto-install resumes at the next newer release", rejected)
		u.recordDeferredReason(reason)
		logger.Info(autoInstallDeferredMessage, "reason", "operator rolled this release back", "version", rejected)
		return
	}
	if !u.inAllowedWindow(u.now()) {
		logger.Info(autoInstallDeferredMessage, "reason", "outside quiet-hours window")
		return
	}
	// Pending-restart fast path: a previous cycle already installed
	// the candidate but deferred the restart for active downloads.
	// Don't re-run the whole install (a ~30 MiB re-download + extract
	// + verify per poll cycle) — just wait for sessions to drain,
	// then restart into the binary already on disk.
	if u.pendingRestart.Load() {
		u.restartWhenDrained()
		return
	}
	// Sessions inflight gate: refuse cleanly. The next poll cycle
	// will try again.
	if n := u.inflightSessions(); n > 0 {
		logger.Info(autoInstallDeferredMessage, "reason", activeDownloadsReason, "inflight", n)
		return
	}

	logger.Info("auto-installing", "from", st.CurrentVersion, "to", st.LatestVersion)
	if _, err := u.Install(ctx, *u.autoInstallOpts); err != nil {
		// Compat-gate refusals are a normal deferred state; the
		// Install path has already populated DeferredReason. Other
		// failures (download, verify, swap) are operator-actionable
		// and stay in the log without polluting the dashboard's
		// held-update card.
		if errors.Is(err, ErrCompatGateRefused) {
			logger.Info(autoInstallDeferredMessage, "err", err)
		} else {
			logger.Error("auto-install failed", "err", err)
		}
		return
	}
	// The binary on disk is now the candidate. Mark pending-restart
	// BEFORE restartWhenDrained so a deferred restart skips the
	// re-install on later poll cycles (see pendingRestart).
	u.pendingRestart.Store(true)
	// restartWhenDrained re-checks the sessions gate AFTER the
	// install: the download phase can run for many minutes and a
	// stream may have started in the meantime (Install itself only
	// gates at entry, and the auto-installer's opts carry
	// Force=false). Restarting now would kill it — defer to the next
	// poll cycle, which takes the pending-restart fast path above.
	u.restartWhenDrained()
}

// inflightSessions reports the auto-installer session tracker's
// inflight-download count (0 when no tracker is wired). Read ONCE per
// gate so the condition and the log line can't observe different
// values.
func (u *Updater) inflightSessions() int64 {
	if u.autoInstallOpts.Sessions == nil {
		return 0
	}
	return u.autoInstallOpts.Sessions.Inflight()
}

// rejectedCandidate reports the rolled-back release blocking
// `candidate`, or "" when nothing blocks it. The rejection lives in
// update-state.json (State.RejectedVersion) because it has to survive
// the restart the rollback needs to take effect.
//
// Read at the gate rather than cached on the Updater: the gate runs at
// most once per poll interval (6 h by default), so the cost is one tiny
// file read, and there is no cache to go stale against a rollback
// performed by the `bridge update` CLI in another process.
func (u *Updater) rejectedCandidate(candidate string) string {
	if u.autoInstallOpts == nil || u.autoInstallOpts.DataDir == "" {
		return ""
	}
	st, err := LoadState(u.autoInstallOpts.DataDir)
	if err != nil || st.RejectedVersion == "" {
		return ""
	}
	if !rejectionBlocks(candidate, st.RejectedVersion) {
		return ""
	}
	return st.RejectedVersion
}

// rejectionBlocks reports whether an operator rollback of `rejected`
// should hold `candidate` back. Only a STRICTLY NEWER candidate gets
// through — the operator rejected one build, not every future one, and
// a bare string equality would leave the auto-installer re-installing
// vN+1 forever the moment vN+2 published nothing.
//
// Exact-string equality is checked first so the block holds even for a
// rejected version semver can't parse (semverGreater deliberately treats
// an unparseable "current" as v0.0.0, which would otherwise let
// everything through).
func rejectionBlocks(candidate, rejected string) bool {
	if rejected == "" {
		return false
	}
	c, r := normalizeTag(candidate), normalizeTag(rejected)
	if c == r {
		return true
	}
	return !semverGreater(c, r)
}

// restartWhenDrained fires the auto-install restart, or — when
// downloads are inflight — logs and defers to the next poll cycle.
// Shared by the pending-restart fast path and the tail of a fresh
// install; the caller guarantees an install candidate is on disk.
func (u *Updater) restartWhenDrained() {
	if n := u.inflightSessions(); n > 0 {
		logger.Info("auto-install restart deferred", "reason", activeDownloadsReason, "inflight", n)
		return
	}
	u.refreshInstallAttemptedAt()
	logger.Info("auto-install complete; restarting to load new binary")
	u.autoInstallRestart()
}

// refreshInstallAttemptedAt re-stamps the pending install marker so the
// boot-time recency window is measured from the RESTART rather than from
// the swap.
//
// A restart deferred for an in-flight download waits a full poll
// interval, and DefaultCheckInterval (6 h) EQUALS recencyWindow — with
// quiet hours configured the next in-window tick can be a day later. The
// marker would then read as abandoned on the very boot it was armed
// for: BootClearAbandoned only clears it, so InstalledAt is never
// stamped, BootCleanupBak never fires, and a ~30 MiB bridge.bak is never
// reclaimed (the install also silently loses its boot-time rollback
// protection).
//
// Best-effort and narrow: only an "installing" marker is touched, and a
// failure just leaves the pre-existing behaviour.
func (u *Updater) refreshInstallAttemptedAt() {
	if u.autoInstallOpts == nil || u.autoInstallOpts.DataDir == "" {
		return
	}
	dir := u.autoInstallOpts.DataDir
	st, err := LoadState(dir)
	if err != nil || st.Status != "installing" {
		return
	}
	st.AttemptedAt = time.Now().UTC()
	if err := SaveState(dir, st); err != nil {
		logger.Warn("could not refresh the install marker before restarting; boot-time confirmation may treat it as abandoned",
			"err", err)
	}
}

// recordDeferredReason persists a single-line explanation of why
// the most-recent install attempt didn't proceed. Surfaced to the
// admin dashboard via Status.DeferredReason. Reset on every gate
// cycle (clearDeferredReason) so a stale "would orphan iPhone X"
// doesn't linger after the device updates.
func (u *Updater) recordDeferredReason(reason string) {
	u.mu.Lock()
	u.status.DeferredReason = reason
	u.mu.Unlock()
}

// clearDeferredReason wipes any prior deferred-install reason.
// Called when the gate decision changes (install succeeds, no
// candidate, etc.) so the dashboard stops showing stale state.
func (u *Updater) clearDeferredReason() {
	u.mu.Lock()
	u.status.DeferredReason = ""
	u.mu.Unlock()
}

// compatGateReason returns a non-empty string when at least one
// paired token's `LastClientVersion` is strictly below
// `minRequired`. The string is the user-visible "would orphan"
// explanation suitable for admin display + log line. Tokens with
// an empty `LastClientVersion` (older iOS builds that don't send
// X-Client-Version) are skipped — we have no signal to compare
// against, and refusing every install on their behalf would mean
// the gate never opens until they update.
//
// Empty `minRequired` (or "0.0.0") short-circuits with no reason —
// no floor means no orphaning is possible.
//
// Exported for tests; production callers go through Install.
func compatGateReason(minRequired string, tokens []auth.Token) string {
	clean := strings.TrimSpace(minRequired)
	if clean == "" || clean == "0.0.0" {
		return ""
	}
	required := normalizeForSemver(clean)
	if !semver.IsValid(required) {
		// Treat malformed floors as "no floor" rather than
		// blocking every install — the operator can spot the
		// release-meta.json bug from the bridge log without
		// users noticing.
		logger.Warn("compat-gate ignoring malformed MinClientVersion", "value", clean)
		return ""
	}
	var orphans []string
	for _, t := range tokens {
		raw := strings.TrimSpace(t.LastClientVersion)
		if raw == "" {
			continue
		}
		live := normalizeForSemver(raw)
		if !semver.IsValid(live) {
			continue
		}
		if semver.Compare(live, required) < 0 {
			orphans = append(orphans, fmt.Sprintf("%q on %s", t.Name, raw))
		}
	}
	if len(orphans) == 0 {
		return ""
	}
	return fmt.Sprintf("would orphan device(s): %s (requires %s+)",
		strings.Join(orphans, ", "), clean)
}

// normalizeForSemver makes "1.2.3" parseable by golang.org/x/mod/
// semver, which expects a leading lowercase "v". Tolerates an
// existing leading "v" / "V" by normalizing — `semver.IsValid` is
// case-sensitive on the prefix, so "V1.0.0" would otherwise be
// rejected as malformed and the gate would silently skip the
// token (Gemini flagged on PR #47).
func normalizeForSemver(v string) string {
	if len(v) > 0 && (v[0] == 'v' || v[0] == 'V') {
		return "v" + v[1:]
	}
	return "v" + v
}

// inAllowedWindow returns true when the auto-installer is allowed
// to fire at wall-clock `t`. The "quiet hours" config field names
// the period the bridge is *quiet enough* to absorb a restart —
// auto-install runs INSIDE the window, defers OUTSIDE.
//
// Default (start == end) means "any time" — no window restriction.
// Under that semantics, an unset config implicitly allows the
// auto-installer at every poll cycle, matching the principle that
// a missing config field shouldn't surprise-disable behaviour the
// operator explicitly opted into via AutoInstall=true.
func (u *Updater) inAllowedWindow(t time.Time) bool {
	// start == end means "any time" (no window restriction). This covers
	// BOTH the unset-config (0,0) case AND a degenerate operator window
	// like "02:00-02:00" (start == end == 120), which config.ParseQuietHours
	// accepts. Handling it HERE — the updater's policy layer — rather than
	// in inWindow keeps inWindow byte-for-byte in lockstep with
	// config.IsInQuietHours, which deliberately treats a zero-length window
	// as "always outside" (false). The updater's policy is the opposite
	// ("any time"), so the two diverge only at this layer; pre-fix the
	// short-circuit fired only for (0,0), so a non-zero equal window fell
	// through to inWindow(120,120,…) → false and auto-install fired NEVER.
	start, end := u.quietHours()
	if start == end {
		return true
	}
	mod := t.Hour()*60 + t.Minute()
	return inWindow(start, end, mod)
}

// inWindow mirrors config.IsInQuietHours but lives here so the
// updater package doesn't import the config package (one-way
// dependency: cmd/bridge wires config → updater Options, never the
// other direction). The two functions are tested against each
// other to stay in lockstep.
func inWindow(startMin, endMin, now int) bool {
	if startMin == endMin {
		return false
	}
	if startMin < endMin {
		return now >= startMin && now <= endMin
	}
	return now >= startMin || now <= endMin
}

// CheckNow forces a poll outside the regular schedule. Used by the
// admin "Check now" button. Returns the post-check status — same as
// the next Status() call would return. Discards the success bool
// because the admin UI reads success/failure from the returned
// Status.LastError field.
func (u *Updater) CheckNow(ctx context.Context) Status {
	_ = u.checkOnce(ctx)
	return u.Status()
}

// Status returns a copy of the cached status. Cheap; safe to call
// from request handlers.
func (u *Updater) Status() Status {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.status
}

// checkOnce hits the GitHub Releases API and updates the cached
// status. Splits into a separate method so Run / CheckNow share one
// definition of "what does a poll do".
//
// Returns true on a successful poll, false otherwise. Run uses the
// bool to gate maybeAutoInstall — a failed poll leaves the cached
// UpdateAvailable from a prior successful poll untouched, and we
// must not auto-install off that stale state.
func (u *Updater) checkOnce(ctx context.Context) bool {
	rel, err := u.client.LatestRelease(ctx)
	now := time.Now().UTC()

	u.mu.Lock()
	defer u.mu.Unlock()

	if err != nil {
		u.status.LastError = err.Error()
		// Don't reset LastCheck — a transient failure shouldn't make the
		// admin UI claim "haven't checked in days". Operators reading
		// the UI care about the last *successful* poll.
		//
		// "Definitely no candidate" sentinels (`ErrNoReleasesPublished`)
		// must also CLEAR the cached version-availability fields. The
		// dashboard template branches on `UpdateAvailable` first, then
		// `LatestVersion`, then `LastError` — a stale "update available"
		// state from a previous successful poll would mask the
		// "check failed" badge once the repo is wiped or made private.
		// Transient errors (rate limit, network blip) leave the cached
		// state alone so the operator still sees the last good answer
		// while the bridge retries (Qodo bot review on PR #89).
		if errors.Is(err, ErrNoReleasesPublished) {
			u.status.LatestVersion = ""
			u.status.UpdateAvailable = false
			u.status.ReleaseNotesURL = ""
		}
		logger.Error("poll", "repo", u.repo, "err", err)
		return false
	}

	latest := normalizeTag(rel.TagName)
	current := normalizeTag(u.status.CurrentVersion)
	avail := semverGreater(latest, current)

	u.status.LatestVersion = latest
	u.status.UpdateAvailable = avail
	u.status.ReleaseNotesURL = rel.HTMLURL
	u.status.LastCheck = now
	u.status.LastError = ""
	return true
}

// normalizeTag strips a leading "v" from a release tag so semver
// compares against the bare numeric form. GitHub release tags
// conventionally carry the "v" prefix; version.ServerVersion does not.
func normalizeTag(s string) string {
	return strings.TrimPrefix(strings.TrimSpace(s), "v")
}

// semverGreater returns true if latest > current under semver ordering.
// Both inputs are bare (no "v" prefix); we re-add the "v" because
// golang.org/x/mod/semver insists on it. Invalid `latest` returns false
// — a malformed remote tag isn't comparable.
//
// **Invalid `current` is treated as the floor (`v0.0.0`)** so that
// non-semver dev/CI builds — `make build` artefacts stamped with a
// bare git short-SHA on a tagless or shallow clone, or the `dev`
// fallback — still surface a real release as an available update. The
// previous "any invalid → false" rule meant operators running such a
// build couldn't see updates at all (Qodo bot review on PR #89).
func semverGreater(latest, current string) bool {
	lv := "v" + latest
	if !semver.IsValid(lv) {
		return false
	}
	cv := "v" + current
	if !semver.IsValid(cv) {
		// Treat a non-semver current as the lowest possible version so
		// any valid release is reported as an upgrade candidate. The
		// admin UI then renders "update available" with the real
		// `latest` tag — operators see a route forward without having
		// to first rebuild against a tagged ref.
		cv = "v0.0.0"
	}
	return semver.Compare(lv, cv) > 0
}
