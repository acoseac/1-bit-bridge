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
// A readable session is idle once its last accepted chunk (fileState.UpdatedAt
// on each file's meta; the manifest stays immutable) is older than SessionTTL.
// A meta written before that field existed falls back to the meta file's
// mtime, then to CreatedAt. The session also expires once CreatedAt is older
// than sessionMaxAge, or older than SessionTTL when that window is longer.
// An orphan whose manifest does not parse falls back to the directory's own
// mtime against the idle window. Age is never taken from a staged payload's
// stat: see removeExpiredTrash for the same rule stated where it bites hardest.
func (m *Manager) Sweep() (removed int, err error) {
	now := m.now()
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
			if !m.sessionExpired(root, e, now) {
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

// sessionMaxAge is the wall-clock bound on a session, whatever its last
// chunk. The idle window is SessionTTL (default 24h). Without a bound, one
// byte an hour holds staged bytes forever. Seven days is the library trash
// window: uncommitted staged bytes do not outlive bytes the operator already
// deleted. When SessionTTL is longer than this, the bound is SessionTTL, so
// the bound never fires inside the idle window the operator set.
const sessionMaxAge = 7 * 24 * time.Hour

func (m *Manager) sessionExpired(root string, e os.DirEntry, now time.Time) bool {
	idle := now.Add(-m.cfg.SessionTTL)
	var doc sessionDoc
	if rerr := readJSONFile(m.manifestPath(root, e.Name()), &doc); rerr != nil {
		// No readable manifest: an orphan. Fall back to the directory's own
		// mtime, which is meaningful here because the directory was created
		// when the session started.
		info, ierr := e.Info()
		if ierr != nil {
			return false
		}
		return info.ModTime().Before(idle)
	}
	bound := sessionMaxAge
	if m.cfg.SessionTTL > bound {
		bound = m.cfg.SessionTTL
	}
	if !doc.CreatedAt.IsZero() && doc.CreatedAt.Before(now.Add(-bound)) {
		return true
	}
	activity, ok := m.sessionActivity(root, e.Name(), doc)
	if !ok {
		return false
	}
	return activity.Before(idle)
}

// sessionActivity is the latest accepted chunk across the session's files.
// A zero UpdatedAt is a meta from before the field existed, and the file's
// mtime stands in for it. A missing meta means that file has taken no chunk
// yet. ok is false when a meta cannot be read: the idle check then stands
// down, and the absolute cap above still applies.
func (m *Manager) sessionActivity(root, sid string, doc sessionDoc) (time.Time, bool) {
	latest := doc.CreatedAt
	ok := true
	for _, f := range doc.Files {
		st, err := m.readState(root, sid, f.ID)
		if err != nil {
			ok = false
			continue
		}
		if !st.UpdatedAt.IsZero() {
			if st.UpdatedAt.After(latest) {
				latest = st.UpdatedAt
			}
			continue
		}
		info, err := os.Stat(m.statePath(root, sid, f.ID))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			ok = false
			continue
		}
		if info.ModTime().After(latest) {
			latest = info.ModTime()
		}
	}
	return latest, ok
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
