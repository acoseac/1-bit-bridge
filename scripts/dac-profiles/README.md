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

A vendor-wide rule names no model and seeds nothing. The script fails unless
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
`DACProfile.swift`, at a named commit ("DACProfile.swift chord.hugo2 at iOS
cd125088"). The author of a database PR reads the registry at that commit and
checks that no imported DoP rate exceeds its row's `maxDSDRate`, and that a
rate a device refused lies above it:

```sh
python3 scripts/dac-profiles/ios-check.py --registry ~/dev/com.acoseac.dsdplayer/com.acoseac.dsdplayer/DACProfile.swift --commit <sha>
```

It prints one line a profile and exits 1 on a violation; paste its output into
the PR. The registry's prose is never read, only each row's id and its
`maxDSDRate`, and only a row's DoP evidence and caveats are imported, never
its name patterns, resync delays as policy, volume topology or notes.
