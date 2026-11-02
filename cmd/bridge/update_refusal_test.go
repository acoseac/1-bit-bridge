package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/updater"
)

// fakeLatestRelease is a GitHub Releases API whose latest release is tag.
func fakeLatestRelease(t *testing.T, tag string) *updater.Updater {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/fake/repo/releases/latest" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"tag_name":%q,"html_url":"https://example.invalid/release"}`, tag)
	}))
	t.Cleanup(srv.Close)
	upd := updater.New(updater.Options{
		RepoOverride: "fake/repo",
		Client:       updater.NewClient("fake/repo", 5*time.Second).WithBaseURL(srv.URL),
	})
	if st := upd.CheckNow(t.Context()); !st.UpdateAvailable {
		t.Fatalf("the fake release %s is not an update: %+v", tag, st)
	}
	return upd
}

// TestTheConsoleShowsAVersionHeldBackForRefusingTheSettings: a version the
// updater held back for refusing this bridge's settings is shown on the
// console's Updates panel, in the row an auto-install deferral uses, whether
// or not the auto-installer has run in this process, and so after a restart.
func TestTheConsoleShowsAVersionHeldBackForRefusingTheSettings(t *testing.T) {
	upd := fakeLatestRelease(t, "v9.9.9")
	dataDir := t.TempDir()
	cfg := filepath.Join(t.TempDir(), "bridge.yaml")
	if err := os.WriteFile(cfg, []byte("scanIntervalSec: 010\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reason := "9.9.9 refuses this bridge's settings: line 1: a whole number is written with a leading zero"
	if err := updater.SaveState(dataDir, updater.State{RejectedVersion: "9.9.9", RejectedReason: reason}); err != nil {
		t.Fatal(err)
	}
	a := updateInfoAdapter{u: upd, dataDir: dataDir, configPath: cfg}
	if got := a.Status().DeferredReason; !strings.Contains(got, "line 1:") || !strings.Contains(got, "9.9.9") {
		t.Errorf("DeferredReason = %q, want the recorded refusal", got)
	}
}
