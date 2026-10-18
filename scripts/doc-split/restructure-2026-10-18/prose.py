"""The new text of the 2026-10-18 restructure, and the sentences elsewhere it corrects (data only).

Everything here is either connecting prose (the organisation notes, the "Where things live" and
skills tables, the skills' frontmatter, one note per rules file and History file), a line that
holds a rule CLAUDE.md keeps while its text moves into a skill (DEPLOY_LINES), a review record's
one line (REVIEW_LINES, checked like a polished line), or a declared edit of a sentence the split
makes false (CORE_EDITS, EDITS; verify_moves.py prints each as a diff).
"""
from routes import DATE

# ---- CLAUDE.md ----------------------------------------------------------------------------------

ORG_NOTE = (
    "**How this file is organised (since %s).** It loads on every request, so it holds only what every "
    "session needs: build and test rules, the cross-repo contract with the iOS app, project-wide workflow "
    "rules, and where everything else lives. The invariants of particular code live in `.claude/rules/*.md`, "
    "which Claude Code loads when a file their `paths:` globs match is **Read** (a Bash `cat` or `grep` does "
    "not load them, so Read a file before changing it); procedures live in skills. A long entry is one line: "
    "its bold title plus its own bolded \"Don't…/Never…\" sentences, ending `→ History-X.md`, which is "
    "`ops/History-X.md` (for a condensed rules file, `ops/History-rules-<name>.md`), where the entry sits "
    "verbatim under a heading naming its section. **Those files are still binding.** When you change an "
    "entry, change its line and its long form in the same commit; new long-form detail goes to the History "
    "file, never back into this one. The record behind a rule still goes to `ops/engineering-log.md`. To "
    "resolve a citation such as \"CLAUDE.md, the X bullet\" or \"under **Scanner**\", grep this file, "
    "`.claude/rules/*.md` and `ops/History-*.md` for the title, the symbol or the PR number: the entries of "
    "\"Things that have bitten before\" moved to the rules files on %s, each under its original section "
    "title." % (DATE, DATE))

SKILLS_NOTE = (
    "**Procedures are skills (since %s).** The local test fixture, production deploys, release day, "
    "Gemini consults, LOUPE and the DeepSeek sweep, and multi-PR batches are project skills in "
    "`.claude/skills/`: only their descriptions load at startup, and a skill's body loads when it is "
    "invoked. See \"Procedures live in skills\" at the end." % DATE)

WHERE = """## Where things live

**Where does a new entry go?** A rule for every session: one line here. A rule about particular code: the rules file whose `paths:` globs match that code, adding a glob when none does. A procedure: a skill. Reasoning, measurements and history: the History file its line points to; the dated record: `ops/engineering-log.md`. The budgets under "Build" keep each of these in bounds, and `python3 scripts/rules-for.py <path>` prints the rules files that govern a path.

| Topic | Rules file |
|---|---|
| The scanner: the walk, the extractors and tag parsers, the deletion and reconciliation passes, the watcher, the store writes | `scanner.md` |
| The wire contract: the served set, optional fields, tombstones, events, query encoding, endpoints and mDNS, `PROTOCOL.md`; the wire-type discipline | `wire.md` |
| Lyrics: embedded, sidecar, and the Atlas network tier | `lyrics.md` |
| Enrichment (MusicBrainz, Cover Art Archive, Deezer, iTunes, the Atlas harvest), artwork, fingerprinting, duplicates | `enrichment.md` |
| The job pools (upscale, optimize, analysis), renditions and their staleness, the sidecar, variant and artwork reapers | `jobs.md` |
| DLNA, UPnP and SSDP discovery, the upstream ingest and proxy, interface eligibility, mDNS | `dlna-upnp.md` |
| Config and settings, managed controls, service install and restart, atomic writes and ownership, the self-updater | `config-lifecycle.md` |
| SQLite predicates and indexes, compaction and the WAL, retention | `sqlite.md` |
| The CLI, `bridge init` and `bridge doctor`, and the serve wiring: gates, loops, locks, shutdown | `cli-serve.md` |
| Auth, pairing, sessions and login tickets, TLS and the pin, what logs and error bodies may say | `auth-tls.md` |
| The admin console, the web player, uploads and the trash | `admin-console.md` |
| Test discipline, CI, SonarCloud and Docker (loads with every test file) | `build-ci-tests.md` |"""

DEPLOY_LINES = """These hold with or without the `production-deploy` skill, which has the fleet, the demo bridge's overrides and the rest of the procedure:

- **Read `ops/deployment-runbook.md` before any deploy, and ask the operator before any production deploy**: the operator decides every one.
- **One SSH ControlMaster per host, never a burst.** The flood protection is on the operator's own network path, so a burst produces a multi-minute timeout that reads as an outage.
- **Never write a real SSH target, IP or key path into a tracked file**: the repo is public. Every `<PLACEHOLDER>` in the runbook resolves in `ops/coordinates.local.md`, which is gitignored, and output pasted from a live host is redacted first."""

# Lines CLAUDE.md gains at the end of a `##` section, by its title.
SECTION_TAIL = {
    "Build": "- **Instruction-file budgets are enforced** by `scripts/check-doc-budgets.py`, which `make docs-check`, "
             "CI's `docs-check` job and a PostToolUse hook in `.claude/settings.json` run: CLAUDE.md ≤ 64 KB, each "
             "rules file ≤ 100 KB, CLAUDE.md plus the rules one file pulls in ≤ 480 KB. Over budget, never cut: move "
             "a topic entry from here to the rules file that loads with its code (\"Where things live\"; "
             "`scripts/doc-split/verify_moves.py --base <rev>` proves nothing was lost), or move an entry's long form "
             "out with `scripts/doc-split/move_entry.py FILE --largest` / `--line N`, which lands it verbatim in an "
             "`ops/History-*.md` file and leaves its one-line pointer. `scripts/doc-split/README.md` has the rest.",
}

MOVED_NOTE = (
    "**The entries themselves moved on %s** into the rules files listed under \"Where things live\", "
    "each under its original `###` title (now a `##`), so that they load with the code they bind. The "
    "dated review records stay below, one line each." % DATE)

REVIEW_HEADING = "Review and fix batches — one line each"

# One line per dated review record, keyed by the record's title (anchor dropped). Checked against
# the record's whole text the way validate_overrides.py checks a polished line.
REVIEW_LINES = {
    "2026-09-22 — review of the #959–#966 fix window":
        '- <a name="review-2026-09-22-fixes"></a>**2026-09-22 — review of the #959–#966 fix window** — '
        "**Every defect was a behaviour the existing tests still accept**, and three were introduced by the "
        "fixes they sit in: a fix placed in FRONT of a check can destroy that check's input, a range gate does "
        "not protect a value that can wrap into the range, an escape hatch re-opens the failure its own PR "
        "fixes; disjointness decays across review rounds → History-Batches.md",
    "2026-09-22 — full review of the post-v0.2.0 window":
        '- <a name="review-2026-09-22"></a>**2026-09-22 — full review of the post-v0.2.0 window** — the '
        "enumeration failure, again, on the destructive site: **Grep for the PATTERN, not the symptom**; a "
        "fix's own blast radius is a thing to check; three tests asserted the defect; a docblock that matches "
        "its code exactly can still be the bug; merge order from file overlap, not instinct → History-Batches.md",
    "2026-09-18 — findings review on the post-#899 window":
        '- <a name="review-2026-09-18"></a>**2026-09-18 — findings review on the post-#899 window** — gate on '
        "the PARSED value and test with the client's real request; a sibling loop that missed the guard; one "
        "consumer that never went live when its writers did; verify a proposed fix's MECHANISM before taking "
        "it; when you correct a claim, grep for its twins in every file that restates the rule → History-Batches.md",
    "2026-09-10 — full-codebase review":
        '- <a name="review-2026-09-10"></a>**2026-09-10 — full-codebase review** — **The worst defect was not '
        "in the new code**: `internal/trash` had never worked on a multi-root bridge; the mechanical sweeps "
        "came back CLEAN; the defects were in code nobody had reviewed and at the seam between two subsystems; "
        "the same enumeration failure, three more times; take the observation, verify the mechanism, write "
        "your own fix → History-Batches.md",
    "2026-09-09 — LOUPE on the 2026-09-04..09 window":
        '- <a name="loupe-2026-09-09"></a>**2026-09-09 — LOUPE on the 2026-09-04..09 window** — **When a fix '
        "enumerates the sites it covers, the enumeration is the defect**; **A fixture that omits a live gate "
        "describes a different bridge than production**; `ExtractorVersion` was not bumped for the lyrics "
        "batch; a managed CONTROL must own the settings FIELD that performs it; a test seam is per-SERVER, "
        "never a package var; commit before the control, always → History-Batches.md",
}

PROCEDURES = """## Procedures live in skills

A procedure, meaning steps taken when a trigger comes up, is a project skill in `.claude/skills/<name>/SKILL.md`. Claude Code loads only each skill's name and description at startup, so the description is the trigger; the body loads when the user types `/<name>` or when the description matches. Use the skill rather than improvising the procedure, and put a new procedure in a new skill, not here.

| Skill | Use it when |
|---|---|
| `local-test-fixture` | running a bridge from a folder of test audio, or touching a bridge database from outside the bridge |
| `production-deploy` | before any deploy to the operator bridge, home-pc or the demo, or any SSH to the fleet; the rules under "Production deployments" hold without it |
| `release-playbook` | release day: the scope, the sanity reviews, the candidate, the tag, the rollout, and the per-release docs |
| `gemini-consult` | a decision is non-obvious and neither the code, a test nor this file settles it: consult rather than guess |
| `review-procedures` | the user asks for LOUPE or the DeepSeek sweep by name |
| `multi-pr-batch` | a job spans three or more PRs |"""

# Where the long form of each condensed CLAUDE.md block goes, by its `##` section's title prefix.
CORE_HIST = [
    ("Build", "History-Build.md"),
    ("Repo clean-up", "History-Build.md"),
    ("Architecture at a glance", "History-Build.md"),
    ("iOS companion (coupled repo)", "History-Cross-Repo.md"),
    ("Production deployments", "History-Docs.md"),
    ("Documentation refresh on each release", "History-Docs.md"),
    ("Agent memory", "History-Workflow.md"),
    ("Follow-ups go in the backlog", "History-Workflow.md"),
    ("Things that have bitten before", "History-Workflow.md"),
    ("Licensing", "History-Workflow.md"),
    ("Bot-review discipline (PR-time)", "History-Workflow.md"),
    ("Development workflow", "History-Workflow.md"),
    ("Post-merge deployment", "History-Workflow.md"),
]

# Sections that stay whole: the policies an agent applies as written.
KEEP_WHOLE = ("Architecture at a glance", "Agent memory", "Follow-ups go in the backlog", "Licensing",
              "Development workflow", "Post-merge deployment")

HISTORY_TITLES = [
    ("History-Build.md", "CLAUDE.md's build and release-wiring entries"),
    ("History-Cross-Repo.md", "CLAUDE.md's iOS-companion entries"),
    ("History-Docs.md", "CLAUDE.md's rules for the public docs"),
    ("History-Workflow.md", "CLAUDE.md's workflow and review-process entries"),
]


def history_header(title, hist, date):
    return ("# Long form: %s\n\n"
            "> Moved verbatim from `CLAUDE.md` on %s so it stops loading on every request. **These entries are "
            "still binding.** CLAUDE.md keeps each one as a single line ending `→ %s`; change an entry here and "
            "its line there in the same commit. Each `##` names the section the entries came from."
            % (title, date, hist))


RULES_NOTE = (
    "Moved verbatim from CLAUDE.md on %s, under the section titles it had there, so a code comment citing "
    "\"CLAUDE.md <title>\" or \"under **<section>**\" means an entry here. The record behind each rule is in "
    "`ops/engineering-log.md`: grep it by symbol, file or PR number before changing anything a rule names." % DATE)

# ---- skills -------------------------------------------------------------------------------------

PROVENANCE = "Moved verbatim from CLAUDE.md on %s. Paths are relative to the repository root." % DATE

SKILLS = [
    {
        "name": "local-test-fixture",
        "description": "Running a local bridge from a folder of test audio (the loopback recipe, and the "
                       "public-mode variant with its four traps), forcing re-enrichment or a tag re-extraction, "
                       "and wiping or editing a bridge database from outside the bridge without tripping the "
                       "migration v4 unicode_lower trap or the WAL read trap. Use it before starting a test "
                       "bridge, or before touching a bridge.db by hand.",
        "title": "Local test fixture",
        "intro": PROVENANCE,
    },
    {
        "name": "production-deploy",
        "description": "Deploying the bridge to the live fleet: the operator bridge on the home NUC "
                       "(self-signed, so health probes need curl -k), home-pc (Windows) and the public demo "
                       "bridge.1-bit.app on the bridge.ars.md VPS, whose deploy must carry SVC and REMOTE_BIN, "
                       "plus where the host coordinates and placeholders live. Use it before any production "
                       "deploy or SSH to those hosts, after reading ops/deployment-runbook.md, and only with the "
                       "operator's go-ahead.",
        "title": "Production deploys",
        "intro": PROVENANCE + " The procedures themselves are `ops/deployment-runbook.md`: read it in full "
                              "before a deploy. CLAUDE.md keeps the rules that hold without this skill, under "
                              "\"Production deployments\" and \"Post-merge deployment\".",
    },
    {
        "name": "release-playbook",
        "description": "Release day for the bridge: ops/release-playbook.md end to end (scope and freeze, the "
                       "three sanity reviews, validating the candidate, the annotated tag and the artifact "
                       "checks, the gated rollout, running it with agents) and docs/release-process.md for the "
                       "per-release docs. Use it when the operator says a release is being cut, tagged or "
                       "rolled out.",
        "title": "Release day",
        "intro": PROVENANCE + " The two rules for any doc edit (user-facing docs live in the 1bitapp repo; "
                              "public prose names the entity ars.md) stay in CLAUDE.md under \"Documentation "
                              "refresh on each release\".",
    },
    {
        "name": "gemini-consult",
        "description": "Consult Gemini on a non-obvious decision instead of guessing: Go concurrency "
                       "subtleties, tsnet and Tailscale internals, behaviour that differs between darwin, linux "
                       "and windows, framework upgrade gotchas. Covers the direct consult.py route, framing the "
                       "question, when to skip, and applying the answer. Use it before shipping anything you "
                       "would otherwise caveat with \"I think this is right but I'm not 100% sure\".",
        "title": "External consultation (Gemini)",
        "intro": PROVENANCE,
    },
    {
        "name": "review-procedures",
        "description": "The bridge's named review procedures: LOUPE (the recent-work review cycle) and the "
                       "DeepSeek code-review sweep. Use it when the user asks for either by name, e.g. \"run "
                       "LOUPE on last week\", \"LOUPE the last N commits\", \"LOUPE the enrich package\", \"LOUPE "
                       "since v0.1.9\", \"run the DeepSeek sweep\".",
        "title": "Review procedures: LOUPE and the DeepSeek sweep",
        "intro": PROVENANCE + " Each procedure's full write-up is the doc it names; read that doc in full "
                              "before starting. The PR-time bot-review rules stay in CLAUDE.md.",
    },
    {
        "name": "multi-pr-batch",
        "description": "Workflow for a bridge job that spans three or more PRs: stacked versus parallel PRs, "
                       "one review wait, one combined fix pass per branch, merging bottom-up with retargeting, "
                       "the follow-up PR, the end-of-session gate and one deploy at the end. Use it when "
                       "planning or running such a batch.",
        "title": "Multi-PR batch workflow",
        "intro": PROVENANCE + " The single-PR loop it extends is CLAUDE.md's \"Development workflow\".",
        # the section's link to "Post-merge deployment" pointed into CLAUDE.md, where that section stays
        "edits": [("(#post-merge-deployment)", "(../../../CLAUDE.md#post-merge-deployment)")],
    },
]

# ---- corrections ----------------------------------------------------------------------------------

# Sentences in blocks that stay in CLAUDE.md, made false by the split: (file, old, new), each once.
CORE_EDITS = [
    ("CLAUDE.md",
     "| Invariants, \"don't undo this\", rejected-and-why decisions | `CLAUDE.md` — the matching subsystem group "
     "under \"## Things that have bitten before\" below |",
     "| Invariants, \"don't undo this\", rejected-and-why decisions | the rules file whose `paths:` globs match the "
     "code, `.claude/rules/<name>.md` (\"Where things live\" above), or `CLAUDE.md` for a rule every session needs; "
     "a long entry is one line there and its long form goes to `ops/History-*.md` |"),
    ("CLAUDE.md",
     "**Not auto-loaded**, so the *rule* must still land in `CLAUDE.md` |",
     "**Not auto-loaded**, so the *rule* must still land in `CLAUDE.md` or a rules file |"),
    ("CLAUDE.md",
     "| Build/test commands and their gotchas | `## Build` above |",
     "| Build/test commands and their gotchas | `## Build` above |\n"
     "| Procedures (the local fixture, deploys, release day, consults, named reviews, multi-PR batches) | a skill, "
     "`.claude/skills/<name>/SKILL.md`, whose `description` says when to use it (\"Procedures live in skills\" "
     "below) |"),
    ("CLAUDE.md",
     "4. **Only `CLAUDE.md` is auto-loaded.** `ops/engineering-log.md` exists so a rule can stay short without "
     "losing its evidence — but a finding written *only* there is the same inert paper as rule 1. Every batch "
     "writes the **rule** here and the **record** there.",
     "4. **Only `CLAUDE.md` and the rules files are auto-loaded** (a rules file when a file its `paths:` globs "
     "match is Read). `ops/engineering-log.md` exists so a rule can stay short without losing its evidence — "
     "but a finding written *only* there is the same inert paper as rule 1. Every batch writes the **rule** "
     "here or in the rules file that loads with its code, and the **record** there."),
    ("CLAUDE.md",
     "**When you fix something here, put the rule in this file and the record in the\nlog** — never only in the "
     "log, because nothing there reaches a session that has\nnot gone looking for it.",
     "**When you fix something here, put the rule in this file or the rules file that\nloads with its code, and "
     "the record in the log** — never only in the log, because\nnothing there reaches a session that has not gone "
     "looking for it."),
]

# Sentences in other files, made false by the split: (file, old, new), each once.
EDITS = [
    ("AGENTS.md",
     "[`CLAUDE.md`](CLAUDE.md) is the deeper\ninstitutional-knowledge companion — its \"things that have bitten "
     "before\"\nsection carries the project's invariants grouped by subsystem. Consult it\nbefore touching",
     "[`CLAUDE.md`](CLAUDE.md) is the deeper\ninstitutional-knowledge companion, and the project's invariants, "
     "grouped by\nsubsystem, are in the `.claude/rules/*.md` files whose `paths:` globs match\nthe code "
     "(`python3 scripts/rules-for.py <path>` names them). Consult them\nbefore touching"),
    ("AGENTS.md",
     "**Agent-doc maintenance.** `CLAUDE.md` is the authority, and it is a living\ndocument: new invariants — the "
     "\"things that have bitten before\" entries a\nfix produces — go THERE, in the same PR as the fix they describe, "
     "with the\nsupporting record appended to `ops/engineering-log.md` in that PR too, so a\nreview reads the rule "
     "beside the code it is about.",
     "**Agent-doc maintenance.** `CLAUDE.md` and the rules files are the authority,\nand they are living documents. "
     "A new invariant, the entry a fix produces,\ngoes in the same PR as the fix it describes: in the "
     "`.claude/rules/*.md`\nfile whose `paths:` globs match the code it binds (CLAUDE.md's \"Where things\nlive\"), "
     "or in `CLAUDE.md` when every session needs it. A long entry is one\nline there, and its long form goes to the "
     "`ops/History-*.md` file the line\npoints to. The supporting record is appended to `ops/engineering-log.md` "
     "in\nthat PR too, so a review reads the rule beside the code it is about."),
    ("AGENTS.md",
     "Only `CLAUDE.md` is auto-loaded into an agent\nsession, so a finding written only into the log is inert.",
     "Claude Code loads `CLAUDE.md` into every\nsession and a rules file whenever it reads a file the globs match, "
     "and a\nfinding written only into the log is inert. Cursor loads this file and\n`CLAUDE.md` but not "
     "`.claude/rules/`, so before changing a file, run\n`python3 scripts/rules-for.py <path>` and read every rules "
     "file it prints;\nbefore committing, run `python3 scripts/rules-for.py --changed`."),
    ("AGENTS.md",
     "| `make check-go-version` | Guards that `Dockerfile`'s `ARG GO_VERSION` matches `go.mod`'s major.minor (alpine "
     "sets `GOTOOLCHAIN=local`, so a stale ARG fails the image build). |",
     "| `make check-go-version` | Guards that `Dockerfile`'s `ARG GO_VERSION` matches `go.mod`'s major.minor (alpine "
     "sets `GOTOOLCHAIN=local`, so a stale ARG fails the image build). |\n"
     "| `make docs-check` | Holds `CLAUDE.md`, the rules files and the skills to their byte budgets "
     "(`scripts/check-doc-budgets.py`; Python stdlib only), and runs the doc tools' self-tests. CI runs it too. |"),
    ("AGENTS.md",
     "  Details in `CLAUDE.md` → \"Multi-PR batch workflow\".",
     "  Details in the `multi-pr-batch` skill (`.claude/skills/multi-pr-batch/SKILL.md`)."),
    ("AGENTS.md",
     "| `CLAUDE.md` | Deep agent knowledge: invariants by subsystem, workflows, external-review processes |",
     "| `CLAUDE.md` | Deep agent knowledge: build and workflow rules, the cross-repo contract, where everything else lives |\n"
     "| `.claude/rules/*.md` | The invariants by subsystem, each file loading with the code its `paths:` globs match "
     "(`scripts/rules-for.py`) |\n"
     "| `.claude/skills/*/SKILL.md` | Procedures: the local fixture, deploys, release day, consults, LOUPE and DeepSeek, "
     "multi-PR batches |\n"
     "| `ops/History-*.md` | The long form of every condensed entry in CLAUDE.md and the rules files (binding, not "
     "auto-loaded) |"),
    ("ops/README.md",
     "2. **It is internal working material** — `engineering-log.md`,\n   `settings-apply-semantics.md`, `plan-*.md`.",
     "2. **It is internal working material** — `engineering-log.md`,\n   `History-*.md`, `settings-apply-semantics.md`, "
     "`plan-*.md`."),
    ("ops/README.md",
     "`engineering-log.md` is the record behind every invariant in `CLAUDE.md` — the",
     "`engineering-log.md` is the record behind every invariant in `CLAUDE.md` and\nthe rules files — the"),
    ("ops/README.md",
     "**Only\n`CLAUDE.md` is auto-loaded**, so a finding written only into the log never\nreaches a session: put the "
     "*rule* in `CLAUDE.md` and the *record* here.",
     "**Only\n`CLAUDE.md` and the rules files are auto-loaded** (`.claude/rules/*.md`, each\nwith the code its globs "
     "match), so a finding written only into the log never\nreaches a session: put the *rule* in `CLAUDE.md` or the "
     "rules file that loads\nwith its code, and the *record* here. The `History-*.md` files hold the long\nform of "
     "every condensed entry in those files, verbatim, since 2026-10-18; they\nare binding, and not auto-loaded "
     "either."),
    ("docs/LoupeReviewCycle.md",
     "### [BRIDGE] The structural check has no rules files to measure",
     "### [BRIDGE] The structural check: which rules file binds the surface"),
    ("docs/LoupeReviewCycle.md",
     "no glob. **This repo has no `.claude/rules/` at all** — every invariant lives in\n`CLAUDE.md`, organised under "
     "`## Things that have bitten before` by subsystem\nheading. So the equivalent structural question is:",
     "no glob. **Since 2026-10-18 this repo does too**: the invariants live in\n`.claude/rules/*.md` by subsystem, "
     "each under the section title it had in\n`CLAUDE.md`'s `## Things that have bitten before`. So the structural "
     "question\nis the iOS one:"),
    ("docs/LoupeReviewCycle.md",
     "- Does the surface have a `### ` section under `## Things that have bitten\n  before`? Name it.",
     "- Does the surface have a rules file its files load (`python3 scripts/rules-for.py\n  <path>`)? Name it."),
    ("docs/LoupeReviewCycle.md",
     "[BRIDGE] A dated entry in `CLAUDE.md` — there are no rules files here, so\neverything lands in the subsystem "
     "section under `## Things that have bitten\nbefore`, and a cross-cutting run gets its own `### <date> — LOUPE on "
     "<surface>`\nentry.",
     "[BRIDGE] A dated entry, and the invariants in the rules file that loads with\nthe surface's code, under its "
     "section (CLAUDE.md's \"Where things live\" lists\nthem; a condensed file takes one line per entry and its long "
     "form goes to\n`ops/History-rules-<name>.md`). A cross-cutting run gets one dated line under\nCLAUDE.md's "
     "\"Review and fix batches — one line each\", its record in\n`ops/History-Batches.md`."),
    ("ops/release-playbook.md",
     "- CLAUDE.md is ALREADY in your context (loaded automatically, ~200k tokens) and is the spec.",
     "- CLAUDE.md is ALREADY in your context (loaded automatically) and is the spec, with the rules\n"
     "  files Claude Code loads beside the files you Read."),
    ("ops/release-playbook.md",
     "- The rule goes in CLAUDE.md (the matching group under \"Things that have bitten before\"), the record\n"
     "  in ops/engineering-log.md, both in the SAME PR as the code.",
     "- The rule goes in the rules file that loads with the code (CLAUDE.md \"Where things live\"; a\n"
     "  condensed file takes one line, the long form its ops/History-rules-*.md), the record in\n"
     "  ops/engineering-log.md, all in the SAME PR as the code."),
]

# The verify_moves.py arguments these edits need: the start of each base block they change, and
# the one heading they rename.
VERIFY_EDITS = [
    "| What | Where |",
    "4. **Only `CLAUDE.md`",
    "**When you fix something here, put the rule",
    "9. **Post-merge deploy** after the whole stack lands",
    "Guidance for AI coding agents working in this repository.",
    "**Agent-doc maintenance.**",
    "- For jobs spanning 3+ PRs use the stack-and-batch pattern",
    "| Command | What it does |",
    "| File | Content |",
    "2. **It is internal working material**",
    "`engineering-log.md` is the record behind every invariant",
    "The iOS repo binds invariants to files",
    "- Does the surface have a",
    "[BRIDGE] A dated entry",
    "```\nYou own ONE backlog entry end to end",
]
VERIFY_RENAMES = [
    "[BRIDGE] The structural check has no rules files to measure=>"
    "[BRIDGE] The structural check: which rules file binds the surface",
]
