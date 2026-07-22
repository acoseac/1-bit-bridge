# Bridge release process (operator)

Extracted from CLAUDE.md. The per-release documentation-refresh procedure:
which files to update, which NOT to touch, the process, gotchas, and the
after-the-tag steps.

## Documentation refresh on each release

> **User-facing docs moved (2026-06-09).** The overview / setup / features / troubleshooting /
> privacy pages now live in the **`acoseac/1bitapp`** repo (local `~/dev/1bitapp`) and are
> published at **`1-bit.app/bridge/*`**. Update them THERE, following that repo's `CLAUDE.md` →
> "Adding a release". The `docs/*.html` files in *this* repo are now **redirect stubs** pointing at
> 1-bit.app — do not edit them. The only per-release doc touch in this repo is the `README.md`
> status line.

The blueprint below was hardened across PR #185 (v0.1.2), where four rounds of bot review surfaced
privacy, link-rendering, and anchor-target issues. The HTML-rendering items now apply to the
`1bitapp` Astro site; the privacy / logging-audit items are cross-repo and still start here.

### Files to update per release (this repo)

- **`README.md`** — bump line ~22 status (`**v0.1.x** — wire protocol frozen…`). Always.
- **Privacy policy (cross-repo).** If logging / storage / lookup-data behaviour changed this
  release, run the logging audit below, then reflect the result in the `1bitapp` repo's
  `src/pages/bridge/privacy.astro` — bump its "Last updated" date **only** on a real content change.
- **`docs/docker.md`, `docs/deployment/*`** — operator/dev docs that stayed here; update if the
  container or VPS flow changed. The env-var table on `1-bit.app/bridge/features/` mirrors
  `internal/config/config.go` — keep them in sync when new env-var overrides land.

### Files NOT to touch per release

- `internal/version/version.go` — `ServerVersion` is injected at build time via `-ldflags -X` (see `.goreleaser.yaml`); the source default stays `0.0.1`.
- `.goreleaser.yaml`, `.github/workflows/release.yml` — pipeline is mature; only touch for genuine pipeline changes (a new platform, signing-cert rotation, etc.).
- `PROTOCOL.md` — only on a wire-protocol break (which also bumps `ProtocolVersion` and triggers the Mirror-PR rule).
- `CONTRIBUTING.md`, `SECURITY.md`, `LICENSE` — stable.
- `docs/*.html` — **redirect stubs** to `1-bit.app/bridge/*`; the real content lives in the `1bitapp` repo. Don't edit (other than to repoint a redirect target if a 1-bit.app URL ever changes).
- `docs/docker.md` — kept as markdown so the GitHub-rendered view + raw-repo-browse view both work; the 1-bit.app docs link to it at the GitHub blob URL (see Gotchas).

### Process

1. **Logging audit before drafting privacy copy.** Grep `internal/pairing/`, `internal/transcode/`, `internal/api/upscale.go`, `internal/api/events.go`, and `internal/admin/handlers_events.go` for `slog.` / `Error(` / `logger.` calls and confirm no formatter feeds raw bearer tokens, client IPs (`r.RemoteAddr`, `RealIP`), or absolute filesystem paths into log lines or `writeError` responses. Privacy-policy claims must hold against the *current* surface, not the v1.0 paths the policy was originally written against. Anything found gets fixed in the same PR — `redactWalkErr` in [internal/api/upscale.go](internal/api/upscale.go) is the canonical helper for `*os.PathError` unwrapping (covers both log AND wire egress channels).
2. **Edit the docs.** In *this* repo, bump the `README.md` status line. Any user-facing doc change happens in the `1bitapp` repo under `src/pages/bridge/*` (overview / setup / features / download / troubleshooting / privacy).
3. **Preview the 1bitapp site.** In the `1bitapp` repo, `npm run build` (fails on broken internal links / TS errors) then `npm run preview`, clicking every nav link, cross-page link, external link, and `#anchor`. Anchors that scroll nowhere don't fail the build — verify them by eye.
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

- **HTML-site rendering gotchas moved to the `1bitapp` repo.** The old GitHub Pages site's hand-rolled traps — `<pre><code>` inside `<li>` rendering light-on-light, `ol.steps` items needing `id="step-N"` for cross-page anchors — were specific to that HTML/CSS. The 1-bit.app docs are Astro; their conventions live in the `1bitapp` repo's `CLAUDE.md`. What survives here: `docs/.nojekyll` **stays** (it lets Pages serve the redirect stubs and `docker.md` without Jekyll), and the 1-bit.app docs link to `docs/docker.md` at the **GitHub blob URL** (`https://github.com/acoseac/1-bit-bridge/blob/main/docs/docker.md`) so GitHub renders the markdown rather than serving it raw.
- **`./bridge stop` and `./bridge start` are NOT real subcommands.** The bridge is started/stopped via the platform service manager: `launchctl load/unload …plist` on macOS, `systemctl --user start/stop 1-bit-bridge` on Linux, `sc.exe start/stop 1-bit-bridge` on Windows. Don't reintroduce the bogus subcommands in restore / troubleshooting docs (Gemini medium PR #185).
- **`*os.PathError`'s `.Error()` string embeds the absolute filesystem path.** `filepath.WalkDir` propagates these for permission-denied / file-not-found failures. Privacy commitment ("absolute filesystem paths are not logged AND not sent over the wire") requires unwrapping for **both** `logger.Error("err", …)` (slog will format the value) AND any `writeError` body. `redactWalkErr` in [internal/api/upscale.go](internal/api/upscale.go) is the canonical helper; reuse it at any new walk-error site (Greptile rounds 1 + 2).
- **iOS 26.4+ ATS lower-layer enforces 825-day cert validity** independently of `NSAllowsLocalNetworking`. This is real, not a typo for iOS 16.4 — Gemini's training cutoff predates iOS 26 and may flag it. Keep the version as written.
- **`MIN_CLIENT_VERSION` env var stays unset in `.github/workflows/release.yml`.** The default `0.0.0` ("no floor") is what gets baked into the release. Only set it when the release is wire-incompatible with currently-paired iOS clients — `ProtocolVersion` stays at 1 for additive changes, so most releases leave the floor alone. Setting a floor would orphan any iOS app version below it from the bridge's auto-installer compat gate.
- **Privacy "Last updated" date** bumps **only** when content changed. A release that only added a new feature without changing logging/storage/lookup behavior leaves the date alone (and the policy itself untouched). Bumping the date without a content change makes the changelog noisy and dilutes the signal that something material moved.

### After the tag pushes

The release workflow drafts a GitHub Release in ~3 min: signed + notarized darwin × amd64/arm64 archives, unsigned linux × amd64/arm64 + windows × amd64/arm64 archives, `release-meta.json` sidecar. Edit the auto-generated changelog (filtered by goreleaser to drop docs/chore commits) into a user-facing summary highlighting the v1.x features that landed, then click Publish. End-user install instructions in `README.md` work from publish onward.

### Container image (GHCR) — auto-published on tag push

`.github/workflows/docker.yml` builds and pushes the multi-arch image
`ghcr.io/acoseac/1-bit-bridge` (linux/amd64 + linux/arm64) on every `v*` tag push, in parallel with
the goreleaser job — **no manual step per release.** It tags `:MAJOR.MINOR.PATCH`, `:MAJOR.MINOR`, and
`:latest`, injecting the tag into `version.ServerVersion` via the `VERSION` build-arg. The GHCR package
was made **public once** (introduced with v0.1.7) and stays public for every future version.

Per-release hygiene:

- **Keep `Dockerfile`'s `ARG GO_VERSION` in step with go.mod's `go` directive.** The alpine golang
  image runs `GOTOOLCHAIN=local`, so a stale value fails the image build with `go.mod requires go >= X`
  (this is what broke the v0.1.7 image build on the first attempt). The `docker` workflow surfaces it as
  a red run, but bump it proactively whenever go.mod bumps Go.
- **Verify after the tag pushes:** the `docker` workflow run is green and the new tags resolve — e.g.
  `docker buildx imagetools inspect ghcr.io/acoseac/1-bit-bridge:<ver>` (shows both arches), or the
  anonymous registry tags list.
- **Keep the pull instructions in sync** when the tag scheme or run/usage changes: `docs/docker.md`
  (this repo) and the 1-bit.app **download** page's container line (`1bitapp` repo,
  `src/pages/bridge/download.astro`).
- **Publishing an image for a tag whose own `Dockerfile` is broken** (a frozen tag that predates a
  `GO_VERSION` fix): dispatch the workflow with `tag=<version>` + `ref=main` so it builds current
  packaging from a ref whose Go source matches that release. This is how the v0.1.7 image was published.

If the operator-side runbook documents anything that's NOT in this CLAUDE.md, fold it back here — this section is the single source of truth for the release process.

