package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// Backlog B650: a library root whose mount does not answer held every doctor
// run on a stat of it in the kernel, with no end on a hard NFS mount, and the
// run said nothing at all. The probes of the roots, and every other work on a
// root, now run as works of the runner's RootProbes, each waited for until the
// bound from when it began. No filesystem a test can make stops answering, so
// a root that does not answer is stood in for at the probe's two calls
// (stallingRoots), each a field of the RootProbes the run uses
// (NewRootProbesForTest).

// testBound is how long the tests' runners wait for a work on a root: short,
// so a root that does not answer costs each case little, and long enough that
// a root that answers, a temporary directory, answers within it under -race.
const testBound = 300 * time.Millisecond

// testDeadline is how long a test waits for a run that a root holds, before
// it fails by its own deadline rather than hang the suite: a run that waits on
// the root with no bound, as the checks did until B650, never returns.
const testDeadline = 60 * time.Second

// stallingRoots stands in for the probe's two calls on the roots it holds,
// which do not answer until it lets them go (letGo): a stat of a root in stat,
// a listing of one in list. Every other root is os.Stat's and os.ReadDir's,
// and so is a held one once it is let go. It counts the stats it was asked
// for, by root: how many probes of each root began.
type stallingRoots struct {
	stat, list map[string]bool
	release    chan struct{}
	once       sync.Once
	mu         sync.Mutex
	stats      map[string]int
}

func newStallingRoots(statHeld, listHeld []string) *stallingRoots {
	s := &stallingRoots{stat: map[string]bool{}, list: map[string]bool{}, release: make(chan struct{}), stats: map[string]int{}}
	for _, r := range statHeld {
		s.stat[r] = true
	}
	for _, r := range listHeld {
		s.list[r] = true
	}
	return s
}

func (s *stallingRoots) statRoot(p string) (fs.FileInfo, error) {
	s.mu.Lock()
	s.stats[p]++
	held := s.stat[p]
	s.mu.Unlock()
	if held {
		<-s.release
	}
	return os.Stat(p)
}

func (s *stallingRoots) listRoot(p string) ([]fs.DirEntry, error) {
	if s.list[p] {
		<-s.release
	}
	return os.ReadDir(p)
}

func (s *stallingRoots) letGo() { s.once.Do(func() { close(s.release) }) }

func (s *stallingRoots) statsOf(p string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats[p]
}

// probesOver is a runner's RootProbes over s, waiting bound for a work, and a
// cleanup that lets s's roots answer and waits for every work to end, so none
// outlives the test.
func probesOver(t *testing.T, s *stallingRoots, bound time.Duration) *RootProbes {
	t.Helper()
	p := NewRootProbesForTest(bound, s.statRoot, s.listRoot)
	t.Cleanup(func() {
		s.letGo()
		if !p.DrainForTest(30 * time.Second) {
			t.Error("a work on a root was still out 30s after its roots answered")
		}
	})
	return p
}

// within runs fn on a goroutine of its own and waits limit for it, and
// answers what it returned and how long it took: a test of a root that does
// not answer fails by its own deadline, never by hanging the suite.
func within[T any](t *testing.T, limit time.Duration, what string, fn func() T) (T, time.Duration) {
	t.Helper()
	done := make(chan T, 1)
	start := time.Now()
	go func() { done <- fn() }()
	select {
	case v := <-done:
		return v, time.Since(start)
	case <-time.After(limit):
		t.Fatalf("%s did not return within %s: a root that does not answer held it", what, limit)
		panic("unreachable")
	}
}

// testClock is a runner's clock (RootProbes.now and timer) whose time moves
// only when the test moves it (advance), firing each timer it passes, so a
// case reads a wait's end at the instant the bound passes rather than by how
// long it took: armed hears each timer a wait sets, with what it waits for.
type testClock struct {
	mu     sync.Mutex
	at     time.Time
	timers []*testTimer
	armed  chan time.Duration
}

type testTimer struct {
	due           time.Time
	c             chan time.Time
	fired, halted bool
}

// clockOf makes c the clock p's runs read, before any run begins.
func clockOf(p *RootProbes) *testClock {
	c := &testClock{at: time.Unix(1_800_000_000, 0), armed: make(chan time.Duration, 64)}
	p.now, p.timer = c.now, c.timer
	return c
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) timer(d time.Duration) (<-chan time.Time, func() bool) {
	c.mu.Lock()
	tm := &testTimer{due: c.at.Add(d), c: make(chan time.Time, 1)}
	c.timers = append(c.timers, tm)
	c.mu.Unlock()
	select {
	case c.armed <- d:
	default:
	}
	return tm.c, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		live := !tm.fired && !tm.halted
		tm.halted = true
		return live
	}
}

// advance moves the clock on by d and fires every timer it passes.
func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
	for _, tm := range c.timers {
		if !tm.fired && !tm.halted && !tm.due.After(c.at) {
			tm.fired = true
			tm.c <- c.at
		}
	}
}

// waitArmed is what the next timer a wait sets waits for, the test failing
// by its deadline where none is set.
func (c *testClock) waitArmed(t *testing.T, what string) time.Duration {
	t.Helper()
	select {
	case d := <-c.armed:
		return d
	case <-time.After(testDeadline):
		t.Fatalf("%s set no wait within %s", what, testDeadline)
		panic("unreachable")
	}
}

// settled is the line of a run that sets no further wait on c: the test fails
// at once where it sets one, or by its deadline where it does not return.
func (c *testClock) settled(t *testing.T, what string, got <-chan Check) Check {
	t.Helper()
	select {
	case line := <-got:
		return line
	case d := <-c.armed:
		t.Fatalf("%s set a wait of %s", what, d)
	case <-time.After(testDeadline):
		t.Fatalf("%s did not return within %s", what, testDeadline)
	}
	panic("unreachable")
}

// probesBegun is how many probes of root p's runs began, read once s has let
// every root answer and p's probes have all ended: a probe that has ended has
// made its one stat, which s counts, while one a run on the test clock has
// stopped waiting for may not have made it yet.
func probesBegun(t *testing.T, s *stallingRoots, p *RootProbes, root string) int {
	t.Helper()
	s.letGo()
	if !p.DrainForTest(testDeadline) {
		t.Fatalf("a probe was still out %s after its roots answered", testDeadline)
	}
	return s.statsOf(root)
}

// runOn begins library-roots for roots as one run of p, on a goroutine of its
// own, its line on the channel it returns.
func runOn(ctx context.Context, p *RootProbes, roots ...string) <-chan Check {
	run := p.BeginRun()
	got := make(chan Check, 1)
	go func() { got <- checkLibraryRoots(ctx, Deps{RootProbes: run, LibraryRoots: roots}) }()
	return got
}

// libraryRoot is a directory holding one file: a root that answers, healthy.
func libraryRoot(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "01.flac"), []byte{0}, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// rootsLine is library-roots for roots, as one run of probes sees it.
func rootsLine(t *testing.T, ctx context.Context, p *RootProbes, roots ...string) (Check, time.Duration) {
	t.Helper()
	run := p.BeginRun()
	return within(t, testDeadline, "library-roots", func() Check {
		return checkLibraryRoots(ctx, Deps{RootProbes: run, LibraryRoots: roots})
	})
}

// checkNamed is the check of the report named name.
func checkNamed(t *testing.T, r Report, name string) Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("the report has no %s line: %+v", name, r.Checks)
	return Check{}
}

// TestARunNamesARootThatDoesNotAnswerWithinItsBound is B650's first case
// through the whole of Run: a root whose stat does not answer is named by
// library-roots, as configured, in a fail, once the bound passes on the run's
// clock (its one wait, of the bound), and every other check is still
// reported. Red on main, whose checkLibraryRoots stat'ed the root on the run's
// goroutine: Run set no wait and never returned, and the test failed by its
// own deadline.
func TestARunNamesARootThatDoesNotAnswerWithinItsBound(t *testing.T) {
	dead := libraryRoot(t, "nas")
	s := newStallingRoots([]string{dead}, nil)
	p := probesOver(t, s, testBound)
	clk := clockOf(p)
	dir := t.TempDir()
	deps := Deps{
		RootProbes:   p.BeginRun(),
		ConfigDir:    filepath.Join(dir, "cfg"),
		DataDir:      filepath.Join(dir, "data"),
		LibraryRoots: []string{dead},
		APIPort:      mustFreePort(t),
		AdminPort:    mustFreePort(t),
	}
	reports := make(chan Report, 1)
	go func() { reports <- Run(context.Background(), deps) }()
	if d := clk.waitArmed(t, "Run"); d != testBound {
		t.Errorf("Run's wait on the root = %s, want the bound (%s)", d, testBound)
	}
	clk.advance(testBound)
	var report Report
	select {
	case report = <-reports:
	case d := <-clk.armed:
		t.Fatalf("once the bound passed, Run set another wait of %s", d)
	case <-time.After(testDeadline):
		t.Fatalf("Run did not return within %s of its bound passing: a root that does not answer held it", testDeadline)
	}
	c := checkNamed(t, report, checkNameLibraryRoots)
	want := fmt.Sprintf("library root %s did not answer within %s: its mount may be stalled", dead, testBound)
	if c.Status != Fail || c.Summary != "1 problem(s)" || !strings.Contains(c.Hint, want) {
		t.Errorf("library-roots = %+v, want a fail saying %q", c, want)
	}
	if !strings.Contains(c.Hint, "before removing anything") {
		t.Errorf("library-roots' hint does not say to check the mount before anything is removed: %q", c.Hint)
	}
	requireEveryCheck(t, report)
}

// requireEveryCheck requires report to carry every line Run gives a Deps with
// none of the optional producers, in Run's order.
func requireEveryCheck(t *testing.T, report Report) {
	t.Helper()
	want := runCheckNames()
	if len(report.Checks) != len(want) {
		t.Fatalf("the report has %d lines, want %d: %+v", len(report.Checks), len(want), report.Checks)
	}
	for i, name := range want {
		if report.Checks[i].Name != name {
			t.Errorf("check[%d] = %q, want %q", i, report.Checks[i].Name, name)
		}
	}
}

// runCheckNames is every line Run gives a Deps with none of the optional
// producers, in Run's order. The watch-limit row is named for what it grades:
// where fsnotify watches through kqueue (macOS, the BSDs) the open files its
// watches take, on Linux its inotify budget, and Windows keeps the inotify row
// as not applicable. The list is watchbudget_kqueue.go's build tag.
func runCheckNames() []string {
	watchLimitRow := "inotify-watch-limit"
	switch runtime.GOOS {
	case "darwin", "dragonfly", "freebsd", "netbsd", "openbsd":
		watchLimitRow = "watcher-fd-budget"
	}
	return []string{
		"platform", "config-file", "config-dir", "tls-cert", "tls-cert-sans",
		"pairing-url",
		"port-api", "port-admin",
		"library-roots", "service-manager", "browser-opener",
		watchLimitRow, "audio-toolchain", "flac-32-bit", "dsd-render-toolchain",
		"fingerprint-toolchain", "log-file-size", "sidecar-paths",
		"variants-index", "waveform-regeneration", "name-collisions",
	}
}

// TestTheSecondOfTwoRootsDoesNotAnswer is B650's second case: of two roots,
// the first answers and the second does not. The first is probed and not
// blamed; the second is named. Red on main, which waited on the second root's
// stat with no end.
func TestTheSecondOfTwoRootsDoesNotAnswer(t *testing.T) {
	healthy, dead := libraryRoot(t, "local"), libraryRoot(t, "nas")
	s := newStallingRoots([]string{dead}, nil)
	p := probesOver(t, s, testBound)
	c, _ := rootsLine(t, context.Background(), p, healthy, dead)
	want := fmt.Sprintf("library root %s did not answer within %s", dead, testBound)
	if c.Status != Fail || c.Summary != "1 problem(s)" || !strings.Contains(c.Hint, want) {
		t.Errorf("library-roots = %+v, want a fail naming %s alone", c, dead)
	}
	if strings.Contains(c.Hint, healthy) {
		t.Errorf("library-roots blames the root that answered: %q", c.Hint)
	}
	if n := s.statsOf(healthy); n != 1 {
		t.Errorf("the root that answers was probed %d times, want once", n)
	}
}

// TestRootsThatDoNotAnswerCostOneBoundTogether: the roots are probed together
// (Carbon's fifth note on the brief), so two that do not answer cost a run one
// bound, not one each, and both are named: the run sets one wait, of the
// bound, and returns once that bound passes on its clock, setting no other.
func TestRootsThatDoNotAnswerCostOneBoundTogether(t *testing.T) {
	const bound = 2 * time.Second
	a, b := libraryRoot(t, "nas-a"), libraryRoot(t, "nas-b")
	s := newStallingRoots([]string{a, b}, nil)
	p := probesOver(t, s, bound)
	clk := clockOf(p)
	got := runOn(context.Background(), p, a, b)
	if d := clk.waitArmed(t, "the run"); d != bound {
		t.Errorf("the run's first wait = %s, want the bound (%s)", d, bound)
	}
	clk.advance(bound)
	c := clk.settled(t, "once one bound passed, the run", got)
	for _, r := range []string{a, b} {
		if !strings.Contains(c.Hint, fmt.Sprintf("library root %s did not answer within %s", r, bound)) {
			t.Errorf("library-roots does not name %s: %+v", r, c)
		}
	}
	if c.Summary != "2 problem(s)" {
		t.Errorf("summary = %q, want 2 problem(s)", c.Summary)
	}
}

// TestARootWhoseListingDoesNotAnswerIsNamed: the probe's listing is bounded as
// its stat is (Carbon's fourth note), so a root whose stat answers and whose
// listing does not is named too.
func TestARootWhoseListingDoesNotAnswerIsNamed(t *testing.T) {
	dead := libraryRoot(t, "nas")
	s := newStallingRoots(nil, []string{dead})
	p := probesOver(t, s, testBound)
	c, _ := rootsLine(t, context.Background(), p, dead)
	if c.Status != Fail || !strings.Contains(c.Hint, fmt.Sprintf("library root %s did not answer within %s", dead, testBound)) {
		t.Errorf("library-roots = %+v, want a fail naming the root whose listing does not answer", c)
	}
}

// TestALaterRunFindsTheRootStillNotAnsweringAndAsksItNoMore is B650's third
// case on one runner: a second run while the first run's probe of a root is
// still out says the root still has not answered, and begins no second probe.
func TestALaterRunFindsTheRootStillNotAnsweringAndAsksItNoMore(t *testing.T) {
	dead := libraryRoot(t, "nas")
	s := newStallingRoots([]string{dead}, nil)
	p := probesOver(t, s, testBound)
	clk := clockOf(p)
	got := runOn(context.Background(), p, dead)
	clk.waitArmed(t, "the first run")
	clk.advance(testBound)
	first := clk.settled(t, "the first run", got)
	if !strings.Contains(first.Hint, "did not answer within") {
		t.Fatalf("the first run's library-roots = %+v, want the root not answering", first)
	}
	// The second run, with the probe out past its bound already, sets no wait.
	second := clk.settled(t, "the second run", runOn(context.Background(), p, dead))
	want := fmt.Sprintf("library root %s still has not answered", dead)
	if second.Status != Fail || !strings.Contains(second.Hint, want) {
		t.Errorf("the second run's library-roots = %+v, want a fail saying %q", second, want)
	}
	if n := probesBegun(t, s, p, dead); n != 1 {
		t.Errorf("two runs began %d probes of the root that does not answer, want one", n)
	}
}

// TestOverlappingRunsShareOneProbeOfARoot: the console's runs are not
// serialised, so runs that overlap while a root does not answer share its one
// probe, and each names the root.
func TestOverlappingRunsShareOneProbeOfARoot(t *testing.T) {
	dead := libraryRoot(t, "nas")
	s := newStallingRoots([]string{dead}, nil)
	p := probesOver(t, s, testBound)
	const runs = 8
	lines := make(chan Check, runs)
	var wg sync.WaitGroup
	for range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lines <- checkLibraryRoots(context.Background(), Deps{RootProbes: p.BeginRun(), LibraryRoots: []string{dead}})
		}()
	}
	within(t, testDeadline, "the overlapping runs", func() struct{} { wg.Wait(); return struct{}{} })
	close(lines)
	for c := range lines {
		if c.Status != Fail || !strings.Contains(c.Hint, "library root "+dead) {
			t.Errorf("an overlapping run's library-roots = %+v, want a fail naming the root", c)
		}
	}
	if n := s.statsOf(dead); n != 1 {
		t.Errorf("%d overlapping runs began %d probes of the root, want one", runs, n)
	}
}

// TestARunStoppedByItsCallerSaysSoAndNotThatTheRootIsStuck: a run whose
// context ends before a root's bound (the console's request gone) says the
// root was not checked because the run was stopped, a warn, never that it did
// not answer; the probe stays out, and the next run finds it still not
// answering. The run's clock does not move until it has returned, so it came
// back at its stop, before its bound.
func TestARunStoppedByItsCallerSaysSoAndNotThatTheRootIsStuck(t *testing.T) {
	const bound = 1500 * time.Millisecond
	dead := libraryRoot(t, "nas")
	s := newStallingRoots([]string{dead}, nil)
	p := probesOver(t, s, bound)
	clk := clockOf(p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := runOn(ctx, p, dead)
	clk.waitArmed(t, "the run")
	cancel()
	c := clk.settled(t, "the stopped run", got)
	want := fmt.Sprintf("library root %s was not checked: the doctor's run was stopped before it answered", dead)
	if c.Status != Warn || c.Summary != "1 root(s) not checked" || c.Hint != want {
		t.Errorf("the stopped run's library-roots = %+v, want a warn saying %q", c, want)
	}
	clk.advance(bound)
	next := clk.settled(t, "the next run", runOn(context.Background(), p, dead))
	if next.Status != Fail || !strings.Contains(next.Hint, "still has not answered") {
		t.Errorf("the next run's library-roots = %+v, want the probe still out and not answering", next)
	}
	if n := probesBegun(t, s, p, dead); n != 1 {
		t.Errorf("the runs began %d probes, want one", n)
	}
}

// TestALateAnswerIsKeptAndTheNextRunAsksAfresh: a probe that answers after
// every run stopped waiting for it ends without blocking (its runner holds no
// work once it has), and the next run probes the root afresh, so a mount that
// answers again reads healthy.
func TestALateAnswerIsKeptAndTheNextRunAsksAfresh(t *testing.T) {
	dead := libraryRoot(t, "nas")
	s := newStallingRoots([]string{dead}, nil)
	p := probesOver(t, s, testBound)
	if c, _ := rootsLine(t, context.Background(), p, dead); c.Status != Fail {
		t.Fatalf("library-roots = %+v, want the root not answering", c)
	}
	s.letGo()
	if !p.DrainForTest(10 * time.Second) {
		t.Fatal("the probe that answered late is still out: its answer blocked its worker")
	}
	c, _ := rootsLine(t, context.Background(), p, dead)
	if c.Status != OK || c.Summary != "1 root(s) reachable" {
		t.Errorf("after the mount answers again, library-roots = %+v, want it reachable", c)
	}
	if n := s.statsOf(dead); n != 2 {
		t.Errorf("the root was probed %d times, want twice: once before it answered, once after", n)
	}
}

// TestConcurrentHealthyRunsAllAnswer: runs over roots that answer, side by
// side on one runner, each grade them healthy.
func TestConcurrentHealthyRunsAllAnswer(t *testing.T) {
	a, b := libraryRoot(t, "a"), libraryRoot(t, "b")
	s := newStallingRoots(nil, nil)
	p := probesOver(t, s, testBound)
	const runs = 16
	lines := make(chan Check, runs)
	var wg sync.WaitGroup
	for range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lines <- checkLibraryRoots(context.Background(), Deps{RootProbes: p.BeginRun(), LibraryRoots: []string{a, b}})
		}()
	}
	within(t, testDeadline, "the concurrent runs", func() struct{} { wg.Wait(); return struct{}{} })
	close(lines)
	for c := range lines {
		if c != ok(checkNameLibraryRoots, "2 root(s) reachable") {
			t.Errorf("a concurrent run's library-roots = %+v, want both reachable", c)
		}
	}
}

// TestTheRunnersLimitHoldsNewProbesBackAcrossConfigChurn is Codex's fourth
// note on the brief: the console reloads its config at every run, so a dead
// root replaced by another, then another, would each leave a probe behind. Past
// the runner's limit a run begins no more, and says so of the root it did not
// check, a warn; a root removed and added again finds its probe still out; and
// once the roots answer, a run probes afresh.
func TestTheRunnersLimitHoldsNewProbesBackAcrossConfigChurn(t *testing.T) {
	roots := make([]string, rootProbeLimit+1)
	for i := range roots {
		roots[i] = libraryRoot(t, fmt.Sprintf("nas-%02d", i))
	}
	s := newStallingRoots(roots, nil)
	p := probesOver(t, s, testBound)
	// A run over each config the churn left, side by side: each begins the
	// probe of its one root.
	var wg sync.WaitGroup
	lines := make([]Check, rootProbeLimit)
	for i, r := range roots[:rootProbeLimit] {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lines[i] = checkLibraryRoots(context.Background(), Deps{RootProbes: p.BeginRun(), LibraryRoots: []string{r}})
		}()
	}
	within(t, testDeadline, "the runs over each config", func() struct{} { wg.Wait(); return struct{}{} })
	for i, c := range lines {
		if c.Status != Fail || !strings.Contains(c.Hint, "did not answer within") {
			t.Fatalf("library-roots of %s = %+v, want the root not answering", roots[i], c)
		}
	}
	last := roots[rootProbeLimit]
	c, _ := rootsLine(t, context.Background(), p, last)
	want := fmt.Sprintf("library root %s was not checked: %d earlier probes of the library roots have not answered yet, and the doctor begins no more until one does", last, rootProbeLimit)
	if c.Status != Warn || c.Summary != "1 root(s) not checked" || c.Hint != want {
		t.Errorf("past the limit, library-roots = %+v, want a warn saying %q", c, want)
	}
	if n := s.statsOf(last); n != 0 {
		t.Errorf("past the limit, a run began %d probes of a new root, want none", n)
	}
	again, _ := rootsLine(t, context.Background(), p, roots[0])
	if again.Status != Fail || !strings.Contains(again.Hint, "still has not answered") {
		t.Errorf("a root added again = %+v, want its probe still out", again)
	}
	if n := s.statsOf(roots[0]); n != 1 {
		t.Errorf("a root removed and added again was probed %d times, want once", n)
	}
	s.letGo()
	if !p.DrainForTest(10 * time.Second) {
		t.Fatal("the probes were still out once their roots answered")
	}
	if c, _ := rootsLine(t, context.Background(), p, last); c.Status != OK {
		t.Errorf("once the roots answer, library-roots = %+v, want the root probed and reachable", c)
	}
}

// TestHealthyRootsReadAsTheyAlwaysDid is B650's guard: over roots that all
// answer, library-roots is what main printed for this fixture, word for word
// (the same test is green on main's tree), and so are a missing root's and an
// empty one's (the watch line's guard is the platform's:
// TestAHealthyLibrarysWatchLineIsAsItWas).
func TestHealthyRootsReadAsTheyAlwaysDid(t *testing.T) {
	a, b := libraryRoot(t, "a"), libraryRoot(t, "b")
	if c := checkLibraryRoots(context.Background(), Deps{LibraryRoots: []string{a, b}}); c != ok(checkNameLibraryRoots, "2 root(s) reachable") {
		t.Errorf("library-roots = %+v", c)
	}
	gone, empty := filepath.Join(t.TempDir(), "gone"), t.TempDir()
	want := fail(checkNameLibraryRoots, "1 problem(s)", "fix or remove, after checking that a volume it is on is mounted: "+gone)
	if c := checkLibraryRoots(context.Background(), Deps{LibraryRoots: []string{a, gone}}); c != want {
		t.Errorf("library-roots of a missing root = %+v, want %+v", c, want)
	}
	want = warn(checkNameLibraryRoots, "1 empty root(s)",
		"empty root (scan will find 0 tracks; a volume not mounted yet reads so, and the scan keeps the tracks it indexed there): "+empty)
	if c := checkLibraryRoots(context.Background(), Deps{LibraryRoots: []string{a, empty}}); c != want {
		t.Errorf("library-roots of an empty root = %+v, want %+v", c, want)
	}
}

// TestTheCountsAreFoldedAsOneWalkCountedThem: the roots' counts, each a work
// of its own since B650, are folded in the roots' order as the one walk the
// check made counted them, so every case in which each count answers keeps the
// one walk's verdict: the first root's failure, the total reaching the cap,
// the sum. A count that does not answer is said, and a failure after one is
// passed over, but a total that reaches the cap without it is a verdict.
func TestTheCountsAreFoldedAsOneWalkCountedThem(t *testing.T) {
	failed := errors.New("the root's walk failed")
	type answer struct {
		n     int
		err   error
		stall bool
	}
	for _, c := range []struct {
		name       string
		answers    []answer
		wantTotal  int
		reached    bool
		err        bool
		unanswered int
	}{
		{"every count under the cap", []answer{{n: 3}, {n: 4}}, 7, false, false, 0},
		{"the total reaches the cap", []answer{{n: 6}, {n: 4}}, 10, true, false, 0},
		{"the first root fails", []answer{{err: failed}, {n: 4}}, 0, false, true, 0},
		{"a root fails after the cap is reached", []answer{{n: 10}, {err: failed}}, 10, true, false, 0},
		{"a count that does not answer", []answer{{stall: true}, {n: 4}}, 4, false, false, 1},
		{"a failure after a count that does not answer", []answer{{stall: true}, {err: failed}}, 0, false, false, 1},
		{"the cap reached beside a count that does not answer", []answer{{stall: true}, {n: 12}}, 12, true, false, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newStallingRoots(nil, nil)
			p := probesOver(t, s, testBound)
			roots := make([]string, len(c.answers))
			byRoot := map[string]answer{}
			for i, a := range c.answers {
				roots[i] = fmt.Sprintf("/library/%d", i)
				byRoot[roots[i]] = a
			}
			p.countWatches = func(root string, _ int) (watchCount, error) {
				a := byRoot[root]
				if a.stall {
					<-s.release
				}
				return watchCount{folders: a.n}, a.err
			}
			run := p.BeginRun()
			tally, _ := within(t, testDeadline, "the tally", func() watchTally {
				return run.tallyWatches(context.Background(), roots, 10, func(w watchCount) int { return w.folders }, unansweredCount("the folders"))
			})
			if tally.total.folders != c.wantTotal || tally.reached != c.reached || (tally.err != nil) != c.err || len(tally.unanswered) != c.unanswered {
				t.Errorf("tally = %+v, want total %d, reached %v, an error %v, %d unanswered", tally, c.wantTotal, c.reached, c.err, c.unanswered)
			}
		})
	}
}

// TestALoadThatDidNotFinishFailsTheConfigLine: cmd/bridge's load of the
// config, a work its runner waits for as it waits for a root, that gave the
// run no answer fails config-file in words of its own, never as a file that
// does not load, and the lines that grade what it sets say they were not
// checked; one the run stopped waiting for, or never began, is not checked, a
// warn.
func TestALoadThatDidNotFinishFailsTheConfigLine(t *testing.T) {
	const path = "/etc/1-bit-bridge/bridge.yaml"
	unfinished := func(w Wait) Deps {
		return Deps{APIPort: 7788, AdminPort: 7789, ConfigFile: &ConfigFile{Path: path, LoadErr: &UnfinishedLoadError{Wait: w}}}
	}
	d := unfinished(Wait{outcome: notAnswered, bound: 5 * time.Second})
	c := checkConfigFile(context.Background(), d)
	if c.Status != Fail || c.Summary != path+": its load did not finish within 5s" ||
		!strings.Contains(c.Hint, "upscale.variantsDir and upscale.tempDir") || strings.Contains(c.Hint, "refuses") {
		t.Errorf("config-file = %+v", c)
	}
	if c := checkAPIPort(context.Background(), d); c != ok(checkNamePortAPI, "not checked: the config that sets this port did not finish loading") {
		t.Errorf("port-api = %+v", c)
	}
	if c := checkTLSCert(context.Background(), d); c != ok(checkNameTLSCert, "not checked: the config that sets its path did not finish loading") {
		t.Errorf("tls-cert = %+v", c)
	}
	if got := d.ConfigFile.NotGradedReason("the data directory"); got != "the config that sets the data directory did not finish loading" {
		t.Errorf("NotGradedReason = %q", got)
	}
	still := checkConfigFile(context.Background(), unfinished(Wait{outcome: notAnswered, still: true, since: 2 * time.Minute, bound: 5 * time.Second}))
	if still.Status != Fail || still.Summary != path+": its load still has not finished, 2m0s after the doctor began it" {
		t.Errorf("config-file of a load still out = %+v", still)
	}
	for _, w := range []Wait{{outcome: stopped}, {outcome: heldBack, limit: otherWorkLimit}} {
		if c := checkConfigFile(context.Background(), unfinished(w)); c.Status != Warn || !strings.HasPrefix(c.Summary, path+" was not checked: ") {
			t.Errorf("config-file of a load not waited for (%v) = %+v, want a warn, not checked", w.outcome, c)
		}
	}
}

// TestTheLoadsHaveALimitOfTheirOwn: a runner keeps at most otherWorkLimit of
// its callers' works out (the config's loads), held back past it in words of
// their own, and roots that do not answer, up to their own limit, hold no load
// back: a load of a config that sets no directory touches no root, and the
// report that names those roots comes with it.
func TestTheLoadsHaveALimitOfTheirOwn(t *testing.T) {
	roots := make([]string, rootProbeLimit)
	for i := range roots {
		roots[i] = libraryRoot(t, fmt.Sprintf("nas-%02d", i))
	}
	s := newStallingRoots(roots, nil)
	p := probesOver(t, s, testBound)
	run := p.BeginRun()
	run.BeginRoots(roots)
	type loads struct {
		first     int
		firstWait Wait
		held      []Wait
		tooMany   Wait
	}
	got, _ := within(t, testDeadline, "the loads", func() loads {
		var l loads
		l.first, l.firstWait = Bounded(context.Background(), run, "a load that touches no root", func() int { return 7 })
		for i := range otherWorkLimit {
			_, w := Bounded(context.Background(), run, fmt.Sprintf("load %d", i), func() int { <-s.release; return i })
			l.held = append(l.held, w)
		}
		_, l.tooMany = Bounded(context.Background(), run, "one load too many", func() int { return 0 })
		return l
	})
	if !got.firstWait.Answered() || got.first != 7 {
		t.Fatalf("beside %d probes out, a load = %d, %+v; want it answered", rootProbeLimit, got.first, got.firstWait)
	}
	for _, w := range got.held {
		if w.Answered() {
			t.Fatalf("a load held until its roots answer answered: %+v", w)
		}
	}
	w := got.tooMany
	err := &UnfinishedLoadError{Wait: w}
	want := fmt.Sprintf("%d earlier loads of the config have not finished yet, and the doctor begins no more until one does", otherWorkLimit)
	if w.outcome != heldBack || err.Error() != want {
		t.Errorf("past the loads' limit, a load = %+v, %q; want it held back, %q", w, err.Error(), want)
	}
}
