package playback

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/decode"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// noNativeAdmission refuses before locating or opening hardware: the
// harness's admission with no approval that reads, or one revoked.
// Production admits by the effective DSD policy (Config.DSD, B728 B7), and
// the other accepting implementations are in _test.go and the overlay.
func noNativeAdmission(format.NativeDSD) (engine.NativePlan, error) {
	return engine.NativePlan{}, &engine.Unplayable{Reason: ReasonDSD, Detail: "native DSD here needs an owner-approved plan, and this backend holds none"}
}

func (b *Backend) openNativeContent(ctx context.Context, o *occ, d decode.DSDDecoder) {
	o.dsd = d
	o.nativeFormat = d.Format()
	if !o.reservation.PromoteNative() {
		o.refused = &engine.Unplayable{Reason: engine.Reason("occurrence_budget_exceeded"), Detail: "native occurrence bound"}
		return
	}
	admit := b.nativeAdmission
	switch {
	case admit != nil:
	case b.cfg.DSD != nil:
		// Production (B728 B7): the effective DSD policy's set, never a record.
		admit = func(f format.NativeDSD) (engine.NativePlan, error) { return b.admitDSD(o, f) }
	case o.permission == nil && o.dsdRefusal != nil:
		u := *o.dsdRefusal
		o.refused = &u
		return
	case b.transitions != nil:
		admit = func(f format.NativeDSD) (engine.NativePlan, error) {
			return o.permission.selectTransition(o.ref, f, b.transitions)
		}
	default:
		admit = o.permission.selectNative
	}
	plan, err := admit(o.nativeFormat)
	if err != nil {
		var u *engine.Unplayable
		if errors.As(err, &u) {
			o.refused = u
		} else {
			o.refused = &engine.Unplayable{Reason: ReasonDSD, Detail: err.Error()}
		}
		return
	}
	if o.converts() {
		// A rate the effective set converts to PCM (B728 B11) has no native
		// or DoP plan, which nativeLogicalRate and nativeTransport would
		// refuse: it opens through the PCM output path, its converter made
		// where its open ends.
		b.openConversion(ctx, o, d)
		return
	}
	plan.Edges = slices.Clone(plan.Edges)
	o.plan = &plan
	// The decoders take DSD512, which neither native DSD nor DoP carries
	// here: a plan for it is refused before anything is located or opened.
	if !nativeLogicalRate(plan.Tuple.BitRate) {
		o.refused = &engine.Unplayable{Reason: ReasonRate, Detail: "DSD64, DSD128 and DSD256 only: no native or DoP transport here carries this DSD rate"}
		return
	}
	o.transport = nativeTransport(plan)
	if o.transport == (alsapcm.Transport{}) {
		o.refused = &engine.Unplayable{Reason: ReasonTransport, Detail: "the plan names no transport the sink opens"}
		return
	}
	planes := [][]byte{make([]byte, chunkFrames), make([]byte, chunkFrames)}
	n, err := d.ReadDSD(ctx, planes)
	o.nativeFirst = [][]byte{planes[0][:n], planes[1][:n]}
	o.firstErr = err
	if n == 0 {
		o.refused = &engine.Unplayable{Reason: ReasonEmpty, Detail: "DSD decoder gave no bits"}
	}
}

// nativeLogicalRate is the DSD the sink opens: DSD64, DSD128 and DSD256.
func nativeLogicalRate(bitRate int) bool {
	return bitRate == 2822400 || bitRate == 5644800 || bitRate == 11289600
}

// nativeTransport is the transport a plan opens, or the zero Transport for a
// plan whose transport, format and rate do not name one the sink opens.
func nativeTransport(p engine.NativePlan) alsapcm.Transport {
	f := format.ALSAFormat(p.Tuple.Format)
	w := format.DSDWordBytes(f)
	kind := format.KindNativeDSD
	if p.Tuple.Transport == "dop" && f == format.S32LE {
		w = 2
		kind = format.KindDoP
	}
	if w == 0 || (p.Tuple.Transport != "" && p.Tuple.Transport != "native" && p.Tuple.Transport != "dop") || (p.Tuple.Transport == "dop" && f != format.S32LE) || !nativeLogicalRate(p.Tuple.BitRate) {
		return alsapcm.Transport{}
	}
	return alsapcm.Transport{Kind: kind, RateHz: p.Tuple.BitRate / (8 * w), Channels: 2, Format: f, ValidBits: 1, LogicalRate: p.Tuple.BitRate, PackingVersion: 1}
}

// streamSink keeps the PCM call contract intact while native data enters through
// WriteDSD. All positions exposed by the native adapter count logical bits.
type streamSink interface {
	Write(context.Context, []byte) (int, error)
	Cut(int64) (bool, error)
	Pause() error
	Resume() error
	Drain(context.Context) error
	Drop() error
	Close() error
	Params() alsapcm.Params
	Position() alsapcm.Position
	Changed() <-chan struct{}
}
type nativeStream struct {
	*alsapcm.DSDSink
	activationMu sync.Mutex
	activated    bool
	life         context.Context
}

func (n *nativeStream) activate(ctx context.Context) error {
	n.activationMu.Lock()
	defer n.activationMu.Unlock()
	if n.activated {
		return nil
	}
	if n.DSDSink.Position().Paused {
		return nil
	}
	err := n.DSDSink.Activate(ctx, 1)
	if err == nil {
		n.activated = true
	}
	return err
}
func (n *nativeStream) resume(ctx context.Context) error {
	n.activationMu.Lock()
	defer n.activationMu.Unlock()
	err := n.DSDSink.ActivatePaused(ctx)
	if err == nil {
		n.activated = true
	}
	return err
}

func (n *nativeStream) Write(context.Context, []byte) (int, error) {
	return 0, errors.New("PCM write to native transport")
}
func (n *nativeStream) Pause() error                    { return n.DSDSink.Pause(n.life) }
func (n *nativeStream) Resume() error                   { return n.resume(n.life) }
func (n *nativeStream) Drain(ctx context.Context) error { return n.Finish(ctx) }
func (n *nativeStream) Drop() error                     { return n.Release(context.Background(), alsapcm.Immediate) }
func (n *nativeStream) Close() error {
	n.BeginRelease(alsapcm.Immediate)
	<-n.Done()
	return n.CleanupResult()
}
func (n *nativeStream) Position() alsapcm.Position {
	p := n.DSDSink.Position()
	state := alsapcm.SinkPlaying
	switch {
	case n.Result() != nil:
		state = alsapcm.SinkEnded
	case p.Released:
		state = alsapcm.SinkReleased
	case p.Paused:
		state = alsapcm.SinkPaused
	}
	select {
	case <-n.Done():
		if n.Result() == nil {
			state = alsapcm.SinkStopped
		}
	default:
	}
	return alsapcm.Position{Submitted: p.Submitted, Written: p.Written, Played: p.Played, State: state, Err: n.Result(), Measured: time.Now()}
}

func (s *session) openNativeOutput(ctx context.Context, o *occ) (*output, error) {
	s.mu.Lock()
	revoked := s.nativeRevoked
	// The set it was admitted under and its plan there, which a refresh's
	// reconciliation repoints under the session's lock (B728 B7).
	admitted, policyPlan := o.admitted, o.policyPlan
	s.mu.Unlock()
	if revoked {
		return nil, errors.New("DSD permission revoked")
	}
	if err := s.b.checkEdge(o.transport, o.plan, o.permission); err != nil {
		return nil, err
	}
	u, err := s.b.cfg.Locator.Locate(ctx)
	if err != nil {
		return nil, err
	}
	out := &output{occurrence: o.id, plan: o.plan, s: s, transport: o.transport, unit: u, policyID: o.plan.PolicyID, permission: o.permission, admitted: admitted}
	pause := alsapcm.PauseKernel
	if o.plan.Pause == "silence" {
		pause = alsapcm.PauseZeros
	}
	open := s.b.nativeOpen
	if open == nil {
		open = s.b.cfg.OpenNative
	}
	if open == nil {
		open = alsapcm.OpenDSDSink
	}
	if s.isReleased() {
		// Released since the open began: nothing is opened (B674 round 3).
		// The native sink's life is its own, not the session's, so its open
		// is fenced here and at its check after open (output.admit).
		s.b.fenced("open")
		return nil, errReleased
	}
	// Native lifetime survives the producer cancellation during TrackChange.
	life, cancel := context.WithCancel(context.Background())
	out.nativeCancel = cancel
	sink, err := open(life, alsapcm.DSDConfig{Kind: o.transport.Kind, Endpoint: u.Endpoint, Format: format.ALSAFormat(o.plan.Tuple.Format), BitRate: o.plan.Tuple.BitRate, Pause: pause, Tail: o.plan.Tail, Prefill: o.plan.Prefill, Check: out.admit, Locate: out.locate})
	if err != nil {
		cancel()
		return nil, err
	}
	// The open committed exactly the plan's format, rate and channels
	// (alsapcm's open refuses any other) and passed the check after open: the
	// tuple's driver_accepted, recorded off the audio path (B728 B7).
	if admitted != nil {
		s.b.noteDriverAccepted(admitted, policyPlan)
	}
	out.native = sink
	out.adapter = &nativeStream{DSDSink: sink, life: s.ctx}
	out.sink = out.adapter
	s.mu.Lock()
	s.outs = append(s.outs, out)
	released := s.releasedLocked()
	s.mu.Unlock()
	if released || ctx.Err() != nil {
		// Owned, so the teardown closes it, and released at once: its life is
		// not the session's, so it begins its own release (B674 round 3).
		if released {
			s.b.fenced("adopt")
		}
		sink.BeginRelease(alsapcm.Immediate)
	}
	s.b.notify(Note{Session: s.id, Occurrence: o.id, output: out, Kind: NoteOpened, Params: sink.Params(), Transport: out.transport, Unit: u.Label})
	return out, nil
}
func (s *session) activateOutput(ctx context.Context, o *occ, out *output) error {
	s.b.activation(o.id)
	if out.native == nil {
		if o.conv != nil {
			// A converted occurrence's activation reads the effective policy
			// again too (B728 B11): one its change did not keep is refused
			// before a frame, never played another way. That it converts is
			// what its open fixed: o.conv, set once in the step that ended the
			// open and never again, since a reconciliation keeps the
			// occurrence (repointing o.admitted, under s.mu), hands it off
			// again as another occurrence or revokes the session, and none of
			// them turns a converted occurrence into another kind. Its set and
			// plan are read in checkDSDActivation alone, under s.mu: read here
			// they raced a reconciliation's Keep (round 2).
			if err := s.checkDSDActivation(o); err != nil {
				return err
			}
		}
		s.b.rememberEdge(out.transport, nil, nil)
		s.installed(o, out, true)
		return nil
	}
	s.mu.Lock()
	fenced := s.releasing || s.nativeRevoked || o.dropped || s.cur != out
	replanned := o.dropped && !o.leading && !s.releasing && !s.nativeRevoked
	paused := s.paused
	s.mu.Unlock()
	if fenced {
		out.native.Revoke()
		if replanned {
			return errReplanned
		}
		return errors.New("native activation fenced")
	}
	if o.permission != nil {
		if err := s.b.checkPermission(ctx, o.permission, nil, edgeKey(o.transport, o.plan)); err != nil {
			return err
		}
	}
	if out.dsdAdmitted() != nil {
		// Every activation reads the effective policy again (B728 B7): an
		// occurrence its change did not keep is refused, never played another
		// way.
		if err := s.checkDSDActivation(o); err != nil {
			out.native.Revoke()
			return err
		}
	}
	if paused {
		if err := out.native.Pause(ctx); err != nil {
			return err
		}
		s.b.rememberEdge(out.transport, o.plan, o.permission)
		s.installed(o, out, true)
		return nil
	}
	err := out.activate(ctx)
	if err == nil {
		s.b.rememberEdge(out.transport, o.plan, o.permission)
		s.installed(o, out, true)
	}
	return err
}
func (s *session) writeNativeChunk(o *occ) bool {
	planes, err := o.nativeFirst, o.firstErr
	o.nativeFirst = nil
	o.firstErr = nil
	if planes == nil {
		planes = [][]byte{make([]byte, chunkFrames), make([]byte, chunkFrames)}
		var n int
		n, err = o.dsd.ReadDSD(s.ctx, planes)
		planes[0] = planes[0][:n]
		planes[1] = planes[1][:n]
	}
	bits := int(min(int64(len(planes[0]))*8, o.nativeFormat.Frames-o.decodedBits))
	o.decodedBits += int64(bits)
	for offset := 0; offset < bits; {
		s.mu.Lock()
		s.serveLocked()
		if s.releasedLocked() {
			// Released, by its Release or the engine's taking it: no more is
			// handed to the native sink (B674 round 3).
			s.mu.Unlock()
			s.b.fenced("write")
			return false
		}
		if s.ctx.Err() != nil {
			s.mu.Unlock()
			return false
		}
		if o.dropped || o.stale || s.writing != o {
			// A stale occurrence is being handed off again (B728 B7): nothing
			// more of it goes to the sink, and the producer waits.
			s.mu.Unlock()
			return true
		}
		if s.wctx == nil || s.wctx.Err() != nil {
			s.wctx, s.wcancel = context.WithCancel(s.ctx)
		}
		ctx := s.wctx
		s.inWrite = true
		s.mu.Unlock()
		n, e := o.out.native.WriteDSD(ctx, uint64(o.id), planes[0], planes[1], offset, bits-offset)
		s.mu.Lock()
		s.inWrite = false
		o.written += int64(n)
		o.out.submitted += int64(n)
		s.mu.Unlock()
		if n > 0 {
			s.signalFirst(nil)
			wake(s.wakeR)
		}
		offset += n
		if e != nil {
			if errors.Is(e, context.Canceled) && s.ctx.Err() == nil {
				continue
			}
			return false
		}
	}
	if err == nil && o.decodedBits < o.nativeFormat.Frames {
		return true
	}
	if err != nil && !errors.Is(err, io.EOF) {
		s.failDecode(o, err)
		return true
	}
	s.mu.Lock()
	o.eof = true
	s.writing = nil
	s.closeOcc(o)
	s.mu.Unlock()
	wake(s.wakeR)
	return true
}
func (o *occ) details() engine.Details {
	if o.plan != nil {
		return engine.Details{Frames: o.nativeFormat.Frames, RateHz: o.nativeFormat.BitRateHz, Bits: 1, Channels: 2, Codec: "DSD", Lossless: true}
	}
	return detailsOf(o.pcm)
}
func (o *output) logicalRate() int {
	if o.native != nil {
		return o.transport.LogicalRate
	}
	return o.transport.RateHz
}

// ReleaseNative establishes the native writer's fence before cancelling its
// producer. Cleanup completion and successful release have separate results.
func (b *Backend) ReleaseNative(ctx context.Context, r engine.ReleaseRequest) error {
	s := b.session(r.Token.Session)
	s.mu.Lock()
	if s.nativeEscalated {
		r.Mode = engine.ReleaseImmediate
	}
	if !s.releasing {
		s.nativeRelease = &r
	}
	s.mu.Unlock()
	return s.release(ctx)
}

// EscalateNative never waits for the releasing caller or a kernel operation.
func (b *Backend) EscalateNative(r engine.ReleaseRequest) {
	s := b.known(r.Token.Session)
	if s == nil {
		return
	}
	s.mu.Lock()
	s.nativeEscalated = true
	for _, o := range s.outs {
		if o.native != nil {
			o.native.BeginRelease(alsapcm.Immediate)
		}
	}
	s.mu.Unlock()
}

// Revoke fences DSD occurrences, including retiring tails, while preserving PCM.
// A late callback never recreates a retired session or starts a successor.
// DSD converted to PCM is the policy's as native and DoP are (B728 B11): a
// prepared converted occurrence is dropped with the native ones, and a PCM
// sink holding a converted occurrence's frames that it has not played is
// dropped at once, as a native sink is released, the session faulting; a PCM
// sink holds no such frames while it plays PCM alone, and plays on.
func (b *Backend) Revoke(r engine.RevokeInput) {
	s := b.known(r.Session)
	if s == nil {
		return
	}
	l := s.hold()
	defer l.release()
	l.lock()
	b.revoking(r.Session)
	// A DSD entry of the session is refused from now on as the successor
	// dropped below is, for this revocation's reason (B853), unless an earlier
	// cause stands.
	s.revokeDSDLocked(engine.Unplayable{Reason: "native_revoked", Detail: r.Reason})
	if s.permission != nil {
		s.permission.revoked.Store(true)
	}
	active := false
	var sinks []*alsapcm.DSDSink
	for _, out := range s.outs {
		if out.native != nil && !out.closed {
			active = true
			sinks = append(sinks, out.native)
		}
	}
	var pcm []*output
	for _, o := range s.chain {
		if !o.opened() || o.dropped || (o.plan == nil && o.conv == nil) {
			continue
		}
		if o.out == nil {
			o.dropped = true
			o.refused = &engine.Unplayable{Reason: "native_revoked", Detail: r.Reason}
			continue
		}
		if out := o.out; o.conv != nil && out.native == nil && !out.closed && !out.drained && !slices.Contains(pcm, out) &&
			!(o.eof && out.position().Played >= o.start+o.written) {
			active = true
			pcm = append(pcm, out)
		}
	}
	if active && s.fault == nil {
		s.fault = &engine.Fault{Reason: "native_revoked", Detail: r.Reason}
	}
	close(s.judged)
	s.judged = make(chan struct{})
	l.unlock()
	for _, sink := range sinks {
		sink.Revoke()
		sink.BeginRelease(alsapcm.Immediate)
	}
	for _, out := range pcm {
		// Dropped on a goroutine of its own: a sink whose writer the kernel
		// holds answers no command, and Revoke never waits for one.
		s.b.spawn(func() { _ = s.call(s.ctx, out.sink.Drop) })
	}
	b.policyMu.Lock()
	if active && b.liveFacts != nil && b.liveFacts.Session == r.Session && (b.liveFacts.LogicalRate > 0 || b.liveFacts.Transport == TransportConverted) && b.liveFacts.State == "open" {
		b.liveFacts.State = "revoked"
		b.liveFacts.ObservedAt = time.Now()
	}
	b.policyMu.Unlock()
	wake(s.wakeP)
	wake(s.wakeR)
}

type retiringEvents struct {
	engine.Events
	to    engine.NativeReleaseEvents
	token engine.ReleaseToken
}

func (v retiringEvents) Details(_ engine.SessionID, o engine.OccurrenceID, d engine.Details) {
	v.to.Retiring(v.token, engine.NativeEvent{Kind: "details", Occurrence: o, Details: d})
}
func (v retiringEvents) Audible(_ engine.SessionID, o engine.OccurrenceID) {
	v.to.Retiring(v.token, engine.NativeEvent{Kind: "audible", Occurrence: o})
}
func (v retiringEvents) Position(_ engine.SessionID, p engine.Position) {
	v.to.Retiring(v.token, engine.NativeEvent{Kind: "position", Position: p})
}
func (v retiringEvents) Fault(_ engine.SessionID, f engine.Fault) {
	v.to.Retiring(v.token, engine.NativeEvent{Kind: "fault", Fault: f})
}

// finalNativeReport runs after the ordinary reporter has joined and after every
// native writer has accounted its accepted prefix and finished cleanup. Its
// callbacks enter the engine mailbox before either acknowledgement path.
func (s *session) finalNativeReport() {
	l := s.hold()
	// s.ev is read under the session's lock, as every write of it is made.
	var ev, reportEv engine.Events
	defer func() {
		// Release the lock before recovery: position callbacks run under it.
		l.release()
		if r := recover(); r != nil {
			s.b.logPanic("the final native report", r)
			s.faulted("the final native report")
			s.finalFault(reportEv)
		}
		s.mu.Lock()
		s.ev = ev
		s.finalReport = false
		s.mu.Unlock()
	}()
	l.lock()
	ev = s.ev
	reportEv = ev
	if s.nativeRelease == nil {
		l.unlock()
		return
	}
	if s.releaseErr != nil && s.fault == nil {
		s.fault = &engine.Fault{Reason: faultReason(s.releaseErr), Detail: s.releaseErr.Error()}
	}
	token := s.nativeRelease.Token
	if s.cur != nil && s.cur.native != nil {
		token.Cutoff = s.cur.native.Position().Written
	}
	var prefixes []engine.NativePrefix
	for _, o := range s.chain {
		if o.dropped || o.out == nil {
			continue
		}
		end := min(o.start+o.written, o.out.position().Written)
		if end > o.start {
			prefixes = append(prefixes, engine.NativePrefix{Occurrence: o.id, Start: o.start, End: end})
		}
	}
	s.finalReport = true
	l.unlock()
	if to, ok := ev.(engine.NativeReleaseEvents); ok {
		to.Retiring(token, engine.NativeEvent{Kind: "cutoff", Prefixes: prefixes})
		// The engine accepts token events only after the cutoff was posted.
		// Before that point recovery must use ordinary events.
		reportEv = retiringEvents{Events: ev, to: to, token: token}
	}
	l.lock()
	s.ev = reportEv
	l.unlock()
	s.reportStep()
}

// finalFault delivers the unreached fault without consulting the observer that
// failed. Both workers have joined. An attempted callback is never retried, and
// even a broken Fault callback cannot prevent reservations and down releasing.
func (s *session) finalFault(ev engine.Events) {
	defer func() {
		if r := recover(); r != nil {
			s.b.logPanic("the final report's report of its fault", r)
		}
	}()
	s.mu.Lock()
	if s.fault == nil || s.faultSent || ev == nil {
		s.mu.Unlock()
		return
	}
	f := *s.fault
	if f.Occurrence == 0 {
		f.Occurrence, f.Frames = s.posOcc, s.posFrames
		if f.Occurrence == 0 && len(s.chain) > 0 {
			f.Occurrence = s.chain[0].id
		}
	}
	s.faultSent = true
	s.mu.Unlock()
	ev.Fault(s.id, f)
}

func (o *output) activate(ctx context.Context) error {
	return o.adapter.activate(ctx)
}
func (o *output) pause(ctx context.Context) error {
	if o.native != nil {
		return o.native.Pause(ctx)
	}
	return o.sink.Pause()
}
func (o *output) resume(ctx context.Context) error {
	if o.native != nil {
		err := o.adapter.resume(ctx)
		if err == nil {
			o.s.b.rememberEdge(o.transport, o.plan, o.permission)
		}
		return err
	}
	return o.sink.Resume()
}
