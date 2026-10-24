// Package library is the bridge player's resolver inside bridge serve (B470
// PR 4b3): it maps an engine.Reference to the content of one of the bridge's
// own indexed files, through the library index and the filesystem, never
// over HTTP. The backend (internal/player/playback) asks it at each
// occurrence's open, on a goroutine of the backend's.
//
// A track is named by its DLNA track id: dlna.TrackID("", path), the id
// /dlna/file/{id} names, for the path the index stores (Reference). A
// reference is that id (the form serve's player and bridge output play wrote
// before B470 PR 5a4), or the canonical URI of the track's /dlna/file/ route
// as an OpenHome controller inserted it (PR 5a4); both are read by the one
// parser, internal/player/trackref, and at an open the URI's host and port
// are not looked at, so a queue keeps playing after the bridge's address
// changes. It names a stored path, not immutable content: what plays is the
// file at that path when an occurrence opens, as the DLNA and loopback routes
// serve it.
//
// Every open checks afresh, never through an answer an earlier open, or
// another reader's cache, found. The id is looked up in the resolver's index
// of the paths the index holds by their ids, which is kept current as a delta
// client of /v1/manifest keeps its library: every row a writer inserts or
// rewrites is stamped past every earlier row (manifest's readNextDeltaStamp),
// so each lookup first takes the rows stamped since the last
// (manifest.Store.TrackPathsIndexedAfter, a seek of the stamps' index when
// nothing changed), and a track a scan adds is found at once, whatever the
// library's size; a path the index once held is taken for the id's only once
// the row is read again. Then, at every open and at every membership check
// (Member, which opens nothing): the index still holds that path's row
// (not_indexed), one path alone hashing to the id among those it holds
// (ambiguous_reference); the row is not an upstream server's (routed), nor an
// SACD image's virtual track (sacd_virtual_track), at whose path nothing is,
// since the bridge never demuxes (manifest.IsSACDVirtualPath, the
// ContentDirectory's own refusal); the path names a library root configured
// now (outside_library), through the live path resolver; and, at an open, the
// file is opened as the loopback route opens one, with its containment
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
// to the size it had at the open. Its reads and its close fail with errors
// that name no path, and a read of the index that fails is logged by its
// class, never its text, which can quote the stored path.
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
	"math"
	"os"
	"runtime"
	"runtime/debug"
	"slices"
	"syscall"

	"github.com/acoseac/1-bit-bridge/internal/ctxerr"
	"github.com/acoseac/1-bit-bridge/internal/dlna"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/logging"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/trackref"
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
	// ReasonSACDVirtual is a row of an SACD image's virtual track
	// (manifest.IsSACDVirtualPath): nothing is at its path, since the bridge
	// never demuxes, so it is not a track the library serves (B470 PR 5a4).
	ReasonSACDVirtual engine.Reason = "sacd_virtual_track"
	// ReasonIndexUnreadable is an index the resolver could not read.
	ReasonIndexUnreadable engine.Reason = "index_unreadable"
	// ReasonResolverFailed is a panic in the resolver, recovered.
	ReasonResolverFailed engine.Reason = "resolver_failed"
)

// staleSlack is how far past twice the rows its last build read the index may
// grow before the next lookup builds it again: a path the index holds is let
// go only when the row is read again (a lookup of its id, or a build), so the
// paths of rows deleted since grow it, and a build every so many inserts keeps
// it within a few times the library, its cost spread over them.
const staleSlack = 2 * engine.MaxEntries

// readCheck is how many rows the index takes between two looks at its
// context.
const readCheck = 256

// ErrCancelled is what Member answers when its context ended before it could
// tell: it opened nothing and refused nothing.
var ErrCancelled = errors.New("player library: the check was cancelled")

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
	// TrackPathsIndexedAfter calls fn with the path and the stamp of every
	// row stamped past after, in stamp order, suppressed and routed rows
	// included: what the index of the paths by their ids is built and kept
	// current from.
	TrackPathsIndexedAfter(ctx context.Context, after int64, fn func(path string, indexedAt int64) error) error
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
// (playback.Resolver), and answers whether a track id names one of them now
// (Member).
type Resolver struct {
	cfg Config
	log *slog.Logger
	// idOf is a path's reference: dlna.TrackID("", path). A test makes two
	// paths share one.
	idOf func(path string) string

	// turn is held by the one lookup that reads or changes ids: a lookup
	// waits for it with its context.
	turn chan struct{}
	ids  idIndex
}

// idIndex is the paths the library index holds, by their track ids, as of
// every row stamped up to after: a lookup takes the rows stamped since first
// (Store.TrackPathsIndexedAfter), so it is current with every write committed
// before it. It may hold the paths of rows deleted since; a lookup reads each
// path it finds for its id again, and lets go of one whose row is gone.
type idIndex struct {
	byID  map[string][]string
	after int64
	// paths counts the paths byID holds, and built those the last build read
	// (-1 while a build has not read to its end).
	paths, built int
	// reads counts the rows taken from the store, builds every build begun:
	// the tests' measure of the work.
	reads, builds int
}

var _ playback.Resolver = (*Resolver)(nil)

// New builds a resolver. It reads nothing: the index of the paths by their
// ids is built at the first lookup.
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
	r := &Resolver{cfg: cfg, log: cfg.Log, idOf: func(p string) string { return dlna.TrackID("", p) }, turn: make(chan struct{}, 1)}
	r.ids.reset()
	return r, nil
}

// reset empties the index, for a build from the first row.
func (x *idIndex) reset() {
	x.byID, x.after, x.paths, x.built = map[string][]string{}, math.MinInt64, 0, -1
	x.builds++
}

// add puts path in the index under id, once.
func (x *idIndex) add(id, path string) {
	if ps := x.byID[id]; !slices.Contains(ps, path) {
		x.byID[id] = append(ps, path)
		x.paths++
	}
}

// drop lets go of path under id.
func (x *idIndex) drop(id, path string) {
	ps := x.byID[id]
	if i := slices.Index(ps, path); i >= 0 {
		ps = slices.Delete(slices.Clone(ps), i, i+1)
		x.paths--
		if len(ps) == 0 {
			delete(x.byID, id)
		} else {
			x.byID[id] = ps
		}
	}
}

// pathsOf is the stored path id names: the one path, among those the index
// holds for it, whose row the index still has. None is not_indexed, two are
// ambiguous_reference, and a read of the index that fails is its failure
// (indexError). It holds the resolver's turn while it reads, and waits for it
// with ctx.
func (r *Resolver) pathsOf(ctx context.Context, id string) (string, error) {
	select {
	case r.turn <- struct{}{}:
	case <-ctx.Done():
		return "", errCancelled(ctx.Err())
	}
	defer func() { <-r.turn }()
	if err := r.catchUp(ctx, id); err != nil {
		return "", err
	}
	var live []string
	for _, p := range slices.Clone(r.ids.byID[id]) {
		t, err := r.cfg.Store.GetTrack(ctx, p)
		switch {
		case err != nil:
			return "", r.indexError(ctx, err, id, "track")
		case t == nil:
			r.ids.drop(id, p)
		default:
			live = append(live, p)
		}
	}
	switch len(live) {
	case 0:
		return "", refuse(ReasonNotIndexed, "no track the library index holds has this reference")
	case 1:
		return live[0], nil
	}
	return "", refuse(ReasonAmbiguous, fmt.Sprintf("%d tracks the library index holds have this reference", len(live)))
}

// catchUp takes into the index every row stamped since it last read, first
// building it again from the first row when it has grown past staleSlack with
// the paths of rows deleted since its last build. A read the context ends
// keeps what it took whole, every row up to the last stamp it finished, and
// the next lookup goes on from there. The caller holds the turn; id is the
// track id it looks up, for the log.
func (r *Resolver) catchUp(ctx context.Context, id string) error {
	x := &r.ids
	if x.built >= 0 && x.paths > 2*x.built+staleSlack {
		x.reset()
	}
	cur, done, n := x.after, x.after, 0
	err := r.cfg.Store.TrackPathsIndexedAfter(ctx, x.after, func(p string, at int64) error {
		if at != cur {
			// Every row of the stamps before at has been taken.
			done, cur = cur, at
		}
		x.add(r.idOf(p), p)
		x.reads++
		if n++; n%readCheck == 0 {
			return ctx.Err()
		}
		return nil
	})
	if err != nil {
		x.after = done
		return r.indexError(ctx, err, id, "paths")
	}
	x.after = cur
	if x.built < 0 {
		x.built = x.paths
	}
	return nil
}

// errCancelled is a lookup whose context ended: ErrCancelled, and why.
func errCancelled(err error) error { return fmt.Errorf("%w: %w", ErrCancelled, err) }

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
	id, perr := trackref.ParseStored(ref.String())
	if perr != nil {
		return playback.Content{}, refuse(ReasonNotIndexed, "the reference is not one of the library's: a track id, or the canonical URI of a bridge track's /dlna/file/ route")
	}
	abs, err := r.check(ctx, id)
	if errors.Is(err, ErrCancelled) {
		return playback.Content{}, refuse(engine.ReasonMissing, "the open was cancelled")
	}
	if err != nil {
		return playback.Content{}, err
	}
	f, info, err := r.openInside(abs)
	if err != nil {
		return playback.Content{}, err
	}
	fc := content{f: f}
	return playback.Content{Data: fc, Size: info.Size(), Closer: fc}, nil
}

// Member reports whether id, a track id (trackref.IsID), names a track the
// library serves now, by every check an open makes short of the file (the
// package documentation): nil, or the refusal an open would answer (an
// *engine.Unplayable), or ErrCancelled when ctx ended before it could tell.
// It opens nothing, and the open of an occurrence checks again. Its cost does
// not grow with the library: a seek of the stamps' index, a row read for each
// path the index holds for the id, and one for its routing (B470 PR 5a4: an
// OpenHome Insert asks it, so a controller learns at once that a URI names no
// track the bridge plays).
func (r *Resolver) Member(ctx context.Context, id string) (err error) {
	defer func() {
		if p := recover(); p != nil {
			logPanic(r.log, p)
			err = refuse(ReasonResolverFailed, "the library resolver failed; the bridge's log has why")
		}
	}()
	if !trackref.IsID(id) {
		return refuse(ReasonNotIndexed, "a track's id is 16 lower-case hexadecimal digits")
	}
	_, err = r.check(ctx, id)
	return err
}

// check is what an open and a membership check ask of id short of the file:
// the one path the index holds for it whose row is there (pathsOf), a row of
// the bridge's own, not an SACD image's virtual track, in a library root
// configured now. It answers the path resolved onto that root, or why not.
func (r *Resolver) check(ctx context.Context, id string) (string, error) {
	path, err := r.pathsOf(ctx, id)
	if err != nil {
		return "", err
	}
	rt, err := r.cfg.Store.GetUPnPRouting(ctx, path)
	switch {
	case err != nil:
		return "", r.indexError(ctx, err, id, "routing")
	case rt != nil:
		return "", refuse(ReasonRouted, "the track is an upstream server's, and the player plays the bridge's own files")
	}
	if manifest.IsSACDVirtualPath(path) {
		return "", refuse(ReasonSACDVirtual, "the track is an SACD image's virtual track: nothing is at its path, since the bridge never demuxes")
	}
	abs, err := r.cfg.Paths.Resolve(path)
	if err != nil {
		return "", refuse(ReasonOutsideLibrary, "the track's path names no library root configured now")
	}
	return abs, nil
}

// content is a library file as the backend reads it, through the two methods
// it calls, whose errors name no path: an *os.File's are *fs.PathErrors
// naming the file's absolute path, and the backend puts a read's error in
// the entry's skip or fault, which serve logs and 5a's controllers will
// read. io.EOF stays io.EOF, which the readers compare.
type content struct{ f *os.File }

func (c content) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.f.ReadAt(p, off)
	return n, pathless(err)
}

func (c content) Close() error { return pathless(c.f.Close()) }

// pathless is err without the path an *fs.PathError in it names: the
// operation, and the error under it, which it still wraps.
func pathless(err error) error {
	var pe *fs.PathError
	if err == nil || !errors.As(err, &pe) {
		return err
	}
	return &fileError{op: pe.Op, err: pe.Err}
}

// fileError is a library file's operation that failed, naming no path.
type fileError struct {
	op  string
	err error
}

func (e *fileError) Error() string { return "the library file: " + e.op + ": " + e.err.Error() }

func (e *fileError) Unwrap() error { return e.err }

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

// indexError is the failure of a read of the index: one whose context had
// ended (an occurrence released or replaced, an Insert past its budget), its
// cancellation or its deadline, is ErrCancelled and logs nothing (an open
// refuses it as cancelled), and any other is logged by what was read (the
// paths, the track's row, its routing), the error's class
// (manifest.ErrorClass) and the track id, never the error's text, which can
// quote the stored path (GetUPnPRouting's does), and refused
// index_unreadable.
func (r *Resolver) indexError(ctx context.Context, err error, id, read string) error {
	if ctx.Err() != nil || ctxerr.WithoutCancellation(ctx, err) == nil {
		return errCancelled(err)
	}
	r.log.Warn("player: the library index could not be read for an entry; it is skipped",
		"read", read, "class", manifest.ErrorClass(err), "reference", id)
	return refuse(ReasonIndexUnreadable, "the library index could not be read; the bridge's log says which read failed, and how")
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
