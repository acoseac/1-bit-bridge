package upload

import (
	"os"
	"path/filepath"
	"time"
)

// Sweep removes abandoned staging directories.
//
// It enumerates the staging directory PHYSICALLY rather than iterating a list
// of sessions it knows about. A crash mid-commit, or a manifest that fails to
// parse, orphans files a state-driven sweeper would never look at — and those
// are exactly the ones nothing else will ever clean up.
//
// Age comes from the manifest's recorded CreatedAt, falling back to the
// directory's own mtime only for the orphan case where no manifest parses.
// It is never taken from a staged file's stat: see removeExpiredTrash for the
// same rule stated where it bites hardest.
func (m *Manager) Sweep() (removed int, err error) {
	cutoff := m.now().Add(-m.cfg.SessionTTL)
	for _, root := range m.roots() {
		base := filepath.Join(root, StagingDirName)
		entries, rerr := os.ReadDir(base)
		if rerr != nil {
			if os.IsNotExist(rerr) {
				continue
			}
			err = rerr
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				// A stray file directly under the staging root is debris by
				// construction — every session is a directory.
				_ = os.Remove(filepath.Join(base, e.Name()))
				continue
			}
			if !m.sessionExpired(root, e, cutoff) {
				continue
			}
			if rmErr := m.removeSession(root, e.Name()); rmErr != nil {
				logger.Warn("sweep staging dir", "root", root, "session", e.Name(), "err", rmErr)
				err = rmErr
				continue
			}
			removed++
			logger.Info("swept abandoned upload session", "session", e.Name())
		}
	}
	return removed, err
}

func (m *Manager) sessionExpired(root string, e os.DirEntry, cutoff time.Time) bool {
	var doc sessionDoc
	if rerr := readJSONFile(m.manifestPath(root, e.Name()), &doc); rerr == nil {
		return doc.CreatedAt.Before(cutoff)
	}
	// No readable manifest: an orphan. Fall back to the directory's own
	// mtime, which is meaningful here because the directory was created when
	// the session started.
	info, ierr := e.Info()
	if ierr != nil {
		return false
	}
	return info.ModTime().Before(cutoff)
}

// RunSweeper runs one pass immediately, then on a ticker until ctx is done.
//
// The startup pass is not optional: a crash leaves orphaned .part files, and a
// ticker-only sweeper waits a full period before noticing them.
func (m *Manager) RunSweeper(ctx interface{ Done() <-chan struct{} }, every time.Duration) {
	if every <= 0 {
		every = time.Hour
	}
	if _, err := m.Sweep(); err != nil {
		logger.Warn("upload sweep (startup)", "err", err)
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := m.Sweep(); err != nil {
				logger.Warn("upload sweep", "err", err)
			}
		}
	}
}
