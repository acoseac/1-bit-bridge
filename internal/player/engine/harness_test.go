package engine

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
)

// watchdog is how long a test waits for an event before it calls the wait
// a hang. Nothing waits on it to let the code run: every wait is for a
// signal the engine or a fake publishes.
var watchdog = flag.Duration("watchdog", time.Minute, "how long a test waits for an event before it fails as hung")

// logRecorder keeps every record an engine logs.
type logRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *logRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *logRecorder) Handle(_ context.Context, rec slog.Record) error {
	var b strings.Builder
	b.WriteString(rec.Level.String() + " " + rec.Message)
	rec.Attrs(func(a slog.Attr) bool {
		b.WriteString(" " + a.Key + "=" + a.Value.String())
		return true
	})
	r.mu.Lock()
	r.lines = append(r.lines, b.String())
	r.mu.Unlock()
	return nil
}

func (r *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *logRecorder) WithGroup(string) slog.Handler      { return r }

// count is the number of lines that contain s.
func (r *logRecorder) count(s string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, l := range r.lines {
		if strings.Contains(l, s) {
			n++
		}
	}
	return n
}

// all is every line so far.
func (r *logRecorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

// reserveRecord is what one reservation write reserved, read from its
// bytes as the file format says.
type reserveRecord struct {
	Version int    `json:"version"`
	IDs     uint32 `json:"ids"`
	Tokens  uint32 `json:"tokens"`
}

// fakeFiles is the engine's file operations: the real ones, in the test's
// directory, with the faults a test asks for.
type fakeFiles struct {
	mu              sync.Mutex
	queueAttempts   int
	queueWrites     [][]byte
	dropQueue       bool
	failQueue       func(attempt int) error
	holdQueue       map[int]gate
	reserves        []reserveRecord
	reserveCalls    int // writes begun, counted before a hold
	reserveAttempts int
	failReserve     error
	panicReserve    any // the next reservation write panics with it, once
	holdReserve     gate
	renameErr       error
	readErr         map[string]error
	changed         chan struct{}
	cleanup         chan struct{}
}

func newFakeFiles() *fakeFiles {
	return &fakeFiles{holdQueue: map[int]gate{}, readErr: map[string]error{}, changed: make(chan struct{}), cleanup: make(chan struct{})}
}

func (f *fakeFiles) signal() {
	close(f.changed)
	f.changed = make(chan struct{})
}

func (f *fakeFiles) writeQueue(path string, data []byte) error {
	f.mu.Lock()
	f.queueAttempts++
	n := f.queueAttempts
	hold := f.holdQueue[n]
	f.signal()
	f.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-f.cleanup:
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	defer f.signal()
	if f.dropQueue {
		return nil
	}
	if f.failQueue != nil {
		if err := f.failQueue(n); err != nil {
			return err
		}
	}
	if err := atomicwrite.WriteBytes(path, data, ".player-queue-*.tmp"); err != nil {
		return err
	}
	f.queueWrites = append(f.queueWrites, append([]byte(nil), data...))
	return nil
}

func (f *fakeFiles) writeReserve(path string, data []byte) error {
	f.mu.Lock()
	f.reserveCalls++
	hold := f.holdReserve
	f.signal()
	f.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-f.cleanup:
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	defer f.signal()
	f.reserveAttempts++
	if p := f.panicReserve; p != nil {
		f.panicReserve = nil
		panic(p)
	}
	if f.failReserve != nil {
		return f.failReserve
	}
	var rec reserveRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return fmt.Errorf("the test cannot read a reservation write: %w", err)
	}
	// The fake writes as the queue file is written, on every platform: the
	// strict writer refuses one with no directory barrier (Windows, where
	// this suite runs too). The reservation's own durability is pinned
	// through the real writer with the barrier replaced
	// (TestAReservationIsMadeOnlyWithItsDirectoryEntry).
	if err := atomicwrite.WriteBytes(path, data, ".player-ids-*.tmp"); err != nil {
		return err
	}
	f.reserves = append(f.reserves, rec)
	return nil
}

// probe is New's barrier probe: it passes, as on a filesystem that can sync
// a directory, on every platform. The real one, over a barrier the test
// replaces, is TestNewRefusesADirectoryThatCannotBeSynced's.
func (f *fakeFiles) probe(string) error { return nil }

func (f *fakeFiles) readFile(path string, limit int64) ([]byte, error) {
	f.mu.Lock()
	err := f.readErr[filepath.Base(path)]
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return readLimited(path, limit)
}

func (f *fakeFiles) rename(from, to string) error {
	f.mu.Lock()
	err := f.renameErr
	f.mu.Unlock()
	if err != nil {
		return err
	}
	return os.Rename(from, to)
}

// releaseHolds lets every held write go, so a test that fails while one is
// held does not hold its engine's shutdown.
func (f *fakeFiles) releaseHolds() {
	f.mu.Lock()
	defer f.mu.Unlock()
	gates := []gate{f.holdReserve}
	for _, g := range f.holdQueue {
		gates = append(gates, g)
	}
	for _, g := range gates {
		if g == nil {
			continue
		}
		select {
		case <-g:
		default:
			g.open()
		}
	}
}

// writes is the number of queue writes that landed.
func (f *fakeFiles) writes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queueWrites)
}

// attempts is the number of queue writes begun.
func (f *fakeFiles) attempts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.queueAttempts
}

// durable is the highest ID and token reservation written so far.
func (f *fakeFiles) durable() (ids, tokens uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.reserves {
		ids, tokens = max(ids, r.IDs), max(tokens, r.Tokens)
	}
	return ids, tokens
}

// waitFor waits until cond, called with f.mu held, holds.
func (f *fakeFiles) waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	timer := time.NewTimer(*watchdog)
	defer timer.Stop()
	for {
		f.mu.Lock()
		ok, ch := cond(), f.changed
		f.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-ch:
		case <-timer.C:
			t.Fatalf("waited %v for the files: %s", *watchdog, what)
		}
	}
}

// harness is one engine over a fake backend, a fake clock and the test's
// directory. Its cleanup shuts the engine down, unblocks every call the
// test held, and requires every goroutine the engine started to have
// ended.
type harness struct {
	t      *testing.T
	dir    string
	clk    *fakeClock
	files  *fakeFiles
	logs   *logRecorder
	tweak  func(*Config)
	be     *fakeBackend
	eng    *Engine
	cancel context.CancelFunc
	ran    chan struct{}
	down   bool
	outGen uint64
	// joining, when set, runs as joined begins, before anything waits for the
	// engine's goroutines: a test that holds one of them until the harness
	// waits for it lets it go there (TestTheContractCheckWaitsForAStartStillOnItsWay).
	joining func()

	appliedMu  sync.Mutex
	appliedOps []string
	appliedCh  chan struct{}
}

// newHarness is a harness that has not built its engine yet: set its
// fields, then boot it.
func newHarness(t *testing.T) *harness {
	h := &harness{t: t, dir: t.TempDir(), clk: newFakeClock(), files: newFakeFiles(), logs: &logRecorder{}, appliedCh: make(chan struct{})}
	t.Cleanup(func() { close(h.files.cleanup) })
	return h
}

// startHarness is a harness booted at once.
func startHarness(t *testing.T) *harness { return newHarness(t).boot() }

// queuePath is the queue file.
func (h *harness) queuePath() string { return filepath.Join(h.dir, "queue.json") }

// reservePath is the reservation file.
func (h *harness) reservePath() string { return h.queuePath() + ".ids" }

// config is the engine's Config over the harness's fakes.
func (h *harness) config() Config {
	cfg := Config{
		PlayerUUID: "0f8fad5b-d9cb-469f-a165-70867728950e",
		Backend:    h.be,
		QueuePath:  h.queuePath(),
		DataDir:    h.dir,
		Log:        slog.New(h.logs),
		seams: seams{
			clock:        h.clk,
			writeQueue:   h.files.writeQueue,
			writeReserve: h.files.writeReserve,
			probe:        h.files.probe,
			readFile:     h.files.readFile,
			rename:       h.files.rename,
			admitted:     func(c *command) { h.noteApplied("admitted " + c.op.String()) },
			applied:      h.noteApplied,
			handled:      func(what string) { h.noteApplied("handled " + what) },
		},
	}
	if h.tweak != nil {
		h.tweak(&cfg)
	}
	return cfg
}

// build builds the engine without running it.
func (h *harness) build() *harness {
	h.t.Helper()
	h.be = newFakeBackend(h.t)
	be := h.be
	e, err := New(h.config())
	if err != nil {
		h.t.Fatalf("New: %v", err)
	}
	// Cleanups run last-registered first: Run's shutdown (registered by
	// run), then the backend's release of what it held, then the check
	// that every goroutine the engine started has ended, and only then the
	// backend's contract check, registered as the backend was made, which
	// must not read the calls while one of them is still on its way (B576).
	h.t.Cleanup(func() { h.joined(e) })
	h.t.Cleanup(be.close)
	h.eng = e
	h.down = false
	return h
}

// boot builds the engine and runs it.
func (h *harness) boot() *harness {
	h.t.Helper()
	h.build()
	h.run()
	return h
}

// run runs the built engine until shutdown.
func (h *harness) run() {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel, h.ran = cancel, make(chan struct{})
	e, ran := h.eng, h.ran
	go func() {
		defer close(ran)
		if err := e.Run(ctx); err != nil {
			h.t.Errorf("Run: %v", err)
		}
	}()
	h.t.Cleanup(func() { h.shutdownEngine(e, cancel, ran) })
	h.t.Cleanup(h.files.releaseHolds)
}

// shutdown cancels Run and fires the clock's timers until it returns.
func (h *harness) shutdown() {
	h.t.Helper()
	h.shutdownEngine(h.eng, h.cancel, h.ran)
}

func (h *harness) shutdownEngine(e *Engine, cancel context.CancelFunc, ran chan struct{}) {
	h.t.Helper()
	if e == h.eng {
		if h.down {
			return
		}
		h.down = true
	}
	cancel()
	h.drive(ran)
}

// drive fires the clock's timers, earliest first, until done is closed.
func (h *harness) drive(done <-chan struct{}) {
	h.t.Helper()
	timer := time.NewTimer(*watchdog)
	defer timer.Stop()
	for {
		armed := h.clk.Armed()
		select {
		case <-done:
			return
		default:
		}
		if h.clk.Pending() > 0 {
			h.clk.advanceToNext()
			continue
		}
		select {
		case <-done:
			return
		case <-armed:
		case <-timer.C:
			h.t.Fatalf("waited %v for the engine to shut down", *watchdog)
		}
	}
}

// joined waits for every goroutine the engine started to end.
func (h *harness) joined(e *Engine) {
	h.t.Helper()
	if h.joining != nil {
		h.joining()
	}
	done := make(chan struct{})
	go func() {
		e.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(*watchdog):
		h.t.Fatalf("waited %v for the engine's goroutines: %d still running", *watchdog, e.live.Load())
	}
}

// restart shuts the engine down and boots another on the same files.
func (h *harness) restart() *Engine {
	h.t.Helper()
	old := h.eng
	h.shutdown()
	h.joinAfter(old)
	h.boot()
	return old
}

// joinAfter unblocks what the old engine's backend held and waits for the
// old engine's goroutines.
func (h *harness) joinAfter(old *Engine) {
	h.t.Helper()
	if fb, ok := old.be.(*fakeBackend); ok {
		fb.close()
	}
	h.joined(old)
}

// crash stops the engine with every queue write from now on lost, as a
// crash before them would, and boots another on the same files.
func (h *harness) crash() {
	h.t.Helper()
	h.files.mu.Lock()
	h.files.dropQueue = true
	h.files.mu.Unlock()
	old := h.eng
	h.shutdown()
	h.joinAfter(old)
	h.files.mu.Lock()
	h.files.dropQueue = false
	h.files.mu.Unlock()
	h.boot()
}

// hangReservations restarts the engine with its start's reservation
// failing, so the next edit that needs a value waits for a write, and holds
// that write until the returned gate opens; calls is the number of writes
// begun before it.
func (h *harness) hangReservations() (g gate, calls int) {
	h.t.Helper()
	h.restartWith(func() { h.files.failReserve = errors.New("read-only file system") })
	g = newGate()
	h.files.mu.Lock()
	defer h.files.mu.Unlock()
	h.files.failReserve, h.files.holdReserve = nil, g
	return g, h.files.reserveCalls
}

// noteApplied records a command the owner has applied, a message it has
// handled ("handled " and its name) or a command it is admitting
// ("admitted " and its op), as the owner reaches it.
func (h *harness) noteApplied(op string) {
	h.appliedMu.Lock()
	defer h.appliedMu.Unlock()
	h.appliedOps = append(h.appliedOps, op)
	close(h.appliedCh)
	h.appliedCh = make(chan struct{})
}

// appliedMark is the number of commands applied so far, for waitApplied.
func (h *harness) appliedMark() int {
	h.appliedMu.Lock()
	defer h.appliedMu.Unlock()
	return len(h.appliedOps)
}

// waitApplied waits until the owner has applied a command of op after the
// first since.
func (h *harness) waitApplied(op string, since int) {
	h.t.Helper()
	h.waitAppliedTimes(op, since, 1)
}

// waitAppliedTimes waits until the owner has applied op, or handled it, n
// times after the first since.
func (h *harness) waitAppliedTimes(op string, since, n int) {
	h.t.Helper()
	timer := time.NewTimer(*watchdog)
	defer timer.Stop()
	for {
		h.appliedMu.Lock()
		ch, count := h.appliedCh, 0
		for _, a := range h.appliedOps[min(since, len(h.appliedOps)):] {
			if a == op {
				count++
			}
		}
		h.appliedMu.Unlock()
		if count >= n {
			return
		}
		select {
		case <-ch:
		case <-timer.C:
			h.t.Fatalf("waited %v for the owner to apply %s %d times", *watchdog, op, n)
		}
	}
}

// snap is the engine's snapshot.
func (h *harness) snap() *Snapshot { return h.eng.Snapshot() }

// waitSnap waits until cond holds for the engine's snapshot and returns it.
func (h *harness) waitSnap(what string, cond func(*Snapshot) bool) *Snapshot {
	h.t.Helper()
	timer := time.NewTimer(*watchdog)
	defer timer.Stop()
	for {
		ch := h.eng.Changed()
		if s := h.eng.Snapshot(); cond(s) {
			return s
		}
		select {
		case <-ch:
		case <-timer.C:
			s := h.eng.Snapshot()
			h.t.Fatalf("waited %v for %s; state %s, selected %d, pending %s, last stop %+v, calls %v",
				*watchdog, what, s.State(), s.Selected(), s.Pending(), s.LastStop(), h.be.callList())
		}
	}
}

// ref is s as a Reference.
func ref(s string) Reference {
	r, err := NewReference(s)
	if err != nil {
		panic(err)
	}
	return r
}

var bg = context.Background()

// insert appends entries for refs, in order, and returns their IDs.
func (h *harness) insert(refs ...string) []uint32 {
	h.t.Helper()
	var ids []uint32
	for _, r := range refs {
		last := uint32(0)
		if all := h.snap().IDs(); len(all) > 0 {
			last = all[len(all)-1]
		}
		id, err := h.eng.InsertAfter(bg, last, ref(r), []byte("meta:"+r))
		if err != nil {
			h.t.Fatalf("insert %s: %v", r, err)
		}
		ids = append(ids, id)
	}
	return ids
}

// report reports the output, numbering the report after the last.
func (h *harness) report(r OutputReport) {
	h.t.Helper()
	if r.Generation == 0 {
		h.outGen++
		r.Generation = h.outGen
	}
	if err := h.eng.ReportOutput(bg, r); err != nil {
		h.t.Fatalf("report output: %v", err)
	}
}

// available reports the output available.
func (h *harness) available() { h.report(OutputReport{Available: true}) }

// async runs f on a goroutine and returns where its error goes.
func async(f func() error) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- f() }()
	return ch
}

// result waits for an async call's error.
func (h *harness) result(what string, ch <-chan error) error {
	h.t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(*watchdog):
		h.t.Fatalf("waited %v for %s to answer", *watchdog, what)
	}
	return nil
}

// must fails the test on err.
func (h *harness) must(what string, err error) {
	h.t.Helper()
	if err != nil {
		h.t.Fatalf("%s: %v", what, err)
	}
}

// refused requires err to be a refusal for reason.
func (h *harness) refused(what string, err error, reason Reason) {
	h.t.Helper()
	if got := ReasonOf(err); got != reason {
		h.t.Fatalf("%s: got %v (reason %q), want reason %q", what, err, got, reason)
	}
}

// playing inserts refs, reports the output available, plays the entry at
// index i, reports its first occurrence audible and returns the IDs and
// the session.
func (h *harness) playing(i int, refs ...string) ([]uint32, *fakeSession) {
	h.t.Helper()
	ids := h.insert(refs...)
	h.available()
	n := h.be.starts()
	h.must("play", h.eng.PlayID(bg, ids[i]))
	s := h.be.nth(n + 1)
	s.audible(s.first.ID)
	h.waitSnap("the first occurrence audible", func(sn *Snapshot) bool {
		a, ok := sn.Audible()
		return ok && a.Occurrence == s.first.ID
	})
	return ids, s
}

// advanceToNext fires the earliest pending timer, moving the clock to it.
func (c *fakeClock) advanceToNext() {
	c.mu.Lock()
	if len(c.timers) == 0 {
		c.mu.Unlock()
		return
	}
	at := c.timers[0].at
	for _, t := range c.timers[1:] {
		if t.at.Before(at) {
			at = t.at
		}
	}
	d := at.Sub(c.now)
	c.mu.Unlock()
	c.Advance(max(d, 0))
}
