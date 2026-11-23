package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/library"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
)

// The console control's publisher (B634, PR 1): every state the player bar
// takes is one capture of the publisher's, ordered by one revision, over the
// real engine on the stand-in backend (ctlRig).

// pubLog is every publication a test saw, by revision, from every source: the
// stream's wake, every command's answer and every read. A revision seen with
// two states fails the test at once.
type pubLog struct {
	t      *testing.T
	mu     sync.Mutex
	seen   map[uint64][]byte
	stream []uint64
}

func newPubLog(t *testing.T) *pubLog { return &pubLog{t: t, seen: map[uint64][]byte{}} }

// add records pub, seen through what.
func (l *pubLog) add(what string, pub admin.PlayerPublication) {
	if pub.Revision == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if b, ok := l.seen[pub.Revision]; ok && !bytes.Equal(b, pub.Bytes) {
		l.t.Errorf("%s: revision %d carries two states:\n%s\n%s", what, pub.Revision, b, pub.Bytes)
	}
	l.seen[pub.Revision] = pub.Bytes
}

// follow records every publication c's wake signals, as the event stream
// reads them, until the test ends.
func (l *pubLog) follow(c *consoleControl) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			ch := c.Wake()
			if pub, ok := c.Latest(); ok {
				l.mu.Lock()
				l.stream = append(l.stream, pub.Revision)
				l.mu.Unlock()
				l.add("the stream", pub)
			}
			select {
			case <-ch:
			case <-ctx.Done():
				return
			}
		}
	}()
	drainLoopOnCleanup(l.t, cancel, done, "the publication log's follower")
}

// position hands the engine a measurement of the occurrence heard, as the
// backend's reporter does, and waits until the engine has taken it.
func (r *ctlRig) position(frames int64, at time.Time) engine.Track {
	r.t.Helper()
	s := r.await("a track heard", func(s *engine.Snapshot) bool { _, ok := s.Track(); return ok })
	tr, _ := s.Track()
	sess, ev, ok := r.standIn.session()
	if !ok {
		r.t.Fatal("no session to measure")
	}
	ev.Position(sess, engine.Position{Occurrence: tr.Occurrence, Frames: frames, RateHz: 44100, Measured: at})
	r.await("the measurement taken", func(s *engine.Snapshot) bool {
		p := s.Position()
		return p.Frames == frames && p.Measured.Equal(at)
	})
	return tr
}

// anchorOf is a publication's anchor, nil for none.
func anchorOf(t *testing.T, pub admin.PlayerPublication) *admin.PlayerAnchor {
	t.Helper()
	return stateOf(t, pub).Anchor
}

// TestEveryStateIsOrderedByThePublishersRevision pins §4.1 and §4.2 on the
// server: every publication, seen through the stream, a command's answer, GET
// now or GET queue, names its own revision and the control's epoch, one
// revision is one state wherever it is seen, the stream's revisions only grow,
// the answers of commands sent in turn are each newer than the one before,
// and a queue read and the read after it agree on one publication.
func TestEveryStateIsOrderedByThePublishersRevision(t *testing.T) {
	r := newCtlRig(t, ctlOptions{})
	log := newPubLog(t)
	log.follow(r.c)
	ctx := context.Background()
	var answers []admin.PlayerPublication
	answer := func(what string, a admin.PlayerAnswer) {
		t.Helper()
		wantRecord(t, what, a, admin.PlayerOpDone, "", "")
		log.add(what, a.Player)
		answers = append(answers, a.Player)
	}
	answer("a replace that plays", r.replace(r.paths(), 0, 0, true))
	answer("repeat on", r.command(admin.PlayerCmdRepeat, func(c *admin.PlayerCommand) { c.On = true }))
	if err := r.eng.SetRepeat(ctx, false); err != nil {
		t.Fatal(err)
	}
	r.await("another controller's repeat off", func(s *engine.Snapshot) bool { return !s.Repeat() })
	log.add("GET now", r.c.Now(ctx))
	q := r.c.Queue(ctx)
	again := r.c.Now(ctx)
	log.add("GET now after GET queue", again)
	st := stateOf(t, again)
	if q.ReadAt != again.Revision || q.QueueRev != again.QueueRev || len(q.Entries) != st.Length || q.Token != st.Token || q.Selected != st.Selected {
		t.Errorf("GET queue read at %d (queueRev %d, %d entries, token %d, selected %d); the publication after it: %d (queueRev %d, %d entries, token %d, selected %d)",
			q.ReadAt, q.QueueRev, len(q.Entries), q.Token, q.Selected, again.Revision, again.QueueRev, st.Length, st.Token, st.Selected)
	}
	answer("pause", r.command(admin.PlayerCmdPause, nil))
	for i := 1; i < len(answers); i++ {
		if answers[i].Revision <= answers[i-1].Revision {
			t.Errorf("answer %d is revision %d, answer %d before it %d: an answer is not newer than the one sent before it", i, answers[i].Revision, i-1, answers[i-1].Revision)
		}
	}
	log.add("the latest", r.c.Now(ctx))
	log.mu.Lock()
	defer log.mu.Unlock()
	for rev, b := range log.seen {
		var got admin.PlayerState
		got = stateOf(t, admin.PlayerPublication{Bytes: b})
		if got.Revision != rev || got.Epoch != r.c.epoch {
			t.Errorf("a publication seen as revision %d names revision %d of epoch %q (the control's %q)", rev, got.Revision, got.Epoch, r.c.epoch)
		}
	}
	for i := 1; i < len(log.stream); i++ {
		if log.stream[i] < log.stream[i-1] {
			t.Errorf("the stream went from revision %d back to %d", log.stream[i-1], log.stream[i])
		}
	}
	if len(log.stream) == 0 {
		t.Error("the stream saw no publication")
	}
}

// TestARevisionIsAssignedOnlyToTheCaptureItNames pins §4.1's last point
// (E5): a capture held at the publisher's seam between its reads and its
// publish has no revision yet, and while it is held every handler answers the
// latest publication: none answers the revision the held capture takes, or a
// state it did not capture. Let go, the capture publishes the state it read
// under that revision.
func TestARevisionIsAssignedOnlyToTheCaptureItNames(t *testing.T) {
	held, release := make(chan struct{}), make(chan struct{})
	var armed atomic.Bool
	seams := &controlSeams{}
	seams.beforePublish = func() {
		if armed.CompareAndSwap(true, false) {
			close(held)
			<-release
		}
	}
	r := newCtlRig(t, ctlOptions{seams: seams})
	var once sync.Once
	letGo := func() { once.Do(func() { close(release) }) }
	t.Cleanup(letGo)
	log := newPubLog(t)
	log.follow(r.c)
	wantRecord(t, "a replace", r.replace(r.paths(), 0, 0, false), admin.PlayerOpDone, "", "")
	before := r.c.Now(context.Background())
	log.add("the publication before", before)
	armed.Store(true)
	// Another controller's change wakes the publisher, whose capture of it is
	// held between its reads and its publish.
	if err := r.eng.SetRepeat(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-held:
	case <-serveGiveUp(t):
		t.Fatal("no capture was held at the publisher's seam")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	now := r.c.Now(ctx)
	q := r.c.Queue(ctx)
	cancel()
	latest, _ := r.c.Latest()
	for _, a := range []struct {
		what string
		pub  admin.PlayerPublication
	}{{"GET now while a capture is held", now}, {"the latest while a capture is held", latest}} {
		log.add(a.what, a.pub)
		if a.pub.Revision != before.Revision {
			t.Errorf("%s answered revision %d, want the latest, %d", a.what, a.pub.Revision, before.Revision)
		}
	}
	if q.ReadAt != before.Revision {
		t.Errorf("GET queue while a capture is held read at %d, want the latest, %d", q.ReadAt, before.Revision)
	}
	letGo()
	after := r.c.Now(context.Background())
	log.add("GET now once the capture is let go", after)
	if after.Revision <= before.Revision || !stateOf(t, after).Repeat {
		t.Fatalf("once let go: revision %d (before %d), repeat %v; want the held capture's state published after", after.Revision, before.Revision, stateOf(t, after).Repeat)
	}
	log.mu.Lock()
	b, ok := log.seen[before.Revision+1]
	log.mu.Unlock()
	if ok && !stateOf(t, admin.PlayerPublication{Bytes: b}).Repeat {
		t.Errorf("revision %d, the held capture's, is a state without the repeat it read: %s", before.Revision+1, b)
	}
}

// TestTheAnchorKeepsItsIDAndTimeThroughChangesThatMoveNoPosition pins §4.4:
// during steady playback another controller's repeat change, its append and
// an output report each publish what they change with the same anchor, its
// id, its time and its seconds included.
func TestTheAnchorKeepsItsIDAndTimeThroughChangesThatMoveNoPosition(t *testing.T) {
	r := newCtlRig(t, ctlOptions{})
	ctx := context.Background()
	wantRecord(t, "a replace that plays", r.replace(r.paths(), 0, 0, true), admin.PlayerOpDone, "", "")
	base := time.Now().Add(-time.Minute)
	r.position(44100*10, base)
	first := r.c.Now(ctx)
	a0 := anchorOf(t, first)
	if a0 == nil || a0.Seconds != 10 || a0.At == "" || !a0.Advancing || a0.ID == 0 {
		t.Fatalf("the anchor of steady playback: %+v, want 10 s measured, advancing, with its id", a0)
	}
	ref := r.refs()[0]
	changes := []struct {
		what      string
		publishes bool
		do        func()
	}{
		{"another controller's repeat change", true, func() {
			if err := r.eng.SetRepeat(ctx, true); err != nil {
				t.Fatal(err)
			}
			r.await("repeat on", func(s *engine.Snapshot) bool { return s.Repeat() })
		}},
		{"another controller's append", true, func() {
			s := r.eng.Snapshot()
			eref, err := engine.NewReference(idOf(t, ref))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.eng.InsertAfter(ctx, s.IDs()[len(s.IDs())-1], eref, nil); err != nil {
				t.Fatal(err)
			}
			r.await("the append", func(s *engine.Snapshot) bool { return s.Len() == 4 })
		}},
		{"an output report", false, func() {
			r.report(2, true, "")
			r.await("the report", func(s *engine.Snapshot) bool { return s.Output().Generation == 2 })
		}},
	}
	last := first
	for _, ch := range changes {
		ch.do()
		pub := r.c.Now(ctx)
		if ch.publishes && pub.Revision <= last.Revision {
			t.Errorf("%s published nothing", ch.what)
		}
		if a := anchorOf(t, pub); a == nil || *a != *a0 {
			t.Errorf("%s: the anchor %+v, want the one before, %+v", ch.what, a, a0)
		}
		last = pub
	}
}

// TestThePlayerFrameIsQuietThroughSteadyPlayback pins §4.4: measurements that
// keep to the anchor's extrapolation publish nothing, and wake no stream; one
// that departs from it by more than a second (a seek, a stall) is a new anchor
// under a revision of its own.
func TestThePlayerFrameIsQuietThroughSteadyPlayback(t *testing.T) {
	r := newCtlRig(t, ctlOptions{})
	ctx := context.Background()
	wantRecord(t, "a replace that plays", r.replace(r.paths(), 0, 0, true), admin.PlayerOpDone, "", "")
	base := time.Now().Add(-time.Minute)
	r.position(44100*10, base)
	first := r.c.Now(ctx)
	wake := r.c.Wake()
	for k := int64(1); k <= 5; k++ {
		r.position(44100*(10+k), base.Add(time.Duration(k)*time.Second))
		if pub := r.c.Now(ctx); pub.Revision != first.Revision {
			t.Fatalf("steady playback %d s on published revision %d (after %d): %s", k, pub.Revision, first.Revision, pub.Bytes)
		}
	}
	select {
	case <-wake:
		t.Fatal("steady playback woke the stream")
	default:
	}
	r.position(44100*60, base.Add(6*time.Second))
	pub := r.c.Now(ctx)
	if a := anchorOf(t, pub); pub.Revision == first.Revision || a == nil || a.ID != pub.Revision || a.Seconds != 60 {
		t.Fatalf("a jump to 60 s: revision %d (before %d), anchor %+v; want a new anchor under its own revision", pub.Revision, first.Revision, a)
	}
}

// awaitState waits until the latest publication's state satisfies cond, on
// the publisher's own wakes alone (as the event stream reads them), until the
// test's deadline.
func (r *ctlRig) awaitState(what string, cond func(admin.PlayerState) bool) admin.PlayerState {
	r.t.Helper()
	for {
		ch := r.c.Wake()
		if pub, ok := r.c.Latest(); ok {
			if st := stateOf(r.t, pub); cond(st) {
				return st
			}
		}
		select {
		case <-ch:
		case <-serveGiveUp(r.t):
			pub, _ := r.c.Latest()
			r.t.Fatalf("waiting for the frame: %s; the latest is %s", what, pub.Bytes)
		}
	}
}

// TestThePlayerFrameFollowsTheEngine pins R9: what another controller does is
// published by the publisher's own wakes, no handler asking: a repeat change,
// a play, a pause and an output report each reach the frame.
func TestThePlayerFrameFollowsTheEngine(t *testing.T) {
	r := newCtlRig(t, ctlOptions{})
	ctx := context.Background()
	wantRecord(t, "a replace", r.replace(r.paths(), 0, 0, false), admin.PlayerOpDone, "", "")
	if err := r.eng.SetRepeat(ctx, true); err != nil {
		t.Fatal(err)
	}
	r.awaitState("repeat on", func(st admin.PlayerState) bool { return st.Repeat })
	if err := r.eng.Play(ctx); err != nil {
		t.Fatal(err)
	}
	r.awaitState("playing", func(st admin.PlayerState) bool { return st.Transport == "playing" && st.Track != nil })
	if err := r.eng.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	r.awaitState("paused", func(st admin.PlayerState) bool { return st.Transport == "paused" })
	r.report(2, false, "device_missing")
	r.awaitState("the DAC gone", func(st admin.PlayerState) bool { return !st.Offered && st.OfferedWhy == "device_missing" })
}

// TestACommandThatChangesNothingVisibleIsAnsweredUnderARevisionOfItsOwn pins
// G3: a Pause of a stopped bridge changes nothing of the engine's, and its
// answer is a publication of its own, newer than the latest, carrying its
// record final.
func TestACommandThatChangesNothingVisibleIsAnsweredUnderARevisionOfItsOwn(t *testing.T) {
	r := newCtlRig(t, ctlOptions{})
	before := r.c.Now(context.Background())
	a := r.command(admin.PlayerCmdPause, nil)
	wantRecord(t, "a pause of a stopped bridge", a, admin.PlayerOpDone, "", "")
	if a.Player.Revision <= before.Revision {
		t.Fatalf("the pause was answered with revision %d, the latest before it being %d: not a publication of its own", a.Player.Revision, before.Revision)
	}
	st := stateOf(t, a.Player)
	found := false
	for _, rec := range st.Operations {
		if rec.Tab == r.tab && rec.Seq == a.Record.Seq && rec.Status == admin.PlayerOpDone {
			found = true
		}
	}
	if !found || st.Transport != stateOf(t, before).Transport {
		t.Fatalf("the answer's state: transport %s (before %s), operations %+v; want the pause's record, done, and nothing else moved", st.Transport, stateOf(t, before).Transport, st.Operations)
	}
}

// TestEveryExplicitCaptureCompletesWhenNothingChanged pins G12: with the
// engine idle and nothing changing, GET now and GET queue each answer at once
// with the latest publication's revision.
func TestEveryExplicitCaptureCompletesWhenNothingChanged(t *testing.T) {
	r := newCtlRig(t, ctlOptions{})
	wantRecord(t, "a replace", r.replace(r.paths(), 0, 0, false), admin.PlayerOpDone, "", "")
	latest, ok := r.c.Latest()
	if !ok {
		t.Fatal("no publication")
	}
	const bound = 2 * time.Second
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		start := time.Now()
		now := r.c.Now(ctx)
		took := time.Since(start)
		start = time.Now()
		q := r.c.Queue(ctx)
		tookQ := time.Since(start)
		cancel()
		if now.Revision != latest.Revision || took > bound {
			t.Fatalf("GET now with nothing changed: revision %d in %v, want %d at once", now.Revision, took, latest.Revision)
		}
		if q.ReadAt != latest.Revision || tookQ > bound {
			t.Fatalf("GET queue with nothing changed: read at %d in %v, want %d at once", q.ReadAt, tookQ, latest.Revision)
		}
	}
}

// TestThePublicationSaysWhenTheQueueLastChanged pins G4: queueRev moves at
// every change of the queue's IDs, the queue an engine loaded from its file
// at token 0 among them, and at nothing else: a repeat change, a track's
// advance.
func TestThePublicationSaysWhenTheQueueLastChanged(t *testing.T) {
	first := newCtlRig(t, ctlOptions{})
	wantRecord(t, "a replace", first.replace(first.paths(), 0, 0, false), admin.PlayerOpDone, "", "")
	first.await("the queue written to its file", func(s *engine.Snapshot) bool {
		p := s.Persist()
		return p.Written == p.Revision && s.Len() == 3
	})
	first.stopEngine()
	// A second engine over the first one's files, whose reservation the
	// storage refuses at its start: the queue loaded from its file is served
	// at token 0 until an edit makes the reservation again.
	var calls atomic.Int32
	seams := &engine.Seams{ReserveWrite: func() error {
		if calls.Add(1) == 1 {
			return errors.New("read-only file system")
		}
		return nil
	}}
	standIn := newPlayerStandIn()
	standIn.details = &engine.Details{Frames: 44100 * 180, RateHz: 44100, Bits: 16, Channels: 2, Codec: "FLAC", Lossless: true}
	if _, err := standIn.build(playback.Config{Resolver: first.res}); err != nil {
		t.Fatal(err)
	}
	be := &ctlBackend{playerStandIn: standIn}
	eng, err := engine.New(engine.Config{PlayerUUID: "4b1c2a3e-7f60-4b2d-9e0f-0123456789ab", Backend: be,
		QueuePath: filepath.Join(first.dataDir, playerQueueFile), ReservePath: filepath.Join(first.dataDir, playerReservationFile),
		DataDir: first.dataDir, Seams: seams})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stopRun := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { defer close(runDone); _ = eng.Run(runCtx) }()
	drainLoopOnCleanup(t, stopRun, runDone, "the second engine")
	ready := make(chan struct{})
	plan := servePlayerPlan{start: true, lists: true}
	p := &bridgePlayer{plan: plan, ready: ready, done: make(chan struct{}), settled: make(chan struct{}), closedKnown: make(chan struct{}),
		eng: eng, be: be, res: first.res}
	c := newConsoleControl(consoleControlWiring{Plan: plan, Player: p, Store: first.store, GOOS: "linux"})
	t.Cleanup(c.stop)
	ctx := context.Background()
	starting := c.Now(ctx)
	if st := stateOf(t, starting); st.Running || st.Why != reasonStarting || st.Length != 0 || starting.QueueRev == 0 {
		t.Fatalf("before the player is ready: %s", starting.Bytes)
	}
	close(ready)
	var loaded admin.PlayerPublication
	for {
		ch := c.Wake()
		if pub, ok := c.Latest(); ok && stateOf(t, pub).Length == 3 {
			loaded = pub
			break
		}
		select {
		case <-ch:
		case <-serveGiveUp(t):
			t.Fatal("the loaded queue was never published")
		}
	}
	if st := stateOf(t, loaded); st.Token != 0 || loaded.QueueRev != loaded.Revision || loaded.QueueRev <= starting.QueueRev {
		t.Fatalf("the queue loaded from its file at token %d: queueRev %d at revision %d (before: %d); want queueRev moved at this publication",
			st.Token, loaded.QueueRev, loaded.Revision, starting.QueueRev)
	}
	if err := eng.ReportOutput(ctx, engine.OutputReport{Generation: 1, Available: true}); err != nil {
		t.Fatal(err)
	}
	s := eng.Snapshot()
	entries, err := s.ReadList(s.IDs())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.InsertAfter(ctx, s.IDs()[2], entries[0].Ref, nil); err != nil {
		t.Fatal(err)
	}
	inserted := c.Now(ctx)
	if inserted.QueueRev != inserted.Revision || inserted.QueueRev <= loaded.QueueRev {
		t.Fatalf("an insert: queueRev %d at revision %d (before %d), want it moved", inserted.QueueRev, inserted.Revision, loaded.QueueRev)
	}
	if err := eng.SetRepeat(ctx, true); err != nil {
		t.Fatal(err)
	}
	repeated := c.Now(ctx)
	if repeated.Revision <= inserted.Revision || repeated.QueueRev != inserted.QueueRev {
		t.Fatalf("a repeat change: queueRev %d at revision %d, want it where it was, %d", repeated.QueueRev, repeated.Revision, inserted.QueueRev)
	}
	if err := eng.Play(ctx); err != nil {
		t.Fatal(err)
	}
	if err := eng.Next(ctx); err != nil {
		t.Fatal(err)
	}
	for {
		ch := eng.Changed()
		if sn := eng.Snapshot(); sn.Selected() == sn.IDs()[1] && sn.State() == engine.Playing {
			break
		}
		select {
		case <-ch:
		case <-serveGiveUp(t):
			t.Fatal("the next track never played")
		}
	}
	advanced := c.Now(ctx)
	if advanced.Revision <= repeated.Revision || advanced.QueueRev != inserted.QueueRev {
		t.Fatalf("a track's advance: queueRev %d at revision %d, want it where it was, %d", advanced.QueueRev, advanced.Revision, inserted.QueueRev)
	}
}

// ctlCommand is one command of the state's commands, sent to the engine as
// the console's operation sends it; applies, where set, says whether the
// state gives it a target (a delete needs an entry).
type ctlCommand struct {
	word    string
	try     func(r *ctlRig) error
	applies func(s *engine.Snapshot) bool
}

// appendOne inserts the first default track at the end of the queue, as an
// append's step does.
func appendOne(r *ctlRig) error {
	ref, err := engine.NewReference(idOf(r.t, library.Reference(defaultCtlTracks[0].path).String()))
	if err != nil {
		return err
	}
	ids := r.eng.Snapshot().IDs()
	after := uint32(0)
	if len(ids) > 0 {
		after = ids[len(ids)-1]
	}
	_, err = r.eng.InsertAfter(context.Background(), after, ref, nil)
	return err
}

// ctlCommands are every command the state may list.
var ctlCommands = []ctlCommand{
	{word: admin.PlayerCmdStop, try: func(r *ctlRig) error { return r.eng.Stop(context.Background()) }},
	{word: admin.PlayerCmdPause, try: func(r *ctlRig) error { return r.eng.Pause(context.Background()) }},
	{word: admin.PlayerCmdRepeat, try: func(r *ctlRig) error {
		return r.eng.SetRepeat(context.Background(), !r.eng.Snapshot().Repeat())
	}},
	{word: admin.PlayerCmdPlay, try: func(r *ctlRig) error { return r.eng.Play(context.Background()) }},
	{word: admin.PlayerCmdNext, try: func(r *ctlRig) error { return r.eng.Next(context.Background()) }},
	{word: admin.PlayerCmdPrevious, try: func(r *ctlRig) error { return r.eng.Previous(context.Background()) }},
	{word: admin.PlayerCmdIndex, try: func(r *ctlRig) error { return r.eng.PlayIndex(context.Background(), 0) }},
	{word: admin.PlayerCmdSeek, try: func(r *ctlRig) error { return r.eng.SeekAbsolute(context.Background(), 1) }},
	// A replace is DeleteAll and its first insert, as the console's replace
	// sends them.
	{word: cmdWordReplace, try: func(r *ctlRig) error {
		if err := r.eng.DeleteAll(context.Background()); err != nil {
			return err
		}
		return appendOne(r)
	}},
	{word: cmdWordAppend, try: appendOne},
	{word: cmdWordDelete, try: func(r *ctlRig) error {
		return r.eng.Delete(context.Background(), r.eng.Snapshot().IDs()[0])
	}, applies: func(s *engine.Snapshot) bool { return s.Len() > 0 }},
}

// ctlState is one state the engine reaches, made on a rig.
type ctlState struct {
	name string
	opts func(*ctlOptions)
	make func(r *ctlRig)
	// shared tries every command on the one rig: what it lists leaves the
	// state as it is.
	shared bool
}

// stoppedWithQueue is a rig with the default tracks queued, stopped.
func stoppedWithQueue(r *ctlRig) {
	r.t.Helper()
	wantRecord(r.t, "a replace", r.replace(r.paths(), 0, 0, false), admin.PlayerOpDone, "", "")
}

var ctlStates = []ctlState{
	{name: "stopped with an empty queue", make: func(*ctlRig) {}},
	{name: "stopped with a queue", make: stoppedWithQueue},
	{name: "playing", make: func(r *ctlRig) {
		wantRecord(r.t, "a replace that plays", r.replace(r.paths(), 0, 0, true), admin.PlayerOpDone, "", "")
		r.await("the track heard", func(s *engine.Snapshot) bool { _, ok := s.Track(); return ok && !s.Starting() })
	}},
	{name: "paused", make: func(r *ctlRig) {
		wantRecord(r.t, "a replace that plays", r.replace(r.paths(), 0, 0, true), admin.PlayerOpDone, "", "")
		r.await("the track heard", func(s *engine.Snapshot) bool { _, ok := s.Track(); return ok && !s.Starting() })
		wantRecord(r.t, "pause", r.command(admin.PlayerCmdPause, nil), admin.PlayerOpDone, "", "")
	}},
	{name: "the output gone", make: func(r *ctlRig) {
		stoppedWithQueue(r)
		r.report(2, false, "device_missing")
	}},
	{name: "maintenance", make: func(r *ctlRig) {
		stoppedWithQueue(r)
		if _, err := r.eng.EnterMaintenance(context.Background(), r.eng.Snapshot().Generation(), "restore"); err != nil {
			r.t.Fatal(err)
		}
	}},
	{name: "the DSD check", make: func(r *ctlRig) {
		stoppedWithQueue(r)
		if _, err := r.eng.EnterCheck(context.Background(), false); err != nil {
			r.t.Fatal(err)
		}
	}},
	{name: "a configuration reserved", make: func(r *ctlRig) {
		stoppedWithQueue(r)
		l, err := r.eng.ReserveConfiguration(context.Background())
		if err != nil {
			r.t.Fatal(err)
		}
		r.t.Cleanup(func() { r.eng.ReleaseConfiguration(l) })
	}},
	{name: "edits refused", opts: func(o *ctlOptions) {
		o.engSeams = &engine.Seams{ReserveWrite: func() error { return errors.New("read-only file system") }}
	}, make: func(r *ctlRig) {
		r.await("edits refused", func(s *engine.Snapshot) bool { return s.Edits().Refused != "" })
	}},
	{name: "a release unconfirmed", shared: true, opts: func(o *ctlOptions) {
		o.holdRelease = make(chan struct{})
	}, make: func(r *ctlRig) {
		wantRecord(r.t, "a replace that plays", r.replace(r.paths(), 0, 0, true), admin.PlayerOpDone, "", "")
		r.t.Cleanup(func() { close(r.standIn.holdRelease) })
		go func() { _ = r.eng.Stop(context.Background()) }()
		r.await("the release unconfirmed", func(s *engine.Snapshot) bool { _, ok := s.ReleaseUnconfirmed(); return ok })
	}},
}

// stateRig makes a rig in st.
func stateRig(t *testing.T, st ctlState) *ctlRig {
	t.Helper()
	o := ctlOptions{}
	if st.opts != nil {
		st.opts(&o)
	}
	r := newCtlRig(t, o)
	st.make(r)
	return r
}

// TestTheCommandsOfferedAreTheOnesTheEngineTakes pins §4.3's commands
// (doc.go's table read off the snapshot): in every state the engine reaches
// here, each command the state lists is taken, sent alone on a rig in that
// state, and each it leaves out is refused, changing nothing.
func TestTheCommandsOfferedAreTheOnesTheEngineTakes(t *testing.T) {
	for _, st := range ctlStates {
		t.Run(st.name, func(t *testing.T) {
			r := stateRig(t, st)
			listed := stateOf(t, r.c.Now(context.Background())).Commands
			var taken []ctlCommand
			for _, c := range ctlCommands {
				if c.applies != nil && !c.applies(r.eng.Snapshot()) {
					continue
				}
				if slices.Contains(listed, c.word) {
					taken = append(taken, c)
					continue
				}
				gen := r.eng.Snapshot().Generation()
				if err := c.try(r); err == nil {
					t.Errorf("%s is not listed (%v) and the engine took it", c.word, listed)
				} else if after := r.eng.Snapshot().Generation(); after != gen {
					t.Errorf("%s, refused (%v), moved the generation", c.word, err)
				}
			}
			for _, c := range taken {
				rr := r
				if !st.shared {
					rr = stateRig(t, st)
				}
				if err := c.try(rr); err != nil {
					t.Errorf("%s is listed (%v) and the engine refused it: %v", c.word, listed, err)
				}
			}
		})
	}
}

// TestStopIsOfferedWhereverTheEngineTakesIt pins §4.3: Stop is offered while
// the engine runs, under the DSD check, maintenance, a release unconfirmed and
// the output gone, and the engine takes it there; under a hold it is the one
// command offered.
func TestStopIsOfferedWhereverTheEngineTakesIt(t *testing.T) {
	for _, st := range ctlStates {
		alone := st.name == "maintenance" || st.name == "the DSD check"
		if !alone && st.name != "a release unconfirmed" && st.name != "the output gone" {
			continue
		}
		t.Run(st.name, func(t *testing.T) {
			r := stateRig(t, st)
			s := stateOf(t, r.c.Now(context.Background()))
			if !slices.Contains(s.Commands, admin.PlayerCmdStop) {
				t.Fatalf("Stop is not offered (%v)", s.Commands)
			}
			if alone && !slices.Equal(s.Commands, []string{admin.PlayerCmdStop}) {
				t.Errorf("under a hold the commands are %v, want Stop alone", s.Commands)
			}
			if err := r.eng.Stop(context.Background()); err != nil {
				t.Fatalf("the engine refused the Stop offered: %v", err)
			}
		})
	}
}

// goBarBoot is a boot snapshot that runs the player on the GO bar, bound by
// its model.
func goBarBoot() *config.Config {
	return &config.Config{Output: config.OutputConfig{Player: config.PlayerConfig{Enabled: true,
		Device: config.PlayerDevice{Binding: config.BindingUniqueModel, VendorID: "20b1", ProductID: "3009"}}}}
}

// TestThePlayerFrameSaysWhyTheBridgeIsNotOffered pins §4.3's offered and
// its why: off at boot, starting, the start failed, the engine closed, the
// DAC unplugged, the player turned off live, another DAC chosen; and the
// check's hold and a release unconfirmed, which the frame names apart.
func TestThePlayerFrameSaysWhyTheBridgeIsNotOffered(t *testing.T) {
	needsDirectorySync(t)
	bare := func(t *testing.T, plan servePlayerPlan, ready bool) *consoleControl {
		p := &bridgePlayer{plan: plan, ready: make(chan struct{}), done: make(chan struct{}), settled: make(chan struct{}), closedKnown: make(chan struct{})}
		if ready {
			close(p.ready)
		}
		c := newConsoleControl(consoleControlWiring{Plan: plan, Player: p, GOOS: "linux"})
		t.Cleanup(c.stop)
		return c
	}
	for _, tc := range []struct {
		name, why string
		c         func(t *testing.T) *consoleControl
	}{
		{"off at boot", "off", func(t *testing.T) *consoleControl { return bare(t, servePlayerPlan{lists: true}, true) }},
		{"starting", reasonStarting, func(t *testing.T) *consoleControl { return bare(t, servePlayerPlan{start: true, lists: true}, false) }},
		{"the start failed", reasonStartFailed, func(t *testing.T) *consoleControl {
			return bare(t, servePlayerPlan{start: true, lists: true}, true)
		}},
		{"the engine closed", reasonClosed, func(t *testing.T) *consoleControl {
			r := newCtlRig(t, ctlOptions{})
			r.stopEngine()
			return r.c
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.c(t)
			st := stateOf(t, c.Now(context.Background()))
			if st.Running || st.Why != tc.why || st.Offered || st.OfferedWhy != tc.why {
				t.Fatalf("running %v why %q, offered %v why %q; want not running, not offered, %q", st.Running, st.Why, st.Offered, st.OfferedWhy, tc.why)
			}
		})
	}
	live := goBarBoot()
	var mu sync.Mutex
	liveNow := func() *config.Config {
		mu.Lock()
		defer mu.Unlock()
		return live
	}
	setLive := func(c *config.Config) {
		mu.Lock()
		live = c
		mu.Unlock()
	}
	for _, tc := range []struct {
		name, why string
		change    func(r *ctlRig)
	}{
		{"the DAC unplugged", "device_missing", func(r *ctlRig) { r.report(2, false, "device_missing") }},
		{"turned off live", "disabled", func(*ctlRig) {
			off := goBarBoot()
			off.Output.Player.Enabled = false
			setLive(off)
		}},
		{"another DAC chosen", reasonBindingChanged, func(*ctlRig) {
			other := goBarBoot()
			other.Output.Player.Device.ProductID = "300a"
			setLive(other)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setLive(goBarBoot())
			r := newCtlRig(t, ctlOptions{boot: goBarBoot(), live: liveNow})
			if st := stateOf(t, r.c.Now(context.Background())); !st.Running || !st.Offered {
				t.Fatalf("before: running %v, offered %v (%q)", st.Running, st.Offered, st.OfferedWhy)
			}
			tc.change(r)
			st := stateOf(t, r.c.Now(context.Background()))
			if !st.Running || st.Offered || st.OfferedWhy != tc.why {
				t.Fatalf("running %v, offered %v why %q; want running, not offered, %q", st.Running, st.Offered, st.OfferedWhy, tc.why)
			}
		})
	}
	t.Run("the DSD check", func(t *testing.T) {
		r := newCtlRig(t, ctlOptions{})
		stoppedWithQueue(r)
		if _, err := r.eng.EnterCheck(context.Background(), false); err != nil {
			t.Fatal(err)
		}
		if st := stateOf(t, r.c.Now(context.Background())); st.Hold != holdCheck || st.Silent {
			t.Fatalf("under the check: hold %q, silent %v; want the check's hold, not silent", st.Hold, st.Silent)
		}
	})
	t.Run("a release unconfirmed", func(t *testing.T) {
		var st ctlState
		for _, s := range ctlStates {
			if s.name == "a release unconfirmed" {
				st = s
			}
		}
		r := stateRig(t, st)
		if s := stateOf(t, r.c.Now(context.Background())); s.Release.State != releaseUnconfirmed || s.Release.Since == "" || s.Silent {
			t.Fatalf("a release unconfirmed: %+v, silent %v; want unconfirmed since when, not silent", s.Release, s.Silent)
		}
	})
}
