## 2026-11-19 — B939 round 2: a paragraph decides where a list item ends

Branch `fix/b939-log-checker-lows`, from round 1's head `ed9c3b20`. Commits: ff386e8a (the
scan), c95819ce (a row that could not tell two readings apart), 702fbc50 (the README),
89d49843 (the fixtures), 25d54448 (the rule), then this record. Round 1's record is
`2026-11-18-b939--fix-b939-log-checker-lows.md`; what it says of `h2_lines` is round 1's.

### What was wrong

Round 1 made list items containers but followed no paragraph: every marker opened an item and
every line indented less than an item's width ended it. So it refused valid entries main's
checker accepted (`2. item` under a paragraph's text, which CommonMark reads as more of that
text, opened an item whose fence ended at the next unindented line and exposed the `## `
inside it; a lazy line ended its item, and a fence four spaces in became code), and accepted
stray `## ` lines main refused (an empty `-` item went on past a blank line, so a later fence
closed early and its closer opened a fence around the heading; a `2.` that is paragraph text
opened a fence around one). Round 1's fuzz had found these: 69 disagreements with CommonMark,
which its record named as limits. It compared the new scan with CommonMark alone, never with
main's checker, so it did not see that some were entries main read right.

### What changed

`h2_lines` reads CommonMark's block starts in their order (quote, ATX heading, fence, setext
underline, thematic break, list item, indented code) and keeps whether a paragraph is open: a
list item interrupts one only with something on its line and, ordered, numbered 1 (`1.`,
`01.`; `0.` and `2.` do not); a run of `=` or `-` under one underlines it; a line four spaces in
under one is more of it; a line that starts no block while one is open is lazy and keeps every
container open; an item that holds nothing yet ends at a blank line. Where a line leaves an item
it would continue, a marker opens a new item whatever it is: markdown-it-py opens an ordered
list for `- foo` then `2. bar`, as the reference algorithm does (with the item unmatched, its
list is the container the start is tried against, not the paragraph), though one review read
it as paragraph text; the scan follows the parsers, and a row pins it. What counts as a `## `
line is unchanged. The docstring says what is read, and where `markdownShellLines` differs:
it follows no paragraph, opens containers inside a fence, takes four spaces as no code, and
reads `- - -` as items. HTML blocks are read by neither, and a tab is four spaces, as before.

### Rows, red first, controls

Sixteen rows give the `## ` lines of entries with paragraphs, the four the reviews named among
them, and one asserts those four refused or accepted by the message. Seven were red at round 1's
head (the empty item at a blank line, `2.` after text, the lazy line, the heading after a `2.`
that is text, `0.`, the empty bullet, the line four spaces in), and the acceptance row; nine pass
there and guard against a fix that blocks every marker after text or ends an item at every
blank line. Red first: round 1's three functions in a copy failed those seven and the
acceptance row. Controls, each predicted: no lazy line (the lazy row, the acceptance row); every
marker interrupting (the three rows on `2.` and `0.` and the acceptance row, and, unpredicted,
the four-spaces row, whose `2.` half needs the same limit); no underline (the `===` row); a
blank line going on in every item (the empty-item row, the acceptance row); a fence's closer
taken for paragraph text (its row); a line four spaces in taken for code (its row). A blank line
ending every item failed the round 1 row for a blank inside an item's fence, and not the row
for an item that holds something, though predicted to: that row's fence sat at two spaces, a
fence at the top level too. c95819ce moves it two spaces into the item, four in all, and the
control then failed it.

Against markdown-it-py 3.0.0's CommonMark, a line counting where it opens an h2 and begins `## `,
over 800,000 random entries (seeds 1 to 6 from 89 lines, one to nine each; seeds 7 and 8 from
99, one to fourteen; no tabs, no HTML): main's checker disagreed on 6,543, round 1's on 249,
this one on none, and no entry main read right is read wrong. A marker four or more spaces in
where only its list goes on, which the reference algorithm may start an item at, is read as a
lazy line here and by markdown-it alike.

### The README

Its bullet on entries filed elsewhere said a folder git ignores whole is not looked in and that
any dated file committed in `ops/` fails. `listing()` enters every folder below a top-level
`ops` folder, ignored ones too, and keeps `--directory` for the rest of the tree; the tracked pass
refuses dated Markdown there, not other kinds. It says that now. In a scratch checkout, a name
like an entry in an ignored `ops/private/` was refused, one in an ignored `scratch/` was not
read, one in `Ops/` was found, and a tracked dated `.txt` in `ops/` passed.

### The fixtures

The dated private note in the checker's and the hook's self-tests is
`ops/2026-11-20-sample-audit.md`, a neutral name that still matches the fixture's
`ops/*audit*.md`. `check()` drops a dated note before it reads whether git ignores it, so a
rename that lost the match would have left every row green; a row asks `listing()` and expects
the note ignored, the plain note not, the note named like an entry ignored. A rename without
`audit` failed that row alone; the ignore file without `ops/*audit*.md` failed it and the two
rows that expect the named note ignored.

### The rule

No rules file governed the checker. `build-ci-tests.md` takes it among its paths, with a bullet:
the scan reads where a fence ends as CommonMark does, paragraphs included, and a change to it is
compared over random entries with a CommonMark parser and with main's checker.

### Not checked from this host

A Windows run of the self-test. The gate on this record's commit is in the round's report.
