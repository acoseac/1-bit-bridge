// Package fs resolves client-supplied relative paths against a configured
// library root and rejects traversal (`..`, absolute paths, symlink
// escapes).
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
)

// ErrBadPath is returned when a client-supplied path fails a safety check
// (absolute, `..`, NUL byte, or escapes the library root).
var ErrBadPath = errors.New("path is not a valid library-relative path")

// ErrNotFound is returned when the resolved path does not exist on disk.
var ErrNotFound = errors.New("path not found")

// ErrUnknownRoot is returned (multi-root mode only) when the path's first
// segment doesn't match any configured root basename.
var ErrUnknownRoot = errors.New("unknown library root")

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
func ValidateRoots(roots []string) error {
	if len(roots) < 2 {
		return nil
	}
	seen := make(map[string]string, len(roots))
	for _, root := range roots {
		b := filepath.Base(root)
		if prior, ok := seen[b]; ok {
			return fmt.Errorf("duplicate library-root basename %q (%s vs %s) — multi-root listing requires unique basenames", b, prior, root)
		}
		seen[b] = root
	}
	return nil
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
	// the only way to see what the client actually asked for.
	for _, seg := range strings.Split(clientPath, "/") {
		if seg == ".." {
			return "", ErrBadPath
		}
	}

	// Canonicalize: collapse ".", empty segments, and leading slashes.
	// Leading "/" is treated as root-relative (a redundant prefix, not an
	// absolute path) and stripped.
	clean := path.Clean("/" + clientPath)
	clean = strings.TrimPrefix(clean, "/")
	if clean == "." {
		clean = ""
	}

	// Pick a root. Snapshot under the read lock so a concurrent SetRoots
	// can't swap roots mid-pick.
	r.mu.RLock()
	roots := r.roots
	basenameIndex := r.basenameIndex
	r.mu.RUnlock()

	var (
		root     string
		suffix   string
		segments = strings.SplitN(clean, "/", 2)
	)
	switch {
	case len(roots) == 1:
		root = roots[0]
		suffix = clean
	case clean == "":
		// Multi-root with an empty path — ambiguous. Refuse.
		return "", ErrUnknownRoot
	default:
		head := segments[0]
		full, ok := basenameIndex[head]
		if !ok {
			return "", ErrUnknownRoot
		}
		root = full
		if len(segments) == 2 {
			suffix = segments[1]
		}
	}

	// Join via filepath.Join which also does final cleaning with native
	// separators. The final check compares against the absolute root to
	// catch any residual "../" that slipped through (shouldn't happen
	// given path.Clean above, but belt-and-braces against Go version drift).
	abs := filepath.Join(root, filepath.FromSlash(suffix))
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	absAbs, err := filepath.Abs(abs)
	if err != nil {
		return "", err
	}
	if absAbs != rootAbs && !strings.HasPrefix(absAbs, rootAbs+string(filepath.Separator)) {
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
