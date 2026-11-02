//go:build !windows

package updater

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The settings check, through the real Install (backlog B613): the archive's
// "bridge" is a shell script that runs the stand-in (standin_test.go), so the
// candidate Install extracts, verifies and checks is a process that answers
// the check as the script tells it to.

// leadingZeroRefusal is the reason a candidate gives for `scanIntervalSec:
// 010` on line 1, in config's own words (B533).
const leadingZeroRefusal = "line 1: a whole number is written with a leading zero, which YAML may read as octal; " +
	"write it in decimal without the leading zero, or with 0o if you mean octal"

// candidateScript is the "bridge" a fixture's archive carries for the
// settings check: a shell script that notes where it ran from and runs the
// stand-in with its arguments.
func candidateScript(t *testing.T) []byte {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(exe, '\'') {
		t.Skipf("the test binary's path holds a quote: %s", exe)
	}
	return []byte("#!/bin/sh\necho \"$0\" >> \"$" + candidateEnv + "/where\"\nexec '" + exe + "' \"$@\"\n")
}

// checkedInstall is a bridge at 0.1.0 whose settings file holds
// `scanIntervalSec: 010`, and a release server offering a candidate that
// answers the settings check as its script says.
type checkedInstall struct {
	fix    *installFixture
	dir    string
	live   string
	config string
	script string
}

func newCheckedInstall(t *testing.T, latest string, files map[string]string) *checkedInstall {
	t.Helper()
	script := candidateStandIn(t, files)
	fix := newInstallFixtureWith(t, latest, candidateScript(t))
	dir := t.TempDir()
	live := filepath.Join(dir, "bridge")
	if err := os.WriteFile(live, []byte("bridge-binary-0.1.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(cfg, []byte("scanIntervalSec: 010\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &checkedInstall{fix: fix, dir: dir, live: live, config: cfg, script: script}
}

// opts are the options of a manual install of the candidate.
func (c *checkedInstall) opts() InstallOptions {
	return InstallOptions{DataDir: c.dir, BinaryPath: c.live, Force: true, Verifier: noopVerifier, ConfigPath: c.config}
}

// updater is an Updater, a process of its own, whose last poll saw the
// candidate.
func (c *checkedInstall) updater(t *testing.T, opts Options) *Updater {
	t.Helper()
	opts.RepoOverride = "fake/repo"
	opts.Client = NewClient("fake/repo", time.Second).WithBaseURL(c.fix.server.URL)
	upd := New(opts)
	upd.mu.Lock()
	upd.status.CurrentVersion = "0.1.0"
	upd.status.LatestVersion = c.fix.latestVersion
	upd.status.UpdateAvailable = true
	upd.mu.Unlock()
	return upd
}

// automatic is an Updater that installs the candidate on its own, and the
// count of the restarts it asked for.
func (c *checkedInstall) automatic(t *testing.T) (*Updater, *atomic.Int32) {
	t.Helper()
	restarts := &atomic.Int32{}
	opts := c.opts()
	upd := c.updater(t, Options{AutoInstall: true, AutoInstallOpts: &opts, AutoInstallRestart: func() { restarts.Add(1) }})
	return upd, restarts
}

// script sets one of the stand-in's script files.
func (c *checkedInstall) setScript(t *testing.T, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(c.script, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// requireNothingSwapped asserts the install left the binary as it was, made
// no backup, and left no staged candidate beside it.
func (c *checkedInstall) requireNothingSwapped(t *testing.T) {
	t.Helper()
	if got, err := os.ReadFile(c.live); err != nil || string(got) != "bridge-binary-0.1.0" {
		t.Errorf("the live binary is %q (%v), want the 0.1.0 it was", got, err)
	}
	if _, err := os.Lstat(c.live + ".bak"); !os.IsNotExist(err) {
		t.Errorf("a backup was made (lstat %v): something was swapped", err)
	}
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".bridge-") {
			t.Errorf("a staged candidate was left beside the binary: %s", e.Name())
		}
	}
}

// requireConfigUnchanged asserts the settings file still holds what the
// test wrote.
func (c *checkedInstall) requireConfigUnchanged(t *testing.T, want string) {
	t.Helper()
	if got, err := os.ReadFile(c.config); err != nil || string(got) != want {
		t.Errorf("the settings file holds %q (%v), want %q untouched", got, err, want)
	}
}

func TestAnInstallWhoseCandidateRefusesTheSettingsSwapsNothing(t *testing.T) {
	c := newCheckedInstall(t, "0.2.0", map[string]string{"check": "refuse", "refusal": leadingZeroRefusal})
	upd := c.updater(t, Options{})

	_, err := upd.Install(context.Background(), c.opts())
	if !errors.Is(err, ErrConfigRefused) {
		t.Fatalf("Install = %v, want ErrConfigRefused", err)
	}
	if !strings.Contains(err.Error(), "line 1:") || !strings.Contains(err.Error(), "leading zero") {
		t.Errorf("the refusal does not name the line and the reason: %v", err)
	}
	if strings.Contains(err.Error(), c.dir) {
		t.Errorf("the refusal names a path: %v", err)
	}
	c.requireNothingSwapped(t)
	c.requireConfigUnchanged(t, "scanIntervalSec: 010\n")

	st, err := LoadState(c.dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status == "installing" || st.SwapStarted {
		t.Errorf("the marker was armed for a candidate that was never installed: %+v", st)
	}
	if st.RejectedVersion != "0.2.0" || !strings.Contains(st.RejectedReason, "line 1:") || st.RejectedConfigDigest == "" {
		t.Errorf("the refusal is not recorded with its reason and the settings it refused: %+v", st)
	}

	// The candidate was asked about the settings file the bridge loaded,
	// from beside the binary it would replace.
	calls := candidateCalls(t, c.script)
	want := "serve --" + ConfigCheckFlag + " --config " + c.config
	if len(calls) != 1 || calls[0] != want {
		t.Errorf("the candidate was run as %q, want once as %q", calls, want)
	}
	where, err := os.ReadFile(filepath.Join(c.script, "where"))
	if err != nil {
		t.Fatalf("the candidate never ran: %v", err)
	}
	if dir := filepath.Dir(strings.TrimSpace(string(where))); dir != c.dir {
		t.Errorf("the candidate was checked from %s, want from beside the binary, %s", dir, c.dir)
	}
}

func TestAnAutomaticInstallHoldsARefusedVersionBackUntilTheSettingsChange(t *testing.T) {
	c := newCheckedInstall(t, "0.2.0", map[string]string{"check": "refuse", "refusal": leadingZeroRefusal})
	upd, restarts := c.automatic(t)

	upd.maybeAutoInstall(context.Background())
	c.requireNothingSwapped(t)
	if restarts.Load() != 0 {
		t.Fatalf("the auto-installer restarted %d times over a refused candidate", restarts.Load())
	}
	if got := c.fix.archiveFetches.Load(); got != 1 {
		t.Fatalf("archive fetched %d times, want 1", got)
	}
	if reason := upd.Status().DeferredReason; !strings.Contains(reason, "0.2.0") || !strings.Contains(reason, "line 1:") {
		t.Errorf("DeferredReason = %q, want it to name 0.2.0 and the refused line", reason)
	}

	// The next poll holds it back before downloading it again, and so does
	// the next process: the hold is on disk.
	upd.maybeAutoInstall(context.Background())
	again, _ := c.automatic(t)
	again.maybeAutoInstall(context.Background())
	if got := c.fix.archiveFetches.Load(); got != 1 {
		t.Errorf("archive fetched %d times while the settings were unchanged, want 1", got)
	}
	if reason := again.Status().DeferredReason; !strings.Contains(reason, "line 1:") {
		t.Errorf("after a restart DeferredReason = %q, want the recorded refusal", reason)
	}
	c.requireNothingSwapped(t)

	// The settings fixed, the version is tried again and installed.
	if err := os.WriteFile(c.config, []byte("scanIntervalSec: 10\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.setScript(t, "check", "valid")
	again.maybeAutoInstall(context.Background())
	if got := c.fix.archiveFetches.Load(); got != 2 {
		t.Errorf("archive fetched %d times after the settings were fixed, want 2", got)
	}
	if got, _ := os.ReadFile(c.live); !bytes.Equal(got, candidateScript(t)) {
		t.Errorf("the candidate was not installed once the settings were fixed: live = %q", got)
	}
}

func TestAManualInstallTakesARefusedVersionOnceTheSettingsAreFixed(t *testing.T) {
	c := newCheckedInstall(t, "0.2.0", map[string]string{"check": "refuse", "refusal": leadingZeroRefusal})
	upd := c.updater(t, Options{})
	if _, err := upd.Install(context.Background(), c.opts()); !errors.Is(err, ErrConfigRefused) {
		t.Fatalf("Install = %v, want ErrConfigRefused", err)
	}

	if err := os.WriteFile(c.config, []byte("scanIntervalSec: 10\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.setScript(t, "check", "valid")
	if _, err := c.updater(t, Options{}).Install(context.Background(), c.opts()); err != nil {
		t.Fatalf("Install after the fix: %v", err)
	}
	if got, _ := os.ReadFile(c.live); !bytes.Equal(got, candidateScript(t)) {
		t.Errorf("live = %q, want the candidate", got)
	}
	st, err := LoadState(c.dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "installing" || st.RejectedVersion != "" || st.RejectedReason != "" || st.RejectedConfigDigest != "" {
		t.Errorf("the install did not arm a fresh marker over the refusal: %+v", st)
	}
}

func TestAnAutomaticInstallAdmitsANewerVersionPastARefusal(t *testing.T) {
	c := newCheckedInstall(t, "0.3.0", map[string]string{"check": "valid"})
	if err := SaveState(c.dir, State{RejectedVersion: "0.2.0", RejectedReason: "0.2.0 refuses this bridge's settings: " + leadingZeroRefusal,
		RejectedConfigDigest: "a digest of the settings 0.2.0 refused"}); err != nil {
		t.Fatal(err)
	}
	upd, restarts := c.automatic(t)
	upd.maybeAutoInstall(context.Background())
	if got, _ := os.ReadFile(c.live); !bytes.Equal(got, candidateScript(t)) {
		t.Errorf("a newer release was held back by an older refusal: live = %q", got)
	}
	if restarts.Load() != 1 {
		t.Errorf("restarts = %d, want 1", restarts.Load())
	}
}

func TestAnAutomaticInstallHoldsBackACandidateThatPredatesTheCheck(t *testing.T) {
	c := newCheckedInstall(t, "0.2.0", map[string]string{"check": "old"})
	upd, restarts := c.automatic(t)
	upd.maybeAutoInstall(context.Background())
	c.requireNothingSwapped(t)
	if restarts.Load() != 0 {
		t.Errorf("restarts = %d, want 0", restarts.Load())
	}
	if reason := upd.Status().DeferredReason; !strings.Contains(reason, "0.2.0") || !strings.Contains(reason, "predates") {
		t.Errorf("DeferredReason = %q, want it to say 0.2.0 predates the check", reason)
	}
	st, err := LoadState(c.dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.RejectedVersion != "0.2.0" || st.RejectedConfigDigest != "" {
		t.Errorf("the hold is not recorded for every setting: %+v", st)
	}
	upd.maybeAutoInstall(context.Background())
	if got := c.fix.archiveFetches.Load(); got != 1 {
		t.Errorf("archive fetched %d times, want 1: a candidate that cannot be checked is not downloaded again", got)
	}
}

func TestAManualInstallTakesACandidateThatPredatesTheCheckWithAWarning(t *testing.T) {
	c := newCheckedInstall(t, "0.2.0", map[string]string{"check": "old"})
	st, err := c.updater(t, Options{}).Install(context.Background(), c.opts())
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if got, _ := os.ReadFile(c.live); !bytes.Equal(got, candidateScript(t)) {
		t.Errorf("live = %q, want the candidate installed by hand", got)
	}
	if !strings.Contains(st.InstallWarning, "0.2.0") || !strings.Contains(st.InstallWarning, "predates") {
		t.Errorf("InstallWarning = %q, want it to say the candidate was installed without the check", st.InstallWarning)
	}
}

func TestACheckThatFailsSwapsNothingAndRecordsNothing(t *testing.T) {
	c := newCheckedInstall(t, "0.2.0", map[string]string{"check": "exit:3"})
	upd := c.updater(t, Options{})
	for attempt := 1; attempt <= 2; attempt++ {
		_, err := upd.Install(context.Background(), c.opts())
		if !errors.Is(err, ErrConfigCheckFailed) {
			t.Fatalf("attempt %d: Install = %v, want ErrConfigCheckFailed", attempt, err)
		}
		if strings.Contains(err.Error(), c.dir) {
			t.Errorf("the failure names a path: %v", err)
		}
	}
	c.requireNothingSwapped(t)
	if calls := candidateCalls(t, c.script); len(calls) != 2 {
		t.Errorf("the candidate was checked %d times, want once each attempt (2)", len(calls))
	}
	st, err := LoadState(c.dir)
	if err != nil {
		t.Fatal(err)
	}
	if st != (State{}) {
		t.Errorf("a check that did not answer left a marker or a hold: %+v", st)
	}
}

func TestACheckThatHangsIsStoppedAtItsBound(t *testing.T) {
	prev := configCheckTimeout
	configCheckTimeout = 2 * time.Second
	t.Cleanup(func() { configCheckTimeout = prev })
	c := newCheckedInstall(t, "0.2.0", map[string]string{"check": "hang"})
	start := time.Now()
	_, err := c.updater(t, Options{}).Install(context.Background(), c.opts())
	if !errors.Is(err, ErrConfigCheckFailed) {
		t.Fatalf("Install = %v, want ErrConfigCheckFailed", err)
	}
	if took := time.Since(start); took > 30*time.Second {
		t.Errorf("a hung check held the install %s", took)
	}
	c.requireNothingSwapped(t)
}

func TestAShutdownDuringTheCheckIsACancellationNotAFailure(t *testing.T) {
	c := newCheckedInstall(t, "0.2.0", map[string]string{"check": "hang"})
	ctx, cancel := context.WithCancel(context.Background())
	installed := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		// Cancel once the check is running: the stand-in notes its call
		// before it hangs.
		defer close(stopped)
		defer cancel()
		for {
			if _, err := os.Stat(filepath.Join(c.script, "argv")); err == nil {
				return
			}
			select {
			case <-installed:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	_, err := c.updater(t, Options{}).Install(ctx, c.opts())
	close(installed)
	<-stopped
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Install = %v, want it to carry the cancellation", err)
	}
	c.requireNothingSwapped(t)
}

func TestARefusalKeepsTheMarkersInstallBookkeeping(t *testing.T) {
	c := newCheckedInstall(t, "0.2.0", map[string]string{"check": "refuse", "refusal": leadingZeroRefusal})
	installedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	if err := SaveState(c.dir, State{Status: "installed", TargetVersion: "0.1.0", PreviousVersion: "0.0.9",
		AttemptedAt: installedAt, InstalledAt: installedAt, SwapStarted: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.updater(t, Options{}).Install(context.Background(), c.opts()); !errors.Is(err, ErrConfigRefused) {
		t.Fatalf("Install = %v, want ErrConfigRefused", err)
	}
	st, err := LoadState(c.dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "installed" || !st.InstalledAt.Equal(installedAt) || st.TargetVersion != "0.1.0" {
		t.Errorf("the refusal dropped the last install's bookkeeping, so its backup is never cleaned up: %+v", st)
	}
	if st.RejectedVersion != "0.2.0" {
		t.Errorf("RejectedVersion = %q, want 0.2.0", st.RejectedVersion)
	}
}

func TestARefusalThatCannotBeRecordedStillSwapsNothing(t *testing.T) {
	c := newCheckedInstall(t, "0.2.0", map[string]string{"check": "refuse", "refusal": leadingZeroRefusal})
	// A directory where the marker goes: neither a refusal's record nor
	// an install's marker can be written.
	if err := os.Mkdir(StatePath(c.dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(StatePath(c.dir), "keep"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := c.updater(t, Options{}).Install(context.Background(), c.opts())
	if !errors.Is(err, ErrConfigRefused) {
		t.Fatalf("Install = %v, want ErrConfigRefused", err)
	}
	c.requireNothingSwapped(t)
}
