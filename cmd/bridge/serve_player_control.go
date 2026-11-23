package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/dlna"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/hotplug"
	"github.com/acoseac/1-bit-bridge/internal/player/library"
	"github.com/acoseac/1-bit-bridge/internal/player/openhome"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/trackref"
)

// The console's control of serve's player (B634, PR 1): admin.PlayerControl,
// which consoleControl is, over the boot plan, the bridgePlayer serve
// started, the library index and the player's resolver. It edits the
// engine's queue as every OpenHome controller does: a replace is a DeleteAll
// and its inserts, the first step conditioned on the tab's generation and the
// later ones not, the queue's IDs compared after each step, and the last
// admitted command wins. It adds no engine command and keeps no queue of its
// own: the engine stays the one owner of the queue and the state.
//
// Every command is an operation, named by its tab, its count in that tab and
// the epoch it was sent in, run one at a time in the order of arrival: one
// that arrives ends the older ones' steps not yet sent, and every operation
// but a Stop waits for every older one not yet ended, whatever Stops came
// between, so the steps of two operations never interleave. A step runs on a context the request's end
// does not end, and the control waits for the engine's own answer, which every
// command the engine takes gets. The record of each tab's last operation is
// kept, for the 64 tabs that ran one most recently, and each tab's mark (the
// highest count it sent) for the 4,096 most recent; the last eight records go
// in every publication.

// The control's bounds: the records and marks it keeps, how long a handler
// waits for its operation before it answers running, the barrier's bound, the
// margin a replace without play waits for a release past the engine's own
// deadline, the seek's tries, and the window's reach before the start track.
const (
	controlRecordTabs  = 64
	controlMarkTabs    = 4096
	controlHandlerWait = 30 * time.Second
	controlBarrier     = 10 * time.Second
	controlReleaseGap  = 500 * time.Millisecond
	controlSeekTries   = 3
	controlBarrierTry  = 3
	controlWindowAhead = 100
	controlStopBound   = 2 * time.Second
	controlLookupBound = 2 * time.Second
	controlSettlePoll  = 20 * time.Millisecond
)

// The words of a record's error, beside the engine's own (wordOf).
const (
	wordStale          = "stale"
	wordNotPlayable    = "not_playable"
	wordQueueFull      = "queue_full"
	wordNotRunning     = "player_not_running"
	wordNotFound       = "not_found"
	wordTone           = "tone"
	wordEditsRefused   = "edits_refused"
	wordEntryNotFound  = "entry_not_found"
	wordPlayerRefused  = "player_refused"
	wordNotInLibrary   = "not_in_library"
	wordMetadataTooBig = "metadata_too_large"
	wordMetadataNotXML = "metadata_not_xml"
	wordReferenceLong  = "reference_too_long"
)

// The kinds of an operation.
const (
	opKindReplace   = "replace"
	opKindAppend    = "append"
	opKindDelete    = "delete"
	opKindTransport = "transport"
	opKindSwitchOut = "switch-out"
)

// factsBackend is a backend that publishes the player's DSD and its live
// observation (playback.Backend.Facts).
type factsBackend interface {
	Facts() (playback.PlayerPolicy, *playback.PlaybackFacts)
}

// controlStore is what the control reads of the library index: the catalog's
// rows a replace's checks ask, and a track's whole row its entry's metadata is
// built from.
type controlStore interface {
	CatalogTrackRowsForPaths(ctx context.Context, paths []string) ([]manifest.CatalogTrackRow, error)
	GetTrack(ctx context.Context, path string) (*manifest.Track, error)
}

// dlnaArtworkBase is what the control asks of the DLNA listener: the base an
// albumArtURI naming a host is built on (dlna.Server.ArtworkBase).
type dlnaArtworkBase interface {
	ArtworkBase(netip.Addr) (string, bool)
}

// controlSeams are a test's holds on the control between its steps; nil in
// production. Each runs on the goroutine that reaches it.
type controlSeams struct {
	// afterStep runs after each engine step's answer, on the operation's
	// goroutine, with the step's name and its answer.
	afterStep func(kind, step string, err error)
	// beforeSend runs in a step that found its operation's unsent steps not
	// ended, before its engine command is sent, with the step's name.
	beforeSend func(kind, step string)
	// awaitPrev runs on an operation's goroutine before it waits for an
	// older operation's end, with that operation's kind.
	awaitPrev func(kind, prev string)
	// afterSilence runs in the barrier once it has observed the bridge
	// silent, before the capture that answers it.
	afterSilence func()
	// afterBase runs in an append once its base is read and found at the
	// tab's generation, before its first insert.
	afterBase func()
	// beforePublish runs on the publisher's goroutine between a capture's
	// reads and its publish.
	beforePublish func()
	// afterFinal runs on the operation's goroutine once the publication that
	// answers it is made, before its end is signalled, with its kind.
	afterFinal func(kind string)
	// Bounds in place of production's, where set.
	handlerWait, barrier, releaseGap time.Duration
}

// consoleControlWiring is what serve hands the console's control.
type consoleControlWiring struct {
	Plan   servePlayerPlan
	Player *bridgePlayer
	// View is the console's view of the player, whose withholding and track
	// naming the control shares.
	View  *consolePlayer
	Boot  *config.Config
	Live  func() *config.Config
	Watch func() <-chan struct{}
	Store controlStore
	// Hosts is the inventory an OpenHome Insert's URI is confined to
	// (newPlayerHosts): a console entry's reference is written on the first
	// host it takes.
	Hosts openhome.Inventory
	GOOS  string
	Seams *controlSeams
	Now   func() time.Time
}

// consoleControl is the console's control of serve's player.
type consoleControl struct {
	plan  servePlayerPlan
	p     *bridgePlayer
	view  *consolePlayer
	boot  *config.Config
	live  func() *config.Config
	watch func() <-chan struct{}
	store controlStore
	hosts openhome.Inventory
	goos  string
	seams *controlSeams
	clock func() time.Time
	epoch string
	dlna  atomic.Pointer[dlnaHeld]
	pub   *publisher
	quit  chan struct{}
	wg    sync.WaitGroup

	mu     sync.Mutex
	closed bool
	// unfinished are the operations admitted and not yet ended, oldest first.
	unfinished []*operation
	marks      map[string]markEntry
	records    map[string]*opRecord
	recent     []*opRecord
	order      uint64
}

// dlnaHeld is the DLNA listener the control asks, once DLNA has started.
type dlnaHeld struct{ a dlnaArtworkBase }

// markEntry is a tab's mark: the highest count it sent, and when, by the
// control's count of operations.
type markEntry struct {
	seq   uint64
	order uint64
}

// opRecord is a record as the control keeps it: admin's, the command's
// digest, and when it was made, by the control's count.
type opRecord struct {
	rec    admin.PlayerRecord
	digest [32]byte
	order  uint64
	final  bool
	op     *operation
}

var _ admin.PlayerControl = (*consoleControl)(nil)

// newConsoleControl is the console's control of serve's player, its
// publisher running; its epoch is 16 random bytes, one per serve.
func newConsoleControl(w consoleControlWiring) *consoleControl {
	goos := w.GOOS
	if goos == "" {
		goos = goosOrHost()
	}
	now := w.Now
	if now == nil {
		now = time.Now
	}
	var b [16]byte
	_, _ = rand.Read(b[:])
	c := &consoleControl{plan: w.Plan, p: w.Player, view: w.View, boot: w.Boot, live: w.Live, watch: w.Watch, store: w.Store,
		hosts: w.Hosts, goos: goos, seams: w.Seams, clock: now, epoch: hex.EncodeToString(b[:]), quit: make(chan struct{}),
		marks: map[string]markEntry{}, records: map[string]*opRecord{}}
	c.pub = newPublisher(c)
	c.pub.start()
	return c
}

// setDLNA hands the control the DLNA listener, once DLNA has started.
func (c *consoleControl) setDLNA(a dlnaArtworkBase) { c.dlna.Store(&dlnaHeld{a}) }

// stop takes no operation from now on, ends the publisher and joins it and
// every operation's goroutine within controlStopBound: serve calls it once its
// player has stopped, so every step an operation waits on has its answer
// (ReasonClosed at the latest), and before the store closes.
func (c *consoleControl) stop() {
	c.mu.Lock()
	already := c.closed
	c.closed = true
	c.mu.Unlock()
	if already {
		return
	}
	close(c.quit)
	ended := c.pub.stop(controlStopBound)
	done := make(chan struct{})
	go func() { c.wg.Wait(); close(done) }()
	t := time.NewTimer(controlStopBound)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		ended = false
	}
	if !ended {
		playerLog.Warn("player control: an operation or the publisher did not end within its bound; it is abandoned and ends with the process", "bound", controlStopBound)
	}
}

// Platform is the host the player runs on.
func (c *consoleControl) Platform() string { return c.goos }

// Lists reports whether serve's boot plan lists the player.
func (c *consoleControl) Lists() bool { return c.plan.lists }

func (c *consoleControl) now() time.Time { return c.clock() }

// handlerWait, barrierBound and releaseGap are the control's bounds.
func (c *consoleControl) handlerWait() time.Duration {
	if s := c.seams; s != nil && s.handlerWait > 0 {
		return s.handlerWait
	}
	return controlHandlerWait
}

func (c *consoleControl) barrierBound() time.Duration {
	if s := c.seams; s != nil && s.barrier > 0 {
		return s.barrier
	}
	return controlBarrier
}

func (c *consoleControl) releaseGap() time.Duration {
	if s := c.seams; s != nil && s.releaseGap > 0 {
		return s.releaseGap
	}
	return controlReleaseGap
}

// engine is the running player's engine and why not: nil with off (the plan
// starts no player), starting (not ready yet) or start_failed (ready with no
// engine); the engine itself once it runs, whose Done and snapshot say
// whether it has closed.
func (c *consoleControl) engine() (*engine.Engine, string) {
	switch {
	case c.p == nil || !c.plan.start:
		return nil, "off"
	case !signalled(c.p.ready):
		return nil, reasonStarting
	case c.p.eng == nil:
		return nil, reasonStartFailed
	}
	return c.p.eng, ""
}

// runningEngine is the engine while it runs, and the word for why not.
func (c *consoleControl) runningEngine() (*engine.Engine, string) {
	eng, why := c.engine()
	if eng == nil {
		return nil, why
	}
	if signalled(eng.Done()) || eng.Snapshot().Closed() {
		return nil, reasonClosed
	}
	return eng, ""
}

// backend is the running player's backend, nil before it runs.
func (c *consoleControl) backend() playBackend {
	if c.p == nil || !c.plan.start || !signalled(c.p.ready) {
		return nil
	}
	return c.p.be
}

// resolver is the player's library resolver, nil before it runs.
func (c *consoleControl) resolver() *library.Resolver {
	if c.p == nil || !c.plan.start || !signalled(c.p.ready) {
		return nil
	}
	return c.p.res
}

// engineSignals are the publisher's wakes from the player: the engine's
// Changed, taken now, its Done, the player's ready while it has not closed,
// and the closed engine's facts while they are not known.
func (c *consoleControl) engineSignals() (changed, ready, closedKnown <-chan struct{}) {
	if c.p == nil {
		return nil, nil, nil
	}
	if !signalled(c.p.ready) {
		ready = c.p.ready
	}
	if !signalled(c.p.closedKnown) {
		closedKnown = c.p.closedKnown
	}
	if eng, _ := c.engine(); eng != nil {
		changed = eng.Changed()
	}
	return changed, ready, closedKnown
}

// cfgChanged is the live config holder's channel, closed at its next change.
func (c *consoleControl) cfgChanged() <-chan struct{} {
	if c.watch == nil {
		return nil
	}
	return c.watch()
}

// liveConfig is the live config, nil without one.
func (c *consoleControl) liveConfig() *config.Config {
	if c.live == nil {
		return c.boot
	}
	return c.live()
}

// feedOf is the player's hotplug feed, nil for none.
func (p *bridgePlayer) feedOf() *hotplug.Feed {
	if p == nil {
		return nil
	}
	return p.feed
}

// trackOf is the occurrence heard as the state names it: by the library path
// its reference names and the row's names (the console's view's lookup,
// once an occurrence), never a controller's metadata.
func (c *consoleControl) trackOf(t engine.Track) *admin.PlayerStateTrack {
	out := &admin.PlayerStateTrack{Entry: t.Entry}
	if t.HasDetails {
		d := t.Details
		out.Codec, out.Bits, out.RateHz, out.Channels = d.Codec, d.Bits, d.RateHz, d.Channels
		if d.RateHz > 0 && d.Frames > 0 {
			out.Duration = float64(d.Frames) / float64(d.RateHz)
		}
	}
	if c.view != nil && c.p != nil {
		ctx, cancel := context.WithTimeout(context.Background(), consoleLookupBound)
		defer cancel()
		if n, ok := c.view.name(ctx, t); ok {
			out.Path, out.Named, out.Title, out.Artist, out.Album = n.path, n.row, n.title, n.artist, n.album
		}
	}
	return out
}

// recentRecords is the last recentRecords records, whatever their tab,
// oldest first, without their handovers.
func (c *consoleControl) recentRecords() []admin.PlayerRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]admin.PlayerRecord, 0, len(c.recent))
	for _, r := range c.recent {
		rec := r.rec
		rec.Handover = nil
		rec.LeftOut = nil
		out = append(out, rec)
	}
	return out
}

// Now is a publication captured now.
func (c *consoleControl) Now(ctx context.Context) admin.PlayerPublication {
	return c.pub.ask(ctx, askRead, nil).PlayerPublication
}

// Latest is the latest publication.
func (c *consoleControl) Latest() (admin.PlayerPublication, bool) {
	pub := c.pub.latestPub()
	return pub.PlayerPublication, pub.Revision > 0
}

// Wake is closed at the next publication.
func (c *consoleControl) Wake() <-chan struct{} { return c.pub.wakeCh() }

// Queue is the queue of a publication captured now, each entry named by the
// library path its reference names (the resolver's StoredPath), or its word.
func (c *consoleControl) Queue(ctx context.Context) admin.PlayerQueue {
	pub := c.pub.ask(ctx, askRead, nil)
	q := admin.PlayerQueue{Epoch: pub.Epoch, ReadAt: pub.Revision, QueueRev: pub.QueueRev, Entries: []admin.PlayerQueueEntry{}}
	s := pub.snap
	if s == nil {
		return q
	}
	q.Token, q.Selected = s.Token(), s.Selected()
	entries, err := s.ReadList(s.IDs())
	if err != nil {
		return q
	}
	for _, e := range entries {
		qe := admin.PlayerQueueEntry{ID: e.ID}
		qe.Path, qe.Word = c.pathOf(ctx, e.Ref)
		if sk, ok := s.Skipped(e.ID); ok && qe.Word == "" {
			qe.Word = string(sk.Reason)
		}
		q.Entries = append(q.Entries, qe)
	}
	return q
}

// pathOf is the library path ref names, or the word for why not.
func (c *consoleControl) pathOf(ctx context.Context, ref engine.Reference) (string, string) {
	res := c.resolver()
	if res == nil {
		return "", wordNotRunning
	}
	lctx, cancel := context.WithTimeout(ctx, controlLookupBound)
	defer cancel()
	p, err := res.StoredPath(lctx, ref)
	if err != nil {
		if w := engine.ReasonOf(err); w != "" {
			return "", string(w)
		}
		return "", "track_gone"
	}
	return p, ""
}

// Plays is what the running player plays: its containers, with dsf and dff
// while its DSD set plays a rate, and each DSD rate's way in that set.
func (c *consoleControl) Plays() (admin.PlayerPlays, bool) {
	if eng, _ := c.runningEngine(); eng == nil {
		return admin.PlayerPlays{}, false
	}
	plays := admin.PlayerPlays{Codecs: playedCodecs(), DSD: map[int]admin.PlayerBridgeWay{}}
	fb, ok := c.backend().(factsBackend)
	if !ok {
		return plays, true
	}
	policy, _ := fb.Facts()
	for _, t := range policy.Tuples {
		way := admin.BridgeWayNative
		if playback.TupleTransport(t.Tuple) == playback.TransportDoP {
			way = admin.BridgeWayDoP
		}
		plays.DSD[t.Tuple.BitRate] = admin.PlayerBridgeWay{Way: way}
	}
	for _, r := range policy.Converted {
		if _, set := plays.DSD[r.BitRate]; !set {
			plays.DSD[r.BitRate] = admin.PlayerBridgeWay{Way: admin.BridgeWayConverted}
		}
	}
	if v := policy.DSD; v != nil {
		for _, r := range v.Refused {
			if _, set := plays.DSD[r.BitRate]; !set {
				plays.DSD[r.BitRate] = admin.PlayerBridgeWay{Way: admin.BridgeWayNone, Reason: r.Reason}
			}
		}
	}
	if policy.PlaysDSD() {
		plays.Codecs = append(plays.Codecs, "dsf", "dff")
	}
	return plays, true
}

// operation is one operation as it runs: its record, whether it is a Stop
// (sent at once), whether its steps not yet sent were ended, and its end.
type operation struct {
	c    *consoleControl
	r    *opRecord
	stop bool

	mu    sync.Mutex
	ended bool
	// endCh is closed once o's unsent steps are ended, which every wait of
	// o's between its steps (a release, the barrier's settle) selects on.
	endCh   chan struct{}
	endOnce sync.Once

	// The operation's goroutine's alone: whether a step was answered nil and
	// whether one answered an effect not known.
	anyNil, anyUnknown bool

	done chan struct{}
	pub  publication
}

// endUnsent ends o's steps not yet sent: o sends nothing more, its step in
// flight is still answered and recorded, and its waits between steps end.
func (o *operation) endUnsent() {
	o.mu.Lock()
	o.ended = true
	o.mu.Unlock()
	o.endOnce.Do(func() { close(o.endCh) })
}

// endedNow reports whether o's unsent steps were ended.
func (o *operation) endedNow() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.ended
}

// admit checks a request's identity and, for a new operation, makes its
// record, sets its tab's mark and puts it in the control's order, ending the
// older operation's unsent steps: in that order, another epoch is
// epoch_changed, a count below the tab's mark replayed, one at the mark
// answers its record (another command under it operation_conflict while the
// record is kept, operation_gone once it is not), and one above it is a new
// operation. Only a new operation is admitted, ending the unsent steps of
// every older one not yet ended; prevs are those older ones, whose ends every
// operation but a Stop waits for, a Stop's among them: each one's step in
// flight answered and its record final (it sends nothing more once ended), so
// the steps of two operations never interleave, a step a Stop overtook in
// flight included, and the newer one starts once every older one's record
// says how it ended.
func (c *consoleControl) admit(op admin.PlayerOp, digest [32]byte, kind, command string) (o *operation, prevs []*operation, a admin.PlayerAnswer, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	refuse := func(code string, status int) (*operation, []*operation, admin.PlayerAnswer, bool) {
		return nil, nil, admin.PlayerAnswer{Refused: code, Status: status, Player: c.pub.latestPub().PlayerPublication}, false
	}
	switch {
	case c.closed:
		ans := admin.PlayerAnswer{Refused: wordNotRunning, Reason: reasonClosed, Status: http.StatusConflict, Player: c.pub.latestPub().PlayerPublication}
		return nil, nil, ans, false
	case op.Epoch != c.epoch:
		return refuse("epoch_changed", http.StatusConflict)
	}
	m, marked := c.marks[op.Tab]
	switch {
	case marked && op.Seq < m.seq:
		return refuse("replayed", http.StatusConflict)
	case marked && op.Seq == m.seq:
		r := c.records[op.Tab]
		if r == nil || r.rec.Seq != op.Seq {
			return refuse("operation_gone", http.StatusNotFound)
		}
		if r.digest != digest {
			return refuse("operation_conflict", http.StatusConflict)
		}
		rec := r.rec
		ans := admin.PlayerAnswer{Record: &rec, Player: c.pub.latestPub().PlayerPublication}
		return nil, nil, ans, false
	}
	c.order++
	c.marks[op.Tab] = markEntry{seq: op.Seq, order: c.order}
	if len(c.marks) > controlMarkTabs {
		c.evictMark()
	}
	r := &opRecord{digest: digest, order: c.order, rec: admin.PlayerRecord{Tab: op.Tab, Seq: op.Seq, Kind: kind, Command: command,
		Status: admin.PlayerOpRunning, Effect: admin.PlayerEffectNone, Last: "", Release: "none"}}
	c.records[op.Tab] = r
	if len(c.records) > controlRecordTabs {
		c.evictRecord()
	}
	c.recent = append(c.recent, r)
	if len(c.recent) > recentRecords {
		c.recent = c.recent[len(c.recent)-recentRecords:]
	}
	o = &operation{c: c, r: r, stop: kind == opKindTransport && command == admin.PlayerCmdStop, done: make(chan struct{}), endCh: make(chan struct{})}
	r.op = o
	for _, u := range c.unfinished {
		u.endUnsent()
	}
	if !o.stop {
		prevs = slices.Clone(c.unfinished)
	}
	c.unfinished = append(c.unfinished, o)
	return o, prevs, admin.PlayerAnswer{}, true
}

// evictMark drops the oldest tab's mark; the caller holds mu.
func (c *consoleControl) evictMark() {
	oldest, at := "", uint64(0)
	for tab, m := range c.marks {
		if oldest == "" || m.order < at {
			oldest, at = tab, m.order
		}
	}
	delete(c.marks, oldest)
}

// evictRecord drops the record of the tab that ran an operation least
// recently; the caller holds mu.
func (c *consoleControl) evictRecord() {
	oldest, at := "", uint64(0)
	for tab, r := range c.records {
		if oldest == "" || r.order < at {
			oldest, at = tab, r.order
		}
	}
	delete(c.records, oldest)
}

// set changes o's record under the control's lock, and wakes the publisher:
// a change of an operation's record.
func (o *operation) set(f func(r *admin.PlayerRecord)) {
	o.c.mu.Lock()
	f(&o.r.rec)
	o.c.mu.Unlock()
	o.c.pub.recordChanged()
}

// record is o's record as it stands.
func (o *operation) record() admin.PlayerRecord {
	o.c.mu.Lock()
	defer o.c.mu.Unlock()
	return o.r.rec
}

// errStepEnded is a step not sent: its operation's unsent steps were ended.
var errStepEnded = errors.New("player control: the operation's steps not yet sent were ended")

// step sends one engine command for o, on a context the request's end does
// not end, under engine.WithAdmission, unless o's unsent steps were ended
// (errStepEnded, nothing sent); it records the step and its answer, and
// what its answer says of the effect.
func (o *operation) step(name string, f func(ctx context.Context) error) error {
	o.mu.Lock()
	if o.ended {
		o.mu.Unlock()
		return errStepEnded
	}
	o.mu.Unlock()
	o.set(func(r *admin.PlayerRecord) { r.Sent++ })
	ctx, _ := engine.WithAdmission(context.Background())
	if s := o.c.seams; s != nil && s.beforeSend != nil {
		s.beforeSend(o.r.rec.Kind, name)
	}
	err := f(ctx)
	switch effectOfStep(name, err) {
	case admin.PlayerEffectSome:
		o.anyNil = true
	case admin.PlayerEffectUnknown:
		o.anyUnknown = true
	}
	o.set(func(r *admin.PlayerRecord) {
		r.Last = lastWord(err)
		r.Effect = o.effect()
	})
	if s := o.c.seams; s != nil && s.afterStep != nil {
		s.afterStep(o.r.rec.Kind, name, err)
	}
	return err
}

// effect is o's effect from its steps' answers: some once one answered nil,
// unknown once one answered an effect not known, none otherwise.
func (o *operation) effect() string {
	switch {
	case o.anyNil:
		return admin.PlayerEffectSome
	case o.anyUnknown:
		return admin.PlayerEffectUnknown
	}
	return admin.PlayerEffectNone
}

// lastWord is a step's answer as a record's Last says it: ok, or the
// engine's word.
func lastWord(err error) string {
	if err == nil {
		return "ok"
	}
	if w := engine.ReasonOf(err); w != "" {
		return string(w)
	}
	return "error"
}

// preEffect are the engine's refusals each step answers before it changes
// anything (J2, K8): a command that did not run (not admitted, overtaken by a
// Stop), one refused stale or under a hold, which dispatch decides before the
// command; and each command's own guards, which it asks before its first
// change (the transport's before its selection and its seek state move, an
// edit's before its queue does, a seek's before its target is set). Every
// other answer is an effect not known: the engine's timeout of a call (which
// releases the session), its shutdown, and a start that failed after the
// command moved the selection among them.
var preEffect = map[string][]engine.Reason{
	"*": {engine.ReasonConflict, engine.ReasonMaintenance, engine.ReasonOvertaken},
	"insert": {engine.ReasonBadReference, engine.ReasonIDNotFound, engine.ReasonMetadataTooLarge, engine.ReasonFull,
		engine.ReasonFileStuck, engine.ReasonIDsExhausted, engine.ReasonReservation},
	"delete":     {engine.ReasonIDNotFound, engine.ReasonFileStuck, engine.ReasonIDsExhausted, engine.ReasonReservation},
	"delete_all": {engine.ReasonFileStuck, engine.ReasonIDsExhausted, engine.ReasonReservation},
	"play": {engine.ReasonReleaseUnconfirmed, engine.ReasonOutputUnknown, engine.ReasonOutputUnavailable,
		engine.ReasonConfiguring, engine.ReasonEmptyQueue},
	"play_id": {engine.ReasonReleaseUnconfirmed, engine.ReasonIDNotFound, engine.ReasonOutputUnknown,
		engine.ReasonOutputUnavailable, engine.ReasonConfiguring, engine.ReasonEmptyQueue},
	"play_index": {engine.ReasonReleaseUnconfirmed, engine.ReasonIndexNotFound, engine.ReasonOutputUnknown,
		engine.ReasonOutputUnavailable, engine.ReasonConfiguring, engine.ReasonEmptyQueue},
	"next": {engine.ReasonReleaseUnconfirmed, engine.ReasonOutputUnknown, engine.ReasonOutputUnavailable,
		engine.ReasonConfiguring, engine.ReasonEmptyQueue},
	"previous": {engine.ReasonReleaseUnconfirmed, engine.ReasonOutputUnknown, engine.ReasonOutputUnavailable,
		engine.ReasonConfiguring, engine.ReasonEmptyQueue},
	"seek": {engine.ReasonSeekUnavailable, engine.ReasonReleaseUnconfirmed, engine.ReasonOutputUnknown,
		engine.ReasonOutputUnavailable, engine.ReasonConfiguring, engine.ReasonSeekUnsupported, engine.ReasonSeekOutOfRange},
}

// effectOfStep is what step's answer err proves of its effect: some for nil,
// none for an answer that proves the command changed nothing (preEffect, and
// a command its context's end kept from being admitted), unknown otherwise.
func effectOfStep(step string, err error) string {
	switch {
	case err == nil:
		return admin.PlayerEffectSome
	case errors.Is(err, errStepEnded), errors.Is(err, engine.ErrNotAdmitted):
		return admin.PlayerEffectNone
	}
	reason := engine.ReasonOf(err)
	if slices.Contains(preEffect["*"], reason) || slices.Contains(preEffect[step], reason) {
		return admin.PlayerEffectNone
	}
	return admin.PlayerEffectUnknown
}

// wordOf is a refused step's word as a record's error says it (the design's
// §5.3 table): the engine's reasons mapped, a hold by what holds the engine,
// and a reason the table does not name player_refused, the engine's word in
// the record's Last.
func wordOf(err error, s *engine.Snapshot) string {
	switch r := engine.ReasonOf(err); r {
	case engine.ReasonConflict:
		return wordStale
	case engine.ReasonMaintenance:
		if s != nil {
			if m, ok := s.Maintenance(); ok && m.Reason == holdCheck {
				return holdCheck
			}
		}
		return holdMaintenance
	case engine.ReasonIDNotFound, engine.ReasonIndexNotFound:
		return wordEntryNotFound
	case engine.ReasonFull:
		return wordQueueFull
	case engine.ReasonReservation, engine.ReasonFileStuck, engine.ReasonIDsExhausted:
		return wordEditsRefused
	case engine.ReasonConfiguring, engine.ReasonOutputUnknown, engine.ReasonOutputUnavailable, engine.ReasonReleaseUnconfirmed,
		engine.ReasonEmptyQueue, engine.ReasonSeekUnavailable, engine.ReasonSeekOutOfRange, engine.ReasonSeekUnsupported,
		engine.ReasonOvertaken, engine.ReasonUnplayable, engine.ReasonAllUnplayable, engine.ReasonBackend, engine.ReasonTimeout,
		engine.ReasonMetadataTooLarge, engine.ReasonBadReference:
		return string(r)
	}
	return wordPlayerRefused
}

// statusOfErr is the final status a step's error ends its operation with:
// superseded is another controller's (changed), the engine's shutdown or a
// cancellation of an admitted command an outcome not known, every other a
// refusal.
func statusOfErr(err error) string {
	switch r := engine.ReasonOf(err); {
	case r == engine.ReasonSuperseded:
		return admin.PlayerOpChanged
	case r == engine.ReasonClosed, r == engine.ReasonCancelled && !errors.Is(err, engine.ErrNotAdmitted):
		return admin.PlayerOpUnknown
	}
	return admin.PlayerOpRefused
}

// finish sets o's record final on the publisher's capture, through eval,
// which sees that capture (J1, K10): the status it answers, the record's
// error its word, the record published under the next revision, which
// answers.
func (o *operation) finish(eval func(cap *capture) (status, word string)) {
	o.settleOn(func(cap *capture) (bool, string, string) {
		status, word := eval(cap)
		return true, status, word
	})
}

// settleOn asks the publisher for a capture on which eval judges o's
// outcome: final, o's record is set final within that capture and published
// under the next revision, which answers o; not final (a barrier whose
// answering capture found the bridge sounding again), the record stays
// running and the capture is published all the same. It reports whether o
// ended.
func (o *operation) settleOn(eval func(cap *capture) (final bool, status, word string)) bool {
	final := false
	pub := o.c.pub.ask(context.Background(), askFinal, func(cap *capture) {
		f, status, word := eval(cap)
		if !f {
			return
		}
		final = true
		o.c.mu.Lock()
		r := &o.r.rec
		r.Status, r.Error = status, word
		r.Effect = o.effect()
		o.r.final = true
		o.c.mu.Unlock()
	})
	if !final {
		return false
	}
	o.pub = pub
	if s := o.c.seams; s != nil && s.afterFinal != nil {
		s.afterFinal(o.record().Kind)
	}
	o.c.mu.Lock()
	o.c.unfinished = slices.DeleteFunc(o.c.unfinished, func(u *operation) bool { return u == o })
	o.c.mu.Unlock()
	close(o.done)
	return true
}

// finishAs ends o with status and word, whatever the capture says.
func (o *operation) finishAs(status, word string) {
	o.finish(func(*capture) (string, string) { return status, word })
}

// finishErr ends o on a step's refusal err, cancelled where its steps were
// ended, the step's status otherwise.
func (o *operation) finishErr(err error) {
	if errors.Is(err, errStepEnded) {
		o.finishAs(admin.PlayerOpCancelled, "")
		return
	}
	status := statusOfErr(err)
	word := ""
	switch status {
	case admin.PlayerOpRefused:
		var s *engine.Snapshot
		if eng, _ := o.c.engine(); eng != nil {
			s = eng.Snapshot()
		}
		word = wordOf(err, s)
	}
	o.finishAs(status, word)
}

// run starts o's body on a goroutine the control joins, once each of prevs,
// the older operations not ended at its admission, has ended (a Stop has none
// to wait for: the engine takes it after an older one's step in flight), and
// waits for o's end, or for the request's end or the handler's wait,
// answering the record as it stands then. The request's end ends o's steps
// not yet sent; neither ends its step in flight.
func (c *consoleControl) run(ctx context.Context, o *operation, prevs []*operation, body func(o *operation)) admin.PlayerAnswer {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				logPlayerPanic("a console control operation", r)
				select {
				case <-o.done:
				default:
					o.finishAs(admin.PlayerOpUnknown, "")
				}
			}
		}()
		for _, p := range prevs {
			if s := c.seams; s != nil && s.awaitPrev != nil {
				s.awaitPrev(o.r.rec.Kind, p.r.rec.Kind)
			}
			select {
			case <-p.done:
			case <-c.quit:
			}
		}
		body(o)
	}()
	t := time.NewTimer(c.handlerWait())
	defer t.Stop()
	select {
	case <-o.done:
	case <-ctx.Done():
		o.endUnsent()
	case <-t.C:
	}
	return c.answer(o)
}

// answer is o's record as it stands and its publication: the one its end
// published, or, while it runs, the latest; a handover's tracks named by
// their library paths.
func (c *consoleControl) answer(o *operation) admin.PlayerAnswer {
	rec := o.record()
	pub := c.pub.latestPub()
	select {
	case <-o.done:
		pub = o.pub
	default:
	}
	a := admin.PlayerAnswer{Record: &rec, Player: pub.PlayerPublication}
	if h := rec.Handover; h != nil {
		a.HandoverPaths = c.handoverPaths(h.Tracks)
	}
	return a
}

// withHandover is a's handover's tracks named by their library paths, for a
// request at its tab's mark answered with the record it names.
func (c *consoleControl) withHandover(a admin.PlayerAnswer) admin.PlayerAnswer {
	if a.Record != nil && a.Record.Handover != nil {
		a.HandoverPaths = c.handoverPaths(a.Record.Handover.Tracks)
	}
	return a
}

// handoverPaths is the library path of each track id, "" for one no row has.
func (c *consoleControl) handoverPaths(ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		ref, err := engine.NewReference(id)
		if err != nil {
			continue
		}
		out[i], _ = c.pathOf(context.Background(), ref)
	}
	return out
}

// Operation is a tab's last record read back, in epoch.
func (c *consoleControl) Operation(epoch, tab string, seq uint64) admin.PlayerReadBack {
	rev := c.pub.latestPub().Revision
	if epoch != c.epoch {
		return admin.PlayerReadBack{Refused: "epoch_changed", Epoch: c.epoch, Revision: rev}
	}
	c.mu.Lock()
	r := c.records[tab]
	var rec admin.PlayerRecord
	found := r != nil && r.rec.Seq == seq
	if found {
		rec = r.rec
	}
	c.mu.Unlock()
	if !found {
		return admin.PlayerReadBack{Refused: "operation_gone", Epoch: c.epoch, Revision: rev}
	}
	rb := admin.PlayerReadBack{Epoch: c.epoch, Revision: rev, Record: &rec}
	if h := rec.Handover; h != nil {
		rb.HandoverPaths = c.handoverPaths(h.Tracks)
	}
	return rb
}

// begin admits a request's operation and, where it is a new one, runs it:
// first the player's running (player_not_running, a record of its own: J3),
// then body.
func (c *consoleControl) begin(ctx context.Context, op admin.PlayerOp, digest [32]byte, kind, command string,
	body func(o *operation, eng *engine.Engine)) admin.PlayerAnswer {
	o, prevs, refusal, ok := c.admit(op, digest, kind, command)
	if !ok {
		return c.withHandover(refusal)
	}
	return c.run(ctx, o, prevs, func(o *operation) {
		eng, why := c.runningEngine()
		if eng == nil {
			o.set(func(r *admin.PlayerRecord) { r.Reason = why })
			o.finishAs(admin.PlayerOpRefused, wordNotRunning)
			return
		}
		body(o, eng)
	})
}

// cond is the condition at carries, which admin's shape check passed.
func cond(at string) (engine.Cond, bool) {
	g, err := engine.ParseGeneration(at)
	if err != nil {
		return engine.Cond{}, false
	}
	return engine.IfGeneration(g), true
}

// Replace is a replace of the engine's queue (§2.4): the checks of every
// track and every limit before anything is changed, then DeleteAll under the
// tab's generation, the start track into the emptied queue, the tracks
// before it from the head, the tracks after it after it, the queue's IDs
// compared after each step; with play, PlayID and the seek; without, the
// release DeleteAll began or found waited for.
func (c *consoleControl) Replace(ctx context.Context, r admin.PlayerReplace) admin.PlayerAnswer {
	return c.begin(ctx, r.Op, r.Digest, opKindReplace, "", func(o *operation, eng *engine.Engine) {
		c.runReplace(o, eng, r)
	})
}

// Append is an append at the end of the engine's queue (§2.4).
func (c *consoleControl) Append(ctx context.Context, r admin.PlayerAppend) admin.PlayerAnswer {
	return c.begin(ctx, r.Op, r.Digest, opKindAppend, "", func(o *operation, eng *engine.Engine) {
		c.runAppend(o, eng, r)
	})
}

// Delete is a delete of one entry under the tab's generation.
func (c *consoleControl) Delete(ctx context.Context, r admin.PlayerDelete) admin.PlayerAnswer {
	return c.begin(ctx, r.Op, r.Digest, opKindDelete, "", func(o *operation, eng *engine.Engine) {
		cnd, ok := cond(r.At)
		if !ok {
			o.finishAs(admin.PlayerOpRefused, wordStale)
			return
		}
		if err := o.step("delete", func(ctx context.Context) error { return eng.Delete(ctx, r.Entry, cnd) }); err != nil {
			o.finishErr(err)
			return
		}
		o.finish(func(cap *capture) (string, string) {
			if cap.snap != nil && !slices.Contains(cap.snap.IDs(), r.Entry) {
				return admin.PlayerOpDone, ""
			}
			return admin.PlayerOpChanged, ""
		})
	})
}

// Transport is one transport command (§2.4, §2.6): every one but pause and
// stop under the tab's generation; done once the engine answered nil and,
// for pause, the capture after it shows the transport paused or stopped.
func (c *consoleControl) Transport(ctx context.Context, r admin.PlayerCommand) admin.PlayerAnswer {
	return c.begin(ctx, r.Op, r.Digest, opKindTransport, r.Command, func(o *operation, eng *engine.Engine) {
		var conds []engine.Cond
		if r.Command != admin.PlayerCmdPause && r.Command != admin.PlayerCmdStop {
			cnd, ok := cond(r.At)
			if !ok {
				o.finishAs(admin.PlayerOpRefused, wordStale)
				return
			}
			conds = []engine.Cond{cnd}
		}
		name, call := transportCall(eng, r, conds)
		if err := o.step(name, call); err != nil {
			o.finishErr(err)
			return
		}
		o.finish(func(cap *capture) (string, string) {
			if r.Command == admin.PlayerCmdPause && cap.snap != nil {
				if st := cap.snap.State(); st != engine.Paused && st != engine.Stopped {
					return admin.PlayerOpChanged, ""
				}
			}
			return admin.PlayerOpDone, ""
		})
	})
}

// transportCall is r's engine command and the step's name.
func transportCall(eng *engine.Engine, r admin.PlayerCommand, conds []engine.Cond) (string, func(context.Context) error) {
	switch r.Command {
	case admin.PlayerCmdPlay:
		return "play", func(ctx context.Context) error { return eng.Play(ctx, conds...) }
	case admin.PlayerCmdPause:
		return "pause", func(ctx context.Context) error { return eng.Pause(ctx) }
	case admin.PlayerCmdStop:
		return "stop", func(ctx context.Context) error { return eng.Stop(ctx) }
	case admin.PlayerCmdNext:
		return "next", func(ctx context.Context) error { return eng.Next(ctx, conds...) }
	case admin.PlayerCmdPrevious:
		return "previous", func(ctx context.Context) error { return eng.Previous(ctx, conds...) }
	case admin.PlayerCmdIndex:
		return "play_index", func(ctx context.Context) error { return eng.PlayIndex(ctx, r.Index, conds...) }
	case admin.PlayerCmdSeek:
		return "seek", func(ctx context.Context) error { return eng.SeekAbsolute(ctx, r.Seconds, conds...) }
	}
	return "repeat", func(ctx context.Context) error { return eng.SetRepeat(ctx, r.On, conds...) }
}

// entryIn is one track a replace or an append sends: its library path, the
// reference and the metadata its entry carries, its index in the list as
// sent, and whether the engine seeks in it (PCM of known length).
type entryIn struct {
	path   string
	ref    engine.Reference
	meta   []byte
	index  int
	seekOK bool
}

// prepared is what an edit's checks made of its list: the entries that pass,
// and what was left out, with its words.
type prepared struct {
	entries []entryIn
	leftOut []admin.PlayerLeftOut
}

// prepare checks every track of tracks, and none of the engine (§6.2): its
// catalog row, the library's membership (library.Member), the way the
// player plays it (admin.BridgeWayOf, the catalog row's bridge field's
// function), its reference (canonical, on the first host an Insert would
// take, within the engine's 128 bytes) and its metadata (the ContentDirectory's
// item, XML 1.0 text within the engine's 64 KiB). A track that fails is left
// out with its word.
func (c *consoleControl) prepare(ctx context.Context, tracks []string) prepared {
	var out prepared
	distinct := []string{}
	seen := map[string]bool{}
	for _, t := range tracks {
		if !seen[t] {
			seen[t] = true
			distinct = append(distinct, t)
		}
	}
	rows := map[string]manifest.CatalogTrackRow{}
	if c.store != nil && len(distinct) > 0 {
		got, err := c.store.CatalogTrackRowsForPaths(ctx, distinct)
		if err != nil {
			playerLog.Warn("player control: the catalog's rows could not be read", "err", err)
		}
		for _, r := range got {
			rows[r.Path] = r
		}
	}
	plays, _ := c.Plays()
	host := c.referenceHost()
	res := c.resolver()
	type verdict struct {
		e    entryIn
		word string
	}
	checked := map[string]verdict{}
	for _, path := range distinct {
		row, ok := rows[path]
		if !ok {
			checked[path] = verdict{word: wordNotInLibrary}
			continue
		}
		if w := admin.BridgeWayOf(path, row.Codec, row.RoutedUDN != "", row.IsDSD, row.SampleRate, plays); w.Way == admin.BridgeWayNone {
			checked[path] = verdict{word: w.Reason}
			continue
		}
		id := dlna.TrackID("", path)
		if res != nil {
			mctx, cancel := context.WithTimeout(ctx, controlLookupBound)
			err := res.Member(mctx, id)
			cancel()
			if err != nil {
				w := string(engine.ReasonOf(err))
				if w == "" {
					w = wordNotInLibrary
				}
				checked[path] = verdict{word: w}
				continue
			}
		}
		e, word := c.entryFor(ctx, path, id, row, host)
		checked[path] = verdict{e: e, word: word}
	}
	for i, t := range tracks {
		v := checked[t]
		if v.word != "" {
			out.leftOut = append(out.leftOut, admin.PlayerLeftOut{Path: t, Word: v.word})
			continue
		}
		e := v.e
		e.index = i
		out.entries = append(out.entries, e)
	}
	return out
}

// refHost is the host a console entry's reference names: a LAN host an
// Insert would take, or none (the loopback literal, with no <res> and no
// albumArtURI in the metadata).
type refHost struct {
	lan  bool
	host string // in a URL's authority form, bracketed for IPv6
	addr netip.Addr
	name string
}

// referenceHost is the first host an OpenHome Insert's confinement takes
// (the hosts /v1/health advertises, held to the inventory and to
// openhome.LANHostRefused, the same rule), a private IPv4 address first, among
// those that keep the URI within the engine's 128 bytes; the loopback literal
// where none does.
func (c *consoleControl) referenceHost() refHost {
	const probeID = "0000000000000000"
	type cand struct {
		u    trackref.URI
		rank int
		host string
	}
	var cands []cand
	if c.hosts.Advertised != nil {
		for _, raw := range c.hosts.Advertised() {
			pu, err := url.Parse(raw)
			if err != nil {
				continue
			}
			h := pu.Hostname()
			hostPort := net.JoinHostPort(h, strconv.Itoa(openhome.DefaultTrackPort))
			u, err := trackref.Parse("http://" + hostPort + "/dlna/file/" + probeID)
			if err != nil || openhome.LANHostRefused(u) || !c.hosts.Names(u) {
				continue
			}
			rank := 2
			if u.Addr.IsValid() {
				rank = 1
				if u.Addr.Is4() && u.Addr.IsPrivate() {
					rank = 0
				}
			}
			cands = append(cands, cand{u: u, rank: rank, host: hostPort})
		}
	}
	slices.SortStableFunc(cands, func(a, b cand) int { return a.rank - b.rank })
	if len(cands) == 0 {
		return refHost{host: net.JoinHostPort("127.0.0.1", strconv.Itoa(openhome.DefaultTrackPort)), addr: netip.MustParseAddr("127.0.0.1")}
	}
	u := cands[0].u
	return refHost{lan: true, host: strings.TrimSuffix(strings.TrimPrefix(u.String(), "http://"), "/dlna/file/"+probeID), addr: u.Addr, name: u.Name}
}

// entryFor is the entry the console writes for the track at path, or the
// word for why it cannot: its reference, the track's canonical URI on host
// (or the loopback literal), and its metadata, the ContentDirectory's item
// for the track from the store's row (dlna.TrackInfo.DIDLItem, the one
// builder Browse uses, inside dlna.WrapDIDLLite), its <res> the reference
// alone (a rendition never stands in for the source) on a LAN host, none on
// the loopback literal, its albumArtURI on the DLNA listener's own endpoint
// while that listener takes connections on the host and serves artwork. A
// character XML 1.0 cannot carry is replaced by U+FFFD in the track's text
// before the item is built, and the metadata is held to what an Insert takes
// (openhome.MetadataTaken) before anything is changed.
func (c *consoleControl) entryFor(ctx context.Context, path, id string, row manifest.CatalogTrackRow, host refHost) (entryIn, string) {
	uri := "http://" + host.host + "/dlna/file/" + id
	if len(uri) > trackref.MaxLength {
		return entryIn{}, wordReferenceLong
	}
	ref, err := engine.NewReference(uri)
	if err != nil {
		return entryIn{}, wordReferenceLong
	}
	var t manifest.Track
	if c.store != nil {
		got, err := c.store.GetTrack(ctx, path)
		if err == nil && got != nil {
			t = *got
		}
	}
	if t.Path == "" {
		t.Path = path
	}
	ti := manifestTrackToDLNATrackInfo(t, "", "")
	ti.Variants = nil
	ti.Title, ti.Artist, ti.AlbumArtist = xmlChars(ti.Title), xmlChars(ti.Artist), xmlChars(ti.AlbumArtist)
	ti.Album, ti.Composer, ti.Genre = xmlChars(ti.Album), xmlChars(ti.Composer), xmlChars(ti.Genre)
	meta := []byte(consoleMetadata(ti, host, c.artworkBase(host)))
	if !openhome.MetadataTaken(meta) {
		if len(meta) > engine.MaxMetadataBytes {
			return entryIn{}, wordMetadataTooBig
		}
		return entryIn{}, wordMetadataNotXML
	}
	dsd := row.IsDSD
	return entryIn{path: path, ref: ref, meta: meta, seekOK: !dsd && row.Duration > 0}, ""
}

// consoleMetadata is a console entry's metadata: the ContentDirectory's item
// for ti inside the DIDL-Lite envelope, on host's track base, the source's
// <res> alone on a LAN host and none on the loopback literal, the albumArtURI
// on artBase ("" for none).
func consoleMetadata(ti dlna.TrackInfo, host refHost, artBase string) string {
	opts := dlna.DIDLItemOpts{TrackBase: "http://" + host.host, ArtworkBase: artBase, Resources: dlna.ResSourceOnly}
	if !host.lan {
		opts.Resources, opts.ArtworkBase = dlna.ResNone, ""
	}
	return dlna.WrapDIDLLite(ti.DIDLItem(opts))
}

// artworkBase is the DLNA listener's own endpoint on host, while it runs,
// serves artwork and takes connections on host; "" otherwise.
func (c *consoleControl) artworkBase(host refHost) string {
	if !host.lan || !host.addr.IsValid() {
		return ""
	}
	h := c.dlna.Load()
	if h == nil || h.a == nil {
		return ""
	}
	base, ok := h.a.ArtworkBase(host.addr)
	if !ok {
		return ""
	}
	return base
}

// xmlChars is s with each character XML 1.0 cannot carry, and each byte that
// is not UTF-8, replaced by U+FFFD (K4's policy: replace), but for the C0
// controls the ContentDirectory's escaper drops, as it drops them from every
// Browse.
func xmlChars(s string) string {
	ok := true
	for _, r := range s {
		if !xmlCharOK(r) {
			ok = false
			break
		}
	}
	if ok {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size <= 1:
			b.WriteRune(utf8.RuneError)
		case xmlCharOK(r):
			b.WriteRune(r)
		default:
			b.WriteRune(utf8.RuneError)
		}
		i += size
	}
	return b.String()
}

// xmlCharOK reports whether r stays as it is: a character of XML 1.0's Char
// production, or a C0 control the escaper drops.
func xmlCharOK(r rune) bool {
	switch {
	case r == utf8.RuneError:
		return true
	case r < 0x20:
		return true
	case r <= 0xD7FF, r >= 0xE000 && r <= 0xFFFD, r >= 0x10000 && r <= 0x10FFFF:
		return true
	}
	return false
}

// window is the part of entries a replace sends (§6.3): all of them where
// they fit the queue's MaxEntries and its MaxTotalMetadataBytes; otherwise
// the start entry, up to controlWindowAhead before it, and after it as far
// as the limits allow, reaching further back where the list ends first, and
// past the metadata's bound giving up entries from its far end after the
// start first, then from its far end before it. It answers lo and hi, the
// window's first and last entries.
func window(entries []entryIn, start int) (lo, hi int) {
	n := len(entries)
	before := min(start, controlWindowAhead)
	after := min(n-1-start, engine.MaxEntries-1-before)
	if rem := engine.MaxEntries - 1 - before - after; rem > 0 {
		before += min(rem, start-before)
	}
	lo, hi = start-before, start+after
	total := 0
	for i := lo; i <= hi; i++ {
		total += len(entries[i].meta)
	}
	for total > engine.MaxTotalMetadataBytes {
		if hi > start {
			total -= len(entries[hi].meta)
			hi--
		} else if lo < start {
			total -= len(entries[lo].meta)
			lo++
		} else {
			break
		}
	}
	return lo, hi
}

// runReplace is a replace's steps.
func (c *consoleControl) runReplace(o *operation, eng *engine.Engine, r admin.PlayerReplace) {
	if r.NotFound {
		o.finishAs(admin.PlayerOpRefused, wordNotFound)
		return
	}
	cnd, ok := cond(r.At)
	if !ok {
		o.finishAs(admin.PlayerOpRefused, wordStale)
		return
	}
	prep := c.prepare(context.Background(), r.Tracks)
	o.set(func(rec *admin.PlayerRecord) { rec.LeftOut = prep.leftOut })
	start := -1
	for i, e := range prep.entries {
		if e.index == r.Start {
			start = i
			break
		}
	}
	if len(prep.entries) == 0 || start < 0 {
		o.finishAs(admin.PlayerOpRefused, wordNotPlayable)
		return
	}
	lo, hi := window(prep.entries, start)
	entries := prep.entries[lo : hi+1]
	k := start - lo
	if lo > 0 || hi < len(prep.entries)-1 {
		o.set(func(rec *admin.PlayerRecord) {
			rec.Window = &admin.PlayerWindow{First: entries[0].index + 1, Last: entries[len(entries)-1].index + 1, Total: len(r.Tracks)}
		})
	}
	n := len(entries)
	of := 1 + n
	if r.Play {
		of++
	}
	o.set(func(rec *admin.PlayerRecord) { rec.Of = of })

	// 1. DeleteAll under the tab's generation. It releases the session the
	// queue held: where the snapshot read before it is at the tab's
	// generation (which moves at every visible change, so it is the state
	// DeleteAll was admitted on) and holds a session, the release is the
	// replace's even where its acknowledgement lands before the snapshot
	// after DeleteAll is read.
	g, _ := engine.ParseGeneration(r.At)
	before := eng.Snapshot()
	held := before.Generation() == g && holdsSession(before.State())
	if err := o.step("delete_all", func(ctx context.Context) error { return eng.DeleteAll(ctx, cnd) }); err != nil {
		o.finishErr(err)
		return
	}
	deleteAnswered := c.now()
	releasing := held || eng.Snapshot().State() == engine.Releasing

	// 2. The start track into the emptied queue, the tracks before it from
	// the head, those after it after it, the queue's IDs compared after each.
	ids := make([]uint32, n)
	var expect []uint32
	insert := func(i int, after uint32) bool {
		var id uint32
		err := o.step("insert", func(ctx context.Context) error {
			var err error
			id, err = eng.InsertAfter(ctx, after, entries[i].ref, entries[i].meta)
			return err
		})
		if err != nil {
			o.finishErr(err)
			return false
		}
		ids[i] = id
		pos := 0
		if after != 0 {
			pos = slices.Index(expect, after) + 1
		}
		expect = slices.Insert(expect, pos, id)
		o.set(func(rec *admin.PlayerRecord) { rec.Entries = slices.Clone(expect) })
		if !slices.Equal(eng.Snapshot().IDs(), expect) {
			o.finishAs(admin.PlayerOpChanged, "")
			return false
		}
		return true
	}
	if !insert(k, 0) {
		return
	}
	prev := uint32(0)
	for i := 0; i < k; i++ {
		if !insert(i, prev) {
			return
		}
		prev = ids[i]
	}
	prev = ids[k]
	for i := k + 1; i < n; i++ {
		if !insert(i, prev) {
			return
		}
		prev = ids[i]
	}

	if !r.Play {
		c.awaitRelease(o, eng, releasing, deleteAnswered, ids[k], expect)
		return
	}
	// 3. PlayID once the queue is the console's alone, which the check after
	// the last insert showed.
	if err := o.step("play_id", func(ctx context.Context) error { return eng.PlayID(ctx, ids[k]) }); err != nil {
		if engine.ReasonOf(err) == engine.ReasonReleaseUnconfirmed {
			o.set(func(rec *admin.PlayerRecord) { rec.Release = releaseUnconfirmed })
		}
		o.finishErr(err)
		return
	}
	// 4. The seek, the one step under a bounded retry, under the generation of
	// a snapshot that shows the start entry selected and playing.
	if r.Position > 0 && !c.seekTo(o, eng, ids[k], entries[k].seekOK, int64(r.Position)) {
		o.finishAs(admin.PlayerOpCancelled, "")
		return
	}
	o.finish(func(cap *capture) (string, string) {
		s := cap.snap
		if s == nil || !slices.Equal(s.IDs(), expect) {
			return admin.PlayerOpChanged, ""
		}
		if sel := s.Selected(); !slices.Contains(expect, sel) || s.State() != engine.Playing {
			return admin.PlayerOpChanged, ""
		}
		return admin.PlayerOpDone, ""
	})
}

// holdsSession reports whether a transport state holds a session, which a
// DeleteAll releases.
func holdsSession(st engine.State) bool {
	return st == engine.Playing || st == engine.Paused || st == engine.Releasing
}

// seekTo seeks the start entry to seconds under the generation of a snapshot
// that shows it selected and playing, retrying a conflict while one still
// shows it so, at most controlSeekTries; otherwise the track plays from its
// start, and the record says why. It reports false where o's unsent steps
// were ended before the seek was sent.
func (c *consoleControl) seekTo(o *operation, eng *engine.Engine, id uint32, seekOK bool, seconds int64) bool {
	if !seekOK {
		o.set(func(rec *admin.PlayerRecord) { rec.Seek = "dsd_or_unknown_length" })
		return true
	}
	for try := 0; try < controlSeekTries; try++ {
		s := eng.Snapshot()
		if s.Selected() != id || s.State() != engine.Playing {
			o.set(func(rec *admin.PlayerRecord) { rec.Seek = "another_entry_plays" })
			return true
		}
		g := s.Generation()
		err := o.step("seek", func(ctx context.Context) error { return eng.SeekAbsolute(ctx, seconds, engine.IfGeneration(g)) })
		switch {
		case err == nil:
			return true
		case errors.Is(err, errStepEnded):
			return false
		case engine.ReasonOf(err) == engine.ReasonConflict:
			continue
		default:
			o.set(func(rec *admin.PlayerRecord) { rec.Seek = lastWord(err) })
			return true
		}
	}
	o.set(func(rec *admin.PlayerRecord) { rec.Seek = "the_state_kept_changing" })
	return true
}

// awaitRelease is a replace without play's end (§2.4, step 5): a release
// under way when DeleteAll answered is waited for, to the first snapshot
// after the last insert whose transport has settled, within ReleaseTimeout
// and releaseGap of DeleteAll's answer; acknowledged, the record's release
// is confirmed, past the engine's deadline unconfirmed (the engine stops the
// transport with the release unconfirmed in one step), and still releasing
// at the bound, which only an owner that stopped applying its own deadline
// leaves, the outcome is not known.
func (c *consoleControl) awaitRelease(o *operation, eng *engine.Engine, releasing bool, from time.Time, start uint32, expect []uint32) {
	release := "none"
	if releasing {
		deadline := from.Add(engine.ReleaseTimeout + c.releaseGap())
		for {
			changed := eng.Changed()
			s := eng.Snapshot()
			if s.State() != engine.Releasing {
				release = releaseConfirmed
				if _, ok := s.ReleaseUnconfirmed(); ok {
					release = releaseUnconfirmed
				}
				break
			}
			wait := deadline.Sub(c.now())
			if wait <= 0 {
				o.set(func(rec *admin.PlayerRecord) { rec.Release = "unknown" })
				o.finishAs(admin.PlayerOpUnknown, "")
				return
			}
			t := time.NewTimer(wait)
			select {
			case <-changed:
			case <-t.C:
			case <-c.quit:
			case <-o.endCh:
			}
			t.Stop()
			if o.endedNow() {
				o.finishAs(admin.PlayerOpCancelled, "")
				return
			}
		}
	}
	o.set(func(rec *admin.PlayerRecord) { rec.Release = release })
	o.finish(func(cap *capture) (string, string) {
		s := cap.snap
		if s == nil || !slices.Equal(s.IDs(), expect) || s.Selected() != start || s.State() != engine.Stopped {
			return admin.PlayerOpChanged, ""
		}
		return admin.PlayerOpDone, ""
	})
}

// runAppend is an append's steps: the base read from a snapshot (one whose
// generation is not the tab's refuses it stale, nothing sent), the room the
// queue leaves (refused whole, queue_full, where it is short), the first
// insert after the base's last entry under the tab's generation, which is
// the check that counts, the rest after it, the queue's IDs compared after
// each.
func (c *consoleControl) runAppend(o *operation, eng *engine.Engine, r admin.PlayerAppend) {
	if r.NotFound {
		o.finishAs(admin.PlayerOpRefused, wordNotFound)
		return
	}
	g, err := engine.ParseGeneration(r.At)
	if err != nil {
		o.finishAs(admin.PlayerOpRefused, wordStale)
		return
	}
	prep := c.prepare(context.Background(), r.Tracks)
	o.set(func(rec *admin.PlayerRecord) { rec.LeftOut = prep.leftOut; rec.Of = len(prep.entries) })
	if len(prep.entries) == 0 {
		o.finishAs(admin.PlayerOpRefused, wordNotPlayable)
		return
	}
	base := eng.Snapshot()
	if base.Generation() != g {
		o.finishAs(admin.PlayerOpRefused, wordStale)
		return
	}
	if s := c.seams; s != nil && s.afterBase != nil {
		s.afterBase()
	}
	baseIDs := base.IDs()
	held := 0
	if got, err := base.ReadList(baseIDs); err == nil {
		for _, e := range got {
			held += len(e.Meta)
		}
	}
	want := 0
	for _, e := range prep.entries {
		want += len(e.meta)
	}
	roomEntries, roomMeta := engine.MaxEntries-len(baseIDs), engine.MaxTotalMetadataBytes-held
	if len(prep.entries) > roomEntries || want > roomMeta {
		o.set(func(rec *admin.PlayerRecord) {
			rec.Room = &admin.PlayerRoom{Entries: max(roomEntries, 0), Metadata: max(roomMeta, 0), Wanted: len(prep.entries)}
		})
		o.finishAs(admin.PlayerOpRefused, wordQueueFull)
		return
	}
	after := uint32(0)
	if len(baseIDs) > 0 {
		after = baseIDs[len(baseIDs)-1]
	}
	expect := slices.Clone(baseIDs)
	for i, e := range prep.entries {
		var conds []engine.Cond
		if i == 0 {
			conds = []engine.Cond{engine.IfGeneration(g)}
		}
		var id uint32
		err := o.step("insert", func(ctx context.Context) error {
			var err error
			id, err = eng.InsertAfter(ctx, after, e.ref, e.meta, conds...)
			return err
		})
		if err != nil {
			o.finishErr(err)
			return
		}
		expect = append(expect, id)
		o.set(func(rec *admin.PlayerRecord) { rec.Entries = slices.Clone(expect[len(baseIDs):]) })
		if !slices.Equal(eng.Snapshot().IDs(), expect) {
			o.finishAs(admin.PlayerOpChanged, "")
			return
		}
		after = id
	}
	o.finish(func(cap *capture) (string, string) {
		if cap.snap == nil || !slices.Equal(cap.snap.IDs(), expect) {
			return admin.PlayerOpChanged, ""
		}
		return admin.PlayerOpDone, ""
	})
}

// SwitchOut is the barrier (§3.2), the one way the browser starts: the
// console's own Pause, sent whatever the bridge reads, and the bridge's
// silence published after it, from the capture that answers (J1).
func (c *consoleControl) SwitchOut(ctx context.Context, r admin.PlayerSwitchOut) admin.PlayerAnswer {
	o, prevs, refusal, ok := c.admit(r.Op, r.Digest, opKindSwitchOut, "")
	if !ok {
		return c.withHandover(refusal)
	}
	return c.run(ctx, o, prevs, func(o *operation) { c.runSwitchOut(o, r.Take) })
}

// runSwitchOut is the barrier's steps.
func (c *consoleControl) runSwitchOut(o *operation, take bool) {
	deadline := c.now().Add(c.barrierBound())
	eng, why := c.runningEngine()
	if eng == nil {
		c.silenceWithoutEngine(o, why, take, deadline)
		return
	}
	s := eng.Snapshot()
	playing := s.State() == engine.Playing || (s.State() == engine.Releasing && s.Pending() == engine.IntentPlay)
	for pauses := 0; ; {
		if err := o.step("pause", func(ctx context.Context) error { return eng.Pause(ctx) }); err != nil {
			switch {
			case errors.Is(err, errStepEnded), engine.ReasonOf(err) == engine.ReasonMaintenance:
				o.finishErr(err)
				return
			case engine.ReasonOf(err) == engine.ReasonClosed:
				o.finishAs(admin.PlayerOpUnknown, "")
				return
			}
		}
		pauses++
		verdict := c.settle(eng, deadline, o.endCh)
		switch verdict {
		case settleEnded:
			o.finishAs(admin.PlayerOpCancelled, "")
			return
		case settleHold:
			o.finishAs(admin.PlayerOpRefused, wordOf(&engine.Error{Reason: engine.ReasonMaintenance}, eng.Snapshot()))
			return
		case settleTone:
			o.finishAs(admin.PlayerOpRefused, wordTone)
			return
		case settleUnconfirmed:
			o.set(func(rec *admin.PlayerRecord) { rec.Release = releaseUnconfirmed })
			o.finishAs(admin.PlayerOpRefused, string(engine.ReasonReleaseUnconfirmed))
			return
		case settleUnsettled:
			o.finishAs(admin.PlayerOpUnknown, "")
			return
		case settleSilent:
			if s := c.seams; s != nil && s.afterSilence != nil {
				s.afterSilence()
			}
			// The silence the barrier answers with is the answering capture's
			// own, never the one it observed (J1): a play another controller
			// started in between makes this capture's not silent, and the
			// barrier pauses again.
			if o.settleOn(func(cap *capture) (bool, string, string) {
				if !silentOf(cap) {
					return false, "", ""
				}
				c.handOver(o, cap, take, playing)
				return true, admin.PlayerOpDone, ""
			}) {
				return
			}
		}
		if o.endedNow() {
			o.finishAs(admin.PlayerOpCancelled, "")
			return
		}
		if pauses >= controlBarrierTry {
			o.finishAs(admin.PlayerOpChanged, "")
			return
		}
	}
}

// handOver sets a switch-out's silence and, with take, its handover, from
// cap, the capture that answers it: the bridge's queue as track ids, the
// selected one's index, its position for PCM (only where the occurrence heard
// is the selected entry's, K2), and whether the bridge was playing when the
// barrier began. It runs under the publisher's capture, before the state.
func (c *consoleControl) handOver(o *operation, cap *capture, take, playing bool) {
	rev := c.pub.nextRevision()
	var h *admin.PlayerHandover
	if take {
		h = handoverOf(cap.snap, playing)
		if h == nil {
			h = handoverOf(c.pub.lastQueueSnap(), false)
		}
	}
	c.mu.Lock()
	o.r.rec.Silence = rev
	if h != nil {
		o.r.rec.Handover = h
	}
	c.mu.Unlock()
}

// handoverOf is s's queue as a handover, nil for no snapshot.
func handoverOf(s *engine.Snapshot, playing bool) *admin.PlayerHandover {
	if s == nil {
		return nil
	}
	h := &admin.PlayerHandover{Tracks: []string{}, Selected: -1, Playing: playing}
	entries, err := s.ReadList(s.IDs())
	if err != nil {
		return h
	}
	sel := s.Selected()
	for i, e := range entries {
		id, err := trackref.ParseStored(e.Ref.String())
		if err != nil {
			id = ""
		}
		h.Tracks = append(h.Tracks, id)
		if e.ID == sel {
			h.Selected = i
		}
	}
	if t, ok := s.Track(); ok && t.Entry == sel && sel != 0 && seekableTrack(t) {
		if pos := s.Position(); pos.Occurrence == t.Occurrence {
			h.Position = float64(int64(pos.Seconds()))
		}
	}
	return h
}

// silenceWithoutEngine is the barrier where no engine runs (§3.1): it sends
// nothing, and a player that never ran one is silent; a closed engine is
// silent only once serve's player has said no session holds the device,
// which it waits for within the bound, refusing release_unconfirmed past it.
func (c *consoleControl) silenceWithoutEngine(o *operation, why string, take bool, deadline time.Time) {
	for why == reasonClosed {
		if f, ok := c.p.closedEngine(); ok {
			if !f.Silent() {
				o.set(func(rec *admin.PlayerRecord) { rec.Release = releaseUnconfirmed })
				o.finishAs(admin.PlayerOpRefused, string(engine.ReasonReleaseUnconfirmed))
				return
			}
			break
		}
		wait := deadline.Sub(c.now())
		if wait <= 0 {
			o.set(func(rec *admin.PlayerRecord) { rec.Release = releaseUnconfirmed })
			o.finishAs(admin.PlayerOpRefused, string(engine.ReasonReleaseUnconfirmed))
			return
		}
		t := time.NewTimer(wait)
		select {
		case <-c.p.closedKnown:
		case <-t.C:
		case <-c.quit:
		}
		t.Stop()
	}
	o.finish(func(cap *capture) (string, string) {
		if !silentOf(cap) {
			return admin.PlayerOpRefused, string(engine.ReasonReleaseUnconfirmed)
		}
		c.handOver(o, cap, take, false)
		return admin.PlayerOpDone, ""
	})
}

// The barrier's verdicts on the engine after its Pause.
type settleVerdict int

const (
	settleSilent settleVerdict = iota
	settlePlaying
	settleHold
	settleTone
	settleUnconfirmed
	settleUnsettled
	settleEnded
)

// settle waits, within deadline, for the engine to settle after the
// barrier's Pause (not starting, not releasing) and judges it: silent,
// playing, a hold, the tone in the device slot, a release that stays
// unconfirmed to the bound (its acknowledgement may never come), or still
// unsettled at the bound; or ended, once ended closes (a newer operation, the
// request's end).
func (c *consoleControl) settle(eng *engine.Engine, deadline time.Time, ended <-chan struct{}) settleVerdict {
	for {
		changed := eng.Changed()
		s := eng.Snapshot()
		tone := false
		if tb := tonesOf(c.backend()); tb != nil {
			tone = tb.ToneStatus().State != playback.ToneIdle
		}
		_, unconfirmed := s.ReleaseUnconfirmed()
		_, held := s.Maintenance()
		st := s.State()
		settled := st != engine.Releasing && !s.Starting()
		switch {
		case held:
			return settleHold
		case tone && settled:
			return settleTone
		case settled && !unconfirmed && (st == engine.Stopped || st == engine.Paused):
			return settleSilent
		case settled && st == engine.Playing:
			return settlePlaying
		}
		wait := deadline.Sub(c.now())
		if wait <= 0 {
			if unconfirmed {
				return settleUnconfirmed
			}
			return settleUnsettled
		}
		t := time.NewTimer(min(wait, controlSettlePoll*50))
		select {
		case <-changed:
		case <-t.C:
		case <-c.quit:
			t.Stop()
			return settleUnsettled
		case <-ended:
			t.Stop()
			return settleEnded
		}
		t.Stop()
	}
}

// goosOrHost is the host's GOOS.
func goosOrHost() string { return runtime.GOOS }
