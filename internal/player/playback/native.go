package playback

import (
	"context"
	"errors"
	"io"
	"slices"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/decode"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// productionNativeAdmission refuses before locating or opening hardware. The
// only accepting implementations in this phase are in _test.go / the overlay.
func productionNativeAdmission(format.NativeDSD) (engine.NativePlan, error) {
	return engine.NativePlan{}, &engine.Unplayable{Reason: ReasonDSD, Detail: "native DSD requires an owner-approved plan; production admission is disabled"}
}

func (b *Backend) openNativeContent(ctx context.Context, o *occ, d decode.DSDDecoder) {
	o.dsd = d
	o.nativeFormat = d.Format()
	admit := b.nativeAdmission
	if admit == nil {
		admit = productionNativeAdmission
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
	plan.Edges = slices.Clone(plan.Edges)
	o.plan = &plan
	o.transport = nativeTransport(plan)
	planes := [][]byte{make([]byte, chunkFrames), make([]byte, chunkFrames)}
	n, err := d.ReadDSD(ctx, planes)
	o.nativeFirst = [][]byte{planes[0][:n], planes[1][:n]}
	o.firstErr = err
	if n == 0 {
		o.refused = &engine.Unplayable{Reason: ReasonEmpty, Detail: "DSD decoder gave no bits"}
	}
}
func nativeTransport(p engine.NativePlan) alsapcm.Transport {
	f := format.ALSAFormat(p.Tuple.Format)
	w := format.DSDWordBytes(f)
	if w == 0 {
		return alsapcm.Transport{}
	}
	return alsapcm.Transport{Kind: format.KindNativeDSD, RateHz: p.Tuple.BitRate / (8 * w), Channels: 2, Format: f, ValidBits: 1, LogicalRate: p.Tuple.BitRate, PackingVersion: 1}
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
type nativeStream struct{ *alsapcm.DSDSink }

func (n *nativeStream) Write(context.Context, []byte) (int, error) {
	return 0, errors.New("PCM write to native transport")
}
func (n *nativeStream) Pause() error                    { return n.DSDSink.Pause(context.Background()) }
func (n *nativeStream) Resume() error                   { return n.DSDSink.ActivatePaused(context.Background()) }
func (n *nativeStream) Drain(ctx context.Context) error { return n.Finish(ctx) }
func (n *nativeStream) Drop() error                     { return n.Release(context.Background(), alsapcm.Immediate) }
func (n *nativeStream) Close() error {
	n.BeginRelease(alsapcm.Immediate)
	<-n.Done()
	return n.Result()
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
	u, err := s.b.cfg.Locator.Locate(ctx)
	if err != nil {
		return nil, err
	}
	out := &output{s: s, transport: o.transport, unit: u, policyID: o.plan.PolicyID}
	pause := alsapcm.PauseKernel
	if o.plan.Pause == "silence" {
		pause = alsapcm.PauseZeros
	}
	open := s.b.nativeOpen
	if open == nil {
		open = alsapcm.OpenDSDSink
	}
	// Native lifetime survives the producer cancellation during TrackChange.
	life, cancel := context.WithCancel(context.Background())
	out.nativeCancel = cancel
	sink, err := open(life, alsapcm.DSDConfig{Endpoint: u.Endpoint, Format: format.ALSAFormat(o.plan.Tuple.Format), BitRate: o.plan.Tuple.BitRate, Pause: pause, Tail: o.plan.Tail, Prefill: o.plan.Prefill, Check: out.admit, Locate: out.locate})
	if err != nil {
		cancel()
		return nil, err
	}
	out.native = sink
	out.sink = &nativeStream{sink}
	s.mu.Lock()
	s.outs = append(s.outs, out)
	releasing := s.releasing
	s.mu.Unlock()
	if releasing || ctx.Err() != nil {
		sink.BeginRelease(alsapcm.Immediate)
	}
	s.b.notify(Note{Session: s.id, Kind: NoteOpened, Params: sink.Params(), Transport: out.transport, Unit: u.Label})
	return out, nil
}
func (s *session) activateOutput(ctx context.Context, o *occ, out *output) error {
	if ev, ok := s.ev.(engine.CurrentOccurrenceEvents); ok {
		ev.CurrentOccurrence(s.id, o.id, o.plan)
	}
	if out.native == nil {
		return nil
	}
	s.mu.Lock()
	fenced := s.releasing || s.nativeRevoked || o.dropped || s.cur != out
	paused := s.paused
	s.mu.Unlock()
	if fenced {
		out.native.Revoke()
		return errors.New("native activation fenced")
	}
	if paused {
		return out.native.Pause(ctx)
	}
	return out.native.Activate(ctx, 1)
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
		if o.dropped || s.releasing {
			s.mu.Unlock()
			return false
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
	s := b.session(r.Token.Session)
	s.mu.Lock()
	s.nativeEscalated = true
	for _, o := range s.outs {
		if o.native != nil {
			o.native.BeginRelease(alsapcm.Immediate)
		}
	}
	s.mu.Unlock()
}

// Revoke applies a per-session native permission revocation; policy producers
// are supplied by the following phase. Revocation never starts a successor.
func (b *Backend) Revoke(r engine.RevokeInput) {
	s := b.session(r.Session)
	s.mu.Lock()
	s.nativeRevoked = true
	for _, o := range s.outs {
		if o.native != nil {
			o.native.Revoke()
		}
	}
	if s.fault == nil {
		s.fault = &engine.Fault{Reason: engine.Reason("native_revoked"), Detail: r.Reason}
	}
	s.mu.Unlock()
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
	s.mu.Lock()
	if s.nativeRelease == nil {
		s.mu.Unlock()
		return
	}
	token := s.nativeRelease.Token
	if s.cur != nil && s.cur.native != nil {
		token.Cutoff = s.cur.native.Position().Written
	}
	ev := s.ev
	if to, ok := ev.(engine.NativeReleaseEvents); ok {
		s.ev = retiringEvents{Events: ev, to: to, token: token}
	}
	s.finalReport = true
	if s.releaseErr != nil && s.fault == nil {
		s.fault = &engine.Fault{Reason: faultReason(s.releaseErr), Detail: s.releaseErr.Error()}
	}
	s.mu.Unlock()
	if to, ok := ev.(engine.NativeReleaseEvents); ok {
		to.Retiring(token, engine.NativeEvent{Kind: "cutoff"})
	}
	s.reportStep()
	s.mu.Lock()
	s.ev = ev
	s.finalReport = false
	s.mu.Unlock()
}
