## 2026-11-20 — B958: the iKKO LOT Magic-ARC joins the DAC catalog, PCM-only

Branch `feat/b958-ikko-magic-arc-row`, from main `d4c5d062` (#1526's merge). Three commits:
`b3707813`, the cases, red; `132c8897`, the row and the export; and this record, with the count
of known siblings and the sibling test's cases in `ops/History-rules-dac-catalog.md`. Every run
below: Go 1.27.3, the tags `ts_omit_webclient,ts_omit_debugeventbus`, `-timeout 30m`.

### The hazard

Without a row the dongle is an unknown DAC on iOS: iOS tries DoP, every carrier is refused, and
the listener hears silence; nor does iOS know its PCM ceiling. A wizard run submitted on
2026-11-19 (app 2.2 (17), iOS 27.2.1) reported the port as `Ikko Magic-Arc`, of type Headphones
(`reportedAsHeadphones`), with `GHW Micro` in its UID's manufacturer field. The submission stays
outside the repository and names its submitter, whose credit is the iOS presentation entry's;
nothing here names a person.

### The maker, the name and GHW Micro

- The submitter gave the maker as "Iko lot". That is iKKO's own line, iKKO LOT (IKKOLOT), more
  than a misspelling: iKKO's store sold the dongle under the brand `ikkolot`. An archived copy of
  its store page captured on 2022-05-29 titles it "IKKO LOT -Type C to 3.5 MM Headphone Jack";
  one captured on 2023-02-19 titles it "iKKO LOT® - Magic-ARC", and iKKO's menu then listed
  "Magic ARC" among its accessories. Both carry the SKU 6971958821204, the barcode a photograph
  of its box (in a review of 2022-04-02) shows beside a manufacturer line naming iKKO's company;
  its GS1 prefix, 6971958, is the one every product on iKKO's store carries. iKKO's store, read
  on 2026-11-20, no longer lists the dongle. Nothing read shows it to be another maker's.
- So the row is `ikko.magicarc`, named `iKKO LOT Magic-ARC`, its maker `iKKO` in iKKO's spelling
  (no iKKO row came before it, so it starts the maker's group). The draft's
  `ikolot.ikolotusbcadaptor` is not used.
- GHW Micro: the USB ID repository's vendor list, read on 2026-11-20, holds no vendor of that
  name, no search found a chip or interface-board maker by it, and the dongle's DAC chip is
  undisclosed (the 2022 review says so). Section 6 step 6 decides by the name: `Ikko Magic-Arc`
  carries iKKO's name and the model's, so the row is an ordinary one whatever GHW Micro is.

### The row, by the iOS procedure's section 6

iOS's alone, as 20 of the 22 rows before it are: `match` empty, `native` and the top-level
`timing` null, no caveat. Order 23.

- Names: `Ikko Magic-Arc`, verbatim, as the legacy pattern, the port name and the token (14
  characters; no other row's names, no generic name and no known sibling holds it, and it holds
  none of theirs), with no excluded token. Step 3: the port was classed as Headphones, so with
  that classification off its name may differ; the pattern is kept and the classification
  written in the row's `ios` line.
- DSD over DoP, from the `dsd` results: DSD64, DSD128 and DSD256 each had three attempts, each
  "Audio output couldn't negotiate … over DoP", the lock proxy never reached, and the listening
  answered silence. The attempts carry no `carrierRefused` key, an app build before B702, and
  step 7 reads such a rate as refused, not heard, so each is `carrierRefused`, credited by one
  clause, `refused DSD64, DSD128, DSD256: submission 2026-11-19`. DSD512 was never attempted;
  the PCM sweep asked its 1,411,200 Hz carrier and 48,000 Hz was negotiated, step 7's
  skipped-carrier rule, so it is refused too, by a clause of its own citing the sweep, as on the
  ZH3, the FiiO KA17 and the Onix Beta XI2. No rate was heard, so the legacy ceiling is none, as
  on the ddHiFi row. The DoP claim's source is the listening, `wizard submission 2026-11-19 (app
  2.2 (17), iOS 27.2.1)`.
- The statement: iKKO's pages give PCM figures alone and no DSD figure, so no maker statement is
  recorded (the brief's C3). But the lint holds a legacy ceiling of 0 to a statement of no DSD,
  both ways (`legacyCeiling`; `TestTheLegacyCeilingAgreesWithTheEvidence`), and with every rate
  refused no other ceiling lints, since no failed rate may lie at or below one. So the row
  records the PCM ceiling's statement, `stated no DSD: pcm ceiling 2026-11-19`, which the export
  gives as `noDSD` from `pcmCeiling` on that day: DoP's lowest carrier, DSD64's, is 176.4 kHz,
  above the 48 kHz measured and above the 96 kHz iKKO states. It is the statement the Apple USB-C
  adapter's row makes of its own 48 kHz ceiling.
- PCM: 48 kHz alone negotiated exactly; every other rate asked, from 44.1 to 1411.2 kHz, 44.1 kHz
  below it included, was renegotiated to 48 kHz. So `maxRateHz` is 48000, the measured ceiling;
  a higher maker figure is recorded and not adopted, since no run negotiated it. Why 48 kHz, the
  run cannot tell. It may be iOS's: the port was classed as Headphones, no run with that
  classification off exists, and nothing in this repository records how iOS negotiates a rate
  on a USB port it classes so (the wizard's own warning calls the measurements valid under it).
  It may be the DAC's: iKKO's page stated "up to 16bits/48kHz lossless" in its 2022 copy before
  "up to 24bit/96kHz lossless" in its 2023 one, the box says 96kHz and the submitter wrote 96
  KHz; a unit of the earlier figure offers 48 kHz alone, and one that offers 96 kHz would have
  negotiated it when asked, unless iOS held it. A run with the classification off would tell.
- `bitDepth` 24 (step 5): the submitter's 24 checked against iKKO's USB PCM figure, 24-bit on
  its page as archived on 2023-02-19, which replaced the 16 bits it stated as archived on
  2022-05-29. The later figure agrees with the submitter's, so 24; both are in the `ios` line.
- Volume `unknown`, as the brief's review decided (R1): the answers say the dongle has no volume
  control of its own and that the iOS volume changes its level. Section 6's host-synced
  paragraph explains the copy of a draft already host-synced and does not classify these
  answers, the draft derives `unknown`, and no DoP attempt played. Nor does iKKO state a hardware
  gain stage: its page speaks of an in-line remote's controls ("wire control function, such as
  microphone volume control, pause and play functions"), which is not one.
- Every iOS time null (R4). `ioBufferMs`: a 40 ms request was granted at 48 kHz, and the draft's
  own source line says the registry keeps a PCM-only adapter on none unless the DAC benefits;
  every PCM-only row (the ddHiFi, both DragonFlys, the Apple adapter) has none, and nothing in
  the run shows a benefit. `coldStartExtraMs`: undetermined, no cold-start try counted, 44.1 kHz
  negotiated as 48 kHz on all three, never a measured zero. `dsdResyncMs`: the settle ladder did
  not run. `pcmEntryResyncMs`: neither PCM-entry step could run (one needs DSD64 heard, the
  other 44.1 kHz passing). `transitionCushionMs`: none, as on every row but the Chord ones.

### The notes the iOS re-pin carries

The notes are the presentation entry's (`DACProfilePresentation.byID`), one of the five fields
the catalog does not carry, so this PR sets none of them (the brief's C2). Their intended text,
for the re-pin to copy:

1. The PCM-only note. The draft's is the older build's, "PCM-only over iOS: no DSD rate produced
   an audible tone over DoP, so DSD plays converted to PCM — or is refused when DSD conversion is
   set to Never." Its attempts carry no `carrierRefused` key and record the engine's "couldn't
   negotiate … over DoP", so step 7 gives the reason as measured, as the ddHiFi and DragonFly
   Copper rows' notes do: "PCM-only over iOS: no DSD rate held DoP lock, so DSD plays converted
   to PCM — or is refused when DSD conversion is set to Never."
2. The skipped-rates note, which app 2.2 (17) does not write (B854 came after it): its PCM sweep
   has a rate below the ceiling that iOS renegotiated, as the ddHiFi row's had, so it is written
   by hand. The fact it says, in the words of the current draft's note where they differ: on this
   DAC iOS renegotiated 44.1 kHz to 48 kHz, so 44.1 kHz material does not play at its own rate.
3. The Headphones note (step 3), its intended text: iOS reported this DAC as Headphones (the
   setting "Treat USB devices as headphones"); with that classification off, the name of its
   port may differ, and the DAC may then not be recognized. On a route iOS classes so, the new
   profile makes the Headphone Safety card show as soon as the DAC is connected (the iOS app's
   behaviour as the brief gives it; the iOS code was not read here).
4. The volume: no host-synced copy, and never "iOS does not scale the samples at any volume" for
   this row (R1). The draft carries no volume note; its source line hedges, "the answers do not
   establish that the DAC sets level independently of iOS". If the entry carries one, hedged as
   that line is: "It has no volume control of its own, and the iOS volume changes its level;
   whether iOS scales the samples or the DAC sets the level is not known." It must not tell the
   listener to keep iOS at maximum and use the DAC's own volume, as the ZH3's unknown-volume note
   does: this DAC has none, so iOS at maximum would be its full output into the headphones.

The rest of step 7's re-pin: the export's bytes and its pin (below); the entry's other fields,
the submitter having consented to credit (`creditConsent`); the expected evidence, transcribed by
hand from the `dsd` results (four `carrierRefused` failures, no rate heard, `noDSD` from
`pcmCeiling`); the observed name `Ikko Magic-Arc`; and the seven siblings below.

### The siblings, and what the Magic-ARC's row would give each

iKKO's products that take USB audio from a host: the five its store listed on 2026-11-20, and
the ITM line's two earlier DACs, from archived copies of its store. Each is a known sibling now,
named as the Magic-ARC names itself (`Ikko <name>`); what each reports on iOS is not known. None
holds the Magic-ARC's token or pattern, and each is declared all the same (R3), so the
bare-maker controls below have names to reach; the legacy pattern is the full port name, since
`firstHit` reads no excluded token. Every value of the Magic-ARC's row is unmeasured on each.
The dangerous ones are the PCM ceiling, 48 kHz, which would cap a DAC that plays more, and the
volume note above, written for a DAC with no volume control of its own; the statement of no DSD
would also keep DoP from a sibling that may play it.

| Sibling | Its page's figures | The Magic-ARC value wrong for it |
|---|---|---|
| `Ikko Zerda ITM01` | an ESS9298 DAC, Type-C and Lightning; no rate read | the 48 kHz ceiling, if it plays more; the volume note |
| `Ikko Zerda ITM02` | an AK4377 DAC, "compatible with PCM and DSD" | no DSD; the 48 kHz ceiling, if it plays more |
| `Ikko Milestone ITM07` | a USB DAC with 3.5 mm, 4.4 mm and optical outputs; no rate read | the 48 kHz ceiling, if it plays more; the volume note |
| `Ikko Heimdallr ITB03` | a Bluetooth amplifier, wired by USB too, "up to 384kHz" | the 48 kHz ceiling |
| `Ikko ITX05` | a docking station "equipped with … audio chips"; no figure | the PCM ceiling and the volume, unknown |
| `Ikko Zerda ITM03` | a CS43198 DAC with an optical output (archived 2021-09-27); no rate read | the 48 kHz ceiling, if it plays more |
| `Ikko Music Patch ITM05` | two CS43198 DACs (archived 2021-08-14); no rate read | the 48 kHz ceiling, if it plays more |

Not siblings: the Gems OH1S (analog in-ear monitors on an MMCX cable), the MC01 (a microphone),
the MindOne Pro Snap-in Case (a DAC case that attaches to iKKO's own phone), the IMD-S1 and
IMD-P5 hubs and the CD player (listed as coming soon), the ITX01 dock (no audio figure read), the
IKKOLOT line's ITB-X (children's earphones), ITS01 and ITS02 (Bluetooth speakers) and H18 PRO
(Bluetooth headphones), and the Arc CTU01 and CTU02 cables (analog).

### The export

`DACCatalog.ios.json`, regenerated: the row appended at order 23, nothing else changed; 23 rows;
SHA-256 `d8d782f2b6b6b74f096ddf617dd9b8d2e2346bf1d199e88d44ea39ce60b14007`, which the iOS re-pin
copies byte for byte with this branch's commit, moving its pin's sha256, bridge commit and row
count (23) together. The baseline and its SHA-256 (`a91415f9…`) are unchanged. The known
siblings are 49: the baseline's 26, the ZH3's 16 and these 7.

### Reds and greens

The cases came first (`b3707813`), the catalog and the export the base's. In the export's
package: `TestTheExportMatchesTheIOSBaseline` red (the catalog's 42 known siblings against the
49 declared, and "the export has 22 rows, the baseline 21 and the declarations 2");
`TestAnAdditionIsHeldToItsDeclaration` red (its fixture row refused, by the same two lines);
`TestAnAddedRowAndEveryOtherRowKeepTheirOwnNames` red ("ikko.magicarc is declared past the
baseline and not in the export", and the seven siblings not among the catalog's); and
`TestALegacyPatternSharedWithAnAddedRowIsACollision`'s three counterpart cases red, its
`namesApart` answering the same undeclared row. In the catalog's package:
`TestNoKnownSiblingReachesAModelRow` red ("the embedded database holds no ikko.magicarc row").
Every other case green. At `132c8897` both packages pass, and the export's `-check` with them.

### Controls

Each predicted in writing before it ran, laid uncommitted on `132c8897`, run on both packages,
and restored with `git checkout --` of the files it changed, `git status` clean before and after.

1. A duplicate ID and a shared pattern, a copy of the row at order 24: the catalog-wide check,
   the lint `Default` runs, refuses the catalog in exactly the five lines predicted, "profile
   ikko.magicarc: listed twice", each copy's token inside the other's token, and each copy's
   token and the other's port name one inside the other. Every test that loads the catalog is
   red, the eleven of the catalog's package and the seven of the export's named in the
   prediction; `TestTheEmbeddedDatabaseIsWrittenInItsCanonicalForm`, which re-encodes the bytes
   without the lint, and `TestAnUnrecordedRowExportsNoEvidence` stay green, and
   `TestTheKernelSeedIsThePinnedTags` skips (no quirks file).
2. The token widened to the bare maker name, `Ikko`: the lint refuses the catalog in exactly
   seven lines, one per iKKO sibling, "the known sibling … reaches this model row through the
   strict matcher, by its token", and the same eleven and seven tests are red, each at the
   catalog's load. So the export's collision assertions never ran here, which a load failure
   cannot show to hold: the next control shows it.
3. The legacy pattern alone widened to `Ikko`, the export regenerated from it: the lint takes it
   and the catalog's package passes. In the export's, exactly as predicted:
   `TestTheExportMatchesTheIOSBaseline` red on "row 23 (ikko.magicarc), legacyPatterns: the
   export's [Ikko], the declaration's [Ikko Magic-Arc]" and, for each of the seven siblings, "the
   export's first hit is "ikko.magicarc", its declared live answer """, with no strict-matcher
   line; `TestAnAddedRowAndEveryOtherRowKeepTheirOwnNames` red on, for each sibling, "holds
   ikko.magicarc's legacy pattern "Ikko", which the live matcher takes it by" and "is the live
   matcher's "ikko.magicarc"", and on "ikko.magicarc: its name "Ikko" is the strict matcher's
   """, the widened pattern now one of the row's own names; `TestAnAdditionIsHeldToItsDeclaration`
   red at its fixture's check; `TestALegacyPatternSharedWithAnAddedRowIsACollision`'s three
   counterpart cases red and its three positive cases green; the golden, determinism and
   `ios`-line tests green.

### Runs

On `132c8897`: the catalog's and the export's tests with `-race -count=20` pass (13.7 s and
42.1 s), and `go vet` of both packages is clean for windows and darwin. The gate's runs on this
record's commit are in the report.
