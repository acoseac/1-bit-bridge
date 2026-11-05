# The DAC profile database's checks

`internal/player/dacprofile/profiles.json` is the bridge player's reviewed
database of DAC profiles (B728; `ops/plan-b728-dsd-capability-design.md`,
section 2): what is known of a DAC model's DSD, keyed by its USB identity and
embedded in the binary. The package's own tests hold the file to its schema
and its rules on every test run: the matches, no two profiles of one
identity, product strings for shared reference IDs, a provenance line for
every claim, the closed caveat list, and the file's canonical form. Two of its
claims rest on files outside this repository, and these two checks hold them
to those files.

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

## The iOS DoP evidence: `ios-check.py`

A profile's DoP claim comes from a row of the iOS app's registry,
`DACProfile.swift`, at a named commit ("DACProfile.swift mytek.brooklyn.dac.plus
at iOS cd125088"). The author of a database PR reads the registry at that
commit and checks that no imported heard or stated DoP rate exceeds its row's
`maxDSDRate`:

```sh
python3 scripts/dac-profiles/ios-check.py --registry ~/dev/com.acoseac.dsdplayer/com.acoseac.dsdplayer/DACProfile.swift --commit <sha>
python3 scripts/dac-profiles/ios-check.py --self-test   # what CI's dac-profiles job runs
```

A rate a device refused is not compared with the row, which may keep a
published maximum beside a refusal at or below it (`ifi.generic` keeps DSD256
while both its measured units refused it); the refusal's evidence is in the
row's comments, and the database's lint holds it to the profile's own
provenance. A heard rate is a listening: a row whose rate a negotiation, a
hardware confirmation or a DoP lock reached gives no heard rate (the design's
section 1.1: the iOS lock hears nothing back).

The lint reads a dop line's evidence in one fixed form, and nothing else of
it. Its clauses are the text after `dop: `, split on `;` and trimmed; a
clause that starts with a marker, `heard`, `refused` or `silence at` (any
case, a word boundary after it), is the marker, its rates separated by
commas, then a colon or the clause's end, and credits those rates alone:

```text
dop: heard DSD64, DSD128: iOS profile <row>, clean with PEQ off (iOS <commit>)
dop: refused DSD256: <source>; silence at DSD512: <source>
```

What follows the colon is free text that nothing reads, so it may say
anything, a negation included (`refused DSD256: no supported altsetting`). A
clause that starts with no marker is free text too, and evidence of nothing
(`DSD128's carrier reached a DoP lock`). A marker clause of any other shape
(`heard at DSD128`, `heard DSD64 and DSD128`, `heard DSD128 not`, a marker
with no rates) is a lint error that shows the form, and so is a rate both
heard and refused in one profile's provenance (`heard DSD256; silence at
DSD256`). `refused` and `silence at` are one result, a refusal, which two
units may give in two ways (`refused DSD256: unit 1 would not negotiate;
silence at DSD256: unit 2 played it silent`).
The heard rate needs a `heard` clause that lists it, and each refused rate a
`refused` or `silence at` one. `ios-check.py` reads the profiles' DoP fields
and not these lines. It prints one line a
profile and exits 1 on a violation; paste its output into the PR. The
registry's prose is never read, only each row's id and its `maxDSDRate`: its
comments are blanked, line and block (which nest, as Swift's do), outside
string literals, and what the reader does not read (a block comment left
open, a multi-line or raw string, an interpolation) is refused, never read
past. Only a row's DoP evidence, its caveats and its DSD resync delay, a
measured value that seeds the profile's `timing.dsdPrefillMs` ("timing: iOS
profile chord.hugo2's DSD resync delay, 0.7 s (iOS cd125088)"), are imported,
never its name patterns, a resync delay as policy, volume topology or notes.
