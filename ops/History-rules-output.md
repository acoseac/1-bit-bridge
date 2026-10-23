# History — the box as an audio output

## The ALSA reader, the MPD client and `bridge output`

### ALSA scalar values use the value grammar, not the dotted-key grammar (2026-10-23, B516)

**Keep integers, reals and strings distinct; never turn an unmodeled value
into a resolved chain, lose a known softvol or route warning, or weaken the
top-level hooks/include guards.**

`lexPCM` distinguishes a definition's key from its value. In keys a dot
separates components (`ttable.0.0`); in values it belongs to the scalar
(`0.5`, a library filename). Braces hold definitions and brackets hold
values; an iterative stack bounds nesting by the input, without recursion.
The key grammar stays restricted. Value tokens stop at ALSA's blanks or
`= , ; { } [ ] ' " \ #`; a leading dot is refused. Includes are not read.
The existing quote/backslash escape subset stays unchanged, with either
quote character delimiting a value.

The basis is alsa-lib `src/conf.c`'s `get_string`, `get_freestring` and
`parse_def`, and its `safe_strtoll`/`safe_strtod` conversion convention:
a bare token beginning with a digit or minus is tried as a base-zero signed
integer, then a real, consuming the entire token; failure is an error, not
a string fallback. Other bare words, including paths, are strings. Quoted
numbers are strings, and a leading plus is a string. The source was not
fetched in this offline session: these are the known source rules, also
used by this file's existing `confInput`, not a fresh differential check
against an installed alsa-lib. Go-only numeric underscores and binary/octal
prefixes are refused; hex and octal integers retain their C numeric value,
and reals have their own token kind. Numeric overflow and underflow to zero
are refused. Locale/libc-specific numeric edges remain unverified.

A real or unsupported string is consumed but remains opaque for resolution.
A numeric `type` or `slave.pcm` is not a string reference, and a negative
integer card is not a card ID. Repeated types remain uncertain even when
the second value is newly accepted. Unknown fields keep their PCM opaque.
`SampleStage` still names the first known sample-changing stage through
empty/plug stages, including softvol with min/max dB and plug with a real
transfer-table gain; it never walks through that stage. The verdict is
unchanged: these are `alsa_conf` warnings beside format information, and
bit exactness remains `not_verifiable`. The Volumio capture still resolves
through empty, plug and hw. No wire shape changes.

Five synthetic `testdata/asound-values/*.conf` files independently cover a
real gain, negative real, negative integer, library path and dotted library
word. `TestPCMValueFixturesKeepResolutionAndStages` resolves the unaffected
PCM and exact chain and keeps the sample stage while refusing its chain;
`TestPCMValueFixturesKeepWarnings` loads those same files through the real
verdict. `TestPCMScalarKindsFollowALSA`, `TestPCMValueKindsDoNotGrantResolution`,
`TestPCMValuesStillRefuseUnsafeFiles` and `TestPCMValueDelimiters` pin types,
opacity, refusals and boundaries. `FuzzPCMConfigValues` adds those tokens to
parser/resolver checks; there was no existing PCM-file fuzz target.

The five fixtures failed against 3f805199's parser and lost all five
warnings in a scratch-only Go 1.27.1 harness; they pass with this change.
The repository's Go test/vet commands could not start because go.mod
requires Go 1.27.2. The harness omits unrelated sysfs/FIFO tests and does
not substitute for the repository gate. The task report carries commands,
negative controls and the exact verification limits.
