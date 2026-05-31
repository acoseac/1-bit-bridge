# Bridge release process (operator)

Extracted from CLAUDE.md. The per-release documentation-refresh procedure:
which files to update, which NOT to touch, the process, gotchas, and the
after-the-tag steps.

## Documentation refresh on each release

Every release tag is accompanied by a doc-site refresh PR — `README.md` status line bump plus whatever updates the new version's user-facing surface needs. Blueprint hardened across PR #185 (v0.1.2) where four rounds of bot review surfaced privacy, link-rendering, and anchor-target issues that this checklist now covers up-front.

### Files to update per release

- **`README.md`** — bump line ~22 status (`**v0.1.x** — wire protocol frozen…`). Always.
- **`docs/index.html`** — refresh hero feature bullets if any new capability shipped, and the "Quick start" three-step panel if the install flow changed.
- **`docs/setup.html`** — beginner walkthrough. Update per-OS install panels if archive-naming or unpack steps changed; refresh scan-time table if perf shifted; update pairing methods if iOS app added/removed a flow.
- **`docs/features.html`** — feature deep-dive index. Add a new `<h2 id="...">` section for any new capability (mirror the existing pattern: short paragraph + 3–5 bullets/dl). The "Container & headless deployments" section's env-var table mirrors `internal/config/config.go` — keep in sync when new env-var overrides land.
- **`docs/troubleshooting.html`** — add a `.symptom` / `.fix` pair for any new known operational issue. Cross-page deep links (`setup.html#step-N`, `features.html#tailscale`, etc.) need their target ids to exist; see "Gotchas" below.
- **`docs/privacy.html`** — verify against the *current* logging / storage / lookup-data shape. Bump "Last updated" date **only** if content changed; bumping for every release without a content change is misleading.
- **`docs/assets/site.css`** — only when adding new component styles. Existing palette / typography / button / nav classes are stable.

### Files NOT to touch per release

- `internal/version/version.go` — `ServerVersion` is injected at build time via `-ldflags -X` (see `.goreleaser.yaml`); the source default stays `0.0.1`.
- `.goreleaser.yaml`, `.github/workflows/release.yml` — pipeline is mature; only touch for genuine pipeline changes (a new platform, signing-cert rotation, etc.).
- `PROTOCOL.md` — only on a wire-protocol break (which also bumps `ProtocolVersion` and triggers the Mirror-PR rule).
- `CONTRIBUTING.md`, `SECURITY.md`, `LICENSE` — stable.
- `docs/docker.md` — kept as markdown so the GitHub-rendered view + raw-repo-browse view both work; HTML pages link at the GitHub blob URL (see Gotchas).

### Process

1. **Logging audit before drafting privacy copy.** Grep `internal/pairing/`, `internal/transcode/`, `internal/api/upscale.go`, `internal/api/events.go`, and `internal/admin/handlers_events.go` for `slog.` / `Error(` / `logger.` calls and confirm no formatter feeds raw bearer tokens, client IPs (`r.RemoteAddr`, `RealIP`), or absolute filesystem paths into log lines or `writeError` responses. Privacy-policy claims must hold against the *current* surface, not the v1.0 paths the policy was originally written against. Anything found gets fixed in the same PR — `redactWalkErr` in [internal/api/upscale.go](internal/api/upscale.go) is the canonical helper for `*os.PathError` unwrapping (covers both log AND wire egress channels).
2. **Edit the docs.** Order that works: `docs/assets/site.css` (if needed) → new pages or new sections in existing pages → `index.html` cross-links → `privacy.html` verification → `README.md` status bump.
3. **Local site preview.** `cd docs && python3 -m http.server 8765`, then open in a browser and click through every nav link, every cross-page link, every external link, every anchor (`#section`) link. **Do not trust `curl -sf` / 200-status checks alone** — they don't catch raw-text rendering issues for `.md` files (PR #185 round 3) or broken anchors that scroll nowhere (round 4).
4. **Local goreleaser dry-run** (per the v0.1.1 release-pipeline notes — APPLE_TEAM_ID + release-meta.json hooks):
   ```sh
   rm -rf dist release-meta.json
   APPLE_TEAM_ID=Y7ZK32ZM7K MACOS_SIGN_P12=fake \
     goreleaser release --snapshot --clean \
     --skip=sign,publish,notarize,announce,validate
   strings dist/bridge_darwin_arm64_v8.0/bridge | grep -c Y7ZK32ZM7K   # → 1
   cat release-meta.json   # → "version" non-empty
   rm -rf dist release-meta.json
   ```
5. `make fmt vet test build-all` — clean. Race-enabled tests; cross-compiles to all 6 targets.
6. **Open PR**, branch + PR convention (never direct to main *for code*; CLAUDE.md alone is the docs-only direct-to-main exception).
7. **Bot review rounds.** Plan for 2 rounds minimum, expect up to 4 on a docs-touch + privacy-adjacent PR. Greptile finds smaller things each round; CodeRabbit confirms "no actionable" once the security-shaped issues are gone. Address all comments per round in a single fix commit; reject suggestions that contradict deliberate in-code rationale (Gemini flagged "iOS 26.4" as a typo for "16.4" on PR #185 — rejected, iOS 26 is real and Gemini's training cutoff predates it).
8. **Merge** once reviews are quiet AND CI is green.
9. **Tag the release.** `git tag v0.1.x && git push --tags` triggers `.github/workflows/release.yml`. The workflow drafts a GitHub Release with the goreleaser-generated changelog; **edit the notes** to highlight user-facing features (use the `### v0.1.x` per-version sections in this CLAUDE.md as raw material), then publish.

### Gotchas (proven on PR #185)

- **`<pre><code>` blocks inside `<li>` render with light-on-light text** (post-merge regression on the live site, fix `18c010c` on main). `docs/assets/site.css`'s `p code, li code, td code, dd code` selector paints inline `<code>` with a light `--bg-soft` background; when a `<pre><code>…</code></pre>` block lives inside a `<li>` (any `ol.steps` step, the install-panel cards in `setup.html`, the upscale-enable steps), `li code` matches the inner `<code>` and overrides the `<pre>`'s dark background — but the foreground stays inherited at `--code-fg` (light), so the code text is invisible. Fix is the explicit `pre code { background: transparent; padding: 0; border: 0; font-size: inherit; color: inherit; }` reset that's now in `site.css`. Don't reintroduce that styling at any new site; verify in the local-preview pass that any new `<pre><code>` block under `<li>` renders dark with light text. The site's pre-PR-#185 inline styles had the same `li code` rule but never tripped because `<pre><code>` blocks lived in plain `<div>` install panels, not under `<li>`.
- **`docs/.nojekyll` makes GH Pages serve `.md` as raw text.** Any `<a href="*.md">` link from an HTML page renders as plain markdown source, not formatted HTML. Either rename `.md` → `.html` (and convert), or point the link at the GitHub blob URL (`https://github.com/acoseac/1-bit-bridge/blob/main/docs/<file>.md`) so GitHub's own renderer handles it. We use the blob-URL form for `docker.md` (Greptile round 3).
- **`<ol class="steps">` items use CSS `counter-increment` for visible numbers.** The `<li>` elements have no `id` attribute by default — cross-page deep links like `setup.html#step-6` would scroll nowhere. Every step `<li>` in `setup.html` carries `id="step-N"` so any future cross-page anchor lands; keep that pattern when adding steps (Greptile round 4).
- **`./bridge stop` and `./bridge start` are NOT real subcommands.** The bridge is started/stopped via the platform service manager: `launchctl load/unload …plist` on macOS, `systemctl --user start/stop 1-bit-bridge` on Linux, `sc.exe start/stop 1-bit-bridge` on Windows. Don't reintroduce the bogus subcommands in restore / troubleshooting docs (Gemini medium PR #185).
- **`*os.PathError`'s `.Error()` string embeds the absolute filesystem path.** `filepath.WalkDir` propagates these for permission-denied / file-not-found failures. Privacy commitment ("absolute filesystem paths are not logged AND not sent over the wire") requires unwrapping for **both** `logger.Error("err", …)` (slog will format the value) AND any `writeError` body. `redactWalkErr` in [internal/api/upscale.go](internal/api/upscale.go) is the canonical helper; reuse it at any new walk-error site (Greptile rounds 1 + 2).
- **iOS 26.4+ ATS lower-layer enforces 825-day cert validity** independently of `NSAllowsLocalNetworking`. This is real, not a typo for iOS 16.4 — Gemini's training cutoff predates iOS 26 and may flag it. Keep the version as written.
- **`MIN_CLIENT_VERSION` env var stays unset in `.github/workflows/release.yml`.** The default `0.0.0` ("no floor") is what gets baked into the release. Only set it when the release is wire-incompatible with currently-paired iOS clients — `ProtocolVersion` stays at 1 for additive changes, so most releases leave the floor alone. Setting a floor would orphan any iOS app version below it from the bridge's auto-installer compat gate.
- **Privacy "Last updated" date** bumps **only** when content changed. A release that only added a new feature without changing logging/storage/lookup behavior leaves the date alone (and the policy itself untouched). Bumping the date without a content change makes the changelog noisy and dilutes the signal that something material moved.

### After the tag pushes

The release workflow drafts a GitHub Release in ~3 min: signed + notarized darwin × amd64/arm64 archives, unsigned linux × amd64/arm64 + windows × amd64/arm64 archives, `release-meta.json` sidecar. Edit the auto-generated changelog (filtered by goreleaser to drop docs/chore commits) into a user-facing summary highlighting the v1.x features that landed, then click Publish. End-user install instructions in `README.md` work from publish onward.

If the operator-side runbook documents anything that's NOT in this CLAUDE.md, fold it back here — this section is the single source of truth for the release process.

