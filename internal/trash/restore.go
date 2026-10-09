package trash

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// Restore moves entries back to their original paths.
func (m *Manager) Restore(ids []string) (*Result, error) {
	if !m.on() {
		return nil, ErrDisabled
	}
	if m.split == nil {
		return nil, ErrRootUnavailable
	}
	res := &Result{}
	dirs := map[string]struct{}{}
	spellers := map[string]*fsutil.Speller{}
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
		// The destination is the path the file had under the root it was
		// trashed from, resolved for the root count in force now. An entry
		// written before that root was recorded is restored only when its
		// stored path has one reading.
		dst, client, suffix, reason := m.restorePlace(root, rel, src)
		if reason != "" {
			out.Status, out.Reason = "failed", reason
			res.Failed++
			res.Outcomes = append(res.Outcomes, out)
			continue
		}
		if fsutil.IsUnderAny(dst, []string{root}) == "" {
			out.Status, out.Reason = "failed", "resolves outside the library root"
			res.Failed++
			res.Outcomes = append(res.Outcomes, out)
			continue
		}
		// Held across the existence check and the rename, then released on
		// every exit of this entry. A defer inside the loop would hold every
		// earlier path until Restore returns.
		unlock := func() {}
		if m.destLock != nil {
			unlock = m.destLock(dst)
		}
		locked := true
		release := func() {
			if locked {
				unlock()
				locked = false
			}
		}
		switch presence, reason := fsutil.StatDestination(m.destStat, dst); presence {
		case fsutil.DestPresent:
			release()
			out.Status, out.Reason = "failed", "a file already exists at the original path"
			res.Failed++
			res.Outcomes = append(res.Outcomes, out)
			continue
		case fsutil.DestUnreadable:
			release()
			out.Status, out.Reason = "failed", reason
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
			release()
			out.Status, out.Reason = "failed", mkErr.Error()
			res.Failed++
			res.Outcomes = append(res.Outcomes, out)
			continue
		}
		if rnErr := atomicwrite.RenameWithRetry(src, dst); rnErr != nil {
			release()
			out.Status, out.Reason = "failed", rnErr.Error()
			res.Failed++
			res.Outcomes = append(res.Outcomes, out)
			continue
		}
		release()
		if info, ierr := os.Stat(dst); ierr == nil {
			out.Bytes = info.Size()
			res.Bytes += info.Size()
		}
		// The file back as its folder lists it. An entry an earlier build
		// trashed is recorded under the spelling its delete was sent with,
		// and the rename above lands in the folder that spelling opens, so a
		// rescan under the recorded spelling indexed the folder's every file
		// a second time (backlog B219).
		spelled, spellErr := spellRel(spellers, root, client, suffix)
		if spellErr != nil {
			res.FullScan = true
			logger.Warn("trash: the restored path's spelling on disk cannot be read; the library will be rescanned",
				"path", rel, "err", spellErr)
		}
		out.Status = "restored"
		res.OK++
		res.Paths = append(res.Paths, spelled)
		if d := path.Dir(spelled); d != "." {
			dirs[d] = struct{}{}
		}
		res.Outcomes = append(res.Outcomes, out)
		removeOriginRecord(m.trashRoot(root), src)
		m.pruneEmptyStamp(root, stamp)
	}
	for d := range dirs {
		res.Dirs = append(res.Dirs, d)
	}
	sort.Strings(res.Dirs)
	m.invalidateReclaim()
	return res, nil
}

// Purge permanently removes entries. A nil id list purges everything — that
// is the "empty trash" action, and it is the only thing that actually frees
// space. A present empty list purges nothing: len cannot tell the two apart,
// and reading both as everything is how {"ids":[]} emptied the trash.
func (m *Manager) Purge(ids []string) (*Result, error) {
	if !m.on() {
		return nil, ErrDisabled
	}
	res := &Result{}
	if ids != nil && len(ids) == 0 {
		return nil, fmt.Errorf("%w: no entries given", ErrInvalidPath)
	}
	if ids == nil {
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
		if info, ierr := os.Stat(src); ierr == nil {
			out.Bytes = info.Size()
		}
		if rmErr := os.Remove(src); rmErr != nil {
			out.Status, out.Reason = "failed", rmErr.Error()
			res.Failed++
			res.Outcomes = append(res.Outcomes, out)
			continue
		}
		removeOriginRecord(m.trashRoot(root), src)
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
				if rel, rerr := filepath.Rel(dir, p); rerr == nil && hiddenTrashPath(filepath.ToSlash(rel)) {
					return nil
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

const (
	differentRootReason   = "this entry was trashed under a different library root than its path names; restore it by hand"
	legacyAmbiguousReason = "this entry was trashed before its library root was recorded, and its path begins with that root's name; restore it by hand"
	unreadableRecord      = "the record of which library root this file came from cannot be read"
)

// restorePlace names the absolute destination, the manifest path under the
// current root count, and the path relative to the root. A non-empty reason
// refuses the entry and leaves the file in the trash.
func (m *Manager) restorePlace(sitting, stored, src string) (dst, client, suffix, reason string) {
	rec, err := readOriginRecord(originSidecar(src))
	if err != nil && !os.IsNotExist(err) {
		return "", "", "", unreadableRecord
	}
	if err == nil {
		return m.placeFromRecord(sitting, rec)
	}
	return m.placeLegacy(sitting, stored)
}

func (m *Manager) configuredRoot(recorded string) (string, bool) {
	want := fsutil.EvalSymlinksOrClean(recorded)
	for _, r := range m.roots() {
		if fsutil.EvalSymlinksOrClean(r) == want {
			return r, true
		}
	}
	return "", false
}

// manifestClientPath is the path a client names under the root count in
// force now: the suffix alone with one root, and that root's basename plus
// the suffix with more than one. root is a configured root the recorded
// path resolved to, so its basename is the one the resolver indexes.
func manifestClientPath(roots []string, root, rel string) string {
	rel = strings.Trim(rel, "/")
	if len(roots) <= 1 {
		return rel
	}
	base := filepath.Base(root)
	if rel == "" {
		return base
	}
	return base + "/" + rel
}

func (m *Manager) placeFromRecord(sitting string, rec originRecord) (dst, client, suffix, reason string) {
	configured, ok := m.configuredRoot(rec.Root)
	if !ok {
		return "", "", "", ErrRootUnavailable.Error()
	}
	if fsutil.EvalSymlinksOrClean(configured) != fsutil.EvalSymlinksOrClean(sitting) {
		return "", "", "", differentRootReason
	}
	client = manifestClientPath(m.roots(), configured, rec.Rel)
	abs, err := m.split.Resolve(client)
	if err != nil {
		return "", "", "", unreadableRecord
	}
	gotRoot, gotSuffix, err := m.split.SplitRoot(client)
	if err != nil || fsutil.EvalSymlinksOrClean(gotRoot) != fsutil.EvalSymlinksOrClean(rec.Root) || path.Clean(gotSuffix) != path.Clean(rec.Rel) {
		return "", "", "", unreadableRecord
	}
	return abs, client, rec.Rel, ""
}

func (m *Manager) placeLegacy(sitting, stored string) (dst, client, suffix, reason string) {
	head, _, _ := strings.Cut(stored, "/")
	roots := m.roots()
	if head == filepath.Base(sitting) {
		return "", "", "", legacyAmbiguousReason
	}
	for _, r := range roots {
		if filepath.Base(r) == head && fsutil.EvalSymlinksOrClean(r) != fsutil.EvalSymlinksOrClean(sitting) {
			return "", "", "", differentRootReason
		}
	}
	client = manifestClientPath(roots, sitting, stored)
	abs, err := m.split.Resolve(client)
	if err != nil {
		return "", "", "", ErrRootUnavailable.Error()
	}
	gotRoot, gotSuffix, err := m.split.SplitRoot(client)
	if err != nil || fsutil.EvalSymlinksOrClean(gotRoot) != fsutil.EvalSymlinksOrClean(sitting) || path.Clean(gotSuffix) != path.Clean(stored) {
		return "", "", "", ErrRootUnavailable.Error()
	}
	return abs, client, stored, ""
}
