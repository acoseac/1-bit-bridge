# B470 native qualification — owner approval and provisional evidence

QualificationVersion is **23**. All older evidence must be rerun, native included. The harness never
creates approval. Production native DSD requires a fresh matching approved record;
DoP S32_LE auditions are provisional and require both acknowledgements; production DoP plays only
an approved DoP policy, under `dsdMode: dop` or `auto` with `allowDoP: true` (2d.2, below). Real-ref and Linux hardware gates remain reviewer/owner work.

Use a candidate built from the reviewed tree. No hardware qualification was run during implementation.
Keep every failure, including unsupported formats. The release must eventually have at least one
owner-qualified native path and one DoP path; 2c.1 has no DoP path.

## Inputs and safety

Use the versioned JSON contracts in `internal/player/qualification/manifest-v1.schema.json` and
`internal/player/qualification/contact-v2.schema.json` (a DoP receipt is
`internal/player/qualification/contact-v3.schema.json`).
The manifest has `schemaVersion`, `calibration`, and `fixtures`; fixture field names are
`ID`, `Path`, `SHA256`, `Container`, `Order`, `PCMReference`, `Rate`, `Channels`, `Frames`.
Paths are relative to the asset directory. Hash the actual files, not decoded approximations.
The calibration fields are `reference`, `peakDBFS: -30`, `leadingSilenceMS: 1000`,
`trailingSilenceMS: 1000`, and `attestation` equal to the exact phrase below.
The reference describes the generator/version, waveform, analogue calibration and channel IDs.
This is typed owner attestation, not a cryptographic signature or firmware proof.
At contact the harness prints and requires the owner to type:
`I confirm the matched -30 dBFS listening fixtures and end silences`.

Prepare independent listening assets for each logical rate 2822400, 5644800, 11289600:
DSF LSB-first LR and swapped audible RL, DSF MSB-first LR, DFF LR and reversed CHNL/storage RL.
Include quiet-tail, odd-length and continuous-split pairs. Decoder test goldens are never listening assets.
Each DSD fixture names its matched WAV reference through `PCMReference`; both have identical duration.
Use 8 seconds: 1 s silence, 2 s left 1 kHz, 1 s silence, 2 s right 2 kHz, 2 s silence.
The peaks are -30 dBFS at the documented analogue reference. Include WAV44 and WAV384 for PCM edges.
There must be at least one second of silence at each end. DSD contact playback is at most 10 seconds.
Use separate manifests/references for each PCM rate edge and repeat contact; hashes bind the choice.
Neither a successful open nor a PCM lock establishes that a DAC plays DSD.

Stop competing Volumio playback through its UI. Do not kill or reconfigure MPD.
One contact is required per native format × logical rate × explicit policy, and after every replug,
host reboot or mixer change. Keep headphones OFF the ears. Start at minimum and raise during the
matched PCM reference until just audible off-ear. Type `PCM audible`, then record the knob position.
At that SAME position DSD must be audible and not louder; type `DSD matched` only after listening.
If DSD is clearly louder, the knob has no effect, or there is noise, Stop: gain bypass blocks release.
On-ear audition starts only at the recorded position; never raise the knob during the matrix.
A receipt missing either exact entry is incomplete. Receipts survive separate CLI invocations on the
same live attachment, not host reboot/re-enumeration or changed mixer/manifest/policy.

## Commands on the Pi (Bash; execute one command and inspect its result at a time)

Set B to the candidate and C to a complete bound loopback configuration (`listenAddress:
127.0.0.1:7788`, `output.player.openhome: false`, `dsdMode: off`, `allowDoP: false`).
Serving copies require their own LAN settings; never pass one to dsd-test.
Set Q to persistent evidence storage and install the calibrated assets and manifest there.
The fixtures are positional manifest IDs after all flags; the parser never finds or sweeps tracks.
`--ack-level` accepts exactly `off-ear` or `contact-position`.

```sh
read -r -p 'Candidate binary: ' B
read -r -p 'Loopback config: ' C
read -r -p 'Evidence directory: ' Q
mkdir -p "$Q/reports"
capture() {
  RUN=$(mktemp -d "$Q/reports/run.XXXXXXXX") || return
  "$B" output dac inspect --config "$C" || return
  read -r -p 'Fresh resolved GO bar card: ' CARD || return
  printf '# card=%s\n' "$CARD" > "$RUN/mixer.txt"
  amixer -c "$CARD" contents >> "$RUN/mixer.txt" || return
}
F=DSD_U32_LE
R=2822400
M=kernel
T=10
P=10
capture && "$B" output dsd-test --config "$C" --format "$F" --bit-rate "$R" --pause-method "$M" --tail-ms "$T" --prefill-ms "$P" --manifest "$Q/manifest.json" --fixtures "$Q/assets" --scenario level-check --seconds 60 --ack-level off-ear --mixer-snapshot "$RUN/mixer.txt" --report "$RUN/report.json" dsd64-lr-lsb
CONTACT="$RUN/report.json"
capture && "$B" output dsd-test --config "$C" --format "$F" --bit-rate "$R" --pause-method "$M" --tail-ms "$T" --prefill-ms "$P" --manifest "$Q/manifest.json" --fixtures "$Q/assets" --contact-report "$CONTACT" --scenario matrix --seconds 600 --ack-level contact-position --mixer-snapshot "$RUN/mixer.txt" --report "$RUN/report.json" dsd64-lr-lsb dsd64-rl-lsb dsd64-lr-msb dsd64-lr-dff dsd64-rl-dff
capture && "$B" output dsd-test --config "$C" --format "$F" --bit-rate "$R" --pause-method "$M" --tail-ms "$T" --prefill-ms "$P" --manifest "$Q/manifest.json" --fixtures "$Q/assets" --contact-report "$CONTACT" --scenario interactive --seconds 600 --ack-level contact-position --mixer-snapshot "$RUN/mixer.txt" --report "$RUN/report.json" dsd64-lr-lsb dsd64-rl-dff dsd64-lr-lsb dsd64-rl-dff
```

The example values are explicit candidates, not defaults or recommendations to trust.
Repeat manually for U32_LE, U32_BE, U16_LE and DSD64/128/256, selecting matching manifest IDs.
Repeat with `M=silence` and each proposed T/P combination; no approval may combine unrun values.
All three policy flags are mandatory. Buffer request is 200 ms, capped at 500 ms after readback;
buffer + rounded tail + 500 ms close margin must be less than the 2 s release deadline.
The report includes actual period/buffer geometry, flags, silence frames, descriptor identity,
qualification version, Go/build identity, fixture hash, live controls and the amixer annex.
A locally unversioned build reports its SHA as unverified and cannot be release evidence.

Matrix plays the matched PCM reference, compatible DSD fixture list, then the last matched PCM
reference: PCM tail/prefill are both zero. Repeat with the WAV384 reference manifest to cover the
cross-rate PCM edge. Check actual DSD indicator, channels, pitch, duration, no bursts/hiss or
quiet-boundary clicks, and no silence/reopen at compatible joins. Record waveform-cut clicks separately.
Different native rates/word formats use separate contact/run commands and closed-device boundaries;
the backend-lifecycle command below measures actual engine/backend occurrence accounting.

Keys are read throughout every scenario, including calibration and both PCM references.
On a TTY, playback uses raw input: s stops and q quits immediately, without Return.
Both request Immediate cleanup, skip every later leg, and issue no contact receipt.
Space pauses/resumes native playback; n/p establishes TrackChange before cancelling
the producer. Stop and quit remain live during that tail and drain. Piped keys also work.
At a silent confirmation prompt, enter s or q as a whole line followed by Return;
letters in the required phrases are text. Ctrl-C works immediately in either mode.
There is no Ctrl-C-only calibration exception. Type confirmations only when prompted.
A confirmed Stop reports `End: stopped`; quit reports `End: quit`; both exit 0.
Cleanup failure preserves that End, records Error, exits 1, and never opens a successor.
The seconds limit is wall clock from the first open, across all pauses and track changes;
it never resets. Expiry reports `End: deadline` and stops admission. An active native
sink retains its existing shared release budget (at most 2 s); opener and PCM worker
waits cannot extend the run deadline. Natural EOF uses that same shared release deadline.
Pause for more than 30 seconds; verify the PCM node closes, then explicitly resume. Both DSD pause
methods use 30 s, irrespective of player config. Repeat busy reopen, unplug, Stop during a tail,
and under library scan load. A fault never recovers or autoplays; a failed harness action ends the run.
No successor may open after an unconfirmed close. Replug requires new CARD, mixer capture and contact.
Record `/proc/asound/card$CARD/pcm0p/sub0/{hw_params,status}` using the fresh card, never an old path.

Before contact, capture mixer state, turn the physical knob off-ear, then capture again:
```sh
capture && KNOB_BEFORE="$RUN/mixer.txt"
# Turn the knob off-ear, without playing anything.
capture && KNOB_AFTER="$RUN/mixer.txt"
```
Attach both annexes. The activation fingerprint excludes a control only when its interface
is SNDRV_CTL_ELEM_IFACE_PCM (3) AND its Access has VOLATILE (1<<2). Volatility alone
never excludes a control: every MIXER and CARD control remains constrained. The same
fingerprint binds contact validation, every admission and contact receipt creation.
Reports retain complete Before/After and ControlObservations snapshots, with excluded
controls also listed in ExcludedBefore/ExcludedAfter. Thus a volatile PCM channel map
may change at open without invalidating contact; mixer values, metadata and membership
remain binding. Every PCM reference verifies the opened descriptor's hash and geometry,
decodes that same descriptor, and checks binding/attachment before any write. If the knob
changes a USB control, restore the captured state before DSD activation or start a new contact;
any reference drift/read failure refuses activation. No controls is a readable empty snapshot.
There are no mixer writes. Model, bcdDevice, manufacturer/product and anchored port are read from
sysfs, never entered manually. Descriptor-invisible firmware changes remain a residual risk.

## Reviewer gates, then owner handoff

On each Linux amd64 and arm64 host, with Go 1.27.3 and a writable GOCACHE:
```sh
GOTOOLCHAIN=go1.27.3 scripts/alsa-abi/check.sh
GOTOOLCHAIN=go1.27.3 go test -race -tags=ts_omit_webclient,ts_omit_debugeventbus -timeout 30m ./internal/alsapcm ./internal/alsactl ./internal/alsa ./internal/player/format ./internal/player/qualification ./cmd/bridge
GOTOOLCHAIN=go1.27.3 go test -tags=ts_omit_webclient,ts_omit_debugeventbus ./internal/player/qualification -run '^TestQualificationVersionGate$' -count=1
```
The ABI script now compares the control UAPI too. These Linux execution gates are pending on Darwin.
Keep the report set, omitted tuple names and reasons. 2c.2 must rerun integrated engine/backend
lifecycle on its own build before any approval, with qualificationVersion bumped for guarded changes.
Claims remain software bit preservation, exact opened format, and owner listening qualification.
There is no end-to-end verified-bit-exact badge, no serial requirement, and no automatic approval.


The gate's `native-qualification` job runs real-sink CLI integration on PRs and main.
Its CheckVersion step runs on PRs only: full history, the actual PR head and fetched
origin/main, then merge-base..HEAD. Missing main/history/merge-base fails; it never skips.
The job is a dependency of `gate`. The guarded implementation files are enumerated in
`internal/player/qualification/version_gate.go`: native and DoP packing, the DoP evidence
reader; ALSA UAPI, PCM device,
native device, params, sys_linux/sys_other, ring, both sinks and writers, native opening;
all three dsd-test files; alsactl control/control_linux/uapi; ALSA qualification identity;
qualification inputs and the version checker itself. Changes require a version increase.

The integration overlay exposes the existing ALSA _test.go fake only to this test command.
The injected PCM opener is below descriptor verification and binding admission; production
hardware guards remain intact. After committing this change, rerun the real-ref checker:
```sh
BASE=$(git merge-base origin/main HEAD) && test -n "$BASE"
GOTOOLCHAIN=go1.27.3 go run -tags ts_omit_webclient,ts_omit_debugeventbus ./scripts/qualification-version -base "$BASE" -head HEAD
python3 scripts/native-integration-overlay.py /tmp/native-overlay.json
GOTOOLCHAIN=go1.27.3 go test -race -tags ts_omit_webclient,ts_omit_debugeventbus -overlay /tmp/native-overlay.json ./cmd/bridge -run '^TestNativeIntegration' -timeout 5m -watchdog 10s
```

## Round 3 evidence rules

Historical round 3 used qualificationVersion 4; current version is **23**. A native contact's
Contact.SchemaVersion remains **2**; a DoP contact's is **3**. All older
contacts are invalid; repeat contact on this build. Required Device and Subdevice
fields bind the configured PCM endpoint, including a nonzero subdevice, before
either audition leg opens. Missing fields are not zero. Both opened-node checks
still run before writes. The report also records Device and Subdevice: use those
numbers in the hw_params/status path above instead of assuming pcm0p/sub0.

Every occurrence in the supplied fixture list is required, including repeated
IDs; other manifest entries are not required for that run. Level-check now plays
the entire supplied list. Completion means Position.Played reached each occurrence's
end, not merely that the decoder queued its bytes. Completed records this coverage.
Navigation marks contact/matrix evidence incomplete and stops after release, even
when it races natural completion; only interactive runs permit unrestricted navigation. First-fixture p
is a no-op recorded in IgnoredKeys, without teardown or reopen.

A contact is published only after final attachment equality and equality of the
filtered control fingerprint. Volatile PCM controls remain excluded. Final reads
remain inside the run deadline; any failure or cancellation clears the contact.
Importing a report requires End=complete and an empty Error as well as a valid
nested contact. A raw contact must satisfy the current schema and version too.

Writer faults immediately revoke the sink and establish the shared cleanup deadline;
Activate on a terminal sink refuses immediately. Tail and drain failures report
unconfirmed release; drain receives only remaining deadline time minus the 500 ms
close margin, and never starts without positive time left. Final STATUS failure
prevents completion. Release cancellation and waiters use the same injected clock
in tests. An opener's completion handle covers open, admission and cleanup. The
harness waits within the original cleanup allowance, then exits 1 with unconfirmed
evidence, without final control reads, a successor or a fresh two-second allowance.

Pause cancelling a Resume prefill remains paused and recoverable. A following
Resume keeps the accepted silence prefix, without replaying the cancelled suffix;
a terminal write fault or unconfirmed close ends the sink. The unaligned carry
writer defensively clears its destination bit; reachable carry resets already
clear the storage and cursor together.

Raw prompts echo printable input, accept Backspace/Ctrl-H, and use CRLF. The file
reader uses poll/wakeup, joins on cancellation and preserves caller-owned stdin
and descriptor flags. Injected in-memory readers are nonblocking; other injected readers must explicitly supply
an Interrupt method through an adapter, which owns its cancellation policy. Asset checks use the shared explicit special-file list,
which accepts cloud placeholders; the CLI itself still requires Linux.

The version gate additionally guards binding, ALSA identity, decoder, format,
control, configuration, USB identity and file-reader directories, plus the shared
CLI output/input code and config-path resolver. Overlay generation fails for a missing/changed or residual build tag,
or missing fake support. CI also requires a passing NativeIntegration test line.
Linux amd64/arm64 execution and ABI gates above remain pending on this Darwin host.

## Round 4 harness exit and input rules

Version 4 invalidates version 3 receipts. A successfully opened sink keeps a cleanup
owner through cancellation: Stop/quit exits 0 only after acknowledgement, and cleanup
errors stay failures. A cancelled Finish is not an expired budget; the harness joins
the sink's original release deadline. At expiry it reports unconfirmed and exits 1.
Native opener and PCM worker waits use the injected clock, capped by the remaining
run deadline and two seconds from cancellation; an expired run grants neither a
fresh allowance nor final reads or successors. Active native sink teardown still
uses its existing shared two-second release budget.

Admission and final validation recheck cancellation after reading attachment and
before reading controls, including when the attachment read returns after exit.
Coverage is per occurrence and per sink, limited to accepted content frames and
Played; excess from one sink never completes an unfed successor. At a boundary the
current fixture remains the outgoing one until some content of its successor has
played. Both playback and the completion-time key drain ignore and record
`p:first-fixture` when that fixture is the first, for any list length.

Confirmation input preserves every echoed printable ASCII byte, including spaces;
Backspace/Ctrl-H removes the preceding byte. CR, LF and CRLF each terminate one line,
including a split CRLF arriving at the next prompt. Tabs and non-ASCII input fail
with a visible printable-ASCII diagnostic. Nothing is silently trimmed or discarded.
The version gate also guards the shared CLI filename and platform config-directory
resolver; configpath and host-tree wiring remain guarded.


## 0.2.2 local approval contract

Approval is local trust, not a cryptographic signer. “Signed” means the owner's typed
`I approve these complete matrix and backend-lifecycle reports` confirmation recorded
in `Approval.Body.Confirmation`. The record AND evidence reports live under the bridge
state directory, owned by the bridge user, with no group/other write bits; their enclosing
directories there have the same restrictions. Use 0700 directories and 0600 files.
`output.player.dsdQualification` is relative to that directory (an absolute path must
still resolve inside it). `dsdMode: native` or `auto` enables only approved native tuples.

The Go contracts are `qualification.Approval`, `ApprovalBody`, `ApprovedPolicy`, `Edge`
and `Evidence` in `internal/player/qualification/approval.go`. JSON uses those exact
exported field names. SchemaVersion=1, or 2 with Transitions and QualifiedOn (2d.2, below); QualificationVersion=23; Revision is the owner's
revision; Identity is the observed VID/ProductID/manufacturer/product/bcdDevice,
anchored controller/port, interface, device and subdevice. No serial or card number is
persistent identity. Unchanged descriptors may retain approval after replug/restart;
firmware that hides changes in bcdDevice and indistinguishable hardware remain accepted
residual risks. Environment and ManifestHash must match the reports.

There is exactly one policy per format/logical-rate base tuple, stereo, Ordering=msb,
PauseReleaseSeconds=30 and PCMTimePolicy=[0,0]. Each policy names one complete matrix
report and one complete backend-lifecycle report as `{Path,SHA256}`. SHA256 hashes report
file bytes; Approval.SHA256 hashes `json.Marshal(Approval.Body)` in the declared Go
struct field order, without whitespace/newline (not the source JSON's whitespace).
Recompute using `qualification.Digest(body)` after the owner reviews and confirms it.
Missing fields, invented combinations of pause/tail/prefill, incomplete/cancelled reports,
unverified builds and unexecuted edges refuse. Promotion is a separate reviewed manual
step; neither harness nor production writes, signs, repairs or auto-promotes an approval.

Tampered means wrong owner/mode or body/evidence hash mismatch. Stale means version or
observed identity mismatch. Every session and activation rereads and verifies; a 100 ms
monitor also detects deletion/replacement, health failure and constrained-control drift.
An identical inode replacement still revokes the old permission. Recovery publishes new
capability only, never Play. Descriptors and mixer reads never write controls.

Persistent Constraints match Interface, Device, Name, Index, Channel and Value (raw channel
bytes); Type/count/readability must support that value. Card and ALSA numid are deliberately
absent. Re-enumeration and unrelated value/unreadability changes do not revoke constrained
policies. Missing/unreadable constrained controls refuse. No constraints require descriptor
feature units and mixer controls both absent. This differs from provisional contact's
whole-snapshot fingerprint, which remains attachment-specific. A failed control-node read
revokes because the constraint cannot be verified.

Native edge keys are `native/FORMAT/BITRATE/2/msb/ID/PAUSE/TAIL_MS/PREFILL_MS/30`;
PCM keys are `pcm/FORMAT/RATE/CHANNELS/VALIDBITS/zero`. Edges are directional `{From,To}`,
including self edges; approval of endpoints alone authorizes no edge. Edges must occur in
the lifecycle reports. The backend enforces them on natural advance and navigation,
including separate sessions, before an incoming DSD open or Locate. PCM opens never depend on DSD approval or edges. Unapproved incoming DSD edges remain unavailable.

## Real backend lifecycle and approved production reports

First construct a visibly provisional record in Q: same schema/hash/identity/policies and
complete current matrix evidence, but Confirmation exactly `CLI candidate; not approved`.
Its Lifecycle is empty. It grants nothing to serve, output play or any API. For this command,
one native tuple must match all explicit flags; WAV fixtures exercise PCM boundaries.
At least one native fixture must finish with measured playback of that tuple.
NativeTuple and NativeCompleted record that execution; PCM-only lists or completion
cannot qualify it. LifecycleActions count observed native outcomes: pause/resume,
release/reopen, navigation between native occurrences, Stop, and natural completion.
An admitted command alone is insufficient. Version 23 invalidates older evidence.
List all candidate directional edges explicitly. No command generates that record.

```sh
capture && "$B" output dsd-test --config "$C" --format "$F" --bit-rate "$R" --pause-method "$M" --tail-ms "$T" --prefill-ms "$P" --manifest "$Q/manifest.json" --fixtures "$Q/assets" --contact-report "$CONTACT" --scenario backend-lifecycle --provisional-record "$Q/candidate.json" --seconds 600 --ack-level contact-position --mixer-snapshot "$RUN/mixer.txt" --report "$RUN/report.json" wav44 dsd64-lr-lsb dsd64-rl-dff wav384
```

This uses the real engine and backend. Exercise n, p, space pause, wait for the 30 s release,
space resume, natural completion of every listed fixture, and x Stop. s/q remain immediate
abort keys and produce incomplete evidence. Only measured completion plus all lifecycle
actions can finish the report. A skipped file, fault or deadline cannot approve. Revisit
fixtures with explicit Play after Stop as needed. Copy the reviewed reports under the bridge
state directory before separate owner promotion. Native cross-rate/cross-format edges are
not evidenced by this single-tuple invocation and must remain absent from approval.

For production, set C to the approved native config. Build a contact bundle
`{"Receipts":[{"Contact":CONTACT_OBJECT,"SHA256":"CANONICAL_CONTACT_HASH"}]}` from the
complete contact reports, one receipt per tuple; use `qualification.Digest(contact)`.
Capture a fresh mixer annex. The three flags are an all-or-none report interlock; supplying
them does not grant permission or accept a candidate. The queue remains in a separate
state directory; the approved record is read from the configured bridge state directory.

```sh
capture && "$B" output play --config "$C" --keys --report "$RUN/production.json" --contact-report "$Q/contacts.json" --mixer-snapshot "$RUN/mixer.txt" "$Q/assets/dsd64-lr.dsf" "$Q/assets/reference.wav"
```

The production report records opens/notes and live pre/post controls; it is diagnostic,
never a replacement for the matrix and backend-lifecycle approval evidence. `output play`
has no `--transport` flag; dsd-test's defaults to native, and a DoP audition passes
`--transport dop` (2d.1, below). All dsd-test invocations require valued `--ack-level`,
`--manifest` and positional fixture IDs. iOS I1–I3 (Track migration, QueueItem propagation and whole-window
pre-DeleteAll/Insert eligibility) remain pending companion PRs; the bridge guarantees its
own content/tuple/record gate before hardware writes.

The PR #1356 review fixes the output-play reservation opt-in and rejects approval
or evidence names resolving to the state directory itself before any ancestor
walk. Native monitoring remains fail-closed without a grace period. The overlay
generator requires both integration suites before publishing its overlay.
Guarded API, config, extractor, output-play and approval paths changed; rerun
qualification with version 23. ExtractorVersion 30 retains the first parsed DFF
PROP's known format facts when a later PROP makes compression unsupported.

Version 13 also covers the deterministic PCM trace capture test. It changes no
playback behavior; test paths are included in the qualification version guard.


## B470 2d.1 — provisional DoP, qualification version 14

The software packer supports only S32_LE, 24 significant bits left-aligned in
32: memory `00 b a marker`, chronological `a` then `b`, identical L/R marker,
05/FA alternating per stereo carrier frame. DSD64/128/256 carriers are
176400/352800/705600 Hz. Unknown or less than 24 significant bits refuses before
prefill, including after a pause release. No alternate PCM packing exists. The
decoders still take DSD512, which no transport here carries: the sink refuses it
before opening, and the backend refuses a plan for it at admission
(`rate_not_supported`) before anything is located. In 2d.1 production selection and
publication stayed native-only; 2d.2 (below) adds approved DoP, its integrated reports
and native↔DoP approval.

Use the bound loopback/OpenHome-off config, calibrated owner manifest, persistent
Q and card-prefixed `capture` function above. Every command is one manual audition;
never run a sweep. PCM references must be 24- or 32-bit WAV at the exact carrier,
with equal duration and the manifest's matched −30 dBFS calibration and end
silences. The harness packs them into S32_LE. Software goldens are not listening
assets. Choose R/M/T/P explicitly and repeat contact after every change.

For a mapped knob, first use the contact command as a PCM setup leg: headphones
off ears, establish the just-audible position during PCM, then press Stop (or type
`s` at the PCM confirmation). This ends without DSD and without a receipt. Recapture
controls with `capture`; repeat the complete contact without moving the knob.
The live fingerprint never rebases during an audition. A changed-state experiment
requires this same setup/recapture sequence and both acknowledgements.

```sh
R=2822400; M=kernel; T=10; P=10
capture && "$B" output dsd-test --config "$C" --transport dop --format S32_LE --bit-rate "$R" --pause-method "$M" --tail-ms "$T" --prefill-ms "$P" --seconds 60 --ack-level off-ear --ack-dop-noise --manifest "$Q/manifest.json" --fixtures "$Q/assets" --mixer-snapshot "$RUN/mixer.txt" --report "$RUN/report.json" --scenario level-check dsd64-lr-lsb
CONTACT="$RUN/report.json"
capture && "$B" output dsd-test --config "$C" --transport dop --format S32_LE --bit-rate "$R" --pause-method "$M" --tail-ms "$T" --prefill-ms "$P" --seconds 600 --ack-level contact-position --ack-dop-noise --manifest "$Q/manifest.json" --fixtures "$Q/assets" --contact-report "$CONTACT" --mixer-snapshot "$RUN/mixer.txt" --report "$RUN/report.json" --scenario matrix dsd64-lr-lsb dsd64-rl-lsb dsd64-lr-msb dsd64-lr-dff dsd64-rl-dff
capture && "$B" output dsd-test --config "$C" --transport dop --format S32_LE --bit-rate "$R" --pause-method "$M" --tail-ms "$T" --prefill-ms "$P" --seconds 600 --ack-level contact-position --ack-dop-noise --manifest "$Q/manifest.json" --fixtures "$Q/assets" --contact-report "$CONTACT" --mixer-snapshot "$RUN/mixer.txt" --report "$RUN/report.json" --scenario interactive dsd64-odd dsd64-subbyte dsd64-continuous-split
```

The single contact fixture plus rounded prefill/tail must fit ten seconds; pause
or navigation invalidates contact. Confirm `PCM audible`, the unchanged knob
position, `DSD matched`, `DAC DSD indicator confirmed`, and `L/R pitch and duration
correct` only after observing them. Stop immediately for noise, lost indication,
louder DSD or ineffective gain. PCM carrier lock alone never completes evidence.
DoP contacts use schema 3; DoP reports use schema 2. Native contacts retain schema 2.
Receipts survive CLI exit, but not replug, reboot, changed endpoint, controls,
manifest, policy or qualification version. Both standalone and enveloped receipts
undergo the same validation.

Repeat at 5644800 and 11289600 with their own IDs, same-carrier references, fresh
contacts, pause policies and tail/prefill. Cover odd/sub-byte joins, silence pause,
kernel pause, >30 s release, busy reopen, Stop during tail, replug and mixer drift.
Retained content must restart with marker 05 after close/reopen; a continuous run
must not reset at an occurrence. Compare same-S32/carrier PCM↔DoP and separately
identified cross-rate cases; cross-rate evidence cannot substitute for same-carrier.
The ordinary matrix intentionally refuses cross-rate references; use separate
manifests/contacts for separately reviewed experiments, never relabel their rate.

Where descriptors expose a feature unit, use the existing control UI off-ear to
choose known non-0 dB attenuation, recapture, and repeat contact with
`--mixer-experiment-state nonzero-attenuation`. Restore the chosen state, recapture,
and repeat with `--mixer-experiment-state restored`. Each report preserves the
state and snapshots. Supply `--mixer-experiments "$Q/experiments.json"` to the matrix;
that file is an array of `{ "Path": "reports/run.…/report.json", "SHA256": "…" }`
with paths relative to Q. Candidate/approval readers independently verify hashes,
complete same-tuple contacts, indication/listening, identity, environment, manifest,
stable snapshots and distinct attenuation/restored states. Place candidate evidence
under Q so these references retain their meaning. No-control qualification requires
readable empty controls and descriptor evidence. Never assume feature-unit processing
position or require lock loss as a successful experiment.

Bridge submission traces and disabled ALSA software fill do not prove USB packets
at drain/close boundaries. Owner qualification on the actual kernel remains required;
no hardware was opened by the implementation tests. Their evidence is software only:
every byte the fake kernel was given equals an independent bit-level DoP oracle,
through the sink and through the real engine, backend and decoder. Linux amd64/arm64
runtime and ALSA ABI checks are separate from cross-builds. On the committed branch,
run the real-ref gate:

```sh
git fetch origin main:refs/remotes/origin/main
BASE=$(git merge-base origin/main HEAD)
go run -tags ts_omit_webclient,ts_omit_debugeventbus ./scripts/qualification-version -base "$BASE" -head HEAD
```

Version 15 covers backlog B563's test changes, merged over 2d.1's version 14:
the native and DoP retiring tests run one helper, testRetiring, that holds the
third occurrence until the release, parks the writer's first wait (ParkNextPoll,
nativeLateWriter) and compares every byte the DAC was given; and
TestNativeReleaseEscalation and TestDoPReleaseEscalation wait on the escalation's
seam rather than a second of wall time; and TestNativeIntegrationDoPEnvelope
requires each contact refusal's own line, a bare contact receipt's included,
and a valid contact's exit code. They change no playback behavior either.

Version 16 covers backlog B557, a guarded change in `internal/player/openhome`:
the OpenHome surface tears a retired socket down, its listener and every
connection on it, before its Status stops listing the address (a kept-alive
connection was answered after Status said the address went away), and its
tests hold each retirement at a new seam, `Seams.Retiring`. It changes no
playback behavior, no PCM or DSD path and no wire field.

## B470 2d.2 — approved DoP and auto selection, qualification version 17

Version 17 makes every contact, report, candidate and approved record of version 16
or below stale, native ones included: run the whole sequence below on the 2d.2 build.
Production now plays DSD from one effective set, which selection and the published
epoch both read: `dsdMode: native` plays native policies (DSD_U32_BE, then DSD_U32_LE,
then DSD_U16_LE); `dop` plays approved S32_LE DoP policies, and only with
`allowDoP: true`; `auto` plays native first, and DoP only at a rate the record holds
no native policy for. A native policy whose constraints no longer hold refuses its
rate (`dsd_policy_unavailable`); DoP never plays in its place, and nothing falls back
at run time. `allowDoP: false` under `dop` or `auto` loads: DSD is refused as
`dop_not_allowed` and PCM plays under a PCM-only epoch.

The owner's real-DAC steps, in this order, one command at a time, inspecting each
result before the next:

1. Version-23 native and DoP contacts and matrices, the mixer experiment included
   (the 2c.1 and 2d.1 blocks above, with fresh contacts).
2. Both backend-lifecycles, native and DoP, with same-carrier and cross-rate WAV
   edges (step 1 of this section).
3. Promotion r1 (step 2).
4. The transition scenario (step 3).
5. Promotion r2 (step 4).
6. The five configs, the output-play blocks and serve (step 5).
7. Serve and iOS checks: phone-asleep advance, and no autoplay after stale health or
   a replug.

### 1. The DoP and native backend-lifecycles

Each runs the real engine and backend on its CLI candidate (`Confirmation` exactly
`CLI candidate; not approved`, one policy, its matrix evidence, its candidate edges),
which grants nothing to serve, output play or any API. The DoP lifecycle plays its
candidate under `dsdMode dop`; its WAV fixtures are PCM edges and need no same-carrier
reference, and once the evidence is complete it asks for `DAC DSD indicator confirmed`
and `L/R pitch and duration correct`. The report records the DoP sink's opens as
`Opened`, which DoP evidence checks open by open, and every other open (a PCM edge's)
as `EdgeOpened`; its edges carry the backend's own keys, `dop/S32_LE/…` for DoP.
Exercise space pause past the 30 s release and resume, n, p, x Stop, and natural
completion of every fixture.

```sh
capture && "$B" output dsd-test --config "$C" --transport dop --format S32_LE --bit-rate "$R" --pause-method "$M" --tail-ms "$T" --prefill-ms "$P" --seconds 600 --ack-level contact-position --ack-dop-noise --manifest "$Q/manifest.json" --fixtures "$Q/assets" --contact-report "$CONTACT" --provisional-record "$Q/dop-candidate.json" --mixer-snapshot "$RUN/mixer.txt" --report "$RUN/report.json" --scenario backend-lifecycle wav176 dsd64-lr-lsb dsd64-rl-dff wav44
capture && "$B" output dsd-test --config "$C" --format "$F" --bit-rate "$R" --pause-method "$M" --tail-ms "$T" --prefill-ms "$P" --seconds 600 --ack-level contact-position --manifest "$Q/manifest.json" --fixtures "$Q/assets" --contact-report "$CONTACT" --provisional-record "$Q/candidate.json" --mixer-snapshot "$RUN/mixer.txt" --report "$RUN/report.json" --scenario backend-lifecycle wav44 dsd64-lr-lsb dsd64-rl-dff wav384
```

`wav176` is a PCM reference at DoP64's carrier (176.4 kHz, 24 or 32 bits): the
same-carrier edge, which must close and reopen the device, never join. `wav44` is the
cross-rate edge. Repeat for each DoP rate and each native policy to be approved.

### 2. Promotion r1

Build `approved.json` by hand from the reviewed reports, as the 0.2.2 contract above
describes: the native and DoP policies, each with its matrix and backend-lifecycle
evidence, and `Edges` listing only edges a lifecycle report passed (its `PassedEdges`).
Schema 1 still reads; schema 2 adds `QualifiedOn`, the owner's qualification date as
`YYYY-MM-DD`, which `/v1/outputs` shows beside the DAC's model and `bcdDevice`. A
schema-1 record carrying `Transitions` or `QualifiedOn` is refused. Recompute the
record's `SHA256` with `qualification.Digest(body)`.

### 3. The transition scenario

`approved-native-dop-native` plays an approved native policy, then an approved DoP
policy, then the native one again. It reads both endpoints from the owner-approved
record through `ReadApproval` (never a candidate): `--native-tuple-id` names the
native policy by its ID, whose rate the record states, and the DoP flags name the
DoP policy by its tuple. Before anything opens or the report is written it refuses a
record that does not read as approved or was approved on another manifest or
environment, an unknown native ID, DoP flags no policy matches, other than three DSD
fixtures (native, DoP, native, each at its policy's rate) and a contact bundle
without a fresh receipt for each endpoint's tuple. Only the two native↔DoP edges
under test are admitted, here alone; the self edges must already be approved. Run
natural transitions, then n and p across the transports, x Stop, and play every
fixture to its end; the run then asks for the DoP indication and listening. The
report names its endpoints by ID and policy digest (`Endpoints`) and the record it
read (`Qualification`). Rates may match or differ.

```sh
read -r -p 'Approved native policy ID: ' NATIVE_ID
capture && "$B" output dsd-test --config "$C" --qualification "$Q/approved.json" --native-tuple-id "$NATIVE_ID" --transport dop --format S32_LE --bit-rate "$R" --pause-method "$M" --tail-ms "$T" --prefill-ms "$P" --seconds 600 --ack-level contact-position --ack-dop-noise --manifest "$Q/manifest.json" --fixtures "$Q/assets" --contact-report "$Q/contacts.json" --mixer-snapshot "$RUN/mixer.txt" --report "$RUN/report.json" --scenario approved-native-dop-native dsd64-lr-lsb dsd64-rl-dff dsd64-lr-lsb
```

### 4. Promotion r2

A native↔DoP edge is approved only by a transition report in schema 2's
`Transitions`: `{Path, SHA256}` of the report's bytes, under the state directory. The
reader checks that report against both endpoint policies by their digests (a policy
that differs by a byte refuses), under DoP's schema, observations and geometry and
both policies' constraints, with every fixture completed and natural, next, previous
and Stop observed; it counts its two cross edges as executed only if the report
passed them. Write r2 as schema 2 with a new `Revision`, the transition report in
`Transitions`, and the two cross edges added to `Edges`; recompute `SHA256`. An edge
no report executed is refused, so approving the endpoints alone allows no transition.

### 5. The five configs, the output-play blocks and serve

Copy the whole bound loopback config C for each file below, replacing only these
`output.player` keys (absolute paths under Q). C stays the only config passed to
dsd-test. A serving copy needs its own LAN `listenAddress`, a loopback admin
address, `output.player.openhome: true`, a free `openhomePort` and its own dataDir
under Q; run one serving copy at a time.

| Config under Q | dsdMode | allowDoP | dsdQualification |
|---|---|---|---|
| native.yaml | native | false | Q/native-approved.json |
| dop.yaml | dop | true | Q/approved.json |
| auto.yaml | auto | true | Q/approved.json |
| auto-fallback.yaml | auto | true | Q/auto-fallback-approved.json |
| auto-no-dop.yaml | auto | false | Q/auto-fallback-approved.json |

`native-approved.json` holds the native policies alone; `auto-fallback-approved.json`
is a subset of `approved.json` with every DSD64 native policy removed and DoP64 kept,
so auto chooses DoP there and only there. `auto-no-dop` must refuse its DSD as
`dop_not_allowed` with zero DSD writes, prefill included, while PCM plays. The
contact bundle `$Q/contacts.json` holds a fresh receipt for every DSD tuple used.

```sh
capture && "$B" output play --config "$Q/native.yaml" --keys --contact-report "$Q/contacts.json" --mixer-snapshot "$RUN/mixer.txt" --report "$RUN/report.json" "$Q/assets/pcm44.wav" "$Q/assets/dsd64-lr-lsb.dsf" "$Q/assets/dsd64-rl.dff" "$Q/assets/pcm44.wav"
capture && "$B" output play --config "$Q/auto.yaml" --keys --contact-report "$Q/contacts.json" --mixer-snapshot "$RUN/mixer.txt" --report "$RUN/report.json" "$Q/assets/pcm44.wav" "$Q/assets/dsd64-lr.dff" "$Q/assets/pcm88.wav"
capture && "$B" output play --config "$Q/dop.yaml" --keys --contact-report "$Q/contacts.json" --mixer-snapshot "$RUN/mixer.txt" --report "$RUN/report.json" "$Q/assets/pcm176.wav" "$Q/assets/dsd64-lr-lsb.dsf" "$Q/assets/pcm44.wav" "$Q/assets/dsd64-rl.dff" "$Q/assets/pcm88.wav"
capture && "$B" output play --config "$Q/auto-fallback.yaml" --keys --contact-report "$Q/contacts.json" --mixer-snapshot "$RUN/mixer.txt" --report "$RUN/report.json" "$Q/assets/pcm44.wav" "$Q/assets/dsd64-lr.dff" "$Q/assets/pcm88.wav"
capture && "$B" output play --config "$Q/auto-no-dop.yaml" --contact-report "$Q/contacts.json" --mixer-snapshot "$RUN/mixer.txt" --report "$RUN/report.json" "$Q/assets/dsd64-lr.dff"
```

The dop.yaml queue covers PCM176→DoP64 (same carrier: close and reopen, never a join)
and PCM44→DoP64 (cross-rate). Auto must choose native where a native policy is
eligible; Stop interrupts each tail, and nothing opens before a release is
acknowledged. For serve, choose one full LAN config, record its logs and the iOS
checks of step 7 of the owner's list; `GET /v1/outputs` then shows each tuple's
`transport` (`dop`, absent for native), the observation's `transport`,
`packingVersion`, `observedAt` and `qualificationRevision`, and the epoch's `model`,
`bcdDevice` and `qualifiedOn`, never the USB port or serial.

```sh
S="$Q/auto.yaml"
capture && "$B" serve --config "$S"
```

### 6. Pruning edges from subset records

A subset record (`native-approved.json`, `auto-fallback-approved.json`) keeps only
policies of `approved.json`, byte for byte, with their evidence. Remove every edge
whose `From` or `To` names a policy the subset dropped, and every transition report
whose two endpoints are not both kept; an edge must still be passed by a report of a
kept policy or by a kept transition report, or the reader refuses it as unexecuted.
Give the subset its own `Revision`, recompute its `SHA256`, and never add an edge or a
policy the full record does not hold.

Linux amd64/arm64 execution and the `alsa-abi` job remain pending on the Darwin host
this was built on; no hardware was opened.

Version 18 covers backlog B576, test files under the guarded
`internal/player/engine/` prefix (main's B470 2d.2 took 17 after this branch
was cut): the engine fake's contract check, that every session the backend saw
had its Start, now runs once every goroutine the engine started has been joined
(`fakeBackend.verify`, which `newFakeBackend` registers), where it ran before
the join and took a Start still on its way for one never made; and
`TestTheContractCheckWaitsForAStartStillOnItsWay` holds such a Start through
the teardown at two new test seams (`fakeBackend.beforeStart`,
`harness.joining`). It changes no playback behavior, no PCM or DSD path and no
wire field.

Version 19 covers backlog B579, test files under the guarded
`internal/player/engine/` prefix: `TestPCMLookaheadHasNoNativeBudget`'s first
lookahead read now waits for the Prepare that names the successor
(`fakeSession.successor`), where it took the first Prepare after the
occurrence, the end's, named while repeat was off, and panicked on its nil
when the successor's had not reached the fake yet; a held row keeps that
Prepare short of the fake until the read waits for it, at two new test seams
(`fakeBackend.beforePrepare`, `fakeBackend.waiting`). It changes no playback
behavior, no PCM or DSD path and no wire field.

Version 20 covers backlog B510 (backlog B579, acoseac/1-bit-bridge#1363, takes
19 and was not merged when this branch was cut), under the guarded
`internal/alsa/` and `internal/player/binding/` prefixes: a sysfs reading says
whether its USB listing named every controller (`SysfsSnapshot.USBComplete`),
and under a port binding Resolve answers a controller missing from a listing
that was not read whole as `identity_incomplete`, never a topology change, and
takes a card whose port path could not be read for unknown, never for a card
at another port: `device_ambiguous` beside the bound unit's card,
`identity_incomplete` with no card at the port, and the check after open
refuses alike. It changes which binding state a port binding answers on such a
reading (GET /v1/outputs' player element may carry `identity_incomplete` or
`device_ambiguous` where it carried `device_missing` or `available`), and no
playback path, PCM or DSD, and no wire field or schema; existing `reason`
values may now differ as described.

Version 21 covers backlog B605, test files under the guarded
`internal/player/engine/` prefix (B601, acoseac/1-bit-bridge#1371, and B604,
#1369, raise the version too; their order is set at merge): four tests no
longer move the engine's fake clock across a call or a step still on its way.
`TestNativePausedTransitionRearmsDeadline` and `TestShutdownJoinsWithinItsBound`
wait for the owner to take the session's Prepare answer before the clock passes
its deadline, or the shutdown's grace, and each gains a row that holds that
answer until then; `TestAParkedEditIsAnsweredWithinReserveTimeoutOfItsPark`
waits for the step that arms the reservation bound, and
`TestAFailingReservationWriteRefusesEdits` for its session's Prepare. The two
native deadline tests read their checkpoint behind a session event, never a
command's reply, and require the paused session's own release unconfirmed,
still paused. It changes no playback behavior, no PCM or DSD path and no wire
field.

Version 22 covers backlog B604, the 0.2.2 logging audit
(acoseac/1-bit-bridge#1369; B605, #1372, took 21 on main first; backlog B601,
acoseac/1-bit-bridge#1371, also raises the version and was not merged when
this was written, so its merge raises it to 23). It changes guarded files in
what a log line says and what a test expects of one, and nothing else. Under
`internal/manifest/`, the
MP4 walks' seven warnings name the track library-relative (`trackLogPath`)
and the walk's error without the absolute path its `*fs.PathError` names
(`walkErrReason`), which is now `fsutil.ErrorWithoutPath` under the guarded
`internal/fsutil/` prefix, with `extract_log_paths_test.go` and
`errpath_test.go` beside them. Under `cmd/bridge/serve_player`,
`TestServeSaysOnceWhenAnAddressOverrideClashesWithOpenHome` expects the
`component=player` the player's logger binds, now that the test logger keeps
what a derived logger binds. It changes no playback behavior, no PCM or DSD
path, no binding state and no wire field.

Version 23 covers backlog B601, the console's Player page
(acoseac/1-bit-bridge#1371, which took 21 on its branch; B605, #1372, took 21
and B604, #1369, 22 on main first, so its merge raises it to 23), under the
guarded `cmd/bridge/serve_player` prefix: serve's player keeps its library
resolver (`bridgePlayer.res`, `serve_player.go`) so the page names the track
heard from the library's row, and the page's view of the player
(`serve_player_console.go`) reads the boot plan, GET /v1/outputs' facts, the
engine's snapshot and the box's cards, and lists them for a choice of DAC. It
calls a transport playable only while the live config withholds nothing and
the player's output is available (`outputOpen`), and it answers the console's
writes, in their config turn, whether the player sounds and whether the change
withholds its output (`Sounding`, `Withholds`). `serve_player_console_test.go`,
`serve_player_console_serve_test.go` and `serve_player_test.go` test them.
With them, outside the guarded prefixes, the resolver names that row through
its own index (`library.Resolver.StoredPath`, in `internal/player/library/`,
which opens nothing). The page writes `output.player.enabled` and
`output.player.device` through the live config holder, which serve's player
already followed (a change withholds the running player's output, as before);
it offers no DSD control. It changes no playback path, PCM or DSD, no wire
field or schema, and no approval record's shape.
