package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// checkNameRestoreOriginals is the doctor's line on the database-only
// restore (backlog B653 PR 2b1).
const checkNameRestoreOriginals = "restore-originals"

// onRestoreOriginalsHeld, nil but in a test, runs once the restore-originals
// line holds the store's lifetime lock and before it reads anything: a test
// parks there to show the hold stands across the reads it classifies.
var onRestoreOriginalsHeld atomic.Pointer[func()]

// doctorRestoreOriginals is the doctor's restore-originals line over the data
// dir cfgPath's config names, one source for bridge doctor and the console's
// doctor (buildDoctorDepsWith; backlog B653 PR 2b1 §4.8): a fail while an
// unfinished restore of the library database stands, its words the remedy
// every surface says (restoreRemedy); a warn while a restore holds the store
// (manifest.ErrStoreRestoring) and where the store's lock could not be had,
// not checked; and ok otherwise, naming the sets of originals the database-
// only restores kept and their size. With no data dir it holds nothing and
// says so; a data dir its record refuses (doctorStoreGate) is not checked and
// nothing is made in it, since the hold makes its lock file; and where
// neither a journal nor a folder of originals stands, by Lstat, there is
// nothing to classify and nothing is held or made (an ordinary doctor run
// changes nothing in the data dir). Otherwise, under the hold,
// taken shared (manifest.HoldLiveStore) and let go only once the reads it
// classifies have ended, it reads the journal as the restore's recovery reads
// it and lists the folder of originals, on a goroutine of its own, waited for
// doctorBackupsBound at most: a read that does not answer is said so, and its
// goroutine lets the hold go when it ends. A managed bridge skips the line
// (doctor.Run).
func doctorRestoreOriginals(cfgPath, dataDir string) func(context.Context) []doctor.Check {
	return func(ctx context.Context) []doctor.Check {
		return []doctor.Check{restoreOriginalsCheck(ctx, cfgPath, dataDir)}
	}
}

// restoreOriginalsCheck is doctorRestoreOriginals' one line.
func restoreOriginalsCheck(ctx context.Context, cfgPath, dataDir string) doctor.Check {
	name := checkNameRestoreOriginals
	if _, err := os.Stat(dataDir); errors.Is(err, fs.ErrNotExist) {
		return doctor.Check{Name: name, Status: doctor.OK, Summary: "no data directory yet, so no restore of the library database to report"}
	}
	if err := doctorStoreGate(cfgPath, dataDir); err != nil {
		return doctor.Check{Name: name, Status: doctor.Warn, Summary: "not checked: " + err.Error()}
	}
	_, journalErr := os.Lstat(manifest.RestoreJournalPath(manifest.DefaultDBPath(dataDir)))
	_, archiveErr := os.Lstat(filepath.Join(dataDir, backup.OriginalsDirName))
	if errors.Is(journalErr, fs.ErrNotExist) && errors.Is(archiveErr, fs.ErrNotExist) {
		return doctor.Check{Name: name, Status: doctor.OK, Summary: "no database-only restore has kept originals"}
	}
	release, err := manifest.HoldLiveStore(manifest.DefaultDBPath(dataDir))
	switch {
	case errors.Is(err, manifest.ErrStoreRestoring):
		return doctor.Check{Name: name, Status: doctor.Warn, Summary: "a restore of the library database is under way: it holds the database",
			Hint: "let it end, then run the doctor again"}
	case err != nil:
		return doctor.Check{Name: name, Status: doctor.Warn, Summary: "not checked: " + fsutil.ErrorWithoutPath(err)}
	}
	out := make(chan doctor.Check, 1)
	go func() {
		defer release()
		if fn := onRestoreOriginalsHeld.Load(); fn != nil {
			(*fn)()
		}
		out <- readRestoreOriginals(cfgPath, dataDir)
	}()
	t := time.NewTimer(doctorBackupsBound)
	defer t.Stop()
	select {
	case c := <-out:
		return c
	case <-t.C:
		return doctor.Check{Name: name, Status: doctor.Warn, Summary: fmt.Sprintf("not checked: the data directory did not answer within %s", doctorBackupsBound)}
	case <-ctx.Done():
		return doctor.Check{Name: name, Status: doctor.Warn, Summary: "not checked: " + ctx.Err().Error()}
	}
}

// readRestoreOriginals is the line's reads, under the hold: the journal, then
// the folder of originals.
func readRestoreOriginals(cfgPath, dataDir string) doctor.Check {
	name := checkNameRestoreOriginals
	j, err := backup.ReadRestoreJournal(dataDir)
	switch {
	case err == nil:
		return doctor.Check{Name: name, Status: doctor.Fail,
			Summary: fmt.Sprintf("an unfinished restore of the library database stands (its journal at phase %s, of %s): every start of the bridge refuses until it is finished",
				j.Phase, j.Snapshot.Given),
			Hint: restoreRemedy(cfgPath, dataDir)}
	case !errors.Is(err, backup.ErrNoRestoreJournal):
		return doctor.Check{Name: name, Status: doctor.Fail,
			Summary: "an unfinished restore of the library database stands: " + err.Error(),
			Hint:    restoreRemedy(cfgPath, dataDir)}
	}
	root := filepath.Join(dataDir, backup.OriginalsDirName)
	runs, err := os.ReadDir(root)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return doctor.Check{Name: name, Status: doctor.OK, Summary: "no database-only restore has kept originals"}
	case err != nil:
		return doctor.Check{Name: name, Status: doctor.Warn, Summary: "not checked: " + fsutil.ErrorWithoutPath(err)}
	}
	sets, total := 0, int64(0)
	for _, r := range runs {
		if !r.IsDir() {
			continue
		}
		sets++
		files, err := os.ReadDir(filepath.Join(root, r.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if fi, err := f.Info(); err == nil && fi.Mode().IsRegular() {
				total += fi.Size()
			}
		}
	}
	if sets == 0 {
		return doctor.Check{Name: name, Status: doctor.OK, Summary: "no database-only restore has kept originals"}
	}
	return doctor.Check{Name: name, Status: doctor.OK,
		Summary: fmt.Sprintf("%d set(s) of a restored database's originals kept in %s, %d bytes; remove a set once you no longer need it", sets, root, total)}
}
