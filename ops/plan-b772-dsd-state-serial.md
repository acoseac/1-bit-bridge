# B772: the player's DSD state stores no DAC serial, without breaking its safety

Design, round 1 (2026-11-13), docs only; backlog B772, target 0.2.4. Read at bridge main
`4678b983`, where every `file:line` below points; `plan-b728` is B728's design,
`ops/plan-b728-dsd-capability-design.md`. Section 2 answers the brief's points 1 to 6 and its
review's 7 to 12 (Codex, Carbon) under their own numbers. A DAC's *model key* is
`dsdstate.KeyOf`'s (`record.go:51-58`), and a *record* is the state's entry for a DAC
(`dsdstate.DAC`, `state.go:127-143`) with the warnings it owns.

## 1. The hazard

B3's rule (`internal/player/dsdstate/doc.go:13-15`): "A DAC is keyed by its model, dacprofile.Key
(…), never by a serial, a port, a card number or a device number, none of which the file holds."
The key's product string is the one the sysfs reader returns (`KeyOf`, `record.go:51-58`, over
`alsa.UnitModel.Product` or `QualificationIdentity.Product`), and the file writes it as it is:
`dacs[].key.product` and `warnings[].dac.product` (`keyJSON`, `state.go:409-414`, filled by
`keyOf`, `:432-442`). A DAC whose USB product string holds its own serial puts the serial on
disk. Such strings are reported for RME's class-compliant units, the serial in parentheses
("ADI-2 DAC (52813721)" below, the number invented); not verified on hardware here.

From the file the serial reaches every backup (a snapshot copies the file's bytes,
`internal/backup/backup.go:328-338`) and every restore of one (`ForRestore` keeps every key,
`restore.go:32-52`), and every `.bad-*` copy a damaged file leaves (`store.go:205-252`), which
nothing deletes. It reaches the journal too: the migration's log lines name the DAC by
`dacLabel` (`cmd/bridge/serve_player_dsd.go:465-471`), which prints the key's product string,
at `:309`, `:320`, `:330` and `:435`, under a doc that says it holds no serial. A unit bound by
serial has that one serial in `bridge.yaml` (0600) by its owner's choice; the state holds the
serial of every DAC ever bound whose product string carries one, under any binding, by nobody's.

Severity low-medium: a unit identifier, not a credential, which leaves the box only in a backup
or a file someone copies off it. Who holds such a file today: no release. `v0.2.3` is `0ddb8c72`
(`ops/deployment-runbook.md:634`), an ancestor of B3's merge `13a388be` (#1405), and the deploy
ledger's last rows are 0.2.3's. The files are those of development and test boxes running main
since B6 (#1413, 2026-11-06), `bridge output play`'s state directories, and their snapshots. If
B772 ships in 0.2.4 beside B728, no release ever writes a key holding a serial.

### What the probe showed

Two scratch tests, one in `dsdstate` and one in `playback`, run once at `4678b983`
(`go test -run TestB772Probe`, Go 1.27.3), then deleted; nothing of them is committed.

1. A migration whose bound unit's product string is "ADI-2 DAC (52813721)", serial 52813721,
   writes the serial twice: `dacs[0].key.product` and `warnings[0].dac.product` (the fallback's
   choice native and its `migration_record_missing`).
2. On that state `ChoiceFor` of the key with the serial taken out answers `auto`, own false (the
   default); the key as read answers `native`, own true.
3. `ForRestore` of that file keeps the serial.
4. `usbid.New("red").Redact("Model red")` is "Model [redacted]"; applied again,
   "Model [[redacted]acted]".
5. HEAD's `Open` over a file whose one change is `"schemaVersion":2` (a DAC with an explicit off
   at revision 3): Damaged, "schema 2, not 1", the bytes kept as they were in
   `player-dsd.json.bad-<timestamp>`, the name holding the refusing state (defaultMode off,
   `state_unreadable`, `Unreadable()` true), so every rate is refused `dsd_state_unreadable`.
6. A DAC entry holding 128 listener answers (two records of 64, merged) does not encode:
   "dacs[0].listener or evidence holds more than 64 entries".
7. `record_model` with two units of one model whose product strings hold their serials: the
   fallback (native, revision 1) and `migration_record_refused` (identity_mismatch) under the
   first unit's key, the record's rows (native, revision 1, first use at DSD64, a clean answer)
   under the second's. One model, two records.
8. The published key (`wireKey`, `dsd_view.go:281-302`) against the key whose product string has
   only the serial taken out (`Serial.Redact`, once, on the string as read); IDs 1234:5678,
   interface 1, port `platform-xhci-hcd.0/usb2-1:1.0`. Equal on every row whose product string
   holds no port form: no serial in it, no serial attribute, the serial appended, in
   parentheses, after "SN" with no separator, overlapping the marker ("red"), a one-byte serial
   inside the model's text ("Model 1 SN1"), two models whose serials are their model texts
   ("XMOS DAC 1.0"/"1.0" and "XMOS DAC 2.0"/"2.0": one key for both, on HEAD's wire already), the
   serial the whole product string, a serial with a leading space. Different only where the
   product string holds a port form (B8 round 2's fixture "Test DAC
   platform-xhci-hcd.0/usb2-1", which the wire takes out and the serial step does not), and for a
   serial that is not UTF-8 and matches inside a character ("Café DAC", serial `\xa9`: the serial
   step's result is not UTF-8 and `NewKey` refuses it; the wire falls back to the key with no
   product). Taking the serial out first and the port forms after, rather than HEAD's ports
   first, with the wire's own fallback, gives HEAD's key on every row but two contrived ones: a
   port form beside a serial overlapping it, or overlapping the marker.

The published keys of those rows, captured for PR 2's case C1: no serial in the product
`d1-3d2cca530809b663`; "Test DAC TESTDAC-0001" `d1-075821a048f647ee`; "ADI-2 DAC (52813721)"
`d1-f62c9e05eaabe91d`; "Model X SN12345" `d1-49e39856855dec73`; "Model red" `d1-f66b122bb9434599`;
"Model 1 SN1" `d1-a60a357ca515f26b`; both XMOS rows `d1-4f3291723bb5bc15`; "ABC123456789" with
that serial `d1-170b62451c783955`; "Model 12345" with " 12345" `d1-b6f689930c18fa4c`; the port
fixture `d1-075821a048f647ee`; the port fixture with the serial `d1-958ef4d6eeda2825`; "Café DAC"
with `\xa9` `d1-737ee4c0efa3ba42`.

## 2. Answers

### 1. No serial is available at migration time

`Open` (`store.go:140`) is given a directory and `ForRestore` (`restore.go:32`) bytes. A serial
exists only in a live reading: the sysfs `serial` attribute (`internal/alsa/sysfs.go:522-529`,
`alsa.USBIdentity.Serial`), and `bridge.yaml` for a unit bound by serial, that unit's alone. A
stored key keeps no mark of which of its bytes are the serial, so nothing at an open or a
restore can clean one without guessing, and nothing does.

**When a record is cleaned:** the next time the boot binding resolves to that very unit, on a
watcher snapshot that is fresh and says available: the unit whose live model key is the stored
key byte for byte, so the stored product string is its own, serial included. In serve that is
the player's start (a new subscription holds the feed's latest snapshot at once,
`internal/player/hotplug/feed.go:69-78`, which the watch loop takes at `serve_player.go:613-614`)
and every replug; `bridge output play` makes the same steps on its own state
(`output_play.go:553`, `:560`). Section 3.3 says how.

**A record that is never resolved** stays under its old key as it is (3.5): read only for a unit
whose live model key is that key, re-keyed when that unit binds, carried by backups and restores
as it is, never written, never deleted. Deleting it would drop an explicit off or a rejection for
a unit that may come back. Residual 1.

### 2. Merging records can make DSD less safe

Both halves are real (the probe's items 2 and 7). So nothing looks a DAC up under its stored key
alone: every read takes the state re-keyed in memory for the bound unit first (3.3), its old
record merged into its stored key's, and the set, the read side and a write's compare-and-set all
see that one record, before and after the write that makes it durable. The merge is 3.4's rule:
an explicit off and a listener's rejection always win; a rejection takes its transport's first
use away under its firmware, as the write that records one does (B728 §3.2, `plan-b728:442-448`);
the lower cap wins; a revision only rises; the positive inputs (a DoP confirmation, an
acknowledgement, a clean answer, evidence) are the model's, kept by firmware, as B728 keys each of
them by model and shares a model's choice between its units (§2.2, §7.2, `:1387-1390`).

### 3. `KeyOf` also feeds profile matching

Separated: a unit carries two keys (3.1), its model key, `KeyOf`'s as on HEAD, and its stored
key. Profile matching (`newReading`, `policy.go:345`; `readDSD`, `dsd_policy.go:490`), the set's
identity (`policy.go:308`, `:277`), the opened node (`dsd_policy.go:890-909`), reconciliation
(`reconcile.go:74`), the probe cache (`dsd_policy.go:434-436`) and the probe's check of its node
(`:1267-1268`) keep the model key; only the state's lookups and writes take the stored key (a new
`policy.Inputs.StateKey`, for `policy.go:320` and `:349`). No profile then matches otherwise.
Matching on the stored key was refused: `matches` compares the product string as read
(`match.go:66-72`), and a serial inside the model's text ("Model 1 SN1", "XMOS DAC 1.0") would
stop a product-string alternative matching its own unit. PR 2's C4 holds a shared reference ID
with its product string, a serial overlapping that text, and an ID-only profile to the same
matches before and after.

### 4. Legacy approvals compare byte for byte

Neither side is sanitized. `Bound.Identity` (`record.go:39-43`) stays the live identity with the
product string as read, and `ReadRecord` compares it with the record's through `sameModel`
unchanged (`record.go:67-71`, `:89-107`); it is transient and never stored. A record made on unit
R is taken for R alone, as on HEAD (the probe's item 7: identity_mismatch for the other unit).
What changes is where its rows land: under R's stored key, which every unit of R's model shares,
the model comparison's own intent ("the model alone (…) Not the port", `record.go:60-66`).

### 5. The wire already redacts

The stored key takes the wire's serial step and nothing else: `usbid.Serial.Redact`, applied once
to the product string as read. The port step (`unitText`, `PortForms`, `dsd_policy.go:387-415`)
stays the wire's alone (point 12). The wire does not change: `wireKey` and `unitText` stay byte
for byte, so by the probe (item 8) the published key equals the stored key's `DACKey` wherever the
product string holds no port form.

What clients see: the same `dsdDAC.dacKey` on every row. For one unit of a model whose product
string holds its serial, the same choice, first use and warnings (a rename). For two units of
such a model, HEAD publishes one dacKey with each unit's own record (two choices and two
revisions under one key, against `PROTOCOL.md:1074-1079`: "two units of one model share theirs,
and their choice"); B772 publishes the one merged record for both, its revision never below
either (3.4), so B9's PUT will answer a base read from the other unit's record 409 `stale`, and
the client reads again. Where a product string holds a port form, or the serial is not UTF-8,
the wire key stays coarser than the stored key: residual 8.

### 6. The strip rule's edges

The rule (3.2) is the wire's serial step, so its edges are the wire's, as the probe measured.

- **Empty:** `usbid.New` trims trailing NULs and white space and an empty result is no serial
  (`serial.go:46-52`); `Redact` then leaves the text as it is (`:67-72`). No serial attribute
  is the same.
- **Short:** taken out wherever it occurs, as on the wire: "Model 1 SN1", serial "1", is
  "Model [redacted] SN[redacted]".
- **A substring of the model's text:** taken out there too. Two units of one model then stay
  apart when one's serial lies in the model's text and the other's does not (no merge, HEAD's
  behaviour, safe). Two models whose serials are their model texts share a stored key ("XMOS
  DAC 1.0"/"1.0", "XMOS DAC 2.0"/"2.0"), as they share a dacKey on HEAD's wire: residual 6. A
  threshold (MPD's four bytes, a whole token) was refused (6.3).
- **Compound prefixes:** a port's spellings, one a prefix of another (`PortForms` takes the
  longest first, `dsd_policy.go:394-415`), are the wire's and never reach the stored key. A
  prefix printed before the serial ("SN", "S/N:", "(") stays outside the marker:
  "Model X SN[redacted]". A product string that prints the serial in another form than the
  attribute (another prefix or separators, another case, a part of it) keeps it: residual 5.
- **`dacprofile.NewKey`'s checks** (`key.go:42-72`): the result of `Redact` on a product string
  NewKey took is never empty, gains no white space at its ends (they are its own, or `[` and
  `]`), holds no NUL, CR or LF, and is UTF-8 whenever the serial is: a UTF-8 string occurs in
  UTF-8 text only between character boundaries. A serial that is not UTF-8 can match only inside
  a character (the probe's "Café DAC"), so it is never taken out, and the stored key is the
  model key there.
- **A leading space:** `usbid.New` trims only the tail, so a serial " 12345" takes its space
  with it ("Model[redacted]"), alike for every unit whose serial is so written.
- **The marker:** a serial overlapping "[redacted]" ("red") is taken out once, from the string as
  read; a stored key is never redacted again (point 12).

### 7. Re-keying is admitted on its own

`playerDSD.resolve` returns unless the legacy migration waits (`serve_player_dsd.go:380`). In PR 2
it asks the DAC's step on every snapshot that is valid, fresh, of the boot binding and available,
and `steps` (`:347-357`) answers yes where the migration's part waits or where the state holds an
entry or a warning under the unit's model key other than its stored key (`State.Rekey` says
whether it moved anything); `retry` (`:397-409`), which stops once nothing waits (`:401`), asks
the same. The step is one `Store.Update`: the re-key, then, where the part waits, the migration's
step on the re-keyed state, so the probe's item 7, made under PR 2, lands as one record. It runs
where today's DAC part runs:

- **serve's start-up bind:** the first snapshot the player's watch loop takes
  (`serve_player.go:613-614`), held at the subscription when the feed has published
  (`feed.go:74-76`), else the first publish;
- **hotplug:** every later snapshot of the binding, a replug among them;
- **`bridge output play`:** the same `resolve` and `retry` over its own state
  (`output_play.go:553`, `:560`);
- **several units of one model:** each bind moves that unit's own old record into the one stored
  key, merged by 3.4, in the order the units are bound, one write each.

A step that cannot be made (the unit not read, a write refused before it landed) is asked again at
the policy tick, bounded (`ask`, `:419-443`), and at the next snapshot; one that landed and is not
known to be durable ends the run's steps (`done`), and the next start reads the file. Meanwhile
no read takes the stored key alone: the set, the read side, the migration's step and every write
re-key in memory first (3.3). So the restrictions hold from the first read, through a failed
write and every retry, whether or not a write ever lands, and point 2's fall back to `auto`
cannot happen.

### 8. The conflict table is complete

Section 3.4 covers every pair of the five modes and an unset choice, under either defaultMode; the
cap; the revision with its stamp and author; DoP confirmations; first use (its firmware, its rate
coverage, the re-arm); listener answers; evidence; warnings; and the 64-entry bounds. Each row's
test asserts the effective outcome at DSD64, DSD128 and DSD256 (`policy.Effective` over the merged
state), not only the fields kept (PR 1). Reachability: today's writers write a choice only through
the migration, one mode for every unit it reaches (the marker's, `migrate.go:249-306`), so two old
records hold equal choices or one holds none; the other rows become reachable once B9, B12 or B13
write, or through a restore of such a state.

### 9. Every writer and the admission check, mapped

Section 3.7 maps each consumer. In short: the policy lookup and the read side take the stored key
on the re-keyed state; `Profiles.Match`, the set's identity, `checkDSDNode`'s opened identity,
reconciliation, the probe cache and the probe's check keep the model key; every write of a record
goes through `Store.UpdateDAC(keys, …)` (new), which re-keys before it writes. A delayed evidence
write (`recordDSD`, `dsd_policy.go:952-980`) captures the unit's two keys when it is asked and
re-keys when its goroutine runs, so it lands under the stored key whether it runs before or after
the bind's re-key, and no writer writes under a model key that holds a serial again. Nothing that
told two units of a model apart at play on HEAD stops doing so: their model keys differ wherever
their product strings do.

### 10. `warnings[].dac` is re-keyed too

The re-key (3.3) moves every warning owned by the unit's model key to its stored key, whether or
not a record sits under either, and 3.4 merges two warnings of one code under one owner (the
later `at`, with its reason). `ClearWarningsFor` (`state.go:336-340`), which B9's write calls for
the bound DAC, then reaches them, and the read side publishes them (`warningsOf`,
`dsd_view.go:263-271`). A `migration_record_refused` left beside rows the record gave another
unit of the model stays until a choice clears it: display only.

### 11. Schema and downgrade

**Schema 2**, written by every write of PR 2's build; schema 1 and 2 read alike (the same fields; a
schema-1 key is a model key that may hold a serial, which 3.3 re-keys). An older reader, any
build from B3 (#1405) to PR 2's parent and none of them a release, refuses schema 2 as it refuses
any schema but 1 (`state.go:484-486`). The probe's item 5 is that refusal: the bytes kept in
`player-dsd.json.bad-<timestamp>`, the refusing state written in their place, every rate refused
`dsd_state_unreadable` until a choice is written. Every restriction survives, as a refusal of
everything; PR 2 runs the case on `4678b983`'s own tree (C6), not a stand-in.

The privacy residual (residual 3): the `.bad-*` copy keeps PR 2's bytes for good, any old record
not yet re-keyed among them, and the box's choices stay there, where nothing reads them, so a box
upgraded again refuses DSD until a choice is written. A rollback to 0.2.3 reads neither schema
(B728's own residual: the legacy keys decide there). Schema 1 with stored keys was refused: an
older reader would look up its model key, miss the explicit off and take the default (point 11's
case; the probe's item 2 is that miss).

### 12. One redaction, applied once, never to the port

- **Raw input from clean input:** the one input the strip takes is a live reading (3.1:
  `alsa.USBIdentity` and the product string as `UnitModel` or `Qualification` read it, both
  trimmed alike, `unit_model.go:83`, `qualification.go:43`). No function takes a stored key and
  redacts it; the re-key compares the stored key made from the reading with the keys the state
  holds by their canonical forms (`Equal`, `key.go:127-129`). A serial `red` makes
  "Model [redacted]" at every bind and after every restart, never "[[redacted]acted]".
- **Never the port:** the stored key never takes `PortForms` out. A product string holding a
  port's text keeps it, since a key that changed with the port a unit sits at would lose its
  choice when the unit moves (a restriction lost). `doc.go`'s "never a port" is about the key's
  fields, none of which names where a unit sits; a product string's own text is not a port, and
  no real descriptor holds one (a unit's firmware cannot know its host's port path).
- **Tests (PR 2):** a serial overlapping the marker, through a bind, a restart and a second bind
  (R8); a unit moved between two ports keeps one stored key (C5); the published dacKey is HEAD's,
  against the keys captured above (C1).

## 3. The design and its budget

### 3.1 Two keys, made from a reading

`dsdstate.UnitKeys` holds a unit's two keys, made only by
`dsdstate.UnitKeysOf(usb alsa.USBIdentity, product string) (UnitKeys, error)` from a live reading:

- `Model()`: `KeyOf`'s key as on HEAD (the IDs, the interface, the product string as read): the
  set's identity, the opened node, reconciliation, the probe and its cache, profile matching. In
  memory alone.
- `Stored()`: the same with the product string's serial taken out (3.2): every lookup and every
  write of the state.

No field is exported, so no caller makes one from a stored key, and it has no encoding
(`dacprofile.Key` has none; the file is written by `keyOf` from the state's entries alone).
`KeyOf` stays, its doc corrected to "the key the hardware is told apart by". `Bound` carries
`UnitKeys` where it carried `Key` (`record.go:18-21`); its `Identity` stays.

### 3.2 The strip rule

The stored key is the model key with its product string replaced by
`usb.Serial.Redact(product)`, applied once to the string as read, when the unit has a serial and
the serial is UTF-8; otherwise it is the model key. The IDs and the interface are the model key's.
`NewKey` takes the result (answer 6). It is part of schema 2: a later change to it changes stored
keys, and is a new schema with a re-key of its own.

### 3.3 The re-key: in every read, in every write, at the bind

`State.Rekey(u)` (in place) and `State.Rekeyed(u)` (a copy) are pure: no clock, no I/O.

1. Where `u.Model()` equals `u.Stored()`, nothing.
2. Every warning the model key owns goes to the stored key (merged by 3.4 where the stored key
   owns one of its code).
3. The record under the model key, if any, is merged into the stored key's by 3.4 (renamed where
   there is none), and removed.

Three uses:

- **Every read.** `readDSD` reads the state once (`dsd_policy.go:449`) and, once it has read the
  unit (`:460-470`), re-keys that copy for it (`Rekeyed`); the set and the read side are made
  from it (`:494`, `:496`). `policy.Effective` looks the record up by `Inputs.StateKey`
  (`policy.go:320`, `:349`), the read side by the set's stored key (`dsd_view.go:169-176`). When
  the write lands, the next refresh reads the same record, so B8's rule publishes nothing new.
- **Every write.** `Store.UpdateDAC(u, fn)` re-keys, makes the stored key's record where there is
  none, and hands it to `fn`, in one `Update`; `Store.Migrate` re-keys before its step. Playback's
  `dacOf` (`dsd_policy.go:926-934`) goes, and its three writers (`:997`, `:1018`, `:1303`) take
  `UnitKeys`.
- **At the bind.** `playerDSD.resolve`'s step (answer 7), so a unit's old record becomes durable
  under its stored key even when nothing else is written for it; one Info line says so, naming
  the DAC by its stored key.

A read and the write that later persists it compute one record from one state, so the published
`choice.revision`, first use and warnings do not move when the write lands, and B9's
compare-and-set compares the revision the read side published.

### 3.4 The conflict rule

Merging the old record `o` into the stored key's record `s`. Negatives win, positives are the
model's, and a revision only rises.

**The mode.** Each mode is the set of transports it lets play: auto, every transport by the
automatic rule; native, native; dop, DoP; convert, conversion; off, none. No choice is no
restriction of the record's own (its unit took the bridge's `defaultMode`). The merged mode lets
play what both let play, and off where that is nothing.

|  | none | auto | native | dop | convert | off |
|---|---|---|---|---|---|---|
| **none** | none | auto | native | dop | convert | off |
| **auto** | auto | auto | native | dop | convert | off |
| **native** | native | native | native | off | off | off |
| **dop** | dop | dop | off | dop | off | off |
| **convert** | convert | convert | off | off | convert | off |
| **off** | off | off | off | off | off | off |

- No choice is the absence of one: the other record's choice stands whatever `defaultMode` is.
  A unit with none played by the bridge-wide default, which every own choice outranks (B728
  §2.5); `defaultMode` is the bridge's and is not merged. An explicit off is an own choice, and
  wins every pair.
- auto is no explicit restriction (B728 §3.2: "`auto` means no explicit choice"), so the other's
  explicit mode stands with its own admission (dop's admits a stated claim, §3.2's dop row).
- Two explicit transports share none (native and dop, native and convert, dop and convert): off.
  None of these pairs is reachable from today's writers (answer 8).

**The cap.** The lower non-zero `maxBitRate`; 0 is no cap. It is kept under convert and off, where
it changes nothing (B728 §3.3).

**The revision, its stamp and its author.** One record holds a choice: it stands as it is. Both
hold equal choices (mode and cap): the higher revision stands with its `setAt` and `setBy`. Both
hold different ones: the revision is one past both, `setAt` the later of the two, and `setBy` the
later's where the merged choice is its, the earlier's where it is the earlier's, else `migration`.
At revision 2 or more `ownChoice` counts any author as the user's (`migrate.go:235-242`), so no
later step of the legacy migration replaces it. A revision never falls: a base a client read from
a record whose choice changed, or from the lower of two equal ones, is below the stored revision
and B9's PUT will answer it 409 `stale`; a base read from the choice that stands as it was is the
choice it read.

**DoP confirmation.** The later `confirmedAt` stands, whole, with its firmware (a tie: `s`'s). The
other lapses, as a firmware update lapses one: DoP then needs a confirmation again under that
firmware.

**Listener answers**, one for each transport, rate and firmware: a rejection (`noise`, `nothing`)
over `clean`, whatever their dates, which cannot order two units' answers; of two rejections, the
later `on` (undated earliest; a tie: `noise`); of two clean answers, the later `on`.

**First use**, one acknowledgement a transport:

1. An acknowledgement of a transport under a firmware where the merged answers hold a rejection of
   that transport is dropped: the re-arm a rejection makes in its own record (B728 §3.2, §6.4),
   which keeps no other mark.
2. One left: it stands.
3. Two under one firmware: one entry, its rates the union of theirs (none listed, every rate, where
   either lists none), `acknowledgedAt` the later, `observedAt` the earlier of the two that are set.
4. Two under different firmware: the later `acknowledgedAt` stands whole (a tie: `s`'s); the other
   firmware needs an acknowledgement again, as after a firmware update.

**Evidence**, one entry for each transport, format, rate and firmware: `streamingObservedAt` the
earlier of the two that are set; `driverAcceptedAt` with its `kernel`, the pair whose time is later
(a tie: `s`'s). Evidence is the ladder's rung alone (`policy.go:440-456`) and admits nothing.

**Warnings.** Re-owned (answer 10); two of one code under one owner: the later `at` with its reason
(a tie: `s`'s).

**The 64-entry bounds** (`state.go:104`, `:707-709`; the probe's item 6). Listener: every rejection
stays, and clean answers go, the earliest `on` first (undated first, then by firmware, transport and
rate), until 64 are left. Where the rejections alone pass 64, the merged choice is off (revision one
past both, `setBy` `migration`, `setAt` the latest stamp either record holds, else the Unix epoch),
and the 64 rejections with the latest `on` stay: off refuses every rate, so the dropped ones guard
nothing until a choice is written. A firmware holds six answers at most (two transports at three
rates), so that needs eleven firmware versions with rejections. Evidence: entries go by the later
of their two times, the earliest first, until 64 are left; only a rung falls.

**What a merge can widen.** A unit gains its model's positive inputs: unit A's first use
acknowledged while B's record held none lets B play that transport under that firmware, as one
record per model has it by design. Every negative of either record (an explicit off, a rejection
and its re-arm, a lower cap, the meet of two explicit modes) holds for both. 6.6 weighs keeping
only the positives both records hold.

### 3.5 Records never resolved

An old record whose unit is not bound again stays under its old key, as it is. It is read only
for a live unit whose model key is that key (3.3) and moved when that unit binds. No writer writes
under it: every write takes a unit's stored key, and a record under a model key that equals its
stored key is the same record. A restore carries it as it is (`ForRestore` has no unit to re-key
by), and so does a backup. Nothing deletes it. A record whose product string holds no serial
needs no move: its two keys are one.

### 3.6 Schema 2

`SchemaVersion` becomes 2 (`state.go:24-28`) with the same fields: a key's product string is the
one the unit reports with its serial taken out by 3.2's rule, where a write of this schema made
it from a live unit; a key read from schema 1, or kept from one, may hold a serial until its unit
binds. `Decode` reads 1 and 2; every write is 2, `Refusing`'s and a restore's included
(`ForRestore`, `restore.go:47`). A file nothing writes stays as it is: `updateLocked` writes nothing
when the encoded state is unchanged (`store.go:338-340`). `TestTheStateIsSchemaOneAsWritten`
(`state_test.go:141`) becomes schema 2's golden.

### 3.7 The key map

| Consumer | Where | HEAD | B772 |
|---|---|---|---|
| The policy's lookup (`ChoiceFor`, `DAC`) | `policy.go:320`, `:349` | the model key | `Inputs.StateKey`, on the state re-keyed for the unit |
| `Profiles.Match` | `policy.go:345`, `dsd_policy.go:490` | model | model |
| The set's identity (`Set.Key`, `Identity`) | `policy.go:308`, `:277` | model | model |
| The read side (`ChoiceFor`, `DAC`, `warningsOf`) | `dsd_view.go:169-176` | the set's key | the set's stored key, re-keyed state |
| The published dacKey (`wireKey`) | `dsd_view.go:230`, `:281-302` | model, through `unitText` | unchanged |
| The opened node (`checkDSDNode`, `setOpenedIdentity`) | `dsd_policy.go:890-909` | model | model |
| Reconciliation (`Opened` against the set's identity) | `reconcile.go:74`; `dsd_policy.go:717`, `:731`, `:865` | model | model |
| Probe caching (`ProbeUnit.Model`, `Forget`) | `dsd_policy.go:434-436`, `:1325-1334` | model | model; `ForgetDSDProbe` is given the model key (`serve_player_dsd.go:91-96`) |
| The probe's check of its node (`probedUnit`) | `dsd_policy.go:1261-1274` | model | model |
| Evidence writes (`noteDriverAccepted`, `noteStreamingObserved`, `noteProbed`) | `dsd_policy.go:992-1032`, `:1280-1316` | the set's key or `KeyOf`, captured when asked | `UnitKeys`, captured when asked; `UpdateDAC` re-keys when it runs |
| The migration's step (`Migrate`, `ownChoice`, its rows and warnings) | `store.go:366-385`, `migrate.go:149-315` | `Bound.Key`, model | the stored key, on the state the same write re-keyed |
| The legacy reader (`ReadRecord`, `sameModel`) | `record.go:67-107` | `Bound.Identity`, as read | unchanged |
| The migration's log lines (`dacLabel`, `ChoiceFor`) | `serve_player_dsd.go:309-331`, `:435` | model: prints the serial | stored |
| A restore (`ForRestore`) | `restore.go:32-52` | keys as decoded | unchanged keys, written as schema 2 |
| B9's PUT, B12's answer, B13's console (not built) | `plan-b728:1325-1390` | (none) | `UpdateDAC`, and `ForgetDSDProbe` given the model key |

The admission check: `admitDSD` and `selectFor` read no key; `checkDSDActivation` judges by
reconciliation, and `checkDSDNode` by the opened identity, both by the model key as on HEAD.

### 3.8 What B9, B12 and B13 take from it

Each writes a record through `Store.UpdateDAC` with the bound unit's `UnitKeys`, and compares
`baseRevision` inside `fn`, after the re-key, with the revision the read side published. B9
answers `dac_changed` by the published dacKey as its design says; where that key is coarser than
the stored key (residual 8) it cannot tell two units apart by it alone. A choice written calls
`ForgetDSDProbe` with the model key.

### 3.9 The budget

| The brief's budget | What B772 uses |
|---|---|
| One redaction rule, shared with the wire | `Serial.Redact`, once, on the product string as read: the stored key's whole rule and the wire's serial step; the wire is unchanged |
| A re-key at bind time | `playerDSD.resolve`'s step (answer 7), in serve and `bridge output play` |
| A stated rule for records never resolved | 3.5 |
| The conservative conflict rule | 3.4, in `State.Rekey` |
| No new files on disk | none: the same file and fields, schema 2 |
| No change to which DACs are told apart | the model key keeps every identity at play (3.7); the state stops telling apart only what a serial told apart, units of one model, as B728 designs it (residual 11) |
| Residuals stated | section 5 |

What the binding points add beyond it: the re-key in every read (point 7), `Store.UpdateDAC`
(point 9), schema 2 (point 11), `policy.Inputs.StateKey` (point 3), and `dacLabel` by the stored
key (the hazard's journal path). No config key, wire member, warning code or Mirror-PR.

## 4. The PRs

Two, stacked, as B728's B10 and B11 were: the logic first, unwired, then the switch. The version
bump ships with its readers, in PR 2: a build that read schema 2 without the re-key in its reads
would look stored keys up by model keys.

**Cases.** An R case is a regression: red on main, green on the PR. A C case is compatibility:
green on main and on the PR. A control is given only where the case asserts the very mechanism the
control removes, beyond what main's red already shows.

### PR 1: the stored key and the re-key, not wired

`dsdstate` gains `UnitKeys`, `UnitKeysOf`, `State.Rekey` and `Rekeyed` with 3.4's merge, its
bounds and its warnings. Nothing calls them; no schema, file or wire change. Main lacks the API,
so its reds run on a names-only shim of main's behaviour (`UnitKeysOf` gives the model key as
both, `Rekeyed` returns the state as it is), where each case fails by assertion.

- **R1** `TestTheStoredKeyTakesOutTheSerialOnce`: answer 6's table (the probe's rows): the model
  key is `KeyOf`'s, the stored key's product string is `Redact`'s once, the key for a serial that
  is not UTF-8 is the model key, `NewKey` takes every result.
- **R2** `TestARekeyMovesTheOldRecordAndItsWarnings`: the record renamed, its warnings re-owned,
  none left under the model key, the encoded state free of the serial.
- **R3** `TestARekeyIsPureAndIdempotent`: `Rekeyed` twice is once, the input unchanged, no clock.
- **R4** `TestTheMergeKeepsEveryRestriction` (in `policy`, which imports `dsdstate`): every cell of
  3.4's mode table under defaultMode auto and off, with caps; for each, `policy.Effective` over the
  merged state at DSD64, DSD128 and DSD256 (a known DAC offering native and DoP at every rate,
  first use acknowledged, gain at unity), and the merged revision. The lookup's key is the
  stored key, the test DAC's profile matching by its IDs.
- **R5** `TestARejectionReArmsFirstUseAcrossAMerge`: one record's native acknowledgement under F,
  the other's noise for native at DSD128 under F: no native acknowledgement, and under auto every
  rate converts or takes DoP, never native.
- **R6** `TestFirmwareAndCoverageMergeByTheirRules`: two confirmations, two acknowledgements under
  one firmware and under two, clean answers and evidence, each with the effective outcome.
- **R7** `TestAMergePastTheBoundsKeepsEveryRejection`: 100 clean and 10 rejections keep 54 and 10;
  65 rejections make the choice off; evidence past 64 trimmed.
- **R8** `TestEveryMergeChecks`: every pair of a generated corpus of valid records merges to a
  state `check` takes and encodes.
- **C** the existing `dsdstate`, `dacprofile` and `policy` suites, unchanged.
- **Controls**, each one rule removed from the new code, its row red: off no longer wins (the later
  choice does); the re-arm removed; the higher cap kept; the revision kept at the higher (not one
  past); a later clean answer over a rejection; a rejection dropped at the bound.

### PR 2: the state keyed by the stored key

Schema 2; `policy.Inputs.StateKey` and `Set`'s stored key; `readDSD` and the read side on the
re-keyed state; `dsdSet` carries `UnitKeys`; `Store.UpdateDAC`, through which `recordDSD` and its
three writers write, `dacOf` gone; `Bound` with `UnitKeys`, `Store.Migrate` re-keying first;
`playerDSD.resolve`'s step on every fresh, available snapshot, its retries as `ask`'s;
`dsdPartChanged` and B9's future write give `ForgetDSDProbe` the model key; `dacLabel` by the
stored key; `ForRestore` writing schema 2. Docs in the same PR: `dsdstate/doc.go` and `KeyOf`'s
doc, CLAUDE.md's `internal/player/dsdstate` row, `player.md`'s DSD state bullet and its History
long form (proposed below), and an `ops/engineering-log.md` record. `player.md` is 99,798 bytes
of its 100,000 at `4678b983`, so the bullet's new words come with an entry's long form moved out
(`scripts/doc-split/move_entry.py .claude/rules/player.md --largest`). `PROTOCOL.md` does not
change: no Mirror-PR. Tests that move with the schema, deliberately: the golden
`TestTheStateIsSchemaOneAsWritten` becomes schema 2's; `TestTheStateRefusesWhatNoWriterWrites`'s
"another schema" row (`state_test.go:174`), which pins that 2 is refused, takes 3; the backup
and restore tests that compare what a restore wrote read 2. Inputs written as schema 1 stay, as
1 is still read.

- **R1** `TestNoDSDStateHoldsASerialOrAPort` (`privacy_test.go:47`) gains the "ADI-2 DAC
  (52813721)" unit: on main the file holds the serial twice (the probe's item 1).
- **R2** `TestPlaybacksEvidenceLandsUnderTheStoredKey`: a backend over that unit records a
  driver's acceptance, streaming observed and a probe's acceptance, one of them held at the
  store's write until the bind's re-key has landed and one run before it: one record, under the
  stored key, the old record not made again. Main: under the model key.
- **R3** `TestServeRekeysAnOldRecordAtTheBind`: a schema-1 file as a development build leaves it
  (the unit's record holding an explicit off and a DoP rejection, its warning, the migration
  done), serve booted with the unit bound: the file is schema 2 and holds no serial, one record
  keeps the off and the rejection, the warning is the stored key's, and GET /v1/outputs' `dsdDAC`
  is the same before and after the write (dacKey, mode off, revision). Main: the file as it was.
- **R4** `TestARekeyThatCannotBeWrittenKeepsTheRestriction`: the first re-key write refused (a
  medium seam), every rate refused `dsd_off_by_choice` throughout, the write landing at a later
  tick. Main: no write.
- **R5** `TestTheMigrationsLinesNameNoSerial`: the migration's Warn and Info lines for that unit
  (`loggingtest`'s recorder, every string of every record, and the line itself present). Main:
  `dac=2a39:3fd2 ADI-2 DAC (52813721)`.
- **R6** `TestTwoUnitsOfAModelShareOneRecord`: the probe's item 7 through serve, units A then R
  bound: one record under the stored key with the record's rows, both units publishing one
  dacKey, one choice and one revision. Main: two records.
- **R7** `TestEveryStateFileIsSchemaTwo`: `Update`, `Migrate`, the damaged path, `ForRestore` and
  `WriteRestored` each write schema 2. Main: 1.
- **R8** `TestABindARestartAndASecondBindLeaveOneRecord`: serial "red": bind, reopen the store,
  bind again: one record, "Model [redacted]", never "[[redacted]acted]". Main: the model key.
- **R9** `TestNoPackageButTheStateMakesARecord`: a type-aware guard over the code the binary
  links, outside `dsdstate`: no `dsdstate.DAC` composite literal and no append to a
  `dsdstate.State`'s `DACs` (`playbacktest`, test support, is outside it). Main:
  `dsd_policy.go:932`.
- **C1** `TestThePublishedDACKeyIsHEADs`: the probe's rows against the keys captured at
  `4678b983` (section 1), beside the unchanged B8 goldens
  (`TestEachKindOfDACsWholeResponseIsItsGolden`) and `TestNoDACKeyOrModelCarriesASerialOrAPort`.
- **C2** `TestTheCheckAfterOpenTellsTwoUnitsOfAModelApart`: units A and B, product strings holding
  their serials: a DSD sink admitted for A and reopened on B is `ReasonWrongDevice`, and a set made
  for B revokes A's occurrence.
- **C3** `TestTheProbeAsksEachUnitAlone`: a probe of A leaves B unprobed.
- **C4** `TestProfilesMatchTheUnitAsItReads`: a shared reference ID with its product string, a
  serial overlapping that text, an ID-only profile: each matched as before.
- **C5** `TestAUnitMovedBetweenPortsKeepsItsStoredKey`: a product string holding a port's text, at
  two ports: one stored key, its choice followed.
- **C6** the older reader, on `4678b983`'s own tree (a copy of it, run once, the run named in the
  engineering log): PR 2's bytes for a DAC with an explicit off and a rejection are Damaged, kept
  aside, and every rate is refused `dsd_state_unreadable`. Beside it, permanent:
  `TestADowngradedBoxRefusesDSDUntilAChoice`: the refusing schema-1 file an older build leaves
  reads back refusing.
- **C7** every existing DSD test of `dsdstate`, `playback` and serve, whose test DAC's product
  string holds no serial (`playbacktest/policy.go:40-54`), unchanged.
- **Controls:** R4 with `readDSD` reading the store's state without the re-key (the restriction
  falls to `auto`: red; main's red covers only the write); R3 with the step gated on the
  migration's wait again; R2 with `recordDSD` writing under the model key; R8 with the stored key
  computed again from a stored key; C2 with `checkDSDNode` by the stored key; C4 with
  `Effective` matching by the stored key; C5 with the strip through `unitText`; C1 with the wire
  key taken from the stored key, ports after (red on the two contrived rows).

**The rules line, proposed** (`player.md`, the DSD state bullet; long form to
`ops/History-rules-player.md`): "…keys each DAC by its model's stored key (`dsdstate.UnitKeysOf`:
the product string as read with the unit's own serial taken out once by `usbid.Serial.Redact`,
never the port), schema 2 since B772, a key from before re-keyed at every read for the bound unit,
in every write for it and at its bind, by the conflict rule (an explicit off and a rejection win,
the rest is the model's).** **Don't write a record but through `Store.UpdateDAC` under a unit's
stored key, look one up on a state not re-keyed for the bound unit, take a serial out of a stored
key, tell the opened node, the set, a reconciliation, the probe or a profile match apart by the
stored key, let a merge drop an explicit off or a rejection, keep an acknowledgement beside a
rejection of its transport under its firmware, change the strip rule without a new schema, or
write schema 1.**"

## 5. Residuals

1. **Old records of units never bound again** keep their serial: on development and test boxes'
   files written between B6 (#1413) and PR 2, in `bridge output play` state directories, and in
   states restored from their snapshots. Moved at the unit's next bind, never otherwise; how many
   there are is unknowable, since a key carries no mark of a serial.
2. **Backups taken before the fix** hold the old file as it was. A snapshot is not rewritten (its
   verification compares the bytes with the baselines its manifest records, B653), so they leave
   with retention's prune, and a copy made elsewhere stays. A restore of one brings its old
   records back until each unit binds.
3. **`.bad-*` copies**: those an older build made of a damaged file, and those an older build
   makes of a schema-2 file after a downgrade (answer 11), each kept for good.
4. **The journal**: the migration's lines logged before PR 2 name the product string with its
   serial (`dacLabel`), until the journal rotates.
5. **A serial in another form**: a product string that prints the serial otherwise than the
   attribute holds it (another prefix or separators, another case, a part of it), or a unit with
   no serial attribute whose product string names one, keeps it. Only the attribute's own bytes
   are known, and anything else would be a guess.
6. **Two models whose serials are their model texts**: two models on one vendor and product ID and
   interface, each unit's serial the text that tells its model from the other's, inside its
   product string, share a stored key, as they share a dacKey on HEAD's wire (the probe's XMOS
   rows). One's positive inputs then stand for the other, under the same firmware alone (every
   acknowledgement, confirmation and answer is held under one); both models' negatives hold for
   both. No such pair is known; a guard that tells them apart needs the serial (6.3).
7. **The marker as text**: a product string that holds "[redacted]" itself shares a record with the
   units whose product strings become it.
8. **The wire coarser than the state**: where a product string holds a port form (B8 round 2's
   fixtures) or the serial is not UTF-8, the published dacKey is coarser than the stored key, and
   B9's `dac_changed` cannot tell those units apart by it alone. No real descriptor holds either.
9. **Downgrades**: a downgrade to a build from B3 to PR 2's parent (none of them a release) refuses
   DSD until a choice is written, the choices left in a `.bad-*` copy (answer 11). A rollback to
   0.2.3 reads neither schema (B728's own residual).
10. **The owner's approval record** (the harness's file `dsdQualification` names) holds the unit's
    identity, port and product string included, by the harness's design; B772 does not touch it.
11. **What the state tells apart**: units of one model whose product strings hold their serials
    now share one record, as B728 designs and PROTOCOL.md says; on HEAD they had one each. Binding
    a second unit can make the model stricter for both (its off and rejections win), and a unit
    gains its model's positive inputs (3.4).

## 6. Alternatives weighed

### 6.1 Redact on backup and export only

Keep the key as read in the live file and take the serial out where the file leaves the box.
Refused: the hazard is the live file too (B3's rule is "none of which the file holds"), its
`.bad-*` copies and the journal. A backup cannot know the serial: `bridge backup` runs without the
player, and the ticker's serve knows the bound unit's alone, so for every other record it could
only drop the product string or the record, and a restore would then lose an explicit off or a
rejection, or merge models on a shared reference ID (the IDs and the interface alone do not tell
them apart). Two forms of one state would also give a restore keys no live writer writes, needing
the re-key anyway. The only export of the state today is GET /v1/outputs, which already redacts.

### 6.2 Re-key at open, by the configured serial

`bridge.yaml` holds the serial of a unit bound by serial. Refused: it covers that one unit, which
its next bind re-keys anyway, and a stored product string holding the configured serial as a
substring proves less than a live unit whose whole key is the stored key: with a short serial it
is a guess.

### 6.3 A threshold or a guard in the strip rule

A length (MPD's four bytes) or a whole-token rule, or no strip on a shared reference ID or where a
profile's product string is the unit's. Refused. Applied to the stored key alone, the stored key and
the published key would differ, where B9's write is to land on the record the dacKey it is sent
names. Applied to both,
the published key would be a hash of a short serial, which B8 refused (`dsd_view.go:273-280`). A
profile-based guard would make a stored key depend on the database, which a release changes: a DAC
given a profile later would change its key and lose its record. A stored key must be a function of
the unit's own reading, frozen with the schema.

### 6.4 Keep the units apart

By an HMAC of the serial under a secret of the box's, or an ordinal per unit. Refused: an HMAC is a
function of the serial (B8's rule), brute-forced where the secret travels with the backups, and a
new secret is a new file; an ordinal cannot be matched to a unit later without the serial.

### 6.5 Key by the IDs and the interface alone

Drop the product string. Refused: on a shared reference ID the product string is what tells models
apart (B728 §2.2), so models would merge.

### 6.6 Keep only the positives both records hold

Strict intersection: an acknowledgement, a confirmation or a clean answer survives a merge only
where both records hold it. Safe, and it asks a user to acknowledge again after every merge.
Refused as the default because B728 keys consent by model (a first use is the model's behaviour,
and two units of one model share a choice, §7.2) and every negative still wins; it is the
owner's to choose instead (below). It would not close residual 6: after a merge, every later write
is the shared record's.

### 6.7 Keep schema 1, or write 2 only where a key holds the marker

Schema 1 with stored keys was refused (answer 11). A version that depends on content (2 only where
a key holds "[redacted]") keeps older development builds reading every other file, since a file
with no marker holds no re-keyed record; refused for one version written always, since the older
readers are development builds alone.

### 6.8 Re-key every connected unit, not just the bound one

The watcher reads every card. Refused: it reads every card's descriptors at every snapshot and
writes for DACs the player does not use, for a population of development boxes.

### 6.9 Delete or expire records never resolved

Refused: it drops an explicit off or a rejection of a unit that may come back.

### 6.10 One function for the wire and the state

Take the serial out first and the port forms after, on the wire too. Refused: by the probe it
changes no real key and two contrived ones, but where a serial overlaps a port form the free text
keeps most of the port ("…/usb2-[redacted]"), which HEAD's order hides; the wire stays as it is.

### 6.11 A mark per record, or a list of old records

A flag on every record a live unit keyed, or schema 2 keeping schema 1's records in a list of their
own, would let the doctor count what is left and a restore tell old records apart. Refused for
this round: it is structure for development boxes' files alone; a follow-up if the owner wants the
count.

### Decisions a review may overturn

The meet of two explicit transports (off, where convert is the other safe answer); the positives
kept by model (6.6); residual 6 accepted without a guard (6.3); one schema version written always
(6.7).
