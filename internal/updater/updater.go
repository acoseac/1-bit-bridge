// Package updater polls the GitHub Releases API for newer 1-bit-bridge
// builds and drives the full self-update lifecycle: poll → download →
// SHA-256 verify (always) → platform signature verify (macOS Apple
// codesign + Team-ID; Linux/Windows currently TLS-of-checksums.txt only,
// see verify_other.go for the seam where Sigstore / minisign / SignPath
// would land) → swap (atomic rename on Unix; pending-rename + restart on
// Windows, see swap_windows.go) → arm rollback marker. The caller (admin
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
	autoInstallOpts    *InstallOptions
	autoInstallRestart func()
	tokenSnapshot      func() []auth.Token
	now                func() time.Time

	mu     sync.RWMutex
	status Status
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

	t := time.NewTicker(u.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if u.checkOnce(ctx) {
				u.maybeAutoInstall(ctx)
			}
		}
	}
}

// maybeAutoInstall is the auto-install entry point. Returns
// silently when auto-install is disabled, no update is available,
// quiet-hours forbids it, sessions are inflight, or the
// install/restart wiring is missing. On success the restart
// callback is invoked — the process exits and service-manager
// respawns into the new binary.
//
// All conditional gates log at info level (so the operator can
// audit "why didn't auto-install fire") but never escalate to
// stderr; a deferred install is a normal state.
func (u *Updater) maybeAutoInstall(ctx context.Context) {
	if !u.autoInstall {
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
	if !u.inAllowedWindow(u.now()) {
		logger.Info("auto-install deferred", "reason", "outside quiet-hours window")
		return
	}
	// Sessions inflight gate: refuse cleanly. The next poll cycle
	// will try again.
	if u.autoInstallOpts.Sessions != nil && u.autoInstallOpts.Sessions.Inflight() > 0 {
		logger.Info("auto-install deferred", "reason", "active downloads", "inflight", u.autoInstallOpts.Sessions.Inflight())
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
			logger.Info("auto-install deferred", "err", err)
		} else {
			logger.Error("auto-install failed", "err", err)
		}
		return
	}
	logger.Info("auto-install complete; restarting to load new binary")
	u.autoInstallRestart()
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
	if u.quietHoursStart == 0 && u.quietHoursEnd == 0 {
		return true
	}
	mod := t.Hour()*60 + t.Minute()
	return inWindow(u.quietHoursStart, u.quietHoursEnd, mod)
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
// golang.org/x/mod/semver insists on it. Invalid input on either side
// returns false — we'd rather miss an upgrade prompt than show a wrong
// one (operator can hit "Check now" and the next valid response wins).
func semverGreater(latest, current string) bool {
	lv := "v" + latest
	cv := "v" + current
	if !semver.IsValid(lv) || !semver.IsValid(cv) {
		return false
	}
	return semver.Compare(lv, cv) > 0
}
