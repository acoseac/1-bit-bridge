package updater

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// The updater polls on runServe's scanCtx, and "Check now" polls on the
// admin request's context. Either can be cancelled mid-poll, by a shutdown
// or by a client that went away, and a poll that STOPPED is not one that
// failed (ctxerr.WithoutCancellation). Each test below cancels from inside
// the request to GitHub: the fake server's handler cancels and holds the
// request until the client gives up. Each has a twin that fails the same
// request on a live context with a 500.

const (
	msgPoll        = "poll"
	msgAutoInstall = "auto-install failed"
	earlierFailure = "github status 503: an earlier poll's failure"
)

// TestAPollStoppedByShutdownChangesNothing: a stopped poll says nothing
// about GitHub, so it is not reported and it does not overwrite what the
// dashboard shows from the last poll that answered.
func TestAPollStoppedByShutdownChangesNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	u := updaterAgainst(t, cancellingGitHub(t, cancel))
	u.mu.Lock()
	u.status.LastError = earlierFailure
	u.mu.Unlock()

	rec := loggingtest.Record(t)
	if u.checkOnce(ctx) {
		t.Fatal("a poll the shutdown stopped reported success")
	}

	if got := rec.Failures(msgPoll); len(got) != 0 {
		t.Errorf("a stopped poll was reported:\n%s", strings.Join(got, "\n"))
	}
	if got := u.Status().LastError; got != earlierFailure {
		t.Errorf("LastError = %q after a stopped poll, want the earlier poll's %q", got, earlierFailure)
	}
}

// TestAPollThatFailsIsStillReported is the twin: a poll GitHub fails on a
// live context is reported and recorded.
func TestAPollThatFailsIsStillReported(t *testing.T) {
	u := updaterAgainst(t, failingGitHub(t))

	rec := loggingtest.Record(t)
	u.checkOnce(context.Background())

	if got := rec.Failures(msgPoll); len(got) != 1 {
		t.Errorf("a failed poll logged %d lines, want 1:\n%s", len(got), strings.Join(got, "\n"))
	}
	if got := u.Status().LastError; !strings.Contains(got, "500") {
		t.Errorf("LastError = %q after a failed poll, want GitHub's 500", got)
	}
}

// TestAnAutoInstallStoppedByShutdownReportsNothing: the install's re-fetch
// of the release is the request the shutdown stops.
func TestAnAutoInstallStoppedByShutdownReportsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	u := autoInstallerAgainst(t, cancellingGitHub(t, cancel))

	rec := loggingtest.Record(t)
	u.maybeAutoInstall(ctx)

	if got := rec.Failures(msgAutoInstall); len(got) != 0 {
		t.Errorf("an install the shutdown stopped was reported:\n%s", strings.Join(got, "\n"))
	}
}

// TestAnAutoInstallThatFailsIsStillReported is the twin.
func TestAnAutoInstallThatFailsIsStillReported(t *testing.T) {
	u := autoInstallerAgainst(t, failingGitHub(t))

	rec := loggingtest.Record(t)
	u.maybeAutoInstall(context.Background())

	if got := rec.Failures(msgAutoInstall); len(got) != 1 {
		t.Errorf("a failed install logged %d lines, want 1:\n%s", len(got), strings.Join(got, "\n"))
	}
}

// cancellingGitHub is a GitHub whose every request cancels the caller's
// context and is held until the client gives up on it, as a shutdown
// does to a request in flight.
func cancellingGitHub(t *testing.T, cancel context.CancelFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		cancel()
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// failingGitHub is a GitHub that answers every request 500.
func failingGitHub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// updaterAgainst builds an Updater that polls srv.
func updaterAgainst(t *testing.T, srv *httptest.Server) *Updater {
	t.Helper()
	return New(Options{
		RepoOverride: "fake/repo",
		Client:       NewClient("fake/repo", 10*time.Second).WithBaseURL(srv.URL),
	})
}

// autoInstallerAgainst builds an auto-installing Updater that polls srv,
// already knowing of a newer release, with every gate before the install's
// first request open: auto-install on, no quiet-hours window, no sessions
// in flight, and a binary path whose directory is writable.
func autoInstallerAgainst(t *testing.T, srv *httptest.Server) *Updater {
	t.Helper()
	dir := t.TempDir()
	binary := filepath.Join(dir, "bridge")
	if err := os.WriteFile(binary, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	u := New(Options{
		RepoOverride:       "fake/repo",
		Client:             NewClient("fake/repo", 10*time.Second).WithBaseURL(srv.URL),
		AutoInstall:        true,
		AutoInstallRestart: func() { t.Error("an install that never happened restarted the bridge") },
		AutoInstallOpts: &InstallOptions{
			DataDir:    t.TempDir(),
			BinaryPath: binary,
			Sessions:   NewTracker(),
			// Never reached: the install stops at its first request. A
			// literal, because noopVerifier is in a !windows file.
			Verifier: func(context.Context, string) error { return nil },
		},
	})
	u.mu.Lock()
	u.status.LatestVersion = "9.9.9"
	u.status.UpdateAvailable = true
	u.status.CurrentVersion = "0.0.1"
	u.mu.Unlock()
	return u
}
