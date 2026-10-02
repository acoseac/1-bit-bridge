package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
)

// TestServeWithBackupsOffTakesNoSnapshotAndFollowsItsCadenceLive boots the
// real serve with `backup.intervalHours: 0` and moves the cadence through
// PATCH /api/settings, asking the backup loop itself after each step
// (backlog B209).
//
// The ticker runs on every bridge since #769, so that 0 → N has a loop
// alive to notice, and runSweepLoop took its first pass before it read the
// interval: a bridge with backups switched off wrote a snapshot, and pruned
// the backups directory to backup.keep, at every boot, while the config
// docs, runServe's comment and the settings matrix all said 0 parks it.
//
// The settings matrix test checks what the PATCH REPORTS; this one checks
// the consumer, through the Jobs card's next run, which the loop writes
// before each wait: armed once the cadence is switched on, and cleared once
// it is switched off again. Neither switch takes a snapshot (a rearm
// re-reads the schedule, it never runs the work).
//
// No step waits for an absence. The loop is one goroutine, so once it has
// armed for the PATCH's cadence it has made its boot decision, and that
// decision cannot come after the PATCH: runServe starts the ticker's
// goroutine more than a thousand lines before the console listens, and the
// ticker settles for no time at all.
func TestServeWithBackupsOffTakesNoSnapshotAndFollowsItsCadenceLive(t *testing.T) {
	b := startConsoleBridge(t, "backup:\n  intervalHours: 0\n", nil)

	patchSettingLive(t, b.console, b.adminBase, "backupIntervalHours", 1, b.stderr)
	waitForBackupNextRun(t, b, true)
	requireNoBackupTaken(t, b, "with the cadence switched on")

	patchSettingLive(t, b.console, b.adminBase, "backupIntervalHours", 0, b.stderr)
	waitForBackupNextRun(t, b, false)
	requireNoBackupTaken(t, b, "with the cadence switched off again")
}

// backupCardView is the part of GET /api/jobs' backups card this file reads.
type backupCardView struct {
	Run *struct {
		LastStartedAt *time.Time `json:"lastStartedAt"`
		NextDueAt     *time.Time `json:"nextDueAt"`
	} `json:"run"`
}

// backupCard reads the Jobs card's backups block.
func backupCard(t *testing.T, b *consoleBridge) backupCardView {
	t.Helper()
	var jobs struct {
		Backups backupCardView `json:"backups"`
	}
	getJSON(t, b.console, b.adminBase+"/api/jobs", "", &jobs)
	if jobs.Backups.Run == nil {
		t.Fatalf("GET /api/jobs carries no backup run state; stderr=%s", b.stderr.String())
	}
	return jobs.Backups
}

// waitForBackupNextRun polls the Jobs card until its backups block shows a
// next run (armed) or none (parked), or until serve's waits give up
// (serveGiveUp).
func waitForBackupNextRun(t *testing.T, b *consoleBridge, armed bool) {
	t.Helper()
	deadline := serveGiveUpTime(t)
	for (backupCard(t, b).Run.NextDueAt != nil) != armed {
		if !deadline.IsZero() && time.Now().After(deadline) {
			t.Fatalf("the backup loop never showed armed=%t on the Jobs card before the test's deadline; "+
				"stdout=%s stderr=%s\nserve's goroutines:\n%s",
				armed, b.stdout.String(), b.stderr.String(), serveStacks())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// requireNoBackupTaken requires that no backup pass has run: nothing under
// the backups root, no `backup (` line on serve's stdout, and no pass on
// the Jobs card.
func requireNoBackupTaken(t *testing.T, b *consoleBridge, when string) {
	t.Helper()
	root := filepath.Join(b.dataDir, backup.BackupsDirName)
	entries, err := os.ReadDir(root)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read backups root: %v", err)
	}
	for _, e := range entries {
		t.Errorf("%s, with backups off since boot, the backups root holds %s", when, e.Name())
	}
	for _, line := range strings.Split(b.stdout.String(), "\n") {
		if strings.HasPrefix(line, "backup (") {
			t.Errorf("%s, with backups off since boot, serve printed %q", when, line)
		}
	}
	if run := backupCard(t, b).Run; run.LastStartedAt != nil {
		t.Errorf("%s, with backups off since boot, the Jobs card shows a backup pass started at %s",
			when, run.LastStartedAt)
	}
}
