package main

import (
	"context"
	"fmt"

	"github.com/acoseac/1-bit-bridge/internal/doctor"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// installProbationCheckName is the doctor's line on an install of this
// version that awaits its confirmation (backlog B660).
const installProbationCheckName = "install-probation"

// doctorInstallProbation is the doctor's install-probation line over the
// install marker in dataDir, one source for `bridge doctor` and the console's
// doctor, which both build their Deps through buildDoctorDepsWith, as the
// player-dac line is: updater.ProbationStatus's words for a reader beside the
// bridge (the count as the marker holds it, a start still running among it),
// for this binary's version. No line where no install of this version awaits
// its confirmation; ok on probation, a warn from two starts that have not
// ended cleanly, a fail at updater.ProbationFailedStarts, beside a crash
// rollback that was refused, or where a start did not answer as ready. It is
// the surface that works while serve cannot start. It reads the marker
// without its lock and writes nothing: a read for display decides nothing. A
// marker that cannot be read is a warn, not checked, naming no path.
func doctorInstallProbation(dataDir string) func(context.Context) []doctor.Check {
	return func(context.Context) []doctor.Check {
		st, err := updater.LoadState(dataDir)
		if err != nil {
			return []doctor.Check{{Name: installProbationCheckName, Status: doctor.Warn,
				Summary: fmt.Sprintf("not checked: the install marker cannot be read (%s)", fsutil.ErrorWithoutPath(err)),
				Hint:    "a start of bridge serve removes an install marker it cannot read, and says so"}}
		}
		p := updater.ProbationStatus(st, version.ServerVersion)
		switch p.Level {
		case updater.ProbationOK:
			return []doctor.Check{{Name: installProbationCheckName, Status: doctor.OK, Summary: p.Says}}
		case updater.ProbationWarn:
			return []doctor.Check{{Name: installProbationCheckName, Status: doctor.Warn, Summary: p.Says,
				Hint: "the bridge log says why each start ended; a start that stays up and answers confirms the install"}}
		case updater.ProbationFail:
			hint := "the bridge log says why each start ended; fix what stops them, or put the backup back by hand where that is safe"
			if st.NotReady && st.RollbackRefused == "" && st.FailedStarts < updater.ProbationFailedStarts {
				hint = "the bridge log says which answer was missing; a start that answers and stays up confirms the install"
			}
			return []doctor.Check{{Name: installProbationCheckName, Status: doctor.Fail, Summary: p.Says, Hint: hint}}
		}
		return nil
	}
}
