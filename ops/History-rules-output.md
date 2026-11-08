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

- **The loopback track route serves this bridge's own tracks to the MPD beside it, on 127.0.0.1 and ::1 and no other address, while `output.mpd` resolves on** (B473 B2c, decision 2; the MPD gate alone: the bridge's own player's gate, B470 PR 5b, starts and answers nothing here). It is the DLNA listener's file route, `/dlna/file/{trackID}`, the same track id and the same serving half (`serveOpenedFile`), on a listener of its own (`dlna.LoopbackServer`), whether or not the DLNA server is enabled, with no SSDP, ContentDirectory, artwork, silence, rendition, routed track, listing or authentication. The port is `output.loopbackPort`, 7791 unless set: MPD keeps the URLs in its queue across a bridge restart, so it is fixed; 0 is a kernel pick for tests. ::1 is optional and the base URL names 127.0.0.1. Serve starts it the first time the localOutputs gate answers true (asked once at boot, and `OnActive` on every later true answer, for an MPD that appears after boot), binds once (a taken port is one Warn and `StartErr` for the checklist), and stops it before the store closes. **Every request in between asks whether `output.mpd` still resolves on** (`LoopbackConfig.Enabled`, which serve wires to `api.Server.LocalOutputsActiveNow`): `off` refuses at once, `on` serves, and `auto` follows the gate's most recent answer, whatever its age or the configuration it was for, so the listener stays up and a request is refused with `404 not_serving`. `Active` records auto's answer under its lock every time it has a verdict (`answerAuto`): from a probe it stored, a probe whose inspection returned past its own deadline and was not stored, the cache, or another caller's probe it waited on. A probe whose inspection was abandoned (it never returned, one still ran, or it panicked) is no verdict, number 0 like a caller that stopped waiting, and records nothing (B473 B3 round 3). Each verdict is numbered under the lock as it is produced (a cache hit at its read, a probe at its completion, the number saved into the flight before `done` closes so every waiter carries it), and a caller records one only when its number is newer than the recorded one's, and is answered with the newest recorded verdict: a caller that carried an older verdict out of the lock never overwrites a newer one (the tests park a caller between its verdict and its record with `beforeRecord`). A caller that stopped waiting on another caller's probe has no verdict of its own (number 0): it is answered with the gate's last answer and records nothing, so a request that ends while a probe runs cannot turn the route off. The route's answer is a read of that record (`localOutputs.activeNow`, false before the first) that never probes, dials or greets MPD, never runs `OnActive`, and never waits on MPD: every holder of the lock releases it before any I/O. A nil `Enabled` serves nothing, and with no output reader wired every reader of the config reads the empty one (`outputConfig`). `Stop` closes a stream still running when its context ends: `http.Server.Shutdown` returns at the context and leaves an active connection open. Before it says whether it serves, the handler refuses a remote address that is not loopback whatever the bind (`loopback_only`), a Host other than `127.0.0.1`, `[::1]` or `localhost` with any port or none (`loopback_host_only`: a page that re-pointed its own name at 127.0.0.1, DNS rebinding, reaches the route from loopback under that name; an empty Host too), and a request that carries `Forwarded`, `X-Forwarded-For`, `X-Forwarded-Host` or `X-Real-IP`, even empty (`forwarded_request`). A proxy on the host that strips all four and forwards to the port is the owner's own configuration, which nothing in the request tells from MPD. It then refuses anything but GET and HEAD of exactly one id (a segment below it, a rendition included, is `not_found`), and a routed track. A file is served only when its links resolved (`fsutil.ResolveLinks`) lie inside a configured root (`fsutil.IsUnderAny`) before the open, the path resolved again after the open is still inside, and the file that opened is the file `os.Root` reaches below that root (`sameFileInRoot`, which follows a link only while it stays inside and never an absolute one). A path that does not begin with the root byte for byte is refused. A configured root's own path and its ancestors are trusted as the owner's (a root that is a link is followed wherever it points); the route defends everything below a root. Refusals are the route's typed JSON (`loopback_only`, `loopback_host_only`, `forwarded_request`, `not_serving`, `method_not_allowed`, `not_found`, `not_a_file`, `outside_library`, `internal`), not `/v1` codes, so PROTOCOL.md's error table does not list them. Its server's `ErrorLog` is `handshakelog.ErrorLog()`, so net/http's own lines keep no client address: a recovered panic named the client it was serving until B604, as the OpenHome surface's and serve's servers already did not (`TestALoopbackRoutePanicIsLoggedWithoutItsClient`). **Don't serve a rendition, a routed track, a listing or a second route there, leave its server's `ErrorLog` nil, drop the remote-address check for a loopback bind, decide whether it serves only at listener start or from the stored probe over the gate's last answer, let an older verdict overwrite a newer, take an abandoned inspection for a verdict, let a request dial MPD to decide, answer a non-loopback Host or a forwarded request, open before the first containment check, or confirm the opened file by `os.Stat`, which follows a link swapped in after the check.** Each window is reproduced by parking the request in it (the handler's `beforeOpen`, `afterOpen`, `beforeConfirm`). `TestLoopbackRouteRefusesARemoteThatIsNotLoopback`, `TestLoopbackRouteServesOnlyWhileOutputIsOn`, `TestTheLoopbackRouteAsksTheGateWithoutDialling`, `TestTheRouteFollowsAnExpiredProbeThatFoundMPD`, `TestTheRouteStopsOnAnExpiredMissAfterATrue`, `TestActiveNowIsTheGatesLastAnswer`, `TestACallerThatStopsWaitingGetsTheGatesLastAnswer`, `TestAnInspectionThatNeverReturnsIsNoVerdict`, `TestActiveNowReadsTheConfigLikeTheProbe`, `TestAnOlderCacheVerdictNeverOverwritesANewerOne`, `TestAWaiterNeverRecordsAnOlderVerdict`, `TestServeAsksTheGateOnEveryLoopbackRequest`, `TestLoopbackRouteAnswersOnlyALoopbackHost`, `TestLoopbackRouteRefusesAForwardedRequest`, `TestLoopbackRouteServesTheFileUnaltered`, `TestLoopbackRouteConfinesToTheLibraryRoots`, `TestLoopbackRouteRefusesALinkSwappedAfterTheCheck`, `TestLoopbackRouteRefusesADirectorySwappedAfterTheCheck`, `TestLoopbackRouteRefusesADirectorySwappedBackAfterTheOpen`, `TestLoopbackRouteRefusesASwapInEveryWindow`, `TestLoopbackRouteListsNothing`, `TestLoopbackRouteServesNoVariantAndNoRoutedTrack`, `TestLoopbackRouteRefusesOtherMethods`, `TestLoopbackRouteRefusesWhatIsNotAFile`, `TestLoopbackServerBindsOnlyLoopback`, `TestLoopbackServerStopsWithTheServer`, `TestOutputLoopbackPort`, `TestOutputsCarryTheLoopbackBaseURL`, `TestServeServesItsTracksOnLoopbackWhileOutputIsOn` and `TestServeStartsNoLoopbackRouteWhileOutputIsOff` pin it.

- **`GET /v1/outputs/{id}/reading` is one reading of the box, taken between two reads of what MPD reports, cut per output and served for 500 ms; every step has a deadline, every step that reads files runs under its own, and a reading that cannot be had is an honest `unknown`** (B473 B3, decision 4; rounds 2 and 3). The feature key is `outputReading`, on the MPD gate alone, asked once per health answer for `localOutputs` and `outputReading` whatever the bridge's own player's gate says (B470 PR 5b: the reading probes and dials MPD for any well-formed id, so a bridge whose `localOutputs` is its player's alone has no reading, and a `bridge:` id is `404 output_not_found` with no probe or dial, `TestABridgeIDNamesNoReading`); main carried B2's route from 2026-10-19, so a bridge with `localOutputs` and no reading route exists. One reading at a time (`readingSnapshot`), on a context detached from its first caller under `limits.build` (3 s), reused for `outputCacheTTL`, its flight completed in a defer however it ends; `ageMs` says how old an answer is, and nothing reads between calls: there is no sampler. What one reading found of each output's PCM device (its stream by subdevice, `hw_params` and owner, and the start the kernel stamped on its run, `trigger_time`, for a run the reading can tell; or none open) and of MPD's DoP settings (every output's `dop` and `allowed_formats`) is kept for the next reading, and only the DoP claim reads it (`watchOpens`, B473 B3 rounds 5 to 7). A reading that cannot tell the stream on a device keeps no evidence of it and erases none (round 6): its card read in part (`alsa.CardReading.Partial`: a substream it could not list may be the open one, and the rest read as closed), whether or not the reading's ids still name it (round 7, `cardReadInPart`: an output whose device names no card lists every USB card as a candidate, and a card is USB by the usbid in its directory, so a card read in part can drop out of them), a substream not read whole, or no ids at all (`/proc` not read, the ALSA configuration not had (`alsa_config_unread`), the probe late with no endpoint). Nor does a reading that finds an open stream whose run it cannot tell (round 7, `streamOn`: not running, never started, or less than `runFloor`, 250 ms, in by its own `hw_ptr`, since a start after the reading could share a younger run's stamp): the stream may still be watched opening from the record before, but it is not kept. The record before then stands for the next reading to compare with, while that reading read MPD's DoP settings and they are the record's; one that could not read them (MPD not read, its outputs differing across the `/proc` read) or read them changed ends it, since the open the record would vouch for may have come in between. A device the reading's ids no longer name keeps nothing unless its card was read in part, and a reading whose own code panics forgets it all (`forgetOpens`). The order is `status`, `currentsong`, `replay_gain_status`, `outputs`, every card in `/proc/asound`, then `status`, `replay_gain_status` and `outputs` again, then the ALSA configuration and `mpd.conf`, after the bracket, so their deadlines never widen it. `mpd.conf` is read again at every reading (the player rewrites it and restarts MPD), never taken from the probe's copy, always using the bridge's configured/discovered file; that file stays under the probe's root or, for a path that names a volume, that volume's root (`confRoot`: a Linux path names none; a Windows test's temp directory sits on another drive than `/`). The dial and each command have `limits.dial` and `limits.command` (750 ms); the `/proc` read, `mpd.conf` with its includes, the command-line hints, the ALSA configuration and the row look have `limits.proc`, `limits.conf`, `limits.hint`, `limits.alsa` and `limits.file` (500 ms) and run through `boundedStep`: a goroutine each, a step still running from an earlier reading neither joined nor doubled (the next reading's step is late at once, so a hung file or mount holds one goroutine, and a joined read would predate the first status), and a panic the step's failure, logged with the step and the stack and the value only when it is the runtime's own error. The probe's inspection, which the reading takes first, is bounded the same way (`inspectBounded`); a probe that expired with no MPD endpoint leaves the output ids unknowable, and every well-formed id is then `unknown` with `mpd_timeout`. A command that failed after it was written drops the session (a late reply would answer the next command), and so does one that panicked (the MPD connection's bullet); an `ACK` keeps it, and so does a command that never got its turn; a failure once the command's deadline has passed is `mpd_timeout`, since the client's socket deadline is the context's and can fire before the context's own timer. `state` is `whole`, `settling` or `unknown`; settling is `signalpath.Settling` over the two status reads, `currentsong` naming another song or, while MPD plays, no `Id`, and `settings_changed` when the two `outputs` replies, the two replay gain replies, or the two statuses' `audio`, `xfade` and `mixrampdelay` differ (`sameSettings`); the two statuses' `volume` is read for the software volume warning, below 100 at either, and not compared, so a volume change does not settle a reading (round 4); unknown is `mpd_unreachable`, `mpd_timeout`, `mpd_refused`, `proc_unreadable`, `proc_timeout`, `alsa_config_unread` (the ALSA configuration read ran late, failed, or stopped at a file that is there and could not be read, `alsa.PCMConfig.Unread`: nothing is resolved from it, B473 B3 rounds 3 and 4; a file the read took that the parser does not model is had, and resolves no PCM name, as GET /v1/outputs' read does and the same way at every read, so their ids agree), `no_card` or `reading_failed` (a panic in the reading's own code, with `mpd`, `song` and `dac` null), and what was read is still reported (a card when MPD is down). The id is matched against the cards GET /v1/outputs lists from this reading's cards (`dacsFrom`, resolving from the ALSA configuration this reading read), `404 output_not_found` otherwise; with `/proc/asound` unreadable, the ALSA configuration not had (a PCM name such as Volumio's `volumio` can be all that names an I2S HAT's card), the ids unknowable or the reading failed, any well-formed id (64 lower-case hex) is answered `unknown`, a malformed one is 404, and no `/proc/asound` is `no_card`, with nothing resolved against the host's ALSA files. The device and every output's chain and sample stage are resolved once a reading, so a cached answer reads nothing. The song is this bridge's row by the loopback route's URL (http, a loopback host, the route's port, `/dlna/file/<id>` only, through the route's own index) or by a root's prefix rule with the rest of the path local to the root (`filepath.IsLocal`) and `os.SameFile` on MPD's file and the root's; the reading never looks a row up at a path outside its roots, and a row is current only while the file is a file by `fsutil.NotAFile`'s list, the scanner's own, so a Windows cloud placeholder (`ModeIrregular`) is one (round 8, `stillTheScannedFile`), and has the scanner's size and `mtime_ns`, read with the row's format in one statement (`GetTrackWithStat`, counted at the driver; serve's lookup reaches it through `outputRowReader`, which has that read alone). Every external string, including DAC mixer and song-row strings, passes every config read's redaction and `conf.RedactSecrets` with `output.password`, and a URL's user information is left out. **Don't add a sampler or background read, join or double a running read, read a file outside `boundedStep`, keep a session a written command failed or panicked on, take `mpd.conf` from the probe's copy, read it or the ALSA configuration inside the bracket, resolve an id from an ALSA configuration not had, read a setting the verdict or its warnings read on one side of the `/proc` read only, name a file the bridge's row by its suffix, read the row in two statements, take its file for a file by `IsRegular`, or put an `alsa`, `mpd` or `signalpath` type on the wire.** `TestReadingTheGoBarOnEveryCapture`, `TestReadingAnIdleBox`, `TestReadingJSONKeepsEveryKey`, `TestTheReadingReadsMPDConfAgain`, `TestMPDConfIsReadUnderItsOwnVolume`, `TestReadingNamesTheBridgesOwnRow`, `TestReadingSettles`, `TestASettingThatChangesAcrossTheReadSettles`, `TestTheReadingIsReadThroughForHalfASecond`, `TestAProcReadPastItsDeadlineIsAnHonestUnknown`, `TestAnMPDCommandPastItsDeadlineIsAnHonestUnknown`, `TestEveryMPDCommandIsHandedItsOwnDeadline`, `TestAConfigReadPastItsDeadlineIsAnHonestUnknown`, `TestAnALSAConfigurationReadPastItsDeadline`, `TestAnALSAConfigurationNotHadLeavesEveryIdUnknown`, `TestAConfigurationNotReadWholeResolvesAsTheListingDoes`, `TestAProbeThatHangsEndsTheReadingInTime`, `TestAnIncludeThatIsANamedPipeEndsTheReadingInTime`, `TestAPanickingStepLeavesTheNextReadingWhole`, `TestACommandThatPanicsDropsTheSession`, `TestTheMPDPasswordReachesNoReadingAndNoLog`, `TestTheReadingSendsOnlyReads`, `TestAReadingNamesItsOutputByID`, `TestHealthAdvertisesOutputReadingWithLocalOutputs`, `TestGetTrackWithStatIsOneStatement`, `TestOutputTrackLookupReadsOneVersionOfTheRow`, `TestAReadingThatCannotTellKeepsNoEvidenceAndErasesNone`, `TestARowOnACloudPlaceholderIsCurrent` and `TestServeReadsTheOutputAndNamesItsOwnTrack` pin it.

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
and `version.ServerVersion`; no `mpd`; `dac` the boot binding's projected
card or `null` (B543: see "The bridge player's DAC projection" below;
`alsa.USBIdentity` is never serialised);
`pathRules` `[]`, an exception to "one element per library root". The
`bridge` object is `json:"bridge,omitempty"` beside `mpd`'s, so a kind `mpd`
element's bytes are main's (`TestAnMPDElementsBytesAreMains`, against bytes
captured from 9e7b2675). Its keys are every one present: `openHome`, `state`,
`reason` (null outside `unavailable`, which the handler enforces), `lastError`,
`libraryScope` `self`, `fixedVolume` (the boot snapshot's `EffectiveMixer` is
`none`), `codecs` (never null), `dsdMode` and `allowDoP` (the boot snapshot's
`EffectiveDSDMode` and `AllowsDoP` until B728's B8, and since then the bound
DAC's, from the set the backend published: "One read of the backend's facts,
the set's own view" below). `TestTheBridgesOwnElement` pins every key and
value.

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

*B676 (2026-11-03): a node refused to this process, by its sentinel too.*
**Never derive a denied device from `LastStop`, playback's `permission_denied`
code or an error's text: the attempt is denied only when its error wraps
`engine.ErrDeviceDenied`, which the backend maps from alsapcm's permission
reason (EACCES and EPERM alike, `openError`), at a start and at a resume's
reopen, as it maps busy.** On Linux the audio group owns the DAC's node; a
desktop login opens it through logind's seat ACL, and a systemd user service
kept alive by lingering, often with nobody at the seat, has neither the ACL
nor, until its user joins the group and its user manager restarts, the
group. The open then failed with the cause lost on the way: the attempt knew
busy alone, so `/api/output/player` and the console's Player page said
nothing, and a refused resume, which moves no generation, was logged nowhere.
The backend's `busy` and `busyError` became `deviceCause` and `causeError`,
one wrap for either sentinel, the text still the alsapcm error's. The attempt
gained `Denied`, `Resume` (a paused session's Resume, a refused one and one
past its deadline alike; false for a Start) and `Detail`, the failure's words,
which serve's `logPlayerResume` logs and nothing decides from. A later attempt
that succeeds, or fails for another reason (an entry refused, a busy device,
words alone that say permission), clears a denied one, as it clears busy.
GET /v1/outputs' `lastError` keeps PROTOCOL.md's closed list, `device_busy` or
null, so a denied attempt is null there; the console's view reads the
attempt of the same snapshot (`bridgeOutput.facts`) and words it
`permission_denied` (`consoleLastError`), the decision on B676's brief (Argon's
recommendation): no wire change and no iOS mirror. Pinned by
`TestADeniedStartIsTheLastAttemptUntilAStartSucceeds`,
`TestADeniedResumeIsTheLastAttemptUntilAResumeSucceeds`,
`TestALaterFailureForAnotherReasonEndsDenied`,
`TestAResumePastItsDeadlineIsAFailedResume`, through the real chain over the
fake kernel's EACCES and EPERM by
`TestPlaybackStartOnADeniedDeviceIsTheEnginesDeniedAttempt` and
`TestPlaybackDeniedResumeAfterTheReleaseIsTheEnginesDeniedAttempt`, the
words-alone guards `TestPlaybackARefusalThatSaysPermissionIsNoDeniedAttempt`
and `TestPlaybackARefusalThatSaysBusyIsNoBusyAttempt` (whose permission decoy
is now denied, as it should be), and in serve by
`TestThePermissionCauseIsTheConsolesWordAlone` and
`TestThePlayersPermissionCauseReachesTheConsoleAndTheLog`.

### The codecs, one source

**List the containers `playback.PlayedContainers` names, which the open's
`dsd_not_in_this_release` refusal reads (`Plays`); never type them twice, and
leave openhome's playable-extension list alone (an extension is not a
container: `.m4a` is ALAC or AAC).**

**Beside them, list dsf and dff while the effective set plays a DSD rate, a
native or DoP tuple or a rate it converts to PCM (`PlayerPolicy.PlaysDSD`,
B728 B11; until B11 a tuple alone listed them), the predicate OpenHome's
ProtocolInfo lists DSF and DFF by too, and never while it plays none.** A
converted rate publishes no tuple: until B8 it published nothing, and a client
granted DSD from the tuples alone; since B8 it is a `converted` entry, which a
client grants DSD from as it does from a tuple
(`TestTheElementListsDSDWhileThePlayerConverts`).

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
dial or write anything but the bound DAC's DSD choice, which its PUT writes
through the player's backend (B728 B9).**

`newBridgeOutput(plan, player, cfg)` is installed with
`api.Server.WithBridgeOutput` right after `startServePlayer` returns, long
before the listeners. It holds the boot plan, the `bridgePlayer` and the boot
snapshot's fixed fields (`EffectiveMixer` `none`, and `EffectiveDSDMode` and
`AllowsDoP`, which since B728's B8 the console's Player page alone reads, until
B13 replaces it); `Enabled` is `plan.lists`. `bridgePlayer.id` is set with `eng`
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

B728's B9 makes the view the choice's route's chooser too
(`api.BridgeDSDChooser`, `serve_player_dsd_choice.go`): `OwnsOutput` reads the
identity as the element does, and `ChooseDSD` writes the bound DAC's choice,
the app's, through the player's backend (`playback.Backend.ChooseDSD`) and
answers from the epoch the backend published once the write ended, read as
GET reads it (`bridgeDSDFacts`); a player not running yet, or a backend that
is not the bridge's, has no bound DAC to write for (`dac_unavailable`). GET's
path writes nothing, as before.

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
replace known serial and anchored port substrings with `[redacted]`, the port
in each spelling, as sysfs gives it (its configuration and interface after the
port), its canonical form, and the port alone, as a binding names it
(`playback.PortForms`, since B728 B8 round 2, which found a label holding the
port alone kept it):
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

## B728 B8 — the read side of the player's DSD (2026-11-07)

The record is in `ops/engineering-log.md`, 2026-11-07, B728 B8. The heading
below is the long form of `output.md`'s bullet on the element's DSD read side;
`wire.md`'s bullets on DSD capability, `logicalRate` and the fixture exporters
have theirs in `History-b470-native.md`, under "The read side (B728 B8)".
`output.md` loads with the read side's tests and goldens
(`cmd/bridge/serve_player_dsd_read*.go`, `serve_player_dsd_golden*.go`,
`testdata/dsd-outputs`) since this change.

### One read of the backend's facts, the set's own view

**Never build `dsdDAC`, `dsdMode` or `allowDoP` from another read (the
element's `dac`, the store's `State`, a refresh, the box), store a new set or
move `policyGeneration` for a change that leaves what plays as it was (an
occurrence admitted under a set is compared with the one published by its
pointer, and only a reconciliation repoints it), leave such a change
unpublished, give an observation the current set's evidence unless it was
admitted under that set (its boot and generation: a `policyId` names no DAC,
firmware or set), put a serial or a port, in any of the port's spellings
(`playback.PortForms`), in a key, a model, a profile's name or a caveat's
note, or give a converted occurrence native or DoP evidence.**

What the element says. Until B8 it said of the player's DSD what B7
published, the tuples under the boot and the epoch, the reason, the model and
the firmware, and the boot snapshot's `dsdMode` and `allowDoP`, which since
B7 are the migration's input alone: a client read what played and nothing of
why, or of the DAC's choice. B8 publishes the rest of the design's section
7.1. On `dsdPlayback`: `converted`, each rate the set converts with its plan
(`rateHz`, `validBits`, `passbandHz`, `dither`, `filter`); `refused`, each
rate from DSD64 to DSD256 the set plays neither way, with its word;
`warnings`, the state's, the bridge's and the bound DAC's own; and each
tuple's `evidence` and `evidenceOn`. `dsdDAC`, the bound DAC: its `dacKey`,
its profile (ID, name, caveats), the choice that applies (`mode`,
`maxBitRate`, `revision`, `dopConfirmed`), each mode's option and each
transport's first use. An option is listed for `native`, `dop`, `convert` and
`off`, never `auto`, which is no explicit choice and resolves to what the set
plays (`optionModes`): native's and DoP's with their rates and each rate's
evidence (`[]` for none), DoP's `needsConfirmation` (a rate it is offered at
that the profile neither heard nor states DoP at) and `blocked` (the gain
check's word, where it refuses DoP at every rate DoP is offered at),
convert's plan while it is offered, and off always offered; DoP is not
offered at a rate the profile refuses it at, but for this box's own clean
answer there. First use is one entry a transport, `pending` (not
acknowledged under the unit's firmware), `acknowledged` or `observed` (an
occurrence of it has since played 5 s of its content with no fault), with
`bitRates` the rates a migrated acknowledgement covers, absent for every
rate. On `playback`: `evidence` and `evidenceOn`, a native or DoP
occurrence's, and `converted`, a converted one's plan and its count of
samples saturated. The section's `check` member is B12's; the write route and
its health flag (`bridgeDSDChoice`) are B9's, which advertises the flag with its
route (`TestTheChoiceFlagArrivesWithItsRoute`), and no flag comes before its
route (`TestNoHealthFlagBeforeItsRoute`). Every word is in a
closed list of PROTOCOL.md's, and the source-derived token guard reads each
from its definitions (`TestDSDOutputProtocolTokens`).

The view is published with the set it describes. `readDSD` reads the DSD
state once (`b.dsdState()`, the store's state in memory) before it computes
the set, hands that state to `policy.Effective` and to `viewOf`, and
`refreshDSD` publishes the view beside the set in one pair (`dsdPublished`),
swapped as one, so no reader sees one set's tuples beside a view made for
another (round 1 carried it in the set, `dsdSet.view`, which round 2's fix
below took out); `copyPolicyLocked` clones the pair's view into
`PlayerPolicy.DSD` beside the tuples and converted rates of the pair's set,
and `Backend.Facts` returns the copy and the live observation under one lock. Serve's view of its player (`bridgeOutput.facts`) calls `Facts`
once a request and builds `dsdPlayback`, `dsdDAC`, `dsdMode`, `allowDoP` and
`playback` from what it returned (`bridgeDSDFacts`). A second read could
show one reading's tuples beside another's choice or first use, and the
store's `State` waits on the store's lock, which a write holds across its
fsync; the read side's read takes neither and opens no file. It waits on
two locks alone: the backend's `policyMu`, held for copies and for the observation's
updates, and the current sink's, which `Position` takes and no kernel call
holds; never `dsdMu`, which a refresh holds across its reading of the box.
`dsdMode` is the mode that applies to the bound DAC, its own choice's or the
state's `defaultMode`, and `allowDoP` whether DoP may play at some rate it is
offered at, by the profile (a heard or stated rate it does not refuse) or the
user's confirmation under the unit's firmware, neither the gain check nor
first use, which the DoP option and first use say: it describes and permits
nothing. Both are `off` and false where no view was read (the backend not
ready, no player, the harness's backend, which publishes none) and where the
set was computed for no DAC (`noView`: none available, its model or its
stream file unread, every rate refused as `dsd_dac_unread`), and a state
that does not read is `off` too. `dsdDAC` is omitted while the view names no
DAC, which the set decides (`DSDView.DAC`), never the element's `dac`, B543's
reading of the box, or another read; `dsdPlayback`, its `refused` and
`warnings` with it, is published whenever the backend is ready, so a state
that does not read shows as `state_unreadable` with no DAC available
(`TestADamagedStateIsPublishedWithNoDAC`, through serve too, the state file
damaged and the DAC unplugged), and a migration's refusal as the bound DAC's
`migration_record_refused` (`TestAMigrationFailureIsPublished`, through
serve with a stale legacy record). The console's Player page keeps the boot
snapshot's legacy words (`bootDSDMode`, `bootAllowDoP`) until B13 replaces
it. `TestTheReadSideIsTheSetsOwn` holds the view to the set it was made
with, for a known DAC whose own choice is native, an unknown DAC with no
choice, no DAC available and a state that does not read;
`TestTheBridgeElementCarriesTheDACChoice` the element's members with no
choice recorded, a choice in a seeded state file, a migrated choice, the
backend not ready, no DAC bound and no DAC available.

A change that moves no plan is published in its epoch. `dsdSet.same`
compares what plays (the unit, the endpoint, the identity, each rate's plan
and refusal), and until B8 a set the same as the one published was dropped,
so a transport's first use observed, an evidence step, a choice's revision
or a warning reached the wire only with the next change that moved a plan.
`refreshDSD` now publishes its view when it differs (`reflect.DeepEqual`),
in a new pair beside the set as it was: it stores no set, the pointer and the
generation stay, and it reconciles and logs nothing, since nothing plays
otherwise; a client's grant, keyed on `bootId` and `policyGeneration`,
stands. The set's pointer must stay: an occurrence admitted under a set is
compared with the one published by that pointer, at a gapless join and at a
converted join window's successor, and only a reconciliation repoints it.
Round 1 stored a new set for such a change, under the generation it had,
which no reconciliation followed, so on every first DSD playback of a
transport (its first use observed at 5 s) the successor admitted before it
was never under the set published again and the producer waited at the join
for good (round 2; `TestAFirstUseObservedWhileDSDPlaysKeepsTheProducerWriting`
holds the producer inside the first track until the monitor has published
the change, then lets it go: a native successor and a converted one in its
predecessor's join window, each must join as it was admitted). The policy is the design's sections 7.1 and
2.5: `policyGeneration` is the epoch of what plays, which section 7.1
publishes the tuples and converted rates under ("under its boot and
epoch"), and section 2.5's records (a first use, the evidence, a choice's
revision, the warnings) describe the DAC without changing what it plays,
while a write that does change it moves a plan and the generation with it.
It reaches the wire at the next refresh: once a second, sooner after a state
write, a probe's answers or a watcher snapshot, and every 100 ms while a
session plays DSD. `TestAChangeThatMovesNoPlanIsPublishedInItsEpoch` drives
each on a backend that has published, and
`TestTheBridgeElementCarriesTheDACChoice`'s "a first use observed on a
published backend" through serve's view.

Evidence is the policy's. A tuple's `evidence` is the rung and date
`policy.Effective` computed for it (`Plan.Evidence`, `EvidenceOn`), never a
field of the state read again, so a probe's acceptance this run holds in its
cache alone is `driver_accepted` with no date, and a durable acceptance under
another kernel or another firmware is no rung of this box's
(`TestTheEvidenceIsThePolicysRung`). An observation's is its tuple's in the
set published now, by its policy ID, while that set is the one it was
admitted under (the observation's boot and generation the set's: since round
2 a generation names one stored set, and so the unit and the firmware it was
computed for), else the one its admission gave it. A policy ID names a
tuple's transport, format, rate and timing, never the DAC, its firmware or
the set, so another DAC's tuple, or the same DAC's on another firmware, can
share it, and round 1 gave a retained observation the next DAC's rung and
date (round 2; `TestARetainedObservationKeepsItsOwnDACsEvidence`, another
model in the bound port and the same model on another firmware). A PCM and a
converted occurrence carry none, since neither plays a native or DoP path. Its words are the ladder's alone
(`reported`, `driver_accepted`, `streaming_observed`, `listener_confirmed`),
none of which says a DAC decodes DSD.

No serial and no port. The key on the wire (`wireKey`) is the model key's
`DACKey` with the unit's own serial and port, the port in each of its
spellings (`playback.PortForms`: as sysfs gives it, with the configuration
and interface after the port, its canonical form, and the port alone, as a
binding names it), taken out of its product string first (`unitText`): a product string that holds the serial would make the
key a hash of it, and the model published beside it, the serial redacted,
gives every other byte of that hash's input, so a short serial could be
found by trying each; taken out, two units of a model share the key. The
free text, the model, a profile's name and its caveats' notes, goes out with
the unit's own serial and port taken out (the backend's `unitText`) and the
boot binding's (serve's `playerText`), since a port binding holds no serial
and a USB string may hold either; the words of closed lists, the hashes, the
dates and the firmware's hexadecimal digits are not redacted, which taking
out a short serial would mangle. `TestNoDACKeyOrModelCarriesASerialOrAPort`
reads every string of the response under a port binding, which holds no
serial, its unit's product string holding the serial and its manufacturer
string the port, two units of the model publishing one key, and through
serve with the GO bar bound by its port, its product string holding its
serial. Round 2 added a unit whose sysfs port carries its interface
(`usb2-1:1.0`) and whose USB strings hold the port alone (`usb2-1`), under a
model binding, which names no port for serve to redact by: neither form is
published, and two such units at two ports publish one key, where round 1,
taking out the sysfs form alone, put the port in the model and hashed it into
the key. Serve's `playerText` takes every spelling of the binding's port out,
and B543's `playerDACText` every spelling of the unit's out of the `dac`
element's labels (`TestBridgeDACRedactsIdentityInsideNameAndID`'s port-alone
cases), by the same helper.

A converted occurrence's observation is its plan's: transport `converted`,
its PCM's format, rate and valid bits, `logicalRate` 0, and `converted` with
the source's DSD rate, the plan's filter, passband and dither, gain 0, and
`clippedSamples`. That count is the converter's (`occ.clipped`, an atomic the
producer stores after each read of at most 4,096 frames, 23 ms at 176.4
kHz), loaded at each request (`Backend.Facts`, `liveClipped`): it runs ahead
of what the DAC has played by what the player holds, its lookahead of about
two seconds and the hardware's buffer, and costs the audio path nothing, no
lock and no allocation there (`TestAConvertedEntryCarriesItsPlan`).

The goldens, `cmd/bridge/testdata/dsd-outputs`, are one whole GET
/v1/outputs response a kind of DAC: a known DAC playing native at every
rate, its evidence on each rung but driver acceptance (`known_native`); an
unknown DAC converting every rate (`unknown_converted`); a known DAC whose
explicit DoP waits for its first use and its confirmation (`dop_pending`);
DSD migrated off (`off`); no DAC available, the state damaged
(`no_dac_damaged_state`); a converted occurrence playing
(`converted_playing`). Each is built from the real backend's published set
through serve's view and the API's route, twice alike, with no host, serial
or port in it; the backend's boot ID, random, stands as `golden-boot`, and
nothing else is normalized. `BRIDGE_WRITE_DSD_GOLDENS` alone rewrites them
(`TestEachKindOfDACsWholeResponseIsItsGolden`), and the iOS mirror's
`dsd_dac_outputs.json` is exported from the same cases (wire.md's exporter
bullet).

## B728 B9 — the write of the bound DAC's DSD choice (2026-11-07)

The long form of `output.md`'s bullet under "The player's DSD choice: its
write (B728 B9)"; the record is in `ops/engineering-log.md` under that date,
and the route's wire half is `wire.md`'s bullet on `PUT /v1/outputs/{id}/dsd`,
whose long form is `History-b470-native.md`'s "The write side (B728 B9)".

**A choice's write (`playback.Backend.ChooseDSD`) checks the state inside the
store's one write, one write at a time, and asks first, by the classification
reconciliation acts by, what the set it would publish interrupts; that set is
pending from before the question until the write settles, and an occurrence
whose open ends, or that the producer would hand a sink, at a rate the pending
set plays otherwise waits for it; a write that landed and is not known to be
durable holds the set to what both states play alike until the next start; a
written choice owes a probe and drops none of its answers. Don't read the
compare-and-set, the confirmation, first use or a rejection outside
`dsdstate.Store.Update`'s closure, decide `would_interrupt` but by
`judgeDSDLocked` against the set the write would publish and the one the state
gives without it, take a session's lock under the store's or the store's under
a lock of the backend's, hold a lock an admission takes across the fsync,
publish before the state is durable or answer before the publish, let an
evidence write move a choice's revision, roll a landed write back, drop the
probe's answers at a choice's write, or leave a set pending past the write's
end, a panic's included.**

The checks that read the state, the compare-and-set, the mode offered, DoP's
confirmation and first use (a listener's rejection re-arming it), are
`policy.Choose`, pure, and `ChooseDSD` runs them inside the store's one write,
on the state that write is made from, so no other writer of the state (the
migration's part for a DAC, which writes a choice; playback's evidence) comes
between the check and the write: a negative control that read the
compare-and-set before the store's write let a write land over a revision the
migration had moved with what plays as it was. Writes are one at a time, end
to end (the writes' turn, `choiceTurn`, its wait bounded by the request's
context): a write that finds the revision moved answers `stale` only once the
write that moved it has published, a stale answer reads the policy again
first, and the revision it carries is the one the next write compares with, a
revision a client read from `GET` between another write's durable write and
its publish included (`TestAWriteBackOfARevisionReadInTheWindowIsAnsweredWithTheDurableOne`).
A state that does not read publishes revision 0: a write-back of 0 over the
fresh state that replaced a damaged file lands and clears its refusal, and one
over a file refused in memory answers `state_unreadable` and writes nothing.

`would_interrupt` is classified before the write, from one reading of the
box, and, since round 2 (below), answered inside the store's closure after
the state's checks: the set the write would publish and the one the state
gives without it, and an
occurrence interrupts where `judgeDSDLocked`, the one classification
`reconcileDSD` acts by too, says it interrupts against the first and not
against the second, so what a change of the box does anyway is no interruption
of the write's. The judgement is `policy.Reconcile` and the backend's rest: a
re-plan of a session's first occurrence, or of an occurrence of a session
being released, is a revocation, and so is one where the engine hands nothing
off again; a re-plan interrupts where frames of the occurrence are in a sink,
and also for the occurrence the producer writes and one whose first bits are
in its predecessor's join window, whose frames can reach the sink before the
write publishes; an occurrence played out, still opening, dropped or PCM is
not judged. The store's closure makes the two sets again from its own state
and sends the write round again where either moved (`errChoiceRound`, at most
eight rounds, then, since round 2, `persist_failed`), so the decision holds at
the commit.

The set the write would publish is pending (`Backend.pending`) from before the
classification until the write settles. The step that ends an occurrence's
open, start's and `openSuccessor`'s, takes its session's lock through
`lockSettled`, which waits, the lock let go, while the pending set plays the
occurrence's rate otherwise than the set published; the producer holds back
from a sink an opened occurrence at such a rate (`next`, `transition` before
its open, a converted join's window in `holdLocked`), and a resume between two
transports waits as well and opens nothing for an occurrence the write left
being handed off again. The pending set is stored before the classification
takes each session's lock, so an occurrence either ended its open before it,
and was judged, or sees the pending set. The brief suggested admitting only
what both sets play while a write is pending; waiting was chosen instead: the
intersection would refuse, and skip, a track at a rate the write changes that
starts in the window, where the wait, bounded by the write's fsync, admits it
under what the write leaves. A write that fails clears the pending set and
leaves the set as it was. One that landed and whose sync failed
(`atomicwrite.ErrLanded`) keeps the state it left in the file
(`landedChoice`), and every refresh holds the set to what the two states play
alike (`policy.Intersect`, the rest refused as `dsd_choice_uncertain`) until
the next start reads the file; the store's fence refuses every later write
(`dsdstate.ErrLandedNotHeld`, `persist_uncertain`). Nothing is rolled back. A
session whose release begins between the classification and the publish has
its DSD revoked by the publish, as reconciliation revokes a releasing
session's, which the write did not count: the session was ending already.
Every end of a write settles what it left pending, once, before the writes'
turn is given back, a panic's included (the API's recoverer answers it): a
negative control that removed the deferred settle left the set pending after
a panic, where an occurrence at a rate the write changes waits for a write
that will never publish (the producer's until its session ends); what a
panic after the state's write left in the state the next refresh publishes
(`TestAWriteThatPanicsLeavesNothingPending`).

Lock order: the writes' turn, then the refreshes' (`dsdMu`), then the
backend's, then each session's, then a sink's; the store's lock is taken
alone and its closure is pure, so no lock an admission takes spans the fsync,
and playback's own writes of the state (`recordDSD`) run on goroutines of
their own, under no lock. Since the merge with B762 (2026-11-07) the
choice's own write is one of the writes of the player's DSD state the
player's stop closes (`dsdWrite`, `CloseDSDWrites`): refused once they are
closed (persist_failed, nothing written), and counted while it runs, so the
stop keeps its share of the data directory's lock until it ends. A written
choice owes the silent probe (`oweProbe`, `ProbeDSDPending`), which serve's
supervisor asks at its next policy tick and which runs, on a goroutine of the
backend's since B762, once the device slot is free, never inside the
request; it drops none of the probe's answers and moves no generation
(`player.md`'s bullet on the silent probe), so a write that changes the
choice and nothing that plays publishes no new set, and a probe under way as
it is written keeps what it asks.
What playback observes (a driver's acceptance, streaming observed, first use
observed) never moves a choice's revision: a negative control that moved it
at each such write made every write of a session's base stale.
`TestTwoWritesOfOneRevisionLandOnce`,
`TestARevisionMovedBeforeTheStoresWriteIsStale`,
`TestAWriteBackOfARevisionReadInTheWindowIsAnsweredWithTheDurableOne`,
`TestTheWriteAsksWhatReconciliationDoes`,
`TestAnOccurrenceWhoseRateTheWriteChangesWaitsAtTheEndOfItsOpen`,
`TestTheProducerHoldsAnOccurrenceAPendingWriteWouldReplan`,
`TestTheChoiceWritesLockOrderHolds`,
`TestALandedWriteIsUncertainAndPublishesWhatBothStatesPlay`,
`TestAWrittenChoiceOwesAProbeAndKeepsTheProbesAnswers`,
`TestOnlyAWriteThatInterruptsNeedsTheGoAhead`,
`TestAnOccurrenceBesideAWriteIsNotRevoked` and
`TestAWriteThatPanicsLeavesNothingPending` pin it.

## B728 B9 round 2 — the write beside what plays (2026-11-07)

The long form of `output.md`'s second bullet under "The player's DSD choice:
its write (B728 B9)"; the record is the engineering log's round 2 entry of
that date.

**Beside what plays (B728 B9 round 2), a write counts an occurrence whose own
sink an open is under way for as reaching it (`occ.claimed`, taken in the step
that found no write pending and kept to the end of its activation), and that
open's adoption and activation wait for a write pending, an occurrence it
handed off again by then, or dropped for its replacement, taken back from its
sink unwritten (`errReplanned`, `unadopt`), and so is one dropped as its
activation begins, by whatever dropped it, its decoder and content closed as it
is taken back, a release then ending the session with no fault (round 3), and
one dropped or handed off again while its sink's own activation admits its node
or writes its prefill is taken back with none of it written, the drop or the
hand-off revoking that sink, whose activation answers its own fence
(`alsapcm.ErrActivationFenced`, never the device's answer), a failed activation
judged by its cause in this order: a release ends the session, a revocation of
the session's DSD keeps its fault, the drop's own answer (the sink's fence,
`errReplanned`) is taken back, and anything else is the fault it was (round 4);
a resume waits in a loop; a refusal
of the policy's that a write or a later set lifts is asked again where the open
ends, the content's checks with it (`settleOpen`, `refusedUnder`), a
successor's DSD monitor starting once it is admitted; a gapless successor waits
out of its sink only while that sink holds more than its floor; the write
decides `would_interrupt` inside the store's closure, after the state's checks,
from a classification made before it, answers an earlier landed write's fence
first, and goes round at most eight times, then `persist_failed`; its answer's
epoch is read inside its turn; and it is judged against the set published, a
reading of the DAC that gives another set published and reconciled first. Don't
open, adopt or activate an occurrence's own sink beside a write that has not
counted it, wait for a write with an `if` where another may follow, fault a
successor handed off again or dropped at its activation, at its fence too, take
a dropped one back with its decoder open, fault a session whose release came
during an activation, let a test of the adoption's wait pass on the activation's
own, take an activation's failure beside a drop for the drop's but by the drop's
own answer, judge a drop before a release or a revocation, leave to its
activation the sink of an occurrence a reconciliation handed off again, answer a
device's failure as the native sink's own fence or that fence by a bare error,
ask a decoder's
refusal again or a policy's without the content's checks, compare a pending set
with any set but the one published, hold a gapless successor past its sink's
floor, take a session's lock inside the store's closure or answer
`would_interrupt` before the state's checks, answer a write the state or the
DAC kept moving under `stale`, read an answer's epoch after its turn, or judge
a write on a reading the set published does not agree with.**

The producer's transition, and a resume between transports, open the next
occurrence's own sink outside the session's lock, and a write that classified
meanwhile saw an occurrence opened and in no sink, a re-plan that interrupts
nothing, while the open went on to adopt and activate the sink before the
publish: a write told it interrupted nothing put b's frames on its node, and
one with the go-ahead stranded b, the engine's replacement refused because
b's frames were the kernel's. The step that finds no write pending now claims
the occurrence in the same lock, and the classification counts a claimed one
as reaching its sink. The claim's adoption waits while a write is pending and
closes the sink unwritten where the occurrence is being handed off again;
its activation waits too, no lock held (`checkDSDActivation`, `awaitWrite`),
for a write that became pending after the adoption, and an occurrence handed
off again by then, or dropped for its replacement (a cut of a successor with
no frame written drops it at once, which the check took for a refusal and
faulted the session on: a control that removed it went red in 4 of 5 runs),
is taken back from its sink unwritten. Of the two waits, round 2's tests held
the activation's alone: a control that removed the adoption's alone stayed green
there, and one that removed both went red. Since round 3 the adoption cases
require the adoption's own wait, the occurrence's activation unreached while the
write is held (`waitedUnactivated`), and a control that removes the adoption's
wait alone goes red in each of them. A resume asks again under
the lock each time a write settles, since another write may be pending by
then. Each wait is bounded by the write's own, which settles at every end of
it, and ends with the session's life, which a release ends, a resume's with
the call's context too.

A successor the set refused by its word was refused for good though the write
that lifts the refusal was pending as it was made, or published before the
open ended. The set it was refused under is kept, the pending fence counts it,
and the step that ends the open asks again, outside the lock, an admission
refused under a set that is no longer the one published, through
`openNativeContent`, its reservation charged once, so the content's checks
are made with it (an empty content refused as that); a decoder's refusal is
never asked again. The fence compares the pending set with the set published,
never with the one the write was made from: a session's start that reads the
durable state before the write's own publish has published what the write
would, and the first try, comparing with the write's own, made an occurrence
admitted under it wait for a publish the test held
(`TestAnOccurrenceBesideAWriteIsNotRevoked`).

The producer held a successor at a rate a write changes in `next()`, whatever
its transport, with no bound, so a write whose fsync outlasted the sink's
lookahead ran the sink dry; a gapless successor now waits only while the sink
it would join holds more than its floor (100 ms, `windowFloor`, the reporter
waking the producer at each measurement), and at the floor, like any other,
takes the transport's step, which drains the sink before it and opens none for
the successor until the write settles: an honest gap, never an xrun. A
converted join window's hold is the same wait, bounded the same way.

`would_interrupt` is the write's last check: classified before the store's
write, under the pending set, with no lock of the store's held, and answered
inside its closure after the policy's checks and the two sets' consistency,
so a write that would interrupt and finds the revision moved is `stale`.
Classifying inside the closure would take each session's lock under the
store's, which a control showed deadlocking `TestTheChoiceWritesLockOrderHolds`
once it held a reader that takes the store's lock under a session's. The
store's fence, checked before the closure, then answers a fenced write first
anyway, and the backend answers it before reading the state as well
(`choiceFenced`), so a fenced write whose reading of the DAC drifts does not go
round to its bound and answer `persist_failed`. Past eight rounds the write is
`persist_failed`; its line is a warning that the write was not made, with why,
never that it was not made durable, which says it landed. The epoch an answer
carries is read by a defer registered after the turn's, at every return
inside the turn: read after it, the next write could publish first, and a
stale answer carry a revision that write moved past. Stale says the base was
not the state's revision at the compare-and-set; the migration's part for a
DAC takes no turn and can move the revision after the answer, which the next
write's compare-and-set answers stale with the newer one.

A write is judged against the set published. Where its reading of the DAC
gives another set than the one published, a reading that glitched or the
DAC's own change, it publishes and reconciles that reading first, a refresh of
its own that reads the box again, and goes another round, sharing the bound:
judged on its own reading, a gain read that glitched in it alone made the DoP
occurrence heard look revoked anyway, so the write was told it interrupted
nothing and its publish, from a reading that did not glitch, revoked DoP. The
other option, publishing exactly the write's own reading, was not taken: what
the box shows is published by a refresh, and the write's publish is one. The
classification also counts a session being released, and an engine that
hands nothing off again, as revocations (`TestAWriteBesideASessionBeingReleasedIsTold`,
`TestAWriteBesideAnEngineThatHandsNothingOffAgainIsTold`, the rig's
`noReplan`). `TestASuccessorsOpenBesideAWriteIsCountedAndWaitsForIt`, `TestAnActivationBesideAWriteWaitsForIt`, `TestASuccessorDroppedAtItsActivationIsTakenBackUnwritten`, `TestAReleaseWhileASuccessorsAdoptionWaitsLetsGoOfTheDevice`, `TestASuccessorDroppedAtItsSinksAdmissionIsTakenBackUnwritten`, `TestASuccessorDroppedWhileItsSinkWritesItsPrefillIsTakenBack`, `TestAnActivationsFailureBesideADropIsJudgedByItsCause`, `TestTheActivationsOwnFenceIsErrActivationFenced`, `TestAResumesOpenBetweenTransportsBesideAWriteIsCounted`, `TestAResumeWaitsForEveryWriteThatChangesWhatItOpens`, `TestAPolicyRefusalAWriteLiftsIsAskedAgain`, `TestASuccessorWhoseOpenEndsBesideAWriteWaitsForIt`, `TestNoSinkOpensForASuccessorWhileAWriteChangesItsRate`, `TestAConvertedJoinWindowWaitsForAWriteThatChangesItsSuccessor`, `TestAGaplessSuccessorWaitsForAWriteNoLongerThanTheSinksLookahead`, `TestAWriteBesideAnEngineThatHandsNothingOffAgainIsTold`, `TestAWriteBesideASessionBeingReleasedIsTold`, `TestTheWritesChecksAreAnsweredInTheirOrder`, `TestAWriteTheStateKeepsMovingUnderIsNotMade`, `TestAStaleAnswerNeverCarriesARevisionAnotherWriteHasPassed`, `TestAWriteIsJudgedAgainstTheSetPublished` and `TestAWriteWhoseReadingNeverAgreesWithTheSetPublishedIsNotMade` pin it.

Round 3 (2026-11-08, the reviews of round 2 and of the merge). The activation's
fence (`activateOutput`) answered a successor dropped while its sink was
adopted, by its replacement once a write the go-ahead let land took its rate, by
an edit that deleted its entry or by a cut, with `native activation fenced`,
which `transition` took for a failed open: the session faulted, `device_error`.
A drop there is `errReplanned` now, where the session is neither being released
nor revoked (those keep the fence, and the session ends), and the sink is taken
back unwritten as at the activation's check. The drop leaves the decoder and
content of the occurrence the producer writes to the producer (`dropLocked`),
which closes a dropped one at its next step; `unadopt` cleared the occurrence
it writes first, so nothing closed them until the session's end (a retirement
or the teardown), and `unadopt` closes a dropped one now. A release that came
while the activation ran, before the session's life ended (between `release`'s
marking and its cancel), had the fence's answer taken for a failed open too: a
Stop there recorded a `device_error`, which the teardown's last report delivered
(the reporter sends nothing while a session is released); `transition` takes a
failed activation of a released session for the release's now
(`fenced("activate")`), and the session ends with no fault. A release while a
successor's own sink waits to be adopted (for a write pending, or in the pause
call its sink takes first, the session having paused while it opened) leaves
the sink to the teardown, which owns every sink opened (the session's outputs),
begins each native one's release and ends each PCM one's life at the release,
and closes it after the files and before it acknowledges: once the release is
acknowledged the node is closed, every content closed and the device free for a
Start. The producer's exits there close nothing of their own, and no code
changed for it: the cases stay as guards. The engine replaces a successor not
yet submitted through Prepare and cuts only a submitted one, which a successor
whose activation is held is not, so the cut case calls the backend's Cut, as
the engine's call reaches it. `TestASuccessorDroppedAtItsActivationIsTakenBackUnwritten`
(the write landing and the replacement made before the activation goes on, and
after it begins to wait for the write; an edit; a cut; a release) and
`TestAReleaseWhileASuccessorsAdoptionWaitsLetsGoOfTheDevice` pin it.

Round 4 (2026-11-08, the reviews of round 3 and of its brief). Past the
activation's own checks, the native sink's activation (`DSDSink.Activate`, on
its writer) makes its check after open of the node, then its fence, then writes
the run's prefill, the profile's DSD silence (up to 5 s), as fast as the DAC
plays it, before it answers. A successor dropped there (by an edit that deleted
its entry, its replacement once a write took its rate, or a cut, which drops a
successor with no frame written at once while the producer is in no Write) had
its unwritten sink revoked by the drop (`dropLocked`), and the activation
answered `native activation fenced`, or in the prefill the write's
cancellation, which `transition` took for a failed open: the session faulted.
One a write handed off again with its replacement still to come (stale, not yet
dropped) had nothing revoke its sink, and the activation wrote its prefill to
the node for an occurrence the set no longer plays. The native sink now answers
its own fence as `alsapcm.ErrActivationFenced` (revoked, released, being
released or paused before the activation was asked, at its admission's end,
with its writer ending before it ran it, or its prefill cut short by an
Immediate release, the cancellation joined to it), never as the device's
answer: an admission the check refused and a write that failed are returned as
they are, and a sink that failed before the activation refuses it (`native
activation refused`). `transition` judges a failed activation by its cause, in
this order: a release ends the session with no fault; a revocation of the
session's DSD keeps its fault; an occurrence dropped or handed off again whose
activation answered `errReplanned` or the sink's own fence is taken back
(`unadopt`) and the session goes on; anything else, a device's refusal beside a
drop among them, is the fault it was. A reconciliation that hands off again an
occurrence whose own sink an activation is under way for (`occ.claimed`)
revokes that sink as a drop does. Only a release and a drop together whose
activation answers the sink's own fence tell the release's place before the
drop's apart, a release while the dropped successor's prefill is written; the
fault of a revocation `Backend.Revoke` records itself while the sink is
registered, so no order of the classification reddens its case, which pins the
outcome. A cut reaches the window through the backend's Cut alone: the engine
replaces a successor it has not submitted through Prepare, so after a cut the
cases tell the engine by the edit that deletes the entry, and check that the
session goes on. `TestASuccessorDroppedAtItsSinksAdmissionIsTakenBackUnwritten`
(an edit, a write's replacement, a write's hand-off with its replacement held, a
cut), `TestASuccessorDroppedWhileItsSinkWritesItsPrefillIsTakenBack`,
`TestAnActivationsFailureBesideADropIsJudgedByItsCause` (a release at the
admission, a release in the prefill, a revocation, a device's refusal) and
`TestTheActivationsOwnFenceIsErrActivationFenced` (the sink's answers) pin it.
