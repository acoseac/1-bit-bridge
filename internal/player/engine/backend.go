package engine

import (
	"context"
	"errors"
	"strconv"
	"time"
)

// The backend calls' deadlines. A call that has not answered by its
// deadline is treated as failed (Backend's contract).
const (
	// ReleaseTimeout bounds a release: Stop, every other end of a session,
	// and a paused session's own release of the device after PauseRelease.
	ReleaseTimeout = 2 * time.Second
	// StartTimeout bounds Start: the file opened and checked, the bound unit
	// resolved and checked, the device opened and the first frames queued.
	StartTimeout = 5 * time.Second
	// CallTimeout bounds Prepare, Cut, Pause, Resume and Discard, each of
	// which hands its session a request and answers.
	CallTimeout = 2 * time.Second
)

// SessionID names one playback session. The engine numbers them from 1 and
// never uses one twice in a run.
type SessionID uint64

// OccurrenceID names one hand-off of an entry to a session. One entry
// handed off twice (repeat with a one-entry queue) is two occurrences, so
// each boundary is reported apart.
type OccurrenceID uint64

// Occurrence is one hand-off of a queue entry.
type Occurrence struct {
	ID    OccurrenceID
	Entry uint32
	Ref   Reference
}

// SessionOptions are the configured values a session plays under; the
// engine passes them in from its Config and decides nothing from them.
type SessionOptions struct {
	// PauseRelease is how long a pause holds the device before the backend
	// releases it (pauseReleaseSeconds).
	PauseRelease time.Duration
	// DSD is the configured DSD mode. This release plays PCM only: its
	// backend refuses DSD content in every mode, off included, before any of
	// it plays, as dsd_not_in_this_release (playback.ReasonDSD), since no
	// setting turns DSD on yet; ReasonDSDOff is for the release that plays
	// it, under mode off.
	DSD string
}

// StartRequest is what Start begins a session with.
type StartRequest struct {
	Session SessionID
	First   Occurrence
	Options SessionOptions
	// Events is where the session's events go.
	Events Events
}

// Join is how a successor follows the content before it.
type Join struct {
	// Gapless is true when its transport is the session's: its frames
	// follow with nothing between.
	Gapless bool
	// Differs names what differs otherwise: the backend drains, closes and
	// reopens within the session, an honest gap.
	Differs []string
}

// Position is the content of one occurrence the hardware has played, as
// the backend establishes it from the sink's Position.Played: frames, never
// silence, and never more than was measured.
type Position struct {
	Occurrence OccurrenceID
	Frames     int64
	RateHz     int
	// Measured is when Frames was established, on the backend's clock.
	Measured time.Time
}

// Seconds is Frames as content time.
func (p Position) Seconds() float64 {
	if p.RateHz <= 0 {
		return 0
	}
	return float64(p.Frames) / float64(p.RateHz)
}

// Fault is a failure after content played: an xrun, a disconnect, a device
// error, a decode failure. Playback stops and does not restart on its own.
type Fault struct {
	Reason Reason
	Detail string
	// Occurrence and Frames are where it happened: the content of that
	// occurrence played before it.
	Occurrence OccurrenceID
	Frames     int64
	At         time.Time
	// Entry is the occurrence's entry; the engine fills it in.
	Entry uint32
}

// Unplayable is an entry the backend refused before any of its content
// played: a missing file, content the decoders refuse, DSD while the DSD
// mode is off. The engine skips it.
type Unplayable struct {
	Reason Reason
	Detail string
}

func (u *Unplayable) Error() string {
	if u.Detail == "" {
		return "player: unplayable: " + string(u.Reason)
	}
	return "player: unplayable: " + string(u.Reason) + ": " + u.Detail
}

// ResumeResult is how a resume went on.
type ResumeResult int

const (
	// ResumeContinued resumed a session that still held the device.
	ResumeContinued ResumeResult = iota + 1
	// ResumeReopened opened the device again after the backend had
	// released it during the pause, checked that the unit opened is the
	// bound one before any sample, and kept the frames it had retained.
	ResumeReopened
)

func (r ResumeResult) String() string {
	switch r {
	case ResumeContinued:
		return "continued"
	case ResumeReopened:
		return "reopened"
	}
	return "resume result " + strconv.Itoa(int(r))
}

// ErrSessionEnding is what Prepare returns when its session has begun
// playing out its last content and takes no successor: the engine plays the
// successor in a session of its own once this one has finished.
var ErrSessionEnding = errors.New("player: the session is playing out its last content")

// Backend is what plays: PR 4b2 implements it over internal/player/decode
// and alsapcm.Sink, and this package's tests over a fake. It is a contract,
// and these are its terms.
//
// Calls. The engine makes every call on a goroutine of its own, never the
// owner's, under a context whose deadline is the call's (StartTimeout,
// ReleaseTimeout, CallTimeout): ctx.Deadline reports it and the context ends
// there with context.DeadlineExceeded, both on the engine's clock (it ends
// with context.Canceled at shutdown), and so does a context derived from
// it, context.WithCancel's or an errgroup's. The engine takes what the call
// returns as its result. A call that has not answered by its deadline is treated as
// failed: for Release the release is unconfirmed (below); for any other call
// the engine releases the session, playback stops with ReasonTimeout, and
// the late answer is ignored, even when it reaches the engine before the
// deadline's own expiry does. A call that ignores its context and never returns
// is abandoned and logged; its goroutine then outlives Run. Events come back
// through the session's Events from any goroutine, in order within a
// session.
//
// Sessions own resources. Start begins a session with an ID the engine
// chose, and every event names its session; the engine applies an event
// only while its session is current, and discards the rest. A session that
// is no longer current must still close what it acquired: Release may come
// while Start is still opening (a Stop during the open), and then whatever
// Start goes on to open is closed at once and never written to, and the
// release is acknowledged only after that. Because each call runs on a
// goroutine of its own, that Release may even reach the backend before its
// Start has begun: the backend must then make that Start acquire nothing,
// and may acknowledge as soon as nothing is held. Prepare, Cut, Pause and
// Resume come only after Start has returned nil. The engine starts a
// session only once the previous session's release is acknowledged, so two
// of its own sessions never hold or race for the device: on the real sink
// the second open would get its busy refusal from the bridge's own process.
//
// Release is confirmed or it is not. Release returns nil once its session
// holds nothing: the device and the decoders closed and its goroutines
// done. A Release that cannot say so by its deadline returns its context's
// error and calls Events.Released once it can. Past ReleaseTimeout the
// engine records the release as unconfirmed and opens nothing until the
// acknowledgement comes: Play, PlayID, PlayIndex, Next and Previous are
// refused with ReasonReleaseUnconfirmed, and maintenance is entered
// unconfirmed. The late acknowledgement clears it, and runs nothing the
// deadline dropped: an acknowledgement at or past the deadline is late
// whichever reaches the engine first, it or the deadline, and the engine
// stamps Events.Released as the backend calls it. Every session ends with
// a Release the engine asks for and its acknowledgement: after Stop, after
// Finished, after a fault, after a failed or refused Start.
//
// Successors. The engine names what follows each occurrence early, for the
// gapless join: Prepare(after, next) once after's frames have entered the
// sink (the first occurrence's when Start returns), with next nil when
// nothing follows and the session finishes after after. A later Prepare for
// the same after replaces an earlier one whose occurrence has not been
// submitted. The backend reports each occurrence's stages once, in order:
// Prepared (its decoder open and the join decided: Join.Gapless, or another
// transport, which it drains, closes and reopens for within the session, an
// honest gap); Submitted (its first frames have entered the sink and can no
// longer be withdrawn by forgetting them); Audible (the played content
// crossed its first frame: cumulative content frames against the sink's
// Position.Played, never the decoder's end of file). Several boundaries
// crossed within one position measurement each get their Audible, in
// order. An occurrence refused before any of its content played is
// reported Refused and never submitted; a first occurrence so refused makes
// Start return an *Unplayable. Prepare returns ErrSessionEnding once the
// session is playing out its last content, and then the engine names no
// further successor in the session. Any other error from a Prepare whose
// successor the backend has already reported Audible is stale: what was
// heard is kept and successors are named after it.
//
// Edits after hand-off. An edit that changes the successor before it is
// submitted is a new Prepare. After submission the engine asks Cut(from):
// end the content before from at its end, and discard every frame of from
// and of what follows it; the content before then has no successor until the
// next Prepare. Cut answers whether it could: false when some frame of from
// has been or will be played, and then the engine stops playback at once
// when from becomes audible. The sink has no tail truncation today, so 4b2
// adds one for the frames the store holds that have not reached the device.
// Edits that do not change the successor make no call.
//
// Pause and resume. Pause pauses (the kernel's PAUSE, or zeros); after
// SessionOptions.PauseRelease the backend releases the device itself and
// reports DeviceReleased, under the same rule as Release: the engine waits
// PauseRelease plus ReleaseTimeout for it and otherwise records the release
// as unconfirmed. Resume answers ResumeContinued or ResumeReopened, or an
// error (busy, the identity check, another) that leaves the session paused
// with the reason; only the backend knows which it was, and the engine
// records it. A reopen checks that the unit it opened is the bound one (4a's
// identity check) before any sample flows and keeps the frames the sink
// retained: today Sink's own reopen opens the saved card number with no
// identity hook, so 4b2 adds that hook.
//
// Faults. An xrun, a disconnect, a device error or a decode failure after
// content played is reported Fault, with where it happened. The backend
// stops playing and never starts again on its own, and never calls the
// sink's Recover on its own; the engine records the fault and releases the
// session.
//
// Finished is everything handed off played out, after a Prepare with no
// successor.
//
// Discard drops whatever the backend prepared outside a session (a cached
// resolution of the bound unit, an opened file): the engine calls it when
// the output changes while nothing plays.
type Backend interface {
	Start(ctx context.Context, req StartRequest) error
	Prepare(ctx context.Context, s SessionID, after OccurrenceID, next *Occurrence) error
	Cut(ctx context.Context, s SessionID, from OccurrenceID) (bool, error)
	Pause(ctx context.Context, s SessionID) error
	Resume(ctx context.Context, s SessionID) (ResumeResult, error)
	Release(ctx context.Context, s SessionID) error
	Discard(ctx context.Context) error
}

// Events is how a backend reports what its sessions do. The engine
// implements it; every method returns at once (after Run, without effect)
// and is safe from any goroutine.
type Events interface {
	// Prepared is o's decoder open and its join decided.
	Prepared(s SessionID, o OccurrenceID, j Join)
	// Refused is o refused before any of its content played.
	Refused(s SessionID, o OccurrenceID, why Unplayable)
	// Submitted is o's first frames in the sink.
	Submitted(s SessionID, o OccurrenceID)
	// Audible is the played content crossing o's first frame.
	Audible(s SessionID, o OccurrenceID)
	// Position is the content of the audible occurrence played.
	Position(s SessionID, p Position)
	// Fault is a failure after content played; the session plays no more.
	Fault(s SessionID, f Fault)
	// Finished is everything handed off played out.
	Finished(s SessionID)
	// DeviceReleased is a paused session's release of the device after
	// SessionOptions.PauseRelease.
	DeviceReleased(s SessionID)
	// Released acknowledges a release Release could not confirm in time.
	Released(s SessionID)
}
