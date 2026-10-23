// Package playback is the bridge player's backend (B470 PR 4b2): it
// implements engine.Backend over internal/player/decode and alsapcm.Sink,
// PCM only, through one DAC the binding names (internal/player/binding),
// opened exclusively. The engine (internal/player/engine) owns the state
// and decides; this package plays what it is handed and reports what the
// hardware did.
//
// # Content and packing
//
// The engine hands the backend an engine.Reference, never a path, and a
// Resolver maps it to content: the harness's over the files it was given,
// release 5a's over the library index with its URI confinement. The backend
// opens nothing a resolver did not hand it. Content inspection chooses the
// decoder (decode.Open), and everything but stereo integer PCM at a rate the
// ALSA backend opens is refused before any of it plays, by its kind:
// DSD, whatever the DSD mode (ReasonDSD); floating-point PCM and every other
// decoder refusal by the decoder's own reason (float_pcm_refused,
// unknown_container, lossy_refused, …); content the resolver could not open
// (engine.ReasonMissing); no frames (ReasonEmpty); a rate outside
// alsapcm.MinRateHz..MaxRateHz (ReasonRate); and content the DAC takes in
// none of its depth's packings (ReasonTransport). Each occurrence's first
// chunk is decoded at its open, so content that cannot give a frame is
// refused, not started.
//
// Samples reach the DAC as the decoder gave them, packed by the format
// package as B470's §1 table says (packings): 16 bits as S16_LE; 24 bits as
// S24_3LE, S24_LE (four bytes, the low 24 significant) or S32_LE (shifted
// left eight, the low byte zero), offered in that order, the one the DAC
// took kept until Discard; 32 bits as S32_LE. Nothing converts a value: no
// resampling, no dither, no gain.
//
// # The contract, term by term
//
// Calls: every call honours its context. Start's work is bound to its
// deadline and to the session's release; Prepare and Cut record what they
// change under the session's lock and answer at once, but for a cut whose
// frames are in a sink the producer is writing to, which the producer makes
// between two writes, ending the write under way; Pause and Resume hand the
// sink its command on a goroutine of their own (call), so a sink the kernel
// holds cannot hold the engine's call past its deadline, and the producer
// hands a sink its Drain, and a new transport's sink the session's pause,
// the same way and outside the session's lock, so such a sink never holds
// the producer, or a Release waiting for that lock, past the session's
// release.
//
// Sessions own resources: each session has a life (its context) that every
// goroutine it starts and every sink it opens ends with. A Release that
// comes before its Start leaves a record the Start finds, which then opens
// nothing; one that comes while Start opens makes Start close what it
// opened without writing to it. The backend refuses a Start while another
// session still holds the device (ErrOverlap, Stats.Overlaps): under the
// engine's contract it never happens, so a defect shows as a refusal a test
// sees, never as the kernel's busy refusal of the bridge's own second open.
//
// Release is confirmed or it is not: Release ends the session's life, so
// its sinks drop their streams and close their devices; a teardown joins
// the session's goroutines and a resume's open between two transports, whose
// node no sink holds until the check after open has answered, closes every
// decoder and content and then every sink, and lets go of the device;
// Release answers nil once that is done,
// or its context's error at its deadline, and the teardown then calls
// Events.Released once nothing is held. The join comes first: a producer
// held in a read of its content that does not return (a file on a mount
// that hangs) still holds its decoder, so nothing is closed under it, and
// the release stays unconfirmed until the read returns. A sink the kernel
// holds in a call that never returns keeps its teardown in Close,
// abandoned, every decoder and content closed already: its release is
// never acknowledged, which is the truth.
//
// Successors: Prepare opens next on a goroutine of its own, decodes its
// first chunk and decides its join: gapless when its transport (rate,
// channels, sample container and packing) is the sink's, so its frames
// follow the last ones with nothing between; otherwise the producer drains
// the sink, closes it and opens one for it, within the session, an honest
// gap whose Join names what differs. A later Prepare for the same after
// replaces what followed it, cutting its frames from the sink when they are
// there, and fails when they cannot be cut (the engine then stops when they
// are heard). Each occurrence's stages are reported once, in order, by the
// reporter: Prepared, Submitted once its first frames are in the sink, and
// Audible when the sink's Position.Played, content frames never silence,
// passes its first frame in that sink, never at the decoder's end of file;
// several boundaries crossed in one measurement each get theirs, in order.
// The sink measures nothing while the kernel drains a sink's last hardware
// buffer (alsapcm.Sink.Drain), so a boundary in a sink's last 200 ms or so
// is reported when that drain ends. Prepare answers engine.ErrSessionEnding
// once the producer plays out the last content, and the session reports
// Finished once that drain ends.
//
// Edits after hand-off: Cut drops an occurrence and what follows it. With
// no frames in a sink it goes at once; with frames there it is cut from the
// sink at its first frame (alsapcm.Sink.Cut), which answers false when a
// frame at or after it is the kernel's or is in the write under way, and
// then nothing is dropped.
//
// Pause and resume: Pause is the sink's (the kernel's PAUSE, or zeros), and
// after SessionOptions.PauseRelease the sink lets the device go itself,
// which the reporter reports as DeviceReleased. A pause acts on the
// transport current when it decides: a sink playing out its last frames
// refuses it, and by then the producer may have opened the next transport's
// sink and started it, which the pause then asks. With no sink (between two
// transports), or the one that refused still current, the session pauses
// after them with no device held, and reports the release once the pause
// release has passed and no sink is current: until the producer has closed
// the sink that played out and recorded the occurrence it opens one for
// next, a resume told of the release would find none to open, and between
// two transports the resume opens the next one's sink itself. A sink the
// producer opens after the pause takes it before a frame. Resume answers
// ResumeContinued for a sink that held
// the device, ResumeReopened for one the sink opened again at the endpoint
// the binding resolves to now (SinkConfig.Locate), checked by the check
// after open before a frame (SinkConfig.Admit), with the frames it kept, or
// a *ResumeError with the reason (busy, wrong_device, not_located, …) and
// the session left paused.
//
// Faults: an xrun, a disconnect, a suspend or another device error stops
// the sink; the reporter reports Fault with the occurrence heard and its
// frames played, and the producer writes no more. Recover is never called.
// A decode that fails after content played plays out what was decoded
// before it, then faults; it is never a skip.
//
// Discard forgets the packings the bound unit took.
//
// What a session keeps: what it may still report. Once a report step has
// sent every event of the occurrences before the one heard, they leave the
// session, their decoders, content and first chunks let go (closeOcc), and
// so does each sink closed that nothing left refers to, its store with it,
// so a session that plays for hours holds a sink or two. One whose open is
// under way (a successor dropped while its opener still reads, which may
// never end) is its opener's until the open ends: retirement passes over
// it, holding back nothing else, and it leaves with its opener. A Cut of an
// occurrence retired answers false: it has played. Stats.Sinks and
// Stats.Occurrences count what the sessions hold.
//
// # Goroutines
//
// A session runs a producer (decode, pack, write, drain, transport
// changes), a reporter (every event, in order), an opener per successor
// while it opens, and a resume's open between two transports while it
// runs. The backend counts every goroutine it starts
// (Stats.Goroutines, Wait), and the teardown joins a session's; the only
// one that can outlive a release is the one in a sink's Close, or a call's
// command, while the kernel holds the sink's writer in a call that never
// returns.
package playback
