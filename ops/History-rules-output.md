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
alone; then hints run in their own single-outstanding bounded step, with
their own busy flag and 500 ms deadline, after the ALSA configuration and
MPD status bracket. A failed or late hint omits only hints. A failed or late
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
names under `/proc/` or `/dev/fd/`, `/dev/stdin`, and paths resolving there
omit hints. A hint worker that times out or panics cannot erase the already
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
