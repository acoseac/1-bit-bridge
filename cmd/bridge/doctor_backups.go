package main

import (
	"context"
	"path/filepath"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
)

// doctorBackups is the doctor's backups lines over the data dir cfg names
// (backlog B653), one source for `bridge doctor` and the console's doctor,
// which both build their Deps through buildDoctorDepsWith: the snapshots'
// manifests as backup.ReadStatus reads them, never a verification and never
// the live database, whether the backups folder grants a lock at all
// (backup.ProbeBackupsLock, which makes nothing and lets go at once), and
// whether a check that did not end holds it still (backup.StuckCheckOf, this
// process's or another's), worded by doctor.BackupChecks.
func doctorBackups(cfg *config.Config) func(context.Context) []doctor.Check {
	root := filepath.Join(cfg.DataDir, backup.BackupsDirName)
	scheduled := cfg.Backup.EffectiveIntervalHours() > 0
	return func(ctx context.Context) []doctor.Check {
		st, err := backup.ReadStatus(ctx, root)
		var stuck *backup.StuckCheck
		if s, held := backup.StuckCheckOf(root); held {
			stuck = &s
		}
		return doctor.BackupChecks(st, err, backup.ProbeBackupsLock(root), stuck, scheduled, time.Now())
	}
}
