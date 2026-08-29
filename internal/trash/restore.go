package trash

import (
	"os"
	"path"
	"path/filepath"
	"sort"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// Restore moves entries back to their original paths.
func (m *Manager) Restore(ids []string) (*Result, error) {
	if !m.on() {
		return nil, ErrDisabled
	}
	res := &Result{}
	dirs := map[string]struct{}{}
	for _, id := range ids {
		out := Outcome{Path: id}
		stamp, rel, err := splitID(id)
		if err != nil {
			out.Status, out.Reason = "failed", err.Error()
			res.Failed++
			res.Outcomes = append(res.Outcomes, out)
			continue
		}
		out.Path = rel
		root, src, ok := m.locate(stamp, rel)
		if !ok {
			out.Status, out.Reason = "failed", "no such entry"
			res.Failed++
			res.Outcomes = append(res.Outcomes, out)
			continue
		}
		res.Root = root
		dst := filepath.Join(root, filepath.FromSlash(rel))
		if fsutil.IsUnderAny(dst, []string{root}) == "" {
			out.Status, out.Reason = "failed", "resolves outside the library root"
			res.Failed++
			res.Outcomes = append(res.Outcomes, out)
			continue
		}
		if _, serr := os.Stat(dst); serr == nil {
			out.Status, out.Reason = "failed", "a file already exists at the original path"
			res.Failed++
			res.Outcomes = append(res.Outcomes, out)
			continue
		}
		// MkdirAll FIRST. The directory a track came from may have been
		// removed after it was trashed — by the operator, or by trashing its
		// last sibling — so a bare rename back returns ENOENT on exactly the
		// album-was-fully-deleted case restore exists for.
		//
		// 0o755, matching the upload commit path: this is the user's music,
		// not the bridge's own state, and 0o700 would break a shared mount.
		if mkErr := os.MkdirAll(filepath.Dir(dst), 0o755); mkErr != nil {
			out.Status, out.Reason = "failed", mkErr.Error()
			res.Failed++
			res.Outcomes = append(res.Outcomes, out)
			continue
		}
		if rnErr := atomicwrite.RenameWithRetry(src, dst); rnErr != nil {
			out.Status, out.Reason = "failed", rnErr.Error()
			res.Failed++
			res.Outcomes = append(res.Outcomes, out)
			continue
		}
		if info, ierr := os.Stat(dst); ierr == nil {
			out.Bytes = info.Size()
			res.Bytes += info.Size()
		}
		out.Status = "restored"
		res.OK++
		res.Paths = append(res.Paths, rel)
		if d := path.Dir(rel); d != "." {
			dirs[d] = struct{}{}
		}
		res.Outcomes = append(res.Outcomes, out)
		m.pruneEmptyStamp(root, stamp)
	}
	for d := range dirs {
		res.Dirs = append(res.Dirs, d)
	}
	sort.Strings(res.Dirs)
	m.invalidateReclaim()
	return res, nil
}

// Purge permanently removes entries. An EMPTY id list purges everything —
// that is the "empty trash" action, and it is the only thing that actually
// frees space.
func (m *Manager) Purge(ids []string) (*Result, error) {
	if !m.on() {
		return nil, ErrDisabled
	}
	res := &Result{}
	if len(ids) == 0 {
		entries, err := m.List()
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			ids = append(ids, e.ID)
		}
	}
	for _, id := range ids {
		out := Outcome{Path: id}
		stamp, rel, err := splitID(id)
		if err != nil {
			out.Status, out.Reason = "failed", err.Error()
			res.Failed++
			res.Outcomes = append(res.Outcomes, out)
			continue
		}
		out.Path = rel
		root, src, ok := m.locate(stamp, rel)
		if !ok {
			out.Status, out.Reason = "failed", "no such entry"
			res.Failed++
			res.Outcomes = append(res.Outcomes, out)
			continue
		}
		res.Root = root
		if info, ierr := os.Stat(src); ierr == nil {
			out.Bytes = info.Size()
		}
		if rmErr := os.Remove(src); rmErr != nil {
			out.Status, out.Reason = "failed", rmErr.Error()
			res.Failed++
			res.Outcomes = append(res.Outcomes, out)
			continue
		}
		out.Status = "purged"
		res.OK++
		res.Bytes += out.Bytes
		res.Outcomes = append(res.Outcomes, out)
		m.pruneEmptyStamp(root, stamp)
	}
	m.invalidateReclaim()
	return res, nil
}

// locate finds the on-disk file for a (stamp, rel) pair across every root.
func (m *Manager) locate(stamp, rel string) (root, src string, ok bool) {
	for _, r := range m.roots() {
		p := filepath.Join(m.trashRoot(r), stamp, filepath.FromSlash(rel))
		if _, err := os.Stat(p); err == nil {
			return r, p, true
		}
	}
	return "", "", false
}

// pruneEmptyStamp removes now-empty directories inside a stamp, best-effort.
func (m *Manager) pruneEmptyStamp(root, stamp string) {
	base := filepath.Join(m.trashRoot(root), stamp)
	for i := 0; i < 32; i++ { // bounded: a pathological tree must not spin
		removed := false
		_ = filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
			if err != nil || !d.IsDir() || p == base {
				return nil //nolint:nilerr // best-effort tidy
			}
			if entries, rerr := os.ReadDir(p); rerr == nil && len(entries) == 0 {
				if os.Remove(p) == nil {
					removed = true
				}
			}
			return nil
		})
		if !removed {
			break
		}
	}
	if entries, err := os.ReadDir(base); err == nil && len(entries) == 0 {
		_ = os.Remove(base)
	}
}

// Sweep purges entries past the TTL.
//
// Age comes from the STAMP DIRECTORY NAME, never from a file's stat. os.Rename
// preserves mtime — measured, not assumed: a file stamped 2019 and trashed
// today reads as thousands of days old the instant it lands. An mtime-driven
// sweeper would purge it on the very next tick, and would do so
// oldest-content-first, destroying the recovery window for precisely the
// material most likely to be irreplaceable. The stamp directory exists for
// this reason.
func (m *Manager) Sweep() (purged int, freed int64, err error) {
	cutoff := m.now().Add(-m.ttl)
	for _, root := range m.roots() {
		base := m.trashRoot(root)
		stamps, rerr := os.ReadDir(base)
		if rerr != nil {
			if os.IsNotExist(rerr) {
				continue
			}
			err = rerr
			continue
		}
		for _, sd := range stamps {
			if !sd.IsDir() {
				_ = os.Remove(filepath.Join(base, sd.Name()))
				continue
			}
			ts, ok := parseStamp(sd.Name())
			if !ok {
				// Not a stamp directory at all: debris. Its age cannot be
				// known, so leave it rather than guess — a wrong guess here
				// deletes user content.
				logger.Warn("unrecognised trash directory left in place", "root", root, "name", sd.Name())
				continue
			}
			if !ts.Before(cutoff) {
				continue
			}
			dir := filepath.Join(base, sd.Name())
			var bytes int64
			var n int
			_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, werr error) error {
				if werr != nil || d.IsDir() {
					return nil //nolint:nilerr // count what we can
				}
				if info, ierr := d.Info(); ierr == nil {
					bytes += info.Size()
					n++
				}
				return nil
			})
			if rmErr := os.RemoveAll(dir); rmErr != nil {
				logger.Warn("purge expired trash", "dir", dir, "err", rmErr)
				err = rmErr
				continue
			}
			purged += n
			freed += bytes
			logger.Info("purged expired trash", "stamp", sd.Name(), "files", n, "bytes", bytes)
		}
	}
	if purged > 0 {
		m.invalidateReclaim()
	}
	return purged, freed, err
}

// RunSweeper runs one pass immediately, then on a ticker.
func (m *Manager) RunSweeper(ctx interface{ Done() <-chan struct{} }, every time.Duration) {
	if every <= 0 {
		every = time.Hour
	}
	if _, _, err := m.Sweep(); err != nil {
		logger.Warn("trash sweep (startup)", "err", err)
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, _, err := m.Sweep(); err != nil {
				logger.Warn("trash sweep", "err", err)
			}
		}
	}
}
