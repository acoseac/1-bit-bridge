package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
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
// One read of a folder is out at a time (backupsReadOf): a run while one is
// out waits for it, and says it did not answer where it still has not, so a
// folder that never answers holds one goroutine, however often the doctor
// runs (backlog B653's fifth round).
func doctorBackups(cfg *config.Config) func(context.Context) []doctor.Check {
	root := filepath.Join(cfg.DataDir, backup.BackupsDirName)
	scheduled := cfg.Backup.EffectiveIntervalHours() > 0
	return func(ctx context.Context) []doctor.Check {
		var stuck *backup.StuckCheck
		if s, held := backup.StuckCheckHere(root); held {
			stuck = &s
		}
		read := backupsReadOf(root)
		t := time.NewTimer(doctorBackupsBound)
		defer t.Stop()
		select {
		case <-read.done:
		case <-t.C:
			return doctor.BackupChecks(backup.Status{}, fmt.Errorf("the backups folder did not answer within %s", doctorBackupsBound),
				nil, stuck, scheduled, time.Now())
		case <-ctx.Done():
			return doctor.BackupChecks(backup.Status{}, fmt.Errorf("the backups folder was not read: %w", ctx.Err()),
				nil, stuck, scheduled, time.Now())
		}
		r := read.r
		if stuck == nil {
			stuck = r.other
		}
		return doctor.BackupChecks(r.st, r.err, r.lockErr, stuck, scheduled, time.Now())
	}
}

// backupsReads is what one read of a backups folder found for the doctor.
type backupsReads struct {
	st      backup.Status
	err     error
	lockErr error
	other   *backup.StuckCheck
}

// backupsRead is one read of a backups folder, r set once done is closed.
type backupsRead struct {
	done chan struct{}
	r    backupsReads
}

// backupsReadsOut is the doctor's read of each backups folder still out, by
// the folder.
var (
	backupsReadsMu  sync.Mutex
	backupsReadsOut = map[string]*backupsRead{}
)

// backupsReadOf is the read of the backups folder at root still out, or one
// begun now: the listing within doctorBackupsBound (ReadStatus's own context,
// no caller's, so a run that stops waiting ends nothing another run waits
// for), the lock's probe, and another process's hold given up. It is
// forgotten once it ends.
func backupsReadOf(root string) *backupsRead {
	backupsReadsMu.Lock()
	defer backupsReadsMu.Unlock()
	if read, out := backupsReadsOut[root]; out {
		return read
	}
	read := &backupsRead{done: make(chan struct{})}
	backupsReadsOut[root] = read
	go func() {
		if end := onDoctorBackupsReadEnd; end != nil {
			defer end()
		}
		bounded, cancel := context.WithTimeout(context.Background(), doctorBackupsBound)
		defer cancel()
		st, err := backup.ReadStatus(bounded, root)
		r := backupsReads{st: st, err: err, lockErr: backup.ProbeBackupsLock(root, backup.ProbeByDoctor)}
		if s, held := backup.StuckCheckOf(root, backup.ProbeByDoctor); held {
			r.other = &s
		}
		backupsReadsMu.Lock()
		read.r = r
		delete(backupsReadsOut, root)
		backupsReadsMu.Unlock()
		close(read.done)
	}()
	return read
}
