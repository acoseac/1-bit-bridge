//go:build !windows

package updater

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// autoInstallHarness wires a real Updater against the fake release
// server with a real on-disk binary, so the rollback tests can assert on
// what actually landed (bytes at the live path, archive fetch count)
// rather than on gate side effects alone.
type autoInstallHarness struct {
	fix      *installFixture
	upd      *Updater
	dir      string
	livePath string
	restarts *atomic.Int32
}

func newAutoInstallHarness(t *testing.T, currentVersion, latestVersion string) *autoInstallHarness {
	t.Helper()
	fix := newInstallFixture(t, latestVersion)
	dir := t.TempDir()
	livePath := filepath.Join(dir, "bridge")
	if err := os.WriteFile(livePath, []byte("bridge-binary-"+currentVersion), 0o755); err != nil {
		t.Fatal(err)
	}
	restarts := &atomic.Int32{}
	upd := New(Options{
		RepoOverride: "fake/repo",
		Client:       NewClient("fake/repo", time.Second).WithBaseURL(fix.server.URL),
		AutoInstall:  true,
		AutoInstallOpts: &InstallOptions{
			DataDir:    dir,
			BinaryPath: livePath,
			Force:      true,
			Verifier:   noopVerifier,
		},
		AutoInstallRestart: func() { restarts.Add(1) },
	})
	upd.mu.Lock()
	upd.status.CurrentVersion = currentVersion
	upd.status.LatestVersion = fix.latestVersion
	upd.status.UpdateAvailable = true
	upd.mu.Unlock()
	return &autoInstallHarness{fix: fix, upd: upd, dir: dir, livePath: livePath, restarts: restarts}
}

func (h *autoInstallHarness) liveBody(t *testing.T) string {
	t.Helper()
	got, err := os.ReadFile(h.livePath)
	if err != nil {
		t.Fatalf("read live binary: %v", err)
	}
	return string(got)
}

func (h *autoInstallHarness) rollbackOpts() InstallOptions {
	return InstallOptions{DataDir: h.dir, BinaryPath: h.livePath, Force: true}
}

// TestRollbackSurvivesTheNextAutoInstallPoll is the F1 regression: a
// manual rollback was silently undone by the very next auto-install
// poll. Nothing recorded the rejected version, so the poller compared
// the now-running vN against the still-latest vN+1, every gate passed
// (quiet hours default to "any time", pendingRestart is false on a fresh
// process, no sessions on a fresh boot), and the release the operator
// had just rejected was re-downloaded, re-swapped, and restarted into —
// the only escape being to hand-edit bridge.yaml to disable autoInstall.
func TestRollbackSurvivesTheNextAutoInstallPoll(t *testing.T) {
	h := newAutoInstallHarness(t, "0.1.0", "0.2.0")

	// 1. Auto-install takes the candidate.
	h.upd.maybeAutoInstall(context.Background())
	if got := h.liveBody(t); got != "bridge-binary-0.2.0" {
		t.Fatalf("live after auto-install = %q, want bridge-binary-0.2.0", got)
	}
	if got := h.restarts.Load(); got != 1 {
		t.Fatalf("restarts after auto-install = %d, want 1", got)
	}

	// 2. Operator finds it broken and rolls it back.
	if err := h.upd.Rollback(h.rollbackOpts()); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got := h.liveBody(t); got != "bridge-binary-0.1.0" {
		t.Fatalf("live after rollback = %q, want bridge-binary-0.1.0", got)
	}

	// 3. The next poll cycle. GitHub still advertises 0.2.0 and the
	//    cached status still says it's available — exactly the state a
	//    restarted, re-polled bridge lands in.
	h.upd.maybeAutoInstall(context.Background())

	if got := h.liveBody(t); got != "bridge-binary-0.1.0" {
		t.Errorf("auto-install re-installed the rolled-back release: live = %q, want bridge-binary-0.1.0", got)
	}
	if got := h.restarts.Load(); got != 1 {
		t.Errorf("restarts = %d after the post-rollback poll, want 1 (the rollback must not be bounced away)", got)
	}
	if got := h.fix.archiveFetches.Load(); got != 1 {
		t.Errorf("archive fetched %d time(s); want 1 (the rejected release must not be re-downloaded)", got)
	}
	if reason := h.upd.Status().DeferredReason; !strings.Contains(reason, "0.2.0") {
		t.Errorf("DeferredReason = %q, want it to name the rolled-back 0.2.0", reason)
	}
}

// TestRollbackRecordsRejectedVersion pins the persistence itself: the
// rollback has to leave something behind that survives the restart it
// needs to take effect. Pre-fix Rollback called ClearState and persisted
// nothing at all.
func TestRollbackRecordsRejectedVersion(t *testing.T) {
	fix := newInstallFixture(t, "0.2.0")
	livePath, upd, err := fix.install(t, "0.1.0")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	dir := filepath.Dir(livePath)

	if err := upd.Rollback(InstallOptions{DataDir: dir, BinaryPath: livePath, Force: true}); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	st, err := LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if st.RejectedVersion != "0.2.0" {
		t.Errorf("state.RejectedVersion = %q, want 0.2.0", st.RejectedVersion)
	}
	// The surviving marker must be inert for boot purposes — it exists
	// only to carry the rejection, and a leftover "installing" would
	// drive a spurious boot-time rollback.
	if got := DecideBootAction(st, "0.1.0", time.Now().UTC()); got != BootNoop {
		t.Errorf("DecideBootAction on the rejection marker = %v, want BootNoop", got)
	}
}

// TestAutoInstallProceedsPastAnOlderRejection is the complement: the
// operator rejected ONE build, not every future one. A strictly newer
// release must install normally — otherwise a single rollback would
// disable auto-install on that host permanently.
func TestAutoInstallProceedsPastAnOlderRejection(t *testing.T) {
	h := newAutoInstallHarness(t, "0.1.0", "0.3.0")
	if err := SaveState(h.dir, State{RejectedVersion: "0.2.0"}); err != nil {
		t.Fatal(err)
	}

	h.upd.maybeAutoInstall(context.Background())

	if got := h.liveBody(t); got != "bridge-binary-0.3.0" {
		t.Errorf("live = %q, want bridge-binary-0.3.0 (a newer release must clear the older rejection)", got)
	}
	if got := h.restarts.Load(); got != 1 {
		t.Errorf("restarts = %d, want 1", got)
	}
	// Installing retires the rejection: the marker is rewritten whole.
	st, err := LoadState(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.RejectedVersion != "" {
		t.Errorf("state.RejectedVersion = %q after installing a newer release, want it cleared", st.RejectedVersion)
	}
}

func TestRejectionBlocks(t *testing.T) {
	cases := []struct {
		name                string
		candidate, rejected string
		want                bool
	}{
		{"nothing rejected", "0.2.0", "", false},
		{"exact match blocks", "0.2.0", "0.2.0", true},
		{"v-prefix skew still blocks", "v0.2.0", "0.2.0", true},
		{"newer patch gets through", "0.2.1", "0.2.0", false},
		{"newer minor gets through", "0.3.0", "0.2.0", false},
		{"older is still blocked", "0.1.9", "0.2.0", true},
		{"prerelease of the rejected build is blocked", "0.2.0-rc1", "0.2.0", true},
		// semverGreater treats an unparseable CURRENT as v0.0.0, so a
		// non-semver rejection would let everything through on the
		// compare alone — the exact-string check is what still holds.
		{"unparseable rejection still blocks its exact self", "dev", "dev", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rejectionBlocks(c.candidate, c.rejected); got != c.want {
				t.Errorf("rejectionBlocks(%q, %q) = %v, want %v", c.candidate, c.rejected, got, c.want)
			}
		})
	}
}
