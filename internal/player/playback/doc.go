// Package playback is the bridge player's backend (B470 PR 4b2): it
// implements engine.Backend over internal/player/decode, alsapcm.Sink for
// PCM and alsapcm.DSDSink for the DSD the player's effective DSD policy
// plays (native, or DoP in S32_LE; B728 B7, the cutover, in place of B470
// 2c.2 and 2d.2's owner-approved policy, which is the harness's alone),
// through one DAC the binding names (internal/player/binding), opened
// exclusively. The engine (internal/player/engine) owns the state and
// decides; this package plays what it is handed and reports what the
// hardware did.
//
// # DSD by the effective policy (B728 B7)
//
// Production (Config.DSD, serve's and bridge output play's backend) reads no
// approval record. One effective set (internal/player/policy's Effective,
// over the player's DSD state, the profile database, the bound DAC's
// altsets, the silent probe's answers and the DoP gain check's reading of
// the card) is published under a generation, and selection admits a DSD
// occurrence by that same set, its plan at the content's rate or the rate's
// refusal by the policy's word; a rate the policy converts to PCM plays
// converted (B728 B11, below), and no set (a stale reading of the box, no
// unit, an identity that did not read) refuses every rate
// (ReasonDACUnread). The set is read again at a session's first occurrence
// and at any DSD occurrence's admission it was read more than 100 ms before,
// at each native activation, on a 100 ms monitor while a session holds DSD,
// and at the supervisor's refresh (RefreshDSD), never on the audio path; a
// change is swapped in one step and reconciled per occurrence
// (policy.Reconcile): kept, handed off again through the engine
// (engine.ReplanEvents) while no frame of it is the kernel's, or revoked. The check after open requires the opened node to be
// the DAC the set was computed for, and for DoP the gain check read afresh.
// There is never a fallback to another transport at run time. Playback
// records in the state what it observed: a real open's driver_accepted, a
// probe's, and once an occurrence played five seconds of content with no
// fault its tuple's streaming_observed and its transport's first use
// observed. The silent probe runs when one is due (ProbeDSD), on the device
// slot a session holds, never beside one.
//
// # DSD converted to PCM (B728 B11)
//
// A rate the effective set converts opens through the PCM output path: the
// occurrence's decoder is internal/player/dsdconv's converter over its DSD
// source, made where its open ends for the plan it is admitted under then,
// and its transport the plan's rate and valid bits in the target's packing
// (policy.Target.Packing, the first of the table's the DAC takes with those
// bits significant), never a packing a PCM track took or the one the sink
// before it holds. Its policy ownership (the set and plan it was admitted
// under) is a DSD occurrence's, apart from its transport: readmission at the
// open's end, the check after open of its node at every open and reopen, its
// activation, reconciliation and revocation judge it as native and DoP, and
// Revoke drops a PCM sink that holds its unplayed frames. A join of two
// converted occurrences is classified by dsdconv.Continuous alone: a
// continuous successor shares the predecessor's converter chain, whose last
// H frames, its join window, wait at its window point until the successor
// settles or the sink's lookahead falls to 100 ms; any other successor of
// the same PCM transport abuts in the same sink, the window zero-padded; one
// of another transport reopens. A cut of a successor whose first bits went
// into its predecessor's window, whether or not it submitted a frame, is the
// producer's alone: before the window reached the sink the predecessor goes
// back to its window point (Rewind); after, the sink's marker goes to the
// window's first frame, the window and what follows are dropped and the
// window submitted again, computed for what follows now; once a frame from
// there is the kernel's, or the window cannot be submitted again, the cut is
// late. Such a predecessor's converter is kept past its end until its
// successor is heard. A converted occurrence's Details are its plan's PCM
// (codec DSD, never lossless), its observation's transport converted with
// the plan, and seeking it stays unsupported.
//
// # Content and packing
//
// The engine hands the backend an engine.Reference, never a path, and a
// Resolver maps it to content: the harness's over the files it was given,
// release 5a's over the library index with its URI confinement. The backend
// opens nothing a resolver did not hand it. Content inspection chooses the
// decoder (decode.Open), and everything but stereo integer PCM at a rate the
// ALSA backend opens, or DSD the effective set plays, is refused before any
// of it plays, by its kind: DSD the effective set does not play, with its
// named reason (ReasonDSD by a backend with no DSD admission); floating-point
// PCM and every other decoder refusal by the decoder's own reason
// (float_pcm_refused, unknown_container, lossy_refused, …); content the
// resolver could not open
// (engine.ReasonMissing); no frames (ReasonEmpty); a rate outside
// alsapcm.MinRateHz..MaxRateHz (ReasonRate); and content the DAC takes in
// none of its depth's packings (ReasonTransport). Each occurrence's first
// chunk is decoded at its open, so content that cannot give a frame is
// refused, not started.
//
// Samples reach the DAC as the decoder gave them, packed by the format
// package as B470's §1 table says (packings): 16 bits as S16_LE, or on a DAC
// that does not take it S24_3LE or S24_LE (shifted left eight, the low byte
// zero) or S32_LE (shifted left sixteen, the low two bytes zero), since
// B735; 24
// bits as S24_3LE, S24_LE (four bytes, the low 24 significant) or S32_LE
// (shifted left eight, the low byte zero); each depth's offered in that
// order, the one the DAC took kept until Discard; 32 bits as S32_LE. Nothing
// after the decoder converts a value: no resampling, no dither, no gain. A
// converted occurrence's decoder is the converter, whose samples are packed
// by the same table.
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
// opened without writing to it. So does the engine's taking the release
// before its Release comes (engine.StartRequest.Released, B674 rounds 2 and
// 3), at every step that would acquire, open, adopt, resume or write: a Start
// acquires nothing, an open is refused before it or at its check after open
// (output.admit, which the sink closes unwritten), a sink opened is closed
// at once rather than adopted, a resume resumes nothing, and the producer
// hands the sink nothing more. The backend refuses a Start while another
// session still holds the device (ErrOverlap, Stats.Overlaps): under the
// engine's contract it never happens, so a defect shows as a refusal a test
// sees, never as the kernel's busy refusal of the bridge's own second open.
//
// The silent probe (Probe, B728 B4) shares that device slot: it takes it in
// the step that finds no session holding it (none plays, opens, is paused or
// is being released, and no release was left unconfirmed), holds it from
// the binding's location to its node's close, and a Start that comes
// meanwhile waits for it, at most ProbeWait, holding nothing, then answers
// the device busy. A probe never waits: one the slot refuses opens nothing.
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
// Just before an occurrence's Audible, to a session's Events that takes
// them (engine.DetailsEvents, B470 PR 5a3), the reporter reports its
// Details: the decoder's own length (its total frames) and format, the
// codec content inspection chose, and lossless. A session's first
// occurrence's, when it is PCM, Start reports before it answers, so a seek
// that comes while the session starts knows the content (B674), and the
// reporter reports them no more: each occurrence's once.
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
// Seeks (B674): a seek within a track is the engine's release of the
// session and a Start whose first occurrence continues the one heard from
// engine.StartRequest.From, so the release's terms hold for it (its sinks
// dropped with the frames the DAC had not played, its successors with them,
// everything joined and closed before the acknowledgement), and so do the
// start's (the binding located again, the node checked after open). Start
// positions the decoder at From with its exact Seek, on Start's goroutine,
// before it decodes the first chunk and before the session's producer
// exists, so no Seek races a ReadFrames and nothing before From reaches the
// sink; the samples after it are the decoder's as at any start, bit for bit.
// The reporter counts the occurrence's position from From (From plus what its
// sink has played of it), and a decode failure's frames the same. A From
// that DSD content would need (any but 0: a demuxer's byte position is no
// time seek), a frame the decoder does not reach, or one with nothing after
// it fails the Start with an error, never a refusal, so the entry is not
// skipped, before the unit is located or the device opened.
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
//
// A panic on one of them is recovered there, logged once (where, its stack,
// and its value only when it is the runtime's own error), and is the
// session's fault, typed backend_failed (ReasonFailed): the reporter
// reports it, the reporter's own panic included, which it sends itself, so
// the engine releases the session and the device is let go as for any
// fault; a call's command answers its caller with it. So is a panic in a
// call the engine makes into a session (Start, Prepare, Cut, Pause,
// Resume), on the engine's goroutine for that call, which answers with the
// fault as its error, so a session a panic cut short mid-change never
// plays on. A teardown goes on
// past one: a file's close recovers its own and closes the other file, and
// a sink whose close panicked ends with the session's life, which the
// teardown waits for before it lets go of the device and acknowledges. Each
// hold of the session's lock across work that can panic (a sink's command,
// the session's bookkeeping) is let go of by a deferred release (lockHold),
// so a recover, and the release after the fault, never find it held. A
// sink's writer that panics ends its sink typed (alsapcm.ReasonPanicked),
// which the reporter reports as the backend's own fault, never the device's.
// ReleaseAll releases every session for an owner that can release none: an
// engine whose Run ended in a panic.
package playback
