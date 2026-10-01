# Release playbook

The end-to-end procedure for a bridge release, as run for **v0.2.1** (2026-09-29 to
2026-10-01). It complements [`docs/release-process.md`](../docs/release-process.md),
which owns the per-release documentation refresh and the tag mechanics, and
[`ops/deployment-runbook.md`](deployment-runbook.md), which owns each host's deploy
commands. Read this first on release day, then those two for their sections.

Every upper-case placeholder (`<OPERATOR-SSH>`, `<DOCKER-TEST-SSH>` and the like) resolves in
the gitignored `ops/coordinates.local.md`; a lower-case one (`<sha>`, `<previous>`, `<arch>`)
is a value of the release in hand. This repo is
public: never write a real host, IP, SSH target or key path into this file. Until a
weakness is fixed, public records (PR titles and bodies, commits, release notes) name only
its backlog entry, never a reproduction recipe.

**Who decides.** The operator decides the scope, every production deploy, the tag and the
publish. Ask before each of those, even mid-run, unless they have just said to go ahead.
Everything else in this playbook runs without stopping for approval.

## 0. Scope and freeze

1. Read `~/Desktop/to-do/bridge-backlog.md` `## Open`. Bring the operator three lists: the
   entries that should block the release, the ones that can wait, and the decisions only they
   can make (product questions, a wire change, a deploy).
2. Agree the scope. From here on, merge only fixes in scope and docs.
3. Start a running list of **release-note lines**: every operator-visible change a fix PR
   names (a new config key, a changed default, a one-time step after updating). Section 4 needs
   it, and it is easy to lose across dozens of PRs.

## 1. Sanity reviews (read-only)

Three reviewers, run in parallel, each with a disjoint scope. Since v0.2.0 the tree had
changed by about 200 PRs, and the reviews found 1 high, 17 mediums and about 30 lows that
months of per-PR review had not.

| Reviewer | Scope |
|---|---|
| network | `internal/{api,admin,auth,adminauth,tls,pairingcode,dlna,upnp*,upnpproxy,atlasharvest,enrich,baseurl}`: auth bypass, SSRF, traversal, leaks into logs or the wire, DoS, wire shape against PROTOCOL.md |
| data | the scanner, the manifest store, extraction, migrations since the last tag, the deletion passes, the skip gate: anything that deletes or rewrites rows that still have files |
| operations | `cmd/bridge` wiring and shutdown, the job pools, integrity sweeps, backup, updater, config, init and doctor, packaging. Include **what the upgrade itself does**: the first boot over the previous release's database, the re-extraction, a rollback, and the previous release's updater installing this one. |

Each reviewer's prompt says:
- read-only: no fix and no PR;
- verify every candidate against the code, and reproduce it with a scratch test or the real
  binary where feasible;
- rate what survives (high / medium / low) with evidence;
- file LOWs straight into the backlog;
- report mediums and highs to the orchestrator only;
- **spawn no sub-agents** (one reviewer spawned six and spent a fifth of the weekly budget in
  ten minutes);
- never use the browser pane;
- delete scratch tests before finishing.

The orchestrator files every medium and high in the backlog as soon as the report arrives,
so a finding cannot be lost if the run stops, then triages them into fix agents.

## 2. Fixes

- **One fresh agent per fix.** Two related fixes can share an agent, in sequence. Every
  agent gets Appendix A, the common brief, and the finding's text verbatim. Each one: files
  or takes its backlog entry, reproduces red first, fixes, negative-controls, writes the
  CLAUDE.md rule and the engineering-log record, runs the gate, opens the PR, works the bot
  rounds, merges, and moves the entry to Done.
- **Ordering.** Fixes in the same file (`scanner.go`, `store.go`) run one after another in
  one agent, never in parallel.
- **New findings.** A fix agent's discoveries go into the backlog; the operator decides
  whether they join the release. In v0.2.1, B222 (year-1 dates) and B233 (a nightly fuzz
  failure) joined late, and B223 (HIGH) was found while fixing B203.

## 3. Validate a release candidate

The candidate is main's head once every fix in scope has merged. Record its sha.

1. **Gate and packaging** on the candidate: `make fmt vet test build-all P=2` (restore
   the two files gofmt 1.27 rewrites, per CLAUDE.md "Build"), then
   `goreleaser release --snapshot --clean --skip=publish`. Remove `dist/` afterwards.
2. **Docker upgrade test** on the Docker test host (`<DOCKER-TEST-SSH>`, `sudo -n docker`):
   - **Build:** clone the repo, check out the candidate, then
     `sudo -n docker buildx build --load -t bridge-p3:rc .` (BuildKit is required; see CLAUDE.md
     on the Dockerfile).
   - **Test library:** generate tagged tracks with the image's own ffmpeg (FLAC at 96/24 and
     44.1/16, an MP3), plus the repo's fixtures (`internal/manifest/testdata/m4a/*.m4a`, a DSF
     from `internal/transcode/testdata/`). Library permissions must be readable by UID 100.
   - **Upgrade path, the one a real install takes:**
     1. Run `ghcr.io/acoseac/1-bit-bridge:<previous>` on a FRESH named volume with `-p
        <port>:7788/tcp -p <port>:7788/udp`, the library mounted read-only and the audio env
        on.
     2. Let it scan, then pair a token (`docker exec … bridge pair --name upg`; the token is
        the line after "Bearer token").
     3. `docker rm -f` it, then run the candidate image on the SAME volume.
   - **Checks:** the TLS fingerprint and the paired token survive; the migrations reach the
     current schema; the re-extraction counts (rows rewritten against rows only stamped; a
     rewritten row must be explained); `/v1/health`; `bridge doctor` with no `--config` (exit
     0); no `TLS handshake error` lines from the HEALTHCHECK; the manifest; HTTP/3; a
     rendition; the original file's bytes; restart and `docker stop` timings (a clean exit 0
     well inside Docker's 10 s); `docker logs` with every ERROR and WARN explained.
     - HTTP/3: curl usually lacks it, so use a ~25-line quic-go `http3.Transport` client in an
       `_`-prefixed directory of the clone, run with `go run` in `golang:<go.mod version>` on
       `--network host`.
     - A rendition: `POST /v1/upscale` (`{"path": …, "kind": "optimize"}`), then download it
       with `?variant=<id>`; its size must equal the manifest's `sizeBytes`.
     - The original file: downloaded, it must be byte-identical to disk.
     - **Encode query paths with `%20`, never curl's `--data-urlencode`**, which writes `+`,
       which the bridge keeps as a literal plus (CLAUDE.md, `safeQuery`). A 404 "path not
       found" there is the client's encoding.
   - **Clean up** your containers, volumes and images.
3. **The candidate on the operator's bridge** (`<OPERATOR-SSH>`), by the runbook's manual
   form: §5.1's steps, with a locally cross-compiled binary built with `make build-all`'s
   ldflags. Watch the journal through the first full scan.
4. **Optionally, the candidate on the cloud tenants** (§5.3's procedure). For v0.2.1 this
   ran the candidate on 19 real tenants before the tag. It is the only test of public mode,
   managed settings, the shared proxy and many databases at once. It needs the operator's
   explicit go-ahead: most tenants are strangers'.
5. **The nightly fuzz on the candidate.** Check
   `gh run list --workflow fuzz.yml --limit 3` for a run on the candidate's sha before
   tagging. v0.2.1's B233 was found this way, after every other check had passed.
6. **The candidate's version string.** `git describe` stamps it `vX.Y.Z-N-g<sha>`, which the
   updater reads as build metadata (`normalizeDescribe`), equal to `vX.Y.Z`: no downgrade is
   offered or auto-installed. An app's own comparison may still show "update available", which
   is cosmetic and ends with the release.

## 4. Tag and publish

1. **Pre-tag checks.**
   - Main's head is the candidate plus fixes and docs only; `gh pr list --state open` is empty.
   - CI on the tag commit:
     `gh api repos/acoseac/1-bit-bridge/commits/<sha>/check-runs --paginate --jq '.check_runs[] | (.conclusion // .status)' | sort | uniq -c`.
     Every check must be green, with one known exception: SonarCloud's main-branch quality
     gate, red on every main commit since before v0.2.0. Its new-code period has run since
     2026-05-14, and three of its conditions fail over it: the reliability rating, the security
     rating and the share of security hotspots reviewed. Clearing them is backlog B242.
   - So the gate cannot show what this release added: look before tagging. List the bugs,
     vulnerabilities and security hotspots created since the previous release (the SonarCloud MCP
     server's `issues` tool with `created_after`, and its `hotspots` tool; or the project's Issues
     and Security Hotspots pages), and give each a disposition: fixed, marked safe or a false
     positive with the reason, or a backlog entry. One with no disposition blocks the tag.
   - The `Dockerfile`'s `ARG GO_VERSION` agrees with go.mod (docs/release-process.md).
2. **Tag**, annotated like the previous tags:
   `git tag -a vX.Y.Z <sha> -m "vX.Y.Z" && git push origin vX.Y.Z`.
3. **Wait for both workflows**, `release` and `docker`:
   `gh run list --limit 10 --json name,headBranch,status,conclusion --jq '.[] | select(.headBranch=="vX.Y.Z")'`.
   They take about 10 minutes.
4. **Verify the draft release.**
   - `gh release view vX.Y.Z --json assets` lists 8 assets: macOS, Linux and Windows for amd64
     and arm64, `checksums.txt` and `release-meta.json`.
   - Download the macOS arm64 archive and `checksums.txt`, then check:
     - `shasum -a 256 -c checksums.txt --ignore-missing` passes;
     - the binary prints `X.Y.Z (protocol v1)`;
     - `codesign -dv --verbose=2` shows `Developer ID Application`, the team id, a `Timestamp`
       and a `Runtime Version`.

     `spctl --assess` answering "not an app" is normal for a command-line binary.
   - The image: `docker buildx imagetools inspect ghcr.io/acoseac/1-bit-bridge:X.Y.Z` lists
     linux/amd64 and linux/arm64, and `:X.Y` and `:latest` carry the same digest.
5. **Release notes**, drafted in a scratch file and in this order:
   - a one-line build statement (tag sha, PR count, wire protocol unchanged or not);
   - **Highlights**, grouped by area (pairing and console; network and discovery; scanning and
     tags, with the ExtractorVersion range; renditions; operations and CLI; dependencies);
   - **Upgrade notes**: the running list from §0 (for v0.2.1, `bridge restart` once on
     macOS/Windows service installs updating from v0.2.0, `metrics.allowCidrs`, a loopback Host
     from a proxy, not rotating the cert for the stale-SAN warning, the one-time re-read);
   - **Verified before release**: §3's results.

   Name the entity "ars.md" in public prose, and include no private hosts and no attribution.
   Grep the file for IPs, host names and `ssh` before publishing. Remove the draft's own
   heading, since the release has a title.
6. **Publish:**
   `gh release edit vX.Y.Z --title "vX.Y.Z" --notes-file <file> --draft=false --latest`, then
   check that `gh api repos/acoseac/1-bit-bridge/releases/latest --jq .tag_name` is `vX.Y.Z`.
7. **Tell the iOS side** (its session or repo) that the release is out, with the upgrade notes
   its copy may need.

## 5. Roll out the release artifact, gated

The order is **the operator's bridge, then the public demo, then the cloud tenants**. Each
step starts only after the previous one is healthy, and only on the operator's go-ahead. Use
the RELEASE ARTIFACT from the GitHub release, never a local build. Its sha256 must equal
`checksums.txt`'s, and the same Linux amd64 binary goes to every amd64 host. Use **one SSH
ControlMaster per host for the whole step, never a burst of connections**: the hosts' flood
protection reads a burst as an attack (runbook, SSH note).

### 5.1 The operator's bridge (`<OPERATOR-SSH>`)

1. Check whether its updater already installed the release: compare health's
   `serverVersion` and the binary's sha256 against the release's. A candidate build compares
   equal to the previous tag, so the updater offers the new release once it is published.
2. Download only what the host needs:
   `gh release download vX.Y.Z -R acoseac/1-bit-bridge -p "*linux_<arch>*" -p checksums.txt`
   (`<arch>` is `amd64` where `uname -m` says `x86_64`, `arm64` where it says `aarch64`), then
   verify and extract.
3. Take a database backup per the runbook.
4. Upload, and verify the sha256 on arrival.
5. Do a DETACHED swap (`setsid nohup`, so a dropped SSH cannot leave it half-swapped):
   - keep the running binary as `/usr/local/bin/bridge.old-<UTC stamp>`;
   - install the new one with its mode;
   - `setcap cap_net_bind_service=+ep` (the `:443` bind);
   - restart the unit.
6. Verify:
   - the unit is active;
   - `curl -k` health (a self-signed cert) answers the new `serverVersion`, with the TLS
     fingerprint unchanged;
   - `bridge doctor` exits 0;
   - the journal through the first full scan: every ERROR and WARN explained, the re-extraction
     counts (rows rewritten against stamped; v0.2.1 rewrote exactly the 45 rows whose year was
     wrong), the DSD renders continuing.
   Watch about 20 minutes.
7. If anything regresses, roll back:
   `sudo mv /usr/local/bin/bridge /usr/local/bin/bridge.broken && sudo mv /usr/local/bin/bridge.old-<stamp> /usr/local/bin/bridge && sudo setcap cap_net_bind_service=+ep /usr/local/bin/bridge && sudo systemctl restart 1-bit-bridge`.
   The database stays: every migration only adds columns or tables, and the previous release
   opens the upgraded schema (measured for v0.2.0 against v0.2.1's schema 50).

### 5.2 The public demo (`bridge.1-bit.app`, on `<VPS-SSH>`)

The demo is its own unit: `1-bit-bridge-demo`, user `onebit-demo`, binary
`/usr/local/bin/bridge-demo`. It runs on the same host as the cloud conductor and the tenants;
touch none of those.
- **Deploy:** `deploy/linux/deploy-bridge-vps.sh` MUST carry `SVC` and `REMOTE_BIN` for the
  demo, because its defaults name another unit. The runbook's manual form works too: backup,
  detached swap keeping `bridge-demo.old-<stamp>`, owner and mode as before, then restart. The
  unit needs no capability.
- **Verify:**
  - `https://bridge.1-bit.app/v1/health`: the new version, a valid public certificate,
    `demoMode` true, the fingerprint unchanged;
  - the read-only refusals hold: `DELETE /v1/atlas-harvest/credential` answers 403
    `demo_read_only`, and the write routes answer 403 or 404 as before;
  - the manifest serves its usual track count;
  - a ranged download answers 206;
  - the journal is clean for about 15 minutes.
- **Rollback:** move `bridge-demo.old-<stamp>` back, then restart.

### 5.3 The cloud tenants

The procedure is the private conductor repo's (`~/dev/1-bit-conductor`):
`ops/server-provisioning.md`'s binaries row, with the newest dated `ops/log.md` fleet-upgrade
entry as the worked precedent. Its shape:
1. **List** the tenants. One may have signed up since the last count.
2. **Stage and install:** stage the binary, check its sha256 on arrival, `install` it into
   `/usr/local/lib/1-bit-bridge/releases/X.Y.Z/` (named LITERALLY; a regex over `bridge
   version` once created a directory name with a newline in it), and check the sha256 again.
3. **Config check:** load every tenant's config under the new binary, as that tenant's user.
4. **Re-point** only the `current` symlink. On that host `/usr/local/bin/bridge` is a regular
   file nothing runs.
5. **Restart** the `bridge@*` units ONE AT A TIME, each waiting for health, and restart a
   tenant mid-stream only in a gap between transfers.
6. **Verify every tenant:**
   - the unit is active;
   - `/v1/health` on :443 and :8443 through the proxy, with the tenant's own hostname
     (`curl --resolve`), verifying its certificate;
   - its console redirects to `/login`, which answers 200;
   - its journal is clean;
   - the conductor's dashboard shows every tenant healthy.

   Re-check at 10, 20 and 30 minutes.
7. **Never read a tenant's library or database** beyond aggregate health: most tenants are
   strangers'.
8. **Rollback:** point `current` at the previous release directory, then restart every
   `bridge@*` unit.

### 5.4 Record

- **This repo:** a PR adding the release's rows to `ops/deployment-runbook.md`'s "Release
  deploy ledger": each host, its time, and its rollback binary or directory.
- **The conductor repo:** an `ops/log.md` entry, the CLAUDE.md "Deployed:" line and
  `ops/hosts.md`'s binaries row. These are docs-only and go straight to main there.
- **Private memory:** a pointer, plus the rollback paths, which are perishable state.

## 6. Running it with agents

The v0.2.1 run used about 30 agents over three days. What made it work, and what cost:
- **Budget.** One full 5-hour window costs about 24-27% of the weekly limit. Read
  `get_usage` on a heartbeat (5-10 minutes), and agree stop lines with the operator: the
  5-hour wrap-up (94% in this run) and a weekly pause (95%, raised to 98% to finish the last
  two fixes).
- **The wrap-up protocol.** Send each agent "WRAP UP": it commits (WIP if needed), pushes
  only if clean, stops its loops, writes a handoff note and reports. After the reset, resume
  it from the note.
- **Context is the cost.** CLAUDE.md is loaded automatically (about 200k tokens), so a fresh
  agent starts at about 285k, and every turn costs about the agent's whole context. Agents
  resumed at 850k+ burned about three times the normal rate.
  - Tell every agent not to Read CLAUDE.md again.
  - Recycle an agent past about 600k: a handoff note, then a fresh agent from it.
  - Before a fresh agent switches to a branch, detach the old worktree
    (`git -C <old worktree> switch --detach`).
- **No sub-agents in a review**, and stop a review's children when stopping the review:
  check the mtimes of `subagents/agent-*.jsonl`.
- **Never the browser pane.** A site it has not seen waits on a permission prompt; one agent
  sat two hours on one. curl with a `Host:` or `Origin:` header and Go tests reproduce
  anything a browser would.
- **Waiting** is a bounded background loop that exits, never `sleep` in the foreground.
  Grep the end marker `exited with code`.
- **CodeRabbit.** Tick a free on-demand review when the pause notice says it is free now, and
  tick ONLY the "Run this review for free" line, never `request-automatic-reviews` (that asks
  the org admins to approve billing). Never tick a paid one. The verdict for a head is the
  walkthrough's `coveredCommitId`. Gemini is often over quota; record that on the PR.
- **Shell traps measured on the way:**
  - curl's `--data-urlencode` writes `+` (above);
  - zsh does not word-split `$var`;
  - `--include=*.go` must be quoted under zsh;
  - a Monitor expires after 30 minutes and must be re-armed.

## Appendix A — the common brief for fix agents

Give every fix agent this text (as a file it reads first), with `<SCRATCHPAD>` set to the
session's scratchpad directory.

```
You own ONE backlog entry end to end: reproduce, fix, PR, bot rounds, merge, backlog update.
The operator is fixing every relevant bug before tagging the release, so correctness beats speed.

Repo and tree
- CLAUDE.md is ALREADY in your context (loaded automatically, ~200k tokens) and is the spec.
  Do NOT Read it again; grep it for line numbers. Grep ops/engineering-log.md by symbol / file /
  PR before changing anything a rule names, and read only what you need.
- Keep your context small: ranged reads, capped output (| tail -40, grep -c), never cat a big log.
- You run in your OWN git worktree; work only there. Start: git fetch origin && git switch -c
  fix/<topic> origin/main. Before merging, git merge origin/main; never rebase or force-push once
  bots reviewed. CLAUDE.md / ops/engineering-log.md conflicts: keep both sides, the log in date order.

Workflow
- Red-first tests; -count=1 for any run that proves something. COMMIT before every negative control.
- CI's gofmt is go.mod's toolchain: check changed files with
  $(GOTOOLCHAIN=go<go.mod version> go env GOROOT)/bin/gofmt -l <files>. If make fmt ran, restore the
  files a newer gofmt rewrites (CLAUDE.md "Build").
- Inner loop: your packages under -race. Before pushing: make vet test build-all P=2; remove dist/.
  A test timing out at exactly 600 s is the default -timeout, not a failure.
- The rule goes in CLAUDE.md (the matching group under "Things that have bitten before"), the record
  in ops/engineering-log.md, both in the SAME PR as the code. Correct any stale claim you find.
- NO Claude attribution anywhere: no Co-Authored-By trailer, no "Generated with" footer.
- Follow-ups outside your entry: a new Open entry in ~/Desktop/to-do/bridge-backlog.md (next free
  id; the file's shape; re-read before each edit; Edit tool). Never spawn_task.

PR and bots
- Title: one plain sentence saying what now holds, ending "(backlog B<id>)". Body: what, why,
  evidence, negative controls, the gate output.
- Waiting: bounded background loops that exit; grep "exited with code". Leave no loop running.
- Read reviews with gh api --paginate; list unresolved threads with GraphQL reviewThreads, paging
  until hasNextPage is false.
- CodeRabbit's verdict is the walkthrough's coveredCommitId. Free on-demand review: tick ONLY the
  "Run this review for free" line (diff the body: exactly one line changed). Never a paid review;
  if no free or included review can start within about an hour, record the unreviewed commits on
  the PR and merge once everything else is green.
- Gemini: /gemini review per head; often over quota, note it and move on.
- A bot's finding is a claim: verify, decline with evidence. Expect two rounds.
- Merge when the required checks are green on the final head, CodeRabbit covered it (or its pause
  is recorded), and every thread is answered: gh pr merge <n> --squash --delete-branch. Then move
  the backlog entry to Done in one line (PR, merge sha, one sentence).

Hosts (never a production host; never deploy)
- Docker test host <DOCKER-TEST-SSH>, Windows test host <WINDOWS-TEST-SSH> (ops/coordinates.local.md).
  Long jobs detached. One SSH ControlMaster per host, never a burst.
- Never use the browser pane (mcp__Claude_Browser__*).

Wrap-up
- On a message starting "WRAP UP": commit (WIP if needed), push only if clean, stop your loops,
  write <SCRATCHPAD>/handoff-<name>.md (branch, PR, state, what is left, next command), report.

Final message: at most 15 lines (PR link and merge sha, what changed, evidence, backlog ids, open items).
```
