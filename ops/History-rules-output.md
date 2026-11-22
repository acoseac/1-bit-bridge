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

- **`GET /v1/outputs` lists MPD's elements only while `output.mpd` resolves on and the host is Linux (the MPD gate, `localOutputs.Active`); the bridge's own player's element, kind `bridge`, is the next bullets' (B470 PR 5b).** `on` is on even when nothing greets; `auto` is on when a configuration was read or a daemon greeted; anything else, and every host that is not Linux, lists no MPD element, and is `404` `local_outputs_not_supported` with no `localOutputs` flag unless the player's gate holds. The DTOs live in `internal/api`. **Don't encode an `alsa` or `mpd` type from the handler.** `kind` is an open string, `mpd` or `bridge`; a client ignores a kind it does not know. `mpd` is a pointer, present only for kind `mpd`, and omitted for kind `bridge`; `bridge` is one present only for kind `bridge` (`omitempty`), so a kind `mpd` element's bytes are what they were before kind `bridge` (`TestAnMPDElementsBytesAreMains`). `mpd.version` is empty when the daemon is down. Each unresolved candidate card is its own element, with the same `mpd` and `pathRules` and a different `id` and `dac`. `isMPDsOutput` is `true` or `null`, never `false`. `mpd.loopbackBaseURL` is the loopback track route's base URL (`http://127.0.0.1:<port>`), present only while the route serves: additive, so a client decodes it as optional. `addresses` are the advertised LAN IPs on which MPD's port greets `OK MPD`; loopback and link-local (`fe80::/10`, `169.254/16`) are never listed, the probes run together, and each probe has one deadline covering the dial and the read. None means `reachableFromLAN` false. The id is the SHA-256 of the data-directory identity (`OutputIdentity`, the same directory `deriveDLNAUDN` hashes, without the friendly name), the MPD endpoint, the card's ALSA id and its USB id, never the TLS fingerprint, the library name or the card index, and it is stable across restarts and certificate renewals. The session opens on the first call and closes about 60s after the last call, on a clock the route takes as `Now` and `AfterFunc`. The idle close does not run while a build is in flight; that build re-arms the timer when it finishes. One build runs at a time, on a context detached from the caller with its own deadline (about 15s), and a success is reused for 500ms. A build that hits that deadline is not cached, and a caller that disconnects does not cancel the waiters. **A build that panics completes its flight all the same** (`Response`'s deferred completion, B473 B3 round 4): with an error, logged without the panic's value (`logOutputPanic`) and never cached, so the next call builds afresh; until then a panic there (a `find` that panicked, which drops the session) left the flight open, and every later call, the console's checklist with them, waited out its own context. **Don't dial from `/v1/health`.** The flag's auto probe is a separate short cache, keyed on one read of `output.mpd`, endpoint, conf and password, and it runs on a context detached from the caller so a cancelled request does not store a miss. Concurrent callers for one key share that in-flight probe. A result whose own deadline fired is not stored. **The inspection runs on a goroutine of its own** (`inspectBounded`, B473 B3 rounds 2 and 3): it reads `mpd.conf` and the player's files, which a dead mount holds whatever the context says, so it is waited for until its deadline and `outputProbeGrace` (100 ms, for a dial that ends with its context) more. One that has not returned by then, a probe's that finds one still running (no probe starts a second), and one that panics (logged) are abandoned: no report and no verdict (the loopback route's bullet), since a file that hangs is no evidence MPD has gone. `TestLocalOutputsGate`, `TestOutputsJSONKeepsEveryKey`, `TestOutputKindIsOpen`, `TestOutputsLANAddresses`, `TestLANProbesRunTogetherAndSkipLinkLocal`, `TestGreetDeadlineCoversDialAndRead`, `TestOutputsCacheAndIdle`, `TestProbeCacheFollowsTheConfig`, `TestProbeCachesTheConfigurationItProbed`, `TestProbeMissesShareOneFlight`, `TestProbeDeadlineIsNotCached`, `TestCancelledProbeIsNotCached`, `TestIdleCloseSkipsABuildInFlight`, `TestOutputsOneBuildInFlight`, `TestCancelledCallerDoesNotSpoilTheBuild`, `TestBuildDeadlineIsNotCached`, `TestOutputsIDStableAcrossRestarts`, `TestOutputIDIgnoresTheCardIndex`, `TestIsMPDsOutputIsTrueOrNull`, `TestAProbeThatHangsEndsTheReadingInTime`, `TestAnInspectionThatNeverReturnsIsNoVerdict` and `TestAPanicInTheOutputsBodyClosesItsFlight` pin it.

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

**Install the view, and the DSD choice's route over the same view
(`api.Server.WithBridgeDSDChoice`), before the API listeners start, whether or
not the plan starts the player; read `ready` before `eng` and `id`, the
surface's pointer under `p.mu`, and the identity file afresh until the engine
runs. Never read `playerOutput`, make or cache the identity, or let the view
start, probe MPD, dial or write anything but the DAC's choice the route hands
the player's backend.**

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

The DSD choice's route (B728 B9 PR 3b) takes the same view:
`apiSrv.WithBridgeDSDChoice(playerView)` is installed beside
`WithBridgeOutput`, so `bridgeOutput.Lists` answers by the identity `facts`
reads, and `bridgeOutput.ChooseDSD` reads `ready` before `eng` and `be`, as
the rest of the view does. The one thing it writes is what the route hands it:
the DAC's choice, through the engine's configuration reservation and the
player's backend, whose write it is (`output.md`'s route bullet).
`TestServeListsItsPlayerOnAGateOfItsOwn` asks serve's `/v1/health` for
`bridgeDSDChoice` beside `bridgeOutput`.

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
samples saturated. The section's `check` member is B12's, and the write route
and its health flag (`bridgeDSDChoice`) are B9's: `/v1/health` advertises each
flag with its route (`TestTheChoiceFlagArrivesWithItsRoute`), and the token
on `dsdDAC` is the one the reading the view was made from minted (B9 PR 3b's
section, below). Every word is in a
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

## B728 B9 PR 2 — the write of the bound DAC's DSD choice while the player is stopped (2026-11-12)

The long forms of `output.md`'s bullet under "The player's DSD choice: its write (B728 B9)" and of its B853 bullet under "Native lifecycle and policy containment"; the record is in `ops/engineering-log.md` under that date. #1434's long forms of a write beside what plays went with its branch, withdrawn by the owner on 2026-11-10.

### The player's DSD choice: its write (B728 B9)

- **A choice's write (`playback.Backend.ChooseDSD`) is made only while the player is stopped (the owner, 2026-11-10: "Stop playback to change DSD"): it takes the configuration lease, under the backend's lock in the step that finds no session record, nothing holding the device, the console's test tone included (B601), and no release unconfirmed, and answers `playback_not_stopped` at once otherwise; while the lease is held a Start waits before it registers, no probe takes the device slot, a probe that held it being waited for, and no tone is admitted; the state's checks run inside the store's one write, one write at a time; and a write that landed and is not known to be durable holds the set to what both states play alike for the rest of the run. Its caller holds the engine's configuration reservation (the section before) across the write, which the engine's owner grants only while the player is stopped: the backend holds no record between a release the engine acknowledged and the Start that follows it. The write carries the token of the DAC its read side was computed for (`DACToken`, `dsdDAC.dacToken`: the reading's key, maker and firmware under the backend's boot key): one of no token's form is `bad_request` before the turn and the lease, and one that is not the write's own reading's is `dac_changed` right after the key's check, before the journal's `from`, the fence and the compare-and-set, the write being the bound unit's own entry alone. Its wait for the refreshes' lock, which a refresh holds across its reading of the DAC, ends at its request's context (`persist_failed`): given up, it lets go of the lease and the engine's reservation with nothing read, and no reading starts after it (`lockReading`); the lock's later takes, its publication's and its hold's end, follow no context. Each check after the DAC's reading publishes that reading first, and every answer carries the pair published as it answered. The refresh it asks once its hold of its reading has ended (`askChoiceRefresh`, `dsd_work.go`) is made, forced, by the refreshes' run that takes its mark, whatever that run's own context says (a later ask replaces the work kept), and its end is said on every way out, a panic's included (`Seams.ChoiceRefreshed`).** The run takes the mark before it asks its own context, and refreshes forced where it holds it (B9P3BFU's round 2, `61018df8`: `dsd_work.go:131-139` at #1525's merge, `6ee6102a`). Before it (`dsd_work.go` at `cb763203`, the commit before `61018df8`; the other files' lines below are the same there and at the merge), the work kept was the last asked (`coalesced.ask`, `dsd_work.go:36`), and the refreshes' run did nothing while its own context had ended, its take of the mark inside that check (`dsd_work.go:128-129`). A write's run kept while another refresh was held and replaced by a later ask was made by that ask's run, forced by the write's force, where the later ask's context was live as the run began; where it had ended by then, the run skipped: the write's refresh unmade, its mark (`choiceRefresh`, B9P3BFU's own) and the force left set, and the seam never run. In serve every refresh but a write's is asked on the supervisor's `work` (`cmd/bridge/serve_player.go:593-608`), a probe's and a migration's part's included, which ends with the supervisor (`serve_player.go:594`; `:621-622`, serve's player stopping; `:631-633`, its engine stopped by a panic). The mark was read for that seam alone, which production leaves unset (`backend.go:71-73`), so the flag left set was nothing a user could see. The refresh skipped was a real reading and publication of the policy, skipped only once the supervisor had ended, after which no refresh it asks runs: a later run on a live context took both flags and made it, forced, and where none came, what it would have published stayed unpublished. A run that began between the ask's mark and its force (`dsd_work.go:142-143`, the force set in `RequestRefreshDSD`, `dsd_work.go:120-122`) took the mark unforced: the idle rule (`dsd_policy.go:578-584`) could skip its reading and the seam ran all the same, while the write's own run, kept behind it, made the refresh, forced, with no seam. (Corrected by bridge lows 12: this said that a tick's replacement alone left the write's refresh unmade, and that the idle rule skipped the write's refresh.) The write (`dsd_choice.go`, the backend's half of `PUT /v1/outputs/{id}/dsd`, whose route is B9 PR 3's) answers in this order: the choice's words (`bad_request`); the writes' turn (`choiceTurn`, one write at a time end to end, its wait bounded by the request's context, `persist_failed` past it); the writes of the player's DSD state closed by the player's stop (`dsdWrite`, `persist_failed`, nothing written; it spans the write from before its lease through its publish); the configuration lease (`takeLease`), whose refusal is `playback_not_stopped`, the console's test tone in the device slot among its causes (B601); a backend built with no DSD policy (`state_unreadable`), answered before the DAC is read, since that reading is the policy's (the brief review's finding 6); a DAC not available (`dac_unavailable`); another DAC (`dac_changed`: the key the wire carries, `wireKey`, and right after it the token the write carries against its own reading's, `hmac.Equal`, before the journal's `from`: another model, maker or firmware, another unit of the wire's key, or another boot's token; one of no token's form is `bad_request` with the choice's words, before the turn and the lease, the journal naming no mode; `TestAWriteCarriesTheTokenItsReadSidePublished`, at revision 0 and at equal revisions, behind the fence too); a state that did not open (`state_unreadable`); the fence of an earlier write that landed and is not known to be durable, any writer's, an evidence write's included (`choiceFenced`, `persist_uncertain`); and, inside the store's one write (`dsdstate.Store.Update`), `policy.Choose` on the state that write is made from: the compare-and-set (`stale`), the mode offered (`mode_not_offered`), DoP's confirmation (`dop_confirmation_required`) and first use (`first_use_unacknowledged`), a listener's rejection re-arming it. A `policy.ChoiceRefusal` keeps its word, and a stale one reads the policy again before it answers, so the revision it carries is the state's as its turn ended (review finding 3). `dsdstate.ErrNotWritable` is `state_unreadable`, `ErrLandedNotHeld` `persist_uncertain`, and `atomicwrite.ErrLanded` `persist_uncertain` with `landedChoice` recorded: every later reading of the box (`readDSD`, the supervisor's `RefreshDSD` every second among them) holds the set to what the state before the write and the one in the file play alike (`policy.Intersect`, the rest refused `dsd_choice_uncertain`) until the next start reads the file, and the store's fence refuses every later write. Anything else is `persist_failed`. The wait for the refreshes' lock, which a refresh holds across a reading of the DAC the kernel may hold in a USB control request, ends at the request's context (B728 B9 PR 3b's second round): a `sync.Mutex` takes no context, so a waiter on a goroutine of the backend's (`spawn`, which `Wait` joins) takes the lock, and one compare-and-swap decides who owns it, the write, which reads on its own goroutine before its lease is let go, or, its wait given up, the waiter, which lets the lock go having read nothing (`lockReading`; an uncontended lock is taken at once, `TryLock`). The write then answers `persist_failed` with the epoch standing and its turn, its lease and the engine's reservation let go, while the refresh is still held: a Start opens the DAC meanwhile, and once the refresh ends the write has read, written and published nothing (`TestAWriteWhoseTimeRunsOutAtItsReadingWritesNothing`, and through the route, the real engine and the real backend, `TestAWriteWaitingForARefreshAnswersWithinItsTime`). The lock's later takes, the publication's and the hold's end, follow no context: the write's hold of its reading keeps every refresh from reading the box under the lock until it has published. A written choice owes a probe (`oweProbe`); every check after the DAC's reading publishes that reading, from the state the write's decision read, and the answer carries the pair it stored (`publishChoice`), all before the turn and the lease are let go, so no later write can have moved past what an answer carries. The lease is `Backend.lease`, beside `holder`, `probing`, `waiters` and `terminalUnconfirmed`, taken under the backend's lock in the step that finds no session record (a session starting, playing, paused with its device open or released, releasing, or a retiring tail), no holder, no tone (`toning`: B601's test tone, which `reserveTone` refuses `choice_write` while the lease is held) and no release unconfirmed: a holder alone is not the check, since a Start registers before it takes the slot (the control that checked the holder alone went red in `TestAStartBetweenItsRegistrationAndItsHoldRefusesTheWrite`). `Release` registers a record too and never waits, its record holding nothing, and a Start that gave its wait for the lease up meets it in either order (`session.abandon`), letting its occurrence's reservation go, so no record is left to refuse every later write (review finding 1). A Start waits for the lease before it registers (`admitStart`), on its own goroutine with no lock held, and gives the wait up at its context's end or at its release (`StartRequest.Released`), holding nothing; admitted after the write, it plays by the set the write published (the fake DAC is given the new choice's format). `reserveProbe` refuses the slot while the lease is held (`ProbeChoiceWrite`); a write that finds a probe holding the slot claims the lease first and waits for the probe within its context (`persist_failed` past it), then reads the DAC. The lease spans the DAC's reading, the store's write, the publish and the answer's epoch, and is let go on every way out, a panic's included (`takeLease`'s deferred `letLease`). No backend or session lock is held across `Store.Update` and its fsync: the store's lock is taken alone and its closure is pure. What playback observes (a driver's acceptance, streaming observed, first use observed) never moves a choice's revision. The uncertain word is PROTOCOL.md's, and GET and the write's answer send `dsd_choice_uncertain` as it is (`TestAnUncertainChoiceGoesOutAsAWordTheProtocolNames`; review finding 5). The precondition is the caller's. The backend sees no record between a release the engine acknowledged and the Start that follows it (a seek, Next or Previous, PlayID or PlayIndex while playing, a skip after a refusal, a Play during a release to play), so it alone cannot keep a write out of the engine's transitions: the route's adapter (`bridgeOutput.ChooseDSD`) holds the configuration reservation, which the engine's owner goroutine grants in the one step that finds the transport stopped, across the backend's write, the write's persistence off that goroutine; `TestAPlayParkedBeforeItsBackendRegistrationRefusesTheWrite` parks a Play between the check and its backend registration (review finding 2). A crash after the rename and before the directory's sync may leave either choice; after the sync, the new one, which the next start reads and publishes, answered or not. #1434 built a write beside what plays over 19 review rounds (`interrupt`, `would_interrupt`, a pending set that every open, the producer, an activation and a resume waited for); the owner withdrew it on 2026-11-10, and none of it came back. **Don't write an entry but the bound unit's own as the write's own reading read it, check the token's form after the turn and the lease, or its reading's identity after the journal's `from` or the fence, wait for the refreshes' lock past the request's context, start a reading after that wait was given up, let a later ask's ended context or the idle rule skip the refresh a write asks, decide the lease by the device's holder alone, take it beside the console's test tone, register a Start or take the slot for a probe while it is held, let a Start wait for it under the backend's lock or past its context or its release, leave a Start that gave its wait up with a record or its occurrence's reservation, let the lease go before the publish and the answer's epoch or keep it past a panic, hold a backend or session lock across `Store.Update`, read the compare-and-set, the confirmation, first use or a rejection outside its closure, let an evidence write move a choice's revision, roll a landed write back or publish the state from before it, or bring back a write beside what plays (`interrupt`, a pending set, a wait for a write in an open, the producer or an activation).** `TestEachStateThePlayerIsNotStoppedInRefusesTheWrite`, `TestAChoicesWriteBesideAToneIsRefused`, `TestAStartBetweenItsRegistrationAndItsHoldRefusesTheWrite`, `TestAStartDuringAWritePlaysByTheChoiceItWrote`, `TestAStartGivingUpItsWaitForTheLeaseLeavesNoRecord`, `TestAStartIsAdmittedAfterAWriteThatDidNotLand`, `TestAWriteWaitsForAProbeUnderWayAndOwesItsOwn`, `TestAProbeAskedDuringAWriteIsRefusedAndStaysOwed`, `TestTheWritesChecksAreAnsweredInTheirOrder`, `TestAWriteCarriesTheTokenItsReadSidePublished`, `TestAWriteWhoseTimeRunsOutAtItsReadingWritesNothing`, `TestAWriteWaitingForARefreshAnswersWithinItsTime`, `TestNativeIntegrationAStartCompletesWhileTheRefreshAWriteWaitedForIsParked`, `TestTheRefreshAWriteAsksIsMadeForcedAndHeardEnding`, `TestTwoWritesOfOneRevisionLandOnce`, `TestARevisionMovedBeforeTheStoresWriteIsStale`, `TestAnEvidenceWriteRacingAChoiceMovesNoRevision`, `TestTheChoiceWritesLockOrderHolds`, `TestALandedWriteIsUncertainAndPublishesWhatBothStatesPlay`, `TestARefreshAfterALandedWriteKeepsTheIntersection`, `TestAnUncertainChoiceGoesOutAsAWordTheProtocolNames` and `TestAWriteThatPanicsLetsGoOfItsLeaseAndItsTurn` pin it.

### Native lifecycle and policy containment: the cause a stopped session's DSD answers (B853)

- **A session whose DSD was stopped refuses each later DSD entry for the cause recorded where it stopped (`revokeDSDLocked`, beside `nativeRevoked`, under the session's lock; B853): `native_revoked` with `Revoke`'s reason, or with the qualification authorization's; `backend_failed` with the session's fault for a contained reconciliation panic, which judged none of its occurrences; and `dsd_dac_unread` with `errDSDUnread`'s words once a reading of the policy failed, which stands whatever came first, its detail too. The cause answers in place of the policy's refusal and of a revocation's own reason: at an admission once the set is read (`admitDSD`), at each open's end before its readmission, whatever the set (`refuseRevokedLocked`: an entry admitted as DSD that nothing refused yet), on the harness's path before its selectors (`openNativeContent`) and again in the step that sets a selector's refusal or the session's cached one (`refuseAfterSelection`), every writer recording its cause before it revokes a permission, in the producer's gapless check, which tells its own revocation by what that revocation recorded (`revoke`, `revokedSeq`), never by the cause's words, in the authorization's reconciliation, whose panic is contained, recorded as a policy reconciliation's is, and revokes the authorization it was replacing, and in `Revoke`'s drop of a prepared successor; with nothing recorded each answers as it did (rounds 2 and 3).** Main refused every DSD entry of a session with `nativeRevoked` set as `dsd_dac_unread` (`admitDSD`'s two refusals), and a successor whose open ended after it as `native_revoked`, "DSD permission revoked" (`openSuccessor`), whatever set the flag: a failed reading of the policy (`stopDSD` from `refreshDSD`'s containment), a reconciliation's contained panic (`stopDSD` from `reconcileDSD`), an ordinary revocation (`Revoke`), or the qualification path's authorization replaced (`reconcilePermission`, which sets the flag itself and calls `Revoke` only where a native output is active: Codex's review of the brief, finding 4). So after a prepared DSD successor was revoked while the PCM before it played on, a later DSD entry was skipped as unread. The cause is recorded where the flag is set (`revokeDSDLocked`, under the session's lock): `Revoke`'s `native_revoked` with its reason; `reconcilePermission`'s `native_revoked`, the selected DSD policy no longer eligible; a reconciliation's contained panic `backend_failed` with the fault `faulted` records, the word `checkDSDActivation` answers for that fault, since the set did read and the session's occurrences were never judged against it, so neither `native_revoked` nor `dsd_dac_unread` says what happened; and a failed reading `dsd_dac_unread` with `errDSDUnread`'s words. A failed reading replaces whatever came first, word and detail, and nothing replaces it; otherwise the first cause stands. `dsdRevokedLocked` answers a copy of the record for both of `admitDSD`'s refusals and for `openSuccessor`. The rig's case, the queue of `TestARevocationOfAPreparedSuccessorLetsThePCMPlayOn` extended past `b` with a PCM entry and a DSD one, was red on main (the DSD entry skipped `dsd_dac_unread`), and the internal cases call `admitDSD` after each path and after two in either order. Controls: the two words swapped, and no cause recorded, each red in the rig and the internal cases. The other reads of `nativeRevoked` (the native open's error, the activation's fence, the producer's take-back) answer no entry's refusal and stay as they were. Round 2 (the same day, on the review of round 1) found the cause losing to the policy's refusal at three boundaries, and downstream on the harness's path. `admitDSD` returned `selectFor`'s error before its second check, so a refresh that removed the DAC's native DSD while an admission read the set, its reconciliation panicking, refused the entry `native_not_offered` for `backend_failed`: the cause is asked once the set is read and before anything the policy answers, the refresh's own refusal included, whose containment records the same words for the session it stopped. A successor's open asked the cause after its readmission, which refused it under a replaced set first, and a session's first occurrence was not asked at all, its readmission returning at once while the set was unchanged, so an ordinary revocation as it opened left the Start reporting its plan, starting a DSD monitor and failing at the native open, "DSD permission revoked", a device's failure: each open's end asks it first (`refuseRevokedLocked`), whatever the set, of an entry admitted as DSD (a plan or a conversion) that nothing refused yet, the condition `admitDSD`'s first check refuses by; a refusal the open made (the budget, DSD512, an empty decode, a transport) stands, and a tuple made unavailable on the harness's path, which records no cause, behaves as before. On the harness's path (an Authorization, no effective policy) the selectors answered a revoked permission as no approval (`dsd_not_in_this_release`): `openNativeContent` asks the cause first there (`occ.stoppedCause`). The producer's gapless check, where a revocation came while it checked the permission and dropped the successor, rebuilt that successor's refusal from the check's error; it keeps the cause recorded before its own revocation now. The authorization's reconciliation dropped before it recorded, and dropped again, for its own word, a successor an earlier revocation had dropped: it records first, leaves what is dropped, as `Revoke` does, and drops for the cause recorded. A panic in it reached its caller (the session's monitor, on a goroutine `spawn` recovers nothing on, or `RefreshAuthorization`'s) and recorded nothing: it is contained as a policy reconciliation's is (`stopDSD`, `backend_failed` and the fault, the session's lock through `lockHold`). And `Revoke` dropped a prepared successor for its own reason whatever stood, and drops it for the cause recorded. Each control that put a policy's refusal first again, or undid one of the harness's path's fixes, turned its own cases red alone, but for one line: the harness's check removed left the containment case's later entry green, `refuseRevokedLocked` refusing it at its open's end (a second layer: the containment revokes no permission, so the selector admits it). Named for the backlog: a revocation after the first occurrence's open has ended and before its native open still fails the Start at `openNativeOutput` ("DSD permission revoked"), a device's failure. Round 3 (the same day) found three more ways the cause lost. On the harness's path `openNativeContent` asked the cause and let go of the session's lock before a selector, or the session's cached refusal (an approval that did not read at its start), answered: a revocation or a replaced authorization in that gap recorded its cause and revoked the permission, which the selector answered as no approval, and `refuseRevokedLocked` passes over an entry refused already. A selector's refusal and the cached one are now set in the step that asks the cause again (`refuseAfterSelection`), and every writer records its cause before it revokes the permission (`Revoke` and `reconcilePermission` in one hold of the session's lock; the containment of a panic in `reconcilePermission` in `stopDSD`'s hold, then the revocation in a hold of its own; a session's start that finds a cause recorded, in its own hold; round 4: this said one step for every writer), so a selector that found the permission revoked finds the cause there, and a refusal set before a later revocation was made before it. The producer's gapless check took any cause in its own revocation's words for its own and answered its check's error: `revokeDSDLocked` answers the count of the cause it recorded (`revokedSeq`), 0 where an earlier cause stood, `Revoke` delegates to `revoke`, which passes it on, and the check's error stands only where the producer's own revocation recorded the cause and it still stands. The containment of a panic in the authorization's reconciliation left the authorization it was replacing live, both selectors admitting under it (the fault, which stops the producer, hid it): once the cause is recorded it is revoked, as a failed reading of it is, the containment holding no lock (the hold's deferred release lets go of it first). Each control that undid one of these turned its own cases red alone. **Don't answer every such entry `dsd_dac_unread`, pair that word with `Revoke`'s reason, let a later cause replace a failed reading's, set `nativeRevoked` but through `revokeDSDLocked`, let a policy's refusal (the set's word, a readmission's, a selector's no approval, a permission check's error) or a revocation's own reason answer where a cause stands, set a selector's refusal but in the step that asks the cause again, revoke a permission before its session's cause is recorded, tell a revocation's own cause by its words, drop again what is dropped, let the authorization's reconciliation panic into its caller, or leave the authorization a contained one was replacing live.** `TestADSDEntryAfterAnOrdinaryRevocationIsRefusedForIt`, `TestAnEntryAfterTheSessionsDSDStoppedIsRefusedForTheCause`, `TestARevocationDuringAnAdmissionIsItsRefusal`, `TestASuccessorWhoseOpenEndsAfterARevocationIsRefusedForIt`, `TestAnAuthorizationReplacedAsASuccessorOpensRefusesItForItsOwnReason`, `TestAReconciliationThatPanicsDuringAnAdmissionIsItsRefusal`, `TestASuccessorWhoseOpenEndsAfterAPanickedReconciliationIsRefusedForIt`, `TestARevocationAsTheFirstOccurrenceOpensIsItsStartsRefusal`, `TestARevocationDropsAPreparedEntryForTheCauseRecordedFirst`, `TestOnTheHarnesssPathAnEntryAfterARevocationIsRefusedForIt`, `TestARevocationBetweenTheCausesAskAndTheSelectionIsTheEntrysRefusal`, `TestAGaplessSuccessorsRefusalIsTheCauseRecordedBeforeItsCheckFailed`, `TestAnEarlierRevocationInTheGaplessChecksOwnWordsIsTheSuccessorsRefusal`, `TestAnEarlierRevocationsCauseStandsWhenTheAuthorizationIsReplaced` and `TestAPanicInTheAuthorizationsReconciliationIsContainedAndRecorded` pin it.

## B601 — the console's test tone in the player's backend (2026-11-15)

The long forms of `output.md`'s bullets under "The console's test tone in the player's backend (B601)"; the record is in `ops/engineering-log.md` under that date.

### The console's test tone in the player's backend (B601)

- **The console's test tone (`playback.Backend.Tone`) is the third holder of the backend's device slot, beside a session and the silent probe: counted among the backend's goroutines first (`toneEnter`, under `gmu` alone; `serve_stopping` once `CloseTones` has run), then admitted in one step under the backend's lock that refuses by word and never waits (`reserveTone`), the count given back on a refusal; it gives the slot back only once its node's close has answered (`endTone`), and `Tone` answers `stop_unconfirmed` past `ToneStopWait` with the slot still held** (2026-11-15, B601). A console click plays a quiet sine through the DAC while serve runs its own player on that DAC, so the tone needs the device as a session does, and the backend has one place that says who holds it: the slot a session's Start (`hold`) and the silent probe (`reserveProbe`) take under the backend's lock. An open that passes says nothing of the player: a pause that let the device go fails the player's next resume, Start or probe, and on a DAC with a second free substream (an onboard codec) the tone would play beside the music. So the tone is the slot's third holder (`toning`, beside `holder` and `probing`). It is counted before it takes the slot because serve's stop joins only what the backend counts (`Wait`, `Running`): a tone that took the slot uncounted could open the DAC after `Wait` returned. The count (`toneEnter`) is taken under `gmu` alone and refused once `CloseTones` has set its flag there, as `wclosed` refuses the DSD state's writes; the slot (`reserveTone`) under `b.mu` alone, refusing in this order: the gate shut (its word), a release never confirmed (`release_unconfirmed`), a probe (`probe_running`), a choice's lease (`choice_write`), a Start waiting (`start_waiting`), a session's record or the device's holder (`session_holds`), another tone (`tone_playing`), and the request ended already (`request_ended`). A refusal gives the count back before it answers. `CloseTones` sets its flag under `gmu`, then, under `b.mu`, shuts the gate for good and ends the tone in the slot: a tone counted before the flag is refused at its slot or ended, and `Wait` joins it either way; the two locks are never taken one under the other, the order `publishProbe` keeps. The tone's context is `context.WithCancelCause` of the request's, and its record's `done` closes only in `endTone`, the step under the lock that gives the slot back once its node's close has answered. `Tone` answers the run's result; once its context has ended it waits at most `ToneStopWait` (2 s, the engine's `ReleaseTimeout`) more, then answers `stop_unconfirmed`, and that timer (on `Config.AfterFunc`) marks the record (`stopUnconfirmed`) for `ToneStatus` until the close answers and the slot, the status with it, is idle again. The test seams (`Seams.ToneEntering`, `ToneCounted`, `ToneAdmitted`, `ToneClosed`) hold a tone before its count, after it, in the slot and after its close: `TestAToneBesideServesStopIsJoinedOrRefused` runs `CloseTones` and `Wait` at each of the first three, no open coming after `Wait` returned, and `TestEveryWayAToneEndsGivesTheSlotBackOnlyAfterItsClose` finds, at `ToneClosed`, the fake's close made and the slot still held, for each way a tone ends. **Don't take the slot before the count, take `gmu` under the backend's lock or that lock under `gmu`, wait for the slot, or give it back before the close has answered.** `TestTheTonesGateReadsTheDeviceSlot`, `TestAToneBesideServesStopIsJoinedOrRefused`, `TestEveryWayAToneEndsGivesTheSlotBackOnlyAfterItsClose` and `TestAToneWhoseCloseIsHeldKeepsTheSlot` pin it.

- **A Start preempts the tone (`hold`: ended `preempted` on the record found under the lock and waited for at most `ToneYield`, then the device busy, holding nothing); a probe it holds the slot from is skipped `tone_holds`, one of `probeSlotSkips`, and stays owed; a choice's write is refused `playback_not_stopped`; and the tone is refused at once beside a probe, a choice's lease, a Start waiting, a session's record or another tone.** A Start never waits on a tone it has not ended: a play the app or an OpenHome controller starts outranks a test tone. `hold` finds the tone in the slot under the backend's lock, ends it (`preempted`) on that record, counts itself among `waiters` and waits for the tone's `done` at most `ToneYield` (1 s, on `Config.AfterFunc`), as it waits for a probe at most `ProbeWait`; then it looks again and finds the slot free, or `terminalUnconfirmed` set by the tone's cleanup, or answers the device busy (`engine.ErrDeviceBusy`), holding nothing: a tone's close held in the kernel is the one case a Start cannot win. Its context and its release end the wait as they end a probe's. The silent probe the tone refuses (`ProbeToneHolds`, `tone_holds`) is one of `probeSlotSkips`: a refusal by the slot left out of that list is published as a probe made (`publishProbe`), and the unit would not be probed again until a replug, a migration's part or a choice; in it the probe stays owed, and the supervisor's next ask (`ProbeDSD`) makes it once the tone has given the slot back, which `TestAProbeBesideAToneStaysOwedAndIsMadeAfterIt` asks through `ProbeDSD`, and `TestTheProbesGateReadsTheDeviceSlot` has the row of a tone in the slot. A choice's write and a tone never overlap: `takeLease` refuses while a tone holds the slot (`playback_not_stopped`), and `reserveTone` refuses while the lease is held (`choice_write`). The tone never waits: beside a probe or a lease it answers at once, and in every state the player is not stopped in (playing PCM or native DSD, paused with the device open or let go, a native tail releasing, a release unconfirmed) it is refused, and admitted once a confirmed Stop has let the device go. **Don't let a Start wait for a tone it has not ended, publish a probe the tone refused as made, or admit a tone and a lease together.** `TestAStartPreemptsTheTone`, `TestAProbeBesideAToneStaysOwedAndIsMadeAfterIt`, `TestAToneBesideAProbeOrAWriteIsRefusedAtOnce`, `TestAChoicesWriteBesideAToneIsRefused` and `TestEachStateThePlayerIsNotStoppedInRefusesTheTone` pin it.

- **The tone's gate (`ToneOutput`) is serve's supervisor's (`toneGate`): shut before the engine hears a report that takes the output away (`withheld` for the live config's reason, `output_unavailable` for any other) or names a unit change (`output_changed`), at a stale snapshot while it is open (`snapshot_stale`) and at a report the engine refused, and opened only once the engine has taken an available report and the latest snapshot, reported first, is fresh; `CloseTones` shuts it for good; every end of a tone is a cause on its own context, set on the record found under the lock, the first standing.** The engine hears the output's availability only through serve's supervisor (`send`), and a tone plays only where the engine would: the gate is the supervisor's half, which the backend asks in the step that admits a tone (`reserveTone`). Its inputs are the report sent, the snapshot's freshness, and whether the engine took the report. A report that takes the output away shuts it before the engine hears it, `withheld` for a reason of the live config (`disabled`, `binding_changed`) and `output_unavailable` for any other, and so does an available report that names a unit change (`output_changed`), which no edge of availability shows: a tone of the old unit ends before the engine hears of the new one, and the next tone locates the new unit. A stale snapshot makes no report (the watcher republishes its last snapshot at its generation, marked stale), so the gate reads every snapshot the subscription delivers, a republish at the same generation included: a stale one shuts it while it is open (`snapshot_stale`), and never takes the place of another shut's word (`output_unknown` before the first report, `withheld`, `output_unavailable`). Whether it opens is asked after `p.report(snap)`, against the snapshot processed and the report the engine took, so a snapshot that is fresh again and also unavailable or re-enumerated opens nothing before `send` shuts the gate; a report the engine refused shuts an open gate (`output_unknown`) and opens nothing. The backend's gate starts shut (`output_unknown`). `CloseTones` shuts it for good (`serve_stopping`): serve calls it as its stop begins, so a tone answers its request while the console still serves, and again first in each teardown (`cli-serve.md`). Every end is `t.cancel(cause)` on the record found under the lock (`hold`'s preemption, `ToneOutput`'s shut, `CloseTones`), and a context's first cause stands: a late end of an earlier tone's request, which `context.AfterFunc` over `b.toning` would have turned on the tone in the slot then, ends nothing of the next. **Don't open the gate on a report the engine has not taken or before the snapshot is reported, let a stale snapshot take another shut's word, end a tone by a later lookup of the tone that plays, or open the gate after `CloseTones`.** `TestTheGateEndsTheToneForEachShut`, `TestAUnitChangeWhileAvailableEndsTheToneAndTheNextOpensTheNewUnit`, `TestALateEndOfAnEarlierTonesRequestLeavesTheNextPlaying`, `TestTheSupervisorShutsTheToneGateBeforeTheEngineHears` and `TestServesStopClosesTonesFirst` pin it.

- **A tone's cleanup is one close of its node, in a function with its own recover: confirmed where nothing was opened or `Close` answered nil; a close that answered an error, a cleanup that panicked before it answered, and an open whose own close answered an error (`alsapcm.ErrReleaseUnconfirmed`, which a refused open carries since B601) or that panicked set `terminalUnconfirmed`, the tone ending `release_unconfirmed`, which fences tones, Starts, probes and leases until serve restarts, said at Warn with the error the cleanup answered; a close the kernel holds keeps the slot.** The slot is given back only once the node's close has answered, so nothing opens the DAC while the tone may still hold it. Completion is nothing opened, or `Close` (which drops and frees before it closes) answering nil: `endTone` frees the slot, records the result and closes `done`. Failure is `Close` answering an error, the cleanup panicking before it answered, or an open whose own cleanup is not confirmed. `alsapcm`'s open closes a node whose setup it refused, and until B601 dropped that close's error: it now carries `ErrReleaseUnconfirmed` and the close's error beside its refusal, its Reason still the refusal's, so a PCM session's open goes on to its next packing by the Reason as before, the native sink's reopen hears its cleanup's failure, and the tone stops there and asks no other pair; a panic in `Config.OpenTone` leaves no device to close, unknown too. Failure sets `terminalUnconfirmed` in `endTone`'s step (and `toneFenced`, which `ToneStatus` says as `release_unconfirmed`), and the end is `release_unconfirmed`, said at Warn with the error the cleanup answered (the close's, or the refused open's), a panic keeping its own line, so an operator can see why the DAC is fenced. That flag fences, until serve restarts: tones (`release_unconfirmed`), Starts (`engine.ErrReleaseUnconfirmed`), probes (`ProbeReleaseUnconfirmed`, owed) and choices' leases (`playback_not_stopped`). On Linux close(2) lets the descriptor go whatever it answers, so a close's error rarely leaves the node held; but what the driver's release did is not readable from the kernel's headers alone (upstream, unverified), and the bridge cannot tell, so it fences rather than guess, the design's default for its open question 7. A close the kernel holds keeps `toning`: every reader of the slot reads it held, and `Wait` counts the goroutine. A panic in the run before the cleanup (a write, the builder) is `failed` and fences nothing when the cleanup's close answers nil; a panic after a close that answered nil (the note of the close, the end's log line) fences nothing either. **Don't give the slot back on an unknown, try another pair after an open whose cleanup is unknown, or fence for a panic after a close that answered nil.** `TestAToneWhoseCleanupFailsFencesTonesStartsProbesAndLeases`, `TestAToneWhoseCloseIsHeldKeepsTheSlot` and `TestARefusedOpenWhoseCloseFailsSaysItsReleaseIsUnconfirmed` pin it.

- **The tone is PCM alone: a 1 kHz sine at -30 dBFS or quieter (`tonebounds.Check`, which the backend and `testtone.Build` ask before anything) for 1 to 30 seconds, in the first of eight (rate, format) pairs the kernel commits exactly (48 kHz then 44.1 kHz: S24_3LE, S24_LE and S32_LE at 24 valid bits, then S16_LE at 16), each asked once; it writes nothing to the DSD state or the packing memory, publishes no observation and names no serial; `bridge output pcm-test` (`player.md`) shares `internal/player/testtone` with it and nothing else.** The ceiling is -30 dBFS because the player adds no gain after the samples (B470's packings widen without loss) and what follows the DAC the bridge neither reads nor writes: the page's warning and its -40 dBFS default stand for the analogue gain it cannot see. The bounds are `internal/tonebounds`'s, the standard library alone and outside `internal/player`, so the console's route checks the same bounds while `internal/admin` imports no player package; `testtone.Build` asks `tonebounds.Check` before it computes anything, so no caller builds a louder tone, and its arithmetic is pcm-test's, unchanged: the amplitude the floor of full scale times 10 to the level over 20, each sample that amplitude times the sine of the frame's phase, rounded, in whole cycles that repeat seamlessly. At -30 dBFS or quieter no frame's top byte is 0x05 or 0xFA, DoP's markers, at any rate from 44.1 to 384 kHz in any packing pcm-test allows. The kernel is asked a rate and a format alone (`alsapcm.Config`), so each of the eight is one pair: at 48 kHz, then 44.1 kHz, the 24-bit row of B470's table (S24_3LE, S24_LE, S32_LE at 24 valid bits), then S16_LE at 16; a refusal by `ReasonNotSupported` or `ReasonMismatch` tries the next, a busy or denied open ends `open_refused` with its `lastError` (`device_busy`, `permission_denied`, as `deviceCause` maps a Start's), any other refusal `open_refused`, and none taken `no_format`, a word of its own since B601's round 2, so the page tells a DAC that takes none of the tone's formats from one that refused to open. The node is checked after open (the unit's `Admit`) before a frame (`wrong_device`); whole repeats are written for its length, about a tenth of a second a chunk, then a drain within the buffer's time and two seconds. Nothing is written to the DSD state or the packing memory (`remember`, `forget`), and no observation is published: the tone itself changes neither `state` nor `playback` in GET /v1/outputs, as PROTOCOL.md's bridge section says, a residual the design names, and a play that ends the tone reads `playing` from its start, before the tone has let the DAC go (corrected by bridge lows 12: this said GET reads the player stopped while the tone sounds). Its two Info lines (at the open the unit's label, the format, rate, valid bits, level and length; at the end its word and frames) name no serial. **Don't put a DSD sink, a DoP packer or `dsdconv` on its path, raise the ceiling, ask a pair twice, or publish what it opened.** `TestTheToneRefusesALevelAboveMinus30`, `TestTheToneNeverCarriesADoPMarker`, `TestTheToneIsTheSineItsLevelNames`, `TestATonesPeakIsBoundedOnTheDevice`, `TestTheBackendRefusesALevelAboveMinus30BeforeAnyOpen`, `TestTheToneOpensPCMAloneBesideADSDSet`, `TestTheToneTriesEachRateAndContainerOnce` and `TestABusyOrDeniedOpenIsTheTonesLastError` pin it.

## B728 B9 PR 3a — the engine's configuration reservation (2026-11-15)

The long form of `output.md`'s bullet under "The engine's configuration reservation (B728 B9 PR 3)"; the record is in `ops/engineering-log.md` under that date.

### The engine's configuration reservation (B728 B9 PR 3)

- **A write that must not run beside playback (a DAC's DSD choice) holds the engine's configuration reservation, which the owner grants in the one step that finds the transport stopped with nothing under way (no session starting, live, paused or releasing, no release unconfirmed: `not_stopped`; maintenance and shutdown refuse it as every command, and maintenance entered while one is held is not refused, a residual: only the harness's `bridge output play` enters it): while one is held, every command whose rule starts a session, or will, is refused `configuring`, changing nothing, wherever the output's refusal is asked, after it; the grant is never lost to its caller (`ReserveConfiguration` asks the owner on a context that does not end, and lets go a grant that outlived its caller's context, answering a cancellation that wraps `ErrNotAdmitted`), the release is a command that answers once the lease is gone (a stalled owner delays that answer, as it delays every command: a residual), and its holder must let it go once its write has returned, however its request ended (the DSD choice's route's adapter, which holds it across the backend's write, B728 B9 PR 3b: `TestTheReservationIsHeldUntilTheWriteReturnsWhateverTheRequestDoes`).** The backend writes a DAC's DSD choice only while it holds no session's record (B9 PR 2's configuration lease, the bullet of the next section of `output.md`), and between a release the engine has acknowledged and the Start that follows it the backend holds none: a seek, a Next or a Previous, a PlayID while playing, a skip past an entry refused at its start, a Play during a release to play. A write let in there would run in the middle of a transition, so the check is the engine owner's, the one goroutine that runs every command, in the one step that finds the transport stopped. `Engine.ReserveConfiguration(ctx)` and `Engine.ReleaseConfiguration(lease)` are two commands of no lane, which never park; the owner keeps the leases held (`owner.configs`, a set, each lease drawn from the engine's randomness, never zero and never one held already), and the snapshot says whether one is held (`Snapshot.ConfigurationReserved`), never which. The grant is refused `not_stopped` unless the transport is Stopped, no session is starting, live, paused or releasing (`o.sess`), none waits for its release's acknowledgement (`o.sessions`: past `ReleaseTimeout`, `releaseDeadline` clears `o.sess` and keeps the session there) and no release is unconfirmed; the last two cover each other in every state the engine reaches, an unconfirmed release always naming a session still held, so each is the other's defence in depth, and only both removed let a grant in beside an unconfirmed release (the brief's first review, finding 1). Maintenance and the shutdown refuse it in `dispatch`, as they refuse every command; a backend call of no session (a Discard while stopped) holds no device and refuses nothing. A grant moves no generation: a controller reads nothing of it, and an `IfGeneration` it made before would conflict for a write that lasts a fsync. While a lease is held, `configRefusal` answers `configuring` wherever `outputRefusal` is asked, after it, so a command refused for the output keeps that word: in `transport()`, for every rule that starts a session or will (`rule.output`), before the move and `selectEntry`, so a refusal leaves the selection, the generation and the backend as they were; in `seek()`, for a seek that plays; and in `afterRelease`, for the play a release carries, which fails the plays waiting on it and leaves the transport stopped. While a lease is held the transport is stopped with no session, so only the first is reachable (a seek while stopped is refused `seek_unavailable` before the output's check, and no release is under way); the other two are defence in depth, which `TestTheReservationIsAskedWhereverASessionWouldStart` reaches through a lease a hook on the owner's goroutine puts in as no grant could. Stop, Pause, the edits, the modes, the output's reports, a seek with no track to seek in and Next past the end run as they would. The grant is never lost to its caller: `ReserveConfiguration` refuses a context that has ended already, as every command does, asks the owner on `context.WithoutCancel(ctx)`, so `do`'s waits end only with the reply or the engine's end and `WithAdmission`'s channel, a value of the context, still closes as the owner admits it, and lets go a grant that outlived its caller's context, by the release, a command, before it answers `ReasonCancelled` wrapping `ErrNotAdmitted` and the context's error: the one refusal of a command the owner admitted that wraps it, which `ReasonCancelled`'s, `ErrNotAdmitted`'s and `WithAdmission`'s docs and the package's say (the brief's first review, finding 4). `TestAReservationGrantedAsItsCallerLeavesIsLetGo` holds the owner at the `replying` seam, the lease granted and published and its reply not sent, ends the caller's context there and, where the command's context (the `admitted` seam's) could end, waits for the caller's return before letting the reply go (review finding D4: a hold at `checked` came before the grant). The release is a command, never a posted message: `msgs` is buffered and the owner's select between it and `cmds` is random, so a Play sent after a posted release could be taken first and refused `configuring` (H19); `TestAReleasedReservationIsGoneWhenItsReleaseReturns` holds the owner as it admits the release, where the release has not returned and the lease is still shown, and once it returns no reservation is shown and a Play is admitted at once (the brief's second review, finding 5). It is dispatched before the shutdown's check, so its holder can let it go in every state, and a lease not held changes nothing and publishes nothing (the brief's second review, finding 3). Rejected: a lock (the owner orders every command already, and a lock would be a second order beside it); an expiry (it could end while the write's fsync runs, which nothing interrupts, and a start would then run beside the write); a posted release (H19). The shutdown keeps the leases: nothing starts once it has begun, and a holder's release, dispatched first, still lets its lease go (`TestAReservationAfterRunIsClosedAndItsReleaseReturns`); after Run both answer at once. Residuals: `EnterMaintenance` is not refused while a lease is held, so a hold and a reservation stand together when maintenance comes second (the brief's second review, finding 1): only the harness's `bridge output play` enters maintenance (`cmd/bridge/output_play.go`), on an engine of its own, holding no reservation, and serve's player never enters it; and a stalled owner delays `ReleaseConfiguration` as it delays every command. The DSD choice's route's adapter (B728 B9 PR 3b, `bridgeOutput.ChooseDSD`) holds it across the backend's write, and lets it go once that write has returned, however its request ended. OpenHome's table answers both reasons 501 (`openhome.md`'s write bullet), and a transport write through the SOAP layer while one is held is 501, Id and TransportState as they were (`TestATransportWriteWhileTheConfigurationIsReservedIsRefused`). **Don't hold a lock for it, grant it beside a session, a release or maintenance, refuse a command whose rule starts nothing, park a start behind it, give it an expiry, post its release as a message, let a request's end let it go while its write runs, or put its lease in a snapshot.** `TestAConfigurationReservationIsGrantedOnlyWhileStopped`, `TestWhateverWouldStartASessionIsRefusedWhileAReservationIsHeld`, `TestTheReservationIsAskedWhereverASessionWouldStart`, `TestAReservationGrantedAsItsCallerLeavesIsLetGo`, `TestTwoReservationsHoldUntilBothAreLetGo`, `TestAReleasedReservationIsGoneWhenItsReleaseReturns`, `TestAReservationAfterRunIsClosedAndItsReleaseReturns`, `TestATransportWriteWhileTheConfigurationIsReservedIsRefused` and `TestAWriteBesideTheConsolesToneIsRefusedAndLetsTheReservationGo` pin it.

## B728 B9 PR 3b — the DSD choice's route (2026-11-16)

The long form of `output.md`'s bullet on `PUT /v1/outputs/{id}/dsd`, under "The player's DSD choice: its write (B728 B9)"; `cli-serve.md`'s clause on the route's wiring has its long form under "Serve's view of its player" above, and `wire.md`'s bullet is written whole there. The record is in `ops/engineering-log.md` under that date.

### The DSD choice's route (B728 B9 PR 3b)

**`PUT /v1/outputs/{id}/dsd` writes the bound DAC's DSD choice through serve's view of its player (`bridgeOutput`, which `api.Server.WithBridgeDSDChoice` takes beside `WithBridgeOutput`), its checks in PROTOCOL.md's order: the write's gate, its 404 word the outputs route's answer as it stands, MPD's asked without a probe; the id, the player's element (`Lists`); the body, one object of at most 4 KiB; then the adapter (`serve_player_output_dsd.go`): the words (`playback.ChoiceRequest.Check`), the player read ready and only then its engine and its backend (`dac_unavailable`, no epoch), the engine's configuration reservation (`playback_not_stopped`, or `persist_failed` for an engine closed or a request ended first, each with the epoch published now), held until the backend's write has returned whatever the request does, and the backend's write, projected as GET projects its facts (`bridgeDSDFacts`); the console's test tone runs outside the engine and itself changes neither GET /v1/outputs' transport nor its playback observation (a Play that preempts it reads playing from its start, before the tone has let the DAC go): the engine grants the reservation beside it, and the backend's lease refuses that write `playback_not_stopped`, as PROTOCOL.md's step 8, its error table and the bridge object's transport line say. The backend mints the token once a reading, in `readBox` (`Backend.dacToken`: HMAC-SHA-256 under a per-boot key of the state's key, the maker's stamp and the canonical firmware), which rides the reading's sets and the view; counts each store of its published pair, a new set or a changed view alone (`dsdPlayback.publication`, `publishLocked`, which the refresh and the write share); and a write holds its reading under the refreshes' lock until it publishes that reading from the state its decision read, a refresh meanwhile owed and asked after, answering with the pair it stored (`ChoiceResult.Epoch`) and a failure's cause where it occurred (`ChoiceResult.Err`). The handler answers every way out, the members by PROTOCOL.md's presence rule: a client gone is `writeErrorLog`'s 499 with the plain envelope, the route's deadline the server's 500 with the members.**

The route is the app's one way to write a DAC's DSD choice. Its checks run in PROTOCOL.md's order, and each refusal costs the least it can. The handler (`internal/api/outputs_dsd_choice.go`, `putOutputDSD`) asks the write's gate first: the player's (`bridgeOutputOn`) and the write wired (`WithBridgeDSDChoice`); its 404 says what `GET /v1/outputs` would, `output_not_found` while that route answers (the player's gate, or MPD's as it last answered, `LocalOutputsActiveNow`, which dials nothing) and `local_outputs_not_supported` while it does not. Then the id (`Lists`, the player's element as GET lists it: `bridge:` and the identity `facts` reads, never an MPD output's id), then the body: one object through `http.MaxBytesReader` at 4 KiB, `dacKey`, `dacToken`, `baseRevision` and `mode` pointers so that one missing or `null` is refused rather than read as a zero (a `baseRevision` of 0 matches a DAC with no choice), `maxBitRate` 0 and each consent false when absent, an unknown member ignored (a consent misspelled is a consent not given), and anything after the object refused (`dec.Token()` answers `io.EOF` alone). None of these calls the write.

Serve's adapter (`cmd/bridge/serve_player_output_dsd.go`, `bridgeOutput.ChooseDSD`) checks the words first (`playback.ChoiceRequest.Check`: a mode, a cap and a token of the forms the state and the backend take), which needs no player, so a malformed write answers `bad_request` whether or not the player runs. Then the player: `ready` read first and only then its engine and its backend, the order the view keeps; with none running, or a backend that makes no choice, the answer is `dac_unavailable` with no epoch, since there is no backend's publication to carry. Then the engine's configuration reservation (the section before): refused `not_stopped` or `maintenance`, the answer is `playback_not_stopped` (and beside the console's test tone, B601, which runs outside the engine and itself changes neither GET /v1/outputs' transport nor its playback observation, the engine grants the reservation and the backend's lease refuses the write with the same word, as PROTOCOL.md's step 8 says; a Play meanwhile reads playing from its start, since the engine publishes its transport before it calls the backend's Start, which ends the tone and waits for its close); `closed` or `cancelled`, `persist_failed` with the engine's error as its cause, which wraps the request's context's error for a client gone; each with the epoch the backend publishes now (`Policy`). The reservation is let go only once the backend's write has returned (a deferred `ReleaseConfiguration` on the request's goroutine): a request cancelled while the state's write runs leaves Play refused `configuring` until that write ends, and the answer is then what happened, a 200 for a write that landed whatever the client did. A panic in the write is contained twice over: a recover registered after the release runs first, so the reservation is let go after it, and one registered before the reservation covers the calls before it; each logs through `logPlayerPanic`, without the panic's value, and answers `internal` with no epoch, since the API's own recoverer logs a panic's value, which the player's rules forbid.

The token. A choice's write names the DAC its client read, and the key on the wire (`dacKey`) names a model, which two units of one model share, as two makers of one key can, and a unit keeps through a firmware change. The state keys an entry by the model and the maker's stamp, an unkeyed digest of the manufacturer string, which stays off every wire and log line: where the string holds a short serial, the serial could be found from the digest by trying each. So the read side publishes a token instead: `t1-` and the first 16 bytes, in hexadecimal, of HMAC-SHA-256 under a key of 32 random bytes the backend draws as it starts (`randomTokenKey`, beside its boot ID; `Seams.DACTokenKey` gives a test one of its own), over the domain `onebit:dsd:dac-token:v1`, the state's key in its canonical form, the maker's stamp and the canonical firmware, NUL between each (`Backend.dacToken`, the one function that computes one). `readBox` mints it once a reading, after the stamp, and it rides the reading (`dsdReading.token`) and every set made from that reading (`dsdSet.token`), which `viewOf` publishes on `dsdDAC` beside the key. The write compares the token it carries with its own reading's (`hmac.Equal`) right after the key's check: another model, maker or firmware, another unit of the wire's key (two units whose product strings hold their serials share a wire key and not a state key), or another boot's token is `dac_changed`, and the answer carries the token of the reading the write made, from which the user chooses again. A token lasts one boot of the backend: after a restart a write answers `dac_changed` with the new one. An empty or unreadable firmware, and a unit with no manufacturer string, give a token of its form, alike on both sides.

The publication counter. Two answers can carry one boot, one token, one revision and one generation and still differ: a change of the view alone (evidence, a first use observed, a warning, an option's offer) is published beside the set as it was, and a refusal moves no revision. So the backend numbers each store of its published pair (`dsdPublished.seq`, New's first 1): `publishLocked`, the one step the refresh and the write share, stores a new set at the next generation, or the set as it was beside a view that changed, and in either case the next number; a reading that changes neither stores nothing and moves nothing. `PlayerPolicy.Publication` carries it, and `bridgeDSDFacts` projects it as `dsdPlayback.publication`. Publications are made one at a time under the refreshes' lock (`dsdMu`), in the order of their readings, so within one boot a larger number is a later publication.

The write's reading. A refresh reads the box and publishes under `dsdMu`; the write takes the same lock to read the clock, the DAC (`readBox`, keeping why it could not read one) and the state, marks its reading held (`choiceReading`) and lets the lock go, so a refresh that comes meanwhile (the supervisor's, a probe's) reads and publishes nothing and is owed (`refreshOwed`): no publication comes between the write's reading and its own. The write checks its context once it holds the lock and again once the DAC is read, before it writes anything (`persist_failed`, the cause the context's error); the box's reading itself is not interrupted, nor is the state's write once begun. The decision keeps the state it read: the store's state with the reading, the closure's copy where the store's write ran, and the state written. Every outcome after the reading publishes that reading (`publishChoice`): the DAC absent for `dac_unavailable`, the reading's own unit for `dac_changed`, the landed intersection where a write landed, from the state the decision read, never one read after it; the answer is the pair the publication stored (`ChoiceResult.Epoch`), captured in the step that stored it, and a refusal answers with the options it was decided on. Once published, the write compares the pair the store's state now gives its reading with the pair it stored: where they differ (another writer's write came between the decision and the publication), a refresh is owed. An owed refresh is asked once the lock is let go (`RequestRefreshDSD` on a context its request's end does not end), and publishes the box as it is at a later number. A publication that panics is contained as a refresh's reading is: logged without its value, the next reading made fresh, the DSD sessions stopped (none can hold the device while the write holds the backend's lease), a refresh asked; the write's outcome stands, and its answer carries no epoch unless the pair was stored before the panic. The cause of every failure is kept where it occurs (`ChoiceResult.Err`): the context's error at the turn's wait and the probe's, `ErrDSDWritesClosed` once serve's stop closed the writes, the store's error for a write that did not land or landed and is not known to be kept or a state that cannot be written, and sentinels for a state that did not open and the fence.

The answer. The handler writes every answer, the deadline's included, since `withCtxTimeout` writes none and a handler that returned without writing would send an empty 200. Each outcome has its status, its code and one sentence of its own that names no path and no serial; 409 for a refusal, 500 for `persist_failed`, `persist_uncertain` and `internal`, 400 for `bad_request`. One helper writes both bodies (`answerDSDChoice`): the members while the answer carries the write's epoch, `dsdPlayback`, `dsdMode` and `allowDoP` together and `dsdDAC` while that epoch names a DAC, each through GET's DTOs and their lists' `[]`, and none otherwise; a 400 and an `internal` carry none whatever the write carried. Its status and code are its callers' constants, which the error table's guard reads at each call, and it is no `write*` name, which the guard would skip. A 5xx's cause, and a 4xx's, is logged at the status's level (`logRequestFailed`). `persist_failed` whose cause is the request's own context ended by its client is `writeErrorLog`'s 499, at Info, with the plain envelope nobody reads; the route's deadline (`dsdChoiceBudget`, ten seconds, a server's own in a test) is the server's, a 500 with the members at Error, and so is a cancellation while the request is still live.

Residuals. A token lasts one boot of the backend. A store's write that stalls holds playback off and the read side at its last publication for as long as it stalls, which fails safe, and serve's stop says a write left running. The response's write deadline can fail the answer of a write whose fsync outlived it; what the write did is in its journal line. The UI, and the console's own write (B13), are the app's and a later PR's.

**Don't send the stamp or an unkeyed digest of it, mint the token outside the reading, publish a reading the write did not make, let a refresh publish between a write's reading and its publication, answer with a pair the write did not publish, answer a refusal without its reading, take a server deadline for a client gone, return without writing, probe MPD for the PUT, or let `internal/api` import a player package for it.**

`TestTheTokenIsTheReadingsOwn`, `TestEveryOutcomeAfterTheReadingPublishesThatReading`, `TestARefusalCarriesTheOptionsItWasDecidedOn`, `TestNoRefreshPublishesBetweenAWritesReadingAndItsPublication`, `TestTheAnswerIsThePairTheWritePublished`, `TestAWritesReadingWaitsForARefreshUnderWay`, `TestAWriteWhoseTimeRunsOutAtItsReadingWritesNothing`, `TestAStateWrittenBetweenTheDecisionAndThePublicationIsPublishedAfterIt`, `TestAPublicationThatPanicsAnswersWithNoEpoch`, `TestTheCauseIsKeptWhereItOccurs`, `TestEveryPublicationMovesThePublicationCount`, `TestNoSessionMeetsAChoicesReading`, `TestThePUTsChecksBeforeTheWrite`, `TestEveryChoiceOutcomeHasItsAnswer`, `TestAServerDeadlineAndAClientGoneAreAnsweredApart`, `TestAPlayParkedBeforeItsBackendRegistrationRefusesTheWrite`, `TestEachTransitionRefusesTheWriteAtTheEngine`, `TestAWriteHoldsEveryStartUntilItReturns`, `TestTheReservationIsHeldUntilTheWriteReturnsWhateverTheRequestDoes`, `TestTheDACTokenRoundTrips`, `TestAWriteFromAnotherUnitsReadSideIsRefusedThroughTheRoute`, `TestAFirmwareChangeBetweenTheReadAndTheWriteIsDACChanged`, `TestAMakerChangeBetweenPublicationAndGETIsOneObservation`, `TestNoStampReachesTheWire`, `TestTheWritesFailedPathsThroughTheRoute`, `TestTheAnswerIsTheEffectiveChoice`, `TestAGETAcquiredBeforeAWritesPublicationOrdersBeforeItsAnswer`, `TestTheChoiceAnswerShapes`, `TestEachChoiceAnswerShapeIsItsGolden`, `TestAWriteBesideTheConsolesToneIsRefusedAndLetsTheReservationGo` and `TestNativeIntegrationAStartCompletesWhileTheRefreshAWriteWaitedForIsParked` pin it.

## B728 B12 PR 2 — the DSD check's hold in the engine (2026-11-20)

The long form of `output.md`'s second bullet under "The engine's configuration reservation (B728 B9 PR 3)"; the record is `ops/engineering-log/2026-11/2026-11-20-b728-b12-pr-2--feat-b12-pr2-check-hold.md`.

### The engine's configuration reservation (B728 B9 PR 3)

- **The DSD check holds the engine through a hold of its own (`EnterCheck`), granted in the one owner step that refuses beside a configuration reservation, another hold, an unconfirmed release or an output unknown or unavailable, and, without `interrupt`, beside anything that starts, plays, is paused or releases; its grant answers only once its release is confirmed, no Stop came first and its expiry has not come, refused in that order otherwise, and an error leaves no hold the caller owns; the owner's cleanup may still be pending; its caller waits at most `ReleaseTimeout` and `checkMargin` (500 ms) from the admission, on the engine's clock, then answers `cancelled`, as at its context's end, one compare-and-swap deciding whether the owner answers it, and the owner ends a grant its caller gave up or did not take, once; under it every Stop ends the check and answers once the check has let go, or at `ReleaseTimeout` after the first parked, by the hold's own timer, the hold still standing** (B728 B12 PR 2; a residual: PR 4's route bounds its own call). The player's optional listening check (the design's §6.2) plays a chime through the bound DAC at the client's request, outside any session the engine runs, so it needs the engine held while it plays, and the one hold the engine had was maintenance. Maintenance's entry asks whether a hold is held and whether the caller's generation is current, nothing else, and a configuration reservation's grant moves no generation, so neither kept a check out of a DSD choice's write in flight: the check would have released, opened and written the DAC while the write held the backend's lease. Nothing decided "something plays", the design's `would_interrupt`, but the reservation's predicate. And under maintenance a Stop answered success and stopped nothing, so an OpenHome Stop during the chime would have answered success while the chime played on. `EnterCheck(ctx, interrupt)` is a command of no lane, dispatched beside `EnterMaintenance` ahead of the maintenance arm, and its one step (`enterCheck`) refuses in this order: a hold held (`maintenance`), a configuration reservation held (`configuring`), a release unconfirmed (`release_unconfirmed`), the output unknown or unavailable (`outputRefusal`: a check needs a DAC that is there), and, without `interrupt`, the transport anything but stopped with nothing under way (`not_stopped`): the reservation's own predicate, one function the two ask (`unsettled`), so a session starting, live, paused or releasing, one whose release is not acknowledged, and a transport paused with no session (an edit dropped the paused content, or a seek left the transport paused at its target) each refuse it. A refusal changes nothing, the generation with it. The reverse held already and stays: a reservation asked under any hold is refused `maintenance` (dispatch's maintenance arm), which the DSD choice's route answers `playback_not_stopped`. Granted, the hold is EnterMaintenance's step (`grantHold`, which the two share): every mutation closed, the waiting and parked commands answered (a pending play dropped), the session released, pending backend work dropped, and its reason "check", so a snapshot reads maintenance and OpenHome's TransportState Stopped; a seek's target and the track it kept go with the step (`settleSeek`: no seek is pending under a hold). EnterMaintenance itself is unchanged, the harness's, its Stop still answered at once. Asked with `interrupt` where no session holds the device (`o.sess` nil), the grant is answered at once, its release confirmed, which is right because no session's release is owed then but one left unconfirmed, which refused the hold first: `startSession` sets the session held and the sessions owed in one step, `releaseDeadline` lets the session go and leaves its release unconfirmed in one, `acked` takes it from the sessions owed before the transport lets it go and a late one clears the unconfirmed release as it does, the paused session's own release (`pauseOverdue`, `onDeviceReleased`) is the held session's alone, and nothing starts while a release is unconfirmed, so no transition reaches the other state (round 2: no code changed; `TestASessionLeavesTheSessionsInTheStepItsReleaseEnds` watches the owner through each way a session is let go). The grant is answered by `deliver` once the release it made, or found under way, settles: its Hold once that release is acknowledged; otherwise its hold ends first and the grant is refused, so a caller that gets an error owns nothing and cleans nothing up: the release past its deadline `release_unconfirmed` (the unconfirmed release still fencing new holds and Starts), a Stop that came while it waited `stopped` (no hold is handed out stopped), the hold's expiry, which `MaintenanceTTL` can bring first since it may be as short as a second, `lease_invalid` (the existing reason whose doc names a hold that expired), and the output going away or its unit changing the report's reason (`output_unavailable`, `output_unit_changed`); a Stop that came first answers `stopped` at the expiry and the report too, and `release_unconfirmed` outranks it. The grant's own answer asks the expiry too (B728 B12 PR 2's round 2): `deliver`, as the release settles, refuses `lease_invalid` a grant whose expiry has come, after the release's confirmation and a Stop, the order the expiry's own message keeps (`endMaintenance`), so a release acknowledged after the expiry and before that message reaches the owner never hands over the expired hold with its signal open, as round 1's `deliver` did (with `MaintenanceTTL` at a second, a release acknowledged at 1.5 s, before its 2 s deadline); a plain hold's grant keeps its contract. Each is answered before `endMaintenance` could answer the same command with a hold, a second answer being dropped (`command.replied`). The hold's stop signal (`Hold.Stop`, a receive-only channel, nil for EnterMaintenance's) closes once, at the hold's first end of any kind (its exit, its expiry, the output's report, which ends it so the backend's check stops on it, the shutdown) or its first Stop. Under the hold a Stop closes the signal and waits in a list of the hold's own, never as its one waiter, until the hold ends, answered success then as a Stop answers once its release is confirmed; or until `ReleaseTimeout` (2 s) after the first Stop parked, a timer of the hold's own on the engine's clock, when each is answered success, the hold held still, and every later Stop under it at once (the one way a Stop is answered while the check's hold stands: the check is told to stop by the signal, and no controller waits past a release's deadline for it); the shutdown answers them `closed` as it begins, before Run returns, so none waits for the engine's end. The timer's message carries its hold (`msgCheckStops`), so one a timer fired before its hold ended changes nothing under a later hold. The command carries its caller's context, so a context that ends before the owner admits it (its look, or the caller's abandonment by the admission's compare-and-swap) stops nothing: `ErrNotAdmitted`, no release asked. Once admitted it runs, the grant perhaps having released a session already, and its caller waits for the owner's answer at most `ReleaseTimeout` and `checkMargin` (500 ms) from the admission, an instant the owner sets as it admits the command (`checkGrant.at`), armed with the engine's clock's `At`, so the tests drive it: the grant is answered once the release it made, or found under way, settles, which comes `ReleaseTimeout` from the admission at the latest, and the margin is the owner's own time to take that message and answer, so an owner that has not answered by then is taken for stalled. That bound, or the context's end, answers `cancelled` at once without `ErrNotAdmitted`, what the check stopped staying stopped. The handoff is one compare-and-swap (`checkGrant.state`) between the owner answering the caller and the caller giving up: every answer of the check goes through `answer`, which queues a reply only on winning it, so a caller that gave up first is answered nothing, and the hold it would have been handed `deliver` ends itself, as an exit ends it (`endMaintenance`'s waiter, too, answers a caller that gave up nothing). A caller that loses the swap has an answer the owner has sent or will send: a refusal already sent it returns, and any other answer, sent or not yet sent (an owner can stall between its decision and the send), it gives back without waiting (`giveBack`: a message, `msgCheckGone`, put in the owner's queue, or, that queue full, posted from a goroutine of its own), which the owner takes in a later step than the one that answered, ending the hold granted to that command if it still stands (`checkGone`, by `maintHold.grantee`); a grant read once the caller's context has ended is given back the same way (`takeGrant`), so no hold is handed to a caller whose context has ended. The hold is ended once, by the owner, never by the caller (B728 B12 PR 2's round 2: round 1's caller waited for the reply or the engine's end, `command.kept`, with no bound of its own, and exited a grant its context outlived with `ExitMaintenance` on a background context, unbounded). No `engine.Reason` was added: OpenHome's reason table maps every reason. Rejected: the draft's `context.WithoutCancel` for the command, which admits a command whose caller left before its admission (its control: the check released what played for a caller already gone); a caller that waits for the reply with no bound of its own, round 1's, which a stalled owner held for ever, and a bound on a wall-clock timer, which the tests' clock never fires; the caller's own cleanup, an exit on a context of its own, which an owner stalled before admitting it refuses once it runs (the command abandoned), the hold left to its expiry, where the owner's cleanup is a message it takes, with nothing to admit; the surface asking the player before its Stop (the design's first draft), where one interception in the engine serves every Stop surface; and a Hold handed out with its signal already closed. Residuals: nothing in production calls `EnterCheck` yet (the backend's check and the routes are the next PRs), and the route that will (PR 4) bounds its own call to it, its request's deadline its own; EnterMaintenance is still granted beside a reservation, the harness's alone; a hold whose caller gave up past its bound stands, its signal open, until the stalled owner runs; and a stalled owner delays every other answer, as it delays every command. **Don't grant it beside a reservation, decide `would_interrupt` outside that step, hand over a grant past its expiry, answer a caller that gave up, let a caller exit a grant it gave back, or answer a Stop under it before the check has let go but at the hold's own timer.** `TestACheckHoldIsRefusedBesideAConfigurationReservation`, `TestACheckHoldDecidesWouldInterruptInItsGrantsStep`, `TestAPlayAfterTheCheckHoldsGrantIsRefused`, `TestACheckHoldIsRefusedWhileAReleaseIsUnconfirmed`, `TestAnInterruptWhoseReleaseIsNotConfirmedSaysSo`, `TestACheckHoldAskedByAGoneCallerStopsNothing`, `TestTheCheckHoldsGrantIsNeverLostToItsCaller`, `TestAGrantItsCallerGaveUpAsItWasSentIsEndedByTheOwner`, `TestACheckWhoseOwnerStallsIsAnsweredAtItsBound`, `TestAGrantReadAfterItsCallersContextEndedIsGivenBack`, `TestAStopUnderACheckHoldSignalsItAndAnswersAtItsExit`, `TestACheckHoldsStopDeadlineIsItsOwn`, `TestACheckHoldsEveryEndClosesItsSignal`, `TestACheckHoldThatExpiresBeforeItsGrantIsNeverHandedOver`, `TestACheckHoldExpiredAsItsReleaseSettlesIsNeverHandedOver`, `TestACheckHoldNeedsItsDACAndEndsWithIt`, `TestASessionLeavesTheSessionsInTheStepItsReleaseEnds`, `TestStopsParkedUnderACheckHoldAreAnsweredAtShutdown`, `TestTheCheckHoldNeverAutoplays`, `TestAnOpenHomeStopEndsTheCheckThroughTheEngine` and `TestOpenHomesOtherWritesAre501DuringACheck` pin it.

  **Its follow-up Lows** (2026-11-21, Bridge lows 13, backlog B971). A grant's expiry is asked on the caller's side too: `takeGrant`, after its context's check, gives back a grant whose hold's expiry has come on the engine's clock and refuses it `lease_invalid` with the owner side's own detail (`expiredBeforeGrant`: no caller needs to tell the two apart, holding nothing either way), so a grant the owner sent before its expiry and its caller read after it, the expiry's message not taken yet, is never handed over expired. The owner says the expiry once, whichever of the two it takes first: its own message (`maintenanceExpiry`, `logExpiry`), or the grant given back, `checkGone` saying it where the hold's expiry has come, the expiry's message then finding the hold ended, or never coming, the end having stopped its timer; a caller-side line was rejected, since it would be written twice where the owner's message comes first. "An error leaves no hold" became "an error leaves no hold the caller owns; the owner's cleanup may still be pending": a grant given back is ended by the owner in a later step. The goroutine `giveBack` starts when the owner's queue is full counts in `Engine.wg` alone, which the tests' join waits on (`harness.joined`), never in `live`, the goroutines the owner joins by their last messages, and `post` lets it go through `Done` at the shutdown and at `abandon`: test hygiene, no shutdown guarantee, since `Run` waits on `wg` nowhere and a production `Wait` would wait on the backend calls it abandons on purpose. Each arm of the caller's wait is forced in a case of its own (`seams.checkArm`, `grantWait.only`, alsapcm's `armPick` pattern), so a receive of the owner's answer that bypassed `takeGrant`, in the main select or after the engine's end, fails its own case. Two pins: a grant whose release is unconfirmed and whose expiry has come is refused `release_unconfirmed`, never `lease_invalid` (`deliver`'s order); and the sessions invariant's watch runs through `pauseOverdue` and `onDeviceReleased`. OpenHome's still clock counts the goroutines its at-once timers start, and its test's cleanup waits for them. `TestAGrantReadAfterItsHoldExpiredIsGivenBack`, `TestTakeGrantAnswersWhatItsCallerMayHold`, `TestAGrantReadAfterItsCallersContextEndedIsGivenBack`, `TestACheckWhoseReleaseIsUnconfirmedPastItsExpiryIsRefusedForTheRelease`, `TestASessionLeavesTheSessionsInTheStepItsReleaseEnds` and `TestAGrantGivenBackPastAFullQueueIsJoined` pin it; the record is `ops/engineering-log/2026-11/2026-11-21-bridge-lows-13--fix-bridge-lows13.md`.

## B728 B12 PR 1 — the check's chime (2026-11-21)

The long form of `output.md`'s bullet under "The player's DSD check (B728 B12)"; the record is `ops/engineering-log/2026-11/2026-11-21-b728-b12-pr-1--feat-b12-pr1-chime.md`.

### The player's DSD check (B728 B12)

- **The check's chime is `internal/player/dsdchime`'s: a fifth-order loop held to its generator's digest, packed, cancellable and bounded, modulated at each rate's own, its oscillators free of libm.** At DSD64, DSD128 and DSD256 the chime is 0.5 s of `0x69`, 0.7 s of 880 Hz on the left, 0.7 s of 1320 Hz on the right and 0.5 s of `0x69`, each tone's other channel `0x69` too. Each tone is modulated at its own rate by the fifth-order CIFF loop of the bridge's DSD fixtures, `modulate()` of `internal/transcode/testdata/gen/dsd_fixtures.py` line for line, each product rounded alone by an explicit conversion, which forbids a fused multiply-add, and the loop reproduces that generator's own digest of its libm-free rotation input (`c164f2ef…`, unpacked to one byte a sample before it is hashed). A higher rate's chime is never a lower one's with each bit repeated, a zero-order hold that moves `0x69`'s idle tone toward the band: against a lower rate's so repeated, about half the samples of each channel's tone section differ, as two streams modulated apart do, counted over the sections alone, since the pads and each tone's other channel are `0x69` at every rate, which differs from itself repeated in half its bits. The tones and their 20 ms raised-cosine edges are rotations by (cos, sin) pairs rounded to nearest from 256-bit arithmetic, with no libm call even at setup, so each rate's golden SHA-256 holds on every architecture CI runs (the test recomputes the constants with `math/big`; the package also passed natively as linux/386). The level is one constant, 0.5 × 10^(−30/20) of the one-bit range, the qualification harness's: −36.02 dBFS through the bridge's own conversion, 6.02 dB below "−30 dBFS of the one-bit full scale"; a DAC that maps 50 % modulation to full scale plays it at −30 dBFS, the console tone's ceiling. Each tone's 0.7 s opens with 10 ms of zero input, from which the loop at rest makes `96 69 69 96` over and over, a pattern as quiet in band as `0x69`, so the join to the pad before it is as clean as `0x69` itself, and closes with a zero-input tail whose length per rate and tone (`rates`' tail) is the first, from 20 ms on, whose join to the pad after it stands within 0.01 of `0x69`'s offset and at most −101 dB SA-CD. Every join, measured as the listening assets' `sdm.c` measures its own (the running sum's offsets from its local means over 0.5 ms either side, against literal `0x69`'s over the same samples, and the audio band against `0x69`), stands within 0.02 and at most −100 dB SA-CD, and the joins test searches for the tails again. With the shortest tail, 20 ms, the 1320 Hz tone's end reads −98.0 dB SA-CD at DSD64 and −98.5 at DSD128, and seeded TPDF dither in the tail was measured and refused: at DSD64 the dithered loop's own in-band noise left no seed under the bound. The planes are packed, bit 7 earliest, and allocated once at their size: 1,693,440, 3,386,880 and 6,773,760 bytes, a build's bytes at most the planes' and 64 KiB. A build asks the context before it allocates anything, so one on an ended context allocates nothing, and the loop asks it every 65,536 samples; an integrator past 4, far from the 0.7 the stable loop stays under, ends the build with an error, never a stream. A rate's chime is built on its first use and kept for the process: concurrent first callers share one build and one publication, the build runs on its first caller's context and publishes nothing once that context ends, a waiter's own end ends only its wait, and a chime kept is answered whatever the caller's context; `Get` answers each caller a copy of the chime kept (the zero `Chime` with an error, and an error for a build that made none, which `build` never does), so what a caller writes over its own leaves every other caller's as it was, and `Chime.ReadAt` copies into the caller's buffer, so no caller changes the audio another reads. Its in-band floor is measured, never compared: the loop's noise transfer function puts its five zeros at DC, 36.8 dB down at 20 kHz at DSD64, so a tone's residual lies 39.1 and 37.3 dB under it at DSD64, 70.6 and 68.8 at DSD128 and 100.7 and 102.9 at DSD256, held to 35, 65 and 95, and each 20 ms edge, the converter's samples less the intended programme, leaves its largest in-band component under the same floor: 41.1 to 106.6 dB under the tone, at least 6.1 dB inside it, where a fall whose rotation is not begun afresh reads 11.8 dB under the tone at every rate. It is inert: nothing in production imports it until the backend's check (B12 PR 3), and a listen on a real DAC is owed before any route exposes the check. **Don't repeat a lower rate's bits for a higher one, return one byte a sample, build without asking the context, or let the loop leave its bounds.** `TestTheChimeLoopIsTheGenerators`, `TestTheChimeHasItsShapeAtEveryRate`, `TestTheChimeIsModulatedAtItsOwnRate`, `TestTheChimeIsTheSameOnEveryArchitecture`, `TestTheChimeStopsWithItsContext`, `TestABuildOnAnEndedContextAllocatesNothing`, `TestTheLoopRefusesToLeaveItsBounds`, `TestTheChimeIsPacked`, `TestTheChimesJoinsToSilenceAreClean`, `TestTheChimesInBandResidualIsUnderItsFloor`, `TestConcurrentFirstCallersShareOneBuild`, `TestAWaitersCancellationEndsOnlyItsWait`, `TestACancelledBuildPublishesNothing`, `TestACancellationBeforePublicationPublishesNothing`, `TestNoCallerCanChangeTheChime`, `TestEachCallerHasAChimeOfItsOwn`, `TestGetOnAnEndedContextIsTheChimeKeptOrNothing` and `TestGetAnswersNoChimeWhereItsBuildMadeNone` pin it.

## B728 B12 PR 0 — a panic on the native sink's goroutines stays in the player (2026-11-22)

The long form of `output.md`'s last bullet under "Native lifecycle and policy containment"; the record is `ops/engineering-log/2026-11/2026-11-22-b728-b12-pr-0--fix-b12-pr0-alsapcm-recover.md`.

### Native lifecycle and policy containment

- **A panic on the native sink's writer or its opening worker stays in the player (B728 B12 PR 0): each is recovered on its own goroutine, typed `ReasonPanicked` and logged once without its value (`DSDConfig.Log`); every close of its node, DROP and HW_FREE included, has a recover of its own (`releaseRecovered`) and closes the node once whatever they do, so a close that panics is an unconfirmed cleanup and `Done` still closes; a reopen whose refused node's close failed answers it in `CleanupResult`; the worker closes a node it opened and did not hand over, then answers its caller or `CompletionResult`; and the backend fences the device after an open that panicked with its node's close unconfirmed. The residuals are backlog B987.** Until then the writer's own panic was already recovered, but untyped and unlogged (`native writer panic: %T`), and a panic in a close of the node or on the opening worker ended `bridge serve` whole, API, console, scanner and player, until its supervisor restarted it: the writer's one deferred function recovered and then closed, so a close that panicked was a new panic in a deferred call that nothing recovered, `Done` never closing; and the opening worker had no recover at all, while it opens the node, reads its setup and runs the caller's check after open (in serve, the backend's, the DoP gain check included). Both goroutines are alsapcm's, so no caller's recover could catch them. The PCM sink has kept this contract since B523 (`player.md`); the native sink keeps it now.

  The writer's own panic is an `*Error` typed `ReasonPanicked`, its value named only when it is the runtime's own error, logged once with where (`the native sink's writer`), the endpoint and the stack, so the backend reports it as its own fault, `backend_failed`, never `device_error`. An activation the panic cut short (`running`, the request under way) answers the record, that panic and any cleanup's failure, and never as one that never ran (`errNativeActivationCancelled`), the sink revoked by its end.

  Every close of the node goes through `releaseRecovered`, each of its steps under a recover of its own: `releaseNative` marks the node closed, then closes it from a defer, once, whatever DROP and HW_FREE do (round 2: a panic in either skipped the close and kept the descriptor open), and a panic of theirs and one of the close each answer as their failure, logged once, beside the other's, so neither replaces the other; each ioctl's result is kept before the next is made, so a panic in HW_FREE keeps DROP's error beside it (round 3: DROP's and HW_FREE's results were joined in one expression, which HW_FREE's panic left unassigned). `closeNative` takes it for each of its callers (the writer's end, a pause's release, and the four refusals of a reopen: its DoP precision, its geometry, its check after open and its fence), its `s.dev` nil first, so a close is never tried again; a close that panics is logged once (`the native sink's close`), recorded in `CleanupResult` beside `ErrReleaseUnconfirmed`, answered to the caller as a failed close is, and the writer's end runs on to `Done`. A reopen whose setup was refused and whose node's close, `openNative`'s own, then failed or panicked answers that failure in `CleanupResult` too (round 2: it reached the sink's result alone, so `Close` answered a clean cleanup and the backend let a successor open the device).

  The opening worker (`openNativeBounded`) holds the one recover of an open: a panic in the open, the kernel's setup, the check after open, or its cancellation arm is answered typed (`the native sink's opener`) through the reply's select, to a caller that waits, or into `CompletionResult` for one that has gone, never blocking, and no answer once its reply has handed the sink over (`delivered`, a test's seam, runs there; `openEnded`, another, once its recover has run). A sink made and not delivered is revoked, its writer woken past a revocation the panic cut short (which may have marked the release begun and woken nothing), and waited for, its result joined, before `Completion` closes. `openDSDSink` closes a node it did not hand over in one deferred close under its own recover, each refusal's failure joined to its answer as before, and reports a failed close to the worker (`nativeSeams.unhanded`), which answers it beside `ErrReleaseUnconfirmed`; it never recovers the open's panic itself. `openNative` keeps both failures of a setup that panicked: its close's failure goes on with the panic (`setupPanic`), which the frame that recovers it answers beside it, the worker, or the writer at a reopen, which records it in `CleanupResult` too.

  Each recovered panic is one Error line (`nativePanicMessage`, through `DSDConfig.Log`, nil the player's component, and the device `Config`'s `log` for `openNative`'s close), two panics two lines; `ErrPanicked` is in every error such a panic answers. The backend passes its logger in `native.go`'s `DSDConfig`, and fences the device after an open whose answer holds both `ErrPanicked` and `ErrReleaseUnconfirmed` (`releaseErr`, so the teardown sets `terminalUnconfirmed`): nothing opens the device again until serve restarts. An open's panic at a native session's start faults the session `backend_error`, the engine's word for a backend call that failed (`engine.ReasonBackend`), and a writer's panic `backend_failed`, the backend's own fault (`playback.ReasonFailed`): the two words are the engine's and the reporter's, deliberately apart.

  The residuals are backlog B987.

  **Don't call `releaseNative` on the writer outside `closeNative`'s recover, close the node anywhere but in `releaseNative`'s defer, leave a refused reopen's failed close out of `CleanupResult`, or answer an open's panic before its node is closed.** `TestANativeSinksCleanupThatPanicsStaysInThePlayer`, `TestANativeWritersOwnPanicIsTypedAndLogged`, `TestANativeOpenThatPanicsStaysInThePlayer` and `TestABackendSurvivesAPanicOnANativeSinksGoroutines` pin it, each case in a child process of its test (`RunEachCaseInAChild`), since a panic that escapes ends the test binary, which the parent reads from the child's output. A case of a release whose ioctl panics requires one error typed `ReasonPanicked` itself for each of its panics (`panickedErrors`, which walks both unwrap forms and counts each such `*Error` once): two panics answer errors with the same details and the same `ErrPanicked`, so `errors.Is` cannot tell one from two. The cancellation arm's control, the opener's recover with its wake of the writer dropped (C8 in the record), is not deterministic: red 18 times in 60 alone under `-race`, a known limit of that control, which cannot hold the writer idle before the worker's revocation, not of the fix.
