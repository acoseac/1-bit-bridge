package updater

import "fmt"

// DisarmResult is what DisarmEarlyRollback did.
type DisarmResult struct {
	// Recorded says it recorded the version in the marker.
	Recorded bool
	// Unreadable is why the marker could not be read, when it removed it.
	Unreadable error
}

// DisarmEarlyRollback records durably in the install marker in dataDir that
// current, a version other than the backup's, is about to write the data dir
// while an install is armed (State.DataDirOpenedBy; ArmsDataDirRecord). From
// then on neither the early rollback (EarlyRollback) nor the boot rollback
// (BootInstallFailed) puts the backup back: what current writes, its store's
// migrations above all, may be what the backup's version does not know, and
// a restore would be a downgrade past it (backlog B613). serve calls it before
// its first write to the data dir (cmd/bridge), and the store's open before
// its first migration step, whichever command opens the store
// (internal/manifest; B613's round 2, Codex's finding 1). save writes the
// marker; nil is SaveState, which syncs the file and its directory.
//
// It does nothing where no install is armed or a version is recorded
// already. A marker it cannot read cannot be told an armed install's or not,
// so it is removed, durably, and the read's error returned in Unreadable for
// the caller to say: the boot rollback cannot read it either, and a later
// start that could read it again must not take it for an install no start of
// this version went on past. An error says the record or the removal did not
// land, durably, and is the caller's to refuse its write over.
func DisarmEarlyRollback(dataDir, current string, save func(dataDir string, st State) error) (DisarmResult, error) {
	st, err := LoadState(dataDir)
	if err != nil {
		if rmErr := ClearStateDurably(dataDir); rmErr != nil {
			return DisarmResult{}, fmt.Errorf("the install marker can neither be read (%v) nor removed (%w)", err, rmErr)
		}
		return DisarmResult{Unreadable: err}, nil
	}
	if !ArmsDataDirRecord(st, current) {
		return DisarmResult{}, nil
	}
	st.DataDirOpenedBy = current
	if save == nil {
		save = SaveState
	}
	if err := save(dataDir, st); err != nil {
		return DisarmResult{}, fmt.Errorf("could not record in the install marker that %s opened the data dir: %w", current, err)
	}
	return DisarmResult{Recorded: true}, nil
}
