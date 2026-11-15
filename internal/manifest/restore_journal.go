package manifest

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// restoreJournalSuffix names the journal of a restore of the database, beside
// it: written by B653 PR 2b's database-only restore before its first rename
// and removed once the restore has finished, and refused, while any entry
// stands at its name, by every opener of the live database before it makes
// anything, by the live snapshot, by the full restore before its first write,
// and by serve's crash and early rollbacks before any marker write or rename
// (B653 PR 2a, which writes none: until 2b these checks are inert, each
// tested with a journal its test writes).
const restoreJournalSuffix = "-restore.json"

// RestoreJournalPath is where a restore of the database at dbPath keeps its
// journal.
func RestoreJournalPath(dbPath string) string { return dbPath + restoreJournalSuffix }

// ErrRestoreIncomplete is the refusal while a restore's journal stands: an
// unfinished restore of the library database. Its words name the journal by
// its base name; PR 2b changes them to name the command that finishes it.
var ErrRestoreIncomplete = errors.New("an unfinished restore of the library database stands")

// errRestoreJournalUnread is the refusal where whether a journal stands could
// not be read: Lstat answered another error than not-exist.
var errRestoreJournalUnread = errors.New("whether an unfinished restore of the library database stands could not be read")

// AdmissionRefused reports whether err, what an opener of the live database
// answered (OpenStore, PendingMigrations), is a refusal its admission made
// before anything of the database was touched: the lifetime lock's
// (ErrStoreRestoring, ErrStoreLockUnavailable) or the restore journal's
// (ErrRestoreIncomplete, or a journal whose standing could not be read).
// The doctor says "not checked" for one (backlog B653 PR 2a).
func AdmissionRefused(err error) bool {
	return errors.Is(err, ErrStoreRestoring) || errors.Is(err, ErrStoreLockUnavailable) ||
		errors.Is(err, ErrRestoreIncomplete) || errors.Is(err, errRestoreJournalUnread)
}

// CheckRestoreJournal answers nil where nothing stands at the journal's name
// beside the database at dbPath, and a refusal otherwise: any entry there, a
// file, a directory, a link (to anything or to nothing), is a journal
// standing (ErrRestoreIncomplete), and an Lstat that answers another error
// than not-exist refuses too, since it cannot show that none does. Nothing is
// opened or made.
func CheckRestoreJournal(dbPath string) error {
	path := RestoreJournalPath(dbPath)
	name := filepath.Base(path)
	_, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err == nil:
		return fmt.Errorf("%w (%s): finish it with the bridge version that began it", ErrRestoreIncomplete, name)
	}
	return fmt.Errorf("%w (%s): %s", errRestoreJournalUnread, name, fsutil.ErrorWithoutPath(err))
}
