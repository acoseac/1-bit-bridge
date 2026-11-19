## 2026-11-19 — Bridge lows 7: the log checker's read set, docstring and untested arm, B942's record as run, and reference definitions and tabs as CommonMark reads them

Branch `fix/bridge-lows7-1119`, from main `91197b67` (#1515's merge, with #1514's B939 and #1513's
B942 under it); every line the brief cites was read again there, and line numbers below are its.
Seven commits: `87b3addd`, item 1, the README; `d47bbc18`, item 2, `h2_lines`' docstring;
`410d06bf`, item 3, a self-test row; `02d2053c`, item 5, B942's record; `32d285d5`, addition 5,
reference definitions under an underline; `0e876643`, addition 6, tabs; and this record. Item 4
was withdrawn by the brief's review: B939's round-2 record names the parser it compared against,
with its version and results, which is evidence, and it stays as written.

### Item 1: what the check reads

The README's bullet on entries filed elsewhere said the check reads every untracked `.md` file
below a top-level `ops` folder, and elsewhere every one whose name holds `--`. `listing()`
(`scripts/check-engineering-log.py` 413-439) lists an untracked file only when its name ends in
`.md`, in any letter case, and does not begin with `.` (its `record()`, 425-426), and the check
reads every tracked path whatever its name. The bullet says so; the rest of it is as it was.

### Item 2: a lone `-` under a paragraph

`h2_lines`' docstring (218-220) gave an empty `-` after a paragraph's text as more of that text
and, in the same sentence, a lone run of `-` under a paragraph as its underline. `starts()` asks
`UNDERLINE` (191) before `LIST_ITEM` (195), so a lone `-` there is an underline, as the
self-test's row for an empty bullet expects. The example is an empty `+` or `*` now, and the
sentence says a lone `-` is read as an underline before it is read as an item. What the function
returns was described rightly and is unchanged.

### Item 3: the interrupt guard's empty-item arm

No row reached the guard's `empty or` term (200): a lone `-` under a paragraph is read as an
underline first, and the empty `2.` rows meet the ordered-number term as well. The new row puts
an empty `+` under a paragraph's text, then a fence two spaces in that holds a `## ` line, and
expects `[1]`. Its control, the term taken out and nothing else, predicted in writing before it
ran: red at `410d06bf`, that row alone, `got [1, 6], want [1]` (the `+` opens an empty item, the
fence opens in it, and the unindented `## sample` ends both and counts); the same at `0e876643`.

### Item 5: B942's record

`2026-11-19-b942--fix-b942-backup-windows-cap.md` kept sentences written before its CI
measurements ran, though its "The CI measurements" section had their numbers: a flush on
windows-latest "costs what the first measurement will show" (94-95), the rule line's commit "is
reverted with the fix's" on a miss (107-108), and Step 5's three steps as instructions (180-200).
The first now gives what windows-latest took, 589.2 s before the fix and 37.2 s after it, 28.8 and
1.8 ms a start over the 20,483 starts; the stop rule and the rule line's commit are told in the
past with their outcome; and each step says what was pushed and what it found: 53.7 % of the
Windows package, over the 25 % stop; a 91.6 % fall, past the 70 % bar; and the package at about
657 s normalised after the fix, under the 796 s bar. Every number is the record's own or computed
from it.

### Addition 5: reference definitions under an underline

What was wrong. `starts()` read a run of `=` or `-` under a paragraph as its underline whatever
the paragraph held. CommonMark takes the link reference definitions off a paragraph's front when
it meets an underline and, where nothing is left, makes no heading: in the spec's example of
`[foo]: /url`, `===` and `[foo]`, the `===` is paragraph text. The checker ended such a paragraph at the
underline, so a `2.` under it opened a list item whose fence hid the `## ` line CommonMark reads
there. This input gave `[1]`:

~~~
## 2026-11-18 — Example
[ref]: /url
===
2. ```
   ## stray
```
~~~

The brief has `[1, 6]` for it. `## stray` is that input's fifth line, and the parser B939's
round-2 record compared against gives `[1, 5]`, which the row expects.

As built. `h2_lines` keeps an open paragraph's lines, each from its first character that is not
a space, and `starts()` asks `only_references()` of them before it reads an underline. That parses
the definitions at the paragraph's front by CommonMark's grammar: a label of at most 999
characters, some not white space, up to its first unescaped `]`; a destination in `<>`, which
holds no line end, or a run with no white space or ASCII control character whose unescaped
parentheses balance (`destination_end`); an optional title in quotes, apostrophes or
parentheses; white space before each part that may hold one line end; and nothing after them on
the line but spaces and tabs, a title followed by more being dropped where its destination ends
a line. Where nothing is left past the definitions, the run is read as any other line under a
paragraph's text: a thematic break where it is one (`---`), more of the paragraph where it is not
(`===`, `--`, and a lone `-`, an empty item, which interrupts no paragraph).

Rows, red first. Four: the input above (`[1, 5]`) and a definition across three lines, its title
on the third (`[1, 7]`), each red before the change with `[1]` (the rows over the unchanged scan);
text past the definitions, whose underline still makes a heading, and a thematic break under
definitions alone, which stays one (`[1]` each), green before and after. Controls, each predicted
in writing first, each an exact-once mutation of the committed file with its bytes restored from
memory, each as predicted: C5a, the underline asking nothing of the paragraph, red the first two
rows with `[1]`; C5b, `only_references` true after its first definition, red the row of text past
the definitions with `[1, 6]`; C5c, an underline under definitions alone read as text, no
thematic break tried, red the break's row with `[1, 5]`; C5d, white space between the parts
holding no line end, red the three-line row with `[1]`. Every other row passed under each.

### Addition 6: tabs at four-column stops

What was wrong. `h2_lines` read every tab as four spaces wherever it stood
(`line.replace("\t", "    ")`, 234), so two spaces and a tab were six columns, where CommonMark's
tab stops make them four. Under `- item`, a line of two spaces, a tab and three backticks, then
`  ## sample` and a closing fence two spaces in, opens a fence two columns into the item that
holds the `## ` line (`[1]`); the checker read the fence's line as six columns in, four into the
item, so more of its paragraph, and counted `## sample` (`[1, 4]`).

As built. Each line is expanded to four-column stops (`str.expandtabs(4)`) before its containers
are measured; a line without a tab is read as before. `markdownShellLines`
(`scripts/third-party-licenses/tags.go` 240) still reads a tab as four spaces, which the docstring
now lists among its differences. Red first: the row over the unchanged scan, `got [1, 4], want
[1]`. Controls, predicted: C6a, main's replacement back, and C6b, stops of eight columns, each red
the row with `[1, 4]`, every other row green.

### The comparison over random entries

As `.claude/rules/build-ci-tests.md` asks of a change to how the scan reads containers or
paragraphs: over random entries, against a CommonMark parser, the one B939's round-2 record used
at the same version, read as the checker reads a `## ` line (a line counts where the parser opens
an h2 on it and it begins `##` after up to three spaces, its tabs expanded to four-column stops),
and against main's checker (`91197b67`'s file). Four variants: main; addition 5 alone
(`32d285d5`'s file); addition 6 alone (main with its tab line changed); and both (`0e876643`'s
file, byte for byte). Each entry is the heading and then lines drawn from one of four pools: 81
lines, four of them blank, with no `[`, no tab and no HTML (headings, fences, markers, quotes,
breaks, underlines, indented lines); those and 65 lines of reference definitions and their
parts; those 81 and 43 lines holding tabs; or all three. 1,000,000 entries a pool: seed 5, one to
fourteen lines, the four pools; seed 6, one to twenty lines, the three with definitions or tabs;
7,000,000 entries in all.

That parser reads a definition as a block of its own from its first line: its reference rule
takes the definition before any paragraph is opened, and any list marker ends it. In CommonMark's
parsing strategy a definition is its paragraph's text until the paragraph closes or meets an
underline, so a marker that cannot interrupt a paragraph is more of it there, and a lazy line
under one goes on in it. A second reading turns that one rule to CommonMark's: the same parser
with its reference rule off, so a definition is paragraph text while the blocks are read, and its
setext rule asking, at an underline, whether the same parser reads the text above it as nothing
but definitions (it parses that text alone to no token), the line then read as any other line.
The definitions are judged by the parser's own grammar there, not the checker's. No parser that
reads definitions CommonMark's way runs on the test host; the second reading emulates that
reading and stands in for no reference implementation.

| seed, pool | main ≠ parser | change ≠ parser | main ≠ second | change ≠ second | change ≠ main | main as parser, change not | main as second, change not |
|---|---:|---:|---:|---:|---:|---:|---:|
| 5, plain | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| 5, definitions | 217 | 215 | 4 | 0 | 4 | 1 | 0 |
| 5, tabs | 737 | 1 | 737 | 1 | 736 | 0 | 0 |
| 5, both | 335 | 116 | 238 | 1 | 237 | 9 | 0 |
| 6, definitions | 425 | 417 | 17 | 1 | 16 | 4 | 0 |
| 6, tabs | 1,221 | 2 | 1,221 | 2 | 1,219 | 0 | 0 |
| 6, both | 656 | 244 | 427 | 0 | 427 | 7 | 0 |

The change read 2,639 entries otherwise than main, every one as the second reading does and
main did not. Against the parser as it ships, 2,617 of them are entries main read otherwise and
the change reads as it does, one is read otherwise by both, differently, and 21 are entries main
read as the parser does and the change does not. Each of the 21 was cut to a 1-minimal witness
(no line can go and keep the outcome) and read: eleven hold a lone `-` under definitions alone,
which the parser opens as an empty list item and the change reads as more of the paragraph,
where main read an underline; nine a lazy line under a definition in a list item, after which the
parser has no paragraph to continue and ends the item, where the change continues the paragraph
and main read them as the parser does through its own errors there (a tab taken for four
spaces, an underline under definitions alone); and one a `0.` under a definition that opens a
paragraph after a list item's fence, more of that paragraph to the change and a new list to the
parser, which main read as an item only because it took a tab two lines above for four spaces.
The second reading agrees with the change on all 21. By the rule's measure against the parser as
it ships they are regressions; against the second reading, which differs from it in definitions
alone, there are none. Which of the two should judge such entries is the owner's to settle, and
the witnesses are listed here so it can be.

The 21, each as a Python string literal of its lines after the heading, with its kind (a, the
lone `-`; b, the lazy line; c, the `0.`), seed 5's first:

~~~
(a) '[ref\\]]: /u\n  -\n0. ```\n   ## h'
(b) '- [ref]: /url\n[ref]: /u\\(\n  \t```\n  ## h'
(a) '> [ref]: /url\n> -\n<1>\n2. ```\n   ## h'
(b) '1. [ref]: /url\n[ref]: /u(x)\n   \t```\n   ## h'
(a) '[ref]: <a b>\n  -\n2. ```\n   ## inside'
(b) '- [ref]:\n===  \n  -\n  \t```\n  ## h'
(b) '- [ref]: /url\n[a b]: /u\n  \t```\n   ## inside'
(b) "- [ref]: /url\ntle'\n  \t```\n   ## inside"
(a) '[ref]: /url\n- \n2. item\n   ```\n##'
(a) '[ref]: /u(x)\n  -\n2. ```\n   ## h'
(a) '[a\\[b]: /u\n-\n2. item\n   ```\n## '
(b) "1. [ref]: /url\n[ref]:/url\n   ===\ntle'\n   ```\n  ## h"
(a) '- [ref]: /url\n  -\n[ref]: /u)\n  ```\n"title\n   ## h'
(a) '[ref]: /url\n  -\n2. ```\n   ## h'
(a) '- [ref]: /url\n  -\n<>\n  ```\n## '
(b) '- [ref]: /url\n[ref]: /u\\(\n   \t```\n   ## h'
(a) '[ref]: /url "title"\n-\t\n-\t\n   ```\n## sub'
(a) '[ref]: /u\\(\n-\n2. ```\n   ## inside'
(b) '- [ref]: /url\n[ref]: <url>\n  \t```\n  ## h'
(b) "- [ref]: /url\n[ref]: /url '\n  \t```\n  ## h"
(c) '- item\n  \t```\n[ref]: <>\n0. ```\n   ## inside'
~~~

What the change still reads otherwise than the parser: entries where the parser's definitions
part from CommonMark's, which the change reads as the second reading does and main read as the
change does but for the 21 above and the one both read otherwise; and the five of the "change ≠
second" column, three of them in the "change ≠ parser" column too, where a nested block quote's
paragraph meets an indented line that would start a block (`    ## h`, a tab then `- item`): the
parser ends the quotes there, where CommonMark's lazy continuation, which the checker reads, goes
on in the paragraph. Main reads those five as the change does, and neither change touches them.

The five, as before, seed 5's first:

~~~
'> > text\n\t- item\ntext\n2.\n   ```\n##'
'> > text\n   \t```\n[ref]: /url "title"\n0. ```\n   ## h'
'> > text\n    - item\n[a\\[b]: /u\n- \n  ```\n10. ```\n   ## h'
'> > text\n      ```\n  text\n2. ```\n   ## h'
'  ```\n```\n> > text\n\t```\n``` a`b\n2. ```\n   ## h'
~~~

### Runs

On `0e876643`, before this record: `python3 scripts/check-engineering-log.py --self-test` PASS,
and the check itself ok over 9 entries. The gate on the branch (the self-test, `make
docs-check`, `make fmt vet` and the full gate) is in its report: a record holds no run made on its
own commit.
