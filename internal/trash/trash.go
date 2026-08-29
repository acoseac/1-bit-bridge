// Package trash implements the console's delete surface.
//
// It moves a file to <root>/.bridge-trash/<unixNano>/<relPath> rather than
// unlinking it. Three things follow from that, and all three are the reason
// for the shape:
//
//   - It is a same-filesystem rename, so it is instant even when the library
//     is a network mount.
//   - The dot-directory is invisible to the scanner (shouldSkipDir returns
//     SkipDir for any "."-prefixed name), so trashed content leaves the
//     manifest and never comes back on the next walk.
//   - It is recoverable for a TTL window.
//
// THE BRIDGE HAS NEVER REMOVED A FILE FROM A LIBRARY ROOT. Every os.Remove in
// the tree is a temp file, a sidecar, a backup, the updater's own binary, the
// config dir on uninstall, tsnet state, or a pidfile — and `bridge uninstall`
// promises the operator in as many words that the library path is read-only by
// design. This package is the first exception, which is why it is gated
// separately from uploads and why deletion goes through a recoverable step.
//
// The reason to be careful is specific rather than abstract: this codebase has
// already shipped a prefix-scoping bug that deleted the wrong things —
// DeleteTracksByPrefix under case-folding LIKE removed /srv/music's rows when
// the operator removed /srv/Music, and the pre-confirm COUNT understated the
// damage because it was case-exact while the delete folded. That was survivable
// only because it destroyed rows and sidecars, both of which regenerate. The
// same bug against library files does not.
package trash

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/logging"
)

var logger = logging.Component("trash")

// DirName is the per-root trash directory. The leading dot is load-bearing.
const DirName = ".bridge-trash"

// DefaultTTL is how long a trashed file stays recoverable.
const DefaultTTL = 7 * 24 * time.Hour

var (
	ErrDisabled    = errors.New("trash: deleting is turned off")
	ErrNotFound    = errors.New("trash: no such entry")
	ErrInvalidPath = errors.New("trash: invalid path")
)

// Entry is one trashed file.
type Entry struct {
	// ID is "<stamp>/<relPath>" — enough to locate the file and to restore
	// it. It travels in a JSON body, never a URL, so it needs no encoding
	// and cannot meet the "+ decodes to a space" class.
	ID           string
	OriginalPath string
	Size         int64
	TrashedAt    time.Time
	Root         string
}

// Outcome is one path's result.
type Outcome struct {
	Path   string
	Status string // "trashed" | "restored" | "purged" | "failed"
	Reason string
	Bytes  int64
}

// Result aggregates a batch.
type Result struct {
	Root     string
	Outcomes []Outcome
	Bytes    int64
	OK       int
	Failed   int
	// Paths that changed state, for the caller to retire or re-scan.
	Paths []string
	// Dirs are the library-relative parents touched, for a subtree scan.
	Dirs []string
}

// Manager owns the trash for every configured root.
type Manager struct {
	roots   func() []string
	enabled func() bool
	ttl     time.Duration
	now     func() time.Time

	reclaimMu sync.Mutex
	reclaim   map[string]reclaimEntry
}

type reclaimEntry struct {
	bytes int64
	until time.Time
}

// Option configures a Manager.
type Option func(*Manager)

// WithClock overrides time.Now.
func WithClock(fn func() time.Time) Option { return func(m *Manager) { m.now = fn } }

// New builds a Manager. `enabled` is read LIVE on every mutating call so the
// setting hot-applies; a nil enabled means disabled, which is the safe
// direction for a destructive feature.
func New(roots func() []string, enabled func() bool, ttl time.Duration, opts ...Option) *Manager {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	m := &Manager{roots: roots, enabled: enabled, ttl: ttl, now: time.Now}
	for _, o := range opts {
		o(m)
	}
	return m
}

func (m *Manager) on() bool { return m.enabled != nil && m.enabled() }

// TTL exposes the configured window.
func (m *Manager) TTL() time.Duration { return m.ttl }

func (m *Manager) trashRoot(root string) string { return filepath.Join(root, DirName) }

// validRel checks a library-relative path in the DELETE direction.
//
// Dot segments are refused, so the staging and trash directories can never
// themselves be targets — a delete of the trash via the delete endpoint would
// be a loop, and a delete of staging would race a live upload.
func validRel(rel string) (string, error) {
	rel = strings.TrimPrefix(strings.TrimSpace(rel), "/")
	if rel == "" {
		return "", fmt.Errorf("%w: empty", ErrInvalidPath)
	}
	if strings.ContainsRune(rel, 0) || strings.ContainsRune(rel, '\\') {
		return "", fmt.Errorf("%w: illegal character", ErrInvalidPath)
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("%w: %q segment", ErrInvalidPath, seg)
		}
		if strings.HasPrefix(seg, ".") {
			return "", fmt.Errorf("%w: dot-prefixed segment", ErrInvalidPath)
		}
	}
	if c := path.Clean(rel); c != rel {
		return "", fmt.Errorf("%w: not in clean form", ErrInvalidPath)
	}
	return rel, nil
}

func (m *Manager) resolveRoot(want string) (string, error) {
	roots := m.roots()
	if len(roots) == 0 {
		return "", ErrNotFound
	}
	if want == "" {
		return roots[0], nil
	}
	for _, r := range roots {
		if fsutil.EvalSymlinksOrClean(r) == fsutil.EvalSymlinksOrClean(want) || filepath.Base(r) == want {
			return r, nil
		}
	}
	return "", ErrNotFound
}

// Trash moves the given library-relative paths into the trash.
func (m *Manager) Trash(rootWant string, rels []string) (*Result, error) {
	if !m.on() {
		return nil, ErrDisabled
	}
	root, err := m.resolveRoot(rootWant)
	if err != nil {
		return nil, err
	}
	stamp := strconv.FormatInt(m.now().UTC().UnixNano(), 10)
	res := &Result{Root: root}
	dirs := map[string]struct{}{}

	for _, raw := range rels {
		out := Outcome{Path: raw}
		rel, verr := validRel(raw)
		if verr != nil {
			out.Status, out.Reason = "failed", verr.Error()
			res.Failed++
			res.Outcomes = append(res.Outcomes, out)
			continue
		}
		out.Path = rel
		src := filepath.Join(root, filepath.FromSlash(rel))
		// Final containment check. On Windows this is the primary defense:
		// the raw segment scan and path.Clean are both slash-based, so it is
		// filepath.Join that would collapse a backslash traversal — which is
		// why validRel refuses backslashes outright, and why this check makes
		// that refusal not need to be perfect.
		if fsutil.IsUnderAny(src, []string{root}) == "" {
			out.Status, out.Reason = "failed", "resolves outside the library root"
			res.Failed++
			res.Outcomes = append(res.Outcomes, out)
			continue
		}
		info, serr := os.Stat(src)
		if serr != nil || info.IsDir() {
			out.Status = "failed"
			if serr != nil {
				out.Reason = serr.Error()
			} else {
				out.Reason = "is a directory"
			}
			res.Failed++
			res.Outcomes = append(res.Outcomes, out)
			continue
		}
		dst := filepath.Join(m.trashRoot(root), stamp, filepath.FromSlash(rel))
		if mkErr := os.MkdirAll(filepath.Dir(dst), 0o700); mkErr != nil {
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
		out.Status, out.Bytes = "trashed", info.Size()
		res.OK++
		res.Bytes += info.Size()
		res.Paths = append(res.Paths, rel)
		if d := path.Dir(rel); d != "." {
			dirs[d] = struct{}{}
		}
		res.Outcomes = append(res.Outcomes, out)
	}
	for d := range dirs {
		res.Dirs = append(res.Dirs, d)
	}
	sort.Strings(res.Dirs)
	m.invalidateReclaim()
	return res, nil
}

// List enumerates every trashed entry across every root, newest first.
func (m *Manager) List() ([]Entry, error) {
	var out []Entry
	for _, root := range m.roots() {
		base := m.trashRoot(root)
		stamps, err := os.ReadDir(base)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, sd := range stamps {
			if !sd.IsDir() {
				continue
			}
			ts, ok := parseStamp(sd.Name())
			if !ok {
				continue
			}
			stampDir := filepath.Join(base, sd.Name())
			err := filepath.WalkDir(stampDir, func(p string, d os.DirEntry, werr error) error {
				if werr != nil || d.IsDir() {
					return nil //nolint:nilerr // a partial listing beats none
				}
				rel, rerr := filepath.Rel(stampDir, p)
				if rerr != nil {
					return nil
				}
				info, ierr := d.Info()
				if ierr != nil {
					return nil
				}
				relSlash := filepath.ToSlash(rel)
				out = append(out, Entry{
					ID:           sd.Name() + "/" + relSlash,
					OriginalPath: relSlash,
					Size:         info.Size(),
					TrashedAt:    ts,
					Root:         root,
				})
				return nil
			})
			if err != nil {
				logger.Warn("walk trash stamp dir", "dir", stampDir, "err", err)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TrashedAt.After(out[j].TrashedAt) })
	return out, nil
}

// reclaimTTL bounds how often Reclaimable walks the trash tree.
//
// The sidebar space widget asks for this on EVERY page load, and the walk is a
// stat per trashed file — fine for a handful, not for an operator who just
// deleted a few thousand tracks. A few seconds of staleness is invisible in a
// number that only changes when the operator themselves deletes or purges, and
// both of those paths invalidate it explicitly.
const reclaimTTL = 5 * time.Second

// Reclaimable is what purging everything would free for a root.
func (m *Manager) Reclaimable(root string) int64 {
	m.reclaimMu.Lock()
	if cached, ok := m.reclaim[root]; ok && m.now().Before(cached.until) {
		m.reclaimMu.Unlock()
		return cached.bytes
	}
	m.reclaimMu.Unlock()

	entries, err := m.List()
	if err != nil {
		// Not cached: a transient read failure must not pin a zero for the
		// whole TTL, which would tell the operator there is nothing to
		// reclaim at the moment they most need to know there is.
		return 0
	}
	var total int64
	for _, e := range entries {
		if root == "" || e.Root == root {
			total += e.Size
		}
	}
	m.reclaimMu.Lock()
	if m.reclaim == nil {
		m.reclaim = map[string]reclaimEntry{}
	}
	m.reclaim[root] = reclaimEntry{bytes: total, until: m.now().Add(reclaimTTL)}
	m.reclaimMu.Unlock()
	return total
}

// invalidateReclaim drops the cache after anything that changes the trash.
func (m *Manager) invalidateReclaim() {
	m.reclaimMu.Lock()
	m.reclaim = nil
	m.reclaimMu.Unlock()
}

// splitID splits "<stamp>/<relPath>" and validates both halves.
func splitID(id string) (stamp string, rel string, err error) {
	stamp, rel, ok := strings.Cut(strings.TrimSpace(id), "/")
	if !ok {
		return "", "", fmt.Errorf("%w: malformed id", ErrInvalidPath)
	}
	if _, valid := parseStamp(stamp); !valid {
		return "", "", fmt.Errorf("%w: malformed stamp", ErrInvalidPath)
	}
	rel, err = validRel(rel)
	if err != nil {
		return "", "", err
	}
	return stamp, rel, nil
}

func parseStamp(name string) (time.Time, bool) {
	n, err := strconv.ParseInt(name, 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}, false
	}
	return time.Unix(0, n).UTC(), true
}
