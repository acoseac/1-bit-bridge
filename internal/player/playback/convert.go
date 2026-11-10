package playback

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync/atomic"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/decode"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdconv"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/policy"
)

// The player's DSD converted to PCM (B728 B11; the design's sections 4.1,
// 4.3, 4.7 and 4.8). Where the effective set converts a DSD rate, the
// occurrence plays down the PCM path: its decoder is internal/player/dsdconv's
// converter, which owns the DSD source; its transport is the plan's rate and
// valid bits in the target's packing (policy.Target.Packing), never the
// packing a PCM track took (the cache formatsFor offers first) nor the one a
// predecessor's sink has; and every claim of it (Details, the observation)
// is its plan's. Its policy ownership (admitted, policyPlan) is a DSD
// occurrence's, apart from its transport (plan, native and DoP alone), so
// readmission, the check after open, a released pause's reopen, every
// activation, reconciliation and revocation judge it as they judge native
// and DoP.
//
// A join between two converted occurrences is classified by
// dsdconv.Continuous alone. A continuous successor shares the predecessor's
// converter chain (Continue): the predecessor's last H frames, its join
// window, wait at its window point (dsdconv.ErrWindow) until the successor
// settles, and are then computed with the successor's first bits. Anything
// else ends the window zero-padded (End): another DSD rate or plan onto the
// same PCM transport, or PCM of that transport, follows in the same sink,
// abutting, with no drain and no reopen; another transport reopens, an
// honest gap. The window's frames are the predecessor's in every count, so
// the boundary is the successor's first frame. A cut of a successor whose
// bits are in its predecessor's window cuts at the window's first frame and
// submits the window again, computed from the converter's snapshot
// (windowCutLocked), or answers that it cannot: the producer alone calls a
// converter, so that cut is always the producer's.

// windowFloor is the sink's lookahead at which a converted occurrence's join
// window waits no more for its successor to settle: it is finished as an end,
// zero-padded, and a successor that comes later abuts (the design's section
// 4.7).
const windowFloor = 100 * time.Millisecond

// conversion is a converted occurrence's own (B728 B11), beside its
// occurrence's: its converter and plan, and its join window's bookkeeping,
// under the session's lock but for the converter's methods, which the
// producer alone calls, and clipped, which the observation reads.
type conversion struct {
	// conv is the converter, which is also the occurrence's decoder (dec),
	// and convPlan its plan.
	conv     *dsdconv.Converter
	convPlan dsdconv.Plan
	// holding: the converter is at its window point (dsdconv.ErrWindow),
	// every frame of it before its window in its sink, the window waiting for
	// what follows. window is those frames, its window's first frame in its
	// sink start+window; -1 before it held.
	holding bool
	window  int64
	// cont is the successor whose first bits went into this occurrence's
	// window (Continue), until that successor is heard, and contPred, in a
	// successor, that predecessor: a cut of cont cuts through the window. A
	// converted occurrence with a cont is kept open past its end, its
	// converter's snapshot with it, until cont is heard, cut or the session
	// ends.
	cont, contPred *occ
	// epoch counts the occurrence's returns to its window point (Rewind):
	// frames read under an earlier epoch are not written.
	epoch uint64
	// clipped is the converter's count of samples saturated at full scale, as
	// the producer last read it, and clipSaid that it was said.
	clipped  atomic.Int64
	clipSaid bool
}

// converts reports whether o was admitted to be converted to PCM.
func (o *occ) converts() bool {
	return o.admitted != nil && o.policyPlan.Outcome == policy.OutcomeConverted
}

// convPlanOf is a conversion target's plan, as the converter takes it.
func convPlanOf(t policy.Target) dsdconv.Plan {
	return dsdconv.Plan{RateHz: t.RateHz, ValidBits: t.ValidBits}
}

// convertedTransport is the transport a conversion target plays as: its
// rate and valid bits in its packing.
func convertedTransport(t policy.Target) alsapcm.Transport {
	return transportFor(t.RateHz, t.ValidBits, t.Packing)
}

// openConversion opens DSD content the effective set converts (B728 B11):
// the conversion's chain at the content's rate designed, which the process
// keeps, so the converter made where the open ends (makeConverterLocked)
// only allocates; and the first DSD chunk read, as a native open reads it,
// to prove the content gives bits, which the converter takes as its first
// bytes.
func (b *Backend) openConversion(ctx context.Context, o *occ, d decode.DSDDecoder) {
	plan := convPlanOf(o.policyPlan.Target)
	if _, err := plan.Window(o.nativeFormat.BitRateHz); err != nil {
		var re *dsdconv.RateError
		if errors.As(err, &re) {
			o.refused = &engine.Unplayable{Reason: ReasonRate, Detail: "DSD64, DSD128 and DSD256 only: the converter takes no other DSD rate"}
		} else {
			o.refused = &engine.Unplayable{Reason: ReasonTransport, Detail: "the effective DSD set's conversion target is no plan the converter makes"}
		}
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

// makeConverterLocked makes a converted occurrence's converter in the step
// that ends its open, after its readmission under the set published now
// (readmitDSDLocked), so the plan it converts by is the one it is admitted
// under: over its DSD source, which the converter owns from here on and
// closes once, with the first chunk its open read as the converter's first
// bytes (copied), holding its window for what follows (Options.Hold). Its
// format is the plan's PCM (the plan's rate and valid bits, floor(Frames/D)
// frames), and its transport the target's packing. s.mu is held.
func (s *session) makeConverterLocked(o *occ) {
	if o.refused != nil || o.conv != nil || !o.converts() {
		return
	}
	t := o.policyPlan.Target
	if !slices.Contains(packings[t.ValidBits], t.Packing) {
		o.refused = &engine.Unplayable{Reason: ReasonTransport, Detail: fmt.Sprintf("the conversion target's packing, %s, does not carry %d-bit PCM", t.Packing, t.ValidBits)}
		return
	}
	var src decode.DSDDecoder = o.dsd
	if err := o.firstErr; err != nil && !errors.Is(err, io.EOF) {
		src = &afterFirst{DSDDecoder: src, err: err}
	}
	if sm := s.b.cfg.Seams; sm != nil && sm.DSDSource != nil {
		src = sm.DSDSource(o.ref, src)
	}
	conv, err := dsdconv.New(src, convPlanOf(t), dsdconv.Options{Hold: true, First: o.nativeFirst})
	if err != nil {
		o.refused = &engine.Unplayable{Reason: engine.ReasonRefusedContent, Detail: "the DSD cannot be converted: " + err.Error()}
		return
	}
	// The converter owns the source now, and holds the first chunk's copy.
	o.dsd, o.nativeFirst, o.firstErr = nil, nil, nil
	o.conv, o.convPlan, o.dec, o.pcm = conv, conv.Plan(), conv, conv.Format()
	o.window = -1
	o.transport = convertedTransport(t)
	if o.pcm.Frames <= 0 {
		o.refused = &engine.Unplayable{Reason: ReasonEmpty, Detail: "the DSD is shorter than one frame of the converted PCM"}
	}
}

// afterFirst is a DSD source whose first chunk, read at the open, came with
// an error: the converter reads that chunk's copy first and takes the error
// at its first read of the source, as the decoder gave it after those bytes.
type afterFirst struct {
	decode.DSDDecoder
	err  error
	sent bool
}

func (a *afterFirst) ReadDSD(ctx context.Context, dst [][]byte) (int, error) {
	if !a.sent {
		a.sent = true
		return 0, a.err
	}
	return a.DSDDecoder.ReadDSD(ctx, dst)
}

// holdLocked is the producer's next step for a, a converted occurrence at
// its window point (the design's section 4.7). Its window waits while what
// follows has not settled: no successor named and no end, or one still
// opening, being handed off again (stale), admitted under a set a refresh
// has replaced, which that refresh's reconciliation keeps or hands off, or at
// a rate a choice's write pending plays otherwise (B728 B9), until it settles. A
// settled successor converted at a's DSD rate under a's plan, which
// dsdconv.Continuous takes and which follows on a's transport, continues
// a's chain (Continue): the window is computed with its first bits. The end
// named after a, any other successor, or the sink's lookahead at its floor
// while nothing has settled, ends the window zero-padded (End). s.mu is held.
//
// It reads the choice's write pending first and the set published after it,
// once, for every check (B728 B9 round 18). A write registers its pending
// set, publishes, has every session reconciled and only then lets its
// pending go (chooseDSD), and its publish takes no session's lock: a
// decision that read the set published before the publish and asked about
// the pending write after it found the write's set the one published and
// nothing pending, whether the write was still registered or had let its
// pending go, and continued a's chain into the bits of an f still admitted
// under the set the write replaced (Codex's review of round 17). Read in this
// order, a publish before the second read leaves f admitted under a set it
// replaced, whose reconciliation's verdict is waited for, the write pending
// still or let go without reconciling (a panic between its publish and its
// reconciliation, whose set the next refresh reconciles); a publish after it
// finds a write registered at the first read waited for; and a write
// registered after the first read publishes after the decision, which the
// set published before it decides, as one made just before any publish is:
// that publish's reconciliation judges what the decision did.
func (s *session) holdLocked(a *occ) step {
	f := s.followingLocked(a)
	p := s.b.pending.Load()
	s.b.deciding(a.id)
	cur := s.b.dsd.Load()
	settled := f != nil && f.opened() && !f.stale && (f.admitted == nil || f.admitted == cur)
	var pend *pendingChoice
	if settled && p != nil && p.changes(cur, f) {
		// A choice's write pending that plays f's rate otherwise (B728 B9) is
		// waited for as an unsettled successor is, so f's first bits enter no
		// window the write's publish would cut through.
		pend, settled = p, false
	}
	switch {
	case f == nil && s.endAfter == a.id:
	case !settled:
		if !s.floorLocked(a) {
			if pend != nil {
				return step{kind: stepWait, wait: pend.done, awaits: f}
			}
			if f != nil && unjudgedAt(f, cur) {
				// Admitted under a set a publish has replaced: the window waits
				// for that publish's reconciliation to judge f (B728 B9 round 17).
				return step{kind: stepWait, wait: s.judged, verdict: f}
			}
			return step{kind: stepWait}
		}
	case f.conv != nil && a.out != nil && a.out.transport.Join(f.transport).Gapless &&
		dsdconv.Continuous(a.conv.Source(), a.convPlan, f.conv.Source(), f.convPlan):
		if err := a.conv.Continue(f.conv); err == nil {
			a.holding, a.cont, f.contPred = false, f, a
			return step{kind: stepWrite, o: a, decided: true}
		}
	}
	if err := a.conv.End(); err != nil {
		if s.fault == nil {
			s.fault = &engine.Fault{Reason: ReasonFailed, Detail: "player backend: a converted occurrence's join window did not end: " + err.Error()}
		}
		wake(s.wakeR)
		return step{kind: stepStop}
	}
	a.holding = false
	return step{kind: stepWrite, o: a, decided: true}
}

// floorLocked reports whether the sink a plays in has at most windowFloor of
// lookahead left. s.mu is held.
func (s *session) floorLocked(a *occ) bool { return outFloorLocked(a.out) }

// outFloorLocked reports whether out has at most windowFloor of lookahead
// left: what was handed to it and is not played, in its own positions'
// unit (a native sink's count logical bits). A sink that is gone has none.
// s.mu is held.
func outFloorLocked(out *output) bool {
	if out == nil || out.closed || out.drained {
		return true
	}
	p := out.sink.Position()
	return p.Submitted-p.Played <= int64(out.logicalRate())*int64(windowFloor)/int64(time.Second)
}

// holdingLocked reports whether the producer holds a converted occurrence's
// window: the reporter wakes it at each of its steps, so the lookahead's
// floor is read as the sink plays. s.mu is held.
func (s *session) holdingLocked() bool {
	w := s.writing
	return w != nil && w.conv != nil && w.holding && !w.dropped
}

// inWindowLocked reports whether o's first bits went into its
// predecessor's join window, which a cut of o must take back. s.mu is held.
func (o *occ) inWindowLocked() bool {
	a := o.contPred
	return a != nil && a.cont == o
}

// windowCutLocked cuts o, whose first bits went into a's join window (a.cont
// is o), and everything after it, on the producer's goroutine (serveLocked),
// the one that calls a's converter. Before a's window reached the sink, a goes
// back to its window point (Rewind) and nothing reaches the sink. After, the
// sink's marker goes to the window's first frame (o's first frame less H):
// the window and everything after it are dropped, and a goes back to its
// window point and is the producer's again, so its window is submitted once
// more, computed for what follows now, then the replacement. The window's
// frames stay a's in every count. False, nothing dropped, when a frame from
// there on is the kernel's or in the write under way (the sink's answer), or
// when the window can no longer be computed or submitted again: a let go of,
// its sink drained, closed, draining or not current. The engine then stops
// playback when o is heard, as for any late cut. s.mu is held.
func (s *session) windowCutLocked(a, o *occ) (bool, error) {
	if a.closed || a.dropped || a.conv == nil {
		return false, nil
	}
	if a.written <= a.window {
		if s.writing != a {
			return false, nil
		}
		if err := s.rewindLocked(a); err != nil {
			return false, err
		}
		s.dropLocked(o)
		return true, nil
	}
	out := a.out
	if out == nil || out != s.cur || out.drained || out.closed || out.sink.Position().State == alsapcm.SinkDraining {
		return false, nil
	}
	at := a.start + a.window
	ok, err := out.sink.Cut(at)
	if err != nil || !ok {
		return false, err
	}
	out.submitted = at
	a.written, a.eof = a.window, false
	if err := s.rewindLocked(a); err != nil {
		// The window is out of the sink and cannot be computed again: the
		// session stops, never playing on without it.
		if s.fault == nil {
			s.fault = &engine.Fault{Reason: ReasonFailed, Detail: "player backend: a converted occurrence's join window could not be computed again: " + err.Error()}
		}
		wake(s.wakeR)
		return false, err
	}
	s.writing = a
	s.dropLocked(o)
	return true, nil
}

// rewindLocked takes a back to its window point (dsdconv's Rewind, from the
// snapshot its window point took): it holds again, the successor named
// before follows nothing (ErrDetached), and frames read under its epoch are
// not written. s.mu is held, on the producer's goroutine.
func (s *session) rewindLocked(a *occ) error {
	if err := a.conv.Rewind(); err != nil {
		return err
	}
	a.epoch++
	if c := a.cont; c != nil {
		c.contPred = nil
	}
	a.holding, a.cont = true, nil
	wake(s.wakeP)
	return nil
}

// saidClipping says, once for an occurrence, that its conversion saturated
// samples at full scale (the design's section 4.4): the count is the
// occurrence's, in the observation too.
func (s *session) saidClipping(o *occ) {
	n := o.clipped.Load()
	s.mu.Lock()
	say := n > 0 && !o.clipSaid
	o.clipSaid = o.clipSaid || say
	s.mu.Unlock()
	if say {
		s.b.log.Info("player: a DSD occurrence converted to PCM saturated samples at full scale", "occurrence", uint64(o.id), "samples", n, "plan", o.convPlan.String(), "fromBitRate", o.nativeFormat.BitRateHz)
	}
}
