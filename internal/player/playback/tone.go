package playback

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/testtone"
	"github.com/acoseac/1-bit-bridge/internal/tonebounds"
)

// The console's test tone (B601): a quiet 1 kHz sine through the DAC the
// running player is bound to, at -30 dBFS or quieter, PCM alone. It is the
// third holder of the backend's device slot, beside a session (holder) and
// the silent probe (probing): counted among the backend's goroutines before it
// takes the slot (toneEnter), so serve's stop joins it (Wait); admitted only in
// the one step under the backend's lock that finds the slot free and the
// supervisor's gate open (reserveTone), never waiting; preempted by any Start
// (hold); ended by its request, the gate, serve's stop or its length; and it
// gives the slot back only once its node's close has answered (endTone). A close
// that answered an error, a cleanup that panicked before it answered, and an
// open whose own cleanup is not confirmed fence the device as a release never
// confirmed does (terminalUnconfirmed), until serve restarts.
//
// Lock order: gmu alone (the count), then b.mu alone (the slot): never one
// under the other, as the backend's order has it (publishProbe's).

// ToneYield is the longest a Start waits for the console's tone to give the
// device slot back once it has ended it (hold): past it the Start answers the
// device busy, holding nothing, as beside a probe (ProbeWait).
const ToneYield = time.Second

// ToneStopWait is the longest Tone waits, once the tone's context has ended,
// for its node's close to answer: then it answers stop_unconfirmed, the slot
// still held (the engine's ReleaseTimeout).
const ToneStopWait = 2 * time.Second

// ToneRequest is a tone the console asks for: its peak in dBFS
// (tonebounds.Check) and its length in whole seconds (tonebounds.CheckSeconds).
type ToneRequest struct {
	LevelDBFS float64
	Seconds   int
}

// ToneOutcome is the word a tone was refused or ended with (ToneWords).
type ToneOutcome string

// The words a tone is refused with, before it takes the slot: nothing was
// opened, and the request answers at once.
const (
	// ToneLevelRefused is a level that is not a number from -90 to -30 dBFS,
	// ToneLengthRefused a length outside 1 to 30 seconds: refused before
	// anything is counted or locked.
	ToneLevelRefused  ToneOutcome = "level_refused"
	ToneLengthRefused ToneOutcome = "length_refused"
	// ToneServeStopping is serve's stop (CloseTones): refused, at the count or
	// at the slot, and the end of a tone that played as it came.
	ToneServeStopping ToneOutcome = "serve_stopping"
	// The gate's words (ToneOutput): a tone is refused while the gate is shut
	// with one, and a tone that plays as it shuts ends with it.
	ToneOutputUnknown     ToneOutcome = "output_unknown"
	ToneWithheld          ToneOutcome = "withheld"
	ToneOutputUnavailable ToneOutcome = "output_unavailable"
	ToneOutputChanged     ToneOutcome = "output_changed"
	ToneSnapshotStale     ToneOutcome = "snapshot_stale"
	// ToneReleaseUnconfirmed is a release never confirmed (terminalUnconfirmed):
	// refused, and the end of a tone whose own cleanup did not answer cleanly,
	// which sets it.
	ToneReleaseUnconfirmed ToneOutcome = "release_unconfirmed"
	// The slot's other holders: the silent probe, a choice's write holding the
	// configuration lease, a Start waiting for the slot, a session's record or
	// the device's holder, another tone.
	ToneProbeRunning ToneOutcome = "probe_running"
	ToneChoiceWrite  ToneOutcome = "choice_write"
	ToneStartWaiting ToneOutcome = "start_waiting"
	ToneSessionHolds ToneOutcome = "session_holds"
	TonePlaying      ToneOutcome = "tone_playing"
	// ToneCheckRunning is the DSD check holding the slot (Check, B728 B12).
	ToneCheckRunning ToneOutcome = "check_running"
	// ToneRequestEnded is the request's own end: before the slot, a refusal;
	// after it, the tone's end.
	ToneRequestEnded ToneOutcome = "request_ended"
)

// The words a tone that took the slot ends with, beside the gate's,
// serve_stopping, request_ended and release_unconfirmed.
const (
	// TonePlayed is a tone that played its length and drained.
	TonePlayed ToneOutcome = "played"
	// TonePreempted is a tone a Start ended (hold).
	TonePreempted ToneOutcome = "preempted"
	// ToneOutputGone is a write or a drain that found the device gone.
	ToneOutputGone ToneOutcome = "output_gone"
	// ToneDeviceError is an xrun, a stalled stream or any other device error.
	ToneDeviceError ToneOutcome = "device_error"
	// ToneWrongDevice is a node the unit's check after open refused.
	ToneWrongDevice ToneOutcome = "wrong_device"
	// ToneNotLocated is a binding that resolves to no unit now.
	ToneNotLocated ToneOutcome = "not_located"
	// ToneOpenRefused is an open the DAC refused: busy, denied (LastError),
	// or any other refusal of its own.
	ToneOpenRefused ToneOutcome = "open_refused"
	// ToneNoFormat is none of the tone's eight pairs taken: the DAC takes
	// none of its formats, each refused or committed otherwise.
	ToneNoFormat ToneOutcome = "no_format"
	// ToneFailed is the backend's own failure, a panic whose cleanup closed
	// the node among them.
	ToneFailed ToneOutcome = "failed"
	// ToneStopUnconfirmed is Tone's answer once the tone was ended and its
	// node's close did not answer within ToneStopWait: the slot is still held.
	ToneStopUnconfirmed ToneOutcome = "stop_unconfirmed"
)

// ToneWords is every word a tone is refused or ended with, in a list of the
// caller's own: a closed list, which the console words one by one.
func ToneWords() []ToneOutcome {
	return []ToneOutcome{
		ToneLevelRefused, ToneLengthRefused, ToneServeStopping,
		ToneOutputUnknown, ToneWithheld, ToneOutputUnavailable, ToneOutputChanged, ToneSnapshotStale,
		ToneReleaseUnconfirmed, ToneProbeRunning, ToneChoiceWrite, ToneStartWaiting, ToneSessionHolds, TonePlaying, ToneCheckRunning, ToneRequestEnded,
		TonePlayed, TonePreempted, ToneOutputGone, ToneDeviceError, ToneWrongDevice, ToneNotLocated, ToneOpenRefused, ToneNoFormat, ToneFailed, ToneStopUnconfirmed,
	}
}

// A refused open's cause (ToneResult.LastError), as deviceCause maps a
// Start's: another process holds the DAC, or this process may not open its
// node.
const (
	ToneLastErrorBusy   = "device_busy"
	ToneLastErrorDenied = "permission_denied"
)

// ToneLastErrors is every LastError word.
func ToneLastErrors() []string { return []string{ToneLastErrorBusy, ToneLastErrorDenied} }

// ToneResult is a tone's answer: the word it was refused or ended with;
// whether it took the slot (Admitted: a refusal did not); a refused open's
// cause; what the kernel committed, zero before an open did; and the frames
// written.
type ToneResult struct {
	Outcome   ToneOutcome
	Admitted  bool
	LastError string
	Format    format.ALSAFormat
	RateHz    int
	ValidBits int
	Frames    int64
}

// ToneState is where the console's tone is (ToneStatus).
type ToneState string

const (
	// ToneIdle is no tone in the slot.
	ToneIdle ToneState = "idle"
	// ToneStatePlaying is a tone in the slot, not ended.
	ToneStatePlaying ToneState = "playing"
	// ToneStateStopping is a tone ended whose node's close has not answered.
	ToneStateStopping ToneState = "stopping"
	// ToneStateStopUnconfirmed is a tone ended more than ToneStopWait ago
	// whose close has not answered: the slot is still held.
	ToneStateStopUnconfirmed ToneState = "stop_unconfirmed"
	// ToneStateReleaseUnconfirmed is a tone whose cleanup did not answer
	// cleanly: nothing opens the device until serve restarts.
	ToneStateReleaseUnconfirmed ToneState = "release_unconfirmed"
)

// ToneStates is every ToneState.
func ToneStates() []ToneState {
	return []ToneState{ToneIdle, ToneStatePlaying, ToneStateStopping, ToneStateStopUnconfirmed, ToneStateReleaseUnconfirmed}
}

// ToneStatus is the console's tone now: its state; the tone in the slot (its
// request and what the kernel committed for it, zero before an open did); and
// the last tone's result, cleared when the next is admitted.
type ToneStatus struct {
	State     ToneState
	Request   ToneRequest
	Format    format.ALSAFormat
	RateHz    int
	ValidBits int
	Last      ToneResult
}

// toneHold is a tone admitted to the device slot: its request, its context
// (the caller's, cancelled with its cause by whatever ends it, on this record,
// never a later lookup of the tone that plays), done closed once it has given
// the slot back, and its result.
type toneHold struct {
	req    ToneRequest
	ctx    context.Context
	cancel context.CancelCauseFunc
	done   chan struct{}
	result chan ToneResult
	frames atomic.Int64

	// Under the backend's lock: what the kernel committed for it, and a stop
	// past ToneStopWait (ToneStatus).
	format          format.ALSAFormat
	rate, bits      int
	stopUnconfirmed bool
}

// toneCause is why a tone ended, its context's cause.
type toneCause struct{ why ToneOutcome }

func (c *toneCause) Error() string {
	return "player backend: the console's test tone ended: " + string(c.why)
}

// end ends t with why, unless it has ended already: the first cause stands.
func (t *toneHold) end(why ToneOutcome) { t.cancel(&toneCause{why: why}) }

// ended is the word t ended with: its context's cause, request_ended for the
// caller's own end, and failed for a context that has not ended, which no
// tone's run leaves.
func (t *toneHold) ended() ToneOutcome {
	var c *toneCause
	if errors.As(context.Cause(t.ctx), &c) {
		return c.why
	}
	if t.ctx.Err() != nil {
		return ToneRequestEnded
	}
	return ToneFailed
}

// Tone plays the console's test tone on the bound DAC and answers once it has
// ended, in this order: the request's bounds (level_refused, length_refused),
// nothing counted or locked; its count among the backend's goroutines, which
// serve's stop refuses once it has closed the tones (serve_stopping); the
// device slot, in one step under the backend's lock that refuses by word and
// never waits (reserveTone), the count given back on a refusal; then its run,
// on the goroutine counted: the bound unit located, the first of its eight
// (rate, format) pairs the kernel commits exactly, the check after open, whole
// repeats of the tone for its length, and a drain. Every end is a cause on its
// context; the slot is given back only once its node's close has answered. Once
// its context has ended Tone waits at most ToneStopWait for the result, then
// answers stop_unconfirmed, the slot still held.
func (b *Backend) Tone(ctx context.Context, req ToneRequest) ToneResult {
	if tonebounds.Check(req.LevelDBFS) != nil {
		return ToneResult{Outcome: ToneLevelRefused}
	}
	if tonebounds.CheckSeconds(req.Seconds) != nil {
		return ToneResult{Outcome: ToneLengthRefused}
	}
	b.toneEntering()
	if !b.toneEnter() {
		return ToneResult{Outcome: ToneServeStopping}
	}
	b.toneCounted()
	t, why := b.reserveTone(ctx, req)
	if t == nil {
		b.leave()
		return ToneResult{Outcome: why}
	}
	b.toneAdmitted()
	go b.counted(false, func() { b.runTone(t) })
	select {
	case r := <-t.result:
		return r
	case <-t.ctx.Done():
	}
	expired := make(chan struct{})
	stop := b.cfg.AfterFunc(ToneStopWait, func() {
		b.mu.Lock()
		if b.toning == t {
			t.stopUnconfirmed = true
		}
		b.mu.Unlock()
		close(expired)
	})
	defer stop()
	select {
	case r := <-t.result:
		return r
	case <-expired:
	}
	b.mu.Lock()
	res := ToneResult{Outcome: ToneStopUnconfirmed, Admitted: true, Format: t.format, RateHz: t.rate, ValidBits: t.bits, Frames: t.frames.Load()}
	b.mu.Unlock()
	b.log.Warn("player: the console's test tone was ended and its node's close has not answered: the device stays held until it does", "after", ToneStopWait)
	return res
}

// toneEnter counts a tone among the backend's goroutines (Wait, Running),
// under gmu alone, unless serve's stop has closed the tones (CloseTones).
func (b *Backend) toneEnter() bool {
	b.gmu.Lock()
	defer b.gmu.Unlock()
	if b.tclosed {
		return false
	}
	b.startLocked(false)
	return true
}

// leave gives back the count of a tone, or a DSD check, refused at the slot.
func (b *Backend) leave() {
	b.gmu.Lock()
	defer b.gmu.Unlock()
	if b.live--; b.live == 0 {
		close(b.zero)
	}
}

// reserveTone takes the device slot for a tone, or says why it cannot, in one
// step under the backend's lock, the lock a Start's hold and a probe take the
// slot under, refusing by word and never waiting, in this order: the gate
// shut (its word); a release never confirmed; a probe; a choice's write's
// configuration lease; a Start waiting for the slot; a session's record or
// the device's holder; another tone; the DSD check (B728 B12); and the request
// ended already. The tone's context is the caller's, cancelled by whatever
// ends it. The last tone's result is cleared as this one is admitted.
func (b *Backend) reserveTone(ctx context.Context, req ToneRequest) (*toneHold, ToneOutcome) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case !b.toneOpen:
		return nil, b.toneShut
	case b.terminalUnconfirmed:
		return nil, ToneReleaseUnconfirmed
	case b.probing != nil:
		return nil, ToneProbeRunning
	case b.lease != nil:
		return nil, ToneChoiceWrite
	case b.waiters > 0:
		return nil, ToneStartWaiting
	case len(b.sessions) > 0 || b.holder != nil:
		return nil, ToneSessionHolds
	case b.toning != nil:
		return nil, TonePlaying
	case b.checking != nil:
		return nil, ToneCheckRunning
	case ctx.Err() != nil:
		return nil, ToneRequestEnded
	}
	tctx, cancel := context.WithCancelCause(ctx)
	t := &toneHold{req: req, ctx: tctx, cancel: cancel, done: make(chan struct{}), result: make(chan ToneResult, 1)}
	b.toning, b.toneLast = t, ToneResult{}
	return t, ""
}

// toneRun is what a tone's run opened and wrote: the device; an open whose
// own cleanup is not confirmed (unknown: its release answered an error, or it
// panicked); a refused open's cause; the valid bits of the pair opened; and
// the error a cleanup not confirmed answered, the node's close's or a refused
// open's whose own close failed (none for a panic, whose line is its own).
type toneRun struct {
	dev        *alsapcm.Device
	unknown    bool
	lastError  string
	bits       int
	cleanupErr error
}

// runTone is a tone's run, on the goroutine its count stands for, and its
// cleanup, deferred from the run's start: the node's close, in a function with
// its own recover (closeTone), then the slot given back (endTone), on every way
// out, a panic's included.
func (b *Backend) runTone(t *toneHold) {
	run := &toneRun{}
	defer func() {
		confirmed := b.closeTone(run)
		b.toneClosed(confirmed)
		b.endTone(t, run, confirmed)
	}()
	defer func() {
		if r := recover(); r != nil {
			b.logPanic("the console's test tone", r)
			t.end(ToneFailed)
		}
	}()
	b.playTone(t, run)
	// Every way playTone returns has ended the tone; this is the backstop.
	t.end(ToneFailed)
}

// tonePacking is one of the tone's (format, valid bits) pairs at a rate.
type tonePacking struct {
	f    format.ALSAFormat
	bits int
}

// toneRates and tonePackings are the tone's eight (rate, format) pairs, in the
// order it asks the DAC: at 48 kHz, then 44.1 kHz, the 24-bit row of B470's
// table (packings) and then the 16-bit row's own container. The kernel is
// asked a rate and a format alone (alsapcm.Config), so each is one pair; a DAC
// whose one PCM format is S32_LE takes the first rate's third, at 24 valid
// bits.
var (
	toneRates    = []int{48000, 44100}
	tonePackings = []tonePacking{{format.S243LE, 24}, {format.S24LE, 24}, {format.S32LE, 24}, {format.S16LE, 16}}
)

// playTone is the run: the bound unit located on the tone's context, the
// node opened (openTone), what the kernel committed recorded and said, the
// unit's check after open of the node before a frame, the tone built for what
// was committed (testtone.Build, which asks the level's bounds again), whole
// repeats of it written for its length, and a drain. Each way it ends sets
// the tone's cause, which one set first outranks: a write or a drain that
// fails because the tone was ended answers that end.
func (b *Backend) playTone(t *toneHold, run *toneRun) {
	if t.ctx.Err() != nil {
		return
	}
	u, err := b.cfg.Locator.Locate(t.ctx)
	if err != nil {
		t.end(ToneNotLocated)
		return
	}
	if !b.openTone(t, run, u.Endpoint) {
		return
	}
	p := run.dev.Params()
	b.mu.Lock()
	t.format, t.rate, t.bits = p.Format, p.RateHz, run.bits
	b.mu.Unlock()
	b.log.Info("player: the console's test tone opened the DAC", "unit", u.Label, "format", string(p.Format), "rateHz", p.RateHz,
		"validBits", run.bits, "levelDBFS", t.req.LevelDBFS, "seconds", t.req.Seconds)
	n, err := run.dev.Node()
	if err != nil {
		t.end(toneDeviceEnd(err))
		return
	}
	if u.Admit != nil {
		if err := u.Admit(n); err != nil {
			t.end(ToneWrongDevice)
			return
		}
	}
	tone, err := testtone.Build(p.RateHz, run.bits, p.Format, t.req.LevelDBFS)
	if err != nil {
		t.end(ToneFailed)
		return
	}
	// A chunk of whole repeats, about a tenth of a second, as bridge output
	// pcm-test writes: the waveform runs on from one chunk into the next.
	reps := max(1, (p.RateHz/10+tone.Frames-1)/tone.Frames)
	chunk := bytes.Repeat(tone.Packed, reps)
	chunkFrames, fb := int64(reps*tone.Frames), int64(p.FrameBytes())
	total := int64(t.req.Seconds) * int64(p.RateHz)
	for written := int64(0); written < total; {
		k, err := run.dev.Write(t.ctx, chunk[:min(chunkFrames, total-written)*fb])
		written += int64(k)
		t.frames.Store(written)
		if err != nil {
			t.end(toneDeviceEnd(err))
			return
		}
	}
	if err := run.dev.Drain(t.ctx, p.BufferTime()+2*time.Second); err != nil {
		t.end(toneDeviceEnd(err))
		return
	}
	t.end(TonePlayed)
}

// openTone opens the bound unit's node for the tone: the first of its eight
// (rate, format) pairs the kernel commits exactly, a refine it refuses or a
// mismatch trying the next (alsapcm closes the refused node). A busy or denied
// open ends the tone open_refused, its cause recorded; any other refusal
// open_refused; none of the eight taken no_format. An open whose own cleanup
// is not confirmed stops there: the cleanup fences the device.
func (b *Backend) openTone(t *toneHold, run *toneRun, ep alsapcm.Endpoint) bool {
	for _, rate := range toneRates {
		for _, pk := range tonePackings {
			if t.ctx.Err() != nil {
				return false
			}
			dev, err := b.openToneNode(run, ep, alsapcm.Config{RateHz: rate, Format: pk.f})
			if run.unknown {
				t.end(ToneReleaseUnconfirmed)
				return false
			}
			if err == nil {
				run.dev, run.bits = dev, pk.bits
				return true
			}
			switch alsapcm.ReasonOf(err) {
			case alsapcm.ReasonNotSupported, alsapcm.ReasonMismatch:
				continue
			case alsapcm.ReasonBusy:
				run.lastError = ToneLastErrorBusy
			case alsapcm.ReasonPermission:
				run.lastError = ToneLastErrorDenied
			}
			t.end(ToneOpenRefused)
			return false
		}
	}
	t.end(ToneNoFormat)
	return false
}

// openToneNode is one open through Config.OpenTone. An open whose release
// answered an error (alsapcm.ErrReleaseUnconfirmed in its chain), and one that
// panicked, from which no Device came back to close, leave it unknown what
// this process holds: the run is marked so, and its cleanup fences the
// device.
func (b *Backend) openToneNode(run *toneRun, ep alsapcm.Endpoint, cfg alsapcm.Config) (dev *alsapcm.Device, err error) {
	defer func() {
		if r := recover(); r != nil {
			b.logPanic("the console's test tone's open", r)
			run.unknown, dev, err = true, nil, errors.New("player backend: the test tone's open panicked; the bridge's log has why")
		}
	}()
	dev, err = b.cfg.OpenTone(ep, cfg)
	switch {
	case err != nil && errors.Is(err, alsapcm.ErrReleaseUnconfirmed):
		run.unknown, run.cleanupErr = true, err
	case err == nil && dev == nil:
		err = errors.New("player backend: the test tone's open answered no device")
	}
	return dev, err
}

// toneDeviceEnd is the word a failed call to the tone's device ends it with:
// a device gone, or any other device error. A call its context ended answers
// that end, which the tone's cause holds already.
func toneDeviceEnd(err error) ToneOutcome {
	switch alsapcm.ReasonOf(err) {
	case alsapcm.ReasonDisconnected, alsapcm.ReasonNoDevice:
		return ToneOutputGone
	case alsapcm.ReasonCancelled:
		return ToneRequestEnded
	}
	return ToneDeviceError
}

// closeTone closes the tone's node, once, and reports whether its close is
// confirmed: nothing was opened, or Close answered nil. Close answering an
// error, a panic in it before it answered, and an open whose own cleanup is
// not confirmed are not: what this process holds is unknown.
func (b *Backend) closeTone(run *toneRun) (confirmed bool) {
	if run.unknown {
		return false
	}
	if run.dev == nil {
		return true
	}
	defer func() {
		if r := recover(); r != nil {
			b.logPanic("the console's test tone's close", r)
			confirmed = false
		}
	}()
	if err := run.dev.Close(); err != nil {
		run.cleanupErr = err
		return false
	}
	return true
}

// endTone gives the slot back once the tone's node's close has answered, in
// one step under the backend's lock: the slot free, the result recorded, and,
// for a close not confirmed, the device fenced as a release never confirmed
// fences it (terminalUnconfirmed: no tone, Start, probe or choice's write
// until serve restarts), the end then release_unconfirmed; then done is
// closed, the result handed to Tone, and the end said: a fence at Warn, with
// the error its cleanup answered where it answered one.
func (b *Backend) endTone(t *toneHold, run *toneRun, confirmed bool) {
	res := ToneResult{Outcome: t.ended(), Admitted: true, LastError: run.lastError, Frames: t.frames.Load()}
	b.mu.Lock()
	res.Format, res.RateHz, res.ValidBits = t.format, t.rate, t.bits
	if !confirmed {
		res.Outcome = ToneReleaseUnconfirmed
		b.terminalUnconfirmed, b.toneFenced = true, true
	}
	if b.toning == t {
		b.toning = nil
	}
	b.toneLast = res
	b.mu.Unlock()
	close(t.done)
	t.result <- res
	defer func() {
		if r := recover(); r != nil {
			b.logPanic("the console's test tone's end", r)
		}
	}()
	if !confirmed {
		attrs := []any{"frames", res.Frames}
		if run.cleanupErr != nil {
			attrs = append(attrs, "err", run.cleanupErr.Error())
		}
		b.log.Warn("player: the console's test tone's node did not close cleanly: nothing opens the DAC until bridge serve restarts", attrs...)
		return
	}
	b.log.Info("player: the console's test tone ended", "outcome", string(res.Outcome), "frames", res.Frames)
}

// ToneOutput opens or shuts the tone's gate, the supervisor's alone (serve's
// player, from the output reports and the watcher's snapshots): open admits
// tones again, unless serve's stop has closed them (CloseTones); shut refuses
// them with why and ends the tone that plays with it, and the DSD check (Check,
// B728 B12) failed, output_changed for a unit change and output_unavailable for
// any other shut. The gate starts shut, output_unknown.
func (b *Backend) ToneOutput(open bool, why ToneOutcome) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if open {
		b.toneOpen = !b.tonesClosed
		return
	}
	if why == "" {
		why = ToneOutputUnavailable
	}
	if b.tonesClosed {
		why = ToneServeStopping
	}
	b.toneOpen, b.toneShut = false, why
	if t := b.toning; t != nil {
		t.end(why)
	}
	if c := b.checking; c != nil {
		r := CheckOutputUnavailable
		if why == ToneOutputChanged {
			r = CheckOutputChanged
		}
		b.endCheckLocked(c, CheckFailed, r)
	}
}

// CloseTones closes the console's tones for good, serve's player's first step
// as it stops: in a step under gmu no tone is counted from then on
// (serve_stopping), and in one under the backend's lock the gate shuts with
// serve_stopping, never to open again, and the tone in the slot is ended. A
// tone counted before is refused at its slot or ended, and Wait joins it
// either way; the two steps take neither lock under the other.
func (b *Backend) CloseTones() {
	b.gmu.Lock()
	b.tclosed = true
	b.gmu.Unlock()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tonesClosed = true
	b.toneOpen, b.toneShut = false, ToneServeStopping
	if t := b.toning; t != nil {
		t.end(ToneServeStopping)
	}
}

// ToneStatus is the console's tone now, read under the backend's lock, with
// no I/O: playing, stopping (ended, its close not answered), stop_unconfirmed
// (ended more than ToneStopWait ago, its close not answered),
// release_unconfirmed (a tone's cleanup did not answer cleanly) or idle. It
// is built in one literal from a phase of its own: internal/api's census of
// the protocol's observation states reads every assignment to a State in
// this package as one.
func (b *Backend) ToneStatus() ToneStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	phase := ToneIdle
	if b.toneFenced {
		phase = ToneStateReleaseUnconfirmed
	}
	t := b.toning
	if t == nil {
		return ToneStatus{State: phase, Last: b.toneLast}
	}
	switch {
	case t.stopUnconfirmed:
		phase = ToneStateStopUnconfirmed
	case t.ctx.Err() != nil:
		phase = ToneStateStopping
	default:
		phase = ToneStatePlaying
	}
	return ToneStatus{State: phase, Request: t.req, Format: t.format, RateHz: t.rate, ValidBits: t.bits, Last: b.toneLast}
}

// The tone's seams (Seams.ToneEntering and the rest).

func (b *Backend) toneEntering() {
	if sm := b.cfg.Seams; sm != nil && sm.ToneEntering != nil {
		sm.ToneEntering()
	}
}

func (b *Backend) toneCounted() {
	if sm := b.cfg.Seams; sm != nil && sm.ToneCounted != nil {
		sm.ToneCounted()
	}
}

func (b *Backend) toneAdmitted() {
	if sm := b.cfg.Seams; sm != nil && sm.ToneAdmitted != nil {
		sm.ToneAdmitted()
	}
}

// toneClosed is the ToneClosed seam, after the node's close answered and
// before the slot is given back; a panic in it is recovered and said, and the
// slot is given back as the close's answer says.
func (b *Backend) toneClosed(confirmed bool) {
	defer func() {
		if r := recover(); r != nil {
			b.logPanic("the console's test tone's note of its close", r)
		}
	}()
	if sm := b.cfg.Seams; sm != nil && sm.ToneClosed != nil {
		sm.ToneClosed(confirmed)
	}
}
