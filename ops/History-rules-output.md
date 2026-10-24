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

**Read cmdline only for one resolved MPD process whose owner thread matches an
enabled live output, through the injected proc tree, bounded to 64 KiB and
inside the config deadline. Never interpret shell text, read the thread id's
cmdline as the process's, print argv, or follow a path outside the root. Use
the first non-option absolute config argument, else MPD's default search with
the injected HOME/XDG inputs; an unusable cmdline or file keeps the configured
fallback. Keep this information and its warnings separate from the existing
DoP veto and song mapping.**

`mpd.configProvenance` is an optional additive sibling of `mpd.config`, so it
can name the attempted fallback even when that file could not be read. Its
`source` is `process_cmdline`, `mpd_default` (MPD's default), or
`configured_fallback`; `path` passes both files' password redaction and the
bridge password's. A step abandoned at its deadline has no provenance.
Discovery and both reads share the existing single-outstanding config step.
The process is the owner's Tgid, never the ALSA status's owner_pid (a thread).
No owner, a foreign process, no matching live output, missing proc root,
unreadable MPD, or multiple MPD process ids means no cmdline read.

Proc argv is NUL-separated, with one final separator, no empty arguments,
and at most 64 KiB. Known no-value options and `--` precede the one config
filename. Unknown options, extra operands, relative paths and backslashes
are conservatively unmodeled and fall back. A relative filename cannot be
resolved from the current cwd reliably after MPD daemonizes. Reads use
`conf.ReadLogical` and `fsutil.OpenAsFile`, so a directory, pipe or device is
refused, and a symlink outside the injected root is refused. Config paths
are logical absolute paths under the tree beside the injected proc tree;
the configured fallback retains its existing root and Windows-volume rule.

The default order is the Linux USE_XDG order already implemented by
`conf.Locate`: the existing absolute XDG config directory's `mpd/mpd.conf`,
else the existing home's `.config/mpd/mpd.conf`; then `.mpdconf`,
`.mpd/mpd.conf`, and `/etc/mpd.conf`. An existing XDG directory suppresses
the `.config` alternative even if its file is missing. The first existing
file candidate is read. The inputs are the bridge's injected HOME/XDG
values, not a claim to have read MPD's private environment; a daemon started
with other variables can still select another default. This is information.
MPD also reads config at startup only: the process path does not prove that
today's bytes are what it loaded. The existing process-name/output-thread
tie does not distinguish a second daemon with matching output names.

The source basis is MPD 0.24.4 `src/CommandLine.cxx` `ParseCommandLine`,
`src/fs/glue/StandardDirectory.cxx` `GetUserConfigDir`, and `doc/mpd.1`'s
`mpd [OPTION...] [CONF_FILE]` synopsis, from source knowledge and the
existing search rule, without network verification in this task. Exact
option spellings and parser behavior require verification against MPD
before merging. Tests drive the reading and the real HTTP handler with a
recorder, so no listener or host proc/sys read is needed.

The configured file is retained for the existing song-row mapping and the
DoP fallback veto when the daemon supplies no live `dop`. The selected file
supplies the displayed config, its output/chain, and config warnings. No
signalpath judging rule changes. The include correction below applies to
both reads, as it does to every config consumer.

### Linked MPD files keep the named directory for includes (2026-10-24, B515)

**An include is relative to the named path's directory, bounded, and not a
glob. A linked main file or include keeps its named directory for the next
include; only cycle keys use the symlink-resolved file. Never weaken the
root checks or optional-include refusals to fix the directory.**

For `/etc/mpd.conf` linked to `/opt/mpd/mpd.conf`, `include "x.conf"` names
`/etc/x.conf`. This applies recursively to linked includes too. `ReadFile`
keeps the named parent directory before resolving the file, resolving the
parent itself to handle root aliases such as macOS `/var`. The include
resolver returns the named host path after the same confinement checks;
`prober.include` resolves its separate stack key so a link back to a file
already being parsed is refused before its contents are parsed twice.
Missing optional includes still pass through `open` to `errNotFound`;
existing outside-root targets remain refusals for both directives.

The source basis is MPD 0.24.4 `src/config/File.cxx`'s `ReadConfigFile` and
include handling, using the named config path's parent. This task had no
network access: the behavior is given in B515 and matches source knowledge;
the exact helper spelling and version should be checked before merging.
`TestLinkedConfigIncludesUseTheNamedDirectory` covers main and nested links;
`TestLinkedConfigIncludesKeepConfinementAndCycleChecks` verifies that a cycle
is refused before a repeat parse and that an outside-root optional include
still produces a problem. Existing include depth, file-count and pattern
bounds remain in place.
