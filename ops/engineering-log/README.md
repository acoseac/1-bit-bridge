# The engineering log, one file per entry

The record behind the rules in `CLAUDE.md` and `.claude/rules/*.md`: the measurement that
settled a rule, the alternative tried and rejected, the test that pins it, the PR. Not
auto-loaded.

- `../engineering-log.md` is the archive up to 2026-11-18, frozen: `make docs-check` holds it
  to the SHA-256 of its bytes (`scripts/check-engineering-log.py`), and the edit hook refuses
  any change. Never edit it. A correction to it is a new entry here that names its heading
  and line. Only a redaction the owner approves changes it: a placeholder of the same line
  count, the checker's two pins updated, and an entry saying why.
- Every record since is a file here, one entry each:
  `<YYYY-MM>/<YYYY-MM-DD>-<entry>--<branch>.md`
  - the date the entry is written, and its month;
  - `<entry>`: the backlog item (`b888`, `b728-b9-pr-3b`), or a topic for an entry with none
    (`ci-ubuntu-2604`); a later entry of the same PR adds a word (`b601-round-2`; pad to two
    digits, `round-02`, where a PR may pass nine rounds, so `ls` keeps them in order);
  - `<branch>`: your PR's branch, as its slug below, so two PRs whose slugs differ never pick
    one name; two branches can fold to one slug (`fix/foo` and `fix-foo`), and git then
    reports the second entry of that name as an add/add conflict, never an overwrite;
  - both lowercased, every other run of characters one `-` (`fix/b898-updater-held-binary`
    is `fix-b898-updater-held-binary`), the whole name at most 120 characters. A branch slug
    longer than 56 characters keeps its first 47, then `-` and the first 8 hex digits of the
    whole slug's SHA-256: `python3 scripts/check-engineering-log.py --slug <branch>` prints
    the slug, for a name and for a lookup alike;
  - its first line `## <YYYY-MM-DD> — <title>`, the date its name's; its sections `###`; no
    other `## ` line outside fenced code; LF line ends and a final newline.
- A new record is a new file: never append to a file another PR wrote. If the name is taken,
  add a word. A round of an open PR may be a file of its own or a `### Round N` section in its
  PR's own file.
- An entry is filed here alone. A file named like one in any other folder fails the check,
  tracked or not, and so does one git ignores by its own name; a folder git ignores whole, as
  `.tmp/` where scratch goes, is not looked in. The edit hook refuses such a file as it is
  written, but below a folder whose name begins with `.`.
- An entry is edited only to correct it. The citation guard (`TestEveryCitedTestNameExists`)
  reads every entry, never the archive, so a PR that renames or retires a test an entry cites
  corrects that citation in the entry.
- Cite an entry by its path, `ops/engineering-log/2026-11/2026-11-18-b888--feat-b888-log-per-entry.md`,
  with a line where one claim in a long entry needs it; an always-loaded file keeps the short
  form, "(2026-11-18, backlog B888)", which `ls ops/engineering-log/*/2026-11-18-b888*` finds.
  The archive is cited as before: `ops/engineering-log.md:<line>`, or by its date and item.
- In a merge, take main's archive (`git checkout origin/main -- ops/engineering-log.md`) and
  keep your record in its own file.

No index is kept: a committed one conflicts as the single log did. To find a record:

- everything about an item, in both halves: `git grep -n -i b653 -- 'ops/engineering-log*'`
- one day: `ls ops/engineering-log/2026-11/2026-11-18-*`
- one PR: `ls ops/engineering-log/*/*--<branch slug>.md`
- every title in order: `grep -h -m1 '^## ' ops/engineering-log/*/*.md`
- one stream, oldest first: `cat ops/engineering-log.md ops/engineering-log/*/*.md`
