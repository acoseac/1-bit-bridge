package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// restoreOriginalsLine runs bridge doctor's checks over cfgPath's install,
// as the console's doctor builds them too (buildDoctorDepsWith), with managed
// as the config says, and answers its restore-originals line.
func restoreOriginalsLine(t *testing.T, cfgPath string, managed bool) doctor.Check {
	t.Helper()
	d := buildDoctorDepsWith(cfgPath, false, doctorDACHost(), doctorLogs{host: hostLogs()})
	if d.RestoreOriginals == nil {
		t.Fatal("the doctor's Deps carry no restore-originals line")
	}
	d.Managed = managed
	for _, c := range doctor.Run(t.Context(), d).Checks {
		if c.Name == checkNameRestoreOriginals {
			return c
		}
	}
	t.Fatal("the report has no restore-originals line")
	return doctor.Check{}
}

// TestTheRestoreOriginalsLineSaysWhatIsKeptAndWhatIsUnfinished is T34: both
// doctors' restore-originals line says, ok, that no database-only restore
// kept originals, then the sets kept and their size; fails while an
// unfinished restore's journal stands, its hint the remedy every surface
// says (the command with this config, the folder of originals, the by-hand
// section); warns while a restore holds the store, and, not checked, where
// its lock cannot be had; is skipped on a managed bridge; and holds the
// store's lock across the reads it classifies, so a restore that comes while
// it reads is refused.
func TestTheRestoreOriginalsLineSaysWhatIsKeptAndWhatIsUnfinished(t *testing.T) {
	cfgPath, dataDir, _ := backupInstall(t)
	if c := restoreOriginalsLine(t, cfgPath, false); c.Status != doctor.OK || !strings.Contains(c.Summary, "no database-only restore has kept originals") {
		t.Errorf("with nothing kept: %+v", c)
	}
	run := filepath.Join(dataDir, backup.OriginalsDirName, "2026-11-17T05-00-00Z")
	if err := os.MkdirAll(run, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, size := range map[string]int{"bridge.db": 4096, "bridge.db-wal": 100, "restore.json": 7} {
		if err := os.WriteFile(filepath.Join(run, name), make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if c := restoreOriginalsLine(t, cfgPath, false); c.Status != doctor.OK || !strings.Contains(c.Summary, "1 set(s)") || !strings.Contains(c.Summary, "4203 bytes") {
		t.Errorf("with a set kept: %+v", c)
	}
	if c := restoreOriginalsLine(t, cfgPath, true); c.Status != doctor.OK || !strings.Contains(c.Summary, "check skipped") {
		t.Errorf("on a managed bridge: %+v", c)
	}

	t.Run("a restore under way", func(t *testing.T) {
		restore := manifest.SetStoreLockTryForTest(func(*os.File, bool) error { return fsutil.ErrLocked })
		c := restoreOriginalsLine(t, cfgPath, false)
		restore()
		if c.Status != doctor.Warn || !strings.Contains(c.Summary, "under way") {
			t.Errorf("beside a restore that holds the store: %+v", c)
		}
	})
	t.Run("a lock that cannot be had", func(t *testing.T) {
		restore := manifest.SetStoreLockTryForTest(func(*os.File, bool) error { return errors.New("the lock manager did not answer") })
		c := restoreOriginalsLine(t, cfgPath, false)
		restore()
		if c.Status != doctor.Warn || !strings.HasPrefix(c.Summary, "not checked:") {
			t.Errorf("where the lock cannot be had: %+v", c)
		}
	})
	t.Run("held across its reads", func(t *testing.T) {
		parked, release := make(chan struct{}), make(chan struct{})
		hook := func() {
			close(parked)
			<-release
		}
		onRestoreOriginalsHeld.Store(&hook)
		done := make(chan doctor.Check, 1)
		go func() { done <- restoreOriginalsCheck(t.Context(), cfgPath, dataDir) }()
		select {
		case <-parked:
		case <-time.After(30 * time.Second):
			t.Fatal("the line never reached its reads")
		}
		h, err := manifest.HoldStoreExclusive(manifest.DefaultDBPath(dataDir))
		if err == nil {
			h.Release()
		}
		close(release)
		<-done
		onRestoreOriginalsHeld.Store(nil)
		if !errors.Is(err, manifest.ErrStoreHeld) {
			t.Errorf("a restore while the line reads = %v, want it refused (the line holds the store)", err)
		}
	})
	t.Run("an unfinished restore", func(t *testing.T) {
		standJournal(t, dataDir)
		t.Cleanup(func() { _ = os.Remove(manifest.RestoreJournalPath(manifest.DefaultDBPath(dataDir))) })
		c := restoreOriginalsLine(t, cfgPath, false)
		if c.Status != doctor.Fail || !strings.Contains(c.Hint, "bridge restore --database-only --config "+restoreConfigArg(cfgPath)) ||
			!strings.Contains(c.Hint, filepath.Join(dataDir, backup.OriginalsDirName)) || !strings.Contains(c.Hint, restoreHandBackSection) {
			t.Errorf("with a journal standing: %+v", c)
		}
	})
}
