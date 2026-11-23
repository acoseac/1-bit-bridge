package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/dlna"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/library"
	"github.com/acoseac/1-bit-bridge/internal/player/openhome"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/trackref"
)

// The console's control of the player (B634, PR 1), driven over the real
// engine on the stand-in backend (playerStandIn), the real library index and
// the player's real resolver, as serve's player runs them: ctlRig builds
// them as serve's player does once it is ready, and the control over them.
// Every race is made by parking at a seam (the control's controlSeams, the
// engine's Config.Seams, the stand-in's holds), never by stress.

// ctlTrack is one library track a rig indexes.
type ctlTrack struct {
	path, codec, title string
	dsd                bool
	rate               int
	seconds            float64
}

// ctlOptions are a rig's departures.
type ctlOptions struct {
	tracks     []ctlTrack
	advertised []string
	seams      *controlSeams
	engSeams   *engine.Seams
	// holdRelease holds every Release of the stand-in until it is closed.
	holdRelease chan struct{}
	// holdStart holds a Start of the reference it names until its channel is
	// closed (playerStandIn.holdStart).
	holdStart map[string]chan struct{}
	// noOutput leaves the engine's output unknown.
	noOutput bool
	policy   playback.PlayerPolicy
	dlna     dlnaArtworkBase
	// details is what the stand-in reports of each session's first
	// occurrence; the zero value is a 180 s FLAC at 44.1 kHz.
	details *engine.Details
	// boot and live are the boot snapshot and the live config the view and
	// the control read the withholding from; nil for none.
	boot *config.Config
	live func() *config.Config
}

// ctlBackend is the stand-in with the player's facts and a test tone's
// state, which serve's backend publishes and the stand-in has not.
type ctlBackend struct {
	*playerStandIn
	mu     sync.Mutex
	policy playback.PlayerPolicy
	tone   playback.ToneState
	// holdResume, when set, holds the next Resume until it is closed, and
	// resuming hears it held: a resume under way, which the engine shows as
	// paused until it answers.
	holdResume chan struct{}
	resuming   chan struct{}
}

// Resume is the stand-in's, held where holdResume says.
func (b *ctlBackend) Resume(ctx context.Context, s engine.SessionID) (engine.ResumeResult, error) {
	b.mu.Lock()
	hold, heard := b.holdResume, b.resuming
	b.holdResume = nil
	b.mu.Unlock()
	if hold != nil {
		close(heard)
		<-hold
	}
	return b.playerStandIn.Resume(ctx, s)
}

func (b *ctlBackend) Facts() (playback.PlayerPolicy, *playback.PlaybackFacts) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.policy, nil
}

func (b *ctlBackend) Tone(context.Context, playback.ToneRequest) playback.ToneResult {
	return playback.ToneResult{}
}
func (b *ctlBackend) ToneOutput(bool, playback.ToneOutcome) {}
func (b *ctlBackend) CloseTones()                           {}
func (b *ctlBackend) ToneStatus() playback.ToneStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.tone
	if st == "" {
		st = playback.ToneIdle
	}
	return playback.ToneStatus{State: st}
}

// setTone puts a tone in the device slot, or takes it out.
func (b *ctlBackend) setTone(st playback.ToneState) {
	b.mu.Lock()
	b.tone = st
	b.mu.Unlock()
}

// ctlRig is a running player and the console's control of it.
type ctlRig struct {
	t       *testing.T
	music   string
	dataDir string
	store   *manifest.Store
	res     *library.Resolver
	standIn *playerStandIn
	be      *ctlBackend
	eng     *engine.Engine
	p       *bridgePlayer
	view    *consolePlayer
	c       *consoleControl
	tab     string
	seq     uint64
	digests uint64
	runDone chan struct{}
	stopRun context.CancelFunc
}

// defaultCtlTracks are three FLAC tracks of one album.
var defaultCtlTracks = []ctlTrack{
	{path: "Album/01 A.flac", codec: "FLAC", title: "A", rate: 44100, seconds: 180},
	{path: "Album/02 B.flac", codec: "FLAC", title: "B", rate: 44100, seconds: 180},
	{path: "Album/03 C.flac", codec: "FLAC", title: "C", rate: 44100, seconds: 180},
}

// newCtlRig builds a rig: the library indexed, the engine running with its
// output reported available (unless noOutput), the control over them.
func newCtlRig(t *testing.T, o ctlOptions) *ctlRig {
	t.Helper()
	needsDirectorySync(t)
	ctlMedium(t)
	root := t.TempDir()
	r := &ctlRig{t: t, music: filepath.Join(root, "Music"), dataDir: filepath.Join(root, "data")}
	for _, d := range []string{r.music, r.dataDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	store, err := manifest.OpenStore(manifest.DefaultDBPath(r.dataDir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	r.store = store
	tracks := o.tracks
	if tracks == nil {
		tracks = defaultCtlTracks
	}
	r.index(tracks...)
	res, err := library.New(library.Config{Store: store, Paths: bridgefs.New([]string{r.music})})
	if err != nil {
		t.Fatal(err)
	}
	r.res = res
	r.standIn = newPlayerStandIn()
	r.standIn.holdRelease, r.standIn.holdStart = o.holdRelease, o.holdStart
	d := engine.Details{Frames: 44100 * 180, RateHz: 44100, Bits: 16, Channels: 2, Codec: "FLAC", Lossless: true}
	if o.details != nil {
		d = *o.details
	}
	r.standIn.details = &d
	if _, err := r.standIn.build(playback.Config{Resolver: res}); err != nil {
		t.Fatal(err)
	}
	r.be = &ctlBackend{playerStandIn: r.standIn, policy: o.policy}
	ecfg := engine.Config{PlayerUUID: "4b1c2a3e-7f60-4b2d-9e0f-0123456789ab", Backend: r.be,
		QueuePath: filepath.Join(r.dataDir, playerQueueFile), ReservePath: filepath.Join(r.dataDir, playerReservationFile),
		DataDir: r.dataDir, Seams: o.engSeams}
	eng, err := engine.New(ecfg)
	if err != nil {
		t.Fatal(err)
	}
	r.eng = eng
	ctx, cancel := context.WithCancel(context.Background())
	r.stopRun, r.runDone = cancel, make(chan struct{})
	go func() { defer close(r.runDone); _ = eng.Run(ctx) }()
	if !o.noOutput {
		r.report(1, true, "")
	}
	ready := make(chan struct{})
	close(ready)
	plan := servePlayerPlan{start: true, lists: true}
	r.p = &bridgePlayer{plan: plan, ready: ready, done: make(chan struct{}), settled: make(chan struct{}), closedKnown: make(chan struct{}),
		eng: eng, be: r.be, res: res}
	r.view = newConsolePlayer(consolePlayerWiring{Plan: plan, Player: r.p, Store: store, GOOS: "linux", LockHeld: true, Boot: o.boot})
	advertised := o.advertised
	if advertised == nil {
		advertised = []string{"https://192.168.1.20:7788", "https://bridge.local:7788"}
	}
	inv := openhome.Inventory{Advertised: func() []string { return advertised }, Ports: []uint16{openhome.DefaultTrackPort}}
	seams := o.seams
	if seams == nil {
		seams = &controlSeams{}
	}
	if seams.handlerWait == 0 {
		seams.handlerWait = 20 * time.Second
	}
	r.c = newConsoleControl(consoleControlWiring{Plan: plan, Player: r.p, View: r.view, Boot: o.boot, Live: o.live, Store: store, Hosts: inv,
		GOOS: "linux", Seams: seams})
	if o.dlna != nil {
		r.c.setDLNA(o.dlna)
	}
	t.Cleanup(r.c.stop)
	drainLoopOnCleanup(t, r.stopRun, r.runDone, "the rig's engine")
	r.tab = strings.Repeat("ab", 16)
	return r
}

// The strict writer's medium seam (atomicwrite.SetMediumForTest) is
// process-wide: a rig made while another rig's engine writes a reservation
// must not set it again, which the race detector reports. ctlMedium installs
// the persistent medium once for every rig alive at a time and puts it back
// when the last of them is let go.
var (
	ctlMediumMu      sync.Mutex
	ctlMediumUsers   int
	ctlMediumRestore func()
)

// ctlMedium holds the persistent medium for a rig: its cleanup, registered
// before the rig's engine's, runs once that engine is joined.
func ctlMedium(t *testing.T) {
	ctlMediumMu.Lock()
	if ctlMediumUsers == 0 {
		ctlMediumRestore = atomicwrite.SetMediumForTest(func(string) error { return nil })
	}
	ctlMediumUsers++
	ctlMediumMu.Unlock()
	t.Cleanup(func() {
		ctlMediumMu.Lock()
		defer ctlMediumMu.Unlock()
		if ctlMediumUsers--; ctlMediumUsers == 0 {
			ctlMediumRestore()
			ctlMediumRestore = nil
		}
	})
}

// stopEngine stops the engine and waits for its Run.
func (r *ctlRig) stopEngine() {
	r.stopRun()
	select {
	case <-r.runDone:
	case <-serveGiveUp(r.t):
		r.t.Error("the engine's Run did not return")
	}
}

// index puts tracks in the library: their files and their rows.
func (r *ctlRig) index(tracks ...ctlTrack) {
	r.t.Helper()
	for _, tr := range tracks {
		p := filepath.Join(r.music, filepath.FromSlash(tr.path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("audio"), 0o644); err != nil {
			r.t.Fatal(err)
		}
		row := &manifest.Track{Path: tr.path, Title: tr.title, Codec: tr.codec, Size: 5, Album: "Album", Artist: "Artist"}
		if tr.rate > 0 {
			rate := float64(tr.rate)
			row.SampleRate = &rate
		}
		if tr.seconds > 0 {
			d := tr.seconds
			row.Duration = &d
		}
		if tr.dsd {
			dsd := true
			row.IsDSD = &dsd
		}
		if err := r.store.UpsertTrack(context.Background(), row); err != nil {
			r.t.Fatal(err)
		}
	}
}

// report hands the engine an output report.
func (r *ctlRig) report(gen uint64, available bool, reason string) {
	r.t.Helper()
	if err := r.eng.ReportOutput(context.Background(), engine.OutputReport{Generation: gen, Available: available, Reason: reason}); err != nil {
		r.t.Fatal(err)
	}
}

// op is the next operation's identity of the rig's tab.
func (r *ctlRig) op() admin.PlayerOp {
	r.seq++
	return admin.PlayerOp{Tab: r.tab, Seq: r.seq, Epoch: r.c.epoch}
}

// digest is a fresh command digest.
func (r *ctlRig) digest() [32]byte {
	r.digests++
	return sha256.Sum256([]byte(strconv.FormatUint(r.digests, 10)))
}

// gen is the engine's generation now, as a tab sends it.
func (r *ctlRig) gen() string { return r.eng.Snapshot().Generation().String() }

// paths are the rig's default tracks' paths.
func (r *ctlRig) paths() []string {
	out := []string{}
	for _, t := range defaultCtlTracks {
		out = append(out, t.path)
	}
	return out
}

// replace sends a replace of tracks from start, and waits for its answer.
func (r *ctlRig) replace(tracks []string, start, position int, play bool) admin.PlayerAnswer {
	return r.c.Replace(context.Background(), admin.PlayerReplace{Op: r.op(), Digest: r.digest(), At: r.gen(), Tracks: tracks,
		Start: start, Position: position, Play: play})
}

// command sends a transport command.
func (r *ctlRig) command(cmd string, set func(*admin.PlayerCommand)) admin.PlayerAnswer {
	c := admin.PlayerCommand{Op: r.op(), Digest: r.digest(), Command: cmd}
	if cmd != admin.PlayerCmdPause && cmd != admin.PlayerCmdStop {
		c.At = r.gen()
	}
	if set != nil {
		set(&c)
	}
	return r.c.Transport(context.Background(), c)
}

// switchOut sends the barrier.
func (r *ctlRig) switchOut(take bool) admin.PlayerAnswer {
	return r.c.SwitchOut(context.Background(), admin.PlayerSwitchOut{Op: r.op(), Digest: r.digest(), Take: take})
}

// await waits until cond holds of the engine's snapshot, until the test's
// deadline, on the engine's change signal.
func (r *ctlRig) await(what string, cond func(*engine.Snapshot) bool) *engine.Snapshot {
	r.t.Helper()
	for {
		ch := r.eng.Changed()
		s := r.eng.Snapshot()
		if cond(s) {
			return s
		}
		select {
		case <-ch:
		case <-serveGiveUp(r.t):
			r.t.Fatalf("waiting for the engine: %s; state %s, queue %v", what, s.State(), s.IDs())
		}
	}
}

// refs is the references of the engine's queue, in order.
func (r *ctlRig) refs() []string {
	s := r.eng.Snapshot()
	entries, err := s.ReadList(s.IDs())
	if err != nil {
		r.t.Fatal(err)
	}
	out := []string{}
	for _, e := range entries {
		out = append(out, e.Ref.String())
	}
	return out
}

// idOf is the track id of the reference ref.
func idOf(t *testing.T, ref string) string {
	t.Helper()
	id, err := trackref.ParseStored(ref)
	if err != nil {
		t.Fatalf("%q is no reference of a track: %v", ref, err)
	}
	return id
}

// wantRecord requires a's record's status, error and effect.
func wantRecord(t *testing.T, what string, a admin.PlayerAnswer, status, word, effect string) *admin.PlayerRecord {
	t.Helper()
	if a.Refused != "" {
		t.Fatalf("%s: refused %s before it became an operation, want a record", what, a.Refused)
	}
	rec := a.Record
	if rec == nil {
		t.Fatalf("%s: no record", what)
	}
	if rec.Status != status || rec.Error != word || (effect != "" && rec.Effect != effect) {
		t.Fatalf("%s: record %s/%q effect %s (last %q, sent %d of %d), want %s/%q effect %s", what, rec.Status, rec.Error, rec.Effect,
			rec.Last, rec.Sent, rec.Of, status, word, effect)
	}
	return rec
}

// state decodes a publication's state.
func stateOf(t *testing.T, pub admin.PlayerPublication) admin.PlayerState {
	t.Helper()
	var st admin.PlayerState
	if err := json.Unmarshal(pub.Bytes, &st); err != nil {
		t.Fatalf("a publication's state does not decode: %v (%q)", err, pub.Bytes)
	}
	return st
}

// wantQueue requires the engine's queue to name tracks, in order.
func (r *ctlRig) wantQueue(what string, tracks ...string) {
	r.t.Helper()
	got := []string{}
	for _, ref := range r.refs() {
		got = append(got, idOf(r.t, ref))
	}
	want := []string{}
	for _, p := range tracks {
		want = append(want, dlna.TrackID("", p))
	}
	if !slices.Equal(got, want) {
		r.t.Fatalf("%s: the queue holds %v, want %v (%v)", what, got, want, tracks)
	}
}

// TestAReplaceOfAnEmptyStoppedQueueGoesOn pins a replace on an empty,
// stopped queue with nothing selected (§2.4): DeleteAll changes nothing
// visible, and the edit goes on from there, the start track selected first,
// the tracks before it from the head, and the play from it.
func TestAReplaceOfAnEmptyStoppedQueueGoesOn(t *testing.T) {
	r := newCtlRig(t, ctlOptions{})
	a := r.replace(r.paths(), 1, 0, true)
	rec := wantRecord(t, "a replace and play", a, admin.PlayerOpDone, "", admin.PlayerEffectSome)
	if rec.Sent != 1+3+1 || rec.Of != rec.Sent {
		t.Fatalf("sent %d of %d steps, want 5 of 5", rec.Sent, rec.Of)
	}
	r.wantQueue("after the replace", r.paths()...)
	s := r.eng.Snapshot()
	if s.State() != engine.Playing || s.Selected() != rec.Entries[1] {
		t.Fatalf("the player is %s on %d, want playing the start track %d", s.State(), s.Selected(), rec.Entries[1])
	}
	st := stateOf(t, a.Player)
	if st.Revision != a.Player.Revision || st.Transport != "playing" {
		t.Fatalf("the answer's publication is revision %d (%d), %s; want its own, playing", st.Revision, a.Player.Revision, st.Transport)
	}
}

// TestAPausedHandoverKeepsTheQueueBeforeItsTrack pins the paused handover
// (§6.5, E6): [A, B, C] paused on B is the engine's [A, B, C], B selected,
// stopped, nothing played; and Previous then plays A.
func TestAPausedHandoverKeepsTheQueueBeforeItsTrack(t *testing.T) {
	r := newCtlRig(t, ctlOptions{})
	a := r.replace(r.paths(), 1, 30, false)
	rec := wantRecord(t, "a replace without play", a, admin.PlayerOpDone, "", admin.PlayerEffectSome)
	r.wantQueue("after the paused handover", r.paths()...)
	s := r.eng.Snapshot()
	if s.State() != engine.Stopped || s.Selected() != rec.Entries[1] {
		t.Fatalf("the player is %s on %d, want stopped on B (%d)", s.State(), s.Selected(), rec.Entries[1])
	}
	if _, _, starts := r.standIn.counts(); starts != 0 {
		t.Fatalf("%d sessions started, want none", starts)
	}
	prev := r.command(admin.PlayerCmdPrevious, nil)
	wantRecord(t, "Previous", prev, admin.PlayerOpDone, "", "")
	s = r.await("A plays", func(s *engine.Snapshot) bool { return s.State() == engine.Playing })
	if s.Selected() != rec.Entries[0] {
		t.Fatalf("Previous plays %d, want A (%d)", s.Selected(), rec.Entries[0])
	}
}

// TestAStaleGenerationChangesNothing pins the first step's condition (§2.2):
// a replace on a generation the engine has moved past is refused stale, its
// DeleteAll sent and refused by the owner, nothing changed.
func TestAStaleGenerationChangesNothing(t *testing.T) {
	r := newCtlRig(t, ctlOptions{})
	wantRecord(t, "the first replace", r.replace(r.paths(), 0, 0, false), admin.PlayerOpDone, "", "")
	stale := r.gen()
	wantRecord(t, "a repeat change", r.command(admin.PlayerCmdRepeat, func(c *admin.PlayerCommand) { c.On = true }), admin.PlayerOpDone, "", "")
	before := r.refs()
	a := r.c.Replace(context.Background(), admin.PlayerReplace{Op: r.op(), Digest: r.digest(), At: stale, Tracks: r.paths()[:1], Play: true})
	rec := wantRecord(t, "a stale replace", a, admin.PlayerOpRefused, wordStale, admin.PlayerEffectNone)
	if rec.Sent != 1 || rec.Last != string(engine.ReasonConflict) {
		t.Fatalf("sent %d, last %q; want its DeleteAll sent and refused %s", rec.Sent, rec.Last, engine.ReasonConflict)
	}
	if !slices.Equal(r.refs(), before) {
		t.Fatalf("the queue changed: %v, was %v", r.refs(), before)
	}
}

// TestAStaleFirstStepChangedNothingThoughItWasSent pins the effect (G9): a
// DeleteAll refused stale by the owner is sent (1) and changed nothing
// (effect none), decided by the engine's answer, never by the count sent;
// and an operation one of whose steps took effect before a later one was
// refused is effect some.
func TestAStaleFirstStepChangedNothingThoughItWasSent(t *testing.T) {
	r := newCtlRig(t, ctlOptions{})
	stale := r.gen()
	wantRecord(t, "a repeat change", r.command(admin.PlayerCmdRepeat, func(c *admin.PlayerCommand) { c.On = true }), admin.PlayerOpDone, "", "")
	a := r.c.Replace(context.Background(), admin.PlayerReplace{Op: r.op(), Digest: r.digest(), At: stale, Tracks: r.paths()})
	rec := wantRecord(t, "a stale replace", a, admin.PlayerOpRefused, wordStale, admin.PlayerEffectNone)
	if rec.Sent != 1 {
		t.Fatalf("sent %d steps, want 1: the stale DeleteAll is sent", rec.Sent)
	}
	// A later step refused: the DAC gone once the console's inserts have
	// landed, which refuses its PlayID; the steps before it took effect.
	r2 := newCtlRig(t, ctlOptions{seams: &controlSeams{}})
	inserts := 0
	r2.c.seams.afterStep = func(kind, step string, err error) {
		if kind == opKindReplace && step == "insert" {
			if inserts++; inserts == 3 {
				r2.report(2, false, "device_missing")
			}
		}
	}
	a = r2.replace(r2.paths(), 0, 0, true)
	wantRecord(t, "a replace whose PlayID the DAC's absence refused", a, admin.PlayerOpRefused, string(engine.ReasonOutputUnavailable), admin.PlayerEffectSome)
}

// fillQueue fills the engine's queue to MaxEntries with another controller's
// entries, as an OpenHome cast would.
func fillQueue(t *testing.T, r *ctlRig) {
	t.Helper()
	ref, err := engine.NewReference(dlna.TrackID("", r.paths()[0]))
	if err != nil {
		t.Fatal(err)
	}
	for r.eng.Snapshot().Len() < engine.MaxEntries {
		if _, err := r.eng.InsertAfter(context.Background(), 0, ref, nil); err != nil {
			t.Fatalf("another controller's insert: %v", err)
		}
	}
}

// TestACastAfterTheConsolesDeleteAllStopsItAtTheNextCheck pins the IDs
// compared after each step (§2.2): a cast's DeleteAll and insert landed at the
// seam after the console's DeleteAll leaves one console insert in the cast's
// queue, nothing played, the record changed.
func TestACastAfterTheConsolesDeleteAllStopsItAtTheNextCheck(t *testing.T) {
	r := newCtlRig(t, ctlOptions{seams: &controlSeams{}})
	var castID uint32
	r.c.seams.afterStep = func(kind, step string, err error) {
		if kind == opKindReplace && step == "delete_all" {
			castID = castInsert(t, r)
		}
	}
	a := r.replace(r.paths(), 0, 0, true)
	rec := wantRecord(t, "a replace raced by a cast", a, admin.PlayerOpChanged, "", admin.PlayerEffectSome)
	ids := r.eng.Snapshot().IDs()
	if len(ids) != 2 || !slices.Contains(ids, castID) || len(rec.Entries) != 1 {
		t.Fatalf("the queue holds %v (the cast's %d, the console's %v), want the cast's entry and one console insert", ids, castID, rec.Entries)
	}
	if _, _, starts := r.standIn.counts(); starts != 0 {
		t.Fatalf("%d sessions started, want none: the console plays nothing in the cast's queue", starts)
	}
}

// castInsert is a cast as 1-bit's: a DeleteAll and an insert, neither under a
// generation; it answers the cast's entry.
func castInsert(t *testing.T, r *ctlRig) uint32 {
	t.Helper()
	ctx := context.Background()
	if err := r.eng.DeleteAll(ctx); err != nil {
		t.Fatalf("the cast's DeleteAll: %v", err)
	}
	ref, err := engine.NewReference("http://192.168.1.20:7790/dlna/file/" + dlna.TrackID("", r.paths()[2]))
	if err != nil {
		t.Fatal(err)
	}
	id, err := r.eng.InsertAfter(ctx, 0, ref, []byte("cast"))
	if err != nil {
		t.Fatalf("the cast's insert: %v", err)
	}
	return id
}

// TestACastsDeleteAllOfAnEmptyConsoleQueueChangesNoMembership pins J6: a
// cast's DeleteAll that lands on the console's queue while it is still empty
// (after the console's DeleteAll, before its first insert) changes neither
// its membership nor its order, so the console's edit is done; the cast's
// insert, delayed past the console's end, then changes the queue, which the
// next frame shows.
func TestACastsDeleteAllOfAnEmptyConsoleQueueChangesNoMembership(t *testing.T) {
	r := newCtlRig(t, ctlOptions{seams: &controlSeams{}})
	r.c.seams.afterStep = func(kind, step string, err error) {
		if kind == opKindReplace && step == "delete_all" {
			if err := r.eng.DeleteAll(context.Background()); err != nil {
				t.Errorf("the cast's DeleteAll: %v", err)
			}
		}
	}
	a := r.replace(r.paths(), 0, 0, false)
	wantRecord(t, "a replace beside a cast's DeleteAll of an empty queue", a, admin.PlayerOpDone, "", "")
	ref, err := engine.NewReference(dlna.TrackID("", r.paths()[2]))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.eng.InsertAfter(context.Background(), 0, ref, nil); err != nil {
		t.Fatal(err)
	}
	pub := r.c.Now(context.Background())
	if stateOf(t, pub).Length != 4 || pub.QueueRev != pub.Revision {
		t.Fatalf("the publication after the cast's late insert: %+v, want the queue changed at it", stateOf(t, pub))
	}
}

// TestAnEditBetweenTwoInsertsStopsTheConsoleWithOneMoreAtMost pins an edit
// of another controller's landing between two of the console's inserts: the
// console stops at its next check, never deletes what it inserted.
func TestAnEditBetweenTwoInsertsStopsTheConsoleWithOneMoreAtMost(t *testing.T) {
	r := newCtlRig(t, ctlOptions{seams: &controlSeams{}})
	inserts := 0
	r.c.seams.afterStep = func(kind, step string, err error) {
		if kind == opKindReplace && step == "insert" {
			inserts++
			if inserts == 1 {
				ref, _ := engine.NewReference(dlna.TrackID("", r.paths()[2]))
				if _, err := r.eng.InsertAfter(context.Background(), 0, ref, nil); err != nil {
					t.Errorf("another controller's insert: %v", err)
				}
			}
		}
	}
	a := r.replace(r.paths(), 0, 0, true)
	rec := wantRecord(t, "a replace raced by an edit", a, admin.PlayerOpChanged, "", admin.PlayerEffectSome)
	if inserts != 1 || len(rec.Entries) != 1 || r.eng.Snapshot().Len() != 2 {
		t.Fatalf("%d console inserts, entries %v, queue %v; want the console stopped after its one", inserts, rec.Entries, r.eng.Snapshot().IDs())
	}
}

// TestARepeatOrAReportBetweenTwoStepsChangesNothingOfTheEdit pins C4: a
// repeat change and an output report landed at the seam between every two
// steps of a replace and of an append move the generation and change no ID,
// so each edit ends done, its queue whole.
func TestARepeatOrAReportBetweenTwoStepsChangesNothingOfTheEdit(t *testing.T) {
	r := newCtlRig(t, ctlOptions{seams: &controlSeams{}})
	reports := uint64(1)
	on := false
	r.c.seams.afterStep = func(kind, step string, err error) {
		if step == "play_id" {
			return
		}
		on = !on
		if err := r.eng.SetRepeat(context.Background(), on); err != nil {
			t.Errorf("a repeat change: %v", err)
		}
		reports++
		if err := r.eng.ReportOutput(context.Background(), engine.OutputReport{Generation: reports, Available: true}); err != nil {
			t.Errorf("an output report: %v", err)
		}
	}
	wantRecord(t, "a replace", r.replace(r.paths(), 1, 0, true), admin.PlayerOpDone, "", "")
	r.wantQueue("after the replace", r.paths()...)
	ap := r.c.Append(context.Background(), admin.PlayerAppend{Op: r.op(), Digest: r.digest(), At: r.gen(), Tracks: r.paths()[:2]})
	wantRecord(t, "an append", ap, admin.PlayerOpDone, "", "")
	r.wantQueue("after the append", append(r.paths(), r.paths()[:2]...)...)
}

// TestAPlayBetweenTheLastInsertAndPlayIDIsOvertakenByIt pins the play's
// window (§2.2): a play of another controller's between the console's last
// check and its PlayID is overtaken by it, the last admitted command
// winning: the console's start track plays, and the edit is done.
func TestAPlayBetweenTheLastInsertAndPlayIDIsOvertakenByIt(t *testing.T) {
	r := newCtlRig(t, ctlOptions{seams: &controlSeams{}})
	inserts := 0
	r.c.seams.afterStep = func(kind, step string, err error) {
		if kind == opKindReplace && step == "insert" {
			if inserts++; inserts == 3 {
				if err := r.eng.Play(context.Background()); err != nil {
					t.Errorf("another controller's play: %v", err)
				}
			}
		}
	}
	a := r.replace(r.paths(), 2, 0, true)
	rec := wantRecord(t, "a replace", a, admin.PlayerOpDone, "", "")
	s := r.await("the start track plays", func(s *engine.Snapshot) bool { return s.State() == engine.Playing && !s.Starting() })
	if s.Selected() != rec.Entries[2] {
		t.Fatalf("%d plays, want the console's start track %d", s.Selected(), rec.Entries[2])
	}
}

// TestAReplaceWhilePlayingPlaysAfterItsOwnRelease pins a replace of a
// playing queue: its DeleteAll releases the session, the inserts run beside
// the release, and its PlayID plays once the release is acknowledged.
func TestAReplaceWhilePlayingPlaysAfterItsOwnRelease(t *testing.T) {
	r := newCtlRig(t, ctlOptions{})
	wantRecord(t, "the first replace", r.replace(r.paths(), 0, 0, true), admin.PlayerOpDone, "", "")
	opened, closed, _ := r.standIn.counts()
	a := r.replace(r.paths()[1:], 0, 0, true)
	wantRecord(t, "a replace while playing", a, admin.PlayerOpDone, "", "")
	o2, c2, _ := r.standIn.counts()
	if c2 != closed+1 || o2 != opened+1 {
		t.Fatalf("opened %d→%d, closed %d→%d: want the old session released and a new one started", opened, o2, closed, c2)
	}
	r.wantQueue("after the second replace", r.paths()[1:]...)
}

// TestAPausedReplaceWaitsForTheReleaseItBegan pins G6 and G10: a replace
// without play of a playing bridge is done, release confirmed, once the
// release is acknowledged, and never changed while the engine releases; with
// the stand-in's Release held past its deadline it is done, release
// unconfirmed, at the engine's deadline; with play, the same release refuses
// the play release_unconfirmed.
func TestAPausedReplaceWaitsForTheReleaseItBegan(t *testing.T) {
	r := newCtlRig(t, ctlOptions{})
	wantRecord(t, "the first replace", r.replace(r.paths(), 0, 0, true), admin.PlayerOpDone, "", "")
	a := r.replace(r.paths(), 1, 0, false)
	rec := wantRecord(t, "a replace without play while playing", a, admin.PlayerOpDone, "", "")
	if rec.Release != releaseConfirmed {
		t.Fatalf("release %q, want confirmed", rec.Release)
	}

	// The release acknowledged before the replace reads the engine after its
	// DeleteAll (held at the seam until the engine shows it stopped) is still
	// the one the replace began: confirmed.
	q := newCtlRig(t, ctlOptions{seams: &controlSeams{}})
	wantRecord(t, "the first replace", q.replace(q.paths(), 0, 0, true), admin.PlayerOpDone, "", "")
	q.c.seams.afterStep = func(kind, step string, err error) {
		if kind != opKindReplace || step != "delete_all" || err != nil {
			return
		}
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		for {
			ch := q.eng.Changed()
			if s := q.eng.Snapshot(); s.State() == engine.Stopped {
				return
			}
			select {
			case <-ch:
			case <-deadline.C:
				t.Errorf("the release DeleteAll began was never acknowledged")
				return
			}
		}
	}
	rec = wantRecord(t, "a replace without play whose release was acknowledged before it looked", q.replace(q.paths(), 1, 0, false),
		admin.PlayerOpDone, "", "")
	if rec.Release != releaseConfirmed {
		t.Fatalf("release %q, want confirmed: the release its DeleteAll began", rec.Release)
	}

	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	h := newCtlRig(t, ctlOptions{holdRelease: hold})
	wantRecord(t, "the first replace", h.replace(h.paths(), 0, 0, true), admin.PlayerOpDone, "", "")
	began := time.Now()
	a = h.replace(h.paths(), 1, 0, false)
	took := time.Since(began)
	rec = wantRecord(t, "a replace without play beside a release held past its deadline", a, admin.PlayerOpDone, "", "")
	if rec.Release != releaseUnconfirmed || took < engine.ReleaseTimeout || took > engine.ReleaseTimeout+controlReleaseGap+2*time.Second {
		t.Fatalf("release %q after %v, want unconfirmed at the engine's deadline (%v)", rec.Release, took, engine.ReleaseTimeout)
	}
	a = h.replace(h.paths(), 0, 0, true)
	rec = wantRecord(t, "a replace and play beside a release unconfirmed", a, admin.PlayerOpRefused, string(engine.ReasonReleaseUnconfirmed), "")
	if rec.Release != releaseUnconfirmed {
		t.Fatalf("release %q, want unconfirmed", rec.Release)
	}
}

// TestEveryLimitIsCheckedBeforeDeleteAll pins step 0 (§6.2, E7): a list none
// of whose tracks passes is refused not_playable with nothing sent, each
// track's word in the record, and the queue as it was.
func TestEveryLimitIsCheckedBeforeDeleteAll(t *testing.T) {
	r := newCtlRig(t, ctlOptions{tracks: append(slices.Clone(defaultCtlTracks),
		ctlTrack{path: "Album/04 D.mp3", codec: "MP3", title: "D", rate: 44100, seconds: 10},
		ctlTrack{path: "Album/05 E.wv", codec: "WAVPACK", title: "E", rate: 44100, seconds: 10})})
	wantRecord(t, "the first replace", r.replace(r.paths(), 0, 0, false), admin.PlayerOpDone, "", "")
	before := r.refs()
	a := r.replace([]string{"Album/04 D.mp3", "Album/05 E.wv", "Album/nothing.flac"}, 0, 0, true)
	rec := wantRecord(t, "a replace no track of which plays", a, admin.PlayerOpRefused, wordNotPlayable, admin.PlayerEffectNone)
	if rec.Sent != 0 {
		t.Fatalf("sent %d steps, want none", rec.Sent)
	}
	words := map[string]string{}
	for _, l := range rec.LeftOut {
		words[l.Path] = l.Word
	}
	want := map[string]string{"Album/04 D.mp3": admin.BridgeWordLossy, "Album/05 E.wv": admin.BridgeWordGated, "Album/nothing.flac": wordNotInLibrary}
	for p, w := range want {
		if words[p] != w {
			t.Fatalf("%s left out as %q, want %q (%v)", p, words[p], w, words)
		}
	}
	if !slices.Equal(r.refs(), before) {
		t.Fatalf("the queue changed: %v", r.refs())
	}
}

// TestRoutedVirtualGatedAndLossyTracksAreLeftOutBeforeTheReplace pins a
// mixed list (§6.2): the tracks that fail are left out with their words, the
// rest queued, before any destructive step.
func TestRoutedVirtualGatedAndLossyTracksAreLeftOutBeforeTheReplace(t *testing.T) {
	r := newCtlRig(t, ctlOptions{tracks: append(slices.Clone(defaultCtlTracks),
		ctlTrack{path: "Album/04 D.mp3", codec: "MP3", title: "D", rate: 44100, seconds: 10},
		ctlTrack{path: "Album/05 E.ape", codec: "APE", title: "E", rate: 44100, seconds: 10})})
	list := []string{r.paths()[0], "Album/04 D.mp3", r.paths()[1], "Album/05 E.ape"}
	a := r.replace(list, 0, 0, false)
	rec := wantRecord(t, "a mixed replace", a, admin.PlayerOpDone, "", "")
	if len(rec.LeftOut) != 2 {
		t.Fatalf("left out %v, want the MP3 and the APE", rec.LeftOut)
	}
	r.wantQueue("the mixed replace", r.paths()[0], r.paths()[1])
}

// TestADSDRateTheSetRefusesIsLeftOutBeforeTheReplace pins the DSD way (§6.2,
// step 4): a DSD128 the set refuses is left out with the set's word, a DSD64
// the set converts is queued, and a DSD512 the set names nowhere is
// dsd_rate_not_played.
func TestADSDRateTheSetRefusesIsLeftOutBeforeTheReplace(t *testing.T) {
	policy := playback.PlayerPolicy{
		Converted: []playback.ConvertedRate{{BitRate: 2822400}},
		DSD:       &playback.DSDView{Refused: []playback.DSDRefusal{{BitRate: 5644800, Reason: "dsd_refused_by_choice"}}},
	}
	r := newCtlRig(t, ctlOptions{policy: policy, tracks: []ctlTrack{
		{path: "DSD/64.dsf", codec: "DSF", title: "64", dsd: true, rate: 2822400, seconds: 60},
		{path: "DSD/128.dsf", codec: "DSF", title: "128", dsd: true, rate: 5644800, seconds: 60},
		{path: "DSD/512.dsf", codec: "DSF", title: "512", dsd: true, rate: 22579200, seconds: 60},
	}})
	a := r.replace([]string{"DSD/64.dsf", "DSD/128.dsf", "DSD/512.dsf"}, 0, 0, false)
	rec := wantRecord(t, "a DSD replace", a, admin.PlayerOpDone, "", "")
	words := map[string]string{}
	for _, l := range rec.LeftOut {
		words[l.Path] = l.Word
	}
	if words["DSD/128.dsf"] != "dsd_refused_by_choice" || words["DSD/512.dsf"] != admin.BridgeWordDSDNotPlayed || len(words) != 2 {
		t.Fatalf("left out %v, want DSD128 by the set's word and DSD512 as not played", words)
	}
	r.wantQueue("the DSD replace", "DSD/64.dsf")
}

// TestALongListIsWindowedWithPreviousIntact pins the window (§6.3): a list of
// more tracks than the queue takes keeps the start track, a hundred before
// it, and as many after it as the queue takes, and the record says which.
func TestALongListIsWindowedWithPreviousIntact(t *testing.T) {
	r := newCtlRig(t, ctlOptions{})
	var list []string
	for i := 0; i < 1500; i++ {
		list = append(list, r.paths()[i%3])
	}
	a := r.replace(list, 600, 0, false)
	rec := wantRecord(t, "a long replace", a, admin.PlayerOpDone, "", "")
	if w := rec.Window; w == nil || w.First != 501 || w.Last != 1500 || w.Total != 1500 {
		t.Fatalf("window %+v, want tracks 501 to 1500 of 1500", rec.Window)
	}
	s := r.eng.Snapshot()
	if s.Len() != engine.MaxEntries {
		t.Fatalf("the queue holds %d, want %d", s.Len(), engine.MaxEntries)
	}
	if idx, _ := s.IndexOf(s.Selected()); idx != 100 {
		t.Fatalf("the start track is at %d, want 100: a hundred before it for Previous", idx)
	}
	if lo, hi := window(make([]entryIn, 1500), 1450); lo != 500 || hi != 1499 {
		t.Fatalf("a start near the end windows %d to %d, want 500 to 1499: it reaches further back", lo, hi)
	}
}

// TestAnAppendThatDoesNotFitIsRefusedWhole pins the append's room (§6.4,
// E7): an append the queue has no room for is refused queue_full with the
// room left, nothing inserted.
func TestAnAppendThatDoesNotFitIsRefusedWhole(t *testing.T) {
	r := newCtlRig(t, ctlOptions{})
	var list []string
	for i := 0; i < engine.MaxEntries-2; i++ {
		list = append(list, r.paths()[0])
	}
	wantRecord(t, "a replace near full", r.replace(list, 0, 0, false), admin.PlayerOpDone, "", "")
	before := r.eng.Snapshot().Len()
	ap := r.c.Append(context.Background(), admin.PlayerAppend{Op: r.op(), Digest: r.digest(), At: r.gen(), Tracks: r.paths()})
	rec := wantRecord(t, "an append past the room", ap, admin.PlayerOpRefused, wordQueueFull, admin.PlayerEffectNone)
	if rec.Room == nil || rec.Room.Entries != 2 || rec.Room.Wanted != 3 || rec.Sent != 0 {
		t.Fatalf("room %+v, sent %d; want room for 2 of 3 and nothing sent", rec.Room, rec.Sent)
	}
	if r.eng.Snapshot().Len() != before {
		t.Fatal("the append inserted something")
	}
}

// TestAnAppendToAnEmptyQueueSelectsItsFirstTrackAndPlaysNothing pins §6.4:
// the engine selects the entry an insert puts into an empty queue, and the
// console starts nothing the user only added.
func TestAnAppendToAnEmptyQueueSelectsItsFirstTrackAndPlaysNothing(t *testing.T) {
	r := newCtlRig(t, ctlOptions{})
	ap := r.c.Append(context.Background(), admin.PlayerAppend{Op: r.op(), Digest: r.digest(), At: r.gen(), Tracks: r.paths()})
	rec := wantRecord(t, "an append to an empty queue", ap, admin.PlayerOpDone, "", "")
	s := r.eng.Snapshot()
	if s.Selected() != rec.Entries[0] || s.State() != engine.Stopped {
		t.Fatalf("%s on %d, want stopped on the first track %d", s.State(), s.Selected(), rec.Entries[0])
	}
}

// TestAnAppendAddsAtTheEndFromTheTabsGeneration pins the append (§2.4): its
// tracks after the queue's last entry, its first insert under the tab's
// generation.
func TestAnAppendAddsAtTheEndFromTheTabsGeneration(t *testing.T) {
	r := newCtlRig(t, ctlOptions{})
	wantRecord(t, "a replace", r.replace(r.paths()[:1], 0, 0, true), admin.PlayerOpDone, "", "")
	ap := r.c.Append(context.Background(), admin.PlayerAppend{Op: r.op(), Digest: r.digest(), At: r.gen(), Tracks: r.paths()[1:]})
	wantRecord(t, "an append", ap, admin.PlayerOpDone, "", "")
	r.wantQueue("after the append", r.paths()...)
}

// TestAnAppendRefusedStaleAtItsReadOrItsFirstInsertChangesNothing pins G7:
// a base read at a moved generation refuses with nothing sent; a change
// landed between the read and the first insert refuses that insert, both
// effect none.
func TestAnAppendRefusedStaleAtItsReadOrItsFirstInsertChangesNothing(t *testing.T) {
	r := newCtlRig(t, ctlOptions{})
	stale := r.gen()
	wantRecord(t, "a repeat change", r.command(admin.PlayerCmdRepeat, func(c *admin.PlayerCommand) { c.On = true }), admin.PlayerOpDone, "", "")
	ap := r.c.Append(context.Background(), admin.PlayerAppend{Op: r.op(), Digest: r.digest(), At: stale, Tracks: r.paths()})
	rec := wantRecord(t, "an append stale at its read", ap, admin.PlayerOpRefused, wordStale, admin.PlayerEffectNone)
	if rec.Sent != 0 {
		t.Fatalf("sent %d, want nothing", rec.Sent)
	}
	// A change landed at the seam between the base's read and the first
	// insert: the insert's own condition refuses it.
	r2 := newCtlRig(t, ctlOptions{seams: &controlSeams{}})
	r2.c.seams.afterBase = func() {
		if err := r2.eng.SetRepeat(context.Background(), true); err != nil {
			t.Errorf("another controller's repeat change: %v", err)
		}
	}
	ap = r2.c.Append(context.Background(), admin.PlayerAppend{Op: r2.op(), Digest: r2.digest(), At: r2.gen(), Tracks: r2.paths()})
	rec = wantRecord(t, "an append whose first insert met a moved generation", ap, admin.PlayerOpRefused, wordStale, admin.PlayerEffectNone)
	if rec.Sent != 1 || r2.eng.Snapshot().Len() != 0 {
		t.Fatalf("sent %d, queue %d; want the first insert sent and refused, nothing inserted", rec.Sent, r2.eng.Snapshot().Len())
	}
}

// TestTheTransportPausesAndPlays pins pause and play: done once the engine
// answered nil, and for pause once the capture shows the transport paused.
func TestTheTransportPausesAndPlays(t *testing.T) {
	r := newCtlRig(t, ctlOptions{})
	wantRecord(t, "a replace", r.replace(r.paths(), 0, 0, true), admin.PlayerOpDone, "", "")
	a := r.command(admin.PlayerCmdPause, nil)
	wantRecord(t, "pause", a, admin.PlayerOpDone, "", "")
	if st := stateOf(t, a.Player); st.Transport != "paused" {
		t.Fatalf("the pause's publication reads %s, want paused", st.Transport)
	}
	wantRecord(t, "play", r.command(admin.PlayerCmdPlay, nil), admin.PlayerOpDone, "", "")
	r.await("playing again", func(s *engine.Snapshot) bool { return s.State() == engine.Playing })
}

// TestTheTransportNextAndPrevious pins next and previous, each under the
// tab's generation.
func TestTheTransportNextAndPrevious(t *testing.T) {
	r := newCtlRig(t, ctlOptions{})
	rec := wantRecord(t, "a replace", r.replace(r.paths(), 0, 0, true), admin.PlayerOpDone, "", "")
	wantRecord(t, "next", r.command(admin.PlayerCmdNext, nil), admin.PlayerOpDone, "", "")
	r.await("the second track plays", func(s *engine.Snapshot) bool { return s.Selected() == rec.Entries[1] && s.State() == engine.Playing })
	wantRecord(t, "previous", r.command(admin.PlayerCmdPrevious, nil), admin.PlayerOpDone, "", "")
	r.await("the first track plays", func(s *engine.Snapshot) bool { return s.Selected() == rec.Entries[0] && s.State() == engine.Playing })
}

// TestTheTransportTakesRepeatAllAlone pins R10: repeat is repeat-all, on or
// off, and the transport has no shuffle and no repeat-one.
func TestTheTransportTakesRepeatAllAlone(t *testing.T) {
	r := newCtlRig(t, ctlOptions{})
	wantRecord(t, "repeat on", r.command(admin.PlayerCmdRepeat, func(c *admin.PlayerCommand) { c.On = true }), admin.PlayerOpDone, "", "")
	if !r.eng.Snapshot().Repeat() {
		t.Fatal("repeat is off")
	}
	for _, c := range admin.PlayerCommands() {
		if strings.Contains(c, "shuffle") || c == "repeat_one" {
			t.Fatalf("the transport takes %s", c)
		}
	}
}

// TestShuffleIsNoControlOp pins R10: there is no shuffle command, and the
// engine's shuffle stays off whatever the console sends.
func TestShuffleIsNoControlOp(t *testing.T) {
	want := []string{admin.PlayerCmdPlay, admin.PlayerCmdPause, admin.PlayerCmdStop, admin.PlayerCmdNext, admin.PlayerCmdPrevious,
		admin.PlayerCmdIndex, admin.PlayerCmdSeek, admin.PlayerCmdRepeat}
	if got := admin.PlayerCommands(); !slices.Equal(got, want) || slices.Contains(got, "shuffle") {
		t.Fatalf("the transport's commands %v, want %v and no shuffle", got, want)
	}
	r := newCtlRig(t, ctlOptions{})
	wantRecord(t, "a replace that plays", r.replace(r.paths(), 0, 0, true), admin.PlayerOpDone, "", "")
	st := stateOf(t, r.c.Now(context.Background()))
	if !slices.Contains(st.Commands, admin.PlayerCmdRepeat) || slices.Contains(st.Commands, "shuffle") {
		t.Fatalf("the state offers %v, want repeat and no shuffle", st.Commands)
	}
	if r.eng.Snapshot().Shuffle() {
		t.Fatal("the engine shuffles")
	}
}

// TestTheSeeksRetryFollowsItsOwnEntryAlone pins the seek (§2.2): it follows
// the console's PlayID under the generation of a snapshot that shows the
// console's entry selected and playing, and a play of another entry before
// it leaves the track playing from its start, the record saying why.
func TestTheSeeksRetryFollowsItsOwnEntryAlone(t *testing.T) {
	r := newCtlRig(t, ctlOptions{})
	a := r.replace(r.paths(), 1, 30, true)
	rec := wantRecord(t, "a replace with a position", a, admin.PlayerOpDone, "", "")
	if rec.Seek != "" {
		t.Fatalf("the seek was not made: %q", rec.Seek)
	}
	r.standIn.mu.Lock()
	last := r.standIn.starts[len(r.standIn.starts)-1]
	r.standIn.mu.Unlock()
	if last.From != 30*44100 {
		t.Fatalf("the last start was from frame %d, want %d", last.From, 30*44100)
	}

	r2 := newCtlRig(t, ctlOptions{seams: &controlSeams{}})
	r2.c.seams.afterStep = func(kind, step string, err error) {
		if step == "play_id" {
			if err := r2.eng.Next(context.Background()); err != nil {
				t.Errorf("another controller's next: %v", err)
			}
			r2.await("the next entry plays", func(s *engine.Snapshot) bool { return s.State() == engine.Playing && !s.Starting() })
		}
	}
	a = r2.replace(r2.paths(), 0, 30, true)
	rec = a.Record
	if rec == nil || rec.Seek != "another_entry_plays" {
		t.Fatalf("a seek after another controller's play: %+v, want it skipped, another entry playing", rec)
	}
}

// TestAHandoverSeeksPCMAndNotDSD pins R5: a switch to the bridge seeks PCM of
// known length to its position, and plays DSD from its start.
func TestAHandoverSeeksPCMAndNotDSD(t *testing.T) {
	r := newCtlRig(t, ctlOptions{policy: playback.PlayerPolicy{Converted: []playback.ConvertedRate{{BitRate: 2822400}}},
		tracks: append(slices.Clone(defaultCtlTracks), ctlTrack{path: "DSD/64.dsf", codec: "DSF", title: "64", dsd: true, rate: 2822400, seconds: 60})})
	a := r.replace([]string{"DSD/64.dsf"}, 0, 20, true)
	rec := wantRecord(t, "a DSD handover with a position", a, admin.PlayerOpDone, "", "")
	if rec.Seek != "dsd_or_unknown_length" {
		t.Fatalf("seek %q, want none for DSD", rec.Seek)
	}
	a = r.replace(r.paths(), 0, 20, true)
	rec = wantRecord(t, "a PCM handover with a position", a, admin.PlayerOpDone, "", "")
	if rec.Seek != "" {
		t.Fatalf("seek %q, want the seek made", rec.Seek)
	}
}

// TestAResentOperationNeverRunsTwice pins G5: a request sent again at its
// tab's mark answers the first record and runs nothing; one below the mark
// is replayed; one sent for another epoch is epoch_changed.
func TestAResentOperationNeverRunsTwice(t *testing.T) {
	r := newCtlRig(t, ctlOptions{})
	op := r.op()
	d := r.digest()
	req := admin.PlayerReplace{Op: op, Digest: d, At: r.gen(), Tracks: r.paths(), Play: true}
	first := r.c.Replace(context.Background(), req)
	wantRecord(t, "the first send", first, admin.PlayerOpDone, "", "")
	_, _, starts := r.standIn.counts()
	again := r.c.Replace(context.Background(), req)
	if again.Record == nil || again.Record.Seq != op.Seq || again.Record.Status != admin.PlayerOpDone {
		t.Fatalf("a resend at the mark: %+v, want the first record", again)
	}
	if _, _, s := r.standIn.counts(); s != starts {
		t.Fatalf("a resend started %d sessions", s-starts)
	}
	wantRecord(t, "a newer op", r.command(admin.PlayerCmdRepeat, func(c *admin.PlayerCommand) { c.On = true }), admin.PlayerOpDone, "", "")
	if a := r.c.Replace(context.Background(), req); a.Refused != "replayed" {
		t.Fatalf("a resend below the mark: %+v, want replayed", a)
	}
	other := req
	other.Op.Epoch = strings.Repeat("0", 32)
	other.Op.Seq = 99
	if a := r.c.Replace(context.Background(), other); a.Refused != "epoch_changed" {
		t.Fatalf("a send of another epoch: %+v, want epoch_changed", a)
	}
	// Round 2's rule kept 256 ids and ran a resend past them again: a tab's
	// mark is kept for 4,096 tabs, whatever the others did.
	for i := 0; i < 300; i++ {
		tab := fmt.Sprintf("%032x", i+1)
		if a := r.c.Transport(context.Background(), admin.PlayerCommand{Op: admin.PlayerOp{Tab: tab, Seq: 1, Epoch: r.c.epoch}, Command: admin.PlayerCmdStop}); a.Record == nil {
			t.Fatalf("tab %d's stop: %+v", i, a)
		}
	}
	if a := r.c.Replace(context.Background(), req); a.Refused != "replayed" {
		t.Fatalf("a resend below the mark after 300 tabs: %+v, want replayed", a)
	}
}

// TestTwoTabsFromOneSessionStorageNeverShareARecord pins J5: two tabs started
// from identical session storage send different commands under one identity:
// the second is refused operation_conflict and changes no record, never
// answered with the first command's record.
func TestTwoTabsFromOneSessionStorageNeverShareARecord(t *testing.T) {
	r := newCtlRig(t, ctlOptions{})
	op := r.op()
	first := r.c.Transport(context.Background(), admin.PlayerCommand{Op: op, Digest: sha256.Sum256([]byte("repeat on")), At: r.gen(), Command: admin.PlayerCmdRepeat, On: true})
	wantRecord(t, "the first tab's command", first, admin.PlayerOpDone, "", "")
	second := r.c.Transport(context.Background(), admin.PlayerCommand{Op: op, Digest: sha256.Sum256([]byte("stop")), Command: admin.PlayerCmdStop})
	if second.Refused != "operation_conflict" || second.Record != nil {
		t.Fatalf("the second tab's command under the same identity: %+v, want operation_conflict and no record", second)
	}
	rb := r.c.Operation(r.c.epoch, op.Tab, op.Seq)
	if rb.Record == nil || rb.Record.Command != admin.PlayerCmdRepeat {
		t.Fatalf("the record read back: %+v, want the first command's, unchanged", rb.Record)
	}
}

// TestATabsLastOperationCanBeReadBackWhateverOtherTabsDid pins G5's bounds: a
// tab's switch-out whose answer is lost, then twenty operations from nine
// other tabs: the tab reads its record back, its handover included; a 65th
// tab's operation takes the oldest tab's record (operation_gone for it).
func TestATabsLastOperationCanBeReadBackWhateverOtherTabsDid(t *testing.T) {
	r := newCtlRig(t, ctlOptions{})
	wantRecord(t, "a replace", r.replace(r.paths(), 1, 0, true), admin.PlayerOpDone, "", "")
	op := r.op()
	a := r.c.SwitchOut(context.Background(), admin.PlayerSwitchOut{Op: op, Digest: r.digest(), Take: true})
	wantRecord(t, "the switch-out", a, admin.PlayerOpDone, "", "")
	for i := 0; i < 20; i++ {
		tab := fmt.Sprintf("%032x", i%9+1)
		r.c.Transport(context.Background(), admin.PlayerCommand{Op: admin.PlayerOp{Tab: tab, Seq: uint64(i + 1), Epoch: r.c.epoch}, Command: admin.PlayerCmdStop})
	}
	rb := r.c.Operation(r.c.epoch, op.Tab, op.Seq)
	if rb.Record == nil || rb.Record.Handover == nil || len(rb.Record.Handover.Tracks) != 3 {
		t.Fatalf("the tab's record after twenty other operations: %+v, want it with its handover", rb.Record)
	}
	for i := 0; i < controlRecordTabs; i++ {
		tab := fmt.Sprintf("%032x", 1000+i)
		r.c.Transport(context.Background(), admin.PlayerCommand{Op: admin.PlayerOp{Tab: tab, Seq: 1, Epoch: r.c.epoch}, Command: admin.PlayerCmdStop})
	}
	if rb := r.c.Operation(r.c.epoch, op.Tab, op.Seq); rb.Refused != "operation_gone" {
		t.Fatalf("the tab's record after %d other tabs: %+v, want operation_gone", controlRecordTabs, rb)
	}
	if a := r.c.SwitchOut(context.Background(), admin.PlayerSwitchOut{Op: op, Digest: r.digest()}); a.Refused != "operation_gone" {
		t.Fatalf("a resend at the mark whose record is gone: %+v, want operation_gone", a)
	}
}

// TestARefusalBeforeTheFirstStepIsStillARecord pins J3: a request refused
// player_not_running, not_playable or queue_full advances its tab's mark and
// stores a final record, so a tab that lost the answer reads back how it
// ended; its effect is none.
func TestARefusalBeforeTheFirstStepIsStillARecord(t *testing.T) {
	r := newCtlRig(t, ctlOptions{})
	for _, tc := range []struct {
		name, word string
		send       func() admin.PlayerAnswer
	}{
		{"not_playable", wordNotPlayable, func() admin.PlayerAnswer { return r.replace([]string{"nothing.flac"}, 0, 0, true) }},
		{"queue_full", wordQueueFull, func() admin.PlayerAnswer {
			var list []string
			for i := 0; i < engine.MaxEntries; i++ {
				list = append(list, r.paths()[0])
			}
			wantRecord(t, "a full queue", r.replace(list, 0, 0, false), admin.PlayerOpDone, "", "")
			return r.c.Append(context.Background(), admin.PlayerAppend{Op: r.op(), Digest: r.digest(), At: r.gen(), Tracks: r.paths()})
		}},
	} {
		a := tc.send()
		rec := wantRecord(t, tc.name, a, admin.PlayerOpRefused, tc.word, admin.PlayerEffectNone)
		rb := r.c.Operation(r.c.epoch, r.tab, rec.Seq)
		if rb.Record == nil || rb.Record.Error != tc.word {
			t.Fatalf("%s read back: %+v", tc.name, rb)
		}
	}
	r.stopEngine()
	a := r.command(admin.PlayerCmdStop, nil)
	rec := wantRecord(t, "a stop of an engine that has closed", a, admin.PlayerOpRefused, wordNotRunning, admin.PlayerEffectNone)
	if rec.Reason != reasonClosed {
		t.Fatalf("reason %q, want closed", rec.Reason)
	}
	if rb := r.c.Operation(r.c.epoch, r.tab, rec.Seq); rb.Record == nil || rb.Record.Error != wordNotRunning {
		t.Fatalf("player_not_running read back: %+v", rb)
	}
}

// TestAnOperationForAnotherEpochNeverRuns pins G2: an operation sent for
// another epoch than the control's never runs, and stores nothing.
func TestAnOperationForAnotherEpochNeverRuns(t *testing.T) {
	r := newCtlRig(t, ctlOptions{})
	op := admin.PlayerOp{Tab: r.tab, Seq: 1, Epoch: strings.Repeat("f", 32)}
	a := r.c.Replace(context.Background(), admin.PlayerReplace{Op: op, Digest: r.digest(), At: r.gen(), Tracks: r.paths(), Play: true})
	if a.Refused != "epoch_changed" {
		t.Fatalf("an operation of another epoch: %+v, want epoch_changed", a)
	}
	if r.eng.Snapshot().Len() != 0 {
		t.Fatal("it ran")
	}
	if rb := r.c.Operation(r.c.epoch, r.tab, 1); rb.Refused != "operation_gone" {
		t.Fatalf("it stored a record: %+v", rb)
	}
}

// TestTheReadBackCarriesItsEpochAndHydrates pins J4: a read back answers the
// epoch and the latest revision beside the record, refuses another epoch,
// and names the handover's tracks by their paths; across a restart (a new
// control, a new epoch) the old epoch's read back is epoch_changed.
func TestTheReadBackCarriesItsEpochAndHydrates(t *testing.T) {
	r := newCtlRig(t, ctlOptions{})
	wantRecord(t, "a replace", r.replace(r.paths(), 0, 0, true), admin.PlayerOpDone, "", "")
	op := r.op()
	wantRecord(t, "the switch-out", r.c.SwitchOut(context.Background(), admin.PlayerSwitchOut{Op: op, Digest: r.digest(), Take: true}), admin.PlayerOpDone, "", "")
	rb := r.c.Operation(r.c.epoch, op.Tab, op.Seq)
	if rb.Epoch != r.c.epoch || rb.Revision == 0 || rb.Record == nil {
		t.Fatalf("the read back: %+v", rb)
	}
	if !slices.Equal(rb.HandoverPaths, r.paths()) {
		t.Fatalf("the handover's paths %v, want %v", rb.HandoverPaths, r.paths())
	}
	restarted := newConsoleControl(consoleControlWiring{Plan: r.c.plan, Player: r.p, View: r.view, Store: r.store, Hosts: r.c.hosts, GOOS: "linux"})
	t.Cleanup(restarted.stop)
	if restarted.epoch == r.c.epoch {
		t.Fatal("a new control drew the same epoch")
	}
	if got := restarted.Operation(r.c.epoch, op.Tab, op.Seq); got.Refused != "epoch_changed" || got.Epoch != restarted.epoch {
		t.Fatalf("a read back across a restart: %+v, want epoch_changed with the new epoch", got)
	}
}

// TestEveryOperationsOutcomeIsPublished pins E3: an operation's record is in
// the publication its answer carries and in the next frame, so a tab that
// reloads between its request and its answer finds its own record in the
// stream's state and reads it back whole.
func TestEveryOperationsOutcomeIsPublished(t *testing.T) {
	r := newCtlRig(t, ctlOptions{})
	a := r.replace(r.paths(), 0, 0, true)
	rec := wantRecord(t, "a replace", a, admin.PlayerOpDone, "", "")
	latest, ok := r.c.Latest()
	if !ok {
		t.Fatal("no publication")
	}
	found := false
	for _, o := range stateOf(t, latest).Operations {
		if o.Tab == rec.Tab && o.Seq == rec.Seq && o.Status == admin.PlayerOpDone {
			found = true
		}
	}
	if !found {
		t.Fatalf("the latest publication's operations %+v hold no record of %d", stateOf(t, latest).Operations, rec.Seq)
	}
	if rb := r.c.Operation(r.c.epoch, rec.Tab, rec.Seq); rb.Record == nil || rb.Record.Status != admin.PlayerOpDone {
		t.Fatalf("read back: %+v", rb)
	}
}

// TestOnlyAKnownPreEffectRefusalIsEffectNone pins J2 and K8: the engine's
// answers that prove no effect (a refusal stale, under a hold, of an output,
// configuring, an empty queue, an entry not found, a seek's three) are effect
// none; a backend's timeout of a call (which releases the session), the
// engine's shutdown and a start that failed after the selection moved are
// unknown, never none.
func TestOnlyAKnownPreEffectRefusalIsEffectNone(t *testing.T) {
	none := map[string]error{
		"play":       &engine.Error{Reason: engine.ReasonEmptyQueue},
		"play_id":    &engine.Error{Reason: engine.ReasonIDNotFound},
		"play_index": &engine.Error{Reason: engine.ReasonIndexNotFound},
		"next":       &engine.Error{Reason: engine.ReasonOutputUnavailable},
		"previous":   &engine.Error{Reason: engine.ReasonConfiguring},
		"seek":       &engine.Error{Reason: engine.ReasonSeekUnsupported},
		"delete_all": &engine.Error{Reason: engine.ReasonConflict},
		"insert":     &engine.Error{Reason: engine.ReasonFull},
		"pause":      &engine.Error{Reason: engine.ReasonMaintenance},
		"stop":       &engine.Error{Reason: engine.ReasonOvertaken},
	}
	for step, err := range none {
		if got := effectOfStep(step, err); got != admin.PlayerEffectNone {
			t.Errorf("%s answered %v: effect %s, want none", step, err, got)
		}
	}
	unknown := map[string]error{
		"pause":   &engine.Error{Reason: engine.ReasonTimeout},
		"play_id": &engine.Error{Reason: engine.ReasonBackend},
		"next":    &engine.Error{Reason: engine.ReasonAllUnplayable},
		"play":    &engine.Error{Reason: engine.ReasonUnplayable},
		"insert":  &engine.Error{Reason: engine.ReasonClosed},
		"seek":    &engine.Error{Reason: engine.ReasonSuperseded},
		"index":   &engine.Error{Reason: "occurrence_budget_exceeded"},
	}
	for step, err := range unknown {
		if got := effectOfStep(step, err); got != admin.PlayerEffectUnknown {
			t.Errorf("%s answered %v: effect %s, want unknown", step, err, got)
		}
	}
	// Through the engine: a Start refused by the stand-in after PlayID
	// moved the selection.
	r := newCtlRig(t, ctlOptions{})
	wantRecord(t, "a replace", r.replace(r.paths(), 0, 0, false), admin.PlayerOpDone, "", "")
	id := dlna.TrackID("", r.paths()[1])
	r.standIn.refuse = map[string]error{"http://192.168.1.20:7790/dlna/file/" + id: errors.New("the stand-in refuses the start")}
	a := r.command(admin.PlayerCmdIndex, func(c *admin.PlayerCommand) { c.Index = 1 })
	if a.Record == nil || a.Record.Effect != admin.PlayerEffectUnknown {
		t.Fatalf("an index whose start failed after the selection moved: %+v, want effect unknown", a.Record)
	}
}

// TestARefusalThroughThePlayWaiterIsAnEffectNotKnown pins the waiter path: a
// play the engine admitted, whose rule moved the selection and released or
// started a session before it waited, and which the engine then refused
// (release_unconfirmed at the release's deadline, maintenance as a hold
// began, output_unavailable as the output went) is effect unknown, whatever
// the word; the same word refused before anything changed stays none.
func TestARefusalThroughThePlayWaiterIsAnEffectNotKnown(t *testing.T) {
	t.Run("release_unconfirmed at the release's deadline", func(t *testing.T) {
		hold := make(chan struct{})
		t.Cleanup(func() { close(hold) })
		r := newCtlRig(t, ctlOptions{holdRelease: hold})
		wantRecord(t, "a replace and play", r.replace(r.paths(), 0, 0, true), admin.PlayerOpDone, "", "")
		a := r.command(admin.PlayerCmdIndex, func(c *admin.PlayerCommand) { c.Index = 1 })
		wantRecord(t, "an index whose release stays unconfirmed past its deadline", a, admin.PlayerOpRefused,
			string(engine.ReasonReleaseUnconfirmed), admin.PlayerEffectUnknown)
		if s := r.eng.Snapshot(); s.Selected() != s.IDs()[1] {
			t.Fatalf("the selection is %d, want the index's entry %d: the index changed it", s.Selected(), s.IDs()[1])
		}
		a = r.command(admin.PlayerCmdIndex, func(c *admin.PlayerCommand) { c.Index = 0 })
		wantRecord(t, "an index refused before it changed anything", a, admin.PlayerOpRefused,
			string(engine.ReasonReleaseUnconfirmed), admin.PlayerEffectNone)
	})
	for _, tc := range []struct {
		name, word string
		refuse     func(r *ctlRig)
	}{
		{"maintenance as a hold begins", string(engine.ReasonMaintenance), func(r *ctlRig) {
			if _, err := r.eng.EnterMaintenance(context.Background(), r.eng.Snapshot().Generation(), "a test's hold"); err != nil {
				r.t.Fatal(err)
			}
		}},
		{"output_unavailable as the output goes", string(engine.ReasonOutputUnavailable), func(r *ctlRig) { r.report(2, false, "") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := "http://192.168.1.20:7790/dlna/file/" + dlna.TrackID("", defaultCtlTracks[0].path)
			hold := make(chan struct{})
			r := newCtlRig(t, ctlOptions{holdStart: map[string]chan struct{}{first: hold}})
			t.Cleanup(func() { close(hold) })
			wantRecord(t, "a replace", r.replace(r.paths(), 0, 0, false), admin.PlayerOpDone, "", "")
			indexed := make(chan admin.PlayerAnswer, 1)
			go func() { indexed <- r.command(admin.PlayerCmdIndex, func(c *admin.PlayerCommand) { c.Index = 0 }) }()
			r.standIn.await(t, "the index's Start held", func(b *playerStandIn) bool { return b.parked == 1 })
			tc.refuse(r)
			rec := wantRecord(t, "an index the engine refused as it waited", <-indexed, admin.PlayerOpRefused, tc.word,
				admin.PlayerEffectUnknown)
			if rec.Last != tc.word {
				t.Fatalf("the index answered %q, want %s", rec.Last, tc.word)
			}
		})
	}
}

// TestAStopIsSentAtOnceBesideAnOperationInFlight pins §2.3: a Stop does not
// wait for an older operation's step in flight, which a held reservation
// write parks, and the engine takes it after that step.
func TestAStopIsSentAtOnceBesideAnOperationInFlight(t *testing.T) {
	hold, held, seams := heldReservations()
	r := newCtlRig(t, ctlOptions{engSeams: seams})
	t.Cleanup(func() {
		select {
		case <-hold:
		default:
			close(hold)
		}
	})
	replaced := make(chan admin.PlayerAnswer, 1)
	go func() { replaced <- r.replace(r.paths(), 0, 0, false) }()
	select {
	case <-held:
	case <-serveGiveUp(t):
		t.Fatal("no reservation write was held")
	}
	stop := r.command(admin.PlayerCmdStop, nil)
	wantRecord(t, "a stop beside a parked insert", stop, admin.PlayerOpDone, "", "")
	close(hold)
	a := <-replaced
	if a.Record == nil || a.Record.Status != admin.PlayerOpCancelled {
		t.Fatalf("the replace a stop ended: %+v, want cancelled", a.Record)
	}
}

// TestANewerOperationEndsTheOldersUnsentSteps pins C1: a newer operation
// ends the older one's steps not yet sent; the older sends nothing more and
// its record is cancelled with what it sent.
func TestANewerOperationEndsTheOldersUnsentSteps(t *testing.T) {
	r := newCtlRig(t, ctlOptions{seams: &controlSeams{}})
	at := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	r.c.seams.afterStep = func(kind, step string, err error) {
		if kind == opKindReplace && step == "insert" {
			once.Do(func() { close(at); <-release })
		}
	}
	replaced := make(chan admin.PlayerAnswer, 1)
	go func() { replaced <- r.replace(r.paths(), 0, 0, true) }()
	<-at
	newer := make(chan admin.PlayerAnswer, 1)
	go func() {
		newer <- r.command(admin.PlayerCmdRepeat, func(c *admin.PlayerCommand) { c.On = true; c.At = "" })
	}()
	waitEnded(t, r.c, opKindTransport)
	close(release)
	a := <-replaced
	if a.Record == nil || a.Record.Status != admin.PlayerOpCancelled || a.Record.Sent != 2 {
		t.Fatalf("the older replace: %+v, want cancelled after its DeleteAll and one insert", a.Record)
	}
	<-newer
	if _, _, starts := r.standIn.counts(); starts != 0 {
		t.Fatalf("%d sessions started: the older operation played", starts)
	}
	if r.eng.Snapshot().Len() != 1 {
		t.Fatalf("the queue holds %d, want the older one's one insert", r.eng.Snapshot().Len())
	}
}

// waitEnded waits until the operation the control admitted last is the newer
// one, of kind: the older ones' unsent steps are ended by then, in the same
// admission.
func waitEnded(t *testing.T, c *consoleControl, kind string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		var last string
		if n := len(c.recent); n > 0 {
			last = c.recent[n-1].rec.Kind
		}
		c.mu.Unlock()
		if last == kind {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("the newer operation (%s) was never admitted", kind)
}

// TestALostReplaceHeldBetweenStepsThenAnotherPickNeverPlaysBoth pins C1's
// case: a replace held after its first insert, past the handler's wait (its
// answer running), its connection dropped after that, which no handler sees,
// and then the bar's pick of this browser: the switch-out ends the replace's
// unsent steps, no console insert and no play reaches the engine after the
// switch-out's Pause, and the switch-out is done only once the replace's
// record reads cancelled.
func TestALostReplaceHeldBetweenStepsThenAnotherPickNeverPlaysBoth(t *testing.T) {
	r := newCtlRig(t, ctlOptions{seams: &controlSeams{handlerWait: 200 * time.Millisecond}})
	at := make(chan struct{})
	release := make(chan struct{})
	var once, letGo sync.Once
	free := func() { letGo.Do(func() { close(release) }) }
	t.Cleanup(free)
	var afterPause atomic.Int32
	r.c.seams.afterStep = func(kind, step string, err error) {
		switch {
		case kind == opKindReplace && step == "insert":
			once.Do(func() { close(at); <-release })
		case kind == opKindSwitchOut && step == "pause":
			afterPause.Store(1)
		case kind == opKindReplace && afterPause.Load() == 1:
			t.Errorf("the replace sent %s after the switch-out's pause", step)
		}
	}
	held := r.c.Replace(context.Background(), admin.PlayerReplace{Op: r.op(), Digest: r.digest(), At: r.gen(), Tracks: r.paths(), Play: true})
	<-at
	if held.Record == nil || held.Record.Status != admin.PlayerOpRunning {
		t.Fatalf("the replace held past the handler's wait answered %+v, want running", held.Record)
	}
	switched := make(chan admin.PlayerAnswer, 1)
	go func() { switched <- r.switchOut(true) }()
	waitEnded(t, r.c, opKindSwitchOut)
	free()
	a := <-switched
	wantRecord(t, "the switch-out", a, admin.PlayerOpDone, "", "")
	if _, _, starts := r.standIn.counts(); starts != 0 {
		t.Fatalf("%d sessions started: the lost replace played beside the browser", starts)
	}
	// The tab's last record is the switch-out's now: the lost replace's is in
	// the switch-out's publication, final before the switch-out began.
	var lost *admin.PlayerRecord
	for _, rec := range stateOf(t, a.Player).Operations {
		if rec.Tab == r.tab && rec.Seq == 1 {
			lost = &rec
		}
	}
	if lost == nil || lost.Status != admin.PlayerOpCancelled {
		t.Fatalf("the lost replace's record in the switch-out's publication: %+v, want cancelled", lost)
	}
}

// TestAStepAStopOvertookInFlightReachesTheEngineBeforeTheSwitchOutsPause
// pins the Stop's order: a replace's PlayID held after its step's check and
// before its send, then Stops and the bar's pick of this browser. The Stop is
// sent at once, and the switch-out waits for the replace as for every older
// operation not yet ended, whatever Stops came between, so the PlayID reaches
// the engine before the switch-out's Pause and the bridge is silent once the
// switch-out answers: a switch-out admitted before the Stop settles, one
// admitted after, and two Stops in a row, the first settling after the second.
func TestAStepAStopOvertookInFlightReachesTheEngineBeforeTheSwitchOutsPause(t *testing.T) {
	for _, tc := range []struct {
		name string
		// stops are the Stops sent; holdFirst holds the first after its answer,
		// before it settles, until the switch-out is admitted (switchWhileHeld)
		// or the Stops after it have settled.
		stops                      int
		holdFirst, switchWhileHeld bool
	}{
		{name: "a switch-out admitted before the Stop settles", stops: 1, holdFirst: true, switchWhileHeld: true},
		{name: "a switch-out admitted after the Stop settles", stops: 1},
		{name: "two Stops in a row", stops: 2, holdFirst: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCtlRig(t, ctlOptions{seams: &controlSeams{}})
			at, release := make(chan struct{}), make(chan struct{})
			var parked, stopParked atomic.Bool
			var letGo, stopLetGo, waitsOnce sync.Once
			free := func() { letGo.Do(func() { close(release) }) }
			t.Cleanup(free)
			stopAt, stopGo := make(chan struct{}), make(chan struct{})
			freeStop := func() { stopLetGo.Do(func() { close(stopGo) }) }
			t.Cleanup(freeStop)
			// waits is closed once the switch-out waits for the replace's end, or
			// once its answer is published, whichever comes first: the held
			// PlayID is let go then.
			waits := make(chan struct{})
			var afterPause atomic.Int32
			r.c.seams.beforeSend = func(kind, step string) {
				if kind == opKindReplace && step == "play_id" && parked.CompareAndSwap(false, true) {
					close(at)
					<-release
				}
			}
			r.c.seams.awaitPrev = func(kind, prev string) {
				if kind == opKindSwitchOut && prev == opKindReplace {
					waitsOnce.Do(func() { close(waits) })
				}
			}
			r.c.seams.afterFinal = func(kind string) {
				if kind == opKindSwitchOut {
					waitsOnce.Do(func() { close(waits) })
				}
			}
			r.c.seams.afterStep = func(kind, step string, err error) {
				switch {
				case kind == opKindSwitchOut && step == "pause":
					afterPause.Store(1)
				case kind == opKindReplace && afterPause.Load() == 1:
					t.Errorf("the replace sent %s after the switch-out's pause", step)
				case kind == opKindTransport && step == "stop" && tc.holdFirst && stopParked.CompareAndSwap(false, true):
					close(stopAt)
					<-stopGo
				}
			}
			replaced := make(chan admin.PlayerAnswer, 1)
			go func() { replaced <- r.replace(r.paths(), 0, 0, true) }()
			select {
			case <-at:
			case <-serveGiveUp(t):
				t.Fatal("the replace's PlayID was never held before its send")
			}
			var first chan admin.PlayerAnswer
			rest := tc.stops
			if tc.holdFirst {
				first = make(chan admin.PlayerAnswer, 1)
				go func() { first <- r.command(admin.PlayerCmdStop, nil) }()
				select {
				case <-stopAt:
				case <-serveGiveUp(t):
					t.Fatal("the first Stop was never answered")
				}
				rest--
			}
			for range rest {
				wantRecord(t, "a Stop beside the held PlayID", r.command(admin.PlayerCmdStop, nil), admin.PlayerOpDone, "", "")
			}
			switched := make(chan admin.PlayerAnswer, 1)
			if tc.switchWhileHeld {
				go func() { switched <- r.switchOut(false) }()
				waitEnded(t, r.c, opKindSwitchOut)
			}
			if first != nil {
				freeStop()
				wantRecord(t, "the first Stop", <-first, admin.PlayerOpDone, "", "")
			}
			if !tc.switchWhileHeld {
				go func() { switched <- r.switchOut(false) }()
			}
			select {
			case <-waits:
			case <-serveGiveUp(t):
				t.Fatal("the switch-out neither answered nor waited for the replace")
			}
			free()
			a := <-switched
			rec := wantRecord(t, "the switch-out", a, admin.PlayerOpDone, "", "")
			if st := stateOf(t, a.Player); !st.Silent || st.Transport == engine.Playing.String() || rec.Silence != st.Revision {
				t.Fatalf("the switch-out's publication: %s silent %v at %d (silence %d), want silent, its own", st.Transport, st.Silent, st.Revision, rec.Silence)
			}
			<-replaced
			if s := r.eng.Snapshot().State(); s == engine.Playing {
				t.Fatal("the bridge plays once the switch-out has answered")
			}
			var overtaken *admin.PlayerRecord
			for _, rec := range stateOf(t, a.Player).Operations {
				if rec.Tab == r.tab && rec.Seq == 1 {
					overtaken = &rec
				}
			}
			if overtaken == nil || overtaken.Status == admin.PlayerOpRunning {
				t.Fatalf("the overtaken replace's record in the switch-out's publication: %+v, want it ended before the switch-out began", overtaken)
			}
		})
	}
}

// TestAnAdmittedStepParkedWhenTheRequestEndsIsAnsweredNotCancelled pins E2:
// an insert admitted and parked behind a held reservation write when its
// request ends: its record reads running until the engine answers, then its
// final status with that step's own answer; no ReasonCancelled reaches it.
func TestAnAdmittedStepParkedWhenTheRequestEndsIsAnsweredNotCancelled(t *testing.T) {
	hold, held, seams := heldReservations()
	r := newCtlRig(t, ctlOptions{engSeams: seams})
	released := false
	t.Cleanup(func() {
		if !released {
			close(hold)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	replaced := make(chan admin.PlayerAnswer, 1)
	go func() {
		replaced <- r.c.Replace(ctx, admin.PlayerReplace{Op: r.op(), Digest: r.digest(), At: r.gen(), Tracks: r.paths()})
	}()
	select {
	case <-held:
	case <-serveGiveUp(t):
		t.Fatal("no reservation write was held")
	}
	cancel()
	a := <-replaced
	if a.Record == nil || a.Record.Status != admin.PlayerOpRunning {
		t.Fatalf("the answer at the request's end: %+v, want running", a.Record)
	}
	released = true
	close(hold)
	deadline := time.Now().Add(10 * time.Second)
	var rb admin.PlayerReadBack
	for time.Now().Before(deadline) {
		if rb = r.c.Operation(r.c.epoch, r.tab, 1); rb.Record != nil && rb.Record.Status != admin.PlayerOpRunning {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if rb.Record == nil || rb.Record.Status != admin.PlayerOpCancelled || rb.Record.Last == string(engine.ReasonCancelled) {
		t.Fatalf("the record once the parked insert answered: %+v, want cancelled with that insert's own answer", rb.Record)
	}
}

// TestTheSwitchOutPausesAResumeUnderWayBeforeTheBrowserStarts pins E4:
// another controller's Play from paused, held in the backend's Resume, so the
// engine reads paused; the barrier's Pause, sent whatever the snapshot
// reads, waits behind the resume and pauses what it started, and the answer
// is captured after that pause, nothing playing once it is in.
func TestTheSwitchOutPausesAResumeUnderWayBeforeTheBrowserStarts(t *testing.T) {
	r := newCtlRig(t, ctlOptions{})
	wantRecord(t, "a replace", r.replace(r.paths(), 0, 0, true), admin.PlayerOpDone, "", "")
	wantRecord(t, "pause", r.command(admin.PlayerCmdPause, nil), admin.PlayerOpDone, "", "")
	letGo := make(chan struct{})
	r.be.mu.Lock()
	r.be.holdResume, r.be.resuming = letGo, make(chan struct{})
	resuming := r.be.resuming
	r.be.mu.Unlock()
	played := make(chan error, 1)
	go func() { played <- r.eng.Play(context.Background()) }()
	<-resuming
	if st := r.eng.Snapshot().State(); st != engine.Paused {
		t.Fatalf("a resume under way reads %s, want paused", st)
	}
	switched := make(chan admin.PlayerAnswer, 1)
	go func() { switched <- r.switchOut(false) }()
	deadline := time.Now().Add(5 * time.Second)
	for r.c.recordOf(r.tab).Kind != opKindSwitchOut || r.c.recordOf(r.tab).Sent == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the switch-out sent no pause")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case a := <-switched:
		t.Fatalf("the switch-out answered %+v while a resume was under way", a.Record)
	case <-time.After(50 * time.Millisecond):
	}
	close(letGo)
	if err := <-played; err != nil {
		t.Fatal(err)
	}
	a := <-switched
	rec := wantRecord(t, "the switch-out", a, admin.PlayerOpDone, "", "")
	st := stateOf(t, a.Player)
	if st.Transport != "paused" || !st.Silent || rec.Silence != st.Revision {
		t.Fatalf("the answer's publication: %s silent %v at %d (silence %d), want paused, silent, its own", st.Transport, st.Silent, st.Revision, rec.Silence)
	}
	if s := r.eng.Snapshot().State(); s == engine.Playing {
		t.Fatal("the bridge plays once the switch-out has answered")
	}
}

// recordOf is a tab's last record as it stands.
func (c *consoleControl) recordOf(tab string) admin.PlayerRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r := c.records[tab]; r != nil {
		return r.rec
	}
	return admin.PlayerRecord{}
}

// TestAPlayAfterTheSwitchOutsCheckShowsTheBridgePlaying pins E4's residual:
// another controller's Play after the switch-out's answer shows in the next
// publication, the bridge playing.
func TestAPlayAfterTheSwitchOutsCheckShowsTheBridgePlaying(t *testing.T) {
	r := newCtlRig(t, ctlOptions{})
	wantRecord(t, "a replace", r.replace(r.paths(), 0, 0, true), admin.PlayerOpDone, "", "")
	a := r.switchOut(true)
	wantRecord(t, "the switch-out", a, admin.PlayerOpDone, "", "")
	if err := r.eng.Play(context.Background()); err != nil {
		t.Fatal(err)
	}
	pub := r.c.Now(context.Background())
	if st := stateOf(t, pub); st.Transport != "playing" || st.Silent || pub.Revision <= a.Player.Revision {
		t.Fatalf("the publication after the play: %s silent %v at %d, want playing after %d", st.Transport, st.Silent, pub.Revision, a.Player.Revision)
	}
}

// TestTheSwitchOutsAnswerIsTheCaptureThatSawSilence pins J1: another
// controller starts playback after the barrier observed silence and before
// the answer is captured: the answer is never done on the earlier silence;
// the barrier pauses again and answers on a capture of its own.
func TestTheSwitchOutsAnswerIsTheCaptureThatSawSilence(t *testing.T) {
	r := newCtlRig(t, ctlOptions{seams: &controlSeams{}})
	wantRecord(t, "a replace", r.replace(r.paths(), 0, 0, false), admin.PlayerOpDone, "", "")
	var once sync.Once
	r.c.seams.afterSilence = func() {
		once.Do(func() {
			if err := r.eng.Play(context.Background()); err != nil {
				t.Errorf("another controller's play: %v", err)
			}
		})
	}
	a := r.switchOut(false)
	rec := wantRecord(t, "the switch-out", a, admin.PlayerOpDone, "", "")
	st := stateOf(t, a.Player)
	if !st.Silent || st.Transport == "playing" || rec.Sent < 2 {
		t.Fatalf("the answer: %s, silent %v, %d pauses; want silent on its own capture after a second pause", st.Transport, st.Silent, rec.Sent)
	}
}

// TestTheSwitchOutsQueueIsTheSilencesOwn pins C3: the answer's handover is
// the queue of the capture that showed the silence, one capture; a cast
// landed at the seam right after the answer's publication, before the
// switch-out's end, shows in the next frame, never in the handover.
func TestTheSwitchOutsQueueIsTheSilencesOwn(t *testing.T) {
	r := newCtlRig(t, ctlOptions{seams: &controlSeams{}})
	wantRecord(t, "a replace", r.replace(r.paths(), 1, 0, true), admin.PlayerOpDone, "", "")
	var once sync.Once
	r.c.seams.afterFinal = func(kind string) {
		if kind != opKindSwitchOut {
			return
		}
		once.Do(func() {
			if err := r.eng.DeleteAll(context.Background()); err != nil {
				t.Errorf("the cast's DeleteAll: %v", err)
				return
			}
			ref, err := engine.NewReference("http://192.168.1.20:7790/dlna/file/" + dlna.TrackID("", r.paths()[2]))
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := r.eng.InsertAfter(context.Background(), 0, ref, []byte("cast")); err != nil {
				t.Errorf("the cast's insert: %v", err)
			}
		})
	}
	a := r.switchOut(true)
	sw := wantRecord(t, "the switch-out", a, admin.PlayerOpDone, "", "")
	if h := sw.Handover; h == nil || len(h.Tracks) != 3 || h.Selected != 1 || !h.Playing {
		t.Fatalf("handover %+v, want the three tracks, the second selected, the bridge playing before", sw.Handover)
	}
	if st := stateOf(t, a.Player); st.Length != 3 {
		t.Fatalf("the answer's publication holds %d entries, want the silence's three", st.Length)
	}
	pub := r.c.Now(context.Background())
	if stateOf(t, pub).Length != 1 || pub.Revision <= a.Player.Revision {
		t.Fatalf("the next publication: %+v, want the cast's queue after the answer's", stateOf(t, pub))
	}
}

// TestTheHandoverPositionBelongsToItsTrack pins K2: a position is handed over
// only when its occurrence is the captured track's and that track is the
// selected entry: A plays, a replace without play selects B, then a switch
// out hands over position 0; and a paused selection that is deleted hands
// over 0.
func TestTheHandoverPositionBelongsToItsTrack(t *testing.T) {
	r := newCtlRig(t, ctlOptions{})
	wantRecord(t, "A plays", r.replace(r.paths(), 0, 40, true), admin.PlayerOpDone, "", "")
	wantRecord(t, "a replace without play on B", r.replace(r.paths(), 1, 0, false), admin.PlayerOpDone, "", "")
	sw := wantRecord(t, "the switch-out", r.switchOut(true), admin.PlayerOpDone, "", "")
	if sw.Handover == nil || sw.Handover.Position != 0 || sw.Handover.Selected != 1 {
		t.Fatalf("handover %+v, want B selected at position 0", sw.Handover)
	}
	rec := wantRecord(t, "A plays again", r.replace(r.paths(), 0, 40, true), admin.PlayerOpDone, "", "")
	wantRecord(t, "pause", r.command(admin.PlayerCmdPause, nil), admin.PlayerOpDone, "", "")
	del := r.c.Delete(context.Background(), admin.PlayerDelete{Op: r.op(), Digest: r.digest(), At: r.gen(), Entry: rec.Entries[0]})
	wantRecord(t, "the paused selection deleted", del, admin.PlayerOpDone, "", "")
	sw = wantRecord(t, "the switch-out", r.switchOut(true), admin.PlayerOpDone, "", "")
	if sw.Handover == nil || sw.Handover.Position != 0 {
		t.Fatalf("handover %+v, want position 0", sw.Handover)
	}
}

// TestTheBarrierRefusesAReleaseThatStaysUnconfirmed pins G10: a Release held
// in the stand-in past its deadline and never acknowledged: the barrier
// answers refused, release_unconfirmed, at its bound, never a wait without
// end.
func TestTheBarrierRefusesAReleaseThatStaysUnconfirmed(t *testing.T) {
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	r := newCtlRig(t, ctlOptions{holdRelease: hold, seams: &controlSeams{barrier: engine.ReleaseTimeout + time.Second}})
	wantRecord(t, "a replace", r.replace(r.paths(), 0, 0, true), admin.PlayerOpDone, "", "")
	if err := r.eng.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	a := r.switchOut(false)
	rec := wantRecord(t, "the switch-out beside an unconfirmed release", a, admin.PlayerOpRefused, string(engine.ReasonReleaseUnconfirmed), "")
	if took := time.Since(began); took > engine.ReleaseTimeout+3*time.Second || rec.Release != releaseUnconfirmed {
		t.Fatalf("answered after %v with release %q, want refused at its bound", took, rec.Release)
	}
}

// TestARefusalOrAPausesSuccessIsNotSilence pins C2 on the server: a
// switch-out under the DSD check is refused check, and the tone in the slot
// refuses it tone; neither is silence.
func TestARefusalOrAPausesSuccessIsNotSilence(t *testing.T) {
	r := newCtlRig(t, ctlOptions{})
	hold, err := r.eng.EnterCheck(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	a := r.switchOut(false)
	wantRecord(t, "a switch-out under the check", a, admin.PlayerOpRefused, holdCheck, admin.PlayerEffectNone)
	if err := r.eng.ExitMaintenance(context.Background(), hold.Lease); err != nil {
		t.Fatal(err)
	}
	r.be.setTone(playback.ToneStatePlaying)
	a = r.switchOut(false)
	wantRecord(t, "a switch-out beside the tone", a, admin.PlayerOpRefused, wordTone, "")
	if stateOf(t, a.Player).Silent {
		t.Fatal("the tone's publication reads silent")
	}
}

// TestAClosedEngineIsSilentOnlyOnceItsSessionsWereLetGo pins G7 and K7: an
// engine closed by a panic with a session held: the barrier waits for the
// shutdown's facts, then answers done; facts that say a device goroutine
// still runs, a release unconfirmed, or a release of every session that
// answered an error, answer refused release_unconfirmed.
func TestAClosedEngineIsSilentOnlyOnceItsSessionsWereLetGo(t *testing.T) {
	for _, tc := range []struct {
		name  string
		facts playerClosedFacts
		want  string
	}{
		{"let go", playerClosedFacts{Ended: true, Panicked: true}, admin.PlayerOpDone},
		{"a release that failed", playerClosedFacts{Ended: true, Panicked: true, ReleaseErr: errors.New("held")}, admin.PlayerOpRefused},
		{"a device goroutine running", playerClosedFacts{Ended: true, DeviceHeld: true}, admin.PlayerOpRefused},
		{"a release unconfirmed", playerClosedFacts{Ended: true, Unconfirmed: true}, admin.PlayerOpRefused},
		{"Run not returned", playerClosedFacts{}, admin.PlayerOpRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCtlRig(t, ctlOptions{seams: &controlSeams{barrier: time.Second}})
			wantRecord(t, "a replace", r.replace(r.paths(), 0, 0, true), admin.PlayerOpDone, "", "")
			r.stopEngine()
			answered := make(chan admin.PlayerAnswer, 1)
			go func() { answered <- r.switchOut(true) }()
			select {
			case a := <-answered:
				t.Fatalf("the barrier answered %+v before the closed engine's facts were known", a.Record)
			case <-time.After(100 * time.Millisecond):
			}
			r.p.noteClosed(tc.facts)
			a := <-answered
			if a.Record == nil || a.Record.Status != tc.want {
				t.Fatalf("%s: %+v, want %s", tc.name, a.Record, tc.want)
			}
			if tc.want == admin.PlayerOpDone && (a.Record.Handover == nil || len(a.Record.Handover.Tracks) != 3) {
				t.Fatalf("the closed engine's handover %+v, want the last queue", a.Record.Handover)
			}
		})
	}
}

// TestASwitchOutWaitingForAClosedEnginesFactsHearsItsEnd pins the closed
// engine's wait (§3.1): a switch-out waiting for the facts of an engine that
// closed is ended by a newer operation, cancelled, and the newer one is
// answered before the facts are known; the control's stop ends the wait,
// the switch-out's record final once the stop has joined it.
func TestASwitchOutWaitingForAClosedEnginesFactsHearsItsEnd(t *testing.T) {
	start := func(t *testing.T) (*ctlRig, chan admin.PlayerAnswer) {
		r := newCtlRig(t, ctlOptions{seams: &controlSeams{barrier: 30 * time.Second, handlerWait: 2 * time.Second}})
		wantRecord(t, "a replace", r.replace(r.paths(), 0, 0, true), admin.PlayerOpDone, "", "")
		r.stopEngine()
		t.Cleanup(func() { r.p.noteClosed(playerClosedFacts{Ended: true}) })
		first := make(chan admin.PlayerAnswer, 1)
		go func() { first <- r.switchOut(true) }()
		waitEnded(t, r.c, opKindSwitchOut)
		return r, first
	}
	t.Run("a newer operation", func(t *testing.T) {
		r, first := start(t)
		wantRecord(t, "a pause sent while the switch-out waits for the facts", r.command(admin.PlayerCmdPause, nil), admin.PlayerOpRefused,
			wordNotRunning, admin.PlayerEffectNone)
		wantRecord(t, "the switch-out a newer operation ended", <-first, admin.PlayerOpCancelled, "", "")
	})
	t.Run("the control's stop", func(t *testing.T) {
		r, first := start(t)
		r.c.stop()
		if rec := r.c.recordOf(r.tab); rec.Kind != opKindSwitchOut || rec.Status == admin.PlayerOpRunning {
			t.Fatalf("the switch-out once the control stopped: %s %s, want it ended", rec.Kind, rec.Status)
		}
		<-first
	})
}

// TestTheTrackURIAndTheArtworkURLAreBuiltApart pins E8: with the DLNA
// listener on another port than 7790, an entry's reference and its <res> are
// on 7790 and its albumArtURI on the listener's port.
func TestTheTrackURIAndTheArtworkURLAreBuiltApart(t *testing.T) {
	r := newCtlRig(t, ctlOptions{dlna: fixedArtwork{base: "http://192.168.1.20:8800", host: "192.168.1.20"}})
	row := manifest.CatalogTrackRow{Path: r.paths()[0], Codec: "FLAC"}
	host := r.c.referenceHost()
	e, word := r.c.entryFor(context.Background(), r.paths()[0], dlna.TrackID("", r.paths()[0]), row, host)
	if word != "" {
		t.Fatal(word)
	}
	ref := "http://192.168.1.20:7790/dlna/file/" + dlna.TrackID("", r.paths()[0])
	if e.ref.String() != ref {
		t.Fatalf("reference %q, want %q", e.ref, ref)
	}
	meta := string(e.meta)
	if !strings.Contains(meta, ">"+ref+"</res>") {
		t.Fatalf("the <res> is not the reference: %s", meta)
	}
	if strings.Count(meta, "<res ") != 1 {
		t.Fatalf("the entry carries %d <res>, want the source's alone: %s", strings.Count(meta, "<res "), meta)
	}
	_ = r.store.UpsertTrack(context.Background(), &manifest.Track{Path: r.paths()[0], Title: "A", Codec: "FLAC", ArtworkMBID: "11111111-2222-4333-8444-555555555555"})
	e, _ = r.c.entryFor(context.Background(), r.paths()[0], dlna.TrackID("", r.paths()[0]), row, host)
	if !strings.Contains(string(e.meta), "<upnp:albumArtURI>http://192.168.1.20:8800/dlna/artwork/") {
		t.Fatalf("the albumArtURI is not on the listener's port: %s", e.meta)
	}
}

// fixedArtwork is a DLNA listener that takes connections on one host and
// answers base for it.
type fixedArtwork struct{ base, host string }

func (f fixedArtwork) ArtworkBase(a netip.Addr) (string, bool) {
	return f.base, a.String() == f.host
}

// TestNoArtworkURLOnAHostTheDLNAListenerDoesNotServe pins E8's other half: a
// host /v1/health advertises that the DLNA listener takes no connection on,
// and DLNA off, give no albumArtURI.
func TestNoArtworkURLOnAHostTheDLNAListenerDoesNotServe(t *testing.T) {
	r := newCtlRig(t, ctlOptions{dlna: fixedArtwork{base: "http://10.0.0.9:7790", host: "10.0.0.9"}})
	_ = r.store.UpsertTrack(context.Background(), &manifest.Track{Path: r.paths()[0], Title: "A", Codec: "FLAC", ArtworkMBID: "11111111-2222-4333-8444-555555555555"})
	row := manifest.CatalogTrackRow{Path: r.paths()[0], Codec: "FLAC"}
	e, word := r.c.entryFor(context.Background(), r.paths()[0], dlna.TrackID("", r.paths()[0]), row, r.c.referenceHost())
	if word != "" || !strings.Contains(string(e.meta), "<dc:title>A</dc:title>") || !strings.Contains(string(e.meta), "<res ") {
		t.Fatalf("the entry's metadata (%q): %s, want the ContentDirectory's item with its <res>", word, e.meta)
	}
	if strings.Contains(string(e.meta), "albumArtURI") {
		t.Fatalf("an albumArtURI on a host the listener does not serve: %s", e.meta)
	}
	off := newCtlRig(t, ctlOptions{})
	_ = off.store.UpsertTrack(context.Background(), &manifest.Track{Path: off.paths()[0], Title: "A", Codec: "FLAC", ArtworkMBID: "11111111-2222-4333-8444-555555555555"})
	e, _ = off.c.entryFor(context.Background(), off.paths()[0], dlna.TrackID("", off.paths()[0]), row, off.c.referenceHost())
	if strings.Contains(string(e.meta), "albumArtURI") {
		t.Fatalf("an albumArtURI with DLNA off: %s", e.meta)
	}
}

// TestABoxWithNoLANHostWritesTheLoopbackURIAndNoRes pins E9 and K3: a box
// whose /v1/health advertises no host an Insert takes writes the canonical
// URI on the loopback literal, and metadata with no <res> and no albumArtURI,
// cached variants included.
func TestABoxWithNoLANHostWritesTheLoopbackURIAndNoRes(t *testing.T) {
	r := newCtlRig(t, ctlOptions{advertised: []string{"https://100.101.102.103:7788", "https://box.tail1234.ts.net:7788"},
		dlna: fixedArtwork{base: "http://127.0.0.1:7790", host: "127.0.0.1"}})
	_ = r.store.UpsertTrack(context.Background(), &manifest.Track{Path: r.paths()[0], Title: "A", Codec: "FLAC", ArtworkMBID: "11111111-2222-4333-8444-555555555555"})
	host := r.c.referenceHost()
	if host.lan {
		t.Fatalf("host %+v is a LAN host", host)
	}
	row := manifest.CatalogTrackRow{Path: r.paths()[0], Codec: "FLAC"}
	e, word := r.c.entryFor(context.Background(), r.paths()[0], dlna.TrackID("", r.paths()[0]), row, host)
	if word != "" {
		t.Fatal(word)
	}
	want := "http://127.0.0.1:7790/dlna/file/" + dlna.TrackID("", r.paths()[0])
	if e.ref.String() != want {
		t.Fatalf("reference %q, want %q", e.ref, want)
	}
	if _, err := trackref.Parse(e.ref.String()); err != nil {
		t.Fatalf("the loopback reference is no canonical URI: %v", err)
	}
	ti := dlna.TrackInfo{TrackID: dlna.TrackID("", r.paths()[0]), Title: "A", Codec: "FLAC", FileExtension: ".flac",
		Variants: []dlna.VariantInfo{{VariantID: "optimized-v2-48000-16", FileExtension: ".flac", Size: 1, BitDepth: 16, SampleRate: 48000}}}
	meta := consoleMetadata(ti, host, "http://127.0.0.1:7790")
	if strings.Contains(meta, "<res") || strings.Contains(meta, "albumArtURI") || strings.Contains(string(e.meta), "<res") {
		t.Fatalf("a no-LAN entry carries a <res> or an albumArtURI: %s", meta)
	}
}

// TestTheConsolesMetadataIsTheContentDirectorysItem pins R7: a console
// entry's metadata is the ContentDirectory's item for the track, its bases
// apart, from the one builder (dlna.TrackInfo.DIDLItem), inside the
// DIDL-Lite envelope.
func TestTheConsolesMetadataIsTheContentDirectorysItem(t *testing.T) {
	ti := dlna.TrackInfo{TrackID: dlna.TrackID("", "Album/01 A.flac"), Title: "A & B", Artist: "Artist", Album: "Album",
		Codec: "FLAC", FileExtension: ".flac", Size: 5, ArtworkKey: "abcdef0123456789"}
	host := refHost{lan: true, host: "192.168.1.20:7790"}
	got := consoleMetadata(ti, host, "http://192.168.1.20:7790")
	want := dlna.WrapDIDLLite(ti.DIDLItem(dlna.DIDLItemOpts{TrackBase: "http://192.168.1.20:7790", ArtworkBase: "http://192.168.1.20:7790", Resources: dlna.ResSourceOnly}))
	if got != want {
		t.Fatalf("the console's metadata\n%s\nis not the ContentDirectory's item\n%s", got, want)
	}
}

// TestTheConsolesMetadataIsXMLTextWhateverTheTags pins K4's policy: a tag
// holding a character XML 1.0 cannot carry (U+FFFE, U+FFFF, a byte that is
// not UTF-8) has it replaced by U+FFFD before the item is built, and the
// metadata is what an Insert takes (openhome.MetadataTaken).
func TestTheConsolesMetadataIsXMLTextWhateverTheTags(t *testing.T) {
	r := newCtlRig(t, ctlOptions{})
	for _, title := range []string{"bad \uFFFE end", "bad \uFFFF end", "bad \xff end", "ctl \x01 end", "ok & <fine>"} {
		_ = r.store.UpsertTrack(context.Background(), &manifest.Track{Path: r.paths()[0], Title: title, Codec: "FLAC"})
		row := manifest.CatalogTrackRow{Path: r.paths()[0], Codec: "FLAC"}
		e, word := r.c.entryFor(context.Background(), r.paths()[0], dlna.TrackID("", r.paths()[0]), row, r.c.referenceHost())
		if word != "" || !openhome.MetadataTaken(e.meta) {
			t.Fatalf("title %q: word %q, metadata taken %v: %s", title, word, openhome.MetadataTaken(e.meta), e.meta)
		}
	}
}

// TestAConsoleEntryIsAFullOpenHomeEntry pins R7: an entry the console
// inserts is one an OpenHome Insert takes: its reference the canonical URI an
// Insert's confinement takes on a host the inventory names, its metadata
// within an Insert's bounds; and the resolver plays it.
func TestAConsoleEntryIsAFullOpenHomeEntry(t *testing.T) {
	r := newCtlRig(t, ctlOptions{})
	wantRecord(t, "a replace", r.replace(r.paths(), 0, 0, false), admin.PlayerOpDone, "", "")
	s := r.eng.Snapshot()
	entries, _ := s.ReadList(s.IDs())
	for _, e := range entries {
		u, err := trackref.Parse(e.Ref.String())
		if err != nil || openhome.LANHostRefused(u) || !r.c.hosts.Names(u) {
			t.Fatalf("entry %d's reference %q is not one an Insert takes: %v", e.ID, e.Ref, err)
		}
		if !openhome.MetadataTaken(e.Meta) {
			t.Fatalf("entry %d's metadata is not one an Insert takes", e.ID)
		}
		if _, err := r.res.StoredPath(context.Background(), e.Ref); err != nil {
			t.Fatalf("the resolver names no track for %q: %v", e.Ref, err)
		}
	}
}

// TestTheConsoleBuildsItsMetadataWithTheContentDirectorysBuilder pins K6
// structurally: the console's metadata is made by dlna.TrackInfo.DIDLItem
// inside dlna.WrapDIDLLite, and no file of cmd/bridge writes DIDL-Lite of
// its own (an <item or a <res element in a string), so a copy of the
// builder cannot stand in for it.
func TestTheConsoleBuildsItsMetadataWithTheContentDirectorysBuilder(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "serve_player_control.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "consoleMetadata" && fd.Recv == nil {
			fn = fd
		}
	}
	if fn == nil {
		t.Fatal("no consoleMetadata")
	}
	built := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isSelector(call.Fun, "dlna", "WrapDIDLLite") || len(call.Args) != 1 {
			return true
		}
		if inner, ok := call.Args[0].(*ast.CallExpr); ok {
			if sel, ok := inner.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "DIDLItem" {
				built = true
			}
		}
		return true
	})
	if !built {
		t.Fatal("consoleMetadata does not wrap the ContentDirectory's item (dlna.WrapDIDLLite of a TrackInfo's DIDLItem)")
	}
	// No production file of the package writes DIDL-Lite of its own: no string
	// literal holds an element of it. A comment may name one.
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		pf, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(pf, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			for _, needle := range []string{"<item", "<res", "<DIDL-Lite", "<dc:title", "<upnp:class"} {
				if strings.Contains(lit.Value, needle) {
					t.Errorf("%s writes DIDL-Lite of its own (%s in %s)", fset.Position(lit.Pos()), needle, lit.Value)
				}
			}
			return true
		})
	}
}

// TestTheControlRefusesWhatTheOutputRefuses pins the output's refusals:
// with the output gone, the edits are taken and whatever starts a session is
// refused with the report's word.
func TestTheControlRefusesWhatTheOutputRefuses(t *testing.T) {
	r := newCtlRig(t, ctlOptions{})
	r.report(2, false, "device_missing")
	wantRecord(t, "a replace without play, the DAC gone", r.replace(r.paths(), 0, 0, false), admin.PlayerOpDone, "", "")
	a := r.command(admin.PlayerCmdPlay, nil)
	wantRecord(t, "play, the DAC gone", a, admin.PlayerOpRefused, string(engine.ReasonOutputUnavailable), admin.PlayerEffectNone)
	st := stateOf(t, a.Player)
	if st.Offered || st.OfferedWhy != "device_missing" || slices.Contains(st.Commands, admin.PlayerCmdPlay) || !slices.Contains(st.Commands, admin.PlayerCmdStop) {
		t.Fatalf("the state with the DAC gone: offered %v (%q), commands %v", st.Offered, st.OfferedWhy, st.Commands)
	}
}

// TestEveryEngineRefusalHasItsAnswer pins the one map from the engine's
// reasons to the record's words (§5.3): each reason the table names maps to
// its word, a hold by what holds the engine, and a reason it does not name
// player_refused.
func TestEveryEngineRefusalHasItsAnswer(t *testing.T) {
	for reason, want := range map[engine.Reason]string{
		engine.ReasonConflict: wordStale, engine.ReasonMaintenance: holdMaintenance, engine.ReasonConfiguring: "configuring",
		engine.ReasonOutputUnknown: "output_unknown", engine.ReasonOutputUnavailable: "output_unavailable",
		engine.ReasonReleaseUnconfirmed: "release_unconfirmed", engine.ReasonEmptyQueue: "empty_queue",
		engine.ReasonIDNotFound: wordEntryNotFound, engine.ReasonIndexNotFound: wordEntryNotFound,
		engine.ReasonSeekUnavailable: "seek_unavailable", engine.ReasonSeekOutOfRange: "seek_out_of_range",
		engine.ReasonSeekUnsupported: "seek_unsupported", engine.ReasonFull: wordQueueFull,
		engine.ReasonReservation: wordEditsRefused, engine.ReasonFileStuck: wordEditsRefused, engine.ReasonIDsExhausted: wordEditsRefused,
		engine.ReasonOvertaken: "overtaken", engine.ReasonUnplayable: "unplayable", engine.ReasonAllUnplayable: "all_unplayable",
		engine.ReasonBackend: "backend_error", engine.ReasonTimeout: "backend_timeout", engine.ReasonNotInRelease: wordPlayerRefused,
	} {
		if got := wordOf(&engine.Error{Reason: reason}, nil); got != want {
			t.Errorf("%s answers %q, want %q", reason, got, want)
		}
	}
	if got := statusOfErr(&engine.Error{Reason: engine.ReasonSuperseded}); got != admin.PlayerOpChanged {
		t.Errorf("superseded is %s, want changed", got)
	}
	if got := statusOfErr(&engine.Error{Reason: engine.ReasonClosed}); got != admin.PlayerOpUnknown {
		t.Errorf("closed is %s, want unknown", got)
	}
}

// heldReservations is an engine's reservation writes as storage that refuses
// the first (New's, so no value is durable at the start) and then holds every
// write until hold is closed: an edit that takes a value parks behind the
// held write, admitted (engine.Config.Seams). held hears each held write.
func heldReservations() (hold chan struct{}, held chan struct{}, seams *engine.Seams) {
	hold, held = make(chan struct{}), make(chan struct{}, 1)
	var calls atomic.Int32
	seams = &engine.Seams{ReserveWrite: func() error {
		if calls.Add(1) == 1 {
			return errors.New("read-only file system")
		}
		select {
		case held <- struct{}{}:
		default:
		}
		<-hold
		return nil
	}}
	return hold, held, seams
}
