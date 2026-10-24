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


### The reading's informational MPD config names its provenance (2026-10-24, B515)

**Select per card from its one MPD Tgid and matching live output thread,
never another card's owner. Require stable start time around bounded cmdline,
argv[0] basename `mpd`, and the same filesystem root. Use MPD's bounded
HOME/XDG environment for defaults; unreadable evidence falls back even for
the same uid. Parse MPD's options anywhere, refuse `--`, unknown options and
ambiguous paths, and report `--no-config` as no file. Never claim a file not
read or expose argv/environ. Redact every external string against every
config read and output.password. Use the selected file for the no-live-dop
veto; retain configured song mapping.**

Round 2 corrects round 1's global owner scan, assumed bridge environment,
option grammar, attempted-file provenance, DoP input and incomplete
redaction. `alsa.Owner.PID` already is the Tgid, resolved from the status of
`Owner.TID` (ALSA's `owner_pid`). Only the output's card is eligible; a second
MPD process on that card is ambiguous. The start time (stat field 22,
parsing after the last `)` in comm) must agree before and after cmdline.
The basename of argv[0] must be `mpd`. The root links under the injected
proc tree must identify the same file, or, if they cannot be inspected,
the two mountinfo `/` entries must agree on major:minor and root. A known
mismatch is never overridden by the other check. Every operation stays in
the bounded, single-outstanding config step; the ALSA configuration is read
first, after the status bracket, so the picks already name the cards.

Cmdline and environ use `ReadLogical`, which already reads cap+1 bytes and
refuses overflow, including a NUL at the cap. Both caps are 64 KiB; stat is
also 64 KiB and mountinfo 1 MiB. Environ is never retained in the snapshot,
printed or logged. If it cannot be read, no default-source claim is made,
regardless of uid: services can override HOME even for the same uid. There
is deliberately no uid-based bridge-environment proxy. Empty readable
environment means no home/XDG candidates. Duplicate or malformed relevant
environment entries are refused. The default order remains the existing
Linux USE_XDG `conf.Locate` order; the bridge does not model another build's
compiled system config location.

MPD 0.24.4 `src/CommandLine.cxx` and `src/cmdline/OptionParser.cxx`, read
from the supplied source copies in round 2, establish the flags, options
anywhere, long `=value`, and rejection of `--`. At most one non-option
filename is accepted; `--no-config` returns before MPD inspects remaining
operands. Absolute file arguments reject backslashes, empty, dot and dotdot
segments before mapping to the config root. Relative filenames remain
unprovable because MPD can change cwd after loading them. No raw argv is
published; validated logical paths are redacted by the same rules as the
file's strings.

The optional additive `configProvenance` identifies only files actually
read: `process_cmdline`, `mpd_default`, `configured_fallback`. With no file
read it is absent, except verified `--no-config`: `mpd_no_config`, empty
path, null config. Config failures and expired steps never claim a file.
Each card keeps its own selection and chains in the shared snapshot, and
all files read supply redaction to every card's external strings, including
DAC mixer/control, kernel names, song row and provenance. The configured
file still supplies song mapping; selected config supplies information,
config warnings and the no-live-dop veto. No signalpath judging algorithm
changes. The process identity/output-thread tie still cannot prove the
network session is this process when two daemons use identical output
names, and current file bytes cannot prove startup settings.

Tests drive the production reading and real handler without listeners.
The round-2 regression cases cover identity refusal, mountinfo root and
device mismatches, root inode agreement/disagreement, default environments,
option and path grammar, cap-boundary truncation, unread provenance,
redaction and the actual no-live-dop veto. Controls and exact gate evidence
are recorded in the task's `report-r2.md` and `controls-r2/`.

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
Existing depth, file-count, read, non-file and pattern bounds remain.

`TestIncludesResolveDotDotPhysicallyAtEveryLevel` covers directory and file
links for main and nested files, with further nested includes.
`TestLinkedConfigIncludesUseTheNamedDirectory` and
`TestLinkedConfigIncludesKeepConfinementAndCycleChecks` retain the round-1
simple-link, cycle and outside-root controls.
