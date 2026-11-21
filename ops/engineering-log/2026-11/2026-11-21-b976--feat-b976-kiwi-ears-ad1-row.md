## 2026-11-21 — B976: the Kiwi Ears AD1 joins the DAC catalog, matched by its exact port name alone

Branch `feat/b976-kiwi-ears-ad1-row`, from main `2b66c579` (#1538's merge). Four commits:
`6141dcaa`, a row declared strict-only in the export's tests, green at the base; `dad320ba`, the
AD1's cases, red; `0f9a7772`, the row and the export; and this record, with the rule amended in
place (its line in `.claude/rules/dac-catalog.md`, its long form in
`ops/History-rules-dac-catalog.md`, the matching sentence of `scripts/dac-profiles/README.md`),
the count of known siblings and the sibling tests' cases in that long form, and the row's two
quotes of its maker's page made exact (provenance only; the export's bytes do not change). Every
run below: Go 1.27.3, the tags `ts_omit_webclient,ts_omit_debugeventbus`, `-timeout 30m`.

### The hazard

Without a row the dongle is an unknown DAC on iOS: no PCM ceiling and no verified DoP grant. A
wizard run submitted on 2026-11-21 (app 2.2 (17), iPhone16,1, iOS 27.2.1, schema version 2)
reported the port as `AD1`, of type Headphones (`reportedAsHeadphones`), with `Kiwi Ears` in its
UID's manufacturer field. DoP played clean at DSD64 and DSD128, and DSD256's carrier was refused.
The submission stays outside the repository and names its submitter, who consented to credit; the
credit is the iOS presentation entry's, and nothing here names a person.

### The maker, the name and the chip

- Kiwi Ears' store, read on 2026-11-21. The product's JSON (`/products/kiwiears-ad1.js`) titles it
  `Kiwi Ears AD1`, vendor `Kiwi Ears`, its SKUs `KiwiEars-AD1-Typec-3.5mm` and
  `KiwiEars-AD1-Typec-3.5mm-SL` (black and silver), "supporting up to 32bit/384kHz audio
  decoding", "With the KT02H20 DAC chip". Its page (`/products/kiwiears-ad1`) gives "Digital audio
  support PCM up to 32-bit / 384 kHz" and answers "What audio formats are supported? PCM up to
  32-bit / 384 kHz"; an archived copy captured on 2026-06-04 gives "PCM Specifications
  32Bit/384kHz" in its table. Neither names DSD. Its series page lists the AD1 beside the AD1 Pro,
  the Allegro Mini, the Allegro BAL and the Allegro Pro.
- So the row is `kiwiears.ad1` (the catalog's form, as `fosiaudio.zh3`), named `Kiwi Ears AD1`,
  its maker `Kiwi Ears`: the maker's spelling and the draft's. No Kiwi Ears row came before it, so
  it starts the maker's group.
- The chip (section 6 step 6, which decides by the name): `AD1` is Kiwi Ears' own model, the one
  its store sells as the Kiwi Ears AD1 beside an AD1 Pro, and no chip's or board's default product
  string; the UID's manufacturer field is Kiwi Ears, not a chip vendor's. So an ordinary row. The
  KT02H20's default product string was not found: the USB ID repository's list (version
  2026.06.26) holds no KTMicro vendor and no vendor 31b2 or 3302, the kernel's `quirks.c` and
  `mixer_quirks.c` at v6.12 name neither ID, its `quirks.c` on master names two other makers' dongles
  on vendor 3302 (3302:17c2, "TTGK Technology USB-C Audio"; 3302:39c3, "NiceHCK Octave") and no
  product string, and the search endpoints reachable from the session refused. Kiwi Ears'
  equalizer (below) does name two units by what look like chips' default strings, `KT-0211L USB
  DAC` (3302:2017) and `KT-1132L USB DAC` (31b2:3006): `USB DAC`, not `AD1`. Its entry for the AD1
  is `KT-0211L AD1` (3302:20e3), another KTMicro part number than the page's KT02H20; the row
  depends on neither.
- A DAC-side equalizer. The page says "This product can be equalized" and links Kiwi Ears' web
  equalizer (`equalizer.kiwiears.com`), whose script programs units over WebHID from a table that
  lists the AD1. So the AD1 has a mode iOS cannot see. Section 6 step 6 sets
  `dopDecodingDependsOnHiddenMode` only on a field report that such a mode plays DoP as noise; none
  exists (this run heard DoP clean, the equalizer's state unknown), so the row carries no
  hidden-mode caveat. The FiiO QX13's PEQ played DoP as noise; a run with the AD1's equalizer
  engaged would tell (a follow-up, in the report).
- No USB match: the equalizer's table names the AD1 3302:20e3, but a row gains a match only from a
  capture or a kernel quirk, so the row stays iOS's alone. Vendor 3302 carries other makers'
  dongles (the two kernel entries above), so a match a later capture gives would want the AD1's
  product string beside its IDs, as a shared reference ID does.

### The row, by the iOS procedure's section 6

iOS's alone, as the ZH3 and the Magic-ARC are: `match` empty, `native` and the top-level `timing`
null, no caveat; order 24, appended at the end of `profiles`. Its values follow the Snowsky Tiny
A's, the closest host-synced dongle with no volume control of its own that plays DoP: DSD64 and
DSD128 heard, DSD256 refused, no maker statement, 384 kHz at 32 bits, host-synced. The iFi GO link
is as close but for iFi's DSD256 statement; the Tiny A's 800 ms PCM entry was measured on it, and
the AD1's run measured every time within its default, which leaves every time null, as on the GO
link.

- DSD over DoP, from every `dsd` result (step 7). DSD64 (2,822,400) and DSD128 (5,644,800): three
  attempts each, each at the lock proxy (3 of 3) at iOS volume 0.3, and the listening answered a
  clean tone: heard, credited by one clause, `heard DSD64, DSD128: submission 2026-11-21`. DSD256
  (11,289,600): three attempts, each "Audio output couldn't negotiate 705.6 kHz over DoP", the lock
  proxy never reached, and the listening answered silence: a carrier refused on every attempt,
  `carrierRefused`. The attempts carry no `carrierRefused` key, an app build before B702, so step
  7's reading of such a file applies, and it changes nothing here: it reads a rate whose carrier
  every attempt refused as refused, never as heard without lock, and this rate's listening
  answered silence, which agrees with the refusal. DSD512 was never attempted; the PCM sweep asked
  its 1,411,200 Hz carrier and 384,000 Hz was negotiated, step 7's skipped-carrier rule, so it is
  refused too, by a clause of its own citing the sweep, as on the ZH3, the Magic-ARC, the FiiO KA17
  and the Onix Beta XI2. The legacy ceiling is DSD128, the highest rate heard, with no failed rate
  at or below it. The DoP claim's source is the listening, `wizard submission 2026-11-21 (app 2.2
  (17), iOS 27.2.1)`.
- The statement: Kiwi Ears gives PCM figures alone and no DSD figure, so neither statement field is
  set and no marker clause is written; a clause of the dop line in no marker's form says so, and
  the export gives the statement as `unrecorded`.
- PCM: every rate from 44.1 to 384 kHz negotiated exactly, and 705.6, 768 and 1411.2 kHz each came
  back as 384 kHz, so `maxRateHz` is 384000, as measured. No rate below the ceiling was
  renegotiated, so no skipped-rates note is owed (B854). `bitDepth` 32 (step 5): the submitter's
  figure ("32Bit/384kHZ") checked against Kiwi Ears' for USB PCM, 32-bit / 384 kHz; the two agree,
  and both are in the row's `ios` line. The page's 24-bit / 48 kHz is the microphone's ADC, another
  mode, which is not this field.
- Volume `hostSyncedDACGain`, by step 6: the identity answers say no volume control of its own and
  that the iOS volume changes its level, which `.hostSyncedDACGain` needs (the slider moves the
  level), and DoP held lock and was heard clean at iOS volume 0.3, below maximum, so iOS did not
  scale the samples, which it needs too. The split step 6 resolves to `.unknown` (answers of
  `.dacOwnsGain` beside DoP heard below maximum) does not arise, and the schema-2 draft derived
  `hostSyncedDACGain` itself, so it stands. The Magic-ARC gave the same answers and stayed
  `unknown` because no DoP attempt played there; here one did.
- Every iOS time null, each with its reason in the row's `ios` line: the IO buffer on the iOS
  default (a 40 ms request granted at 44.1 kHz, 2.7 ms at 384 kHz); the DSD settle clean at 150 ms,
  within the 400 ms DSD default; PCM after DoP and a PCM rate change each clean at 150 ms, within
  the 150 ms PCM default; the cold start clean on one cold DSD64 try 1.0 s after a replug, with no
  extra wait; and the transition cushion unset: no clicks, and no Chord DAC.
- Names. `AD1`, verbatim, is its port name and its one key. Step 3: the port was classed as
  Headphones, so with that classification off its name may differ; the name is kept and the
  classification written in the row's `ios` line. No legacy pattern: `AD1` lies inside its
  sibling's name, `AD1 Pro`, so any pattern that takes the AD1 takes the AD1 Pro, which namesApart
  refuses (B944: no row reaches a sibling it brings, by a legacy pattern either). No token: `AD1` is
  under the four characters a token needs, and a token is taken from a reported port name, never
  seeded from a pattern or a display name (`Kiwi Ears AD1` is one). This is an exception to step
  7's "and a token of at least four characters", for a port name too short to give one, which the
  iOS re-pin records in section 6. Collisions, both ways: no catalog key, generic name, known
  sibling or corpus name lies inside `AD1` or holds it, at the base and with the row
  (`TestAnAddedRowAndEveryOtherRowKeepTheirOwnNames`, and the lint).
- So today's iOS builds, whose live matcher (`DACProfileRegistry.profile(matching:)`) matches by a
  legacy pattern alone, know no row for the AD1, as now, and iOS 2.3's strict matcher (B730 PR 3b,
  `DACRouteResolver`: an exact port name, then a token one row alone holds) matches `AD1` exactly,
  its case kept, and the AD1 Pro to no row. The row's `ios` line says so.

### A row declared strict-only

The export's tests could not take such a row. namesApart asked the live matcher to answer each of
an added row's names with that row, and the parity let the two matchers answer apart only the four
corpus names iOS PR 1 declares (`brooklyn`, `brooklyn dac`, `ka3`, `topping e30`, folded), the
four `DACNameMatcherParityTests` holds on iOS. A row may now be declared strict-only, with its
reason (`strictOnly` in `additions`; the AD1's: "a port name too short for a legacy pattern that
leaves its sibling out"), and the declaration exempts it from those two checks and no other:

- namesApart asks the live matcher to answer the row's names with no row, in place of its own;
- the parity lets the two matchers answer apart the row's exact port names alone (`unpermitted`):
  a declared name with its Exception set, no live answer and the row's exact match, and only where
  the name, trimmed with its case kept, is a port name the row is declared with. So `AD1` and
  `AD1 ` are permitted, and `ad1`, `Ad1` and `aD1`, which the strict matcher, keeping a name's
  case, answers with no row, cannot be declared otherwise; nor can a name a token matches.

Everything else holds as for any added row: its names reach it through the strict matcher, no
other row's pattern or token lies inside them and no other row's name reaches it, and no sibling it
brings is reached by either matcher. The declarations are held to their rows by
`strictOnlyProblems` (`TestEveryStrictOnlyDeclarationHoldsItsRow` over the shipped ones): a
declaration naming no row declared past the baseline, naming one twice or giving no reason, a
declared row given a legacy pattern by its declaration or the export, or whose declared port name
the strict matcher does not answer with its exact match, and a row added past the baseline that the
export gives no legacy pattern and no declaration names, are each refused. So an exact-only row
added undeclared fails three checks. `TestAStrictOnlyRowIsExemptFromTheLiveMatcherAlone` holds the
mechanism on a fixture row (`fixture.strict`, its port name `FX1` inside its sibling's `FX1 Pro`):
declared, it lints and passes the parity, namesApart and the declaration check, `FX1` and `FX1 `
answered with it and `fx1`, `Fx1` and `fX1` with no row, by either matcher; and each of these is
refused, by the diagnostics the case names: the row undeclared; declared with a legacy pattern, in
the catalog and its declaration or in the catalog alone; a declaration naming no row, which the
declaration check alone refuses; the row's ID misspelt in its declaration; its port name changed
in the catalog; a declaration with no reason, or given twice; a lower-case name declared the row's
exact match, or declared answered apart; and a name the row's token matches declared a permitted
disagreement. The two matchers, the lint's rules and the export's format are unchanged.

### What the AD1 does on each surface until the iOS re-pin

The consumers of `DACCatalog.ios.json` and of the catalog's iOS projection, at this branch:

- The iOS app bundles a byte copy of the export pinned to one bridge commit
  (`DACCatalogExportPinTests`) and builds its registry and name keys from it; nothing reads the
  export live. Shipped iOS (2.2, the App Store's build) holds no row for the AD1, so it stays an
  unknown DAC, as now. A build that bundles this export but still matches by legacy pattern alone
  takes no port to a row with no pattern, so the AD1 stays unknown there too, and the row is safe
  there. The re-pin therefore merges after B730 PR 3b, when the strict matcher takes `AD1`. Whether
  iOS's registry builder and tests accept a row with no legacy pattern, and the parity test there
  learns the strict-only exception (its four declared disagreements, to which the AD1 adds `AD1`
  and `AD1 `), is the re-pin's to check: the iOS repository was not read here.
- The bridge's own player matches by USB identity alone and never reads the `ios` projection, and
  the row has no match (`TestAnIOSOnlyRowMatchesNoUnit` makes units of its port name and finds
  none): no effect.
- The bridge's endpoints and the console: nothing in this repository serves, embeds or reads the
  export or the projection, beyond `scripts/dac-profiles/ios-export` (a tool run by hand, whose
  `-check` CI's `dac-profiles` job runs) and the catalog's own lint and tests.
- The site's DAC page (`1-bit.app/learn/dac-compatibility/`, read on 2026-11-21): its "Profiled in
  1-bit" list is "generated from the app's own DAC catalogue and holds the 18 DACs that 1-bit 2.2,
  the version on the App Store, has a profile for", a static page with no script; the AD1 shows
  there only once a released app build carries it.

So no live consumer gives an older iOS build this row.

### The notes the iOS re-pin carries

The notes are the presentation entry's (`DACProfilePresentation.byID`), so this PR sets none of
them. Their intended text, for the re-pin to copy:

1. The DoP note, the draft's: "DSD over DoP up to DSD128 verified on iOS." Step 6 keeps "verified"
   off the notes of a DAC with a field report of DoP noise in a hidden mode, and the AD1 has none;
   if one comes (its equalizer, above), the row gains the caveat and the note loses the word.
2. The volume note, the draft's, already in its current form (B462's opening for a DAC with no
   volume control of its own, B497's ending): "It has no volume control of its own: the iOS slider
   sets its level, and the DAC applies it itself — iOS does not scale the samples at any volume."
3. The Headphones note (step 3): iOS reported this DAC as Headphones (the setting "Treat USB
   devices as headphones"); with that classification off, the name of its port may differ, and the
   DAC may then not be recognized. On a route iOS classes so, the new profile makes the Headphone
   Safety card show as soon as the DAC is connected (the iOS app's behaviour as the brief gives it;
   the iOS code was not read here).

No skipped-rates note (no rate below the ceiling was renegotiated) and no PCM-only note. The rest
of step 7's re-pin: the export's bytes and its pin (below); the entry's other fields, the credit
consented; the expected evidence, transcribed by hand from the `dsd` results (DSD64 and DSD128
heard, DSD256 and DSD512 `carrierRefused`, each from the submission of 2026-11-21, the statement
unrecorded); the observed name `AD1`; and the twelve siblings below.

### The siblings, and what the AD1's row would give each

Kiwi Ears' products that take USB audio from a host, read on 2026-11-21: those of its store and
its series page, the original Allegro from archived copies, and the units its equalizer programs.
Each is a known sibling now, named as the AD1 names itself: as the equalizer names the unit after
its chip's part number (its AD1 entry, `KT-0211L AD1`, gives the very name the AD1's port
reported), and by its model alone where the equalizer has no entry. What each reports on iOS is
not known. None holds a key of the AD1's, which has no pattern and no token, so the exact name
`AD1` reaches none of them, by either matcher; each is declared all the same, so a later key that
reached one would fail. Every value of the AD1's row is unmeasured on each; the dangerous ones are
the DoP grant (DSD64 and DSD128 heard, DSD256 refused, a DSD128 ceiling) and the host-synced
volume, whose note says iOS does not scale the samples.

| Sibling | Named after | Its maker's figures | The AD1 values wrong for it |
|---|---|---|---|
| `AD1 Pro` | the equalizer's `KT-0113L AD1 Pro` (31b2:0113) | a dongle in a 3.5 mm or a 4.4 mm version, with EQ, on another KTMicro part; no rate read | the DoP grant and the refusals, the 384 kHz ceiling, the host-synced volume |
| `Allegro` | its model, as archived (2024-03-31) | ES9028Q2M, PCM 32-bit / 384 kHz, "DSD up to DSD256", 3.5 and 4.4 mm | DSD256 refused and the DSD128 ceiling, if it plays DSD256 over DoP; the volume |
| `Allegro Mini` | the equalizer's `KT-0211L Allegro Mini` (31b2:0111) | PCM 32-bit / 384 kHz, DSD64 and DSD128 (as archived 2026-10-19), 3.5 and 4.4 mm | the DoP grant, unheard on it; the volume |
| `Allegro Nano` | the equalizer's `KT-0211L Allegro Nano` (31b2:0117) | not on the store; no figure read | every value |
| `Allegro Pro` | its model (the equalizer lists it with no entry of its own) | dual ES9603 amplifiers, PCM 32-bit / 384 kHz, DSD64 and DSD128, "sound modes" (as archived 2026-10-19) | the DoP grant, under modes iOS cannot see; the volume |
| `Allegro BAL` | the equalizer's `Allegro BAL` (0bda:0023, a Realtek ID) | a right-angle 4.4 mm balanced adapter; no rate read | every value, on another chip family |
| `Kiwi Ears Coda` | the equalizer's `KT-0211L Kiwi Ears Coda` (31b2:011b) | PCM 32-bit / 384 kHz, a web EQ, 3.5 and 4.4 mm (as archived 2026-10-18); no DSD figure | the DoP grant; the volume |
| `Kiwi Ears Stardust` | the equalizer's `KT-1132L Kiwi Ears Stardust` (31b2:3030) | in-ear monitors in a Type-C version; no figure read | every value |
| `B-Media Chorus` | the equalizer's `KT-1132L B-Media Chorus` (31b2:1132) | in-ear monitors, a collaboration, in a Type-C version | every value |
| `Belle` | its model (the equalizer lists a Type-C version with no entry of its own) | in-ear monitors in a Type-C version | every value |
| `S-link` | the equalizer's `KT-3016L S-link` (31b2:3016) | a 0.78 mm two-pin cable, KT0231H with a mini DSP, its firmware name Kiwi Ears S-Link (as archived 2026-09-29) | every value, through a DSP iOS cannot see |
| `S-Link-Typec-0.78-Mic` | the equalizer's `KT-3001 S-Link-Typec-0.78-Mic` (31b2:3001) | the S-Link's other entry | every value |

Not siblings: the equalizer's generic entries (`KT-0211L USB DAC`, `KT-1132L USB DAC`, `KT-12C3L
USB D1`, `CB-5000X USB DAC`, `CB-3000S USB DAC`), which name no Kiwi Ears model; and Kiwi Ears'
in-ear monitors and headphones with no USB version, its other cables, ear tips and cases.

### The export

`DACCatalog.ios.json`, regenerated: the row appended at order 24, nothing else changed; 24 rows;
SHA-256 `c3b0446cfc7a784706f02e6d14853cb240128b51d859a6fe508e5277e5882546`, which the iOS re-pin
copies byte for byte with this branch's commit, moving its pin's sha256, bridge commit and row
count (24) together. It is the first row whose `legacyPatterns` is `[]`. The baseline and its
SHA-256 (`a91415f9…`) are unchanged. The known siblings are 61: the baseline's 26, the ZH3's 16,
the Magic-ARC's 7 and these 12.

### Reds and greens

At `6141dcaa`, the mechanism alone, both packages pass, every case of
`TestAStrictOnlyRowIsExemptFromTheLiveMatcherAlone` with them. At `dad320ba`, the cases on the
base's catalog and export: in the catalog's package `TestNoKnownSiblingReachesAModelRow` (`the
embedded database holds no kiwiears.ad1 row, or not "AD1 Pro" among the known siblings`) and
`TestARowWithNoPatternOrTokenIsMatchedByItsExactPortNameAlone` ("holds no kiwiears.ad1 row") red,
every other test green and the kernel seed skipped; in the export's,
`TestTheExportMatchesTheIOSBaseline` red (the catalog's 49 known siblings against the 61 declared,
and "the export has 23 rows, the baseline 21 and the declarations 3"),
`TestAnAdditionIsHeldToItsDeclaration` red (its fixture refused by the same),
`TestAnAddedRowAndEveryOtherRowKeepTheirOwnNames` red ("kiwiears.ad1 is declared past the baseline
and not in the export", and the twelve siblings not among the catalog's), the three counterpart
cases of `TestALegacyPatternSharedWithAnAddedRowIsACollision` red,
`TestEveryStrictOnlyDeclarationHoldsItsRow` red ("kiwiears.ad1 is declared strict-only and not in
the export") and `TestAStrictOnlyRowIsExemptFromTheLiveMatcherAlone` red (its fixture's order, 25,
leaves 24 empty, which the lint refuses); the golden, determinism, unrecorded-row and `ios`-line
tests green. At `0f9a7772` both packages pass, the export's `-check` with them, and the kernel seed
too, run against `sound/usb/quirks.c` at v6.12 fetched for the run and not kept.

### Controls

Each predicted in writing before it ran (every test expected red, by name, in each package, and
its diagnostics), laid uncommitted on `0f9a7772`, run on both packages, and restored with
`git checkout --` of the file it changed, `git status` clean before and after. None regenerates the
golden, so the golden's red is part of each prediction that changes the catalog's export.

1. A duplicate ID and a shared key (`profiles.json`): a copy of the row at order 25, its ID and its
   one key, the port name `AD1`, shared. The catalog-wide check, the lint `Default` runs, refuses
   the catalog in exactly the one line predicted, "profile kiwiears.ad1: listed twice" (the lint
   names a port name two rows give only when their IDs differ). Red, as predicted: the twelve tests
   of the catalog's package that load the catalog and the nine of the export's (thirteen results
   with `TestTheBaselineDecidesWhichIOSLineARowNames`' four subtests), each at the load;
   `TestTheEmbeddedDatabaseIsWrittenInItsCanonicalForm` and `TestAnUnrecordedRowExportsNoEvidence`
   green, the kernel seed skipped.
2. A legacy pattern, `AD1`, given the declared row (`profiles.json`, canonical, the golden not
   regenerated). The lint takes it. Red, as predicted: in the catalog's package
   `TestARowWithNoPatternOrTokenIsMatchedByItsExactPortNameAlone`; in the export's
   `TestTheExportIsItsGolden` (the live export no longer the committed one),
   `TestTheExportMatchesTheIOSBaseline` (the export-versus-declaration check, `row 24
   (kiwiears.ad1), legacyPatterns: the export's [AD1], the declaration's []`, and the replay of the
   AD1's names and the sibling `AD1 Pro`, each now `the export's first hit is "kiwiears.ad1"`),
   `TestAnAddedRowAndEveryOtherRowKeepTheirOwnNames` (namesApart's sibling check through the live
   matcher, `the sibling "AD1 Pro" holds kiwiears.ad1's legacy pattern "AD1", which the live
   matcher takes it by`, and the strict-only row's names now the live matcher's),
   `TestEveryStrictOnlyDeclarationHoldsItsRow` (the declaration's no-legacy-pattern check,
   `kiwiears.ad1 is declared strict-only, and the export gives it the legacy patterns ["AD1"]`),
   `TestAnAdditionIsHeldToItsDeclaration` at its fixture's check, the three counterpart cases of
   `TestALegacyPatternSharedWithAnAddedRowIsACollision` (its three positive cases green) and
   `TestAStrictOnlyRowIsExemptFromTheLiveMatcherAlone` at its clean check.
3. The port name misspelt, `AD 1` (`profiles.json`, the golden not regenerated). The lint takes it.
   Red, as predicted: `TestARowWithNoPatternOrTokenIsMatchedByItsExactPortNameAlone`; the golden;
   the parity (`row 24 (kiwiears.ad1), nameKeys: the export's …`, `"AD1": the strict matcher over
   the export's keys answers {"unknown":"noMatch"}` and `the strict and live answers differ false,
   and its declaration says they differ true`); namesApart's strict resolution, `kiwiears.ad1: its
   name "AD1" is the strict matcher's ""`; the declaration check's, `the strict matcher over the
   export answers its declared port name "AD1" with {"unknown":"noMatch"}, not its exact match`;
   and the same three propagated tests and cases as control 2. The AD1's two sibling cases of
   `TestNoKnownSiblingReachesAModelRow` give their diagnostics still, and stay green.
4. The row removed from the strict-only declaration (`main_test.go`; the catalog and the golden
   untouched). Red, as predicted, in the export's package alone: namesApart's both-matchers
   requirement (`kiwiears.ad1: its name "AD1" is the live matcher's ""`), the parity (`"AD1": its
   declaration says the strict and live answers differ, which only a strict-only row's exact port
   name may, and kiwiears.ad1 is not declared strict-only`, and the names answered apart beyond
   those permitted), the declaration check (`kiwiears.ad1 has no legacy pattern, and no
   declaration says it is strict-only`), and the propagated `TestAnAdditionIsHeldToItsDeclaration`,
   the collision test's three counterpart cases and the mechanism test's clean check; the golden,
   determinism, unrecorded-row and `ios`-line tests green.
5. A declaration of an ID no row has, `kiwiears.ad2`, beside the AD1's (`main_test.go`). Red, as
   predicted: the declaration check alone (`the strict-only declaration "kiwiears.ad2" names no row
   declared past the baseline`), in the two tests that run it over the shipped declarations,
   `TestEveryStrictOnlyDeclarationHoldsItsRow` and `TestAStrictOnlyRowIsExemptFromTheLiveMatcherAlone`
   at its clean check; the parity and namesApart say nothing.

No control went red beyond its prediction.

### Runs

On `0f9a7772`: the catalog's and the export's tests with `-race -count=20` pass (16.2 s and
169.1 s), and `go vet` of both packages is clean for windows and darwin. The gate's runs on this
record's commit are in the report.
