package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/updater"
)

// The confirmation of an install of this version (backlog B660). Until B660
// the boot's housekeeping stamped the install installed at the first boot of
// the version installed, before anything listened, and the boot after it
// removed the backup, so a version that crashed after its housekeeping had
// its backup deleted by its own second start. Now the install stays on
// probation until one start of it has answered as ready and stayed up for
// updater.ProbationWindow: a goroutine serve starts once ServeTLS is running,
// only where the marker awaits this version's confirmation, joined through
// bgWriters on scanCtx (serveProbation.run).
//
// Ready is three answers in one round, all of them this process's: a connect
// to the API listener's bound address that this process's accept loop took
// (acceptWatch: a connect alone can land in the kernel's backlog with nobody
// accepting), GET /v1/health run in-process through the API handler answering
// 200 with this version (healthAnswers), and a read of the store. Not ready's
// business: a scan or how far it got, enrichment, the library roots, a TLS
// handshake (the reason health.go gives: an expired or rotated certificate
// reads as dead, and a restart cannot fix that), the tailnet, the player,
// DLNA. So a slow first scan cannot delay the confirmation, and a no never
// rolls anything back: a start that never answers is never confirmed and
// keeps running, and says so once, recording NotReady. A counted start that
// has not begun serving within its deadline from its admission says so too,
// and records the same (startWatch; backlog B760).

// probationSeams is a boot test's hold on the confirmation of an install of
// this version (backlog B660). Nil in production.
type probationSeams struct {
	// timing replaces production's: a zero field is production's.
	timing probationTiming
	// observe is told each thing the confirmation does, on its goroutine, as
	// it does it.
	observe func(probationEvent)
	// storeRead stands in for a round's store read (the store's
	// CountServedTracks).
	storeRead func(ctx context.Context) error
	// beforeReady runs once a round has first answered as ready, before this
	// start counts as ready (backlog B660's third round).
	beforeReady func()
	// beforeServing runs on serve's goroutine just before it begins serving
	// (ServeTLS): a boot test's hold on a start that has not begun serving
	// (backlog B760).
	beforeServing func()
}

// probationTiming is the confirmation's clock: the window one start stays up
// for once it has answered as ready (updater.ProbationWindow), how often it
// asks before its first yes, how often after a no at the window's end (and
// after the window's first lapse with no yes), one round's bound, and how
// long a start has from its admission to begin serving (updater.ProbationWindow
// too; backlog B760). Timers and time.Since read the monotonic clock: a box
// with no RTC boots with a stale date, and nothing here reads the wall clock.
type probationTiming struct {
	window, ask, retry, round, serve time.Duration
}

// productionProbation is production's timing.
var productionProbation = probationTiming{window: updater.ProbationWindow, ask: 5 * time.Second,
	retry: 30 * time.Second, round: 5 * time.Second, serve: updater.ProbationWindow}

// orProduction is t with each zero field production's.
func (t probationTiming) orProduction() probationTiming {
	if t.window <= 0 {
		t.window = productionProbation.window
	}
	if t.ask <= 0 {
		t.ask = productionProbation.ask
	}
	if t.retry <= 0 {
		t.retry = productionProbation.retry
	}
	if t.round <= 0 {
		t.round = productionProbation.round
	}
	if t.serve <= 0 {
		t.serve = productionProbation.serve
	}
	return t
}

// probationEventKind is what a probationEvent says happened.
type probationEventKind int

const (
	// probationRound: a round answered, ready or why not.
	probationRound probationEventKind = iota + 1
	// probationWindow: the first yes, which starts the window.
	probationWindow
	// probationConfirmed: the install was stamped installed.
	probationConfirmed
	// probationNotReady: the window lapsed with no confirmation, or the start
	// had not begun serving by its deadline (startWatch; backlog B760), said
	// once and recorded in the marker.
	probationNotReady
	// probationUnconfirmed: a confirmation that could not be made at this
	// ask (its lock, its write), asked again at the next, or never where
	// the filesystem keeps no locks.
	probationUnconfirmed
	// probationEnded: the confirmation's goroutine returned.
	probationEnded
	// probationServing: the start began serving, which ended the watch on it
	// from its admission (backlog B760).
	probationServing
)

// probationEvent is one thing the confirmation did.
type probationEvent struct {
	kind  probationEventKind
	ready bool
	why   string
}

// acceptWatch is the API listener as serve hands it to ServeTLS, telling a
// probe which connections this process's accept loop took. A probe dials the
// listener's own address and waits until its accept loop has taken that very
// connection, matched by the probe's local address, which the server sees as
// the connection's remote one: a connect alone can complete in the kernel's
// backlog while nothing accepts.
type acceptWatch struct {
	net.Listener

	mu sync.Mutex
	// probing counts the probes under way; seen keeps the addresses the
	// accept loop took while one is, and changed is closed at each.
	probing int
	seen    map[netip.AddrPort]struct{}
	changed chan struct{}
}

// maxSeenAccepts bounds what one probe keeps of the accept loop's
// connections: a probe looks for its own among the few taken in the moment
// its connect takes.
const maxSeenAccepts = 4096

func newAcceptWatch(l net.Listener) *acceptWatch {
	return &acceptWatch{Listener: l, changed: make(chan struct{})}
}

// Accept is the listener's, telling a probe under way what it took.
func (w *acceptWatch) Accept() (net.Conn, error) {
	c, err := w.Listener.Accept()
	if err == nil {
		w.took(c.RemoteAddr())
	}
	return c, err
}

func (w *acceptWatch) took(addr net.Addr) {
	key, ok := addrKey(addr)
	if !ok {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.probing == 0 {
		return
	}
	if len(w.seen) < maxSeenAccepts {
		w.seen[key] = struct{}{}
	}
	close(w.changed)
	w.changed = make(chan struct{})
}

// addrKey is a TCP address as a probe compares it: an IPv4 address mapped in
// IPv6, as a dual-stack listener reports one, read as the IPv4 address.
func addrKey(addr net.Addr) (netip.AddrPort, bool) {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok || tcp == nil {
		return netip.AddrPort{}, false
	}
	ap := tcp.AddrPort()
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()), true
}

// probe dials the listener's own address, a wildcard on loopback as `bridge
// health` dials it (probeHosts), and waits until this process's accept loop
// has taken that connection or ctx ends. Where the wildcard is the IPv6 one it
// dials the IPv4 loopback first and the IPv6 loopback after it, each connect
// but the last bounded by its share of what is left of ctx (backlog B660's
// third round: a listener on the IPv6 wildcard alone, beside another
// process's listener on the port's IPv4 loopback, took the probe's one IPv4
// connect for that process's, and the round waited out its bound). The
// connection is closed without a byte, which the API listener's error log
// drops for a peer on this host (internal/handshakelog).
func (w *acceptWatch) probe(ctx context.Context) error {
	w.mu.Lock()
	if w.probing == 0 {
		w.seen = map[netip.AddrPort]struct{}{}
	}
	w.probing++
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		if w.probing--; w.probing == 0 {
			w.seen = nil
		}
		w.mu.Unlock()
	}()
	host, port, err := net.SplitHostPort(w.Addr().String())
	if err != nil {
		return fmt.Errorf("the API listener's address does not read: %w", err)
	}
	hosts := probeHosts(host)
	for i, h := range hosts {
		err = w.probeAt(ctx, net.JoinHostPort(h, port), len(hosts)-i)
		if err == nil || ctx.Err() != nil {
			return err
		}
	}
	return err
}

// probeAt is probe's connect to addr, one of left the probe has still to try:
// all but the last get their share of what is left of ctx, and with no
// deadline on ctx, of a round's.
func (w *acceptWatch) probeAt(ctx context.Context, addr string, left int) error {
	if left > 1 {
		share := productionProbation.round / time.Duration(left)
		if deadline, ok := ctx.Deadline(); ok {
			share = time.Until(deadline) / time.Duration(left)
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, share)
		defer cancel()
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("a connect to the API listener at %s failed: %w", addr, err)
	}
	defer conn.Close()
	mine, ok := addrKey(conn.LocalAddr())
	if !ok {
		return fmt.Errorf("a connect to the API listener at %s has no TCP address of its own", addr)
	}
	for {
		w.mu.Lock()
		_, taken := w.seen[mine]
		changed := w.changed
		w.mu.Unlock()
		if taken {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return fmt.Errorf("a connect to the API listener at %s was not taken by this process's accept loop", addr)
		}
	}
}

// maxHealthAnswer bounds what a round keeps of /v1/health's answer.
const maxHealthAnswer = 256 << 10

// healthRecorder is the ResponseWriter a round's in-process GET /v1/health
// writes into: its status, and its body to a bound. Not net/http/httptest's,
// which production code does not import.
type healthRecorder struct {
	header http.Header
	status int
	body   []byte
}

func (r *healthRecorder) Header() http.Header { return r.header }

func (r *healthRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
}

func (r *healthRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	if room := maxHealthAnswer - len(r.body); room > 0 {
		r.body = append(r.body, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// SetWriteDeadline is what the API's bounded routes ask of a writer
// (http.ResponseController): an in-process call has no socket to bound.
func (r *healthRecorder) SetWriteDeadline(time.Time) error { return nil }

// healthAnswers runs GET /v1/health through h, the handler the API listener
// serves, in this process, as an unauthenticated caller on this host at
// host, and requires 200 with serverVersion current.
func healthAnswers(ctx context.Context, h http.Handler, host, current string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/v1/health", nil)
	if err != nil {
		return fmt.Errorf("GET /v1/health could not be made: %w", err)
	}
	req.Host, req.RemoteAddr = host, "127.0.0.1:0"
	rec := &healthRecorder{header: http.Header{}}
	h.ServeHTTP(rec, req)
	if rec.status != http.StatusOK {
		return fmt.Errorf("GET /v1/health answered %d", rec.status)
	}
	var answer struct {
		ServerVersion string `json:"serverVersion"`
	}
	if err := json.Unmarshal(rec.body, &answer); err != nil {
		return errors.New("GET /v1/health's answer does not read as its JSON")
	}
	if !updater.SameVersion(answer.ServerVersion, current) {
		return fmt.Errorf("GET /v1/health answered as %q, not as %s", answer.ServerVersion, current)
	}
	return nil
}

// readiness is what a round asks: the API listener's accept loop, /v1/health
// through the API handler, and the store, all of this process.
type readiness struct {
	accepts *acceptWatch
	handler http.Handler
	current string
	store   func(ctx context.Context) error
}

// round asks the three, in turn, within ctx.
func (r readiness) round(ctx context.Context) error {
	if err := r.accepts.probe(ctx); err != nil {
		return err
	}
	if err := healthAnswers(ctx, r.handler, r.accepts.Addr().String(), r.current); err != nil {
		return err
	}
	if err := r.store(ctx); err != nil {
		return fmt.Errorf("the store did not answer a read: %w", err)
	}
	return nil
}

// serveProbation is one start's confirmation of the install of its version.
type serveProbation struct {
	dataDir, current, previous string
	ready                      readiness
	timing                     probationTiming
	observe                    func(probationEvent)
	beforeReady                func()
	stderr                     io.Writer
	// onReady is told this start's first ready answer, which arms the
	// take-back of its count as a stop arrives (stopTakeBack.arm; backlog
	// B660's third round), and reports whether it did: false where a stop came
	// first, which ends the confirmation there. Nil where the start has no
	// count to take back.
	onReady func() bool
	// out is the round still out past its bound, nil when none: no round
	// starts beside it, and its late answer counts for nothing.
	out chan error
}

// newServeProbation is the confirmation of this start's install, or nil
// where the marker in dataDir awaits no confirmation of current (no install,
// another version's, one confirmed already, or a marker that cannot be read):
// a goroutine serve starts only where there is one to make.
func newServeProbation(dataDir, current string, ready readiness, seams *probationSeams, stderr io.Writer) *serveProbation {
	st, err := updater.LoadState(dataDir)
	if err != nil || !updater.AwaitsConfirmation(st, current) {
		return nil
	}
	p := &serveProbation{dataDir: dataDir, current: current, previous: st.PreviousVersion, ready: ready,
		timing: productionProbation, stderr: stderr}
	if seams != nil {
		p.timing = seams.timing.orProduction()
		p.observe, p.beforeReady = seams.observe, seams.beforeReady
		if seams.storeRead != nil {
			p.ready.store = seams.storeRead
		}
	}
	return p
}

func (p *serveProbation) note(e probationEvent) {
	if p.observe != nil {
		p.observe(e)
	}
}

// run asks every timing.ask until the first ready answer, which makes this
// start ready (onReady: from then on a stop takes its count back as it
// arrives; backlog B660's third round) and starts the window; asks once more
// at the window's end, and every timing.retry after a no, and confirms the
// install at the first yes past the window. A window that lapses with no
// confirmation is said once and recorded (State.NotReady), and past that lapse
// the asks before a first yes come every timing.retry. It returns once the
// install is confirmed, the marker awaits no confirmation of this version any
// more, the filesystem keeps no locks, or ctx ends (serve's stop), which a wait
// and a round's bound both answer at once, a stop that came before the ready
// answer was counted included.
func (p *serveProbation) run(ctx context.Context) {
	defer p.note(probationEvent{kind: probationEnded})
	began := time.Now()
	lapsed := false
	wait := p.timing.ask
	for {
		if !sleepUnlessStopped(ctx, wait) {
			return
		}
		err := p.ask(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			break
		}
		if !lapsed && time.Since(began) >= p.timing.window {
			lapsed = true
			p.notReady(ctx, err)
			wait = p.timing.retry
		}
	}
	// The first yes: this start is ready, where no stop came first, and the
	// take-back of its count is armed (stopTakeBack.arm, which asks serve's
	// own context itself, so a stop that lands between the round's answer and
	// here is told apart).
	if p.beforeReady != nil {
		p.beforeReady()
	}
	if p.onReady != nil && !p.onReady() {
		return
	}
	if ctx.Err() != nil {
		return
	}
	fmt.Fprintf(p.stderr, "updater: this start of %s has answered as ready (its API listener, /v1/health and its store): "+
		"its install is confirmed once it has stayed up for %s\n", p.current, p.timing.window)
	p.note(probationEvent{kind: probationWindow})
	if !sleepUnlessStopped(ctx, p.timing.window) {
		return
	}
	for {
		err := p.ask(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if !lapsed {
				lapsed = true
				p.notReady(ctx, err)
			}
		} else if p.confirm(ctx) {
			return
		}
		if !sleepUnlessStopped(ctx, p.timing.retry) {
			return
		}
	}
}

// sleepUnlessStopped waits d, and reports false where ctx ended first.
func sleepUnlessStopped(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// roundGrace is how long past timing.round a round's answer is waited for:
// its checks run on a context that ends at timing.round, so a check that
// takes its cancel (the accept loop's, the store's) says why it ended within
// it, and a call that takes none is a no at the bound.
const roundGrace = time.Second

// ask runs one round on a goroutine of its own under timing.round, one at a
// time: ServeHTTP takes no cancel (the health handler's endpoint walk is a
// singleflight that ignores its caller's context), so a round still out at
// its bound (timing.round and roundGrace) is a no, no second round starts
// beside it, its late answer counts for nothing, and serve's stop does not
// wait for it. A round that panics is a no.
func (p *serveProbation) ask(ctx context.Context) error {
	if p.out != nil {
		select {
		case <-p.out:
			p.out = nil
		default:
			err := errors.New("the round before this one has not answered yet")
			p.note(probationEvent{kind: probationRound, why: err.Error()})
			return err
		}
	}
	answer := make(chan error, 1)
	roundCtx, cancel := context.WithTimeout(ctx, p.timing.round)
	go func() {
		defer cancel()
		defer func() {
			if recover() != nil {
				answer <- errors.New("a round panicked")
			}
		}()
		answer <- p.ready.round(roundCtx)
	}()
	bound := time.NewTimer(p.timing.round + roundGrace)
	defer bound.Stop()
	var err error
	select {
	case err = <-answer:
	case <-bound.C:
		p.out = answer
		err = fmt.Errorf("a round did not answer within %s", p.timing.round+roundGrace)
	case <-ctx.Done():
		p.out = answer
		return ctx.Err()
	}
	e := probationEvent{kind: probationRound, ready: err == nil}
	if err != nil {
		e.why = err.Error()
	}
	p.note(e)
	return err
}

// notReady says once that this start has not answered as ready within the
// window, and records it in the marker (updater.RecordNotReady), which the
// next start's admission and the confirmation clear.
func (p *serveProbation) notReady(ctx context.Context, why error) {
	fmt.Fprintf(p.stderr, "updater: %s has not answered as ready within %s of serving (%v), so its install is not confirmed: "+
		"it goes on serving, and a start that answers and stays up for %s confirms it\n", p.current, p.timing.window, why, p.timing.window)
	if _, err := updater.RecordNotReady(ctx, p.dataDir, p.current); err != nil && ctx.Err() == nil {
		fmt.Fprintf(p.stderr, "updater: the install marker does not record that this start has not answered as ready: %v\n", err)
	}
	p.note(probationEvent{kind: probationNotReady, why: why.Error()})
}

// confirm asks updater.ConfirmInstall, and reports whether nothing is left
// to ask: the install confirmed, the marker no longer an install of this
// version awaiting confirmation (a newer install made meanwhile, a
// rollback), the filesystem keeping no locks, or serve's stop. A lock it
// cannot hold, or a write that fails, leaves the marker, says so on one line,
// and is asked again at the next round.
func (p *serveProbation) confirm(ctx context.Context) bool {
	confirmed, err := updater.ConfirmInstall(ctx, p.dataDir, p.current, time.Now().UTC())
	switch {
	case err == nil && confirmed:
		fmt.Fprintf(p.stderr, "updater: %s has answered as ready and stayed up for %s, so its install is confirmed: "+
			"the backup of %s is removed at the next start\n", p.current, p.timing.window, p.previous)
		p.note(probationEvent{kind: probationConfirmed})
		return true
	case err == nil:
		return true
	case ctx.Err() != nil:
		return true
	case updater.KeepsNoLocks(err):
		fmt.Fprintf(p.stderr, "updater: the install of %s is not confirmed: %v\n", p.current, err)
		p.note(probationEvent{kind: probationUnconfirmed, why: err.Error()})
		return true
	}
	fmt.Fprintf(p.stderr, "updater: the install of %s is not confirmed at this ask (%v): it is asked again in %s\n",
		p.current, err, p.timing.retry)
	p.note(probationEvent{kind: probationUnconfirmed, why: err.Error()})
	return false
}

// startWatch is the watch on a counted start's beginning to serve (backlog
// B760). The confirmation begins once ServeTLS runs, so a start that hung
// before it (a migration at the store's open that never ends, a lock it waits
// for) and was stopped cleanly was never said not to be ready: its admission
// counted it, its stop took the count back, and the doctor read "ok, on
// probation" throughout. Armed at the admission, it says once that the start
// has not begun serving and records NotReady (updater.RecordNotReady), which
// the doctor and the console fail on and the next start's admission and the
// confirmation clear, where the start has not begun serving within
// timing.serve of its admission (updater.ProbationWindow in production), on
// the monotonic clock. Reporting only: the count stays the admission's and the
// take-back's, so a slow but healthy start stopped cleanly counts nothing, and
// a start stopped before its deadline is not said not to be ready.
type startWatch struct {
	cancel  context.CancelFunc
	done    chan struct{}
	serving atomic.Bool
}

// watchTheStart arms the watch on the start of current in dataDir, its
// timing and observer seams' (production's where seams is nil), until the
// start begins serving (served), ctx ends (serve's stop, and runServe's own
// end), or the teardown ends it (end).
func watchTheStart(ctx context.Context, dataDir, current string, seams *probationSeams, stderr io.Writer) *startWatch {
	timing := productionProbation
	var observe func(probationEvent)
	if seams != nil {
		timing, observe = seams.timing.orProduction(), seams.observe
	}
	note := func(e probationEvent) {
		if observe != nil {
			observe(e)
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	w := &startWatch{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(w.done)
		if !sleepUnlessStopped(ctx, timing.serve) {
			if w.serving.Load() {
				note(probationEvent{kind: probationServing})
			}
			return
		}
		fmt.Fprintf(stderr, "updater: %s has not begun serving within %s of its start, so its install is not confirmed: "+
			"it goes on starting, and a start that answers as ready and stays up for %s confirms it\n", current, timing.serve, timing.window)
		if _, err := updater.RecordNotReady(ctx, dataDir, current); err != nil && ctx.Err() == nil {
			fmt.Fprintf(stderr, "updater: the install marker does not record that this start has not answered as ready: %v\n", err)
		}
		note(probationEvent{kind: probationNotReady, why: fmt.Sprintf("this start has not begun serving within %s of its start", timing.serve)})
	}()
	return w
}

// served is told that the start begins serving: the watch ends there, joined.
// Safe on nil.
func (w *startWatch) served() {
	if w == nil {
		return
	}
	w.serving.Store(true)
	w.end()
}

// end ends the watch and joins it. Safe on nil, and more than once.
func (w *startWatch) end() {
	if w == nil {
		return
	}
	w.cancel()
	<-w.done
}
