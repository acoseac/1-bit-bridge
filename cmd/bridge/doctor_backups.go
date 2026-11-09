package main

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
)

// doctorBackupsBound is how long the doctor's backups lines wait for the
// backups folder to answer, a test's stand-in aside.
var doctorBackupsBound = 5 * time.Second

// onDoctorBackupsReadEnd, nil but in a test, runs as the doctor's read of
// the backups folder ends, on the goroutine it ran on, whether or not the
// doctor still waited for it: a test that stood a folder that does not
// answer in for the folder waits for it before it puts its stand-ins back.
var onDoctorBackupsReadEnd func()

// doctorBackups is the doctor's backups lines over the data dir cfg names
// (backlog B653), one source for `bridge doctor` and the console's doctor,
// which both build their Deps through buildDoctorDepsWith: the snapshots'
// manifests as backup.ReadStatus reads them, never a verification and never
// the live database, whether the backups folder grants a lock at all
// (backup.ProbeBackupsLock, which makes nothing and lets go at once), and
// whether a verification given up holds it still, worded by
// doctor.BackupChecks. This process's own verification given up is asked
// first, of no filesystem (backup.StuckCheckHere), so its line stands
// whatever the folder answers; every read of the folder after it (the
// listing, the lock's probe, another process's marker) is waited for
// doctorBackupsBound at most, and a folder that does not answer is said so.
func doctorBackups(cfg *config.Config) func(context.Context) []doctor.Check {
	root := filepath.Join(cfg.DataDir, backup.BackupsDirName)
	scheduled := cfg.Backup.EffectiveIntervalHours() > 0
	return func(ctx context.Context) []doctor.Check {
		var stuck *backup.StuckCheck
		if s, held := backup.StuckCheckHere(root); held {
			stuck = &s
		}
		type reads struct {
			st      backup.Status
			err     error
			lockErr error
			other   *backup.StuckCheck
		}
		bounded, cancel := context.WithTimeout(ctx, doctorBackupsBound)
		defer cancel()
		out := make(chan reads, 1)
		go func() {
			if end := onDoctorBackupsReadEnd; end != nil {
				defer end()
			}
			st, err := backup.ReadStatus(bounded, root)
			r := reads{st: st, err: err, lockErr: backup.ProbeBackupsLock(root)}
			if stuck == nil {
				if s, held := backup.StuckCheckOf(root); held {
					r.other = &s
				}
			}
			out <- r
		}()
		var r reads
		select {
		case r = <-out:
		case <-bounded.Done():
			return doctor.BackupChecks(backup.Status{}, fmt.Errorf("the backups folder did not answer within %s", doctorBackupsBound),
				nil, stuck, scheduled, time.Now())
		}
		if stuck == nil {
			stuck = r.other
		}
		return doctor.BackupChecks(r.st, r.err, r.lockErr, stuck, scheduled, time.Now())
	}
}
