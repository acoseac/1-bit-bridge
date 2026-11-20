## 2026-11-20 — Bridge lows 11: a pin list names its email case; B945's record says what it did not record

Branch `fix/bridge-lows11`, from main `fb8706d8`, #1523's merge (the Fosi Audio ZH3 row), which
the brief required on main first. Two commits:
- `f7d5e8be`: both records corrected, `ops/History-rules-dac-catalog.md` and B945's record;
- this record.

No code, test or rule line changes. Lines cited are `fb8706d8`'s unless a commit is named. Every
command below ran from the checkout's root with `GOTOOLCHAIN=go1.27.3`, each `go test` with
`TMPDIR` on disk in a scratch folder below the checkout's `.tmp/`.

### Item 1: the submission test's pins name its email case

What was true. In `ops/History-rules-dac-catalog.md`, the long form of the catalog's first entry
(7) listed what `TestARowAddedPastTheRegistryNamesItsSubmission` pins: each form beside a DoP
claim sourced to the listening; a submission with no date, a date that is none, a date run into
its text or another kind; and a DoP claim citing the registry beside a submission. The test
refuses one case more, a submission whose free text holds an email address
(`internal/player/dacprofile/lint_test.go:191`), which B944's second round added (its record's
Lows, (a): `ops/engineering-log/2026-11/2026-11-19-b944--feat-b944-fosi-zh3-row.md:229-234`) and
the lint refuses as it refuses an email address in every source (`sourced`,
`internal/player/dacprofile/lint.go:938-948`). The entry's bold head says the free text "names no
person and holds no email address"; its body's sentence on the same free text said "names no
person" alone.

As corrected. The pin list gives the case an item of its own, "a submission whose free text holds
an email address", where the test puts it, after the submission's forms and before the DoP claim
(`lint_test.go:186-195`); the body's sentence says "names no person and holds no email address",
as the bold head does. The rule line, `.claude/rules/dac-catalog.md:13`, names the test without
its cases, and its bold head, the same as the long form's, already says that, so it is unchanged;
the long form's bold head and its "Don't" sentence are unchanged too.

### Item 2: B945's record says which tests its controls' command line ran

What was true. B945's record,
`ops/engineering-log/2026-11/2026-11-19-b945--fix-b945-verify-hold-timing.md`, gives one command
line for its round 2's controls (227), introduced as what its scratch generator runs for each
control (225), and that line's `-run` selects `TestAGivenUpVerificationReadsItsRecordBack` and
`TestAnAbandonedCheckKeepsItsHoldsUntilItsCheckerEnds` alone. Its controls of the nil-gate path
(249-253) ran `TestARecheckGivenUpLeavesTheVetoItMade`, which that `-run` does not select. That
record gives no command line for them, and no other tracked file does.

As corrected, in place (225-232 at `f7d5e8be`): the sentence before the command line says it is
the two gated tests', and the one after it that its `-run` selects those two alone, that
`TestARecheckGivenUpLeavesTheVetoItMade` is not among them, and that its command line was not
recorded. No command line is reconstructed, and no section is added.

### Runs, at `f7d5e8be`

`make docs-check` over both edits before their commit: ok ("check-engineering-log: ok — … 17
entries since"). The citation guard, over the edits before their commit and again at
`f7d5e8be`:
`go test -tags ts_omit_webclient,ts_omit_debugeventbus -count=1 -timeout 30m -run '^TestEveryCitedTestNameExists$' -v ./cmd/bridge/`:
"--- PASS: TestEveryCitedTestNameExists (0.81s)".

Two controls, each that command with one citation suffixed (`…Ghost`) on disk for the whole run,
predicted in writing before either ran, the file restored from memory after it and the tree
checked clean:

| | mutation | predicted | seen |
| --- | --- | --- | --- |
| G1 | item 1's pin list, its citation of `TestARowAddedPastTheRegistryNamesItsSubmission`, suffixed | the guard red, exit 1, with one error alone, naming the suffixed name "(cited by ops/History-rules-dac-catalog.md)" | as predicted, at `cited_test_names_test.go:74` |
| G2 | item 2's corrected sentence, its citation of `TestARecheckGivenUpLeavesTheVetoItMade`, suffixed | as G1, "(cited by ops/engineering-log/2026-11/2026-11-19-b945--fix-b945-verify-hold-timing.md)" | as predicted, at the same line |

The two show the guard reading each edited file rather than infer it. With this record staged on
`f7d5e8be` and not yet committed, `make docs-check` said "18 entries since", and G3, item 1's
citation of `TestARowAddedPastTheRegistryNamesItsSubmission` in this file suffixed, predicted and
run as the two were, went red naming this file alone, exit 1: the guard reads it too.

The gate on this record's commit is in the branch's report: a record holds no run made on its
own commit.
