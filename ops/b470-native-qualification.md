# B470 native qualification — owner approval and provisional evidence

QualificationVersion is **10**. All older evidence must be rerun. The harness never
creates approval. Production native DSD requires a fresh matching approved record;
DoP remains unimplemented. Real-ref and Linux hardware gates remain reviewer/owner work.

Use a candidate built from the reviewed tree. No hardware qualification was run during implementation.
Keep every failure, including unsupported formats. The release must eventually have at least one
owner-qualified native path and one DoP path; 2c.1 has no DoP path.

## Inputs and safety

Use the versioned JSON contracts in `internal/player/qualification/manifest-v1.schema.json` and
`internal/player/qualification/contact-v2.schema.json`.
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
`internal/player/qualification/version_gate.go`: native packing; ALSA UAPI, PCM device,
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

Historical round 3 used qualificationVersion 4; current version is **10** and Contact.SchemaVersion remains **2**. All older
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
exported field names. SchemaVersion=1; QualificationVersion=10; Revision is the owner's
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
An admitted command alone is insufficient. Version 10 invalidates older evidence.
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
never a replacement for the matrix and backend-lifecycle approval evidence. No `--transport`
flag exists. All dsd-test invocations require valued `--ack-level`, `--manifest` and
positional fixture IDs. iOS I1–I3 (Track migration, QueueItem propagation and whole-window
pre-DeleteAll/Insert eligibility) remain pending companion PRs; the bridge guarantees its
own content/tuple/record gate before hardware writes.

The PR #1356 review fixes the output-play reservation opt-in and rejects approval
or evidence names resolving to the state directory itself before any ancestor
walk. Native monitoring remains fail-closed without a grace period. The overlay
generator requires both integration suites before publishing its overlay.
Guarded API, config, extractor, output-play and approval paths changed; rerun
qualification with version 10. ExtractorVersion 30 retains the first parsed DFF
PROP's known format facts when a later PROP makes compression unsupported.
