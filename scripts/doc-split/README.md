# doc-split — keeping CLAUDE.md and the rules files small

`CLAUDE.md` loads on every request and each `.claude/rules/*.md` loads with every file its `paths:`
globs match, so every byte in them is paid again and again; a skill in `.claude/skills/` costs only
its description until it is invoked. On 2026-10-18 `CLAUDE.md` went from 941,500 bytes, 878 KB of
it one section ("Things that have bitten before"), to a core of about 34 KB: the topic entries
moved into twelve path-scoped rules files, seven procedure sections into six skills, and every long
entry that stayed, or that sat in a rules file over its budget, into an `ops/History-*.md` file
(`restructure-2026-10-18/`). Nothing was deleted: every original block moved byte for byte, and the
scripts here prove it.

These scripts are a port of the iOS repo's (`acoseac/1-bit`, `scripts/doc-split/`). The bridge's
differences: the History files live in **`ops/`**, not `docs/`, because `docs/` here is the public
GitHub Pages site and the long forms are internal working material (`ops/README.md`); there is no
test-count checker, so the claim checks are stubbed (`mdblocks.load_claim_regex` returns a pattern
that matches nothing); the citations checked live in Go, JavaScript, shell, PowerShell, the
`Makefile`, `.github/**` and the docs; and an override in `overrides/<name>.json` is keyed by the
start of its block's text, so it still applies when a rules file is regenerated on a newer base.

Day to day, edits are made by hand, following the layout below, and two tools keep the files
from growing back: `scripts/check-doc-budgets.py` fails `make docs-check` and CI's `docs-check`
job (and tells Claude right after an edit) when a budget is exceeded, and `move_entry.py` makes
room without losing anything. The condensing scripts are for a whole file that has grown; they run
once per file.

## The layout

- **Where an entry goes:** a rule every session needs goes in `CLAUDE.md`; a rule about particular
  code goes in the rules file whose `paths:` globs match that code (its "Where things live" table
  lists them; add a glob when none matches); a procedure goes in a skill. A rules file loads only
  when Claude **Reads** a matching file, so a rule for code nobody Reads first belongs in `CLAUDE.md`.
- **Always-loaded file** (`CLAUDE.md`, or a condensed rules file): one line per entry, made of its
  bold title (which states the rule), its own bolded "Don't…/Never…" sentences, and `→ History-X.md`.
  Entries of 450 bytes or less may stay whole.
- **Long form** (reasoning, measurements, rejected alternatives, incident history):
  `ops/History-*.md` for `CLAUDE.md` entries, `ops/History-rules-<name>.md` for a condensed rules
  file. **Change an entry's line and its long form in the same commit.** The dated evidence still
  goes to `ops/engineering-log.md`; a History file is the entry's own text, grouped by topic.
- **Procedures** (the local fixture, a deploy, release day, a consult, a named review cycle, a
  multi-PR batch): a skill, `.claude/skills/<name>/SKILL.md`. Its `description` is all Claude sees
  at startup, so it must say when to use the skill; a rule that has to hold even when the skill is
  not loaded (one SSH connection per host, say) also keeps a line in `CLAUDE.md`.
- **Keep `ops/` flat for History files** (`ops/History-*.md`): the tools find them there.
- Agents other than Claude Code do not load `.claude/rules/`; `AGENTS.md` points them at
  `scripts/rules-for.py <path>`.

## Tools

| Script | What it does |
|---|---|
| `mdblocks.py` | The block parser, the one-line condenser, and the argument checks every script shares. |
| `verify_moves.py` | Lossless proof for any restructure: compared as a whole with a base commit, `CLAUDE.md`, `AGENTS.md`, the rules, the skills, `docs/*.md` and `ops/*.md` (the engineering log excepted) still hold every block (relative links compared by the file they reach) and heading, and every cited phrase still resolves; declared edits print as diffs, and new text is listed for review. |
| `measure.py` | Bytes per heading and a block-size histogram for any markdown file (sizes and first-line labels only). |
| `move_entry.py` | Moves ONE entry out of `CLAUDE.md` or a rules file, verbatim, into a History file it creates or adds to (see below). |
| `../check-doc-budgets.py` | The budgets (see below); run by `make docs-check`, CI's `docs-check` job and the PostToolUse hook in `.claude/settings.json`. |
| `../rules-for.py` | Prints the rules files that govern a path (`--changed`: every file this branch changes). |
| `condense_rules.py` | Condenses `.claude/rules/<name>.md` from a base commit, applying `overrides/<name>.json`. |
| `validate_overrides.py` | Vets hand-polished lines before they land (see below). |
| `verify_rules.py` | Lossless proof of a condensation: every original block, heading and cited phrase survives (see below). |
| `restructure-2026-10-18/` | The restructure itself: `split.py` (with `routes.py`, `prose.py`, `core_overrides.json`) rebuilds the core, the rules files, the skills and the History files from a base commit, with `--routes`, `--coverage` and `--edits` reports; `citations.py` resolves every `CLAUDE.md` citation in the code. |

Every script checks its arguments before touching git or the filesystem, because Claude
sessions run them too: `NAME` must be one of the files in `.claude/rules`, a revision can never
be read as a git option, and `--out`, `--work` and `--tsv` must point outside the repository at
a new directory or one holding only an earlier run's output (recognised by the `.doc-split-stage`
file these scripts leave in it). That last check is what stops a mistyped `--out ~` from
overwriting `~/.claude/rules`, which Claude Code loads in every session. No script writes through
a symlink. Every file except `measure.py`'s TSV reports is written by `mdblocks.write_text`, which
replaces it whole and syncs it and its directory to disk before returning, so no later write can
reach the disk first. `split.py --out` clears its earlier output before writing, so nothing stale
survives a run.

## Budgets, and making room without losing anything

| Budget | What |
|---|---|
| 64 KB | `CLAUDE.md` (loads with every request) |
| 100 KB | each `.claude/rules/*.md` (loads with every file its `paths:` match; a rules file without `paths:` loads everywhere and is refused) |
| 480 KB | `CLAUDE.md` plus every rules file one tracked file pulls in, for the worst such file |
| — | a skill: no byte budget, since only its description loads at startup, but one whose `name:` differs from its folder or that has no `description:` is refused |

Over budget, move an entry instead of cutting it. In `CLAUDE.md`, a topic entry belongs in the
rules file that loads with its code: move the line there by hand, under the section it came from,
rebasing any relative link (`mdblocks.rebase_links`), and prove the move with
`verify_moves.py --base <rev>`. Otherwise:

1. `scripts/doc-split/move_entry.py CLAUDE.md --largest` lists the largest entries that can move,
   where each would go, and the command for each.
2. `scripts/doc-split/move_entry.py CLAUDE.md --line N --dry-run` shows the one-line pointer it
   would leave; drop `--dry-run` to move. The entry lands verbatim, under a heading naming its
   section, in the History file its neighbours already point to (a rules file's goes to
   `ops/History-rules-<name>.md`, created if missing). `--to ops/History-X.md` adds it to a
   file you choose; `--new ops/History-Y.md --title "…"` creates one.
3. `--text "…"` replaces the generated pointer line with a better one; it is checked like a
   polished line (same pointer, every "Don't" kept, no invented symbols).

The History file is written and read back before the source changes, so an interruption can
leave an entry in both files but never in neither. A condensed file is never re-condensed:
`condense_rules.py` refuses to overwrite one that differs, because that would drop what was added
or moved since.

## Condensing another rules file (for example `dlna-upnp.md`)

1. Measure: `python3 scripts/doc-split/measure.py .claude/rules/dlna-upnp.md --headings`.
2. Commit anything pending, then note the base: `BASE=$(git rev-parse --short HEAD)`. The base must
   hold the uncondensed file; the script refuses one that is already condensed.
3. Draft: `scripts/doc-split/condense_rules.py --base $BASE --out /tmp/stage dlna-upnp`, then read a
   few lines of `/tmp/stage/.claude/rules/dlna-upnp.md`.
4. Polish the first-sentence lines (entries without a bold title; their first sentence is often
   provenance, not the rule): `--work /tmp/work dlna-upnp` writes a list with each entry's current
   line, its prefix key, the suffix to keep, its "Don't" sentences and its full text. Write
   replacements into `overrides/dlna-upnp.json`, keyed by the start of the entry's text (or its line
   number in `$BASE`'s file), and run `scripts/doc-split/validate_overrides.py --base $BASE dlna-upnp`
   until it reports 0 rejected.
5. Generate in place: `scripts/doc-split/condense_rules.py --base $BASE dlna-upnp`.
6. Verify: `scripts/doc-split/verify_rules.py --base $BASE dlna-upnp` and `make docs-check`.
7. Commit (docs-only), and add the file and its base to the table below.

What `validate_overrides.py` rejects: a changed bullet/paragraph shape or pointer suffix; a dropped
"Don't" sentence; a `code span` (single or double backticks) or `#number` that isn't in the
entry's original text as a whole identifier (the guard against invented or truncated symbols:
`cached` does not pass for `cachedThumbnail`); unbalanced markdown; more than 600 bytes.

What `verify_rules.py` requires: every original block is a whole block of the rules file or its
long form, byte for byte and as many times as the original has it (a block that grew by one word
fails); the headings are the original's, in order; the frontmatter survives; and every phrase
another file cites in the rules file (`rules/<name>.md` § PHRASE or ("PHRASE"), from the docs, the
rules, `scripts/` and the Go, JavaScript, shell and workflow sources) still resolves if it resolved
before. Citations that were already stale before the condensation are listed, not failed.

## Reproducing what is committed

| Files | Base | Command |
|---|---|---|
| `CLAUDE.md` as the core, 12 rules files (uncondensed), 6 skills, `ops/History-{Build,Cross-Repo,Docs,Workflow,Batches}.md`, and the corrections in `AGENTS.md`, `ops/README.md`, `ops/release-playbook.md` and `docs/LoupeReviewCycle.md` (restructure step 1) | `6f816e49` | `python3 scripts/doc-split/restructure-2026-10-18/split.py --out /tmp/x`, and in the repo `verify_moves.py --base 6f816e49` with the arguments `split.py --edits` prints |

On a newer `main`, `split.py --base origin/main` regenerates step 1 from that commit: every prefix
and title in `routes.py` and `prose.py` must still match exactly one block or heading, so a moved
or reworded entry stops the run with its name instead of being routed by accident. Commit its
output, then condense from that commit as in the steps above, with the same overrides files.

A reproduction matches only until someone edits the files by hand, which is expected.

## What the automation gets wrong (learned the hard way)

- Rules files wrap prose at about 80 columns, so a bold title can span lines; titles are matched on
  whitespace-collapsed text.
- An automatic "(PR #…)" suffix attached PR numbers an entry only *cites*; only the title's own
  parenthetical is kept now.
- A lowercase bolded "**don't …**" mid-sentence is a separate rule, so the pattern matches both cases.
- About 20% of long entries have no bold title; their first sentence is often a field report or a
  test list, which is why the polish step exists. A bold title that does not start with "Don't" or
  "Never" carries no rule into the line either (`**Copy the file; never rewrite it on one side.**`):
  polish those too.
- Verify against the base commit, never `HEAD`: after a condensation `HEAD` holds the condensed file.
- A verifier that counts substrings passes a block that grew by a word and a heading that gained
  a suffix (the first version did, shown by planting both); compare whole parsed blocks.
- To test the citation check on its own, plant a phrase that spans a moved block and the kept
  block after it. Any other phrase is either inside a block, where the block check catches it
  first, or at the start of a moved entry, which its one-line form repeats.
- A paragraph ends at a numbered-list line (`1. `) as well as at `- `.
- `.gitignore` keeps `ops/*audit*.md`, `ops/*review*.md` and `ops/*findings*.md` untracked, so a
  History file named `History-Reviews.md` was never tracked while every check that read the disk
  passed. `verify_moves.py` reads only the files git tracks or would track, so an ignored long form
  now fails the proof; name History files away from those patterns.
- A code comment quotes across its own line breaks (`// CLAUDE.md, "POST` / `// /api/restart …"`), so
  the citation scan joins a comment's continuation lines before it reads a quote.
