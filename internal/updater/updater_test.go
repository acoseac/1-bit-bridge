package updater

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/version"
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
