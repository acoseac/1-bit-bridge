package doctor

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"time"
)

// The doctor's waits on a library root (backlog B650). A root on a network
// mount whose server has gone, or on a disk that spun down badly, holds a stat
// of it in the kernel for as long as the mount waits, without end on a hard NFS
// mount, and nothing cancels the call. checkLibraryRoots stat'ed and listed
// each root on the run's own goroutine and ignored its context, so `bridge
// doctor` printed nothing at all and the console's doctor never answered,
// exactly when the operator asked why the library was stuck. Every step of a
// run that touches a library root or anything below it now runs on a
// goroutine of its own, as a work of the runner's RootProbes, and the run
// waits for it rootProbeBound from when the work began: the probe of each
// root (its stat and its listing, probeRoot), the count of what the library
// watcher would hold under each (checkWatchLimit), and cmd/bridge's load of
// the config (Bounded), whose validation resolves each root where
// upscale.variantsDir or upscale.tempDir is set. A work that does not answer
// cannot be cancelled: it holds its goroutine until the kernel lets go, the
// process's end included.

// rootProbeBound is how long a run waits for a work on a library root,
// counted from when the work began. Five seconds, as cmd/bridge's
// doctorBackupsBound waits for the backups folder: a mount that answers at all
// answers a stat and a listing of one folder in far less (a local disk in
// microseconds, NFS or SMB over a LAN in milliseconds), and the run is an
// interactive diagnostic, which the console runs on every settings render. A
// disk spinning up can take longer, which the line says: the next run finds
// that work answered and asks afresh.
const rootProbeBound = 5 * time.Second

// rootProbeLimit is how many works on the roots (probes and counts) a runner
// keeps out at once, and otherWorkLimit how many of its callers' (the
// config's loads): twenty in all. A work that does not answer holds its
// goroutine, and the thread under it, until the kernel lets go, and the
// console reloads its config at every run, so a dead root replaced by
// another, then by another, would each leave one behind, and with
// upscale.variantsDir or upscale.tempDir set, a load of each config they were
// in. Past a limit a run begins no more of that kind, and says what it did not
// check, and why. Sixteen holds a probe and a count of each root of a library
// of several roots on one NAS that went away. The loads have a limit of their
// own so that roots that do not answer never hold back a load, which with no
// directory setting touches no root, and the report that names them with it.
const (
	rootProbeLimit = 16
	otherWorkLimit = 4
)

// RootProbes is a doctor runner's works on the library roots that are out:
// at most one for each key at a time (a root's probe, a root's count, a
// config's load), held from when it begins until its worker returns, whatever
// the run that began it did meanwhile. A later run finds it and waits for it
// rather than beginning another, so a root that never answers holds one
// goroutine however often the doctor runs, and a run after it returned begins
// afresh, which is how a mount that answers again reads healthy at the next
// run. Its worker keeps what it found and closes its done channel, so an
// answer that comes after every run stopped waiting blocks nothing. Safe for
// concurrent runs: the console's doctor is not serialised, and GET /api/doctor
// runs on every settings render.
//
// It belongs to the runner, never to a run: cmd/bridge's console runner
// (adminDoctorRunnerFor) makes one and keeps it across its runs, whose Deps it
// rebuilds every time, and the CLI's commands share one for their process.
type RootProbes struct {
	bound time.Duration
	// limit and otherLimit hold new works back: rootProbeLimit and
	// otherWorkLimit.
	limit, otherLimit int
	// stat and readDir are the probe's two calls on a root: os.Stat and
	// os.ReadDir, or a test's stand-ins for a root that does not answer
	// (NewRootProbesForTest), which no filesystem a test can make provides.
	stat    func(string) (fs.FileInfo, error)
	readDir func(string) ([]fs.DirEntry, error)
	// countWatches counts what the library watcher would hold under one root,
	// this platform's countRootWatches, or a test's stand-in.
	countWatches func(root string, stopAt int) (watchCount, error)
	// now and timer are the runner's clock: when a run and a work began, and
	// the timer a wait for a work's bound sets (time.Now and time.NewTimer's
	// channel and Stop), or a test's, whose time moves only when the test
	// moves it.
	now   func() time.Time
	timer func(d time.Duration) (c <-chan time.Time, stop func() bool)

	mu  sync.Mutex
	out map[workKey]*rootWork
}

// NewRootProbes is a runner's RootProbes, with no work out.
func NewRootProbes() *RootProbes {
	return &RootProbes{
		bound:        rootProbeBound,
		limit:        rootProbeLimit,
		otherLimit:   otherWorkLimit,
		stat:         os.Stat,
		readDir:      os.ReadDir,
		countWatches: countRootWatches,
		now:          time.Now,
		timer:        realTimer,
		out:          map[workKey]*rootWork{},
	}
}

// realTimer is a timer of the time package's, as RootProbes.timer gives one.
func realTimer(d time.Duration) (<-chan time.Time, func() bool) {
	t := time.NewTimer(d)
	return t.C, t.Stop
}

// NewRootProbesForTest is NewRootProbes waiting bound for a work and probing a
// root through stat and readDir, a nil one os.Stat's or os.ReadDir's: a test
// stands in for a root that does not answer. Tests only.
func NewRootProbesForTest(bound time.Duration, stat func(string) (fs.FileInfo, error), readDir func(string) ([]fs.DirEntry, error)) *RootProbes {
	p := NewRootProbes()
	p.bound = bound
	if stat != nil {
		p.stat = stat
	}
	if readDir != nil {
		p.readDir = readDir
	}
	return p
}

// DrainForTest waits, within, until p has no work out, and says whether it
// has none: a test that stood a root that does not answer in lets it answer,
// then waits for every work it held to end before it puts a stand-in back.
// Tests only.
func (p *RootProbes) DrainForTest(within time.Duration) bool {
	for deadline := time.Now().Add(within); ; time.Sleep(5 * time.Millisecond) {
		p.mu.Lock()
		n := len(p.out)
		p.mu.Unlock()
		if n == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

// ProbeRun is one doctor run's view of a runner's RootProbes: the works it
// begins and waits for, and when it began, which tells a root still not
// answering a work an earlier run began from one this run asked. It keeps
// every work it has met, answered or not, so each root is probed once a run,
// as before B650, wherever the run first asks for it (beside the config's
// load, at the run's start, in its check). Deps carries it
// (Deps.RootProbes).
type ProbeRun struct {
	p     *RootProbes
	began time.Time
	mu    sync.Mutex
	met   map[workKey]*rootWork
}

// BeginRun is a run of p beginning now.
func (p *RootProbes) BeginRun() *ProbeRun {
	return &ProbeRun{p: p, began: p.now(), met: map[workKey]*rootWork{}}
}

// probeRun is d's run of its runner's RootProbes, or a run of a RootProbes of
// its own, for Deps that carry none: a check called alone, as a test calls
// it, and a caller whose process ends with the run.
func (d Deps) probeRun() *ProbeRun {
	if d.RootProbes != nil {
		return d.RootProbes
	}
	return NewRootProbes().BeginRun()
}

// workKey names a work: its kind and what it is of.
type workKey struct{ kind, name string }

// The kinds of work a runner keeps out.
const (
	workRoot  = "root"  // a root's probe: its stat and its listing
	workCount = "count" // the count of what the watcher would hold under a root
	workOther = "other" // a caller's work (Bounded): cmd/bridge's load of the config
)

// rootWork is one work: when it began, and, once done is closed, what its
// worker returned, or the panic it recovered (workPanic).
type rootWork struct {
	began time.Time
	done  chan struct{}
	value any
}

// workPanic is a panic a work's worker recovered, which the run that waits
// for the work raises again on its own goroutine, as the panic would have
// been raised before the work had a goroutine of its own: net/http recovers
// the console's, and the CLI's ends the process.
type workPanic struct{ v any }

// begin is the work the run met under key already, or the one out under key,
// or fn begun now as that work on a goroutine of its own; nil where the
// runner's limit for its kind of work holds a new one back, which a later
// call of the run asks again.
func (r *ProbeRun) begin(key workKey, fn func() any) *rootWork {
	r.mu.Lock()
	defer r.mu.Unlock()
	if w, met := r.met[key]; met {
		return w
	}
	w := r.p.beginOut(key, fn)
	if w != nil {
		r.met[key] = w
	}
	return w
}

// beginOut is the work out under key, or fn begun now as that work; nil
// where the limit for its kind holds a new one back.
func (p *RootProbes) beginOut(key workKey, fn func() any) *rootWork {
	p.mu.Lock()
	defer p.mu.Unlock()
	if w, out := p.out[key]; out {
		return w
	}
	if p.outOfKind(key.kind) >= p.limitOf(key.kind) {
		return nil
	}
	w := &rootWork{began: p.now(), done: make(chan struct{})}
	p.out[key] = w
	go p.work(key, w, fn)
	return w
}

// outOfKind is how many works of kind's class are out: the works on the
// roots (probes and counts) together, or the callers'. Caller holds p.mu.
func (p *RootProbes) outOfKind(kind string) int {
	n := 0
	for k := range p.out {
		if (k.kind == workOther) == (kind == workOther) {
			n++
		}
	}
	return n
}

// limitOf is the runner's limit for kind's class.
func (p *RootProbes) limitOf(kind string) int {
	if kind == workOther {
		return p.otherLimit
	}
	return p.limit
}

// work runs fn as w, keeps what it returned or the panic it recovered, and
// lets go of key once fn has returned, and only then.
func (p *RootProbes) work(key workKey, w *rootWork, fn func() any) {
	var v any
	defer func() {
		if r := recover(); r != nil {
			v = workPanic{r}
		}
		p.mu.Lock()
		w.value = v
		if p.out[key] == w {
			delete(p.out, key)
		}
		p.mu.Unlock()
		close(w.done)
	}()
	v = fn()
}

// Wait is how a work a run waited for ended for that run: answered, or why
// not.
type Wait struct {
	outcome waitOutcome
	// still says an earlier run began the work, which has not answered
	// since; since is how long it had been out when the wait ended.
	still bool
	since time.Duration
	bound time.Duration
	limit int
}

type waitOutcome int

const (
	answered    waitOutcome = iota
	notAnswered             // its bound passed first
	stopped                 // the run's context ended first, within the bound
	heldBack                // never begun: the runner's limit
)

// Answered says the work answered.
func (w Wait) Answered() bool { return w.outcome == answered }

// waitFor waits for w, a work of kind, until it answers, the bound from when
// it began passes, or ctx ends, and says which; a nil w is one the runner's
// limit for kind held back. A work that answers as its bound passes or as ctx
// ends is answered, and one past its bound is not answered whatever ctx says:
// the run's stop cut no wait short there. A panic its worker recovered is
// raised again here.
func (r *ProbeRun) waitFor(ctx context.Context, w *rootWork, kind string) Wait {
	p := r.p
	if w == nil {
		return Wait{outcome: heldBack, bound: p.bound, limit: p.limitOf(kind)}
	}
	if left := w.began.Add(p.bound).Sub(p.now()); left > 0 && !isDone(w) {
		passed, stop := p.timer(left)
		select {
		case <-w.done:
		case <-passed:
		case <-ctx.Done():
			if !isDone(w) && p.now().Before(w.began.Add(p.bound)) {
				stop()
				return Wait{outcome: stopped, bound: p.bound}
			}
		}
		stop()
	}
	if !isDone(w) {
		return Wait{outcome: notAnswered, still: w.began.Before(r.began),
			since: p.now().Sub(w.began).Round(time.Second), bound: p.bound}
	}
	if wp, ok := w.value.(workPanic); ok {
		panic(wp.v)
	}
	return Wait{outcome: answered, bound: p.bound}
}

func isDone(w *rootWork) bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

// Bounded is fn as a work of r's runner, out under key: begun now on a
// goroutine of its own, or the one out under key already, whichever run began
// it, waited for as a probe of a root is (waitFor). It answers fn's value
// where the work answered, and the zero value where it did not, with the
// Wait, which says why. cmd/bridge's load of the doctor's config is one
// (backlog B650), keyed by what the load reads.
func Bounded[T any](ctx context.Context, r *ProbeRun, key string, fn func() T) (T, Wait) {
	w := r.begin(workKey{workOther, key}, func() any { return fn() })
	wait := r.waitFor(ctx, w, workOther)
	var v T
	if wait.Answered() {
		v, _ = w.value.(T)
	}
	return v, wait
}

// BeginRoots begins the probe of each root, where none is out already, and
// waits for none. Run begins them before its first check, and cmd/bridge
// before it loads the config, so their bound runs beside the load's.
func (r *ProbeRun) BeginRoots(roots []string) {
	for _, root := range roots {
		r.beginRoot(root)
	}
}

// beginRoot is the probe of root out, or begun now.
func (r *ProbeRun) beginRoot(root string) *rootWork {
	p := r.p
	return r.begin(workKey{workRoot, root}, func() any { return p.probeRoot(root) })
}

// rootAnswer is what a probe of a root found, in the terms checkLibraryRoots
// grades: the stat's failure, a root that is no directory, the listing's
// failure, or whether it lists nothing.
type rootAnswer struct {
	statErr error
	notDir  bool
	readErr error
	empty   bool
}

// probeRoot is the probe of one root: a stat, and a listing of a root that
// is a directory. It runs as a work, never on a run's goroutine.
func (p *RootProbes) probeRoot(root string) rootAnswer {
	info, err := p.stat(root)
	if err != nil {
		return rootAnswer{statErr: err}
	}
	if !info.IsDir() {
		return rootAnswer{notDir: true}
	}
	entries, err := p.readDir(root)
	if err != nil {
		return rootAnswer{readErr: err}
	}
	return rootAnswer{empty: len(entries) == 0}
}

// watchCount is what the library watcher would hold under a root: on Linux
// its directories, one inotify watch each (folders alone); where fsnotify
// watches through kqueue, an open file for each folder and each entry in one
// (manifest.WatchSetCount).
type watchCount struct {
	folders, entries int
}

// countAnswer is what the count of a root found.
type countAnswer struct {
	c   watchCount
	err error
}

// beginCount is the count of root, stopping at stopAt, out, or begun now.
// stopAt is in its key: a count to another cap is another work.
func (r *ProbeRun) beginCount(root string, stopAt int) *rootWork {
	p := r.p
	return r.begin(workKey{workCount, fmt.Sprintf("%d\x00%s", stopAt, root)}, func() any {
		c, err := p.countWatches(root, stopAt)
		return countAnswer{c: c, err: err}
	})
}

// watchTally is the counts of a run's roots folded in their order, as the
// watcher's own count walked them when it was one walk (countDirs,
// manifest.CountWatchSet): it stops at the first root whose count failed
// (err) and where the total reaches stopAt (reached), over the roots whose
// count answered; unanswered says of each root whose count gave the run no
// answer why, and a root's failure after such a root is passed over, since a
// verdict is not known there anyway. With every count answered, the verdict is
// the one walk's.
type watchTally struct {
	total      watchCount
	reached    bool
	err        error
	unanswered []string
}

// tallyWatches begins (or joins) the count of each root, stopping at stopAt,
// and folds them (watchTally); size is what the platform's budget counts of a
// watchCount, and words says why a count gave no answer.
func (r *ProbeRun) tallyWatches(ctx context.Context, roots []string, stopAt int, size func(watchCount) int, words func(root string, w Wait) string) watchTally {
	works := make([]*rootWork, len(roots))
	for i, root := range roots {
		works[i] = r.beginCount(root, stopAt)
	}
	var t watchTally
	for i, root := range roots {
		w := r.waitFor(ctx, works[i], workCount)
		if !w.Answered() {
			t.unanswered = append(t.unanswered, words(root, w))
			continue
		}
		a := works[i].value.(countAnswer)
		if a.err != nil {
			if len(t.unanswered) == 0 {
				t.err = a.err
				return t
			}
			continue
		}
		t.total.folders += a.c.folders
		t.total.entries += a.c.entries
		if size(t.total) >= stopAt {
			t.reached = true
			return t
		}
	}
	return t
}

// unansweredCount says why the count of noun (the folders, or the folders and
// entries) under a root gave a run no answer. It names the root as
// configured, and no path below it.
func unansweredCount(noun string) func(root string, w Wait) string {
	return func(root string, w Wait) string {
		switch w.outcome {
		case notAnswered:
			if w.still {
				return fmt.Sprintf("%s under library root %s are still not all counted, %s after the doctor began counting them: a mount under it may be stalled", noun, root, w.since)
			}
			return fmt.Sprintf("%s under library root %s were not all counted within %s: a mount under it may be stalled", noun, root, w.bound)
		case stopped:
			return fmt.Sprintf("%s under library root %s were not counted: the doctor's run was stopped first", noun, root)
		case heldBack:
			return fmt.Sprintf("%s under library root %s were not counted: %s", noun, root, heldBackWords(w))
		}
		return ""
	}
}

// stalledCountAdvice is what a watch-budget line says to do of a root whose
// count did not finish.
const stalledCountAdvice = "check the server or the disk each folder under that root is on, and its mount: the library watcher's own walk of the root waits for it too"

// unansweredRoot says why the probe of root gave a run no answer. It names the
// root as configured, and no path below it.
func unansweredRoot(root string, w Wait) string {
	switch w.outcome {
	case notAnswered:
		if w.still {
			return fmt.Sprintf("library root %s still has not answered, %s after the doctor first asked it: its mount may be stalled", root, w.since)
		}
		return fmt.Sprintf("library root %s did not answer within %s: its mount may be stalled", root, w.bound)
	case stopped:
		return fmt.Sprintf("library root %s was not checked: the doctor's run was stopped before it answered", root)
	case heldBack:
		return fmt.Sprintf("library root %s was not checked: %s", root, heldBackWords(w))
	}
	return ""
}

// heldBackWords says why a run began no work: the runner's limit.
func heldBackWords(w Wait) string {
	return fmt.Sprintf("%d earlier probes of the library roots have not answered yet, and the doctor begins no more until one does", w.limit)
}
