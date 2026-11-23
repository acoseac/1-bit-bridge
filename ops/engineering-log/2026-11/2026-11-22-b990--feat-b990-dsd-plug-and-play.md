## 2026-11-22 — B990: the player plays what the DAC supports, with no first use asked

Branch `feat/b990-dsd-plug-and-play`, from main `9f98e0c1`. Commits: `f3fe6938`, the tests first;
`1db09523`, the change with its tests, the catalog, the goldens, PROTOCOL.md, the design and the
rules; `19b1353b`, this record; and a fourth, the two guarded `internal/alsapcm` tests the gate's
first run found (below) and the qualification version's bump, 56 to 57, which they need. No
state, route or protocol version changes.

### The decision and the change

The owner's decision of 2026-11-22: "It should be plug and play: the user plays, and it plays
what it supports or converts to PCM what it doesn't." Until then a transport played only once its
first use was acknowledged, and no client could write one (the console's and the app's halves of
the choice had not shipped), so a production bridge bound to a known DAC whose kernel offered
`DSD_U32_BE` at every rate converted every rate.

`policy.decide` now asks no acknowledgement. Under `auto`, at a rate the cap admits: a DAC a
profile knows plays native where the kernel offers it and no listener rejected it, else DoP where
the profile heard it or the user confirmed it (carrier offered, gain at unity, no rejection), else
converts; a DAC no profile knows plays native the same way where it is vouched for (a listener's
latest native answer at the rate is `clean`, or its IDs are no generic board's), and DoP only on
a clean answer and the confirmation, as before. An explicit `native` or `dop` drops its
acknowledgement refusal; an explicit `native` on a generic board's IDs at a rate no clean answer
holds is refused `native_check_required`, a new word, after `native_not_offered` and
`listener_rejected` and before `dsd_rate_above_choice` (no existing word fits: `native_not_offered`
would contradict the read side's offer). `first_use_pending` and `first_use_unacknowledged` stay
in the code and in PROTOCOL.md for older clients; this bridge produces neither. `policy.Choose`
takes an explicit choice with no `firstUseAcknowledged`, which, carried, still records an
acknowledgement (`dsdDAC.firstUse`, informational). Production: 68 lines added and 44 removed in
Go, comments included, beside the catalog's data.

An empty state's default is `auto` (`dsdstate.Defaults`, `internal/player/dsdstate/state.go:340`),
and neither the write nor the publisher gates a mode or a rate option on first use
(`internal/player/playback/dsd_view.go`'s `viewOf`, `:200-254` at the base).

### The generic boards

`reading.genericBridge` is the catalog's own list, `profiles.json`'s `sharedReferenceIds`, matched
by the unit key's vendor and product IDs (`dacprofile.Database.SharedReference`), and asked only
where no profile matched: a product-qualified profile on a shared ID, which the lint requires
there, and a wildcard profile on an ID the list does not hold both play as known. The list:

- `20b1:0010`, the XMOS reference ID an iFi unit carries; `16d0:071a`, the Amanero Combo384 board;
  `0d8c:0316`, a C-Media ID the kernel quirks under one maker's name while another maker's firmware
  ships it; `20a0:4143`, the WaveIO board: each the catalog's already, its source in its `why`.
- `16d0:0a23`, added: the Amanero Combo384 board on its newer firmware. Linux kernel commit
  `ed993c6fdfa7` (2017-08-18) adds DSD support for "new Amanero Combo384 firmware version with a
  new PID"; `sound/usb/quirks.c` at v6.12 lists the ID under its Amanero Combo384 heading. Its
  profile, `kernel.16d0-0a23`, a wildcard seeded from that quirk (no product string, `ios: null`),
  named no DAC and is removed. The iOS export does not move:
  `scripts/dac-profiles/ios-export/DACCatalog.ios.json`, 24 rows, SHA-256
  `c3b0446cfc7a784706f02e6d14853cb240128b51d859a6fe508e5277e5882546`, the same as a fresh export.
- Left out, no source read here: `20b1:000a`, `20b1:3008`, `20b1:2009`, `20b1:0008`, `262a:9023`,
  `262a:1001`, `2fc6:f00a`. Neither `quirks.c` nor `format.c` at v6.12 names them; XMOS appears
  there only vendor-wide, `VENDOR_FLG(0x20b1, QUIRK_FLAG_DSD_RAW)`.

### The two rules this round chose

**The retry.** With no acknowledgement to re-arm, a listener's rejection goes only with its
transport's retry: a write of an explicit `native` or `dop` that makes that transport the DAC's
mode, the DAC's own choice before it another mode or none. In the same `Store.Update` it clears
that transport's latest answers that are not `clean`, under the unit's firmware, at the rates its
`maxBitRate` admits (every rate for 0), and keeps the rejections above its cap, the transport's
clean answers, the other transport's answers and those under another firmware. A write that
leaves the mode as it was clears none: a cap-only write in the same explicit mode, one that
carries `firstUseAcknowledged`, and `auto`, `convert` or `off` over themselves or no choice; nor
does a write on a unit whose firmware does not read. `TestARetryClearsTheRejectionsAtTheRatesItsCapAdmits`
and `TestAWriteInTheSameModeKeepsEveryRejection` hold the two apart; the route's
`TestAnExplicitChoiceWithNoAcknowledgementIsWrittenThroughTheRoute` sends real PUTs with the
field absent.

**No list, fail closed.** Where no profile database reads (`Inputs.Profiles` nil), every unit
counts as a generic board's: the bridge cannot tell a board from a DAC, so native waits for a
listener's clean answer at the rate under `auto`, and an explicit `native` is refused
`native_check_required` until one.

### Red first

The final tests against `9f98e0c1`'s production code (its `policy.go`, `choice.go`, `match.go`,
`profiles.json` and `dsd_choice.go` given by `go test -overlay`, with a names-only shim for the
new reason's constant; each run `-count=1 -v`). Each failed on an outcome or a refusal, never a
build or a panic:

| Test | Red at the base |
|---|---|
| `TestAKnownDACPlaysNativeWithNoAcknowledgement` | auto on the Mytek at DSD64: converted, want native |
| `TestAKnownDACPlaysDoPWithNoAcknowledgement` | auto on a row that heard DoP: converted, want dop |
| `TestAnUnknownDACPlaysNativeUnlessItsIDsAreAGenericBoards` | auto on an unknown DAC: converted, want native (26 messages) |
| `TestAListenersCleanAnswerKeepsNativeOnAGenericBoard` | migrated native past its record's rate: `first_use_pending`, want `native_check_required` |
| `TestAnExplicitChoicePlaysWithNoAcknowledgement` | explicit native on the Mytek: `first_use_pending`, want native |
| `TestARejectionAndTheGainCheckStillRefuse` | auto at the rates no listener rejected: converted, want native |
| `TestAnExplicitChoiceIsWrittenWithoutAnAcknowledgement` | the write refused `first_use_unacknowledged` |
| `TestARetryClearsTheRejectionsAtTheRatesItsCapAdmits` | the write refused `first_use_unacknowledged` |
| `TestAWriteInTheSameModeKeepsEveryRejection` | the write refused `first_use_unacknowledged` |
| `TestAnExplicitNativeOnAGenericBoardWaitsForACleanAnswer` | explicit native on the board, acknowledged, accepted and observed: native, want `native_check_required` |
| `TestASharedReferenceIDNeedsItsProductString` (dacprofile) | the database does not list 16d0:0a23 as a shared reference ID |
| `TestAnExplicitChoiceWithNoAcknowledgementIsWrittenThroughTheRoute` (cmd/bridge) | the PUT answered 409 `first_use_unacknowledged` |

### Controls

Predicted in writing before any run; each a mutated copy of `policy.go` given through `-overlay`
on `1db09523`, the tracked file never written, `git status` clean after; the policy and playback
packages, `-count=1 -v`.

- C1, `n.Acknowledged` back in `auto`'s known branch: red as predicted,
  `TestAKnownDACPlaysNativeWithNoAcknowledgement` ("auto on the Mytek at 2822400: converted, want
  native"), with `TestAMigratedAcknowledgementGatesNoRate`, `TestARejectionAndTheGainCheckStillRefuse`,
  `TestAnUnknownDACPlaysNativeUnlessItsIDsAreAGenericBoards` (its row and wildcard parts) and
  playback's `TestANativeRateIsUnaffectedByAnUnverifiableDoPGain`.
- C2, `genericBridge` dropped from `auto`'s unknown branch: red as predicted,
  `TestAnUnknownDACPlaysNativeUnlessItsIDsAreAGenericBoards` ("auto on the generic board
  0d8c:0316 at 2822400: native, want converted"), with 13 more failing names in policy, subtests
  counted (among them the table's unknown-DAC row, the listener's clean answer's auto half, the
  explicit native's auto half and the schema-1 cases), and 6 in playback.
- C3 (the explicit native's guard removed): red as predicted,
  `TestAnExplicitNativeOnAGenericBoardWaitsForACleanAnswer` ("native on the board, acknowledged,
  accepted and observed at 2822400: native, want refused native_check_required"), with
  `TestTheRefusalsComeInTheirOrder`, `TestTheEffectivePolicyTable/native`,
  `TestAListenersCleanAnswerKeepsNativeOnAGenericBoard/native` and playback's
  `TestAMigratedChoicePlaysWhatTheTablePlays`.

### What moved

- The read side's goldens (`cmd/bridge/testdata/dsd-outputs`, written by
  `BRIDGE_WRITE_DSD_GOLDENS=1`): `dop_pending.json` (DoP plays at DSD64 and DSD128), and, for
  conversion's coverage, `converted_playing.json` (a `convert` state) and
  `unknown_converted.json` (a DAC that offers no native format); `unknown_native.json` is new;
  `choice_written.json` and `choice_stale.json` move `policyGeneration` from 2 to 3.
- The iOS fixtures, exported at the base and at `1db09523` (manifest, API, cmd/bridge, one at a
  time): `dsd_dac_outputs.json` and `dsd_choice_answers.json` move, and `fixtures-info.json` with
  them; `dsd_outputs.json` is the same, and `dsd_full.json` and `dsd_delta.json` differ only by
  clock times.
- PROTOCOL.md: `firstUse` informational, `native_check_required` in the refused reasons' row and
  prose, `first_use_pending` an earlier bridge's (out of the row, which `TestDSDOutputProtocolTokens`
  holds to the words the code emits), step 16 of the write's checks refusing none, the retry, and
  the client rules. SHA-256 at `1db09523`:
  `2f9623cb17f00123896c2d24265edfadcdb3ee0be96ed5f93f3ac3199bfaedb1`.
- The design's §3.2 table and every section that asked for the acknowledgement, amended in
  place; `player.md`'s DoP, state and admission entries and a new B990 entry, each long form in
  `ops/History-rules-player.md`; two long entries moved out for the file's budget
  (`scripts/doc-split/move_entry.py`, the pause entry with its own pointer and the player-off
  warning's with a pointer that keeps its rule on `forgetPlayerOffReason`), the moves proved
  alone on a copy of `f3fe6938` by `verify_moves.py` (2 blocks, 3,020 bytes).

### The gate's first run, and the qualification version

`make fmt vet test build-all` on `19b1353b` failed one package, `internal/alsapcm`, whose
external tests drive the player's backend over the real sinks and the fake kernel: two of them
asserted the acknowledgement's gate. The runs before it covered the policy, the catalog, the
backend, the API and cmd/bridge's player tests, and not these.

- `TestAnUnknownDACPlaysDSDConverted`'s case "a known DAC whose first use is not acknowledged"
  wanted conversion, and the DAC played native (`DSD_U32_BE`). It is now "a known DAC with no
  acknowledgement plays native": one `DSD_U32_BE` open, no PCM open, no skip or fault. The
  test's other cases convert still, their rig (`newConvRig`) having no profile database, so every
  unit is a generic board's; its comment and the test's docblock now say so.
- `TestAPlayedOutDoPOccurrenceStopsNothingAfterIt` set DoP at DSD64 by a migrated record that
  approved native at DSD128 alone, and native now plays at every rate offered. Its state adds a
  listener's later noise for native at DSD64, so DSD64 plays DoP as before, and the set once the
  volume leaves 0 dB is native at DSD128 and DSD256 with no DoP (it was DSD128 alone). The opens
  stay a's DoP `S32_LE`, then b's native `DSD_U32_BE`.

Both files are under the guard's `internal/alsapcm/` prefix, test files included, so
`scripts/qualification-version` asks for a bump: 57, the runbook's five pins with it
(`TestQualificationRunbookVersion`) and a paragraph of its own, as B972's test-only change took
56.

### Runs

Before the gate: `internal/player/policy` under `-race -count=20`, 1,340 passes and nothing
else, 38.1 s. The gate on `19b1353b`, in one slot session: fmt (no change) 0.7 s, vet 17.0 s,
test 2,361.5 s (91 packages ok, `internal/alsapcm` failed as above), build-all 145.7 s, vet for
darwin 6.9 s, windows 6.6 s, linux/arm 25.3 s and linux/386 25.5 s, both native-integration
overlays (51.0 s and 6.1 s, every named test passed), docs-check 0.8 s. The gate's run on the
fourth commit is in the report.
