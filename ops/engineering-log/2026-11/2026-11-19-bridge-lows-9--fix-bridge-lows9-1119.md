## 2026-11-19 — Bridge lows 9: a tab in a label counted as one character, and a label's backslashes and a title on the next line pinned as they are

Branch `fix/bridge-lows9-1119`, from main `b332bb6c` (#1519's merge, Lows 7's `bc79885c` under
it); line numbers below are that commit's `scripts/check-engineering-log.py`. Five commits:
`1352ee90`, item 1's rows, two of them failing; `dcaf6803`, item 1's fix; `478c4d6d`, item 2's
rows; `f9c34a28`, item 3's rows; and this record. The verdicts are CommonMark 0.31.2's. As the
brief's review asked, items 2 and 3 are preservation: rows that pass at the base, and no change
to the checker. The README and the rule state none of these three rules, so neither changes. In
the inputs below, `\t` is a tab, as in a Python string.

### Item 1: a tab is one of a label's 999 characters

What was wrong. `h2_lines` expanded each line's tabs to four-column stops before it read the line
(`line.expandtabs(4)`, 302) and kept an open paragraph's lines in that form (`rest.lstrip(" ")`,
314 and 320), so `only_references` (178-199) held a label to its 999 characters (`m.end() - pos >
1001`, 187) with a tab counted as the one to four spaces it expands to. The spec expands tabs only
where spaces define block structure (§2.2): a paragraph's text keeps them, and a label's 999
(§6.3) count characters. A label of 997 `a`, a tab and a `b`, the tab two columns short of a stop,
measured 1,000 and was refused, so the `===` under it made a heading and the `2.` after that
opened an item whose fence hid the `## ` line. This entry, as a Python string, gave `[1]` where
CommonMark gives `[1, 5]`:

~~~
"## 2026-11-18 — B888: the log\n" + "[" + "a" * 997 + "\tb]: /url\n===\n2. ```\n   ## stray\n```\n"
~~~

As built. The blocks are still read from the expanded line. `as_written(raw, rest)` gives each
paragraph line as written, from the character the expanded text begins at: the characters that
are neither a space nor a tab are the same in both lines and in the same order, so the line as
written is cut where as many of them are left as the expanded text holds. A line with no tab is
read as before, its expanded text being the line as written.

Rows, red first. `1352ee90` adds six rows, and at it the self-test fails two, as the base's scan
does under them: "a label's 999 characters count a tab as one", the entry above, `got [1], want
[1, 5]`; and "a tab that indents a paragraph's later line is no character of the label on it",
the same label on a paragraph's second line, indented by a tab, after `[ref]: /url`, `got [1],
want [1, 6]`. `dcaf6803` passes both. The other four pass at both commits and at the base: a label
of 1,000 characters, one of them a tab, still refused (`[1]`), and the three controls the brief's
review asked for, that a tab after a list marker (`` -\t``` `` then `` 2. ``` ``, `[1]`), after a
quote marker (`` >\t``` ``, then `foo`, then `` 2. ``` ``, `[1, 5]`) and between a thematic break's
stars (`*\t*\t*` under a paragraph's text, `[1]`) still read as at the base. Every commit's
self-test, from that commit's file: the base passes, `1352ee90` fails those two rows alone, and
`dcaf6803`, `478c4d6d` and `f9c34a28` pass.

`as_written` against a mapping of its own, the line as written from the character whose expanded
position (`len(raw[:i].expandtabs(4))`) is where the text begins: every line of up to eight
characters over a tab, a space, `a`, `>`, `-`, `[` and a carriage return, at every suffix of its
expansion that is not blank, 62,924,640 suffixes of 6,725,601 lines. None differs.

Controls, each predicted in writing first, each an exact-once mutation of the committed file with
its bytes restored from memory, every one as predicted. K1, the fix taken back: red the two rows
above, `[1]` each. K2, the expanded line's offset taken as an index into the line as written: red
the second alone, `[1]`. K3, the kept text's tabs dropped, and K4, the label's limit one higher:
red the 1,000-character row, `[1, 5]`. K5, the blocks read with no tab expanded: red the three
controls (`[1, 4]`, `[1]`, `[1, 6]`), Lows 7's row of two spaces and a tab (`[1, 4]`) and the
second label row (`[1]`). K6, the blocks read with a tab as one column: every row green, so no row
tells a tab that reaches its stop from one read as a single space (a follow-up, below).

### Item 2: a label's backslashes, as they are

`LABEL` (87) finds where a label ends and unescapes nothing: a backslash takes the character
after it along. The characters that end or break a label, `[`, `]` and the backslash itself, are
ASCII punctuation, which a backslash escapes (§2.4), so it ends every label where CommonMark's
escapes end it, and a backslash before anything else stays a literal one. Shown over every string
of up to seven characters after the opener, from the construct's own delimiters, a backslash,
`a`, `q`, a space, a line end and `!`: `LABEL`, `TITLE` in each of its three forms and `ANGLED`
end where scanners written from the spec's words end, which escape ASCII punctuation alone, in all
2,396,745 strings each. Over the strings of up to five characters the same comparison finds 1,666
differences for a label read with no escapes and 1,000 for one that escapes punctuation alone and
refuses a lone backslash (`[\a]` the first), so it would have seen one. No input's verdict
differs, so `LABEL` is unchanged.

Rows (`478c4d6d`), each passing at the base and after: `[a\]: /url` defines nothing, its `]`
escaped, so the underline makes a heading (`[1]`); `[a\\]: /url` defines (`[1, 5]`); `[a\q]:
/url` defines, its backslash a literal one (`[1, 5]`); and so does a label holding a backslash
before its line end (`[1, 6]`). Controls, predicted and as predicted: L1, `LABEL` with no escapes,
red the first (`[1, 5]`); L2, escapes before punctuation alone and a lone backslash refused, red
the third and fourth (`[1]` each); L3, `LABEL` without `re.S`, red the fourth (`[1]`).

### Item 3: a title on the next line, as it is

`only_references` reads a title followed on its line by more as no part of its definition, which
then ends at its destination's line where that ends one (195), and refuses the paragraph on its
next pass: the spec's example 210, `[foo]: /url` then `"title" ok`, a definition and a paragraph.
Rows (`f9c34a28`), each passing at the base and after: a title alone on the next line (`[1, 6]`);
the same with ` ok` after it (`[1]`); and the two edge rows the brief's review asked for, each
under `===` and then a `2.` that holds a fence:

- `[ref]: /url`, `  "title"`, `[two]: /u2`. CommonMark (§4.7): a title may stand on the line after
  its destination, after spaces, as in the spec's example of `[bar]: /bar-url` with `"bar"`
  indented on the next line; two definitions and nothing left, so the `===` is paragraph text and
  the `## ` line under the `2.` counts: `[1, 7]`. The base: `[1, 7]`.
- `[ref]: /url "line1`, `line2" trailing`. A title may span lines, but no further character may
  occur after it, and the destination does not end its line, so nothing is defined; the underline
  makes a heading, and the `2.` after it opens an item whose fence holds the `## ` line: `[1]`.
  The base: `[1]`.

They agree, so there is no finding to report. No row tells the fallback from an outright refusal
of such a definition, and none can: after the fallback the next pass begins at the title's
opener, a quote, an apostrophe or a parenthesis, never a label's bracket, and refuses at once, as
the refusal does. Over every paragraph of one to four lines from 45 definition lines and
fragments, 4,193,820 of them, the fallback taken in 116,959, `only_references` with the fallback
and with it refused read none otherwise. Controls, predicted and as predicted: L4, the fallback
refused, every row green; L5, a title only on its destination's line, red the title alone on the
next line, the indented one before `[two]` and Lows 7's definition across three lines (`[1]`
each); L6, text after a title accepted, red the row with ` ok` and the title across two lines
(`[1, 6]` each).

### The comparison over random entries

As Lows 7's record describes it, again: its four pools, seeds, lengths and sizes (seed 5, one to
fourteen lines, the four pools; seed 6, one to twenty lines, the three with definitions or tabs;
1,000,000 entries a pool), each entry read by the parser that record compared against, at the same
version, as it ships and in its second reading, as the checker reads a `## ` line. Its pools were
checked equal to the earlier ones line for line. The variants: the base's checker, the change's
(`f9c34a28`'s), and the change with its title fallback refused. A reading and a pool were added
for item 1, since that parser holds a label to no length. The third reading is the second with
the text above an underline judged as CommonMark's paragraph holds it (each line from its first
character that is neither a space nor a tab) and a label held to 999 characters. The fifth pool is
the four pools' lines and 96 definitions whose labels hold 998 to 1,000 characters with a tab at
each of the four columns a stop can leave, alone and after `- `, `> `, two spaces, a tab, `>\t`,
`-\t` and `1. `.

| seed, pool | change ≠ base | base ≠ parser | change ≠ parser | base ≠ second | change ≠ second | base ≠ third | change ≠ third |
|---|---:|---:|---:|---:|---:|---:|---:|
| 5, plain | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| 5, definitions | 0 | 215 | 215 | 0 | 0 | 0 | 0 |
| 5, tabs | 0 | 1 | 1 | 1 | 1 | 1 | 1 |
| 5, both | 0 | 116 | 116 | 1 | 1 | 1 | 1 |
| 5, long labels | 0 | 203 | 203 | 0 | 0 | 0 | 0 |
| 6, definitions | 0 | 417 | 417 | 1 | 1 | 1 | 1 |
| 6, tabs | 0 | 2 | 2 | 2 | 2 | 2 | 2 |
| 6, both | 0 | 244 | 244 | 0 | 0 | 0 | 0 |
| 6, long labels | 3 | 393 | 390 | 7 | 4 | 3 | 0 |

The base's columns over Lows 7's pools are the change's columns of that record's table, count for
count, and the change reads none of those 7,000,000 entries otherwise than the base. The change
with its fallback refused reads none of the 9,000,000 otherwise than the change. The third
reading agrees with the second on all 7,000,000 and parts from it on 4 of the 2,000,000 entries
with long labels.

Random lines seldom build what a label's length decides (definitions alone, an underline, a `2.`
that holds a fence), so item 1 had a comparison of its own: 1,000,000 entries, seed 12, each the
heading, up to two lines from the pools, maybe a blank line, a container's marker (none, `- `,
`> `, `1. `, `-\t`, `>\t` or two spaces) and a definition whose label holds 996 to 1,001
characters with one or two tabs anywhere, maybe across two lines; maybe a second definition, long
or short, on a line that goes on in the container, lazily, or indented by a tab; an underline; and
tail lines, most of them a `2.` (or `0.`, `10.`, `2.\t`) that holds a fence, then a `## ` line. The
change reads 5,198 of them otherwise than the base. Against the third reading the base departs in
5,200 and the change in 4; against the second, 10,933 and 6,613; against the parser, 50,947 and
47,521. Of the 5,198:

- 3,981: the change reads them as all three readings do, and the base as none does.
- 778: as the second and third readings do, where the base read them as the parser does, which
  takes a definition as a block of its own; each of the 98 among the first 120,000 entries, and
  its 1-minimal witness, holds an empty `-` under the definitions, which the parser opens as an
  empty list item (that record's kind a).
- 331: as the parser and the third reading do, where the base read them as the second does,
  whose reparse of the text above the underline reads a definition on a line indented four
  columns or more as code.
- 107: as the third reading does, where the parser and the second read that indented line as
  code, the one as shipped and the other in its reparse.
- 1: as none of the three does, where the base read it as all three do (below).

By the rule's measure (`.claude/rules/build-ci-tests.md`: an entry main read right that the change
reads wrong), the change breaks 886 entries against the parser, 439 against the second reading
and 1 against the third, and each is one of those kinds: the parser's definitions and its empty
item (778), the reparse of an indented line as code (331, and 107 against both), and the one
entry. In the first three kinds a reading that agrees with the change reads definitions as the
spec's parsing strategy does where the other does not: the second and third differ from the
parser in how they read definitions alone, and the third from the second in a paragraph's text
and a label's limit alone; the limit decides none of these entries, whose long labels all stand
in the paragraph the change reads as definitions, each at most 999 characters as written. The one
entry is traced below.

### What remains

The change's departures, every one, against each reading:

- Against the parser as it ships, over Lows 7's pools: 992 entries the second and third readings
  read as the change does, the parser reading a definition as a block of its own from its first
  line (that record's account); and three of the nested block quotes below. Over the 2,000,000
  entries with long labels and the aimed ones: 589 and 42,461 more of that kind; 4 and 5,058 the
  third reading alone reads as the change does, a label of more than 999 characters, which neither
  the parser nor the second reading refuses, or a definition on a line indented four columns or
  more, which both read as code; and two of the aimed entries' four below.
- Against the second and third readings, over Lows 7's pools: the five entries that record lists,
  each a nested block quote whose paragraph meets a line indented four columns or more that would
  start a block (`> > text`, then `\t- item` or six spaces and three backticks, and the rest): the
  parser ends the quotes there, where CommonMark's lazy continuation, which the checker reads,
  goes on. Two of the five the parser as shipped reads as the checker does. Neither the change nor
  the base touches them.
- Against the second reading, over the entries with long labels and the aimed ones: 4 and 6,611,
  its labels over 999 characters and its reparse of a definition on an indented line as code; and
  two of the aimed entries' four below.
- Against the third, over the aimed entries, four entries of three kinds. One more nested block
  quote of that kind (`> > text`, then a space and a tab before `   ## x`, then `=`, then
  `` 0. ``` ``). Two of the third reading's own: it strips a continuation line indented four
  columns that holds a definition's destination, three backticks (`[ref]:`, then two spaces, a tab
  and the backticks), and its reparse then reads that line as a fence, where the parser, the
  second reading and the checker read it as the destination. And one where all three readings part
  from CommonMark: `[ref]:`, then `2.`, then, two spaces in, a definition whose label holds 999
  characters, two of them tabs, then `=`, `` 2. ``` `` and `   ## inside`. `2.` cannot interrupt a
  paragraph, so it is the first definition's destination, both definitions leave the underline no
  text, and the `## ` line counts (`[1, 7]`, the change's); the parser's scan for a definition's
  lines ends at any list marker, so the three readings take no definition there and read `[1]`.
  The base read `[1]` too, through its own count of the tabs. The entry's 1-minimal form, `[ref]:`,
  `2.`, `=`, `` 2. ``` ``, `   ## inside`, is read `[1, 6]` by the checker at the base and after
  it, and `[1]` by all three readings.

### A follow-up

K6: no row tells a tab that reaches its four-column stop from one read as a single column. Read
as one space, a tab after a list marker makes `` -\t``` `` an item two columns wide, so `   ## h`
under it sits in the item's fence: `` -\t``` `` then `   ## h` reads `[1, 3]` at stops of four, as
all three readings do, and `[1]` at one column, a row that would pin it. Left out of this bundle,
whose rows are its three items'.

### Runs

On `f9c34a28`, before this record: `python3 scripts/check-engineering-log.py --self-test` PASS.
The comparison's runs took 396 s, 466 s and 169 s at fourteen workers. The gate on the branch (the
self-test and `make docs-check` with this record staged) is in its report: a record holds no run
made on its own commit.
