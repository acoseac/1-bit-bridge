package engine

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/logging"
)

// The defaults and bounds of the configured values the engine takes.
const (
	// DefaultPauseRelease is Config.PauseRelease when it is 0: B470's 30
	// seconds.
	DefaultPauseRelease = 30 * time.Second
	// DefaultMaintenanceTTL is Config.MaintenanceTTL when it is 0.
	DefaultMaintenanceTTL = time.Minute

	minPauseRelease = time.Second
	maxPauseRelease = 10 * time.Minute
	minMaintenance  = time.Second
	maxMaintenance  = time.Hour
	// shutdownGrace is how long Run waits, once the release has settled and
	// the queue file is flushed, for backend calls still under way before
	// it abandons them.
	shutdownGrace = time.Second
)

// Config is what New builds an engine from. Every configured value is a
// parameter here: 4b2 wires them from the bridge's configuration, and the
// engine reads no configuration of its own.
type Config struct {
	// PlayerUUID is the identity this engine runs. Required, in canonical
	// lowercase UUID form; serve passes the player-uuid it holds under lock.
	PlayerUUID string
	// Backend plays. Required.
	Backend Backend
	// QueuePath is the queue file. Required.
	QueuePath string
	// ReservePath is the ID and token reservation file, "" for QueuePath
	// with ".ids" appended. It must lie below DataDir. Neither path may end
	// in a separator, "." or "..", which name a directory
	// (atomicwrite.NamesDirectory): New refuses it.
	ReservePath string
	// DataDir is the bridge's data directory, which 4b2 passes, and the
	// durable root of every reservation (atomicwrite.WriteBytesDurable):
	// each makes every directory entry between its file and DataDir durable,
	// and never DataDir's own. Required. It must exist (New refuses one that
	// does not, or is not a directory), and the reservation file must lie
	// plainly below it: no ".." element, and no symbolic link or mount point
	// between them (atomicwrite.ErrNotUnderRoot). A data directory that is a
	// link, or lies below one, is taken: the writer resolves it once. It must
	// be on storage that keeps what is written to it through a reboot: New
	// refuses tmpfs, ramfs, and an overlay whose upper layer is one
	// (atomicwrite.ErrVolatile), and storage of which that cannot be told
	// (atomicwrite.ErrMediumUnknown).
	//
	// Nothing makes DataDir's own entry durable: bridge init creates it, and
	// serve creates it where it is missing (lockServeDataDir), and neither
	// syncs it. No ID or token is handed out twice all the same, because
	// DataDir holds the player's identity (player-uuid,
	// config.EnsurePlayerUUID): a crash that takes DataDir takes that
	// identity with it, and the player that comes back is a new device to
	// every controller, which has seen none of its IDs or tokens. On an
	// overlay that does not hold: a DataDir in the lower layer alone, its
	// identity with it, is copied up by the first write, and a crash that
	// took the copy would keep the identity below it, so there the writer
	// syncs every directory from DataDir up to the root of its mount, in
	// New's probe and once each reservation lands, New's first among them
	// (atomicwrite.WriteBytesDurable; Codex on round 10).
	DataDir string
	// PauseRelease is how long a pause holds the device before the backend
	// releases it, passed to every session; 0 is DefaultPauseRelease, and
	// it must be 1 s to 10 min.
	PauseRelease time.Duration
	// DSD is the configured DSD mode, passed to every session.
	DSD string
	// MaintenanceTTL is how long a maintenance hold lasts unrenewed; 0 is
	// DefaultMaintenanceTTL, and it must be 1 s to 1 h.
	MaintenanceTTL time.Duration
	// Log is where the engine logs; nil is the "player" component.
	Log *slog.Logger
	// Seams are a test's holds on the engine from a package above it; nil in
	// production. The package's own tests set seams.
	Seams *Seams

	seams seams
}

// Seams are the engine's test seams for the packages above it (B470 PR 5a4,
// round 2: the OpenHome surface's edit gate is tested against an engine held
// in a reservation write). Each runs on the goroutine that reaches it, and
// may block there; a nil field, like a nil Seams, changes nothing.
type Seams struct {
	// ReserveWrite runs before each reservation write, on the goroutine that
	// makes it (New's, for the run's first): an error it returns is that
	// write's, which fails as one the filesystem refused, and while it
	// blocks the write is under way, as on a filesystem that holds it, so
	// the edits that need it wait, at most ReserveTimeout. A test that holds
	// a write lets it go before the engine stops: Run's shutdown waits for a
	// reservation write under way.
	ReserveWrite func() error
}

// seams is what the tests replace on one engine: the clock, the file
// operations, the source of randomness, and a look at each command as the
// owner admits it (before it asks the command's context) and once it has
// applied it, and at each message once the owner has handled it. A zero
// field is the real one.
type seams struct {
	clock        clock
	writeQueue   func(path string, data []byte) error
	writeReserve func(path string, data []byte) error
	probe        func(path string) error
	readFile     func(path string, limit int64) ([]byte, error)
	lstat        func(path string) (fs.FileInfo, error)
	rename       func(from, to string) error
	random       func(b []byte)
	admitted     func(c *command)
	applied      func(op string)
	handled      func(what string)
	// checked sees a command whose context the owner found alive, before it
	// decides the command's admission (command.admission).
	checked func(c *command)
	// replying sees each reply as it is sent, with the snapshot published
	// then; writerWaits, the queue file writer about to wait, with the
	// newest revision it has seen.
	replying    func(op string, r reply, s *Snapshot)
	writerWaits func(seen uint64)
	// exiting sees each goroutine the engine started at its end, its last
	// message handed over and nothing of it counted any more, so a test can
	// hold it there.
	exiting func()
}

// Engine owns the player's state: one goroutine, the owner, runs every
// command, backend event and output report, one at a time, and publishes
// an immutable Snapshot after each change. Commands are its methods; reads
// are the snapshot's and never wait on the owner.
type Engine struct {
	cfg  Config
	log  *slog.Logger
	clk  clock
	be   Backend
	sm   seams
	run  uint64
	evts *events

	cmds chan *command
	msgs chan any
	done chan struct{}
	// doneOnce closes done: once Run's shutdown ends, or as the owner leaves
	// by a panic (abandon).
	doneOnce sync.Once
	ran      atomic.Bool

	// life is cancelled as Run ends; every backend call's context is
	// derived from it.
	life       context.Context
	lifeCancel context.CancelFunc

	snap    atomic.Pointer[Snapshot]
	chMu    sync.Mutex
	changed chan struct{}

	// live counts the goroutines the engine started and has not joined,
	// each joined by its last message (spawn), wg those that have not
	// returned, abandoned the backend calls Run gave up on (the tests read
	// them).
	wg        sync.WaitGroup
	live      atomic.Int64
	abandoned atomic.Int64

	w *writer
	o owner
}

// New builds an engine: it loads the queue file and the reservation file,
// moving a bad one aside, and makes the reservation of this run durable
// before anything is handed out. A file problem is logged and leaves the
// engine running, refusing what it cannot do safely (Snapshot.Edits). A bad
// Config is an error, a DataDir that is not an existing directory included,
// and so is a reservation file that does not lie plainly below DataDir, or
// whose directories cannot be synced (atomicwrite.ProbeDurable, run before
// anything is read): a filesystem whose directory fsync always fails, as
// some network and FUSE mounts' does, would make every reservation fail and
// the engine refuse every edit for ever, so the error names the directory
// that failed and says the data directory must move to a filesystem that
// can sync a directory; a platform with no directory fsync (Windows) is
// refused the same way, and a directory that is not one, or that the probe
// could not read, is refused saying so, with no advice to move (notDurable).
// Commands wait until Run starts the owner.
func New(cfg Config) (*Engine, error) {
	switch {
	case cfg.Backend == nil:
		return nil, errors.New("player: a Backend is required")
	case cfg.QueuePath == "":
		return nil, errors.New("player: a QueuePath is required")
	case cfg.DataDir == "":
		return nil, errors.New("player: a DataDir is required")
	case !validPlayerUUID(cfg.PlayerUUID):
		return nil, errors.New("player: a canonical lowercase PlayerUUID is required")
	}
	if cfg.ReservePath == "" {
		cfg.ReservePath = cfg.QueuePath + ".ids"
	}
	// A path whose last element names a directory would be written where it
	// cleans to and read as it is given, which a regular file answers ENOTDIR:
	// the restart would find the file stuck and refuse every insert.
	for _, f := range []struct{ what, path string }{{"queue", cfg.QueuePath}, {"reservation", cfg.ReservePath}} {
		if atomicwrite.NamesDirectory(f.path) {
			return nil, fmt.Errorf("player: the %s file's path %s names a directory, not a file: it ends in a separator, \".\" or \"..\"", f.what, f.path)
		}
	}
	if cfg.PauseRelease == 0 {
		cfg.PauseRelease = DefaultPauseRelease
	}
	if cfg.PauseRelease < minPauseRelease || cfg.PauseRelease > maxPauseRelease {
		return nil, fmt.Errorf("player: a pause release of %v is outside %v..%v", cfg.PauseRelease, minPauseRelease, maxPauseRelease)
	}
	if cfg.MaintenanceTTL == 0 {
		cfg.MaintenanceTTL = DefaultMaintenanceTTL
	}
	if cfg.MaintenanceTTL < minMaintenance || cfg.MaintenanceTTL > maxMaintenance {
		return nil, fmt.Errorf("player: a maintenance hold of %v is outside %v..%v", cfg.MaintenanceTTL, minMaintenance, maxMaintenance)
	}
	if cfg.Log == nil {
		cfg.Log = logging.Component("player")
	}
	sm := cfg.seams
	if sm.clock == nil {
		sm.clock = realClock{}
	}
	if sm.writeQueue == nil {
		sm.writeQueue = func(path string, data []byte) error {
			return atomicwrite.WriteBytes(path, data, ".player-queue-*.tmp")
		}
	}
	dataDir := cfg.DataDir
	if sm.writeReserve == nil {
		// A reservation is made only once its bytes and every directory
		// entry between it and the data directory are fsynced: the values it
		// covers are handed out at once, and a rename whose entry is not
		// durable can roll back at a crash, so atomicwrite's tolerance of a
		// failed directory fsync is not taken here.
		sm.writeReserve = func(path string, data []byte) error {
			return atomicwrite.WriteBytesDurable(dataDir, path, data, ".player-ids-*.tmp")
		}
	}
	if s := cfg.Seams; s != nil && s.ReserveWrite != nil {
		write, before := sm.writeReserve, s.ReserveWrite
		sm.writeReserve = func(path string, data []byte) error {
			if err := before(); err != nil {
				return err
			}
			return write(path, data)
		}
	}
	if sm.probe == nil {
		sm.probe = func(path string) error { return atomicwrite.ProbeDurable(dataDir, path) }
	}
	switch info, err := os.Stat(dataDir); {
	case err != nil:
		return nil, fmt.Errorf("player: the data directory %s cannot be used, and it must exist before the player starts: %w", dataDir, err)
	case !info.IsDir():
		return nil, fmt.Errorf("player: the data directory %s is not a directory", dataDir)
	}
	if err := sm.probe(cfg.ReservePath); err != nil {
		return nil, notDurable(cfg.ReservePath, dataDir, err)
	}
	if sm.readFile == nil {
		sm.readFile = readLimited
	}
	if sm.lstat == nil {
		sm.lstat = os.Lstat
	}
	if sm.rename == nil {
		sm.rename = atomicwrite.RenameWithRetry
	}
	if sm.random == nil {
		sm.random = func(b []byte) { _, _ = rand.Read(b) }
	}
	e := &Engine{
		cfg:     cfg,
		log:     cfg.Log,
		clk:     sm.clock,
		be:      cfg.Backend,
		sm:      sm,
		cmds:    make(chan *command),
		msgs:    make(chan any, 256),
		done:    make(chan struct{}),
		changed: make(chan struct{}),
	}
	e.evts = &events{e: e}
	e.life, e.lifeCancel = context.WithCancel(context.Background())
	for e.run == 0 {
		var b [8]byte
		sm.random(b[:])
		e.run = binary.BigEndian.Uint64(b[:])
	}
	e.w = newWriter(e)
	if err := e.start(); err != nil {
		e.lifeCancel()
		return nil, err
	}
	return e, nil
}

// notDurable is New's refusal of a reservation file the barrier probe
// refused: every reservation written there would fail, and the engine would
// refuse every edit for ever, so it does not start. It names the directory
// that failed, the one the probe's *fs.PathError names (the reservation
// file's directory, or one above it up to the data directory), and the
// reservation file's directory when the error names none. It advises moving
// the data directory only where a directory fsync ran and failed
// (atomicwrite.ErrBarrierFailed); a directory the probe could not read (a
// Stat or an open refused: EACCES, EIO) is said to be unreadable, with the
// probe's error, since another filesystem would not mend it; a reservation
// file that does not lie plainly below the data directory is said to be so;
// mounts the platform gives no identity for (atomicwrite.ErrMountsUnknown:
// on Linux, no mount ID from statx or /proc/self/fdinfo) are named as that,
// never as a directory; and a data directory on storage that does not
// outlive a reboot (atomicwrite.ErrVolatile: tmpfs, ramfs, an overlay whose
// upper layer is one, as Raspberry Pi OS's "Overlay File System" sets up), or
// on storage of which that cannot be told (atomicwrite.ErrMediumUnknown), is
// named, with the IDs and tokens that would repeat after a reboot and the
// remedy, persistent storage: the orchestrator's decision on round 9's
// review, to fail closed. An overlay subtree bind (ErrOverlayBind, also
// ErrMediumUnknown) is instead a crash risk even on persistent storage: its
// copy-up parents cannot be synced through that mount.
func notDurable(path, dataDir string, err error) error {
	dir := filepath.Dir(path)
	if pe := (*fs.PathError)(nil); errors.As(err, &pe) {
		dir = pe.Path
	}
	switch {
	case errors.Is(err, atomicwrite.ErrNotUnderRoot):
		return fmt.Errorf("player: the reservation file %s is not below the data directory %s as the strict writer takes it, with no \"..\" element and no symbolic link or mount point between them or at the file, so its durability cannot rest on the data directory's: %w", path, dataDir, err)
	case errors.Is(err, atomicwrite.ErrMountsUnknown):
		return fmt.Errorf("player: the platform gives no mount identity for the data directory %s or an entry below it, so no mount point between it and the reservation file %s can be ruled out: %w", dataDir, path, err)
	case errors.Is(err, atomicwrite.ErrVolatile):
		return fmt.Errorf("player: the data directory %s is on storage that does not outlive a reboot, so the player's IDs and tokens would repeat after a reboot: put the data directory on persistent storage: %w", dataDir, err)
	case errors.Is(err, atomicwrite.ErrOverlayBind):
		return fmt.Errorf("player: the data directory %s is on an overlay bind whose copy-up parents cannot be synced, even on persistent storage, so a crash could lose the reservation and repeat the player's IDs and tokens: %w", dataDir, err)
	case errors.Is(err, atomicwrite.ErrMediumUnknown):
		return fmt.Errorf("player: whether the data directory %s outlives a reboot cannot be told, and on storage that does not, the player's IDs and tokens would repeat after a reboot: put the data directory on persistent storage: %w", dataDir, err)
	case errors.Is(err, atomicwrite.ErrNoDirectoryBarrier):
		return fmt.Errorf("player: the reservation file's directory %s cannot be synced: the player keeps its IDs and tokens durable with a directory fsync, which this platform does not have: %w", filepath.Dir(path), err)
	case errors.Is(err, syscall.ENOTDIR):
		return fmt.Errorf("player: %s is not a directory, so no reservation can be written to %s: %w", dir, path, err)
	case errors.Is(err, atomicwrite.ErrBarrierFailed):
		return fmt.Errorf("player: the directory %s cannot be synced, so no ID or token reserved in %s would survive a crash: move the data directory to a filesystem that can sync a directory: %w", dir, path, err)
	}
	return fmt.Errorf("player: the directory %s cannot be read, so no reservation can be written to %s: %w", dir, path, err)
}

// Run is the owner: it runs commands and events until ctx ends, then shuts
// down. Shutdown refuses every mutation from its first moment, releases
// the session (at most ReleaseTimeout), flushes the queue file's newest
// revision, waits shutdownGrace for backend calls still under way and
// abandons the rest, ends the owner and joins every goroutine the engine
// started but an abandoned call, each by its last message, which comes
// once nothing of the goroutine is left to count (spawn). The flush is
// atomicwrite's, synchronous and fsynced, and an fsync cannot be
// cancelled, so a filesystem that hangs there holds Run past that bound.
// Run returns once, and nil; a second Run is an error.
//
// A panic on the owner is not recovered here: it goes on to Run's caller,
// and its shutdown never runs. On its way out the engine ends what the
// shutdown would have (abandon): every backend call's context ends, the
// queue file's writer writes the newest revision it was handed and exits,
// and Done closes, so nothing waits on an owner that is gone. The session
// the backend plays is not released: that is the caller's, through the
// backend (serve's player has it release every session it holds).
func (e *Engine) Run(ctx context.Context) error {
	if !e.ran.CompareAndSwap(false, true) {
		return errors.New("player: Run called twice")
	}
	e.loop(ctx)
	return nil
}

// Done is closed once Run has returned, or is about to, a return or a panic
// on the owner.
func (e *Engine) Done() <-chan struct{} { return e.done }

// Snapshot is the state after the last change. It never waits on the
// owner.
func (e *Engine) Snapshot() *Snapshot { return e.snap.Load() }

// Changed returns a channel that is closed the next time a snapshot is
// published, so a reader learns of a change without polling.
func (e *Engine) Changed() <-chan struct{} {
	e.chMu.Lock()
	defer e.chMu.Unlock()
	return e.changed
}

// Read is the entry id, from the current snapshot.
func (e *Engine) Read(id uint32) (Entry, bool) { return e.Snapshot().Read(id) }

// ReadList is Snapshot.ReadList on the current snapshot.
func (e *Engine) ReadList(ids []uint32) ([]Entry, error) { return e.Snapshot().ReadList(ids) }

// IDArray is the token and the IDs, from one snapshot.
func (e *Engine) IDArray() (uint32, []uint32) { return e.Snapshot().IDArray() }

// Play plays: from stopped, the selection (the first entry when none); from
// paused, on from the paused position. The package documentation's table
// says what it does in every state.
func (e *Engine) Play(ctx context.Context, conds ...Cond) error {
	return e.do(&command{op: opPlay, ctx: ctx, conds: conds}).err
}

// Pause pauses playback, keeping the position.
func (e *Engine) Pause(ctx context.Context, conds ...Cond) error {
	return e.do(&command{op: opPause, ctx: ctx, conds: conds}).err
}

// Stop stops playback and releases the device, within ReleaseTimeout; it
// answers once the release is confirmed or its deadline has passed
// (Snapshot.ReleaseUnconfirmed).
func (e *Engine) Stop(ctx context.Context, conds ...Cond) error {
	return e.do(&command{op: opStop, ctx: ctx, conds: conds}).err
}

// Next plays the entry after the selection, in every state; past the last
// entry with repeat off it stops (the package documentation's table).
func (e *Engine) Next(ctx context.Context, conds ...Cond) error {
	return e.do(&command{op: opNext, ctx: ctx, conds: conds}).err
}

// Previous plays the entry before the selection, or the selection again at
// the head, in every state.
func (e *Engine) Previous(ctx context.Context, conds ...Cond) error {
	return e.do(&command{op: opPrevious, ctx: ctx, conds: conds}).err
}

// PlayID plays entry id from its start.
func (e *Engine) PlayID(ctx context.Context, id uint32, conds ...Cond) error {
	return e.do(&command{op: opPlayID, ctx: ctx, conds: conds, id: id}).err
}

// PlayIndex plays the entry at index, looked up by the owner as it plays
// it (5a's SeekIndex).
func (e *Engine) PlayIndex(ctx context.Context, index int, conds ...Cond) error {
	return e.do(&command{op: opPlayIndex, ctx: ctx, conds: conds, index: index}).err
}

// InsertAfter inserts an entry after afterID, at the head when afterID is
// 0, and returns its ID. meta is copied before the owner sees it, so the
// caller may reuse it as soon as InsertAfter returns, whatever it returned;
// metadata past MaxMetadataBytes is refused without a copy.
func (e *Engine) InsertAfter(ctx context.Context, afterID uint32, ref Reference, meta []byte, conds ...Cond) (uint32, error) {
	c := &command{op: opInsert, ctx: ctx, conds: conds, id: afterID, ref: ref, metaLen: len(meta)}
	if len(meta) <= MaxMetadataBytes {
		c.meta = slices.Clone(meta)
	}
	r := e.do(c)
	return r.id, r.err
}

// Delete deletes entry id.
func (e *Engine) Delete(ctx context.Context, id uint32, conds ...Cond) error {
	return e.do(&command{op: opDelete, ctx: ctx, conds: conds, id: id}).err
}

// DeleteAll empties the queue, stops playback and selects nothing.
func (e *Engine) DeleteAll(ctx context.Context, conds ...Cond) error {
	return e.do(&command{op: opDeleteAll, ctx: ctx, conds: conds}).err
}

// SetRepeat sets repeat-all.
func (e *Engine) SetRepeat(ctx context.Context, on bool, conds ...Cond) error {
	return e.do(&command{op: opSetRepeat, ctx: ctx, conds: conds, on: on}).err
}

// SetShuffle takes false; true is release 7a's and refused with
// ReasonNotInRelease.
func (e *Engine) SetShuffle(ctx context.Context, on bool, conds ...Cond) error {
	return e.do(&command{op: opSetShuffle, ctx: ctx, conds: conds, on: on}).err
}

// ReportOutput gives the owner the output's availability (OutputReport).
func (e *Engine) ReportOutput(ctx context.Context, r OutputReport) error {
	return e.do(&command{op: opOutput, ctx: ctx, report: r}).err
}

// EnterMaintenance checks want against the generation and, in the same
// step, closes the engine to mutations; then it releases the session,
// within ReleaseTimeout, and drops all pending backend work. It answers
// once the release is confirmed, or its deadline has passed
// (Hold.ReleaseConfirmed false: entered, release unconfirmed). The lease is
// the caller's alone. A second enter is refused with ReasonMaintenance.
func (e *Engine) EnterMaintenance(ctx context.Context, want Generation, reason string) (Hold, error) {
	r := e.do(&command{op: opEnterMaint, ctx: ctx, want: want, reason: reason})
	return r.hold, r.err
}

// RenewMaintenance extends the hold to MaintenanceTTL from now and returns
// its new expiry. A wrong or stale lease is refused with ReasonLease.
func (e *Engine) RenewMaintenance(ctx context.Context, lease Lease) (time.Time, error) {
	r := e.do(&command{op: opRenewMaint, ctx: ctx, lease: lease})
	return r.expires, r.err
}

// ExitMaintenance ends the hold; the transport is stopped, never resumed.
// A wrong or stale lease is refused with ReasonLease.
func (e *Engine) ExitMaintenance(ctx context.Context, lease Lease) error {
	return e.do(&command{op: opExitMaint, ctx: ctx, lease: lease}).err
}

// op is a command's kind.
type op int

const (
	opPlay op = iota
	opPause
	opStop
	opNext
	opPrevious
	opPlayID
	opPlayIndex
	opInsert
	opDelete
	opDeleteAll
	opSetRepeat
	opSetShuffle
	opOutput
	opEnterMaint
	opRenewMaint
	opExitMaint
)

var opNames = [...]string{"play", "pause", "stop", "next", "previous", "play id", "play index", "insert", "delete", "delete all",
	"set repeat", "set shuffle", "report output", "enter maintenance", "renew maintenance", "exit maintenance"}

func (o op) String() string { return opNames[o] }

// command is one call of a method, and where its reply goes.
type command struct {
	op    op
	ctx   context.Context
	conds []Cond
	id    uint32
	index int
	ref   Reference
	// meta is the owner's copy of an insert's metadata, and metaLen the
	// caller's length, which a refusal past MaxMetadataBytes names.
	meta    []byte
	metaLen int
	on      bool
	report  OutputReport
	lease   Lease
	reason  string
	want    Generation

	reply   chan reply
	replied bool
	parked  bool
	// admission is decided once, between the caller and the owner: pending
	// until the owner admits the command (it runs) or its caller, whose
	// context ended first, abandons it (it does not run, and the caller is
	// told so: ErrNotAdmitted).
	admission atomic.Int32
}

// A command's admission (command.admission).
const (
	admissionPending int32 = iota
	admissionAdmitted
	admissionAbandoned
)

// reply is a command's answer.
type reply struct {
	err     error
	id      uint32
	hold    Hold
	expires time.Time
}

// do hands c to the owner and waits for its reply. A context that has
// ended, or ends before the owner admits c, means c does not run, and the
// refusal wraps ErrNotAdmitted: before the owner takes it, and once taken
// and not yet admitted, where do abandons it (command.admission), so the
// owner refuses it in turn; one that ends after the owner admitted c means
// c may run and its reply is dropped. After shutdown every command is
// answered ReasonClosed at once. c holds nothing of the caller's once it is
// sent: do copies the conditions here, and InsertAfter the metadata, so a
// caller that returns early may reuse its slices while the owner still runs
// the command.
func (e *Engine) do(c *command) reply {
	c.conds = slices.Clone(c.conds)
	name := c.op.String()
	if err := c.ctx.Err(); err != nil {
		return reply{err: notAdmitted(name, err)}
	}
	select {
	case <-e.done:
		return reply{err: refuse(name, ReasonClosed, "the engine has shut down")}
	default:
	}
	c.reply = make(chan reply, 1)
	select {
	case e.cmds <- c:
	case <-c.ctx.Done():
		return reply{err: notAdmitted(name, c.ctx.Err())}
	case <-e.done:
		return reply{err: refuse(name, ReasonClosed, "the engine has shut down")}
	}
	select {
	case r := <-c.reply:
		return r
	case <-c.ctx.Done():
		select {
		case r := <-c.reply:
			return r
		default:
		}
		if c.admission.CompareAndSwap(admissionPending, admissionAbandoned) {
			// The owner has taken c and not admitted it, and now never will.
			return reply{err: notAdmitted(name, c.ctx.Err())}
		}
		return reply{err: &Error{Reason: ReasonCancelled, Op: name, Detail: "the context ended while the command ran: it may have taken effect", Err: c.ctx.Err()}}
	case <-e.done:
		select {
		case r := <-c.reply:
			return r
		default:
		}
		return reply{err: refuse(name, ReasonClosed, "the engine has shut down")}
	}
}

// WithAdmission returns a context derived from ctx and a channel the owner
// closes once it admits the command that context is passed with (B470 PR
// 5a4, round 2): from then on the command's caller no longer decides whether
// it runs (a context that ends after it is answered ReasonCancelled without
// ErrNotAdmitted), and its answer comes once it has run, which for a Stop is
// once its release is confirmed or past its deadline. A command the owner
// never admits (its context ended first, its caller abandoned it, the engine
// had shut down) never closes it. The OpenHome surface gives a Stop's request
// slot back there, so the Stops that wait for a release hold no slot. A
// context so made is for one command: another passed it closes nothing more.
func WithAdmission(ctx context.Context) (context.Context, <-chan struct{}) {
	n := &admissionNote{ch: make(chan struct{})}
	return context.WithValue(ctx, admissionKey{}, n), n.ch
}

// admissionKey carries an admissionNote in a command's context.
type admissionKey struct{}

// admissionNote is WithAdmission's channel, closed once.
type admissionNote struct {
	once sync.Once
	ch   chan struct{}
}

// noteAdmission closes the channel c's context carries (WithAdmission), if
// it carries one: the owner has admitted c. Closing never blocks the owner.
func noteAdmission(c *command) {
	if n, ok := c.ctx.Value(admissionKey{}).(*admissionNote); ok {
		n.once.Do(func() { close(n.ch) })
	}
}

// notAdmitted is a command whose context ended before the owner admitted
// it: it wraps ErrNotAdmitted and the context's error.
func notAdmitted(name string, err error) error {
	return &Error{Reason: ReasonCancelled, Op: name, Detail: "the context ended before the command was admitted: it did not run",
		Err: fmt.Errorf("%w: %w", ErrNotAdmitted, err)}
}

// answer gives c its reply, once. The reply is sent when the step that
// decided it has published its snapshot, so a caller reads its own change
// as soon as its command returns; replies never block the owner.
func (e *Engine) answer(c *command, r reply) {
	if c.replied {
		return
	}
	c.replied = true
	e.o.outbox = append(e.o.outbox, answered{c: c, r: r})
}

// answered is a reply waiting for its step's snapshot.
type answered struct {
	c *command
	r reply
}

// sendReplies sends the step's replies, each command's channel holding
// one, and then tells the tests' seams what the step applied and handled.
func (e *Engine) sendReplies() {
	for _, a := range e.o.outbox {
		if e.sm.replying != nil {
			e.sm.replying(a.c.op.String(), a.r, e.snap.Load())
		}
		a.c.reply <- a.r
	}
	clear(e.o.outbox)
	e.o.outbox = e.o.outbox[:0]
	for _, n := range e.o.notes {
		if n.applied {
			e.sm.applied(n.what)
		} else {
			e.sm.handled(n.what)
		}
	}
	e.o.notes = e.o.notes[:0]
}

// note is a step's command applied or message handled, for the seams.
type note struct {
	what    string
	applied bool
}

// post hands the owner a message from another goroutine; after Run it
// returns at once.
func (e *Engine) post(m any) {
	select {
	case e.msgs <- m:
	case <-e.done:
	}
}

// panicMessage is the line a panic the engine recovers is logged with.
const panicMessage = "player: a panic was recovered in the engine; what panicked fails as it would on an error"

// logPanic logs a panic recovered on one of the engine's goroutines, once:
// where, its stack, and its value only when it is the runtime's own error,
// which carries nothing the engine was handed (a value code panics with
// could quote a path).
func (e *Engine) logPanic(where string, r any) {
	attrs := []any{"where", where, "stack", string(debug.Stack())}
	if rerr, ok := r.(runtime.Error); ok {
		attrs = append(attrs, "error", rerr.Error())
	}
	e.log.Error(panicMessage, attrs...)
}

// spawn runs f on a goroutine the engine counts, and hands the owner what f
// returns, the goroutine's last message, once it no longer counts: f's
// work, its own deferred calls with it, is done by then, so the owner, which
// joins each goroutine by its last message (a call's answer, a reservation
// write's result, the writer's exit), has joined it once it has that
// message, and Run returns with an abandoned call alone still counted. A
// goroutine counted until it returned, after its message, was still counted
// when Run returned (B526). Each f recovers its own work's panic as that
// work's failure: a backend call's (callBackend), a reservation write's
// (reserve), the queue file writer's (writer.run, writer.attempt).
func (e *Engine) spawn(f func() any) {
	e.wg.Add(1)
	e.live.Add(1)
	go func() {
		defer e.wg.Done()
		last := f()
		e.live.Add(-1)
		e.post(last)
		if e.sm.exiting != nil {
			e.sm.exiting()
		}
	}()
}

// publish stores s and wakes every Changed reader.
func (e *Engine) publish(s *Snapshot) {
	e.snap.Store(s)
	e.chMu.Lock()
	close(e.changed)
	e.changed = make(chan struct{})
	e.chMu.Unlock()
}
