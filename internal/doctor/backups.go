package doctor

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// The backups' lines (backlog B653), read from the snapshots' manifests
// alone (backup.ReadStatus), never from a verification and never from the
// live database: backup-newest, the newest snapshot written, its age and its
// two verdicts with their reason codes; backup-verified, the newest snapshot
// whose database's verdict is VERIFIED, the recovery point, and its age. A
// snapshot is named by its folder's name, a time stamp, and no line names a
// path. cmd/bridge's doctorBackups feeds them, for bridge doctor and the
// console's doctor alike. And backup-lock, a warn shown only where the backups
// folder grants no lock (backup.ProbeBackupsLock answers ErrBackupsUnlocked),
// where nothing is verified or pruned (backlog B653's second round); and
// backup-check, a warn shown only while a check, a prune or a restore that did
// not end holds the backups folder (backup.StuckCheckOf, its third and fifth
// rounds; a restore's since backlog B826's second round).
// A hint that advises a bridge backup command gives its config as the
// placeholder <bridge.yaml>, and says to use the config this doctor read: the
// doctor names no path.
const (
	checkNameBackupNewest   = "backup-newest"
	checkNameBackupVerified = "backup-verified"
	checkNameBackupLock     = "backup-lock"
	checkNameBackupCheck    = "backup-check"
)

// backupsSyncsParentDir lets tests select the platform's directory barrier.
var backupsSyncsParentDir = fsutil.SyncsParentDir

// sameConfig is what a hint that advises a bridge backup command says of its
// --config.
const sameConfig = "<bridge.yaml> being the config this doctor read"

// BackupChecks are the backups' lines for st, the backups folder as its
// manifests read at now, or for err, the listing's failure; lockErr is what
// backup.ProbeBackupsLock answered for the folder; stuck, when not nil, the
// check that did not end and holds it still (backup.StuckCheckOf); scheduled
// says the bridge takes snapshots on a cadence.
func BackupChecks(st backup.Status, err, lockErr error, stuck *backup.StuckCheck, scheduled bool, now time.Time) []Check {
	var out []Check
	if errors.Is(lockErr, backup.ErrBackupsUnlocked) {
		out = append(out, warn(checkNameBackupLock,
			"no lock is granted on the backups folder, so no snapshot there is verified or pruned, and the scheduled snapshot waits while the newest one owes its check",
			"put the data directory on a filesystem that grants file locks: a local disk, or a network mount whose lock service runs"))
	}
	if stuck != nil {
		step := ""
		if stuck.Step != "" {
			step = " (" + stuck.Step + ")"
		}
		what := "a verification of snapshot " + stuck.Snapshot
		hint := "the end of the process that runs it ends it: restart the bridge, or stop the backup --verify command that runs it; if it comes back, the data directory's storage is not answering (backlog B653)"
		switch {
		case stuck.Restoring():
			// A restore's (backlog B826): the bridge restore command that runs
			// it ends it, never a restart of the bridge, which runs none.
			what = "a restore of snapshot " + stuck.Snapshot
			hint = "the end of the bridge restore command that runs it ends it: stop that command; if it comes back, the storage that holds the snapshots is not answering"
		case stuck.Probing():
			// A probe of the lock whose release did not end (backlog B871): this
			// process's own, the doctor's or the scheduled snapshot's, marked in
			// this process alone, never a restore's.
			what = stuck.Probe
			hint = "the storage that holds the snapshots stopped answering as the probe let go of the lock it tried: the hold ends when that storage answers or the process that made the probe ends; check that storage (a mount that stopped answering), and restart the bridge if the hold does not end"
		case stuck.Pruning():
			what = "a prune of the snapshots"
		}
		out = append(out, warn(checkNameBackupCheck,
			fmt.Sprintf("%s did not end%s and was given up %s, and still holds the backups folder: no snapshot there is verified or pruned until it ends, and the scheduled snapshot waits",
				what, step, agoWords(now.Sub(stuck.Since))),
			hint))
	}
	if err != nil {
		return append(out, warn(checkNameBackupNewest,
			fmt.Sprintf("not checked: the snapshots could not be listed (%s)", fsutil.ErrorWithoutPath(err)),
			"give the user the bridge runs as read access to the backups folder in its data directory"))
	}
	newest, found := st.Newest()
	if !found {
		if !scheduled {
			return append(out, ok(checkNameBackupNewest, "no snapshot: scheduled snapshots are off"))
		}
		return append(out, warn(checkNameBackupNewest, "no snapshot yet",
			"bridge serve takes one at its next start; or run bridge backup --config <bridge.yaml>, "+sameConfig))
	}
	return append(out, newestLine(newest, now), verifiedLine(st, now))
}

// newestLine is backup-newest: ok while both verdicts of the newest snapshot
// are VERIFIED (its database UNVERIFIED where the install had none to
// capture), or its check is under way, and a warn naming what failed
// otherwise.
func newestLine(sn backup.SnapshotStatus, now time.Time) Check {
	v := sn.Verdict
	summary := fmt.Sprintf("newest snapshot %s, %s: database %s, files %s", sn.DirName, agoWords(now.Sub(sn.CreatedAt)),
		backup.StateWords(v.DatabaseState, v.DatabaseReasons), backup.FilesWords(v))
	switch {
	case v.DatabaseState == backup.StatePending || v.FilesState == backup.StatePending:
		return ok(checkNameBackupNewest, summary+" (its verification is under way)")
	case v.Passes():
		return ok(checkNameBackupNewest, summary)
	case sn.Legacy:
		return warn(checkNameBackupNewest, summary,
			"it was written before snapshots were verified; the next snapshot is verified as it is taken")
	case v.DatabaseState == backup.StateFailed:
		return warn(checkNameBackupNewest, summary,
			"its database failed its check: the live database may be damaged; run bridge backup --config <bridge.yaml> --verify <snapshot-dir> --full, "+sameConfig+", and keep the snapshots that are verified")
	case !backupsSyncsParentDir && (slices.Contains(v.DatabaseReasons, backup.ReasonUnsynced) || slices.Contains(v.FilesReasons, backup.ReasonUnsynced)):
		return warn(checkNameBackupNewest, summary,
			"this record cannot be trusted or re-recorded on this platform; take a fresh snapshot with bridge backup --config <bridge.yaml>, "+sameConfig)
	case v.DatabaseState == backup.StateIncomplete || v.FilesState == backup.StateIncomplete:
		return warn(checkNameBackupNewest, summary,
			"its check did not finish; run bridge backup --config <bridge.yaml> --verify <snapshot-dir> on it, "+sameConfig+", or wait for the next snapshot")
	case v.DatabaseState == backup.StateVerified && v.FilesState == backup.StateFailed:
		return warn(checkNameBackupNewest, summary,
			"a file of the snapshot is not the one it captured; its database is still a recovery point, and the next snapshot captures the files again")
	case v.DatabaseState == backup.StateUnverified && slices.Contains(v.DatabaseReasons, backup.ReasonNoDatabase) && v.FilesState == backup.StateFailed:
		return warn(checkNameBackupNewest, summary,
			"a file of the snapshot is not the one it captured; the next snapshot captures the files again")
	}
	return warn(checkNameBackupNewest, summary,
		"run bridge backup --config <bridge.yaml> to take and check a new snapshot, "+sameConfig)
}

// verifiedLine is backup-verified: ok naming the newest snapshot whose
// database is VERIFIED, a fail where none is and one has FAILED, and a warn
// where none is otherwise.
func verifiedLine(st backup.Status, now time.Time) Check {
	if sn, found := st.NewestVerified(); found {
		level := ""
		if sn.Verdict.Level != "" {
			level = fmt.Sprintf(" (%s check)", sn.Verdict.Level)
		}
		return ok(checkNameBackupVerified, fmt.Sprintf("newest database-verified snapshot %s, %s%s", sn.DirName,
			agoWords(now.Sub(sn.CreatedAt)), level))
	}
	for _, sn := range st.Snapshots {
		if sn.Verdict.DatabaseState == backup.StateFailed {
			return fail(checkNameBackupVerified, "no snapshot's database is verified, and one has failed its check",
				"the live database may be damaged: run bridge backup --config <bridge.yaml>, then bridge backup --config <bridge.yaml> --verify <snapshot-dir> --full on the snapshot it writes, "+sameConfig)
		}
	}
	return warn(checkNameBackupVerified, "no snapshot's database is verified yet",
		"the next snapshot is verified as it is taken; or run bridge backup --config <bridge.yaml>, "+sameConfig)
}

// agoWords says how long ago d was, in the largest whole unit.
func agoWords(d time.Duration) string {
	switch {
	case d < 0:
		return "written in the future by this host's clock"
	case d < time.Minute:
		return "less than a minute ago"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d/time.Hour))
	}
	return fmt.Sprintf("%d days ago", int(d/(24*time.Hour)))
}
