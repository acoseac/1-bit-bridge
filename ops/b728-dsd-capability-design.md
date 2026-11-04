# B728: the bridge player's DSD by DAC capability and the user's choice

Design and PR plan, 2026-11-04. This round writes no code.

**Status.** For review. The decision this implements is binding: the owner decided it on
2026-11-04, after the Argon and Codex consult, and the reviewers' notes on the brief are folded in
(Codex 1 to 3 and Argon 2 to 5). Every current behaviour this design changes is cited as
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
| `listener_confirmed` | A person answered "clean" to the optional chime (§6) for this transport, rate, identity and firmware | A listener heard the chime as a chime, on a date | Bit-exactness, or anything about other rates, modes or firmware |

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
- **Listener rejection.** A `noise` or `nothing` answer (§6) takes that transport and rate out of
  `auto` for the identity until the user chooses that mode again.
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
        "source": { "kind": "ios-registry", "ref": "DACProfile.swift fiio.qx13 at iOS <commit>" }
      },
      "timing": { "dsdPrefillMs": 400, "pcmEntryMs": 800 },
      "caveats": [
        { "code": "dop_noise_in_hidden_mode", "transport": "dop",
          "note": "With PEQ on, this DAC plays DoP as noise, and the bridge cannot see the mode: turn PEQ off, or choose Convert to PCM." }
      ],
      "provenance": [ "dop: heard clean at DSD64/128/256 with PEQ off on iOS; noise with PEQ on (iOS 2.0.1 report)", "identity: dac inspect capture <date>" ]
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
  - `refusedBitRates`: rates refused on a device, or heard as silence.

  Its source is the iOS registry at a named commit.
- **`timing.dsdPrefillMs`** is the DSD silence written before content in each hardware run. It is
  seeded from the registry's resync delays ((iOS) `DACProfile.swift:217-275`) and defaults to 400 ms,
  iOS's DSD resync default ((iOS) `DACProfile.swift:152-183`).
- **`caveats[].code`** is a closed list (§2.3). Its `note` is display text.
- **`provenance`** has one line per claim.

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
- **On the wire** a DAC is named by `dacKey`: `d1-` followed by the first 16 hex digits of the
  SHA-256 of the four key fields. It carries no serial or port.

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
  - provenance for every claim;
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
  "dacs": [
    {
      "key": { "vendorId": "<vid>", "productId": "<pid>", "interface": 1, "product": "<product>" },
      "choice": { "mode": "auto", "maxBitRate": 0, "revision": 4, "setAt": "2026-11-05T10:00:00Z", "setBy": "app" },
      "dopConfirmed": null,
      "firstUse": [ { "transport": "native", "bcdDevice": "<bcd>", "acknowledgedAt": "…", "observedAt": "…" } ],
      "evidence": [
        { "transport": "native", "format": "DSD_U32_BE", "bitRate": 2822400, "bcdDevice": "<bcd>",
          "driverAcceptedAt": "…", "kernel": "<release>", "streamingObservedAt": "…",
          "listenerConfirmedOn": "2026-11-05", "listenerRejectedOn": null }
      ]
    }
  ],
  "migratedFrom": null
}
```

- **Why the data directory and not `bridge.yaml`.** A paired app sets the choice, per DAC, as it
  does the queue the player keeps (the queue bullets in `.claude/rules/player.md`). The evidence
  has to sit beside the choice. One writer, serve's player, serializes the app's and the console's
  writes without adding a writer of `bridge.yaml` and B506's lock discipline
  (`.claude/rules/config-lifecycle.md`). And `bridge.yaml` stays the operator's file.
- **Durability.** A write is durable before it is answered. It goes through
  `atomicwrite.WriteBytesDurable`, the writer the player's ID reservations use; the data directory
  must already be on storage that outlives a reboot (the reservation bullet in
  `.claude/rules/player.md`). A `noise` answer lost to a crash would otherwise put DoP back.
- **A file that does not read** is moved aside with a timestamp and never deleted, as the queue
  file is. The player then runs on the defaults: unknown DACs convert, and known DACs convert until
  a client acknowledges first use again.
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
- **Acknowledged:** first use of the transport acknowledged under the current bcdDevice (§6).
- **Conversion offered:** the DAC takes a target of §4.3.

| Choice | Plays native at r when | Plays DoP at r when | Otherwise, at r |
|---|---|---|---|
| `auto`, known DAC | native offered; acknowledged | the profile heard DoP at r, or the user confirmed DoP; carrier offered; gain at unity; acknowledged; no listener rejection | converted |
| `auto`, unknown DAC | a listener confirmation for native at r under this bcdDevice; native offered; acknowledged | a listener confirmation for DoP at r; DoP confirmed; carrier offered; gain at unity; acknowledged | converted |
| `native` | native offered; r ≤ `maxBitRate` | never | refused: `native_not_offered`, `dsd_rate_above_choice` |
| `dop` | never | carrier offered; r ≤ `maxBitRate`; gain at unity; the profile heard or stated DoP at r, or DoP confirmed; no listener rejection | refused: `dop_not_offered`, `dop_gain_not_unity`, `dop_gain_unverifiable`, `dop_confirmation_required`, `dsd_rate_above_choice`, `listener_rejected` |
| `convert` | never | never | converted (refused as `conversion_not_offered` if the DAC takes no target) |
| `off` | never | never | refused: `dsd_off_by_choice` |

- **`auto` means no explicit choice.** It is the preselection the decision asks for. The app shows
  the mode it resolves to, and the user may keep it.
- **Native comes before DoP** under `auto` where both qualify at a rate, today's preference
  (`authorization.go:258-269`).
- **The cap under `auto`:** a `maxBitRate` caps native and DoP, and rates above it are converted
  (§3.3).
- **Explicit `native` or `dop`** carries its first-use acknowledgement in the same write (§7.2), so
  it never meets a pending first use. A bcdDevice change that lapses it refuses as
  `first_use_pending` until the app acknowledges again.
- **A known DAC with no DoP claim at r** treats DoP as an unknown DAC does: `auto` needs the user's
  confirmation, and an explicit `dop` write without `confirmDoP` is refused (§7.2).
- **The listener-confirmation clause** under `auto` on an unknown DAC is what keeps a migrated
  owner approval (§3.6), and a DAC whose chime the user passed, on what they were heard on.
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
- **So: detect, and refuse.** The backend reads the bound card's controls through `alsactl.Reader`
  (bounded and read-only; `internal/alsactl/control.go:58-113`):
  - before a DoP occurrence is admitted;
  - at the check after its open;
  - on the 100 ms monitor while it plays (today's tick, `authorization.go:457-474`).

  Every MIXER-interface control named `… Playback Volume` must read 0 dB on every channel by its dB
  TLV (`Control.TLV`, `internal/alsactl/control.go:26`). B5 parses `DB_SCALE`, `DB_MINMAX`
  (with its mute variant), `DB_LINEAR` and `DB_RANGE`.
  - A control that reads otherwise refuses DoP as `dop_gain_not_unity`.
  - One with no dB scale, or that cannot be read, refuses it as `dop_gain_unverifiable`.
  - A muted playback switch is silence, not corruption, and is allowed.
- **A change while DoP plays** revokes the occurrence, as a constraint change does today: an
  immediate stop, and that rate out of the epoch (`PROTOCOL.md:1020`).
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
supervisor's refresh (`cmd/bridge/serve_player.go:526-534`). The new "replaced" is a change of
the DSD state's revision, of the profile match or of the live capabilities, and it revokes as a
replaced record does now.

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

- `qualification.ReadApproval` and `ReadCandidate`;
- `NewQualificationBackend` and `NewTransitionQualificationBackend` (`authorization.go:509-547`);
- the approval contract (`ops/b470-native-qualification.md:248-295`).

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
- **They are migrated once,** at the first start of a B728 bridge whose block sets any of them,
  into the state of the DAC the binding resolves to. A migration waits for that DAC.

| Legacy | And | Migrates to |
|---|---|---|
| `dsdMode` unset or `off` | — | nothing: the defaults (§3.2) |
| `native` | a record that reads for this unit's model identity (the port no longer compared) | choice `native`; each approved native tuple `listener_confirmed` on the record's `QualifiedOn` (schema 2) or undated (schema 1); first use acknowledged |
| `dop` | `allowDoP: true` and such a record | choice `dop`; DoP confirmed; the DoP tuples confirmed; first use acknowledged |
| `dop` | `allowDoP: false` | nothing (today it refuses DoP as `dop_not_allowed`, `authorization.go:250-252`) |
| `auto` | such a record | choice `auto`; its tuples confirmed, so `auto` plays them even on an unknown DAC (§3.2); DoP confirmed only with `allowDoP: true` |
| any | no record, or one that does not read | nothing; one Warn naming the reason |

- **The record is read once,** by the harness's reader, and never again. The state records its
  digest (`migratedFrom`).
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
- **Start.** The delay lines start full of DSD silence, `0x69`
  (`internal/alsapcm/sink_dsd_writer.go:302-306`), never zero bits: all-zero bits are a full-scale
  negative offset (the DSD silence bullet in `.claude/rules/dlna-upnp.md`).
- **End.** Bits past `Frames`, such as a DSF's padding in its last byte
  (`internal/player/format/pcm.go:105-108`), read as silence. The group delay is flushed with
  silence, as frames of the occurrence.
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
  352.8 kHz costs about twice as much. Adding it later is one table row and a new CPU measurement,
  if the owner wants it (§11, owner decisions).

### 4.4 Gain, headroom and clipping

- **Unity gain.** Bit 1 is +1.0, bit 0 is −1.0, and PCM full scale is the one-bit amplitude:
  - the SACD reference, 50 % modulation, lands at −6.02 dBFS;
  - the Scarlet Book's +3 dB peak, 70.7 %, lands at −3 dBFS;
  - stage 3's overshoot on a full-scale step is under 1 dB (Gibbs).

  Legal content therefore peaks below about −2 dBFS, with no gain applied.
- **No +6 dB.** The offline chain adds it only after measuring the whole file's true peak
  (`dsd_render_chain.go:37-50`; `ClipGuardedGainDB`, `:171-178`), which a player that starts at once
  cannot do. Unity gain also keeps every track of a gapless album at one level.
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

- **Gapless.** A converted occurrence is a PCM transport: rate, channels, packing and valid bits
  (`.claude/rules/player.md:80`, `:135`).
  - Converted occurrences joined at one DSD rate and one target share one converter instance. The
    next track's bits enter the same delay lines, so the join has no gap and no restart transient,
    as the DSD stream would play natively.
  - A converted occurrence ahead of a PCM successor of the same transport flushes its group delay
    with silence, as frames of its own. The PCM frames then follow in the same sink.
  - Another transport means drain, close and reopen: an honest gap (`Transport.Join`).
- **The boundary** is the first output frame whose centre bit belongs to the next track. Position
  and Audible are exact to one output frame.
- **Stop and Cut** are the PCM sink's, unchanged: Drop is immediate, and Cut works at a content
  frame. The converter's lookahead, under 1 ms, lives in the producer, never in the sink.
- **Pause and release** are the PCM sink's: PAUSE, or zeros. After `pauseReleaseSeconds` the release
  keeps the unplayed frames in the store, and a resume writes them (the sink's pause bullet in
  `.claude/rules/player.md`). The converter takes no part in a resume.
- **Seek.** Seeking within converted DSD stays unsupported: `timeSeek.dsd` remains `unsupported`
  (`internal/player/engine/backend.go:85-90`). The DSD demuxer's byte seek
  (`internal/player/decode/decoder.go:45-47`) makes it possible later.

### 4.8 What Signal Path may claim

- **The observation** says `transport: converted`, with the source DSD rate, the target rate and
  bits, the filter, the gain (0 dB) and the clipped count (§7.1).
- **Details:** codec `DSD`, the target rate, 24 bits and `Lossless: false`, so OpenHome Info's
  Details say lossless 0 (the Time and Info bullet in `.claude/rules/openhome.md`).
- **The app** says "DSD converted to PCM by the bridge's player · DSD64 → 176.4 kHz / 24-bit", and
  never bit-exact. Its "Converted" status already exists ((iOS) `SignalPath.swift:479`).

### 4.9 Relation to `upscale`'s DSD render

**Shared with the offline render:**

- the target response (30 kHz pass / 40 kHz stop, at least 100 dB; `dsd_render_chain.go:119-125`);
- the 176.4 kHz / 24-bit faithful target (`dsd_render.go:20`, `:69-79`);
- DSD silence as `0x69`.

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

**Before the first DSD play** on a newly enabled native or DoP transport. That is either a write
that selects it, or `auto` resolving to it with `firstUse` pending.

- The app shows a sheet: turn the amplifier or the headphones down before the first DSD track; a
  large Stop stays on screen.
- DoP on an unknown DAC adds the confirmation (§7.4).
- The write then carries `firstUseAcknowledged` (§7.2).

**During that first play** (first use `acknowledged`, not yet `observed`), the bridge player's
now-playing view shows a large Stop.

**The optional check:**

1. "Play a test chime" sends POST check.
2. While the chime plays, a large Stop sends DELETE check.
3. When it completes, the app asks: "Did you hear a clean chime, first on the left, then on the
   right?" The answers are clean, noise or nothing, sent by POST answer.
4. Noise or nothing offers Convert to PCM as the next step.

### 6.2 The bridge's half

**The check.** One runs at a time.

- If something is playing, the request needs `interrupt: true`, or it gets 409 `would_interrupt`.
- The bridge stops the engine and waits for the release's acknowledgement.
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

It is modulated once, at DSD64, by B725's modulator, in a production package by then.
`internal/dsdtone` is first-order, with a −45 dBFS noise floor, and is for tests only
(`internal/dsdtone/dsdtone.go:1-14`). For DSD128 and DSD256 the DSD64 chime is repeated bit for bit:
a valid stream at that rate, which exercises the DAC's decoding there with DSD64's content. It is
held in memory (DSD256: about 1.4 MB per channel per second, 7 MB stereo for 2.4 s), and nothing is
embedded in the binary.

**`streaming_observed`** is recorded by the backend when an occurrence of a native or DoP tuple has
played 5 s of content without a fault (§1.1). First use then becomes `observed`.

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
| First use acknowledged | `firstUse[transport].acknowledgedAt`, with the bcdDevice |
| 5 s of content played without a fault | `evidence.streamingObservedAt`; first use `observedAt` |
| The chime completes and the answer is clean | `listenerConfirmedOn` (a date) for the transport, rate and bcdDevice |
| The chime completes and the answer is noise or nothing | `listenerRejectedOn`; that transport and rate leave `auto` (§3.2) until chosen again |
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
    "converted": [ { "bitRate": 11289600, "rateHz": 176400, "validBits": 24, "filter": "dsdconv-1" } ],
    "refused": [],
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
      { "mode": "convert", "offered": true, "rateHz": 176400, "validBits": 24 },
      { "mode": "off", "offered": true }
    ],
    "firstUse": [ { "transport": "native", "state": "observed" }, { "transport": "dop", "state": "pending" } ],
    "check": null
  },
  "playback": { "transport": "converted", "format": "S24_3LE", "rateHz": 176400, "validBits": 24, "logicalRate": 0,
    "converted": { "fromBitRate": 2822400, "filter": "dsdconv-1", "gainDB": 0, "clippedSamples": 0 } }
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

- **`dsdPlayback.converted`** (optional array): `{bitRate, rateHz, validBits, filter}` for each
  converted rate. An older bridge omits it; it is `[]` when no rate converts.
- **`dsdPlayback.refused`** (optional array): `{bitRate, reason}` for each rate from DSD64 to DSD256
  that plays neither way, with `reason` from §3.2's closed list.
- **`dsdDAC`** (optional object): the bound DAC's key; its profile (id, name, caveats); the choice
  and its revision; the options for each mode (offered, rates with evidence, `needsConfirmation`,
  `blocked`); first use per transport; and the running or last check. It is omitted while no DAC is
  available.
- **`playback`:** `transport` gains `converted`, with `format`, `rateHz` and `validBits` the PCM's
  and `logicalRate` 0. A new optional `converted` object gives `{fromBitRate, filter, gainDB,
  clippedSamples}`. A native or DoP occurrence gets an optional `evidence` member.
- **Health flags:** `bridgeDSDChoice` (the PUT) and `bridgeDSDCheck` (the check routes), each
  advertised while the player's gate holds and the routes are wired. A client keys on the flags,
  never on a version ("Don't label a spec section with a version", `.claude/rules/wire.md`).

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
   `firstUseAcknowledged` (409 `first_use_unacknowledged`).
8. A change that would stop what plays, because the occurrence's transport leaves the effective
   set, carries `interrupt` (409 `would_interrupt`, nothing changed). This is the console's rule and
   its word (the console bullet in `.claude/rules/player.md`).

Then the bridge:

1. writes the state durably (500 `persist_failed`, nothing changed);
2. probes the DAC if it is free;
3. recomputes and publishes the effective set (`policyGeneration` + 1);
4. revokes what left it;
5. answers 200 with `{dsdDAC, dsdPlayback}`, read from the published epoch.

That read-back is the effective choice, exactly as `GET` returns it afterwards.

**`POST /v1/outputs/{id}/dsd/check`.** Body: `{dacKey, transport, bitRate, interrupt}`. Answers 202
`{check}`. Refusals: 409 `dac_changed`, `would_interrupt`, `check_running`, or `check_unavailable`
(the tuple is not offered now, its gain is not at unity, or the device is busy).

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

### 7.5 iOS: Signal Path labels

- **The qualification line is replaced.** "Owner listening qualified" and "Owner listening
  qualification: <model> (firmware <bcd>), <transport> <rate>, <date>." ((iOS)
  `BridgeOutputPlayerSignalInfo.swift:78-87`) become an evidence line: "DAC evidence: <rung> · <model>
  (firmware <bcd>)".
- **What stays:** the transport lines ("Opened DAC transport: native DSD …", "… DoP …"; `:70-77`) and
  the footer (`:89`).
- **Converted** gets "DSD converted to PCM by the bridge's player", with "DSD64 → PCM 176.4 kHz /
  24-bit · 30 kHz low-pass · unity gain", and "<n> samples clipped" when any clipped. It stays
  distinct from the rendition's "DSD converted to PCM on the bridge" and the phone's "on this
  iPhone" ((iOS) `SignalPath.swift:556-558`).
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
  positive `rateHz` and `validBits`, from the same grant's set. `converted` lives inside
  `dsdPlayback`, so I3b's epoch and freshness rules cover it unchanged.
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

**After B728** the version binds only the harness's own reports and records. B1 narrows the guard to
the code that changes DSD bytes or the harness's evidence:

- **Paths:** `internal/player/format/dsd_pack.go`, `internal/player/format/dop_pack.go`,
  `internal/player/playback/native.go`, `internal/player/playback/edges.go`,
  `internal/alsa/qualification.go`, and `internal/alsactl/control.go`, `control_linux.go` and
  `uapi.go`.
- **Prefixes:** `internal/alsapcm/`, `internal/player/qualification/` and `cmd/bridge/output_dsdtest`.
- **Dropped:** everything else in today's two lists.

**What this plan touches, after B1:**

| PR | Guarded paths it touches | Version |
|---|---|---|
| B1 | `internal/player/qualification/version_gate.go` | 36 |
| B4, the probe | `internal/alsapcm/` | 37 |
| B7, the policy wired | `internal/player/playback/native.go` and `edges.go` (plan construction) | 38 |
| B12, the check | `native.go`, and `internal/alsapcm/` if the check needs a sink seam | 39 |
| every other PR | none | unchanged |

Two PRs that take the same number collide at merge, as they do today, and the later one raises it
at merge (`ops/b470-native-qualification.md:651-655`). B1 also rewrites the runbook's header: no
production path reads an approval record, and the version invalidates harness evidence alone.

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
   a drain, a close and a new sink, an honest gap its `Join` names; converted DSD occurrences joined
   at one DSD rate share one converter, which carries its delay lines across each join, and a
   converted occurrence ahead of anything else flushes its group delay with DSD silence as frames of
   its own** (B728)." It adds: "a converted occurrence's transport is the PCM it becomes." The Don't
   becomes: "**Don't write anything between two occurrences of one transport, report a join gapless
   across transports, restart the converter inside a run of joined converted occurrences, or count
   its flush as the successor's.**" The sink's own gapless bullet (`:80`) is unchanged: the sink is.
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
     converts the rest; an explicit `native` or `dop` refuses what it cannot play; DoP needs its
     carrier with at least 24 significant bits, every playback volume control of the card at 0 dB,
     and its first use acknowledged. A native↔DoP edge needs no report in production; the
     transition scenario is the harness's. **Don't play DoP on a DAC with no DoP claim without the
     user's confirmation, with a volume control off 0 dB or unreadable, or before its first use was
     acknowledged; fall back to another transport at run time; publish anything but the effective
     set; join same-carrier PCM and DoP; or put the USB port or serial on the wire.**"
10. **A new section in `.claude/rules/player.md`, "Player DSD by capability and choice (B728)".**
    Each bullet lands with its code:
    - B2: "**The DAC profile database matches by USB identity, never by name: vendor and product
      IDs, the interface, and the product string, which a shared reference ID needs; bcdDevice only
      narrows.** **Don't match a profile by its name, add a claim without its provenance, let two
      profiles match one identity, or import from the iOS registry anything but its DoP evidence
      and caveats.**"
    - B3: "**The player's DSD state (`player-dsd.json`) keys each DAC by its model identity, and a
      write is durable before it is answered.** **Don't store a serial, a port, a card number or a
      devnum, answer a write before it is durable, or delete a state file that does not read: move
      it aside.**"
    - B4: "**The silent probe opens the bound node only while it is free and no session holds it,
      refines each candidate, and closes.** **Don't send HW_PARAMS, SW_PARAMS, PREPARE, a write or
      a control write from it, wait on a busy node, or probe on a timer.**"
    - B5: "**The evidence ladder is reported, driver accepted, streaming observed, listener
      confirmed, and no word claims that a DAC decodes DSD.** **Don't word a rung as verified,
      decoded, bit-exact or a lock, count a rung across a bcdDevice change, or publish DoP's driver
      acceptance as more than its carrier's.**"
    - B7: "**DoP plays only while every MIXER-interface playback volume control of the bound card
      reads 0 dB by its dB scale.** **Don't write a control, take a control with no dB scale as
      unity, or keep DoP playing once a control leaves 0 dB.**"
    - B10: "**`dsdconv` is linear phase and unity gain, 30 kHz pass / 40 kHz stop at 100 dB or
      more, starts and ends on DSD silence, saturates and counts, and rounds 24-bit output with no
      added dither (TPDF at 16 bits).** **Don't add gain the stream was not measured for, limit or
      compress, prime a delay line with zero bits, or read past `Frames`.**"
    - B12: "**The check plays its chime through the transport under test inside the engine's
      maintenance hold, for at most 10 s, and every Stop ends it.** **Don't touch the queue,
      autoplay after it, or record listener confirmed for a check that did not play to its end.**"
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
14. **`ops/b470-native-qualification.md:1-10`** (B1): "Since B728 no production path reads an
    approval record or a candidate: the player plays DSD by its effective policy
    (`ops/b728-dsd-capability-design.md`). This runbook is the engineering harness's. Its evidence
    binds `qualificationVersion`, whose guard covers the DSD byte path and the harness alone."

### 9.2 iOS

1. **(iOS) `.claude/rules/bridge-player.md:64`**, I3a and R12 (I2). Step 8 becomes: "a tuple of that
   `bitRate` with `channels == 2` whose B574 eligibility is native or DoP, or a
   `dsdPlayback.converted` entry of that `bitRate` with positive `rateHz` and `validBits` (B728)
   (`.dsdRateNotPlayable(label)`, or `.dsdUnplayable` with neither at any rate)". The Don't
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
4. **New** (I4): "**The bridge player's DSD choice is written only by `PUT /v1/outputs/{id}/dsd`,
   with the DAC key and revision just read, after the first-use sheet for native or DoP, and after
   the confirmation for DoP on a DAC the bridge's database does not know.** **Don't send native or
   DoP without `firstUseAcknowledged`, DoP without `confirmDoP` where the bridge asks for it, or a
   choice for a DAC the app did not just read, and don't decide eligibility from the choice.**"
5. **New** (I5): "**The first DSD play on a newly enabled native or DoP mode keeps a large Stop on
   screen until the bridge reports first use observed, and a chime's answer is sent only for a check
   that completed.** **Don't hide the Stop before `observed`, answer for a stopped check, or present
   a listener confirmation as a verification.**"
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
| iOS evidence used on Linux | Kernel quirks, implicit feedback or class compliance differ | Only DoP evidence is imported (DoP decoding is the DAC's, whatever the host); provenance says "heard on iOS"; first use | A DAC that behaves differently under Linux |
| False refusals from the mixer check | A feature unit placed after DoP detection, or a missing dB TLV, refuses DoP needlessly | The reason tells the user to set 0 dB, or to choose native or convert | Inconvenience only |
| An older app with a B728 bridge | — | Native and DoP start only after a new client acknowledges first use; converted rates are invisible to the old grant; no qualification line is shown | none |
| The state file lost or damaged | Choices or answers lost | Durable writes; a damaged file is moved aside; the defaults are the conservative ones | Re-acknowledging first use |
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
| B1 | bridge | The qualification guard narrows to the DSD byte path and the harness (§8); version 36; the runbook's header (§9.1, item 14) | none | `TestQualificationVersionGate` gains two rows: a change under `internal/config/` passes without a bump (red before), and one to `internal/alsapcm/sink_dsd_writer.go` is still refused without one | player |
| B2 | bridge | `internal/player/dacprofile`: schema, embedded JSON, matcher, lint; the kernel-quirk seed; iOS DoP evidence for captured models; `dac inspect --json` gains `manufacturer`, `product` and `bcdDevice`; its bullet in the new section (§9.1, item 10) | none | `TestAProfileMatchesByUSBIdentityNeverByName`, `TestASharedReferenceIDNeedsItsProductString`, `TestNoTwoProfilesMatchOneIdentity`, `TestEveryClaimNamesItsProvenance`, `TestTheQX13CarriesItsHiddenModeCaveat`, `TestDACInspectPrintsTheModelIdentity` | standard |
| B3 | bridge | The player's DSD state store (§2.5) and its migration reader: pure, not wired; its bullet in the new section | B2 | `TestAWriteIsDurableBeforeItReturns`, `TestAStateFileThatDoesNotReadIsMovedAside`, `TestNoDSDStateHoldsASerialOrAPort`, `TestTheMigrationTable` (every row of §3.6), `TestABackupCarriesTheChoicesAndRestoreDropsThisBoxsEvidence` | standard |
| B4 | bridge | The silent probe (`alsapcm.Probe`) and its cache (§5); version 37; its bullet in the new section | B1 | `TestTheProbeRefinesAndNeverCommits` (the fake kernel records no HW_PARAMS, SW_PARAMS, PREPARE or WRITEI, one open and one close), `TestTheProbeLeavesABusyNodeAlone`, `TestTheProbeReadsTheDoPCarriersSignificantBits`, `TestNoProbeWhileASessionHoldsTheDevice` | player |
| B5 | bridge | `policy.Effective` and the DoP gain check (§3.2 to §3.4): pure, not wired; the evidence-ladder bullet | B2, B3 | `TestTheEffectivePolicyTable` (every row of §3.2), `TestAutoConvertsAboveTheCapAndAnExplicitChoiceRefuses`, `TestDoPNeedsEveryPlaybackVolumeAtZeroDB` (each TLV kind, no TLV, unreadable), `TestNoDoPOnAnUnknownDACWithoutConfirmation`, `TestAutoOnAnUnknownDACPlaysOnlyWhatAListenerConfirmed` | standard |
| B6 | bridge | Migration at serve's player start; the legacy keys still taken, each warned once; config words (§3.6); the rule at `player.md:86` | B3 | `TestLegacyDSDKeysStillLoadAndAreWarnedOnce`, `TestAnApprovedRecordMigratesToListenerConfirmed`, `TestTheMigrationRunsOnceAndWaitsForTheDAC`, `TestAPlayerBlockWithLegacyKeysNeverTurnsThePlayerOff` | standard |
| B7 | bridge | The effective policy replaces `readPermission` in serve's backend and in `output play`; the approval readers stay the harness's; until B11 a converted rate refuses as `dsd_not_in_this_release`; version 38; the rules at `player.md:133`, `:187`, `:189` and `:197`, and the DoP-gain bullet | B4, B5, B6 | `TestServeReadsNoApprovalRecord`, `TestSelectionAndPublicationReadOneSet` (kept from 2d.2), `TestTheOpenedNodeMustBeTheIdentityThePolicyWasComputedFor`, `TestAStateChangeRevokesWhatLeftTheSet`, `TestNoTransportFallsBackAtRunTime`, `TestAMigratedOwnerSetupPlaysAsBefore` | player |
| B8 | bridge | The read side (§7.1): `dsdDAC`, `evidence`, `converted` (empty until B11), `refused`, the `converted` playback token, the health flags; PROTOCOL.md; fixtures; the `wire.md` DSD bullet | B7 | `TestTheBridgeElementCarriesTheDACChoice`, `TestDSDOutputProtocolMembers` and `TestDSDOutputProtocolTokens` (new rows), `TestNoDACKeyOrModelCarriesASerialOrAPort`, the fixture exporter | standard, mirror (I1) |
| B9 | bridge | `PUT /v1/outputs/{id}/dsd` (§7.2): CAS, key binding, confirmation, acknowledgement, `would_interrupt`, durable write, read-back; the `wire.md` bullet completed | B8 | `TestTheChoiceIsDurableBeforeItIsAnswered`, `TestAStaleRevisionChangesNothing`, `TestAChoiceForAnotherDACIsRefused`, `TestDoPOnAnUnknownDACNeedsConfirmation`, `TestNativeOrDoPNeedsFirstUseAcknowledged`, `TestAChoiceThatWouldStopPlaybackNeedsTheGoAhead`, `TestTheAnswerIsTheEffectiveChoice`; the route classification and error-table guards | standard, mirror (I4) |
| B10 | bridge | `internal/player/dsdconv` (§4.2 to §4.5), not wired; the rules at `player.md:27` and `:36`, and the converter's bullet | none | `TestTheResponseMeetsItsMask` (from the coefficients), `TestAliasingIsADifferential` (CIFF fixtures, `internal/transcode/testdata/gen/dsd_fixtures.py`, or B725's), `TestUnityGainLevels` (a −20 dBFS `dsdtone` sine gives −20 dBFS ± 0.05), `TestOvermodulationSaturatesAndCounts`, `TestDelayLinesStartOnSilence`, `TestPaddingPastFramesIsSilence`, `TestTwoTracksContinueWithoutATransient`, `TestTheOutputIsDeterministic`, `BenchmarkConvert{DSD64,DSD128,DSD256}` | standard |
| B11 | bridge | Conversion in the player: open, transport, joins, flush, observation and Details (§4.1, §4.7, §4.8); the rules at CLAUDE.md:91, `jobs.md:132`, `player.md:133-135` and `output.md` | B7, B10 | `TestAnUnknownDACPlaysDSDConverted`, `TestConvertedDSDReachesTheDACPackedByTheTable`, `TestConvertedTracksJoinInOneSinkWithoutAGap`, `TestAConvertedTrackFlushesBeforeAPCMSuccessor`, `TestStopDuringConversionIsBounded`, `TestTheObservationSaysConverted`, `TestAConvertedTrackIsNotLossless`, `TestNoConvertedStreamIsServed` | player, hardware (CPU and xruns on a Pi 4 and a Pi 5, §4.6; a listening pass) |
| B12 | bridge | First use, `streaming_observed`, the check and its answer (§6); an OpenHome Stop ends a check; PROTOCOL.md; version 39; the `openhome.md` rule and the check's bullet | B9, B11, B725's modulator in a production package | `TestTheCheckPlaysOnlyItsTupleAndLeavesTheQueue`, `TestEveryStopEndsTheCheckWithinItsBound`, `TestTheCheckNeverAutoplays`, `TestOnlyACompleteCheckIsAnswered`, `TestANoiseAnswerTakesTheTransportOutOfAuto`, `TestStreamingObservedNeedsFiveSecondsWithoutAFault` | player, hardware (the owner hears the chime on a real DAC), mirror (I5) |
| B13 | bridge | The console's Player page shows and sets the per-DAC choice and evidence (`cmd/bridge/serve_player_console.go:517-559`, the wording at `:532`) | B9 | the console's view and write tests for each state | standard |
| I1 | iOS | Mirror of B8: PROTOCOL.md copied; DTOs for `dsdDAC`, `evidence`, `converted`, `refused` and the `converted` playback token; fixtures | B8 | decode tests: a malformed new member withholds only its own object (I1's rule); `converted` reads as its own token | full suite |
| I2 | iOS | R12 admits converted rates; the refusal copy (§7.6) | I1 | `testAConvertedRateIsEligibleUnderAFreshGrant`, `testAConvertedEntryFromAnotherEpochAdmitsNothing`, `testNoConvertedEntryKeepsTheRateRefused`, `testARefusedRateSaysItsReason` | full suite |
| I3 | iOS | Signal Path: the converted card and the evidence line (§7.5) | I1 | copy-table pins with no forbidden word for DSD on the bridge's player; `testAConvertedOccurrenceIsNeverBitExact` | full suite |
| I4 | iOS | The per-DAC mode UI (§7.4); mirror of B9 | B9, I1 | view-model tests for each state (known and unknown, offered and blocked, `stale`, `dac_changed`, `would_interrupt`) | full suite, mirror |
| I5 | iOS | The first-use sheet, the large Stop, the chime and its answer (§6.1); mirror of B12 | B12, I4 | flow state-machine tests: the Stop stays until `observed`; no answer for a stopped check | full suite, mirror; the owner's hardware run |

### Order and parallelism

**Tracks:**

- **Policy:** B2 → B3 → B5 → B6 → B7 → B8 → B9 → B13.
- **Probe:** B1 → B4, which joins at B7.
- **Conversion:** B10 from day one; B11 once B7 has merged.
- **First use:** B12, after B9 and B11.
- **iOS:** I1 after B8; then I2 and I3; I4 after B9; I5 after B12 and I4.

**The critical path** is B2, B3, B5, B6, B7, B8, B9, B12, I5: nine PRs in sequence. Merge B11 before
B9 where the two are ready together, so a `convert` choice plays on the day the PUT ships.

**Nothing changes for an unconfigured user until B11:** until then a rate the policy would convert
is refused as today. Migrated owner setups keep playing native and DoP from B7 on.

**Owner decisions to confirm before B1 merges:**

1. Narrowing the qualification guard (§8).
2. The 176.4 kHz cap on the conversion target (§4.3).
3. Unity gain for live conversion (§4.4).
4. That native and DoP play only after a client acknowledges first use, even on a known DAC (§3.2).

**Owner work the plan depends on:** captures of real units for the iOS-profiled models (§2.3), and
the hardware runs of B11 and B12.
