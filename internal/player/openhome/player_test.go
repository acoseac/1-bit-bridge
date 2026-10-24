package openhome

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// The read actions' tests run the engine's real Run over fakeBackend, a
// backend that plays nothing and does what the test says: the backend's
// events (Details, Audible, Position) are the test's to send, and entries
// are made through the engine's own API. Every wait is on a signal the
// engine or the fake publishes, bounded by testWait; nothing sleeps.

// testWait bounds a wait for the engine or the fake.
const testWait = time.Minute

// playerUUID is the test engines' identity.
const playerUUID = "0f8fad5b-d9cb-469f-a165-70867728950e"

// fakeBackend is an engine.Backend that plays nothing: Start answers at
// once and keeps the session's Events, so the test reports what the
// hardware would; every other call answers at once (Cut true unless the
// test says it is too late, Resume continued), and Start, Pause, Resume and
// Release answer at once unless the test holds them.
type fakeBackend struct {
	mu       sync.Mutex
	sessions []*fakeSession
	changed  chan struct{}
	// hold, while set, holds every Release until it closes (holdReleases);
	// startHold every Start, pauseHold every Pause and resumeHold every
	// Resume, until they close or the call's context ends (holdStarts,
	// holdPauses, holdResumes).
	hold, startHold, pauseHold, resumeHold chan struct{}
	// refuseStart, when set, is what Start answers once it is let go: a
	// refusal of the device (busy) or an *engine.Unplayable.
	refuseStart func(engine.StartRequest) error
	// cutTooLate, while set, makes Cut answer false: the occurrence's
	// frames are the kernel's already.
	cutTooLate bool
	// begun counts the calls begun, by method (callsBegun).
	begun map[string]int
}

// fakeSession is one session the engine started.
type fakeSession struct {
	b     *fakeBackend
	id    engine.SessionID
	first engine.Occurrence
	ev    engine.Events
	// next is each Prepare's successor, in order (nil: nothing follows).
	next []*engine.Occurrence
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{changed: make(chan struct{}), begun: map[string]int{}}
}

// signal wakes every waiter; b.mu is held.
func (b *fakeBackend) signal() {
	close(b.changed)
	b.changed = make(chan struct{})
}

// call counts a call of op begun and returns the gate that holds it, if
// any; b.mu is held.
func (b *fakeBackend) call(op string, hold chan struct{}) chan struct{} {
	b.begun[op]++
	b.signal()
	return hold
}

// wait holds a call at its gate until the gate opens or ctx ends.
func wait(ctx context.Context, hold chan struct{}) error {
	if hold == nil {
		return nil
	}
	select {
	case <-hold:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *fakeBackend) Start(ctx context.Context, req engine.StartRequest) error {
	b.mu.Lock()
	b.sessions = append(b.sessions, &fakeSession{b: b, id: req.Session, first: req.First, ev: req.Events})
	hold, refuse := b.call("Start", b.startHold), b.refuseStart
	b.mu.Unlock()
	if err := wait(ctx, hold); err != nil {
		return err
	}
	if refuse != nil {
		return refuse(req)
	}
	return nil
}

func (b *fakeBackend) Prepare(_ context.Context, id engine.SessionID, _ engine.OccurrenceID, next *engine.Occurrence) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.begun["Prepare"]++
	for _, s := range b.sessions {
		if s.id == id {
			var n *engine.Occurrence
			if next != nil {
				cp := *next
				n = &cp
			}
			s.next = append(s.next, n)
		}
	}
	b.signal()
	return nil
}

func (b *fakeBackend) Cut(context.Context, engine.SessionID, engine.OccurrenceID) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.call("Cut", nil)
	return !b.cutTooLate, nil
}

func (b *fakeBackend) Pause(ctx context.Context, _ engine.SessionID) error {
	b.mu.Lock()
	hold := b.call("Pause", b.pauseHold)
	b.mu.Unlock()
	return wait(ctx, hold)
}

// holdStarts holds every Start from now until the returned function, which
// may be called again and runs at the test's end, lets them go.
func (b *fakeBackend) holdStarts(tb testing.TB) (letGo func()) {
	return b.holdCalls(tb, &b.startHold)
}

// holdPauses holds every Pause the same way, and holdResumes every Resume.
func (b *fakeBackend) holdPauses(tb testing.TB) (letGo func()) {
	return b.holdCalls(tb, &b.pauseHold)
}

func (b *fakeBackend) holdResumes(tb testing.TB) (letGo func()) {
	return b.holdCalls(tb, &b.resumeHold)
}

// holdCalls sets *slot to a gate, and returns what opens it.
func (b *fakeBackend) holdCalls(tb testing.TB, slot *chan struct{}) func() {
	g := make(chan struct{})
	b.mu.Lock()
	*slot = g
	b.mu.Unlock()
	letGo := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if *slot == g {
			*slot = nil
		}
		closeOnce(g)
	}
	tb.Cleanup(letGo)
	return letGo
}

// callsBegun waits until n calls of op (a Backend method's name) have
// begun.
func (b *fakeBackend) callsBegun(tb testing.TB, op string, n int) {
	tb.Helper()
	give := time.After(testWait)
	for {
		b.mu.Lock()
		got, ch := b.begun[op], b.changed
		b.mu.Unlock()
		if got >= n {
			return
		}
		select {
		case <-ch:
		case <-give:
			tb.Fatalf("waited %v for %d calls of %s, %d began", testWait, n, op, got)
		}
	}
}

func (b *fakeBackend) Resume(ctx context.Context, _ engine.SessionID) (engine.ResumeResult, error) {
	b.mu.Lock()
	hold := b.call("Resume", b.resumeHold)
	b.mu.Unlock()
	if err := wait(ctx, hold); err != nil {
		return 0, err
	}
	return engine.ResumeContinued, nil
}

func (b *fakeBackend) Release(context.Context, engine.SessionID) error {
	b.mu.Lock()
	hold := b.call("Release", b.hold)
	b.mu.Unlock()
	if hold != nil {
		<-hold
	}
	return nil
}

func (b *fakeBackend) Discard(context.Context) error { return nil }

// holdReleases holds every Release from now until the returned function,
// which may be called again, lets them go.
func (b *fakeBackend) holdReleases() (letGo func()) {
	g := make(chan struct{})
	b.mu.Lock()
	b.hold = g
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.hold == g {
			b.hold = nil
		}
		closeOnce(g)
	}
}

// letGoAll lets every held Release, Start and Pause go, for the engine's
// stop.
func (b *fakeBackend) letGoAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, hold := range []chan struct{}{b.hold, b.startHold, b.pauseHold, b.resumeHold} {
		closeOnce(hold)
	}
	b.hold, b.startHold, b.pauseHold, b.resumeHold = nil, nil, nil, nil
}

// closeOnce closes g unless it is nil or closed; the caller holds b.mu,
// under which every gate is closed.
func closeOnce(g chan struct{}) {
	if g == nil {
		return
	}
	select {
	case <-g:
	default:
		close(g)
	}
}

// sessionCount is how many sessions the engine has started.
func (b *fakeBackend) sessionCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.sessions)
}

// session waits for the n-th session (from 1) to start.
func (b *fakeBackend) session(tb testing.TB, n int) *fakeSession {
	tb.Helper()
	give := time.After(testWait)
	for {
		b.mu.Lock()
		if len(b.sessions) >= n {
			s := b.sessions[n-1]
			b.mu.Unlock()
			return s
		}
		ch := b.changed
		b.mu.Unlock()
		select {
		case <-ch:
		case <-give:
			tb.Fatalf("waited %v for session %d to start", testWait, n)
		}
	}
}

// successor waits for s's n-th Prepare (from 1) and returns its successor.
func (s *fakeSession) successor(tb testing.TB, n int) *engine.Occurrence {
	tb.Helper()
	give := time.After(testWait)
	for {
		s.b.mu.Lock()
		if len(s.next) >= n {
			next := s.next[n-1]
			s.b.mu.Unlock()
			return next
		}
		ch := s.b.changed
		s.b.mu.Unlock()
		select {
		case <-ch:
		case <-give:
			tb.Fatalf("waited %v for prepare %d in session %d", testWait, n, s.id)
		}
	}
}

// hear reports o's content heard, as the backend does: its Details first,
// when d is not nil, then Audible.
func (s *fakeSession) hear(o engine.OccurrenceID, d *engine.Details) {
	if d != nil {
		s.ev.(engine.DetailsEvents).Details(s.id, o, *d)
	}
	s.ev.Audible(s.id, o)
}

// position reports frames of o's content played at rate.
func (s *fakeSession) position(o engine.OccurrenceID, frames int64, rate int) {
	s.ev.Position(s.id, engine.Position{Occurrence: o, Frames: frames, RateHz: rate, Measured: time.Now()})
}

// testPlayer is a real engine over fakeBackend, on the test's own data
// directory, its output reported available.
type testPlayer struct {
	tb     testing.TB
	dir    string
	be     *fakeBackend
	eng    *engine.Engine
	outGen uint64
	// shut ends the engine's Run, leaving any held release held; stop ends
	// it, lets every release go and waits for Run.
	shut, stop func()
	// uuid is the engine's identity, and queue and reserve name its files in
	// dir: playerUUID and the harness's names (reserve "" for the engine's
	// default), or serve's beside player-uuid (servesPlayer).
	uuid, queue, reserve string
	// lib is the library an Insert through p's handler asks (testLibrary):
	// every track id a member until the test refuses one.
	lib *testLibrary
	// unreported leaves the output unreported at each start: unknown to the
	// engine, which refuses whatever would start a session.
	unreported bool
}

// newTestPlayer starts an engine. The engine keeps its IDs and tokens
// durable with a directory fsync, which Windows has not, on storage that
// outlives a reboot: a test directory that is not (a TMPDIR on tmpfs)
// skips, saying so, as the backend's rig does.
func newTestPlayer(tb testing.TB) *testPlayer {
	tb.Helper()
	return newTestPlayerWith(tb, func(*testPlayer) {})
}

// newTestPlayerWith is newTestPlayer with set applied to the player before
// its engine starts.
func newTestPlayerWith(tb testing.TB, set func(*testPlayer)) *testPlayer {
	tb.Helper()
	p, err := tryTestPlayerWith(tb, set)
	if err != nil {
		tb.Skipf("the engine does not start on this test's data directory: %v", err)
	}
	return p
}

// tryTestPlayer is newTestPlayer, or why the engine does not start.
func tryTestPlayer(tb testing.TB) (*testPlayer, error) {
	tb.Helper()
	return tryTestPlayerWith(tb, func(*testPlayer) {})
}

func tryTestPlayerWith(tb testing.TB, set func(*testPlayer)) (*testPlayer, error) {
	tb.Helper()
	if runtime.GOOS == "windows" {
		return nil, errors.New("the engine keeps its IDs durable with a directory fsync, which Windows has not: the player runs on Linux")
	}
	p := &testPlayer{tb: tb, dir: filepath.Join(tb.TempDir(), "data"), uuid: playerUUID, queue: "queue.json", lib: newTestLibrary()}
	set(p)
	if err := os.MkdirAll(filepath.Dir(filepath.Join(p.dir, p.queue)), 0o700); err != nil {
		tb.Fatal(err)
	}
	if err := p.start(); err != nil {
		return nil, err
	}
	return p, nil
}

// start builds and runs an engine on p's directory, or says why New
// refused it.
func (p *testPlayer) start() error {
	p.tb.Helper()
	p.be = newFakeBackend()
	reserve := ""
	if p.reserve != "" {
		reserve = filepath.Join(p.dir, p.reserve)
	}
	eng, err := engine.New(engine.Config{PlayerUUID: p.uuid, Backend: p.be, QueuePath: filepath.Join(p.dir, p.queue), ReservePath: reserve, DataDir: p.dir})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan struct{})
	go func() {
		defer close(ran)
		_ = eng.Run(ctx)
	}()
	var once sync.Once
	be, tb := p.be, p.tb
	p.stop = func() {
		once.Do(func() {
			be.letGoAll()
			cancel()
			select {
			case <-ran:
			case <-time.After(testWait):
				tb.Errorf("the engine's Run did not return within %v", testWait)
			}
		})
	}
	p.tb.Cleanup(p.stop)
	p.eng, p.outGen, p.shut = eng, 0, cancel
	if !p.unreported {
		p.available()
	}
	return nil
}

// restart stops the engine and starts another on the same directory.
func (p *testPlayer) restart() {
	p.tb.Helper()
	p.stop()
	if err := p.start(); err != nil {
		p.tb.Fatalf("the engine does not start again: %v", err)
	}
}

// available reports the output available, numbering the report.
func (p *testPlayer) available() {
	p.tb.Helper()
	p.outGen++
	if err := p.eng.ReportOutput(context.Background(), engine.OutputReport{Generation: p.outGen, Available: true}); err != nil {
		p.tb.Fatal(err)
	}
}

// add inserts an entry for ref with meta after the last one and returns its
// ID.
func (p *testPlayer) add(ref string, meta []byte) uint32 {
	p.tb.Helper()
	r, err := engine.NewReference(ref)
	if err != nil {
		p.tb.Fatal(err)
	}
	after := uint32(0)
	if ids := p.eng.Snapshot().IDs(); len(ids) > 0 {
		after = ids[len(ids)-1]
	}
	id, err := p.eng.InsertAfter(context.Background(), after, r, meta)
	if err != nil {
		p.tb.Fatalf("insert %s: %v", ref, err)
	}
	return id
}

// waitSnap waits until cond holds of the engine's snapshot.
func (p *testPlayer) waitSnap(what string, cond func(*engine.Snapshot) bool) *engine.Snapshot {
	p.tb.Helper()
	give := time.After(testWait)
	for {
		ch := p.eng.Changed()
		if s := p.eng.Snapshot(); cond(s) {
			return s
		}
		select {
		case <-ch:
		case <-give:
			s := p.eng.Snapshot()
			p.tb.Fatalf("waited %v for %s; the engine is %s, selected %d", testWait, what, s.State(), s.Selected())
		}
	}
}

// heardNow waits until the engine hears occurrence o.
func (p *testPlayer) heardNow(o engine.OccurrenceID) *engine.Snapshot {
	p.tb.Helper()
	return p.waitSnap("the occurrence heard", func(s *engine.Snapshot) bool {
		tr, ok := s.Track()
		return ok && tr.Occurrence == o
	})
}

// must fails the test on err.
func must(tb testing.TB, what string, err error) {
	tb.Helper()
	if err != nil {
		tb.Fatalf("%s: %v", what, err)
	}
}

// handler is a handler over p's engine, the device's room room, its Inserts
// confined to p's library and the test's hosts (testHosts).
func (p *testPlayer) handler(room string) *handler {
	return p.handlerWith(room, p.eng, DefaultLimits.orDefaults())
}

// handlerWith is handler over eng (p's engine, or one wrapping it) and lim.
func (p *testPlayer) handlerWith(room string, eng Engine, lim Limits) *handler {
	h := newHandler(Device{UUID: testUUID, Room: room, ModelNumber: "test"}, eng, lim, nil)
	h.library, h.hosts = p.lib, testHosts
	return h
}

// result is a control request's answer, as a controller reads it: the
// HTTP status, a fault's UPnP code and description, and a response's
// out-arguments, by name and in order.
type result struct {
	status      int
	fault       int
	description string
	names       []string
	values      map[string]string
	raw         []byte
}

// invoke sends service s's action with args (name, value, name, value…),
// each value escaped as a controller escapes it, through h, and reads the
// answer. It checks what every answer carries (the EXT header; for a
// response, its Content-Type and a Content-Length that is the body's), and
// that a response is the action's: one Envelope, one Body, one
// <action>Response in the service's namespace whose children are the
// action's out-arguments, in the SCPD's order, none nested.
func invoke(tb testing.TB, h *handler, s *service, action string, args ...string) result {
	tb.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, actionRequest(s, action, args...))
	return readResult(tb, rec.Result(), s, action)
}

// actionRequest is the control request of s's action with args (name, value,
// name, value…), each value escaped as a controller escapes it.
func actionRequest(s *service, action string, args ...string) *http.Request {
	var b strings.Builder
	for i := 0; i+1 < len(args); i += 2 {
		b.WriteString("<" + args[i] + ">" + escape(args[i+1]) + "</" + args[i] + ">")
	}
	return soapRequest(s, header(s, action), `text/xml; charset="utf-8"`, envelope(call(s, action, b.String())))
}

// sent is a request sent through a handler on a goroutine of its own, whose
// answer the test reads on its own goroutine (answer).
type sent struct {
	s      *service
	action string
	res    chan *http.Response
}

// send sends s's action with args through h on a goroutine of its own.
func send(h *handler, s *service, action string, args ...string) sent {
	return sendRequest(h, s, action, actionRequest(s, action, args...))
}

// sendRequest sends r, a request of s's action, through h on a goroutine of
// its own.
func sendRequest(h *handler, s *service, action string, r *http.Request) sent {
	c := sent{s: s, action: action, res: make(chan *http.Response, 1)}
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		c.res <- rec.Result()
	}()
	return c
}

// answer waits for the request's answer and reads it as invoke does.
func (c sent) answer(tb testing.TB) result {
	tb.Helper()
	select {
	case res := <-c.res:
		return readResult(tb, res, c.s, c.action)
	case <-time.After(testWait):
		tb.Fatalf("waited %v for %s#%s to answer", testWait, c.s.name, c.action)
		return result{}
	}
}

// pending reports whether the request has not been answered yet.
func (c sent) pending() bool { return len(c.res) == 0 }

// readResult reads res, an answer to s's action, as invoke says.
func readResult(tb testing.TB, res *http.Response, s *service, action string) result {
	tb.Helper()
	raw, _ := io.ReadAll(res.Body)
	out := result{status: res.StatusCode, raw: raw, values: map[string]string{}}
	// The handler writes EXT as UDA does; a client reading it from the wire
	// keeps it canonical, Ext.
	v, ok := res.Header["EXT"]
	if !ok {
		v, ok = res.Header["Ext"]
	}
	if !ok || len(v) != 1 || v[0] != "" {
		tb.Errorf("%s#%s: the EXT header is %q", s.name, action, v)
	}
	switch res.StatusCode {
	case http.StatusInternalServerError:
		var f soapFaultDoc
		if err := xml.Unmarshal(raw, &f); err != nil || f.Fault.Detail == nil {
			tb.Fatalf("%s#%s: a 500 that is no UPnP fault: %v\n%s", s.name, action, err, raw)
		}
		if !knownFault(f.Fault.Detail.Code, f.Fault.Detail.Description) {
			tb.Errorf("%s#%s: fault %d, %q, which this layer does not define", s.name, action, f.Fault.Detail.Code, f.Fault.Detail.Description)
		}
		out.fault, out.description = f.Fault.Detail.Code, f.Fault.Detail.Description
		return out
	case http.StatusOK:
	default:
		return out
	}
	if ct := res.Header.Get("Content-Type"); ct != `text/xml; charset="utf-8"` {
		tb.Errorf("%s#%s: Content-Type %q", s.name, action, ct)
	}
	if cl := res.Header.Get("Content-Length"); cl != strconv.Itoa(len(raw)) {
		tb.Errorf("%s#%s: Content-Length %q for a body of %d bytes", s.name, action, cl, len(raw))
	}
	out.names, out.values = parseResponse(tb, raw, s, action)
	var want []string
	for _, g := range s.action(action).outs() {
		want = append(want, g.name)
	}
	if strings.Join(out.names, " ") != strings.Join(want, " ") {
		tb.Errorf("%s#%s: the response carries %v, want the out-arguments %v", s.name, action, out.names, want)
	}
	return out
}

// parseResponse reads a SOAP response to s's action as a controller does,
// strictly, and returns its arguments' names, in order, and their values.
func parseResponse(tb testing.TB, raw []byte, s *service, action string) ([]string, map[string]string) {
	tb.Helper()
	d := xml.NewDecoder(bytes.NewReader(raw))
	d.Strict = true
	var path []xml.Name
	var names []string
	values := map[string]string{}
	var text strings.Builder
	want := []xml.Name{{Space: soapEnvelopeNS, Local: "Envelope"}, {Space: soapEnvelopeNS, Local: "Body"}, {Space: s.serviceType(), Local: action + "Response"}}
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			tb.Fatalf("%s#%s: the response does not parse: %v\n%s", s.name, action, err, raw)
		}
		switch tk := tok.(type) {
		case xml.StartElement:
			path = append(path, tk.Name)
			switch n := len(path); {
			case n <= 3:
				if tk.Name != want[n-1] {
					tb.Fatalf("%s#%s: element %v where %v belongs\n%s", s.name, action, tk.Name, want[n-1], raw)
				}
			case n == 4:
				if tk.Name.Space != "" {
					tb.Errorf("%s#%s: the argument %s is in the namespace %q", s.name, action, tk.Name.Local, tk.Name.Space)
				}
				names = append(names, tk.Name.Local)
				text.Reset()
			default:
				tb.Fatalf("%s#%s: an element inside an argument\n%s", s.name, action, raw)
			}
		case xml.CharData:
			if len(path) == 4 {
				text.Write(tk)
			}
		case xml.EndElement:
			if len(path) == 4 {
				values[names[len(names)-1]] = text.String()
			}
			path = path[:len(path)-1]
		}
	}
	return names, values
}

// read is invoke for a read that must answer: it fails the test unless the
// answer is a response, and returns its values.
func read(tb testing.TB, h *handler, s *service, action string, args ...string) map[string]string {
	tb.Helper()
	r := invoke(tb, h, s, action, args...)
	if r.status != http.StatusOK {
		tb.Fatalf("%s#%s: %d, fault %d, want a response\n%s", s.name, action, r.status, r.fault, r.raw)
	}
	return r.values
}
