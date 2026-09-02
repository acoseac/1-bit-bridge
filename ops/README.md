# ops/ — internal operator documents (deliberately NOT published)

GitHub Pages serves this repo's **`docs/`** directory (`main:/docs`) at
<https://acoseac.github.io/1-bit-bridge/>, and the repository is **public**. Anything
under `docs/` is therefore on the open web — indexable, scrapeable, and readable without a
GitHub account.

These files live here instead of `docs/` because they must **not** be published. Two
different mechanisms are in play — moving a file to `ops/` only stops **Pages** from serving
it, and a tracked file in a public repo is still readable by anyone at
`raw.githubusercontent.com`. Files whose content is sensitive regardless of how it's reached
must also be **untracked** (`.gitignore`), not merely relocated:

| File | Why it can't be published | How it's kept out |
|---|---|---|
| `deployment-runbook.md` | Host coordinates: SSH user@host for both bridges, key filename, the router port-forward endpoint, the tailnet addresses, and the ufw posture (which documents that an IP allowlist is the only control on SSH and the admin console). | **Tracked, but placeholdered.** Every real coordinate is a `<PLACEHOLDER>`; the values live in `coordinates.local.md`. |
| `coordinates.local.md` | The real values behind the runbook's placeholders. | Untracked — `.gitignore: ops/*.local.md` |
| `audit-2026-07-18.md` | A full codebase audit — effectively an exploit index, with `file:line` and fix sketches for every finding. Actionable against any bridge whose operator hasn't upgraded yet, which is why "the fixes are merged" is not on its own sufficient. | Untracked — `.gitignore: ops/*audit*.md` |
| `1bit-audit-2026-07-18.md` | A second, independently-run audit of the same Go bridge (`internal/*` + `cmd/bridge`, 38 packages) — same exposure as above. | Untracked — `.gitignore: ops/*audit*.md` |

### Not everything here is sensitive

Two different reasons put a file in `ops/`, and only the first is about secrecy:

1. **It must not be published** — the table above. Host coordinates, exploit
   indexes. These are placeholdered or untracked.
2. **It is internal working material** — `engineering-log.md`,
   `settings-apply-semantics.md`, `plan-*.md`. Nothing in them is sensitive;
   they simply aren't user-facing documentation, and `docs/` is the public
   website. They are tracked, and normal.

`engineering-log.md` is the record behind every invariant in `CLAUDE.md` — the
measurement that settled it, the alternative that was rejected, the test that
pins it, the PR. It was extracted from `CLAUDE.md` on 2026-09-02 because that
file is auto-loaded into every agent session and had grown to 520 KB. **Only
`CLAUDE.md` is auto-loaded**, so a finding written only into the log never
reaches a session: put the *rule* in `CLAUDE.md` and the *record* here.

**Don't move these back into `docs/`, and don't re-`git add` the untracked ones.** If a future
document names a live host, an IP, a port-forward, a key path, or enumerates unfixed
weaknesses, it belongs here — and if the sensitivity is in the *content* rather than merely
the URL it's served from, add it to `.gitignore` too.

What legitimately stays in `docs/`: the redirect stubs to `1-bit.app/bridge/*`, `docker.md`
(linked publicly at its GitHub blob URL), `deployment/public-vps.md` (a generic guide with no
live coordinates), `release-process.md`, and `.nojekyll`.

**Note on history.** Neither relocating a file out of `docs/` nor untracking it purges git
history, existing clones and forks, or search-engine caches. The audit docs were tracked from
2026-07-18 until the v0.1.8 release prep, so their blobs remain reachable by SHA to anyone who
already has (or fetches) the history; untracking stops the bleeding but is **mitigation, not
erasure**. A full history rewrite was considered and declined — it invalidates every clone and
rewrites every SHA since the audit landed, breaking existing PR and CI links, which isn't a
trade worth making for content whose findings are now fixed and shipped.

The same asymmetry applies to secrets: for a credential or endpoint that was ever published,
the move is necessary but not sufficient — **rotate it**.
