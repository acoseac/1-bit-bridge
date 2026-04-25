// Package updater polls the GitHub Releases API for newer 1-bit-bridge
// builds and exposes the result to the api + admin packages. Phase A
// is poll-only: it advertises "X.Y.Z is available" but does not download
// or install anything. Phase B will wire the actual swap-and-restart path
// onto this same Updater type.
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
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/semver"

	"github.com/acoseac/1-bit-bridge/internal/version"
)

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
// cadences — lower values would be wasteful and could push the
// unauthed-IP budget into rate-limit territory if many bridges share
// one egress IP.
const minCheckInterval = 5 * time.Minute

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
}

// Updater owns the cached status and the polling goroutine.
type Updater struct {
	repo     string
	interval time.Duration
	channel  string
	client   *Client

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
	return &Updater{
		repo:     repo,
		interval: interval,
		channel:  channel,
		client:   client,
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
// Designed to be invoked as `go updater.Run(scanCtx)` from serveCmd
// alongside the periodic scanner — same lifetime, same cancellation
// semantics.
func (u *Updater) Run(ctx context.Context) {
	// Initial check on startup so the admin UI has something to show
	// before the first interval elapses. Best-effort; errors are
	// captured in Status.LastError.
	u.checkOnce(ctx)

	t := time.NewTicker(u.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			u.checkOnce(ctx)
		}
	}
}

// CheckNow forces a poll outside the regular schedule. Used by the
// admin "Check now" button. Returns the post-check status — same as
// the next Status() call would return.
func (u *Updater) CheckNow(ctx context.Context) Status {
	u.checkOnce(ctx)
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
func (u *Updater) checkOnce(ctx context.Context) {
	rel, err := u.client.LatestRelease(ctx)
	now := time.Now().UTC()

	u.mu.Lock()
	defer u.mu.Unlock()

	if err != nil {
		u.status.LastError = err.Error()
		// Don't reset LastCheck — a transient failure shouldn't make the
		// admin UI claim "haven't checked in days". Operators reading
		// the UI care about the last *successful* poll.
		log.Printf("updater: poll %s: %v", u.repo, err)
		return
	}

	latest := normalizeTag(rel.TagName)
	current := normalizeTag(u.status.CurrentVersion)
	avail := semverGreater(latest, current)

	u.status.LatestVersion = latest
	u.status.UpdateAvailable = avail
	u.status.ReleaseNotesURL = rel.HTMLURL
	u.status.LastCheck = now
	u.status.LastError = ""
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

// describeStatusForLog returns a short human-readable form of the
// current status for log lines. Not used by Status() callers.
func (u *Updater) describeStatusForLog() string {
	s := u.Status()
	if s.LatestVersion == "" {
		return fmt.Sprintf("repo=%s no-data", u.repo)
	}
	if s.UpdateAvailable {
		return fmt.Sprintf("repo=%s current=%s latest=%s update-available", u.repo, s.CurrentVersion, s.LatestVersion)
	}
	return fmt.Sprintf("repo=%s current=%s latest=%s up-to-date", u.repo, s.CurrentVersion, s.LatestVersion)
}
