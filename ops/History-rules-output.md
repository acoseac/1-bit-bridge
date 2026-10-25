# History — the box as an audio output

## The ALSA reader, the MPD client and `bridge output`

### ALSA scalar values use the value grammar, not the dotted-key grammar (2026-10-23, B516)

**Keep integers, reals and strings distinct; failed numeric conversions are
opaque strings, optional `=` is accepted, and comments/strings are bytes.
Refuse a leading-dot value; continue a key at a dot after blanks/comments.
Never turn an unmodeled value into a resolved chain, lose a known softvol
or route warning, or weaken hooks/include guards at any token start.**

`lexPCM` distinguishes a definition's key from its value. In keys a dot
separates components even after blanks or `#` comments: `slave .pcm`,
`ttable.0 .1` and `pcm.x # c\n.y` are `slave.pcm`, `ttable.0.1` and
`pcm.x.y`. `parse_def` decides on the character `get_nonwhite` returns.
Within values dots belong to the scalar (`0.5`, a library filename), but
`get_string` refuses a leading dot before `get_freestring` or `parse_value`
runs: in a value after `=` and in an array element. After a key with no
`=`, a dot continues the key: `defaults.value .5 x` is the key
`defaults.value.5` with the value `x`. Braces hold
definitions and brackets hold values; an iterative stack bounds nesting
by the input, without recursion.
The key grammar stays restricted. Value tokens stop at ALSA's blanks or
`= , ; { } [ ] ' " \ #`. One optional `=` after a key precedes a scalar,
compound or array, including `pcm.x = { type = hw card = 2 }` and
PipeWire's `libs.native = /usr/lib/… ;`. A missing value or a repeated `=`
still refuses the file. Includes are not read, at keys, values or bracket
elements. The existing quote/backslash escape subset stays unchanged,
with either quote character delimiting a value. Comments and string values
are byte-oriented: Latin-1 text does not invalidate the file. NUL still
refuses it, and keys retain their restricted grammar.

The basis is alsa-lib v1.2.14 `src/conf.c`'s `get_string`, `get_freestring`,
`parse_def` and `parse_value`, with `safe_strtoll`/`safe_strtod`: a bare token beginning
with a digit or minus is tried as a base-zero signed integer, then a real,
consuming the entire token. **If both conversions fail, parse_value falls
through to STRING, never a parse error.** This corrects round 1's false
claim and refusal tests. `2chMaster`, `0.5garbage`, `-5dB`, `--5`, `1e`,
`1_000`, `0x`, `-`, `1e9999` and `1e-9999` are consumed as opaque strings.
Other admitted bare words, including paths and leading-plus words, are
strings; quoted numbers stay strings. Go-only numeric underscores and
binary/octal prefixes stay strings too. Hex and octal integers retain
their C numeric value; reals have their own token kind. Overflow and
underflow to zero fall back to strings. The session that ran the task
checked the reviews' reading of alsa-lib; the task, offline, fetched and
ran nothing of it, and libc/locale numeric edges remain unverified
(nonzero subnormals, NaN payloads and newer libc prefixes).

A real or newly admitted string is consumed but remains opaque for
resolution. A numeric `type` or `slave.pcm` is not a string reference, and
a negative integer card is not a card ID. Negative devices stay opaque
through the existing sign check; `subdevice` stays an unmodeled field.
Repeated types remain uncertain even when the second value is newly
accepted. Unknown fields keep their PCM opaque. `SampleStage` still names
the first known sample-changing stage through empty/plug stages, including
softvol with min/max dB and plug with a real transfer-table gain; it never
walks through that stage. The verdict is unchanged: these are `alsa_conf`
warnings beside format information, and bit exactness remains
`not_verifiable`. The Volumio capture still resolves through empty, plug
and hw. No wire shape changes.

Seven synthetic `testdata/asound-values/*.conf` files cover a real gain,
negative real, negative integer, PipeWire's assigned library path, dotted
library word, failed numeric conversions and Latin-1 bytes.
`TestPCMValueFixturesKeepResolutionAndStages` resolves the unaffected PCM
and exact chain and keeps the sample stage while refusing its chain;
`TestPCMValueFixturesKeepWarnings` loads those same files through the real
verdict. `TestPCMScalarKindsFollowALSA`,
`TestPCMValueKindsDoNotGrantResolution`, `TestPCMFailedNumbersAreOpaqueStrings`,
`TestPCMOptionalEquals`, `TestPCMByteStringsAndComments`,
`TestPCMSpacedDottedKeys`, `TestPCMSpacedDottedRouteKeepsWarning`,
`TestPCMValuesStillRefuseUnsafeFiles` and `TestPCMValueDelimiters` pin kinds,
opacity, assignments, byte strings, refusals and boundaries.
`FuzzPCMConfigValues` covers the tokens plus hooks and includes in key,
value and array positions; no PCM-file fuzz target preceded B516.

The original five fixtures failed against 3f805199's parser. Round 2's
numeric-string, optional-equals and byte-string tests fail against round
1's parser; the include and negative-device/subdevice guards already
held, with new mutation controls pinning them. The author's sandbox had
Go 1.27.1 while go.mod requires 1.27.2, so its own runs used a
scratch-only harness which omits unrelated sysfs/FIFO tests. In the
repository, on Go 1.27.2, `go vet`, `go test` and `-race` on
`internal/alsa` and `internal/signalpath` and `make docs-check` passed for
both rounds, and `FuzzPCMConfigValues` for round 2.

## B514 — the player's startup refusals (2026-10-23)

The output rules load beside player.md for engine changes. Startup refusal
wording belongs to the player's durable reservation rule: an overlay subtree
bind threatens copy-up durability at a crash even on persistent storage.
ErrOverlayBind distinguishes it from reboot-volatile media while retaining
ErrMediumUnknown compatibility. The output state machine and wire are
unchanged; player.md and History-rules-player.md carry the operative rule.


### The reading keeps the configured MPD file and adds a command-line hint (2026-10-24, B515, round 3)

**Never substitute an argument or inferred default for the configured file:
its config, warnings, song mapping and no-live-dop veto stay together. Keep
per-card Tgid/output-thread, stable-start, argv[0] and root checks; parse
MPD's grammar and emit no raw argv. Compare file identities physically in
the bridge's view and never read the operand's contents. Do not read process
environ or infer default paths. Redact every external string against every
config read, including includes, and output.password.**

Round 3 replaces rounds 1–2's unshipped provenance design. The reviews
found that a private bind mount over `/etc/mpd.conf` can leave the same root
inode; argv can be rewritten after loading; `/proc/self/cwd/...` resolves
in the bridge; PID reuse can precede the first stat; MPD falls back from an
unset or non-directory HOME to `getpwuid(getuid())->pw_dir`; and an unreadable
but different argument file could leave the configured file displayed as a
fallback. Linux cannot prove the file MPD loaded through these observations.
Signal Path therefore describes the bridge's configured file and limits the
new field to a hint about the command line observed now.

`mpd.configHint` is optional and additive: `argument` is `same`, `different`,
`none` or `no_config`. Only `different` has `path`, a validated, redacted
logical operand. `same` compares physical file identity, including symlinks
and hard links, in the bridge's view. A file operand without available
identities, a non-file or an out-of-root operand omits the hint; unreadable
contents alone do not. Neither `none` nor `no_config` suppresses the configured
file. No process environment is read and no MPD default is inferred.

Only this card's unique MPD Tgid and matching enabled output thread may
supply a hint. Stat field 22 must agree around the bounded command-line
read; argv[0]'s basename is `mpd`; root links compare by SameFile, with root
mount device and root fields as fallback only when links cannot be inspected.
These are observations, not proof of startup identity or mount equivalence.
Cmdline is capped at 64 KiB with overflow detection, stat at 64 KiB and
mountinfo at 1 MiB. The configured file is published from the config step
alone; after all verdict-bearing inputs (including the song row and
stream-open observation), hints run in their own single-outstanding bounded
step with their own busy flag, bounded by 500 ms and the reading time left. A failed or late hint omits only hints. A failed or late
config step publishes neither config nor hints. A cached view performs no I/O and retains
a separate hint for each card.

MPD 0.24.4 CommandLine.cxx and OptionParser.cxx define options anywhere,
hidden flags, long `=value`, rejection of `--`, and at most one file operand.
`--no-config` returns before checking remaining operands. Relative paths,
backslashes, empty/dot/dotdot components and unmodeled grammar omit the hint;
no raw argv or process environment reaches the response or logs. Every
external string, including hint paths, DAC mixer/control, kernel names,
song rows and MPD fields, uses the configured file's collected secrets
(includes too) and output.password. The argument file contributes no bytes.

The production reading and handler tests cover all four values, file identity,
identity and grammar refusals, per-card caching, unreadable files, bounded
publication, configured warnings and DoP, and privacy. The exact red/green,
controls and gates are in the task's `report-r3.md` and `controls-r3/`.

### Linked MPD files keep the named directory for includes (2026-10-24, B515)

**An include is relative to the named path's directory, bounded, and not a
glob. Concatenate the unresolved named directory and include name; resolve
symlinks and `..` physically for the open, root check and cycle key at every
level. Never lexically clean a link/.. pair or resolve the named parent
early. Never weaken root checks or optional-include refusals.**

MPD 0.24.4 `src/config/File.cxx`, read in round 2, opens the named path and
passes `path.GetDirectoryName()` to the parser. Includes use
`AllocatedPath::Apply(directory, name)` and recurse with that named path.
For `/etc/mpd.conf` linked to `/opt/mpd/mpd.conf`, `include "x.conf"` still
names `/etc/x.conf`. But for `/etc/linked/child.conf` with `linked` pointing
to `/opt/mpd/sub`, `include "../x.conf"` opens `/opt/mpd/x.conf`. A lexical
clean would wrongly open `/etc/x.conf`.

Round 1 resolved the main named parent early, used filepath.Join/Clean
for includes, and used filepath.Dir for child parents; those inconsistent
rules are replaced by unresolved string concatenation and a lexical
substring for the parent at every level. EvalSymlinks walks components
before applying `..`. The physical path supplies confinement, the open and
the cycle key; the unresolved name supplies only the next include base.
A missing optional path is checked through its nearest existing physical
parent and then ignored as missing; outside-root targets remain refusals.
Round 3 also classifies physical resolution failure at a dangling link as
not found, so plain includes report absence and optional includes skip it.
The resolution error's missing component is checked against its physical
parent before absence is accepted; a dangling target outside the root stays
a refusal. Resolution failures never emit the host path.
Existing depth, file-count, read, non-file and pattern bounds remain.

`TestIncludesResolveDotDotPhysicallyAtEveryLevel` covers directory and file
links for main and nested files, with further nested includes.
`TestLinkedConfigIncludesUseTheNamedDirectory` and
`TestLinkedConfigIncludesKeepConfinementAndCycleChecks` retain the round-1
simple-link, cycle and outside-root controls.


### B515 round 4: named reopen, independent hints and confined identities

The production inspection resolves an explicit file for its display path.
The reading reopens `output.conf` by the configured name, preserving MPD's
named include directory through inspection and the later reread. Nested
include diagnostics use the resolved cycle key against the canonical root;
the unresolved named path still supplies the next include directory.

Hints use `conf.StatHost`: physical resolution and the conf reader's root
check, followed by a stat beneath an opened root. An unresolved or escaped
identity omits the hint, and no operand contents are read. Process-relative
names under `/proc`, `/dev/fd`, `/dev/stdin`, `/dev/stdout` or `/dev/stderr`
omit hints at every operand prefix or symlink target, even if the final file
is ordinary. The walk uses Lstat/Readlink one link at a time, refuses failed
resolution and more than 40 hops, and preserves relative link/.. semantics.
Absolute links retain the conf reader's host semantics and confinement; the
named root and canonical root are both accepted for in-root absolute links. A hint worker that times out or panics cannot erase the already
published configured file, nor block fresh configured reads on the next
reading. One hint worker may remain outstanding; later readings do not
start a second. The configured block's no-live-dop fallback is read once
through `namedBlock`, retaining both yes and no behavior.

`TestReadingKeepsInspectNamedConfig`, `TestReadingOmitsProcessRelativeConfigHint`,
`TestReadingHintOmissionEvidence`, `TestHungHintDoesNotBlockLaterConfigReads`,
`TestFailedHintKeepsConfiguredFile`,
`TestReadingHintRefusesAbsoluteLinkOutsideInjectedRoot`,
`TestNestedIncludeLocationsUnderLinkedRoot` and
`TestConfiguredDoPVetoWithoutLiveAttribute` pin these paths. The task's
`report-r4.md` and `controls-r4/` record the red runs, restored controls,
main comparison and gates.


### B515 round 5: hints wait for verdict inputs and omit intermediate aliases

The hint previously ran before song lookup on the shared three-second reading
context. A stalled hint could consume the remaining time and turn a known
source into `withheld/source_unknown`. Config acquisition, song lookup and
the timestamped stream-open observation now all precede the optional hint.
Its separate 500 ms step uses only the remaining reading time; timeout, panic
or an outstanding worker can change hint presence alone.

Checking only the written operand and fully resolved endpoint missed chains
such as `/etc/alias.conf -> /proc/self/cwd/mpd.conf` and descriptor aliases
whose endpoint is a regular file. `hintOperandHost` checks every traversed
prefix before Lstat and every absolute link target before following it;
relative targets are traversed component by component, never cleaned across
a link/.. pair. `/proc`, `/dev/fd`, `/dev/stdin`, `/dev/stdout` and `/dev/stderr`
all cause omission. More than 40 links, missing paths and confinement failures
also omit hints. The final identity still comes from `conf.StatHost` under
its opened root, without reading operand contents.

`TestStalledHintCannotSpendSongLookupTime`,
`TestReadingOmitsIntermediateProcessAliases` and `TestReadingHintLinkHopLimit`
failed on round 4 and pass on the fix. The Linux-only
`TestHintOmitsLiveProcessAliasesAtHostRoot` holds a regular file open and
uses real `/proc/self/fd`, `/dev/fd` and `/proc/self/cwd` aliases under root `/`;
its runtime result must come from Linux, not the macOS cross-compile.


### B515 round 6: root boundaries, valid links and informational limits

An absolute link targeting exactly the injected root is inside it, under either
its named or canonical spelling. A relative link target can climb to canonical
root `/` and stay there on further `..` components, as the kernel and
EvalSymlinks do. Every other injected root still refuses a parent step with
no component left. Link targets remain uncleaned until preceding links have
been followed. Forty symlink hops retain a hint; forty-one omit it. An in-root
absolute link and a relative `dir-link/../x` chain retain a hint too.

The process-relative exclusion is by name only: procfs mounted elsewhere,
such as `/host/proc` or another `mount -t proc` location, is not recognised by
filesystem type. No statfs check is made. The final confined `conf.StatHost`
also resolves the pathname again after the walk. A file replaced by a link
between those operations can still yield a hint, including through a
process-relative alias whose final endpoint is ordinary. Confinement holds,
but the alias exclusion is not an atomic guarantee across the two resolutions.
These are limits of an informational hint, already documented as not proof
of what MPD loaded; neither changes the configured file or verdict inputs.

`TestHintParentAtHostRootStaysAtRoot` pins host-root saturation against the
conf reader's resolution and refusal at an injected root.
`TestReadingHintLinkToInjectedRoot`, `TestReadingHintFortyLinks` and
`TestReadingHintValidLinkChains` exercise the production hint path, including
a decoy configured file that would turn a lexically cleaned link/.. chain
into the wrong identity. `TestReadingHintLinkHopLimit` pins forty-one hops.
The host-root case is POSIX-only; the other cases run on every OS with symlink
support. Round-6 controls restore each rejection, lower the limit to 39,
refuse all links, and clean a relative target early; each fails its intended
assertion. Exact gates and controls are in `report-r6.md` and `controls-r6/`.

## B470 PR 5b — GET /v1/outputs lists the bridge's own player (2026-10-25)

The record is in `ops/engineering-log.md`, 2026-10-25, B470 PR 5b. Each
heading below is the long form of rule lines that point here: `output.md`'s
five bullets on the route and its element, `cli-serve.md`'s line on serve's
view of its player, and `player.md`'s line on the last play attempt (that
file's budget holds one line). `output.md` loads with
`cmd/bridge/serve_player_output*.go` since this change, where the element's
words and identity are read.

### Two gates, never one: MPD's, and the player's

**Ask the MPD gate first and on every request, wherever it was asked
before (`/v1/health`, `GET /v1/outputs`), and only then combine it with the
player's: never `bridgeOn || Active(ctx)`. Never start the loopback route,
show the checklist, open a reading or let `bridge doctor` print on the
player's gate.**

Until 5b one boolean, `localOutputs.Active`, did four jobs: the health flag,
the loopback track route's start (`OnActive`) and its per-request answer
(`activeNow`), the player checklist, and `bridge doctor`. Widening it to the
player would have started the loopback route and shown an MPD checklist that
fails on a box with no MPD. So it stays exactly as it was, now named the MPD
gate (`mpdOn`), and the player has a gate of its own (`bridgeOn`):
`api.BridgeOutput.Enabled`, which serve's `bridgeOutput` answers from the boot
plan's `lists` (`planServePlayer`): `config.PlayerEnabled` on the boot snapshot
(enabled, a block that reads and is valid, Linux, not public, no OpenHome port
clash) and serve's data-directory lock held. It reads nothing and probes,
dials and starts nothing, and a live change of `output.player` does not move
it: the running player reports a live disable as `disabled` through the
output report it already has.

A short-circuit is the defect this rule exists for: with the player on,
`bridgeOn || Active(ctx)` never asks MPD's gate, so `OnActive` does not run
for an MPD that appears after boot and `auto`'s recorded answer, the one the
loopback route reads, goes stale. The boot call has already run `OnActive`
once, so a test that reads `activeNow` with `on` alone passes either way:
`TestTheMPDGateIsAskedOnEveryRequestWhateverThePlayer` counts `OnActive`
(six for three health answers and three bodies with the player on), and flips
`auto` from a hit to a miss through each route, asserting the route's answer
follows. Swapping the operands at health (control A1) and reading the gate's
last answer instead of asking it at the route (A2) each go red there.

`/v1/health`: `localOutputs` while either gate holds; `bridgeOutput` exactly
while the player's does (a client tells a bridge older than 5b from one whose
player has no identity yet by it: wire.md's rule to name a feature flag);
`outputReading` exactly while MPD's does, since the reading probes and dials
MPD for any well-formed id. `GET /v1/outputs` is `404
local_outputs_not_supported` only while both are shut. With MPD's gate shut
its builder is never entered: no session, dial, `status`, LAN greeting,
`find`, `mpd.conf` read or `ProveRoots`. With `off` or off Linux nothing is
probed; with `auto`, the gate's own probe (`answerAuto`) runs as it did, and
after a miss nothing more. `TestTheOutputsRouteHasTwoGates` is the matrix:
MPD off, on, auto with a hit, auto with a miss and off Linux, each with the
player on and off, the three keys, the status, and the probe, dial, greeting,
track-list, card and `OnActive` counts. `TestThePlanSaysWhetherGETOutputsListsThePlayer`
maps the player's conditions (enabled, disabled, public, off Linux, no DAC,
the lock, a pause release out of range, an OpenHome port clash) onto
`Enabled`. `TestServeListsItsPlayerOnAGateOfItsOwn` boots serve with the player
and `output.mpd: off`: the keys, the element, no loopback route, no checklist.

### The element of kind bridge, and the identity it never makes

**List the element only while the player's gate holds and the player has an
identity; never make, write or cache the identity: read the started player's
own once it runs, and the file, afresh, before that or with no engine. Never
list it without serve's lock: the file is then another serve's player's.**

The identity is `<dataDir>/player-uuid`, made by the player's first start or
by `bridge output dac bind`, both under every gate and the lock (player.md).
The route's view (`bridgeOutput.identity`) reads `bridgePlayer.id`, set before
`ready` closes, once the player runs, and `config.ReadPlayerUUID` otherwise,
on every request: a miss is never kept (a start that makes the file is seen at
once), nor a value (a start that makes it and has its engine refuse the
storage removes it with `ForgetPlayerUUID`, and the next answer lists
nothing). `TestThePlayersElementAcrossItsStart` walks missing, made, ready
and closed, and made, refused and removed, asking from the start's own seam;
`TestTheOutputsRouteMakesNoPlayerFile` compares the data directory entry by
entry around GET /v1/outputs, with no identity and with one. With no element
the answer is `200` with `outputs: []` while the MPD gate is shut.

`internal/api` imports no `internal/player` package: the handler builds the
element from `BridgeOutputFacts`, plain facts `cmd/bridge` fills, so no
player type reaches the wire by an encoding of its own.
`TestTheAPIImportsNoPlayerPackage` reads every source file of the package,
whatever its build constraint. (The package depends on the engine through
`internal/backup`, as it did before this change: the rule is about what it
imports.)

The element: `id` `bridge:` and the UUID; `kind` `bridge`; `player` `bridge`
and `version.ServerVersion`; no `mpd`; `dac` `null` (not reported yet: the
bound card's `/proc/asound` stream, matched by ALSA id and `usbId` and never
by index, is a later change, and `alsa.USBIdentity` is never serialised);
`pathRules` `[]`, an exception to "one element per library root". The
`bridge` object is `json:"bridge,omitempty"` beside `mpd`'s, so a kind `mpd`
element's bytes are main's (`TestAnMPDElementsBytesAreMains`, against bytes
captured from 9e7b2675). Its keys are every one present: `openHome`, `state`,
`reason` (null outside `unavailable`, which the handler enforces), `lastError`,
`libraryScope` `self`, `fixedVolume` (the boot snapshot's `EffectiveMixer` is
`none`), `codecs` (never null), `dsdMode` and `allowDoP` (the boot snapshot's
`EffectiveDSDMode` and `AllowsDoP`). `TestTheBridgesOwnElement` pins every key
and value.

### One composer, a fresh slice, the MPD half as it was

**Compose the body in `composeOutputs` alone: MPD's elements as its builder
answers them, then the player's element built afresh, in a slice of its own.
Never append into the cached body, and never put the player's element behind
MPD's cache, flight or deadline.**

The route and `Server.LocalOutputsResponse` (the console's checklist, both its
panel and its doctor) call one composer, so the console and the app read one
body (`TestTheServersOutputsBodyIsTheRoutesWithThePlayersElement`); the
one-shot builder (`api.NewLocalOutputs`, `bridge doctor`) has no player and
stays MPD's. MPD's half keeps its flight, its 500 ms body cache, its probe
cache and its path-rule cache unchanged. The composer copies MPD's elements
into `make([]OutputInfo, 0, len(mpd)+1)` and appends the player's: the cached
`Response` shares its slice with every answer the cache serves, so an append
into it, with room to spare, would write one request's element into another's
answer and into the cache. `TestTwoRequestsInTheCacheWindowLeaveTheCachedBodyAlone`
runs two requests at once inside the window under `-race`, over the builder's
own body and over a cached body given room to grow; the in-place append
(control A7) is a data race and an element left in the cached array. An MPD
failure with both gates on keeps today's `500`, the player's element with it
(`TestAnMPDFailureWithBothGatesOnIsStill500`).

### State, reason and lastError: a closed list

**Build them from one engine snapshot per request, in this order: the plan's
refusal, the start's outcome (`ready` read before `eng`), the engine closed
(`Snapshot`, then `Done`, then `Closed`), the output (not known, or its
report's word), maintenance, an unconfirmed release, then the transport.
Never parse free text, never read `playerOutput`, and never send a word
PROTOCOL.md's bridge section does not name.**

The plan's refusals are words set where `planServePlayer` refuses
(`not_configured`, `config_invalid`); the start's are `starting` (`ready`
open) and `start_failed` (`ready` closed with no engine: a recorded failure,
a supervisor panic recovered, which closes `ready` and records nothing, or
serve's stop during the start); `closed` an engine whose shutdown began or
whose Run ended. The output's reason is passed on when it is one of
`outputReasons` (every `binding.State` but `available`, and serve's
`binding_changed`), else `output_unavailable`. Maintenance is `unavailable`
here, where 5a3's OpenHome mapping reads it as Stopped; a starting session is
`playing`, where 5a3 has Buffering; a release reads by its pending intent,
none `stopped`. `TestThePlayersStateFromOneSnapshot` is the table over the
snapshot's half; `TestThePlayersElementThroughItsPlan` and
`TestThePlayersElementAcrossItsStart` the plan's and the start's, the panic
and the cancellation included; `TestServeListsItsPlayerOnAGateOfItsOwn` and
`TestTheElementSaysAReleaseIsUnconfirmedUntilItsLateAcknowledgement` the real
engine's (stopped, a busy start's `lastError`, playing, paused, maintenance,
a live disable, closed; the release's deadline and its late acknowledgement).
`TestEveryOutputReasonIsPassedOn` enumerates `binding.States`, which
`TestStatesIsEveryStateDeclared` holds to the source, and
`TestEveryBridgeWordIsInTheProtocol` requires every word, backticked, inside
PROTOCOL.md's bridge section (control B15 deletes the `closed` row, a word
found elsewhere in the file, and goes red).

### The last play attempt, and a busy device by its sentinel

**Never derive `lastError` from `LastStop`: a start that succeeds stops
nothing and leaves it. The engine records each Start and Resume whose answer
it takes as the last attempt (`Snapshot.LastAttempt`), busy only when the
error wraps `engine.ErrDeviceBusy`, which the backend maps from alsapcm's
busy reason, at a start and at a resume's reopen; never from playback's
`busy` code or any error's text, and the engine never imports alsapcm.**

An `Attempt` carries a sequence number, failed and busy; a later attempt
replaces it whichever way it went, so a busy start, a retry that succeeds and
a pause read `null` from the retry on, and a busy resume after the pause let
the DAC go is ended by the next resume that succeeds, or by a later failure
for another reason. A Start past its deadline is a failed attempt, not a busy
one. The backend's `busy` wraps the alsapcm error beside the sentinel
(`busyError`, its text the error's), at `session.start`'s `%w` and in
`resumeError`'s `Err`. The engine's attempt tests (`attempt_test.go`) run on
the fake backend; `playback_busy_test.go` runs the real backend over the fake
kernel's EBUSY, through the real wrap chain, for a start and for a resume
after the release, and a decoy whose detail says busy; a `%w` turned `%v` at
the start (E3) or the resume (E4) goes red. The engine imports neither ALSA
package (`TestTheEngineImportsNoALSAPackage`, every source file whatever its
build constraint), so its darwin and windows builds are as they were.

### The codecs, one source

**List the containers `playback.PlayedContainers` names, which the open's
`dsd_not_in_this_release` refusal reads (`Plays`); never type them twice, and
leave openhome's playable-extension list alone (an extension is not a
container: `.m4a` is ALAC or AAC).**

`format.Containers` is every container the decoders reconstruct from and
`Container.IsDSD` the DSD ones, each pinned to the source and cross-checked on
every golden against the decoder the content opens
(`TestContainersIsEveryContainerDeclared`,
`TestADecoderGivesDSDExactlyForADSDContainer`). `Plays` is a container's
question alone; content of a container it takes can still be refused (float
PCM, a rate, a depth, a transport the DAC does not take), and a client's
check covers scope and container only until `dac` ships.
`TestTheContainersThePlayerPlaysAreTheOnesItsOpenTakes` opens real content of
every container through the backend's own open; `TestTheCodecsAreTheManifestsCodecsThePlayerPlays`
holds the client's mapping (the manifest's `codec`: `FLAC`, `WAV`, `AIFF`,
`ALAC`) to the scanner's codec and the decoder's container on real files, an
AAC `.m4a` among them, and to PROTOCOL.md's table.

### The Source's endpoints, from its LOCATION's own function

**Build `openHome.endpoints` from `openhome.Authority`, the function every
LOCATION is built from (`sourceLocation`), over the running surface read
under the player's `mu`; never from the live config. List no Source while
the boot snapshot's opt-in is off, the surface lists no address, or its stop
has begun.**

A client correlates a discovered Source with the element only when its UDN
matches and its LOCATION's host and port are one of `endpoints`: SSDP carries
the UDN in clear. `Authority` writes a URL's authority (an IPv6 zone as
`%25`, an IPv4-mapped address unmapped), which today's inline LOCATION
(`"http://" + l.Bound.String()`) did not do for a zoned address: it did not
parse. The surface binds IPv4 alone, so nothing advertised changes.
`TestAuthorityIsHowALocationNamesItsSocket`,
`TestEveryLocationTheSourceAdvertisesIsOneOfItsEndpoints` (every LOCATION in
alive notifications and search replies, against `Status.Endpoints`, and none
once `Refuse` has run) and `TestServeNamesItsOpenHomeSourceWhileItListens`
pin it.

### Serve's view of its player

**Install the view before the API listeners start, whether or not the plan
starts the player; read `ready` before `eng` and `id`, the surface's pointer
under `p.mu`, and the identity file afresh until the engine runs. Never read
`playerOutput`, make or cache the identity, or let the view start, probe MPD,
dial or write anything.**

`newBridgeOutput(plan, player, cfg)` is installed with
`api.Server.WithBridgeOutput` right after `startServePlayer` returns, long
before the listeners. It holds the boot plan, the `bridgePlayer` and the boot
snapshot's fixed fields (`EffectiveMixer` `none`, `EffectiveDSDMode`,
`AllowsDoP`); `Enabled` is `plan.lists`. `bridgePlayer.id` is set with `eng`
and `be` before `ready` closes, so once `ready` reads closed both are the
start's own; before that the view reads neither, and with no engine after it
reads the file. `playerOutput` (`p.out`) is the supervisor's alone, as its
doc says, with no lock for another reader: the view reads the output from
the engine's snapshot instead. The surface pointer is written once, by
`startOpenHome` under `p.mu`, and never cleared, so after the stop the view
reads `Status().Stopped` and lists no Source; `Status` is the surface's own
locked read. `TestThePlayersElementAcrossItsStart` asks from the
start's own seam at each step (missing, made, ready, closed; made, refused,
removed; a recorded failure; a supervisor panic; serve's stop during the
start; no lock), `TestTheOutputsRouteMakesNoPlayerFile` compares the data
directory around the route, and `TestServeListsItsPlayerOnAGateOfItsOwn` and
`TestServeNamesItsOpenHomeSourceWhileItListens` run serve itself (control
B17 removes the wiring).

B543 adds a bounded hardware read for the boot binding's DAC, with at most
one in flight per view; it reads neither MPD nor the supervisor's state.

### What stays MPD's

`bridge doctor`'s checklist (the one-shot builder, on the MPD gate, never
reading the player's identity: `TestBridgeDoctorStaysMPDs`), the console's
checklist (`outputChecklistSource.Active` is the MPD gate, and
`checklistInput` reads kind `mpd` elements alone:
`TestTheChecklistReadsNoBridgeElement`, with a bridge element carrying a DAC
as a later change will), the loopback track route (`OnActive`, `activeNow`)
and the reading (a `bridge:` id is `404 output_not_found`, with no probe or
dial: `TestABridgeIDNamesNoReading`). The admin page's JavaScript reads the
checklist's lines, never an `OutputInfo`.

## The bridge player's DAC projection (B543, iteration 2)

The bridge player's `dac` is resolved from its boot USB binding, never an old
card index or ALSA id. Require an available player and a non-stale watcher
snapshot of that binding. The snapshot has no stream altsets, so
`readPlayerDACObserved` brackets `Observer.ObserveWithProc` with an earlier
sysfs identity read. The resolved card must have been that same unit
(`binding.SameUnit`) at that same card index and readable, nonzero `DevNum`
before `/proc` was read. An identical-model swap or re-enumeration during the
read yields null. A swap completed before the bracket still resolves normally.
`Observer.AfterProc` is an instance test seam, nil in production, that injects
exactly the swap after `/proc` and before the final identity observation.

Only the bound PCM device's whole playback altsets are projected. Empty
`pcmBitDepths`, `nativeDSDRates` and `dsdFormats` are normalized to `[]` in the
projection even though the ALSA helper currently already returns non-nil
slices. No identity or longname crosses the seam. The bridge's `name` and `id`
replace known serial and anchored port substrings with `[redacted]`:
`Serial.Redact` does the serial replacement without exposing its raw value.
MPD's projection remains unchanged; `isMPDsOutput` stays null for bridge.

Concurrent callers share one flight, guarded by a mutex held only to select
or complete it. Each caller waits within its own 500 ms bound; the shared
reader has a separate 500 ms context detached from caller cancellation.
A timeout ends a wait, never the slot: a stalled kernel read still owns it.
Completion, including panic, clears the slot and closes a broadcast channel
with the result; a read past its deadline publishes null. Results are not
cached after completion. Never start a second reader while one is stalled,
let a caller cancel the shared read, or keep the slot after its reader returns.

`TestBridgeDACIdentityStableAcrossProc` pins a mid-read identical-unit swap and
a changed enumeration. `TestBridgeDACRedactsIdentityInsideNameAndID` puts both
private strings inside each public label. `TestBridgeDACCapabilityListsAreArrays`
checks PCM-only and DSD-only encoded bodies. The stalled/panic recovery tests
require a subsequent fixture DAC, and `TestBridgeDACUnavailableAndStale` proves
availability again immediately before making the snapshot stale.
`TestBridgeDACConcurrentCallersShareRead` and
`TestBridgeDACCallerCancellationKeepsSharedRead` pin overlapping waits and
independent caller deadlines; the stalled test counts one reader after fifty
callers exhaust their bounds. Tests use `testing/synctest` for deterministic
holds and virtual deadlines, without wall-clock sleeps or sockets.
