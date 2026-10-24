package openhome

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/netip"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/trackref"
)

// The track ids of the writes' tests: goldenID is Artist/Album (2020)/01
// Track One.flac's, the first 8 bytes of SHA-256("\x00" + that path), computed
// outside Go; the others name tracks of the test library alike.
const (
	goldenID = "b5b4c8dc9764772e"
	trackB   = "1111111111111111"
	trackC   = "2222222222222222"
)

// uriOf is the URI 1-bit inserts for the track id on the test's bridge, at
// its LAN address and the DLNA port.
func uriOf(id string) string { return "http://192.168.1.5:7790/dlna/file/" + id }

// testHosts is the test bridge's inventory: health advertises its LAN
// address, a ULA, its .local name and a custom endpoint's name, and a tailnet
// address and name (which no URI may name); the DLNA listener serves its LAN
// address and a second interface's; tracks are served on 7790 and 7800.
var testHosts = Inventory{
	Advertised: func() []string {
		return []string{"https://192.168.1.5:7788", "https://[fd00::5]:7788", "https://bridge.local:7788",
			"https://music.example.com", "https://100.101.102.103:7788", "https://bridge.tail1234.ts.net:7788"}
	},
	Served: func(a netip.Addr) bool {
		return a == netip.MustParseAddr("192.168.1.5") || a == netip.MustParseAddr("10.0.0.5")
	},
	Ports: []uint16{DefaultTrackPort, 7800},
}

var _ Hosts = testHosts

// testLibrary is a test's library: every track id a member, but those the
// test refuses, each with its refusal; and the ids it was asked.
type testLibrary struct {
	mu      sync.Mutex
	refused map[string]error
	asked   []string
}

func newTestLibrary() *testLibrary { return &testLibrary{refused: map[string]error{}} }

func (l *testLibrary) Member(_ context.Context, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.asked = append(l.asked, id)
	if !trackref.IsID(id) {
		return &engine.Unplayable{Reason: "not_indexed"}
	}
	return l.refused[id]
}

// refuse makes id refused with err from now on.
func (l *testLibrary) refuse(id string, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refused[id] = err
}

// watchedCtx is a command's context that tells the test when the engine's
// owner has admitted the command (admitted closes): the engine's do asks Err
// once before it hands a command over, and its owner once more as it admits
// it, in the step that runs or parks it, and nothing asks it in between. A
// command sent after admitted closes reaches the owner after that step.
type watchedCtx struct {
	context.Context
	asks     atomic.Int32
	admitted chan struct{}
}

func (c *watchedCtx) Err() error {
	if c.asks.Add(1) == 2 {
		close(c.admitted)
	}
	return c.Context.Err()
}

// spyEngine is the test player's engine as a handler sees it: every command
// is the engine's, its answer recorded by the action's engine command; a
// command whose op the test watches gets a watchedCtx, handed to the test;
// and an InsertAfter runs beforeInsert first, when set, which answers it in
// the engine's place when it returns an error.
type spyEngine struct {
	Engine
	mu           sync.Mutex
	answers      map[string][]error
	watched      map[string]chan *watchedCtx
	beforeInsert func(context.Context) error
}

func newSpy(e Engine) *spyEngine {
	return &spyEngine{Engine: e, answers: map[string][]error{}, watched: map[string]chan *watchedCtx{}}
}

// watch hands the test each command of op's context, watched, from now on.
func (e *spyEngine) watch(op string) <-chan *watchedCtx {
	e.mu.Lock()
	defer e.mu.Unlock()
	ch := make(chan *watchedCtx, 16)
	e.watched[op] = ch
	return ch
}

// admitted waits for the next command of a watched op to be admitted.
func admitted(tb testing.TB, ch <-chan *watchedCtx) {
	tb.Helper()
	give := time.After(testWait)
	select {
	case w := <-ch:
		select {
		case <-w.admitted:
		case <-give:
			tb.Fatalf("waited %v for the engine to admit a command", testWait)
		}
	case <-give:
		tb.Fatalf("waited %v for the handler to call the engine", testWait)
	}
}

func (e *spyEngine) ctx(op string, ctx context.Context) context.Context {
	e.mu.Lock()
	ch := e.watched[op]
	e.mu.Unlock()
	if ch == nil {
		return ctx
	}
	w := &watchedCtx{Context: ctx, admitted: make(chan struct{})}
	ch <- w
	return w
}

func (e *spyEngine) note(op string, err error) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.answers[op] = append(e.answers[op], err)
	return err
}

// answered is every answer the engine gave op's commands, in order.
func (e *spyEngine) answered(op string) []error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.answers[op])
}

func (e *spyEngine) Play(ctx context.Context, c ...engine.Cond) error {
	return e.note("Play", e.Engine.Play(e.ctx("Play", ctx), c...))
}
func (e *spyEngine) Pause(ctx context.Context, c ...engine.Cond) error {
	return e.note("Pause", e.Engine.Pause(e.ctx("Pause", ctx), c...))
}
func (e *spyEngine) Stop(ctx context.Context, c ...engine.Cond) error {
	return e.note("Stop", e.Engine.Stop(e.ctx("Stop", ctx), c...))
}
func (e *spyEngine) PlayID(ctx context.Context, id uint32, c ...engine.Cond) error {
	return e.note("PlayID", e.Engine.PlayID(e.ctx("PlayID", ctx), id, c...))
}
func (e *spyEngine) InsertAfter(ctx context.Context, after uint32, ref engine.Reference, meta []byte, c ...engine.Cond) (uint32, error) {
	if e.beforeInsert != nil {
		if err := e.beforeInsert(ctx); err != nil {
			return 0, e.note("InsertAfter", err)
		}
	}
	id, err := e.Engine.InsertAfter(e.ctx("InsertAfter", ctx), after, ref, meta, c...)
	return id, e.note("InsertAfter", err)
}
func (e *spyEngine) Delete(ctx context.Context, id uint32, c ...engine.Cond) error {
	return e.note("Delete", e.Engine.Delete(e.ctx("Delete", ctx), id, c...))
}
func (e *spyEngine) DeleteAll(ctx context.Context, c ...engine.Cond) error {
	return e.note("DeleteAll", e.Engine.DeleteAll(e.ctx("DeleteAll", ctx), c...))
}

// soapFault requires s's action through h to be a fault, and returns it.
func soapFault(tb testing.TB, h *handler, s *service, action string, args ...string) result {
	tb.Helper()
	r := invoke(tb, h, s, action, args...)
	if r.status != http.StatusInternalServerError {
		tb.Fatalf("%s#%s: %d, want a fault\n%s", s.name, action, r.status, r.raw)
	}
	return r
}

// newID is an Insert's NewId through h.
func newID(tb testing.TB, h *handler, after uint32, uri, meta string) uint32 {
	tb.Helper()
	return ui4Value(tb, "NewId", read(tb, h, playlist, "Insert", "AfterId", u(after), "Uri", uri, "Metadata", meta)["NewId"])
}

// queueOf is the queue's IDs as a controller reads them, from IdArray.
func queueOf(t *testing.T, h *handler) []uint32 {
	t.Helper()
	return decodeIDArray(t, read(t, h, playlist, "IdArray")["Array"])
}

// TestEachWriteIsItsEngineCommand pins every write action against a real
// engine through the SOAP layer: its success and what a read then shows, and
// each of its refusals with its code. Insert takes AfterId (0 is the head),
// answers NewId, and Read gives the URI back byte for byte; an AfterId the
// queue does not hold is 800; DeleteId and DeleteAll edit, an ID not held 800;
// SetRepeat sets Repeat; SetShuffle(0) succeeds and SetShuffle(1) is 804
// (shuffle is release 7a's), Shuffle staying 0; Play, Pause, Stop, Next,
// Previous, SeekId and SeekIndex are the engine's transport, each starting the
// session the engine starts (SeekId of an ID not held 800, SeekIndex past the
// queue 802); the seeks within a track are 803, changing nothing; and Product's
// writes: SetStandby(0) succeeds and SetStandby(1) is 899, Standby staying 0
// and the transport as it was (no Stop is made of it); SetSourceIndex(0) and
// SetSourceIndexByName(Playlist) succeed, and any other index or name is 801,
// Source not found.
func TestEachWriteIsItsEngineCommand(t *testing.T) {
	p := newTestPlayer(t)
	h := p.handler("")
	a := newID(t, h, 0, uriOf(goldenID), "<DIDL-Lite>a</DIDL-Lite>")
	c := newID(t, h, a, uriOf(trackC), "")
	b := newID(t, h, a, uriOf(trackB), "meta b")
	if got := queueOf(t, h); !slices.Equal(got, []uint32{a, b, c}) {
		t.Fatalf("the queue %v, want a, b after a, c", got)
	}
	if got := read(t, h, playlist, "Read", "Id", u(b)); got["Uri"] != uriOf(trackB) || got["Metadata"] != "meta b" {
		t.Errorf("Read(b): %v, want the URI and metadata inserted", got)
	}
	if got := read(t, h, playlist, "Id")["Value"]; got != u(a) {
		t.Errorf("Id %s, want a, inserted into the empty queue", got)
	}
	if r := soapFault(t, h, playlist, "Insert", "AfterId", "999999", "Uri", uriOf(goldenID), "Metadata", ""); r.fault != 800 || r.description != "Id not found" {
		t.Errorf("Insert after an ID not held: %d %q, want 800 \"Id not found\"", r.fault, r.description)
	}

	read(t, h, playlist, "SetRepeat", "Value", "1")
	if got := read(t, h, playlist, "Repeat")["Value"]; got != "1" {
		t.Errorf("Repeat after SetRepeat(1): %s", got)
	}
	read(t, h, playlist, "SetRepeat", "Value", "false")
	if got := read(t, h, playlist, "Repeat")["Value"]; got != "0" {
		t.Errorf("Repeat after SetRepeat(false): %s", got)
	}
	read(t, h, playlist, "SetShuffle", "Value", "0")
	if r := soapFault(t, h, playlist, "SetShuffle", "Value", "1"); r.fault != 804 || r.description != "Shuffle not currently possible" {
		t.Errorf("SetShuffle(1): %d %q, want 804", r.fault, r.description)
	}
	if got := read(t, h, playlist, "Shuffle")["Value"]; got != "0" {
		t.Errorf("Shuffle after SetShuffle(1): %s", got)
	}

	state := func(want, what string) {
		t.Helper()
		if got := read(t, h, playlist, "TransportState")["Value"]; got != want {
			t.Errorf("%s: TransportState %s, want %s", what, got, want)
		}
	}
	started := 0
	plays := func(what string, entry uint32) *fakeSession {
		t.Helper()
		started++
		s := p.be.session(t, started)
		if s.first.Entry != entry {
			t.Fatalf("%s: session %d started for %d, want %d", what, started, s.first.Entry, entry)
		}
		if got := read(t, h, playlist, "Id")["Value"]; got != u(entry) {
			t.Errorf("%s: Id %s, want %d", what, got, entry)
		}
		s.hear(s.first.ID, nil)
		p.heardNow(s.first.ID)
		return s
	}
	read(t, h, playlist, "Play")
	plays("Play", a)
	state("Playing", "Play")
	read(t, h, playlist, "Pause")
	state("Paused", "Pause")
	read(t, h, playlist, "Play")
	state("Playing", "Play after Pause")
	read(t, h, playlist, "Next")
	plays("Next", b)
	read(t, h, playlist, "Previous")
	plays("Previous", a)
	read(t, h, playlist, "SeekId", "Value", u(c))
	plays("SeekId", c)
	if r := soapFault(t, h, playlist, "SeekId", "Value", "999999"); r.fault != 800 {
		t.Errorf("SeekId of an ID not held: %d, want 800", r.fault)
	}
	read(t, h, playlist, "SeekIndex", "Value", "1")
	plays("SeekIndex", b)
	if r := soapFault(t, h, playlist, "SeekIndex", "Value", "3"); r.fault != 802 || r.description != "Index not found" {
		t.Errorf("SeekIndex past the queue: %d %q, want 802", r.fault, r.description)
	}
	before := p.eng.Snapshot().Generation()
	for _, seek := range []struct{ action, value string }{{"SeekSecondAbsolute", "30"}, {"SeekSecondRelative", "-5"}} {
		if r := soapFault(t, h, playlist, seek.action, "Value", seek.value); r.fault != 803 || r.description != "Seek failed" {
			t.Errorf("%s: %d %q, want 803", seek.action, r.fault, r.description)
		}
	}
	if p.eng.Snapshot().Generation() != before {
		t.Errorf("a seek within the track changed the engine")
	}
	read(t, h, playlist, "Stop")
	state("Stopped", "Stop")

	read(t, h, playlist, "DeleteId", "Value", u(b))
	if got := queueOf(t, h); !slices.Equal(got, []uint32{a, c}) {
		t.Errorf("after DeleteId(b): %v", got)
	}
	if r := soapFault(t, h, playlist, "DeleteId", "Value", u(b)); r.fault != 800 {
		t.Errorf("DeleteId of an ID gone: %d, want 800", r.fault)
	}
	read(t, h, playlist, "DeleteAll")
	if got := queueOf(t, h); len(got) != 0 || read(t, h, playlist, "Id")["Value"] != "0" {
		t.Errorf("after DeleteAll: %v", got)
	}

	read(t, h, product, "SetStandby", "Value", "0")
	before = p.eng.Snapshot().Generation()
	if r := soapFault(t, h, product, "SetStandby", "Value", "1"); r.fault != 899 || r.description != "Wip - action not (yet) implemented" {
		t.Errorf("SetStandby(1): %d %q, want 899", r.fault, r.description)
	}
	if got := read(t, h, product, "Standby")["Value"]; got != "0" || p.eng.Snapshot().Generation() != before {
		t.Errorf("after SetStandby(1): Standby %s, the engine moved %v", got, p.eng.Snapshot().Generation() != before)
	}
	read(t, h, product, "SetSourceIndex", "Value", "0")
	read(t, h, product, "SetSourceIndexByName", "Value", "Playlist")
	for _, tc := range [][]string{{"SetSourceIndex", "1"}, {"SetSourceIndex", "4294967295"}, {"SetSourceIndexByName", "Radio"}, {"SetSourceIndexByName", "playlist"}} {
		if r := soapFault(t, h, product, tc[0], "Value", tc[1]); r.fault != 801 || r.description != "Source not found" {
			t.Errorf("%s(%s): %d %q, want 801 \"Source not found\"", tc[0], tc[1], r.fault, r.description)
		}
	}
}

// commandless is the test player's engine for the writes that make no engine
// command: its snapshot and Done are the engine's, and every command it is
// asked fails the test.
type commandless struct {
	Engine
	tb testing.TB
}

func (e commandless) called(op string) error {
	e.tb.Errorf("%s reached the engine", op)
	return nil
}
func (e commandless) Play(context.Context, ...engine.Cond) error     { return e.called("Play") }
func (e commandless) Pause(context.Context, ...engine.Cond) error    { return e.called("Pause") }
func (e commandless) Stop(context.Context, ...engine.Cond) error     { return e.called("Stop") }
func (e commandless) Next(context.Context, ...engine.Cond) error     { return e.called("Next") }
func (e commandless) Previous(context.Context, ...engine.Cond) error { return e.called("Previous") }
func (e commandless) PlayID(context.Context, uint32, ...engine.Cond) error {
	return e.called("PlayID")
}
func (e commandless) PlayIndex(context.Context, int, ...engine.Cond) error {
	return e.called("PlayIndex")
}
func (e commandless) InsertAfter(context.Context, uint32, engine.Reference, []byte, ...engine.Cond) (uint32, error) {
	return 0, e.called("InsertAfter")
}
func (e commandless) Delete(context.Context, uint32, ...engine.Cond) error {
	return e.called("Delete")
}
func (e commandless) DeleteAll(context.Context, ...engine.Cond) error { return e.called("DeleteAll") }
func (e commandless) SetRepeat(context.Context, bool, ...engine.Cond) error {
	return e.called("SetRepeat")
}
func (e commandless) SetShuffle(context.Context, bool, ...engine.Cond) error {
	return e.called("SetShuffle")
}

// TestTheWritesThatNeedNoCommandMakeNone pins the writes the engine is not
// asked for: SetShuffle(1), 804; the seeks within a track, 803; and Product's
// writes, SetStandby(0) and (1), SetSourceIndex and SetSourceIndexByName, a
// success or their fault, answered without a command, so no budget applies
// and nothing of the engine moves; and an Insert refused by its confinement
// or its metadata never reaches it either.
func TestTheWritesThatNeedNoCommandMakeNone(t *testing.T) {
	p := newTestPlayer(t)
	h := p.handlerWith("", commandless{Engine: p.eng, tb: t}, DefaultLimits.orDefaults())
	for _, c := range []struct {
		s     *service
		args  []string
		fault int
	}{
		{playlist, []string{"SetShuffle", "Value", "1"}, 804},
		{playlist, []string{"SeekSecondAbsolute", "Value", "10"}, 803},
		{playlist, []string{"SeekSecondRelative", "Value", "-10"}, 803},
		{product, []string{"SetStandby", "Value", "0"}, 0},
		{product, []string{"SetStandby", "Value", "1"}, 899},
		{product, []string{"SetSourceIndex", "Value", "0"}, 0},
		{product, []string{"SetSourceIndex", "Value", "2"}, 801},
		{product, []string{"SetSourceIndexByName", "Value", "Playlist"}, 0},
		{product, []string{"SetSourceIndexByName", "Value", "Radio"}, 801},
		{playlist, []string{"Insert", "AfterId", "0", "Uri", "http://127.0.0.1:7790/dlna/file/" + goldenID, "Metadata", ""}, 600},
		{playlist, []string{"Insert", "AfterId", "0", "Uri", uriOf(goldenID), "Metadata", strings.Repeat("m", engine.MaxMetadataBytes+1)}, 605},
	} {
		r := invoke(t, h, c.s, c.args[0], c.args[1:]...)
		if r.fault != c.fault || (c.fault == 0) != (r.status == http.StatusOK) {
			t.Errorf("%s#%s(%v): %d, fault %d, want fault %d", c.s.name, c.args[0], c.args[1:], r.status, r.fault, c.fault)
		}
	}
}

// zeroID is the test player's engine with an InsertAfter that answers no ID
// and no error, which the engine never does.
type zeroID struct{ Engine }

func (zeroID) InsertAfter(context.Context, uint32, engine.Reference, []byte, ...engine.Cond) (uint32, error) {
	return 0, nil
}

// TestAnInsertIsNeverAnsweredWithoutAnID pins that an Insert answers success
// only with a NewId above 0: an engine answer of no ID and no error, which
// the engine never gives, is 501, so a controller never reads 0, which 1-bit
// takes for no insert, as an entry.
func TestAnInsertIsNeverAnsweredWithoutAnID(t *testing.T) {
	p := newTestPlayer(t)
	h := p.handlerWith("", zeroID{p.eng}, DefaultLimits.orDefaults())
	if r := soapFault(t, h, playlist, "Insert", "AfterId", "0", "Uri", uriOf(goldenID), "Metadata", ""); r.fault != errActionFailed {
		t.Errorf("an Insert the engine answered with no ID: fault %d, want 501", r.fault)
	}
}

// TestTheEnginesCapacityIsPlaylistFull is a synthetic test of the engine's
// capacity, which 1-bit never reaches (it holds at most 500 entries on a
// player): the 1000th entry is taken, and an Insert past it is 801, "Playlist
// full", Playlist's own description, which 1-bit reads as a full player.
func TestTheEnginesCapacityIsPlaylistFull(t *testing.T) {
	p := newTestPlayer(t)
	h := p.handler("")
	for i := 1; i < engine.MaxEntries; i++ {
		p.add(uriOf(goldenID), nil)
	}
	last := newID(t, h, 0, uriOf(goldenID), "")
	if last == 0 || len(queueOf(t, h)) != engine.MaxEntries {
		t.Fatalf("the %dth entry: %d, the queue %d long", engine.MaxEntries, last, len(queueOf(t, h)))
	}
	if r := soapFault(t, h, playlist, "Insert", "AfterId", "0", "Uri", uriOf(goldenID), "Metadata", ""); r.fault != 801 || r.description != "Playlist full" {
		t.Errorf("an Insert past %d entries: %d %q, want 801 \"Playlist full\"", engine.MaxEntries, r.fault, r.description)
	}
}

// reasonsOf is every constant of type Reason (engine's) or engine.Reason
// declared in the Go file at path, read from its source: so a reason added
// there is in the fault table's test the day it is written.
func reasonsOf(t *testing.T, path string) map[string]engine.Reason {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]engine.Reason{}
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, spec := range g.Specs {
			vs := spec.(*ast.ValueSpec)
			typed := false
			switch tt := vs.Type.(type) {
			case *ast.Ident:
				typed = tt.Name == "Reason"
			case *ast.SelectorExpr:
				typed = tt.Sel.Name == "Reason"
			}
			if !typed {
				continue
			}
			for i, name := range vs.Names {
				lit := vs.Values[i].(*ast.BasicLit)
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				out[name.Name] = engine.Reason(v)
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s declares no reason: the scan is broken", path)
	}
	return out
}

// TestEveryEngineReasonHasItsFault pins the fault table over every write of
// Playlist's and every reason: every reason the engine's errors.go names has
// a row; ohPipeline's codes are answered where it has the condition, and only
// on the action that has it (800 Id not found wherever an ID is named; 801
// Playlist full on Insert alone, so 1-bit never takes another refusal for a
// full player and plays what is in; 802 on SeekIndex; 804 on SetShuffle), a
// metadata too large is 605, String Argument Too Long, and every other reason
// is 501, Action Failed, the resolver's and the backend's own among them
// (internal/player/library's and playback's, read from their sources) and one
// nobody wrote; and the two 801s are each their service's.
func TestEveryEngineReasonHasItsFault(t *testing.T) {
	var reasons []engine.Reason
	for name, reason := range reasonsOf(t, filepath.Join("..", "engine", "errors.go")) {
		if _, ok := reasonFaults[reason]; !ok {
			t.Errorf("engine.%s (%s) has no row in the fault table", name, reason)
		}
		reasons = append(reasons, reason)
	}
	for _, src := range []string{filepath.Join("..", "library", "library.go"), filepath.Join("..", "playback", "open.go")} {
		for name, reason := range reasonsOf(t, src) {
			if _, ok := reasonFaults[reason]; ok {
				t.Errorf("%s's %s (%s) has a row of the engine's", src, name, reason)
			}
			reasons = append(reasons, reason)
		}
	}
	reasons = append(reasons, "a_reason_no_one_wrote")
	for reason, f := range reasonFaults {
		if !knownFault(f.code, f.text()) {
			t.Errorf("%s answers %d %q, a fault this layer does not define", reason, f.code, f.text())
		}
	}
	for _, name := range writeActions["Playlist"] {
		a := playlist.action(name)
		for _, reason := range reasons {
			want := faultReply{code: errActionFailed}
			switch {
			case reason == engine.ReasonIDNotFound:
				want = faultIDNotFound
			case reason == engine.ReasonMetadataTooLarge:
				want = faultReply{code: errStringTooLong}
			case reason == engine.ReasonFull && name == "Insert":
				want = faultPlaylistFull
			case reason == engine.ReasonIndexNotFound && name == "SeekIndex":
				want = faultIndexNotFound
			case reason == engine.ReasonNotInRelease && name == "SetShuffle":
				want = faultShuffle
			}
			if got := faultOf(a, reason); got != want {
				t.Errorf("%s refused %s: %d %q, want %d %q", name, reason, got.code, got.text(), want.code, want.text())
			}
		}
	}
	if d := faultEnvelope(faultPlaylistFull); !bytes.Contains(d, []byte("<errorCode>801</errorCode><errorDescription>Playlist full</errorDescription>")) {
		t.Errorf("Playlist's 801 reads %s", d)
	}
	if d := faultEnvelope(faultSourceNotFound); !bytes.Contains(d, []byte("<errorCode>801</errorCode><errorDescription>Source not found</errorDescription>")) {
		t.Errorf("Product's 801 reads %s", d)
	}
}

// TestATransportWritesAnswerIsTheEnginesAdmission pins transportFault: nil and
// a superseded playback are success; a cancellation of a command the engine
// admitted (it may have run) is success, and one it never admitted (wrapping
// engine.ErrNotAdmitted) is 501; the engine closing is 5a3's no-engine fault,
// 501; every other refusal its row.
func TestATransportWritesAnswerIsTheEnginesAdmission(t *testing.T) {
	play := playlist.action("Play")
	for _, tc := range []struct {
		what string
		err  error
		want *faultReply
	}{
		{"nil", nil, nil},
		{"superseded", &engine.Error{Reason: engine.ReasonSuperseded}, nil},
		{"a cancellation once admitted", &engine.Error{Reason: engine.ReasonCancelled, Err: context.DeadlineExceeded}, nil},
		{"a cancellation never admitted", &engine.Error{Reason: engine.ReasonCancelled, Err: fmt.Errorf("%w: %w", engine.ErrNotAdmitted, context.DeadlineExceeded)}, &faultReply{code: errActionFailed}},
		{"closed", &engine.Error{Reason: engine.ReasonClosed}, &faultReply{code: errActionFailed}},
		{"overtaken", &engine.Error{Reason: engine.ReasonOvertaken}, &faultReply{code: errActionFailed}},
		{"all unplayable", &engine.Error{Reason: engine.ReasonAllUnplayable}, &faultReply{code: errActionFailed}},
		{"an error with no reason", errors.New("no reason"), &faultReply{code: errActionFailed}},
	} {
		got := transportFault(play, tc.err)
		if (got == nil) != (tc.want == nil) || got != nil && *got != *tc.want {
			t.Errorf("%s: %+v, want %+v", tc.what, got, tc.want)
		}
	}
}

// TestTheLibrarysRefusalsAreItsReasons holds notALibraryTrack's words to
// internal/player/library's reasons, which it names as they are written there:
// the refusals that say a URI names no track of the bridge's are 600, and the
// library's other refusals 501.
func TestTheLibrarysRefusalsAreItsReasons(t *testing.T) {
	reasons := reasonsOf(t, filepath.Join("..", "library", "library.go"))
	for _, name := range []string{"ReasonNotIndexed", "ReasonRouted", "ReasonSACDVirtual", "ReasonOutsideLibrary"} {
		if r, ok := reasons[name]; !ok || !notALibraryTrack(r) {
			t.Errorf("library.%s (%q) is not one of the refusals that name no track", name, r)
		}
	}
	for _, name := range []string{"ReasonAmbiguous", "ReasonIndexUnreadable", "ReasonResolverFailed", "ReasonNotAFile"} {
		if r, ok := reasons[name]; !ok || notALibraryTrack(r) {
			t.Errorf("library.%s (%q): taken for a URI that names no track", name, r)
		}
	}
}

// TestASupersededPlayAnswersSuccess pins superseded as success: another
// controller's Play, its session's Start held, then a SeekId while it starts,
// which replaces the playback the Play asked for; the hold lifted inside the
// budget. The engine answers the Play superseded (the spy records it), the
// Play answers success, and the SeekId's entry is the one that plays.
func TestASupersededPlayAnswersSuccess(t *testing.T) {
	p := newTestPlayer(t)
	spy := newSpy(p.eng)
	h := p.handlerWith("", spy, DefaultLimits.orDefaults())
	p.add(uriOf(goldenID), nil)
	b := p.add(uriOf(trackB), nil)
	letGo := p.be.holdStarts(t)
	play := send(h, playlist, "Play")
	p.be.session(t, 1)
	seek := send(h, playlist, "SeekId", "Value", u(b))
	s2 := p.be.session(t, 2)
	if s2.first.Entry != b {
		t.Fatalf("the SeekId started %d, want b (%d)", s2.first.Entry, b)
	}
	letGo()
	if r := play.answer(t); r.status != http.StatusOK {
		t.Fatalf("the superseded Play: %d, fault %d, want success", r.status, r.fault)
	}
	if got := spy.answered("Play"); len(got) != 1 || engine.ReasonOf(got[0]) != engine.ReasonSuperseded {
		t.Fatalf("the engine answered the Play %v, want superseded", got)
	}
	if r := seek.answer(t); r.status != http.StatusOK {
		t.Fatalf("the SeekId: %d, fault %d", r.status, r.fault)
	}
	if got := read(t, h, playlist, "Id")["Value"]; got != u(b) {
		t.Errorf("Id %s, want the SeekId's %d", got, b)
	}
	if got := read(t, h, playlist, "TransportState")["Value"]; got != "Buffering" && got != "Playing" {
		t.Errorf("TransportState %s, want b starting", got)
	}
}

// TestAnOvertakenPlayIsAFault pins overtaken as a fault: a Play parked behind
// a Pause or a Resume under way (the backend's call held; the engine parks a
// Play behind nothing else, owner.go's blocked), then a Stop, which overtakes
// it: it did not run, and it answers 501. The command under way is answered
// by the Stop as the engine answers it: the Pause nil, the Resume's Play
// superseded, each success.
func TestAnOvertakenPlayIsAFault(t *testing.T) {
	for _, tc := range []struct {
		what string
		// underWay sends the command the parked Play waits behind, its
		// backend call held, and returns it.
		underWay func(t *testing.T, p *testPlayer, h *handler) sent
	}{
		{"behind a Pause", func(t *testing.T, p *testPlayer, h *handler) sent {
			playing(t, p, h)
			p.be.holdPauses(t)
			c := send(h, playlist, "Pause")
			p.be.callsBegun(t, "Pause", 1)
			return c
		}},
		{"behind a Resume", func(t *testing.T, p *testPlayer, h *handler) sent {
			playing(t, p, h)
			read(t, h, playlist, "Pause")
			p.be.holdResumes(t)
			c := send(h, playlist, "Play")
			p.be.callsBegun(t, "Resume", 1)
			return c
		}},
	} {
		t.Run(tc.what, func(t *testing.T) {
			p := newTestPlayer(t)
			spy := newSpy(p.eng)
			h := p.handlerWith("", spy, DefaultLimits.orDefaults())
			p.add(uriOf(goldenID), nil)
			first := tc.underWay(t, p, h)
			watched := spy.watch("Play")
			parked := send(h, playlist, "Play")
			admitted(t, watched)
			read(t, h, playlist, "Stop")
			if r := parked.answer(t); r.fault != errActionFailed {
				t.Fatalf("the Play a Stop overtook: %d, fault %d, want 501", r.status, r.fault)
			}
			if r := first.answer(t); r.status != http.StatusOK {
				t.Errorf("the %s the Stop answered: %d, fault %d, want success", first.action, r.status, r.fault)
			}
			overtaken := 0
			for _, err := range spy.answered("Play") {
				switch engine.ReasonOf(err) {
				case engine.ReasonOvertaken:
					overtaken++
				case "", engine.ReasonSuperseded:
				default:
					t.Errorf("the engine answered a Play %v", err)
				}
			}
			if overtaken != 1 {
				t.Fatalf("the engine answered %d Plays overtaken, want the parked one: %v", overtaken, spy.answered("Play"))
			}
			if got := read(t, h, playlist, "TransportState")["Value"]; got != "Stopped" {
				t.Errorf("TransportState %s, want Stopped", got)
			}
		})
	}
}

// TestAPlayDuringAStartChangesNothing pins what a second Play sent while a
// start is under way answers, today's outcome: the engine does not park it
// (a start blocks a Pause, not a Play: owner.go's blocked) and its table
// does nothing for a Play while a session starts, so it answers success at
// once, the first Play still waiting for its start; a Stop then supersedes
// the first, which answers success too, and nothing plays.
func TestAPlayDuringAStartChangesNothing(t *testing.T) {
	p := newTestPlayer(t)
	spy := newSpy(p.eng)
	h := p.handlerWith("", spy, DefaultLimits.orDefaults())
	p.add(uriOf(goldenID), nil)
	p.be.holdStarts(t)
	first := send(h, playlist, "Play")
	p.be.callsBegun(t, "Start", 1)
	if r := invoke(t, h, playlist, "Play"); r.status != http.StatusOK {
		t.Fatalf("a Play during a start: %d, fault %d, want success", r.status, r.fault)
	}
	if !first.pending() {
		t.Fatal("the first Play answered before its start did")
	}
	read(t, h, playlist, "Stop")
	if r := first.answer(t); r.status != http.StatusOK {
		t.Errorf("the Play the Stop superseded: %d, fault %d, want success", r.status, r.fault)
	}
	if got := spy.answered("Play"); len(got) != 2 || got[0] != nil || engine.ReasonOf(got[1]) != engine.ReasonSuperseded {
		t.Errorf("the engine answered the Plays %v, want the second nil at once and the first superseded", got)
	}
	if got := read(t, h, playlist, "TransportState")["Value"]; got != "Stopped" {
		t.Errorf("TransportState %s, want Stopped", got)
	}
}

// playing plays p's first entry through h, heard.
func playing(t *testing.T, p *testPlayer, h *handler) *fakeSession {
	t.Helper()
	read(t, h, playlist, "Play")
	s := p.be.session(t, p.be.sessionCount())
	s.hear(s.first.ID, nil)
	p.heardNow(s.first.ID)
	return s
}

// TestAPauseWhileAReleaseCarriesAPlayChangesNothing pins a limit the
// engine's table makes, today's outcome, so that a change to it is made
// deliberately: a SeekId while playing releases the session with a play to
// follow, and a Pause sent while that release is under way (before the
// SeekId has replied: a serial controller such as 1-bit cannot) succeeds and
// changes nothing, so the SeekId's entry then plays, and a controller reads
// Playing after its Pause succeeded.
func TestAPauseWhileAReleaseCarriesAPlayChangesNothing(t *testing.T) {
	p := newTestPlayer(t)
	h := p.handler("")
	p.add(uriOf(goldenID), nil)
	b := p.add(uriOf(trackB), nil)
	playing(t, p, h)
	letGo := p.be.holdReleases()
	defer letGo()
	seek := send(h, playlist, "SeekId", "Value", u(b))
	p.be.callsBegun(t, "Release", 1)
	if r := invoke(t, h, playlist, "Pause"); r.status != http.StatusOK {
		t.Fatalf("a Pause while the release runs: %d, fault %d, want success", r.status, r.fault)
	}
	letGo()
	if r := seek.answer(t); r.status != http.StatusOK {
		t.Fatalf("the SeekId: %d, fault %d", r.status, r.fault)
	}
	s := p.be.session(t, 2)
	if s.first.Entry != b {
		t.Fatalf("the session after the release started %d, want b (%d)", s.first.Entry, b)
	}
	s.hear(s.first.ID, nil)
	p.heardNow(s.first.ID)
	if got := read(t, h, playlist, "TransportState")["Value"]; got != "Playing" {
		t.Errorf("TransportState %s after the Pause, want Playing: the Pause changed nothing", got)
	}
	if got := read(t, h, playlist, "Id")["Value"]; got != u(b) {
		t.Errorf("Id %s, want b", got)
	}
}

// TestARefusedStartIsAFault pins the start's refusals 1-bit must see, so it
// stops and reverts: a start the backend refuses (a busy device) and a queue
// every entry of which is refused before any of it plays (repeat off, and on,
// which makes one lap) each answer Play 501, the transport Stopped after, Id
// on the entry the start was refused for (the first for a busy device, the
// last refused for an unplayable queue), every entry still in the queue.
func TestARefusedStartIsAFault(t *testing.T) {
	for _, tc := range []struct {
		what   string
		repeat bool
		refuse func(engine.StartRequest) error
		lastID int
	}{
		{"a busy device", false, func(engine.StartRequest) error { return errors.New("player: the device is busy") }, 0},
		{"every entry unplayable", false, func(engine.StartRequest) error { return &engine.Unplayable{Reason: "dsd_not_in_this_release"} }, 1},
		{"every entry unplayable, repeat on", true, func(engine.StartRequest) error { return &engine.Unplayable{Reason: "lossy_refused"} }, 1},
	} {
		t.Run(tc.what, func(t *testing.T) {
			p := newTestPlayer(t)
			h := p.handler("")
			ids := []uint32{p.add(uriOf(goldenID), nil), p.add(uriOf(trackB), nil)}
			if tc.repeat {
				read(t, h, playlist, "SetRepeat", "Value", "1")
			}
			p.be.mu.Lock()
			p.be.refuseStart = tc.refuse
			p.be.mu.Unlock()
			if r := soapFault(t, h, playlist, "Play"); r.fault != errActionFailed {
				t.Fatalf("Play: fault %d, want 501", r.fault)
			}
			p.waitSnap("stopped", func(s *engine.Snapshot) bool { return s.State() == engine.Stopped })
			if got := read(t, h, playlist, "TransportState")["Value"]; got != "Stopped" {
				t.Errorf("TransportState %s, want Stopped", got)
			}
			if got := read(t, h, playlist, "Id")["Value"]; got != u(ids[tc.lastID]) {
				t.Errorf("Id %s, want %d", got, ids[tc.lastID])
			}
			if got := queueOf(t, h); !slices.Equal(got, ids) {
				t.Errorf("the queue %v, want every entry, %v", got, ids)
			}
		})
	}
}

// TestARefusalAtTheOpenIsTheEnginesSkip pins what a controller reads when
// the backend refuses an entry before any of it plays (a format this release
// does not play, DSD or lossy, which Insert takes: the decoder decides the
// format, not the URI): the engine skips it with its reason
// (Snapshot.Skipped, which no OpenHome read shows), and the entry stays in
// the queue, Read giving its URI as inserted. Refused at its start, the entry
// that follows plays: Play answers success, Id reads that entry at once, and
// TransportState Buffering until it is heard, then Playing. Refused as a
// successor (prepared while the entry before it plays), Id does not move: it
// reads the entry playing, TransportState Playing, until the entry after the
// refused one is heard.
func TestARefusalAtTheOpenIsTheEnginesSkip(t *testing.T) {
	t.Run("the first entry, a playable one after it", func(t *testing.T) {
		p := newTestPlayer(t)
		h := p.handler("")
		x := newID(t, h, 0, uriOf(goldenID), "")
		b := newID(t, h, x, uriOf(trackB), "")
		p.be.mu.Lock()
		p.be.refuseStart = func(req engine.StartRequest) error {
			if req.First.Entry == x {
				return &engine.Unplayable{Reason: "dsd_not_in_this_release"}
			}
			return nil
		}
		p.be.mu.Unlock()
		read(t, h, playlist, "Play")
		if got := read(t, h, playlist, "Id")["Value"]; got != u(b) {
			t.Errorf("Id %s once Play answered, want b (%d), the entry after the refused one", got, b)
		}
		if got := read(t, h, playlist, "TransportState")["Value"]; got != "Buffering" {
			t.Errorf("TransportState %s before b is heard, want Buffering", got)
		}
		if sk, ok := p.eng.Snapshot().Skipped(x); !ok || sk.Reason != "dsd_not_in_this_release" {
			t.Errorf("x's skip: %+v %v", sk, ok)
		}
		s := p.be.session(t, 2)
		if s.first.Entry != b {
			t.Fatalf("the second session started %d, want b", s.first.Entry)
		}
		s.hear(s.first.ID, nil)
		p.heardNow(s.first.ID)
		if got := read(t, h, playlist, "TransportState")["Value"]; got != "Playing" {
			t.Errorf("TransportState %s once b is heard, want Playing", got)
		}
		if got := read(t, h, playlist, "Read", "Id", u(x))["Uri"]; got != uriOf(goldenID) {
			t.Errorf("Read(x)'s Uri %q", got)
		}
		if got := queueOf(t, h); !slices.Equal(got, []uint32{x, b}) {
			t.Errorf("the queue %v, want x and b", got)
		}
	})
	t.Run("a prefetched successor", func(t *testing.T) {
		p := newTestPlayer(t)
		h := p.handler("")
		a := newID(t, h, 0, uriOf(goldenID), "")
		y := newID(t, h, a, uriOf(trackB), "")
		c := newID(t, h, y, uriOf(trackC), "")
		s := playing(t, p, h)
		next := s.successor(t, 1)
		if next == nil || next.Entry != y {
			t.Fatalf("the first successor %+v, want y (%d)", next, y)
		}
		s.ev.Refused(s.id, next.ID, engine.Unplayable{Reason: "lossy_refused"})
		after := s.successor(t, 2)
		if after == nil || after.Entry != c {
			t.Fatalf("the successor after the refused one %+v, want c (%d)", after, c)
		}
		if got := read(t, h, playlist, "Id")["Value"]; got != u(a) {
			t.Errorf("Id %s after the successor was refused, want a, still playing", got)
		}
		if got := read(t, h, playlist, "TransportState")["Value"]; got != "Playing" {
			t.Errorf("TransportState %s, want Playing", got)
		}
		s.ev.Submitted(s.id, after.ID)
		s.hear(after.ID, nil)
		p.heardNow(after.ID)
		if got := read(t, h, playlist, "Id")["Value"]; got != u(c) {
			t.Errorf("Id %s once c is heard, want c", got)
		}
		if sk, ok := p.eng.Snapshot().Skipped(y); !ok || sk.Reason != "lossy_refused" {
			t.Errorf("y's skip: %+v %v", sk, ok)
		}
		if got := read(t, h, playlist, "Read", "Id", u(y))["Uri"]; got != uriOf(trackB) {
			t.Errorf("Read(y)'s Uri %q", got)
		}
	})
}

// TestTheMetadataIsStoredAsSentWithinItsBounds pins Insert's metadata: empty
// is taken; a DIDL-Lite that is not well-formed is stored as it was sent,
// never parsed, and Read gives it back byte for byte; the engine's 64 KiB is
// taken, and a byte more is 605, String Argument Too Long, before the engine;
// a character XML 1.0 forbids, literal or by reference, and a byte that is not
// UTF-8, are refused by the SOAP layer before any action sees them (400, row
// 7); and metadata within 64 KiB whose escaping takes the request past the
// body's 256 KiB is 413, the body's bound unchanged. A refusal leaves the
// queue as it was.
func TestTheMetadataIsStoredAsSentWithinItsBounds(t *testing.T) {
	p := newTestPlayer(t)
	h := p.handler("")
	for _, meta := range []string{"", `<DIDL-Lite><item id="1"><dc:title>Q&A <b>unclosed`, strings.Repeat("m", engine.MaxMetadataBytes)} {
		id := newID(t, h, 0, uriOf(goldenID), meta)
		if got := read(t, h, playlist, "Read", "Id", u(id))["Metadata"]; got != meta {
			t.Errorf("Read of metadata of %d bytes: %d bytes back, want them as sent", len(meta), len(got))
		}
	}
	before := queueOf(t, h)
	if r := soapFault(t, h, playlist, "Insert", "AfterId", "0", "Uri", uriOf(goldenID), "Metadata", strings.Repeat("m", engine.MaxMetadataBytes+1)); r.fault != errStringTooLong {
		t.Errorf("metadata a byte past 64 KiB: fault %d, want 605", r.fault)
	}
	raw := func(meta string) *http.Request {
		return soapRequest(playlist, header(playlist, "Insert"), "text/xml",
			envelope(call(playlist, "Insert", "<AfterId>0</AfterId><Uri>"+uriOf(goldenID)+"</Uri><Metadata>"+meta+"</Metadata>")))
	}
	for _, meta := range []string{"a\x01b", "a&#x1;b", "a&#xFFFE;b", "a￿b", "a\xffb"} {
		if got := serve(t, h, raw(meta)); got.status != http.StatusBadRequest {
			t.Errorf("metadata %q: %v, want 400 from the SOAP layer", meta, got)
		}
	}
	if got := serve(t, h, raw(strings.Repeat("&amp;", engine.MaxMetadataBytes))); got.status != http.StatusRequestEntityTooLarge {
		t.Errorf("64 KiB of metadata escaped past the body's bound: %v, want 413", got)
	}
	if got := queueOf(t, h); !slices.Equal(got, before) {
		t.Errorf("the queue %v after the refusals, want %v", got, before)
	}
}

// TestOnlyXMLTextIsMetadata pins metadataFault's character check, which no
// request through the SOAP layer can fail (row 7 refuses what XML 1.0 cannot
// carry first): XML 1.0's Char production is taken, tab, line feed and
// carriage return, U+FFFD and the supplementary planes among it; a control, a
// noncharacter U+FFFE or U+FFFF, a surrogate's encoding and a byte that is not
// UTF-8 are 600, Argument Value Invalid.
func TestOnlyXMLTextIsMetadata(t *testing.T) {
	for _, s := range []string{"", "\t\n\r", " ~\x7f", "é漢", "�", "\U0001F3B5", "\U0010FFFF", "퟿", ""} {
		if f := metadataFault([]byte(s)); f != nil {
			t.Errorf("%q: fault %d, want taken", s, f.code)
		}
	}
	for _, s := range []string{"\x00", "a\x01", "\x08", "\x0b", "\x0c", "\x1f", "￾", "￿", "\xed\xa0\x80", "\xff", "a\xc3"} {
		if f := metadataFault([]byte(s)); f == nil || f.code != errArgumentValueInvalid {
			t.Errorf("%q: %+v, want 600", s, f)
		}
	}
}

// TestATransportWriteAdmittedPastItsBudgetIsSuccess pins the budget's end for
// a command the engine admitted: the budget shortened through the listener's
// limits (Write 2 s, a budget of 1 s), a Play whose session's Start the
// backend holds answers success at the budget's end (the engine answers it
// cancelled, without ErrNotAdmitted: it may yet run), and the reads that
// follow show the start under way, then the session once the hold is let go.
func TestATransportWriteAdmittedPastItsBudgetIsSuccess(t *testing.T) {
	p := newTestPlayer(t)
	spy := newSpy(p.eng)
	h := p.handlerWith("", spy, Limits{Write: 2 * time.Second}.orDefaults())
	a := p.add(uriOf(goldenID), nil)
	letGo := p.be.holdStarts(t)
	began := time.Now()
	if r := invoke(t, h, playlist, "Play"); r.status != http.StatusOK {
		t.Fatalf("a Play past its budget: %d, fault %d, want success", r.status, r.fault)
	}
	if took := time.Since(began); took < time.Second || took > 2*time.Second {
		t.Errorf("the Play answered after %v, want at its budget's end, 1 s", took)
	}
	if got := spy.answered("Play"); len(got) != 1 || engine.ReasonOf(got[0]) != engine.ReasonCancelled || errors.Is(got[0], engine.ErrNotAdmitted) {
		t.Fatalf("the engine answered %v, want a cancellation of a command it admitted", got)
	}
	if got := read(t, h, playlist, "TransportState")["Value"]; got != "Buffering" {
		t.Errorf("TransportState %s while the start is held, want Buffering", got)
	}
	if got := read(t, h, playlist, "Id")["Value"]; got != u(a) {
		t.Errorf("Id %s, want a", got)
	}
	letGo()
	s := p.be.session(t, 1)
	s.hear(s.first.ID, nil)
	p.heardNow(s.first.ID)
	if got := read(t, h, playlist, "TransportState")["Value"]; got != "Playing" {
		t.Errorf("TransportState %s once the start is let go, want Playing", got)
	}
}

// TestACommandParkedPastItsBudgetThenOvertakenNeverRan pins what success past
// the budget means: a Play parked behind a Pause under way, and a SeekId
// parked behind it, each answer success once their budget (1 s) ends; a Stop
// then overtakes both, and the reads show neither ran: the selection is the
// one before them, and the transport Stopped.
func TestACommandParkedPastItsBudgetThenOvertakenNeverRan(t *testing.T) {
	p := newTestPlayer(t)
	spy := newSpy(p.eng)
	h := p.handlerWith("", spy, Limits{Write: 2 * time.Second}.orDefaults())
	a := p.add(uriOf(goldenID), nil)
	b := p.add(uriOf(trackB), nil)
	read(t, h, playlist, "Play")
	s := p.be.session(t, 1)
	s.hear(s.first.ID, nil)
	p.heardNow(s.first.ID)
	p.be.holdPauses(t)
	pause := send(h, playlist, "Pause")
	p.be.callsBegun(t, "Pause", 1)
	plays, seeks := spy.watch("Play"), spy.watch("PlayID")
	play := send(h, playlist, "Play")
	admitted(t, plays)
	seek := send(h, playlist, "SeekId", "Value", u(b))
	admitted(t, seeks)
	for _, c := range []sent{play, seek} {
		if r := c.answer(t); r.status != http.StatusOK {
			t.Fatalf("%s parked past its budget: %d, fault %d, want success", c.action, r.status, r.fault)
		}
	}
	read(t, h, playlist, "Stop")
	if got := read(t, h, playlist, "Id")["Value"]; got != u(a) {
		t.Errorf("Id %s after the Stop, want a, the selection before the commands that never ran", got)
	}
	if got := read(t, h, playlist, "TransportState")["Value"]; got != "Stopped" {
		t.Errorf("TransportState %s, want Stopped", got)
	}
	if n := p.be.sessionCount(); n != 1 {
		t.Errorf("%d sessions started, want the first alone", n)
	}
	pause.answer(t)
}

// slowBody is a request body that gives its bytes only after a delay: a
// controller that takes that long to send its body.
type slowBody struct {
	r     io.Reader
	delay <-chan time.Time
}

func (b *slowBody) Read(p []byte) (int, error) {
	if b.delay != nil {
		<-b.delay
		b.delay = nil
	}
	return b.r.Read(p)
}

// slowRequest is actionRequest whose body takes delay to arrive.
func slowRequest(s *service, action string, delay time.Duration, args ...string) *http.Request {
	r := actionRequest(s, action, args...)
	r.Body = io.NopCloser(&slowBody{r: r.Body, delay: time.After(delay)})
	return r
}

// TestAnEditLateInItsBudgetIsRefusedBeforeTheEngine pins the edit gate: an
// edit whose body took 4 s to arrive (inside the listener's 5 s for a body),
// or 3.5 s (its budget then holding engine.ReserveTimeout and less than
// editBudget), is answered 501 without the engine being called, Insert,
// DeleteId and DeleteAll alike, the queue as it was; the same edits whose
// bodies arrive at once are taken.
func TestAnEditLateInItsBudgetIsRefusedBeforeTheEngine(t *testing.T) {
	p := newTestPlayer(t)
	spy := newSpy(p.eng)
	h := p.handlerWith("", spy, DefaultLimits.orDefaults())
	a := p.add(uriOf(goldenID), nil)
	edits := [][]string{
		{"Insert", "AfterId", "0", "Uri", uriOf(trackB), "Metadata", ""},
		{"DeleteId", "Value", u(a)},
		{"DeleteAll"},
	}
	var late []sent
	for _, delay := range []time.Duration{4 * time.Second, 3500 * time.Millisecond} {
		for _, e := range edits {
			late = append(late, sendRequest(h, playlist, e[0], slowRequest(playlist, e[0], delay, e[1:]...)))
		}
	}
	for _, c := range late {
		if r := c.answer(t); r.fault != errActionFailed {
			t.Errorf("%s late in its budget: %d, fault %d, want 501", c.action, r.status, r.fault)
		}
	}
	for _, op := range []string{"InsertAfter", "Delete", "DeleteAll"} {
		if n := len(spy.answered(op)); n != 0 {
			t.Errorf("a late edit reached the engine's %s (%d calls)", op, n)
		}
	}
	if got := queueOf(t, h); !slices.Equal(got, []uint32{a}) {
		t.Fatalf("the queue %v after the late edits, want a alone", got)
	}
	b := newID(t, h, a, uriOf(trackB), "")
	read(t, h, playlist, "DeleteId", "Value", u(a))
	if got := queueOf(t, h); !slices.Equal(got, []uint32{b}) {
		t.Fatalf("the queue %v after an Insert and a DeleteId at once, want b alone", got)
	}
	read(t, h, playlist, "DeleteAll")
	if got := queueOf(t, h); len(got) != 0 {
		t.Fatalf("the queue %v after a DeleteAll at once", got)
	}
}

// TestAnEditThatPassesTheGateIsAnsweredInsideItsBudget pins why the gate is
// editBudget: an Insert that passes it with as little of its budget left as
// it lets (a budget of editBudget and a fifth of a second, through the
// listener's limits) is answered inside that budget however long the engine
// may hold it for a reservation write, which is at most engine.ReserveTimeout,
// its own timer refusing it then (in the engine,
// TestAParkedEditIsAnsweredWithinReserveTimeoutOfItsPark): held a little less,
// it answers its NewId; held a little more and refused as the engine refuses
// it there, 501.
func TestAnEditThatPassesTheGateIsAnsweredInsideItsBudget(t *testing.T) {
	p := newTestPlayer(t)
	lim := Limits{Write: editBudget + budgetMargin + 200*time.Millisecond}.orDefaults()
	const epsilon = 50 * time.Millisecond
	for _, tc := range []struct {
		what    string
		hold    time.Duration
		refused bool
	}{
		{"held a little less than ReserveTimeout", engine.ReserveTimeout - epsilon, false},
		{"held a little more, then refused", engine.ReserveTimeout + epsilon, true},
	} {
		t.Run(tc.what, func(t *testing.T) {
			t.Parallel()
			spy := newSpy(p.eng)
			spy.beforeInsert = func(ctx context.Context) error {
				select {
				case <-time.After(tc.hold):
				case <-ctx.Done():
					return &engine.Error{Reason: engine.ReasonCancelled, Op: "insert", Err: ctx.Err()}
				}
				if tc.refused {
					return &engine.Error{Reason: engine.ReasonReservation, Op: "insert", Detail: "the reservation write did not land within ReserveTimeout"}
				}
				return nil
			}
			h := p.handlerWith("", spy, lim)
			began := time.Now()
			r := invoke(t, h, playlist, "Insert", "AfterId", "0", "Uri", uriOf(goldenID), "Metadata", "")
			if took := time.Since(began); took >= lim.writeBudget() {
				t.Errorf("answered after %v, past its %v budget", took, lim.writeBudget())
			}
			switch {
			case tc.refused && r.fault != errActionFailed:
				t.Errorf("an Insert the engine refused: %d, fault %d, want 501", r.status, r.fault)
			case !tc.refused && (r.status != http.StatusOK || ui4Value(t, "NewId", r.values["NewId"]) == 0):
				t.Errorf("an Insert the engine took: %d, fault %d, NewId %q", r.status, r.fault, r.values["NewId"])
			}
		})
	}
}
