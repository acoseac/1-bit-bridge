package main

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/acoseac/1-bit-bridge/internal/doctor"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// installProbationCheckName is the doctor's line on an install of this
// version that awaits its confirmation (backlog B660).
const installProbationCheckName = "install-probation"

// storeMigrationsCheckName is the doctor's line on a store whose migrations
// it leaves pending (backlog B660's second round).
const storeMigrationsCheckName = "store-migrations"

// doctorInstallProbation is the doctor's install-probation line over the
// install marker in dataDir, one source for `bridge doctor` and the console's
// doctor, which both build their Deps through buildDoctorDepsWith, as the
// player-dac line is: updater.ProbationStatus's words for a reader beside the
// bridge, for this binary's version, the start of a serve that holds the data
// dir's lock now taken off the count (updater.ServeHoldsDataDir; the second
// round: until then one start was taken off whether or not a serve ran). No
// line where no install of this version awaits its confirmation; ok on
// probation, a warn from one start that did not end cleanly, a fail at
// updater.ProbationFailedStarts, beside a crash rollback that was refused, or
// where a start did not answer as ready. It is the surface that works while
// serve cannot start. It reads the marker without its lock and writes
// nothing: a read for display decides nothing. A marker that cannot be read
// is a warn, not checked, naming no path.
//
// Beside it, the store-migrations line, where a command may not migrate the
// store (updater.MigrationHeld) and it is behind this version's ladder: a
// warn saying how many migrations are pending, which bridge doctor leaves to
// serve's own start, as it leaves the checks that read the store
// (doctorOpenStore).
func doctorInstallProbation(dataDir string) func(context.Context) []doctor.Check {
	return func(context.Context) []doctor.Check {
		st, err := updater.LoadState(dataDir)
		if err != nil {
			return []doctor.Check{{Name: installProbationCheckName, Status: doctor.Warn,
				Summary: fmt.Sprintf("not checked: the install marker cannot be read (%s)", fsutil.ErrorWithoutPath(err)),
				Hint:    "a start of bridge serve removes an install marker it cannot read, and says so"}}
		}
		running := updater.ServeHoldsDataDir(dataDir)
		var out []doctor.Check
		p := updater.ProbationStatus(st, version.ServerVersion, running)
		switch p.Level {
		case updater.ProbationOK:
			out = append(out, doctor.Check{Name: installProbationCheckName, Status: doctor.OK, Summary: p.Says})
		case updater.ProbationWarn:
			out = append(out, doctor.Check{Name: installProbationCheckName, Status: doctor.Warn, Summary: p.Says,
				Hint: "the bridge log says why each start ended; a start that stays up and answers confirms the install"})
		case updater.ProbationFail:
			hint := "the bridge log says why each start ended; fix what stops them, or put the backup back by hand where that is safe"
			if st.NotReady && st.RollbackRefused == "" && st.FailedStarts-boolInt(running) < updater.ProbationFailedStarts {
				hint = "the bridge log says which answer was missing; a start that answers and stays up confirms the install"
			}
			out = append(out, doctor.Check{Name: installProbationCheckName, Status: doctor.Fail, Summary: p.Says, Hint: hint})
		}
		if why := updater.MigrationHeld(st, version.ServerVersion, running); why != "" {
			if line, ok := storeMigrationsLine(manifest.DefaultDBPath(dataDir), why); ok {
				out = append(out, line)
			}
		}
		return out
	}
}

// boolInt is 1 for true, 0 for false.
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// migrationsPending says n pending migrations in words.
func migrationsPending(n int) string {
	if n == 1 {
		return "1 migration pending"
	}
	return fmt.Sprintf("%d migrations pending", n)
}

// storeMigrationsLine is the store-migrations line for the store at dbPath,
// whose migrations a command may not run for why: none where none is
// pending.
func storeMigrationsLine(dbPath, why string) (doctor.Check, bool) {
	n, err := manifest.PendingMigrations(dbPath)
	switch {
	case err != nil:
		return doctor.Check{Name: storeMigrationsCheckName, Status: doctor.Warn,
			Summary: fmt.Sprintf("not checked: the store's version could not be read (%s)", fsutil.ErrorWithoutPath(err))}, true
	case n == 0:
		return doctor.Check{}, false
	}
	return doctor.Check{Name: storeMigrationsCheckName, Status: doctor.Warn,
		Summary: fmt.Sprintf("%s, which bridge doctor runs none of: %s", migrationsPending(n), why),
		Hint:    "start bridge serve, whose own start migrates the store; the checks that read the store wait for it"}, true
}

// doctorOpenStore opens the store at dbPath for one of the doctor's probes,
// as manifest.OpenStore does, except where a command may not migrate it
// (updater.MigrationHeld) and it is behind this version's ladder: then it
// opens nothing and says why, which the probe's line carries, the
// store-migrations line saying the rest (backlog B660's second round). The
// store's own open would refuse too, and run no step; asked here, the probes
// say it in a few words.
func doctorOpenStore(dbPath string) (*manifest.Store, error) {
	dataDir := filepath.Dir(dbPath)
	if st, err := updater.LoadState(dataDir); err == nil {
		if why := updater.MigrationHeld(st, version.ServerVersion, updater.ServeHoldsDataDir(dataDir)); why != "" {
			if n, err := manifest.PendingMigrations(dbPath); err == nil && n > 0 {
				return nil, fmt.Errorf("the store has %s, which bridge doctor does not run while the install awaits its confirmation "+
					"(the %s line)", migrationsPending(n), storeMigrationsCheckName)
			}
		}
	}
	return manifest.OpenStore(dbPath)
}
