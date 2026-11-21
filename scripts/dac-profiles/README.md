# The DAC profile database's checks

`internal/player/dacprofile/profiles.json` is the DAC catalog: the bridge
player's reviewed database of DAC profiles (B728;
`ops/plan-b728-dsd-capability-design.md`, section 2), what is known of a DAC
model's DSD, keyed by its USB identity and embedded in the binary, and since
B730 B1 the one source of the iOS app's DAC registry, each row's iOS
projection. The package's own tests hold the file to its schema and its rules
on every test run: the matches, no two profiles of one identity, product
strings for shared reference IDs, a provenance line for every claim, DoP
evidence rate by rate, the iOS name keys, the closed caveat list, and the
file's canonical form. Two checks hold it to files outside this repository:
the kernel seed to the kernel's quirks, and the iOS projection, through its
export, to the iOS registry's baseline.

## The kernel seed: `kernel-check.sh`

A profile whose native claim comes from a kernel quirk names the Linux
kernel's `sound/usb/quirks.c` at the tag `internal/alsapcm/uapi.go` pins
(`const uapiKernel`), the one `scripts/alsa-abi` checks the PCM interface
against. `kernel-check.sh` fetches that file at that tag and runs
`TestTheKernelSeedIsThePinnedTags` with it, which reads the file's DSD format
quirk and its flags table and compares, both ways:

- every kernel-seeded profile claims an ID the tag gives a device-specific
  DSD quirk, with the tag's formats, its source and its native line naming
  the tag and how the driver finds the altsetting;
- every device-specific ID of the tag is seeded, or listed as a shared
  reference ID, which seeds nothing without its product string.

A vendor-wide rule names no model and seeds nothing, and a kernel-seeded
claim states no rate: its `maxBitRate` is 0, unspecified, never unlimited,
and the rates offered are the live altsets'. Nor does the kernel's naming of
one model make an ID that model's alone: a chip maker's reference ID or an
interface board's is shipped by other products too, so it is listed as a
shared reference ID and seeded with no profile until a capture gives its
product string (16d0:071a, the Amanero board; 0d8c:0316, a C-Media ID another
maker's firmware ships; 20a0:4143, the WaveIO board). The script fails unless
the test ran and passed (without its file it skips, and a skip looks like a
pass). CI runs it on linux/amd64, the `dac-profiles` job in
`.github/workflows/gate.yml`; by hand it needs curl and Go:

```sh
bash scripts/dac-profiles/kernel-check.sh
DACPROFILE_KERNEL_QUIRKS=/path/to/quirks.c bash scripts/dac-profiles/kernel-check.sh   # a copy you already have
```

The file is kernel source and is never vendored: the script reads it in a
temporary directory and deletes it, and prints its git blob id, which
`git ls-tree <tag> sound/usb/quirks.c` in any kernel clone shows for the same
bytes. The test logs the facts it read (USB IDs, DSD formats, how the driver
finds the altsetting) and none of the file's text, and the database copies no
code and no comment of it: a kernel-seeded profile is named by its ID.

To follow a newer kernel, move the pin in `internal/alsapcm/uapi.go` (see
`scripts/alsa-abi/README.md`), run this script, and seed, change or drop what
it reports.

## The iOS projection and its export: `ios-export`

Since B730 B1 the catalog is the one source of the iOS app's DAC registry. A
profile's `ios` object is its registry row in the app's words: its order (the
app's live matcher takes the first row whose patterns a port's name holds,
until iOS PR 3), its maker, its legacy patterns and ceiling (the row's
`matchPatterns` and `maxDSDRate` as the registry holds them, which iOS PR 3
retires), its port names (verbatim, a trailing space kept), its name tokens
and excluded tokens (iOS PR 1's strict keys, never the legacy patterns: the
KA3's token is `FiiO KA3`, not `KA3`), its scope (`model`, or `family`, whose
siblings' results are its sibling note's alone), its PCM ceiling, its volume
topology and its times in whole milliseconds; `iosGenericNames` are the names
that name no model, and `iosKnownSiblings` iOS PR 1's known siblings, models
with no row of their own, as the registry's baseline lists them, then those
a row added past the baseline brings (B944): the lint
refuses a catalog whose keys answer one of them with a model row through the
strict matcher, by a port name, matched exactly before any token and never
ruled out by an excluded token, or by a token none of the row's excluded
tokens rules out (`TOPPING E30 II Lite` and the E30 II's `Lite`). Every
legacy pattern, name key and generic name is printable ASCII. A profile with
no `match` is iOS's alone: it makes no claim only a match reaches, and
`Match` never knows it. The player reads none of the projection.

`ios-export` reads the embedded catalog and writes `DACCatalog.ios.json`
beside itself: the rows with `ios`, by `ios.order`, each with what the app
reads (the projection, the hidden-mode caveat, and the DoP evidence below)
and nothing only the bridge reads. Run it from the repository root after a
change of the catalog, and commit what it writes:

```sh
go run -tags ts_omit_webclient,ts_omit_debugeventbus ./scripts/dac-profiles/ios-export
go run -tags ts_omit_webclient,ts_omit_debugeventbus ./scripts/dac-profiles/ios-export -check
```

The second writes nothing and exits 1 when the file is not what the catalog
exports now; both print the file's SHA-256, which iOS PR 2 pins beside the
bridge commit it came from, so a catalog change is a bridge PR, then a
re-pin. CI's `dac-profiles` job runs `-check` and the catalog's tests. The
export's tests hold it to its golden and to the iOS registry's baseline,
`ios-export/testdata/ios-registry-3145606a.json`: a dump of the live
registry's objects at iOS 3145606a, pinned by its SHA-256, an input never
derived from the catalog. A parity failure is fixed in the catalog, never in
the baseline.

The baseline cannot grow with the catalog, so a row added past it (B944: a
row from a wizard submission, which no registry held) is held to its
declaration in the test instead: `additions` in `ios-export/main_test.go`,
its rows in the export's shape and in order after the baseline's, the names
its unit reported and the known siblings it brings, each name with the
answers both matchers must give it, and the rows declared strict-only
(B976): a row whose port name is too short for a legacy pattern that
leaves its sibling out (the Kiwi Ears AD1's `AD1`, inside its sibling's
`AD1 Pro`) has none, the live matcher answers its names with no row, and
the two matchers may answer apart its exact port names alone, as
declared; no other check is waived, and a declaration naming no row, or
a row so declared with a legacy pattern, is refused. A declaration is
written by hand from the row's submission and its maker's figures, never
copied from the catalog or the export, and changes only with what it was
written from. The parity
keeps every check of the baseline and replays its corpus over the whole
export, the added rows included, since a row added last in order can still
take a corpus name no earlier row holds.

### DoP evidence, rate by rate

A profile's `dop` holds `heardBitRates`, the rates a listening heard, each a
listening at that rate, which says nothing of the rates below it, and never
one a negotiation, a hardware confirmation or a DoP lock reached (the
design's section 1.1: the iOS lock hears nothing back); `refusedBitRates`,
the rates that failed, which may lie below a heard one; and one statement at
most: `statedMaxBitRate`, a maker's figure that says DoP, or
`statedWithoutDoP`, a DSD figure that names no transport (`{"kind": "dsd",
"maxBitRate": N}`) or no DSD at all (`{"kind": "none", "maxBitRate": 0}`),
which the player never reads.

The lint reads a dop line's evidence in one fixed form, and nothing else of
it. Its clauses are the text after `dop: `, split on `;` and trimmed. A
clause that starts with a marker (in any case, a word boundary after it) is
the marker, its rates separated by commas, then a colon or the clause's end,
and credits those alone:

- `heard`: the rates a listening heard;
- `refused`, `silence at`, `noise at` and `no frames at`: the rates that
  failed, their carrier refused, played as silence or as noise, or no
  attempt's frames taken;
- `stated DoP to` and `stated DSD to`, each with one rate, and `stated no
  DSD`, with none: the statement.

```text
dop: heard DSD64, DSD128: submission 2026-10-18; refused DSD256: submission 2026-10-18 (its carrier refused)
dop: stated DSD to DSD256: maker (the row's note, iOS 6a1241eb: a DSD256 figure that names no transport)
```

The rates heard clauses credit are `heardBitRates`, failure clauses' are
`refusedBitRates`, and the statement clause is the statement, each both ways.
A rate heard and failed is a contradiction, while the four failures are one
result, which two units may give in two ways. What follows the colon is free
text, so it may say anything, a negation included (`refused DSD256: no
supported altsetting`); a clause that starts with no marker is evidence of
nothing (`DSD128's carrier reached a DoP lock`); and a marker clause of any
other shape (`heard at DSD128`, `heard DSD64 and DSD128`, `stated DoP up to
DSD256`) is a lint error that shows the form.

In a profile with `ios`, the text after each marker clause's colon opens with
its source's head: its kind (`submission`, `field log`, `maker`, `pcm
ceiling`, `chip specification` or `retailer`) and its date, which a
`submission` and a `field log` always name and the other four name where
they have one, then nothing, `, ` or ` (` and free text. One clause credits each rate and
the statement, and the export reads that clause's head alone as the entry's
source (`fieldLog`, `2026-06-21`). A source names a kind and a date, never a
person. A DoP claim's `source` is its own row of the registry
("DACProfile.swift <row> at iOS <commit>"), a dated listening or a dated
maker's document. A row added past the registry's baseline, which no commit
of the registry holds as a row, names in its `ios` line the dated wizard
submission it came from (`ios: wizard submission 2026-11-19`), and its DoP
claim then cites no registry row (B944).
