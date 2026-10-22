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
// holds cannot hold the engine's call past its deadline.
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
// the session's goroutines, closes every sink and every decoder and
// content, and lets go of the device; Release answers nil once that is
// done, or its context's error at its deadline, and the teardown then calls
// Events.Released once nothing is held. A sink the kernel holds in a call
// that never returns keeps its teardown in Close, abandoned: its release is
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
// Prepare answers engine.ErrSessionEnding once the producer plays out the
// last content, and the session reports Finished once that drain ends.
//
// Edits after hand-off: Cut drops an occurrence and what follows it. With
// no frames in a sink it goes at once; with frames there it is cut from the
// sink at its first frame (alsapcm.Sink.Cut), which answers false when a
// frame at or after it is the kernel's or is in the write under way, and
// then nothing is dropped.
//
// Pause and resume: Pause is the sink's (the kernel's PAUSE, or zeros), and
// after SessionOptions.PauseRelease the sink lets the device go itself,
// which the reporter reports as DeviceReleased. A pause that finds a sink
// playing out its last frames, or no sink (between two transports), pauses
// after them with no device held, and reports the release once the pause
// release has passed. Resume answers ResumeContinued for a sink that held
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
// # Goroutines
//
// A session runs a producer (decode, pack, write, drain, transport
// changes), a reporter (every event, in order) and an opener per successor
// while it opens. The backend counts every goroutine it starts
// (Stats.Goroutines, Wait), and the teardown joins a session's; the only
// one that can outlive a release is the one in a sink's Close, or a call's
// command, while the kernel holds the sink's writer in a call that never
// returns.
package playback
