package backup

import "strings"

// StateWords is a verdict with its reason codes, as every surface shows it
// (bridge backup, the doctor, the console): "VERIFIED", or
// "FAILED: hash-mismatch".
func StateWords(s State, reasons []Reason) string {
	if len(reasons) == 0 {
		return string(s)
	}
	words := make([]string, 0, len(reasons))
	for _, r := range reasons {
		words = append(words, string(r))
	}
	return string(s) + ": " + strings.Join(words, ", ")
}

// FilesWords is v's files' verdict with its reason codes and the files that
// failed, each by its name in the snapshot, never a path:
// "FAILED: hash-mismatch (server.crt)".
func FilesWords(v Verification) string {
	s := StateWords(v.FilesState, v.FilesReasons)
	var failed []string
	for _, f := range v.FilesFailed {
		if f.Name != "" {
			failed = append(failed, f.Name)
		}
	}
	if len(failed) > 0 {
		s += " (" + strings.Join(failed, ", ") + ")"
	}
	return s
}

// Passes reports whether v is a verified recovery point with every file as
// captured: both verdicts VERIFIED, the database's UNVERIFIED only where the
// snapshot holds none because the install had none (ReasonNoDatabase).
func (v Verification) Passes() bool {
	noDatabase := v.DatabaseState == StateUnverified && len(v.DatabaseReasons) == 1 && v.DatabaseReasons[0] == ReasonNoDatabase
	return (v.DatabaseState == StateVerified || noDatabase) && v.FilesState == StateVerified
}

// CancelledOnly reports whether neither verdict failed, at least one is
// INCOMPLETE, and every INCOMPLETE has cancellation as its only reason.
// An aborted check or an expired deadline must still be reported at shutdown.
func (v Verification) CancelledOnly() bool {
	if v.DatabaseState == StateFailed || v.FilesState == StateFailed {
		return false
	}
	databaseIncomplete := v.DatabaseState == StateIncomplete
	filesIncomplete := v.FilesState == StateIncomplete
	if databaseIncomplete && (len(v.DatabaseReasons) != 1 || v.DatabaseReasons[0] != ReasonCancelled) {
		return false
	}
	if filesIncomplete && (len(v.FilesReasons) != 1 || v.FilesReasons[0] != ReasonCancelled) {
		return false
	}
	return databaseIncomplete || filesIncomplete
}
