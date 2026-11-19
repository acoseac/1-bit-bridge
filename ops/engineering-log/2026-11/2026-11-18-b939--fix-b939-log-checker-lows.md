## 2026-11-18 — B939: the per-entry log's checker and README, the review Lows

Branch `fix/b939-log-checker-lows`, from `e51c2b2b` (main with #1511, so with B888, #1509).
Thirteen commits: 6b60857e (item 1), eb02ed80 (2), a159a355 (3), f600cd81 (3b,
the reviews' untracked half), c509466e (4), 0be8e08f (5), a4754327 (6), 46cefa16 (7),
4b941248 (8), cb27f195 (9), 334ad104 (10), 28967714 (the search commands, the reviews'
item), e1b0fd17 (three wordings a read-through caught); then this record. No Go code changes.
The files: `scripts/check-engineering-log.py` and its self-test, `scripts/check-doc-budgets.py`
(the hook's dispatcher and its self-test, which the binding reviews put in the budget),
`ops/engineering-log/README.md`, CLAUDE.md's and AGENTS.md's lines that repeat it,
`.github/workflows/gate.yml`'s docs-check job, and the release playbook's fix-agent brief,
which repeats two of the instructions corrected here.

Every red-first run and control ran on copies of the committed scripts under the scratch
folder, a mutation applied in memory to each, the tracked tree never written; each prediction
was written down before its run, and the two that missed are said where they missed.

### 1. Names stay apart where the slugs differ

The README and the docstring said two PRs never pick one name. Two branch names fold to one
slug, `fix/foo` and `fix-foo` both `fix-foo` (a self-test row pins it). In a scratch
repository, `fix/foo` and `fix-foo` each added `2026-11/2026-11-20-b1--fix-foo.md` with other
bytes; with `fix/foo` merged, `git merge-tree --write-tree main fix-foo` exited 1 with
"CONFLICT (add/add)" and main kept the first entry. Both now say that differing slugs keep the
names apart; "if the name is taken, add a word" stays. CLAUDE.md's row did not repeat the
claim.

### 2. The README is required

`check()` wanted git to list something below the folder, so a branch that deleted the README
and kept an entry passed. It now refuses a tree whose README git does not track, beside the
empty-listing check, whose wording stays. Rows: a checkout with the archive and a tracked
entry and no README (one problem, that one); the README put back on disk and still out of the
index (the same). Red first: the check taken out, both rows failed.

### 3. A name like an entry in another folder

`classify()` refused one by `ENTRY_SHAPED`, but every row sat in `ops/`, where `DATED_IN_OPS`
refused the same names. A checkout tracks `docs/2026-11-20-b888--feat-x.md` and expects its
one misfiled problem: green at the base, as the clause was there; with the clause taken out
(at a159a355) that row alone failed.

The untracked half (3b): `listing()` asked git for untracked files below a top-level folder
named like `ops` alone, so an untracked `docs/…--feat-x.md` was never seen. It now also asks
`git ls-files --others` for the pathspec `:(glob,icase)**/*--*.md`, once excluding what git
ignores and once for what it ignores with `--directory`, which prints a folder ignored whole
as itself and does not enter it: an ignored name is found where git ignores it by its own
name, and a draft in `.tmp/` is not. The edit hook's dispatcher (`LOG_PATH`) hands `hook_for`
a name like an entry in any folder but one whose name begins with `.`. Red first and
controls: the two queries out, the ignored one out, and the ignored one without
`--directory` (the draft in an ignored folder refused) each failed the untracked row; the
dispatcher without its arm failed the hook's `docs/` and top rows; the arm without its dot
rule failed the row that edits a draft below `.tmp/` under an open that raises (the hook
dispatched and read it). Cost, the real check's mean of 20 runs each, alternated, in this
checkout: 46.7 ms before, 59.2 ms after; with 40,000 files, 4,000 of them named like entries,
in an ignored folder of the scratch, 45.6 and 57.8 ms: git does not enter it.

### 4. A private dated note in `ops/`

`classify()` called every dated Markdown file directly in `ops/` misfiled, and its three
callers refused it, so an ignored private note there (`ops/*audit*.md`) failed the local check
and the hook while CI, which never has one, passed. It now returns `dated` for one not named
like an entry, and each caller keeps its policy: the tracked pass refuses it, the untracked
pass and `hook_for` let it be; one named like an entry is refused everywhere, and the case-fold
arm stays. The dispatcher's dated arm went: it took nothing the entry-shaped arm does not but
the notes `hook_for` now lets be. The fixture's ignore file gains `ops/*audit*.md`. Red first:
`classify()` as it was failed the three dated-note rows; with the dispatcher's arm back too,
the hook entry point's row; the arm back alone stayed green, `hook_for`'s own policy being the
second layer. Controls: the tracked pass letting a note be, the untracked pass not skipping
one and `hook_for` not letting one be each failed their rows.

### 5. An untracked entry is read

Chosen over the README's alternative: git add commits a file as it stands, so a legal-named
symlink, CRLF ends or a body with no heading were refused only once staged. The untracked pass
now hands an entry, and the README, that git does not ignore to `file_problems`; an ignored one
keeps its message and is not read. The good checkout's CRLF entry, "judged by its name alone",
is a good entry now. Rows: CR, a body with no heading, an ignored entry with CR that gets the
ignore message alone, and a symlink. Red first: names alone again failed four rows. Controls:
an ignored entry read before its ignore is asked failed three rows, not the four predicted (the
good checkout's count held: the ignored entry's one message was replaced by one heading
problem); every untracked kind read failed the untracked-archive row, as the first draft of
this change did.

### 6. A fence in a list item ends with the item

`h2_lines` took block quotes as the only containers: a fence two spaces under `- item` opened
as a top-level fence and hid the `## ` line that ends the item, and a `- ```` opener was not
seen, so its indented closer opened a fence that hid the next one. List items are containers
now, as `markdownShellLines`' are, and a fence ends where a container it opened in ends. Where
it departs from that scanner, to agree with CommonMark: code in a fence opens no container,
more than three spaces make code, and a thematic break opens no item. What counts as a `## `
line is unchanged, so a quoted one and one in a real fence stay allowed. Twelve rows name the
`## ` lines of an entry with list items. Red first: the old scan failed seven of them (the two
fixtures that passed and must fail, and the controls inside and after an item's fence).
Controls: a fence's closer looked for whatever containers a line goes on in failed five rows,
the Go scanner's reading inside a fence one, no thematic-break check one, a blank line ending
an item one.

Against markdown-it-py 3.0.0's CommonMark (its commonmark preset), a line counting where it
opens an h2 and the line begins `## `: the 21 hand rows agree. On 600,000 random entries (seeds
1 to 6, one to nine lines from an alphabet of 65 list, quote, fence, heading, break and text
lines, no tabs) the old scan disagreed on 6,188 and this one on 69. Each of the 69 agreed once
one rule's trigger was taken out of it: 16 an empty item after paragraph text, 33 an ordered
item not starting at 1 after paragraph text, 1 an empty item ended by a blank line, 19 a lazy
continuation line; none was other. The docstring names those four rules as not followed,
the Go scanner following none either. The thematic-break check came from this run: before it,
seeds 4 to 6 gave 169 disagreements, and the check took 134 of them away.

### 7. A read that fails once open, and a junction

`read_bytes` caught an `OSError` at the open alone; the fstat and the read raised through
the check and the hook. One handler now holds the fstat and an `os.read` loop, the descriptor
closed in a finally. Rows fail the fstat and then the read of the archive and of an entry, for
that file alone (`os.path.samestat`), through `check()` in a checkout and through `hook_for`
(exit 2, "(unread)" or "could not be read"). Red first: the old `read_bytes` failed all eight
rows, the fstat ones by the error escaping and the read ones because `f.read()` reads past an
`os.read` that fails; with the handler taken out of the new one, all eight by the escape.

`path_state` refused a component only where `S_ISLNK` held, and Windows' lstat reports a
junction as a folder. A reparse tag with the name-surrogate bit (`0x20000000`: junctions,
symbolic links) is now a link; a cloud file's tag is not. A stand-in stat for `ops/` carries
each tag, everywhere; controls: no tag check failed the junction row, any tag the cloud row.
A second row makes a real junction with `_winapi.CreateJunction` on Windows alone; run here with
its guard forced and the call standing in as a directory symlink, it made its junction and
passed, which shows its steps and not what Windows reports.

### 8. The merge, and the checker's own advice

The README, CLAUDE.md's row, the release playbook's brief and `RESTORE`, which every archive
failure prints, said to take main's archive in a merge, unconditionally. Each now says to move a
record the branch added to it into an entry file named by its heading's date first, and to keep
an approved redaction's bytes with `FROZEN_BYTES` and `FROZEN_SHA256`. A row holds the message
to that order; red first, `RESTORE` as it was failed it.

### 9. CI's Python, and the floor

The job ran `ubuntu-latest`'s python3. It now runs on `ubuntu-24.04`, a setup step puts that
image's own python3.12 first on PATH, and the next asserts 3.12 before make. No setup action:
the repository commit-pins every action, and the image's interpreter needs none. The checker
states its floor, 3.8: its calls are 3.8's by reading, `ast.parse`'s `feature_version` and
Windows' `st_reparse_tag` among them, and a row parses the file as `ast` does for 3.8. A match
statement planted in `slug()` failed it; a parenthesized `with` item, predicted to, passed:
CPython does not mark it by version, so the docstring says the row refuses most later syntax.
The job's steps run here as the runner runs them (`bash -e`, `GITHUB_PATH` applied between
steps): as written, the setup step failed (no python3.12 here); with 3.14 standing in, the job
passed; with the shim on 3.14 and the 3.12 assertion kept, it failed before make. The build-site
census and the tags check ran green over the edited workflow.

### 10. The hook reports an archive edit

Run as the PostToolUse command runs it, on a fixture archive the pins do not match, the hook
exited 2 with its diagnostic and the archive kept the edited bytes; its path only reads. The
README and CLAUDE.md say it reports an edit and leaves it, and that the check fails until the
archive's bytes and the pins agree again.

### The search commands

AGENTS.md, CLAUDE.md twice, the README and the release playbook's brief gave `git grep` over
`'ops/engineering-log*'`. In a fixture checkout with an entry written and not added, it found
the archive's record alone; `grep -rn -i b653 ops/engineering-log.md ops/engineering-log/` found
both. Each site gives the latter now. The citation guard (`TestEveryCitedTestNameExists`) ran
green over the edited docs.

### On the final tree

Every control ran again on the branch's last code commit. Item 3's, whose text item 4 rewrote,
was restated (`classify()` without `ENTRY_SHAPED`): it failed seven rows of the checker's
self-test and three of the hook's, as predicted; the dispatcher without its entry-shaped arm
now fails the hook's `ops/` row too, since that arm carries `ops/` names since item 4.

### Residuals

- CI runs the floor's grammar check on 3.12, never Python 3.8: no hosted Ubuntu image's own
  python3 is that old, and a setup action needs a commit pin this host cannot resolve.
- The junction refusal is shown off Windows by a stand-in stat; the real-junction row runs only
  where the self-test runs on Windows, and CI's docs-check job runs on Linux.
- `h2_lines` follows no paragraph, so four list rules of CommonMark are not followed (69 of
  600,000 random entries), as above.
- The hook cannot ask git what it ignores: a name like an entry below a folder git ignores whole
  but whose name begins with no `.` (`dist/`) is reported by the hook and let be by the check.
- A tab is still four spaces, as before, not a tab stop.

### Not checked from this host

GitHub's runner itself (the job's steps ran here with 3.14 standing in); a Windows run of the
self-test. The gate run on this record's commit is in the branch's report: a record cannot
hold the runs made on its own commit.
