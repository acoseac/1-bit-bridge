# ops/ — internal operator documents (deliberately NOT published)

GitHub Pages serves this repo's **`docs/`** directory (`main:/docs`) at
<https://acoseac.github.io/1-bit-bridge/>, and the repository is **public**. Anything
under `docs/` is therefore on the open web — indexable, scrapeable, and readable without a
GitHub account.

These files live here instead of `docs/` because they must **not** be published:

| File | Why it can't be published |
|---|---|
| `deployment-runbook.md` | Live host coordinates: SSH user@host for both bridges, key filename, the router port-forward endpoint, and the ufw posture (which documents that an IP allowlist is the only control on SSH and the admin console). |
| `audit-2026-07-18.md` | A full codebase audit — effectively an exploit index. Publishing it while fixes are unreleased is a zero-day disclosure. |
| `1bit-audit-2026-07-18.md` | Same, for the iOS companion repo. |

**Don't move these back into `docs/`.** If a future document names a live host, an IP, a
port-forward, a key path, or enumerates unfixed weaknesses, it belongs here.

What legitimately stays in `docs/`: the redirect stubs to `1-bit.app/bridge/*`, `docker.md`
(linked publicly at its GitHub blob URL), `deployment/public-vps.md` (a generic guide with no
live coordinates), `release-process.md`, and `.nojekyll`.

**Note on history.** Moving a file out of `docs/` stops Pages serving it going forward; it
does **not** purge git history, existing clones, or search-engine caches. For a secret that
was published, the move is necessary but not sufficient — rotate the underlying credential or
endpoint.
