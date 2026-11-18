## 2026-11-18 — B888: the engineering log as one file per entry, the old file frozen

Branch `feat/b888-log-per-entry`, from main `156d180b`. The design round's document (SHA-256
`b494ff55…`, written at `54fd3749`) was re-verified at that main: every site it names held,
the line numbers of `cli-serve.md` and `build-ci-tests.md` moved by the two merges since, and
the archive had grown to 7,148,762 bytes and 106,268 lines, with no carriage return.

### Why

Every PR appended its record to `ops/engineering-log.md` (106 commits since 11-01), so each
merge left every open PR conflicting at the file's end, and each then needed main merged in
and a full CI run before it could merge. Two PRs that each add a file of their own never
conflict, so every record since this entry is a file of its own, and the archive is frozen.

### What changed

- **The layout.** `ops/engineering-log/<YYYY-MM>/<YYYY-MM-DD>-<entry>--<branch>.md`, one
  `##` entry each; `ops/engineering-log/README.md` states the rule and the lookups. No index.
  A branch slug longer than 56 characters keeps its first 47, then `-` and 8 hex digits of
  the whole slug's SHA-256 (`scripts/check-engineering-log.py --slug <branch>`), so the name
  stays within 120 characters and two long branches that begin alike stay apart; the design's
  "a long branch shortens the entry, never the branch" gave way to that bounded shortening.
- **The freeze.** `scripts/check-engineering-log.py` pins the archive's size and SHA-256
  (`FROZEN_BYTES`, `FROZEN_SHA256`; the branch's last commit appends the pointer line and sets
  them, and the check's ok line prints both). The archive is read through `lstat` for every
  component, opened with `O_NOFOLLOW` where the OS has it, and hashed as bytes, so a link, a
  folder, a missing file and a newline translated by a text read all fail. `.gitattributes`
  pins both halves to LF.
- **The names and contents.** Every tracked file below the folder is the README or an entry
  by name (the month folder of a real date, lowercase slugs, one `--`, the length bounds) and
  by content (a regular file, UTF-8, LF, a final newline, its first line `## <its date> —
  <title>`, no other `## ` line outside fenced code). The fences follow `markdownShellLines`
  in `scripts/third-party-licenses/tags.go`: block quote markers and up to three spaces come
  off, three or more backticks or tildes open (a backtick fence's info holds no backtick), a
  closer is the same character, at least as many, then only white space, and a fence opened
  in a block quote ends where the quote ends. List items are not containers there.
- **The listing.** `git ls-files -z --cached` repository-wide, so `Ops/…` and `OPS/…` are seen;
  `git ls-files -z --others` below every top-level folder named like `ops` in any case,
  ignored files included, judged by name alone (what an ignored file holds is not read), and
  only a name that could be a record's: a `.md` not beginning with `.`, so an editor's lock,
  `.DS_Store` and a backup's `~` fail no local run (committed, they are tracked and refused). A
  misfiled entry is the log's path in another case, a dated Markdown file directly in `ops/`
  (where `ops/*review*.md`, `*audit*` and `*findings*` are ignored, so such an entry could
  vanish from a commit), or a file named like an entry in any other folder. Two of the log's
  paths that a case-folding volume would take for one file fail. The check fails closed when
  git cannot list the tree, or lists nothing below the folder.
- **The hook.** `check-doc-budgets.py --hook` decides the log's paths on the path as the editor
  named it (`repo_path`: `normpath`, no link inside the repository resolved; one above it, as
  macOS's `/var`, is found by the folder that resolves to the repository), in any case, and
  hands them to `hook_for`, which refuses a link wherever it stands on the path and lets a
  deleted entry be. `.claude/settings.json` is unchanged.
- **The readers.** `TestEveryCitedTestNameExists` takes the archive as history by its exact
  path (`isFrozenArchive`): the walk never opens it and the collector drops it if handed one.
  At the freeze it cited 4,715 distinct test names, 332 of them in no other file the guard
  reads (the design's 1,652 counted the docs alone, without the Go sources). The build-site
  census exempts every file below `ops/engineering-log/` by its path, spelled with `/`.
  `verify_rules.py` reads the entries as citing sources, with a self-test of its own.
- **The instructions.** CLAUDE.md's Agent-memory table states the rule once; its other four
  pointers, AGENTS.md (its `make docs-check` row naming the checker), the release playbook's
  fix-agent brief (its conflict line kept both sides of the log in date order, which would
  break the freeze), the twelve rules files' record sentence, `build-ci-tests.md`'s census and
  citation-guard lines with their long forms, the production-deploy skill, `ops/README.md`,
  the LOUPE cycle, the doc-split README and `verify_moves.py`'s docstring follow it. Every
  rules file stays within its budget: `cli-serve.md` 99,453 bytes, CLAUDE.md 89.1%.

### Reds first

- `TestScanTestCitationsTakesTheFrozenArchiveAsHistory`, before the exemption: the scan read
  the archive, reported its ghost, counted four `.md` citations for three, and the collector
  took the archive's name.
- `TestMarkdownRecordsAreNotBuildSites` and `TestCensusExcludedTreesAndRecords`, before the
  census's clause: an entry's and the README's fences entered the census ("unreviewed
  build/test site"), and an entry spelled with the OS's separator was a build site.
- The checker in a scratch clone of the branch, each planted alone: a misfiled entry directly
  in `ops/` that git ignores (found though ignored, and refused by the hook); one name in
  `ops/` and `Ops/` (misfiled, and one file where case folds); the archive with one byte
  changed at the same size and line count, two lines swapped, a line moved (same size and
  count), one byte cut, one line cut, a line added, CRLF ends, each LF a lone CR at the same
  size, a link to an identical copy and the file gone, each refused with how it moved; an
  entry that is a link, a link to nothing and an entry below a folder that is a link, each
  refused by the hook, and a deleted entry let be; a branch part of 57 characters refused,
  and the same branch as `--slug` shortens it accepted. Two branches that add one entry name
  with different bytes conflict (`git merge-tree`: "CONFLICT (add/add)").

### Controls

Each ran on the committed scripts, mutated in memory or through `go test -overlay`, so no
tracked file was edited and nothing needed restoring; each went red on the rows named.

- The freeze: comparing the size alone (the same-size rows), the line count alone (the
  same-count rows and the grew/shrank messages), reading in text mode (each LF a lone CR),
  following links (the link rows).
- The names: no month backreference, a capital letter allowed, a single `-` accepted, no
  shortening of a long branch slug.
- The fences: a closer with text after it, a fence outliving its block quote, a backtick info
  holding a backtick opening a fence.
- The listing: a literal `ops/` pathspec for the tracked paths, the literal `ops` alone for
  the untracked scan, ignored files left out, untracked entries judged by content, every
  untracked name a candidate (a lock's and a backup's too), a file that is no Markdown read as
  an entry, no case-fold check, an empty listing passing.
- The hook: no dispatch, a dispatch on the resolved path, a dispatch that heeds case, a path
  `stat` cannot follow taken for a delete, only the last component asked for a link.
- `verify_rules.py`: no entries' glob, an unanchored recursive one.
- The citation guard: no exemption, an exemption by basename (the look-alike missed), by the
  prefix `ops/engineering-log` (the entry missed), the walk's guard alone removed, the
  collector's alone removed.
- The census: no clause for the folder, the prefix without its slash (the sibling
  `ops/engineering-log-notes.md`), any folder of the name (`docs/engineering-log/…`). Its
  `filepath.ToSlash` spelling stayed green here, as predicted: `ToSlash` is a no-op on POSIX,
  so only the Windows leg can show it red.

### Costs

The edit hook, mean of 10 runs: an unrelated path 27.3 ms against main's 26.5 (20 runs each,
alternated; the dispatch's imports are loaded only when it dispatches), the archive 43.8 ms,
an entry 35.9 ms, CLAUDE.md 157.2 ms as before.

### Not checked from this host

Links into the archive from the iOS repository, backlog prompts that tell an agent to append
to the log, and tools outside this repository that read `ops/engineering-log.md`: each needs
the folder too, and the pathspec `'ops/engineering-log*'` covers both halves. The census's
Windows spelling, as above. The gate run on the branch's last commit is in the branch's
report: a record cannot hold the runs made on its own commit.
