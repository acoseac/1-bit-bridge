// Package fs resolves client-supplied relative paths against a configured
// library root and rejects LEXICAL traversal (`..`, absolute paths, NUL
// bytes). Resolution does not follow symlinks: a symlink the operator
// planted inside a root is trusted content and IS served (see
// TestResolveStillStopsSymlinkEscape) — the guarantee is that a client
// path can never escape a root without the server's help.
//
// Wire convention (see PROTOCOL.md): client paths are always forward-slash
// separated regardless of the server OS. The resolver maps to the native
// separator internally. With a single library root, the whole path is
// relative to it. With multiple roots, the first path segment must be the
// basename of one of the configured roots.
package fs

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// ErrBadPath is returned when a client-supplied path fails a safety check
// (absolute, `..`, NUL byte, or escapes the library root).
var ErrBadPath = errors.New("path is not a valid library-relative path")

// ErrNotFound is returned when the resolved path does not exist on disk.
var ErrNotFound = errors.New("path not found")

// ErrUnknownRoot is returned (multi-root mode only) when the path's first
// segment doesn't match any configured root basename.
var ErrUnknownRoot = errors.New("unknown library root")

// rootInfo holds the precomputed absolute path + containment prefix for
// one configured root. Both are pure functions of the root path, which
// only changes under setRootsLocked — so computing them once there keeps
// filepath.Abs + the prefix string-build off the per-request Resolve hot
// path (every served track goes through Resolve).
type rootInfo struct {
	abs    string // filepath.Abs(root)
	prefix string // abs + separator (TrimSuffix-corrected): the containment prefix
	absErr error  // error from filepath.Abs (≈never for absolute roots)
}

// Resolver maps a client path to an absolute on-disk path, enforcing
// root-scoping. Safe for concurrent use; roots can be hot-swapped at runtime
// via SetRoots so the admin console can add or remove a library root without
// a server restart.
type Resolver struct {
	mu sync.RWMutex
	// roots is the list of configured library roots, absolute paths. Order
	// matches config; single-entry is the common case.
	roots []string

	// basenameIndex maps root basename → full root path, for the multi-root
	// routing rule. Empty when len(roots) == 1.
	basenameIndex map[string]string

	// info maps each root path → its precomputed abs + containment prefix,
	// rebuilt alongside roots in setRootsLocked. Replaced (never mutated in
	// place) atomically with roots/basenameIndex, so a single Resolve
	// snapshot stays consistent. Read-only after publish.
	info map[string]rootInfo
}

// New returns a Resolver for the given library roots. The roots must be
// absolute paths (config.Load enforces this). Callers that want to
// reject duplicate basenames early should call ValidateRoots first;
// when two roots share a basename, New keeps the last one (the older
// behavior) to avoid panicking deep inside api.New.
func New(roots []string) *Resolver {
	r := &Resolver{}
	r.setRootsLocked(roots)
	return r
}

// SetRoots atomically replaces the Resolver's root list. In-flight Resolve
// calls complete against their original snapshot; calls that start after
// SetRoots returns see the new list. The admin console calls this after
// adding or removing a library root.
func (r *Resolver) SetRoots(roots []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.setRootsLocked(roots)
}

// setRootsLocked rebuilds the roots slice and basename index. Caller must
// hold r.mu (write) or be in New before publication.
func (r *Resolver) setRootsLocked(roots []string) {
	r.roots = append([]string(nil), roots...)
	r.basenameIndex = nil
	r.info = make(map[string]rootInfo, len(roots))
	for _, root := range roots {
		abs, err := filepath.Abs(root)
		ri := rootInfo{abs: abs, absErr: err}
		if err == nil {
			// TrimSuffix handles the filesystem-root case (abs == "/" on
			// Unix or "C:\" on Windows): without it the prefix would be
			// "//" / "C:\\" and never match a real path. See Resolve.
			ri.prefix = strings.TrimSuffix(abs, string(filepath.Separator)) + string(filepath.Separator)
		}
		r.info[root] = ri
	}
	if len(roots) > 1 {
		r.basenameIndex = make(map[string]string, len(roots))
		for _, root := range roots {
			r.basenameIndex[filepath.Base(root)] = root
		}
	}
}

// ValidateRoots returns a descriptive error if two library roots share a
// basename. The multi-root listing protocol keys each top-level entry by
// basename (e.g. /data/Music and /archive/Music both present as "Music"),
// so a collision makes one root unreachable. cmd/bridge calls this after
// config.Load so misconfiguration surfaces at startup rather than as a
// silent 404 from /v1/list.
//
// **The comparison folds case**, so /srv/Music and /srv/music collide.
// Two independent reasons, and the second is the sharp one:
//
//   - Resolve's basenameIndex is an exact-match map, but the paths it
//     routes are compared against a filesystem that is case-insensitive
//     on macOS and Windows — so the two roots are the same directory
//     there, and accepting both silently makes one unreachable.
//   - Several store-side prefix operations key off the basename, and
//     the deletion path used to match case-insensitively: removing
//     /srv/Music deleted /srv/music's rows AND unlinked its variant and
//     waveform files. Those predicates are now case-exact, but the
//     configuration that made the collision expressible in the first
//     place is worth refusing outright rather than relying on every
//     downstream consumer to keep getting it right.
//
// Unicode-aware lowering (the same cases.Lower(language.Und) the
// scanner's fold uses) rather than strings.ToLower, so "MÚSICA" and
// "música" collide too — a root basename is user-supplied text, not
// ASCII. The error names both original spellings, since folded output
// would be confusing to act on.
func ValidateRoots(roots []string) error {
	if len(roots) < 2 {
		return nil
	}
	seen := make(map[string]string, len(roots))
	for _, root := range roots {
		b := filepath.Base(root)
		key := FoldRootBasename(root)
		if prior, ok := seen[key]; ok {
			return fmt.Errorf("duplicate library-root basename %q (%s vs %s) — multi-root listing requires unique basenames (compared case-insensitively)", b, prior, root)
		}
		seen[key] = root
	}
	return nil
}

// basenameFolder is the shared caser. Package-level because
// cases.Lower allocates a fresh caser per call otherwise, and this runs
// per-root inside admin request handling.
var basenameFolder = cases.Lower(language.Und)

// FoldRootBasename returns the comparison key for a library root's
// basename: its final path element, Unicode-lowered.
//
// Exported so the admin console's remove-root guard keys off the SAME
// function ValidateRoots does. Those two agreeing is the invariant —
// ValidateRoots decides which configurations are expressible, the admin
// guard decides which removals are unambiguous, and if their notions of
// "same basename" ever diverge you get a config that validates but whose
// removal path can't tell the two roots apart. Keeping one function
// makes that divergence impossible rather than merely unlikely.
func FoldRootBasename(root string) string {
	return basenameFolder.String(filepath.Base(root))
}

// Resolve maps a client-supplied relative path to an absolute server path.
// It guarantees the returned path is within one of the configured roots.
// The existence of the path is NOT checked here — callers that care can
// os.Stat after; Resolve is a pure safety / routing operation.
func (r *Resolver) Resolve(clientPath string) (string, error) {
	if strings.ContainsRune(clientPath, 0) {
		return "", ErrBadPath
	}

	// Reject any ".." segment in the raw input *before* canonicalizing.
	// path.Clean("/../x") would silently collapse to "/x", hiding the
	// escape attempt from downstream checks. Walking the raw segments is
	// the only way to see what the client actually asked for. Done as a
	// zero-alloc byte scan rather than strings.Split (which would allocate
	// a slice + substrings on every served path) — equivalent: it checks
	// the exact same '/'-delimited segments, including the empty-string
	// and leading/trailing-slash cases.
	for start, i := 0, 0; i <= len(clientPath); i++ {
		if i == len(clientPath) || clientPath[i] == '/' {
			if clientPath[start:i] == ".." {
				return "", ErrBadPath
			}
			start = i + 1
		}
	}

	// Canonicalize: collapse ".", empty segments, and leading slashes.
	// Leading "/" is treated as root-relative (a redundant prefix, not an
	// absolute path) and stripped.
	//
	// Pre-fix used `path.Clean("/" + clientPath)` then `TrimPrefix("/")`,
	// paying two string allocations per call. The path-traversal safety
	// (".." rejection) lives in the segment walk above this — by the
	// time path.Clean runs, the input is known safe — so this is purely
	// canonicalisation, not a security check. Direct `path.Clean` is
	// equivalent: the "/foo" leading-slash case still trims via
	// TrimPrefix; the "" empty-string case maps `path.Clean("")` ==
	// "." to "". Verified equivalent across the three input shapes
	// (empty / leading-slash / bare relative).
	clean := path.Clean(clientPath)
	if clean == "." {
		clean = ""
	}
	clean = strings.TrimPrefix(clean, "/")

	// Pick a root. Snapshot under the read lock so a concurrent SetRoots
	// can't swap roots mid-pick. `info` is replaced atomically alongside
	// roots/basenameIndex in setRootsLocked, so one snapshot is consistent.
	r.mu.RLock()
	roots := r.roots
	basenameIndex := r.basenameIndex
	info := r.info
	r.mu.RUnlock()

	var (
		root   string
		suffix string
	)
	switch {
	case len(roots) == 1:
		root = roots[0]
		suffix = clean
	case clean == "":
		// Multi-root with an empty path — ambiguous. Refuse.
		return "", ErrUnknownRoot
	default:
		// Split off the first path segment (the root basename) without
		// strings.SplitN's slice allocation. Equivalent to
		// SplitN(clean, "/", 2): no '/' → head=clean, suffix="".
		head := clean
		if idx := strings.IndexByte(clean, '/'); idx >= 0 {
			head = clean[:idx]
			suffix = clean[idx+1:]
		}
		full, ok := basenameIndex[head]
		if !ok {
			return "", ErrUnknownRoot
		}
		root = full
	}

	// rootAbs + prefix are precomputed per root in setRootsLocked (pure
	// functions of the root path), so the per-request cost here is just the
	// suffix Join + the final containment check. `root` came from the same
	// snapshot `info` was built from, so the lookup hits; the recompute
	// branch is a defensive fallback for an impossible miss.
	ri, ok := info[root]
	if !ok {
		var err error
		ri.abs, err = filepath.Abs(root)
		if err != nil {
			return "", err
		}
		ri.prefix = strings.TrimSuffix(ri.abs, string(filepath.Separator)) + string(filepath.Separator)
	} else if ri.absErr != nil {
		return "", ri.absErr
	}

	// Join via filepath.Join which also does final cleaning with native
	// separators. The final check compares against the absolute root to
	// catch any residual "../" that slipped through (shouldn't happen
	// given path.Clean above, but belt-and-braces against Go version drift).
	abs := filepath.Join(root, filepath.FromSlash(suffix))
	absAbs, err := filepath.Abs(abs)
	if err != nil {
		return "", err
	}
	// ri.prefix's TrimSuffix handles the filesystem-root case (rootAbs ==
	// "/" on Unix or "C:\" on Windows): without it the prefix would be
	// "//" / "C:\\" and never match, 400-ing every request against a
	// root-mounted library (Docker mount directly to /).
	if absAbs != ri.abs && !strings.HasPrefix(absAbs, ri.prefix) {
		return "", ErrBadPath
	}
	return absAbs, nil
}

// ResolveChecked is Resolve plus an os.Stat; it returns ErrNotFound if the
// path does not exist. Convenient for handlers that need both.
func (r *Resolver) ResolveChecked(clientPath string) (string, os.FileInfo, error) {
	abs, err := r.Resolve(clientPath)
	if err != nil {
		return "", nil, err
	}
	info, err := os.Stat(abs)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil, ErrNotFound
	}
	if err != nil {
		return "", nil, err
	}
	return abs, info, nil
}

// Roots returns the configured roots (copy). Exposed for the /v1/list
// handler to enumerate the synthetic top-level in multi-root mode.
func (r *Resolver) Roots() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.roots))
	copy(out, r.roots)
	return out
}
