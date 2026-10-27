# B470 2c.1 — provisional native sink and qualification

The implementation deliberately uses DSDSink beside Sink. PCM remains automatically active;
its base ioctl/HW_PARAMS/byte traces for 44.1/16 and 384/32 pin pause/release and natural advance.
Native transport identity adds logical rate and packing version, zero for PCM. The production
backend still refuses DSD; the only entry point is `output dsd-test`, never an HTTP route.

Canonical planar stereo is MSB-first. Native words take the oldest byte in their most significant
byte, then serialize LE/BE and interleave channels. Independent asymmetric literals pin packing.
WriteDSD counts accepted logical bits per channel, including its owned partial-word carry. Retry
only the suffix. Empty input allocates no occurrence. Final EOF pads with phased 0x69; compatible
occurrences share the carry. Ring, retained hardware prefix, carry, scratch and fixed ledger have a
32 MiB aggregate bound and 1024 outstanding occurrences; a small-bound test refuses the fifth.
Cross-layer reservation, occurrence notifications and engine startup integration are 2c.2.

Opening native is inert. Activate rechecks binding and the provisional descriptor/mixer snapshot;
Pause wins its fence without closing, Stop/revoke closes. Accepted prefill marks Committed.
The writer exclusively owns all device operations; no ioctl is issued while it holds the sink mutex.
TrackChange cancels the current write, accounts its accepted prefix and discards the suffix/software
queue, then emits only the qualified 0x69 tail and drains. Immediate escalates without waiting.
The shared release budget is two seconds, including cleanup. Parked DROP/HW_FREE/close retains
writer ownership after a bounded unconfirmed answer; no successor opens on that answer. Close
errors remain faults. Xrun/suspend/disconnect never calls Recover. Native pause explicitly chooses
kernel or silence, releases at 30 s, and rechecks on reopen; a busy reopen remains paused.
Durations round up at the hardware rate. PCM sides have zero tail and prefill.

The CLI injects its opener, control reader, clock and input. Both real native and reference openers
refuse testing.Testing(), and the CLI TestMain installs a separate control-node tripwire. Every
mandatory flag is validated before the opener. The wall-clock budget spans keys, pause and reopen.
The manifest and contact JSON schemas are versioned, with typed exact owner confirmation and no
cryptography. Calibrated listening assets are external owner inputs; software goldens are not used.
Descriptors are qualification-only reads: missing firmware/model fields never disable PCM binding.
Control snapshots are bounded read-only UAPI, with facts added to the existing Linux C/Go ABI gate.

qualificationVersion guards an enumerated path list. Its four-case test uses temporary Git objects
and read-only diff/show, including rename/delete. R8 approval version checking remains 2c.2.
No production admission, fallback, autoplay, engine lock/event, approval reader or approval writer
is claimed here. See b470-native-qualification.md for owner commands and pending Linux execution.

Cancelled opens retain cleanup ownership in the opening worker. Inert Pause also releases at 30 s; reopen geometry cannot outgrow the allocated store. Actual close acknowledgement wins over an expired release timer. The CLI continues reading Stop/quit while TrackChange is draining.

## Round 2 — 2026-10-27

PCM's packing table and refusal expectations are restored. The native opener has a
separate format table and NativeFrameBytes; native writes use that frame width.
Native setup, post-open refusal and writer cleanup retain DROP/HW_FREE/close errors.
An unconfirmed reopen cleanup is terminal before its waiter receives the error.

Control Access is retained. Only volatile PCM-interface controls are excluded from
the one fingerprint used by admission, contact validation and receipt creation.
All MIXER/CARD controls stay constrained. Reports include full snapshots at admission
and explicit excluded values before/after; an opening-worker timeout never races report
serialization.

One stdin reader cancels the whole audition on Stop/quit, including PCM references and
level-check. Playback keys are immediate on a raw TTY; at silent text prompts s/q are
whole lines. End is stopped/quit and exit 0 only after successful cleanup. Cleanup failure
keeps End, records Error and exits 1. No later leg or receipt follows either key.
PCM playback verifies and decodes one descriptor and passes binding admission to OpenSink.
The test-only overlay shares the real fake kernel with the complete CLI without shipping
a fake in the binary or weakening hardware guards.

TrackChange is established synchronously before producer cancellation. Finish cancellation
cannot escalate it. Same-mode releases join the first cutoff/deadline; only initial release
or Immediate escalation interrupts. Finish observes the release deadline even at natural
EOF while a parked writer retains cleanup ownership. Activation/resume waiters receive their
own request result even if cleanup completes first. Resume requires an activation generation,
rechecks fences after STATUS, and closes a newly opened device when admission is fenced.

The native-qualification CI job is in gate's dependencies. CheckVersion runs against the
actual PR head's merge base with fetched main, with full history and fail-closed ref
resolution; main pushes run integration but do not compare a commit to itself.
GuardedPaths now also includes all activation-deciding control, attachment and evidence
paths, the native opener, platform openers, console input and version checker. Version is 2.
Escalation, retained-word replay and all seven mandatory-flag tests are regression pins:
their explicit mutations, not a baseline failure, demonstrate sensitivity.

A round-2 control exposed the idle-wait race too: Pause can win after the writer's
loop snapshot but before watchNative installs its context. watchNative rechecks an
unapplied Pause under the mutex, returning to the writer instead of installing an
uncancelled wait. TestNativeIdlePauseFence pins that exact window.

## Round 3 — 2026-10-27, PR #1355 merge gate

Contact schema 2 and qualificationVersion 3 invalidate earlier receipts. Device and
Subdevice are required pointer fields on import, compared before either opener;
resolution exposes the expected subdevice and opened-node checks still precede
writes. Final attachment and filtered mixer equality now gate publication. Failed,
cancelled or incomplete reports cannot retain or import a contact. Volatile PCM
controls remain excluded. Final validation shares the run deadline.

Completeness is per supplied fixture occurrence, including duplicate IDs, from
Position.Played rather than decoder EOF. Level-check no longer truncates its list.
Navigation stops qualification after release and makes evidence incomplete,
including queued navigation behind a space racing natural completion; the whole
queued batch is inspected. First-fixture p is ignored and recorded without a transition.
Interactive navigation remains available.

Writer faults establish Immediate/revocation/deadline before teardown; terminal
Activate refuses without waiting. TrackChange reserves the close margin from the
remaining budget, refuses a nonpositive drain budget, wraps tail/drain failures as
unconfirmed, and propagates final measurement errors. The release clock and timer
are injected together; completed cleanup wins over an expired waiter. Cancelled
openers expose completion through admission and cleanup. The harness follows that
fence within its original allowance and skips final reads/successors after expiry.

G1's proposed Pause-to-Immediate sequence did not hold: Pause sets held before the
writer handles cancelled prefill; a later Resume only queues work. Regression
pins cover empty and partly accepted prefill cancellation, the following Resume
(no replay of interrupted prefill), and terminal write faults. G7's dirty carry
requires an impossible cursor-only reset; clearing before setting is defensive.

Console cancellation joins the reader. Production files reuse pcmTestStoppableFile's
poll/wakeup without closing stdin or changing shared descriptor flags. Injected
readers provide an explicit Interrupt adapter for cancellation. Raw prompts echo printable
bytes, implement Backspace/Ctrl-H, and translate newlines. Asset stat checks share
fsutil.NotAFile (cloud placeholders allowed); Linux-only audition remains unchanged.
The integration overlay fails closed on missing/changed/residual tags or empty fake
support; CI requires an actual passing integration test. The version checker adds
binding/, alsa/ and decode/ dependencies, plus shared format, file, config, USB
identity and CLI output/input dependencies. Linux execution and ABI are still pending;
no hardware qualification, production DSD admission or 2c.2 wiring is claimed.

The full-harness deadline case also pins a late admission: after its blocked
checker returns, it rechecks the run context before any control read. A parent
context alone misses the harness wall deadline. Unsupported injected readers
refuse before opening audio unless they provide an explicit Interrupt contract.
