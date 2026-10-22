# Long form: CLAUDE.md's workflow and review-process entries

> Moved verbatim from `CLAUDE.md` on 2026-10-18 so it stops loading on every request. **These entries are still binding.** CLAUDE.md keeps each one as a single line ending `→ History-Workflow.md`; change an entry here and its line there in the same commit. Each `##` names the section the entries came from.

## Things that have bitten before

**Twenty-one claims in this list have been wrong and been corrected** — the
WAV/AIFF extractor gap, the `deletedIds` field name, "the bridge has no DLNA
Search", `manualDescriptionURL` being unimplemented, (2026-09-22)
"`waveform_path` has the same shape and NO adoption yet", which #954 had
falsified two days earlier by wiring `integrity.LocateWaveform` into
`analysisStoreAdapter`, (2026-09-25) "a broken existing config cannot block
the re-init that replaces it", which held for config-file and not for the port
checks, (2026-09-26) "`os.ReadDir("")` reads the process working
directory", (2026-09-27) "init never prompts for `customEndpoints`, so
the old value survives the rewrite", (2026-09-27) "`Load` serves a config
giving a blank name `DefaultLibraryName`", (2026-09-28) "`analyze --gc`'s
`Consider` requires `.1bwf`", (2026-09-28) "`mtime_ns = 0` does not
force a re-extraction", (2026-09-29) "the app's SSDP path has no
LOCATION-versus-source check", (2026-09-29) "Go binds a multicast
listener to the group address", (2026-09-29) "`isLossyCodec` gates every
`BitsPerSample` write site", (2026-09-29) "reconciliation never
crosses directories", (2026-09-29) "`os.SameFile` answers [a
directory's identity] portably", (2026-10-02) "an explicit delete
path list sidesteps the case-fold class entirely", (2026-10-15) "a
cover the cap evicts does not come back, a `local-` cover included",
(2026-10-16) "the two ID3v2 readers still differ on TXXX names (B126)",
(2026-10-22) "`resolveOutput` resolves the block's merge keys as
gopkg.in/yaml.v3 v3.0.1's decoder does", and (2026-10-22) "an output
refusal names the setting, never its value". The first five cost a later session real
time; the fourth was written **after** the PR that falsified it, by a session
that had this very warning in front of it, and the fifth sent `bridge doctor`
on telling operators to run `bridge analyze --force` — hours of decoding to
recover curves the next request rebinds for free. The sixth was found by measuring
the re-init rather than reading the bullet. The seventh was never true: a
bot's finding on #531, accepted without a probe, it spread to six code
comments and three test docblocks, two of whose tests passed with the
refusal deleted, and a bot on #1030 quoted it back as a rule. The eighth
was the premise a preflight judgement rested on, and no rewrite had ever
kept the value: measured, the same run also minted a new TLS pair over an
install that named its own. The ninth sat in a test docblock whose own table
asserted the opposite (`served: "  "`, as written), and `Load` gave the
default only to an exactly-empty name until #1042. The tenth named an
extension the bridge has never written (waveforms have been `.waveform.bin`
since #395), and survived because its conclusion, that a directory symlink
is no candidate there, holds for either spelling. The eleventh outlived the
code it described: #574 moved the skip gate to `GetTrackStat`, which reads
the `mtime_ns` column, and the bullet went on describing `GetTrack`, which
reads the mtime inside `tags_json`. The twelfth was about the OTHER repo, and
true for a day: the iOS app merged the check (acoseac/1-bit#1998), which
nothing in this repo sees, so it was corrected only because that session filed
the bridge's doc change as a backlog entry (B78). The thirteenth was the
premise of a backlog entry (B71) and, in another form, of a code comment
that chose a socket by it (the renderer discovery client's); measuring the
consequence the entry predicted is what found the premise false, from the
toolchain's own source. The fourteenth named a function #225's review had
already replaced (a denylist that failed open on an empty codec, for the
allowlist `canSetBitsPerSample`), and the comment beside the wire field said
the same; both were found while naming the compressed AIFF-C and WAV encodings
(B124).
The fifteenth sat in the Scanner bullet on the reconciliation passes, true of
four passes and not of the fifth, the year fill by release id, which crosses
folders by design; it was found while replaying the passes in memory (B188).
The sixteenth was the model a fix was told to copy: B203's entry pointed at
the delete handler's directory identity, kept from an `os.Stat`, and the
first version built the same way passed on macOS and deleted 39 rows of 40
on Windows, where `os.Stat` reads a directory's identity only when it is
compared.
The seventeenth was true of a PREFIX and read as true of a path: a delete
naming a track in another case moved it on a case-insensitive volume,
retired no row and rescanned its folder a second time (B219).
The eighteenth was half true and became the premise of the backlog entry
that fixed the cap (B84): two tests had pinned the other half since before
it was written (`TestScanner_RecoversWipedLocalArtworkCache` and
`TestArtworkLocalSentinelStaysPendingWhenEnrichmentComplete`), and
measuring the entry's own case end to end showed the `local-` cover come
back with the next scan.
The nineteenth was the twelfth's kind, about the other repo: the app shipped
B126 (acoseac/1-bit #2024) on 2026-09-30, the day B116 merged, and B116's
bullet said the readers differed for sixteen more days, until B127 rewrote the
sentence around it.
The twentieth and the twenty-first were written by the session that wrote
the code they describe, in B505's round 2, and that round's review found
both. The twentieth described a copy of a decoder's rules as the decoder: the
copy read a `!!binary` key by its encoded text, followed 64 merged blocks
and no more, and dropped what an alias key named twice. The twenty-first
called a refusal value-free while the decoder's own error, printed by the
same path, still quoted the value. Round 3 replaced the copy with the
decoder, and made the whole path value-free.
(Sections further down keep their own running tally of the same class, which
reaches higher; this count is of THIS list.) **Check the code before believing
any doc about it, including this one** — and when you find a stale claim,
correct it here rather than working around it.

**The same rot reaches CODE COMMENTS, and there it is more dangerous**, because a
comment is read as the reason for the code beside it. The retention LOUPE found
`apiDiagnostics` claiming "It touches no database" with its own "Three PRAGMAs"
and "Two COUNTs and a MIN" comments directly beneath, and `app.js` carrying a
second copy of that claim as the stated JUSTIFICATION for a 5-second poll. Both
were true when written. A false comment that explains a design choice is how the
next change gets made on the same reasoning.

## Things that have bitten before › Build, CI, and test discipline

- **A rate-limited bot's silence is not approval — and CodeRabbit's notice is
  both invisible and TRANSIENT.** Both bots post a quota notice and then don't
  review; "no comments" after one means unreviewed. CodeRabbit's is an HTML
  COMMENT inside its walkthrough body
  (`<!-- … rate limited by coderabbit.ai -->`), NOT a comment of its own, so
  every way of listing the thread reads as normally-reviewed. The allowance is
  derived from RECENT USE — one review per *hour* off ~88 attempts in 7 days —
  so it binds hardest on a multi-PR day, which is already the day this file says
  review gets skipped.
  **While you are blocked**, the marker is greppable and that is when it
  matters: `gh api --paginate repos/acoseac/1-bit-bridge/issues/<pr>/comments
  --jq '.[].body' | grep -c "rate limited by coderabbit"`. **Afterwards it is
  not**: CodeRabbit EDITS the marker out of the walkthrough when the review finally
  runs, so a later 0 is not evidence a review happened. Auditing after the fact
  asks a different question — whether the bot left any review or inline comment
  at all. Ask for the pass rather than waiting it out — but **`@coderabbitai
  review` does NOT clear a plan-limit pause**: on 2026-09-18 it answered
  "Review rate limited" on all five paused PRs, and the notice's own small
  print says the command applies only when AUTOMATIC reviews are paused. What
  works is the **"Run this review for free" checkbox inside the walkthrough
  comment** — tick it by PATCHing the comment body (`- [ ]` → `- [x]` on the
  `checkboxId` line; the repo owner may edit the bot's comment), and the
  review runs within minutes. **Unless the notice's own wait has passed**:
  on #1008 (2026-09-25) it said "wait 1 minute for your next included
  review", nothing had resumed two minutes later, and `@coderabbitai
  review` then answered "Review triggered" and ran a full pass. Once the
  wait is up, the command is the included route; the checkbox is an
  on-demand review, which the notice prices per reviewed file.
  `/gemini review` still works as written.
  CodeRabbit's real pass says "No actionable comments were generated" or
  "Actionable comments posted: N"; Gemini's says it has no comments to
  address. Anything less than one of those is not a round.
  **#900 is the live instance**: merged nineteen minutes after opening with the
  notice standing, and it carries zero CodeRabbit reviews and zero inline
  comments to this day. #901 was limited from its FIRST review too, which left
  both fix rounds unread while all twelve checks stayed green — it was only
  caught by looking for the notice on purpose.
  **And the verdict is in the WALKTHROUGH, not in `pulls/N/reviews`.**
  Querying the reviews API showed CodeRabbit's last review sitting on an
  older commit for five of six PRs while every one of them had in fact
  passed on its current head — the pass is an EDIT to the walkthrough issue
  comment. **Compare `coveredCommitId` to the PR head, never
  `headCommitId`**: the marker is `final_review_risk_coverage`'s
  `"coveredCommitId"`, and it moves only when a review finishes. This
  bullet said `headCommitId` until #1022, and a paused walkthrough defeats
  that check: its "Run this review for free" checkbox carries the head's
  `headCommitId` while the last round's verdict string stays in the
  comment, so on #1022 grep said "No actionable comments" and head
  `4eb5ff6f` while `coveredCommitId` said `5ff22781`. Absence of new
  findings is not a pass, and saying so out loud without checking is how
  this was learned twice. (#967–#972) **Editing the PR body re-renders the
  walkthrough and drops a pause notice's checkbox**; `@coderabbitai review`
  then answers "Review rate limited" and puts the notice, checkbox and all,
  back for the current head (#1022). **Push nothing while a ticked review
  runs**: on #1026 an on-demand run still "Starting" when two fix commits
  landed never posted, and the notice came back for the new head, so the
  checkbox had to be ticked again.

- **Read a PR's reviews and comments with `gh api --paginate`: it returns 30
  a page, and a long PR's newest round is on the page it drops.** On #996
  (40 reviews, 38 review comments) every check read page one only. Two
  CodeRabbit findings (12:17, 12:58) and two clean Gemini passes were never
  fetched. The report that went out said "CodeRabbit clean, Gemini silent,
  probably quota", and that story reached the engineering log, the PR body
  and a PR comment before an unresolved-thread query (GraphQL
  `reviewThreads`) showed otherwise. This is the
  complement of the walkthrough rule above, not a correction of it: the
  walkthrough's coverage and merge risk say a review RAN on a head, while a
  review that FOUND something posts a review ("Actionable comments posted:
  N") and inline comments. "Covered, and nothing seen" is clean only when
  every read was paginated. Before a merge, list the unresolved threads, and
  page that query too: `reviewThreads` is a connection, complete only once
  `pageInfo.hasNextPage` is false (CodeRabbit, on the first draft of this
  very rule, which said the query "returned them all").

- **Merging with review comments outstanding is a process failure**, not a
  shortcut — expect two rounds minimum. Verify a bot's severity label before
  acting: recurring false positives here include `windows.Errno` vs
  `syscall.Errno` (an alias — and the "fix" doesn't compile), guards proposed
  after `url.Parse` for a backslash host (Go refuses it outright), and claims
  that `omitempty` keeps a non-nil empty map. Reply on the thread with the
  evidence when declining.

## Bot-review discipline (PR-time)

- **`windows.Errno` vs `syscall.Errno`.** Gemini has flagged `errors.Is(err, windows.WSAEADDRINUSE)` as HIGH — "cross-package type mismatch, can never match" — and suggested `syscall.WSAEADDRINUSE`. Both halves are wrong: `x/sys/windows/aliases.go` declares `type Errno = syscall.Errno` (an **alias**, no distinct type) and `zerrors_windows.go` declares the constant AS `syscall.Errno`, so `errors.Is` compares the stdlib type with itself; and stdlib `syscall` on Windows has no `WSAEADDRINUSE`, so the suggested fix **would not compile**. The rationale now lives in `isAddrInUse`'s comment ([doctor_windows.go](../internal/doctor/doctor_windows.go)) — read it before re-raising.

- **A "compilation failure" claim that is really a test failure.** `os.Geteuid` IS defined on Windows (returns -1). Fixing the *stated* cause (a `//go:build !windows` tag) would have hidden a test from the platform where its other branch still works; the real fix was a runtime skip. The claim recurred on #1023, twice in one Gemini review, about root skips in files with no build tag. `GOOS=windows go test -c -o /dev/null <pkg>` settles it in seconds, and the PR's own `test (windows-latest)` leg is the evidence to cite.

**Merging with review comments outstanding is a process failure, not a shortcut.** PRs #562 / #563 / #564 (2026-07-22) each merged with one commit and no fix round — #563 nine minutes after its review landed, #564 while a comment was still in flight. That deferred one Major (the FLAC preflight double-read, #568) and one High (unvalidated updater asset URLs, #569) into a separate remediation batch, and both were real. The documented loop — *"don't merge after round 1 — expect 2 rounds minimum"* — is what catches this; a same-day 9-PR batch is exactly when it gets skipped and exactly when it matters.
