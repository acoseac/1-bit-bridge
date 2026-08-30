package updater

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/version"
	"golang.org/x/mod/semver"
)

func TestSemverGreater(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"0.1.0", "0.0.1", true},
		{"0.0.2", "0.0.1", true},
		{"1.0.0", "0.9.99", true},
		{"0.0.1", "0.0.1", false},
		{"0.0.1", "0.0.2", false},
		{"", "0.0.1", false},    // invalid → false
		{"abc", "0.0.1", false}, // invalid → false
	}
	for _, c := range cases {
		if got := semverGreater(c.latest, c.current); got != c.want {
			t.Errorf("semverGreater(%q, %q) = %v, want %v",
				c.latest, c.current, got, c.want)
		}
	}
}

func TestNormalizeTag(t *testing.T) {
	cases := map[string]string{
		"v1.2.3":   "1.2.3",
		"1.2.3":    "1.2.3",
		" v1.2.3 ": "1.2.3",
		"":         "",
	}
	for in, want := range cases {
		if got := normalizeTag(in); got != want {
			t.Errorf("normalizeTag(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeReleasesServer stands in for api.github.com. Tests hand it a
// release shape and (optionally) a status code; it serves the same
// response on every /releases/latest hit.
func fakeReleasesServer(t *testing.T, status int, rel *Release) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	hits := &atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		// Sanity-check the headers our client sends.
		if ua := r.Header.Get("User-Agent"); !strings.HasPrefix(ua, "1-bit-bridge-updater/") {
			t.Errorf("User-Agent = %q, want 1-bit-bridge-updater/...", ua)
		}
		if a := r.Header.Get("Accept"); a != "application/vnd.github+json" {
			t.Errorf("Accept = %q, want application/vnd.github+json", a)
		}
		if !strings.HasSuffix(r.URL.Path, "/releases/latest") {
			t.Errorf("path = %q, want suffix /releases/latest", r.URL.Path)
		}
		w.WriteHeader(status)
		if rel != nil {
			_ = json.NewEncoder(w).Encode(rel)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, hits
}

func TestUpdaterDetectsNewRelease(t *testing.T) {
	current := version.ServerVersion // typically "0.0.1" in dev
	srv, _ := fakeReleasesServer(t, 200, &Release{
		TagName: "v9.9.9",
		HTMLURL: "https://example.test/releases/v9.9.9",
	})
	u := New(Options{
		RepoOverride: "fake/repo",
		Client:       NewClient("fake/repo", time.Second).WithBaseURL(srv.URL),
	})
	got := u.CheckNow(context.Background())
	if got.LatestVersion != "9.9.9" {
		t.Errorf("LatestVersion = %q, want 9.9.9", got.LatestVersion)
	}
	if !got.UpdateAvailable {
		t.Errorf("UpdateAvailable = false, want true (current %q vs latest 9.9.9)", current)
	}
	if got.ReleaseNotesURL != "https://example.test/releases/v9.9.9" {
		t.Errorf("ReleaseNotesURL = %q", got.ReleaseNotesURL)
	}
	if got.LastCheck.IsZero() {
		t.Error("LastCheck not stamped on success")
	}
	if got.LastError != "" {
		t.Errorf("LastError = %q, want empty on success", got.LastError)
	}
	if got.CurrentVersion != current {
		t.Errorf("CurrentVersion = %q, want %q", got.CurrentVersion, current)
	}
}

func TestUpdaterReportsUpToDate(t *testing.T) {
	srv, _ := fakeReleasesServer(t, 200, &Release{
		TagName: "v" + version.ServerVersion, // exactly current
		HTMLURL: "https://example.test/releases/current",
	})
	u := New(Options{
		RepoOverride: "fake/repo",
		Client:       NewClient("fake/repo", time.Second).WithBaseURL(srv.URL),
	})
	got := u.CheckNow(context.Background())
	if got.UpdateAvailable {
		t.Errorf("UpdateAvailable = true on equal version (%q == %q)",
			got.LatestVersion, got.CurrentVersion)
	}
	if got.LatestVersion != version.ServerVersion {
		t.Errorf("LatestVersion = %q, want %q", got.LatestVersion, version.ServerVersion)
	}
}

func TestUpdaterIgnoresPrereleaseAndDraft(t *testing.T) {
	for _, c := range []struct {
		name string
		rel  *Release
	}{
		{"draft", &Release{TagName: "v9.9.9", Draft: true}},
		{"prerelease", &Release{TagName: "v9.9.9", Prerelease: true}},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, _ := fakeReleasesServer(t, 200, c.rel)
			u := New(Options{
				RepoOverride: "fake/repo",
				Client:       NewClient("fake/repo", time.Second).WithBaseURL(srv.URL),
			})
			got := u.CheckNow(context.Background())
			if got.UpdateAvailable {
				t.Errorf("UpdateAvailable = true for %s release", c.name)
			}
			if got.LatestVersion != "" {
				t.Errorf("LatestVersion = %q, want empty for %s", got.LatestVersion, c.name)
			}
		})
	}
}

func TestUpdaterRecordsRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "9999999999")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	u := New(Options{
		RepoOverride: "fake/repo",
		Client:       NewClient("fake/repo", time.Second).WithBaseURL(srv.URL),
	})
	got := u.CheckNow(context.Background())
	if got.LastError == "" {
		t.Error("LastError empty on rate-limited response")
	}
	if got.UpdateAvailable {
		t.Error("UpdateAvailable = true on rate-limited response")
	}
}

// TestUpdaterSurfaces404AsLastError pins the contract that a 404 from
// /releases/latest produces a non-empty LastError, so the dashboard's
// "check failed" branch fires (LastError != "") instead of the
// permanent "checking…" branch (LastError == "" && LatestVersion == "").
//
// 404 is GitHub's response to either (a) a public repo with no releases
// yet, or (b) a private/missing repo on the unauthenticated API path —
// the same sentinel covers both.
func TestUpdaterSurfaces404AsLastError(t *testing.T) {
	srv, _ := fakeReleasesServer(t, 404, nil)
	u := New(Options{
		RepoOverride: "fake/repo",
		Client:       NewClient("fake/repo", time.Second).WithBaseURL(srv.URL),
	})
	got := u.CheckNow(context.Background())
	if got.LastError != ErrNoReleasesPublished.Error() {
		t.Errorf("LastError = %q, want %q",
			got.LastError, ErrNoReleasesPublished.Error())
	}
	if got.UpdateAvailable {
		t.Error("UpdateAvailable = true on 404 (no candidate)")
	}
	if got.LatestVersion != "" {
		t.Errorf("LatestVersion = %q, want empty on 404", got.LatestVersion)
	}
}

// TestUpdaterClearsCachedAvailabilityOn404 pins the regression Qodo
// flagged in PR #89 review: a successful poll that surfaces an update
// followed by a poll that 404s must clear the cached
// LatestVersion/UpdateAvailable/ReleaseNotesURL fields. Otherwise the
// dashboard's `UpdateAvailable` / `LatestVersion` template branches
// outrank the `LastError` branch and operators see a stale "update
// available" badge with no way to know the check is broken.
func TestUpdaterClearsCachedAvailabilityOn404(t *testing.T) {
	// First poll: real release surfaces an update.
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"tag_name":"v9.9.9","html_url":"https://x"}`))
	}))
	u := New(Options{
		RepoOverride: "fake/repo",
		Client:       NewClient("fake/repo", time.Second).WithBaseURL(first.URL),
	})
	got := u.CheckNow(context.Background())
	if !got.UpdateAvailable || got.LatestVersion != "9.9.9" {
		t.Fatalf("first poll should surface update; got %+v", got)
	}
	first.Close()

	// Second poll: 404 (repo went private OR releases removed).
	second, _ := fakeReleasesServer(t, 404, nil)
	u.client = NewClient("fake/repo", time.Second).WithBaseURL(second.URL)
	got = u.CheckNow(context.Background())
	if got.LastError != ErrNoReleasesPublished.Error() {
		t.Errorf("LastError = %q, want sentinel", got.LastError)
	}
	if got.UpdateAvailable {
		t.Error("UpdateAvailable should be cleared on definitive-no-candidate sentinel")
	}
	if got.LatestVersion != "" {
		t.Errorf("LatestVersion = %q, want cleared", got.LatestVersion)
	}
	if got.ReleaseNotesURL != "" {
		t.Errorf("ReleaseNotesURL = %q, want cleared", got.ReleaseNotesURL)
	}
}

// TestSemverGreater_TreatsInvalidCurrentAsZero pins the Qodo bot fix:
// non-semver `current` (a bare git SHA from `make build` on a tagless
// clone, the "dev" fallback) must NOT permanently suppress
// UpdateAvailable. semverGreater treats invalid current as v0.0.0 so
// any valid release surfaces as an upgrade.
func TestSemverGreater_TreatsInvalidCurrentAsZero(t *testing.T) {
	cases := []struct {
		name            string
		latest, current string
		wantGreater     bool
	}{
		{"valid > valid", "1.0.0", "0.9.0", true},
		{"valid == valid", "1.0.0", "1.0.0", false},
		{"valid < valid", "0.9.0", "1.0.0", false},
		{"valid latest, dev current", "1.0.0", "dev", true},
		{"valid latest, sha current", "0.1.2", "abc1234", true},
		{"valid latest, dirty desc current", "0.1.2", "0.1.1-4-gabcdef-dirty", true},
		{"invalid latest is rejected", "not-a-version", "0.0.0", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := semverGreater(c.latest, c.current); got != c.wantGreater {
				t.Errorf("semverGreater(%q, %q) = %v, want %v",
					c.latest, c.current, got, c.wantGreater)
			}
		})
	}
}

// TestUpdaterSurfacesPrereleaseAsLastError pins the Gemini bot fix:
// the prerelease/draft branch of LatestRelease used to return an
// empty Release with no error — same dashboard "checking…"-stuck
// symptom as the 404 path. Now both share `ErrNoReleasesPublished`.
func TestUpdaterSurfacesPrereleaseAsLastError(t *testing.T) {
	srv, _ := fakeReleasesServer(t, 200, &Release{
		TagName:    "v9.9.9-rc1",
		Prerelease: true,
	})
	u := New(Options{
		RepoOverride: "fake/repo",
		Client:       NewClient("fake/repo", time.Second).WithBaseURL(srv.URL),
	})
	got := u.CheckNow(context.Background())
	if got.LastError != ErrNoReleasesPublished.Error() {
		t.Errorf("LastError = %q, want sentinel for prerelease", got.LastError)
	}
	if got.LatestVersion != "" {
		t.Errorf("LatestVersion = %q, want empty for prerelease", got.LatestVersion)
	}
}

func TestUpdaterRunHonoursContextCancel(t *testing.T) {
	srv, hits := fakeReleasesServer(t, 200, &Release{TagName: "v9.9.9"})
	u := New(Options{
		RepoOverride:  "fake/repo",
		CheckInterval: minCheckInterval, // clamped, not actually used during 50 ms run
		Client:        NewClient("fake/repo", time.Second).WithBaseURL(srv.URL),
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		u.Run(ctx)
		close(done)
	}()
	// Wait for the on-entry check to happen, then cancel.
	deadline := time.Now().Add(2 * time.Second)
	for hits.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("on-entry poll never happened")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestNewClampsCheckInterval(t *testing.T) {
	u := New(Options{CheckInterval: time.Second})
	if u.interval != minCheckInterval {
		t.Errorf("interval clamp: got %v, want %v", u.interval, minCheckInterval)
	}
	u2 := New(Options{})
	if u2.interval != DefaultCheckInterval {
		t.Errorf("interval default: got %v, want %v", u2.interval, DefaultCheckInterval)
	}
}

func TestStatusInitiallyHasCurrentVersion(t *testing.T) {
	u := New(Options{RepoOverride: "fake/repo"})
	got := u.Status()
	if got.CurrentVersion != version.ServerVersion {
		t.Errorf("CurrentVersion = %q, want %q",
			got.CurrentVersion, version.ServerVersion)
	}
	if got.UpdateAvailable {
		t.Error("UpdateAvailable = true before any poll has happened")
	}
}

// TestLiveProvidersOverrideStaticOptions pins the three update settings
// being read at DECISION time rather than captured in New().
//
// The distinction matters most for autoInstall: it gates a background
// binary swap and a supervised restart, so an operator switching it off
// must have that bind before the next poll, not after a restart they were
// never told to perform.
func TestLiveProvidersOverrideStaticOptions(t *testing.T) {
	var live atomic.Bool
	var hours atomic.Int64
	hours.Store(int64(3 * time.Hour))
	var qStart, qEnd atomic.Int64

	u := New(Options{
		// Static values deliberately the OPPOSITE of the live ones, so a
		// pass can only come from the provider being consulted.
		AutoInstall:       false,
		CheckInterval:     9 * time.Hour,
		QuietHoursStart:   111,
		QuietHoursEnd:     222,
		LiveAutoInstall:   live.Load,
		LiveCheckInterval: func() time.Duration { return time.Duration(hours.Load()) },
		LiveQuietHours:    func() (int, int) { return int(qStart.Load()), int(qEnd.Load()) },
	})

	if u.autoInstallEnabled() {
		t.Error("live provider says false; the static true must not win")
	}
	live.Store(true)
	if !u.autoInstallEnabled() {
		t.Error("flipping the live provider must bind without reconstructing the Updater")
	}

	if got := u.checkInterval(); got != 3*time.Hour {
		t.Errorf("checkInterval = %v, want the live 3h (not the static 9h)", got)
	}
	s, e := u.quietHours()
	if s != 0 || e != 0 {
		t.Errorf("quietHours = (%d,%d), want the live (0,0)", s, e)
	}
	qStart.Store(60)
	qEnd.Store(120)
	if s, e := u.quietHours(); s != 60 || e != 120 {
		t.Errorf("quietHours = (%d,%d) after a live change, want (60,120)", s, e)
	}
}

// TestLiveCheckIntervalIsClamped — the floor and default that New()
// applies to the static value are the CONTRACT, not an artifact of
// construction. A live provider that skipped them could poll GitHub every
// second, which is exactly the abuse minCheckInterval exists to prevent.
func TestLiveCheckIntervalIsClamped(t *testing.T) {
	var d atomic.Int64
	u := New(Options{LiveCheckInterval: func() time.Duration { return time.Duration(d.Load()) }})

	d.Store(0) // "unset" resolves to the default, as at construction
	if got := u.checkInterval(); got != DefaultCheckInterval {
		t.Errorf("zero interval = %v, want DefaultCheckInterval %v", got, DefaultCheckInterval)
	}
	d.Store(int64(time.Second)) // below the floor
	if got := u.checkInterval(); got != minCheckInterval {
		t.Errorf("1s interval = %v, want the floor %v", got, minCheckInterval)
	}
	d.Store(int64(-5 * time.Hour)) // negative
	if got := u.checkInterval(); got != DefaultCheckInterval {
		t.Errorf("negative interval = %v, want DefaultCheckInterval %v", got, DefaultCheckInterval)
	}
	d.Store(int64(4 * time.Hour)) // clear of both clamps
	if got := u.checkInterval(); got != 4*time.Hour {
		t.Errorf("4h interval = %v, want it passed through", got)
	}
}

// TestNilLiveProvidersKeepStaticBehaviour — every caller that is not
// cmd/bridge (tests, the CLI) leaves these nil and must be unaffected.
func TestNilLiveProvidersKeepStaticBehaviour(t *testing.T) {
	u := New(Options{AutoInstall: true, CheckInterval: 2 * time.Hour,
		QuietHoursStart: 60, QuietHoursEnd: 120})
	if !u.autoInstallEnabled() {
		t.Error("nil provider must fall back to the static AutoInstall")
	}
	if got := u.checkInterval(); got != 2*time.Hour {
		t.Errorf("checkInterval = %v, want the static 2h", got)
	}
	if s, e := u.quietHours(); s != 60 || e != 120 {
		t.Errorf("quietHours = (%d,%d), want the static (60,120)", s, e)
	}
}

// TestRunRearmRereadsTheIntervalWithoutPolling pins the fix for the gap
// Gemini caught on the cadence PR: LiveCheckInterval alone made the new
// value readable but not READ, because the loop was parked on a timer
// built from the old one — up to 6 h on the default, which is
// indistinguishable from the change being ignored.
//
// Also pins that a rearm does NOT poll. A settings save is not a request
// to check GitHub for updates, and turning one into the other would put
// an outbound request behind every unrelated Save.
//
// Both signals are channels fed from the real code paths — the httptest
// handler for polls, the interval provider for reads — so the assertions
// are about what the loop actually did. An earlier draft counted polls
// with a variable nothing incremented, which made the no-poll half
// vacuous.
func TestRunRearmRereadsTheIntervalWithoutPolling(t *testing.T) {
	polls := make(chan struct{}, 16)
	reads := make(chan struct{}, 16)
	// Non-blocking so the loop is never gated on the test draining.
	signal := func(ch chan struct{}) {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	await := func(ch <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for %s", what)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		signal(polls)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"tag_name":"v0.0.1","assets":[]}`)
	}))
	defer srv.Close()

	rearm := make(chan struct{}, 1)
	client := NewClient("acoseac/1-bit-bridge", 2*time.Second)
	client.baseURL = srv.URL
	u := New(Options{
		// An hour: long enough that the timer cannot fire, so a second
		// interval read can only come from the rearm.
		LiveCheckInterval: func() time.Duration { signal(reads); return time.Hour },
		Rearm:             rearm,
		Client:            client,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); u.Run(ctx) }()

	// Run polls once on entry, then reads the interval for its first wait.
	await(polls, "the entry poll")
	await(reads, "the first interval read")

	// Rearm: the loop must consult the provider again...
	rearm <- struct{}{}
	await(reads, "an interval read after the rearm")

	// ...and must not have polled for it. Give a wrong implementation
	// room to make the request before concluding it did not.
	time.Sleep(150 * time.Millisecond)
	select {
	case <-polls:
		t.Error("rearm triggered a poll — a settings save is not a request to check " +
			"for updates")
	default:
	}
	cancel()
	<-done
}

// TestSemverGreater_DescendantBuildIsNotOlderThanItsTag is the regression
// gate for the downgrade bug found on bridge.ars.md (2026-08-30): the host
// ran v0.1.9-65-g8b092ad, /api/updates reported latest 0.1.9 with
// updateAvailable true, and Install was armed — pressing it would have
// rolled the binary BACKWARDS 65 commits. With auto-install on, every edge
// build would have reverted itself to the last tag on the next poll.
//
// The cause is spec-correct semver: a hyphenated suffix is a PRE-RELEASE,
// and a pre-release sorts below its release. `git describe` output has to
// be read as build metadata instead, which semver ignores when ordering.
//
// Both directions are pinned in one table on purpose. A fix that only
// suppressed the downgrade would be trivially achievable by suppressing
// EVERY update for a describe build, which is a worse bug and a silent one
// — so the "still upgrades" rows below are the real assertion.
func TestSemverGreater_DescendantBuildIsNotOlderThanItsTag(t *testing.T) {
	cases := []struct {
		name            string
		latest, current string
		want            bool
		why             string
	}{
		// The reported case, verbatim.
		{"the reported downgrade", "0.1.9", "0.1.9-65-g8b092ad", false,
			"65 commits past the tag is not behind the tag"},
		{"dirty descendant", "0.1.9", "0.1.9-65-g8b092ad-dirty", false,
			"a dirty tree does not make a descendant older"},
		{"exact tag, dirty tree", "0.1.9", "0.1.9-dirty", false,
			"describe appends -dirty with no commit count on an exact tag"},
		{"exact tag", "0.1.9", "0.1.9", false, "equal is not greater"},

		// The half that must keep working: a genuine release still lands.
		{"patch release over descendant", "0.1.10", "0.1.9-65-g8b092ad", true,
			"a real release above the tag must still be offered"},
		{"minor release over descendant", "0.2.0", "0.1.9-65-g8b092ad", true, ""},
		{"major release over dirty descendant", "1.0.0", "0.1.9-65-g8b092ad-dirty", true, ""},
		{"descendant of an older tag", "0.1.9", "0.1.8-3-gdeadbee", true,
			"still behind by a whole patch release"},

		// A hand-written pre-release must KEEP pre-release ordering. This is
		// the row that fails if the describe pattern is loosened to any
		// hyphenated suffix.
		{"real prerelease sorts below its release", "0.2.0", "0.2.0-rc.1", true,
			"rc.1 genuinely precedes 0.2.0 and must still upgrade"},
		{"real prerelease is not a descendant", "0.2.0-rc.1", "0.2.0", false,
			"the release is not superseded by its own rc"},
		{"beta suffix untouched", "0.2.0", "0.2.0-beta1", true, ""},

		// Unparseable current keeps the documented floor behaviour.
		{"bare sha current still upgrades", "0.1.9", "abc1234", true,
			"documented v0.0.0 floor for tagless builds"},
		{"dev current still upgrades", "0.1.9", "dev", true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := semverGreater(c.latest, c.current); got != c.want {
				t.Errorf("semverGreater(latest=%q, current=%q) = %v, want %v — %s",
					c.latest, c.current, got, c.want, c.why)
			}
		})
	}
}

// TestNormalizeDescribe pins the transform itself, separately from the
// comparison, so a failure says which half broke.
func TestNormalizeDescribe(t *testing.T) {
	cases := []struct{ in, want string }{
		{"0.1.9-65-g8b092ad", "0.1.9+65.g8b092ad"},
		{"0.1.9-65-g8b092ad-dirty", "0.1.9+65.g8b092ad.dirty"},
		{"0.1.9-dirty", "0.1.9+dirty"},
		{"0.1.9-0-gabc1234", "0.1.9+0.gabc1234"},
		// Left alone: releases, real pre-releases, and anything unparseable.
		{"0.1.9", "0.1.9"},
		{"0.2.0-rc.1", "0.2.0-rc.1"},
		{"0.2.0-beta1", "0.2.0-beta1"},
		{"abc1234", "abc1234"},
		{"dev", "dev"},
		{"", ""},
		// A pre-release that merely CONTAINS digits and a g-word must not be
		// mistaken for describe output — the pattern is end-anchored.
		{"0.2.0-g12345", "0.2.0-g12345"},
		{"0.2.0-12-nothex", "0.2.0-12-nothex"},
	}
	for _, c := range cases {
		if got := normalizeDescribe(c.in); got != c.want {
			t.Errorf("normalizeDescribe(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestNormalizeDescribeWithBuildMetadataInTheTag is the CodeRabbit
// finding on PR #797, and it matters more than it looks.
//
// Semver permits exactly one "+". A tag that already carries build
// metadata — `v0.1.9+ci.1`, which describe extends to
// `v0.1.9+ci.1-65-g8b092ad` — would otherwise be rewritten with a
// SECOND one. That string fails semver.IsValid, `current` falls to the
// v0.0.0 floor, and the release is reported as an upgrade: the downgrade
// this function exists to prevent, back again for that input shape.
//
// Verified against golang.org/x/mod before fixing: `v0.1.9+ci.1+65.g...`
// is rejected, `v0.1.9+ci.1.65.g...` is accepted.
func TestNormalizeDescribeWithBuildMetadataInTheTag(t *testing.T) {
	cases := []struct{ in, want string }{
		{"0.1.9+ci.1-65-g8b092ad", "0.1.9+ci.1.65.g8b092ad"},
		{"0.1.9+ci.1-65-g8b092ad-dirty", "0.1.9+ci.1.65.g8b092ad.dirty"},
		{"0.1.9+ci.1-dirty", "0.1.9+ci.1.dirty"},
		// Unchanged: no metadata in the tag, single "+" as before.
		{"0.1.9-65-g8b092ad", "0.1.9+65.g8b092ad"},
	}
	for _, c := range cases {
		got := normalizeDescribe(c.in)
		if got != c.want {
			t.Errorf("normalizeDescribe(%q) = %q, want %q", c.in, got, c.want)
		}
		if !semver.IsValid("v" + got) {
			t.Errorf("normalizeDescribe(%q) = %q, which semver rejects — `current` "+
				"would fall to the v0.0.0 floor and the tag would be offered as an "+
				"upgrade, which is the downgrade bug all over again", c.in, got)
		}
	}
	// End to end: the descendant of a metadata-carrying tag must not be
	// offered its own tag as an update.
	if semverGreater("0.1.9", "0.1.9+ci.1-65-g8b092ad") {
		t.Error("a descendant of v0.1.9+ci.1 was offered v0.1.9 as an upgrade")
	}
}
