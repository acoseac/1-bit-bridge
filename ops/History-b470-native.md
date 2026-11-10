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

- **Owner approval (2c.2b) is the harness's alone since B728; production rechecks the effective policy, the opened identity and DSD-only revocation as it rechecked approval.** Since B728's B7 (the cutover) this section is the engineering harness's admission alone: production (serve, and `bridge output play` without `--report`) decides DSD by the effective policy over the player's DSD state and reads no record or candidate. It rechecks the policy as this section rechecks the record, at a session's first DSD occurrence, every activation and on a 100 ms monitor, the opened node's identity at every open, and revokes DSD alone, never PCM. **Don't read an approval record in serve.** `TestServeReadsNoApprovalRecord`, `TestProductionConfiguresNoApprovalBesideItsPolicy` and `TestDoPCandidateRefusedByServeAndOutputPlay` pin it.

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

### iOS mirror precision

PROTOCOL.md enumerates baseline and optional bridge members, every nested member's
JSON type and omission rule, exact native/PCM format, pause and observation state
vocabularies, and unknown-token handling. DSD policy reason is display text;
logicalRate is always present, zero for PCM. Manifest evidence and channel absence
are explicit. TestDSDOutputProtocolTokens compares the protocol tables in both
directions with parsed producer definitions and the native format filter.
TestExportIOSManifestFixtures scans synthetic DSF/DFF/WAV through extraction,
SQLite and full/since streaming. TestExportIOSOutputFixtures exercises the real
HTTP handler without sockets for six independent synthetic players, collected
with the real OutputsResponse serializer into one response fixture (a live
bridge lists only its own player). Both build and assert their documents on ordinary test runs; only writes
to the export directory require BRIDGE_EXPORT_IOS_FIXTURES. Export manifest then
API with -p=1: the manifest creates fixtures-info.json and the API adds its input
hashes and output-index cases (since B728's B8, `cmd/bridge`'s exporter third:
"The read side (B728 B8)" below). BRIDGE_FIXTURE_COMMIT supplies provenance only
when git rev-parse HEAD is unavailable. The commit is the base revision; the
protocol hash and explicit working-tree label avoid claiming uncommitted changes
belong to it. Fixture files are consumer artifacts, not hardware qualification.
Qualification version 11 supersedes 10 because the guard includes test paths
under internal/manifest; no playback or extraction implementation changed.

### Final delta review

Both fixture exporters keep extraction/HTTP serialization assertions active without
BRIDGE_EXPORT_IOS_FIXTURES, Git or HEAD. Only publication requires provenance;
the manifest exporter resolves Git/BRIDGE_FIXTURE_COMMIT inside its export gate.
The API exporter's provenance-file dependency was already export-gated.
TestDSDOutputProtocolMembers compares raw HTTP JSON against PROTOCOL.md's member
table: exact member sets, JSON types, required zero/empty values, optional fields
absent when empty and present when set, absent objects and non-null empty arrays.
A DTO decode round trip alone cannot detect omitted mandatory zero values.
Qualification version 12 supersedes 11 because manifest/qualification tests match
the path guard. TestQualificationRunbookVersion pins all five current runbook
version references to Version; historical evidence records retain their versions.
No playback/extraction implementation or wire contract changed.

### The read side (B728 B8)

The long form of `wire.md`'s bullets on DSD capability, `logicalRate` and the
fixture exporters as B728's B8 (2026-11-07) changed them; the record is in
`ops/engineering-log.md` under that date, and the element's read side is
`output.md`'s bullet on it, whose long form is in `History-rules-output.md`,
"One read of the backend's facts, the set's own view".

**DSD capability is `bridge.dsdPlayback`'s effective set, its native and DoP
tuples and its `converted` rates, under its boot and epoch, never passive DAC
arrays; `bridge.dsdDAC` is the bound DAC's choice and evidence;
`bridge.playback` is the distinct current occurrence observation. Don't
publish a rate the player would not play, grant from `dsdDAC`, `dsdMode` or
`allowDoP`, or put a serial or a port in a DAC key.** The design's section
9.1 item 12, B8's half: its write route, `PUT /v1/outputs/{id}/dsd`, the one
way to change the choice, is B9's, which completes the bullet. A client
grants DSD at a rate from a stereo tuple or a converted entry of the
published set at that rate (PROTOCOL.md's eligibility), the set being what
selection admits by. B11 converted and published no converted rate, and
three exclusions kept it so, which B8 removes: `bridgeDSDFacts` sent no
`converted`, `TestDSDOutputProtocolTokens` excluded `TransportConverted` from
`playback.transport`'s words, and PROTOCOL.md's `playback.transport` row did
not list `converted`. `dsdDAC`, `dsdMode` and `allowDoP` describe the DAC
(its choice, what each mode offers, the evidence and the first use), which
the app shows and B9's write changes; they grant nothing, and a client reads
none of them to decide what to send. A `dacKey` names a model, never a unit:
the model key's `DACKey`, its product string with the unit's serial and port,
every spelling of the port, taken out first (`wireKey`, `playback.PortForms`). Two members went: `qualificationRevision`, on
`dsdPlayback` and `playback`, and `qualifiedOn`, on `dsdPlayback`, carried an
owner's approved record's revision and qualification date, which no
production path reads since B7's cutover: serve had sent neither since (B7's
`TestOutputsPublishTheEffectiveSet` held their absence on `dsdPlayback`, and
production's observation had no record to name), and only the harness's
admission, on no route, stated them. B8 takes them out of the DTOs,
PROTOCOL.md's member table and the fixtures, and the member table's guard
asserts each absent (`TestDSDOutputProtocolMembers`'s removal rows); in every
other way the change is additive, `ProtocolVersion` 1. The iOS mirror removes
them with its copy of PROTOCOL.md and its DTOs. `TestDSDOutputProtocolMembers`,
`TestDSDOutputProtocolTokens`, `TestTheBridgeElementCarriesTheDACChoice`,
`TestAConvertedEntryCarriesItsPlan`, `TestNoDACKeyOrModelCarriesASerialOrAPort`
and `TestEachKindOfDACsWholeResponseIsItsGolden` pin it.

**`bridge.playback.logicalRate` is always present: zero for PCM and for DSD
the player converts, logical bits/s for native DSD and DoP.** A converted
occurrence's `format`, `rateHz` and `validBits` are its PCM's, its plan's,
and `converted.fromBitRate` carries its DSD rate, never `logicalRate`
(`TestAConvertedEntryCarriesItsPlan`, the golden `converted_playing`).

**Export the manifest, the API and then `cmd/bridge` packages sequentially
(`-p=1`) with `BRIDGE_EXPORT_IOS_FIXTURES`; never hand-author their JSON, and
rewrite the read side's goldens only with `BRIDGE_WRITE_DSD_GOLDENS`.** The
manifest's exporter (`TestExportIOSManifestFixtures`) makes
`fixtures-info.json`, keeping from an earlier run the sections the others
write (`outputRecords`, `dacRecords`); the API's
(`TestExportIOSOutputFixtures`) writes `dsd_outputs.json` and
`outputRecords`; `cmd/bridge`'s (`TestExportIOSBridgeDSDFixtures`) writes
`dsd_dac_outputs.json`, the read side's six kinds of DAC from the real
backend through serve's view and the route, the goldens' cases, and
`dacRecords`, each kind's facts with their hash and its golden's path. Each
builds and asserts its documents on every run, and only publication waits on
the variable (`TestIOSBridgeDSDFixturesWithoutProvenance`, with no export
directory, no Git and no HEAD). The token guard (`dsdReadSideTokens`) reads
each new vocabulary from the definitions that emit it, never a second list:
the evidence ladder (`policy.Rung`), the dither words and the filter name
(`dsdconv`), the refusal words (the policy's, from the functions that decide
a rate, and the backend's for a set it could not compute), the warning codes
and reasons, the modes and the transports (`dsdstate`), the modes an option
is listed for (`optionModes`), the caveat codes and the transports the lint
lets a caveat name (`dacprofile`), the gain check's words that block DoP and
first use's states.

## Native lifecycle fences and containment — 2026-11-10

A queue deletion or cut can reach a successor after its output was adopted but
before activation completed. The backend takes that occurrence back only for
`errReplanned` or `ErrActivationFenced`, closes its content and sink, and continues
with the replacement. A device refusal or unconfirmed close remains a fault.
Release wins over take-back. Reconciliation records `native_revoked` before it
fences an activating output, while the occurrence is still claimed; delayed
`Revoke` cannot lose or replace that fault after take-back closes the output.
The same claim covers the first occurrence through the activation answer: a
successful activation followed by revocation still fails Start before workers run.
Converted output is kept unwritten by the producer's first fault check.

`nativeAnswer` distinguishes a command's own failure, held refusal, success,
fence, and request never run. After the sink's life ends, `verdict` reads that
answer together with a fresh `nativeRecord`: device and cleanup failures retain
their error identities but cannot match cancellation or the activation fence.
Successful requests remain successful only after confirmed fault-free cleanup;
other clean stops carry the named command's typed cancellation. The release
budget is established once, including when its caller abandons a wait. A reply
sent before writer completion is not proof of completion: `Done` owns that proof.
The writer's final close failure reaches both its outcome and pending requests.

An occurrence admitted under an older published set waits on the session's
`judged` channel before opening its own output, resuming between transports, or
joining its predecessor. Reconciliation and revocation each notify judgement.
A gapless join waits only above the sink's measured 100 ms floor; at the floor it
drains for an honest transport gap. A converted join window ends zero-padded at
the floor. Positions use logical bits for native DSD, PCM frames otherwise.

`refreshDSD` contains the first policy-read or publish panic and returns
`errDSDUnread`; every session holding DSD, including a retiring native tail, is
stopped immediately. A session reconciliation contains its own judgement,
hand-off, and revocation callbacks; its first failure stops that session while
other sessions receive their verdict. There is no retry counter or grace period.
Containment does not call a failed Revoke again or read positions to discover
which outputs need stopping. Native outputs begin Immediate release; PCM outputs
are dropped through counted calls. Teardown still joins real writer completion.
An output remembers its first position-callback panic, retaining only its last
measured prefix and an error thereafter. Cleanup never calls that observer again;
a failed final report cannot strand teardown after the outputs have closed.

The portable fake-kernel suites cover activation/admission/prefill deletions,
release and failure precedence, first and successor activation, converted output,
revocation before and during take-back, all native command verdict/wait arms,
re-read records, watcher-first and writer-first endings, fixed release deadlines,
and reconciliation waits at resume, transport, gapless and converted-window
boundaries. Hotplug capability changes run through the real refresh and
reconciliation; ordering tests retain the barriers before delayed Revoke. The
floor tests check the 350/400 ms measurement boundary and compare the converted
window's samples with an independently converted zero-padded ending.
Qualification version 48 invalidates earlier harness evidence; LegacyVersion
remains 35. Test and negative-control evidence is in `ops/engineering-log.md`.
