// Package library is the bridge player's resolver inside bridge serve (B470
// PR 4b3): it maps an engine.Reference to the content of one of the bridge's
// own indexed files, through the library index and the filesystem, never
// over HTTP. The backend (internal/player/playback) asks it at each
// occurrence's open, on a goroutine of the backend's.
//
// A reference is the DLNA track id of an original: dlna.TrackID("", path),
// the id /dlna/file/{id} names, for the path the index stores (Reference).
// It names a stored path, not immutable content: what plays is the file at
// that path when an occurrence opens, as the DLNA and loopback routes serve
// it. How a controller names an entry, a URI and its confinement, is
// release 5a's; nothing here parses one.
//
// Every open checks afresh, never through an answer an earlier open, or
// another reader's cache, found. The reference is matched against the
// paths the index holds, each hashed; only which path a reference names is
// kept from one open to the next, since a hash makes that permanent. Then,
// at every open: the index still holds that path's row (not_indexed); the
// row is not an upstream server's (routed); the path names a library root
// configured now (outside_library), through the live path resolver; and
// the file is opened as the loopback route opens one, with its containment
// check: its links resolved inside a root before the open
// (fsutil.ResolveLinks, fsutil.IsUnderAny), opened as a file and never
// waited on, a named pipe included (fsutil.OpenAsFile; not_a_file), its
// links resolved again after the open and still inside a root, and the
// file that opened the one that root reaches (dlna.SameFileInRoot), so a
// link swapped in between is refused. A file that is not there, or cannot
// be opened, is engine.ReasonMissing. Each refusal is an *engine.Unplayable,
// which the backend passes on as it is and the engine records as the
// entry's skip; its detail names no path, absolute or stored, since the
// snapshot that carries it is what a controller will read (5a).
//
// A file replaced in place is not detected: the row's size and mtime are not
// compared with the file's, because a file retagged since the scan that
// indexed it would then be refused until the next scan, six hours by
// default, and the reference names the path, not those bytes. Within one
// occurrence the content is the file that opened: one descriptor, read up
// to the size it had at the open.
//
// A panic in the resolver is recovered as the entry's refusal
// (resolver_failed), logged with its stack and never its value: the
// backend's goroutines recover nothing.
package library

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"regexp"
	"runtime"
	"runtime/debug"
	"sync"
	"syscall"

	"github.com/acoseac/1-bit-bridge/internal/ctxerr"
	"github.com/acoseac/1-bit-bridge/internal/dlna"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/logging"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
)

// The reasons the resolver refuses a reference with, beside
// engine.ReasonMissing: the file is not there, or could not be opened.
const (
	// ReasonNotIndexed is a reference no row of the index has now: never
	// one of the library's, or a row the index no longer holds.
	ReasonNotIndexed engine.Reason = "not_indexed"
	// ReasonRouted is a row an upstream server routes: not one of the
	// bridge's own files, which alone the player plays.
	ReasonRouted engine.Reason = "routed"
	// ReasonOutsideLibrary is a file that, its links resolved, is not
	// inside a library root configured now, or a path that names none.
	ReasonOutsideLibrary engine.Reason = "outside_library"
	// ReasonNotAFile is a directory, a named pipe, a socket or a device at
	// the row's path.
	ReasonNotAFile engine.Reason = "not_a_file"
	// ReasonAmbiguous is a reference two rows of the index have.
	ReasonAmbiguous engine.Reason = "ambiguous_reference"
	// ReasonIndexUnreadable is an index the resolver could not read.
	ReasonIndexUnreadable engine.Reason = "index_unreadable"
	// ReasonResolverFailed is a panic in the resolver, recovered.
	ReasonResolverFailed engine.Reason = "resolver_failed"
)

// maxKnown bounds the references whose path the resolver keeps: twice the
// most entries a queue holds. Past it the record starts again.
const maxKnown = 2 * engine.MaxEntries

// referenceShape is a reference's: 16 lowercase hex digits.
var referenceShape = regexp.MustCompile(`^[0-9a-f]{16}$`)

// Reference is the reference of the original the index stores at path:
// dlna.TrackID("", path).
func Reference(path string) engine.Reference {
	r, err := engine.NewReference(dlna.TrackID("", path))
	if err != nil {
		panic(err) // 16 hex digits are always a reference
	}
	return r
}

// Store is what the resolver reads of the library index: the manifest
// store's own methods.
type Store interface {
	// TrackPaths is every path the index holds.
	TrackPaths(ctx context.Context) ([]string, error)
	// GetTrack is the row at path, nil when there is none.
	GetTrack(ctx context.Context, path string) (*manifest.Track, error)
	// GetUPnPRouting is the routing row of path, nil for a file of the
	// bridge's own.
	GetUPnPRouting(ctx context.Context, path string) (*manifest.UPnPRouting, error)
}

// Paths maps a stored path onto the library roots configured now: serve's
// path resolver (internal/fs), whose roots the console changes live.
type Paths interface {
	Resolve(storedPath string) (string, error)
	Roots() []string
}

// Config is what New builds a resolver from.
type Config struct {
	// Store is the library index. Required.
	Store Store
	// Paths is the live path resolver. Required.
	Paths Paths
	// Log is where the resolver logs; nil is the player component.
	Log *slog.Logger
	// Seams are a test's hold on the resolver between its steps; nil in
	// production.
	Seams *Seams
}

// Seams are the resolver's test seams. Each runs on the goroutine that
// reaches it, and may block or panic there.
type Seams struct {
	// Opens runs as each Open begins, with its reference.
	Opens func(engine.Reference)
	// BeforeOpen runs between the containment check and the open.
	BeforeOpen func()
}

// Resolver maps the engine's references to the library's files
// (playback.Resolver).
type Resolver struct {
	cfg Config
	log *slog.Logger
	// idOf is a path's reference: dlna.TrackID("", path). A test makes two
	// paths share one.
	idOf func(path string) string

	mu    sync.Mutex
	known map[string]string // a reference, and the path it names
}

var _ playback.Resolver = (*Resolver)(nil)

// New builds a resolver.
func New(cfg Config) (*Resolver, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("player library: a Store is required")
	case cfg.Paths == nil:
		return nil, errors.New("player library: a Paths is required")
	}
	if cfg.Log == nil {
		cfg.Log = logging.Component("player")
	}
	return &Resolver{cfg: cfg, log: cfg.Log, idOf: func(p string) string { return dlna.TrackID("", p) }, known: map[string]string{}}, nil
}

// refuse is a refusal with reason and a detail that names no path.
func refuse(reason engine.Reason, detail string) *engine.Unplayable {
	return &engine.Unplayable{Reason: reason, Detail: detail}
}

// Open opens the file ref names, after checking it afresh, or refuses it
// (the package documentation).
func (r *Resolver) Open(ctx context.Context, ref engine.Reference) (c playback.Content, err error) {
	defer func() {
		if p := recover(); p != nil {
			logPanic(r.log, p)
			c, err = playback.Content{}, refuse(ReasonResolverFailed, "the library resolver failed; the bridge's log has why")
		}
	}()
	if s := r.cfg.Seams; s != nil && s.Opens != nil {
		s.Opens(ref)
	}
	path, err := r.pathOf(ctx, ref)
	if err != nil {
		return playback.Content{}, err
	}
	t, err := r.cfg.Store.GetTrack(ctx, path)
	switch {
	case err != nil:
		return playback.Content{}, r.indexError(ctx, err)
	case t == nil:
		return playback.Content{}, refuse(ReasonNotIndexed, "the library index no longer holds the track this reference names")
	}
	rt, err := r.cfg.Store.GetUPnPRouting(ctx, path)
	switch {
	case err != nil:
		return playback.Content{}, r.indexError(ctx, err)
	case rt != nil:
		return playback.Content{}, refuse(ReasonRouted, "the track is an upstream server's, and the player plays the bridge's own files")
	}
	abs, err := r.cfg.Paths.Resolve(path)
	if err != nil {
		return playback.Content{}, refuse(ReasonOutsideLibrary, "the track's path names no library root configured now")
	}
	f, info, err := r.openInside(abs)
	if err != nil {
		return playback.Content{}, err
	}
	return playback.Content{Data: f, Size: info.Size(), Closer: f}, nil
}

// pathOf is the stored path ref names: one the resolver has matched it to
// before, or the one row of the index whose path hashes to it now.
func (r *Resolver) pathOf(ctx context.Context, ref engine.Reference) (string, error) {
	id := ref.String()
	if !referenceShape.MatchString(id) {
		return "", refuse(ReasonNotIndexed, "the reference is not one of the library's: a track's is 16 hex digits")
	}
	r.mu.Lock()
	p, ok := r.known[id]
	r.mu.Unlock()
	if ok {
		return p, nil
	}
	all, err := r.cfg.Store.TrackPaths(ctx)
	if err != nil {
		return "", r.indexError(ctx, err)
	}
	var found []string
	for _, q := range all {
		if r.idOf(q) == id {
			found = append(found, q)
		}
	}
	switch len(found) {
	case 0:
		return "", refuse(ReasonNotIndexed, "no track the library index holds has this reference")
	case 1:
	default:
		return "", refuse(ReasonAmbiguous, fmt.Sprintf("%d tracks the library index holds have this reference", len(found)))
	}
	r.mu.Lock()
	if len(r.known) >= maxKnown {
		clear(r.known)
	}
	r.known[id] = found[0]
	r.mu.Unlock()
	return found[0], nil
}

// openInside opens abs as the loopback route opens a library file: its
// links resolved inside a root configured now before the open, opened as a
// file and never waited on, resolved again after the open and still inside,
// and the file that opened the one that root reaches.
func (r *Resolver) openInside(abs string) (*os.File, fs.FileInfo, error) {
	roots := r.cfg.Paths.Roots()
	resolved, err := fsutil.ResolveLinks(abs)
	if err != nil {
		return nil, nil, openRefusal(err)
	}
	if fsutil.IsUnderAny(resolved, roots) == "" {
		return nil, nil, refuse(ReasonOutsideLibrary, "the file, its links resolved, is not inside a library root configured now")
	}
	if s := r.cfg.Seams; s != nil && s.BeforeOpen != nil {
		s.BeforeOpen()
	}
	f, info, err := fsutil.OpenAsFile(abs)
	if err != nil {
		return nil, nil, openRefusal(err)
	}
	again, err := fsutil.ResolveLinks(abs)
	root := ""
	if err == nil {
		root = fsutil.IsUnderAny(again, roots)
	}
	if root == "" || !dlna.SameFileInRoot(info, root, again) {
		_ = f.Close()
		return nil, nil, refuse(ReasonOutsideLibrary, "the file changed as it was opened, and what opened is not inside a library root configured now")
	}
	return f, info, nil
}

// openRefusal is the refusal of a file that could not be resolved or
// opened, naming the kind of failure and never the path.
func openRefusal(err error) *engine.Unplayable {
	if kind := fsutil.NotAFileKind(err); kind != "" {
		return refuse(ReasonNotAFile, "a "+kind+" is at the track's path, not a file")
	}
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		return refuse(engine.ReasonMissing, "the file is not there")
	case errors.Is(err, fs.ErrPermission):
		return refuse(engine.ReasonMissing, "the bridge may not read the file")
	}
	detail := "the file could not be opened"
	if pe := (*fs.PathError)(nil); errors.As(err, &pe) {
		detail += ": " + pe.Op + ": " + pe.Err.Error()
	}
	return refuse(engine.ReasonMissing, detail)
}

// indexError is the refusal of a read of the index that failed: an open
// whose context ended (its occurrence released or replaced) is refused
// as cancelled and logs nothing, and any other failure is logged.
func (r *Resolver) indexError(ctx context.Context, err error) *engine.Unplayable {
	if ctxerr.WithoutCancellation(ctx, err) == nil {
		return refuse(engine.ReasonMissing, "the open was cancelled")
	}
	r.log.Warn("player: the library index could not be read for an entry; it is skipped", "err", err)
	return refuse(ReasonIndexUnreadable, "the library index could not be read; the bridge's log has why")
}

// panicMessage is the line a recovered panic logs.
const panicMessage = "player: the library resolver panicked; the entry is refused"

// logPanic logs a panic in the resolver: its stack, and its value only when
// it is the runtime's own error, which carries nothing the resolver read (a
// value code panics with could quote a path).
func logPanic(log *slog.Logger, p any) {
	attrs := []any{"stack", string(debug.Stack())}
	if rerr, ok := p.(runtime.Error); ok {
		attrs = append(attrs, "error", rerr.Error())
	}
	log.Error(panicMessage, attrs...)
}
