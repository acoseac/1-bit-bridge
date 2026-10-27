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

## Round 4 — 2026-10-27, PR #1355 third merge-gate pass

Qualification version 4. C1's successful-open/cancel publication race is closed by
an unbuffered ownership handoff: either the caller receives the sink and owns its
release, or the opener worker keeps it through Done. The remaining-budget waiter
retains the worker fence and cleanup errors. C2/A1 now joins Release on the original
sink deadline even when Finish returned cancellation with ErrReleaseUnconfirmed;
a confirmed release removes only that transient marker, retaining cancellation and
other faults. The sink lifecycle is unchanged. A2's opener/PCM waits use env.now/at,
never later than the run deadline and never a second two-second allowance. The real
integration test advances injected time instead of sleeping 1900 ms.

C3 checks cancellation between attachment and controls in both admission and final
validation. Its integration test parks attachment itself; late workers cannot start
controls after cancellation. A3 uses one played-frame current-occurrence rule in
playback and the post-done key drain: the first occurrence remains current at its
exact end until successor content has played. A4 bounds coverage to frames actually
accepted from each occurrence by that sink, capped at its content length; duplicate
IDs and multi-sink partial runs cannot borrow excess Played from another occurrence.

A5's suggested configpath and host-tree gaps were already guarded (configpath.go
explicitly, output_host.go via output_, usbid/ by prefix). Grep found two actual
transitive dependencies: cli_consts.go's defaultConfigPath and packaging.go's
DefaultConfigDir. Both now have unchanged-version rejection cases, beside the
already guarded loader and host-tree file; the guard does not cover all cmd/bridge.
A6 preserves exactly the echoed edited line, accepts CR/LF/CRLF as one terminator,
and refuses tabs/non-ASCII visibly. A split CRLF is swallowed even if its LF arrives
after the next prompt starts. Runbook records each behavior. No hardware, Linux
execution/ABI qualification, production DSD admission or 2c.2 integration is claimed.

Review precision: A4's proposed production overcount is not reachable from silence
or padding: sink_dsd_writer.go's measureNative counts contentAmong and clamps Played
to submitted bits. feedNative feeds the remaining list into the same sink. The new
accepted-frame bounds are defensive at the harness seam; excess-Played mocks supply
the negative evidence, not a claimed real-sink trace. A3's post-done first-fixture
case on a multi-item list likewise needs an early-completing mock; real Finish waits
for all fed content. The exact first-boundary playback case is reachable, and both
branches now share the rule. A1's already-expired teardown case must still exit
unconfirmed; the corrected reachable case is cancellation before expiry (C2).

## Round 6 — 2026-10-27, PR #1355 CodeRabbit follow-up

Qualification version 5. Raw mode moves from command entry into the console's
execution scope, after option/evidence validation and report-file creation.
The instance makeRaw hook returns restoration of the exact captured terminal
state. It runs only for tty input; a deferred restore follows the console's
reader join and precedes diagnostics and report serialization, also on errors
and panic unwinds. A restore failure is joined with the run error and prevents
successful qualification. Tests inject terminal state, never touch the host's.

The native release tests use the injected clock and channel handoffs; closed
fault activation waits for the writer's Done before calling Activate. No 100 ms
or three-second wall-time assertion remains in these cases. The test binary's
-timeout is the deadlock watchdog, not a release-latency assertion. No wider
sleep or scheduler-performance bound replaces them.

Queued-entry assertions parse complete timestamped lines and compare each full
path with filepath.Clean, preserving OS separator rules, exact count and queue
order. Every queued line and the availability report remain after the warning
and before Start, independently of one another. Equivalent ./ paths pass;
a suffix appended to a path, reordered paths and misplaced entries fail.


## Owner approval (2c.2b)

Native admission now requires owner-controlled state-directory records and complete matching
matrix/lifecycle reports, each hashed, qualificationVersion 9, exact sysfs identity and
semantic read-only mixer constraints. The exact typed owner phrase is local approval,
not a cryptographic signature. The harness's visibly non-approved candidate is limited
to its real-engine/backend lifecycle constructor. Production constructors accept no candidate.
The record is checked at session start, after the inert open and before activation, and by
monitors that survive the producer's cancellation through a retiring tail. Replaced/deleted
records and stale health revoke existing DSD permissions; recovery does not play.

The directional edge interpreter remembers tuple and full policy across sessions, including
an initially paused installation before any resume. Missing directional edge approval
refuses an incoming DSD open before Locate and writes; incoming PCM is never gated. Revocation
discards prepared DSD without faulting active PCM, escalates every open retiring DSD sink to
Immediate, and never recreates retired IDs. Constructor overlay tests use the actual approval
reader and backend with fake kernel I/O, not a substituted native admission decision.

Persistent controls exclude card/numid from identity. Unrelated changes/unreadability do not
fail semantic constraints; constrained disappearance/unreadability fails. Descriptor feature
unit evidence and a readable control node are mandatory, including the no-control case.
No DoP packer ships; its tuples stay absent. Sessions retain immutable policies and separately
revocable tokens. Capability snapshots and live open/released/revoked facts carry boot/epoch
identity and remain distinct wire DTOs; GET performs no probes.

ExtractorVersion 29 backfills validated DSF channels and DFF raw/missing/unsupported evidence.
Structural malformation stamps unsupported even after raw evidence; full/delta persistence
and an unchanged no-op scan are tested. The bridge checks actual content before hardware;
iOS Track/QueueItem and pre-DeleteAll/Insert window checks remain I1–I3 companion work.
Runbook: ops/b470-native-qualification.md. Linux execution, ABI and owner qualification remain
separate gates; no hardware was used to implement this change. Single-tuple candidate runs
can evidence native/PCM edges; cross-native tuple transitions remain unapproved without
integrated multi-tuple evidence. The native+DoP release qualification gate remains closed.

- **Native approval is local owner trust, canonical/report hashes, version and descriptor identity; candidates are CLI-only.** Reverify each session/open/activation; semantic mixer drift, replacement and stale health revoke DSD occurrences and retiring tails, never PCM with only queued DSD. Exact directional tuple/policy edges gate incoming opens; keep live occurrence facts separate from capability epochs. No DoP implementation or autoplay recovery. → History-b470-native.md, “Owner approval (2c.2b)”.

### Owner approval review round 2 (2026-10-27)

PCM gapless joins and every PCM open ignore DSD approval and transition edges.
Session admission and session-local revocation cannot publish capability epochs;
only RefreshAuthorization publishes changed effective sets. Mixer constraints filter
policies individually and activation checks the selected policy. Prepared ineligible
DSD is dropped without faulting PCM; monitors still fence open and retiring DSD.
Native paused installation records its predecessor edge and rechecks permission;
gapless installation rechecks both before accepting the successor's bytes.

DFF decoding validates the whole bounded form, skipping payloads by seek. Trailing
PROP bytes, missing odd padding, duplicate PROP after sound and bytes outside FRM8
refuse before hardware. The extractor preserves established format stamps even
when later structure within a PROP makes compression evidence unsupported (version 29);
duplicate-PROP preservation is corrected in version 30 below.

Live facts read sink state and correlate lifecycle notifications by physical output,
so buffering cannot invent a reopen and an old sink cannot release a new observation.
The optional occurrence observer is separate from unchanged PCM hardware notes.
Lifecycle reports bind measured native completion to the exact candidate tuple;
command admission alone is no outcome, and PCM actions cannot count. Edge keys come
from each installed occurrence, including gapless PCM, never an unrelated sink's note.
The production policy reader supplies negative surface fixtures, including actual
revocation after valid approval. Codec words remain protocol-v1 lowercase dsf/dff.
qualificationVersion 9 invalidates version-8 evidence. No hardware qualification,
Linux execution, iOS companion work or DoP implementation is claimed.

### PR #1356 review

Qualification version 10 invalidates version-9 and older reports because guarded
API, config, extraction, output-play and approval code changed. Output-play forwards
OwnsOccurrenceReservations through its playTee wrapper so the real engine hands off
shared reservations; a backend that opts out or has no opt-in still gets none.
Approval and evidence paths must resolve strictly below the state directory, never
to the directory itself, including empty, dot, absolute and symlink spellings.
The ancestor walk also refuses a filesystem root reached before the state root.
That second bound is defensive after the strict-relative-path check.
The overlay generator requires both integration source files and publishes its
mapping only after validating both; a missing backend suite is an error.

ExtractorVersion 30 preserves the first parsed DFF PROP's facts across later PROP
chunks while keeping duplicate compression evidence unsupported. Version-29 rows
are re-extracted; changed rows reach full and delta manifests once. Version 29 is
the documented baseline for DSF channel validation and DFF compression evidence.
PCM playback always serializes logicalRate as zero, with optional policyId, matching
PROTOCOL.md; this is additive protocol v1. Invalid dsdMode diagnostics list the
accepted modes and retain the prohibition on DSD-to-PCM conversion.

ReleaseNative still uses the session-creating lookup: release may arrive before
start and must leave the fence record. Native permission monitoring still revokes
on the first failed check; uncertain authorization is not permission to continue,
and recovery never starts playback. These two review suggestions were declined.
