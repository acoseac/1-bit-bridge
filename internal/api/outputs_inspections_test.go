package api

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/mpd"
)

// Backlog B650's third round: bridge doctor makes an outputs builder for each
// gather, and the MPD inspection each one's probe made, which a file on a dead
// mount holds in the kernel (mpd.Inspect's reads of mpd.conf and the player's
// files end with no context), was guarded by its own builder alone, so every
// gather began another and stranded its goroutine. Builders given one
// MPDInspections share one inspection in flight for what it reads.

// inspectionDeadline is how long a deadline guard gives what a case waits
// for: a run that never comes back fails the case by it, never the suite.
const inspectionDeadline = 60 * time.Second

// lockedClock is manualClock for builders that run on goroutines of their
// own: its tasks are kept under a lock, a task is run outside it, and set
// hears the duration of each task it is given.
type lockedClock struct {
	mu    sync.Mutex
	now   time.Time
	tasks []*manualTask
	set   chan time.Duration
}

func newLockedClock() *lockedClock {
	return &lockedClock{now: time.Unix(1_800_000_000, 0), set: make(chan time.Duration, 64)}
}

func (c *lockedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *lockedClock) AfterFunc(d time.Duration, fn func()) func() {
	c.mu.Lock()
	task := &manualTask{deadline: c.now.Add(d), fn: fn}
	c.tasks = append(c.tasks, task)
	c.mu.Unlock()
	select {
	case c.set <- d:
	default:
	}
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		task.fn = nil
	}
}

func (c *lockedClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []func()
	for _, task := range c.tasks {
		if task.fn == nil || task.deadline.After(c.now) {
			continue
		}
		due = append(due, task.fn)
		task.fn = nil
	}
	c.mu.Unlock()
	for _, fn := range due {
		fn()
	}
}

// heldInspection stands in for mpd.Inspect on a box whose mpd.conf is on a
// mount that does not answer: each call counts as an inspection begun, says
// so on started, and does not return until release, a test's own (the
// cleanup lets it go); once let go, every call answers at once, a
// configuration read.
type heldInspection struct {
	begun   atomic.Int32
	started chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	ctxs    []context.Context
}

func newHeldInspection(t *testing.T) *heldInspection {
	t.Helper()
	h := &heldInspection{started: make(chan struct{}, 64), release: make(chan struct{})}
	t.Cleanup(h.letGo)
	return h
}

func (h *heldInspection) inspect(ctx context.Context, _ mpd.Options) mpd.Report {
	h.begun.Add(1)
	h.mu.Lock()
	h.ctxs = append(h.ctxs, ctx)
	h.mu.Unlock()
	select {
	case h.started <- struct{}{}:
	default:
	}
	<-h.release
	return mpd.Report{ConfPath: "/etc/mpd.conf"}
}

func (h *heldInspection) letGo() { h.once.Do(func() { close(h.release) }) }

// ctxOf is the context the n-th inspection begun (from 0) was given.
func (h *heldInspection) ctxOf(n int) context.Context {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ctxs[n]
}

// sharedBuilder is a builder of a gather on a Linux box whose output.mpd is
// mode, inspecting through insp, sharing table, on clock.
func sharedBuilder(mode string, clock *lockedClock, table *MPDInspections, insp func(context.Context, mpd.Options) mpd.Report) *localOutputs {
	cfg := quietOutputs("linux", mode)
	cfg.Now, cfg.AfterFunc = clock.Now, clock.AfterFunc
	cfg.Inspect = insp
	cfg.Inspections = table
	return newLocalOutputs(cfg)
}

// withinInspection is what fn returns, from a goroutine of its own, the case
// failing by inspectionDeadline where it does not return.
func withinInspection[T any](t *testing.T, what string, fn func() T) T {
	t.Helper()
	done := make(chan T, 1)
	go func() { done <- fn() }()
	select {
	case v := <-done:
		return v
	case <-time.After(inspectionDeadline):
		t.Fatalf("%s did not return within %s", what, inspectionDeadline)
		panic("unreachable")
	}
}

func receive[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(inspectionDeadline):
		t.Fatalf("%s did not come within %s", what, inspectionDeadline)
		panic("unreachable")
	}
}

// drainedInspections waits until table holds no inspection in flight.
func drainedInspections(t *testing.T, table *MPDInspections) {
	t.Helper()
	for deadline := time.Now().Add(inspectionDeadline); table.InFlightForTest() > 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("an inspection was still in flight %s after it was let go", inspectionDeadline)
		}
	}
}

// TestBuildersSharingATableBeginOneInspection is the third round's case on
// builders, as bridge doctor's reconstructed gathers make them, on a box
// whose mpd.conf does not answer: the first gather begins one inspection
// and its probe stops waiting once that inspection's deadline and the grace
// have passed on the clock, with no verdict; the gathers after it, while the
// inspection is held, begin none and answer at once, each with no verdict;
// once the inspection returns its key is let go, and the next gather begins
// a fresh one and is answered by it. Red at 0b1a26f0, where each builder's
// own guard began an inspection for every gather.
func TestBuildersSharingATableBeginOneInspection(t *testing.T) {
	clock, table, insp := newLockedClock(), &MPDInspections{}, newHeldInspection(t)
	first := sharedBuilder("auto", clock, table, insp.inspect)
	answered := make(chan bool, 1)
	go func() { answered <- first.Active(context.Background()) }()
	receive(t, "the first gather's inspection", insp.started)
	clock.Advance(outputProbeLimit + outputProbeGrace)
	if receive(t, "the first gather's answer", answered) {
		t.Fatal("an inspection that did not answer: the gate said on")
	}
	for i := 2; i <= 3; i++ {
		later := sharedBuilder("auto", clock, table, insp.inspect)
		_, _, _, seq, late := withinInspection(t, "a later gather's probe", func() probeAnswer {
			found, rep, opt, seq, late := later.probeVerdict(context.Background())
			return probeAnswer{found, rep, opt, seq, late}
		}).unpack()
		if seq != 0 || !late {
			t.Errorf("gather %d's probe: verdict number %d, late %v; want none, and late", i, seq, late)
		}
	}
	if n := insp.begun.Load(); n != 1 {
		t.Fatalf("three gathers began %d inspections while one was held, want one", n)
	}
	insp.letGo()
	drainedInspections(t, table)
	last := sharedBuilder("auto", clock, table, insp.inspect)
	if !withinInspection(t, "the gather after the inspection returned", func() bool { return last.Active(context.Background()) }) {
		t.Error("once the inspection returned, the next gather's own inspection read a configuration, and the gate said off")
	}
	if n := insp.begun.Load(); n != 2 {
		t.Errorf("after the inspection returned, %d inspections were begun, want a second", n)
	}
}

type probeAnswer struct {
	found bool
	rep   mpd.Report
	opt   mpd.Options
	seq   uint64
	late  bool
}

func (a probeAnswer) unpack() (bool, mpd.Report, mpd.Options, uint64, bool) {
	return a.found, a.rep, a.opt, a.seq, a.late
}

// TestAProbeWaitingOnASharedInspectionStopsWithItsOwnContext: three gathers
// wait on one inspection, the first having begun it; the first and the second
// stop waiting as their own contexts end, with no verdict, while the
// inspection, on a context of its own, goes on, and the third, still waiting,
// is answered by it once it returns.
func TestAProbeWaitingOnASharedInspectionStopsWithItsOwnContext(t *testing.T) {
	clock, table, insp := newLockedClock(), &MPDInspections{}, newHeldInspection(t)
	waiting := make(chan struct{}, 8)
	table.waiting = func() { waiting <- struct{}{} }
	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	t.Cleanup(func() { cancelA(); cancelB() })
	a, b, c := make(chan bool, 1), make(chan bool, 1), make(chan bool, 1)
	start := func(ctx context.Context, out chan<- bool) {
		o := sharedBuilder("auto", clock, table, insp.inspect)
		go func() { out <- o.Active(ctx) }()
		receive(t, "a gather's wait", waiting)
	}
	start(ctxA, a)
	receive(t, "the inspection", insp.started)
	start(ctxB, b)
	start(context.Background(), c)
	cancelA()
	if receive(t, "the first gather, its context ended", a) {
		t.Error("the first gather, its context ended, was answered on")
	}
	cancelB()
	if receive(t, "the second gather, its context ended", b) {
		t.Error("the second gather, its context ended, was answered on")
	}
	if err := insp.ctxOf(0).Err(); err != nil {
		t.Errorf("the inspection's context ended with the gathers that stopped waiting: %v", err)
	}
	select {
	case got := <-c:
		t.Fatalf("the third gather, its context alive, stopped waiting before the inspection answered (%v)", got)
	default:
	}
	insp.letGo()
	if !receive(t, "the third gather", c) {
		t.Error("the inspection read a configuration, and the gather still waiting on it was answered off")
	}
	if n := insp.begun.Load(); n != 1 {
		t.Errorf("three gathers began %d inspections, want one", n)
	}
}

// TestAnEditedConfigurationBeginsAnInspectionOfItsOwn: a gather over another
// MPD configuration, an inspection of other reads, begins its own and is
// answered by it while the first configuration's is held, and a gather over
// the first configuration again waits on the one held.
func TestAnEditedConfigurationBeginsAnInspectionOfItsOwn(t *testing.T) {
	const held, edited = "/mnt/nas/mpd.conf", "/etc/mpd.conf"
	clock, table, insp := newLockedClock(), &MPDInspections{}, newHeldInspection(t)
	var other atomic.Int32
	inspect := func(ctx context.Context, opt mpd.Options) mpd.Report {
		if opt.Conf == edited {
			other.Add(1)
			return mpd.Report{ConfPath: edited}
		}
		return insp.inspect(ctx, opt)
	}
	builder := func(conf string) *localOutputs {
		o := sharedBuilder("auto", clock, table, inspect)
		o.output = func() config.OutputConfig { return config.OutputConfig{MPD: "auto", Conf: conf} }
		return o
	}
	first := make(chan bool, 1)
	go func() { first <- builder(held).Active(context.Background()) }()
	receive(t, "the first configuration's inspection", insp.started)
	if !withinInspection(t, "a gather over the edited configuration", func() bool { return builder(edited).Active(context.Background()) }) {
		t.Error("the edited configuration's own inspection read it, and the gate said off")
	}
	if other.Load() != 1 {
		t.Errorf("the edited configuration began %d inspections of its own, want one", other.Load())
	}
	again := make(chan bool, 1)
	go func() { again <- builder(held).Active(context.Background()) }()
	clock.Advance(outputProbeLimit + outputProbeGrace)
	receive(t, "the first gather", first)
	receive(t, "the gather over the first configuration again", again)
	if n := insp.begun.Load(); n != 1 {
		t.Errorf("the held configuration was inspected %d times, want once", n)
	}
}

// TestASharedInspectionThatPanicsLetsItsKeyGo: an inspection that panics is
// no verdict, logged, and lets its key go, so the next gather begins afresh.
func TestASharedInspectionThatPanicsLetsItsKeyGo(t *testing.T) {
	clock, table := newLockedClock(), &MPDInspections{}
	var calls atomic.Int32
	inspect := func(context.Context, mpd.Options) mpd.Report {
		if calls.Add(1) == 1 {
			panic("an inspection that panics")
		}
		return mpd.Report{ConfPath: "/etc/mpd.conf"}
	}
	first := sharedBuilder("auto", clock, table, inspect)
	if _, _, _, seq, late := withinInspection(t, "the gather whose inspection panics", func() probeAnswer {
		found, rep, opt, seq, late := first.probeVerdict(context.Background())
		return probeAnswer{found, rep, opt, seq, late}
	}).unpack(); seq != 0 || !late {
		t.Errorf("an inspection that panicked: verdict number %d, late %v; want none, and late", seq, late)
	}
	drainedInspections(t, table)
	if !withinInspection(t, "the next gather", func() bool {
		return sharedBuilder("auto", clock, table, inspect).Active(context.Background())
	}) {
		t.Error("the next gather's inspection read a configuration, and the gate said off")
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("%d inspections, want the one that panicked and a fresh one", n)
	}
}

// TestTheBuildsProbeSharesTheTableToo: output.mpd on asks the gate nothing,
// and each gather's build probes MPD (build, probe): those builds share the
// table's one inspection as the gate's probes do.
func TestTheBuildsProbeSharesTheTableToo(t *testing.T) {
	clock, table, insp := newLockedClock(), &MPDInspections{}, newHeldInspection(t)
	first := sharedBuilder("on", clock, table, insp.inspect)
	built := make(chan error, 1)
	go func() {
		_, err := first.Response(context.Background())
		built <- err
	}()
	receive(t, "the first build's inspection", insp.started)
	clock.Advance(outputProbeLimit + outputProbeGrace)
	if err := receive(t, "the first build", built); err != nil {
		t.Fatalf("the first build: %v", err)
	}
	for i := 2; i <= 3; i++ {
		later := sharedBuilder("on", clock, table, insp.inspect)
		if err := withinInspection(t, "a later gather's build", func() error {
			_, err := later.Response(context.Background())
			return err
		}); err != nil {
			t.Fatalf("gather %d's build: %v", i, err)
		}
	}
	if n := insp.begun.Load(); n != 1 {
		t.Errorf("three builds began %d inspections while one was held, want one", n)
	}
}
