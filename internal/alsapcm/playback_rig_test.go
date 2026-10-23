package alsapcm_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
)

// The player's backend (internal/player/playback) is tested here, beside
// the fake kernel it plays through, because only an external test package
// in this directory can reach the fake (export_test.go). The tests run the
// engine's Run with the backend over the real Sink, the fake node playing
// the simulated time each test grants, and the engine on the real clock: no
// test sleeps, and every wait is on a signal under the watchdog.

var bg = context.Background()

// waitOn waits until cond holds, looking again each time changed's channel
// closes, under the watchdog.
func waitOn(t *testing.T, what string, changed func() <-chan struct{}, cond func() bool) {
	t.Helper()
	timer := time.NewTimer(alsapcm.Watchdog())
	defer timer.Stop()
	for {
		ch := changed()
		if cond() {
			return
		}
		select {
		case <-ch:
		case <-timer.C:
			t.Fatalf("waited %v for %s", alsapcm.Watchdog(), what)
		}
	}
}

// files is the resolver: what each reference names, and every content it
// handed out, to check each was closed.
type files struct {
	mu      sync.Mutex
	content map[string][]byte
	fail    map[string]error
	opened  []*tracked
	block   map[string]chan struct{} // an open that waits for the channel's close
	stalls  map[string]*stall        // reads that do not return (stall)
	changed chan struct{}            // closed and replaced at each content's close
}

func newFiles() *files {
	return &files{content: map[string][]byte{}, fail: map[string]error{}, block: map[string]chan struct{}{},
		stalls: map[string]*stall{}, changed: make(chan struct{})}
}

// tracked is one content the resolver handed out.
type tracked struct {
	ref    string
	closed bool
	f      *files
	st     *stall
}

func (c *tracked) Close() error {
	if c.st != nil {
		c.st.mu.Lock()
		if c.st.in > 0 {
			c.st.closedUnder = true
		}
		c.st.mu.Unlock()
	}
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.closed = true
	close(c.f.changed)
	c.f.changed = make(chan struct{})
	return nil
}

// stall is a read of a content held inside its ReadAt (files.stall).
type stall struct {
	at     int64
	parked chan struct{}
	gate   chan struct{}
	held   sync.Once // parked closed
	open   sync.Once // gate closed

	mu          sync.Mutex
	in          int  // the reads held now
	closedUnder bool // the content was closed while a read of it was held
}

// stalledReader is a content whose reads past its stall's byte wait for
// the stall's release.
type stalledReader struct {
	r  io.ReaderAt
	st *stall
}

func (s stalledReader) ReadAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > s.st.at {
		s.st.mu.Lock()
		s.st.in++
		s.st.mu.Unlock()
		s.st.held.Do(func() { close(s.st.parked) })
		<-s.st.gate
		s.st.mu.Lock()
		s.st.in--
		s.st.mu.Unlock()
	}
	return s.r.ReadAt(p, off)
}

// stall holds every read of ref's content that reaches past byte at inside
// its ReadAt until release is called: a read that does not return, as one
// of a file on a mount that hangs does, which no context ends. parked is
// closed once a read is held.
func (f *files) stall(ref string, at int64) (parked <-chan struct{}, release func()) {
	st := &stall{at: at, parked: make(chan struct{}), gate: make(chan struct{})}
	f.mu.Lock()
	f.stalls[ref] = st
	f.mu.Unlock()
	return st.parked, func() { st.open.Do(func() { close(st.gate) }) }
}

// isClosed reports whether a content of ref was handed out and closed.
func (f *files) isClosed(ref string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.opened {
		if c.ref == ref && c.closed {
			return true
		}
	}
	return false
}

// closedUnderARead reports whether a content of ref was closed while a read
// of it was held in its ReadAt.
func (f *files) closedUnderARead(ref string) bool {
	f.mu.Lock()
	st := f.stalls[ref]
	f.mu.Unlock()
	if st == nil {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.closedUnder
}

// fed waits until the producer has given the sink every frame of ref: it
// closes an occurrence's content at its decoder's end. A grant of
// simulated time plays at once, faster than any producer decodes, so a test
// that grants more than the hardware buffer holds waits for this first, or
// the fake DAC runs dry.
func (f *files) fed(t *testing.T, ref string) {
	t.Helper()
	waitOn(t, "every frame of "+ref+" in the sink", func() <-chan struct{} {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.changed
	}, func() bool { return f.isClosed(ref) })
}

func (f *files) add(ref string, b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.content[ref] = b
}

func (f *files) Open(ctx context.Context, ref engine.Reference) (playback.Content, error) {
	f.mu.Lock()
	b, ok := f.content[ref.String()]
	fail := f.fail[ref.String()]
	block := f.block[ref.String()]
	st := f.stalls[ref.String()]
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return playback.Content{}, ctx.Err()
		}
	}
	if fail != nil {
		return playback.Content{}, fail
	}
	if !ok {
		return playback.Content{}, fmt.Errorf("no file %q", ref)
	}
	c := &tracked{ref: ref.String(), f: f, st: st}
	f.mu.Lock()
	f.opened = append(f.opened, c)
	f.mu.Unlock()
	var data io.ReaderAt = bytes.NewReader(b)
	if st != nil {
		data = stalledReader{r: data, st: st}
	}
	return playback.Content{Data: data, Size: int64(len(b)), Closer: c}, nil
}

// allClosed fails the test unless every content handed out was closed.
func (f *files) allClosed(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.opened {
		if !c.closed {
			t.Errorf("the content of %q was never closed", c.ref)
		}
	}
}

// unit is the locator: the fake's endpoint, or where a test moved it, and a
// check after open the test scripts.
type unit struct {
	mu      sync.Mutex
	at      alsapcm.Endpoint
	err     error
	refuse  int // the next checks to refuse
	checked []alsapcm.NodeInfo
	located int
	// holdIn and holdGate hold the next check (holdNextAdmit).
	holdIn, holdGate chan struct{}
}

func newUnit() *unit { return &unit{at: alsapcm.FakeEndpoint()} }

// holdNextAdmit holds the next check after open until release, as a check
// that reads a slow sysfs does: the node is open, and no sink exists yet
// whose writer would close it. held is closed once the check is held.
func (u *unit) holdNextAdmit() (held <-chan struct{}, release func()) {
	in, gate := make(chan struct{}), make(chan struct{})
	u.mu.Lock()
	u.holdIn, u.holdGate = in, gate
	u.mu.Unlock()
	var once sync.Once
	return in, func() { once.Do(func() { close(gate) }) }
}

func (u *unit) Locate(context.Context) (playback.Unit, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.located++
	if u.err != nil {
		return playback.Unit{}, u.err
	}
	return playback.Unit{Endpoint: u.at, Admit: u.admit, Label: "the fake DAC at " + u.at.String()}, nil
}

func (u *unit) admit(n alsapcm.NodeInfo) error {
	u.mu.Lock()
	in, gate := u.holdIn, u.holdGate
	u.holdIn, u.holdGate = nil, nil
	u.mu.Unlock()
	if in != nil {
		close(in)
		<-gate
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.checked = append(u.checked, n)
	if u.refuse > 0 {
		u.refuse--
		return errors.New("card 5 is another unit now")
	}
	if n.Card != u.at.Card {
		return fmt.Errorf("the node opened is card %d, and the binding names card %d", n.Card, u.at.Card)
	}
	return nil
}

// event is one event the backend reported.
type event struct {
	kind  string
	s     engine.SessionID
	o     engine.OccurrenceID
	join  engine.Join
	why   engine.Unplayable
	pos   engine.Position
	fault engine.Fault
}

func (e event) String() string {
	switch e.kind {
	case "prepared":
		return fmt.Sprintf("prepared %d gapless=%v %v", e.o, e.join.Gapless, e.join.Differs)
	case "refused":
		return fmt.Sprintf("refused %d %s", e.o, e.why.Reason)
	case "position":
		return fmt.Sprintf("position %d %d", e.pos.Occurrence, e.pos.Frames)
	case "fault":
		return fmt.Sprintf("fault %s at %d+%d", e.fault.Reason, e.fault.Occurrence, e.fault.Frames)
	case "submitted", "audible":
		return fmt.Sprintf("%s %d", e.kind, e.o)
	}
	return e.kind
}

// call is one backend call the engine made.
type call struct {
	kind  string
	s     engine.SessionID
	after engine.OccurrenceID
	next  *engine.Occurrence
	from  engine.OccurrenceID
	ok    bool
	err   error
}

// recorder keeps what the backend reported and what the engine asked of
// it, in order, and tells waiters of each.
type recorder struct {
	mu      sync.Mutex
	evs     []event
	calls   []call
	changed chan struct{}
}

func newRecorder() *recorder { return &recorder{changed: make(chan struct{})} }

func (r *recorder) note(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f()
	close(r.changed)
	r.changed = make(chan struct{})
}

func (r *recorder) ch() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.changed
}

func (r *recorder) events() []event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.evs)
}

func (r *recorder) callList() []call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

// recEvents records each event and passes it on to the engine.
type recEvents struct {
	r    *recorder
	next engine.Events
}

func (v *recEvents) add(e event) { v.r.note(func() { v.r.evs = append(v.r.evs, e) }) }

func (v *recEvents) Prepared(s engine.SessionID, o engine.OccurrenceID, j engine.Join) {
	v.add(event{kind: "prepared", s: s, o: o, join: j})
	v.next.Prepared(s, o, j)
}

func (v *recEvents) Refused(s engine.SessionID, o engine.OccurrenceID, why engine.Unplayable) {
	v.add(event{kind: "refused", s: s, o: o, why: why})
	v.next.Refused(s, o, why)
}

func (v *recEvents) Submitted(s engine.SessionID, o engine.OccurrenceID) {
	v.add(event{kind: "submitted", s: s, o: o})
	v.next.Submitted(s, o)
}

func (v *recEvents) Audible(s engine.SessionID, o engine.OccurrenceID) {
	v.add(event{kind: "audible", s: s, o: o})
	v.next.Audible(s, o)
}

func (v *recEvents) Position(s engine.SessionID, p engine.Position) {
	v.add(event{kind: "position", s: s, o: p.Occurrence, pos: p})
	v.next.Position(s, p)
}

func (v *recEvents) Fault(s engine.SessionID, f engine.Fault) {
	v.add(event{kind: "fault", s: s, o: f.Occurrence, fault: f})
	v.next.Fault(s, f)
}

func (v *recEvents) Finished(s engine.SessionID) {
	v.add(event{kind: "finished", s: s})
	v.next.Finished(s)
}

func (v *recEvents) DeviceReleased(s engine.SessionID) {
	v.add(event{kind: "device released", s: s})
	v.next.DeviceReleased(s)
}

func (v *recEvents) Released(s engine.SessionID) {
	v.add(event{kind: "released", s: s})
	v.next.Released(s)
}

// tee is the backend as the engine sees it: every call recorded, and every
// session's events through the recorder.
type tee struct {
	*playback.Backend
	r *recorder
}

func (b tee) Start(ctx context.Context, req engine.StartRequest) error {
	req.Events = &recEvents{r: b.r, next: req.Events}
	err := b.Backend.Start(ctx, req)
	b.r.note(func() { b.r.calls = append(b.r.calls, call{kind: "start", s: req.Session, err: err}) })
	return err
}

func (b tee) Prepare(ctx context.Context, s engine.SessionID, after engine.OccurrenceID, next *engine.Occurrence) error {
	err := b.Backend.Prepare(ctx, s, after, next)
	var n *engine.Occurrence
	if next != nil {
		cp := *next
		n = &cp
	}
	b.r.note(func() { b.r.calls = append(b.r.calls, call{kind: "prepare", s: s, after: after, next: n, err: err}) })
	return err
}

func (b tee) Cut(ctx context.Context, s engine.SessionID, from engine.OccurrenceID) (bool, error) {
	ok, err := b.Backend.Cut(ctx, s, from)
	b.r.note(func() { b.r.calls = append(b.r.calls, call{kind: "cut", s: s, from: from, ok: ok, err: err}) })
	return ok, err
}

// nopEvents is the engine's side of a session a test drives itself.
type nopEvents struct{}

func (nopEvents) Prepared(engine.SessionID, engine.OccurrenceID, engine.Join)      {}
func (nopEvents) Refused(engine.SessionID, engine.OccurrenceID, engine.Unplayable) {}
func (nopEvents) Submitted(engine.SessionID, engine.OccurrenceID)                  {}
func (nopEvents) Audible(engine.SessionID, engine.OccurrenceID)                    {}
func (nopEvents) Position(engine.SessionID, engine.Position)                       {}
func (nopEvents) Fault(engine.SessionID, engine.Fault)                             {}
func (nopEvents) Finished(engine.SessionID)                                        {}
func (nopEvents) DeviceReleased(engine.SessionID)                                  {}
func (nopEvents) Released(engine.SessionID)                                        {}

// direct is the backend over the fake kernel with no engine: a test calls
// it as the engine would, and reads what it reported.
type direct struct {
	t     *testing.T
	k     *alsapcm.FakeKernel
	be    *playback.Backend
	rec   *recorder
	files *files
	unit  *unit
	ticks chan time.Time
}

func newDirect(t *testing.T, openSink func(func(context.Context, alsapcm.SinkConfig) (*alsapcm.Sink, error)) func(context.Context, alsapcm.SinkConfig) (*alsapcm.Sink, error)) *direct {
	t.Helper()
	return newDirectWith(t, openSink, nil)
}

// newDirectWith is newDirect with edit applied to the backend's Config.
func newDirectWith(t *testing.T, openSink func(func(context.Context, alsapcm.SinkConfig) (*alsapcm.Sink, error)) func(context.Context, alsapcm.SinkConfig) (*alsapcm.Sink, error), edit func(*playback.Config)) *direct {
	t.Helper()
	d := &direct{t: t, k: alsapcm.NewFakeKernel(t), rec: newRecorder(), files: newFiles(), unit: newUnit(), ticks: make(chan time.Time)}
	open := d.k.OpenSink
	if openSink != nil {
		open = openSink(open)
	}
	cfg := playback.Config{Resolver: d.files, Locator: d.unit, OpenSink: open,
		Tick: func() (<-chan time.Time, func()) { return d.ticks, func() {} }}
	if edit != nil {
		edit(&cfg)
	}
	be, err := playback.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d.be = be
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(bg, alsapcm.Watchdog())
		defer cancel()
		if err := be.Wait(ctx); err != nil {
			t.Errorf("the backend's goroutines: %v", err)
		}
	})
	return d
}

// occurrence is an engine occurrence of ref.
func occurrence(id engine.OccurrenceID, entry uint32, ref string) engine.Occurrence {
	r, err := engine.NewReference(ref)
	if err != nil {
		panic(err)
	}
	return engine.Occurrence{ID: id, Entry: entry, Ref: r}
}

// start starts session s with ref as its first occurrence (id 1).
func (d *direct) start(s engine.SessionID, ref string) error {
	return d.be.Start(bg, engine.StartRequest{Session: s, First: occurrence(engine.OccurrenceID(s)*100+1, 1, ref),
		Options: engine.SessionOptions{PauseRelease: time.Second, DSD: "off"}, Events: &recEvents{r: d.rec, next: nopEvents{}}})
}

// tick has the session's reporter measure once, as rig.tick does: the
// second send is the barrier.
func (d *direct) tick() {
	d.t.Helper()
	for range 2 {
		select {
		case d.ticks <- time.Time{}:
		case <-time.After(alsapcm.Watchdog()):
			d.t.Fatalf("no reporter took a tick within %v", alsapcm.Watchdog())
		}
	}
}

// release releases s and fails the test unless it is confirmed.
func (d *direct) release(s engine.SessionID) {
	d.t.Helper()
	if err := d.be.Release(bg, s); err != nil {
		d.t.Fatalf("Release(%d): %v", s, err)
	}
}

// rig is one engine and backend over the fake kernel.
type rig struct {
	t     *testing.T
	k     *alsapcm.FakeKernel
	be    *playback.Backend
	eng   *engine.Engine
	rec   *recorder
	files *files
	unit  *unit
	ticks chan time.Time
	notes chan playback.Note

	cancel context.CancelFunc
	ran    chan struct{}
	outGen uint64
}

// rigOptions are what a test changes before the rig starts.
type rigOptions struct {
	pauseRelease time.Duration
	locator      playback.Locator
	openSink     func(func(context.Context, alsapcm.SinkConfig) (*alsapcm.Sink, error)) func(context.Context, alsapcm.SinkConfig) (*alsapcm.Sink, error)
	afterFunc    func(time.Duration, func()) func() bool
	realTick     bool
	seams        *playback.Seams
}

// newRig starts an engine with the backend over a fresh fake kernel, and
// reports the output available. The engine's data directory is the test's,
// on storage that outlives a reboot: the engine refuses one that does not,
// and platforms without a directory fsync (Windows), where the player does
// not run.
func newRig(t *testing.T, opts ...func(*rigOptions)) *rig {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the engine keeps its IDs durable with a directory fsync, which Windows has not: the player runs on Linux")
	}
	var o rigOptions
	for _, f := range opts {
		f(&o)
	}
	r := &rig{t: t, k: alsapcm.NewFakeKernel(t), rec: newRecorder(), files: newFiles(), unit: newUnit(),
		ticks: make(chan time.Time), notes: make(chan playback.Note, 64), ran: make(chan struct{})}
	var loc playback.Locator = r.unit
	if o.locator != nil {
		loc = o.locator
	}
	open := r.k.OpenSink
	if o.openSink != nil {
		open = o.openSink(open)
	}
	cfg := playback.Config{
		Resolver: r.files, Locator: loc, OpenSink: open, AfterFunc: o.afterFunc, Seams: o.seams,
		Notify: func(n playback.Note) {
			select {
			case r.notes <- n:
			default:
			}
		},
	}
	if !o.realTick {
		cfg.Tick = func() (<-chan time.Time, func()) { return r.ticks, func() {} }
	}
	be, err := playback.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.be = be
	data := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(data, 0o700); err != nil {
		t.Fatal(err)
	}
	eng, err := engine.New(engine.Config{Backend: tee{Backend: be, r: r.rec}, QueuePath: filepath.Join(data, "queue.json"),
		DataDir: data, PauseRelease: o.pauseRelease, DSD: "off"})
	if err != nil {
		t.Skipf("the engine does not start on this test's data directory: %v", err)
	}
	r.eng = eng
	ctx, cancel := context.WithCancel(bg)
	r.cancel = cancel
	go func() {
		defer close(r.ran)
		_ = eng.Run(ctx)
	}()
	t.Cleanup(r.stop)
	r.available()
	return r
}

// stop shuts the engine down, waits for Run and the backend's goroutines,
// and checks what every session left: no overlap, every content closed,
// every open of the node closed once.
func (r *rig) stop() {
	r.t.Helper()
	r.cancel()
	select {
	case <-r.ran:
	case <-time.After(alsapcm.Watchdog()):
		r.t.Errorf("the engine's Run did not return within %v", alsapcm.Watchdog())
		return
	}
	ctx, cancel := context.WithTimeout(bg, alsapcm.Watchdog())
	defer cancel()
	if err := r.be.Wait(ctx); err != nil {
		r.t.Errorf("the backend's goroutines: %v", err)
	}
	if st := r.be.Stats(); st.Overlaps != 0 || st.Goroutines != 0 {
		r.t.Errorf("backend stats %+v: want no overlap and every goroutine joined", st)
	}
	r.files.allClosed(r.t)
	r.k.CheckClosedEach(r.t)
}

// available reports the output available, as the adapter does.
func (r *rig) available() {
	r.t.Helper()
	r.outGen++
	if err := r.eng.ReportOutput(bg, engine.OutputReport{Generation: r.outGen, Available: true}); err != nil {
		r.t.Fatal(err)
	}
}

// insert queues refs in order and returns their IDs.
func (r *rig) insert(refs ...string) []uint32 {
	r.t.Helper()
	var ids []uint32
	after := uint32(0)
	if s := r.eng.Snapshot(); s.Len() > 0 {
		after, _ = s.IDAt(s.Len() - 1)
	}
	for _, ref := range refs {
		rf, err := engine.NewReference(ref)
		if err != nil {
			r.t.Fatal(err)
		}
		id, err := r.eng.InsertAfter(bg, after, rf, nil)
		if err != nil {
			r.t.Fatalf("insert %s: %v", ref, err)
		}
		ids = append(ids, id)
		after = id
	}
	return ids
}

// play plays and fails the test unless the engine took it.
func (r *rig) play() {
	r.t.Helper()
	if err := r.eng.Play(bg); err != nil {
		r.t.Fatalf("play: %v", err)
	}
}

// waitSnap waits until the engine's snapshot satisfies cond.
func (r *rig) waitSnap(what string, cond func(*engine.Snapshot) bool) *engine.Snapshot {
	r.t.Helper()
	waitOn(r.t, what, r.eng.Changed, func() bool { return cond(r.eng.Snapshot()) })
	return r.eng.Snapshot()
}

// waitStopped waits for playback to stop for why.
func (r *rig) waitStopped(why engine.Reason) *engine.Snapshot {
	r.t.Helper()
	s := r.waitSnap("playback to stop: "+string(why), func(s *engine.Snapshot) bool {
		return s.State() == engine.Stopped && s.LastStop().Reason != ""
	})
	if got := s.LastStop(); got.Reason != why {
		r.t.Fatalf("playback stopped for %s (%s); want %s; events %v", got.Reason, got.Detail, why, r.rec.events())
	}
	return s
}

// waitEvents waits until the events reported satisfy cond.
func (r *rig) waitEvents(what string, cond func([]event) bool) []event {
	r.t.Helper()
	waitOn(r.t, what, r.rec.ch, func() bool { return cond(r.rec.events()) })
	return r.rec.events()
}

// waitCall waits until the engine has made a call that satisfies cond.
func (r *rig) waitCall(what string, cond func(call) bool) call {
	r.t.Helper()
	var got call
	waitOn(r.t, what, r.rec.ch, func() bool {
		for _, c := range r.rec.callList() {
			if cond(c) {
				got = c
				return true
			}
		}
		return false
	})
	return got
}

// tick has every session's reporter measure once: the send returns when a
// reporter took it, and a second send when it has done what the first
// asked (the barrier).
func (r *rig) tick() {
	r.t.Helper()
	for range 2 {
		select {
		case r.ticks <- time.Time{}:
		case <-time.After(alsapcm.Watchdog()):
			r.t.Fatalf("no reporter took a tick within %v", alsapcm.Watchdog())
		}
	}
}

// runningAndWaiting is the stream started and its writer waiting on the DAC
// for time none was granted: a grant from here plays content. A writer that
// waits on a stream not yet started is starved too, and a grant then plays
// nothing.
func runningAndWaiting(v alsapcm.KernelView) bool { return v.State == "RUNNING" && v.Starved }

// count is the events of kind for occurrence o (0: any).
func count(evs []event, kind string, o engine.OccurrenceID) int {
	n := 0
	for _, e := range evs {
		if e.kind == kind && (o == 0 || e.o == o) {
			n++
		}
	}
	return n
}

// kinds is the events as strings, for a failure.
func kinds(evs []event) string {
	var b strings.Builder
	for i, e := range evs {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(e.String())
	}
	return b.String()
}

// golden is one of PR 1's fixtures: its file, its PCM and the hash of its
// samples, which reference tools made (decode's testdata).
type golden struct {
	file           string
	rate, bits     int
	frames         int64
	sha            string
	ref            string
	refusal        string
	data           []byte
	samplesFromSHA bool
}

// unpack reads b back into samples from format f carrying bits valid bits,
// as the DAC reads them, and fails the test on a byte the packing must have
// left zero.
func unpack(t *testing.T, b []byte, f format.ALSAFormat, bits int) []int32 {
	t.Helper()
	var out []int32
	switch f {
	case format.S16LE:
		for i := 0; i+2 <= len(b); i += 2 {
			out = append(out, int32(int16(binary.LittleEndian.Uint16(b[i:]))))
		}
	case format.S243LE:
		for i := 0; i+3 <= len(b); i += 3 {
			v := int32(b[i]) | int32(b[i+1])<<8 | int32(b[i+2])<<16
			out = append(out, v<<8>>8)
		}
	case format.S24LE:
		for i := 0; i+4 <= len(b); i += 4 {
			if b[i+3] != 0 {
				t.Fatalf("S24_LE sample %d has a high byte %#x: the container's top byte is zero", i/4, b[i+3])
			}
			v := int32(binary.LittleEndian.Uint32(b[i:]))
			out = append(out, v<<8>>8)
		}
	case format.S32LE:
		for i := 0; i+4 <= len(b); i += 4 {
			v := int32(binary.LittleEndian.Uint32(b[i:]))
			if bits == 24 {
				if b[i] != 0 {
					t.Fatalf("S32_LE sample %d carrying 24 bits has a low byte %#x, not zero", i/4, b[i])
				}
				v >>= 8
			}
			out = append(out, v)
		}
	default:
		t.Fatalf("no unpacking for %s", f)
	}
	return out
}

// sumOf is the samples' hash as the goldens' manifests give it: SHA-256 of
// each sample as a little-endian int32.
func sumOf(samples []int32) string {
	h := sha256.New()
	var le [4]byte
	for _, s := range samples {
		binary.LittleEndian.PutUint32(le[:], uint32(s))
		h.Write(le[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// wav is a canonical integer PCM WAV of stereo samples at rate.
func wav(rate, bits int, samples []int32) []byte {
	return wavFormat(1, rate, bits, len(samples)/2, samples)
}

// wavFormat is a WAV of format tag tag (1 integer PCM, 3 IEEE float) whose
// data chunk declares frames frames and holds samples.
func wavFormat(tag uint16, rate, bits, frames int, samples []int32) []byte {
	bps := bits / 8
	data := make([]byte, 0, len(samples)*bps)
	for _, s := range samples {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(s))
		data = append(data, b[:bps]...)
	}
	declared := frames * 2 * bps
	var w bytes.Buffer
	w.WriteString("RIFF")
	_ = binary.Write(&w, binary.LittleEndian, uint32(36+declared))
	w.WriteString("WAVEfmt ")
	_ = binary.Write(&w, binary.LittleEndian, uint32(16))
	_ = binary.Write(&w, binary.LittleEndian, tag)
	_ = binary.Write(&w, binary.LittleEndian, uint16(2))
	_ = binary.Write(&w, binary.LittleEndian, uint32(rate))
	_ = binary.Write(&w, binary.LittleEndian, uint32(rate*2*bps))
	_ = binary.Write(&w, binary.LittleEndian, uint16(2*bps))
	_ = binary.Write(&w, binary.LittleEndian, uint16(bits))
	w.WriteString("data")
	_ = binary.Write(&w, binary.LittleEndian, uint32(declared))
	w.Write(data)
	return w.Bytes()
}

// pattern is frames stereo frames of bits-bit samples, none zero and none
// shared with another track's, so a frame of one track in another's place,
// a frame twice, or silence where music belongs shows.
func pattern(track, frames, bits int) []int32 {
	limit := int64(1)<<(bits-1) - 1
	out := make([]int32, 0, frames*2)
	for f := range frames {
		for c := range 2 {
			v := (int64(f)*7919+int64(track)*104729+int64(c)*31)%(limit-1) + 1
			if (f+c+track)%2 == 1 {
				v = -v
			}
			out = append(out, int32(v))
		}
	}
	return out
}
