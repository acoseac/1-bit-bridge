# B728: the bridge player's DSD by DAC capability and the user's choice

Design and PR plan, 2026-11-04. This round writes no code.

**Status.** For review, round 3. The decision this implements is binding: the owner decided it on
2026-11-04, after the Argon and Codex consult, and the reviewers' notes on the brief are folded in
(Codex 1 to 3 and Argon 2 to 5). Round 2 folds in Codex's and Argon's reviews of this design and the
owner's four decisions on its open points. Round 3 folds in the last points from Codex and Gemini.
All of these are listed at the end of §11. Every current behaviour this design changes is cited as
`path:line` at bridge main `1df423fc`. iOS citations read `(iOS) path:line` at iOS main
`cea8bdf6`, with paths relative to the app's source directory unless they begin with `.claude/`,
`docs/` or `Tests/`.

**In one paragraph.** The bridge's player no longer requires an owner-approved listening
qualification for DSD. It decides each DSD rate for the bound DAC from three inputs:

- a reviewed profile database keyed by USB identity;
- what the kernel and a silent probe establish about the DAC now;
- the user's choice in the app: native, DoP, convert to PCM or off, plus a maximum rate.

A DAC the database does not know gets DSD converted to PCM by default. A new pure-Go converter does
that, and its output is labelled `converted`, never bit-exact. Evidence is labelled on a ladder:
reported, driver accepted, streaming observed, listener confirmed. No label claims that a DAC
decodes DSD. The owner's qualification harness (`bridge output dsd-test`) stays as an engineering
tool.

## 0. What changes, at a glance

| Today | Where | B728 |
|---|---|---|
| Production DSD needs a local owner-approved record: no record, no DSD | `internal/player/playback/authorization.go:117-189` (`readPermission`); `ops/b470-native-qualification.md:3-6` | The effective policy (§3): profile × live capabilities × the DAC's choice. Production reads no record |
| `dsdMode`, `allowDoP` and `dsdQualification` are `bridge.yaml` keys | `internal/config/player.go:110-120` | Legacy keys, still accepted (an unknown key would turn the player off), migrated once into the player's DSD state (§3.6) |
| DSD is never converted | `internal/config/player.go:86-88`, `:113`, `:220-227`, `:427`; `internal/player/playback/open.go:21-24`, `:160`; `internal/player/playback/codecs.go:9-19` | `internal/player/dsdconv` converts where the policy says so (§4), labelled `converted` |
| The approval's identity includes the anchored USB port | `internal/player/qualification/approval.go:20-25`, `:375-376` | The DAC model's identity: vendor and product IDs, interface and product string, with bcdDevice recorded beside it, never the port or the serial (§2.2) |
| `dsdPlayback.tuples` is the approved record's effective set | `internal/api/outputs_bridge.go:233-259`; `PROTOCOL.md:929-1011` | The effective set's native and DoP tuples, plus `converted` and `refused` rates and `bridge.dsdDAC` (§7.1) |
| `model`, `bcdDevice` and `qualifiedOn` are the owner's qualification | `PROTOCOL.md:956-961`; `authorization.go:362-369` | `model` and `bcdDevice` are the bound unit's. `qualifiedOn` is no longer sent (§7.1) |
| The console says "whether the DAC decodes it is what an owner's listening qualification establishes" | `cmd/bridge/serve_player_console.go:532` | The ladder's words (§1.3) |
| A change under 18 path prefixes bumps `qualificationVersion` (35) and makes all owner evidence stale | `internal/player/qualification/version.go:4`; `version_gate.go:12`, `:15` | The guard narrows to the DSD byte path and the harness (§8) |
| The app grants DSD only from tuples | (iOS) `BridgeOutputPlayerQueue.swift:424-442`; (iOS) `.claude/rules/bridge-player.md:64` | It grants from tuples or `converted` entries (§7.6) |

## 1. The evidence model

### 1.1 The four rungs

Evidence is held per DAC model identity (§2.2), transport (`native` or `dop`) and logical DSD rate
(DSD64, DSD128, DSD256). It is published per tuple.

| Rung (wire word) | What establishes it | What it says | What it does not say |
|---|---|---|---|
| `reported` | The profile database (§2) names the transport at that rate for this identity: DoP from the iOS registry's evidence, native from the kernel's DSD quirk for the device | Someone characterized this model somewhere else | Anything about this box, this unit, its firmware mode or its settings |
| `driver_accepted` | The silent probe's `HW_REFINE` (§5), or a real open whose `HW_PARAMS` committed exactly the format, rate and channels asked (`internal/alsapcm/device.go:374-399`, read back at `:461-500`) | The kernel's driver takes that exact format at that rate on this endpoint. For DoP: only that it takes the PCM carrier, S32_LE at rate ÷ 16 with at least 24 significant bits | That the DAC decodes it. For DoP it says nothing about DSD at all |
| `streaming_observed` | An occurrence of that tuple played until the kernel established 5 s of its content played (`Position.Played`), with no xrun, fault or unconfirmed release | The transport ran on this box at that rate without a fault | That the DAC made music of it. On iOS the DoP "lock" counts the packer's own frames and hears nothing back ((iOS) `PlayerService.swift:28786-28799`). The feedback endpoint measures frame rate, not interpretation (`internal/alsa/stream.go:41-55`) |
| `listener_confirmed` | A person answered "clean" to the optional chime (§6) for this transport, rate, identity and firmware, and no later answer replaced it (§1.2) | A listener heard the chime as a chime, on a date | Bit-exactness, or anything about other rates, modes or firmware |

`driver_accepted`, `streaming_observed` and `listener_confirmed` order what has been established on
this box. `reported` stands apart as the database's claim. A tuple publishes its highest rung. A
DAC nobody has characterized or probed publishes none, and the member is absent.

### 1.2 Keys and lapses

- **bcdDevice.** Every rung above `reported` is recorded with the unit's `bcdDevice`, read from
  sysfs as the harness reads it (`internal/alsa/qualification.go:18-57`). If the bcdDevice differs
  at use, the rung is not published (it is kept, since the firmware may come back), a DoP
  confirmation lapses (§3.2) and first use re-arms (§6).
- **Kernel release.** The probe's `driver_accepted` also records the kernel release. A new kernel
  is probed again.
- **Listener answers.** Each transport, rate and bcdDevice holds one listener answer, the latest.
  A `noise` or `nothing` answer (§6) replaces an earlier `clean` one, so the confirmation is gone.
  It refuses that transport at that rate in every mode (§3.2) and re-arms that transport's first
  use. Only a later write that chooses that mode explicitly clears it, carrying the first-use
  acknowledgement again (§7.2).
- **Code changes.** Nothing lapses on a code change. `qualificationVersion` binds only the
  harness's own reports (§8).

### 1.3 Words

Allowed:

- "Reported by the DAC list"
- "Driver accepted", and for DoP, "Driver accepted the DoP carrier"
- "Streaming observed"
- "Listener confirmed on <date>"
- "Converted to PCM by the bridge's player"

Never used for DSD on the bridge's player: "verified", "decoded", "bit-exact", "bit-perfect",
"lock", "seal" or "qualified".

The bridge's PCM keeps the claims B470 earned (`.claude/rules/player.md:27`, `:134`). A converted
occurrence is never bit-exact or lossless. The app's copy table already refuses these words for
today's line ((iOS) `BridgeOutputPlayerSignalInfo.swift:22-25`); §7.5 replaces only the
qualification line.

## 2. The DAC profile database

### 2.1 Schema

`internal/player/dacprofile/profiles.json`, schema 1. The FiiO QX13 entry below is the example the
decision names. Its USB identity is a placeholder until a capture supplies it (§2.3).

```json
{
  "schemaVersion": 1,
  "sharedReferenceIds": [ { "vendorId": "<vid>", "productId": "<pid>", "why": "a reference firmware many products ship" } ],
  "profiles": [
    {
      "id": "fiio.qx13",
      "name": "FiiO QX13",
      "match": [ { "vendorId": "<captured>", "productId": "<captured>", "interface": null, "product": "<captured>", "bcdDevice": null } ],
      "native": null,
      "dop": {
        "heardMaxBitRate": 11289600,
        "statedMaxBitRate": 11289600,
        "refusedBitRates": [],
        "analogVolumeControls": [],
        "unityAtRawMax": [],
        "source": { "kind": "ios-registry", "ref": "DACProfile.swift fiio.qx13 at iOS <commit>" }
      },
      "timing": { "dsdPrefillMs": 400, "pcmEntryMs": 800 },
      "caveats": [
        { "code": "dop_noise_in_hidden_mode", "transport": "dop",
          "note": "With PEQ on, this DAC plays DoP as noise, and the bridge cannot see the mode: turn PEQ off, or choose Convert to PCM." }
      ],
      "provenance": [ "dop: heard DSD64, DSD128, DSD256: clean with PEQ off on iOS; noise with PEQ on (iOS 2.0.1 report)", "identity: dac inspect capture <date>" ]
    }
  ]
}
```

Fields:

- **`match[]`** lists alternatives, each one an AND of its fields:
  - `vendorId` and `productId` are required, as four lowercase hex digits (`usbid.Hex16`).
  - `interface` is optional: the USB interface the card is (`internal/alsa/sysfs.go:127-128`).
  - `product` is optional, except for an ID listed in `sharedReferenceIds`.
  - `bcdDevice` is an optional list that narrows a claim to known firmware.
- **`native`** gives a maximum bit rate and formats. Its source must be a kernel quirk at the
  pinned tag, or a captured stream file whose altsets list the DSD format.
- **`dop`** gives:
  - `heardMaxBitRate`: on-device listening evidence;
  - `statedMaxBitRate`: the maker's figure;
  - `refusedBitRates`: rates refused on a device, or heard as silence;
  - `analogVolumeControls`: the mixer controls this model is known to apply after its DoP decoder,
    such as an analogue `Master Playback Volume` beside a digital `PCM Playback Volume`. Each one is
    named by interface, name and index, with its own provenance line. The DoP gain check (§3.4)
    passes these over, and checks every other control as strictly as for an unknown DAC.
  - `unityAtRawMax`: level controls with no dB scale whose raw maximum is unity on this model (Gemini).
    Each is `{interface, name, index, rawMax, bcdDevice, evidence}`. The DoP gain check (§3.4)
    admits such a control only while it reads exactly `rawMax` on every channel and its range's
    maximum is still `rawMax`, on a firmware `bcdDevice` lists.

    The `evidence` must be one of two kinds:
    - `document`: the maker's documentation that this control's maximum passes samples unchanged,
      cited by its title and section;
    - `measured`: a capture showing it. Either the unit's digital output or loopback, at the raw
      maximum, compared byte for byte with PCM sent in; or a DoP stream at the raw maximum that the
      DAC's own indicator reports as DSD and that is heard clean.

    A `measured` entry names the bcdDevice it was taken on. The lint refuses an entry with neither
    kind, or with no bcdDevice.

  Its source is the iOS registry at a named commit, and a capture for the volume controls.
- **`timing.dsdPrefillMs`** is the DSD silence written before content in each hardware run. It is
  seeded from the registry's resync delays ((iOS) `DACProfile.swift:217-275`) and defaults to 400 ms,
  iOS's DSD resync default ((iOS) `DACProfile.swift:152-183`).
- **`caveats[].code`** is a closed list (§2.3). Its `note` is display text.
- **`provenance`** has one line per claim, `<claim>: …`, naming its source and its date or version.
  A `dop` line's evidence is written in one fixed form, since a lint cannot read prose (B2's
  round 4; round 3 read negation, and the review broke it from both sides):
  - its clauses are the text after `dop: `, split on `;`, each trimmed;
  - a clause that starts with a marker, `heard`, `refused` or `silence at` (any case, a word
    boundary after it), is `<marker> <rate>(, <rate>)*` followed by `:` or the clause's end, as in
    `heard DSD64, DSD128: <source>`; the free text after the colon is read by nothing, so it may say
    anything, a negation included (`refused DSD256: no supported altsetting`);
  - only the rates that list names count: `heard` credits them as heard, `refused` and
    `silence at` as refused;
  - a clause that starts with no marker is free text and evidence of nothing
    (`DSD128's carrier reached a DoP lock`);
  - a marker clause of any other shape (`heard at DSD128`, `heard DSD64 and DSD128`, a marker with
    no rates) is a lint error that shows the form, and so is one rate both heard and refused in a
    profile's provenance (`heard DSD256; silence at DSD256`); `refused` and `silence at` are one
    result, a refusal, which two units may give in two ways (`refused DSD256: unit 1 …; silence at
    DSD256: unit 2 …`).

  `heardMaxBitRate` needs a `heard` clause that lists it, and each of `refusedBitRates` a `refused`
  or `silence at` clause.

### 2.2 Keying and matching

- **The key** is the vendor ID, product ID, interface and product string. bcdDevice narrows a match
  and is recorded with evidence. It is never part of a choice's key.
- **What never keys anything:**
  - the serial, which is password-class (`internal/usbid`, the serial bullet in
    `.claude/rules/player.md`);
  - the port, a card number, an ALSA id or a devnum (`internal/config/player.go:151-156`).
- **A match** is exactly one profile with a matching alternative. That DAC is known. No match
  means the DAC is unknown. Two matches are a lint failure at build time, and read as unknown at
  run time.
- **Shared reference IDs.** Many products ship a reference firmware's vendor:product, and a match
  on one of those needs its product string. The list is part of the database and its test.
- **Never by name.** The iOS registry matches the iOS port name by substring, first hit first
  ((iOS) `DACProfile.swift:204-208`, `:449-450`, `:1408-1412`), because iOS exposes no USB IDs
  ((iOS) `DACProfile.swift:723-731`). The bridge reads the IDs from sysfs and imports nothing of
  that matcher.
- **On the wire** a DAC is named by `dacKey`: `d1-` followed by the lowercase hex of the first 8
  bytes (16 hex digits) of the SHA-256 of the key's canonical form. It carries no serial or port.
- **The canonical form** (Gemini) is five lines of UTF-8, joined by a single LF (0x0A), with no
  trailing LF:
  1. `dacKey/1`, the form's version.
  2. The vendor ID, as four lowercase hex digits (`usbid.Hex16`).
  3. The product ID, likewise.
  4. The interface: `=` and its number in decimal (no sign, no leading zero, `=0` for zero), or a
     lone `-` when the reading has none. Null and absent are the same, `-`.
  5. The product string: `=` and the string exactly as the sysfs reader returns it, or a lone `-`
     when the unit has none. The reader trims surrounding white space and refuses an empty value
     and any NUL, CR or LF (`internal/alsa/qualification.go:43-46`), so a field holds no LF and
     the lines cannot run together. Nothing else is normalized: no case folding and no Unicode
     normalization.

  The `=` prefix keeps a present value apart from absence, so a product named `-` is `=-`. The
  state file's `key` object holds the same fields, with `null` for an absent interface or product.
  A key is compared by its canonical form, never field by field, and the form's version changes
  only with a new `d` prefix. B2 pins it with test vectors, an absent interface and a product of
  `-` among them.

### 2.3 Seeding

**What the iOS registry holds**, read at iOS `cea8bdf6`:

- 20 entries. 16 carry a DoP maximum rate, which the file defines as the rate reachable by DoP on
  iOS ((iOS) `DACProfile.swift:3`).
- Of those 16:
  - 9 have on-device evidence in comments ("heard", "hardware-confirmed");
  - 6 rest on the maker's specification;
  - one deliberately carries the published rate over the one measured on its units.
- No entry has a USB ID, a bcdDevice field or structured provenance.
- No entry has a native-DSD field. Its native mentions say iOS cannot send native DSD.

**So the bridge imports from the iOS registry:**

- DoP only:
  - `heardMaxBitRate` from on-device evidence;
  - `statedMaxBitRate` from the specification;
  - `refusedBitRates` from on-device refusals, such as the Mytek's DSD256 carrier refused by its
    descriptor ((iOS) `DACProfile.swift:553-579`) and the JM98MAX's DSD256 heard as silence
    (`:1170-1175`).
- Caveats, as codes from a closed list:
  - `dop_noise_in_hidden_mode`: the QX13's hidden PEQ mode ((iOS) `DACProfile.swift:1021-1026`,
    its `dopDecodingDependsOnHiddenMode`, `:334-338`);
  - `mode_switch_caps_rates`: a UAC1/UAC2 switch that caps rates (KA17 `:906-908`; JM98MAX
    `:1154-1155`, `:1179`);
  - `transition_clicks`: clicks at DSD128 and DSD256 (X8 `:1226-1227`).
- Nothing else: no resync delays as policy (they seed only the prefill), no volume topology, no
  name patterns.

**The USB identity** comes from a capture of a real unit. B2 adds `manufacturer`, `product` and
`bcdDevice` to `bridge output dac inspect --json`; these are the sysfs attributes
`alsa.Qualification` already reads (`internal/alsa/qualification.go:35-48`). A model with no
capture stays out of the database. It is unknown, converted by default, and DoP stays selectable
with the user's confirmation.

**From the kernel:** the device-specific native DSD quirks in `sound/usb/quirks.c` (the per-ID DSD
format quirk and the DSD_RAW flag table), at the kernel tag `scripts/alsa-abi` pins. Each becomes
a `native` claim with that source. A vendor-wide quirk names no model and seeds nothing. Neither
does a shared reference ID without a product string. The live altsets still decide what is offered
(`internal/alsa/capabilities.go:10-22`, `:108-131`); the seed only makes a model known.

### 2.4 Where it lives, and how it is updated

- **Location.** A reviewed JSON file in the repo, embedded with `go:embed`. That gives reviewable
  diffs, no network, the same data for everyone on a release, and one binary.
- **Updates** come by PR and never at run time, and there is no local override file. The user's
  per-DAC choice is the override, and a hand-edited file that enabled DoP for the wrong IDs would
  be one more way to send DoP to a DAC that does not decode it.
- **What a database PR runs:**
  - the schema check;
  - no overlapping matches;
  - product strings for shared IDs;
  - provenance for every claim, each heard and refused DoP rate credited by a dop clause in the
    form §2.1 gives;
  - the closed caveat list;
  - the kernel-derived entries against the pinned tag.

  The author also runs a script against an iOS checkout at a named commit, checking that no
  imported DoP rate exceeds its iOS row, and pastes the output into the PR.
- **iOS changes.** A DAC row changed on iOS (the iOS data-only tier) changes nothing here until a
  bridge PR imports it with a capture.

### 2.5 The user's choice and the evidence: the player's DSD state

`<dataDir>/player-dsd.json`, schema 1:

```json
{
  "schemaVersion": 1,
  "defaultMode": "auto",
  "dacs": [
    {
      "key": { "vendorId": "<vid>", "productId": "<pid>", "interface": 1, "product": "<product>" },
      "choice": { "mode": "auto", "maxBitRate": 0, "revision": 4, "setAt": "2026-11-05T10:00:00Z", "setBy": "app" },
      "dopConfirmed": null,
      "firstUse": [ { "transport": "native", "bcdDevice": "<bcd>", "bitRates": [2822400], "acknowledgedAt": "…", "observedAt": "…" } ],
      "evidence": [
        { "transport": "native", "format": "DSD_U32_BE", "bitRate": 2822400, "bcdDevice": "<bcd>",
          "driverAcceptedAt": "…", "kernel": "<release>", "streamingObservedAt": "…",
          "listener": { "answer": "clean", "on": "2026-11-05" } }
      ]
    }
  ],
  "migratedFrom": null,
  "warnings": [ { "code": "migration_record_refused", "reason": "stale_version", "at": "…" } ]
}
```

- **The choice that applies.** A DAC's own `choice` applies when it has one. A DAC with none takes
  `defaultMode`: `auto` on a fresh install, or `off` where an explicit `dsdMode: off` was migrated
  (§3.6).
- **One listener answer per tuple.** `listener` holds the latest answer for the transport, rate and
  bcdDevice: `clean`, `noise` or `nothing` (§1.2).
- **What an acknowledgement covers.** A `firstUse` entry with no `bitRates` covers every rate: a
  write that carried `firstUseAcknowledged` made it (§7.2). One with `bitRates` covers those rates
  alone: a migrated record made it, and lists the rates the record approved (§3.6). A later
  acknowledgement of the transport, or a write that chooses the mode again, replaces the entry with
  one covering every rate.
- **Warnings** record what the app must be able to explain: a migration that could not carry a
  record over, and a damaged state moved aside. Each has a code and, where one applies, a reason
  word (§7.1). They are kept across restarts, and a write that chooses for the DAC concerned clears
  that DAC's warnings.

- **Why the data directory and not `bridge.yaml`.** A paired app sets the choice, per DAC, as it
  does the queue the player keeps (the queue bullets in `.claude/rules/player.md`). The evidence
  has to sit beside the choice. One writer, serve's player, serializes the app's and the console's
  writes without adding a writer of `bridge.yaml` and B506's lock discipline
  (`.claude/rules/config-lifecycle.md`). And `bridge.yaml` stays the operator's file.
- **Durability.** A write is durable before it is answered. It goes through
  `atomicwrite.WriteBytesDurable`, the writer the player's ID reservations use; the data directory
  must already be on storage that outlives a reboot (the reservation bullet in
  `.claude/rules/player.md`). A `noise` answer lost to a crash would otherwise put DoP back.
- **A file that does not read** is moved aside with a timestamp and never deleted, as the queue file
  is. In its place the player durably writes a fresh state that holds only the `state_unreadable`
  warning, so the refusal survives a restart. DSD is then refused at every rate
  (`dsd_state_unreadable`) until a choice is written again, and PCM plays. Falling back to the defaults would turn conversion on for a user whose file held
  `defaultMode: off`. A missing file is not a damaged one: it means the defaults, and the migration
  (§3.6) runs again from any legacy key still set.
- **No serial, port, card number or devnum** is stored. A test reads the file for them.
- **Backup and restore.** A backup carries the file. A restore installs the choices, DoP
  confirmations, first-use acknowledgements and listener answers. It drops `driverAcceptedAt` and
  `streamingObservedAt`, which belong to the box they were observed on and are quick to establish
  again.

## 3. The effective policy

### 3.1 Inputs

- **Profile:** §2, matched against the bound unit's identity from sysfs
  (`internal/alsa/sysfs.go:115-137`, `internal/alsa/qualification.go:18-57`).
- **Live capabilities:**
  - the bound PCM device's playback altsets, from its stream file (`internal/alsa/stream.go:57-81`,
    `internal/alsa/capabilities.go:28-86`);
  - the probe's answers (§5);
  - for DoP, the card's playback volume controls (§3.4).
- **The DAC's state:** its choice, DoP confirmation, first use and evidence (§2.5).
- **Health:** the hotplug snapshot is fresh. This is today's `Healthy`
  (`cmd/bridge/serve_player.go:448-455`).

### 3.2 The rule, per DSD rate

The rule covers DSD64, DSD128 and DSD256. DSD512 is refused in every mode, as today
(`rate_not_supported`; `internal/player/playback/native.go:56-61`, `authorization.go:310-312`).

Definitions, for a rate r:

- **Native offered:** an altset of the bound PCM device lists a native format at r ÷ its width, in
  today's order `DSD_U32_BE`, `DSD_U32_LE`, `DSD_U16_LE` (`authorization.go:229`), and the probe
  under this identity and kernel did not refuse it.
- **DoP carrier offered:** S32_LE at r ÷ 16 with at least 24 significant bits, the packer's
  requirement (`ops/b470-native-qualification.md:355-359`), and not refused by the probe.
- **DoP gain at unity:** §3.4.
- **Acknowledged at r:** first use of the transport acknowledged under the current bcdDevice by an
  entry that covers r, and not re-armed since by a rejection (§6). A write's acknowledgement covers
  every rate; a migrated one covers only the rates its record approved (§2.5, §3.6). So an owner who
  approved DSD64 alone gets no DSD128 until the app acknowledges again.
- **No rejection:** the latest listener answer for that transport at r under this bcdDevice is not
  `noise` or `nothing` (§1.2). A rejection outranks every positive input: a profile claim, a driver
  acceptance, streaming observed, and an earlier `clean`.
- **Conversion offered:** the DAC takes a target of §4.3.

The choice is the DAC's own, or the state's `defaultMode` for a DAC with none (§2.5).

| Choice | Plays native at r when | Plays DoP at r when | Otherwise, at r |
|---|---|---|---|
| `auto`, known DAC | native offered; acknowledged at r; no rejection | the profile heard DoP at r, or the user confirmed DoP; carrier offered; gain at unity; acknowledged at r; no rejection | converted |
| `auto`, unknown DAC | the latest listener answer for native at r is `clean`; native offered; acknowledged at r | the latest listener answer for DoP at r is `clean`; DoP confirmed; carrier offered; gain at unity; acknowledged at r | converted |
| `native` | native offered; r ≤ `maxBitRate`; acknowledged at r; no rejection | never | refused: `native_not_offered`, `dsd_rate_above_choice`, `first_use_pending`, `listener_rejected` |
| `dop` | never | carrier offered; r ≤ `maxBitRate`; gain at unity; the profile heard or stated DoP at r, or DoP confirmed; acknowledged at r; no rejection | refused: `dop_not_offered`, `dop_gain_not_unity`, `dop_gain_unverifiable`, `dop_confirmation_required`, `dsd_rate_above_choice`, `first_use_pending`, `listener_rejected` |
| `convert` | never | never | converted (refused as `conversion_not_offered` if the DAC takes no target) |
| `off` | never | never | refused: `dsd_off_by_choice` |

- **`auto` means no explicit choice.** It is the preselection the decision asks for. The app shows
  the mode it resolves to, and the user may keep it.
- **After a rejection under `auto`,** that transport is out at that rate. `auto` tries the next one
  (DoP after native), which needs its own first use, or converts.
- **Native comes before DoP** under `auto` where both qualify at a rate, today's preference
  (`authorization.go:258-269`).
- **The cap under `auto`:** a `maxBitRate` caps native and DoP, and rates above it are converted
  (§3.3).
- **Explicit `native` or `dop`** carries its first-use acknowledgement in the same write (§7.2), so
  it starts acknowledged at every rate. A bcdDevice change, or a rejection, re-arms first use. The
  mode then refuses as `first_use_pending` or `listener_rejected` until the app writes it again with
  the acknowledgement.
- **A migrated `native` or `dop`** (§3.6) starts acknowledged only at the rates its record approved.
  Any other offered rate refuses as `first_use_pending` under the explicit mode, or converts under
  `auto`, until the user acknowledges the transport or chooses the mode again. Either write widens
  the acknowledgement to every rate (§2.5).
- **A known DAC with no DoP claim at r** treats DoP as an unknown DAC does: `auto` needs the user's
  confirmation, and an explicit `dop` write without `confirmDoP` is refused (§7.2).
- **The listener-answer clause** under `auto` on an unknown DAC keeps a migrated owner approval
  (§3.6), and a DAC whose chime the user passed, on what was heard. A later `noise` takes it away.
- **One effective set** feeds both selection and publication, as today
  (`authorization.go:202-213`, R10 and R11). The `effectiveSet` shape stays, gaining the converted
  rates.

### 3.3 Above the effective maximum

The effective maximum is the user's `maxBitRate`, or what the profile, the kernel and the probe
offer.

- **Under `auto`,** a rate above it is converted.
- **Under explicit `native` or `dop`,** it is refused with `dsd_rate_above_choice` or the
  not-offered reason, and never converted. The app shows that row as not playable, with the reason
  (§7.6).
- **Under `convert`** there is no maximum: DSD64 to DSD256 all convert.

### 3.4 DoP and the hardware mixer

- **The risk.** A USB Audio Class feature unit can scale samples ahead of the DAC's DoP detection,
  and a scaled DoP word loses its markers.
- **Enforcing 0 dB is rejected.** The bridge writes no control (`internal/alsactl/control.go:1-2`;
  `ops/b470-native-qualification.md:132`), and its player has no mixer
  (`internal/config/player.go:121-123`). A write would set a level the user did not choose, for
  every later PCM track too, and the DAC's own volume may be the only one the user has.
- **So: detect, and refuse, failing closed.** The backend reads the bound card's controls through
  `alsactl.Reader` (bounded and read-only; `internal/alsactl/control.go:58-113`) and its descriptors
  through `QualificationGain` (`internal/alsa/qualification_gain.go:9-58`). It does so:
  - before a DoP occurrence is admitted;
  - at the check after its open;
  - on the 100 ms monitor while it plays (today's tick, `authorization.go:457-474`).

  B5 narrows the descriptor scan to the feature units on the playback path: those between the
  streaming interface's input terminal and its output terminal. Today's scan counts any feature
  unit on the device. A "level control" below is a MIXER-interface integer control whose name does
  not mark it as a capture control, whatever else its name says, not only `… Playback Volume`.

  DoP passes only in one of two topologies:
  1. **No gain stage.** The playback path has no feature unit, and the card has no level control.
     This is the topology today's unconstrained policies require (`authorization.go:161-174`;
     `internal/player/qualification/approval.go:218-229`).
  2. **Every gain stage visible and at unity.** Each level control is at unity in one of three
     ways:
     - it reads 0 dB on every channel by its dB TLV;
     - it is one of the profile's `analogVolumeControls` (§2.1);
     - it is one of the profile's `unityAtRawMax` controls (§2.1), reads exactly its `rawMax` on
       every channel, its range's maximum is still `rawMax`, and the firmware is one the entry
       lists.

     If the playback path has a feature unit with a volume control, at least one level control
     exists. The TLV is `Control.TLV` (`internal/alsactl/control.go:26`); B5 parses `DB_SCALE`,
     `DB_MINMAX` (with its mute variant), `DB_LINEAR` and `DB_RANGE`.

  Anything else refuses:
  - a level control off 0 dB, or a `unityAtRawMax` control below its `rawMax`, refuses as
    `dop_gain_not_unity`;
  - these refuse as `dop_gain_unverifiable`:
    - a level control with no dB scale that is not a `unityAtRawMax` control, or a control that
      cannot be read;
    - a `unityAtRawMax` control whose range's maximum is no longer `rawMax`, or on a firmware the
      entry does not list;
    - a feature unit on the playback path with no visible level control;
    - descriptors that cannot be read or parsed.

  A missing control never stands for unity while the descriptors show a gain stage. Switches and
  enumerations are not gains, and are allowed: a muted switch is silence, not corruption, and an
  enumeration that selects a DSP mode is the profile caveats' domain.
- **The analogue allowlist** (Argon). Some DACs expose an analogue attenuator after their DoP
  decoder, as a `Master Playback Volume` beside a digital `PCM Playback Volume`. Requiring 0 dB on
  it would make the user run the analogue stage at full level, against the first-use guidance. A
  profile names such controls in `dop.analogVolumeControls`, with provenance, and the check passes
  them over. An unknown DAC has none, so every level control must read 0 dB on it.
- **The raw-maximum flag** (Gemini). A known DAC can expose a digital level control with no dB
  scale whose raw maximum is unity. Without the flag it refuses DoP as `dop_gain_unverifiable`,
  which stays the default. A profile names such a control in `dop.unityAtRawMax`, and DoP is
  admitted only at that raw maximum, as the analogue allowlist admits its controls.

  Setting the flag needs evidence (§2.1): the maker's documentation, or a measurement on a named
  bcdDevice. The measurement is a byte-for-byte digital capture at the raw maximum, or a DoP stream
  the DAC's own indicator reports as DSD and that is heard clean. An unknown DAC, or a firmware the
  entry does not list, gets no such pass.
- **A change while DoP plays** revokes the occurrence: an immediate stop, and that rate out of the
  epoch (`PROTOCOL.md:1020`). It is a safety revocation, which needs no consent (§3.5).
- **What this cannot see:** a DAC's own DSP, such as a PEQ mode or a firmware EQ. That is why
  profiles carry caveats and why first use exists.
- **Native DSD has no such requirement.** A DSD-capable chip's own DSD volume is its own
  processing. A DAC whose volume does not apply to DSD is a level risk, which first use covers (§6).

### 3.5 What replaces `readPermission`, and what stays

**Replaced.** `policy.Effective`, over §3.1's inputs, replaces three parts of today's code:

- `readPermission`'s mandatory record read, with its stale, tampered and candidate refusals
  (`authorization.go:117-189`);
- its record monitor and the supervisor's refresh (`:457-507`);
- the reconciliation keyed on the record's hash (`:551-582`).

It is read again at the same points: every session, every activation, the 100 ms tick and the
supervisor's refresh (`cmd/bridge/serve_player.go:526-534`).

**Reconciled per occurrence, never wholesale.** Today a replaced record revokes every DSD
occurrence (`authorization.go:551-582`). B728 instead judges each occurrence of the session against
the new set whenever the effective set changes: a choice write, a probe answer, the gain check, a
hotplug or an identity change. Each occurrence is:

- **kept,** if its plan's tuple is still in the set (same transport, format, rate and timing). It
  plays on, untouched.
- **re-planned,** if its plan left the set and no frame of it is the kernel's yet. The backend asks
  the engine to hand it off again, through the path an edit after hand-off already takes: a
  successor not yet submitted is replaced, and a submitted one is cut
  (`.claude/rules/player.md:116`). It then plays under the new set, and is never skipped for the
  change.
- **revoked,** if its plan left the set and it is heard, or a frame of it is the kernel's. It stops
  at once, as today.

A choice write (§7.2) needs the user's `interrupt` if it would revoke the occurrence heard, or
re-plan one whose frames are already in a sink. Without `interrupt` it changes nothing. So a write
that changes only an unused rate cap revokes nothing, and asks nothing. A safety change revokes
without asking: the gain leaving 0 dB, the identity changing, the DAC leaving, or the state
becoming unreadable.

**Kept, unchanged:**

- the check after open compares the opened node's identity with the identity the set was computed
  for (today, with the record's: `authorization.go:427-436`), without the port;
- rate, format and channels exact (`internal/alsapcm/device.go:461-500`);
- the DoP writer's marker continuity (`internal/alsapcm/sink_dsd_writer.go:237-240`, `:262-300`);
- the bounded Stop and release (the release bullets in `.claude/rules/player.md`);
- hotplug and fault cancellation;
- the rate caps;
- no transport retry and no fallback at run time. A native open that fails faults the occurrence.
  The next selection reads the probe's new answer, which is a selection, never a retry.

**Kept for the harness only:**

- `qualification.ReadApproval` and `ReadCandidate`, which require the current version
  (`internal/player/qualification/approval.go:113`, and each report's at `:178`);
- `NewQualificationBackend` and `NewTransitionQualificationBackend` (`authorization.go:509-547`);
- the approval contract (`ops/b470-native-qualification.md:248-295`).

**The legacy reader, for the cutover alone** (§3.6, §11). `qualification.ReadLegacyApproval` runs
`readApproval`'s checks with two of them made parameters:

- **the version:** from `LegacyVersion` (35, frozen) to the build's own `Version`, never past it
  (B1's review: a newer bridge's evidence, written before a downgrade, is nothing this code can
  vouch for), for the record and its reports, where `ReadApproval` requires the current version;
- **the identity comparison:** production keeps today's full comparison, port included, until B7.
  The migration (§3.6) compares the model alone: vendor and product IDs, manufacturer, product,
  interface, device and subdevice, with no port.

Everything else stays: trust, the strict decode, the digest, schema, bounds, the evidence hashes,
the executed edges and the constraints (`:95-253`). Production reads through it from B1 to B7, so
no version bump stops an owner's record from playing before it is migrated. After B7 the migration
is its only caller.

**Dropped for users:** approval records, manual promotion, the anchored port in identity, edge
approval between transports, and invalidation on every code change.

**Refusal words.** The approval's words (`dsd_approval_unavailable`, `dop_not_allowed`,
`dsd_tuple_not_approved`, `dsd_policy_unavailable`; `authorization.go:63-82`) become the
harness's. Production refuses with §3.2's words. `dsd_not_in_this_release` (`open.go:21-24`)
stays the refusal of a backend built with no policy. Until B11 it also refuses every rate the
policy would convert.

### 3.6 Config keys and migration

**Today:** `output.player.dsdMode` (off, native, dop or auto; off by default), `allowDoP` and
`dsdQualification` (`internal/config/player.go:110-120`). `Problem` refuses an unknown mode
(`:424-428`), and the block's keys are listed at `:667`.

**B728:**

- **No new `bridge.yaml` key.** The per-DAC choice lives in the player's DSD state (§2.5).
- **The three keys stay** in `playerKeys`. The reader refuses an unknown key, and an unread block
  turns the player off (`internal/config/player.go:744-795`), so removing them would turn off every
  player that sets them.
- **They are migrated once,** at the first start of a bridge carrying B6 whose block sets any of
  them. The migration runs whenever the state holds no `migratedFrom`, so a state file deleted by
  hand is migrated again from the keys still set. It writes `defaultMode` at once. Anything for a
  particular DAC waits for that DAC to be the one the binding resolves to.
- **Unset and explicit `off` differ.** The raw field tells them apart: `""` is unset and `"off"` is
  written (`internal/config/player.go:110-114`, mapped together at `:220-227`). Today both refuse DSD
  (`authorization.go:119`). B728 keeps the explicit opt-out and gives the unset key the new
  defaults.

| Legacy | And | Migrates to |
|---|---|---|
| `dsdMode` unset | — | nothing: the defaults (§3.2) |
| `dsdMode: off`, written | — | `defaultMode: off`: every DAC with no choice of its own refuses DSD (`dsd_off_by_choice`), a DAC bound later included, until an explicit per-DAC choice in the app or the console. Warning `migrated_off` |
| `native` | a record the legacy reader takes for this unit's model (§3.5) | choice `native`; each approved native tuple's listener answer `clean`, on the record's `QualifiedOn` (schema 2) or undated (schema 1); first use acknowledged at the approved rates alone (`bitRates`, §2.5) |
| `dop` | `allowDoP: true` and such a record | choice `dop`; DoP confirmed; the DoP tuples `clean`; first use acknowledged at the approved rates alone |
| `dop` | `allowDoP: false` | `defaultMode: off`: today this refuses all DSD as `dop_not_allowed` (`authorization.go:250-252`), and converting would play what the operator held back. Warning `migrated_off` |
| `auto` | such a record | choice `auto`; its tuples `clean`, so `auto` plays them even on an unknown DAC (§3.2); first use acknowledged at the approved rates alone, for native, and for DoP only with `allowDoP: true`, which also confirms DoP. With `false`, DoP stays out until the app acknowledges its first use, an explicit act |
| `native` | no record, or one the legacy reader refuses | choice `native` with no answers and first use not acknowledged: it refuses (`first_use_pending`) until the app acknowledges, and never converts. Warning `migration_record_refused`, or `migration_record_missing` |
| `dop` | `allowDoP: true`, and no record or a refused one | choice `dop`, DoP not confirmed: it refuses until the app confirms and acknowledges. The same warning |
| `auto` | no record, or a refused one | nothing: the defaults. The same warning, which is how the app explains why DSD now plays converted |

- **The record is read through the legacy reader** (§3.5), with the model comparison: the port is
  not compared, and versions from 35 to the build's own are taken. The record's bcdDevice is recorded with each
  answer it migrates, which therefore publishes only on that firmware (§1.2). The state keeps the
  record's digest in `migratedFrom`, and nothing reads the record again.
- **The approval's rate limits survive** (Codex). The migrated acknowledgement lists the rates the
  record approved (`bitRates`), and admission is per rate (§3.2). A rate the record did not approve
  stays `first_use_pending` until the user acknowledges the transport or chooses the mode again.
- **A failure is a warning the app can read, not only a log line** (Gemini). A refused record
  carries its reason word (`stale_version`, `identity_mismatch`, `hash_mismatch`, `unreadable`); a
  missing record is `migration_record_missing`; a migrated opt-out is `migrated_off`. Each is
  written into the state's `warnings` (§2.5) beside the Warn line, and B8 publishes it in
  `dsdPlayback.warnings` (§7.1).
- **Validate warns once** for each legacy key while it is set, saying it was migrated and is no
  longer read. `logPlayerOff`'s once-per-reason rule (`internal/config/player.go:52-81`) is the
  model.
- **Words in `internal/config/player.go` change:**
  - `PlayerConfig`'s doc (`:83-98`), `EffectiveDSDMode`'s comment (`:220-227`) and `Problem`'s
    dsdMode refusal (`:427`);
  - `neverSettings`'s reason (`:683-687`). The keys it refuses by name (`dsdtopcm`, `dsd2pcm`,
    `dither`, `resample`) stay refused, since no config key converts or resamples. The reason stops
    saying that the player never converts.
- **`engine.Config.DSD`** (`cmd/bridge/serve_player.go:469`; `internal/player/engine/engine.go:93`)
  no longer decides anything.

## 4. The DSD→PCM path

### 4.1 Where it sits

- **`internal/player/dsdconv`** is a `decode.Decoder` over a `decode.DSDDecoder`
  (`internal/player/decode/decoder.go:23-49`). Canonical DSD planes go in; stereo int32 PCM with
  24 valid bits comes out.
- **The backend's open** creates it (`internal/player/playback/open.go:146-153`) when the effective
  set converts the content's rate. Everything after it is the PCM path, unchanged:
  - the packings table (`internal/player/playback/packing.go:8-19`);
  - the PCM sink and gapless joins;
  - the reporter, pause, release and Cut.
- **Pure Go, in process, no exec.** The bridge is one static binary with no runtime dependencies
  (CLAUDE.md, the pure-Go stack bullet). ffmpeg and sox are optional host tools behind the
  renditions' exec boundary.

### 4.2 Decoder and filter

**The target response:**

- linear phase;
- the passband within ±0.01 dB to 20 kHz and within 0.1 dB to 30 kHz;
- at least 100 dB stopband from 40 kHz. This is the phone converter's response and the faithful
  rendition's (`internal/transcode/dsd_render_chain.go:119-125`).

The chain, for 176.4 kHz output:

| Stage | Input → output | Filter | Work per output sample, per channel |
|---|---|---|---|
| 1 | one bit at r → 352.8 kHz (÷8, ÷16, ÷32 for DSD64, 128, 256) | linear-phase FIR, evaluated per input byte through 256-entry tables of partial sums, 8 taps a table | 12 / 20 / 40 table lookups and adds (96 / 160 / 320 taps) |
| 2 | 352.8 → 176.4 kHz (÷2) | half-band, about 31 taps | about 8 multiply-adds |
| 3 | 176.4 kHz | 30 kHz pass / 40 kHz stop, at least 100 dB, about 127 taps, symmetric | about 64 multiply-adds |

- **Tap counts** come from Kaiser's estimate for 110 dB, N ≈ (A − 8) / (2.285 · 2π · Δf / fs):
  - Stage 1 must hold 110 dB down the bands that fold into 0–40 kHz, k · 352.8 kHz ± 40 kHz. With
    Δf = 272.8 kHz that is 74, 147 and 294 taps, rounded up to table multiples.
  - Stage 3 needs Δf = 10 kHz at 176.4 kHz: 125 taps.

  These are starting sizes. B10 measures the response from the coefficients.
- **Tables and precision.** Stage 1's tables are float32 or int32: for DSD256, 40 tables halved by
  symmetry give 20 × 256 × 4 bytes = 20 KiB, inside a Cortex-A72's 32 KiB L1 data cache.
  Accumulation is float64.
- **88.2 kHz output** evaluates stage 3 at every other output; its aliases are already at least
  100 dB down.
- **44.1 kHz output** takes the compact tier's band: stage 3 becomes 20 kHz pass and 22.05 kHz
  stop.
- **Start.** The delay lines start at numeric zero in the filter's arithmetic, which is silence. They
  never start with zero bits, which map to −1.0, a full-scale negative offset (the DSD silence bullet
  in `.claude/rules/dlna-upnp.md`). DSD's own silence pattern, `0x69`
  (`internal/alsapcm/sink_dsd_writer.go:302-306`), filters to the same within the stopband. Exact
  zeros keep the joins of §4.7 exact by linearity.
- **End.** Bits past `Frames`, such as a DSF's padding in its last byte
  (`internal/player/format/pcm.go:105-108`), are not read. The group delay is flushed with numeric
  zeros, as frames of the occurrence.
- **Frames.** The output has `Frames ÷ D` frames, where D is the total ratio (16 for DSD64 to
  176.4 kHz). With the group delay compensated, output frame k is centred on input bit k · D, so
  positions stay exact to one output frame.

### 4.3 Target rate and depth

- **The DAC's best within the 44.1 kHz family, capped at 176.4 kHz:**
  - 176.4 kHz where the DAC's altsets take it at 24 bits or more, else 88.2 kHz, else 44.1 kHz;
  - 24 valid bits, in the first packing the table offers (`S24_3LE`, `S24_LE`, `S32_LE`;
    `packing.go:15-19`);
  - 16 bits only for a DAC that takes no 24-bit target.
- **Integer ratios only,** so there is no resampler. 176.4 kHz / 24-bit is also the faithful
  rendition's target (`pcm-v2-176400-24`; `internal/transcode/dsd_render.go:20`, `:69-79`).
- **Why not 352.8 kHz.** Nothing passes the 40 kHz stop that 176.4 kHz cannot carry, and stage 3 at
  352.8 kHz costs about twice as much. The owner decided on 2026-11-04 to cap conversion at
  176.4 kHz (§11). Going higher later would be one table row and a new CPU measurement.

### 4.4 Gain, headroom and clipping

- **Unity gain.** Bit 1 is +1.0, bit 0 is −1.0, and PCM full scale is the one-bit amplitude:
  - the SACD reference, 50 % modulation, lands at −6.02 dBFS;
  - the Scarlet Book's +3 dB peak, 70.7 %, lands at −3 dBFS;
  - stage 3's overshoot on a full-scale step is under 1 dB (Gibbs).

  Legal content therefore peaks below about −2 dBFS, with no gain applied.
- **No +6 dB.** The offline chain adds it only after measuring the whole file's true peak
  (`dsd_render_chain.go:37-50`; `ClipGuardedGainDB`, `:171-178`), which a player that starts at once
  cannot do. Unity gain also keeps every track of a gapless album at one level. The owner decided on
  unity gain for live conversion on 2026-11-04 (§11).
- **Clipping.** A sample past full scale, from an overmodulated stream, saturates at ±(2²³ − 1) and
  is counted. The count belongs to the occurrence (`playback.converted.clippedSamples`, §7.1), with
  one Info line per occurrence that clipped. There is no limiter or compressor.
- **Later, outside this plan:** a clip-guarded gain from the track's or the album's measured peak
  (`internal/manifest/dsd_peaks.go:10-29`), to match the loudness of the phone's converter.

### 4.5 Dither

- **24-bit output:** round half to even, with no added dither. DSD64's own in-band noise, about
  −120 dBFS, is about 24 dB above the 24-bit step (−144 dBFS), so it decorrelates the rounding
  error. A test checks that the error spectrum is flat. The output is deterministic, so golden
  hashes stay stable.
- **16-bit output** (a DAC with no 24-bit target): TPDF dither of ±1 LSB, from a fixed-seed xorshift
  re-seeded per occurrence, so tests stay deterministic.

### 4.6 CPU budget

These figures are estimates. B11 measures them before it merges.

| Rate | Stage 1 lookups/s | Stages 2–3 multiply-adds/s | Pi 4 (Cortex-A72), % of one core | Pi 5 (Cortex-A76), % of one core |
|---|---|---|---|---|
| DSD64 | 8.5 M | 25 M | about 4 % (13 % with a 3× allowance) | about 1.7 % (5 %) |
| DSD128 | 14 M | 25 M | about 5 % (14 %) | about 2 % (6 %) |
| DSD256 | 28 M | 25 M | about 6 % (19 %) | about 2.5 % (7 %) |

- **The estimate's basis:** stereo output at 176.4 kHz; 2 cycles per multiply-add and 1.5 per
  lookup for scalar Go, at the Pi 4's slower 1.5 GHz clock; a 3× allowance for bounds checks and the
  lack of SIMD; and a Pi 5 core about 2.5× a Pi 4's.
- **An independent bracket:** the offline chain's Stage A (ffmpeg's DSD decoder, then sox's
  very-high-quality `rate` and the 30/40 kHz sinc, in C with SIMD) measured 0.009, 0.014 and 0.022 s
  per second of DSD64, DSD128 and DSD256 audio on a fast Mac (`ops/engineering-log.md:5584-5586`,
  PR #863). A Pi 4 core is roughly 6 to 8 times slower single-threaded, which puts that chain at
  5–18 % of a Pi 4 core.
- **The gate:** B11's hardware tier runs `BenchmarkConvert` and real playback on a Pi 4 and a Pi 5,
  at DSD64, DSD128 and DSD256, watching CPU and xruns.
  - The budget is at most 25 % of one Pi 4 core at DSD256.
  - A miss gets int32 tables and an unrolled stage 3 first.
  - If it still misses, B11 ships conversion for DSD64 and DSD128 only, and DSD256 is refused
    (`conversion_not_offered`) until a faster converter lands. It never xruns by design.

### 4.7 Gapless, Stop, pause and seek

**Joins.** A converted occurrence is a PCM transport for the sink: rate, channels, packing and valid
bits (`.claude/rules/player.md:80`, `:135`). A join is one of three kinds:

| The successor | The join | At the boundary |
|---|---|---|
| converted, at the same DSD rate under the same plan (target and filter) | **continuous** | One converter carries its delay lines across. The boundary's frames are exactly the filtering of the continuous bit stream, as native playback sounds. |
| converted at another DSD rate onto the same PCM transport (Argon's case), or PCM of the same transport | **abutting** | The same open sink: no drain and no reopen. The predecessor's converter flushes its group delay with numeric zeros, and a new converter, primed with zeros, starts the successor. Nothing is inserted, so the join is gapless, but it is not sample-continuous. That is acceptable, since a continuous recording does not change its DSD rate or its codec mid-stream. |
| another PCM transport | **reopen** | Drain, close and reopen: an honest gap (`Transport.Join`). |

The boundary is the first output frame whose centre bit belongs to the next track. Position and
Audible are exact to one output frame.

**The join window, and Cut.** With the group delay compensated (§4.2), a continuous join makes the
predecessor's last H output frames depend on the successor's first bits. H is the composite
filter's half-length in output frames: about 70 at 176.4 kHz, or 0.4 ms. Today's Cut drops a
successor from its own first frame (`internal/player/playback/session.go:903-921`; the sink's Cut
at `o.start`, `:915`). Here that would leave the cut successor's onset folded into the
predecessor's last 0.4 ms, with the converter's state advanced past it. So:

- **Hold back.** The producer holds a converted occurrence's last H frames, with a snapshot of its
  converter taken before any successor bit enters, until the successor settles:
  - a continuous successor: the frames are computed with its bits;
  - an abutting successor, or none: they are computed from the snapshot with zero padding.

  The producer waits for that only while the sink's lookahead stays above a floor (100 ms). Below
  it, the window is finished as an end, and a successor that arrives later joins abutting.
- **Replacing or cutting the successor before the window is submitted** recomputes the window from
  the snapshot, for the new successor or for none. Nothing reached the sink, so nothing is rolled
  back.
- **After the window is submitted,** a cut of the successor puts the sink's marker at the window's
  first frame (the successor's first frame − H), not at the successor's own:
  1. it drops the window and everything after it;
  2. it submits the window again, recomputed from the snapshot;
  3. then the replacement, if there is one.

  The window's frames count as the predecessor's in every count, so the boundary stays at the
  successor's first frame.
- **Too late.** If any frame from that marker on is already the kernel's, the sink refuses the cut
  (`internal/alsapcm/sink.go:679`) and drops nothing. As with any late cut, the engine then stops
  playback when the successor is heard (`removed_entry_audible`; `.claude/rules/player.md:116`).
- **B11's tests delete, and replace,** a prepared converted successor at three moments: before the
  window is submitted; after it is submitted and before the kernel has it; and once the kernel has
  it. In each, the DAC's bytes must equal an oracle that converts the predecessor alone, zero-padded,
  followed by the replacement, or else show the late cut's stop.

**Stop** is the PCM sink's Drop, unchanged and immediate. The converter's lookahead, under 1 ms,
lives in the producer, never in the sink.
- **Pause and release** are the PCM sink's: PAUSE, or zeros. After `pauseReleaseSeconds` the release
  keeps the unplayed frames in the store, and a resume writes them (the sink's pause bullet in
  `.claude/rules/player.md`). The converter takes no part in a resume.
- **Seek.** Seeking within converted DSD stays unsupported: `timeSeek.dsd` remains `unsupported`
  (`internal/player/engine/backend.go:85-90`). The DSD demuxer's byte seek
  (`internal/player/decode/decoder.go:45-47`) makes it possible later.

### 4.8 What Signal Path may claim

Every claim is read from the occurrence's actual conversion plan, never from the 176.4 kHz / 24-bit
case. The plan is its target rate (176.4, 88.2 or 44.1 kHz, §4.3), its valid bits (24 or 16), its
passband (30 kHz, or 20 kHz at 44.1 kHz, §4.2), its dither (none at 24 bits, TPDF at 16, §4.5) and
its gain (0 dB). The plan is fixed when the occurrence opens.

- **The observation** says `transport: converted` and carries the plan with the source DSD rate and
  the clipped count (§7.1).
- **Details:** codec `DSD`, the plan's rate and valid bits, and `Lossless: false`, so OpenHome
  Info's Details say lossless 0 (the Time and Info bullet in `.claude/rules/openhome.md`).
- **The app** composes its line from those members, for example "DSD converted to PCM by the
  bridge's player · DSD64 → 44.1 kHz / 16-bit · 20 kHz low-pass · TPDF dither". It never says
  bit-exact. Its "Converted" status already exists ((iOS) `SignalPath.swift:479`).

### 4.9 Relation to `upscale`'s DSD render

**Shared with the offline render:**

- the target response (30 kHz pass / 40 kHz stop, at least 100 dB; `dsd_render_chain.go:119-125`);
- the 176.4 kHz / 24-bit faithful target (`dsd_render.go:20`, `:69-79`).

**Different:** the render runs offline, through ffmpeg and sox behind the exec boundary (GPL tools,
optional on a host; `internal/config/config.go:2069-2099`). It measures the whole file first,
applies a clip-guarded album gain of up to +6 dB and shaped dither, and serves a FLAC sidecar to
the phone. The player's converter runs in process, in pure Go, in real time, at unity gain. It
plays only to the bound DAC and puts nothing on the wire.

**Rejected: playing a fresh `pcm-*` rendition in place of live conversion.** It carries a
clip-guarded album gain, so a rendition beside live conversion in one album would step the level
at a gapless join. Renditions are opt-in, so the common case still needs the converter. And the
player's open would inherit the variants' freshness rules in a path that today opens one file.

**The invariant.** "No server-side transcoding" (CLAUDE.md:91; `.claude/rules/jobs.md:132`; long
form `ops/History-Cross-Repo.md:11`, "the rule is about the serving path") is scoped, not dropped
(§9.1).

## 5. The silent probe

**When it runs.** All of these must hold: the bound DAC is available, the hotplug snapshot is
fresh, no engine session holds the device, and no release is pending. The probe runs:

- at the player's start, once the output is available;
- when the bound unit's enumeration changes (the hotplug generation);
- after a bcdDevice change;
- after a choice write (§7.2).

It never runs on a timer, and never while a session holds the device or a pause still holds it.

**What it does:**

- It opens the bound endpoint's playback node as every open does: non-blocking, exclusive
  (`internal/alsapcm/device.go:265-289`).
- It runs PVERSION and INFO (`:332-372`).
- For each candidate it runs HW_REFINE only (`:401-406`), with access, format, subformat, channels
  and rate set as `setHW` sets them (`:376-382`). The candidates are:
  - the native formats at r ÷ width;
  - S32_LE at the DoP carriers, reading the refined significant bits;
  - the conversion targets of §4.3.

  It also reads whether the setup offers PAUSE, which sets the tuple's pause method (§7.1).
- It never sends HW_PARAMS, SW_PARAMS or PREPARE, writes nothing, and writes no control.
- It closes the node. HW_FREE is not needed, since nothing was set.
- Each candidate's answer (accepted, or refused with EINVAL) goes into the DSD state, with the
  identity, the bcdDevice and the kernel release.

**A busy node.** EBUSY, usually MPD holding the DAC, is recorded as not probed, and nothing waits.
The holder is left alone (`internal/alsapcm/device.go:302-303`; the busy bullet in
`.claude/rules/player.md`). The probe takes the backend's device slot, so a Start that arrives
meanwhile waits for it. That wait is bounded at 1 s; a probe is one open, a few ioctls and a close.

**What it establishes:** that the kernel's driver accepts that exact access, format, subformat,
channel count and rate on this endpoint, given the device's descriptors and the kernel's quirks.
That is `driver_accepted`. For native DSD: the kernel would configure a DSD altset. For DoP: only
that the kernel takes the PCM carrier with at least 24 significant bits.

**What it does not establish:**

- that the DAC decodes anything;
- that the altset switch at HW_PARAMS and PREPARE succeeds (rare refusals happen there);
- that the bus has bandwidth for DSD256 on a busy hub;
- that the device's bit order matches the kernel's quirk;
- that a DoP word survives the DAC's own DSP;
- anything about the analogue level.

**Silence is not guaranteed on every device.** HW_REFINE is computation in the kernel and selects no
altset: snd-usb-audio sets the interface in its hw_params and prepare paths. But the open itself can
wake a device from USB autosuspend. A DAC that clicks on waking clicks at a probe as at any open,
which is why the probe runs only at the moments above and never periodically.

**Why probe beside `/proc`.** The stream file already lists each altset's formats, rates and bits
(`internal/alsa/stream.go:57-81`). The probe asks the combination an open will ask, under the
kernel's rules across altsets, and gets the kernel's own answer rather than a parse.

## 6. First use and the optional check

### 6.1 The app's half

The acknowledgement sheet and the large Stop ship with the mode UI in I4, so no build of the app can
enable native or DoP without them. Only the optional chime waits for I5 (§11).

**Before the first DSD play** on a newly enabled native or DoP transport (I4). That is either a
write that selects it, or `auto` resolving to it with `firstUse` pending.

- The app shows a sheet: turn the amplifier or the headphones down before the first DSD track; a
  large Stop stays on screen.
- DoP on an unknown DAC adds the confirmation (§7.4).
- The write then carries `firstUseAcknowledged` (§7.2).

**During that first play** (I4), while first use is `acknowledged` and not yet `observed`, the
bridge player's now-playing view shows a large Stop. It is OpenHome's Stop, the engine's immediate
release.

**The optional check (I5):**

1. "Play a test chime" sends POST check.
2. While the chime plays, a large Stop sends DELETE check.
3. When it completes, the app asks: "Did you hear a clean chime, first on the left, then on the
   right?" The answers are clean, noise or nothing, sent by POST answer.
4. Noise or nothing offers Convert to PCM as the next step.

### 6.2 The bridge's half

**First use and `streaming_observed`** (B7, published by B8). The backend records
`streaming_observed` when an occurrence of a native or DoP tuple has played 5 s of content without a
fault (§1.1), and first use then becomes `observed`. B9's write takes the acknowledgement. So the
safety half the app needs is on the bridge before the app's I4 ships.

**The check (B12).** One runs at a time.

- **Admission is the effective policy's.** The tuple asked for, transport and rate, must be one the
  effective set plays now for this DAC (§3.2): offered, gain at unity, first use acknowledged, DoP
  confirmed where the DAC has no DoP claim, and no rejection. Being merely offered is not enough. A
  tuple outside the set is refused, `check_unavailable` with the set's reason. So the check cannot
  play anything an ordinary track could not. To check native on an unknown DAC, the user first
  chooses native, with its acknowledgement, which makes native the set's for this DAC.
- If something is playing, the request needs `interrupt: true`, or it gets 409 `would_interrupt`.
- The bridge prepares the chime (below) before it stops anything.
- It stops the engine and waits for the release's acknowledgement.
- It enters maintenance: the engine's own hold, which closes mutations; exit leaves the transport
  stopped and never autoplays (the maintenance bullet in `.claude/rules/player.md`).
- It plays the chime through the transport and rate under test, with that tuple's plan.
- It drains, releases and exits maintenance.
- The whole check is bounded at 10 s of wall clock, the harness's own bound for a contact
  (`ops/b470-native-qualification.md:33`).

**The chime:**

- 0.5 s of DSD silence;
- 0.7 s of 880 Hz on the left;
- 0.7 s of 1320 Hz on the right;
- 0.5 s of DSD silence;
- 20 ms raised-cosine edges, and a peak of −30 dBFS of the one-bit full scale, the harness's
  listening level (`ops/b470-native-qualification.md:20-21`).

The two tones are modulated natively at the check's rate, by B725's modulator, which will be in a
production package by then. `internal/dsdtone` is first-order, with a −45 dBFS noise floor, and is
for tests only (`internal/dsdtone/dsdtone.go:1-14`).

The silent channel during each tone, and the two pads, are `0x69` bytes at that rate. Nothing is
repeated bit for bit. Repetition is a zero-order hold in the one-bit domain: `0x69` with each bit
doubled reads `00111100…`, which moves the silence's idle tone down toward the audio band and
raises its local offset (Argon).

Modulating 0.7 s of one channel at DSD256 is about 7.9 M modulator steps. At an estimated
20–50 ns a step in scalar Go on a Pi 4, the two tones take 0.3–0.8 s. Each rate's chime is made once
per process and kept in memory (DSD256: 2.4 s of stereo, about 6.8 MB). Nothing is embedded in the
binary. The check reports `preparing` until the chime is ready, bounded at 2 s, and B12 measures
the time on a Pi 4.

### 6.3 Stop paths

Every Stop ends the check, bounded by the sink's release deadline (2 s):

- the app's DELETE;
- an OpenHome Stop: the surface asks the player first, since maintenance answers writes with 501
  (`.claude/rules/openhome.md`);
- the console's Stop;
- the DAC unplugged (the sink's ENODEV fault);
- a fault;
- the 10 s bound;
- serve's shutdown.

The first real play's Stop is the ordinary one: OpenHome's Stop, which is the engine's immediate
release.

### 6.4 What is recorded

| Event | Recorded in the DSD state |
|---|---|
| First use acknowledged | `firstUse[transport].acknowledgedAt`, with the bcdDevice and, for a migration, the `bitRates` it covers (§2.5) |
| 5 s of content played without a fault | `evidence.streamingObservedAt`; first use `observedAt` |
| The chime completes and the answer is clean | `listener: {answer: clean, on}` for the transport, rate and bcdDevice, replacing any earlier answer |
| The chime completes and the answer is noise or nothing | `listener: {answer: noise or nothing, on}`, replacing an earlier `clean`. That transport is refused at that rate in every mode, and its first use re-arms (§3.2), until a write chooses that mode again |
| The chime is stopped or fails | nothing but the check's own state; an answer to it is refused (`check_not_answerable`) |

Never recorded: who answered, beyond `app` or `console`; a device token; a serial; a port.

## 7. The protocol

### 7.1 The read side: `GET /v1/outputs`, kind `bridge`

All additions are additive, and `ProtocolVersion` stays 1 (the `/v1/outputs` bullet in
`.claude/rules/wire.md`).

```json
"bridge": {
  "dsdMode": "auto",
  "allowDoP": false,
  "dsdPlayback": {
    "bootId": "…", "policyGeneration": 12,
    "tuples": [
      { "format": "DSD_U32_BE", "bitRate": 2822400, "channels": 2, "policyId": "native/DSD_U32_BE/2822400",
        "pause": "kernel", "tailMs": 100, "prefillMs": 400, "evidence": "streaming_observed", "evidenceOn": "2026-11-05" }
    ],
    "converted": [ { "bitRate": 11289600, "rateHz": 176400, "validBits": 24, "passbandHz": 30000, "dither": "none", "filter": "dsdconv-1" } ],
    "refused": [],
    "warnings": [ { "code": "migration_record_refused", "reason": "stale_version" } ],
    "model": "<manufacturer and product>", "bcdDevice": "<bcd>"
  },
  "dsdDAC": {
    "dacKey": "d1-<16 hex>",
    "profile": { "id": "fiio.qx13", "name": "FiiO QX13",
      "caveats": [ { "code": "dop_noise_in_hidden_mode", "transport": "dop", "note": "…" } ] },
    "choice": { "mode": "auto", "maxBitRate": 0, "revision": 4, "dopConfirmed": false },
    "options": [
      { "mode": "native", "offered": true, "rates": [ { "bitRate": 2822400, "evidence": "streaming_observed" } ] },
      { "mode": "dop", "offered": true, "needsConfirmation": false, "blocked": "dop_gain_not_unity",
        "rates": [ { "bitRate": 2822400, "evidence": "reported" } ] },
      { "mode": "convert", "offered": true, "rateHz": 176400, "validBits": 24, "passbandHz": 30000, "dither": "none" },
      { "mode": "off", "offered": true }
    ],
    "firstUse": [ { "transport": "native", "state": "observed", "bitRates": [2822400] }, { "transport": "dop", "state": "pending" } ],
    "check": null
  },
  "playback": { "transport": "converted", "format": "S24_3LE", "rateHz": 176400, "validBits": 24, "logicalRate": 0,
    "converted": { "fromBitRate": 2822400, "filter": "dsdconv-1", "passbandHz": 30000, "dither": "none", "gainDB": 0, "clippedSamples": 0 } }
}
```

**Existing members, new meanings:**

- **`dsdMode` and `allowDoP`** stay baseline members, always present. `dsdMode` is the bound DAC's
  choice word now: `auto`, `native`, `dop`, `convert` or `off`. `allowDoP` says whether DoP may play
  for it, by its profile or by the user's confirmation. They describe; they permit nothing. The app
  already reads them that way ((iOS) `BridgeOutputsResponse.swift:324`, `:326`). Today they are the
  boot config (`cmd/bridge/serve_player_output.go:123-126`; `PROTOCOL.md:904`).
- **`dsdPlayback.tuples`** are the effective set's native and DoP tuples, built as today
  (`serve_player_output.go:180-197`).
  - `policyId` stays an opaque string.
  - `pause` is `kernel` where the setup offers PAUSE, otherwise `silence`.
  - `prefillMs` is the profile's timing or 400 ms; `tailMs` defaults to 100 ms. The hardware tier
    measures both.
  - New optional members: `evidence` (§1.1) and `evidenceOn` (YYYY-MM-DD).
- **`qualificationRevision` and `qualifiedOn`** are no longer sent, since there is no record. A
  client already reads them as optional.
- **`model` and `bcdDevice`** are the bound unit's manufacturer and product strings and its
  descriptor firmware code, redacted as today (`serve_player_output.go:199-210`).
- **`codecs`** lists `dsf` and `dff` while `tuples` or `converted` is non-empty. Today `tuples`
  alone decides (`serve_player_output.go:167-169`; `PROTOCOL.md:915`).

**New members:**

- **`dsdPlayback.converted`** (optional array): each converted rate's plan (§4.8), as `{bitRate,
  rateHz, validBits, passbandHz, dither, filter}`:
  - `validBits` is 24 or 16;
  - `passbandHz` is 30000, or 20000 at 44.1 kHz;
  - `dither` is `none` or `tpdf`.

  An older bridge omits it; it is `[]` when no rate converts.
- **`dsdPlayback.refused`** (optional array): `{bitRate, reason}` for each rate from DSD64 to DSD256
  that plays neither way, with `reason` from §3.2's closed list.
- **`dsdPlayback.warnings`** (optional array, Gemini): `{code, reason}` for what the app must be able
  to explain, from the state's `warnings` (§2.5). The codes are a closed list:
  - `migration_record_refused`, with the legacy reader's reason word (`stale_version`,
    `identity_mismatch`, `hash_mismatch`, `unreadable`);
  - `migration_record_missing`;
  - `migrated_off`;
  - `state_unreadable`, a damaged state moved aside, with DSD refused until a choice is written.

  It sits in `dsdPlayback`, which is published whenever the backend is ready. A damaged state
  therefore shows even while no DAC is available and `dsdDAC` is omitted. It is `[]` when there is
  nothing to explain, and an older bridge omits it.
- **`dsdDAC`** (optional object): the bound DAC's key; its profile (id, name, caveats); the choice
  and its revision; the options for each mode (offered, rates with evidence, `needsConfirmation`,
  `blocked`); first use per transport, with `bitRates` listing the rates a migrated acknowledgement
  covers (absent: every rate); and the running or last check. It is omitted while no DAC is
  available.
- **`playback`:** `transport` gains `converted`, with `format`, `rateHz` and `validBits` the PCM's,
  the plan's actual values, and `logicalRate` 0. A new optional `converted` object gives the plan's
  `{fromBitRate, filter, passbandHz, dither, gainDB, clippedSamples}`. A native or DoP occurrence
  gets an optional `evidence` member.
- **Health flags.** `bridgeDSDChoice` arrives with the PUT (B9) and `bridgeDSDCheck` with the check
  routes (B12). Each is advertised while the player's gate holds and its route is mounted, and is
  absent before (Argon). B8 publishes the read members alone, so no client sees a flag for a route
  that is not mounted. A client keys on the flags, never on a version ("Don't label a spec section
  with a version", `.claude/rules/wire.md`).

### 7.2 The write side

All four routes are `rateWrite`, bearer-token (`s.authed`) and bounded, in the pattern of
`internal/api/route_classification.go:258-331`. Each answers 404 `local_outputs_not_supported` while
neither gate holds, as `GET` does. It answers 404 `output_not_found` for an id that is not this
bridge's player, which covers every id while the player's own gate is shut.

**`PUT /v1/outputs/{id}/dsd`: the choice.**

Body: `{"dacKey", "baseRevision", "mode", "maxBitRate", "confirmDoP", "firstUseAcknowledged",
"interrupt"}`. `mode` is `auto`, `native`, `dop`, `convert` or `off`. `maxBitRate` is 0 (no cap),
2822400, 5644800 or 11289600.

Checks, in order:

1. The body parses (400 `bad_request`).
2. A bound DAC is available now (409 `dac_unavailable`).
3. `dacKey` is that DAC's key (409 `dac_changed`).
4. `baseRevision` is the stored revision (409 `stale`, carrying the current `dsdDAC`).
5. The mode is offered at some rate (409 `mode_not_offered`).
6. DoP on a DAC whose profile has no DoP claim carries `confirmDoP` (409
   `dop_confirmation_required`).
7. Native or DoP whose first use is not acknowledged under this bcdDevice carries
   `firstUseAcknowledged` (409 `first_use_unacknowledged`). A rejection re-arms first use (§1.2),
   so a write that chooses a rejected transport again always carries it, and that write clears the
   rejection.
8. The write would leave alone what plays, or carries `interrupt` (409 `would_interrupt`, nothing
   changed). The test is §3.5's per-occurrence predicate, computed against the set the write would
   publish: would it revoke the occurrence heard, or re-plan one whose frames are already in a sink?
   A write that changes only an unused rate cap, or an unused mode, therefore needs no `interrupt`.
   This is the console's rule and its word (the console bullet in `.claude/rules/player.md`).

Then the bridge:

1. writes the state durably (500 `persist_failed`, nothing changed);
2. probes the DAC if it is free;
3. recomputes and publishes the effective set (`policyGeneration` + 1);
4. reconciles each occurrence (§3.5): it keeps what stays in the set, re-plans successors that left
   it, and revokes only what step 8 allowed;
5. answers 200 with `{dsdDAC, dsdPlayback}`, read from the published epoch.

That read-back is the effective choice, exactly as `GET` returns it afterwards.

**`POST /v1/outputs/{id}/dsd/check`.** Body: `{dacKey, transport, bitRate, interrupt}`. Answers 202
`{check}`, whose state goes `preparing`, `playing`, then `complete`, `stopped` or `failed`.

Admission is the effective policy's (§6.2). The tuple must be one the effective set plays now, so
first use acknowledged, DoP confirmed where the DAC has no DoP claim, gain at unity and no rejection
all hold. Being merely offered is not enough.

Refusals:
- 409 `dac_changed`, `would_interrupt` or `check_running`;
- 409 `check_unavailable`, for a tuple outside the effective set (with the set's reason for it,
  such as `first_use_pending`, `dop_confirmation_required` or `dop_gain_not_unity`), or a busy
  device.

**`DELETE /v1/outputs/{id}/dsd/check`.** Answers 204 once the check's release is confirmed, or at
once when no check runs.

**`POST /v1/outputs/{id}/dsd/check/answer`.** Body: `{dacKey, checkId, heard}`, where `heard` is
`clean`, `noise` or `nothing`. Answers 200 `{dsdDAC}`. 409 `check_not_answerable` covers a check
that did not complete, is older than 10 minutes, was already answered, or belongs to another DAC.

**Error table.** New rows: `dac_unavailable`, `dac_changed`, `mode_not_offered`,
`dop_confirmation_required`, `first_use_unacknowledged`, `would_interrupt`, `check_running`,
`check_unavailable`, `check_not_answerable`. Reused rows: `bad_request`, `stale`, `persist_failed`,
`output_not_found`, `local_outputs_not_supported`. Guards hold PROTOCOL.md's table to what the
handlers send (the error table bullet in `.claude/rules/wire.md`).

**Identity binding.** The key names the DAC model the binding resolves to at the write, and the
state is keyed by model identity. So another DAC plugged into a port binding keeps its own choice,
and two units of one model share one choice, as the phone's per-DAC choice does ((iOS)
`SettingsAudioView.swift:76-78`).

### 7.3 Versioning and the Mirror-PR pair

- **Version.** Additive members, optional objects, new tokens (`converted`, the evidence words, the
  refusal words, the caveat codes) and new routes. `ProtocolVersion` stays 1, and so does the app's
  `supportedProtocolVersion` (the wire-protocol bullet in CLAUDE.md).
- **What an older app does with a B728 bridge:**
  - it drops unknown members, and treats `transport: converted` as unknown ((iOS)
    `.claude/rules/bridge-player.md:60`);
  - it grants DSD from tuples alone, so converted rates stay refused, as DSD is today;
  - it shows no qualification line, which needs `qualifiedOn` ((iOS)
    `BridgeOutputPlayerSignalInfo.swift:78-87`).

  Native and DoP tuples reach an older app only after a new client has acknowledged first use, or
  where an owner approval was migrated (§3.6). So a bridge update alone never starts native or DoP
  behind an app that cannot show the guidance (§10).
- **The pairs.** Each bridge PR that changes PROTOCOL.md (B8, B9 and B12) has an iOS pair that
  copies the file byte for byte, with the DTOs and the fixtures the bridge's exporter writes
  (`BRIDGE_EXPORT_IOS_FIXTURES`, the last bullet of `.claude/rules/wire.md`).

### 7.4 iOS: the per-DAC mode UI

- **Where.** The bridge player's settings, reached from its row in the output picker, get a "DSD on
  <DAC name>" section. The name is the profile's, or the USB product string with "Unknown DAC".
- **The mode picker:**
  - "Automatic — <resolved mode>" (recommended), "Native DSD", "DoP", "Convert to PCM" and "Off";
  - a mode that is not offered is disabled, with the bridge's reason;
  - a maximum-rate picker for native and DoP offers DSD64, DSD128 and DSD256, among the offered
    rates.
- **Caveats.** The profile's caveats show under the picker, in the profile's words. The phone's own
  "DSD per DAC" section is the model ((iOS) `SettingsAudioView.swift:552`, `:534`, `:35-45`).
- **DoP on an unknown DAC** asks for confirmation: "1-bit doesn't know this DAC. DoP sent to a DAC
  that doesn't decode it plays as noise, much of it ultrasonic. Turn on DoP only if this DAC's maker
  says it supports DoP." The buttons are Turn On and Cancel.
- **Conflicts.** Writes carry the key and the revision just read. On 409 `stale` or `dac_changed`
  the app reads again and shows the current state. On `would_interrupt` it asks "Stop the DSD
  that's playing?"
- **Evidence** shows per rate, in §1.3's words.
- **A rate a migrated acknowledgement does not cover** (`firstUse.bitRates`) asks for the first-use
  sheet again before the app sends that rate's write.
- **Warnings** (`dsdPlayback.warnings`) each have a sentence. For example, for
  `migration_record_refused`: "Your earlier approval for this DAC couldn't be carried over (it was
  made by another version), so DSD plays as set here until you choose a mode." For
  `state_unreadable`: "The bridge couldn't read its DSD settings, so DSD is off until you choose
  again."

### 7.5 iOS: Signal Path labels

- **The qualification line is replaced.** "Owner listening qualified" and "Owner listening
  qualification: <model> (firmware <bcd>), <transport> <rate>, <date>." ((iOS)
  `BridgeOutputPlayerSignalInfo.swift:78-87`) become an evidence line: "DAC evidence: <rung> · <model>
  (firmware <bcd>)".
- **What stays:** the transport lines ("Opened DAC transport: native DSD …", "… DoP …"; `:70-77`) and
  the footer (`:89`).
- **Converted** gets "DSD converted to PCM by the bridge's player". Its detail is composed from the
  observation's plan (§4.8, §7.1), never from fixed text:
  - "<source> → PCM <rateHz> / <validBits>-bit";
  - "<passbandHz> low-pass";
  - "TPDF dither", when `dither` is `tpdf`;
  - "unity gain";
  - "<n> samples clipped", when any clipped.

  For example, "DSD64 → PCM 176.4 kHz / 24-bit · 30 kHz low-pass · unity gain", or "DSD128 → PCM
  44.1 kHz / 16-bit · 20 kHz low-pass · TPDF dither · unity gain". It stays distinct from the
  rendition's "DSD converted to PCM on the bridge" and the phone's "on this iPhone" ((iOS)
  `SignalPath.swift:556-558`).
- **Never,** for DSD on the bridge's player: verified, decoded, bit-exact, seal or lock. The copy
  table's header rule extends to it.

### 7.6 iOS: the DSD grant (I2) and R12

**Today:**

- A grant exists only for a correlated cast with fresh evidence ((iOS)
  `BridgeOutputPlayerQueue.swift:141-151`; (iOS) `.claude/rules/bridge-player.md:50`, `:80`).
- R12's step 8 needs a stereo tuple at the row's exact `bitRate` whose eligibility is native or DoP
  ((iOS) `BridgeOutputPlayerQueue.swift:424-442`; (iOS) `.claude/rules/bridge-player.md:64`).
- The refusal copy says "a rate <player> isn't approved for" ((iOS) `BridgeOutputPlayerQueue.swift:642`).

**B728:**

- **Step 8** also passes a row when a `dsdPlayback.converted` entry has its exact `bitRate`, with
  positive `rateHz`, a `validBits` of 24 or 16, and a known `dither` word, from the same grant's
  set. `converted` lives inside `dsdPlayback`, so I3b's epoch and freshness rules cover it unchanged.
- **With neither,** the reason is `.dsdRateNotPlayable(label)`, with the copy "<title> is <rate>, a
  rate <player> isn't set to play." When `refused` names the rate, its reason's sentence follows.
- **`codecs`** keeps gating: the grant's codecs must list `dsf` or `dff`, which the bridge does
  whenever `converted` or `tuples` is non-empty.
- **I5's switch** keeps stopping all DSD, converted included.
- **No grant** comes from `dsdDAC`, `dsdMode` or `allowDoP`.

## 8. The qualification version and the guarded prefixes

**Today:** `qualificationVersion` is 35 (`internal/player/qualification/version.go:4`). The guard
is `GuardedPaths` (`version_gate.go:12`) plus `guardedPrefixes` (`:15`):

- `cmd/bridge/serve_player`, `cmd/bridge/output_`
- `internal/player/` hotplug, openhome, engine, playback, binding, decode, format and qualification
- `internal/api/outputs_bridge`, `internal/manifest/`
- `internal/alsa/`, `internal/alsapcm/`, `internal/alsactl/`
- `internal/fsutil/`, `internal/usbid/`, `internal/config/`

CI's native-qualification job refuses a change there without a bump (`CheckVersion`, `:17-75`), and
each bump makes all owner evidence stale (`ops/b470-native-qualification.md:3-4`). Versions 18 to 35
were mostly test, logging and console changes that changed no DSD byte
(`ops/b470-native-qualification.md:600-713`).

**After B728** the version binds only the harness's own reports and records. The owner decided on
2026-11-04 to narrow the guard as proposed here (§11).

**The cutover's order (Codex).** Until B7, production still reads the owner's record, and
`ReadApproval` refuses a record of another version or another identity
(`internal/player/qualification/approval.go:113`, `:132`, and each report's at `:178`). So any bump
merged before B7 would stop that record from playing before B6 migrated it. B1 therefore does three
things in one PR:

1. it adds the legacy reader (§3.5), whose version and identity checks are parameters;
2. it switches production's read to it, taking versions from 35 to its own, so no later bump strands
   the record;
3. it narrows the guard and takes version 36.

B6 then migrates through the same reader with the model comparison. B7 is the first PR in which
production reads no record. No guarded change merges before B1, since B1 comes first.

B1 narrows the guard to the code that changes DSD bytes or the harness's evidence:

- **Paths:** `internal/player/format/dsd_pack.go`, `internal/player/format/dop_pack.go`,
  `internal/player/playback/native.go`, `internal/player/playback/edges.go`,
  and `internal/alsactl/control.go`, `control_linux.go` and
  `uapi.go`. B1's review added, by an audit of the calls from the DSF and DFF demuxers' reads to
  the sink's write (the record is in `ops/engineering-log.md`): the files that shape the native
  DSD bytes or their count, `internal/player/decode/dsd.go`, `dsf.go`, `dff.go`,
  `pcm_region.go` and `offset.go`, and `internal/player/format/pcm.go`; and the helpers the
  harness's readers take evidence and a unit's identity through, `internal/fsutil/notafile.go`
  and `notafile_unix.go`, and `internal/usbid/port.go`. Round 3 added `internal/alsactl/constraints.go`,
  `CheckConstraints`, which holds an approval's mixer constraints at play time.
- **Prefixes:** `internal/alsapcm/`, `internal/alsa/`, `internal/player/qualification/` and
  `cmd/bridge/output_dsdtest`. Round 3 put `internal/alsa/` back whole: the qualification identity,
  the sysfs reader and its tree reads, the capabilities and the qualification gain are all
  evidence the harness rests on, and since B1's legacy reader takes every version from 35 to the
  build's own, a bump no longer strands the owner's record; it costs only the harness's
  in-flight contacts, candidates and reports.
- **Dropped:** everything else in today's two lists.

**What this plan touches, after B1:**

| PR | Guarded paths it touches | Version |
|---|---|---|
| B1 | `internal/player/qualification/approval.go` (the legacy reader) and `version_gate.go`. Production's switch to the legacy reader is in `authorization.go`, which is unguarded once B1's own narrowing applies | 36 |
| B4, the probe | `internal/alsapcm/` | 37 |
| B7, the cutover | `internal/player/playback/native.go` and `edges.go` (plan construction) | 38 |
| B11, conversion wired | `native.go`, whose DSD open (`internal/player/playback/native.go:23-74`) is where a convert plan branches off | 39 |
| B12, the check | `native.go`, and `internal/alsapcm/` if the check needs a sink seam | 40 |
| every other PR (B2, B3, B5, B6, B8, B9, B10, B13) | none. Their code is in new packages or paths B1 unguards | unchanged |

Two PRs that take the same number collide at merge, as they do today, and the later one raises it
at merge (`ops/b470-native-qualification.md:651-655`). The runbook's header changes twice:

- in B1: production reads an approval record only through the legacy reader, until B7, and the
  version invalidates harness evidence alone;
- in B7: no production path reads an approval record.

## 9. Rules to change (proposed wording, not applied)

Each rule lands in the PR that changes its code. Its long form goes in its History file in the same
commit, and the record goes in `ops/engineering-log.md`. Text in "…" stands for a sentence kept as it
is.

The conversion is two code PRs, and each carries the scoped replacements its code needs. B10, the
converter, carries `player.md:27` (no transformed samples) and `:36` (no DSD to PCM): its package
would otherwise break both. B11, the wiring that makes the player convert, carries `:133` to `:135`,
the gapless rule included, with CLAUDE.md:91 and `jobs.md:132`. No conversion code merges before the
rule that permits it.

### 9.1 Bridge

1. **CLAUDE.md:91** (B11). OLD: "**No server-side transcoding, ever.** 1-bit is bit-exact by
   mission: … a rendition never substitutes for the bit-exact source when the client asked for the
   source." NEW: "**No server-side transcoding on the wire, ever.** 1-bit is bit-exact by mission:
   `/v1/download` serves the file as-is via `http.ServeContent`, renditions are OFFLINE sidecar files
   served through `serveVariant`, and a rendition never substitutes for the bit-exact source when the
   client asked for the source. The bridge's own player converting DSD to PCM for its own DAC (B728)
   is playback, not transcoding, and happens only where the DAC's effective policy converts.
   **Never serve a stream the player converted, on `/v1`, DLNA or the loopback route, and never
   call a converted occurrence bit-exact.** → History-Cross-Repo.md". The long form at
   `ops/History-Cross-Repo.md:11` changes in the same commit.
2. **`.claude/rules/jobs.md:132`** (B11). NEW: "**No server-side transcoding in a request path,
   ever.** Conversion for a client is offline; `/v1/download` serves bit-exact via
   `http.ServeContent`. The DSD → PCM renditions (PR #863) are conversion in exactly that sense, a
   sidecar the job pool built earlier and served as a file, never a decode in the request path. The
   player's live DSD → PCM (`internal/player/dsdconv`, B728) feeds only the player's DAC. **Don't
   decode in a request path, and don't serve the player's converted PCM.**"
3. **`.claude/rules/player.md:27`** (B10). NEW: "**PCM is stereo signed integer, 16/24/32 valid bits
   only.** No downmixing, truncation, resampling, floating intermediates, ReplayGain, normalization,
   dither or crossfade in the PCM path. Native DSD is produced by the DSF and DFF demuxers (the DSD
   section below), and DoP's S32_LE packer plays what the effective policy names (B728). The one
   transform that changes samples is `internal/player/dsdconv` (B728): DSD to PCM, a `Decoder`
   upstream of the packers, used only where the effective policy converts, whose occurrences report
   `converted` and never lossless or bit-exact (its section below). **Don't add another transform
   that changes samples, and don't let a converted occurrence report itself lossless or bit-exact.**"
4. **`.claude/rules/player.md:36`** and its long form, `ops/History-rules-player.md:19` (B10). The
   Don't becomes: "**Don't convert DSD to PCM in a demuxer or anywhere but `dsdconv`, don't reverse
   a byte the container declared MSB-first, and don't round a DSD count up by adding before
   dividing.**"
5. **`.claude/rules/player.md:86`** (B6). The Don't becomes: "**Don't fail the load over the block,
   quote a value in a reason unless it is a word its field knows, save an alias whose anchor the save
   does not write, add a config setting that changes samples (DSD conversion is the per-DAC choice the
   player's DSD state keeps, B728), drop a legacy DSD key from the keys the block takes (an unknown
   key leaves the block unread and the player off), or make Mode answer for the player.**"
6. **`.claude/rules/player.md:133`** (B7, completed in B11). The title becomes "**Content is refused
   before any of it plays, by its kind, and DSD the effective policy does not play.**" The DSD
   sentence becomes: "DSD at a rate the effective set (B728: the profile × the live capabilities ×
   the DAC's choice) neither plays nor converts is refused with its named reason
   (`dsd_off_by_choice`, `native_not_offered`, `dop_not_offered`, `dop_gain_not_unity`,
   `dop_gain_unverifiable`, `dop_confirmation_required`, `first_use_pending`,
   `dsd_rate_above_choice`, `listener_rejected`, `conversion_not_offered`, `rate_not_supported`),
   and as `dsd_not_in_this_release` (`playback.ReasonDSD`) by a backend built with no policy; the
   approval's words (`dsd_approval_unavailable`, `dop_not_allowed`, `dsd_tuple_not_approved`,
   `dsd_policy_unavailable`) are the harness's alone." The Don't becomes: "**Don't refuse with any
   reason but the kind's or the policy's, play DSD at a rate the effective set does not name, or
   start content whose first chunk did not decode.**"
7. **`.claude/rules/player.md:134`** (B11). It adds: "Nothing after the decoder converts a value; a
   converted occurrence's decoder is `dsdconv`, whose samples are packed by the same table." The
   Don't becomes: "**Don't convert a sample after the decoder, offer a packing outside the table, or
   keep a packing past `Discard`.**"
8. **`.claude/rules/player.md:135`**, gapless (B11). The title becomes: "**A successor of the same
   transport follows in the same sink with nothing between, and one of another transport plays after
   a drain, a close and a new sink, an honest gap its `Join` names. Converted DSD occurrences joined
   at one DSD rate under one plan share one converter, whose last H frames before the join wait for
   the successor to settle; any other successor on the same transport abuts, each side zero-padded,
   in the same sink** (B728)." It adds: "a converted occurrence's transport is the PCM it becomes;
   the join window's frames are the predecessor's." The Don't becomes: "**Don't write anything
   between two occurrences of one transport, report a join gapless across transports, restart the
   converter inside a continuous join, submit a converted occurrence's join window before its
   successor settles (or the lookahead reaches its floor), or cut a converted successor at its own
   first frame once its window is submitted: cut at the window and submit it again.**" The sink's
   own gapless bullet (`:80`) is unchanged, since the sink is.
9. **`.claude/rules/player.md:187`, `:189` and `:197`**, with their long forms at
   `ops/History-rules-player.md:805-847` and `:1001-1007` and in `ops/History-b470-native.md` under
   "Owner approval (2c.2b)" (B7):
   - `:187`'s title becomes "**Native engine hand-off (B470 2c.2): DSD plays under the effective
     policy (B728).**"
   - `:189` becomes "**Owner approval (2c.2b) is the harness's alone since B728; production
     rechecks the effective policy, edges and DSD-only revocation as it rechecked approval.** …
     **Don't read an approval record in serve.**"
   - `:197` becomes "**DoP (B470 2d.2, B728): one effective set over the profile × the live
     capabilities × the DAC's choice feeds selection and the published epoch.** `auto` plays native
     first where the kernel offers it, DoP where the profile heard it or the user confirmed it, and
     converts the rest; an explicit `native` or `dop` refuses what it cannot play. DoP needs three
     things: its carrier with at least 24 significant bits; a gain topology §3.4 passes (no gain
     stage, or every level control at 0 dB or on the profile's analogue list); and its first use
     acknowledged. A listener's rejection refuses a transport at a rate in every mode. A native↔DoP
     edge needs no report in production; the transition scenario is the harness's. **Don't play DoP
     on a DAC with no DoP claim without the user's confirmation, with a level control off 0 dB,
     unreadable or missing beside a gain stage, or before its first use was acknowledged; play a
     transport a listener rejected; fall back to another transport at run time; publish anything
     but the effective set; join same-carrier PCM and DoP; or put the USB port or serial on the
     wire.**"
10. **A new section in `.claude/rules/player.md`, "Player DSD by capability and choice (B728)".**
    Each bullet lands with its code:
    - B1: "**Until B7, production reads an approval record only through the legacy reader, which
      takes versions from 35 to the build's own; the migration reads it the same way, comparing
      the model and not the port.** **Don't bump `qualificationVersion` while production reads
      through `ReadApproval`, or re-validate a record being migrated against the current version or
      its port.**"
    - B2: "**The DAC profile database matches by USB identity, never by name: vendor and product
      IDs, the interface, and the product string, which a shared reference ID needs; bcdDevice only
      narrows. A `dacKey` hashes the key's canonical form, five LF-joined lines with `=` before a
      present optional field and `-` for an absent one.** **Don't match a profile by its name, add a
      claim without its provenance, let two profiles match one identity, import from the iOS
      registry anything but its DoP evidence and caveats, compare keys field by field, or let an
      absent interface or product hash like a present one.**"
    - B3: "**The player's DSD state (`player-dsd.json`) keys each DAC by its model identity, keeps
      a bridge-wide `defaultMode`, records which rates each acknowledgement covers and which
      warnings the app must explain, and makes a write durable before it is answered. A state that
      does not read refuses DSD until a choice is written again.** **Don't store a serial, a port, a
      card number or a devnum, answer a write before it is durable, delete a state file that does
      not read (move it aside), fall back to the defaults over one, or widen a migrated
      acknowledgement past its record's rates without a write.**"
    - B4: "**The silent probe opens the bound node only while it is free and no session holds it,
      refines each candidate, and closes.** **Don't send HW_PARAMS, SW_PARAMS, PREPARE, a write or
      a control write from it, wait on a busy node, or probe on a timer.**"
    - B5: "**The evidence ladder is reported, driver accepted, streaming observed, listener
      confirmed, and no word claims that a DAC decodes DSD. The latest listener answer stands, and a
      rejection outranks every positive input.** **Don't word a rung as verified, decoded,
      bit-exact or a lock, count a rung across a bcdDevice change, publish DoP's driver acceptance
      as more than its carrier's, or let an earlier `clean` survive a later `noise`.**"
    - B7: "**Admission is per rate. DoP plays only where the playback path shows no gain stage,
      or every level control reads 0 dB by its dB scale, is on the profile's analogue list, or is a
      raw-maximum control at its raw maximum on a listed firmware. A policy change is reconciled per
      occurrence (kept, re-planned or revoked), and a user's write revokes only with
      `interrupt`.** **Don't admit a rate a migrated acknowledgement does not cover, write a
      control, take a missing control or one with no dB scale for unity unless the profile's
      evidence says its raw maximum is, keep DoP playing once a control leaves unity, revoke an
      occurrence the new set still plays, or skip a successor a change re-planned.**"
    - B10: "**`dsdconv` is linear phase and unity gain, 30 kHz pass / 40 kHz stop at 100 dB or
      more (20 kHz at 44.1 kHz), starts and ends at numeric zero, saturates and counts, and rounds
      24-bit output with no added dither (TPDF at 16 bits).** **Don't add gain the stream was not
      measured for, limit or compress, prime a delay line with zero bits, or read past `Frames`.**"
    - B11: "**A converted occurrence's claims (Details, the observation, `converted`) are its
      plan's: rate, valid bits, passband and dither.** **Don't state 24 bits, 30 kHz or 176.4 kHz
      for a plan that is not.**"
    - B12: "**The check admits only a tuple the effective set plays now, prepares a chime modulated
      natively at that rate, and plays it inside the engine's maintenance hold, for at most 10 s;
      every Stop ends it.** **Don't play a tuple that is merely offered, repeat a lower rate's bits
      for a higher one, touch the queue, autoplay after it, or record an answer for a check that did
      not play to its end.**"
11. **`.claude/rules/output.md`**, the codecs bullet (B11). The title becomes: "**The element's codecs
    are `playback.PlayedContainers` (flac, wav, aiff, alac) and, while the effective set names a
    tuple or a converted rate, dsf and dff (B728); …**" The Never adds: "**…list dsf or dff while
    the effective set plays no DSD…**"
12. **`.claude/rules/wire.md`**, the DSD capability bullet (B8, completed in B9). NEW: "**DSD
    capability is `bridge.dsdPlayback`'s effective set, its native and DoP tuples and its
    `converted` rates, under its boot and epoch, never passive DAC arrays; `bridge.dsdDAC` is the
    bound DAC's choice and evidence, and `PUT /v1/outputs/{id}/dsd` is the one way to change it
    (B728); `bridge.playback` is the distinct current occurrence observation.** **Don't publish a
    rate the player would not play, grant from `dsdDAC`, `dsdMode` or `allowDoP`, or put a serial
    or a port in a DAC key.**"
13. **`.claude/rules/openhome.md`**, new (B12): "**While the player's DSD check runs, the surface's
    Stop ends the check and answers success; every other write answers 501, as in maintenance**
    (B728). **Don't let a Stop wait past the sink's release bound, or start anything when the check
    ends.**"
14. **`ops/b470-native-qualification.md:1-10`**, in two steps:
    - B1: "Production reads an approval record only through the legacy reader, which takes versions
      from 35 to this build's own and none past it, until B728's cutover
      (`ops/plan-b728-dsd-capability-design.md`). This runbook's evidence binds `qualificationVersion`,
      whose guard covers the DSD byte path and the harness alone."
    - B7: "Since B728 no production path reads an approval record or a candidate: the player plays
      DSD by its effective policy. This runbook is the engineering harness's."

### 9.2 iOS

1. **(iOS) `.claude/rules/bridge-player.md:64`**, I3a and R12 (I2). Step 8 becomes: "a tuple of that
   `bitRate` with `channels == 2` whose B574 eligibility is native or DoP, or a
   `dsdPlayback.converted` entry of that `bitRate` with positive `rateHz`, a `validBits` of 24 or 16
   and a known `dither` word (B728) (`.dsdRateNotPlayable(label)`, or `.dsdUnplayable` with neither
   at any rate)". The Don't
   becomes: "**Don't admit DSD by anything but the grant's tuples and `converted` entries (no MPD
   gate, DAC altset, renderer capability, `dsdDAC`, `dsdMode` or `allowDoP`), …**"
2. **(iOS) `.claude/rules/bridge-player.md:60`**, B574 (I1). It adds: "playback's `dop`, `pcm`,
   `converted` or absent; a tuple's `evidence` and `evidenceOn`, `dsdPlayback.converted` and
   `refused`, read as I1 reads its own." The Don't adds: "**…read `converted` as PCM or as
   bit-exact, or an evidence word as a verification…**"
3. **(iOS) `.claude/rules/bridge-player.md:84`**, I4 (I3). The qualification line's condition gives
   way to the evidence line. The Don't becomes: "**…or say bit-exact, bit-perfect, verified,
   decoded, a seal or a lock of DSD on the bridge's player, or show a converted occurrence as
   bit-exact (the footer says what is not checked).**"
4. **New** (I4, which ships the sheet and the Stop with the mode UI): "**The bridge player's DSD
   choice is written only by `PUT /v1/outputs/{id}/dsd`, with the DAC key and revision just read,
   after the first-use sheet for native or DoP, and after the confirmation for DoP on a DAC the
   bridge's database does not know; the first DSD play on a newly enabled native or DoP mode keeps a
   large Stop on screen until the bridge reports first use `observed`.** **Don't send native or DoP
   without `firstUseAcknowledged` from a sheet the user saw, DoP without `confirmDoP` where the
   bridge asks for it, or a choice for a DAC the app did not just read; don't hide the Stop before
   `observed`, and don't decide eligibility from the choice.**"
5. **New** (I5, the optional chime alone): "**A chime is offered only for a tuple the bridge plays
   now, and its answer is sent only for a check that completed.** **Don't answer for a stopped
   check, or present a listener confirmation as a verification.**"
6. **(iOS) `.claude/rules/settings-ui.md:123`**, B677 (I4). Its statement about bridge 0.2.2's player
   playing PCM becomes: the player plays PCM, and DSD as the bridge's per-DAC choice allows.
7. **(iOS) `docs/BridgeProtocol.md`**, in each mirror.

## 10. Risks, and what stays in the engineering harness

| Risk | What happens | Mitigation | Residual |
|---|---|---|---|
| DoP to a DAC that does not decode it (an unknown DAC the user confirmed, or a known DAC in a hidden mode) | It plays the PCM words. The markers alone are a square wave at the carrier's Nyquist (88.2 kHz for DSD64) at about −28 dBFS, with the payload's noise below it (Argon's figures in the consult): not 0 dBFS, but continuous ultrasonic energy into an amplifier and tweeters at full gain | Never by default on an unknown DAC; explicit confirmation; first-use guidance to turn the gain down; a large Stop; the chime; the `dop_noise_in_hidden_mode` caveat; the unity-gain check | A user who confirms DoP wrongly, at high gain |
| Native DSD whose bit order or format is wrong for a firmware | Noise, up to full scale | Kernel quirks were checked on hardware by whoever added them; first use; Stop | A firmware the quirk does not fit |
| Gain bypass: the DAC's volume does not apply to DSD | DSD louder than PCM | First-use guidance; a caveat code where it is known | An unknown DAC that bypasses |
| Transport switches | Clicks at a PCM↔DSD reopen on some DACs | Honest gaps; the `transition_clicks` caveat; prefill per profile | Clicks on unprofiled DACs |
| Conversion CPU (DSD256 on a Pi 4) | Xruns | B11's measured gate; DSD256 conversion refused if it misses | none at merge |
| Conversion quality | Aliasing or a weak stopband | The response from the coefficients; aliasing measured as a differential and the stopband on a steady state (the lessons in `ops/History-rules-jobs.md` under the decimation and `sox stats` bullets); a golden | The filter's documented shape |
| Profile database errors | A wrong ID or a shared ID preselects the wrong mode | Product strings for shared IDs, captures, provenance, review, lint; first use still applies | A wrong capture that passes review |
| iOS evidence used on Linux | Kernel quirks, implicit feedback or class compliance differ | Only DoP evidence is imported (DoP decoding is the DAC's, whatever the host); its provenance names the iOS source (`heard DSD128: iOS profile <row> … (iOS <commit>)`); first use | A DAC that behaves differently under Linux |
| False refusals from the mixer check | DoP refused needlessly by a feature unit placed after DoP detection, a missing dB TLV, an analogue attenuator on an unprofiled DAC, or a feature unit the playback-path parse cannot place | The reason says which; a profile's `analogVolumeControls`; native or convert | Inconvenience only: the check fails closed by design |
| The cutover strands an owner's record | A version bump before B7 would refuse the record, and the owner's DSD would stop before it was migrated | B1 switches production to the legacy reader (versions 35 to its own) before any bump; B6 migrates before B7 stops reading (§8) | none |
| A converted successor cut near its boundary | Its onset folded into the predecessor's last 0.4 ms | The join window is held back and recomputed on a cut (§4.7) | A cut too late stops playback at the boundary, as any late cut does |
| An older app with a B728 bridge | — | Native and DoP start only after a new client acknowledges first use; converted rates are invisible to the old grant; no qualification line is shown | none |
| The state file lost or damaged | Choices or answers lost | Durable writes. A damaged file is moved aside, and DSD is refused until a choice is written again, so a migrated `off` never becomes conversion. A missing file migrates again from the keys still set | Choosing again after damage |
| Privacy | — | `dacKey` is a hash; no serial or port is stored or sent; redaction is kept | none |
| A check while something plays | Playback interrupted | `interrupt` is explicit; maintenance never autoplays | none |
| A click at a probe (wake from autosuspend) | One click on some DACs | The probe runs only at the moments in §5 | A click on such DACs |

**What stays in the engineering harness:**

- `bridge output dsd-test`: contacts, the matrix, interactive runs, the backend lifecycle and the
  transition scenario;
- `bridge output play --contact-report`;
- the approval and candidate readers, and the manifest and contact schemas;
- `qualificationVersion`, with the narrowed guard and CI's `CheckVersion`;
- `ops/b470-native-qualification.md`, relabelled.

Two uses remain. When a DSD byte-path change ships (a packer, a sink, the probe), the owner can rerun
contacts and the matrix on a real DAC. And when a profile's native claim needs hearing deliberately,
this is how. The harness never grants anything in production.

## 11. The PR plan

### Merge-gate tiers

The bridge's tiers are named here, from the jobs of `.github/workflows/gate.yml`:

- **docs:** `make docs-check`; CI's docs-check job.
- **standard:** the pre-push gate `make fmt vet test build-all`, run through the shared build lock,
  and the whole CI gate (vet, including the 32-bit vet; `test -race`; build-all; gofmt; docs-check).
- **player:** standard, plus CI's native-qualification job (`CheckVersion` with the bump where a
  guarded path changed, and both native-integration overlays), plus `alsa-abi` on amd64 and arm64
  for `internal/alsapcm` and `internal/alsactl`.
- **hardware:** player, plus the owner's run on a Raspberry Pi with a real DAC, its numbers and
  observations recorded in the PR.
- **mirror:** the iOS PR copies PROTOCOL.md byte for byte with the exported fixtures, and the pair
  merges together.

iOS: every iOS PR here changes app logic, so the full suite is its gate (the Release compile, then
XCTest and Swift Testing on iOS 26.5 and 27.0; (iOS) `docs/MultiPRBatchWorkflow.md:40`). The
data-only tier ((iOS) `docs/MultiPRBatchWorkflow.md:47`) does not apply, since the profiles live in
the bridge.

### The PRs

Each PR is about a day of worker time. Tests marked red first fail on the tree before the PR.

| # | Repo | PR | Depends on | Tests, red first | Tier |
|---|---|---|---|---|---|
| B1 | bridge | The cutover's first step (§8): `qualification.ReadLegacyApproval`, with the version and the identity comparison as parameters (§3.5). Production reads through it, taking versions from 35 to its own. The guard narrows to §8's paths and prefixes; version 36; the runbook's first header (§9.1, item 14); the legacy reader's bullet | none | `TestProductionStillPlaysAVersion35RecordAfterTheBump` (red before: `stale approval version`), `TestTheLegacyReaderRefusesBelowVersion35`, `TestTheLegacyReaderKeepsEveryOtherCheck` (a tampered body, a changed evidence byte, an unexecuted edge), and `TestQualificationVersionGate`: a change under `internal/config/` passes without a bump (red before), and one to `internal/alsapcm/sink_dsd_writer.go` still needs one | player |
| B2 | bridge | `internal/player/dacprofile`: schema with `analogVolumeControls`, embedded JSON, matcher, lint; the kernel-quirk seed; iOS DoP evidence for captured models; `dac inspect --json` gains `manufacturer`, `product` and `bcdDevice`; its bullet (§9.1, item 10) | B1 (its `dac inspect` change is guarded until B1) | `TestAProfileMatchesByUSBIdentityNeverByName`, `TestASharedReferenceIDNeedsItsProductString`, `TestNoTwoProfilesMatchOneIdentity`, `TestEveryClaimNamesItsProvenance`, `TestAnAnalogueControlNeedsItsProvenance`, `TestAUnityAtRawMaxEntryNeedsItsEvidenceAndFirmware`, `TestTheDACKeyMatchesItsVectors` (§2.2: an absent interface, an absent product, a product of `-`), `TestTheQX13CarriesItsHiddenModeCaveat`, `TestDACInspectPrintsTheModelIdentity` | standard |
| B3 | bridge | The player's DSD state (§2.5: `defaultMode`, the latest listener answer, an acknowledgement's `bitRates`, `warnings`, a damaged file refuses DSD) and the migration's logic through the legacy reader with the model comparison (§3.6): pure, not wired; its bullet | B1, B2 | `TestAWriteIsDurableBeforeItReturns`, `TestADamagedStateRefusesDSDAndIsMovedAside`, `TestNoDSDStateHoldsASerialOrAPort`, `TestTheMigrationTable` (every row of §3.6, explicit `off` among them), `TestTheMigrationIgnoresThePortAndTakesVersion35`, `TestAMigratedAcknowledgementListsItsRecordsRates`, `TestAMigrationFailureIsAWarningInTheState`, `TestABackupCarriesTheChoicesAndRestoreDropsThisBoxsEvidence` | standard |
| B4 | bridge | The silent probe (`alsapcm.Probe`) and its cache (§5); version 37; its bullet | B1 | `TestTheProbeRefinesAndNeverCommits` (the fake kernel records no HW_PARAMS, SW_PARAMS, PREPARE or WRITEI, one open and one close), `TestTheProbeLeavesABusyNodeAlone`, `TestTheProbeReadsTheDoPCarriersSignificantBits`, `TestNoProbeWhileASessionHoldsTheDevice` | player |
| B5 | bridge | `policy.Effective`: the §3.2 table with the rejection rule and `defaultMode`; the DoP gain topology (§3.4: the playback-path descriptor parse, every level control, the analogue list); the per-occurrence reconciliation predicate (§3.5). Pure, not wired; the evidence-ladder bullet | B2, B3 | `TestTheEffectivePolicyTable` (every row of §3.2), `TestARejectionOutranksEveryPositiveInput` (native under `auto` on a known and an unknown DAC, after an earlier `clean`), `TestAutoConvertsAboveTheCapAndAnExplicitChoiceRefuses`, `TestAMissingControlIsNotUnity` (a playback feature unit with no visible level control), `TestEveryLevelControlCountsWhateverItsName`, `TestTheAnalogueListAndTheRawMaxFlagAreTheOnlyExemptions`, `TestARawMaxControlPassesOnlyAtItsRawMaximum` (below it, a changed range and an unlisted firmware refuse), `TestAdmissionIsPerRateForAMigratedAcknowledgement`, `TestNoDoPOnAnUnknownDACWithoutConfirmation`, `TestAChangeIsReconciledPerOccurrence` | standard |
| B6 | bridge | The migration at serve's player start (§3.6): explicit `off` becomes `defaultMode: off`; an approval's acknowledgement covers its approved rates alone; each failure is written as a warning beside its Warn line; the legacy keys still taken, each warned once; config words; the rule at `player.md:86`. Production still reads the record through the legacy reader | B3 | `TestAnExplicitOffSurvivesAsTheDefault` (red before: unset and `off` both refuse, and nothing tells them apart), `TestAMigratedApprovalCoversOnlyItsRates` (red before: a DSD64 approval's transport-wide acknowledgement admits DSD128), `TestAVersion35RecordOnAnotherPortMigrates`, `TestARefusedRecordLeavesAWarning`, `TestTheMigrationRunsOnceAndAgainAfterTheStateIsDeleted`, `TestLegacyDSDKeysStillLoadAndAreWarnedOnce`, `TestAPlayerBlockWithLegacyKeysNeverTurnsThePlayerOff` | standard |
| B7 | bridge | The cutover: the effective policy replaces `readPermission` in serve and in `output play`, with admission per rate (a migrated choice plays only the rates its record approved); per-occurrence reconciliation, with prepared successors re-planned through the engine's hand-off path; `driver_accepted` from real opens; `streaming_observed` and first use `observed` recorded; until B11 a converted rate refuses as `dsd_not_in_this_release`; the runbook's second header; version 38; the rules at `player.md:133`, `:187`, `:189` and `:197`, and the DoP-topology bullet | B4, B5, B6 | `TestServeReadsNoApprovalRecord`, `TestSelectionAndPublicationReadOneSet` (kept from 2d.2), `TestTheOpenedNodeMustBeTheIdentityThePolicyWasComputedFor`, `TestAnUnusedRateCapChangeRevokesNothing`, `TestAChangedChoiceReplansThePreparedSuccessorAndSkipsNothing`, `TestAGainChangeRevokesDoPWithoutConsent`, `TestStreamingObservedNeedsFiveSecondsWithoutAFault`, `TestNoTransportFallsBackAtRunTime`, `TestAMigratedOwnerSetupPlaysAsBefore`, `TestAMigratedChoicePlaysOnlyItsApprovedRatesUntilAcknowledged` | player |
| B10 | bridge | `internal/player/dsdconv` (§4.2 to §4.5), not wired, with the join-window snapshot and recompute (§4.7); the rules at `player.md:27` and `:36`, and the converter's bullet | none | `TestTheResponseMeetsItsMask` (from the coefficients, the 30 kHz and the 20 kHz plans), `TestAliasingIsADifferential` (CIFF fixtures, `internal/transcode/testdata/gen/dsd_fixtures.py`, or B725's), `TestUnityGainLevels` (a −20 dBFS `dsdtone` sine gives −20 dBFS ± 0.05), `TestOvermodulationSaturatesAndCounts`, `TestDelayLinesStartAtNumericZero`, `TestPaddingPastFramesIsNotRead`, `TestTheJoinWindowRecomputesFromTheSnapshot`, `TestAnAbuttingJoinIsZeroPadded`, `TestTheOutputIsDeterministic`, `BenchmarkConvert{DSD64,DSD128,DSD256}` | standard |
| B11 | bridge | Conversion in the player: `native.go`'s DSD open branches for a convert plan; continuous, abutting and reopen joins (DSD64 then DSD128 on one sink included); the join window held back and recomputed on a cut; the observation and Details from the plan (§4.1, §4.7, §4.8); version 39; the rules at CLAUDE.md:91, `jobs.md:132`, `player.md:133-135` and `output.md`, and the claims bullet | B7, B10 | `TestAnUnknownDACPlaysDSDConverted`, `TestConvertedDSDReachesTheDACPackedByTheTable`, `TestConvertedTracksJoinContinuouslyAtOneRate`, `TestDSD64ThenDSD128ConvertedShareTheSink` (no reopen), `TestDeletingOrReplacingAPreparedConvertedSuccessor` (the three moments of §4.7, against the oracle), `TestStopDuringConversionIsBounded`, `TestTheClaimsAreThePlans` (a 44.1 kHz, 16-bit DAC is said to get 16 bits, 20 kHz and TPDF), `TestAConvertedTrackIsNotLossless`, `TestNoConvertedStreamIsServed` | player, hardware (CPU and xruns on a Pi 4 and a Pi 5, §4.6; a listening pass) |
| B8 | bridge | The read side (§7.1): `dsdDAC` with first use and its `bitRates`, `evidence`, `converted` with each plan, `refused`, `warnings` (migration failures and a damaged state), the `converted` playback token and plan; no health flag yet; PROTOCOL.md; fixtures; the `wire.md` DSD bullet | B11 | `TestTheBridgeElementCarriesTheDACChoice`, `TestDSDOutputProtocolMembers` and `TestDSDOutputProtocolTokens` (new rows), `TestAConvertedEntryCarriesItsPlan`, `TestAMigrationFailureIsPublished`, `TestADamagedStateIsPublishedWithNoDAC`, `TestNoHealthFlagBeforeItsRoute`, `TestNoDACKeyOrModelCarriesASerialOrAPort`, the fixture exporter | standard, mirror (I1) |
| B9 | bridge | `PUT /v1/outputs/{id}/dsd` (§7.2): CAS, key binding, confirmation, acknowledgement, the per-occurrence `would_interrupt`, durable write, read-back; the `bridgeDSDChoice` flag; the `wire.md` bullet completed | B8 | `TestTheChoiceIsDurableBeforeItIsAnswered`, `TestAStaleRevisionChangesNothing`, `TestAChoiceForAnotherDACIsRefused`, `TestDoPOnAnUnknownDACNeedsConfirmation`, `TestNativeOrDoPNeedsFirstUseAcknowledged`, `TestChoosingARejectedTransportAgainClearsTheRejection`, `TestAnAcknowledgementWidensAMigratedChoiceToEveryRate`, `TestAWriteClearsItsDACsWarnings`, `TestOnlyAWriteThatInterruptsNeedsTheGoAhead`, `TestTheAnswerIsTheEffectiveChoice`, `TestTheChoiceFlagArrivesWithItsRoute`; the route classification and error-table guards | standard, mirror (I4) |
| B12 | bridge | The check (§6.2, §7.2): admission by the effective policy, the chime modulated at its own rate, the `preparing` state, the answer, the `bridgeDSDCheck` flag; an OpenHome Stop ends a check; PROTOCOL.md; version 40; the `openhome.md` rule and the check's bullet | B9, B11, B725's modulator in a production package | `TestTheCheckAdmitsOnlyWhatTheSetPlays` (red before: an offered tuple with first use unacknowledged, and unconfirmed DoP), `TestTheChimeIsModulatedAtItsOwnRate` (no repeated bits; the pads are `0x69`), `TestEveryStopEndsTheCheckWithinItsBound`, `TestTheCheckNeverAutoplays`, `TestOnlyACompleteCheckIsAnswered`, `TestANoiseAnswerRefusesTheTransportInEveryMode`, `TestTheCheckFlagArrivesWithItsRoute` | player, hardware (the owner hears the chime on a real DAC), mirror (I5) |
| B13 | bridge | The console's Player page shows and sets the per-DAC choice and evidence, with the same first-use sheet (`cmd/bridge/serve_player_console.go:517-559`, the wording at `:532`) | B9 | the console's view and write tests for each state | standard |
| I1 | iOS | Mirror of B8: PROTOCOL.md copied; DTOs for `dsdDAC` with first use and its `bitRates`, `evidence`, `converted` with its plan, `refused`, `warnings`, and the `converted` playback token; fixtures | B8 | decode tests: a malformed new member withholds only its own object (I1's rule); `converted` reads as its own token | full suite |
| I2 | iOS | R12 admits converted rates; the refusal copy (§7.6) | I1 | `testAConvertedRateIsEligibleUnderAFreshGrant`, `testAConvertedEntryFromAnotherEpochAdmitsNothing`, `testNoConvertedEntryKeepsTheRateRefused`, `testARefusedRateSaysItsReason` | full suite |
| I3 | iOS | Signal Path: the converted card composed from the plan, and the evidence line (§7.5) | I1 | copy-table pins with no forbidden word for DSD on the bridge's player; `testTheConvertedLineIsThePlans`; `testAConvertedOccurrenceIsNeverBitExact` | full suite |
| I4 | iOS | The per-DAC mode UI (§7.4) with the first-use sheet and the large Stop (§6.1), the sheet shown again for a rate a migrated acknowledgement does not cover, and each warning's sentence; mirror of B9 | B9, I1 | view-model tests for each state (known and unknown, offered and blocked, `stale`, `dac_changed`, `would_interrupt`); `testNativeOrDoPIsNeverSentWithoutTheSheet`; `testTheStopStaysUntilObserved`; `testAnUncoveredRateAsksForTheSheet`; `testAWarningShowsItsSentence` | full suite, mirror |
| I5 | iOS | The optional chime and its answer (§6.1); mirror of B12 | B12, I4 | `testAChimeIsOfferedOnlyForATupleTheBridgePlays`, `testNoAnswerForAStoppedCheck` | full suite, mirror; the owner's hardware run |

### Order and parallelism

**The merge order:** B1, B2, B3, B4, B5, B6, B7, B10, B11, B8 with I1, B9 with I4, B12 with I5,
B13. I2 and I3 follow I1. B10 can be written from day one, but merges before B11.

**Tracks:**

- **The cutover:** B1, then B2, B3, B5 and B6, with B4 beside them; B7 joins them.
- **Conversion, then the protocol:** B10 → B11 → B8 → B9 (Argon). `converted` is published only
  once the player converts, and `mode: convert` is writable only once `converted` is published. No
  build shows a rate the player refuses.
- **First use:** B7 records `observed`; B9 takes the acknowledgement; I4 ships the sheet and the
  Stop with the mode UI (Codex); B12 and I5 add the optional chime.
- **iOS:** I1 after B8; I2 and I3 after I1; I4 after B9; I5 after B12 and I4.

**The critical path** is B1, B2, B3, B5, B6, B7, B11, B8, B9, B12 and I5: eleven PRs in sequence.

**What changes for whom, and when:**

- Until B11, a rate the policy would convert is refused, as today. From B11, an unknown DAC's DSD
  plays converted for every controller, unless an explicit `off` was migrated.
- A known DAC converts until its first use is acknowledged, which needs B9 and I4.
- Migrated owner setups keep playing native and DoP throughout: through the legacy reader until B7,
  then from their migrated state.

**Decided** (the owner, 2026-11-04, taking Argon's recommendations):

1. Narrow the qualification guard as §8 proposes.
2. Cap conversion at 176.4 kHz (§4.3).
3. Unity gain for live conversion (§4.4).
4. First-use acknowledgement before native or DoP, even on a known DAC (§3.2).

**Owner work the plan depends on:** captures of real units for the iOS-profiled models, and their
analogue volume controls where there are any (§2.3); and the hardware runs of B11 and B12.

### Round 2: the reviews

| Finding | Change |
|---|---|
| Codex P1: B1 invalidates production approvals before migration | §3.5, §3.6, §8: the legacy reader, with version and identity as parameters; production switches to it in B1 itself, before any bump; B6 migrates through it by model; B7 cuts over |
| Codex P1: native `auto` ignores a listener's rejection | §1.2, §3.2, §6.4: one answer per tuple, the latest; a rejection outranks every positive input in every row and re-arms first use |
| Codex P1: the enabling UI comes before its safety flow | §6.1, §6.2, §11: B7 records `observed`, B9 takes the acknowledgement, I4 ships the sheet and the Stop; only the chime waits for I5 |
| Codex P1: the check bypasses acknowledgement | §6.2, §7.2: the effective policy admits the check |
| Codex P1: an empty control match reads as unity | §3.4: two topologies, failing closed; every level control counts whatever its name; a feature unit with no visible control refuses |
| Codex P2: explicit Off is lost | §2.5, §3.6: explicit `off` becomes `defaultMode: off`; a damaged state refuses DSD |
| Codex P2: revision invalidation contradicts `interrupt` | §3.5, §7.2: reconciliation per occurrence; one predicate for the reconciliation and `would_interrupt` |
| Codex P2: shared filter state needs new Cut semantics | §4.7: the join window held back, recomputed from a snapshot, cut at the window with the sink's marker; three-moment tests |
| Codex P2: conversion claims ignore fallback targets | §4.8, §7.1, §7.5: every claim is read from the plan |
| Argon 2: the chime's bit repetition | §6.2: each rate modulated natively; `0x69` pads; no repetition |
| Argon 3: post-decoder analogue controls | §2.1, §3.4: `analogVolumeControls`, per profile, with provenance |
| Argon 4: converted DSD64 to DSD128 | §4.7: an abutting join in the same sink |
| Argon 5: health flags before their routes | §7.1, §11: `bridgeDSDChoice` in B9, `bridgeDSDCheck` in B12 |
| Argon 6: the PR order | §11: B10 → B11 → B8 → B9 |

### Round 3: the last review points

| Finding | Change |
|---|---|
| Codex P1: migration loses the approval's rate limits | §2.5, §3.2, §3.6, B6, B7: a migrated acknowledgement lists its record's rates (`bitRates`), and admission is per rate; another rate stays `first_use_pending` until the user acknowledges or chooses the mode again |
| Gemini: `dacKey` with an absent interface | §2.2: the canonical form, five LF-joined lines, `=` before a present optional field and `-` for an absent one; test vectors in B2 |
| Gemini: DoP and a level control with no dB scale | §2.1, §3.4: fail-closed by default; a profile's `unityAtRawMax` admits DoP only at the documented or measured raw maximum, on a listed firmware, with its evidence required |
| Gemini: migration failures only logged | §2.5, §3.6, §7.1, B8: `dsdPlayback.warnings` (`migration_record_refused` with its reason, `migration_record_missing`, `migrated_off`, `state_unreadable`), kept in the state |
| Gemini: a limiter or default attenuation | Declined, and answered on the thread: unity gain is the owner's decision, and saturations are counted and shown (§4.4). Nothing changed |
